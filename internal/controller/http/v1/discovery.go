package v1

import (
	"context"
	"math"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/discovery"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// Discoverer answers customers' search questions.
type Discoverer interface {
	Nearby(ctx context.Context, q discovery.Query) ([]discovery.Outlet, error)
	Outlet(ctx context.Context, id string) (discovery.Outlet, error)
	Menu(ctx context.Context, outletID string) (discovery.MenuView, error)
}

type discoveryRoutes struct {
	uc Discoverer
	l  logger.Interface
}

// NewDiscoveryRoutes registers the public search and browse endpoints.
func NewDiscoveryRoutes(g fiber.Router, uc Discoverer, l logger.Interface) {
	r := &discoveryRoutes{uc: uc, l: l}
	g.Get("/outlets/nearby", r.nearby)
	g.Get("/outlets/:id", r.outlet)
	g.Get("/outlets/:id/menu", r.menu)
}

func parseCoordinate(c *fiber.Ctx, name string, limit float64) (float64, error) {
	raw := c.Query(name)
	if raw == "" {
		return 0, badRequest(name + " is required")
	}

	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.Abs(v) > limit {
		return 0, badRequest(name + " is not a valid coordinate")
	}

	return v, nil
}

// nearby lists outlets around a position, nearest first.
//
//	GET /v1/outlets/nearby?lat=-6.2&lng=106.8&radius_m=2000&open_now=true&q=teh&limit=20
func (r *discoveryRoutes) nearby(c *fiber.Ctx) error {
	lat, err := parseCoordinate(c, "lat", 90)
	if err != nil {
		return err
	}

	lng, err := parseCoordinate(c, "lng", 180)
	if err != nil {
		return err
	}

	q := discovery.Query{Latitude: lat, Longitude: lng, Text: c.Query("q"), OpenNow: c.QueryBool("open_now", false)}

	if raw := c.Query("radius_m"); raw != "" {
		if q.RadiusM, err = strconv.ParseFloat(raw, 64); err != nil || q.RadiusM <= 0 {
			return badRequest("radius_m must be a positive number")
		}
	}

	if raw := c.Query("limit"); raw != "" {
		if q.Limit, err = strconv.Atoi(raw); err != nil || q.Limit <= 0 {
			return badRequest("limit must be a positive number")
		}
	}

	outlets, err := r.uc.Nearby(c.UserContext(), q)
	if err != nil {
		return fail(r.l, err)
	}

	c.Set(fiber.HeaderCacheControl, "no-store")

	return ok(c, fiber.Map{"outlets": outlets})
}

func (r *discoveryRoutes) outlet(c *fiber.Ctx) error {
	id, err := outletID(c)
	if err != nil {
		return err
	}

	o, err := r.uc.Outlet(c.UserContext(), id)
	if err != nil {
		return fail(r.l, err)
	}

	c.Set(fiber.HeaderCacheControl, "no-store")

	return ok(c, o)
}

func (r *discoveryRoutes) menu(c *fiber.Ctx) error {
	id, err := outletID(c)
	if err != nil {
		return err
	}

	m, err := r.uc.Menu(c.UserContext(), id)
	if err != nil {
		return fail(r.l, err)
	}

	// Prices change often; let clients keep a copy only briefly.
	c.Set(fiber.HeaderCacheControl, "public, max-age=15")

	return ok(c, m)
}

// outletID reads the :id parameter; anything that is not a UUID is simply not found.
func outletID(c *fiber.Ctx) (string, error) {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return "", fail(nil, entity.ErrNotFound)
	}

	return id.String(), nil
}
