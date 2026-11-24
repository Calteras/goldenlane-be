// Package http wires the HTTP routes.
package http

import (
	"net/http"

	"github.com/ansrivas/fiberprometheus/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/Calteras/streetfood-customer-api/config"
	"github.com/Calteras/streetfood-customer-api/internal/controller/http/middleware"
	v1 "github.com/Calteras/streetfood-customer-api/internal/controller/http/v1"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

// Dependencies are the use cases the routes call.
type Dependencies struct {
	Auth      middleware.Authenticator
	Events    v1.EventReceiver
	Discovery v1.Discoverer
	Profiles  v1.Profiles
	Merchants v1.Merchants
	Orders    v1.Orders
	Payments  Payments
	Hub       v1.Watcher
	// Streaming tunes the order stream; the zero value is right for production.
	Streaming v1.OrdersOptions
}

// Payments is the payment service as the routes use it.
type Payments interface {
	v1.Callbacks
	v1.Gateways
}

// NewRouter registers every route.
func NewRouter(app *fiber.App, cfg *config.Config, l logger.Interface, d Dependencies) {
	app.Use(recover.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins: cfg.HTTP.CORSAllowOrigins,
		AllowHeaders: "Authorization, Content-Type",
		AllowMethods: "GET, POST, PUT, PATCH, DELETE, OPTIONS",
	}))
	app.Use(middleware.Logger(l))

	if cfg.Metrics.Enabled {
		prometheus := fiberprometheus.New(cfg.App.Name)
		prometheus.RegisterAt(app, "/metrics")
		app.Use(prometheus.Middleware)
	}

	// Kubernetes and Docker probe.
	app.Get("/healthz", func(c *fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	api := app.Group("/v1")

	v1.NewIntegrationRoutes(api, d.Events, l)
	v1.NewPaymentCallbackRoutes(api, d.Payments, l)
	v1.NewDiscoveryRoutes(api, d.Discovery, l)
	v1.NewMeRoutes(api, middleware.Customer(d.Auth), d.Profiles, l)
	v1.NewOrdersRoutes(api, middleware.Customer(d.Auth), d.Orders, d.Hub, l, d.Streaming)
	v1.NewAdminRoutes(api.Group("/admin", middleware.Admin(cfg.Admin.Token)), d.Merchants, d.Payments, l)
}
