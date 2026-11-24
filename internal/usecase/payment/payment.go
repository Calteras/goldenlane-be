// Package payment takes online payments with each merchant's own gateway account (Xendit or
// Midtrans). The money goes straight to the merchant. The platform records its fees in a ledger
// when the order is fulfilled, and returns the money of orders that will not be.
package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/persistent"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/gateway"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
	"github.com/Calteras/streetfood-customer-api/pkg/secretbox"
)

const (
	_batch         = 10
	_pollAfter     = 20 * time.Second
	_refundLease   = 2 * time.Minute
	_maxRefundTry  = 8
	_gatewayTimout = 12 * time.Second
)

var (
	// ErrNoGateway means the merchant cannot take online payments.
	ErrNoGateway = errors.New("this merchant does not take online payments")
	// ErrInvalid means a gateway setup request is malformed.
	ErrInvalid = errors.New("invalid gateway setup")
	// ErrUnverified means the gateway did not accept the credentials.
	ErrUnverified = errors.New("the gateway did not accept these credentials")
)

var _refundBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour}

// Store is what the use case needs from storage.
type Store interface {
	LinkByMerchant(ctx context.Context, merchantID string) (entity.Link, error)
	GatewayByMerchant(ctx context.Context, merchantID string) (*entity.Gateway, error)
	GatewayByID(ctx context.Context, id string) (*entity.Gateway, error)
	SaveGateway(ctx context.Context, merchantID, provider, credentialsEnc string, sandbox bool) (*entity.Gateway, error)
	DeactivateGateway(ctx context.Context, merchantID string) error

	OrderByID(ctx context.Context, id string) (*entity.Order, error)
	CustomerOrder(ctx context.Context, customerID, id string) (*entity.Order, error)
	StartAttempt(ctx context.Context, in persistent.NewAttempt) (*entity.Payment, bool, error)
	SetCharge(ctx context.Context, paymentID, chargeID, payURL string, expiresAt time.Time) error
	FailAttempt(ctx context.Context, paymentID, code, message string) error
	PaymentByID(ctx context.Context, id string) (*entity.Payment, error)
	PaymentByExternal(ctx context.Context, gatewayID, externalID string) (*entity.Payment, error)
	ApplyCharge(ctx context.Context, paymentID string, u persistent.ChargeUpdate, notify persistent.PaymentNotify) (persistent.ChargeResult, error)
	PendingPayments(ctx context.Context, olderThan time.Duration, limit int) ([]*entity.Payment, error)
	TouchPayment(ctx context.Context, id string) error
	ExpiredUnpaid(ctx context.Context, limit int) ([]*entity.Order, error)
	OpenAttempts(ctx context.Context, orderID string) ([]*entity.Payment, error)
	ExpireUnpaid(ctx context.Context, orderID string, notify persistent.Notify) (bool, []*entity.Payment, error)
	CancelUnpaid(ctx context.Context, customerID, orderID string, notify persistent.Notify) (bool, []*entity.Payment, error)
	ClaimRefunds(ctx context.Context, limit int, lease time.Duration) ([]string, error)
	FinishRefund(ctx context.Context, paymentID string, out persistent.RefundOutcome, notify persistent.PaymentNotify) error
	FeeStatement(ctx context.Context, merchantID string, from, to time.Time) (entity.FeeStatement, error)
	SettleFeesManually(ctx context.Context, merchantID string, through time.Time, note string) (int, int64, error)
}

// Providers finds the adapter of a gateway.
type Providers interface {
	For(provider string) gateway.Provider
}

// Settings tune payments.
type Settings struct {
	Rates entity.FeeRates
	// Window is how long a customer has to pay an order.
	Window time.Duration
	// ReturnURL is where gateways send the customer after paying (a deep link into the app).
	ReturnURL string
	// PublicBaseURL is this API's address, to tell merchants where gateways should call back.
	PublicBaseURL string
}

// UseCase is the payment service.
type UseCase struct {
	store     Store
	providers Providers
	box       *secretbox.Box
	set       Settings
	log       logger.Interface
	now       func() time.Time
	// orderNote builds the push for an order that expired or was cancelled unpaid.
	orderNote persistent.Notify
}

// New builds the use case.
func New(store Store, providers Providers, box *secretbox.Box, set Settings, log logger.Interface) *UseCase {
	if set.Window <= 0 {
		set.Window = 15 * time.Minute
	}

	return &UseCase{store: store, providers: providers, box: box, set: set, log: log, now: time.Now}
}

// SetOrderNotifier tells the use case how orders announce their own state changes.
func (u *UseCase) SetOrderNotifier(n persistent.Notify) { u.orderNote = n }

// Deadline is when an order placed now stops waiting for payment.
func (u *UseCase) Deadline(now time.Time) time.Time { return now.Add(u.set.Window) }

// Rates are the platform's fees.
func (u *UseCase) Rates() entity.FeeRates { return u.set.Rates }

// -------------------------------------------------------------- gateway setup

// GatewayInput is what an operator submits to connect a merchant's gateway account.
type GatewayInput struct {
	Provider      string
	Secret        string
	CallbackToken string
	Sandbox       bool
}

// GatewayView is a gateway as shown to the operator. It never carries the keys.
type GatewayView struct {
	entity.Gateway
	// CallbackURL is the address the merchant must enter in the gateway's dashboard.
	CallbackURL string `json:"callbackUrl"`
}

type sealed struct {
	Secret        string `json:"secret"`
	CallbackToken string `json:"callbackToken,omitempty"`
}

func (u *UseCase) view(g *entity.Gateway) GatewayView {
	return GatewayView{Gateway: *g, CallbackURL: strings.TrimRight(u.set.PublicBaseURL, "/") + "/v1/integrations/payments/" + strings.ToLower(g.Provider) + "/" + g.ID}
}

// SetGateway connects (or replaces) a merchant's gateway account after proving the keys work.
func (u *UseCase) SetGateway(ctx context.Context, merchantID string, in GatewayInput) (GatewayView, error) {
	in.Provider = strings.ToUpper(strings.TrimSpace(in.Provider))
	in.Secret = strings.TrimSpace(in.Secret)
	in.CallbackToken = strings.TrimSpace(in.CallbackToken)

	provider := u.providers.For(in.Provider)

	switch {
	case provider == nil:
		return GatewayView{}, fmt.Errorf("%w: provider must be XENDIT or MIDTRANS", ErrInvalid)
	case in.Secret == "" || len(in.Secret) > 300:
		return GatewayView{}, fmt.Errorf("%w: secret is required", ErrInvalid)
	case in.Provider == entity.ProviderXendit && (in.CallbackToken == "" || len(in.CallbackToken) > 300):
		return GatewayView{}, fmt.Errorf("%w: callbackToken is required for Xendit", ErrInvalid)
	}

	if _, err := u.store.LinkByMerchant(ctx, merchantID); err != nil {
		return GatewayView{}, err
	}

	checkCtx, cancel := context.WithTimeout(ctx, _gatewayTimout)
	defer cancel()

	if err := provider.Check(checkCtx, gateway.Credentials{Secret: in.Secret, CallbackToken: in.CallbackToken, Sandbox: in.Sandbox}); err != nil {
		return GatewayView{}, fmt.Errorf("%w: %s", ErrUnverified, err.Error())
	}

	raw, err := json.Marshal(sealed{Secret: in.Secret, CallbackToken: in.CallbackToken})
	if err != nil {
		return GatewayView{}, err
	}

	enc, err := u.box.Seal(string(raw), merchantID)
	if err != nil {
		return GatewayView{}, err
	}

	g, err := u.store.SaveGateway(ctx, merchantID, in.Provider, enc, in.Sandbox)
	if err != nil {
		return GatewayView{}, err
	}

	return u.view(g), nil
}

// Gateway shows a merchant's gateway.
func (u *UseCase) Gateway(ctx context.Context, merchantID string) (GatewayView, error) {
	g, err := u.store.GatewayByMerchant(ctx, merchantID)
	if err != nil {
		return GatewayView{}, err
	}

	return u.view(g), nil
}

// RemoveGateway stops new online payments for a merchant.
func (u *UseCase) RemoveGateway(ctx context.Context, merchantID string) error {
	return u.store.DeactivateGateway(ctx, merchantID)
}

// open unseals a gateway's keys.
func (u *UseCase) open(g *entity.Gateway) (gateway.Provider, gateway.Credentials, error) {
	provider := u.providers.For(g.Provider)
	if provider == nil {
		return nil, gateway.Credentials{}, fmt.Errorf("unknown provider %q", g.Provider)
	}

	plain, err := u.box.Open(g.CredentialsEnc, g.MerchantID)
	if err != nil {
		return nil, gateway.Credentials{}, fmt.Errorf("gateway keys unreadable: %w", err)
	}

	var s sealed
	if err := json.Unmarshal([]byte(plain), &s); err != nil {
		return nil, gateway.Credentials{}, fmt.Errorf("gateway keys unreadable: %w", err)
	}

	return provider, gateway.Credentials{Secret: s.Secret, CallbackToken: s.CallbackToken, Sandbox: g.Sandbox}, nil
}

// ------------------------------------------------------------------- charging

// Start opens a payment attempt for an unpaid order and asks the gateway for a payment page.
// It is safe to repeat: while an attempt is open it is returned as it is. When the gateway
// cannot be reached the attempt is recorded as failed and returned, so the customer sees it and
// can retry; only an order that cannot be paid at all is an error.
func (u *UseCase) Start(ctx context.Context, o *entity.Order) (*entity.Payment, error) {
	if o.Status != entity.OrderUnpaid || o.TotalMinor == nil {
		return nil, entity.ErrNotPayable
	}

	g, err := u.store.GatewayByMerchant(ctx, o.MerchantID)
	if err != nil || !g.Active {
		return nil, ErrNoGateway
	}

	provider, creds, err := u.open(g)
	if err != nil {
		return nil, err
	}

	fees := u.set.Rates.Compute(*o.TotalMinor)

	p, created, err := u.store.StartAttempt(ctx, persistent.NewAttempt{
		OrderID: o.ID, Gateway: g, Rates: u.set.Rates, Fees: fees, OrderTotal: *o.TotalMinor,
	})
	if err != nil || !created {
		return p, err
	}

	chargeCtx, cancel := context.WithTimeout(ctx, _gatewayTimout)
	defer cancel()

	expires := u.now().Add(u.set.Window)
	if p.ExpiresAt != nil {
		expires = *p.ExpiresAt
	}

	charge, err := provider.Create(chargeCtx, creds, gateway.ChargeRequest{
		ExternalID: p.ExternalID, AmountRupiah: p.AmountMinor / 100, Description: "Pesanan di " + o.OutletName,
		ExpiresAt: expires, ReturnURL: u.set.ReturnURL,
	})
	if err != nil {
		code := "GATEWAY_UNAVAILABLE"

		var gerr *gateway.Error
		if errors.As(err, &gerr) && gerr.Permanent() {
			code = "GATEWAY_REJECTED"
		}

		u.log.Warn("payment page not created", "order", o.ID, "provider", g.Provider, "error", err.Error())

		if ferr := u.store.FailAttempt(ctx, p.ID, code, err.Error()); ferr != nil {
			return nil, ferr
		}

		return u.store.PaymentByID(ctx, p.ID)
	}

	if err := u.store.SetCharge(ctx, p.ID, charge.ChargeID, charge.PayURL, charge.ExpiresAt); err != nil {
		return nil, err
	}

	return u.store.PaymentByID(ctx, p.ID)
}

// Retry starts a new attempt on a customer's unpaid order (or returns the open one).
func (u *UseCase) Retry(ctx context.Context, customerID, orderID string) (*entity.Order, error) {
	o, err := u.store.CustomerOrder(ctx, customerID, orderID)
	if err != nil {
		return nil, err
	}

	if o.PaymentMode != entity.PaymentModeOnline || o.Status != entity.OrderUnpaid {
		return nil, entity.ErrNotPayable
	}

	if _, err := u.Start(ctx, o); err != nil {
		return nil, err
	}

	return u.store.CustomerOrder(ctx, customerID, orderID)
}

// Cancel withdraws an order the customer has not paid and closes its payment pages.
func (u *UseCase) Cancel(ctx context.Context, customerID, orderID string) (*entity.Order, error) {
	done, closed, err := u.store.CancelUnpaid(ctx, customerID, orderID, nil)
	if err != nil {
		return nil, err
	}

	if !done {
		return nil, entity.ErrCannotCancel
	}

	u.closeAtGateway(ctx, closed)

	return u.store.CustomerOrder(ctx, customerID, orderID)
}

// closeAtGateway tells gateways to stop accepting payment on attempts we gave up. Best
// effort: a payment that still arrives is refunded.
func (u *UseCase) closeAtGateway(ctx context.Context, attempts []*entity.Payment) {
	for _, p := range attempts {
		g, err := u.store.GatewayByID(ctx, p.GatewayID)
		if err != nil {
			continue
		}

		provider, creds, err := u.open(g)
		if err != nil {
			continue
		}

		chargeID := p.ExternalID
		if p.ProviderChargeID != nil {
			chargeID = *p.ProviderChargeID
		} else if g.Provider == entity.ProviderXendit {
			continue // no invoice was ever made
		}

		callCtx, cancel := context.WithTimeout(ctx, _gatewayTimout)
		if err := provider.Expire(callCtx, creds, p.ExternalID, chargeID); err != nil {
			u.log.Warn("payment page not closed", "payment", p.ID, "error", err.Error())
		}

		cancel()
	}
}

// ------------------------------------------------------------------- results

// Callback handles a gateway notification. It proves the notification came from the gateway,
// then asks the gateway for the payment's real state and uses that, so a forged or stale
// notification can change nothing.
func (u *UseCase) Callback(ctx context.Context, provider, gatewayID string, header func(string) string, body []byte) error {
	g, err := u.store.GatewayByID(ctx, gatewayID)
	if err != nil || !strings.EqualFold(g.Provider, provider) {
		return entity.ErrNotFound
	}

	adapter, creds, err := u.open(g)
	if err != nil {
		return err
	}

	externalID, err := adapter.Callback(creds, header, body)
	if err != nil {
		return entity.ErrUnauthorized
	}

	p, err := u.store.PaymentByExternal(ctx, g.ID, externalID)
	if errors.Is(err, entity.ErrNotFound) {
		return nil // a test ping, or a payment that is not ours
	}

	if err != nil {
		return err
	}

	return u.reconcile(ctx, g, adapter, creds, p)
}

// reconcile asks the gateway about an attempt and records the answer.
func (u *UseCase) reconcile(ctx context.Context, g *entity.Gateway, adapter gateway.Provider, creds gateway.Credentials, p *entity.Payment) error {
	chargeID := p.ExternalID
	if p.ProviderChargeID != nil {
		chargeID = *p.ProviderChargeID
	}

	callCtx, cancel := context.WithTimeout(ctx, _gatewayTimout)
	defer cancel()

	st, err := adapter.Get(callCtx, creds, p.ExternalID, chargeID)
	if err != nil {
		return fmt.Errorf("ask gateway: %w", err)
	}

	update := persistent.ChargeUpdate{Method: st.Method}

	switch st.State {
	case gateway.StatePending:
		return u.store.TouchPayment(ctx, p.ID)
	case gateway.StatePaid:
		update.State = entity.PaymentPaid
		if !st.PaidAt.IsZero() {
			update.PaidAt = &st.PaidAt
		}

		if st.PaidRupiah != 0 && st.PaidRupiah*100 < p.AmountMinor {
			// Less than asked for: do not release the order. The merchant sees the money
			// in their account and can settle it by hand.
			u.log.Error("payment is less than the charge", "payment", p.ID, "paid", st.PaidRupiah, "charge", p.AmountMinor/100)

			update = persistent.ChargeUpdate{State: entity.PaymentFailed, Code: "AMOUNT_MISMATCH", Message: "paid amount is less than the charge"}
		}
	case gateway.StateFailed:
		update = persistent.ChargeUpdate{State: entity.PaymentFailed, Code: "PAYMENT_DECLINED", Message: "the gateway declined the payment"}
	case gateway.StateExpired:
		update = persistent.ChargeUpdate{State: entity.PaymentExpired}
	}

	res, err := u.store.ApplyCharge(ctx, p.ID, update, u.paymentNote)
	if err != nil {
		return err
	}

	u.closeAtGateway(ctx, res.Closed)

	return nil
}

// Poll asks gateways about attempts whose notification never came.
func (u *UseCase) Poll(ctx context.Context) (bool, error) {
	pending, err := u.store.PendingPayments(ctx, _pollAfter, _batch)
	if err != nil {
		return false, err
	}

	for _, p := range pending {
		g, err := u.store.GatewayByID(ctx, p.GatewayID)
		if err != nil {
			u.log.Error("payment without a gateway", "payment", p.ID, "error", err.Error())

			continue
		}

		adapter, creds, err := u.open(g)
		if err != nil {
			u.log.Error("gateway keys unreadable", "payment", p.ID, "error", err.Error())

			continue
		}

		if err := u.reconcile(ctx, g, adapter, creds, p); err != nil {
			u.log.Warn("payment not checked", "payment", p.ID, "error", err.Error())
			// Look again later, not in a tight loop.
			if terr := u.store.TouchPayment(ctx, p.ID); terr != nil {
				return false, terr
			}
		}
	}

	return len(pending) == _batch, nil
}

// ExpireUnpaid closes orders nobody paid in time. Before it does, it asks the gateway once more,
// so a payment that was made but not yet reported is not lost.
func (u *UseCase) ExpireUnpaid(ctx context.Context) (bool, error) {
	orders, err := u.store.ExpiredUnpaid(ctx, _batch)
	if err != nil {
		return false, err
	}

	for _, o := range orders {
		attempts, err := u.store.OpenAttempts(ctx, o.ID)
		if err != nil {
			return false, err
		}

		for _, p := range attempts {
			if g, err := u.store.GatewayByID(ctx, p.GatewayID); err == nil {
				if adapter, creds, err := u.open(g); err == nil {
					if err := u.reconcile(ctx, g, adapter, creds, p); err != nil {
						u.log.Warn("last check before expiry failed", "payment", p.ID, "error", err.Error())
					}
				}
			}
		}

		_, closed, err := u.store.ExpireUnpaid(ctx, o.ID, u.orderNote)
		if err != nil {
			return false, err
		}

		u.closeAtGateway(ctx, closed)
	}

	return len(orders) == _batch, nil
}

// ------------------------------------------------------------------- refunds

// Refund returns the money of paid orders that will not be fulfilled. A gateway that refuses
// for good leaves the payment marked so the merchant, who holds the money, can refund by hand.
func (u *UseCase) Refund(ctx context.Context) (bool, error) {
	ids, err := u.store.ClaimRefunds(ctx, _batch, _refundLease)
	if err != nil {
		return false, err
	}

	for _, id := range ids {
		u.refundOne(ctx, id)
	}

	return len(ids) == _batch, nil
}

func (u *UseCase) refundOne(ctx context.Context, id string) {
	p, err := u.store.PaymentByID(ctx, id)
	if err != nil {
		u.log.Error("refund: payment unreadable", "payment", id, "error", err.Error())

		return
	}

	out := u.sendRefund(ctx, p)
	if !out.Done && !out.Fail && p.RefundAttempts >= _maxRefundTry {
		out = persistent.RefundOutcome{Fail: true, Error: "gave up after repeated errors: " + out.Error}
	}

	if out.Fail {
		u.log.Error("refund needs to be done by the merchant", "payment", p.ID, "error", out.Error)
	}

	if err := u.store.FinishRefund(ctx, p.ID, out, u.paymentNote); err != nil {
		u.log.Error("refund result not stored", "payment", p.ID, "error", err.Error())
	}
}

func (u *UseCase) sendRefund(ctx context.Context, p *entity.Payment) persistent.RefundOutcome {
	retry := func(err error) persistent.RefundOutcome {
		// RefundAttempts already counts this try.
		wait := _refundBackoff[min(max(p.RefundAttempts-1, 0), len(_refundBackoff)-1)]

		return persistent.RefundOutcome{Retry: wait, Error: err.Error()}
	}

	g, err := u.store.GatewayByID(ctx, p.GatewayID)
	if err != nil {
		return retry(err)
	}

	adapter, creds, err := u.open(g)
	if err != nil {
		return retry(err)
	}

	chargeID := p.ExternalID
	if p.ProviderChargeID != nil {
		chargeID = *p.ProviderChargeID
	}

	callCtx, cancel := context.WithTimeout(ctx, _gatewayTimout)
	defer cancel()

	res, err := adapter.Refund(callCtx, creds, gateway.RefundRequest{
		ExternalID: p.ExternalID, ChargeID: chargeID, AmountRupiah: p.AmountMinor / 100,
		Reason: "Order was not fulfilled", Key: "sfr_" + strings.ReplaceAll(p.ID, "-", ""),
	})
	if err != nil {
		var gerr *gateway.Error
		if errors.As(err, &gerr) && gerr.Permanent() {
			return persistent.RefundOutcome{Fail: true, Error: err.Error()}
		}

		return retry(err)
	}

	return persistent.RefundOutcome{Done: true, Ref: res.Ref}
}

// ---------------------------------------------------------------------- fees

// FeeStatement totals what a merchant owes in [from, to).
func (u *UseCase) FeeStatement(ctx context.Context, merchantID string, from, to time.Time) (entity.FeeStatement, error) {
	if _, err := u.store.LinkByMerchant(ctx, merchantID); err != nil {
		return entity.FeeStatement{}, err
	}

	return u.store.FeeStatement(ctx, merchantID, from, to)
}

// ManualSettlement is the result of marking fees collected by hand.
type ManualSettlement struct {
	Entries     int   `json:"entries"`
	AmountMinor int64 `json:"amountMinor"`
}

// SettleFeesManually marks a merchant's fees collected outside the subscription charge, because
// the operator invoiced them directly. Fees already set aside for a subscription charge in
// flight are left alone.
func (u *UseCase) SettleFeesManually(ctx context.Context, merchantID string, through time.Time, note string) (ManualSettlement, error) {
	note = strings.TrimSpace(note)
	if note == "" || len(note) > 300 {
		return ManualSettlement{}, fmt.Errorf("%w: a note saying how it was collected is required", entity.ErrBadRequest)
	}

	if _, err := u.store.LinkByMerchant(ctx, merchantID); err != nil {
		return ManualSettlement{}, err
	}

	n, total, err := u.store.SettleFeesManually(ctx, merchantID, through, note)

	return ManualSettlement{Entries: n, AmountMinor: total}, err
}

// ------------------------------------------------------------ push messages

func rupiah(minor int64) string {
	n := minor / 100
	s := fmt.Sprintf("%d", n)

	var out []byte

	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, '.')
		}

		out = append(out, c)
	}

	return "Rp " + string(out)
}

// paymentNote decides which payment events the customer is told about.
func (u *UseCase) paymentNote(o *entity.Order, p *entity.Payment) *entity.Notification {
	n := &entity.Notification{
		CustomerID: o.CustomerID, OrderID: o.ID,
		Data: map[string]string{"type": "payment", "orderId": o.ID, "status": o.Status, "paymentStatus": p.Status},
	}

	switch {
	case p.RefundStatus != nil && *p.RefundStatus == entity.RefundDone:
		n.Kind, n.Title = "REFUNDED", "Dana dikembalikan"
		n.Body = rupiah(p.AmountMinor) + " untuk pesanan di " + o.OutletName + " dikembalikan ke metode pembayaranmu"
	case p.RefundStatus != nil && *p.RefundStatus == entity.RefundFailed:
		n.Kind, n.Title = "REFUND_FAILED", "Pengembalian dana diproses merchant"
		n.Body = "Pesanan di " + o.OutletName + " tidak dapat dilayani. " + o.OutletName + " akan mengembalikan " + rupiah(p.AmountMinor) + " secara manual"
	case p.Status == entity.PaymentPaid:
		n.Kind, n.Title = "PAYMENT_PAID", "Pembayaran berhasil"
		n.Body = "Pesananmu dikirim ke " + o.OutletName + ". Menunggu konfirmasi merchant"
	case p.Status == entity.PaymentFailed:
		n.Kind, n.Title = fmt.Sprintf("PAYMENT_FAILED_%d", p.Attempt), "Pembayaran gagal"
		n.Body = "Pembayaran untuk " + o.OutletName + " belum berhasil. Pesananmu belum dibayar, ketuk untuk mencoba lagi"
	default:
		return nil
	}

	return n
}
