package persistent

import (
	"context"
	"fmt"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// CreateReview stores the rating of a completed order of the customer. One per order.
func (s *Store) CreateReview(ctx context.Context, customerID, orderID string, rating int, comment *string) (entity.Review, error) {
	var r entity.Review

	err := s.inTx(ctx, func(tx txn) error {
		var outletID, status string
		if err := tx.QueryRow(ctx, `SELECT outlet_id, status FROM merchant_orders WHERE id = $1 AND customer_id = $2`, orderID, customerID).
			Scan(&outletID, &status); err != nil {
			return notFound(err)
		}

		if status != entity.OrderCompleted {
			return fmt.Errorf("%w: only a completed order can be reviewed", entity.ErrBadRequest)
		}

		return tx.QueryRow(ctx, `
			INSERT INTO reviews (order_id, customer_id, outlet_id, rating, comment) VALUES ($1, $2, $3, $4, $5)
			RETURNING id, order_id, rating, comment, created_at`, orderID, customerID, outletID, rating, comment,
		).Scan(&r.ID, &r.OrderID, &r.Rating, &r.Comment, &r.CreatedAt)
	})
	if isUnique(err) {
		return entity.Review{}, entity.ErrConflict
	}

	return r, err
}

// OutletReviews lists an outlet's reviews, newest first. The reviewer shows as a first name only.
func (s *Store) OutletReviews(ctx context.Context, outletID string, before *time.Time, limit int) ([]entity.Review, error) {
	cutoff := time.Now().Add(time.Hour)
	if before != nil {
		cutoff = *before
	}

	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.rating, r.comment, COALESCE(NULLIF(split_part(c.name, ' ', 1), ''), 'Customer'), r.created_at
		FROM reviews r JOIN customers c ON c.id = r.customer_id
		WHERE r.outlet_id = $1 AND r.created_at < $2 ORDER BY r.created_at DESC LIMIT $3`, outletID, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("reviews: %w", err)
	}
	defer rows.Close()

	out := []entity.Review{}

	for rows.Next() {
		var r entity.Review
		if err := rows.Scan(&r.ID, &r.Rating, &r.Comment, &r.Reviewer, &r.CreatedAt); err != nil {
			return nil, err
		}

		out = append(out, r)
	}

	return out, rows.Err()
}

// Ratings summarizes the reviews of the given outlets.
func (s *Store) Ratings(ctx context.Context, outletIDs []string) (map[string]entity.Rating, error) {
	out := map[string]entity.Rating{}
	if len(outletIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT outlet_id::text, round(avg(rating)::numeric, 1)::float8, count(*)::int
		FROM reviews WHERE outlet_id = ANY($1::uuid[]) GROUP BY outlet_id`, outletIDs)
	if err != nil {
		return nil, fmt.Errorf("ratings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id string
			r  entity.Rating
		)

		if err := rows.Scan(&id, &r.Average, &r.Count); err != nil {
			return nil, err
		}

		out[id] = r
	}

	return out, rows.Err()
}
