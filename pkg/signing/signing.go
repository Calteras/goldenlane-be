// Package signing implements the request signature shared with the POS
// (docs/streetfood-os/INTEGRATION_CONTRACT.md section 2). Both directions use it.
//
//	canonical = timestamp \n METHOD \n path(+query) \n sha256hex(rawBody)
//	signature = hex(HMAC-SHA256(secret, canonical))
package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Header names of a signed request.
const (
	HeaderKeyID     = "X-SF-Key-Id"
	HeaderTimestamp = "X-SF-Timestamp"
	HeaderSignature = "X-SF-Signature"
)

// MaxSkew is how far a timestamp may be from now; MaxBodyBytes caps a signed body.
const (
	MaxSkew      = 300 * time.Second
	MaxBodyBytes = 1 << 20
)

// Failures of Verify. A caller reports both as 401 without saying which.
var (
	ErrSignatureInvalid = errors.New("signature invalid")
	ErrTimestampSkew    = errors.New("timestamp outside the allowed window")
)

var (
	timestampRe = regexp.MustCompile(`^\d{1,12}$`)
	signatureRe = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
)

// BodyDigest is the lowercase hex SHA-256 of the body (the empty body included).
func BodyDigest(body []byte) string {
	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:])
}

// Canonical builds the string that is signed.
func Canonical(timestamp, method, path string, body []byte) string {
	return strings.Join([]string{timestamp, strings.ToUpper(method), path, BodyDigest(body)}, "\n")
}

// Sign returns the hex signature of a request.
func Sign(secret, timestamp, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(Canonical(timestamp, method, path, body)))

	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a received request. now is injected for tests.
func Verify(secret, timestamp, signature, method, path string, body []byte, now time.Time) error {
	if !timestampRe.MatchString(timestamp) || !signatureRe.MatchString(signature) {
		return ErrSignatureInvalid
	}

	sent, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrSignatureInvalid
	}

	skew := now.Sub(time.Unix(sent, 0))
	if skew < 0 {
		skew = -skew
	}

	if skew > MaxSkew {
		return ErrTimestampSkew
	}

	expected := Sign(secret, timestamp, method, path, body)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(signature))) != 1 {
		return ErrSignatureInvalid
	}

	return nil
}
