package persistent

import (
	"context"
	"fmt"
	"time"
)

// The *Due methods tell a background worker when the next item that is scheduled for later
// falls due (a retry after a backoff, an expired lease, a deadline), so it can sleep until
// then instead of polling. ok is false when nothing is scheduled. A negative wait means an
// item is already due.
//
// Each follows the conditions of the claim query it belongs to; change them together.

func (s *Store) due(ctx context.Context, what, sql string, args ...any) (time.Duration, bool, error) {
	var seconds *float64

	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&seconds); err != nil {
		return 0, false, fmt.Errorf("%s due: %w", what, err)
	}

	if seconds == nil {
		return 0, false, nil
	}

	return time.Duration(*seconds * float64(time.Second)), true, nil
}

// InboxDue is the next POS event to apply or whose lease runs out (ClaimInbox).
func (s *Store) InboxDue(ctx context.Context) (time.Duration, bool, error) {
	return s.due(ctx, "inbox", `
		SELECT extract(epoch FROM (min(CASE status WHEN 'PENDING' THEN available_at ELSE lease_until END) - now()))::float8
		FROM integration_inbox WHERE status IN ('PENDING', 'PROCESSING')`)
}

// NotificationDue is the next push message to send or whose lease runs out (ClaimNotifications).
func (s *Store) NotificationDue(ctx context.Context) (time.Duration, bool, error) {
	return s.due(ctx, "notifications", `
		SELECT extract(epoch FROM (min(CASE status WHEN 'PENDING' THEN available_at ELSE lease_until END) - now()))::float8
		FROM notification_outbox WHERE status IN ('PENDING', 'PROCESSING')`)
}

// SubmitDue is the next order to send to its POS, or whose lease runs out (ClaimSubmits).
func (s *Store) SubmitDue(ctx context.Context) (time.Duration, bool, error) {
	return s.due(ctx, "submits", `
		SELECT extract(epoch FROM (min(GREATEST(available_at, COALESCE(lease_until, available_at))) - now()))::float8
		FROM merchant_orders WHERE status = 'CREATED'`)
}

// PaymentCheckDue is when the gateway should next be asked about an open payment (PendingPayments).
func (s *Store) PaymentCheckDue(ctx context.Context, after time.Duration) (time.Duration, bool, error) {
	return s.due(ctx, "payment check", `
		SELECT extract(epoch FROM (min(checked_at) + make_interval(secs => $1) - now()))::float8
		FROM payments WHERE status = 'PENDING' AND provider_charge_id IS NOT NULL`, after.Seconds())
}

// UnpaidDue is the next payment deadline of an unpaid order (ExpiredUnpaid).
func (s *Store) UnpaidDue(ctx context.Context) (time.Duration, bool, error) {
	return s.due(ctx, "unpaid", `
		SELECT extract(epoch FROM (min(payment_deadline) - now()))::float8
		FROM merchant_orders WHERE status = 'UNPAID'`)
}

// RefundDue is the next refund to start, retry or take over after a lost lease (ClaimRefunds).
func (s *Store) RefundDue(ctx context.Context) (time.Duration, bool, error) {
	return s.due(ctx, "refunds", `
		SELECT extract(epoch FROM (min(GREATEST(p.refund_available_at, COALESCE(p.refund_lease_until, p.refund_available_at))) - now()))::float8
		FROM payments p JOIN merchant_orders o ON o.id = p.order_id
		WHERE p.status = 'PAID' AND (p.refund_status IS NULL OR p.refund_status = 'PENDING')
		  AND (o.status IN ('CANCELLED', 'REJECTED', 'EXPIRED', 'SUBMIT_FAILED')
		       OR EXISTS (SELECT 1 FROM payments q WHERE q.order_id = p.order_id AND q.status = 'PAID' AND q.id <> p.id
		                  AND (q.paid_at, q.id) < (p.paid_at, p.id)))`)
}
