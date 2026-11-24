// Package poller runs a sync in the background on an interval: the inbound adapter for
// event streams the app pulls rather than receives, such as a reconciliation against another system.
package poller

import (
	"context"
	"errors"
	"time"

	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// _maxBackoff caps the wait between attempts while a sync keeps failing.
const _maxBackoff = time.Minute

// ErrShutdownTimeout means the running sync did not finish in time.
var ErrShutdownTimeout = errors.New("poller: shutdown timed out")

// SyncFunc does one round of work; more asks to run again without waiting.
type SyncFunc func(ctx context.Context) (more bool, err error)

// Poller calls a SyncFunc until shut down: right away while it reports more work,
// every interval when idle, and with exponential backoff while it fails.
type Poller struct {
	name     string
	sync     SyncFunc
	interval time.Duration
	l        logger.Interface

	cancel context.CancelFunc
	done   chan struct{}
}

// New returns a stopped Poller; name labels its log lines.
func New(name string, sync SyncFunc, interval time.Duration, l logger.Interface) *Poller {
	return &Poller{name: name, sync: sync, interval: interval, l: l, done: make(chan struct{})}
}

// Start begins polling in a goroutine.
func (p *Poller) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	go p.run(ctx)
}

// Shutdown stops polling and waits up to timeout for the running sync to return.
func (p *Poller) Shutdown(timeout time.Duration) error {
	if p.cancel == nil {
		return nil
	}

	p.cancel()

	select {
	case <-p.done:
		return nil
	case <-time.After(timeout):
		return ErrShutdownTimeout
	}
}

func (p *Poller) run(ctx context.Context) {
	defer close(p.done)

	failures := 0

	for {
		more, err := p.sync(ctx)

		if ctx.Err() != nil {
			return
		}

		wait := p.interval

		switch {
		case err != nil:
			failures++
			wait = backoff(p.interval, failures)
			p.l.Error("poller - "+p.name, "error", err.Error(), "failures", failures, "retry_in", wait.String())
		case more:
			failures = 0
			wait = 0
		default:
			failures = 0
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}

// backoff doubles the interval for each consecutive failure, up to _maxBackoff.
func backoff(interval time.Duration, failures int) time.Duration {
	wait := interval
	for i := 1; i < failures && wait < _maxBackoff; i++ {
		wait *= 2
	}

	return min(wait, _maxBackoff)
}
