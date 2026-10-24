package licensing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Signer struct {
	keyID      string
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

type TokenClaims struct {
	Issuer        string     `json:"iss"`
	Subject       string     `json:"sub"`
	UserID        string     `json:"uid"`
	Email         string     `json:"email"`
	DeviceID      string     `json:"did"`
	EntitlementID string     `json:"eid"`
	Plan          Plan       `json:"plan"`
	LicenseType   string     `json:"license_type"`
	ValidUntil    *time.Time `json:"valid_until,omitempty"`
	MaxDevices    int        `json:"max_devices"`
	IssuedAt      time.Time  `json:"iat"`
	ExpiresAt     time.Time  `json:"exp"`
	TokenID       string     `json:"jti"`
}

type tokenHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

func NewSigner(keyID string, seed []byte) (*Signer, error) {
	if keyID == "" {
		keyID = "meadow-license-v1"
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("ed25519 seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Signer{keyID: keyID, privateKey: privateKey, publicKey: publicKey}, nil
}

func NewEphemeralSigner(keyID string) (*Signer, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return NewSigner(keyID, seed)
}

func ParseSeedBase64(raw string) ([]byte, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		seed, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(raw))
	}
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("decoded seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return seed, nil
}

func (s *Signer) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(s.publicKey)
}

func (s *Signer) KeyID() string {
	return s.keyID
}

func (s *Signer) Sign(claims TokenClaims) (string, error) {
	headerBytes, err := json.Marshal(tokenHeader{
		Algorithm: "EdDSA",
		Type:      "MEADOW-LICENSE",
		KeyID:     s.keyID,
	})
	if err != nil {
		return "", err
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString(headerBytes)
	payload := base64.RawURLEncoding.EncodeToString(claimsBytes)
	signed := header + "." + payload
	signature := ed25519.Sign(s.privateKey, []byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func VerifyToken(token string, publicKey ed25519.PublicKey) (TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return TokenClaims{}, errors.New("invalid token shape")
	}
	signed := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return TokenClaims{}, err
	}
	if !ed25519.Verify(publicKey, []byte(signed), signature) {
		return TokenClaims{}, errors.New("invalid token signature")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return TokenClaims{}, err
	}
	var header tokenHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return TokenClaims{}, err
	}
	if header.Algorithm != "EdDSA" || header.Type != "MEADOW-LICENSE" {
		return TokenClaims{}, errors.New("unsupported token header")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}, err
	}
	var claims TokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return TokenClaims{}, err
	}
	if time.Now().After(claims.ExpiresAt) {
		return TokenClaims{}, errors.New("token expired")
	}
	return claims, nil
}
