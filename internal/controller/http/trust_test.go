package http_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheCustomersOrderShowsThePickupCodeOnlyWhileItIsOpen(t *testing.T) {
	a := ordersAPI(t)

	status, out := a.checkout("tok-rina", "k-code", a.checkoutBody(nil))
	require.Equal(t, 201, status)

	order := data(out)["order"].(map[string]any)
	require.Regexp(t, `^[0-9]{4}$`, order["pickupCode"], "shown at the cart to find the order")
	require.Nil(t, order["closeReasonCode"])

	id := order["id"].(string)

	status, _, _ = a.do("POST", "/v1/orders/"+id+"/cancel", nil, bearer("tok-rina"))
	require.Equal(t, 200, status)

	status, body, _ := a.do("GET", "/v1/orders/"+id, nil, bearer("tok-rina"))
	require.Equal(t, 200, status)

	closed := data(body)
	require.Nil(t, closed["pickupCode"], "a finished order has nothing to show")
	require.Equal(t, "CUSTOMER_CANCELLED", closed["closeReasonCode"])
}

func TestAdminTrustEndpoints(t *testing.T) {
	a := ordersAPI(t)
	admin := bearer(adminToken)

	status, out := a.checkout("tok-rina", "k-trust", a.checkoutBody(nil))
	require.Equal(t, 201, status)

	rina := data(out)["order"].(map[string]any)["id"].(string)

	var customer string
	require.NoError(t, a.e.Pool.QueryRow(a.e.Ctx, `SELECT customer_id FROM merchant_orders WHERE id = $1`, rina).Scan(&customer))

	t.Run("they need the operator token", func(t *testing.T) {
		for _, h := range []map[string]string{nil, bearer("tok-rina"), bearer("wrong")} {
			status, _, _ := a.do("GET", "/v1/admin/customers/"+customer+"/trust", nil, h)
			require.Equal(t, 401, status)

			status, _, _ = a.do("GET", "/v1/admin/trust/distribution", nil, h)
			require.Equal(t, 401, status)
		}
	})

	t.Run("a customer's report", func(t *testing.T) {
		status, body, _ := a.do("GET", "/v1/admin/customers/"+customer+"/trust", nil, admin)
		require.Equal(t, 200, status)

		report := data(body)
		require.Equal(t, "NEW", report["profile"].(map[string]any)["tier"])
		require.Contains(t, report, "policy")
		require.Contains(t, report, "timeline")
		require.Contains(t, report, "config")
	})

	t.Run("an unknown id is not found", func(t *testing.T) {
		status, _, _ := a.do("GET", "/v1/admin/customers/not-a-uuid/trust", nil, admin)
		require.Equal(t, 404, status)
	})

	t.Run("the distribution", func(t *testing.T) {
		status, body, _ := a.do("GET", "/v1/admin/trust/distribution", nil, admin)
		require.Equal(t, 200, status)
		require.Contains(t, data(body), "tiers")
	})
}
