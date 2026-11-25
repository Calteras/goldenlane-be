// Package identity turns a verified sign-in token into a customer account and manages
// the customer's profile.
package identity

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

const provider = "firebase"

const (
	// _accountTTL is how long a signed-in customer's account is remembered. Firebase has
	// already proved who the caller is; without this, every request would also open a
	// transaction to look the account up, which keeps a serverless database awake while
	// anyone has the app open. A suspended or deleted account is refused within this time
	// (at once on this instance and, with Redis, on the others when it is deleted here).
	_accountTTL = 5 * time.Minute

	// _accountCap bounds the memory: past it, expired entries go first, then everything.
	_accountCap = 10000
)

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
	now      func() time.Time

	mu       sync.Mutex
	accounts map[string]remembered // by sign-in identity
	byID     map[string]string     // customer id to identity
	onForget func(customerID string)
}

type remembered struct {
	customer entity.Customer
	until    time.Time
}

// New builds the use case.
func New(store Store, verifier TokenVerifier) *UseCase {
	return &UseCase{
		store: store, verifier: verifier, now: time.Now,
		accounts: map[string]remembered{}, byID: map[string]string{},
	}
}

// OnForget sets what is called, with the customer's id, when this instance changes an account
// that other instances may have remembered (to tell them over Redis).
func (u *UseCase) OnForget(fn func(customerID string)) { u.onForget = fn }

// Forget drops a customer's remembered account, after a change made here or on another instance.
func (u *UseCase) Forget(customerID string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if key, ok := u.byID[customerID]; ok {
		delete(u.accounts, key)
		delete(u.byID, customerID)
	}
}

func (u *UseCase) changed(customerID string) {
	u.Forget(customerID)

	if u.onForget != nil {
		u.onForget(customerID)
	}
}

func (u *UseCase) recall(key string) (entity.Customer, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	r, ok := u.accounts[key]
	if !ok || !u.now().Before(r.until) {
		return entity.Customer{}, false
	}

	return r.customer, true
}

func (u *UseCase) remember(key string, c entity.Customer) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if len(u.accounts) >= _accountCap {
		now := u.now()

		for k, r := range u.accounts {
			if !now.Before(r.until) {
				delete(u.accounts, k)
				delete(u.byID, r.customer.ID)
			}
		}

		if len(u.accounts) >= _accountCap {
			u.accounts, u.byID = map[string]remembered{}, map[string]string{}
		}
	}

	u.accounts[key] = remembered{customer: c, until: u.now().Add(_accountTTL)}
	u.byID[c.ID] = key
}

// Authenticate verifies a token and returns its customer, creating the account on first
// sign-in. The phone comes from the verified token, never from the request. Suspended or
// deleted accounts are refused.
func (u *UseCase) Authenticate(ctx context.Context, token string) (entity.Customer, error) {
	claims, err := u.verifier.Verify(ctx, token)
	if err != nil {
		return entity.Customer{}, entity.ErrUnauthorized
	}

	key := provider + ":" + claims.UserID

	c, ok := u.recall(key)
	if !ok {
		var err error

		if c, err = u.store.FindOrCreateCustomer(ctx, provider, claims.UserID, claims.Phone, claims.Name); err != nil {
			return entity.Customer{}, err
		}

		u.remember(key, c)
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

	c, err := u.store.UpdateCustomer(ctx, id, name, avatarURL)
	if err == nil {
		u.changed(id)
	}

	return c, err
}

// Delete closes the account and erases personal data.
func (u *UseCase) Delete(ctx context.Context, id string) error {
	err := u.store.DeleteCustomer(ctx, id)
	if err == nil {
		u.changed(id)
	}

	return err
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
