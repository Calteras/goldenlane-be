package ordering

import (
	"context"
	"sync"
	"time"

	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// Hub tells open streams that an order changed. Changes come from the database's
// notifications, so a stream on one instance hears changes made through another.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[string]map[chan struct{}]struct{}{}} }

// Subscribe returns a channel that receives a signal whenever the order changes. Signals
// coalesce: a slow reader gets one, not a queue. Call cancel when done.
func (h *Hub) Subscribe(orderID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	h.mu.Lock()
	if h.subs[orderID] == nil {
		h.subs[orderID] = map[chan struct{}]struct{}{}
	}

	h.subs[orderID][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()

		delete(h.subs[orderID], ch)

		if len(h.subs[orderID]) == 0 {
			delete(h.subs, orderID)
		}
	}
}

// Notify wakes everyone watching the order.
func (h *Hub) Notify(orderID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.subs[orderID] {
		select {
		case ch <- struct{}{}:
		default: // a signal is already waiting
		}
	}
}

// Run listens for order changes until ctx ends, reconnecting if the connection drops.
func (h *Hub) Run(ctx context.Context, listen func(ctx context.Context, fn func(orderID string)) error, log logger.Interface) {
	wait := time.Second

	for ctx.Err() == nil {
		err := listen(ctx, h.Notify)
		if ctx.Err() != nil {
			return
		}

		log.Warn("order updates listener stopped", "error", err.Error(), "retry_in", wait.String())

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		wait = min(wait*2, 30*time.Second)
	}
}
