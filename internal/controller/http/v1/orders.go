package v1

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Calteras/streetfood-customer-api/internal/controller/http/middleware"
	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/ordering"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/payment"
	"github.com/Calteras/streetfood-customer-api/pkg/httpserver"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// Orders is the ordering service as the handlers use it.
type Orders interface {
	Checkout(ctx context.Context, c entity.Customer, key string, req ordering.CheckoutRequest) (*entity.Order, bool, error)
	Order(ctx context.Context, customerID, id string) (*entity.Order, error)
	Orders(ctx context.Context, customerID string, before *time.Time, limit int) ([]*entity.Order, error)
	Cancel(ctx context.Context, customerID, id string) (*entity.Order, error)
	Pay(ctx context.Context, customerID, id string) (*entity.Order, error)
	Review(ctx context.Context, customerID, orderID string, rating int, comment *string) (entity.Review, error)
	OutletReviews(ctx context.Context, outletID string, before *time.Time, limit int) ([]entity.Review, error)
}

// Watcher tells a stream when an order changes.
type Watcher interface {
	Subscribe(orderID string) (<-chan struct{}, func())
}

// Stream timings. A stream is closed after _streamMaxAge and the app reconnects, so a
// stuck connection never lives forever and token expiry is re-checked.
const (
	_heartbeat    = 15 * time.Second
	_streamMaxAge = 10 * time.Minute

	// _safetyRead is how often an open stream re-reads the order when nothing woke it. Changes
	// made through this instance wake the stream at once; this only catches one made through
	// another instance, so it can be slow. A stream that re-read on every heartbeat would keep a
	// serverless database awake for as long as a customer watches an order.
	_safetyRead  = 2 * time.Minute
	_writeWindow = 10 * time.Second
)

type ordersRoutes struct {
	uc        Orders
	hub       Watcher
	l         logger.Interface
	heartbeat time.Duration
	maxAge    time.Duration
	safety    time.Duration
}

// OrdersOptions tunes streaming (tests use short timings).
type OrdersOptions struct {
	Heartbeat time.Duration
	MaxAge    time.Duration

	// SafetyRead is how often a stream re-reads the order without a wake-up.
	SafetyRead time.Duration
}

// NewOrdersRoutes registers checkout, orders and reviews. Everything except reading an
// outlet's reviews needs a signed-in customer.
func NewOrdersRoutes(g fiber.Router, signedIn fiber.Handler, uc Orders, hub Watcher, l logger.Interface, opt OrdersOptions) {
	r := &ordersRoutes{uc: uc, hub: hub, l: l, heartbeat: _heartbeat, maxAge: _streamMaxAge, safety: _safetyRead}
	if opt.Heartbeat > 0 {
		r.heartbeat = opt.Heartbeat
	}

	if opt.MaxAge > 0 {
		r.maxAge = opt.MaxAge
	}

	if opt.SafetyRead > 0 {
		r.safety = opt.SafetyRead
	}

	g.Post("/checkouts", signedIn, r.checkout)
	g.Get("/orders", signedIn, r.list)
	g.Get("/orders/:id", signedIn, r.get)
	g.Post("/orders/:id/cancel", signedIn, r.cancel)
	g.Post("/orders/:id/payment", signedIn, r.pay)
	g.Get("/orders/:id/stream", signedIn, r.stream)
	g.Post("/orders/:id/review", signedIn, r.review)
	g.Get("/outlets/:id/reviews", r.outletReviews)
}

// ---------------------------------------------------------------------- views

type orderView struct {
	ID          string             `json:"id"`
	Status      string             `json:"status"`
	Open        bool               `json:"open"`
	CanCancel   bool               `json:"canCancel"`
	Outlet      outletRef          `json:"outlet"`
	Fulfillment fulfillmentView    `json:"fulfillment"`
	Notes       *string            `json:"notes"`
	Items       []entity.OrderItem `json:"items"`
	Pricing     *pricingView       `json:"pricing"`
	PaymentMode string             `json:"paymentMode"`
	// Payment is the latest payment attempt of an order paid in the app.
	Payment         *paymentView `json:"payment"`
	PaymentDeadline *time.Time   `json:"paymentDeadline"`
	// CanPay is true while an unpaid order can still be paid (start or retry a payment).
	CanPay      bool       `json:"canPay"`
	AcceptBy    *time.Time `json:"acceptBy"`
	PrepMinutes *int       `json:"prepTimeMinutes"`
	Reason      *string    `json:"reason"`
	Error       *errorView `json:"error"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	CompletedAt *time.Time `json:"completedAt"`
	// PickupCode is what to show at the cart; only while the order is open.
	PickupCode *string `json:"pickupCode"`
	// ReadyAt is when the order became ready for pickup.
	ReadyAt *time.Time `json:"readyAt"`
	// CloseReasonCode says why a finished order ended (NO_SHOW, CUSTOMER_CANCELLED, ...).
	CloseReasonCode *string `json:"closeReasonCode"`
}

type outletRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type fulfillmentView struct {
	Type  string  `json:"type"`
	Label *string `json:"label"`
}

type pricingView struct {
	SubtotalMinor int64 `json:"subtotalMinor"`
	TaxMinor      int64 `json:"taxMinor"`
	ServiceMinor  int64 `json:"serviceMinor"`
	TotalMinor    int64 `json:"totalMinor"`
}

type paymentView struct {
	Status           string      `json:"status"`
	Provider         string      `json:"provider"`
	Attempt          int         `json:"attempt"`
	AmountMinor      int64       `json:"amountMinor"`
	OrderTotalMinor  int64       `json:"orderTotalMinor"`
	CustomerFeeMinor int64       `json:"customerFeeMinor"`
	PayURL           *string     `json:"payUrl"`
	ExpiresAt        *time.Time  `json:"expiresAt"`
	PaidAt           *time.Time  `json:"paidAt"`
	Method           *string     `json:"method"`
	Error            *errorView  `json:"error"`
	Refund           *refundView `json:"refund"`
}

type refundView struct {
	Status     string     `json:"status"` // PENDING, DONE, or FAILED (the merchant refunds by hand)
	RefundedAt *time.Time `json:"refundedAt"`
}

type errorView struct {
	Code   string          `json:"code"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

func present(o *entity.Order) orderView {
	v := orderView{
		ID: o.ID, Status: o.Status, Open: entity.IsOpen(o.Status),
		CanCancel:   o.Status == entity.OrderUnpaid || o.Status == entity.OrderCreated || o.Status == entity.OrderPendingAccept,
		Outlet:      outletRef{ID: o.OutletID, Name: o.OutletName},
		Fulfillment: fulfillmentView{Type: o.FulfillmentType, Label: o.FulfillmentLbl},
		Notes:       o.Notes, Items: o.Items, PaymentMode: o.PaymentMode, AcceptBy: o.AcceptBy,
		PrepMinutes: o.PrepMinutes, Reason: o.Reason, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, CompletedAt: o.CompletedAt,
		PaymentDeadline: o.PaymentDeadline,
	}
	if v.Items == nil {
		v.Items = []entity.OrderItem{}
	}

	v.ReadyAt, v.CloseReasonCode = o.ReadyAt, o.CloseReasonCode
	if entity.IsOpen(o.Status) {
		v.PickupCode = o.PickupCode
	}

	v.CanPay = o.Status == entity.OrderUnpaid && o.PaymentDeadline != nil && o.PaymentDeadline.After(time.Now())

	if p := o.Payment; p != nil {
		pv := &paymentView{
			Status: p.Status, Provider: p.Provider, Attempt: p.Attempt, AmountMinor: p.AmountMinor,
			OrderTotalMinor: p.OrderTotalMinor, CustomerFeeMinor: p.CustomerFeeMinor,
			PayURL: p.PayURL, ExpiresAt: p.ExpiresAt, PaidAt: p.PaidAt, Method: p.Method,
		}
		if p.ErrorCode != nil {
			pv.Error = &errorView{Code: *p.ErrorCode}
		}

		if p.RefundStatus != nil {
			pv.Refund = &refundView{Status: *p.RefundStatus, RefundedAt: p.RefundedAt}
		}

		if p.Status != entity.PaymentPending {
			pv.PayURL = nil // a finished attempt has no page to open
		}

		v.Payment = pv
	}

	if o.TotalMinor != nil {
		v.Pricing = &pricingView{
			SubtotalMinor: deref(o.SubtotalMinor), TaxMinor: deref(o.TaxMinor),
			ServiceMinor: deref(o.ServiceMinor), TotalMinor: *o.TotalMinor,
		}
	}

	if o.ErrorCode != nil {
		v.Error = &errorView{Code: *o.ErrorCode, Detail: o.ErrorDetail}
	}

	return v
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}

	return *v
}

// ------------------------------------------------------------------ checkout

type lineBody struct {
	ProductID   string   `json:"productId"`
	VariantID   *string  `json:"variantId"`
	ModifierIDs []string `json:"modifierIds"`
	Quantity    int      `json:"quantity"`
	Notes       *string  `json:"notes"`
}

// checkout places an order.
//
//	POST /v1/checkouts   Idempotency-Key: <unique per attempt>
func (r *ordersRoutes) checkout(c *fiber.Ctx) error {
	var body struct {
		OutletID    string `json:"outletId"`
		Fulfillment struct {
			Type  string  `json:"type"`
			Label *string `json:"label"`
		} `json:"fulfillment"`
		Notes              *string    `json:"notes"`
		Items              []lineBody `json:"items"`
		ExpectedTotalMinor *int64     `json:"expectedTotalMinor"`
		Payment            struct {
			Mode string `json:"mode"`
		} `json:"payment"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest("body is not valid JSON")
	}

	if body.Fulfillment.Type == "" {
		body.Fulfillment.Type = entity.FulfillmentPickup
	}

	req := ordering.CheckoutRequest{
		OutletID: body.OutletID, FulfillmentType: body.Fulfillment.Type, FulfillmentLabel: body.Fulfillment.Label,
		Notes: body.Notes, ExpectedTotalMinor: body.ExpectedTotalMinor, Payment: body.Payment.Mode,
	}
	for _, l := range body.Items {
		req.Items = append(req.Items, ordering.Line{ProductID: l.ProductID, VariantID: l.VariantID, ModifierIDs: l.ModifierIDs, Quantity: l.Quantity, Notes: l.Notes})
	}

	order, created, err := r.uc.Checkout(c.UserContext(), middleware.CustomerFrom(c), c.Get("Idempotency-Key"), req)
	if err != nil {
		return r.checkoutError(err)
	}

	c.Set(fiber.HeaderCacheControl, "no-store")

	if !created {
		return ok(c, fiber.Map{"order": present(order)})
	}

	return created201(c, fiber.Map{"order": present(order)})
}

// checkoutError maps what can go wrong at checkout to what the app needs to show.
func (r *ordersRoutes) checkoutError(err error) error {
	var (
		problems *ordering.ProblemsError
		rejected *ordering.RejectedError
		refused  *ordering.RefusedError
		changed  *ordering.PriceChangedError
	)

	switch {
	case errors.As(err, &problems):
		return httpserver.NewUnprocessableEntity("some items cannot be ordered as chosen", nil).
			WithReason("ORDER_INVALID").WithData(fiber.Map{"problems": problems.Problems})
	case errors.As(err, &rejected):
		status := fiber.StatusUnprocessableEntity

		switch rejected.Code {
		case "PRICE_CHANGED", "OUTLET_CLOSED", "IDEMPOTENCY_CONFLICT", "OUTLET_UNAVAILABLE", "PREPAY_REQUIRED":
			// PREPAY_REQUIRED: this vendor wants to be paid in the app. CUSTOMER_NOT_ACCEPTED (a
			// 422) is the vendor's own choice and says nothing more about why.
			status = fiber.StatusConflict
		}

		data := fiber.Map{"order": present(rejected.Order)}
		for k, v := range rejected.Detail { // problems, or the new total
			if k != "error" && k != "statusCode" {
				data[k] = v
			}
		}

		e := &httpserver.AppError{Code: status, Message: rejected.Error(), Reason: rejected.Code, Data: data}

		return e
	case errors.As(err, &changed):
		return httpserver.NewConflict("prices have changed", nil).WithReason("PRICE_CHANGED").
			WithData(fiber.Map{"totalMinor": changed.TotalMinor})
	case errors.As(err, &refused):
		status := fiber.StatusUnprocessableEntity

		switch refused.Code {
		case "OUTLET_CLOSED", "OUTLET_UNAVAILABLE":
			status = fiber.StatusConflict
		}

		return &httpserver.AppError{Code: status, Message: refused.Error(), Reason: refused.Code, Data: refused.Detail}
	case errors.Is(err, ordering.ErrOnlineUnavailable):
		return httpserver.NewUnprocessableEntity("this merchant does not take online payments, pay at pickup instead", nil).WithReason("ONLINE_PAYMENT_UNAVAILABLE")
	case errors.Is(err, ordering.ErrMerchantUnreachable):
		return httpserver.NewServiceUnavailable("the merchant cannot be reached right now, try again shortly", nil).WithReason("MERCHANT_UNREACHABLE")
	case errors.Is(err, ordering.ErrOutletClosed):
		return httpserver.NewConflict("this outlet is not taking orders right now", nil).WithReason("OUTLET_CLOSED")
	case errors.Is(err, entity.ErrIdempotency):
		return httpserver.NewConflict("this Idempotency-Key was used for a different order", nil).WithReason("IDEMPOTENCY_CONFLICT")
	case errors.Is(err, entity.ErrTooMany):
		return httpserver.NewTooManyRequests("too many open orders or checkouts, try again later", nil).WithReason("TOO_MANY_ORDERS")
	default:
		return fail(r.l, err)
	}
}

func created201(c *fiber.Ctx, data any) error {
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": true, "message": "created", "data": data})
}

// ------------------------------------------------------------------ reading

func (r *ordersRoutes) list(c *fiber.Ctx) error {
	before, limit, err := paging(c)
	if err != nil {
		return err
	}

	orders, err := r.uc.Orders(c.UserContext(), middleware.CustomerFrom(c).ID, before, limit)
	if err != nil {
		return fail(r.l, err)
	}

	views := make([]orderView, 0, len(orders))
	for _, o := range orders {
		views = append(views, present(o))
	}

	c.Set(fiber.HeaderCacheControl, "no-store")

	return ok(c, fiber.Map{"orders": views})
}

func (r *ordersRoutes) get(c *fiber.Ctx) error {
	o, err := r.uc.Order(c.UserContext(), middleware.CustomerFrom(c).ID, c.Params("id"))
	if err != nil {
		return fail(r.l, err)
	}

	c.Set(fiber.HeaderCacheControl, "no-store")

	return ok(c, present(o))
}

func (r *ordersRoutes) cancel(c *fiber.Ctx) error {
	o, err := r.uc.Cancel(c.UserContext(), middleware.CustomerFrom(c).ID, c.Params("id"))
	switch {
	case errors.Is(err, entity.ErrCannotCancel):
		return httpserver.NewConflict("the merchant already accepted this order; ask them to cancel it", nil).WithReason("CANNOT_CANCEL")
	case err != nil:
		return fail(r.l, err)
	}

	return ok(c, present(o))
}

// pay starts a new payment attempt on an unpaid order, or returns the one still open.
//
//	POST /v1/orders/:id/payment
func (r *ordersRoutes) pay(c *fiber.Ctx) error {
	o, err := r.uc.Pay(c.UserContext(), middleware.CustomerFrom(c).ID, c.Params("id"))

	switch {
	case errors.Is(err, entity.ErrNotPayable):
		return httpserver.NewConflict("this order cannot be paid any more", nil).WithReason("NOT_PAYABLE")
	case errors.Is(err, payment.ErrNoGateway):
		return httpserver.NewUnprocessableEntity("this merchant does not take online payments", nil).WithReason("ONLINE_PAYMENT_UNAVAILABLE")
	case err != nil:
		return fail(r.l, err)
	}

	c.Set(fiber.HeaderCacheControl, "no-store")

	return ok(c, present(o))
}

// stream sends the order as server-sent events: once on connect, then on every change.
// The connection ends after ten minutes; the app reconnects.
func (r *ordersRoutes) stream(c *fiber.Ctx) error {
	customerID := middleware.CustomerFrom(c).ID

	first, err := r.uc.Order(c.UserContext(), customerID, c.Params("id"))
	if err != nil {
		return fail(r.l, err)
	}

	changes, cancel := r.hub.Subscribe(first.ID)
	conn := c.Context().Conn()

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer cancel()

		// The server's write timeout is for ordinary responses; extend it as we go.
		write := func(format string, args ...any) bool {
			_ = conn.SetWriteDeadline(time.Now().Add(_writeWindow))

			if _, err := fmt.Fprintf(w, format, args...); err != nil {
				return false
			}

			return w.Flush() == nil
		}

		send := func(o *entity.Order) bool {
			raw, err := json.Marshal(present(o))
			if err != nil {
				return false
			}

			return write("event: order\ndata: %s\n\n", raw)
		}

		if !send(first) {
			return
		}

		lastSent := first.UpdatedAt
		lastRead := time.Now()
		ticker := time.NewTicker(r.heartbeat)
		deadline := time.NewTimer(r.maxAge)

		defer ticker.Stop()
		defer deadline.Stop()

		refresh := func() bool {
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()

			lastRead = time.Now()

			o, err := r.uc.Order(ctx, customerID, first.ID)
			if err != nil {
				return true // try again at the next beat
			}

			if o.UpdatedAt.Equal(lastSent) {
				return true
			}

			lastSent = o.UpdatedAt

			return send(o)
		}

		for {
			select {
			case <-changes:
				if !refresh() {
					return
				}
			case <-ticker.C:
				// A comment keeps proxies open. A slow re-read catches a change made through
				// another instance, whose wake-up this one never heard.
				if !write(": ping\n\n") || (time.Since(lastRead) >= r.safety && !refresh()) {
					return
				}
			case <-deadline.C:
				write("event: reconnect\ndata: {}\n\n")

				return
			}
		}
	})

	return nil
}

// -------------------------------------------------------------------- reviews

func (r *ordersRoutes) review(c *fiber.Ctx) error {
	var body struct {
		Rating  int     `json:"rating"`
		Comment *string `json:"comment"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest("body is not valid JSON")
	}

	rev, err := r.uc.Review(c.UserContext(), middleware.CustomerFrom(c).ID, c.Params("id"), body.Rating, body.Comment)
	if errors.Is(err, entity.ErrConflict) {
		return httpserver.NewConflict("this order was already reviewed", nil).WithReason("ALREADY_REVIEWED")
	}

	if err != nil {
		return fail(r.l, err)
	}

	return created201(c, rev)
}

func (r *ordersRoutes) outletReviews(c *fiber.Ctx) error {
	before, limit, err := paging(c)
	if err != nil {
		return err
	}

	reviews, err := r.uc.OutletReviews(c.UserContext(), c.Params("id"), before, limit)
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, fiber.Map{"reviews": reviews})
}

// paging reads ?before=<RFC 3339 time>&limit=<n>. A "+" in a time zone offset that the
// client forgot to escape arrives as a space; it is read as the "+" it was meant to be.
func paging(c *fiber.Ctx) (*time.Time, int, error) {
	var before *time.Time

	if raw := c.Query("before"); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, strings.ReplaceAll(raw, " ", "+"))
		if err != nil {
			return nil, 0, badRequest("before must be an RFC 3339 time")
		}

		before = &t
	}

	limit := 20

	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, 0, badRequest("limit must be a positive number")
		}

		limit = n
	}

	return before, limit, nil
}
