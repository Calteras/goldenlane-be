package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const _xenditBase = "https://api.xendit.co"

// Xendit is the Xendit invoice API: one invoice per attempt, the customer pays on its page.
type Xendit struct {
	base string
	http httpClient
}

// NewXendit builds the adapter; base is empty for the real API.
func NewXendit(base string) *Xendit {
	if base == "" {
		base = _xenditBase
	}

	return &Xendit{base: strings.TrimRight(base, "/"), http: newHTTP()}
}

func (x *Xendit) fail(status int, raw []byte) error {
	var b struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
	}

	_ = json.Unmarshal(raw, &b)

	return &Error{Status: status, Code: b.ErrorCode, Message: b.Message}
}

// Check reads one invoice, which needs the same permission as creating them.
func (x *Xendit) Check(ctx context.Context, c Credentials) error {
	status, raw, err := x.http.do(ctx, http.MethodGet, x.base+"/v2/invoices?limit=1", c.Secret, nil, nil)
	if err != nil {
		return err
	}

	if status != http.StatusOK {
		return x.fail(status, raw)
	}

	if c.CallbackToken == "" {
		return errors.New("callbackToken is required for Xendit")
	}

	return nil
}

// Create opens an invoice.
func (x *Xendit) Create(ctx context.Context, c Credentials, r ChargeRequest) (Charge, error) {
	seconds := int(time.Until(r.ExpiresAt).Seconds())
	if seconds < 60 {
		seconds = 60
	}

	body := map[string]any{
		"external_id": r.ExternalID, "amount": r.AmountRupiah, "currency": "IDR",
		"description": r.Description, "invoice_duration": seconds,
	}
	if r.CustomerName != "" {
		body["customer"] = map[string]string{"given_names": r.CustomerName}
	}

	if r.ReturnURL != "" {
		body["success_redirect_url"] = r.ReturnURL
		body["failure_redirect_url"] = r.ReturnURL
	}

	status, raw, err := x.http.do(ctx, http.MethodPost, x.base+"/v2/invoices", c.Secret, body, nil)
	if err != nil {
		return Charge{}, err
	}

	if status != http.StatusOK && status != http.StatusCreated {
		return Charge{}, x.fail(status, raw)
	}

	var out struct {
		ID         string    `json:"id"`
		InvoiceURL string    `json:"invoice_url"`
		ExpiryDate time.Time `json:"expiry_date"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" || out.InvoiceURL == "" {
		return Charge{}, fmt.Errorf("xendit invoice is not readable: %w", errors.Join(err, errors.New(string(raw[:min(len(raw), 200)]))))
	}

	if out.ExpiryDate.IsZero() {
		out.ExpiryDate = r.ExpiresAt
	}

	return Charge{ChargeID: out.ID, PayURL: out.InvoiceURL, ExpiresAt: out.ExpiryDate}, nil
}

// Get reads an invoice.
func (x *Xendit) Get(ctx context.Context, c Credentials, _, chargeID string) (Status, error) {
	status, raw, err := x.http.do(ctx, http.MethodGet, x.base+"/v2/invoices/"+url.PathEscape(chargeID), c.Secret, nil, nil)
	if err != nil {
		return Status{}, err
	}

	if status != http.StatusOK {
		return Status{}, x.fail(status, raw)
	}

	var out struct {
		Status        string          `json:"status"`
		PaidAmount    json.RawMessage `json:"paid_amount"`
		PaymentMethod string          `json:"payment_method"`
		PaidAt        time.Time       `json:"paid_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Status{}, fmt.Errorf("xendit invoice is not readable: %w", err)
	}

	st := Status{State: StatePending, PaidRupiah: rupiah(out.PaidAmount), Method: xenditMethod(out.PaymentMethod), PaidAt: out.PaidAt}

	switch strings.ToUpper(out.Status) {
	case "PAID", "SETTLED":
		st.State = StatePaid
	case "EXPIRED":
		st.State = StateExpired
	}

	return st, nil
}

func xenditMethod(m string) string {
	switch strings.ToUpper(m) {
	case "QR_CODE", "QRIS":
		return "QRIS"
	case "EWALLET":
		return "E_WALLET"
	case "BANK_TRANSFER":
		return "BANK_TRANSFER"
	default:
		return "OTHER"
	}
}

// Expire closes an unpaid invoice.
func (x *Xendit) Expire(ctx context.Context, c Credentials, _, chargeID string) error {
	status, raw, err := x.http.do(ctx, http.MethodPost, x.base+"/invoices/"+url.PathEscape(chargeID)+"/expire!", c.Secret, nil, nil)
	if err != nil {
		return err
	}

	if status != http.StatusOK && status != http.StatusCreated {
		return x.fail(status, raw)
	}

	return nil
}

// Refund returns a paid invoice. The key makes a repeat harmless.
func (x *Xendit) Refund(ctx context.Context, c Credentials, r RefundRequest) (RefundResult, error) {
	body := map[string]any{"invoice_id": r.ChargeID, "amount": r.AmountRupiah, "reason": "CANCELLATION", "reference_id": r.Key}

	status, raw, err := x.http.do(ctx, http.MethodPost, x.base+"/refunds", c.Secret, body, map[string]string{"Idempotency-key": r.Key})
	if err != nil {
		return RefundResult{}, err
	}

	if status != http.StatusOK && status != http.StatusCreated {
		return RefundResult{}, x.fail(status, raw)
	}

	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return RefundResult{}, fmt.Errorf("xendit refund is not readable: %w", err)
	}

	switch strings.ToUpper(out.Status) {
	case "FAILED", "CANCELLED":
		return RefundResult{}, &Error{Status: http.StatusUnprocessableEntity, Code: "REFUND_" + strings.ToUpper(out.Status)}
	case "SUCCEEDED":
		return RefundResult{Ref: out.ID, Done: true}, nil
	default:
		return RefundResult{Ref: out.ID}, nil
	}
}

// Callback checks the callback token and returns the external id of the invoice.
func (x *Xendit) Callback(c Credentials, header func(string) string, body []byte) (string, error) {
	got := header("X-Callback-Token")
	if c.CallbackToken == "" || subtle.ConstantTimeCompare([]byte(got), []byte(c.CallbackToken)) != 1 {
		return "", ErrBadCallback
	}

	var b struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(body, &b); err != nil || b.ExternalID == "" {
		return "", fmt.Errorf("%w: no external_id", ErrBadCallback)
	}

	return b.ExternalID, nil
}
