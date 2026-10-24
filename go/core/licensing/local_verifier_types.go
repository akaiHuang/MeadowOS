package licensing

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"
)

const (
	EntitlementEnvelopeSchema = "meadow.entitlement.v1"
	RevocationEnvelopeSchema  = "meadow.revocations.v1"
	DeviceBindingSHA256       = "sha256"
)

var (
	// ErrLicenseDenied is returned, directly or through errors.Is, for every
	// verification failure. Callers must treat every non-nil error as denied.
	ErrLicenseDenied = errors.New("licensing: authorization denied")

	// Store implementations use these sentinels only when a value has never
	// been written. Corrupt, unreadable, or unavailable state must return a
	// different error so verification fails closed.
	ErrSecureStoreNotFound       = errors.New("licensing: secure value not found")
	ErrAntiRollbackStateNotFound = errors.New("licensing: anti-rollback state not found")
	ErrAntiRollbackConflict      = errors.New("licensing: anti-rollback compare-and-swap conflict")
)

// DeviceBinding is a SHA-256 digest of a product-defined, stable device
// identity. Digest is canonical unpadded base64url for exactly 32 bytes.
type DeviceBinding struct {
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
}

// CanonicalEntitlement is the signed authorization payload. Unix timestamps
// are UTC seconds. Capabilities must be strictly sorted and unique.
type CanonicalEntitlement struct {
	Product             string        `json:"product"`
	Version             uint64        `json:"version"`
	Capabilities        []string      `json:"capabilities"`
	IssuedAt            int64         `json:"issued_at"`
	NotBefore           int64         `json:"not_before"`
	ExpiresAt           int64         `json:"expires_at"`
	DeviceBinding       DeviceBinding `json:"device_binding"`
	ActivationID        string        `json:"activation_id"`
	OfflineGraceSeconds uint64        `json:"offline_grace_seconds"`
	RevocationEpoch     uint64        `json:"revocation_epoch"`
}

// SignedEntitlementEnvelope is serialized as canonical JSON. Signature is
// canonical unpadded base64url and signs EntitlementEnvelopeSigningBytes.
type SignedEntitlementEnvelope struct {
	Schema      string               `json:"schema"`
	KeyID       string               `json:"key_id"`
	Entitlement CanonicalEntitlement `json:"entitlement"`
	Signature   string               `json:"signature"`
}

// CanonicalRevocationList is a signed, monotonically versioned snapshot.
// Both revoked lists must be present, strictly sorted, and unique.
type CanonicalRevocationList struct {
	Product              string   `json:"product"`
	Epoch                uint64   `json:"epoch"`
	IssuedAt             int64    `json:"issued_at"`
	ExpiresAt            int64    `json:"expires_at"`
	RevokedActivationIDs []string `json:"revoked_activation_ids"`
	RevokedKeyIDs        []string `json:"revoked_key_ids"`
}

// SignedRevocationListEnvelope is serialized as canonical JSON. Signature is
// canonical unpadded base64url and signs RevocationEnvelopeSigningBytes.
type SignedRevocationListEnvelope struct {
	Schema     string                  `json:"schema"`
	KeyID      string                  `json:"key_id"`
	Revocations CanonicalRevocationList `json:"revocations"`
	Signature  string                  `json:"signature"`
}

// SecureStore persists verified canonical artifacts in an OS-protected store.
// StoreSecureValues must atomically commit all entries or none of them.
type SecureStore interface {
	LoadSecureValue(ctx context.Context, key string) ([]byte, error)
	StoreSecureValues(ctx context.Context, values map[string][]byte) error
}

// AntiRollbackState is security-sensitive monotonic state. ObservedUnix is the
// greatest trusted wall-clock second and RevocationEpoch is the greatest
// accepted signed revocation epoch.
type AntiRollbackState struct {
	ObservedUnix    int64
	RevocationEpoch uint64
}

// AntiRollbackStore must be tamper-resistant. CompareAndSwapAntiRollback must
// atomically replace previous with next. A nil previous means no state existed.
// Mismatch must return ErrAntiRollbackConflict; it must never overwrite a newer
// state. This contract prevents concurrent verifiers from lowering the floor.
type AntiRollbackStore interface {
	LoadAntiRollback(ctx context.Context, product string) (AntiRollbackState, error)
	CompareAndSwapAntiRollback(ctx context.Context, product string, previous *AntiRollbackState, next AntiRollbackState) error
}

// LocalVerifierConfig contains public verification material only. Private keys
// must remain in the issuer and are intentionally unsupported by this API.
type LocalVerifierConfig struct {
	Product                string
	Version                uint64
	PublicKeys             map[string]ed25519.PublicKey
	RequiredCapabilities   []string
	MaxOfflineGrace        time.Duration
	FutureClockSkew        time.Duration
	ClockRollbackTolerance time.Duration
	SecureStore            SecureStore
	AntiRollbackStore      AntiRollbackStore
	Now                    func() time.Time
}

type LocalVerifyRequest struct {
	DeviceBinding DeviceBinding
}

type LocalVerificationResult struct {
	Entitlement        CanonicalEntitlement
	RevocationEpoch    uint64
	EffectiveExpiresAt time.Time
	InOfflineGrace     bool
}

// LocalVerificationError exposes a stable reason without leaking parsing,
// storage, or cryptographic details. It always matches ErrLicenseDenied.
type LocalVerificationError struct {
	Reason string
}

func (e *LocalVerificationError) Error() string {
	return ErrLicenseDenied.Error() + ": " + e.Reason
}

func (e *LocalVerificationError) Unwrap() error {
	return ErrLicenseDenied
}

