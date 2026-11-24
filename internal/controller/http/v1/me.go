package v1

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"github.com/Calteras/streetfood-customer-api/internal/controller/http/middleware"
	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// Profiles manages the signed-in customer's account.
type Profiles interface {
	Me(ctx context.Context, id string) (entity.Customer, error)
	UpdateProfile(ctx context.Context, id string, name, avatarURL *string) (entity.Customer, error)
	Delete(ctx context.Context, id string) error
	RegisterDevice(ctx context.Context, id, platform, token string) error
}

type meRoutes struct {
	uc Profiles
	l  logger.Interface
}

// NewMeRoutes registers the account endpoints, each behind the given sign-in check.
// The check is attached per route: a middleware on a prefix-less group would also catch
// unknown paths and other groups' routes.
func NewMeRoutes(g fiber.Router, signedIn fiber.Handler, uc Profiles, l logger.Interface) {
	r := &meRoutes{uc: uc, l: l}
	g.Get("/me", signedIn, r.get)
	g.Patch("/me", signedIn, r.update)
	g.Delete("/me", signedIn, r.delete)
	g.Post("/devices", signedIn, r.device)
}

func (r *meRoutes) get(c *fiber.Ctx) error {
	me, err := r.uc.Me(c.UserContext(), middleware.CustomerFrom(c).ID)
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, me)
}

func (r *meRoutes) update(c *fiber.Ctx) error {
	var body struct {
		Name      *string `json:"name"`
		AvatarURL *string `json:"avatarUrl"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest("body is not valid JSON")
	}

	me, err := r.uc.UpdateProfile(c.UserContext(), middleware.CustomerFrom(c).ID, body.Name, body.AvatarURL)
	if err != nil {
		return fail(r.l, err)
	}

	return ok(c, me)
}

func (r *meRoutes) delete(c *fiber.Ctx) error {
	if err := r.uc.Delete(c.UserContext(), middleware.CustomerFrom(c).ID); err != nil {
		return fail(r.l, err)
	}

	return c.SendStatus(fiber.StatusNoContent)
}

func (r *meRoutes) device(c *fiber.Ctx) error {
	var body struct {
		Platform string `json:"platform"`
		Token    string `json:"token"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest("body is not valid JSON")
	}

	if err := r.uc.RegisterDevice(c.UserContext(), middleware.CustomerFrom(c).ID, body.Platform, body.Token); err != nil {
		return fail(r.l, err)
	}

	return c.SendStatus(fiber.StatusNoContent)
}
