package persistent

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// ReservationTTL is how long fees stay set aside for a billing period that was not paid. After
// that they are free to be reserved by the next period. It is longer than the renewal window plus
// the 14 days the POS keeps retrying its report of a payment, so a payment that is reported late
// still finds its fees reserved and they are never charged twice.
const ReservationTTL = 21 * 24 * time.Hour

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var feeLabels = map[string]struct{ code, label string }{
	entity.FeeSourceOnline: {"ONLINE_ORDERS", "Biaya platform pesanan online"},
	entity.FeeSourcePickup: {"PICKUP_ORDERS", "Biaya platform pesanan bayar di tempat"},
}

// readFees totals a merchant's open fees matching a filter, grouped the way invoices show them.
// where is a SQL condition on the ledger alias l; args follow the merchant id ($1).
func readFees(ctx context.Context, q querier, merchantID, where string, args ...any) ([]entity.FeeLine, int64, error) {
	rows, err := q.Query(ctx, `
		SELECT l.source, count(DISTINCT l.order_id), COALESCE(sum(l.amount_minor), 0)
		FROM fee_ledger l WHERE l.merchant_id = $1 AND l.settled_at IS NULL AND `+where+`
		GROUP BY l.source ORDER BY l.source`, append([]any{merchantID}, args...)...)
	if err != nil {
		return nil, 0, fmt.Errorf("read fees: %w", err)
	}
	defer rows.Close()

	lines := []entity.FeeLine{}

	var total int64

	for rows.Next() {
		var (
			source string
			orders int
			amount int64
		)

		if err := rows.Scan(&source, &orders, &amount); err != nil {
			return nil, 0, err
		}

		meta := feeLabels[source]
		lines = append(lines, entity.FeeLine{Code: meta.code, Label: meta.label, Orders: orders, AmountMinor: amount})
		total += amount
	}

	return lines, total, rows.Err()
}

// PendingFees is what a merchant owes now: every fee not yet collected, reserved or not.
func (s *Store) PendingFees(ctx context.Context, merchantID string) (entity.FeeReservation, error) {
	lines, total, err := readFees(ctx, s.pool, merchantID, "true")

	return entity.FeeReservation{Lines: lines, TotalMinor: total}, err
}

// ReserveFees sets aside everything the merchant owes for one billing period, so the charge and
// the settlement refer to the same entries. It is safe to repeat: the same period always gets the
// same entries, however often it is asked and whatever accrues meanwhile. A period whose fees were
// already collected gets nothing more. Reservations nobody settled in ReservationTTL are released.
func (s *Store) ReserveFees(ctx context.Context, merchantID, ref string) (entity.FeeReservation, error) {
	out := entity.FeeReservation{BillingRef: ref}

	err := s.inTx(ctx, func(tx txn) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "fees:"+merchantID); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE fee_ledger SET billing_ref = NULL, reserved_at = NULL
			WHERE merchant_id = $1 AND settled_at IS NULL AND billing_ref IS NOT NULL
			  AND reserved_at < now() - make_interval(secs => $2)`, merchantID, ReservationTTL.Seconds()); err != nil {
			return err
		}

		var collected, reserved bool
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(bool_or(settled_at IS NOT NULL), false), count(*) > 0
			FROM fee_ledger WHERE merchant_id = $1 AND billing_ref = $2`, merchantID, ref).Scan(&collected, &reserved); err != nil {
			return err
		}

		if collected {
			out.Collected = true
			out.Lines = []entity.FeeLine{}

			return nil
		}

		if !reserved {
			if _, err := tx.Exec(ctx, `
				UPDATE fee_ledger SET billing_ref = $2, reserved_at = now()
				WHERE merchant_id = $1 AND settled_at IS NULL AND billing_ref IS NULL`, merchantID, ref); err != nil {
				return err
			}
		}

		var err error

		out.Lines, out.TotalMinor, err = readFees(ctx, tx, merchantID, "l.billing_ref = $2", ref)

		return err
	})

	return out, err
}

// SettleFees marks the fees reserved for a billing period as collected, because the merchant's
// subscription payment that included them succeeded. Repeating it changes nothing. It returns how
// many entries were settled and their total.
func (s *Store) SettleFees(ctx context.Context, merchantID, ref, settlementRef string) (int, int64, error) {
	var (
		n     int
		total int64
	)

	err := s.pool.QueryRow(ctx, `
		WITH done AS (
			UPDATE fee_ledger SET settled_at = now(), settlement_ref = $3
			WHERE merchant_id = $1 AND billing_ref = $2 AND settled_at IS NULL RETURNING amount_minor
		) SELECT count(*), COALESCE(sum(amount_minor), 0) FROM done`, merchantID, ref, settlementRef).Scan(&n, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("settle fees: %w", err)
	}

	return n, total, nil
}

// SettleFeesManually marks fees collected outside the subscription charge (the operator invoiced
// the merchant). Only entries created before through, and not set aside for a billing period in
// flight, are touched.
func (s *Store) SettleFeesManually(ctx context.Context, merchantID string, through time.Time, note string) (int, int64, error) {
	var (
		n     int
		total int64
	)

	err := s.pool.QueryRow(ctx, `
		WITH done AS (
			UPDATE fee_ledger SET settled_at = now(), settlement_ref = 'manual', settlement_note = $3
			WHERE merchant_id = $1 AND settled_at IS NULL AND billing_ref IS NULL AND created_at < $2 RETURNING amount_minor
		) SELECT count(*), COALESCE(sum(amount_minor), 0) FROM done`, merchantID, through, note).Scan(&n, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("settle fees manually: %w", err)
	}

	return n, total, nil
}
