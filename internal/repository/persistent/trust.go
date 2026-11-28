package persistent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// insertTrustEvent writes one event. An event of the same kind for the same order is ignored,
// so a POS event delivered twice records nothing new.
func insertTrustEvent(ctx context.Context, tx txn, ev entity.TrustEvent) error {
	evidence := ev.Evidence
	if len(evidence) == 0 {
		evidence = json.RawMessage(`{}`)
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO customer_trust_events (customer_id, event_type, source, order_id, outlet_id, merchant_id, evidence, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE(NULLIF($8::timestamptz, '0001-01-01'::timestamptz), now()))
		ON CONFLICT DO NOTHING`,
		ev.CustomerID, ev.Type, ev.Source, ev.OrderID, ev.OutletID, ev.MerchantID, []byte(evidence), ev.CreatedAt)

	return err
}

// RecordTrustEvent writes an event outside an order change (for example a refused checkout).
func (s *Store) RecordTrustEvent(ctx context.Context, ev entity.TrustEvent) error {
	return s.inTx(ctx, func(tx txn) error { return insertTrustEvent(ctx, tx, ev) })
}

// recordOutcome writes the trust events of an order that just changed state.
func recordOutcome(ctx context.Context, tx txn, orderID string, o entity.Outcome) error {
	var (
		readyAt  *time.Time
		payMode  string
		customer string
		outlet   string
		merchant string
	)

	if err := tx.QueryRow(ctx,
		`SELECT customer_id, outlet_id, merchant_id, payment_mode, ready_at FROM merchant_orders WHERE id = $1`, orderID,
	).Scan(&customer, &outlet, &merchant, &payMode, &readyAt); err != nil {
		return err
	}

	o.CustomerID, o.OrderID, o.OutletID, o.MerchantID, o.PaymentMode, o.ReadyAt = customer, orderID, outlet, merchant, payMode, readyAt

	for _, ev := range entity.OutcomeEvents(o) {
		if err := insertTrustEvent(ctx, tx, ev); err != nil {
			return err
		}
	}

	return nil
}

// newPickupCode picks four digits no other open order of the outlet uses. The caller holds the
// outlet's pickup lock, so two checkouts never pick the same one.
func newPickupCode(ctx context.Context, tx txn, outletID string) (string, error) {
	for range entity.PickupCodeMaxOpenTries {
		n, err := rand.Int(rand.Reader, big.NewInt(10000))
		if err != nil {
			return "", err
		}

		code := fmt.Sprintf("%04d", n.Int64())

		var taken bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM merchant_orders WHERE outlet_id = $1 AND pickup_code = $2
			  AND status IN ('UNPAID','CREATED','PENDING_ACCEPTANCE','ACCEPTED','PREPARING','READY'))`,
			outletID, code).Scan(&taken); err != nil {
			return "", err
		}

		if !taken {
			return code, nil
		}
	}

	// Crowded outlet: take the first free code, whichever it is.
	var code string

	err := tx.QueryRow(ctx, `
		SELECT lpad(c::text, 4, '0') FROM generate_series(0, 9999) c
		WHERE NOT EXISTS (SELECT 1 FROM merchant_orders WHERE outlet_id = $1 AND pickup_code = lpad(c::text, 4, '0')
		  AND status IN ('UNPAID','CREATED','PENDING_ACCEPTANCE','ACCEPTED','PREPARING','READY'))
		LIMIT 1`, outletID).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", entity.ErrTooMany
	}

	return code, err
}

// RecordCapHit notes that a customer ran into the open-order or hourly limit. At most one is
// kept per ten minutes, so a client that retries does not fill the log.
func (s *Store) RecordCapHit(ctx context.Context, customerID, outletID, merchantID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO customer_trust_events (customer_id, event_type, source, outlet_id, merchant_id)
		SELECT $1, 'OPEN_CAP_HIT', 'SYSTEM', $2, $3
		WHERE NOT EXISTS (
		  SELECT 1 FROM customer_trust_events
		  WHERE customer_id = $1 AND event_type = 'OPEN_CAP_HIT' AND created_at > now() - interval '10 minutes')`,
		customerID, outletID, merchantID)

	return err
}

// TrustEvents lists a customer's events since a time, oldest first.
func (s *Store) TrustEvents(ctx context.Context, customerID string, since time.Time) ([]entity.TrustEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, customer_id, event_type, source, order_id, outlet_id, merchant_id, evidence, created_at
		FROM customer_trust_events WHERE customer_id = $1 AND created_at >= $2 ORDER BY created_at, id`, customerID, since)
	if err != nil {
		return nil, fmt.Errorf("trust events: %w", err)
	}
	defer rows.Close()

	return scanTrustEvents(rows)
}

// RecentTrustEvents lists the latest events of a customer, newest first, for the operator.
func (s *Store) RecentTrustEvents(ctx context.Context, customerID string, limit int) ([]entity.TrustEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, customer_id, event_type, source, order_id, outlet_id, merchant_id, evidence, created_at
		FROM customer_trust_events WHERE customer_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, customerID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent trust events: %w", err)
	}
	defer rows.Close()

	return scanTrustEvents(rows)
}

func scanTrustEvents(rows pgx.Rows) ([]entity.TrustEvent, error) {
	var out []entity.TrustEvent

	for rows.Next() {
		var ev entity.TrustEvent
		if err := rows.Scan(&ev.ID, &ev.CustomerID, &ev.Type, &ev.Source, &ev.OrderID, &ev.OutletID, &ev.MerchantID, &ev.Evidence, &ev.CreatedAt); err != nil {
			return nil, err
		}

		out = append(out, ev)
	}

	return out, rows.Err()
}

// SaveTrustProfile stores a computed profile, replacing the previous one.
func (s *Store) SaveTrustProfile(ctx context.Context, p entity.TrustProfile) error {
	reasons, err := json.Marshal(p.Reasons)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO customer_trust_profile (customer_id, score, tier, finished_orders, completed_orders, reasons, computed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (customer_id) DO UPDATE SET score = $2, tier = $3, finished_orders = $4,
		  completed_orders = $5, reasons = $6, computed_at = $7`,
		p.CustomerID, p.Score, p.Tier, p.FinishedOrders, p.CompletedOrders, reasons, p.ComputedAt)

	return err
}

// TrustConfig returns the rows of the trust_config table, by key. Empty means defaults.
func (s *Store) TrustConfig(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM trust_config`)
	if err != nil {
		return nil, fmt.Errorf("trust config: %w", err)
	}
	defer rows.Close()

	out := map[string]json.RawMessage{}

	for rows.Next() {
		var (
			key   string
			value []byte
		)

		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}

		out[key] = value
	}

	return out, rows.Err()
}

// CustomersToScore lists customers who have events newer than their stored profile (or no
// profile at all).
func (s *Store) CustomersToScore(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.customer_id FROM customer_trust_events e
		LEFT JOIN customer_trust_profile p ON p.customer_id = e.customer_id
		GROUP BY e.customer_id, p.computed_at
		HAVING p.computed_at IS NULL OR max(e.created_at) > p.computed_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("customers to score: %w", err)
	}
	defer rows.Close()

	var ids []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// CustomersWithEvents lists every customer who has any event, for the tier distribution.
func (s *Store) CustomersWithEvents(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT customer_id FROM customer_trust_events LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("customers with events: %w", err)
	}
	defer rows.Close()

	var ids []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// OpenOrderCount is how many orders of a customer are still going on.
func (s *Store) OpenOrderCount(ctx context.Context, customerID string) (int, error) {
	var n int

	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM merchant_orders WHERE customer_id = $1
		  AND status IN ('UNPAID','CREATED','PENDING_ACCEPTANCE','ACCEPTED','PREPARING','READY')`, customerID).Scan(&n)

	return n, err
}
