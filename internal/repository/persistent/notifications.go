package persistent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// ClaimNotifications leases push messages that are due.
func (s *Store) ClaimNotifications(ctx context.Context, limit int, lease time.Duration) ([]entity.Notification, string, error) {
	token := uuid.NewString()

	rows, err := s.pool.Query(ctx, `
		WITH picked AS (
			SELECT id FROM notification_outbox
			WHERE (status = 'PENDING' AND available_at <= now()) OR (status = 'PROCESSING' AND lease_until < now())
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT $1
		)
		UPDATE notification_outbox n SET status = 'PROCESSING', attempts = n.attempts + 1,
		  lease_until = now() + make_interval(secs => $2), lease_token = $3
		WHERE n.id IN (SELECT id FROM picked)
		RETURNING n.id, n.customer_id, n.order_id, n.kind, n.title, n.body, n.data, n.attempts`, limit, lease.Seconds(), token)
	if err != nil {
		return nil, "", fmt.Errorf("claim notifications: %w", err)
	}
	defer rows.Close()

	var out []entity.Notification

	for rows.Next() {
		var (
			n    entity.Notification
			data []byte
		)

		if err := rows.Scan(&n.ID, &n.CustomerID, &n.OrderID, &n.Kind, &n.Title, &n.Body, &data, &n.Attempts); err != nil {
			return nil, "", err
		}

		_ = json.Unmarshal(data, &n.Data)

		out = append(out, n)
	}

	return out, token, rows.Err()
}

// FinishNotification marks a message sent, or given up on.
func (s *Store) FinishNotification(ctx context.Context, id, token string, sent bool, reason string) error {
	status := "SENT"
	if !sent {
		status = "FAILED"
	}

	_, err := s.pool.Exec(ctx, `
		UPDATE notification_outbox SET status = $3, sent_at = CASE WHEN $3 = 'SENT' THEN now() END,
		  last_error = NULLIF(left($4, 300), ''), lease_until = NULL, lease_token = NULL
		WHERE id = $1 AND lease_token = $2`, id, token, status, reason)

	return err
}

// RetryNotification queues a message again after delay.
func (s *Store) RetryNotification(ctx context.Context, id, token, reason string, delay time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE notification_outbox SET status = 'PENDING', available_at = now() + make_interval(secs => $3),
		  last_error = left($4, 300), lease_until = NULL, lease_token = NULL
		WHERE id = $1 AND lease_token = $2`, id, token, delay.Seconds(), reason)

	return err
}

// DeviceTokens lists a customer's push tokens.
func (s *Store) DeviceTokens(ctx context.Context, customerID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT fcm_token FROM devices WHERE customer_id = $1 ORDER BY last_seen_at DESC`, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []string

	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}

		tokens = append(tokens, t)
	}

	return tokens, rows.Err()
}

// DeleteDevice forgets a push token the provider says is no longer valid.
func (s *Store) DeleteDevice(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM devices WHERE fcm_token = $1`, token)

	return err
}
