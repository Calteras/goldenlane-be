// Package gateway talks to the payment gateways merchants bring: Xendit and Midtrans. Each
// merchant uses its own account, so every call carries that merchant's credentials.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

const (
	_timeout     = 12 * time.Second
	_maxResponse = 2 << 20
)

// Credentials are a merchant's gateway keys.
type Credentials struct {
	// Secret is the Xendit secret API key or the Midtrans server key.
	Secret string `json:"secret"`
	// CallbackToken is Xendit's callback verification token (Midtrans signs with the server key).
	CallbackToken string `json:"callbackToken,omitempty"`
	Sandbox       bool   `json:"-"`
}

// ChargeRequest asks the gateway for a payment page.
type ChargeRequest struct {
	ExternalID   string // unique per attempt; the gateway echoes it in callbacks
	AmountRupiah int64
	Description  string
	CustomerName string
	ExpiresAt    time.Time
	ReturnURL    string // optional deep link the page returns to
}

// Charge is a payment page the customer can open.
type Charge struct {
	ChargeID  string // the gateway's own id; equals ExternalID where it has none
	PayURL    string
	ExpiresAt time.Time
}

// State is where a charge stands.
type State string

// Charge states.
const (
	StatePending State = "PENDING"
	StatePaid    State = "PAID"
	StateFailed  State = "FAILED"
	StateExpired State = "EXPIRED"
)

// Status is what the gateway says about a charge.
type Status struct {
	State      State
	PaidRupiah int64
	Method     string // QRIS, E_WALLET, BANK_TRANSFER or OTHER
	PaidAt     time.Time
}

// RefundRequest asks the gateway to return a whole payment.
type RefundRequest struct {
	ExternalID   string
	ChargeID     string
	AmountRupiah int64
	Reason       string
	Key          string // repeating a key never refunds twice
}

// RefundResult is the gateway's answer to a refund.
type RefundResult struct {
	Ref  string
	Done bool // false: accepted, the gateway is still sending the money back
}

// Error is a refusal from a gateway.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("gateway answered %d %s %s", e.Status, e.Code, e.Message)
}

// Permanent reports whether the same request cannot succeed later: the gateway understood
// it and said no. Authentication problems, throttling and server errors may pass.
func (e *Error) Permanent() bool {
	switch {
	case e.Status == http.StatusUnauthorized, e.Status == http.StatusForbidden,
		e.Status == http.StatusRequestTimeout, e.Status == http.StatusTooManyRequests:
		return false
	default:
		return e.Status >= 400 && e.Status < 500
	}
}

// ErrBadCallback means a callback did not prove it came from the gateway.
var ErrBadCallback = errors.New("callback is not authentic")

// Provider is one gateway.
type Provider interface {
	// Check proves the credentials work.
	Check(ctx context.Context, c Credentials) error
	Create(ctx context.Context, c Credentials, r ChargeRequest) (Charge, error)
	Get(ctx context.Context, c Credentials, externalID, chargeID string) (Status, error)
	// Expire closes a charge nobody should pay any more. Best effort.
	Expire(ctx context.Context, c Credentials, externalID, chargeID string) error
	Refund(ctx context.Context, c Credentials, r RefundRequest) (RefundResult, error)
	// Callback checks a notification and returns the external id it is about.
	Callback(c Credentials, header func(string) string, body []byte) (externalID string, err error)
}

// Endpoints are the gateways' addresses. Empty fields use the real ones; tests and staging
// can point them elsewhere.
type Endpoints struct {
	Xendit       string
	MidtransSnap string
	MidtransAPI  string
}

// Registry finds the adapter of a provider.
type Registry struct {
	xendit   *Xendit
	midtrans *Midtrans
}

// NewRegistry builds both adapters.
func NewRegistry(e Endpoints) *Registry {
	return &Registry{xendit: NewXendit(e.Xendit), midtrans: NewMidtrans(e.MidtransSnap, e.MidtransAPI)}
}

// For returns the adapter of a provider name, or nil.
func (r *Registry) For(provider string) Provider {
	switch provider {
	case entity.ProviderXendit:
		return r.xendit
	case entity.ProviderMidtrans:
		return r.midtrans
	default:
		return nil
	}
}

type httpClient struct{ c *http.Client }

func newHTTP() httpClient {
	return httpClient{c: &http.Client{
		Timeout:       _timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// do sends a JSON request with basic auth (the key as user name) and returns the status and body.
func (h httpClient) do(ctx context.Context, method, url, secret string, body any, headers map[string]string) (int, []byte, error) {
	var reader io.Reader = http.NoBody

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("gateway request: %w", err)
		}

		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("gateway request: %w", err)
	}

	req.SetBasicAuth(secret, "")
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := h.c.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("gateway request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, _maxResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("gateway response: %w", err)
	}

	return resp.StatusCode, raw, nil
}

// rupiah reads an amount the gateways send as a number or as "10000.00".
func rupiah(v json.RawMessage) int64 {
	if len(v) == 0 {
		return 0
	}

	var n float64
	if json.Unmarshal(v, &n) == nil {
		return int64(n + 0.5)
	}

	var s string
	if json.Unmarshal(v, &s) == nil {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f + 0.5)
		}
	}

	return 0
}
