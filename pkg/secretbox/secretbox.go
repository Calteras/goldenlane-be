// Package secretbox encrypts small secrets at rest with AES-256-GCM. The context
// string is authenticated but not stored, so a ciphertext copied to another row
// cannot be decrypted there.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalid means the ciphertext is malformed, was tampered with, or belongs to another context.
var ErrInvalid = errors.New("secretbox: cannot decrypt")

// Box seals and opens values under one key.
type Box struct{ aead cipher.AEAD }

// New builds a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 { //nolint:mnd // AES-256
		return nil, errors.New("secretbox: key must be 32 bytes")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}

	return &Box{aead: aead}, nil
}

// Seal returns "iv.tag+ciphertext" as base64 parts.
func (b *Box) Seal(plaintext, context string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: %w", err)
	}

	sealed := b.aead.Seal(nil, nonce, []byte(plaintext), []byte(context))

	return base64.StdEncoding.EncodeToString(nonce) + "." + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal for the same context.
func (b *Box) Open(sealed, context string) (string, error) {
	nonceB64, bodyB64, ok := strings.Cut(sealed, ".")
	if !ok {
		return "", ErrInvalid
	}

	nonce, err1 := base64.StdEncoding.DecodeString(nonceB64)
	body, err2 := base64.StdEncoding.DecodeString(bodyB64)

	if err1 != nil || err2 != nil || len(nonce) != b.aead.NonceSize() {
		return "", ErrInvalid
	}

	plain, err := b.aead.Open(nil, nonce, body, []byte(context))
	if err != nil {
		return "", ErrInvalid
	}

	return string(plain), nil
}
