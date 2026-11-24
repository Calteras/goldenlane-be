package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
)

// FakeOrderPOS behaves like a merchant's POS for orders: submitting the same platform order
// id again returns the same order, every change raises its version, and failures can be
// scripted. Error answers are the real ones captured from the POS (testdata/pos).
type FakeOrderPOS struct {
	mu     sync.Mutex
	orders map[string]*pos.OrderResult

	// Down makes every call fail like an unreachable POS.
	Down bool
	// SubmitErr, if set, answers the next submissions with this error.
	SubmitErr *pos.Error
	// OnSubmit runs inside SubmitOrder, before it answers: a hook to cause races.
	OnSubmit func(req pos.SubmitRequest)
	// LoseAnswer makes SubmitOrder store the order but fail to answer, like a timeout.
	LoseAnswer bool
	// CancelErr answers cancels with this error.
	CancelErr *pos.Error

	// QuoteErr answers quotes with this error; QuoteTotal overrides the quoted total.
	QuoteErr   *pos.Error
	QuoteTotal int64

	Submitted []pos.SubmitRequest
	Cancelled []string
	Calls     []string
}

// NewFakeOrderPOS returns a POS that accepts everything.
func NewFakeOrderPOS() *FakeOrderPOS { return &FakeOrderPOS{orders: map[string]*pos.OrderResult{}} }

func (f *FakeOrderPOS) record(call string) { f.Calls = append(f.Calls, call) }

// SubmitOrder implements ordering.POS.
func (f *FakeOrderPOS) SubmitOrder(_ context.Context, _ pos.Credentials, req pos.SubmitRequest) (pos.OrderResult, error) {
	f.mu.Lock()
	hook := f.OnSubmit
	f.mu.Unlock()

	if hook != nil {
		hook(req)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("submit:" + req.PlatformOrderID)
	f.Submitted = append(f.Submitted, req)

	if f.Down {
		return pos.OrderResult{}, errors.New("connection refused")
	}

	if f.SubmitErr != nil {
		return pos.OrderResult{}, f.SubmitErr
	}

	if existing, ok := f.orders[req.PlatformOrderID]; ok {
		return *existing, nil
	}

	total := int64(1554000)
	if req.ExpectedTotalMinor != nil && *req.ExpectedTotalMinor != total {
		return pos.OrderResult{}, &pos.Error{Status: 409, Code: "PRICE_CHANGED", Data: map[string]any{"totalMinor": float64(total)}}
	}

	r := &pos.OrderResult{
		POSOrderID: "pos-" + req.PlatformOrderID, PlatformOrderID: req.PlatformOrderID, Status: entity.OrderPendingAccept,
		Pricing:  pos.Pricing{SubtotalMinor: 1400000, TaxMinor: 154000, TotalMinor: total},
		AcceptBy: time.Now().Add(5 * time.Minute), Version: 1,
	}
	f.orders[req.PlatformOrderID] = r

	if f.LoseAnswer {
		return pos.OrderResult{}, errors.New("timeout")
	}

	return *r, nil
}

// Quote implements ordering.POS: it prices like SubmitOrder does, and stores nothing.
func (f *FakeOrderPOS) Quote(_ context.Context, _ pos.Credentials, _ pos.QuoteRequest) (pos.Pricing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("quote")

	if f.Down {
		return pos.Pricing{}, errors.New("connection refused")
	}

	if f.QuoteErr != nil {
		return pos.Pricing{}, f.QuoteErr
	}

	total := int64(1554000)
	if f.QuoteTotal != 0 {
		total = f.QuoteTotal
	}

	return pos.Pricing{SubtotalMinor: 1400000, TaxMinor: total - 1400000, TotalMinor: total}, nil
}

// OrderStatus implements ordering.POS.
func (f *FakeOrderPOS) OrderStatus(_ context.Context, _ pos.Credentials, id string) (pos.OrderResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("status:" + id)

	if f.Down {
		return pos.OrderResult{}, errors.New("connection refused")
	}

	r, ok := f.orders[id]
	if !ok {
		return pos.OrderResult{}, &pos.Error{Status: 404, Code: "ORDER_NOT_FOUND"}
	}

	return *r, nil
}

// CancelOrder implements ordering.POS.
func (f *FakeOrderPOS) CancelOrder(_ context.Context, _ pos.Credentials, id, _ string) (pos.OrderResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("cancel:" + id)
	f.Cancelled = append(f.Cancelled, id)

	if f.Down {
		return pos.OrderResult{}, errors.New("connection refused")
	}

	if f.CancelErr != nil {
		return pos.OrderResult{}, f.CancelErr
	}

	r, ok := f.orders[id]
	if !ok {
		return pos.OrderResult{}, &pos.Error{Status: 404, Code: "ORDER_NOT_FOUND"}
	}

	if r.Status == entity.OrderCancelled {
		return *r, nil
	}

	if r.Status != entity.OrderPendingAccept {
		return pos.OrderResult{}, &pos.Error{Status: 409, Code: "INVALID_TRANSITION"}
	}

	r.Status, r.Version = entity.OrderCancelled, r.Version+1

	return *r, nil
}

// Set changes an order at the POS, as the merchant does, and returns its new version.
func (f *FakeOrderPOS) Set(id, status string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	r := f.orders[id]
	r.Status, r.Version = status, r.Version+1

	return r.Version
}

// Has reports whether the POS holds the order.
func (f *FakeOrderPOS) Has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.orders[id]

	return ok
}

// Count reports how many calls started with prefix.
func (f *FakeOrderPOS) Count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, c := range f.Calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}

	return n
}

// RealError returns the POS error captured for a scripted call (testdata/pos/order-calls.json).
func RealError(t *testing.T, name string) *pos.Error {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(), "internal", "testutil", "testdata", "pos", "order-calls.json"))
	require.NoError(t, err)

	var calls map[string]struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}

	require.NoError(t, json.Unmarshal(raw, &calls))

	c, ok := calls[name]
	require.True(t, ok, "no captured call %q", name)

	return pos.ParseError(c.Status, c.Body)
}
