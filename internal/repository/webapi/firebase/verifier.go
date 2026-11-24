// Package firebase verifies Firebase Authentication ID tokens: RS256 signed by
// Google's published keys, for this project, not expired.
package firebase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/Calteras/streetfood-customer-api/internal/usecase/identity"
)

// ErrInvalidToken is any reason a token is not accepted.
var ErrInvalidToken = errors.New("invalid token")

const _maxSubjectLength = 128

// Verifier checks ID tokens. The signing keys are fetched from Google and refreshed in
// the background.
type Verifier struct {
	keys      keyfunc.Keyfunc
	projectID string
	issuer    string
	now       func() time.Time
}

// New builds a verifier. ctx bounds the background key refresh. issuer may be empty
// (then https://securetoken.google.com/<project>).
func New(ctx context.Context, jwksURL, projectID, issuer string) (*Verifier, error) {
	keys, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("firebase keys: %w", err)
	}

	if issuer == "" {
		issuer = "https://securetoken.google.com/" + projectID
	}

	return &Verifier{keys: keys, projectID: projectID, issuer: issuer, now: time.Now}, nil
}

type claims struct {
	jwt.RegisteredClaims
	Phone    string `json:"phone_number"`
	Name     string `json:"name"`
	AuthTime int64  `json:"auth_time"`
}

// Verify returns the claims of a valid token.
func (v *Verifier) Verify(_ context.Context, token string) (identity.Claims, error) {
	var c claims

	parsed, err := jwt.ParseWithClaims(token, &c, v.keys.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.projectID),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
		jwt.WithLeeway(10*time.Second),
	)
	if err != nil || !parsed.Valid {
		return identity.Claims{}, ErrInvalidToken
	}

	// Firebase's documented extra checks: a subject, and sign-in in the past.
	if c.Subject == "" || len(c.Subject) > _maxSubjectLength || c.AuthTime > v.now().Add(10*time.Second).Unix() {
		return identity.Claims{}, ErrInvalidToken
	}

	return identity.Claims{UserID: c.Subject, Phone: c.Phone, Name: c.Name}, nil
}
