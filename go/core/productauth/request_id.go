package productauth

import (
	"crypto/rand"
	"encoding/base64"
)

// NewRequestID creates a server-compatible public request identifier while
// retaining the full 192 bits of random nonce material. It is not a token or
// PKCE generator: those protocols keep their existing byte/length contracts.
func NewRequestID() (string, error) {
	return newRequestID(func(buffer []byte) error {
		_, err := rand.Read(buffer)
		return err
	})
}

func (service *Service) randomRequestID() (string, error) {
	return newRequestID(service.random)
}

func newRequestID(random func([]byte) error) (string, error) {
	buffer := make([]byte, transactionBytes)
	if err := random(buffer); err != nil {
		return "", err
	}
	// Canonical TS identifier requires an alphanumeric first character.
	// Prefix instead of replacing a nonce character or rerolling randomness.
	return "req_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}
