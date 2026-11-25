// Package postgres implements postgres connection.
package postgres

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	_defaultMaxPoolSize  = 1
	_defaultConnAttempts = 10
	_defaultConnTimeout  = time.Second

	// _maxConnIdleTime closes idle connections before Neon's 5 minute scale-to-zero
	// timeout drops them, so the pool never hands out a connection the server has already
	// cut. With no minimum connections nothing is held open or reopened while idle.
	_maxConnIdleTime = 4 * time.Minute

	// _pingAfter is how long a connection may sit idle before the pool checks it is alive
	// when it is handed out. The default is one second, which adds a round trip (and a
	// statement) to nearly every request of a quiet service. A connection idle this long may
	// belong to a compute that has suspended, so it is worth checking.
	_pingAfter = 3 * time.Minute
)

// Postgres -.
type Postgres struct {
	maxPoolSize  int
	connAttempts int
	connTimeout  time.Duration

	tracer          *CommitTracer
	applicationName string

	Builder squirrel.StatementBuilderType
	Pool    *pgxpool.Pool
}

// New -.
func New(url string, opts ...Option) (*Postgres, error) {
	pg := &Postgres{
		maxPoolSize:  _defaultMaxPoolSize,
		connAttempts: _defaultConnAttempts,
		connTimeout:  _defaultConnTimeout,
	}

	// Custom options
	for _, opt := range opts {
		opt(pg)
	}

	pg.Builder = squirrel.StatementBuilder.PlaceholderFormat(squirrel.Dollar)

	poolConfig, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("postgres - NewPostgres - pgxpool.ParseConfig: %w", err)
	}

	poolConfig.MaxConns = int32(pg.maxPoolSize) //nolint:gosec // skip integer overflow conversion int -> int32
	poolConfig.MinConns = 0
	poolConfig.MaxConnIdleTime = _maxConnIdleTime
	poolConfig.ShouldPing = func(_ context.Context, p pgxpool.ShouldPingParams) bool {
		return p.IdleDuration > _pingAfter
	}

	if _, set := poolConfig.ConnConfig.RuntimeParams["application_name"]; !set && pg.applicationName != "" {
		poolConfig.ConnConfig.RuntimeParams["application_name"] = pg.applicationName
	}

	if pg.tracer != nil {
		poolConfig.ConnConfig.Tracer = pg.tracer
		poolConfig.BeforeClose = pg.tracer.forget
	}

	for pg.connAttempts > 0 {
		pg.Pool, err = pgxpool.NewWithConfig(context.Background(), poolConfig)
		if err == nil {
			break
		}

		log.Printf("Postgres is trying to connect, attempts left: %d", pg.connAttempts)

		time.Sleep(pg.connTimeout)

		pg.connAttempts--
	}

	if err != nil {
		return nil, fmt.Errorf("postgres - NewPostgres - connAttempts == 0: %w", err)
	}

	return pg, nil
}

// Close -.
func (p *Postgres) Close() {
	if p.Pool != nil {
		p.Pool.Close()
	}
}
