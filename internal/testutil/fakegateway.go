package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/gateway"
)

// CallbackToken is the token the fake gateway expects on notifications.
const CallbackToken = "cb-token-1"

// FakeCharge is one payment page at the fake gateway.
type FakeCharge struct {
	Request gateway.ChargeRequest
	State   gateway.State
	Method  string
	Paid    int64 // rupiah the customer paid; 0 means exactly the charge
	Expired bool  // Expire was called
}

// FakeGateway behaves like a payment gateway: charges stay pending until a test pays, fails or
// expires them, and errors can be scripted.
type FakeGateway struct {
	mu      sync.Mutex
	Charges map[string]*FakeCharge

	CheckErr  error
	CreateErr error
	GetErr    error
	RefundErr error
	// Refunded lists the refunds it was asked for.
	Refunded []gateway.RefundRequest
}

// NewFakeGateway returns a gateway that accepts everything.
func NewFakeGateway() *FakeGateway { return &FakeGateway{Charges: map[string]*FakeCharge{}} }

// For implements payment.Providers: both providers are this one fake.
func (f *FakeGateway) For(provider string) gateway.Provider {
	if provider == "XENDIT" || provider == "MIDTRANS" {
		return f
	}

	return nil
}

// Check implements gateway.Provider.
func (f *FakeGateway) Check(context.Context, gateway.Credentials) error { return f.CheckErr }

// Create implements gateway.Provider.
func (f *FakeGateway) Create(_ context.Context, _ gateway.Credentials, r gateway.ChargeRequest) (gateway.Charge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.CreateErr != nil {
		return gateway.Charge{}, f.CreateErr
	}

	f.Charges[r.ExternalID] = &FakeCharge{Request: r, State: gateway.StatePending}

	return gateway.Charge{ChargeID: "inv-" + r.ExternalID, PayURL: "https://pay.test/" + r.ExternalID, ExpiresAt: r.ExpiresAt}, nil
}

// Get implements gateway.Provider.
func (f *FakeGateway) Get(_ context.Context, _ gateway.Credentials, externalID, _ string) (gateway.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.GetErr != nil {
		return gateway.Status{}, f.GetErr
	}

	c, ok := f.Charges[externalID]
	if !ok {
		return gateway.Status{State: gateway.StatePending}, nil
	}

	paid := c.Paid
	if paid == 0 && c.State == gateway.StatePaid {
		paid = c.Request.AmountRupiah
	}

	st := gateway.Status{State: c.State, PaidRupiah: paid, Method: c.Method}
	if c.State == gateway.StatePaid {
		st.PaidAt = time.Now()
	}

	return st, nil
}

// Expire implements gateway.Provider.
func (f *FakeGateway) Expire(_ context.Context, _ gateway.Credentials, externalID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c, ok := f.Charges[externalID]; ok {
		c.Expired = true
	}

	return nil
}

// Refund implements gateway.Provider.
func (f *FakeGateway) Refund(_ context.Context, _ gateway.Credentials, r gateway.RefundRequest) (gateway.RefundResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Refunded = append(f.Refunded, r)

	if f.RefundErr != nil {
		return gateway.RefundResult{}, f.RefundErr
	}

	return gateway.RefundResult{Ref: "rf-" + r.Key, Done: true}, nil
}

// Callback implements gateway.Provider: the token must match, the body names the charge.
func (f *FakeGateway) Callback(_ gateway.Credentials, header func(string) string, body []byte) (string, error) {
	if header("X-Callback-Token") != CallbackToken {
		return "", gateway.ErrBadCallback
	}

	var b struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(body, &b); err != nil || b.ExternalID == "" {
		return "", gateway.ErrBadCallback
	}

	return b.ExternalID, nil
}

// Settle moves a charge to a state, as the customer or the bank would.
func (f *FakeGateway) Settle(externalID string, state gateway.State, method string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.Charges[externalID]
	c.State, c.Method = state, method
}

// Charge returns what the gateway holds for a charge.
func (f *FakeGateway) Charge(externalID string) (*FakeCharge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.Charges[externalID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errors.New("no such charge"), externalID)
	}

	return c, nil
}

// Only returns the one charge there is.
func (f *FakeGateway) Only() (string, *FakeCharge) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for id, c := range f.Charges {
		return id, c
	}

	return "", nil
}
