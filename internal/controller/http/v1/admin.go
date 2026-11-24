package v1

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/merchant"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// Merchants registers and manages the businesses behind the outlets.
type Merchants interface {
	Register(ctx context.Context, in merchant.Registration) (entity.Merchant, error)
	List(ctx context.Context) ([]entity.Merchant, error)
	Suspend(ctx context.Context, id string) error
}

type adminRoutes struct {
	uc Merchants
	l  logger.Interface
}

// NewAdminRoutes registers the operator endpoints. g must already require the operator token.
func NewAdminRoutes(g fiber.Router, uc Merchants, pay Gateways, l logger.Interface) {
	r := &adminRoutes{uc: uc, l: l}
	newGatewayRoutes(g, pay, l)
	g.Post("/merchants", r.register)
	g.Get("/merchants", r.list)
	g.Delete("/merchants/:id", r.suspend)
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
