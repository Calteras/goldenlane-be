// Command migrate applies the database migrations. It is the migration image's entrypoint
// (Dockerfile target "migration"), which every release runs before starting the app:
//
//	sh scripts/compose.sh staging run --rm migrate                  # apply pending migrations
//	sh scripts/compose.sh staging run --rm migrate version          # current version
//	sh scripts/compose.sh staging run --rm migrate force <version>  # after fixing a failed one
//
// It reads PG_URL the way the app does (config.LoadEnv); in a container that is the
// mounted migration secrets file, which holds nothing else.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"

	"github.com/Calteras/streetfood-customer-api/config"
	"github.com/Calteras/streetfood-customer-api/pkg/migrator"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("path", "migrations", "directory of the SQL migrations")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: migrate [-path dir] [up | version | force <version>]")
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := config.LoadEnv(); err != nil {
		return err
	}

	databaseURL := os.Getenv("PG_URL")
	if databaseURL == "" {
		return errors.New("PG_URL is not set (secrets/<stage>/migration.env)")
	}

	m, err := migrator.Open(*dir, databaseURL, log.Printf)
	if err != nil {
		return err
	}
	defer m.Close()

	switch args := flag.Args(); {
	case len(args) == 0 || len(args) == 1 && args[0] == "up":
		changed, err := migrator.Up(m)
		if err != nil {
			return err
		}

		if !changed {
			log.Print("migrate: no pending migrations")
		}

		return printVersion(m)
	case len(args) == 1 && args[0] == "version":
		return printVersion(m)
	case len(args) == 2 && args[0] == "force":
		version, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("force: %q is not a migration version", args[1])
		}

		if err := m.Force(version); err != nil {
			return fmt.Errorf("force: %w", err)
		}

		return printVersion(m)
	default:
		flag.Usage()

		return errors.New("unknown command")
	}
}

func printVersion(m *migrate.Migrate) error {
	version, dirty, err := m.Version()

	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		log.Print("migrate: no migration applied yet")
	case err != nil:
		return fmt.Errorf("version: %w", err)
	case dirty:
		log.Printf("migrate: version %d is DIRTY: it failed halfway; fix the database, then `force %d`", version, version)
	default:
		log.Printf("migrate: at version %d", version)
	}

	return nil
}
