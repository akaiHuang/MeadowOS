package licensing

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	secureEntitlementSuffix = "entitlement"
	secureRevocationSuffix  = "revocations"
)

// LocalVerifier verifies signed local entitlements without issuer secrets.
type LocalVerifier struct {
	product                string
	version                uint64
	publicKeys             map[string]ed25519.PublicKey
	requiredCapabilities   []string
	maxOfflineGraceSeconds uint64
	futureClockSkewSeconds  int64
	rollbackTolerance      int64
	secureStore            SecureStore
	antiRollbackStore      AntiRollbackStore
	now                    func() time.Time
}

func NewLocalVerifier(config LocalVerifierConfig) (*LocalVerifier, error) {
	if err := validateName("product", config.Product); err != nil {
		return nil, err
	}
	if config.Version == 0 {
		return nil, errors.New("licensing: verifier version must be non-zero")
	}
	if config.SecureStore == nil {
		return nil, errors.New("licensing: secure store is required")
	}
	if config.AntiRollbackStore == nil {
		return nil, errors.New("licensing: anti-rollback store is required")
	}
	if len(config.PublicKeys) == 0 {
		return nil, errors.New("licensing: at least one public key is required")
	}
	if config.MaxOfflineGrace < 0 || config.FutureClockSkew < 0 || config.ClockRollbackTolerance < 0 {
		return nil, errors.New("licensing: verifier durations cannot be negative")
	}
	maxSeconds, err := durationSeconds(config.MaxOfflineGrace)
	if err != nil {
		return nil, fmt.Errorf("licensing: max offline grace: %w", err)
	}
	futureSeconds, err := signedDurationSeconds(config.FutureClockSkew)
	if err != nil {
		return nil, fmt.Errorf("licensing: future clock skew: %w", err)
	}
	rollbackSeconds, err := signedDurationSeconds(config.ClockRollbackTolerance)
	if err != nil {
		return nil, fmt.Errorf("licensing: rollback tolerance: %w", err)
	}

	keys := make(map[string]ed25519.PublicKey, len(config.PublicKeys))
	for keyID, publicKey := range config.PublicKeys {
		if err := validateKeyID(keyID); err != nil {
			return nil, err
		}
		if len(publicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("licensing: public key %q must be %d bytes", keyID, ed25519.PublicKeySize)
		}
		keys[keyID] = append(ed25519.PublicKey(nil), publicKey...)
	}

	required := append([]string(nil), config.RequiredCapabilities...)
	sort.Strings(required)
	if err := validateSortedNames("required capability", required); err != nil {
		return nil, err
	}

	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &LocalVerifier{
		product:                config.Product,
		version:                config.Version,
		publicKeys:             keys,
		requiredCapabilities:   required,
		maxOfflineGraceSeconds: maxSeconds,
		futureClockSkewSeconds:  futureSeconds,
		rollbackTolerance:      rollbackSeconds,
		secureStore:            config.SecureStore,
		antiRollbackStore:      config.AntiRollbackStore,
		now:                    now,
	}, nil
}

// Verify validates fresh canonical artifacts and atomically persists them only
// after every authorization check succeeds.
func (v *LocalVerifier) Verify(ctx context.Context, entitlementEnvelope, revocationEnvelope []byte, request LocalVerifyRequest) (LocalVerificationResult, error) {
	return v.verify(ctx, entitlementEnvelope, revocationEnvelope, request, true)
}

// VerifyStored validates the last securely persisted entitlement and signed
// revocation snapshot. Missing or unreadable state is denied.
func (v *LocalVerifier) VerifyStored(ctx context.Context, request LocalVerifyRequest) (LocalVerificationResult, error) {
	if ctx == nil {
		return LocalVerificationResult{}, deny("invalid_context")
	}
	entitlementEnvelope, err := v.secureStore.LoadSecureValue(ctx, v.secureKey(secureEntitlementSuffix))
	if err != nil {
		return LocalVerificationResult{}, deny("secure_entitlement_unavailable")
	}
	revocationEnvelope, err := v.secureStore.LoadSecureValue(ctx, v.secureKey(secureRevocationSuffix))
	if err != nil {
		return LocalVerificationResult{}, deny("secure_revocations_unavailable")
	}
	return v.verify(ctx, entitlementEnvelope, revocationEnvelope, request, false)
}

func (v *LocalVerifier) verify(ctx context.Context, entitlementBytes, revocationBytes []byte, request LocalVerifyRequest, persist bool) (LocalVerificationResult, error) {
	if ctx == nil {
		return LocalVerificationResult{}, deny("invalid_context")
	}
	if err := ctx.Err(); err != nil {
		return LocalVerificationResult{}, deny("context_unavailable")
	}
	if err := validateDeviceBinding(request.DeviceBinding); err != nil {
		return LocalVerificationResult{}, deny("invalid_device_binding")
	}

	entitlementEnvelope, entitlementSigningBytes, entitlementSignature, err := parseSignedEntitlementEnvelope(entitlementBytes)
	if err != nil {
		return LocalVerificationResult{}, deny("invalid_entitlement_envelope")
	}
	revocationEnvelope, revocationSigningBytes, revocationSignature, err := parseSignedRevocationEnvelope(revocationBytes)
	if err != nil {
		return LocalVerificationResult{}, deny("invalid_revocation_envelope")
	}

	entitlementKey, ok := v.publicKeys[entitlementEnvelope.KeyID]
	if !ok || !ed25519.Verify(entitlementKey, entitlementSigningBytes, entitlementSignature) {
		return LocalVerificationResult{}, deny("invalid_entitlement_signature")
	}
	revocationKey, ok := v.publicKeys[revocationEnvelope.KeyID]
	if !ok || !ed25519.Verify(revocationKey, revocationSigningBytes, revocationSignature) {
		return LocalVerificationResult{}, deny("invalid_revocation_signature")
	}

	entitlement := entitlementEnvelope.Entitlement
	revocations := revocationEnvelope.Revocations
	if entitlement.Product != v.product || revocations.Product != v.product {
		return LocalVerificationResult{}, deny("product_mismatch")
	}
	if entitlement.Version != v.version {
		return LocalVerificationResult{}, deny("version_mismatch")
	}
	if entitlement.OfflineGraceSeconds > v.maxOfflineGraceSeconds {
		return LocalVerificationResult{}, deny("offline_grace_exceeds_policy")
	}
	if entitlement.RevocationEpoch > revocations.Epoch {
		return LocalVerificationResult{}, deny("revocation_snapshot_too_old")
	}
	if containsSorted(revocations.RevokedKeyIDs, revocationEnvelope.KeyID) {
		return LocalVerificationResult{}, deny("revocation_signing_key_revoked")
	}
	if containsSorted(revocations.RevokedKeyIDs, entitlementEnvelope.KeyID) {
		return LocalVerificationResult{}, deny("entitlement_signing_key_revoked")
	}
	if containsSorted(revocations.RevokedActivationIDs, entitlement.ActivationID) {
		return LocalVerificationResult{}, deny("activation_revoked")
	}
	if !containsAllSorted(entitlement.Capabilities, v.requiredCapabilities) {
		return LocalVerificationResult{}, deny("required_capability_missing")
	}
	if entitlement.DeviceBinding.Algorithm != request.DeviceBinding.Algorithm || subtle.ConstantTimeCompare([]byte(entitlement.DeviceBinding.Digest), []byte(request.DeviceBinding.Digest)) != 1 {
		return LocalVerificationResult{}, deny("device_mismatch")
	}

	previous, previousExists, err := v.loadAntiRollback(ctx)
	if err != nil {
		return LocalVerificationResult{}, deny("anti_rollback_unavailable")
	}
	nowUnix := v.now().UTC().Unix()
	effectiveNow := nowUnix
	if previousExists {
		if nowUnix < previous.ObservedUnix-v.rollbackTolerance {
			return LocalVerificationResult{}, deny("clock_rollback_detected")
		}
		if previous.ObservedUnix > effectiveNow {
			effectiveNow = previous.ObservedUnix
		}
		if revocations.Epoch < previous.RevocationEpoch {
			return LocalVerificationResult{}, deny("revocation_epoch_rollback")
		}
	}
	if entitlement.IssuedAt > nowUnix+v.futureClockSkewSeconds || revocations.IssuedAt > nowUnix+v.futureClockSkewSeconds {
		return LocalVerificationResult{}, deny("future_issue_time")
	}
	if entitlement.NotBefore > effectiveNow+v.futureClockSkewSeconds {
		return LocalVerificationResult{}, deny("entitlement_not_yet_valid")
	}
	if effectiveNow > revocations.ExpiresAt {
		return LocalVerificationResult{}, deny("revocation_snapshot_expired")
	}
	effectiveExpiresAt := entitlement.ExpiresAt + int64(entitlement.OfflineGraceSeconds)
	if effectiveNow > effectiveExpiresAt {
		return LocalVerificationResult{}, deny("entitlement_expired")
	}

	next := AntiRollbackState{ObservedUnix: effectiveNow, RevocationEpoch: revocations.Epoch}
	if previousExists {
		if previous.ObservedUnix > next.ObservedUnix {
			next.ObservedUnix = previous.ObservedUnix
		}
		if previous.RevocationEpoch > next.RevocationEpoch {
			next.RevocationEpoch = previous.RevocationEpoch
		}
	}
	var expected *AntiRollbackState
	if previousExists {
		copyOfPrevious := previous
		expected = &copyOfPrevious
	}
	if err := v.antiRollbackStore.CompareAndSwapAntiRollback(ctx, v.product, expected, next); err != nil {
		return LocalVerificationResult{}, deny("anti_rollback_update_failed")
	}
	if persist {
		values := map[string][]byte{
			v.secureKey(secureEntitlementSuffix): append([]byte(nil), entitlementBytes...),
			v.secureKey(secureRevocationSuffix):  append([]byte(nil), revocationBytes...),
		}
		if err := v.secureStore.StoreSecureValues(ctx, values); err != nil {
			return LocalVerificationResult{}, deny("secure_store_update_failed")
		}
	}

	return LocalVerificationResult{
		Entitlement:        entitlement,
		RevocationEpoch:    revocations.Epoch,
		EffectiveExpiresAt: time.Unix(effectiveExpiresAt, 0).UTC(),
		InOfflineGrace:     effectiveNow > entitlement.ExpiresAt,
	}, nil
}

func (v *LocalVerifier) loadAntiRollback(ctx context.Context) (AntiRollbackState, bool, error) {
	state, err := v.antiRollbackStore.LoadAntiRollback(ctx, v.product)
	if err == nil {
		if state.ObservedUnix <= 0 || state.RevocationEpoch == 0 {
			return AntiRollbackState{}, false, errors.New("invalid anti-rollback state")
		}
		return state, true, nil
	}
	if errors.Is(err, ErrAntiRollbackStateNotFound) {
		return AntiRollbackState{}, false, nil
	}
	return AntiRollbackState{}, false, err
}

func (v *LocalVerifier) secureKey(suffix string) string {
	return "meadow.licensing/" + v.product + "/" + suffix
}

func deny(reason string) error {
	return &LocalVerificationError{Reason: reason}
}

func containsSorted(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func containsAllSorted(available, required []string) bool {
	for _, capability := range required {
		if !containsSorted(available, capability) {
			return false
		}
	}
	return true
}

func durationSeconds(duration time.Duration) (uint64, error) {
	if duration < 0 || duration%time.Second != 0 {
		return 0, errors.New("duration must be a non-negative whole number of seconds")
	}
	return uint64(duration / time.Second), nil
}

func signedDurationSeconds(duration time.Duration) (int64, error) {
	if duration < 0 || duration%time.Second != 0 {
		return 0, errors.New("duration must be a non-negative whole number of seconds")
	}
	return int64(duration / time.Second), nil
}
