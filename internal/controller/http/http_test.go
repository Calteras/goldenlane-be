package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/config"
	httpctl "github.com/Calteras/streetfood-customer-api/internal/controller/http"
	v1 "github.com/Calteras/streetfood-customer-api/internal/controller/http/v1"
	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/discovery"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/identity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/merchant"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/ordering"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/payment"
	"github.com/Calteras/streetfood-customer-api/pkg/httpserver"
	"github.com/Calteras/streetfood-customer-api/pkg/signing"
)

const adminToken = "a-long-operator-token-of-at-least-32-chars"

// tokens maps a bearer token to the claims a real sign-in provider would prove.
type tokens map[string]identity.Claims

type fakeVerifier struct{ tokens tokens }

func (f fakeVerifier) Verify(_ context.Context, token string) (identity.Claims, error) {
	if c, ok := f.tokens[token]; ok {
		return c, nil
	}

	return identity.Claims{}, errors.New("bad token")
}

type api struct {
	t      *testing.T
	e      *testutil.Env
	app    *fiber.App
	server *httpserver.Server
	pos    *testutil.FakeOrderPOS
	orders *ordering.UseCase
	pay    *payment.UseCase
	gw     *testutil.FakeGateway
	hub    *ordering.Hub
	kiosk  testutil.Kiosk
	cancel context.CancelFunc
}

type apiOptions struct {
	writeTimeout time.Duration
	streaming    v1.OrdersOptions
}

func newAPI(t *testing.T) *api { return newAPIWith(t, apiOptions{}) }

func newAPIWith(t *testing.T, opt apiOptions) *api {
	t.Helper()

	e := testutil.NewEnv(t)
	cfg := &config.Config{
		HTTP:  config.HTTP{CORSAllowOrigins: "http://localhost:5173"},
		Admin: config.Admin{Token: adminToken},
	}

	ident := identity.New(e.Store, fakeVerifier{tokens: tokens{
		"tok-rina": {UserID: "uid-rina", Phone: "+628123456789", Name: "Rina"},
		"tok-budi": {UserID: "uid-budi"},
	}})
	merch := merchant.New(e.Store, e.POS, e.Integration, e.Box, merchant.Settings{AllowedHosts: []string{"pos.example.com"}}, testutil.SilentLogger{})

	fake := testutil.NewFakeOrderPOS()
	gw := testutil.NewFakeGateway()
	pay := payment.New(e.Store, gw, e.Box, payment.Settings{
		Rates: entity.FeeRates{CustomerBps: 250, MerchantBps: 250}, Window: 15 * time.Minute, PublicBaseURL: "https://api.test",
	}, testutil.SilentLogger{})
	orders := ordering.New(e.Store, fake, pay, e.Box, testutil.SilentLogger{})
	pay.SetOrderNotifier(orders.Notification)
	e.Integration.SetOrderMirror(orders)

	hub := ordering.NewHub()
	ctx, cancel := context.WithCancel(context.Background())

	go hub.Run(ctx, e.Store.ListenOrderUpdates, testutil.SilentLogger{})
	t.Cleanup(cancel)

	var serverOpts []httpserver.Option
	if opt.writeTimeout > 0 {
		serverOpts = append(serverOpts, httpserver.WriteTimeout(opt.writeTimeout))
	}

	server := httpserver.New(serverOpts...)
	httpctl.NewRouter(server.App, cfg, testutil.SilentLogger{}, httpctl.Dependencies{
		Auth: ident, Events: e.Integration, Discovery: discovery.New(e.Store), Profiles: ident, Merchants: merch,
		Orders: orders, Hub: hub, Payments: pay, Streaming: opt.streaming,
	})

	return &api{t: t, e: e, app: server.App, server: server, pos: fake, orders: orders, pay: pay, gw: gw, hub: hub, cancel: cancel}
}

func (a *api) do(method, path string, body any, headers map[string]string) (int, map[string]any, nethttp.Header) {
	a.t.Helper()

	var reader io.Reader

	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		require.NoError(a.t, err)

		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := a.app.Test(req, -1)
	require.NoError(a.t, err)

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var out map[string]any
	_ = json.Unmarshal(raw, &out)

	return resp.StatusCode, out, resp.Header
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func (a *api) signedEvents(secret string, events ...map[string]any) (int, map[string]any) {
	a.t.Helper()

	body, err := json.Marshal(map[string]any{"events": events})
	require.NoError(a.t, err)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path := "/v1/integrations/pos/events"
	status, out, _ := a.do("POST", path, body, map[string]string{
		signing.HeaderKeyID: a.e.KeyID, signing.HeaderTimestamp: ts,
		signing.HeaderSignature: signing.Sign(secret, ts, "POST", path, body),
	})

	return status, out
}

func TestHealthAndUnknownRoutes(t *testing.T) {
	a := newAPI(t)

	status, _, _ := a.do("GET", "/healthz", nil, nil)
	require.Equal(t, 200, status)

	status, body, _ := a.do("GET", "/v1/nope", nil, nil)
	require.Equal(t, 404, status)
	require.Equal(t, false, body["status"])
}

func TestPOSEvents(t *testing.T) {
	a := newAPI(t)
	event := map[string]any{"eventId": "e1", "type": "outlet.updated", "outletId": "o1", "version": 1, "occurredAt": time.Now().UTC().Format(time.RFC3339), "data": map[string]any{
		"outlet": testutil.POSOutlet("o1", "Kios", 1, -6.2, 106.8, "FIXED"),
	}}

	status, _ := a.signedEvents(testutil.SecretText, event)
	require.Equal(t, 204, status)

	status, body := a.signedEvents("sfs_wrong", event)
	require.Equal(t, 401, status)
	require.Equal(t, "UNAUTHORIZED", body["code"])

	status, _, _ = a.do("POST", "/v1/integrations/pos/events", []byte(`{"events":[]}`), nil)
	require.Equal(t, 401, status, "unsigned requests are refused before anything is read")

	status, body = a.signedEvents(testutil.SecretText)
	require.Equal(t, 400, status, "a signed but empty batch is a bad request")
	require.Equal(t, "BAD_REQUEST", body["code"])

	// The signature binds the body: the same headers with a different body fail.
	good, err := json.Marshal(map[string]any{"events": []map[string]any{event}})
	require.NoError(t, err)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signing.Sign(testutil.SecretText, ts, "POST", "/v1/integrations/pos/events", good)
	tampered := bytes.Replace(good, []byte(`"e1"`), []byte(`"e2"`), 1)
	status, _, _ = a.do("POST", "/v1/integrations/pos/events", tampered, map[string]string{
		signing.HeaderKeyID: a.e.KeyID, signing.HeaderTimestamp: ts, signing.HeaderSignature: sig,
	})
	require.Equal(t, 401, status)

	a.e.Drain()
	require.True(t, a.e.Outlet("o1").Published)
}

func TestNearbyValidation(t *testing.T) {
	a := newAPI(t)

	for _, q := range []string{
		"", "lat=-6.2", "lng=106.8", "lat=abc&lng=106.8", "lat=91&lng=0", "lat=0&lng=181",
		"lat=-6.2&lng=106.8&radius_m=abc", "lat=-6.2&lng=106.8&radius_m=-5", "lat=-6.2&lng=106.8&limit=0", "lat=NaN&lng=1",
	} {
		status, body, _ := a.do("GET", "/v1/outlets/nearby?"+q, nil, nil)
		require.Equal(t, 400, status, q)
		require.Equal(t, "BAD_REQUEST", body["code"], q)
	}

	status, body, headers := a.do("GET", "/v1/outlets/nearby?lat=-6.2&lng=106.8", nil, nil)
	require.Equal(t, 200, status)
	require.Equal(t, true, body["status"])
	require.Equal(t, "no-store", headers.Get("Cache-Control"))
	require.Empty(t, body["data"].(map[string]any)["outlets"])
}

func TestBrowseOutlets(t *testing.T) {
	a := newAPI(t)
	t0 := time.Now().Add(-time.Hour)

	a.e.Apply(testutil.OutletEvent("o", testutil.POSOutlet("o1", "Kios Melati", 1, -6.2, 106.8, "FIXED"), t0))
	a.e.POS.Snapshots["o1"] = &entity.POSSnapshot{GeneratedAt: t0.Add(time.Minute), Outlet: testutil.POSOutlet("o1", "Kios Melati", 1, -6.2, 106.8, "FIXED"),
		Catalog: testutil.Catalog(2, entity.POSProduct{ID: "p1", Name: "Es Teh", PriceMinor: 500000})}
	a.e.Apply(testutil.Event{ID: "c", Type: "catalog.changed", Outlet: "o1", At: t0.Add(time.Minute), Data: map[string]any{}})

	status, body, _ := a.do("GET", "/v1/outlets/nearby?lat=-6.2&lng=106.8&q=melati", nil, nil)
	require.Equal(t, 200, status)

	list := body["data"].(map[string]any)["outlets"].([]any)
	require.Len(t, list, 1)

	first := list[0].(map[string]any)
	id := first["id"].(string)
	require.Equal(t, "Kios Melati", first["name"])
	require.Equal(t, true, first["acceptingOrders"])
	require.Equal(t, "FIXED", first["location"].(map[string]any)["mode"])

	status, body, _ = a.do("GET", "/v1/outlets/"+id, nil, nil)
	require.Equal(t, 200, status)
	require.Equal(t, "Warung Sri", body["data"].(map[string]any)["merchant"])

	status, body, headers := a.do("GET", "/v1/outlets/"+id+"/menu", nil, nil)
	require.Equal(t, 200, status)
	require.Equal(t, float64(2), body["data"].(map[string]any)["catalogVersion"])
	require.Contains(t, headers.Get("Cache-Control"), "max-age=15")
	require.Contains(t, body["data"].(map[string]any)["menu"].(map[string]any)["categories"].([]any)[0].(map[string]any)["products"].([]any)[0].(map[string]any)["name"], "Es Teh")

	for _, path := range []string{"/v1/outlets/not-a-uuid", "/v1/outlets/00000000-0000-4000-8000-000000000000", "/v1/outlets/00000000-0000-4000-8000-000000000000/menu"} {
		status, body, _ = a.do("GET", path, nil, nil)
		require.Equal(t, 404, status, path)
		require.Equal(t, "NOT_FOUND", body["code"], path)
	}
}

func TestCustomerAccount(t *testing.T) {
	a := newAPI(t)

	for _, h := range []map[string]string{nil, bearer("nope"), {"Authorization": "Basic abc"}, {"Authorization": "Bearer "}} {
		status, body, _ := a.do("GET", "/v1/me", nil, h)
		require.Equal(t, 401, status)
		require.Equal(t, "UNAUTHORIZED", body["code"])
	}

	status, body, _ := a.do("GET", "/v1/me", nil, bearer("tok-rina"))
	require.Equal(t, 200, status)

	me := body["data"].(map[string]any)
	require.Equal(t, "Rina", me["name"])
	require.Equal(t, "+628123456789", me["phone"], "the phone comes from the verified token")

	_, again, _ := a.do("GET", "/v1/me", nil, bearer("tok-rina"))
	require.Equal(t, me["id"], again["data"].(map[string]any)["id"], "the same person keeps one account")

	_, other, _ := a.do("GET", "/v1/me", nil, bearer("tok-budi"))
	require.NotEqual(t, me["id"], other["data"].(map[string]any)["id"])

	status, body, _ = a.do("PATCH", "/v1/me", map[string]any{"name": "  Rina S  ", "phone": "+6200"}, bearer("tok-rina"))
	require.Equal(t, 200, status)
	require.Equal(t, "Rina S", body["data"].(map[string]any)["name"], "trimmed")
	require.Equal(t, "+628123456789", body["data"].(map[string]any)["phone"], "the phone cannot be edited here")

	for _, bad := range []map[string]any{{"name": ""}, {"name": strings.Repeat("a", 81)}, {"avatarUrl": "http://insecure/x.png"}} {
		status, _, _ = a.do("PATCH", "/v1/me", bad, bearer("tok-rina"))
		require.Equal(t, 400, status)
	}

	status, _, _ = a.do("POST", "/v1/devices", map[string]any{"platform": "android", "token": "fcm-1"}, bearer("tok-rina"))
	require.Equal(t, 204, status)
	status, _, _ = a.do("POST", "/v1/devices", map[string]any{"platform": "palm", "token": "x"}, bearer("tok-rina"))
	require.Equal(t, 400, status)

	// Another account registering the same token takes it over.
	status, _, _ = a.do("POST", "/v1/devices", map[string]any{"platform": "ios", "token": "fcm-1"}, bearer("tok-budi"))
	require.Equal(t, 204, status)

	var owner string
	require.NoError(t, a.e.Pool.QueryRow(a.e.Ctx, `SELECT c.name FROM devices d JOIN customers c ON c.id = d.customer_id WHERE fcm_token = 'fcm-1'`).Scan(&owner))
	require.Empty(t, owner, "the token now belongs to the second account (no name)")

	// Deleting erases personal data and closes the account.
	status, _, _ = a.do("DELETE", "/v1/me", nil, bearer("tok-rina"))
	require.Equal(t, 204, status)

	status, body, _ = a.do("GET", "/v1/me", nil, bearer("tok-rina"))
	require.Equal(t, 403, status)
	require.Equal(t, "ACCOUNT_UNAVAILABLE", body["code"])

	var phone *string
	require.NoError(t, a.e.Pool.QueryRow(a.e.Ctx, `SELECT phone FROM customers WHERE id = $1`, me["id"]).Scan(&phone))
	require.Nil(t, phone)
}

func TestAdminMerchants(t *testing.T) {
	a := newAPI(t)
	admin := bearer(adminToken)
	reg := map[string]any{
		"name": "Kopi Senja", "posTenantId": "tenant-2", "keyId": "sfk_aaaaaaaaaaaaaaaaaaaaaaaa",
		"secret": "sfs_new_merchant_secret", "posBaseUrl": "https://pos.example.com",
	}

	for _, h := range []map[string]string{nil, bearer("wrong"), bearer(strings.ToUpper(adminToken)), {"Authorization": "Bearer"}} {
		status, _, _ := a.do("GET", "/v1/admin/merchants", nil, h)
		require.Equal(t, 401, status)
	}

	// A customer token is not an operator token.
	status, _, _ := a.do("GET", "/v1/admin/merchants", nil, bearer("tok-rina"))
	require.Equal(t, 401, status)

	// The POS must accept the credentials before they are stored.
	a.e.POS.Err = errors.New("401")
	status, body, _ := a.do("POST", "/v1/admin/merchants", reg, admin)
	require.Equal(t, 422, status)
	require.Equal(t, "LINK_UNVERIFIED", body["code"])
	a.e.POS.Err = nil

	for name, patch := range map[string]map[string]any{
		"host not allowed": {"posBaseUrl": "https://evil.example.org"},
		"plain http":       {"posBaseUrl": "http://pos.example.com"},
		"with a path":      {"posBaseUrl": "https://pos.example.com/app"},
		"with credentials": {"posBaseUrl": "https://user:pw@pos.example.com"},
		"internal address": {"posBaseUrl": "https://169.254.169.254"},
		"bad key id":       {"keyId": "abc"},
		"bad secret":       {"secret": "plain"},
		"no name":          {"name": " "},
		"no tenant":        {"posTenantId": ""},
	} {
		bad := map[string]any{}
		for k, v := range reg {
			bad[k] = v
		}

		for k, v := range patch {
			bad[k] = v
		}

		status, body, _ = a.do("POST", "/v1/admin/merchants", bad, admin)
		require.Equal(t, 422, status, name)
		require.Equal(t, "MERCHANT_INVALID", body["code"], name)
	}

	status, body, _ = a.do("POST", "/v1/admin/merchants", reg, admin)
	require.Equal(t, 201, status)
	require.NotContains(t, mustJSON(t, body), "sfs_new_merchant_secret", "the secret is never echoed")

	merchantID := body["data"].(map[string]any)["id"].(string)

	status, _, _ = a.do("POST", "/v1/admin/merchants", reg, admin)
	require.Equal(t, 409, status, "the same key or tenant cannot be registered twice")

	status, body, _ = a.do("GET", "/v1/admin/merchants", nil, admin)
	require.Equal(t, 200, status)
	require.NotContains(t, mustJSON(t, body), "sfs_new_merchant_secret")
	require.Contains(t, mustJSON(t, body), "Kopi Senja")

	var stored string
	require.NoError(t, a.e.Pool.QueryRow(a.e.Ctx, `SELECT secret_enc FROM merchant_links WHERE key_id = 'sfk_aaaaaaaaaaaaaaaaaaaaaaaa'`).Scan(&stored))
	require.NotContains(t, stored, "sfs_new_merchant_secret", "stored encrypted")

	require.GreaterOrEqual(t, a.e.POS.CallCount("outlets:sfk_aaaa"), 2, "verified, then synced right away")

	status, _, _ = a.do("DELETE", "/v1/admin/merchants/"+merchantID, nil, admin)
	require.Equal(t, 204, status)
	status, _, _ = a.do("DELETE", "/v1/admin/merchants/"+merchantID, nil, admin)
	require.Equal(t, 204, status, "suspending twice is harmless")
	status, _, _ = a.do("DELETE", "/v1/admin/merchants/not-a-uuid", nil, admin)
	require.Equal(t, 404, status)
	status, _, _ = a.do("DELETE", "/v1/admin/merchants/00000000-0000-4000-8000-000000000000", nil, admin)
	require.Equal(t, 404, status)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	require.NoError(t, err)

	return string(raw)
}

// eventsAs sends a signed (empty-ish) batch as the given merchant key and returns the status.
func (a *api) eventsAs(keyID, secret string) int {
	a.t.Helper()

	body := []byte(`{"events":[{"eventId":"e-` + keyID + secret + `","type":"unknown.event","occurredAt":"2026-11-21T00:00:00Z","data":{}}]}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path := "/v1/integrations/pos/events"
	status, _, _ := a.do("POST", path, body, map[string]string{
		signing.HeaderKeyID: keyID, signing.HeaderTimestamp: ts,
		signing.HeaderSignature: signing.Sign(secret, ts, "POST", path, body),
	})

	return status
}

func TestAdminUpdatesAMerchantsLink(t *testing.T) {
	a := newAPI(t)
	admin := bearer(adminToken)

	const (
		oldKey, oldSecret = "sfk_aaaaaaaaaaaaaaaaaaaaaaaa", "sfs_old_merchant_secret"
		newKey, newSecret = "sfk_bbbbbbbbbbbbbbbbbbbbbbbb", "sfs_new_merchant_secret"
	)

	status, body, _ := a.do("POST", "/v1/admin/merchants", map[string]any{
		"name": "Kopi Senja", "posTenantId": "tenant-2", "keyId": oldKey, "secret": oldSecret, "posBaseUrl": "https://pos.example.com",
	}, admin)
	require.Equal(t, 201, status)

	id := body["data"].(map[string]any)["id"].(string)
	path := "/v1/admin/merchants/" + id + "/link"
	update := func(over map[string]any) map[string]any {
		out := map[string]any{"keyId": newKey, "secret": newSecret, "posBaseUrl": "https://pos.example.com"}
		for k, v := range over {
			out[k] = v
		}

		return out
	}

	require.Equal(t, 204, a.eventsAs(oldKey, oldSecret), "the merchant works with its first credentials")

	status, _, _ = a.do("PUT", path, update(nil), nil)
	require.Equal(t, 401, status, "operators only")

	status, _, _ = a.do("PUT", "/v1/admin/merchants/not-a-uuid/link", update(nil), admin)
	require.Equal(t, 404, status)
	status, _, _ = a.do("PUT", "/v1/admin/merchants/00000000-0000-4000-8000-000000000000/link", update(nil), admin)
	require.Equal(t, 404, status)

	for name, over := range map[string]map[string]any{
		"bad key id":       {"keyId": "abc"},
		"bad secret":       {"secret": "plain"},
		"host not allowed": {"posBaseUrl": "https://evil.example.org"},
	} {
		status, body, _ = a.do("PUT", path, update(over), admin)
		require.Equal(t, 422, status, name)
		require.Equal(t, "MERCHANT_INVALID", body["code"], name)
	}

	// The POS must accept the new credentials, or the working ones stay.
	a.e.POS.Err = errors.New("401")
	status, body, _ = a.do("PUT", path, update(nil), admin)
	require.Equal(t, 422, status)
	require.Equal(t, "LINK_UNVERIFIED", body["code"])

	a.e.POS.Err = nil

	require.Equal(t, 204, a.eventsAs(oldKey, oldSecret), "a refused update changed nothing")

	// Another merchant's key cannot be taken.
	status, _, _ = a.do("PUT", path, update(map[string]any{"keyId": a.e.KeyID}), admin)
	require.Equal(t, 409, status)

	// A reconnect brings a new key id and secret.
	status, body, _ = a.do("PUT", path, update(nil), admin)
	require.Equal(t, 200, status)
	require.Equal(t, newKey, body["data"].(map[string]any)["keyId"])
	require.NotContains(t, mustJSON(t, body), newSecret, "the secret is never echoed")

	var stored string
	require.NoError(t, a.e.Pool.QueryRow(a.e.Ctx, `SELECT secret_enc FROM merchant_links WHERE merchant_id = $1`, id).Scan(&stored))
	require.NotContains(t, stored, newSecret, "stored encrypted")

	require.Equal(t, 401, a.eventsAs(oldKey, oldSecret), "the old credentials stop working")
	require.Equal(t, 204, a.eventsAs(newKey, newSecret))

	// A rotation keeps the key id and changes only the secret.
	const rotated = "sfs_rotated_merchant_secret"

	status, _, _ = a.do("PUT", path, update(map[string]any{"secret": rotated}), admin)
	require.Equal(t, 200, status)
	require.Equal(t, 401, a.eventsAs(newKey, newSecret))
	require.Equal(t, 204, a.eventsAs(newKey, rotated))

	// A suspended merchant stays off even with working credentials: only reinstating it can change that.
	status, _, _ = a.do("DELETE", "/v1/admin/merchants/"+id, nil, admin)
	require.Equal(t, 204, status)

	status, _, _ = a.do("PUT", path, update(map[string]any{"secret": "sfs_after_suspension"}), admin)
	require.Equal(t, 200, status)
	require.Equal(t, 401, a.eventsAs(newKey, "sfs_after_suspension"))
}
