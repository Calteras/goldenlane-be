// Package migrator applies the SQL migrations in migrations/ with golang-migrate. The
// migration image (cmd/migrate) uses it for deployed stages, and `make run` (built with
// -tags migrate) before starting the app locally.
package migrator

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"

	// Drivers: Postgres database, migrations read from files.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

const (
	_defaultAttempts = 20
	_defaultInterval = time.Second
)

// Open connects to the database at databaseURL and reads the migrations in dir, retrying
// while the database comes up. logf reports each retry. Close the result when done.
func Open(dir, databaseURL string, logf func(format string, args ...any)) (*migrate.Migrate, error) {
	var (
		m   *migrate.Migrate
		err error
	)

	for attempt := _defaultAttempts; attempt > 0; attempt-- {
		m, err = migrate.New("file://"+dir, withSSLMode(databaseURL))
		if err == nil {
			return m, nil
		}

		if attempt > 1 {
			logf("migrate: waiting for the database, %d attempts left", attempt-1)
			time.Sleep(_defaultInterval)
		}
	}

	return nil, fmt.Errorf("cannot open the database or the migrations in %s: %s", dir, redact(err, databaseURL))
}

// Up applies every pending migration and reports whether there were any.
func Up(m *migrate.Migrate) (bool, error) {
	err := m.Up()
	if errors.Is(err, migrate.ErrNoChange) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("up: %w", err)
	}

	return true, nil
}

// redact hides the database password, which some driver errors quote with the URL.
func redact(err error, databaseURL string) string {
	msg := err.Error()

	if u, parseErr := url.Parse(databaseURL); parseErr == nil {
		if password, ok := u.User.Password(); ok && password != "" {
			msg = strings.ReplaceAll(msg, password, "xxxxx")
		}
	}

	return msg
}

// withSSLMode defaults to plaintext for a local Postgres container, but keeps an explicit
// sslmode (Neon URLs carry sslmode=require) and other query parameters.
func withSSLMode(databaseURL string) string {
	if strings.Contains(databaseURL, "sslmode=") {
		return databaseURL
	}

	if strings.Contains(databaseURL, "?") {
		return databaseURL + "&sslmode=disable"
	}

	return databaseURL + "?sslmode=disable"
}
