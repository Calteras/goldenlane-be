package ordering

import (
	"context"
	"errors"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

const (
	_notifyBatch    = 20
	_notifyLease    = 60 * time.Second
	_notifyAttempts = 5
)

var _notifyBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// Pusher delivers one push message to one device.
type Pusher interface {
	Send(ctx context.Context, deviceToken, title, body string, data map[string]string) error
}

// NotifierStore is what the notifier needs from storage.
type NotifierStore interface {
	ClaimNotifications(ctx context.Context, limit int, lease time.Duration) ([]entity.Notification, string, error)
	FinishNotification(ctx context.Context, id, token string, sent bool, reason string) error
	RetryNotification(ctx context.Context, id, token, reason string, delay time.Duration) error
	DeviceTokens(ctx context.Context, customerID string) ([]string, error)
	DeleteDevice(ctx context.Context, token string) error
}

// Notifier sends the queued push messages.
type Notifier struct {
	store  NotifierStore
	pusher Pusher
	log    logger.Interface
}

// NewNotifier builds a notifier. With no Pusher (push not configured) it drains the queue
// without sending, so it does not grow forever.
func NewNotifier(store NotifierStore, pusher Pusher, log logger.Interface) *Notifier {
	return &Notifier{store: store, pusher: pusher, log: log}
}

// Process sends one batch. more asks the poller to run again at once.
func (n *Notifier) Process(ctx context.Context) (bool, error) {
	batch, token, err := n.store.ClaimNotifications(ctx, _notifyBatch, _notifyLease)
	if err != nil {
		return false, err
	}

	for _, msg := range batch {
		if err := n.deliver(ctx, msg, token); err != nil {
			return false, err
		}
	}

	return len(batch) == _notifyBatch, nil
}

func (n *Notifier) deliver(ctx context.Context, msg entity.Notification, token string) error {
	if n.pusher == nil {
		return n.store.FinishNotification(ctx, msg.ID, token, false, "push is not configured")
	}

	devices, err := n.store.DeviceTokens(ctx, msg.CustomerID)
	if err != nil {
		return err
	}

	if len(devices) == 0 {
		return n.store.FinishNotification(ctx, msg.ID, token, false, "no devices")
	}

	delivered := 0

	var lastErr error

	for _, d := range devices {
		switch err := n.pusher.Send(ctx, d, msg.Title, msg.Body, msg.Data); {
		case err == nil:
			delivered++
		case errors.Is(err, entity.ErrUnregistered):
			if derr := n.store.DeleteDevice(ctx, d); derr != nil {
				return derr
			}
		default:
			lastErr = err
		}
	}

	switch {
	case delivered > 0:
		return n.store.FinishNotification(ctx, msg.ID, token, true, "")
	case lastErr == nil:
		return n.store.FinishNotification(ctx, msg.ID, token, false, "no valid devices")
	case msg.Attempts >= _notifyAttempts:
		n.log.Warn("push given up", "order", msg.OrderID, "error", lastErr.Error())

		return n.store.FinishNotification(ctx, msg.ID, token, false, lastErr.Error())
	default:
		return n.store.RetryNotification(ctx, msg.ID, token, lastErr.Error(), _notifyBackoff[min(msg.Attempts-1, len(_notifyBackoff)-1)])
	}
}
