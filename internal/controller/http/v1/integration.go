package v1

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
	"github.com/Calteras/streetfood-customer-api/pkg/signing"
)

// EventReceiver stores a verified batch of POS events.
type EventReceiver interface {
	Receive(ctx context.Context, keyID, timestamp, signature, method, path string, body []byte) error
	PendingFees(ctx context.Context, keyID, timestamp, signature, method, path string, body []byte) (entity.FeeReservation, error)
	ReserveFees(ctx context.Context, keyID, timestamp, signature, method, path string, body []byte) (entity.FeeReservation, error)
}

type integrationRoutes struct {
	uc EventReceiver
	l  logger.Interface
}

// NewIntegrationRoutes registers the endpoint the POS sends its events to.
func NewIntegrationRoutes(g fiber.Router, uc EventReceiver, l logger.Interface) {
	r := &integrationRoutes{uc: uc, l: l}
	g.Post("/integrations/pos/events", r.receive)
	g.Get("/integrations/pos/fees/pending", r.pendingFees)
	g.Post("/integrations/pos/fees/reserve", r.reserveFees)
}

// receive accepts a signed batch. It answers 204 once the batch is stored; the POS
// retries on anything else.
func (r *integrationRoutes) receive(c *fiber.Ctx) error {
	err := r.uc.Receive(
		c.UserContext(),
		c.Get(signing.HeaderKeyID),
		c.Get(signing.HeaderTimestamp),
		c.Get(signing.HeaderSignature),
		c.Method(),
		c.Path(), // the POS signs the path without a query string
		c.Body(),
	)
	if err != nil {
		return fail(r.l, err)
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// pendingFees tells the POS what its merchant owes now. Signed like events.
func (r *integrationRoutes) pendingFees(c *fiber.Ctx) error {
	out, err := r.uc.PendingFees(c.UserContext(), c.Get(signing.HeaderKeyID), c.Get(signing.HeaderTimestamp),
		c.Get(signing.HeaderSignature), c.Method(), c.Path(), c.Body())
	if err != nil {
		return fail(r.l, err)
	}

	c.Set(fiber.HeaderCacheControl, "no-store")

	return ok(c, out)
}

// reserveFees sets aside the fees of one billing period for the POS to add to a subscription
// charge. The same billingRef always gets the same answer.
func (r *integrationRoutes) reserveFees(c *fiber.Ctx) error {
	out, err := r.uc.ReserveFees(c.UserContext(), c.Get(signing.HeaderKeyID), c.Get(signing.HeaderTimestamp),
		c.Get(signing.HeaderSignature), c.Method(), c.Path(), c.Body())
	if err != nil {
		return fail(r.l, err)
	}

	c.Set(fiber.HeaderCacheControl, "no-store")

	return ok(c, out)
}
