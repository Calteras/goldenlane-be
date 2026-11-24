// Package persistent is the PostgreSQL storage of the platform.
package persistent

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// txn is the transaction handle the storage methods share.
type txn = pgx.Tx

// Store is the storage of all domains. Its methods group by file.
type Store struct{ pool *pgxpool.Pool }

// New wraps a connection pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const _uniqueViolation = "23505"

// isUnique reports a unique-constraint violation.
func isUnique(err error) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == _uniqueViolation
}

// notFound turns "no rows" into the shared sentinel.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return entity.ErrNotFound
	}

	return err
}

// inTx runs fn in a transaction and rolls back on any error.
func (s *Store) inTx(ctx context.Context, fn func(tx txn) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	return nil
}
