package v1

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/merchant"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/trust"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// Merchants registers and manages the businesses behind the outlets.
type Merchants interface {
	Register(ctx context.Context, in merchant.Registration) (entity.Merchant, error)
	List(ctx context.Context) ([]entity.Merchant, error)
	Suspend(ctx context.Context, id string) error
	UpdateLink(ctx context.Context, id string, in merchant.LinkUpdate) (entity.Merchant, error)
	Connect(ctx context.Context, in merchant.Registration) (entity.Merchant, bool, error)
}

// Trust is what the operator can read about customers' trust (read only for now).
type Trust interface {
	Report(ctx context.Context, customerID string) (trust.Report, error)
	Distribution(ctx context.Context) (trust.Distribution, error)
	Vendors(ctx context.Context) (trust.VendorReport, error)
}

type adminRoutes struct {
	uc Merchants
	tr Trust
	l  logger.Interface
}

// NewAdminRoutes registers the operator endpoints. g must already require the operator token.
func NewAdminRoutes(g fiber.Router, uc Merchants, pay Gateways, tr Trust, l logger.Interface) {
	r := &adminRoutes{uc: uc, tr: tr, l: l}
	newGatewayRoutes(g, pay, l)
	g.Get("/customers/:id/trust", r.customerTrust)
	g.Get("/trust/distribution", r.trustDistribution)
	g.Get("/trust/vendors", r.trustVendors)
	g.Post("/merchants", r.register)
	g.Get("/merchants", r.list)
	g.Delete("/merchants/:id", r.suspend)
	g.Put("/merchants/:id/link", r.updateLink)
}

// register links a merchant's POS. The credentials come from the owner's link in the POS.
func (r *adminRoutes) register(c *fiber.Ctx) error {
	var body struct {
		Name        string `json:"name"`
		POSTenantID string `json:"posTenantId"`
		KeyID       string `json:"keyId"`
		Secret      string `json:"secret"`
		POSBaseURL  string `json:"posBaseUrl"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest("body is not valid JSON")
	}

	m, err := r.uc.Register(c.UserContext(), merchant.Registration{
		Name: body.Name, POSTenantID: body.POSTenantID, KeyID: body.KeyID, Secret: body.Secret, POSBaseURL: body.POSBaseURL,
	})
	if err != nil {
		return fail(r.l, err)
	}

	return created(c, m)
}

func (r *adminRoutes) list(c *fiber.Ctx) error {
	list, err := r.uc.List(c.UserContext())
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, fiber.Map{"merchants": list})
}

func (r *adminRoutes) suspend(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fail(r.l, entity.ErrNotFound)
	}

	if err := r.uc.Suspend(c.UserContext(), id.String()); err != nil {
		return fail(r.l, err)
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// updateLink replaces a registered business's POS credentials. The owner gets a new secret in the
// POS when they rotate it or reconnect, and the operator passes it on here.
func (r *adminRoutes) updateLink(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fail(r.l, entity.ErrNotFound)
	}

	var body struct {
		KeyID      string `json:"keyId"`
		Secret     string `json:"secret"`
		POSBaseURL string `json:"posBaseUrl"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest("body is not valid JSON")
	}

	m, err := r.uc.UpdateLink(c.UserContext(), id.String(), merchant.LinkUpdate{
		KeyID: body.KeyID, Secret: body.Secret, POSBaseURL: body.POSBaseURL,
	})
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, m)
}

// customerTrust shows where one customer stands and the events behind it: the score, the tier,
// the readable reasons, what the policy would do, and the latest events. Computing it also
// refreshes the stored profile.
func (r *adminRoutes) customerTrust(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fail(r.l, entity.ErrNotFound)
	}

	report, err := r.tr.Report(c.UserContext(), id.String())
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, report)
}

// trustDistribution shows how customers spread over the tiers, so thresholds are chosen on real
// data before any of them is enforced.
func (r *adminRoutes) trustDistribution(c *fiber.Ctx) error {
	d, err := r.tr.Distribution(c.UserContext())
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, d)
}

// trustVendors compares the merchants with each other: how often each calls a customer a no-show,
// cancels, or rejects, next to the median of the others, and which ones stand out. A vendor that
// stands out has its reports about customers counted for less.
func (r *adminRoutes) trustVendors(c *fiber.Ctx) error {
	report, err := r.tr.Vendors(c.UserContext())
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, report)
}
