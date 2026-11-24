package http_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/gateway"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
	"github.com/Calteras/streetfood-customer-api/pkg/signing"
)

func admin() map[string]string { return map[string]string{"Authorization": "Bearer " + adminToken} }

func (a *api) connectGateway() map[string]any {
	a.t.Helper()

	status, body, _ := a.do("PUT", "/v1/admin/merchants/"+a.e.Merchant.ID+"/gateway",
		map[string]any{"provider": "XENDIT", "secret": "xnd_development_hidden", "callbackToken": testutil.CallbackToken}, admin())
	require.Equal(a.t, 200, status, body)

	return data(body)
}

func (a *api) onlineBody() map[string]any {
	return a.checkoutBody(map[string]any{"payment": map[string]any{"mode": "ONLINE"}})
}

func orderOf(body map[string]any) map[string]any { return data(body)["order"].(map[string]any) }

func TestConnectingAGatewayOverHTTP(t *testing.T) {
	a := ordersAPI(t)
	path := "/v1/admin/merchants/" + a.e.Merchant.ID + "/gateway"

	status, _, _ := a.do("PUT", path, map[string]any{"provider": "XENDIT"}, nil)
	require.Equal(t, 401, status, "operators only")

	status, body, _ := a.do("PUT", path, map[string]any{"provider": "PAYPAL", "secret": "x"}, admin())
	require.Equal(t, 422, status)
	require.Equal(t, "GATEWAY_INVALID", body["code"])

	a.gw.CheckErr = &gateway.Error{Status: 401}
	status, body, _ = a.do("PUT", path, map[string]any{"provider": "MIDTRANS", "secret": "bad"}, admin())
	require.Equal(t, 422, status)
	require.Equal(t, "GATEWAY_UNVERIFIED", body["code"])
	a.gw.CheckErr = nil

	status, _, _ = a.do("GET", path, nil, admin())
	require.Equal(t, 404, status, "nothing connected yet")

	view := a.connectGateway()
	require.Equal(t, "XENDIT", view["provider"])
	require.Equal(t, "https://api.test/v1/integrations/payments/xendit/"+view["id"].(string), view["callbackUrl"])

	status, body, _ = a.do("GET", path, nil, admin())
	require.Equal(t, 200, status)

	raw, err := json.Marshal(body)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "xnd_development_hidden", "keys never leave the service")
	require.NotContains(t, string(raw), testutil.CallbackToken)
	require.NotContains(t, strings.ToLower(string(raw)), "credentials")

	status, body, _ = a.do("GET", "/v1/outlets/"+a.kiosk.Outlet.ID, nil, nil)
	require.Equal(t, 200, status)
	require.Equal(t, true, data(body)["acceptsOnlinePayment"], "the app learns it can offer online payment")

	status, _, _ = a.do("DELETE", path, nil, admin())
	require.Equal(t, 204, status)

	_, body, _ = a.do("GET", "/v1/outlets/"+a.kiosk.Outlet.ID, nil, nil)
	require.Equal(t, false, data(body)["acceptsOnlinePayment"])

	status, _, _ = a.do("PUT", "/v1/admin/merchants/not-a-uuid/gateway", map[string]any{"provider": "XENDIT"}, admin())
	require.Equal(t, 404, status)
}

func TestPayingOnlineOverHTTP(t *testing.T) {
	a := ordersAPI(t)

	status, body := a.checkout("tok-rina", "k0", a.onlineBody())
	require.Equal(t, 422, status, "the merchant has no gateway yet")
	require.Equal(t, "ONLINE_PAYMENT_UNAVAILABLE", body["code"])

	gw := a.connectGateway()

	status, body = a.checkout("tok-rina", "k1", a.onlineBody())
	require.Equal(t, 201, status, body)

	order := orderOf(body)
	require.Equal(t, "UNPAID", order["status"])
	require.Equal(t, "ONLINE", order["paymentMode"])
	require.Equal(t, true, order["open"])
	require.Equal(t, true, order["canPay"])
	require.Equal(t, true, order["canCancel"])
	require.NotNil(t, order["paymentDeadline"])
	require.EqualValues(t, 1554000, order["pricing"].(map[string]any)["totalMinor"])
	require.Empty(t, a.pos.Submitted, "the merchant sees nothing until it is paid")

	pay := order["payment"].(map[string]any)
	require.Equal(t, "PENDING", pay["status"])
	require.EqualValues(t, 1592900, pay["amountMinor"])
	require.EqualValues(t, 38900, pay["customerFeeMinor"], "the app can show the 2.5% fee")
	require.EqualValues(t, 1554000, pay["orderTotalMinor"])
	require.Contains(t, pay["payUrl"], "https://pay.test/sfp_")

	// The same request again is the same order and the same payment page.
	status, again := a.checkout("tok-rina", "k1", a.onlineBody())
	require.Equal(t, 200, status)
	require.Equal(t, pay["payUrl"], orderOf(again)["payment"].(map[string]any)["payUrl"])
	require.Len(t, a.gw.Charges, 1)

	// Gateway notifications: forged ones are refused, the real one releases the order.
	callback := "/v1/integrations/payments/xendit/" + gw["id"].(string)
	externalID, _ := a.gw.Only()
	notice := map[string]any{"external_id": externalID, "status": "PAID"}

	status, body, _ = a.do("POST", callback, notice, map[string]string{"X-Callback-Token": "forged"})
	require.Equal(t, 401, status)
	require.Equal(t, "UNAUTHORIZED", body["code"])

	status, _, _ = a.do("POST", "/v1/integrations/payments/xendit/00000000-0000-0000-0000-000000000000", notice, map[string]string{"X-Callback-Token": testutil.CallbackToken})
	require.Equal(t, 404, status)

	status, _, _ = a.do("POST", "/v1/integrations/payments/stripe/"+gw["id"].(string), notice, nil)
	require.Equal(t, 404, status)

	a.gw.Settle(externalID, gateway.StatePaid, "QRIS")

	status, _, _ = a.do("POST", callback, notice, map[string]string{"X-Callback-Token": testutil.CallbackToken})
	require.Equal(t, 200, status)

	status, body, _ = a.do("GET", "/v1/orders/"+order["id"].(string), nil, bearer("tok-rina"))
	require.Equal(t, 200, status)

	paid := data(body)
	require.Equal(t, "CREATED", paid["status"], "paid, on its way to the merchant")
	require.Equal(t, false, paid["canPay"])
	require.Equal(t, "PAID", paid["payment"].(map[string]any)["status"])
	require.Nil(t, paid["payment"].(map[string]any)["payUrl"], "a finished payment has no page to open")

	status, body, _ = a.do("POST", "/v1/orders/"+order["id"].(string)+"/payment", nil, bearer("tok-rina"))
	require.Equal(t, 409, status)
	require.Equal(t, "NOT_PAYABLE", body["code"])

	a.orders.ProcessSubmits(t.Context())

	_, body, _ = a.do("GET", "/v1/orders/"+order["id"].(string), nil, bearer("tok-rina"))
	require.Equal(t, "PENDING_ACCEPTANCE", data(body)["status"])
	require.Equal(t, "PREPAID", a.pos.Submitted[0].Payment["mode"])
}

func TestRetryingAFailedPaymentOverHTTP(t *testing.T) {
	a := ordersAPI(t)
	gw := a.connectGateway()

	_, body := a.checkout("tok-rina", "k1", a.onlineBody())
	order := orderOf(body)
	orderPath := "/v1/orders/" + order["id"].(string)

	status, _, _ := a.do("POST", orderPath+"/payment", nil, nil)
	require.Equal(t, 401, status)

	status, _, _ = a.do("POST", orderPath+"/payment", nil, bearer("tok-budi"))
	require.Equal(t, 404, status, "someone else's order")

	externalID, _ := a.gw.Only()
	a.gw.Settle(externalID, gateway.StateFailed, "")

	status, _, _ = a.do("POST", "/v1/integrations/payments/xendit/"+gw["id"].(string),
		map[string]any{"external_id": externalID}, map[string]string{"X-Callback-Token": testutil.CallbackToken})
	require.Equal(t, 200, status)

	_, body, _ = a.do("GET", orderPath, nil, bearer("tok-rina"))
	failed := data(body)
	require.Equal(t, "UNPAID", failed["status"], "stays unpaid")
	require.Equal(t, true, failed["canPay"], "the app shows a retry button")
	require.Equal(t, "FAILED", failed["payment"].(map[string]any)["status"])
	require.Equal(t, "PAYMENT_DECLINED", failed["payment"].(map[string]any)["error"].(map[string]any)["code"])

	status, body, _ = a.do("POST", orderPath+"/payment", nil, bearer("tok-rina"))
	require.Equal(t, 200, status)

	retried := data(body)["payment"].(map[string]any)
	require.EqualValues(t, 2, retried["attempt"])
	require.Equal(t, "PENDING", retried["status"])
	require.NotNil(t, retried["payUrl"])

	// Cancelling an unpaid order is free.
	status, body, _ = a.do("POST", orderPath+"/cancel", nil, bearer("tok-rina"))
	require.Equal(t, 200, status)
	require.Equal(t, "CANCELLED", data(body)["status"])
	require.Equal(t, false, data(body)["canPay"])
}

func TestOnlineCheckoutRefusals(t *testing.T) {
	a := ordersAPI(t)
	a.connectGateway()

	wrong := a.checkoutBody(map[string]any{"payment": map[string]any{"mode": "ONLINE"}, "expectedTotalMinor": 1})
	status, body := a.checkout("tok-rina", "k1", wrong)
	require.Equal(t, 409, status)
	require.Equal(t, "PRICE_CHANGED", body["code"])
	require.EqualValues(t, 1554000, data(body)["totalMinor"])

	a.pos.QuoteErr = testutil.RealError(t, "submit-closed")
	status, body = a.checkout("tok-rina", "k2", a.onlineBody())
	require.Equal(t, 409, status)
	require.Equal(t, "OUTLET_CLOSED", body["code"])

	a.pos.QuoteErr = nil
	a.pos.Down = true
	status, body = a.checkout("tok-rina", "k3", a.onlineBody())
	require.Equal(t, 503, status)
	require.Equal(t, "MERCHANT_UNREACHABLE", body["code"])
	a.pos.Down = false

	status, body = a.checkout("tok-rina", "k4", a.checkoutBody(map[string]any{"payment": map[string]any{"mode": "BARTER"}}))
	require.Equal(t, 400, status)
	require.Equal(t, "BAD_REQUEST", body["code"])
	require.Empty(t, a.gw.Charges, "nobody was asked to pay")
}

func TestWhatAMerchantOwesOverHTTP(t *testing.T) {
	a := ordersAPI(t)
	a.connectGateway()

	_, body := a.checkout("tok-rina", "k1", a.onlineBody())
	order := orderOf(body)

	externalID, _ := a.gw.Only()
	a.gw.Settle(externalID, gateway.StatePaid, "QRIS")
	require.NoError(t, a.pay.Callback(t.Context(), "xendit", a.gatewayID(), func(string) string { return testutil.CallbackToken }, []byte(`{"external_id":"`+externalID+`"}`)))

	a.orders.ProcessSubmits(t.Context())

	var platformOrderID string
	require.NoError(t, a.e.Pool.QueryRow(t.Context(), `SELECT platform_order_id FROM merchant_orders WHERE id = $1`, order["id"]).Scan(&platformOrderID))

	version := a.pos.Set(platformOrderID, "COMPLETED")
	require.NoError(t, a.orders.ApplyEvent(t.Context(), version, []byte(`{"platformOrderId":"`+platformOrderID+`","status":"COMPLETED"}`)))

	path := "/v1/admin/merchants/" + a.e.Merchant.ID + "/fees"

	status, _, _ := a.do("GET", path, nil, nil)
	require.Equal(t, 401, status)

	status, body, _ = a.do("GET", path, nil, admin())
	require.Equal(t, 200, status)

	st := data(body)
	require.EqualValues(t, 1, st["orders"])
	require.EqualValues(t, 1554000, st["salesMinor"])
	require.EqualValues(t, 38900, st["customerFeeMinor"])
	require.EqualValues(t, 38900, st["merchantFeeMinor"])
	require.EqualValues(t, 77800, st["totalOwedMinor"])
	require.Len(t, st["entries"], 2)

	status, body, _ = a.do("GET", path+"?from=2020-01-01&to=2020-02-01", nil, admin())
	require.Equal(t, 200, status)
	require.EqualValues(t, 0, data(body)["orders"])

	for _, q := range []string{"?from=yesterday", "?to=never", "?from=2026-02-01&to=2026-01-01"} {
		status, _, _ = a.do("GET", path+q, nil, admin())
		require.Equal(t, 400, status, q)
	}

	status, _, _ = a.do("GET", "/v1/admin/merchants/"+"00000000-0000-0000-0000-000000000000"+"/fees", nil, admin())
	require.Equal(t, 404, status)
}

func (a *api) gatewayID() string {
	a.t.Helper()

	g, err := a.e.Store.GatewayByMerchant(a.t.Context(), a.e.Merchant.ID)
	require.NoError(a.t, err)

	return g.ID
}

func (a *api) signedPOS(method, path string, body []byte) map[string]string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	return map[string]string{
		signing.HeaderKeyID: a.e.KeyID, signing.HeaderTimestamp: ts,
		signing.HeaderSignature: signing.Sign(testutil.SecretText, ts, method, path, body),
	}
}

func TestFeeCollectionEndpointsOverHTTP(t *testing.T) {
	a := ordersAPI(t)

	// A pay-at-pickup order that was completed owes the merchant fee.
	_, body := a.checkout("tok-rina", "k1", a.checkoutBody(nil))
	order := orderOf(body)

	var platformOrderID string
	require.NoError(t, a.e.Pool.QueryRow(t.Context(), `SELECT platform_order_id FROM merchant_orders WHERE id = $1`, order["id"]).Scan(&platformOrderID))

	version := a.pos.Set(platformOrderID, "COMPLETED")
	require.NoError(t, a.orders.ApplyEvent(t.Context(), version, []byte(`{"platformOrderId":"`+platformOrderID+`","status":"COMPLETED"}`)))

	const pending = "/v1/integrations/pos/fees/pending"

	status, _, _ := a.do("GET", pending, nil, nil)
	require.Equal(t, 401, status, "only the merchant's POS")

	status, _, _ = a.do("GET", pending, nil, map[string]string{signing.HeaderKeyID: a.e.KeyID, signing.HeaderTimestamp: "1", signing.HeaderSignature: "00"})
	require.Equal(t, 401, status)

	status, got, _ := a.do("GET", pending, nil, a.signedPOS("GET", pending, nil))
	require.Equal(t, 200, status)
	require.EqualValues(t, 38900, data(got)["totalMinor"])
	require.Equal(t, "PICKUP_ORDERS", data(got)["lines"].([]any)[0].(map[string]any)["code"])

	const reserve = "/v1/integrations/pos/fees/reserve"

	reserveBody := []byte(`{"billingRef":"sub-1:2026-12-01"}`)

	status, _, _ = a.do("POST", reserve, reserveBody, nil)
	require.Equal(t, 401, status)

	status, got, _ = a.do("POST", reserve, []byte(`{}`), a.signedPOS("POST", reserve, []byte(`{}`)))
	require.Equal(t, 400, status)
	require.Equal(t, "BAD_REQUEST", got["code"])

	status, got, _ = a.do("POST", reserve, reserveBody, a.signedPOS("POST", reserve, reserveBody))
	require.Equal(t, 200, status)
	require.EqualValues(t, 38900, data(got)["totalMinor"])
	require.Equal(t, "sub-1:2026-12-01", data(got)["billingRef"])

	// The operator's view and manual settlement.
	feesPath := "/v1/admin/merchants/" + a.e.Merchant.ID + "/fees"
	_, got, _ = a.do("GET", feesPath, nil, admin())
	require.EqualValues(t, 38900, data(got)["outstandingMinor"])
	require.EqualValues(t, 0, data(got)["settledMinor"])

	settlePath := feesPath + "/settle"
	through := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	status, _, _ = a.do("POST", settlePath, map[string]any{"through": through, "note": "x"}, nil)
	require.Equal(t, 401, status)

	status, got, _ = a.do("POST", settlePath, map[string]any{"through": "someday", "note": "x"}, admin())
	require.Equal(t, 400, status)

	status, got, _ = a.do("POST", settlePath, map[string]any{"through": through}, admin())
	require.Equal(t, 400, status, "a note is required")
	require.Equal(t, "BAD_REQUEST", got["code"])

	status, got, _ = a.do("POST", settlePath, map[string]any{"through": through, "note": "transfer"}, admin())
	require.Equal(t, 200, status)
	require.EqualValues(t, 0, data(got)["entries"], "the reserved fees belong to the charge in flight")
}
