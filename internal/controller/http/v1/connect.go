package v1

import (
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Calteras/streetfood-customer-api/internal/usecase/merchant"
	"github.com/Calteras/streetfood-customer-api/pkg/httpserver"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

type connectRoutes struct {
	uc Merchants
	l  logger.Interface
}

// NewConnectRoutes registers the call a business's own POS makes to join the platform. It has no
// operator token: the POS credentials are verified against the POS itself, which must also confirm
// the tenant, and only POS hosts in POS_ALLOWED_HOSTS are ever contacted.
func NewConnectRoutes(g fiber.Router, uc Merchants, l logger.Interface) {
	r := &connectRoutes{uc: uc, l: l}

	// Each call makes the API contact a POS, so it is rate limited per client.
	g.Post("/merchants/connect", newAttemptLimit(20, time.Minute), r.connect)
}

func (r *connectRoutes) connect(c *fiber.Ctx) error {
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

	m, wasCreated, err := r.uc.Connect(c.UserContext(), merchant.Registration{
		Name: body.Name, POSTenantID: body.POSTenantID, KeyID: body.KeyID, Secret: body.Secret, POSBaseURL: body.POSBaseURL,
	})
	if err != nil {
		return fail(r.l, err)
	}

	if wasCreated {
		return created(c, m)
	}

	return ok(c, m)
}

// newAttemptLimit allows at most max calls per client address in each window.
func newAttemptLimit(limit int, window time.Duration) fiber.Handler {
	var (
		mu    sync.Mutex
		seen  = map[string][]time.Time{}
		sweep time.Time
	)

	return func(c *fiber.Ctx) error {
		now := time.Now()
		ip := c.IP()

		mu.Lock()

		if now.After(sweep) {
			for k, v := range seen {
				if len(v) == 0 || now.Sub(v[len(v)-1]) > window {
					delete(seen, k)
				}
			}

			sweep = now.Add(window)
		}

		recent := seen[ip][:0]

		for _, t := range seen[ip] {
			if now.Sub(t) < window {
				recent = append(recent, t)
			}
		}

		if len(recent) >= limit {
			seen[ip] = recent
			mu.Unlock()

			return httpserver.NewTooManyRequests("too many attempts, try again in a minute", nil).WithReason("RATE_LIMITED")
		}

		seen[ip] = append(recent, now)
		mu.Unlock()

		return c.Next()
	}
}
