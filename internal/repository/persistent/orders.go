package persistent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

const orderColumns = `
	o.id, o.checkout_id, o.customer_id, o.merchant_id, o.outlet_id, ou.name, o.platform_order_id,
	o.pos_order_id, o.status, o.pos_version, o.fulfillment_type, o.fulfillment_label, o.notes,
	o.payment_mode, o.subtotal_minor, o.tax_minor, o.service_minor, o.total_minor,
	o.expected_total_minor, o.accept_by, o.prep_time_minutes, o.reason, o.error_code, o.error_detail,
	o.submit_attempts, o.created_at, o.updated_at, o.completed_at, o.payment_deadline`

const orderFrom = ` FROM merchant_orders o JOIN outlets ou ON ou.id = o.outlet_id `

func scanOrder(row pgx.Row) (*entity.Order, error) {
	var o entity.Order

	err := row.Scan(
		&o.ID, &o.CheckoutID, &o.CustomerID, &o.MerchantID, &o.OutletID, &o.OutletName, &o.PlatformOrderID,
		&o.POSOrderID, &o.Status, &o.POSVersion, &o.FulfillmentType, &o.FulfillmentLbl, &o.Notes,
		&o.PaymentMode, &o.SubtotalMinor, &o.TaxMinor, &o.ServiceMinor, &o.TotalMinor,
		&o.ExpectedTotal, &o.AcceptBy, &o.PrepMinutes, &o.Reason, &o.ErrorCode, &o.ErrorDetail,
		&o.SubmitAttempts, &o.CreatedAt, &o.UpdatedAt, &o.CompletedAt, &o.PaymentDeadline)
	if err != nil {
		return nil, err
	}

	return &o, nil
}

// loadItems fills the lines of the given orders.
func (s *Store) loadItems(ctx context.Context, orders ...*entity.Order) error {
	if len(orders) == 0 {
		return nil
	}

	byID := make(map[string]*entity.Order, len(orders))
	ids := make([]string, 0, len(orders))

	for _, o := range orders {
		byID[o.ID] = o
		o.Items = []entity.OrderItem{}

		ids = append(ids, o.ID)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT order_id, line_no, product_id, variant_id, modifier_ids, quantity, notes, name, variant_name
		FROM merchant_order_items WHERE order_id = ANY($1::uuid[]) ORDER BY order_id, line_no`, ids)
	if err != nil {
		return fmt.Errorf("order items: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			orderID string
			it      entity.OrderItem
			mods    []byte
		)

		if err := rows.Scan(&orderID, &it.LineNo, &it.ProductID, &it.VariantID, &mods, &it.Quantity, &it.Notes, &it.Name, &it.VariantName); err != nil {
			return fmt.Errorf("scan item: %w", err)
		}

		if err := json.Unmarshal(mods, &it.ModifierIDs); err != nil {
			return fmt.Errorf("decode modifiers: %w", err)
		}

		byID[orderID].Items = append(byID[orderID].Items, it)
	}

	return rows.Err()
}

// loadDetails fills the lines and latest payment of the given orders.
func (s *Store) loadDetails(ctx context.Context, orders ...*entity.Order) error {
	if err := s.loadItems(ctx, orders...); err != nil {
		return err
	}

	return s.loadPayments(ctx, orders...)
}

// CheckoutInput is a validated checkout ready to store.
type CheckoutInput struct {
	CustomerID         string
	IdempotencyKey     string
	RequestHash        string
	MerchantID         string
	OutletID           string
	PlatformOrderID    string
	FulfillmentType    string
	FulfillmentLabel   *string
	Notes              *string
	ExpectedTotalMinor *int64
	Items              []entity.OrderItem
	// Online is an order paid in the app: it starts UNPAID with the POS's price (Quote) and a
	// deadline, and goes to the POS only once paid.
	// MerchantFeeBps is the merchant fee rate in force when the order is placed.
	MerchantFeeBps  int
	Online          bool
	PaymentDeadline time.Time
	Quote           *entity.Pricing
}

// CreateCheckout stores a checkout and its order. Repeating the same key and request returns
// the stored order (created is false); the same key with another request is ErrIdempotency.
// A customer with too many open orders, or too many checkouts this hour, is ErrTooMany.
func (s *Store) CreateCheckout(ctx context.Context, in CheckoutInput) (*entity.Order, bool, error) {
	var (
		orderID string
		created bool
	)

	err := s.inTx(ctx, func(tx txn) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "checkout:"+in.CustomerID); err != nil {
			return err
		}

		var (
			checkoutID string
			hash       string
		)

		switch err := tx.QueryRow(ctx,
			`SELECT id, request_hash FROM checkouts WHERE customer_id = $1 AND idempotency_key = $2`,
			in.CustomerID, in.IdempotencyKey).Scan(&checkoutID, &hash); {
		case err == nil:
			if hash != in.RequestHash {
				return entity.ErrIdempotency
			}

			return tx.QueryRow(ctx, `SELECT id FROM merchant_orders WHERE checkout_id = $1 ORDER BY created_at LIMIT 1`, checkoutID).Scan(&orderID)
		case err != pgx.ErrNoRows:
			return err
		}

		var open, recent int
		if err := tx.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM merchant_orders WHERE customer_id = $1
			     AND status IN ('UNPAID','CREATED','PENDING_ACCEPTANCE','ACCEPTED','PREPARING','READY')),
			  (SELECT count(*) FROM checkouts WHERE customer_id = $1 AND created_at > now() - interval '1 hour')`,
			in.CustomerID).Scan(&open, &recent); err != nil {
			return err
		}

		if open >= entity.MaxOpenOrders || recent >= entity.MaxCheckoutsPerHour {
			return entity.ErrTooMany
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO checkouts (customer_id, idempotency_key, request_hash) VALUES ($1, $2, $3) RETURNING id`,
			in.CustomerID, in.IdempotencyKey, in.RequestHash).Scan(&checkoutID); err != nil {
			return err
		}

		status, mode := entity.OrderCreated, entity.PaymentModePayAtPick

		var (
			deadline                      *time.Time
			subtotal, tax, service, total *int64
		)

		if in.Online {
			status, mode = entity.OrderUnpaid, entity.PaymentModeOnline
			deadline = &in.PaymentDeadline
			subtotal, tax, service, total = &in.Quote.SubtotalMinor, &in.Quote.TaxMinor, &in.Quote.ServiceMinor, &in.Quote.TotalMinor
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO merchant_orders (checkout_id, customer_id, merchant_id, outlet_id, platform_order_id,
			  fulfillment_type, fulfillment_label, notes, expected_total_minor,
			  status, payment_mode, payment_deadline, subtotal_minor, tax_minor, service_minor, total_minor, merchant_fee_bps)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17) RETURNING id`,
			checkoutID, in.CustomerID, in.MerchantID, in.OutletID, in.PlatformOrderID,
			in.FulfillmentType, in.FulfillmentLabel, in.Notes, in.ExpectedTotalMinor,
			status, mode, deadline, subtotal, tax, service, total, in.MerchantFeeBps).Scan(&orderID); err != nil {
			return err
		}

		for _, it := range in.Items {
			mods, err := json.Marshal(it.ModifierIDs)
			if err != nil {
				return err
			}

			if _, err := tx.Exec(ctx, `
				INSERT INTO merchant_order_items (order_id, line_no, product_id, variant_id, modifier_ids, quantity, notes, name, variant_name)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				orderID, it.LineNo, it.ProductID, it.VariantID, mods, it.Quantity, it.Notes, it.Name, it.VariantName); err != nil {
				return err
			}
		}

		created = true

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	o, err := s.OrderByID(ctx, orderID)

	return o, created, err
}

// ExistingCheckout returns the order of an earlier checkout with the same key, or nil if there is
// none. The same key with another request is ErrIdempotency.
func (s *Store) ExistingCheckout(ctx context.Context, customerID, key, requestHash string) (*entity.Order, error) {
	var (
		checkoutID string
		hash       string
	)

	switch err := s.pool.QueryRow(ctx,
		`SELECT id, request_hash FROM checkouts WHERE customer_id = $1 AND idempotency_key = $2`, customerID, key).Scan(&checkoutID, &hash); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil //nolint:nilnil // no earlier checkout is a normal answer
	case err != nil:
		return nil, err
	}

	if hash != requestHash {
		return nil, entity.ErrIdempotency
	}

	var orderID string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM merchant_orders WHERE checkout_id = $1 ORDER BY created_at LIMIT 1`, checkoutID).Scan(&orderID); err != nil {
		return nil, notFound(err)
	}

	return s.OrderByID(ctx, orderID)
}

// OrderByID returns an order with its lines.
func (s *Store) OrderByID(ctx context.Context, id string) (*entity.Order, error) {
	o, err := scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderColumns+orderFrom+`WHERE o.id = $1`, id))
	if err != nil {
		return nil, notFound(err)
	}

	return o, s.loadDetails(ctx, o)
}

// CustomerOrder returns an order only if it belongs to the customer.
func (s *Store) CustomerOrder(ctx context.Context, customerID, id string) (*entity.Order, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, entity.ErrNotFound
	}

	o, err := scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderColumns+orderFrom+`WHERE o.id = $1 AND o.customer_id = $2`, id, customerID))
	if err != nil {
		return nil, notFound(err)
	}

	return o, s.loadDetails(ctx, o)
}

// ListCustomerOrders returns a customer's orders, newest first. before pages backwards.
func (s *Store) ListCustomerOrders(ctx context.Context, customerID string, before *time.Time, limit int) ([]*entity.Order, error) {
	cutoff := time.Now().Add(time.Hour)
	if before != nil {
		cutoff = *before
	}

	rows, err := s.pool.Query(ctx, `SELECT `+orderColumns+orderFrom+`
		WHERE o.customer_id = $1 AND o.created_at < $2 ORDER BY o.created_at DESC LIMIT $3`, customerID, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()

	var orders []*entity.Order

	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}

		orders = append(orders, o)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return orders, s.loadDetails(ctx, orders...)
}

// SubmitData is what is needed to send an order to its POS.
type SubmitData struct {
	Order        *entity.Order
	POSOutletID  string
	CustomerName string
	Phone        *string
}

// SubmitDataFor loads an order with the details the POS needs.
func (s *Store) SubmitDataFor(ctx context.Context, orderID string) (SubmitData, error) {
	o, err := s.OrderByID(ctx, orderID)
	if err != nil {
		return SubmitData{}, err
	}

	d := SubmitData{Order: o}

	err = s.pool.QueryRow(ctx, `
		SELECT ou.pos_outlet_id, c.name, c.phone FROM outlets ou, customers c
		WHERE ou.id = $1 AND c.id = $2`, o.OutletID, o.CustomerID).Scan(&d.POSOutletID, &d.CustomerName, &d.Phone)

	return d, notFound(err)
}

// ClaimSubmits leases orders still to be sent to their POS.
func (s *Store) ClaimSubmits(ctx context.Context, limit int, lease time.Duration) ([]string, string, error) {
	token := uuid.NewString()

	rows, err := s.pool.Query(ctx, `
		WITH picked AS (
			SELECT id FROM merchant_orders
			WHERE status = 'CREATED' AND available_at <= now() AND (lease_until IS NULL OR lease_until < now())
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT $1
		)
		UPDATE merchant_orders o SET submit_attempts = o.submit_attempts + 1,
		  lease_until = now() + make_interval(secs => $2), lease_token = $3
		WHERE o.id IN (SELECT id FROM picked) RETURNING o.id`, limit, lease.Seconds(), token)
	if err != nil {
		return nil, "", fmt.Errorf("claim submits: %w", err)
	}
	defer rows.Close()

	var ids []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, "", err
		}

		ids = append(ids, id)
	}

	return ids, token, rows.Err()
}

// RetrySubmit puts an order back in the queue after delay.
func (s *Store) RetrySubmit(ctx context.Context, id string, delay time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE merchant_orders SET available_at = now() + make_interval(secs => $2), lease_until = NULL, lease_token = NULL
		WHERE id = $1 AND status = 'CREATED'`, id, delay.Seconds())

	return err
}

// FailSubmit gives up on an order that never reached the POS. It reports whether it did:
// false means the order had already moved on (cancelled by the customer, or accepted).
func (s *Store) FailSubmit(ctx context.Context, id, code string, detail json.RawMessage) (bool, error) {
	var d any
	if len(detail) > 0 {
		d = []byte(detail)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE merchant_orders SET status = 'SUBMIT_FAILED', error_code = $2, error_detail = $3,
		  lease_until = NULL, lease_token = NULL, updated_at = now()
		WHERE id = $1 AND status = 'CREATED'`, id, code, d)
	if err != nil {
		return false, err
	}

	if tag.RowsAffected() == 1 {
		_, err = s.pool.Exec(ctx, `SELECT pg_notify('order_updates', $1)`, id)
	}

	return tag.RowsAffected() == 1, err
}

// CancelCreated cancels an order the POS has not been told about yet. It reports whether
// it did: false means the order had been sent meanwhile.
func (s *Store) CancelCreated(ctx context.Context, customerID, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE merchant_orders SET status = 'CANCELLED', reason = 'Cancelled by customer',
		  lease_until = NULL, lease_token = NULL, updated_at = now()
		WHERE id = $1 AND customer_id = $2 AND status = 'CREATED'`, id, customerID)
	if err != nil {
		return false, err
	}

	if tag.RowsAffected() == 1 {
		_, err = s.pool.Exec(ctx, `SELECT pg_notify('order_updates', $1)`, id)
	}

	return tag.RowsAffected() == 1, err
}

// Notify builds the push message for an order that just changed, or nil for none.
type Notify func(o *entity.Order) *entity.Notification

// ApplyPOSStatus records what the POS says about an order, but only if it is newer than what
// is held (versions only grow). It reports whether anything changed. Subscribers are woken and
// the push message is queued in the same transaction.
func (s *Store) ApplyPOSStatus(ctx context.Context, platformOrderID string, st entity.POSOrderState, notify Notify) (*entity.Order, bool, error) {
	var (
		id      string
		changed bool
	)

	err := s.inTx(ctx, func(tx txn) error {
		var version int
		if err := tx.QueryRow(ctx,
			`SELECT id, pos_version FROM merchant_orders WHERE platform_order_id = $1 FOR UPDATE`, platformOrderID,
		).Scan(&id, &version); err != nil {
			return notFound(err)
		}

		var err error

		changed, err = applyLocked(ctx, tx, id, version, st, notify)

		return err
	})
	if err != nil {
		return nil, false, err
	}

	o, err := s.OrderByID(ctx, id)

	return o, changed, err
}

// MarkSubmitted records the POS's answer to a submitted order. It reports whether it did:
// false means the order had left CREATED meanwhile (the customer cancelled it while it was
// being sent), and the caller must undo it at the POS.
func (s *Store) MarkSubmitted(ctx context.Context, id string, st entity.POSOrderState, notify Notify) (bool, error) {
	var applied bool

	err := s.inTx(ctx, func(tx txn) error {
		var (
			status  string
			version int
		)

		if err := tx.QueryRow(ctx, `SELECT status, pos_version FROM merchant_orders WHERE id = $1 FOR UPDATE`, id).
			Scan(&status, &version); err != nil {
			return notFound(err)
		}

		if status != entity.OrderCreated {
			return nil
		}

		applied = true
		_, err := applyLocked(ctx, tx, id, version, st, notify)

		return err
	})

	return applied, err
}

// applyLocked updates a locked order row if st is newer, queues its push message and wakes
// listeners.
func applyLocked(ctx context.Context, tx txn, id string, version int, st entity.POSOrderState, notify Notify) (bool, error) {
	if st.Version <= version {
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE merchant_orders SET
		  status = $2, pos_version = $3,
		  pos_order_id = COALESCE(NULLIF($4, ''), pos_order_id),
		  subtotal_minor = COALESCE($5, subtotal_minor), tax_minor = COALESCE($6, tax_minor),
		  service_minor = COALESCE($7, service_minor), total_minor = COALESCE($8, total_minor),
		  accept_by = COALESCE($9, accept_by), prep_time_minutes = COALESCE($10, prep_time_minutes),
		  reason = $11, completed_at = CASE WHEN $2 = 'COMPLETED' THEN now() ELSE completed_at END,
		  error_code = NULL, error_detail = NULL, lease_until = NULL, lease_token = NULL,
		  checked_at = now(), updated_at = now()
		WHERE id = $1`,
		id, st.Status, st.Version, st.POSOrderID, st.Subtotal, st.Tax, st.Service, st.Total,
		st.AcceptBy, st.PrepMinutes, st.Reason); err != nil {
		return false, err
	}

	if st.Status == entity.OrderCompleted {
		// The platform's fees fall due when an order is fulfilled. An online order owes both fees,
		// from the payment that was made. A pay-at-pickup order owes the merchant fee only, at the
		// rate in force when it was placed.
		if _, err := tx.Exec(ctx, `
			INSERT INTO fee_ledger (merchant_id, order_id, payment_id, kind, amount_minor, source)
			SELECT o.merchant_id, o.id, p.id, k.kind, k.amount, 'ONLINE'
			FROM merchant_orders o
			JOIN payments p ON p.id = (SELECT id FROM payments WHERE order_id = o.id AND status = 'PAID' ORDER BY paid_at, id LIMIT 1),
			LATERAL (VALUES ('CUSTOMER_FEE', p.customer_fee_minor), ('MERCHANT_FEE', p.merchant_fee_minor)) AS k(kind, amount)
			WHERE o.id = $1
			ON CONFLICT (order_id, kind) DO NOTHING`, id); err != nil {
			return false, err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO fee_ledger (merchant_id, order_id, payment_id, kind, amount_minor, source)
			SELECT o.merchant_id, o.id, NULL, 'MERCHANT_FEE', ((o.total_minor * o.merchant_fee_bps / 10000 + 50) / 100) * 100, 'PICKUP'
			FROM merchant_orders o
			WHERE o.id = $1 AND o.payment_mode = 'PAY_AT_PICKUP' AND o.total_minor IS NOT NULL AND o.merchant_fee_bps > 0
			  AND ((o.total_minor * o.merchant_fee_bps / 10000 + 50) / 100) * 100 > 0
			ON CONFLICT (order_id, kind) DO NOTHING`, id); err != nil {
			return false, err
		}
	}

	if notify != nil {
		o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+orderFrom+`WHERE o.id = $1`, id))
		if err != nil {
			return false, err
		}

		if n := notify(o); n != nil {
			data, err := json.Marshal(n.Data)
			if err != nil {
				return false, err
			}

			if _, err := tx.Exec(ctx, `
				INSERT INTO notification_outbox (customer_id, order_id, kind, title, body, data)
				VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (order_id, kind) DO NOTHING`,
				n.CustomerID, n.OrderID, n.Kind, n.Title, n.Body, data); err != nil {
				return false, err
			}
		}
	}

	_, err := tx.Exec(ctx, `SELECT pg_notify('order_updates', $1)`, id)

	return true, err
}

// StaleOpen lists open orders the POS has not been asked about for a while.
func (s *Store) StaleOpen(ctx context.Context, olderThan time.Duration, limit int) ([]*entity.Order, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+orderColumns+orderFrom+`
		WHERE o.status IN ('PENDING_ACCEPTANCE','ACCEPTED','PREPARING','READY')
		  AND o.checked_at < now() - make_interval(secs => $1)
		ORDER BY o.checked_at LIMIT $2`, olderThan.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("stale orders: %w", err)
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

// TouchChecked remembers that the POS was asked about an order.
func (s *Store) TouchChecked(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE merchant_orders SET checked_at = now() WHERE id = $1`, id)

	return err
}

// ListenOrderUpdates calls fn with the id of every order that changes, on any instance,
// until ctx ends or the connection fails.
func (s *Store) ListenOrderUpdates(ctx context.Context, fn func(orderID string)) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `LISTEN order_updates`); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}

		fn(n.Payload)
	}
}
