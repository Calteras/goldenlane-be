package gateway

import (
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	_midtransSnapProduction = "https://app.midtrans.com"
	_midtransSnapSandbox    = "https://app.sandbox.midtrans.com"
	_midtransAPIProduction  = "https://api.midtrans.com"
	_midtransAPISandbox     = "https://api.sandbox.midtrans.com"
	// Midtrans times are Western Indonesian Time.
	_wib = 7 * time.Hour
)

// Midtrans is the Midtrans Snap API: a token and page per attempt. Its v2 API answers HTTP
// 200 with the real status inside the body, so every call reads status_code too.
type Midtrans struct {
	snap string // overrides both environments when set
	api  string
	http httpClient
}

// NewMidtrans builds the adapter; empty addresses use the real hosts.
func NewMidtrans(snap, api string) *Midtrans {
	return &Midtrans{snap: strings.TrimRight(snap, "/"), api: strings.TrimRight(api, "/"), http: newHTTP()}
}

func (m *Midtrans) snapURL(c Credentials) string {
	switch {
	case m.snap != "":
		return m.snap
	case c.Sandbox:
		return _midtransSnapSandbox
	default:
		return _midtransSnapProduction
	}
}

func (m *Midtrans) apiURL(c Credentials) string {
	switch {
	case m.api != "":
		return m.api
	case c.Sandbox:
		return _midtransAPISandbox
	default:
		return _midtransAPIProduction
	}
}

// call runs a v2 call and folds the status inside the body into an error.
func (m *Midtrans) call(ctx context.Context, c Credentials, method, path string, body any) ([]byte, int, error) {
	status, raw, err := m.http.do(ctx, method, m.apiURL(c)+path, c.Secret, body, nil)
	if err != nil {
		return nil, 0, err
	}

	var head struct {
		StatusCode string `json:"status_code"`
		Message    string `json:"status_message"`
	}

	_ = json.Unmarshal(raw, &head)

	code := status
	if head.StatusCode != "" {
		var n int
		if _, err := fmt.Sscanf(head.StatusCode, "%d", &n); err == nil {
			code = n
		}
	}

	return raw, code, nil
}

func midtransError(code int, raw []byte) error {
	var b struct {
		Message string `json:"status_message"`
	}

	_ = json.Unmarshal(raw, &b)

	return &Error{Status: code, Code: fmt.Sprintf("MIDTRANS_%d", code), Message: b.Message}
}

// Check asks about an order that does not exist: a valid key gets "not found", a wrong one is refused.
func (m *Midtrans) Check(ctx context.Context, c Credentials) error {
	raw, code, err := m.call(ctx, c, http.MethodGet, "/v2/sf-credential-check/status", nil)
	if err != nil {
		return err
	}

	if code == http.StatusNotFound || code == http.StatusOK {
		return nil
	}

	return midtransError(code, raw)
}

// Create asks Snap for a payment page.
func (m *Midtrans) Create(ctx context.Context, c Credentials, r ChargeRequest) (Charge, error) {
	minutes := int(time.Until(r.ExpiresAt).Minutes())
	if minutes < 1 {
		minutes = 1
	}

	body := map[string]any{
		"transaction_details": map[string]any{"order_id": r.ExternalID, "gross_amount": r.AmountRupiah},
		"expiry": map[string]any{
			"start_time": time.Now().UTC().Add(_wib).Format("2006-01-02 15:04:05") + " +0700",
			"unit":       "minutes", "duration": minutes,
		},
	}
	if r.CustomerName != "" {
		body["customer_details"] = map[string]string{"first_name": r.CustomerName}
	}

	if r.ReturnURL != "" {
		body["callbacks"] = map[string]string{"finish": r.ReturnURL}
	}

	status, raw, err := m.http.do(ctx, http.MethodPost, m.snapURL(c)+"/snap/v1/transactions", c.Secret, body, nil)
	if err != nil {
		return Charge{}, err
	}

	if status != http.StatusOK && status != http.StatusCreated {
		var b struct {
			Messages []string `json:"error_messages"`
		}

		_ = json.Unmarshal(raw, &b)

		return Charge{}, &Error{Status: status, Code: "MIDTRANS_SNAP", Message: strings.Join(b.Messages, "; ")}
	}

	var out struct {
		Token       string `json:"token"`
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.RedirectURL == "" {
		return Charge{}, fmt.Errorf("midtrans token is not readable: %w", errors.Join(err, errors.New(string(raw[:min(len(raw), 200)]))))
	}

	return Charge{ChargeID: r.ExternalID, PayURL: out.RedirectURL, ExpiresAt: r.ExpiresAt}, nil
}

// Get reads a transaction. One the customer has not started yet does not exist, which is pending.
func (m *Midtrans) Get(ctx context.Context, c Credentials, externalID, _ string) (Status, error) {
	raw, code, err := m.call(ctx, c, http.MethodGet, "/v2/"+url.PathEscape(externalID)+"/status", nil)
	if err != nil {
		return Status{}, err
	}

	var out struct {
		TransactionStatus string          `json:"transaction_status"`
		FraudStatus       string          `json:"fraud_status"`
		PaymentType       string          `json:"payment_type"`
		GrossAmount       json.RawMessage `json:"gross_amount"`
		SettlementTime    string          `json:"settlement_time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Status{}, fmt.Errorf("midtrans status is not readable: %w", err)
	}

	switch {
	case out.TransactionStatus == "" && code == http.StatusNotFound:
		return Status{State: StatePending}, nil
	case out.TransactionStatus == "" && code >= 400:
		return Status{}, midtransError(code, raw)
	}

	// A transaction that was denied (202) or expired (407) answers with an error-looking code but
	// a perfectly good status.
	st := Status{State: StatePending, PaidRupiah: rupiah(out.GrossAmount), Method: midtransMethod(out.PaymentType)}

	if t, err := time.Parse("2006-01-02 15:04:05", out.SettlementTime); err == nil {
		st.PaidAt = t.Add(-_wib).UTC()
	}

	switch out.TransactionStatus {
	case "settlement", "refund", "partial_refund":
		st.State = StatePaid
	case "capture":
		if out.FraudStatus == "" || out.FraudStatus == "accept" {
			st.State = StatePaid
		}
	case "deny", "cancel", "failure":
		st.State = StateFailed
	case "expire":
		st.State = StateExpired
	}

	return st, nil
}

func midtransMethod(t string) string {
	switch t {
	case "qris":
		return "QRIS"
	case "gopay", "shopeepay", "dana", "ovo", "linkaja":
		return "E_WALLET"
	case "bank_transfer", "echannel", "permata", "bca_klikpay":
		return "BANK_TRANSFER"
	default:
		return "OTHER"
	}
}

// Expire cancels a pending transaction. One that never started does not exist: nothing to do.
func (m *Midtrans) Expire(ctx context.Context, c Credentials, externalID, _ string) error {
	raw, code, err := m.call(ctx, c, http.MethodPost, "/v2/"+url.PathEscape(externalID)+"/expire", nil)
	if err != nil {
		return err
	}

	if code == http.StatusOK || code == http.StatusNotFound {
		return nil
	}

	return midtransError(code, raw)
}

// Refund returns a settled payment. Midtrans refunds only cards, GoPay, ShopeePay, DANA, OVO
// and QRIS; for anything else it refuses and the merchant must refund by hand.
func (m *Midtrans) Refund(ctx context.Context, c Credentials, r RefundRequest) (RefundResult, error) {
	raw, code, err := m.call(ctx, c, http.MethodPost, "/v2/"+url.PathEscape(r.ExternalID)+"/refund",
		map[string]any{"refund_key": r.Key, "amount": r.AmountRupiah, "reason": r.Reason})
	if err != nil {
		return RefundResult{}, err
	}

	switch code {
	case http.StatusOK:
		var out struct {
			ID json.Number `json:"refund_chargeback_id"`
		}

		_ = json.Unmarshal(raw, &out)

		return RefundResult{Ref: r.Key + ":" + out.ID.String(), Done: true}, nil
	case http.StatusNotAcceptable: // 406: this refund key was already used, so it was accepted
		return RefundResult{Ref: r.Key, Done: true}, nil
	default:
		return RefundResult{}, midtransError(code, raw)
	}
}

// Callback checks the signature of a notification and returns its order id.
func (m *Midtrans) Callback(c Credentials, _ func(string) string, body []byte) (string, error) {
	var b struct {
		OrderID     string `json:"order_id"`
		StatusCode  string `json:"status_code"`
		GrossAmount string `json:"gross_amount"`
		Signature   string `json:"signature_key"`
	}
	if err := json.Unmarshal(body, &b); err != nil || b.OrderID == "" || b.Signature == "" {
		return "", fmt.Errorf("%w: unreadable", ErrBadCallback)
	}

	sum := sha512.Sum512([]byte(b.OrderID + b.StatusCode + b.GrossAmount + c.Secret))
	want := hex.EncodeToString(sum[:])

	if subtle.ConstantTimeCompare([]byte(strings.ToLower(b.Signature)), []byte(want)) != 1 {
		return "", ErrBadCallback
	}

	return b.OrderID, nil
}
