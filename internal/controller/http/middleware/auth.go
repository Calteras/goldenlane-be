// Package middleware holds the cross-cutting HTTP concerns.
package middleware

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/pkg/httpserver"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// CustomerLocal is where the authenticated customer is kept for the request.
const CustomerLocal = "customer"

// Authenticator turns a bearer token into a customer.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (entity.Customer, error)
}

func bearer(c *fiber.Ctx) (string, bool) {
	h := c.Get(fiber.HeaderAuthorization)
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:]), true
	}

	return "", false
}

// Customer requires a valid customer sign-in token.
func Customer(auth Authenticator) fiber.Handler {
	return func(c *fiber.Ctx) error {
		token, found := bearer(c)
		if !found {
			return httpserver.NewUnauthorized("sign in required", nil).WithReason("UNAUTHORIZED")
		}

		customer, err := auth.Authenticate(c.UserContext(), token)

		switch {
		case err == nil:
			c.Locals(CustomerLocal, customer)

			return c.Next()
		case err == entity.ErrForbidden:
			return httpserver.NewForbidden("this account cannot use the app", nil).WithReason("ACCOUNT_UNAVAILABLE")
		case err == entity.ErrUnauthorized:
			return httpserver.NewUnauthorized("sign in required", nil).WithReason("UNAUTHORIZED")
		default:
			return httpserver.NewInternalServerError("internal server error", err)
		}
	}
}

// CustomerFrom returns the authenticated customer set by Customer.
func CustomerFrom(c *fiber.Ctx) entity.Customer {
	customer, _ := c.Locals(CustomerLocal).(entity.Customer)

	return customer
}

// Admin requires the operator token, compared in constant time.
func Admin(token string) fiber.Handler {
	want := []byte(token)

	return func(c *fiber.Ctx) error {
		got, found := bearer(c)
		if !found || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			return httpserver.NewUnauthorized("unauthorized", nil).WithReason("UNAUTHORIZED")
		}

		return c.Next()
	}
}

// Logger writes one line per request. The path is logged, the query string is not:
// searches carry the customer's position.
func Logger(l logger.Interface) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		// The error handler renders after this middleware, so work out the status it will use.
		status := c.Response().StatusCode()

		var appErr *httpserver.AppError

		var fiberErr *fiber.Error

		switch {
		case errors.As(err, &appErr):
			status = appErr.Code
		case errors.As(err, &fiberErr):
			status = fiberErr.Code
		case err != nil:
			status = fiber.StatusInternalServerError
		}

		l.Info("request", "method", c.Method(), "path", c.Path(), "status", status, "ms", time.Since(start).Milliseconds())

		return err
	}
}
