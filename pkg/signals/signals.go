// Package signals tells the other instances of the service what this one just did, over
// Redis pub/sub: that rows were written (so their workers wake and their caches drop what
// changed), that an order changed (so their open order streams update), and that a customer's
// cached account must be forgotten.
//
// Redis here is a doorbell, never a store. A message that is lost, or a Redis that is down,
// delays other instances until their next sweep or cache expiry and loses nothing. With one
// instance, or without Redis, the service works the same: this process always acts on its
// own commits directly.
package signals

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// Kinds of message.
const (
	// KindWrite: rows were committed. Tables names them.
	KindWrite = "write"

	// KindNotify: a database notification was committed (Channel and Payload).
	KindNotify = "notify"

	// KindForget: a customer's cached account is out of date (Payload is the customer id).
	KindForget = "forget"
)

const (
	// _window is how long outgoing messages are gathered into one publish, so a burst of
	// commits is one Redis command.
	_window = 100 * time.Millisecond

	_queue          = 512
	_publishTimeout = 2 * time.Second
	_logEvery       = time.Minute
	_healthEvery    = 30 * time.Second
)

// Message is one signal.
type Message struct {
	Kind    string   `json:"k"`
	Tables  []string `json:"t,omitempty"`
	Channel string   `json:"c,omitempty"`
	Payload string   `json:"p,omitempty"`
}

type envelope struct {
	From     string    `json:"f"`
	Messages []Message `json:"m"`
}

// Bus publishes this instance's signals and delivers other instances' signals. The zero
// value, and a nil *Bus, do nothing, so callers need not check whether Redis is configured.
type Bus struct {
	rdb     *redis.Client
	channel string
	id      string
	l       logger.Interface

	out     chan Message
	lastLog atomic.Int64
}

// New prepares a bus on the Redis at url (redis:// or rediss://) and a channel name. It does
// not connect: Run does, and keeps retrying, so a Redis that is down at start never stops the service.
func New(url, channel string, l logger.Interface) (*Bus, error) {
	// The client logs every failed reconnect to the standard logger. Run reports the first
	// failure once, so the rest stays quiet.
	redis.SetLogger(quietLogger{})

	opts, err := redis.ParseURL(url)
	if err != nil {
		// The parser's message can quote the URL, which holds the password.
		return nil, errors.New("REDIS_URL is not a valid redis:// or rediss:// address")
	}

	opts.DialTimeout = 5 * time.Second
	opts.ReadTimeout = 5 * time.Second
	opts.WriteTimeout = 3 * time.Second
	opts.PoolSize = 2
	opts.MinIdleConns = 0
	opts.ClientName = "streetfood-customer-api"

	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("instance id: %w", err)
	}

	return &Bus{
		rdb: redis.NewClient(opts), channel: channel, id: hex.EncodeToString(id), l: l,
		out: make(chan Message, _queue),
	}, nil
}

// quietLogger drops the Redis client's own log lines.
type quietLogger struct{}

func (quietLogger) Printf(context.Context, string, ...interface{}) {}

// Publish queues a signal for the other instances. It never blocks: if the queue is full
// (Redis is down for long) the signal is dropped, which only delays the others to their sweep.
func (b *Bus) Publish(m Message) {
	if b == nil || b.out == nil {
		return
	}

	select {
	case b.out <- m:
	default:
	}
}

// Run publishes queued signals and calls handle for each signal from another instance, until
// ctx ends. It blocks, so run it in a goroutine.
func (b *Bus) Run(ctx context.Context, handle func(Message)) {
	if b == nil || b.rdb == nil {
		return
	}

	go b.publishLoop(ctx)
	b.subscribeLoop(ctx, handle)

	_ = b.rdb.Close()
}

func (b *Bus) subscribeLoop(ctx context.Context, handle func(Message)) {
	ps := b.rdb.Subscribe(ctx, b.channel)
	defer ps.Close()

	// The first confirmation tells whether Redis is reachable; the client reconnects by itself.
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	if _, err := ps.Receive(probe); err != nil {
		b.l.Warn("signals - redis is not reachable; instances will sync on their next sweep", "error", err.Error())
	} else {
		b.l.Info("signals - subscribed", "channel", b.channel)
	}

	cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ps.Channel(redis.WithChannelHealthCheckInterval(_healthEvery)):
			if !ok {
				return
			}

			var env envelope
			if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil || env.From == b.id {
				continue
			}

			for _, m := range env.Messages {
				handle(m)
			}
		}
	}
}

func (b *Bus) publishLoop(ctx context.Context) {
	for {
		var first Message

		select {
		case <-ctx.Done():
			return
		case first = <-b.out:
		}

		batch := []Message{first}
		timer := time.NewTimer(_window)

	gather:
		for {
			select {
			case m := <-b.out:
				batch = append(batch, m)
			case <-timer.C:
				break gather
			case <-ctx.Done():
				timer.Stop()

				return
			}
		}

		b.send(ctx, merge(batch))
	}
}

func (b *Bus) send(ctx context.Context, msgs []Message) {
	raw, err := json.Marshal(envelope{From: b.id, Messages: msgs})
	if err != nil {
		return
	}

	pub, cancel := context.WithTimeout(ctx, _publishTimeout)
	defer cancel()

	if err := b.rdb.Publish(pub, b.channel, raw).Err(); err != nil && ctx.Err() == nil {
		// One line a minute: Redis being down must not flood the log.
		now := time.Now().UnixNano()
		if last := b.lastLog.Load(); now-last > int64(_logEvery) && b.lastLog.CompareAndSwap(last, now) {
			b.l.Warn("signals - publish failed; other instances will sync on their next sweep", "error", err.Error())
		}
	}
}

// merge joins the write signals of a batch into one and drops duplicates.
func merge(batch []Message) []Message {
	var (
		out    []Message
		tables = map[string]struct{}{}
		wrote  bool
		seen   = map[string]struct{}{}
	)

	for _, m := range batch {
		if m.Kind == KindWrite {
			wrote = true

			for _, t := range m.Tables {
				tables[t] = struct{}{}
			}

			continue
		}

		key := m.Kind + "|" + m.Channel + "|" + m.Payload
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			out = append(out, m)
		}
	}

	if wrote {
		names := make([]string, 0, len(tables))
		for t := range tables {
			names = append(names, t)
		}

		sort.Strings(names)

		out = append([]Message{{Kind: KindWrite, Tables: names}}, out...)
	}

	return out
}
