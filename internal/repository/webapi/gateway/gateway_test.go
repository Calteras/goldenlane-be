package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/gateway"
)

type seen struct {
	Method, Path, User string
	Header             http.Header
	Body               map[string]any
}

// stub serves canned answers and records what it was asked.
func stub(t *testing.T, answer func(r *http.Request) (int, string)) (*httptest.Server, *[]seen) {
	t.Helper()

	var calls []seen

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var body map[string]any

		_ = json.Unmarshal(raw, &body)

		user, _, _ := r.BasicAuth()
		calls = append(calls, seen{Method: r.Method, Path: r.URL.Path, User: user, Header: r.Header.Clone(), Body: body})

		status, out := answer(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)

	return srv, &calls
}

func TestXenditInvoices(t *testing.T) {
	srv, calls := stub(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/invoices":
			return 200, `{"id":"inv-1","external_id":"sfp_1","status":"PENDING","invoice_url":"https://checkout.xendit.co/web/inv-1","expiry_date":"2026-11-21T10:15:00.000Z"}`
		case r.Method == "GET" && r.URL.Path == "/v2/invoices/inv-1":
			return 200, `{"id":"inv-1","status":"SETTLED","paid_amount":15929,"payment_method":"QR_CODE","paid_at":"2026-11-21T10:05:00.000Z"}`
		case r.URL.Path == "/invoices/inv-1/expire!":
			return 200, `{}`
		case r.URL.Path == "/refunds":
			return 200, `{"id":"rfd-1","status":"PENDING"}`
		}

		return 404, `{"error_code":"NOT_FOUND","message":"nope"}`
	})
	x := gateway.NewXendit(srv.URL)
	cred := gateway.Credentials{Secret: "xnd_secret", CallbackToken: "tok"}
	ctx := context.Background()

	charge, err := x.Create(ctx, cred, gateway.ChargeRequest{
		ExternalID: "sfp_1", AmountRupiah: 15929, Description: "Pesanan di Kios", CustomerName: "Rina",
		ExpiresAt: time.Now().Add(15 * time.Minute), ReturnURL: "streetfood://paid",
	})
	require.NoError(t, err)
	require.Equal(t, "inv-1", charge.ChargeID)
	require.Equal(t, "https://checkout.xendit.co/web/inv-1", charge.PayURL)
	require.False(t, charge.ExpiresAt.IsZero())

	create := (*calls)[0]
	require.Equal(t, "xnd_secret", create.User, "the secret key is the basic-auth user name")
	require.Equal(t, "sfp_1", create.Body["external_id"])
	require.EqualValues(t, 15929, create.Body["amount"])
	require.Equal(t, "IDR", create.Body["currency"])
	require.InDelta(t, 900, create.Body["invoice_duration"], 5, "seconds")
	require.Equal(t, "streetfood://paid", create.Body["success_redirect_url"])

	st, err := x.Get(ctx, cred, "sfp_1", "inv-1")
	require.NoError(t, err)
	require.Equal(t, gateway.StatePaid, st.State, "SETTLED counts as paid")
	require.EqualValues(t, 15929, st.PaidRupiah)
	require.Equal(t, "QRIS", st.Method)
	require.False(t, st.PaidAt.IsZero())

	require.NoError(t, x.Expire(ctx, cred, "sfp_1", "inv-1"))

	res, err := x.Refund(ctx, cred, gateway.RefundRequest{ExternalID: "sfp_1", ChargeID: "inv-1", AmountRupiah: 15929, Key: "sfr_1"})
	require.NoError(t, err)
	require.Equal(t, "rfd-1", res.Ref)
	require.False(t, res.Done, "pending at the gateway")

	refund := (*calls)[3]
	require.Equal(t, "sfr_1", refund.Header.Get("Idempotency-Key"), "a repeated refund cannot pay out twice")
	require.Equal(t, "inv-1", refund.Body["invoice_id"])
	require.EqualValues(t, 15929, refund.Body["amount"])

	_, err = x.Get(ctx, cred, "x", "missing")

	var gerr *gateway.Error

	require.ErrorAs(t, err, &gerr)
	require.Equal(t, "NOT_FOUND", gerr.Code)
	require.True(t, gerr.Permanent())
}

func TestXenditStates(t *testing.T) {
	for name, tc := range map[string]struct {
		status string
		want   gateway.State
	}{"pending": {"PENDING", gateway.StatePending}, "paid": {"PAID", gateway.StatePaid}, "expired": {"EXPIRED", gateway.StateExpired}} {
		srv, _ := stub(t, func(*http.Request) (int, string) { return 200, `{"status":"` + tc.status + `"}` })
		st, err := gateway.NewXendit(srv.URL).Get(context.Background(), gateway.Credentials{Secret: "k"}, "x", "i")
		require.NoError(t, err, name)
		require.Equal(t, tc.want, st.State, name)
	}
}

func TestXenditRefundFailureIsAnError(t *testing.T) {
	srv, _ := stub(t, func(*http.Request) (int, string) { return 200, `{"id":"r","status":"FAILED"}` })
	_, err := gateway.NewXendit(srv.URL).Refund(context.Background(), gateway.Credentials{Secret: "k"}, gateway.RefundRequest{ChargeID: "i", Key: "k"})

	var gerr *gateway.Error

	require.ErrorAs(t, err, &gerr)
	require.True(t, gerr.Permanent(), "a refund the gateway failed will not work on retry")
}

func TestXenditCallback(t *testing.T) {
	x := gateway.NewXendit("")
	cred := gateway.Credentials{CallbackToken: "secret-token"}
	body := []byte(`{"id":"inv-1","external_id":"sfp_1","status":"PAID"}`)

	id, err := x.Callback(cred, func(string) string { return "secret-token" }, body)
	require.NoError(t, err)
	require.Equal(t, "sfp_1", id)

	for name, token := range map[string]string{"wrong": "other", "missing": ""} {
		_, err := x.Callback(cred, func(string) string { return token }, body)
		require.ErrorIs(t, err, gateway.ErrBadCallback, name)
	}

	_, err = x.Callback(gateway.Credentials{}, func(string) string { return "" }, body)
	require.ErrorIs(t, err, gateway.ErrBadCallback, "an account with no token accepts nothing")

	_, err = x.Callback(cred, func(string) string { return "secret-token" }, []byte(`{}`))
	require.ErrorIs(t, err, gateway.ErrBadCallback)
}

func TestMidtransSnapAndStatus(t *testing.T) {
	srv, calls := stub(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/snap/v1/transactions":
			return 201, `{"token":"tok-1","redirect_url":"https://app.sandbox.midtrans.com/snap/v4/redirection/tok-1"}`
		case r.URL.Path == "/v2/sfp_paid/status":
			return 200, `{"status_code":"200","transaction_status":"settlement","payment_type":"qris","gross_amount":"15929.00","settlement_time":"2026-11-21 17:05:00"}`
		case r.URL.Path == "/v2/sfp_new/status":
			return 200, `{"status_code":"404","status_message":"Transaction doesn't exist."}`
		case r.URL.Path == "/v2/sfp_capture/status":
			return 200, `{"status_code":"200","transaction_status":"capture","fraud_status":"challenge","payment_type":"credit_card","gross_amount":"100.00"}`
		case r.URL.Path == "/v2/sfp_denied/status":
			return 200, `{"status_code":"202","transaction_status":"deny","gross_amount":"100.00"}`
		case r.URL.Path == "/v2/sfp_expired/status":
			return 200, `{"status_code":"407","transaction_status":"expire","gross_amount":"100.00"}`
		case r.URL.Path == "/v2/sfp_bad/status":
			return 200, `{"status_code":"401","status_message":"Unauthorized"}`
		case r.URL.Path == "/v2/sfp_paid/expire":
			return 200, `{"status_code":"200"}`
		}

		return 200, `{"status_code":"404"}`
	})
	m := gateway.NewMidtrans(srv.URL, srv.URL)
	cred := gateway.Credentials{Secret: "SB-Mid-server-key", Sandbox: true}
	ctx := context.Background()

	charge, err := m.Create(ctx, cred, gateway.ChargeRequest{ExternalID: "sfp_paid", AmountRupiah: 15929, CustomerName: "Rina", ExpiresAt: time.Now().Add(15 * time.Minute)})
	require.NoError(t, err)
	require.Equal(t, "sfp_paid", charge.ChargeID, "Midtrans is addressed by our own order id")
	require.Contains(t, charge.PayURL, "tok-1")

	create := (*calls)[0]
	require.Equal(t, "SB-Mid-server-key", create.User)

	details := create.Body["transaction_details"].(map[string]any)
	require.Equal(t, "sfp_paid", details["order_id"])
	require.EqualValues(t, 15929, details["gross_amount"])

	expiry := create.Body["expiry"].(map[string]any)
	require.Equal(t, "minutes", expiry["unit"])
	require.InDelta(t, 14, expiry["duration"], 1)
	require.Regexp(t, `^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \+0700$`, expiry["start_time"], "the expiry starts now, in the format Midtrans documents")

	for id, want := range map[string]gateway.State{
		"sfp_paid": gateway.StatePaid, "sfp_new": gateway.StatePending, "sfp_capture": gateway.StatePending,
		"sfp_denied": gateway.StateFailed, "sfp_expired": gateway.StateExpired,
	} {
		st, err := m.Get(ctx, cred, id, "")
		require.NoError(t, err, id)
		require.Equal(t, want, st.State, id)
	}

	st, err := m.Get(ctx, cred, "sfp_paid", "")
	require.NoError(t, err)
	require.EqualValues(t, 15929, st.PaidRupiah)
	require.Equal(t, "QRIS", st.Method)
	require.Equal(t, 10, st.PaidAt.Hour(), "17:05 in Jakarta is 10:05 UTC")

	_, err = m.Get(ctx, cred, "sfp_bad", "")

	var gerr *gateway.Error

	require.ErrorAs(t, err, &gerr, "a refusal inside a 200 answer is still a refusal")
	require.Equal(t, 401, gerr.Status)
	require.False(t, gerr.Permanent(), "a key problem may be fixed, so it is retried")

	require.NoError(t, m.Expire(ctx, cred, "sfp_paid", ""))
	require.NoError(t, m.Expire(ctx, cred, "sfp_new", ""), "nothing to close when the customer never started")
}

func TestMidtransCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		wantErr bool
	}{
		"good key sees an unknown order": {`{"status_code":"404"}`, false},
		"bad key":                        {`{"status_code":"401","status_message":"Access denied"}`, true},
	} {
		srv, _ := stub(t, func(*http.Request) (int, string) { return 200, tc.body })
		err := gateway.NewMidtrans(srv.URL, srv.URL).Check(context.Background(), gateway.Credentials{Secret: "k"})
		require.Equal(t, tc.wantErr, err != nil, name)
	}
}

func TestMidtransRefund(t *testing.T) {
	for name, tc := range map[string]struct {
		body      string
		ok        bool
		permanent bool
	}{
		"refunded":              {`{"status_code":"200","refund_chargeback_id":7}`, true, false},
		"already refunded":      {`{"status_code":"406","status_message":"Duplicate refund ID"}`, true, false},
		"not a refundable kind": {`{"status_code":"412","status_message":"Merchant cannot modify the status of the transaction"}`, false, true},
		"bad amount":            {`{"status_code":"414"}`, false, true},
		"refunds not enabled":   {`{"status_code":"403"}`, false, false},
	} {
		srv, calls := stub(t, func(*http.Request) (int, string) { return 200, tc.body })
		res, err := gateway.NewMidtrans(srv.URL, srv.URL).Refund(context.Background(), gateway.Credentials{Secret: "k"},
			gateway.RefundRequest{ExternalID: "sfp_1", AmountRupiah: 15929, Key: "sfr_1", Reason: "r"})

		call := (*calls)[0]
		require.Equal(t, "/v2/sfp_1/refund", call.Path, name)
		require.Equal(t, "sfr_1", call.Body["refund_key"], name)
		require.EqualValues(t, 15929, call.Body["amount"], name)

		if tc.ok {
			require.NoError(t, err, name)
			require.True(t, res.Done, name)

			continue
		}

		var gerr *gateway.Error

		require.ErrorAs(t, err, &gerr, name)
		require.Equal(t, tc.permanent, gerr.Permanent(), name)
	}
}

func TestMidtransCallbackSignature(t *testing.T) {
	m := gateway.NewMidtrans("", "")
	cred := gateway.Credentials{Secret: "SB-Mid-server-key"}

	// SHA-512 of order_id + status_code + gross_amount + server key, as Midtrans documents.
	const signature = "86f54458b938a4381da9139ba5f56961ffd86bc28e6e87de2f3c2b3cd280375cdd0af6bc966cc8a3a3569c0edfcaafe59480aca4cfb40b0aa48fcfac8733247f"

	body := func(sig string) []byte {
		raw, _ := json.Marshal(map[string]string{"order_id": "order-1", "status_code": "200", "gross_amount": "15929.00", "signature_key": sig})

		return raw
	}

	id, err := m.Callback(cred, nil, body(signature))
	require.NoError(t, err)
	require.Equal(t, "order-1", id)

	_, err = m.Callback(cred, nil, body("00"+signature[2:]))
	require.ErrorIs(t, err, gateway.ErrBadCallback)

	_, err = m.Callback(gateway.Credentials{Secret: "another-key"}, nil, body(signature))
	require.ErrorIs(t, err, gateway.ErrBadCallback, "signed with someone else's key")

	_, err = m.Callback(cred, nil, []byte(`{"order_id":"order-1","status_code":"200","gross_amount":"1.00"}`))
	require.ErrorIs(t, err, gateway.ErrBadCallback, "unsigned")
}

func TestRegistry(t *testing.T) {
	r := gateway.NewRegistry(gateway.Endpoints{})
	require.NotNil(t, r.For("XENDIT"))
	require.NotNil(t, r.For("MIDTRANS"))
	require.Nil(t, r.For("PAYPAL"))
}
