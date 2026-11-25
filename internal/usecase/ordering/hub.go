package ordering

import "sync"

// Hub tells open streams that an order changed. It is told by the commits this instance makes;
// with several instances, a change made by another one reaches a stream at its next safety
// re-read (see the order stream) unless the instances share a signal such as Redis pub/sub.
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

// OrderUpdates is the notification channel the store sends when an order changes.
const OrderUpdates = "order_updates"

// OnNotification wakes the watchers of an order when the database commits a change to it.
// The service hears its own commits through the connection tracer (pkg/postgres.CommitTracer),
// so no connection is held open for LISTEN, which would reconnect after every suspend and
// keep a serverless database awake.
func (h *Hub) OnNotification(channel, payload string) {
	if channel == OrderUpdates {
		h.Notify(payload)
	}
}
