package persistent

import (
	"context"
	"fmt"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// CreateMerchant stores a merchant and its POS link together.
func (s *Store) CreateMerchant(ctx context.Context, id, name, posTenantID, keyID, secretEnc, baseURL string) (entity.Merchant, error) {
	var m entity.Merchant

	err := s.inTx(ctx, func(tx txn) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO merchants (id, pos_tenant_id, name) VALUES ($1, $2, $3)
			RETURNING id, pos_tenant_id, name, status, created_at`, id, posTenantID, name,
		).Scan(&m.ID, &m.POSTenantID, &m.Name, &m.Status, &m.CreatedAt); err != nil {
			return err
		}

		return tx.QueryRow(ctx, `
			INSERT INTO merchant_links (merchant_id, key_id, secret_enc, pos_base_url) VALUES ($1, $2, $3, $4)
			RETURNING key_id, pos_base_url, status`, m.ID, keyID, secretEnc, baseURL,
		).Scan(&m.KeyID, &m.POSBaseURL, &m.LinkStatus)
	})
	if isUnique(err) {
		return entity.Merchant{}, entity.ErrConflict
	}

	if err != nil {
		return entity.Merchant{}, fmt.Errorf("create merchant: %w", err)
	}

	return m, nil
}

// ListMerchants returns every merchant with its link status (never the secret).
func (s *Store) ListMerchants(ctx context.Context) ([]entity.Merchant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.pos_tenant_id, m.name, m.status, l.key_id, l.pos_base_url, l.status, m.created_at
		FROM merchants m JOIN merchant_links l ON l.merchant_id = m.id
		ORDER BY m.created_at`)
	if err != nil {
		return nil, fmt.Errorf("list merchants: %w", err)
	}
	defer rows.Close()

	out := []entity.Merchant{}

	for rows.Next() {
		var m entity.Merchant
		if err := rows.Scan(&m.ID, &m.POSTenantID, &m.Name, &m.Status, &m.KeyID, &m.POSBaseURL, &m.LinkStatus, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan merchant: %w", err)
		}

		out = append(out, m)
	}

	return out, rows.Err()
}

const linkCols = `m.id, m.pos_tenant_id, l.key_id, l.secret_enc, l.pos_base_url, l.status`

// LinkByKeyID finds the link a signed request names. Suspended merchants have no usable link.
func (s *Store) LinkByKeyID(ctx context.Context, keyID string) (entity.Link, error) {
	var l entity.Link

	err := s.pool.QueryRow(ctx, `
		SELECT `+linkCols+` FROM merchant_links l JOIN merchants m ON m.id = l.merchant_id
		WHERE l.key_id = $1 AND l.status = 'active' AND m.status = 'active'`, keyID,
	).Scan(&l.MerchantID, &l.POSTenantID, &l.KeyID, &l.SecretEnc, &l.BaseURL, &l.Status)
	if err != nil {
		return entity.Link{}, notFound(err)
	}

	return l, nil
}

// LinkByMerchant finds a merchant's link in any state.
func (s *Store) LinkByMerchant(ctx context.Context, merchantID string) (entity.Link, error) {
	var l entity.Link

	err := s.pool.QueryRow(ctx, `
		SELECT `+linkCols+` FROM merchant_links l JOIN merchants m ON m.id = l.merchant_id
		WHERE m.id = $1`, merchantID,
	).Scan(&l.MerchantID, &l.POSTenantID, &l.KeyID, &l.SecretEnc, &l.BaseURL, &l.Status)
	if err != nil {
		return entity.Link{}, notFound(err)
	}

	return l, nil
}

// ActiveLinks lists the links to reconcile.
func (s *Store) ActiveLinks(ctx context.Context) ([]entity.Link, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+linkCols+` FROM merchant_links l JOIN merchants m ON m.id = l.merchant_id
		WHERE l.status = 'active' AND m.status = 'active' ORDER BY m.created_at`)
	if err != nil {
		return nil, fmt.Errorf("active links: %w", err)
	}
	defer rows.Close()

	var out []entity.Link

	for rows.Next() {
		var l entity.Link
		if err := rows.Scan(&l.MerchantID, &l.POSTenantID, &l.KeyID, &l.SecretEnc, &l.BaseURL, &l.Status); err != nil {
			return nil, fmt.Errorf("scan link: %w", err)
		}

		out = append(out, l)
	}

	return out, rows.Err()
}

// Suspend stops a merchant: its link is revoked and its outlets disappear from search.
func (s *Store) Suspend(ctx context.Context, merchantID string) error {
	return s.inTx(ctx, func(tx txn) error {
		tag, err := tx.Exec(ctx, `UPDATE merchants SET status = 'suspended', updated_at = now() WHERE id = $1`, merchantID)
		if err != nil {
			return err
		}

		if tag.RowsAffected() == 0 {
			return entity.ErrNotFound
		}

		if _, err := tx.Exec(ctx, `UPDATE merchant_links SET status = 'revoked', updated_at = now() WHERE merchant_id = $1`, merchantID); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `UPDATE outlets SET published = false, updated_at = now() WHERE merchant_id = $1`, merchantID)

		return err
	})
}
