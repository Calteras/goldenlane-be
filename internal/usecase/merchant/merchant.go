// Package merchant registers the businesses whose POS feeds this platform.
package merchant

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
	"github.com/Calteras/streetfood-customer-api/pkg/secretbox"
)

var (
	keyIDRe = regexp.MustCompile(`^sfk_[a-f0-9]{24}$`)

	// ErrUnverified means the POS did not accept the credentials we were given.
	ErrUnverified = errors.New("the POS did not accept these credentials")
	// ErrInvalid means the registration request is malformed.
	ErrInvalid = errors.New("invalid merchant registration")
)

// Store is what the use case needs from storage.
type Store interface {
	CreateMerchant(ctx context.Context, id, name, posTenantID, keyID, secretEnc, baseURL string) (entity.Merchant, error)
	ListMerchants(ctx context.Context) ([]entity.Merchant, error)
	LinkByMerchant(ctx context.Context, merchantID string) (entity.Link, error)
	Suspend(ctx context.Context, merchantID string) error
	UpdateLink(ctx context.Context, merchantID, keyID, secretEnc, baseURL string) (entity.Merchant, error)
}

// Reconciler pulls a new merchant's outlets and menus right away.
type Reconciler interface {
	ReconcileLink(ctx context.Context, link entity.Link) error
}

// Verifier proves credentials work by reading the merchant's published outlets.
type Verifier interface {
	Verify(ctx context.Context, cred pos.Credentials) error
}

// Settings limits where a POS may be.
type Settings struct {
	AllowedHosts []string
	Production   bool
}

// UseCase is the merchant service.
type UseCase struct {
	store      Store
	verifier   Verifier
	reconciler Reconciler
	box        *secretbox.Box
	settings   Settings
	log        logger.Interface
}

// New builds the use case.
func New(store Store, verifier Verifier, reconciler Reconciler, box *secretbox.Box, settings Settings, log logger.Interface) *UseCase {
	return &UseCase{store: store, verifier: verifier, reconciler: reconciler, box: box, settings: settings, log: log}
}

// Registration is what an operator submits after the merchant created a link in the POS.
type Registration struct {
	Name        string
	POSTenantID string
	KeyID       string
	Secret      string
	POSBaseURL  string
}

// Register verifies the credentials against the POS, stores the merchant with its secret
// encrypted, and starts loading its outlets and menus.
func (u *UseCase) Register(ctx context.Context, in Registration) (entity.Merchant, error) {
	base, err := u.checkBaseURL(in.POSBaseURL)
	if err != nil {
		return entity.Merchant{}, err
	}

	in.Name = strings.TrimSpace(in.Name)
	in.POSTenantID = strings.TrimSpace(in.POSTenantID)

	switch {
	case in.Name == "" || len(in.Name) > 120:
		return entity.Merchant{}, fmt.Errorf("%w: name is required (up to 120 characters)", ErrInvalid)
	case in.POSTenantID == "" || len(in.POSTenantID) > 100:
		return entity.Merchant{}, fmt.Errorf("%w: posTenantId is required", ErrInvalid)
	case !keyIDRe.MatchString(in.KeyID):
		return entity.Merchant{}, fmt.Errorf("%w: keyId is not a POS key id", ErrInvalid)
	case !strings.HasPrefix(in.Secret, "sfs_") || len(in.Secret) > 200:
		return entity.Merchant{}, fmt.Errorf("%w: secret is not a POS secret", ErrInvalid)
	}

	cred := pos.Credentials{BaseURL: base, KeyID: in.KeyID, Secret: in.Secret}
	if err := u.verifier.Verify(ctx, cred); err != nil {
		return entity.Merchant{}, fmt.Errorf("%w: %s", ErrUnverified, err.Error())
	}

	id := uuid.NewString()

	sealed, err := u.box.Seal(in.Secret, id)
	if err != nil {
		return entity.Merchant{}, err
	}

	m, err := u.store.CreateMerchant(ctx, id, in.Name, in.POSTenantID, in.KeyID, sealed, base)
	if err != nil {
		return entity.Merchant{}, err
	}

	// Load outlets and menus now so the merchant is searchable at once. A failure here is
	// not fatal: the periodic reconciliation retries.
	if link, err := u.store.LinkByMerchant(ctx, m.ID); err == nil {
		if err := u.reconciler.ReconcileLink(ctx, link); err != nil {
			u.log.Warn("first sync of merchant failed", "merchant", m.ID, "error", err.Error())
		}
	}

	return m, nil
}

// LinkUpdate is a registered business's new POS credentials, after it created a new secret or
// disconnected and connected again.
type LinkUpdate struct {
	KeyID      string
	Secret     string
	POSBaseURL string
}

// UpdateLink replaces a merchant's POS credentials. They are tested against the POS first, so a
// mistake cannot cut a working merchant off. Nothing else about the merchant changes. The
// outlets are re-read at once.
func (u *UseCase) UpdateLink(ctx context.Context, id string, in LinkUpdate) (entity.Merchant, error) {
	base, err := u.checkBaseURL(in.POSBaseURL)
	if err != nil {
		return entity.Merchant{}, err
	}

	switch {
	case !keyIDRe.MatchString(in.KeyID):
		return entity.Merchant{}, fmt.Errorf("%w: keyId is not a POS key id", ErrInvalid)
	case !strings.HasPrefix(in.Secret, "sfs_") || len(in.Secret) > 200:
		return entity.Merchant{}, fmt.Errorf("%w: secret is not a POS secret", ErrInvalid)
	}

	if _, err := uuid.Parse(id); err != nil {
		return entity.Merchant{}, entity.ErrNotFound
	}

	if _, err := u.store.LinkByMerchant(ctx, id); err != nil {
		return entity.Merchant{}, err
	}

	cred := pos.Credentials{BaseURL: base, KeyID: in.KeyID, Secret: in.Secret}
	if err := u.verifier.Verify(ctx, cred); err != nil {
		return entity.Merchant{}, fmt.Errorf("%w: %s", ErrUnverified, err.Error())
	}

	sealed, err := u.box.Seal(in.Secret, id)
	if err != nil {
		return entity.Merchant{}, err
	}

	m, err := u.store.UpdateLink(ctx, id, in.KeyID, sealed, base)
	if err != nil {
		return entity.Merchant{}, err
	}

	// Pick up outlets and menus right away. The periodic reconciliation retries a failure.
	if link, err := u.store.LinkByMerchant(ctx, id); err == nil {
		if err := u.reconciler.ReconcileLink(ctx, link); err != nil {
			u.log.Warn("sync after a link update failed", "merchant", id, "error", err.Error())
		}
	}

	return m, nil
}

// List returns all merchants.
func (u *UseCase) List(ctx context.Context) ([]entity.Merchant, error) {
	return u.store.ListMerchants(ctx)
}

// Suspend stops a merchant and removes its outlets from search.
func (u *UseCase) Suspend(ctx context.Context, id string) error {
	return u.store.Suspend(ctx, id)
}

// checkBaseURL accepts an https origin on an allowed host. Plain http to localhost is
// allowed outside production for development. This keeps the platform from being pointed
// at internal addresses.
func (u *UseCase) checkBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" {
		return "", fmt.Errorf("%w: posBaseUrl must be an origin such as https://pos.example.com", ErrInvalid)
	}

	host := strings.ToLower(parsed.Hostname())
	local := !u.settings.Production && (host == "localhost" || host == "127.0.0.1")

	switch {
	case parsed.Scheme == "https" && slices.Contains(u.settings.AllowedHosts, host):
	case local && (parsed.Scheme == "http" || parsed.Scheme == "https"):
	default:
		return "", fmt.Errorf("%w: the POS host is not allowed (POS_ALLOWED_HOSTS)", ErrInvalid)
	}

	return parsed.Scheme + "://" + parsed.Host, nil
}
