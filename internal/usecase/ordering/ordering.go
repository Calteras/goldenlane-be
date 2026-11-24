// Package ordering takes customers' orders and carries them to the merchants' POS: it
// checks the cart, stores the order, sends it (retrying until the POS has it or it is clear
// it never will), mirrors every status change back, and lets the customer cancel while the
// merchant has not yet accepted.
package ordering

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/persistent"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
	"github.com/Calteras/streetfood-customer-api/pkg/secretbox"
)

const (
	_submitTimeout = 8 * time.Second
	// _submitGiveUp is how long an order may wait for an unreachable POS before the customer
	// is told it did not go through.
	_submitGiveUp    = 2 * time.Minute
	_claimBatch      = 10
	_lease           = 45 * time.Second
	_staleAfter      = 2 * time.Minute
	_maxNote         = 300
	_maxLineNote     = 200
	_maxLabel        = 40
	_maxKeyLength    = 100
	_maxReviewLength = 500
	// ErrorUnreachable is the code of an order the POS never answered.
	ErrorUnreachable = "MERCHANT_UNREACHABLE"
)

var _submitBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

// Store is what the use case needs from storage.
type Store interface {
	PublicOutlet(ctx context.Context, id string) (*entity.Outlet, error)
	PublicMenu(ctx context.Context, outletID string) (*entity.Menu, error)
	LinkByMerchant(ctx context.Context, merchantID string) (entity.Link, error)
	CreateCheckout(ctx context.Context, in persistent.CheckoutInput) (*entity.Order, bool, error)
	ExistingCheckout(ctx context.Context, customerID, key, requestHash string) (*entity.Order, error)
	OrderByID(ctx context.Context, id string) (*entity.Order, error)
	CustomerOrder(ctx context.Context, customerID, id string) (*entity.Order, error)
	ListCustomerOrders(ctx context.Context, customerID string, before *time.Time, limit int) ([]*entity.Order, error)
	SubmitDataFor(ctx context.Context, orderID string) (persistent.SubmitData, error)
	ClaimSubmits(ctx context.Context, limit int, lease time.Duration) ([]string, string, error)
	RetrySubmit(ctx context.Context, id string, delay time.Duration) error
	FailSubmit(ctx context.Context, id, code string, detail json.RawMessage) (bool, error)
	CancelCreated(ctx context.Context, customerID, id string) (bool, error)
	ApplyPOSStatus(ctx context.Context, platformOrderID string, st entity.POSOrderState, notify persistent.Notify) (*entity.Order, bool, error)
	MarkSubmitted(ctx context.Context, id string, st entity.POSOrderState, notify persistent.Notify) (bool, error)
	StaleOpen(ctx context.Context, olderThan time.Duration, limit int) ([]*entity.Order, error)
	TouchChecked(ctx context.Context, id string) error
	CreateReview(ctx context.Context, customerID, orderID string, rating int, comment *string) (entity.Review, error)
	OutletReviews(ctx context.Context, outletID string, before *time.Time, limit int) ([]entity.Review, error)
}

// POS is the merchant's POS as the ordering needs it.
type POS interface {
	SubmitOrder(ctx context.Context, cred pos.Credentials, req pos.SubmitRequest) (pos.OrderResult, error)
	OrderStatus(ctx context.Context, cred pos.Credentials, platformOrderID string) (pos.OrderResult, error)
	CancelOrder(ctx context.Context, cred pos.Credentials, platformOrderID, reason string) (pos.OrderResult, error)
	Quote(ctx context.Context, cred pos.Credentials, req pos.QuoteRequest) (pos.Pricing, error)
}

// Payer takes online payments for orders.
type Payer interface {
	Start(ctx context.Context, o *entity.Order) (*entity.Payment, error)
	Retry(ctx context.Context, customerID, orderID string) (*entity.Order, error)
	Cancel(ctx context.Context, customerID, orderID string) (*entity.Order, error)
	Deadline(now time.Time) time.Time
	Rates() entity.FeeRates
}

// UseCase is the ordering service.
type UseCase struct {
	store Store
	pos   POS
	pay   Payer
	box   *secretbox.Box
	log   logger.Interface
	now   func() time.Time
}

// New builds the use case.
func New(store Store, posClient POS, payer Payer, box *secretbox.Box, log logger.Interface) *UseCase {
	return &UseCase{store: store, pos: posClient, pay: payer, box: box, log: log, now: time.Now}
}

// Line is one line of a checkout request.
type Line struct {
	ProductID   string
	VariantID   *string
	ModifierIDs []string
	Quantity    int
	Notes       *string
}

// CheckoutRequest is what the customer submits.
type CheckoutRequest struct {
	OutletID           string
	FulfillmentType    string
	FulfillmentLabel   *string
	Notes              *string
	Items              []Line
	ExpectedTotalMinor *int64
	// Payment is entity.PaymentModePayAtPick (the default) or entity.PaymentModeOnline.
	Payment string
}

// ProblemsError reports lines that cannot be ordered.
type ProblemsError struct{ Problems []entity.LineProblem }

func (e *ProblemsError) Error() string { return "some items cannot be ordered as chosen" }

// RejectedError means the order was stored but the POS refused it. The order carries the
// reason; Code is the POS's own (PRICE_CHANGED, OUTLET_CLOSED, ORDER_INVALID, ...).
type RejectedError struct {
	Order  *entity.Order
	Code   string
	Detail map[string]any
}

func (e *RejectedError) Error() string { return "the merchant could not take this order: " + e.Code }

// RefusedError means the POS refused to price an order for online payment; nothing was stored.
// Code is the POS's own (OUTLET_CLOSED, ORDER_INVALID, ...).
type RefusedError struct {
	Code   string
	Detail map[string]any
}

func (e *RefusedError) Error() string { return "the merchant could not take this order: " + e.Code }

// PriceChangedError means the price differs from what the customer expected.
type PriceChangedError struct{ TotalMinor int64 }

func (e *PriceChangedError) Error() string { return "prices have changed" }

var (
	// ErrOutletClosed means the outlet is not taking orders right now.
	ErrOutletClosed = errors.New("this outlet is not taking orders right now")
	// ErrOnlineUnavailable means the outlet's merchant does not take online payments.
	ErrOnlineUnavailable = errors.New("this merchant does not take online payments")
	// ErrMerchantUnreachable means the POS could not be asked, so nothing was started.
	ErrMerchantUnreachable = errors.New("the merchant cannot be reached right now")
)

// Checkout stores a customer's order and sends it to the merchant. Repeating the same
// idempotency key with the same request returns the same order. created is false then.
func (u *UseCase) Checkout(ctx context.Context, customer entity.Customer, key string, req CheckoutRequest) (*entity.Order, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > _maxKeyLength {
		return nil, false, fmt.Errorf("%w: Idempotency-Key is required (up to %d characters)", entity.ErrBadRequest, _maxKeyLength)
	}

	if req.Payment == "" {
		req.Payment = entity.PaymentModePayAtPick
	}

	if err := validateShape(req); err != nil {
		return nil, false, err
	}

	if _, err := uuid.Parse(req.OutletID); err != nil {
		return nil, false, entity.ErrNotFound
	}

	outlet, err := u.store.PublicOutlet(ctx, req.OutletID)
	if err != nil {
		return nil, false, err
	}

	if !outlet.AcceptingOrders(u.now()) {
		return nil, false, ErrOutletClosed
	}

	online := req.Payment == entity.PaymentModeOnline
	if online && !outlet.OnlinePayment {
		return nil, false, ErrOnlineUnavailable
	}

	// A repeat of an earlier request returns that order without asking anyone anything.
	hash := hashRequest(req)

	existing, err := u.store.ExistingCheckout(ctx, customer.ID, key, hash)
	if err != nil {
		return nil, false, err
	}

	if existing != nil {
		return existing, false, u.refusal(existing)
	}

	menu, err := u.store.PublicMenu(ctx, outlet.ID)
	if err != nil {
		return nil, false, ErrOutletClosed // no menu yet: nothing to order
	}

	index, err := entity.ParseMenuIndex(menu.Payload)
	if err != nil {
		return nil, false, fmt.Errorf("stored menu is not readable: %w", err)
	}

	lines := make([]entity.OrderLine, 0, len(req.Items))
	for _, l := range req.Items {
		lines = append(lines, entity.OrderLine{ProductID: l.ProductID, VariantID: l.VariantID, ModifierIDs: l.ModifierIDs, Quantity: l.Quantity, Notes: l.Notes})
	}

	items, problems := index.Check(lines)
	if len(problems) > 0 {
		return nil, false, &ProblemsError{Problems: problems}
	}

	in := persistent.CheckoutInput{
		CustomerID: customer.ID, IdempotencyKey: key, RequestHash: hash,
		MerchantID: outlet.MerchantID, OutletID: outlet.ID,
		PlatformOrderID: "mo_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		FulfillmentType: req.FulfillmentType, FulfillmentLabel: req.FulfillmentLabel, Notes: req.Notes,
		ExpectedTotalMinor: req.ExpectedTotalMinor, Items: items,
		// Every order owes the merchant fee, whichever way it is paid, at today's rate.
		MerchantFeeBps: u.pay.Rates().MerchantBps,
	}

	if online {
		// The POS prices the order before the customer is asked to pay, so the charge is exactly
		// what the merchant will see, and an order the merchant cannot take is never paid for.
		quote, err := u.quote(ctx, outlet, items)
		if err != nil {
			return nil, false, err
		}

		if req.ExpectedTotalMinor != nil && *req.ExpectedTotalMinor != quote.TotalMinor {
			return nil, false, &PriceChangedError{TotalMinor: quote.TotalMinor}
		}

		if quote.TotalMinor <= 0 {
			return nil, false, fmt.Errorf("%w: an order with no price cannot be paid online", entity.ErrBadRequest)
		}

		in.Online, in.Quote, in.PaymentDeadline = true, &quote, u.pay.Deadline(u.now())
	}

	order, created, err := u.store.CreateCheckout(ctx, in)
	if err != nil {
		return nil, false, err
	}

	if created && online {
		if _, err := u.pay.Start(ctx, order); err != nil {
			return nil, false, err
		}

		order, err = u.store.OrderByID(ctx, order.ID)

		return order, true, err
	}

	if created {
		// Try now, so the customer usually gets the answer in this request. If the POS is
		// slow or down the worker keeps trying.
		sendCtx, cancel := context.WithTimeout(ctx, _submitTimeout)
		u.submit(sendCtx, order.ID)
		cancel()

		if order, err = u.store.OrderByID(ctx, order.ID); err != nil {
			return nil, false, err
		}
	}

	return order, created, u.refusal(order)
}

// refusal is the error to report with an order the POS refused for good, or nil.
func (u *UseCase) refusal(o *entity.Order) error {
	if o.Status == entity.OrderSubmitFailed && o.ErrorCode != nil && *o.ErrorCode != ErrorUnreachable {
		return u.rejected(o)
	}

	return nil
}

func (u *UseCase) rejected(o *entity.Order) *RejectedError {
	e := &RejectedError{Order: o, Code: *o.ErrorCode}
	if len(o.ErrorDetail) > 0 {
		_ = json.Unmarshal(o.ErrorDetail, &e.Detail)
	}

	return e
}

// quote asks the POS what an online order costs. A refusal that retrying cannot fix comes back
// as RefusedError; anything else means the merchant cannot be asked right now.
func (u *UseCase) quote(ctx context.Context, outlet *entity.Outlet, items []entity.OrderItem) (entity.Pricing, error) {
	cred, err := u.credentials(ctx, outlet.MerchantID)
	if err != nil {
		return entity.Pricing{}, err
	}

	lines := make([]pos.SubmitItem, 0, len(items))
	for _, it := range items {
		lines = append(lines, pos.SubmitItem{ProductID: it.ProductID, VariantID: it.VariantID, ModifierIDs: it.ModifierIDs, Quantity: it.Quantity, Notes: it.Notes})
	}

	quoteCtx, cancel := context.WithTimeout(ctx, _submitTimeout)
	defer cancel()

	p, err := u.pos.Quote(quoteCtx, cred, pos.QuoteRequest{OutletID: outlet.POSOutletID, Items: lines})
	if err != nil {
		var perr *pos.Error
		if errors.As(err, &perr) && perr.Permanent() {
			code := perr.Code
			if code == "" {
				code = "ORDER_REJECTED"
			}

			return entity.Pricing{}, &RefusedError{Code: code, Detail: perr.Data}
		}

		u.log.Warn("could not price an order", "outlet", outlet.ID, "error", err.Error())

		return entity.Pricing{}, ErrMerchantUnreachable
	}

	return entity.Pricing{SubtotalMinor: p.SubtotalMinor, TaxMinor: p.TaxMinor, ServiceMinor: p.ServiceMinor, TotalMinor: p.TotalMinor}, nil
}

func validateShape(req CheckoutRequest) error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", entity.ErrBadRequest, msg) }

	if req.FulfillmentType != entity.FulfillmentPickup && req.FulfillmentType != entity.FulfillmentDineIn {
		return bad("fulfillment type must be PICKUP or DINE_IN")
	}

	if req.Payment != entity.PaymentModePayAtPick && req.Payment != entity.PaymentModeOnline {
		return bad("payment must be PAY_AT_PICKUP or ONLINE")
	}

	if req.FulfillmentLabel != nil && len([]rune(*req.FulfillmentLabel)) > _maxLabel {
		return bad("fulfillment label is too long")
	}

	if req.Notes != nil && len([]rune(*req.Notes)) > _maxNote {
		return bad("notes are too long")
	}

	if len(req.Items) == 0 || len(req.Items) > entity.MaxLinesPerOrder {
		return bad(fmt.Sprintf("an order has 1 to %d lines", entity.MaxLinesPerOrder))
	}

	if req.ExpectedTotalMinor != nil && *req.ExpectedTotalMinor < 0 {
		return bad("expectedTotalMinor cannot be negative")
	}

	total := 0

	for _, l := range req.Items {
		if l.Quantity < 1 || l.Quantity > entity.MaxItemsPerOrder {
			return bad("each line has a quantity of 1 to 50")
		}

		if l.Notes != nil && len([]rune(*l.Notes)) > _maxLineNote {
			return bad("item notes are too long")
		}

		if len(l.ModifierIDs) > 30 {
			return bad("too many options on one line")
		}

		total += l.Quantity
	}

	if total > entity.MaxItemsPerOrder {
		return bad(fmt.Sprintf("an order has at most %d items", entity.MaxItemsPerOrder))
	}

	return nil
}

// hashRequest fingerprints a request, ignoring the order of options within a line.
func hashRequest(req CheckoutRequest) string {
	type line struct {
		P string   `json:"p"`
		V *string  `json:"v"`
		M []string `json:"m"`
		Q int      `json:"q"`
		N *string  `json:"n"`
	}

	lines := make([]line, 0, len(req.Items))

	for _, l := range req.Items {
		m := append([]string{}, l.ModifierIDs...)
		slices.Sort(m)
		lines = append(lines, line{l.ProductID, l.VariantID, m, l.Quantity, l.Notes})
	}

	raw, _ := json.Marshal(map[string]any{ //nolint:errchkjson // plain data
		"o": req.OutletID, "f": req.FulfillmentType, "l": req.FulfillmentLabel, "n": req.Notes,
		"e": req.ExpectedTotalMinor, "i": lines, "p": req.Payment,
	})
	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:])
}

// ------------------------------------------------------------------ reading

// Order returns one of the customer's orders.
func (u *UseCase) Order(ctx context.Context, customerID, id string) (*entity.Order, error) {
	return u.store.CustomerOrder(ctx, customerID, id)
}

// Orders lists the customer's orders, newest first.
func (u *UseCase) Orders(ctx context.Context, customerID string, before *time.Time, limit int) ([]*entity.Order, error) {
	if limit <= 0 {
		limit = 20
	}

	return u.store.ListCustomerOrders(ctx, customerID, before, min(limit, 50))
}

// ----------------------------------------------------------------- sending

func (u *UseCase) credentials(ctx context.Context, merchantID string) (pos.Credentials, error) {
	link, err := u.store.LinkByMerchant(ctx, merchantID)
	if err != nil {
		return pos.Credentials{}, err
	}

	secret, err := u.box.Open(link.SecretEnc, link.MerchantID)
	if err != nil {
		return pos.Credentials{}, fmt.Errorf("merchant secret unreadable: %w", err)
	}

	return pos.Credentials{BaseURL: link.BaseURL, KeyID: link.KeyID, Secret: secret}, nil
}

func stateOf(r pos.OrderResult) entity.POSOrderState {
	st := entity.POSOrderState{
		POSOrderID: r.POSOrderID, Status: r.Status, Version: r.Version,
		Subtotal: &r.Pricing.SubtotalMinor, Tax: &r.Pricing.TaxMinor, Service: &r.Pricing.ServiceMinor, Total: &r.Pricing.TotalMinor,
		PrepMinutes: r.PrepTimeMinutes, Reason: r.Reason,
	}
	if !r.AcceptBy.IsZero() {
		st.AcceptBy = &r.AcceptBy
	}

	return st
}

// submit sends one stored order to its POS and records the outcome. It never fails the
// caller: whatever goes wrong is recorded on the order or retried.
func (u *UseCase) submit(ctx context.Context, orderID string) {
	data, err := u.store.SubmitDataFor(ctx, orderID)
	if err != nil {
		u.log.Error("order data unreadable", "order", orderID, "error", err.Error())

		return
	}

	order := data.Order
	if order.Status != entity.OrderCreated {
		return
	}

	cred, err := u.credentials(ctx, order.MerchantID)
	if err != nil {
		u.retryOrGiveUp(ctx, order, err)

		return
	}

	items := make([]pos.SubmitItem, 0, len(order.Items))
	for _, it := range order.Items {
		items = append(items, pos.SubmitItem{ProductID: it.ProductID, VariantID: it.VariantID, ModifierIDs: it.ModifierIDs, Quantity: it.Quantity, Notes: it.Notes})
	}

	payment := map[string]any{"mode": entity.PaymentModePayAtPick}
	expected := order.ExpectedTotal

	if order.PaymentMode == entity.PaymentModeOnline {
		// Only a paid order goes to the POS. It is sent with the price the customer paid for, so
		// a price that moved meanwhile is refused (and refunded) instead of silently changing.
		p := order.Payment
		if p == nil || p.Status != entity.PaymentPaid {
			u.log.Error("online order without a payment was queued", "order", order.ID)

			return
		}

		method := entity.MethodOther
		if p.Method != nil && *p.Method != "" {
			method = *p.Method
		}

		payment = map[string]any{"mode": "PREPAID", "method": method, "reference": p.ExternalID}
		expected = order.TotalMinor
	}

	name := strings.TrimSpace(data.CustomerName)
	if name == "" {
		name = "Customer"
	}

	result, err := u.pos.SubmitOrder(ctx, cred, pos.SubmitRequest{
		PlatformOrderID: order.PlatformOrderID,
		CheckoutID:      "co_" + strings.ReplaceAll(order.CheckoutID, "-", ""),
		OutletID:        data.POSOutletID,
		Fulfillment:     pos.Fulfillment{Type: order.FulfillmentType, Label: order.FulfillmentLbl},
		Customer:        pos.OrderCustomer{PlatformCustomerID: "cu_" + strings.ReplaceAll(order.CustomerID, "-", ""), Name: name, Phone: data.Phone},
		Notes:           order.Notes, Items: items, ExpectedTotalMinor: expected,
		Payment: payment,
	})
	if err != nil {
		var perr *pos.Error
		if errors.As(err, &perr) && perr.Permanent() {
			u.reject(ctx, order, perr)

			return
		}

		u.retryOrGiveUp(ctx, order, err)

		return
	}

	applied, err := u.store.MarkSubmitted(ctx, order.ID, stateOf(result), u.notification)
	if err != nil {
		u.log.Error("order status not stored", "order", order.ID, "error", err.Error())

		return
	}

	if !applied {
		// Not applied means the order left CREATED meanwhile. Either another attempt got there
		// first (nothing to do), or the customer cancelled while it was on its way: a local
		// cancel never carries a POS version, so that is the only case to take back.
		cur, err := u.store.OrderByID(ctx, order.ID)
		if err == nil && cur.Status == entity.OrderCancelled && cur.POSVersion == 0 {
			u.undo(ctx, order, cred)
		}
	}
}

// undo cancels at the POS an order the customer cancelled while it was being sent.
func (u *UseCase) undo(ctx context.Context, o *entity.Order, cred pos.Credentials) {
	res, err := u.pos.CancelOrder(ctx, cred, o.PlatformOrderID, "Cancelled by customer")
	if err != nil {
		u.log.Warn("could not take back a cancelled order", "order", o.ID, "error", err.Error())

		return
	}

	if _, _, err := u.store.ApplyPOSStatus(ctx, o.PlatformOrderID, stateOf(res), nil); err != nil {
		u.log.Error("cancelled order not stored", "order", o.ID, "error", err.Error())
	}
}

func (u *UseCase) reject(ctx context.Context, o *entity.Order, perr *pos.Error) {
	code := perr.Code
	if code == "" {
		code = "ORDER_REJECTED"
	}

	detail, _ := json.Marshal(perr.Data) //nolint:errchkjson // plain data

	if _, err := u.store.FailSubmit(ctx, o.ID, code, detail); err != nil {
		u.log.Error("rejected order not stored", "order", o.ID, "error", err.Error())
	}
}

// retryOrGiveUp keeps trying an unreachable POS, then tells the customer. Before giving up
// it asks the POS whether it has the order after all (an answer may have been lost).
func (u *UseCase) retryOrGiveUp(ctx context.Context, o *entity.Order, cause error) {
	if u.now().Sub(o.CreatedAt) < _submitGiveUp {
		delay := _submitBackoff[min(o.SubmitAttempts, len(_submitBackoff)-1)]
		if err := u.store.RetrySubmit(ctx, o.ID, delay); err != nil {
			u.log.Error("order not requeued", "order", o.ID, "error", err.Error())
		}

		return
	}

	if cred, err := u.credentials(ctx, o.MerchantID); err == nil {
		if res, err := u.pos.OrderStatus(ctx, cred, o.PlatformOrderID); err == nil {
			if _, err := u.store.MarkSubmitted(ctx, o.ID, stateOf(res), u.notification); err == nil {
				return
			}
		}
	}

	u.log.Warn("order never reached the merchant", "order", o.ID, "cause", cause.Error())

	if _, err := u.store.FailSubmit(ctx, o.ID, ErrorUnreachable, nil); err != nil {
		u.log.Error("failed order not stored", "order", o.ID, "error", err.Error())
	}
}

// ProcessSubmits sends orders that are waiting for their POS. more asks the poller to run again.
func (u *UseCase) ProcessSubmits(ctx context.Context) (bool, error) {
	ids, _, err := u.store.ClaimSubmits(ctx, _claimBatch, _lease)
	if err != nil {
		return false, err
	}

	for _, id := range ids {
		sendCtx, cancel := context.WithTimeout(ctx, _submitTimeout)
		u.submit(sendCtx, id)
		cancel()
	}

	return len(ids) == _claimBatch, nil
}

// -------------------------------------------------------------- cancelling

// Cancel withdraws an order before the merchant accepts it. After that only the merchant can.
func (u *UseCase) Cancel(ctx context.Context, customerID, id string) (*entity.Order, error) {
	o, err := u.store.CustomerOrder(ctx, customerID, id)
	if err != nil {
		return nil, err
	}

	if o.Status == entity.OrderCancelled {
		return o, nil
	}

	if o.Status == entity.OrderUnpaid {
		cancelled, err := u.pay.Cancel(ctx, customerID, id)
		if errors.Is(err, entity.ErrCannotCancel) {
			// It was paid in the meantime: carry on as for a paid order.
			if o, err = u.store.CustomerOrder(ctx, customerID, id); err != nil {
				return nil, err
			}
		} else {
			return cancelled, err
		}
	}

	if o.Status == entity.OrderCreated {
		done, err := u.store.CancelCreated(ctx, customerID, id)
		if err != nil {
			return nil, err
		}

		if done {
			return u.store.CustomerOrder(ctx, customerID, id)
		}

		// It was sent in the meantime: carry on as for a sent order.
		if o, err = u.store.CustomerOrder(ctx, customerID, id); err != nil {
			return nil, err
		}
	}

	if o.Status != entity.OrderPendingAccept {
		return nil, entity.ErrCannotCancel
	}

	cred, err := u.credentials(ctx, o.MerchantID)
	if err != nil {
		return nil, err
	}

	res, err := u.pos.CancelOrder(ctx, cred, o.PlatformOrderID, "Cancelled by customer")
	if err != nil {
		var perr *pos.Error
		if errors.As(err, &perr) && perr.Code == "INVALID_TRANSITION" {
			// The merchant accepted first. Show the truth and refuse.
			u.refresh(ctx, o)

			return nil, entity.ErrCannotCancel
		}

		return nil, fmt.Errorf("cancel at the merchant: %w", err)
	}

	updated, _, err := u.store.ApplyPOSStatus(ctx, o.PlatformOrderID, stateOf(res), nil)

	return updated, err
}

// Pay starts another payment attempt on the customer's unpaid order, or returns the open one.
func (u *UseCase) Pay(ctx context.Context, customerID, id string) (*entity.Order, error) {
	return u.pay.Retry(ctx, customerID, id)
}

// ----------------------------------------------------------------- mirroring

// ApplyEvent applies an order.status.changed event from the POS. Orders that are not ours
// (or not known yet) are ignored.
func (u *UseCase) ApplyEvent(ctx context.Context, version int, data json.RawMessage) error {
	var d struct {
		PlatformOrderID string  `json:"platformOrderId"`
		POSOrderID      string  `json:"posOrderId"`
		Status          string  `json:"status"`
		Reason          *string `json:"reason"`
		PrepTimeMinutes *int    `json:"prepTimeMinutes"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.PlatformOrderID == "" || !knownStatus(d.Status) {
		return nil //nolint:nilerr // malformed or unknown: nothing to apply
	}

	_, _, err := u.store.ApplyPOSStatus(ctx, d.PlatformOrderID, entity.POSOrderState{
		POSOrderID: d.POSOrderID, Status: d.Status, Version: version, PrepMinutes: d.PrepTimeMinutes, Reason: d.Reason,
	}, u.notification)
	if errors.Is(err, entity.ErrNotFound) {
		return nil
	}

	return err
}

func knownStatus(s string) bool {
	switch s {
	case entity.OrderPendingAccept, entity.OrderAccepted, entity.OrderPreparing, entity.OrderReady,
		entity.OrderCompleted, entity.OrderRejected, entity.OrderCancelled, entity.OrderExpired:
		return true
	default:
		return false
	}
}

// refresh asks the POS for an order's state and applies it.
func (u *UseCase) refresh(ctx context.Context, o *entity.Order) {
	cred, err := u.credentials(ctx, o.MerchantID)
	if err != nil {
		return
	}

	res, err := u.pos.OrderStatus(ctx, cred, o.PlatformOrderID)
	if err != nil {
		return
	}

	if _, _, err := u.store.ApplyPOSStatus(ctx, o.PlatformOrderID, stateOf(res), u.notification); err != nil {
		u.log.Error("order status not stored", "order", o.ID, "error", err.Error())
	}
}

// RefreshStale is the safety net for lost events: it asks the POS about open orders it has
// not been asked about for a while.
func (u *UseCase) RefreshStale(ctx context.Context) (bool, error) {
	orders, err := u.store.StaleOpen(ctx, _staleAfter, _claimBatch)
	if err != nil {
		return false, err
	}

	for _, o := range orders {
		u.refresh(ctx, o)

		if err := u.store.TouchChecked(ctx, o.ID); err != nil {
			return false, err
		}
	}

	return len(orders) == _claimBatch, nil
}

// Notification is what the customer is told about an order that just changed, or nil for nothing.
func (u *UseCase) Notification(o *entity.Order) *entity.Notification { return u.notification(o) }

// notification decides which status changes the customer is told about by push.
func (u *UseCase) notification(o *entity.Order) *entity.Notification {
	n := &entity.Notification{
		CustomerID: o.CustomerID, OrderID: o.ID, Kind: o.Status,
		Data: map[string]string{"type": "order", "orderId": o.ID, "status": o.Status},
	}

	switch o.Status {
	case entity.OrderAccepted:
		n.Title = "Pesanan diterima"
		n.Body = o.OutletName + " sedang menyiapkan pesananmu"

		if o.PrepMinutes != nil && *o.PrepMinutes > 0 {
			n.Body += fmt.Sprintf(" (±%d menit)", *o.PrepMinutes)
		}
	case entity.OrderReady:
		n.Title = "Pesanan siap diambil"
		n.Body = "Ambil pesananmu di " + o.OutletName
	case entity.OrderRejected:
		n.Title = "Pesanan ditolak"
		n.Body = o.OutletName + " tidak dapat menerima pesananmu"

		if o.Reason != nil && *o.Reason != "" {
			n.Body += ": " + *o.Reason
		}
	case entity.OrderCancelled:
		n.Title = "Pesanan dibatalkan"
		n.Body = o.OutletName + " membatalkan pesananmu"

		if o.Reason != nil && *o.Reason != "" {
			n.Body += ": " + *o.Reason
		}
	case entity.OrderExpired:
		if o.ErrorCode != nil && *o.ErrorCode == entity.ErrorPaymentExpired {
			n.Title = "Waktu pembayaran habis"
			n.Body = "Pesanan di " + o.OutletName + " dibatalkan karena belum dibayar. Pesan lagi kapan saja"

			break
		}

		n.Title = "Pesanan tidak direspons"
		n.Body = o.OutletName + " belum merespons. Coba lagi atau pilih tempat lain"
	default:
		return nil
	}

	return n
}

// ------------------------------------------------------------------ reviews

// Review rates a completed order, once.
func (u *UseCase) Review(ctx context.Context, customerID, orderID string, rating int, comment *string) (entity.Review, error) {
	if rating < 1 || rating > 5 {
		return entity.Review{}, fmt.Errorf("%w: rating is 1 to 5", entity.ErrBadRequest)
	}

	if comment != nil {
		c := strings.TrimSpace(*comment)
		if len([]rune(c)) > _maxReviewLength {
			return entity.Review{}, fmt.Errorf("%w: comment is up to %d characters", entity.ErrBadRequest, _maxReviewLength)
		}

		comment = &c
		if c == "" {
			comment = nil
		}
	}

	if _, err := uuid.Parse(orderID); err != nil {
		return entity.Review{}, entity.ErrNotFound
	}

	return u.store.CreateReview(ctx, customerID, orderID, rating, comment)
}

// OutletReviews lists an outlet's reviews.
func (u *UseCase) OutletReviews(ctx context.Context, outletID string, before *time.Time, limit int) ([]entity.Review, error) {
	if _, err := uuid.Parse(outletID); err != nil {
		return nil, entity.ErrNotFound
	}

	if limit <= 0 {
		limit = 20
	}

	return u.store.OutletReviews(ctx, outletID, before, min(limit, 50))
}
