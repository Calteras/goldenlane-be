package firebase_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/firebase"
)

const project = "streetfood-test"

type keyPair struct {
	key *rsa.PrivateKey
	kid string
}

func newKey(t *testing.T, kid string) keyPair {
	t.Helper()

	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	return keyPair{key: k, kid: kid}
}

func (k keyPair) jwk() map[string]string {
	return map[string]string{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": k.kid,
		"n": base64.RawURLEncoding.EncodeToString(k.key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.key.E)).Bytes()),
	}
}

func (k keyPair) token(t *testing.T, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()

	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = k.kid

	signed, err := tok.SignedString(k.key)
	require.NoError(t, err)

	return signed
}

func claims(over jwt.MapClaims) jwt.MapClaims {
	c := jwt.MapClaims{
		"iss": "https://securetoken.google.com/" + project, "aud": project, "sub": "uid-123",
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		"auth_time": time.Now().Add(-time.Minute).Unix(), "phone_number": "+628123456789", "name": "Rina",
	}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}

	return c
}

func TestVerify(t *testing.T) {
	good := newKey(t, "good")
	other := newKey(t, "other")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{good.jwk()}})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	v, err := firebase.New(ctx, srv.URL, project, "")
	require.NoError(t, err)

	rs := jwt.SigningMethodRS256

	t.Run("a valid token", func(t *testing.T) {
		got, err := v.Verify(ctx, good.token(t, rs, claims(nil)))
		require.NoError(t, err)
		require.Equal(t, "uid-123", got.UserID)
		require.Equal(t, "+628123456789", got.Phone)
		require.Equal(t, "Rina", got.Name)
	})

	t.Run("a sign-in without a phone number", func(t *testing.T) {
		got, err := v.Verify(ctx, good.token(t, rs, claims(jwt.MapClaims{"phone_number": nil})))
		require.NoError(t, err)
		require.Empty(t, got.Phone)
	})

	for name, tok := range map[string]string{
		"wrong audience":        good.token(t, rs, claims(jwt.MapClaims{"aud": "another-project"})),
		"wrong issuer":          good.token(t, rs, claims(jwt.MapClaims{"iss": "https://securetoken.google.com/another"})),
		"expired":               good.token(t, rs, claims(jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()})),
		"no expiry":             good.token(t, rs, claims(jwt.MapClaims{"exp": nil})),
		"no subject":            good.token(t, rs, claims(jwt.MapClaims{"sub": nil})),
		"signed in the future":  good.token(t, rs, claims(jwt.MapClaims{"auth_time": time.Now().Add(time.Hour).Unix()})),
		"signed by another key": other.token(t, rs, claims(nil)),
		"not a token":           "garbage",
		"empty":                 "",
		"hmac with the public key": func() string {
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims(nil))
			tok.Header["kid"] = "good"
			s, _ := tok.SignedString(good.key.N.Bytes())

			return s
		}(),
		"alg none": func() string {
			tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims(nil))
			tok.Header["kid"] = "good"
			s, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)

			return s
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(ctx, tok)
			require.ErrorIs(t, err, firebase.ErrInvalidToken)
		})
	}
}
