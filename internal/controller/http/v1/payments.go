package v1

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/payment"
	"github.com/Calteras/streetfood-customer-api/pkg/httpserver"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// Callbacks receives notifications from payment gateways.
type Callbacks interface {
	Callback(ctx context.Context, provider, gatewayID string, header func(string) string, body []byte) error
}

// Gateways is what the operator API needs to connect merchants' payment accounts.
type Gateways interface {
	SetGateway(ctx context.Context, merchantID string, in payment.GatewayInput) (payment.GatewayView, error)
	Gateway(ctx context.Context, merchantID string) (payment.GatewayView, error)
	RemoveGateway(ctx context.Context, merchantID string) error
	FeeStatement(ctx context.Context, merchantID string, from, to time.Time) (entity.FeeStatement, error)
	SettleFeesManually(ctx context.Context, merchantID string, through time.Time, note string) (payment.ManualSettlement, error)
}

type callbackRoutes struct {
	uc Callbacks
	l  logger.Interface
}

// NewPaymentCallbackRoutes registers the address gateways call when a payment changes. The
// merchant's gateway account id is in the path; the notification is proven authentic with that
// account's own secret before anything is read from it.
func NewPaymentCallbackRoutes(g fiber.Router, uc Callbacks, l logger.Interface) {
	r := &callbackRoutes{uc: uc, l: l}
	g.Post("/integrations/payments/:provider/:gatewayId", r.receive)
}

func (r *callbackRoutes) receive(c *fiber.Ctx) error {
	provider := strings.ToLower(c.Params("provider"))
	if provider != "xendit" && provider != "midtrans" {
		return fail(r.l, entity.ErrNotFound)
	}

	if _, err := uuid.Parse(c.Params("gatewayId")); err != nil {
		return fail(r.l, entity.ErrNotFound)
	}

	err := r.uc.Callback(c.UserContext(), provider, c.Params("gatewayId"),
		func(name string) string { return c.Get(name) }, c.Body())
	if err != nil {
		// A gateway retries anything but 2xx. Let it retry our own failures; tell it clearly
		// when the notification was not authentic.
		return fail(r.l, err)
	}

	return c.SendStatus(fiber.StatusOK)
}

// ------------------------------------------------------------- operator API

type gatewayRoutes struct {
	uc Gateways
	l  logger.Interface
}

func newGatewayRoutes(g fiber.Router, uc Gateways, l logger.Interface) {
	r := &gatewayRoutes{uc: uc, l: l}
	g.Put("/merchants/:id/gateway", r.set)
	g.Get("/merchants/:id/gateway", r.get)
	g.Delete("/merchants/:id/gateway", r.remove)
	g.Get("/merchants/:id/fees", r.fees)
	g.Post("/merchants/:id/fees/settle", r.settle)
}

func merchantID(c *fiber.Ctx) (string, error) {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return "", entity.ErrNotFound
	}

	return id.String(), nil
}

func (r *gatewayRoutes) set(c *fiber.Ctx) error {
	id, err := merchantID(c)
	if err != nil {
		return fail(r.l, err)
	}

	var body struct {
		Provider      string `json:"provider"`
		Secret        string `json:"secret"`
		CallbackToken string `json:"callbackToken"`
		Sandbox       bool   `json:"sandbox"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest("body is not valid JSON")
	}

	v, err := r.uc.SetGateway(c.UserContext(), id, payment.GatewayInput{
		Provider: body.Provider, Secret: body.Secret, CallbackToken: body.CallbackToken, Sandbox: body.Sandbox,
	})

	switch {
	case errors.Is(err, payment.ErrInvalid):
		return httpserver.NewUnprocessableEntity(err.Error(), nil).WithReason("GATEWAY_INVALID")
	case errors.Is(err, payment.ErrUnverified):
		return httpserver.NewUnprocessableEntity(err.Error(), nil).WithReason("GATEWAY_UNVERIFIED")
	case err != nil:
		return fail(r.l, err)
	}

	return ok(c, v)
}

func (r *gatewayRoutes) get(c *fiber.Ctx) error {
	id, err := merchantID(c)
	if err != nil {
		return fail(r.l, err)
	}

	v, err := r.uc.Gateway(c.UserContext(), id)
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, v)
}

func (r *gatewayRoutes) remove(c *fiber.Ctx) error {
	id, err := merchantID(c)
	if err != nil {
		return fail(r.l, err)
	}

	if err := r.uc.RemoveGateway(c.UserContext(), id); err != nil {
		return fail(r.l, err)
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// fees lists what a merchant owes the platform. from and to are dates (2026-11-01) or RFC 3339
// times; the period is [from, to) and defaults to the current month.
func (r *gatewayRoutes) fees(c *fiber.Ctx) error {
	id, err := merchantID(c)
	if err != nil {
		return fail(r.l, err)
	}

	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)

	if v := c.Query("from"); v != "" {
		if from, err = parseTime(v); err != nil {
			return badRequest("from is not a date")
		}
	}

	if v := c.Query("to"); v != "" {
		if to, err = parseTime(v); err != nil {
			return badRequest("to is not a date")
		}
	}

	if !to.After(from) {
		return badRequest("to must be after from")
	}

	st, err := r.uc.FeeStatement(c.UserContext(), id, from, to)
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, st)
}

// settle marks fees collected outside the subscription charge. through is a date or RFC 3339
// time: fees created before it are settled.
func (r *gatewayRoutes) settle(c *fiber.Ctx) error {
	id, err := merchantID(c)
	if err != nil {
		return fail(r.l, err)
	}

	var body struct {
		Through string `json:"through"`
		Note    string `json:"note"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest("body is not valid JSON")
	}

	through, err := parseTime(body.Through)
	if err != nil {
		return badRequest("through is not a date")
	}

	out, err := r.uc.SettleFeesManually(c.UserContext(), id, through, body.Note)
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, out)
}

func parseTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}

	return time.Parse("2006-01-02", v)
}
