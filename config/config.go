// Package config reads the service settings from the environment (twelve-factor).
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

type (
	// Config holds every setting. See .env.example for what each one means.
	Config struct {
		App         App
		HTTP        HTTP
		Log         Log
		PG          PG
		Firebase    Firebase
		Admin       Admin
		Secrets     Secrets
		Integration Integration
		Workers     Workers
		Redis       Redis
		Payments    Payments
		Metrics     Metrics
	}

	// App identifies the deployment.
	App struct {
		Name    string `env:"APP_NAME" envDefault:"streetfood-customer-api"`
		Version string `env:"APP_VERSION" envDefault:"dev"`
		Env     string `env:"APP_ENV" envDefault:"development"`
	}

	// HTTP is the listener and browser access.
	HTTP struct {
		Port             string `env:"HTTP_PORT" envDefault:"8080"`
		CORSAllowOrigins string `env:"HTTP_CORS_ALLOW_ORIGINS" envDefault:"http://localhost:5173"`
	}

	// Log sets the verbosity.
	Log struct {
		Level string `env:"LOG_LEVEL" envDefault:"info"`
	}

	// PG is the database.
	PG struct {
		PoolMax int    `env:"PG_POOL_MAX" envDefault:"10"`
		URL     string `env:"PG_URL,required,notEmpty"`
	}

	// Firebase verifies customer sign-in tokens.
	Firebase struct {
		ProjectID string `env:"FIREBASE_PROJECT_ID,required,notEmpty"`
		// JWKSURL is Google's published signing keys; override only in tests.
		JWKSURL string `env:"FIREBASE_JWKS_URL" envDefault:"https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com"`
		// Issuer overrides https://securetoken.google.com/<project>; tests only.
		Issuer string `env:"FIREBASE_ISSUER"`
		// ServiceAccountFile is a Firebase service account key (JSON) that lets this API
		// send push notifications. Without it, push is off and the rest works.
		ServiceAccountFile string `env:"FCM_SERVICE_ACCOUNT_FILE"`
	}

	// Admin guards the operator endpoints that register merchants.
	Admin struct {
		Token string `env:"ADMIN_API_TOKEN,required,notEmpty"`
	}

	// Secrets protects merchant link secrets at rest.
	Secrets struct {
		// EncryptionKey is 32 random bytes, base64 encoded.
		EncryptionKey string `env:"ENCRYPTION_KEY,required,notEmpty"`
		// POSAllowedHosts lists the POS hostnames a merchant link may point to.
		POSAllowedHosts string `env:"POS_ALLOWED_HOSTS"`
	}

	// Integration tunes the POS link.
	Integration struct {
		// ReconcileInterval is how often each merchant's outlets are re-read from the POS, as
		// a safety net: the POS pushes every change.
		ReconcileInterval time.Duration `env:"INTEGRATION_RECONCILE_INTERVAL" envDefault:"1h"`
	}

	// Redis is optional. It only carries signals between instances of this service (see
	// pkg/signals); nothing is stored in it.
	Redis struct {
		// URL is redis://[:password@]host:port/db, or rediss:// for TLS. Empty means one instance.
		URL string `env:"REDIS_URL"`
	}

	// Workers tunes the background workers. They run when work is created (a commit wrote
	// rows), when a retry or deadline falls due, and otherwise only on this sweep, so an
	// idle service lets the database sleep (Neon suspends it after 5 idle minutes).
	Workers struct {
		// SweepInterval is the longest a worker sleeps with nothing scheduled. It only has to
		// catch work that another instance created or a wake-up that was missed.
		SweepInterval time.Duration `env:"WORKER_SWEEP_INTERVAL" envDefault:"1h"`
	}

	// Payments tunes online payment. Fees are in basis points (100 = 1%) of the order total.
	Payments struct {
		// CustomerFeeBps is added to what the customer pays; MerchantFeeBps is owed by the merchant.
		CustomerFeeBps int `env:"FEE_CUSTOMER_BPS" envDefault:"250"`
		MerchantFeeBps int `env:"FEE_MERCHANT_BPS" envDefault:"250"`
		// Window is how long a customer has to pay an order before it expires.
		Window time.Duration `env:"PAYMENT_WINDOW" envDefault:"15m"`
		// PublicBaseURL is this API's public address, shown to merchants for gateway callbacks.
		PublicBaseURL string `env:"PUBLIC_BASE_URL"`
		// ReturnURL is where gateways send customers after paying (an app deep link).
		ReturnURL string `env:"PAYMENT_RETURN_URL"`
		// Gateway addresses; empty means the real ones. For tests and staging stubs.
		XenditURL       string `env:"XENDIT_BASE_URL"`
		MidtransSnapURL string `env:"MIDTRANS_SNAP_URL"`
		MidtransAPIURL  string `env:"MIDTRANS_API_URL"`
	}

	// Metrics exposes Prometheus metrics at /metrics.
	Metrics struct {
		Enabled bool `env:"METRICS_ENABLED" envDefault:"true"`
	}
)

// Deployment stages accepted in APP_ENV.
const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

var (
	// ErrUnknownEnv means APP_ENV is not one of the deployment stages.
	ErrUnknownEnv = errors.New("APP_ENV must be development, staging or production")
	// ErrAdminToken means the operator token is too weak to guard the admin API.
	ErrAdminToken = errors.New("ADMIN_API_TOKEN must be at least 32 characters")
	// ErrEncryptionKey means the key is not 32 base64 encoded bytes.
	ErrEncryptionKey = errors.New("ENCRYPTION_KEY must be 32 bytes, base64 encoded")
	// ErrInsecureURL means a hosted stage allows a non-https browser origin.
	ErrInsecureURL = errors.New("staging and production need https:// browser origins")
	// ErrSweep means the worker sweep is too short to let the database sleep, or too long to be a safety net.
	ErrSweep = errors.New("WORKER_SWEEP_INTERVAL must be between 1m and 24h")
	// ErrFees means a fee is outside what makes sense.
	ErrFees = errors.New("FEE_CUSTOMER_BPS and FEE_MERCHANT_BPS must be between 0 and 2000")
)

// minAdminToken is the shortest operator token accepted.
const minAdminToken = 32

// New reads and validates the configuration.
func New() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	return cfg, nil
}

// IsProduction reports whether the app serves real users.
func (c *Config) IsProduction() bool { return c.App.Env == EnvProduction }

// POSHosts returns the allowed POS hostnames, lower case.
func (c *Config) POSHosts() []string {
	var hosts []string

	for h := range strings.SplitSeq(c.Secrets.POSAllowedHosts, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hosts = append(hosts, h)
		}
	}

	return hosts
}

// EncryptionKeyBytes returns the decoded key.
func (c *Config) EncryptionKeyBytes() ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(c.Secrets.EncryptionKey)
	if err != nil || len(key) != 32 {
		return nil, ErrEncryptionKey
	}

	return key, nil
}

func (c *Config) validate() error {
	switch c.App.Env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		return fmt.Errorf("%w, got %q", ErrUnknownEnv, c.App.Env)
	}

	if len(c.Admin.Token) < minAdminToken {
		return ErrAdminToken
	}

	if _, err := c.EncryptionKeyBytes(); err != nil {
		return err
	}

	if c.Workers.SweepInterval < time.Minute || c.Workers.SweepInterval > 24*time.Hour {
		return ErrSweep
	}

	for _, bps := range []int{c.Payments.CustomerFeeBps, c.Payments.MerchantFeeBps} {
		if bps < 0 || bps > 2000 {
			return ErrFees
		}
	}

	if c.App.Env != EnvDevelopment {
		for origin := range strings.SplitSeq(c.HTTP.CORSAllowOrigins, ",") {
			origin = strings.TrimSpace(origin)
			if origin == "" {
				continue
			}

			if u, err := url.Parse(origin); err != nil || u.Scheme != "https" || u.Host == "" {
				return fmt.Errorf("%w: %s", ErrInsecureURL, origin)
			}
		}
	}

	return nil
}
