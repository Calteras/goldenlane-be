package postgres

import "time"

// Option -.
type Option func(*Postgres)

// MaxPoolSize -.
func MaxPoolSize(size int) Option {
	return func(c *Postgres) {
		c.maxPoolSize = size
	}
}

// ConnAttempts -.
func ConnAttempts(attempts int) Option {
	return func(c *Postgres) {
		c.connAttempts = attempts
	}
}

// ConnTimeout -.
func ConnTimeout(timeout time.Duration) Option {
	return func(c *Postgres) {
		c.connTimeout = timeout
	}
}

// Tracer reports commits to the process (see CommitTracer).
func Tracer(t *CommitTracer) Option {
	return func(c *Postgres) {
		c.tracer = t
	}
}

// ApplicationName names this service's connections in pg_stat_activity and in the Neon
// console, unless the connection string already sets one.
func ApplicationName(name string) Option {
	return func(c *Postgres) {
		c.applicationName = name
	}
}
