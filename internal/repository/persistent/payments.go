package persistent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// ------------------------------------------------------------------ gateways

const gatewayColumns = `id, merchant_id, provider, credentials_enc, sandbox, active, verified_at, created_at, updated_at`

func scanGateway(row pgx.Row) (*entity.Gateway, error) {
	var g entity.Gateway

	if err := row.Scan(&g.ID, &g.MerchantID, &g.Provider, &g.CredentialsEnc, &g.Sandbox, &g.Active, &g.VerifiedAt, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, err
	}

	return &g, nil
}

// GatewayByMerchant returns a merchant's gateway in any state.
func (s *Store) GatewayByMerchant(ctx context.Context, merchantID string) (*entity.Gateway, error) {
	g, err := scanGateway(s.pool.QueryRow(ctx, `SELECT `+gatewayColumns+` FROM merchant_gateways WHERE merchant_id = $1`, merchantID))
	if err != nil {
		return nil, notFound(err)
	}

	return g, nil
}

// GatewayByID returns a gateway.
func (s *Store) GatewayByID(ctx context.Context, id string) (*entity.Gateway, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, entity.ErrNotFound
	}

	g, err := scanGateway(s.pool.QueryRow(ctx, `SELECT `+gatewayColumns+` FROM merchant_gateways WHERE id = $1`, id))
	if err != nil {
		return nil, notFound(err)
	}

	return g, nil
}

// SaveGateway sets a merchant's gateway, replacing any earlier one (the id stays, so open
// payments keep pointing at it).
func (s *Store) SaveGateway(ctx context.Context, merchantID, provider, credentialsEnc string, sandbox bool) (*entity.Gateway, error) {
	g, err := scanGateway(s.pool.QueryRow(ctx, `
		INSERT INTO merchant_gateways (merchant_id, provider, credentials_enc, sandbox, active, verified_at)
		VALUES ($1, $2, $3, $4, true, now())
		ON CONFLICT (merchant_id) DO UPDATE SET provider = $2, credentials_enc = $3, sandbox = $4,
		  active = true, verified_at = now(), updated_at = now()
		RETURNING `+gatewayColumns, merchantID, provider, credentialsEnc, sandbox))
	if err != nil {
		return nil, fmt.Errorf("save gateway: %w", err)
	}

	return g, nil
}

// DeactivateGateway stops new online payments for a merchant. Payments already open still finish.
func (s *Store) DeactivateGateway(ctx context.Context, merchantID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE merchant_gateways SET active = false, updated_at = now() WHERE merchant_id = $1`, merchantID)
	if err != nil {
		return err
	}

	if tag.RowsAffected() == 0 {
		return entity.ErrNotFound
	}

	return nil
}

// ------------------------------------------------------------------ payments

const paymentColumns = `
	p.id, p.order_id, p.gateway_id, p.provider, p.attempt, p.external_id, p.provider_charge_id, p.status,
	p.amount_minor, p.order_total_minor, p.customer_fee_minor, p.merchant_fee_minor,
	p.customer_fee_bps, p.merchant_fee_bps, p.pay_url, p.expires_at, p.paid_at, p.method,
	p.error_code, p.error_message, p.refund_status, p.refund_attempts, p.refunded_at, p.created_at`

func scanPayment(row pgx.Row) (*entity.Payment, error) {
	var p entity.Payment

	err := row.Scan(&p.ID, &p.OrderID, &p.GatewayID, &p.Provider, &p.Attempt, &p.ExternalID, &p.ProviderChargeID, &p.Status,
		&p.AmountMinor, &p.OrderTotalMinor, &p.CustomerFeeMinor, &p.MerchantFeeMinor,
		&p.CustomerFeeBps, &p.MerchantFeeBps, &p.PayURL, &p.ExpiresAt, &p.PaidAt, &p.Method,
		&p.ErrorCode, &p.ErrorMessage, &p.RefundStatus, &p.RefundAttempts, &p.RefundedAt, &p.CreatedAt)
	if err != nil {
		return nil, err
	}

	return &p, nil
}

// loadPayments attaches each order's latest payment attempt.
func (s *Store) loadPayments(ctx context.Context, orders ...*entity.Order) error {
	if len(orders) == 0 {
		return nil
	}

	byID := make(map[string]*entity.Order, len(orders))
	ids := make([]string, 0, len(orders))

	for _, o := range orders {
		byID[o.ID] = o

		ids = append(ids, o.ID)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (p.order_id) `+paymentColumns+`
		FROM payments p WHERE p.order_id = ANY($1::uuid[])
		ORDER BY p.order_id, (p.status = 'PAID') DESC, p.attempt DESC`, ids)
	if err != nil {
		return fmt.Errorf("order payments: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return fmt.Errorf("scan payment: %w", err)
		}

		byID[p.OrderID].Payment = p
	}

	return rows.Err()
}

// PaymentByID returns one payment attempt.
func (s *Store) PaymentByID(ctx context.Context, id string) (*entity.Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments p WHERE p.id = $1`, id))
	if err != nil {
		return nil, notFound(err)
	}

	return p, nil
}

// PaymentByExternal finds the attempt a gateway notification is about.
func (s *Store) PaymentByExternal(ctx context.Context, gatewayID, externalID string) (*entity.Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments p WHERE p.gateway_id = $1 AND p.external_id = $2`, gatewayID, externalID))
	if err != nil {
		return nil, notFound(err)
	}

	return p, nil
}

// NewAttempt is a payment attempt to start.
type NewAttempt struct {
	OrderID    string
	Gateway    *entity.Gateway
	Rates      entity.FeeRates
	Fees       entity.Fees
	OrderTotal int64
}

// StartAttempt opens a payment attempt on an unpaid order. If an attempt is already open and
// still valid it is returned instead (created is false), so retrying is harmless. An order that
// is paid, closed or past its deadline is ErrNotPayable.
func (s *Store) StartAttempt(ctx context.Context, in NewAttempt) (*entity.Payment, bool, error) {
	var (
		id      string
		created bool
	)

	err := s.inTx(ctx, func(tx txn) error {
		var (
			status   string
			deadline *time.Time
		)

		if err := tx.QueryRow(ctx, `SELECT status, payment_deadline FROM merchant_orders WHERE id = $1 FOR UPDATE`, in.OrderID).
			Scan(&status, &deadline); err != nil {
			return notFound(err)
		}

		if status != entity.OrderUnpaid || deadline == nil || !deadline.After(time.Now()) {
			return entity.ErrNotPayable
		}

		// An open attempt that has not expired is the answer; an expired one is closed.
		var openID string

		err := tx.QueryRow(ctx, `SELECT id FROM payments WHERE order_id = $1 AND status = 'PENDING' AND (expires_at IS NULL OR expires_at > now() + interval '20 seconds')`, in.OrderID).Scan(&openID)

		switch {
		case err == nil:
			id = openID

			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		if _, err := tx.Exec(ctx, `UPDATE payments SET status = 'EXPIRED', updated_at = now() WHERE order_id = $1 AND status = 'PENDING'`, in.OrderID); err != nil {
			return err
		}

		external := "sfp_" + strings.ReplaceAll(uuid.NewString(), "-", "")

		if err := tx.QueryRow(ctx, `
			INSERT INTO payments (order_id, gateway_id, provider, attempt, external_id, amount_minor, order_total_minor,
			  customer_fee_minor, merchant_fee_minor, customer_fee_bps, merchant_fee_bps, expires_at)
			VALUES ($1, $2, $3, (SELECT COALESCE(max(attempt), 0) + 1 FROM payments WHERE order_id = $1), $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING id`,
			in.OrderID, in.Gateway.ID, in.Gateway.Provider, external, in.Fees.ChargeMinor, in.OrderTotal,
			in.Fees.CustomerFeeMinor, in.Fees.MerchantFeeMinor, in.Rates.CustomerBps, in.Rates.MerchantBps, deadline).Scan(&id); err != nil {
			return err
		}

		created = true

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	p, err := s.PaymentByID(ctx, id)

	return p, created, err
}

// SetCharge records the payment page the gateway made for an attempt.
func (s *Store) SetCharge(ctx context.Context, paymentID, chargeID, payURL string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE payments SET provider_charge_id = $2, pay_url = $3, expires_at = $4, updated_at = now()
		WHERE id = $1 AND status = 'PENDING'`, paymentID, chargeID, payURL, expiresAt)

	return err
}

// FailAttempt ends an open attempt that could not be started or was refused.
func (s *Store) FailAttempt(ctx context.Context, paymentID, code, message string) error {
	if len(message) > 300 {
		message = message[:300]
	}

	_, err := s.pool.Exec(ctx, `
		UPDATE payments SET status = 'FAILED', error_code = $2, error_message = $3, updated_at = now()
		WHERE id = $1 AND status = 'PENDING'`, paymentID, code, message)

	return err
}

// ChargeUpdate is what a gateway says about an attempt.
type ChargeUpdate struct {
	State   string // PAID, FAILED or EXPIRED
	PaidAt  *time.Time
	Method  string
	Code    string
	Message string
}

// ChargeResult is what applying an update changed.
type ChargeResult struct {
	Changed bool
	Order   *entity.Order
	Payment *entity.Payment
	// Closed are other open attempts of the order that were cancelled because this one was paid;
	// their payment pages must be closed at the gateway.
	Closed []*entity.Payment
}

// PaymentNotify builds the push message for a payment event, or nil for none.
type PaymentNotify func(o *entity.Order, p *entity.Payment) *entity.Notification

// ApplyCharge records a gateway's verdict on an attempt. An open attempt becomes paid, failed or
// expired. A paid one sends its order on to the POS. An attempt that is already settled is left
// alone, except that a payment arriving after the attempt had been closed is still recorded as
// paid: the money moved, so it must be used or refunded.
func (s *Store) ApplyCharge(ctx context.Context, paymentID string, u ChargeUpdate, notify PaymentNotify) (ChargeResult, error) {
	var res ChargeResult

	err := s.inTx(ctx, func(tx txn) error {
		var orderID, status string
		if err := tx.QueryRow(ctx, `SELECT order_id, status FROM payments WHERE id = $1`, paymentID).Scan(&orderID, &status); err != nil {
			return notFound(err)
		}

		// Lock the order first, then the payment: every path takes them in this order.
		var orderStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM merchant_orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&orderStatus); err != nil {
			return notFound(err)
		}

		if err := tx.QueryRow(ctx, `SELECT status FROM payments WHERE id = $1 FOR UPDATE`, paymentID).Scan(&status); err != nil {
			return notFound(err)
		}

		switch {
		case status == entity.PaymentPaid:
			return nil
		case u.State == entity.PaymentPaid:
			// fall through: pay it, whatever state it was in
		case status != entity.PaymentPending:
			return nil
		}

		paidAt := time.Now()
		if u.PaidAt != nil && !u.PaidAt.IsZero() {
			paidAt = *u.PaidAt
		}

		switch u.State {
		case entity.PaymentPaid:
			if _, err := tx.Exec(ctx, `
				UPDATE payments SET status = 'PAID', paid_at = $2, method = NULLIF($3, ''), error_code = NULL, error_message = NULL,
				  updated_at = now() WHERE id = $1`, paymentID, paidAt, u.Method); err != nil {
				return err
			}

			if orderStatus == entity.OrderUnpaid {
				if _, err := tx.Exec(ctx, `
					UPDATE merchant_orders SET status = 'CREATED', available_at = now(), submit_attempts = 0,
					  lease_until = NULL, lease_token = NULL, checked_at = now(), updated_at = now() WHERE id = $1`, orderID); err != nil {
					return err
				}

				closed, err := cancelOpenAttempts(ctx, tx, orderID, paymentID)
				if err != nil {
					return err
				}

				res.Closed = closed
			}
		case entity.PaymentFailed, entity.PaymentExpired:
			if _, err := tx.Exec(ctx, `
				UPDATE payments SET status = $2, error_code = NULLIF($3, ''), error_message = NULLIF($4, ''), updated_at = now()
				WHERE id = $1`, paymentID, u.State, u.Code, u.Message); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown payment state %q", u.State)
		}

		res.Changed = true

		return finishPaymentEvent(ctx, tx, orderID, paymentID, notify, &res)
	})
	if err != nil {
		return ChargeResult{}, err
	}

	return res, nil
}

// cancelOpenAttempts closes the open attempts of an order other than keep.
func cancelOpenAttempts(ctx context.Context, tx txn, orderID, keep string) ([]*entity.Payment, error) {
	rows, err := tx.Query(ctx, `
		UPDATE payments p SET status = 'CANCELLED', updated_at = now()
		WHERE p.order_id = $1 AND p.status = 'PENDING' AND p.id <> $2 RETURNING `+paymentColumns, orderID, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*entity.Payment

	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, rows.Err()
}

// finishPaymentEvent reloads the order and payment, queues the push message and wakes listeners.
func finishPaymentEvent(ctx context.Context, tx txn, orderID, paymentID string, notify PaymentNotify, res *ChargeResult) error {
	o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+orderFrom+`WHERE o.id = $1`, orderID))
	if err != nil {
		return err
	}

	p, err := scanPayment(tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments p WHERE p.id = $1`, paymentID))
	if err != nil {
		return err
	}

	o.Payment, res.Order, res.Payment = p, o, p

	if notify != nil {
		if err := queueNotification(ctx, tx, notify(o, p)); err != nil {
			return err
		}
	}

	_, err = tx.Exec(ctx, `SELECT pg_notify('order_updates', $1)`, orderID)

	return err
}

func queueNotification(ctx context.Context, tx txn, n *entity.Notification) error {
	if n == nil {
		return nil
	}

	data, err := json.Marshal(n.Data)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO notification_outbox (customer_id, order_id, kind, title, body, data)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (order_id, kind) DO NOTHING`,
		n.CustomerID, n.OrderID, n.Kind, n.Title, n.Body, data)

	return err
}

// PendingPayments lists open attempts the gateway has not been asked about for a while.
func (s *Store) PendingPayments(ctx context.Context, olderThan time.Duration, limit int) ([]*entity.Payment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+paymentColumns+` FROM payments p
		WHERE p.status = 'PENDING' AND p.provider_charge_id IS NOT NULL AND p.checked_at < now() - make_interval(secs => $1)
		ORDER BY p.checked_at LIMIT $2`, olderThan.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("pending payments: %w", err)
	}
	defer rows.Close()

	var out []*entity.Payment

	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, rows.Err()
}

// TouchPayment remembers that the gateway was asked about an attempt.
func (s *Store) TouchPayment(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE payments SET checked_at = now() WHERE id = $1`, id)

	return err
}

// ------------------------------------------------------------ unpaid orders

// OrderGateway is an order with the gateway to pay it through.
type OrderGateway struct {
	Order   *entity.Order
	Gateway *entity.Gateway
}

// ExpiredUnpaid lists unpaid orders past their deadline.
func (s *Store) ExpiredUnpaid(ctx context.Context, limit int) ([]*entity.Order, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+orderColumns+orderFrom+`
		WHERE o.status = 'UNPAID' AND o.payment_deadline <= now() ORDER BY o.payment_deadline LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("expired unpaid: %w", err)
	}
	defer rows.Close()

	var out []*entity.Order

	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, o)
	}

	return out, rows.Err()
}

// OpenAttempts lists an order's attempts that are still open.
func (s *Store) OpenAttempts(ctx context.Context, orderID string) ([]*entity.Payment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+paymentColumns+` FROM payments p WHERE p.order_id = $1 AND p.status = 'PENDING'`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*entity.Payment

	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, rows.Err()
}

// ExpireUnpaid closes an unpaid order whose deadline passed. It reports whether it did: false
// means the order was paid or cancelled meanwhile. Open attempts are closed too and returned.
func (s *Store) ExpireUnpaid(ctx context.Context, orderID string, notify Notify) (bool, []*entity.Payment, error) {
	var (
		done   bool
		closed []*entity.Payment
	)

	err := s.inTx(ctx, func(tx txn) error {
		tag, err := tx.Exec(ctx, `
			UPDATE merchant_orders SET status = 'EXPIRED', error_code = $2, reason = 'Payment was not completed in time',
			  updated_at = now()
			WHERE id = $1 AND status = 'UNPAID' AND payment_deadline <= now()`, orderID, entity.ErrorPaymentExpired)
		if err != nil {
			return err
		}

		if tag.RowsAffected() == 0 {
			return nil
		}

		done = true

		if closed, err = closeAttempts(ctx, tx, orderID); err != nil {
			return err
		}

		return afterOrderChange(ctx, tx, orderID, notify)
	})

	return done, closed, err
}

// CancelUnpaid cancels an order the customer has not paid. It reports whether it did.
func (s *Store) CancelUnpaid(ctx context.Context, customerID, orderID string, notify Notify) (bool, []*entity.Payment, error) {
	var (
		done   bool
		closed []*entity.Payment
	)

	err := s.inTx(ctx, func(tx txn) error {
		tag, err := tx.Exec(ctx, `
			UPDATE merchant_orders SET status = 'CANCELLED', reason = 'Cancelled by customer', updated_at = now()
			WHERE id = $1 AND customer_id = $2 AND status = 'UNPAID'`, orderID, customerID)
		if err != nil {
			return err
		}

		if tag.RowsAffected() == 0 {
			return nil
		}

		done = true

		if closed, err = closeAttempts(ctx, tx, orderID); err != nil {
			return err
		}

		return afterOrderChange(ctx, tx, orderID, notify)
	})

	return done, closed, err
}

func closeAttempts(ctx context.Context, tx txn, orderID string) ([]*entity.Payment, error) {
	rows, err := tx.Query(ctx, `
		UPDATE payments p SET status = 'CANCELLED', updated_at = now()
		WHERE p.order_id = $1 AND p.status = 'PENDING' RETURNING `+paymentColumns, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*entity.Payment

	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, rows.Err()
}

// afterOrderChange queues the push for the order's new state and wakes listeners.
func afterOrderChange(ctx context.Context, tx txn, orderID string, notify Notify) error {
	if notify != nil {
		o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+orderFrom+`WHERE o.id = $1`, orderID))
		if err != nil {
			return err
		}

		if err := queueNotification(ctx, tx, notify(o)); err != nil {
			return err
		}
	}

	_, err := tx.Exec(ctx, `SELECT pg_notify('order_updates', $1)`, orderID)

	return err
}

// -------------------------------------------------------------------- refunds

// ClaimRefunds leases paid payments that must be returned: their order will not be fulfilled,
// or they paid an order that another payment already paid.
func (s *Store) ClaimRefunds(ctx context.Context, limit int, lease time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT p.id FROM payments p JOIN merchant_orders o ON o.id = p.order_id
			WHERE p.status = 'PAID' AND (p.refund_status IS NULL OR p.refund_status = 'PENDING')
			  AND p.refund_available_at <= now() AND (p.refund_lease_until IS NULL OR p.refund_lease_until < now())
			  AND (o.status IN ('CANCELLED', 'REJECTED', 'EXPIRED', 'SUBMIT_FAILED')
			       OR EXISTS (SELECT 1 FROM payments q WHERE q.order_id = p.order_id AND q.status = 'PAID' AND q.id <> p.id
			                  AND (q.paid_at, q.id) < (p.paid_at, p.id)))
			ORDER BY p.paid_at FOR UPDATE OF p SKIP LOCKED LIMIT $1
		)
		UPDATE payments p SET refund_status = 'PENDING', refund_attempts = p.refund_attempts + 1,
		  refund_lease_until = now() + make_interval(secs => $2), updated_at = now()
		WHERE p.id IN (SELECT id FROM due) RETURNING p.id`, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim refunds: %w", err)
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

// RefundOutcome is how a refund attempt ended.
type RefundOutcome struct {
	Done  bool          // money is on its way back
	Ref   string        // the gateway's reference
	Retry time.Duration // when not done and not failed: try again after this
	Fail  bool          // the gateway refused for good: the merchant must refund by hand
	Error string
}

// FinishRefund records how a refund attempt ended and tells the customer.
func (s *Store) FinishRefund(ctx context.Context, paymentID string, out RefundOutcome, notify PaymentNotify) error {
	if len(out.Error) > 300 {
		out.Error = out.Error[:300]
	}

	return s.inTx(ctx, func(tx txn) error {
		var orderID string

		switch {
		case out.Done:
			if err := tx.QueryRow(ctx, `
				UPDATE payments SET refund_status = 'DONE', refund_ref = $2, refund_error = NULL, refunded_at = now(),
				  refund_lease_until = NULL, updated_at = now() WHERE id = $1 RETURNING order_id`, paymentID, out.Ref).Scan(&orderID); err != nil {
				return notFound(err)
			}
		case out.Fail:
			if err := tx.QueryRow(ctx, `
				UPDATE payments SET refund_status = 'FAILED', refund_error = $2, refund_lease_until = NULL, updated_at = now()
				WHERE id = $1 RETURNING order_id`, paymentID, out.Error).Scan(&orderID); err != nil {
				return notFound(err)
			}
		default:
			_, err := tx.Exec(ctx, `
				UPDATE payments SET refund_error = $2, refund_lease_until = NULL,
				  refund_available_at = now() + make_interval(secs => $3), updated_at = now() WHERE id = $1`,
				paymentID, out.Error, out.Retry.Seconds())

			return err
		}

		res := ChargeResult{}

		return finishPaymentEvent(ctx, tx, orderID, paymentID, notify, &res)
	})
}

// ------------------------------------------------------------------- the fees

// FeeStatement totals the fees of completed orders in [from, to), and what the merchant owes now.
func (s *Store) FeeStatement(ctx context.Context, merchantID string, from, to time.Time) (entity.FeeStatement, error) {
	st := entity.FeeStatement{MerchantID: merchantID, From: from, To: to, Entries: []entity.FeeEntry{}}

	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT l.order_id),
		  COALESCE(sum(l.amount_minor) FILTER (WHERE l.kind = 'CUSTOMER_FEE'), 0),
		  COALESCE(sum(l.amount_minor) FILTER (WHERE l.kind = 'MERCHANT_FEE'), 0),
		  COALESCE(sum(l.amount_minor) FILTER (WHERE l.settled_at IS NOT NULL), 0),
		  COALESCE((SELECT sum(o.total_minor) FROM merchant_orders o WHERE o.id IN
		     (SELECT order_id FROM fee_ledger WHERE merchant_id = $1 AND created_at >= $2 AND created_at < $3)), 0)
		FROM fee_ledger l WHERE l.merchant_id = $1 AND l.created_at >= $2 AND l.created_at < $3`,
		merchantID, from, to).Scan(&st.Orders, &st.CustomerFeeMinor, &st.MerchantFeeMinor, &st.SettledMinor, &st.SalesMinor); err != nil {
		return st, fmt.Errorf("fee statement: %w", err)
	}

	st.TotalOwedMinor = st.CustomerFeeMinor + st.MerchantFeeMinor

	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(sum(amount_minor), 0) FROM fee_ledger WHERE merchant_id = $1 AND settled_at IS NULL`, merchantID,
	).Scan(&st.OutstandingMinor); err != nil {
		return st, fmt.Errorf("fees outstanding: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT order_id, payment_id, kind, source, amount_minor, created_at, settled_at FROM fee_ledger
		WHERE merchant_id = $1 AND created_at >= $2 AND created_at < $3 ORDER BY created_at, kind LIMIT 1000`, merchantID, from, to)
	if err != nil {
		return st, fmt.Errorf("fee entries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var e entity.FeeEntry
		if err := rows.Scan(&e.OrderID, &e.PaymentID, &e.Kind, &e.Source, &e.AmountMinor, &e.CreatedAt, &e.SettledAt); err != nil {
			return st, err
		}

		st.Entries = append(st.Entries, e)
	}

	return st, rows.Err()
}
