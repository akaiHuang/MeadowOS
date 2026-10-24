package licensing

import (
	"context"
	"errors"
)

// StoredVerifier is the read-only local authorization surface safe to inject
// into consumers. It cannot refresh envelopes, access secure-store values, or
// expose verification keys and configuration.
type StoredVerifier interface {
	VerifyStored(context.Context, LocalVerifyRequest) (LocalVerificationResult, error)
}

// StoredVerifierProvider owns an already configured LocalVerifier while
// exposing only its read-only stored-verification capability.
type StoredVerifierProvider interface {
	Verifier() StoredVerifier
}

type storedVerifierProvider struct {
	verifier *LocalVerifier
}

// NewStoredVerifierProvider safely constructs and contains a LocalVerifier.
// LocalVerifierConfig contains public verification material and protected
// store interfaces only; issuer private keys are unsupported.
func NewStoredVerifierProvider(config LocalVerifierConfig) (StoredVerifierProvider, error) {
	verifier, err := NewLocalVerifier(config)
	if err != nil {
		return nil, err
	}
	return &storedVerifierProvider{verifier: verifier}, nil
}

// ProvideStoredVerifier contains an existing production LocalVerifier without
// exposing its stores, envelopes, keys, or mutable verification method.
func ProvideStoredVerifier(verifier *LocalVerifier) (StoredVerifierProvider, error) {
	if verifier == nil {
		return nil, errors.New("licensing: local verifier is required")
	}
	return &storedVerifierProvider{verifier: verifier}, nil
}

func (provider *storedVerifierProvider) Verifier() StoredVerifier {
	if provider == nil || provider.verifier == nil {
		return nil
	}
	return provider.verifier
}

var _ StoredVerifier = (*LocalVerifier)(nil)
var _ StoredVerifierProvider = (*storedVerifierProvider)(nil)
