// Package pos is the signed client for a merchant's POS (contract sections 2 and 3).
package pos

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
	"github.com/Calteras/streetfood-customer-api/pkg/signing"
)

const (
	_prefix       = "/app/terrapos/integrations/streetfood"
	_timeout      = 10 * time.Second
	_maxResponse  = 10 << 20
	_okStatusFrom = 200
	_okStatusTo   = 300
)

// ErrStatus is any non-2xx answer; the message carries the status, never the body.
var ErrStatus = errors.New("pos answered with an error")

// Client calls POS endpoints with signed requests.
type Client struct {
	http *http.Client
	now  func() time.Time
}

// New returns a client that never follows redirects (a signed request must not be
// replayed to another host) and gives up after ten seconds.
func New() *Client {
	return &Client{
		http: &http.Client{
			Timeout:       _timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now: time.Now,
	}
}

// Credentials identify one merchant link.
type Credentials struct {
	BaseURL string
	KeyID   string
	Secret  string
}

type envelope[T any] struct {
	Data T `json:"data"`
}

// Error is an answer from the POS that is not a success. Code is the POS's machine-readable
// reason (for example PRICE_CHANGED); Data carries its details (problems, new totals).
type Error struct {
	Status  int
	Code    string
	Message string
	Data    map[string]any
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%v: %d %s", ErrStatus, e.Status, e.Code)
	}

	return fmt.Sprintf("%v: %d", ErrStatus, e.Status)
}

// Unwrap lets callers test errors.Is(err, ErrStatus).
func (e *Error) Unwrap() error { return ErrStatus }

// Permanent reports whether retrying the same request cannot help: the POS understood it
// and said no. Authentication problems, timeouts, throttling and server errors are not
// permanent.
func (e *Error) Permanent() bool {
	switch {
	case e.Status == http.StatusUnauthorized, e.Status == http.StatusForbidden,
		e.Status == http.StatusRequestTimeout, e.Status == http.StatusTooManyRequests:
		return false
	default:
		return e.Status >= 400 && e.Status < 500
	}
}

func call[T any](ctx context.Context, c *Client, cred Credentials, method, path string, body []byte) (T, error) {
	var zero T

	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, cred.BaseURL+path, reader)
	if err != nil {
		return zero, fmt.Errorf("pos request: %w", err)
	}

	timestamp := strconv.FormatInt(c.now().Unix(), 10)
	req.Header.Set(signing.HeaderKeyID, cred.KeyID)
	req.Header.Set(signing.HeaderTimestamp, timestamp)
	req.Header.Set(signing.HeaderSignature, signing.Sign(cred.Secret, timestamp, method, path, body))
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return zero, fmt.Errorf("pos request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, _maxResponse))
	if err != nil {
		return zero, fmt.Errorf("pos response: %w", err)
	}

	if resp.StatusCode < _okStatusFrom || resp.StatusCode >= _okStatusTo {
		return zero, ParseError(resp.StatusCode, raw)
	}

	var out envelope[T]
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("pos response is not valid: %w", err)
	}

	return out.Data, nil
}

// ParseError reads the POS's error body: {code, message, data:{...details}}.
func ParseError(status int, raw []byte) *Error {
	e := &Error{Status: status}

	var body struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &body) == nil {
		e.Code, e.Message, e.Data = body.Code, body.Message, body.Data
	}

	return e
}

func get[T any](ctx context.Context, c *Client, cred Credentials, path string) (T, error) {
	return call[T](ctx, c, cred, http.MethodGet, path, nil)
}

// Outlets lists the outlets the merchant published, with today's state.
func (c *Client) Outlets(ctx context.Context, cred Credentials) ([]entity.POSOutlet, time.Time, error) {
	data, err := get[struct {
		GeneratedAt time.Time          `json:"generatedAt"`
		Outlets     []entity.POSOutlet `json:"outlets"`
	}](ctx, c, cred, _prefix+"/outlets")
	if err != nil {
		return nil, time.Time{}, err
	}

	if data.GeneratedAt.IsZero() {
		data.GeneratedAt = c.now()
	}

	return data.Outlets, data.GeneratedAt, nil
}

// Snapshot reads one outlet's state and full catalog.
func (c *Client) Snapshot(ctx context.Context, cred Credentials, posOutletID string) (*entity.POSSnapshot, error) {
	snap, err := get[entity.POSSnapshot](ctx, c, cred, _prefix+"/outlets/"+posOutletID+"/snapshot")
	if err != nil {
		return nil, err
	}

	if snap.GeneratedAt.IsZero() {
		snap.GeneratedAt = c.now()
	}

	return &snap, nil
}

// Verify proves credentials work by reading the merchant's published outlets.
func (c *Client) Verify(ctx context.Context, cred Credentials) error {
	_, _, err := c.Outlets(ctx, cred)

	return err
}

// SubmitRequest is an order to send to the POS (contract section 3.3). The POS prices it.
type SubmitRequest struct {
	PlatformOrderID    string         `json:"platformOrderId"`
	CheckoutID         string         `json:"checkoutId,omitempty"`
	OutletID           string         `json:"outletId"`
	Fulfillment        Fulfillment    `json:"fulfillment"`
	Customer           OrderCustomer  `json:"customer"`
	Notes              *string        `json:"notes,omitempty"`
	Items              []SubmitItem   `json:"items"`
	ExpectedTotalMinor *int64         `json:"expectedTotalMinor,omitempty"`
	Payment            map[string]any `json:"payment"`
}

// Fulfillment is how the customer gets the order.
type Fulfillment struct {
	Type  string  `json:"type"`
	Label *string `json:"label,omitempty"`
}

// OrderCustomer is who is ordering.
type OrderCustomer struct {
	PlatformCustomerID string  `json:"platformCustomerId,omitempty"`
	Name               string  `json:"name"`
	Phone              *string `json:"phone,omitempty"`
}

// SubmitItem is one line.
type SubmitItem struct {
	ProductID   string   `json:"productId"`
	VariantID   *string  `json:"variantId,omitempty"`
	ModifierIDs []string `json:"modifierIds,omitempty"`
	Quantity    int      `json:"quantity"`
	Notes       *string  `json:"notes,omitempty"`
}

// OrderResult is the POS's view of an order.
type OrderResult struct {
	POSOrderID      string    `json:"posOrderId"`
	PlatformOrderID string    `json:"platformOrderId"`
	Status          string    `json:"status"`
	Pricing         Pricing   `json:"pricing"`
	AcceptBy        time.Time `json:"acceptBy"`
	PrepTimeMinutes *int      `json:"prepTimeMinutes"`
	Reason          *string   `json:"reason"`
	Version         int       `json:"version"`
}

// Pricing is the POS's price for an order, in minor units.
type Pricing struct {
	SubtotalMinor int64 `json:"subtotalMinor"`
	TaxMinor      int64 `json:"taxMinor"`
	ServiceMinor  int64 `json:"serviceMinor"`
	TotalMinor    int64 `json:"totalMinor"`
}

// SubmitOrder sends an order. Sending the same platformOrderId again returns the same order,
// so a retry after a timeout is safe.
func (c *Client) SubmitOrder(ctx context.Context, cred Credentials, req SubmitRequest) (OrderResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return OrderResult{}, fmt.Errorf("pos order: %w", err)
	}

	return call[OrderResult](ctx, c, cred, http.MethodPost, _prefix+"/orders", body)
}

// OrderStatus reads an order's current state.
func (c *Client) OrderStatus(ctx context.Context, cred Credentials, platformOrderID string) (OrderResult, error) {
	return get[OrderResult](ctx, c, cred, _prefix+"/orders/"+platformOrderID)
}

// CancelOrder asks the POS to cancel an order that is still waiting for the merchant.
func (c *Client) CancelOrder(ctx context.Context, cred Credentials, platformOrderID, reason string) (OrderResult, error) {
	body, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return OrderResult{}, fmt.Errorf("pos cancel: %w", err)
	}

	return call[OrderResult](ctx, c, cred, http.MethodPost, _prefix+"/orders/"+platformOrderID+"/cancel", body)
}

// QuoteRequest asks what an order would cost.
type QuoteRequest struct {
	OutletID string       `json:"outletId"`
	Items    []SubmitItem `json:"items"`
}

// Quote asks the POS to price an order without placing it. The POS refuses it the same way it
// would refuse the order (outlet closed, item unavailable), so a payment is never asked for an
// order the merchant could not take.
func (c *Client) Quote(ctx context.Context, cred Credentials, req QuoteRequest) (Pricing, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Pricing{}, fmt.Errorf("pos quote: %w", err)
	}

	out, err := call[struct {
		Pricing Pricing `json:"pricing"`
	}](ctx, c, cred, http.MethodPost, _prefix+"/orders/quote", body)

	return out.Pricing, err
}
