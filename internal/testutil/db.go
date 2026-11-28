// Package testutil holds helpers for tests that need a database or a POS.
package testutil

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Calteras/streetfood-customer-api/pkg/migrator"
	"github.com/Calteras/streetfood-customer-api/pkg/postgres"
)

func init() {
	// Tests run in UTC, like the service (internal/app).
	time.Local = time.UTC
}

var (
	migrateOnce sync.Once
	migrateErr  error
)

// DB returns a pool on the database named by TEST_PG_URL with the migrations applied and
// every table emptied. Tests are skipped when TEST_PG_URL is not set. Run packages one at
// a time (go test -p 1): they share the database.
func DB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, _ := DBWithCommits(t)

	return pool
}

// DBWithCommits is DB plus the tracer that reports this pool's commits, as the service wires it:
// set its OnWrite and OnNotify to hear them.
func DBWithCommits(t *testing.T) (*pgxpool.Pool, *postgres.CommitTracer) {
	t.Helper()

	url := os.Getenv("TEST_PG_URL")
	if url == "" {
		t.Skip("TEST_PG_URL is not set")
	}

	migrateOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		dir := filepath.Join(filepath.Dir(file), "..", "..", "migrations")

		m, err := migrator.Open(dir, url, t.Logf)
		if err != nil {
			migrateErr = err

			return
		}
		defer m.Close()

		_, migrateErr = migrator.Up(m)
	})

	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	tracer := &postgres.CommitTracer{}
	cfg.ConnConfig.Tracer = tracer

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(pool.Close)

	_, err = pool.Exec(context.Background(), `
		TRUNCATE integration_inbox, menu_items, outlet_menus, outlet_hours, outlets,
		  merchant_links, merchants, devices, external_identities, customers,
		  checkouts, merchant_orders, merchant_order_items, notification_outbox, reviews, trust_config CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}

	return pool, tracer
}
