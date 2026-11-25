package http_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	nethttp "net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	v1 "github.com/Calteras/streetfood-customer-api/internal/controller/http/v1"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
)

// ordersAPI is an API with a published kiosk and two signed-in customers.
func ordersAPI(t *testing.T) *api {
	t.Helper()

	a := newAPI(t)
	a.kiosk = a.e.PublishKiosk(t)

	return a
}

func (a *api) checkoutBody(over map[string]any) map[string]any {
	body := map[string]any{
		"outletId":    a.kiosk.Outlet.ID,
		"fulfillment": map[string]any{"type": "PICKUP"},
		"items": []map[string]any{{
			"productId": a.kiosk.Product, "variantId": a.kiosk.Variant, "modifierIds": []string{a.kiosk.Modifier}, "quantity": 2,
		}},
	}
	for k, v := range over {
		body[k] = v
	}

	return body
}

func (a *api) checkout(token, key string, body map[string]any) (int, map[string]any) {
	a.t.Helper()

	h := bearer(token)
	if key != "" {
		h["Idempotency-Key"] = key
	}

	status, out, _ := a.do("POST", "/v1/checkouts", body, h)

	return status, out
}

func data(m map[string]any) map[string]any { return m["data"].(map[string]any) }

func TestCheckoutOverHTTP(t *testing.T) {
	a := ordersAPI(t)

	status, body := a.checkout("", "k1", a.checkoutBody(nil))
	require.Equal(t, 401, status)
	require.Equal(t, "UNAUTHORIZED", body["code"])

	status, body = a.checkout("tok-rina", "", a.checkoutBody(nil))
	require.Equal(t, 400, status, "an idempotency key is required")
	require.Equal(t, "BAD_REQUEST", body["code"])

	status, body = a.checkout("tok-rina", "k1", a.checkoutBody(nil))
	require.Equal(t, 201, status)

	order := data(body)["order"].(map[string]any)
	require.Equal(t, "PENDING_ACCEPTANCE", order["status"])
	require.Equal(t, true, order["open"])
	require.Equal(t, true, order["canCancel"])
	require.Equal(t, "Kios Melati", order["outlet"].(map[string]any)["name"])
	require.Equal(t, "PAY_AT_PICKUP", order["paymentMode"])
	require.EqualValues(t, 1554000, order["pricing"].(map[string]any)["totalMinor"])
	require.Equal(t, "Es Teh Manis", order["items"].([]any)[0].(map[string]any)["name"])
	require.Nil(t, order["error"])
	require.Equal(t, "Rina", a.pos.Submitted[0].Customer.Name)

	status, again := a.checkout("tok-rina", "k1", a.checkoutBody(nil))
	require.Equal(t, 200, status, "a retry gets the same order, with 200 not 201")
	require.Equal(t, order["id"], data(again)["order"].(map[string]any)["id"])
	require.Len(t, a.pos.Submitted, 1)

	status, body = a.checkout("tok-rina", "k1", a.checkoutBody(map[string]any{"notes": "different"}))
	require.Equal(t, 409, status)
	require.Equal(t, "IDEMPOTENCY_CONFLICT", body["code"])
}

func TestCheckoutRefusals(t *testing.T) {
	a := ordersAPI(t)

	status, body, _ := a.do("POST", "/v1/checkouts", []byte(`{nope`), map[string]string{"Authorization": "Bearer tok-rina", "Idempotency-Key": "k"})
	require.Equal(t, 400, status)
	require.Equal(t, "BAD_REQUEST", body["code"])

	status, body = a.checkout("tok-rina", "k2", a.checkoutBody(map[string]any{"items": []map[string]any{{"productId": "nope", "quantity": 1}}}))
	require.Equal(t, 422, status)
	require.Equal(t, "ORDER_INVALID", body["code"])

	problem := data(body)["problems"].([]any)[0].(map[string]any)
	require.Equal(t, "PRODUCT_UNAVAILABLE", problem["code"])
	require.EqualValues(t, 0, problem["line"])

	status, body = a.checkout("tok-rina", "k3", a.checkoutBody(map[string]any{"fulfillment": map[string]any{"type": "DELIVERY"}}))
	require.Equal(t, 400, status)

	status, body = a.checkout("tok-rina", "k4", a.checkoutBody(map[string]any{"outletId": "00000000-0000-4000-8000-000000000000"}))
	require.Equal(t, 404, status)
	require.Equal(t, "NOT_FOUND", body["code"])

	_, err := a.e.Pool.Exec(a.e.Ctx, `UPDATE outlets SET online_status = 'CLOSED'`)
	require.NoError(t, err)

	status, body = a.checkout("tok-rina", "k5", a.checkoutBody(nil))
	require.Equal(t, 409, status)
	require.Equal(t, "OUTLET_CLOSED", body["code"])
	require.Empty(t, a.pos.Submitted, "refused before the merchant was asked")
}

func TestTheMerchantsRefusalReachesTheApp(t *testing.T) {
	a := ordersAPI(t)

	a.pos.SubmitErr = testutil.RealError(t, "submit-price-changed")
	status, body := a.checkout("tok-rina", "k1", a.checkoutBody(map[string]any{"expectedTotalMinor": 1}))
	require.Equal(t, 409, status)
	require.Equal(t, "PRICE_CHANGED", body["code"])
	require.EqualValues(t, 1554000, data(body)["totalMinor"], "the app can show the new price")
	require.Equal(t, "SUBMIT_FAILED", data(body)["order"].(map[string]any)["status"])

	a.pos.SubmitErr = testutil.RealError(t, "submit-invalid")
	status, body = a.checkout("tok-rina", "k2", a.checkoutBody(nil))
	require.Equal(t, 422, status)
	require.Equal(t, "ORDER_INVALID", body["code"])
	require.NotEmpty(t, data(body)["problems"], "and which lines")

	a.pos.SubmitErr = testutil.RealError(t, "submit-closed")
	status, body = a.checkout("tok-rina", "k3", a.checkoutBody(nil))
	require.Equal(t, 409, status)
	require.Equal(t, "OUTLET_CLOSED", body["code"])

	// An unreachable merchant is not an error: the order is kept and retried.
	a.pos.SubmitErr, a.pos.Down = nil, true
	status, body = a.checkout("tok-rina", "k4", a.checkoutBody(nil))
	require.Equal(t, 201, status)
	require.Equal(t, "CREATED", data(body)["order"].(map[string]any)["status"])
	require.Equal(t, true, data(body)["order"].(map[string]any)["open"])
}

func TestTooManyOrders(t *testing.T) {
	a := ordersAPI(t)

	for i := 0; i < 5; i++ {
		status, _ := a.checkout("tok-rina", "k"+string(rune('a'+i)), a.checkoutBody(nil))
		require.Equal(t, 201, status)
	}

	status, body := a.checkout("tok-rina", "kz", a.checkoutBody(nil))
	require.Equal(t, 429, status)
	require.Equal(t, "TOO_MANY_ORDERS", body["code"])

	status, _ = a.checkout("tok-budi", "kz", a.checkoutBody(nil))
	require.Equal(t, 201, status, "another customer is unaffected")
}

func TestReadingAndCancellingOrders(t *testing.T) {
	a := ordersAPI(t)

	_, first := a.checkout("tok-rina", "k1", a.checkoutBody(nil))
	_, second := a.checkout("tok-rina", "k2", a.checkoutBody(nil))
	_, theirs := a.checkout("tok-budi", "k1", a.checkoutBody(nil))
	id1 := data(first)["order"].(map[string]any)["id"].(string)
	id2 := data(second)["order"].(map[string]any)["id"].(string)
	idB := data(theirs)["order"].(map[string]any)["id"].(string)

	status, body, headers := a.do("GET", "/v1/orders", nil, bearer("tok-rina"))
	require.Equal(t, 200, status)
	require.Equal(t, "no-store", headers.Get("Cache-Control"))

	list := data(body)["orders"].([]any)
	require.Len(t, list, 2, "only their own")
	require.Equal(t, id2, list[0].(map[string]any)["id"], "newest first")

	status, body, _ = a.do("GET", "/v1/orders?limit=1&before="+list[0].(map[string]any)["createdAt"].(string), nil, bearer("tok-rina"))
	require.Equal(t, 200, status)
	require.Equal(t, id1, data(body)["orders"].([]any)[0].(map[string]any)["id"], "paging backwards")

	for _, q := range []string{"limit=0", "limit=x", "before=yesterday"} {
		status, _, _ = a.do("GET", "/v1/orders?"+q, nil, bearer("tok-rina"))
		require.Equal(t, 400, status, q)
	}

	status, body, _ = a.do("GET", "/v1/orders/"+id1, nil, bearer("tok-rina"))
	require.Equal(t, 200, status)
	require.Equal(t, id1, data(body)["id"])

	for _, path := range []string{"/v1/orders/" + idB, "/v1/orders/not-a-uuid", "/v1/orders/00000000-0000-4000-8000-000000000000"} {
		status, body, _ = a.do("GET", path, nil, bearer("tok-rina"))
		require.Equal(t, 404, status, path)
		require.Equal(t, "NOT_FOUND", body["code"], path)
	}

	status, _, _ = a.do("GET", "/v1/orders/"+id1, nil, nil)
	require.Equal(t, 401, status)

	status, _, _ = a.do("POST", "/v1/orders/"+idB+"/cancel", nil, bearer("tok-rina"))
	require.Equal(t, 404, status, "not theirs to cancel")

	status, body, _ = a.do("POST", "/v1/orders/"+id1+"/cancel", nil, bearer("tok-rina"))
	require.Equal(t, 200, status)
	require.Equal(t, "CANCELLED", data(body)["status"])
	require.Equal(t, false, data(body)["canCancel"])
	require.Equal(t, false, data(body)["open"])

	// The merchant accepts the other one: it can no longer be cancelled by the customer.
	require.NoError(t, a.orders.ApplyEvent(a.e.Ctx, 2, statusEvent(a.platformID(id2), "ACCEPTED")))

	status, body, _ = a.do("POST", "/v1/orders/"+id2+"/cancel", nil, bearer("tok-rina"))
	require.Equal(t, 409, status)
	require.Equal(t, "CANNOT_CANCEL", body["code"])
}

func (a *api) platformID(orderID string) string {
	a.t.Helper()

	var id string

	require.NoError(a.t, a.e.Pool.QueryRow(a.e.Ctx, `SELECT platform_order_id FROM merchant_orders WHERE id = $1`, orderID).Scan(&id))

	return id
}

func statusEvent(platformOrderID, status string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"platformOrderId": platformOrderID, "status": status})

	return raw
}

func TestReviewsOverHTTP(t *testing.T) {
	a := ordersAPI(t)

	_, placed := a.checkout("tok-rina", "k1", a.checkoutBody(nil))
	id := data(placed)["order"].(map[string]any)["id"].(string)

	status, body, _ := a.do("POST", "/v1/orders/"+id+"/review", map[string]any{"rating": 5}, bearer("tok-rina"))
	require.Equal(t, 400, status, "not completed yet")

	require.NoError(t, a.orders.ApplyEvent(a.e.Ctx, 5, statusEvent(a.platformID(id), "COMPLETED")))

	status, _, _ = a.do("POST", "/v1/orders/"+id+"/review", map[string]any{"rating": 9}, bearer("tok-rina"))
	require.Equal(t, 400, status)

	status, _, _ = a.do("POST", "/v1/orders/"+id+"/review", map[string]any{"rating": 5}, bearer("tok-budi"))
	require.Equal(t, 404, status)

	status, body, _ = a.do("POST", "/v1/orders/"+id+"/review", map[string]any{"rating": 5, "comment": "Enak"}, bearer("tok-rina"))
	require.Equal(t, 201, status)
	require.EqualValues(t, 5, data(body)["rating"])

	status, body, _ = a.do("POST", "/v1/orders/"+id+"/review", map[string]any{"rating": 4}, bearer("tok-rina"))
	require.Equal(t, 409, status)
	require.Equal(t, "ALREADY_REVIEWED", body["code"])

	// Reviews are public, and show only a first name.
	status, body, _ = a.do("GET", "/v1/outlets/"+a.kiosk.Outlet.ID+"/reviews", nil, nil)
	require.Equal(t, 200, status)

	reviews := data(body)["reviews"].([]any)
	require.Len(t, reviews, 1)
	require.Equal(t, "Rina", reviews[0].(map[string]any)["reviewer"])
	require.Equal(t, "Enak", reviews[0].(map[string]any)["comment"])

	status, _, _ = a.do("GET", "/v1/outlets/not-a-uuid/reviews", nil, nil)
	require.Equal(t, 404, status)

	// The rating shows on the outlet and in search.
	status, body, _ = a.do("GET", "/v1/outlets/"+a.kiosk.Outlet.ID, nil, nil)
	require.Equal(t, 200, status)
	require.EqualValues(t, 5, data(body)["rating"].(map[string]any)["average"])
	require.EqualValues(t, 1, data(body)["rating"].(map[string]any)["count"])

	status, body, _ = a.do("GET", "/v1/outlets/nearby?lat=-6.25&lng=106.85", nil, nil)
	require.Equal(t, 200, status)
	require.EqualValues(t, 1, data(body)["outlets"].([]any)[0].(map[string]any)["rating"].(map[string]any)["count"])
}

// ----------------------------------------------------------------- streaming

type sse struct {
	events chan string
	lines  chan string
	closed chan struct{}
}

// openStream serves the API on a real socket and connects to an order's stream.
func openStream(t *testing.T, a *api, token, orderID string) (*sse, int) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go func() { _ = a.app.Listener(ln) }()

	t.Cleanup(func() { _ = a.app.Shutdown() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := nethttp.NewRequestWithContext(ctx, "GET", "http://"+ln.Addr().String()+"/v1/orders/"+orderID+"/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := nethttp.DefaultClient.Do(req)
	require.NoError(t, err)

	s := &sse{events: make(chan string, 20), lines: make(chan string, 100), closed: make(chan struct{})}

	if resp.StatusCode != 200 {
		_ = resp.Body.Close()

		return s, resp.StatusCode
	}

	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	go func() {
		defer close(s.closed)
		defer resp.Body.Close()

		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			s.lines <- line

			if strings.HasPrefix(line, "data: ") {
				s.events <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	return s, 200
}

func (s *sse) next(t *testing.T, within time.Duration) map[string]any {
	t.Helper()

	select {
	case raw := <-s.events:
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &m))

		return m
	case <-time.After(within):
		t.Fatal("no event arrived")
	}

	return nil
}

func TestOrderStream(t *testing.T) {
	a := newAPIWith(t, apiOptions{streaming: v1.OrdersOptions{Heartbeat: 150 * time.Millisecond, MaxAge: 1500 * time.Millisecond}})
	a.kiosk = a.e.PublishKiosk(t)

	_, placed := a.checkout("tok-rina", "k1", a.checkoutBody(nil))
	id := data(placed)["order"].(map[string]any)["id"].(string)
	platformID := a.platformID(id)

	t.Run("only the owner may watch", func(t *testing.T) {
		_, status := openStream(t, a, "tok-budi", id)
		require.Equal(t, 404, status)
		_, status = openStream(t, a, "nope", id)
		require.Equal(t, 401, status)
	})

	t.Run("the first event is the current state, then every change, then a reconnect", func(t *testing.T) {
		s, status := openStream(t, a, "tok-rina", id)
		require.Equal(t, 200, status)

		first := s.next(t, 2*time.Second)
		require.Equal(t, "PENDING_ACCEPTANCE", first["status"])

		time.Sleep(300 * time.Millisecond) // the listener connects in the background

		require.NoError(t, a.orders.ApplyEvent(a.e.Ctx, 2, statusEvent(platformID, "ACCEPTED")))
		require.Equal(t, "ACCEPTED", s.next(t, 3*time.Second)["status"], "a change reaches the open stream at once")

		require.NoError(t, a.orders.ApplyEvent(a.e.Ctx, 4, statusEvent(platformID, "READY")))
		require.Equal(t, "READY", s.next(t, 3*time.Second)["status"])

		// Quiet moments carry a heartbeat comment so proxies keep the connection.
		var sawPing, sawReconnect bool

		deadline := time.After(4 * time.Second)

	read:
		for {
			select {
			case line := <-s.lines:
				sawPing = sawPing || line == ": ping"
				sawReconnect = sawReconnect || line == "event: reconnect"
			case <-s.closed:
				// Lines that arrived just before the end are still queued.
				for drained := false; !drained; {
					select {
					case line := <-s.lines:
						sawPing = sawPing || line == ": ping"
						sawReconnect = sawReconnect || line == "event: reconnect"
					default:
						drained = true
					}
				}

				break read
			case <-deadline:
				t.Fatal("the stream did not end by itself")
			}
		}

		require.True(t, sawPing)
		require.True(t, sawReconnect, "after its maximum age the stream asks the app to reconnect")
	})
}

func TestOrderStreamOutlivesTheServersWriteTimeout(t *testing.T) {
	a := newAPIWith(t, apiOptions{
		writeTimeout: 400 * time.Millisecond,
		streaming:    v1.OrdersOptions{Heartbeat: 100 * time.Millisecond, MaxAge: 2 * time.Second},
	})
	a.kiosk = a.e.PublishKiosk(t)

	_, placed := a.checkout("tok-rina", "k1", a.checkoutBody(nil))
	id := data(placed)["order"].(map[string]any)["id"].(string)

	s, status := openStream(t, a, "tok-rina", id)
	require.Equal(t, 200, status)
	require.Equal(t, "PENDING_ACCEPTANCE", s.next(t, 2*time.Second)["status"])

	// Five times the write timeout later the stream is still alive and still writing.
	time.Sleep(1500 * time.Millisecond)

	select {
	case <-s.closed:
		t.Fatal("the server cut the stream at its write timeout")
	default:
	}

	pings := 0

	for len(s.lines) > 0 {
		if <-s.lines == ": ping" {
			pings++
		}
	}

	require.Greater(t, pings, 8, "heartbeats kept flowing")
}
