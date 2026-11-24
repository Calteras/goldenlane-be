// Package v1 holds the HTTP handlers of API version 1.
package v1

import (
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/merchant"
	"github.com/Calteras/streetfood-customer-api/pkg/httpserver"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// ok sends a successful answer in the common envelope.
func ok(c *fiber.Ctx, data any) error {
	return c.JSON(fiber.Map{"status": true, "message": "ok", "data": data})
}

// created sends a 201 answer.
func created(c *fiber.Ctx, data any) error {
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": true, "message": "created", "data": data})
}

// fail maps a domain error to an HTTP error. Anything unexpected is logged and hidden
// behind a plain 500: errors may quote data we must not echo.
func fail(l logger.Interface, err error) error {
	switch {
	case errors.Is(err, entity.ErrUnauthorized):
		return httpserver.NewUnauthorized("unauthorized", nil).WithReason("UNAUTHORIZED")
	case errors.Is(err, entity.ErrForbidden):
		return httpserver.NewForbidden("this account cannot use the app", nil).WithReason("ACCOUNT_UNAVAILABLE")
	case errors.Is(err, entity.ErrNotFound):
		return httpserver.NewNotFound("not found", nil).WithReason("NOT_FOUND")
	case errors.Is(err, entity.ErrConflict):
		return httpserver.NewConflict("already exists", nil).WithReason("CONFLICT")
	case errors.Is(err, entity.ErrBadRequest):
		return httpserver.NewBadRequest(err.Error(), nil).WithReason("BAD_REQUEST")
	case errors.Is(err, entity.ErrNotPayable):
		return httpserver.NewConflict("this order cannot be paid any more", nil).WithReason("NOT_PAYABLE")
	case errors.Is(err, merchant.ErrInvalid):
		return httpserver.NewUnprocessableEntity(err.Error(), nil).WithReason("MERCHANT_INVALID")
	case errors.Is(err, merchant.ErrUnverified):
		return httpserver.NewUnprocessableEntity(err.Error(), nil).WithReason("LINK_UNVERIFIED")
	default:
		l.Error("request failed", "error", err.Error())

		return httpserver.NewInternalServerError("internal server error", nil)
	}
}

func badRequest(msg string) error {
	return httpserver.NewBadRequest(msg, nil).WithReason("BAD_REQUEST")
}
