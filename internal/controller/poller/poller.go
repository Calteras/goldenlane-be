// Package poller runs background work without keeping the database awake.
//
// A Poller runs its SyncFunc when it is woken (a commit wrote rows: new work may exist),
// when the next scheduled item falls due (a retry, a lease, a deadline), and otherwise only
// on a slow sweep that repairs anything a wake-up missed. An idle service therefore touches
// the database once per sweep instead of every second.
package poller

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

const (
	// _maxBackoff caps the wait between attempts while a sync keeps failing.
	_maxBackoff = time.Minute

	// _minGap is the shortest time between two runs that were not asked for by the
	// work itself, so a burst of commits does not turn into a burst of runs.
	_minGap = 250 * time.Millisecond

	// _dueFloor is the shortest wait for an item that is already due but was not
	// taken (another worker holds it): look again soon, but never in a tight loop.
	_dueFloor = time.Second
)

// ErrShutdownTimeout means the running sync did not finish in time.
var ErrShutdownTimeout = errors.New("poller: shutdown timed out")

// SyncFunc does one round of work; more asks to run again without waiting.
type SyncFunc func(ctx context.Context) (more bool, err error)

// DueFunc says how long until the next item that is scheduled for later falls due:
// a retry after a backoff, an expired lease, a deadline. ok is false when nothing is scheduled.
type DueFunc func(ctx context.Context) (wait time.Duration, ok bool, err error)

// Option tunes a Poller.
type Option func(*Poller)

// WithDue makes the poller sleep until the next scheduled item instead of only sweeping.
func WithDue(due DueFunc) Option {
	return func(p *Poller) { p.due = due }
}

// Poller calls a SyncFunc until shut down: right away while it reports more work, when woken,
// when the next scheduled item is due, every sweep interval at the latest, and with
// exponential backoff while it fails.
type Poller struct {
	name  string
	sync  SyncFunc
	sweep time.Duration
	due   DueFunc
	l     logger.Interface

	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

// New returns a stopped Poller; name labels its log lines and sweep is the longest it
// sleeps with nothing scheduled.
func New(name string, sync SyncFunc, sweep time.Duration, l logger.Interface, opts ...Option) *Poller {
	p := &Poller{
		name: name, sync: sync, sweep: sweep, l: l,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// Wake asks for a run soon. It never blocks, and wake-ups that arrive while the poller is busy
// or already woken coalesce into one more run.
func (p *Poller) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
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
		ended := time.Now()

		if ctx.Err() != nil {
			return
		}

		wait := p.sweep

		switch {
		case err != nil:
			failures++
			wait = backoff(p.interval(), failures)
			p.l.Error("poller - "+p.name, "error", err.Error(), "failures", failures, "retry_in", wait.String())
		case more:
			failures = 0
			wait = 0
		default:
			failures = 0
			wait = p.untilDue(ctx)
		}

		if !p.sleep(ctx, wait) {
			return
		}

		// However the sleep ended, runs are spaced: a burst of commits is one run, not many.
		if rest := _minGap - time.Since(ended); rest > 0 && !pause(ctx, rest) {
			return
		}
	}
}

// interval is the base for the failure backoff: short, whatever the sweep.
func (p *Poller) interval() time.Duration { return min(p.sweep, 5*time.Second) }

// untilDue is how long to sleep when the last run found nothing more to do.
func (p *Poller) untilDue(ctx context.Context) time.Duration {
	if p.due == nil {
		return p.sweep
	}

	wait, ok, err := p.due(ctx)

	switch {
	case ctx.Err() != nil:
		return p.sweep
	case err != nil:
		p.l.Warn("poller - "+p.name+" - due", "error", err.Error())

		return min(p.sweep, _maxBackoff)
	case !ok:
		return p.sweep
	default:
		return min(max(wait, _dueFloor), p.sweep)
	}
}

// sleep waits for the timer, a wake-up or shutdown; a zero wait does not wait at all.
// It reports false when the poller is shutting down.
func (p *Poller) sleep(ctx context.Context, wait time.Duration) bool {
	if wait <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	case <-p.wake:
		return true
	}
}

// pause waits for d or shutdown. A wake-up does not end it: it stays queued for the next run.
func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
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

// Group wakes a set of pollers together: after a commit anywhere, any worker may have work.
type Group struct {
	mu sync.Mutex
	ps []*Poller
}

// Add puts pollers in the group.
func (g *Group) Add(ps ...*Poller) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.ps = append(g.ps, ps...)
}

// Wake wakes every poller in the group.
func (g *Group) Wake() {
	g.mu.Lock()
	ps := append([]*Poller(nil), g.ps...)
	g.mu.Unlock()

	for _, p := range ps {
		p.Wake()
	}
}
