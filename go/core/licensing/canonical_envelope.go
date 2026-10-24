package licensing

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

type entitlementSigningDocument struct {
	Schema      string               `json:"schema"`
	KeyID       string               `json:"key_id"`
	Entitlement CanonicalEntitlement `json:"entitlement"`
}

type revocationSigningDocument struct {
	Schema      string                  `json:"schema"`
	KeyID       string                  `json:"key_id"`
	Revocations CanonicalRevocationList `json:"revocations"`
}

// EntitlementEnvelopeSigningBytes returns the only byte representation an
// issuer may sign for an entitlement envelope.
func EntitlementEnvelopeSigningBytes(keyID string, entitlement CanonicalEntitlement) ([]byte, error) {
	doc := entitlementSigningDocument{
		Schema:      EntitlementEnvelopeSchema,
		KeyID:       keyID,
		Entitlement: entitlement,
	}
	if err := validateKeyID(keyID); err != nil {
		return nil, err
	}
	if err := validateCanonicalEntitlement(entitlement); err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}

// MarshalSignedEntitlementEnvelope combines issuer-produced signature bytes
// with their canonical signing document. It does not accept private keys.
func MarshalSignedEntitlementEnvelope(keyID string, entitlement CanonicalEntitlement, signature []byte) ([]byte, error) {
	if _, err := EntitlementEnvelopeSigningBytes(keyID, entitlement); err != nil {
		return nil, err
	}
	if len(signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("ed25519 signature must be %d bytes", ed25519.SignatureSize)
	}
	return json.Marshal(SignedEntitlementEnvelope{
		Schema:      EntitlementEnvelopeSchema,
		KeyID:       keyID,
		Entitlement: entitlement,
		Signature:   base64.RawURLEncoding.EncodeToString(signature),
	})
}

// RevocationEnvelopeSigningBytes returns the only byte representation an
// issuer may sign for a revocation-list envelope.
func RevocationEnvelopeSigningBytes(keyID string, revocations CanonicalRevocationList) ([]byte, error) {
	doc := revocationSigningDocument{
		Schema:      RevocationEnvelopeSchema,
		KeyID:       keyID,
		Revocations: revocations,
	}
	if err := validateKeyID(keyID); err != nil {
		return nil, err
	}
	if err := validateCanonicalRevocationList(revocations); err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}

// MarshalSignedRevocationListEnvelope combines issuer-produced signature
// bytes with their canonical signing document. It does not accept private keys.
func MarshalSignedRevocationListEnvelope(keyID string, revocations CanonicalRevocationList, signature []byte) ([]byte, error) {
	if _, err := RevocationEnvelopeSigningBytes(keyID, revocations); err != nil {
		return nil, err
	}
	if len(signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("ed25519 signature must be %d bytes", ed25519.SignatureSize)
	}
	return json.Marshal(SignedRevocationListEnvelope{
		Schema:      RevocationEnvelopeSchema,
		KeyID:       keyID,
		Revocations: revocations,
		Signature:   base64.RawURLEncoding.EncodeToString(signature),
	})
}

func parseSignedEntitlementEnvelope(raw []byte) (SignedEntitlementEnvelope, []byte, []byte, error) {
	var envelope SignedEntitlementEnvelope
	if err := strictCanonicalJSON(raw, &envelope); err != nil {
		return SignedEntitlementEnvelope{}, nil, nil, err
	}
	if envelope.Schema != EntitlementEnvelopeSchema {
		return SignedEntitlementEnvelope{}, nil, nil, errors.New("unsupported entitlement schema")
	}
	signingBytes, err := EntitlementEnvelopeSigningBytes(envelope.KeyID, envelope.Entitlement)
	if err != nil {
		return SignedEntitlementEnvelope{}, nil, nil, err
	}
	signature, err := decodeCanonicalSignature(envelope.Signature)
	if err != nil {
		return SignedEntitlementEnvelope{}, nil, nil, err
	}
	return envelope, signingBytes, signature, nil
}

func parseSignedRevocationEnvelope(raw []byte) (SignedRevocationListEnvelope, []byte, []byte, error) {
	var envelope SignedRevocationListEnvelope
	if err := strictCanonicalJSON(raw, &envelope); err != nil {
		return SignedRevocationListEnvelope{}, nil, nil, err
	}
	if envelope.Schema != RevocationEnvelopeSchema {
		return SignedRevocationListEnvelope{}, nil, nil, errors.New("unsupported revocation schema")
	}
	signingBytes, err := RevocationEnvelopeSigningBytes(envelope.KeyID, envelope.Revocations)
	if err != nil {
		return SignedRevocationListEnvelope{}, nil, nil, err
	}
	signature, err := decodeCanonicalSignature(envelope.Signature)
	if err != nil {
		return SignedRevocationListEnvelope{}, nil, nil, err
	}
	return envelope, signingBytes, signature, nil
}

func strictCanonicalJSON(raw []byte, out any) error {
	if len(raw) == 0 {
		return errors.New("empty JSON document")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	canonical, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, canonical) {
		return errors.New("JSON is not canonical")
	}
	return nil
}

func decodeCanonicalSignature(raw string) ([]byte, error) {
	signature, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	if len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != raw {
		return nil, errors.New("signature is not canonical Ed25519 base64url")
	}
	return signature, nil
}

func validateCanonicalEntitlement(entitlement CanonicalEntitlement) error {
	if err := validateName("product", entitlement.Product); err != nil {
		return err
	}
	if entitlement.Version == 0 {
		return errors.New("version must be non-zero")
	}
	if entitlement.Capabilities == nil {
		return errors.New("capabilities must be an array")
	}
	if err := validateSortedNames("capability", entitlement.Capabilities); err != nil {
		return err
	}
	if entitlement.IssuedAt <= 0 || entitlement.NotBefore <= 0 || entitlement.ExpiresAt <= 0 {
		return errors.New("entitlement timestamps must be positive")
	}
	if entitlement.NotBefore < entitlement.IssuedAt || entitlement.ExpiresAt < entitlement.NotBefore {
		return errors.New("entitlement timestamps are out of order")
	}
	if err := validateDeviceBinding(entitlement.DeviceBinding); err != nil {
		return err
	}
	if err := validateIdentifier("activation_id", entitlement.ActivationID); err != nil {
		return err
	}
	if entitlement.OfflineGraceSeconds > math.MaxInt64 || entitlement.ExpiresAt > math.MaxInt64-int64(entitlement.OfflineGraceSeconds) {
		return errors.New("offline grace overflows expiration")
	}
	if entitlement.RevocationEpoch == 0 {
		return errors.New("revocation_epoch must be non-zero")
	}
	return nil
}

func validateCanonicalRevocationList(revocations CanonicalRevocationList) error {
	if err := validateName("product", revocations.Product); err != nil {
		return err
	}
	if revocations.Epoch == 0 {
		return errors.New("revocation epoch must be non-zero")
	}
	if revocations.IssuedAt <= 0 || revocations.ExpiresAt <= revocations.IssuedAt {
		return errors.New("revocation timestamps are invalid")
	}
	if revocations.RevokedActivationIDs == nil || revocations.RevokedKeyIDs == nil {
		return errors.New("revoked lists must be arrays")
	}
	if err := validateSortedIdentifiers("revoked activation id", revocations.RevokedActivationIDs); err != nil {
		return err
	}
	if err := validateSortedIdentifiers("revoked key id", revocations.RevokedKeyIDs); err != nil {
		return err
	}
	return nil
}

func validateDeviceBinding(binding DeviceBinding) error {
	if binding.Algorithm != DeviceBindingSHA256 {
		return errors.New("unsupported device-binding algorithm")
	}
	digest, err := base64.RawURLEncoding.DecodeString(binding.Digest)
	if err != nil {
		return errors.New("invalid device-binding digest")
	}
	if len(digest) != 32 || base64.RawURLEncoding.EncodeToString(digest) != binding.Digest {
		return errors.New("device-binding digest is not canonical SHA-256 base64url")
	}
	return nil
}

func validateSortedNames(label string, values []string) error {
	for index, value := range values {
		if err := validateName(label, value); err != nil {
			return err
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("%ss must be strictly sorted and unique", label)
		}
	}
	return nil
}

func validateSortedIdentifiers(label string, values []string) error {
	for index, value := range values {
		if err := validateIdentifier(label, value); err != nil {
			return err
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("%ss must be strictly sorted and unique", label)
		}
	}
	return nil
}

func validateKeyID(value string) error {
	return validateIdentifier("key_id", value)
}

func validateName(label, value string) error {
	if len(value) == 0 || len(value) > 128 {
		return fmt.Errorf("%s length is invalid", label)
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' || character == ':' {
			continue
		}
		return fmt.Errorf("%s contains a non-canonical character", label)
	}
	return nil
}

func validateIdentifier(label, value string) error {
	if len(value) == 0 || len(value) > 128 {
		return fmt.Errorf("%s length is invalid", label)
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' || character == ':' {
			continue
		}
		return fmt.Errorf("%s contains an invalid character", label)
	}
	return nil
}

