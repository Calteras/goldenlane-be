// Package identity turns a verified sign-in token into a customer account and manages
// the customer's profile.
package identity

import (
	"context"
	"fmt"
	"strings"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

const provider = "firebase"

// Claims are the facts about the user a token proves.
type Claims struct {
	UserID string
	Phone  string
	Name   string
}

// TokenVerifier checks a bearer token with the sign-in provider.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (Claims, error)
}

// Store is what the use case needs from storage.
type Store interface {
	FindOrCreateCustomer(ctx context.Context, provider, providerUserID, phone, name string) (entity.Customer, error)
	GetCustomer(ctx context.Context, id string) (entity.Customer, error)
	UpdateCustomer(ctx context.Context, id string, name, avatarURL *string) (entity.Customer, error)
	DeleteCustomer(ctx context.Context, id string) error
	UpsertDevice(ctx context.Context, customerID, platform, token string) error
}

// UseCase is the identity service.
type UseCase struct {
	store    Store
	verifier TokenVerifier
}

// New builds the use case.
func New(store Store, verifier TokenVerifier) *UseCase {
	return &UseCase{store: store, verifier: verifier}
}

// Authenticate verifies a token and returns its customer, creating the account on first
// sign-in. The phone comes from the verified token, never from the request. Suspended or
// deleted accounts are refused.
func (u *UseCase) Authenticate(ctx context.Context, token string) (entity.Customer, error) {
	claims, err := u.verifier.Verify(ctx, token)
	if err != nil {
		return entity.Customer{}, entity.ErrUnauthorized
	}

	c, err := u.store.FindOrCreateCustomer(ctx, provider, claims.UserID, claims.Phone, claims.Name)
	if err != nil {
		return entity.Customer{}, err
	}

	if c.Status != "active" {
		return entity.Customer{}, entity.ErrForbidden
	}

	return c, nil
}

// Me returns the customer's profile.
func (u *UseCase) Me(ctx context.Context, id string) (entity.Customer, error) {
	return u.store.GetCustomer(ctx, id)
}

// UpdateProfile changes the name and avatar that were given.
func (u *UseCase) UpdateProfile(ctx context.Context, id string, name, avatarURL *string) (entity.Customer, error) {
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" || len([]rune(trimmed)) > 80 {
			return entity.Customer{}, fmt.Errorf("%w: name must be 1 to 80 characters", entity.ErrBadRequest)
		}

		name = &trimmed
	}

	if avatarURL != nil && (len(*avatarURL) > 500 || !strings.HasPrefix(*avatarURL, "https://")) {
		return entity.Customer{}, fmt.Errorf("%w: avatarUrl must be an https URL", entity.ErrBadRequest)
	}

	return u.store.UpdateCustomer(ctx, id, name, avatarURL)
}

// Delete closes the account and erases personal data.
func (u *UseCase) Delete(ctx context.Context, id string) error {
	return u.store.DeleteCustomer(ctx, id)
}

// RegisterDevice saves a push token for the customer.
func (u *UseCase) RegisterDevice(ctx context.Context, id, platform, token string) error {
	switch platform {
	case "ios", "android", "web":
	default:
		return fmt.Errorf("%w: platform must be ios, android or web", entity.ErrBadRequest)
	}

	if token = strings.TrimSpace(token); token == "" || len(token) > 4096 {
		return fmt.Errorf("%w: token is required", entity.ErrBadRequest)
	}

	return u.store.UpsertDevice(ctx, id, platform, token)
}
