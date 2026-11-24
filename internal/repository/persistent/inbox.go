package persistent

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// InsertInbox stores received events. A redelivered event (same key and id) is ignored;
// the number of new events is returned.
func (s *Store) InsertInbox(ctx context.Context, keyID string, events []entity.InboxEvent) (int, error) {
	batch := &pgx.Batch{}

	for _, e := range events {
		batch.Queue(`
			INSERT INTO integration_inbox (key_id, event_id, type, outlet_id, version, occurred_at, payload)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (key_id, event_id) DO NOTHING`,
			keyID, e.EventID, e.Type, e.OutletID, e.Version, e.OccurredAt, []byte(e.Payload))
	}

	results := s.pool.SendBatch(ctx, batch)
	defer results.Close()

	inserted := 0

	for range events {
		tag, err := results.Exec()
		if err != nil {
			return 0, fmt.Errorf("insert inbox: %w", err)
		}

		inserted += int(tag.RowsAffected())
	}

	return inserted, nil
}

// ClaimInbox takes up to limit due events under a lease so other workers skip them.
// Events of one outlet are handed out oldest first.
func (s *Store) ClaimInbox(ctx context.Context, limit int, lease time.Duration) ([]entity.InboxRow, string, error) {
	token := uuid.NewString()

	rows, err := s.pool.Query(ctx, `
		WITH picked AS (
			SELECT id FROM integration_inbox
			WHERE (status = 'PENDING' AND available_at <= now())
			   OR (status = 'PROCESSING' AND lease_until < now())
			ORDER BY occurred_at, received_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE integration_inbox i
		SET status = 'PROCESSING', attempts = i.attempts + 1,
		    lease_until = now() + make_interval(secs => $2), lease_token = $3
		WHERE i.id IN (SELECT id FROM picked)
		RETURNING i.id, i.key_id, i.event_id, i.type, i.outlet_id, i.version, i.occurred_at, i.payload, i.attempts`,
		limit, lease.Seconds(), token)
	if err != nil {
		return nil, "", fmt.Errorf("claim inbox: %w", err)
	}
	defer rows.Close()

	var out []entity.InboxRow

	for rows.Next() {
		var r entity.InboxRow
		if err := rows.Scan(&r.ID, &r.KeyID, &r.EventID, &r.Type, &r.OutletID, &r.Version, &r.OccurredAt, &r.Payload, &r.Attempts); err != nil {
			return nil, "", fmt.Errorf("scan inbox: %w", err)
		}

		out = append(out, r)
	}

	return out, token, rows.Err()
}

// FinishInbox marks an event applied. A worker whose lease expired changes nothing.
func (s *Store) FinishInbox(ctx context.Context, id, token string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE integration_inbox
		SET status = 'DONE', processed_at = now(), lease_until = NULL, lease_token = NULL, last_error = NULL
		WHERE id = $1 AND lease_token = $2`, id, token)

	return err
}

// RetryInbox puts a failed event back after delay, or gives up on it.
func (s *Store) RetryInbox(ctx context.Context, id, token, message string, delay time.Duration, giveUp bool) error {
	status := "PENDING"
	if giveUp {
		status = "FAILED"
	}

	_, err := s.pool.Exec(ctx, `
		UPDATE integration_inbox
		SET status = $3, available_at = now() + make_interval(secs => $4),
		    lease_until = NULL, lease_token = NULL, last_error = left($5, 500)
		WHERE id = $1 AND lease_token = $2`, id, token, status, delay.Seconds(), message)

	return err
}
