package persistent

import (
	"context"
	"fmt"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

const customerCols = `c.id, c.phone, c.name, c.avatar_url, c.status, c.created_at`

// FindOrCreateCustomer returns the customer of a sign-in identity, creating it on first
// sight. Two requests racing on a first sign-in end up with one customer.
func (s *Store) FindOrCreateCustomer(ctx context.Context, provider, providerUserID, phone, name string) (entity.Customer, error) {
	var c entity.Customer

	find := func(tx txn) error {
		return tx.QueryRow(ctx, `
			SELECT `+customerCols+` FROM customers c
			JOIN external_identities e ON e.customer_id = c.id
			WHERE e.provider = $1 AND e.provider_user_id = $2`, provider, providerUserID,
		).Scan(&c.ID, &c.Phone, &c.Name, &c.AvatarURL, &c.Status, &c.CreatedAt)
	}

	err := s.inTx(ctx, func(tx txn) error {
		// One first sign-in at a time per identity.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "identity:"+provider+":"+providerUserID); err != nil {
			return err
		}

		if err := find(tx); err == nil {
			return nil
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO customers (phone, name) VALUES (NULLIF($1, ''), $2)
			RETURNING `+`id, phone, name, avatar_url, status, created_at`, phone, name,
		).Scan(&c.ID, &c.Phone, &c.Name, &c.AvatarURL, &c.Status, &c.CreatedAt); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO external_identities (provider, provider_user_id, customer_id) VALUES ($1, $2, $3)`,
			provider, providerUserID, c.ID)

		return err
	})
	if err != nil {
		return entity.Customer{}, fmt.Errorf("find or create customer: %w", err)
	}

	return c, nil
}

// GetCustomer returns a customer by id.
func (s *Store) GetCustomer(ctx context.Context, id string) (entity.Customer, error) {
	var c entity.Customer

	err := s.pool.QueryRow(ctx, `SELECT `+customerCols+` FROM customers c WHERE c.id = $1`, id).Scan(&c.ID, &c.Phone, &c.Name, &c.AvatarURL, &c.Status, &c.CreatedAt)
	if err != nil {
		return entity.Customer{}, notFound(err)
	}

	return c, nil
}

// UpdateCustomer changes the profile fields that were given.
func (s *Store) UpdateCustomer(ctx context.Context, id string, name, avatarURL *string) (entity.Customer, error) {
	var c entity.Customer

	err := s.pool.QueryRow(ctx, `
		UPDATE customers c SET name = COALESCE($2, c.name), avatar_url = COALESCE($3, c.avatar_url), updated_at = now()
		WHERE c.id = $1 AND c.status = 'active'
		RETURNING `+customerCols, id, name, avatarURL,
	).Scan(&c.ID, &c.Phone, &c.Name, &c.AvatarURL, &c.Status, &c.CreatedAt)
	if err != nil {
		return entity.Customer{}, notFound(err)
	}

	return c, nil
}

// DeleteCustomer erases the personal data and closes the account. The sign-in identity
// stays, so the same person is not silently given a new account.
func (s *Store) DeleteCustomer(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx txn) error {
		tag, err := tx.Exec(ctx, `
			UPDATE customers SET status = 'deleted', phone = NULL, name = '', avatar_url = NULL,
			  deleted_at = now(), updated_at = now()
			WHERE id = $1 AND status <> 'deleted'`, id)
		if err != nil {
			return err
		}

		if tag.RowsAffected() == 0 {
			return entity.ErrNotFound
		}

		_, err = tx.Exec(ctx, `DELETE FROM devices WHERE customer_id = $1`, id)

		return err
	})
}

// UpsertDevice registers or refreshes a push token. A token moves to whoever registers it last.
func (s *Store) UpsertDevice(ctx context.Context, customerID, platform, token string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO devices (customer_id, platform, fcm_token) VALUES ($1, $2, $3)
		ON CONFLICT (fcm_token) DO UPDATE SET customer_id = $1, platform = $2, last_seen_at = now()`,
		customerID, platform, token)

	return err
}
