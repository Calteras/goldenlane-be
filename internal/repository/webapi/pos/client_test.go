package pos_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
	"github.com/Calteras/streetfood-customer-api/pkg/signing"
)

const secret = "sfs_client_test_secret"

type captured struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func fixtures(t *testing.T) map[string]captured {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testutil", "testdata", "pos", "order-calls.json"))
	require.NoError(t, err)

	var m map[string]captured
	require.NoError(t, json.Unmarshal(raw, &m))

	return m
}

// posDouble answers like the POS and checks every request's signature the way the POS does.
type posDouble struct {
	t        *testing.T
	answer   captured
	method   string
	path     string
	body     []byte
	verified error
}

func (d *posDouble) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.method, d.path = r.Method, r.URL.Path
	d.body, _ = io.ReadAll(r.Body)
	d.verified = signing.Verify(secret, r.Header.Get(signing.HeaderTimestamp), r.Header.Get(signing.HeaderSignature), r.Method, r.URL.RequestURI(), d.body, time.Now())

	if r.Header.Get(signing.HeaderKeyID) != "sfk_0123456789abcdef01234567" {
		d.verified = signing.ErrSignatureInvalid
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(d.answer.Status)
	_, _ = w.Write(d.answer.Body)
}

func setup(t *testing.T, answer captured) (*pos.Client, pos.Credentials, *posDouble) {
	t.Helper()

	d := &posDouble{t: t, answer: answer}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)

	return pos.New(), pos.Credentials{BaseURL: srv.URL, KeyID: "sfk_0123456789abcdef01234567", Secret: secret}, d
}

func TestSubmitOrder(t *testing.T) {
	fx := fixtures(t)
	c, cred, d := setup(t, fx["submit-ok"])

	phone := "+628123456789"
	res, err := c.SubmitOrder(context.Background(), cred, pos.SubmitRequest{
		PlatformOrderID: "mo_abc123", OutletID: "o1", Fulfillment: pos.Fulfillment{Type: "PICKUP"},
		Customer: pos.OrderCustomer{Name: "Rina", Phone: &phone},
		Items:    []pos.SubmitItem{{ProductID: "p1", Quantity: 2}},
		Payment:  map[string]any{"mode": "PAY_AT_PICKUP"},
	})
	require.NoError(t, err)
	require.NoError(t, d.verified, "the POS would accept this signature")
	require.Equal(t, "POST", d.method)
	require.Equal(t, "/app/terrapos/integrations/streetfood/orders", d.path)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(d.body, &sent))
	require.Equal(t, "mo_abc123", sent["platformOrderId"])
	require.NotContains(t, sent, "notes", "empty optional fields are left out: the POS rejects unknown or null ones")
	require.NotContains(t, sent, "expectedTotalMinor")

	require.Equal(t, "PENDING_ACCEPTANCE", res.Status)
	require.Equal(t, int64(1554000), res.Pricing.TotalMinor)
	require.Equal(t, 1, res.Version)
	require.False(t, res.AcceptBy.IsZero())
	require.NotEmpty(t, res.POSOrderID)
}

func TestErrorsFromTheRealPOS(t *testing.T) {
	fx := fixtures(t)

	for name, tc := range map[string]struct {
		call      string
		code      string
		permanent bool
	}{
		"price moved":     {"submit-price-changed", "PRICE_CHANGED", true},
		"items gone":      {"submit-invalid", "ORDER_INVALID", true},
		"closed":          {"submit-closed", "OUTLET_CLOSED", true},
		"unknown outlet":  {"submit-unknown-outlet", "OUTLET_UNAVAILABLE", true},
		"id reused":       {"submit-conflict", "IDEMPOTENCY_CONFLICT", true},
		"malformed":       {"submit-bad-body", "", true},
		"cancel too late": {"cancel-late", "INVALID_TRANSITION", true},
	} {
		t.Run(name, func(t *testing.T) {
			c, cred, _ := setup(t, fx[tc.call])

			_, err := c.SubmitOrder(context.Background(), cred, pos.SubmitRequest{PlatformOrderID: "mo_abc123"})

			var perr *pos.Error

			require.ErrorAs(t, err, &perr)
			require.ErrorIs(t, err, pos.ErrStatus)
			require.Equal(t, tc.code, perr.Code)
			require.Equal(t, fx[tc.call].Status, perr.Status)
			require.Equal(t, tc.permanent, perr.Permanent())
		})
	}

	t.Run("details are kept", func(t *testing.T) {
		c, cred, _ := setup(t, fx["submit-price-changed"])
		_, err := c.SubmitOrder(context.Background(), cred, pos.SubmitRequest{})

		var perr *pos.Error

		require.ErrorAs(t, err, &perr)
		require.EqualValues(t, 1554000, perr.Data["totalMinor"])
	})
}

func TestWhatIsWorthRetrying(t *testing.T) {
	for status, permanent := range map[int]bool{
		400: true, 404: true, 409: true, 422: true,
		401: false, 403: false, 408: false, 429: false, // credentials, timing, throttling: may pass later
		500: false, 502: false, 503: false, 302: false,
	} {
		require.Equal(t, permanent, (&pos.Error{Status: status}).Permanent(), "status %d", status)
	}
}

func TestStatusAndCancel(t *testing.T) {
	fx := fixtures(t)

	c, cred, d := setup(t, fx["status"])
	res, err := c.OrderStatus(context.Background(), cred, "mo_abc123")
	require.NoError(t, err)
	require.NoError(t, d.verified)
	require.Equal(t, "GET", d.method)
	require.Equal(t, "/app/terrapos/integrations/streetfood/orders/mo_abc123", d.path)
	require.Equal(t, "PENDING_ACCEPTANCE", res.Status)

	c, cred, d = setup(t, fx["cancel-ok"])
	res, err = c.CancelOrder(context.Background(), cred, "mo_abc123", "changed my mind")
	require.NoError(t, err)
	require.NoError(t, d.verified)
	require.Equal(t, "/app/terrapos/integrations/streetfood/orders/mo_abc123/cancel", d.path)
	require.JSONEq(t, `{"reason":"changed my mind"}`, string(d.body))
	require.Equal(t, "CANCELLED", res.Status)
	require.Equal(t, 2, res.Version)

	c, cred, _ = setup(t, fx["status-unknown"])
	_, err = c.OrderStatus(context.Background(), cred, "mo_nope")

	var perr *pos.Error

	require.ErrorAs(t, err, &perr)
	require.Equal(t, "ORDER_NOT_FOUND", perr.Code)
}

func TestNeverFollowsRedirects(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the signed request was replayed to another host")
	}))
	defer elsewhere.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer origin.Close()

	_, err := pos.New().OrderStatus(context.Background(), pos.Credentials{BaseURL: origin.URL, KeyID: "k", Secret: secret}, "mo_abc123")

	var perr *pos.Error

	require.ErrorAs(t, err, &perr)
	require.Equal(t, 302, perr.Status)
	require.False(t, perr.Permanent())
}

func TestUnreachableAndGarbage(t *testing.T) {
	cred := pos.Credentials{BaseURL: "http://127.0.0.1:1", KeyID: "k", Secret: secret}
	_, err := pos.New().OrderStatus(context.Background(), cred, "mo_abc123")

	var perr *pos.Error

	require.Error(t, err)
	require.NotErrorAs(t, err, &perr, "a connection failure is not an answer from the POS")

	c, cred2, _ := setup(t, captured{Status: 200, Body: json.RawMessage(`"not an envelope"`)})
	_, err = c.OrderStatus(context.Background(), cred2, "mo_abc123")
	require.Error(t, err)
}
