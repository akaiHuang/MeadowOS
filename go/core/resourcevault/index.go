package resourcevault

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

func readAndVerifyIndex(
	ctx context.Context,
	indexReader io.Reader,
	signatureReader io.Reader,
	keys VerificationKeyProvider,
	config Config,
	limits Limits,
) (Index, error) {
	signaturePayload, err := readBounded(signatureReader, limits.SignatureBytes)
	if err != nil {
		return Index{}, fmt.Errorf("read resource vault signature: %w", err)
	}
	defer wipe(signaturePayload)

	var signature Signature
	if err := decodeStrictJSON(signaturePayload, &signature); err != nil {
		return Index{}, fmt.Errorf("decode resource vault signature: %w", err)
	}
	if signature.SchemaVersion != SchemaVersion || signature.Algorithm != SignatureAlgorithmEd25519 {
		return Index{}, fmt.Errorf("unsupported resource vault signature contract")
	}
	if signature.KeyID != config.ExpectedSigningKeyID {
		return Index{}, fmt.Errorf("resource vault signing key id mismatch")
	}
	if err := validateOpaqueID(signature.KeyID, 128, false); err != nil {
		return Index{}, fmt.Errorf("invalid resource vault signing key id: %w", err)
	}
	signatureBytes, err := decodeCanonicalBase64(signature.SignatureBase64)
	if err != nil || len(signatureBytes) != ed25519.SignatureSize {
		wipe(signatureBytes)
		return Index{}, fmt.Errorf("resource vault signature is malformed")
	}
	defer wipe(signatureBytes)

	indexPayload, err := readBounded(indexReader, limits.IndexBytes)
	if err != nil {
		return Index{}, fmt.Errorf("read resource vault index: %w", err)
	}
	defer wipe(indexPayload)

	publicKey, err := keys.PublicKey(ctx, signature.KeyID)
	if err != nil {
		return Index{}, fmt.Errorf("resolve resource vault verification key: %w", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return Index{}, fmt.Errorf("resource vault verification key is malformed")
	}
	if !ed25519.Verify(publicKey, indexPayload, signatureBytes) {
		return Index{}, fmt.Errorf("resource vault index signature verification failed")
	}
	if err := ctx.Err(); err != nil {
		return Index{}, err
	}

	var index Index
	if err := decodeStrictJSON(indexPayload, &index); err != nil {
		return Index{}, fmt.Errorf("decode signed resource vault index: %w", err)
	}
	if err := validateIndex(index, config.ExpectedBundleID, config.ExpectedBundleVersion, limits); err != nil {
		return Index{}, err
	}
	return index, nil
}

// MarshalAndSignIndex is intended for an offline release packager. signer may
// be HSM-backed; no private key is stored by the package or emitted in either
// returned document.
func MarshalAndSignIndex(index Index, keyID string, signer crypto.Signer) ([]byte, []byte, error) {
	if signer == nil {
		return nil, nil, fmt.Errorf("resource vault signer is required")
	}
	if err := validateOpaqueID(keyID, 128, false); err != nil {
		return nil, nil, fmt.Errorf("invalid resource vault signing key id: %w", err)
	}
	publicKey, ok := signer.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, nil, fmt.Errorf("resource vault signer must use Ed25519")
	}
	limits := DefaultLimits()
	if err := validateIndex(index, index.BundleID, index.BundleVersion, limits); err != nil {
		return nil, nil, err
	}
	indexPayload, err := json.Marshal(index)
	if err != nil {
		return nil, nil, fmt.Errorf("encode resource vault index: %w", err)
	}
	if int64(len(indexPayload)) > limits.IndexBytes {
		wipe(indexPayload)
		return nil, nil, fmt.Errorf("resource vault index exceeds hard size limit")
	}
	signatureBytes, err := signer.Sign(rand.Reader, indexPayload, crypto.Hash(0))
	if err != nil {
		wipe(indexPayload)
		return nil, nil, fmt.Errorf("sign resource vault index: %w", err)
	}
	defer wipe(signatureBytes)
	if len(signatureBytes) != ed25519.SignatureSize || !ed25519.Verify(publicKey, indexPayload, signatureBytes) {
		wipe(indexPayload)
		return nil, nil, fmt.Errorf("resource vault signer returned an invalid signature")
	}
	document := Signature{
		SchemaVersion:   SchemaVersion,
		Algorithm:       SignatureAlgorithmEd25519,
		KeyID:           keyID,
		SignatureBase64: base64.StdEncoding.EncodeToString(signatureBytes),
	}
	signaturePayload, err := json.Marshal(document)
	if err != nil {
		wipe(indexPayload)
		return nil, nil, fmt.Errorf("encode resource vault signature: %w", err)
	}
	return indexPayload, signaturePayload, nil
}

func validateIndex(index Index, expectedBundleID string, expectedBundleVersion string, limits Limits) error {
	if index.SchemaVersion != SchemaVersion {
		return fmt.Errorf("resource vault index schema_version must be %d", SchemaVersion)
	}
	if err := validateBundleID(index.BundleID); err != nil {
		return fmt.Errorf("resource vault bundle_id is invalid: %w", err)
	}
	if err := validateOpaqueID(index.BundleVersion, 128, false); err != nil {
		return fmt.Errorf("resource vault bundle_version is invalid: %w", err)
	}
	if index.BundleID != expectedBundleID || index.BundleVersion != expectedBundleVersion {
		return fmt.Errorf("resource vault index is bound to a different bundle or version")
	}
	if len(index.Resources) == 0 || len(index.Resources) > limits.Resources {
		return fmt.Errorf("resource vault resource count is outside the permitted range")
	}
	previousID := ""
	for position, entry := range index.Resources {
		if err := validateEntry(entry, index.BundleID, index.BundleVersion, limits); err != nil {
			return fmt.Errorf("resource vault entry %d: %w", position, err)
		}
		if previousID != "" && entry.ResourceID <= previousID {
			return fmt.Errorf("resource vault entries must be strictly sorted by resource_id")
		}
		previousID = entry.ResourceID
	}
	return nil
}

func validateEntry(entry ResourceEntry, bundleID string, bundleVersion string, limits Limits) error {
	binding := ResourceBinding{
		SchemaVersion:    SchemaVersion,
		BundleID:         bundleID,
		BundleVersion:    bundleVersion,
		ResourceID:       entry.ResourceID,
		ResourceType:     entry.ResourceType,
		ResourceVersion:  entry.ResourceVersion,
		Capabilities:     entry.Capabilities,
		PlaintextBytes:   entry.PlaintextBytes,
		CipherAlgorithm:  entry.CipherAlgorithm,
		KeyWrapAlgorithm: entry.KeyWrapAlgorithm,
		KeyID:            entry.KeyID,
	}
	if err := validateBinding(binding, limits); err != nil {
		return err
	}
	if entry.PayloadBytes <= 0 || entry.PayloadBytes > limits.PayloadBytes {
		return fmt.Errorf("payload_bytes is outside the permitted range")
	}
	if _, err := decodeSHA256(entry.PayloadSHA256); err != nil {
		return fmt.Errorf("payload_sha256 is malformed")
	}
	return nil
}

func validateBinding(binding ResourceBinding, limits Limits) error {
	if binding.SchemaVersion != SchemaVersion {
		return fmt.Errorf("resource binding schema_version must be %d", SchemaVersion)
	}
	if err := validateBundleID(binding.BundleID); err != nil {
		return fmt.Errorf("bundle_id is invalid: %w", err)
	}
	if err := validateOpaqueID(binding.BundleVersion, 128, false); err != nil {
		return fmt.Errorf("bundle_version is invalid: %w", err)
	}
	if err := validateOpaqueID(binding.ResourceID, 192, false); err != nil {
		return fmt.Errorf("resource_id is invalid: %w", err)
	}
	switch binding.ResourceType {
	case ResourceTypePrompt, ResourceTypePolicy, ResourceTypeCapability:
	default:
		return fmt.Errorf("resource_type is unsupported")
	}
	if err := validateOpaqueID(binding.ResourceVersion, 128, false); err != nil {
		return fmt.Errorf("resource_version is invalid: %w", err)
	}
	if len(binding.Capabilities) == 0 || len(binding.Capabilities) > maxCapabilitiesPerResource {
		return fmt.Errorf("capability count is outside the permitted range")
	}
	if !sort.StringsAreSorted(binding.Capabilities) {
		return fmt.Errorf("capabilities must be sorted")
	}
	previous := ""
	for _, capability := range binding.Capabilities {
		if err := validateOpaqueID(capability, 128, true); err != nil {
			return fmt.Errorf("capability is invalid: %w", err)
		}
		if capability == previous {
			return fmt.Errorf("capabilities must be unique")
		}
		previous = capability
	}
	if binding.PlaintextBytes <= 0 || binding.PlaintextBytes > limits.PlaintextBytes {
		return fmt.Errorf("plaintext_bytes is outside the permitted range")
	}
	if binding.CipherAlgorithm != CipherAlgorithm(CipherAlgorithmAES256GCM) {
		return fmt.Errorf("cipher_algorithm is unsupported")
	}
	if binding.KeyWrapAlgorithm != KeyWrapAlgorithm(KeyWrapAlgorithmRSAOAEP256) {
		return fmt.Errorf("key_wrap_algorithm is unsupported")
	}
	if err := validateOpaqueID(binding.KeyID, 128, false); err != nil {
		return fmt.Errorf("key_id is invalid: %w", err)
	}
	return nil
}

func bindingFor(index Index, entry ResourceEntry) ResourceBinding {
	capabilities := append([]string(nil), entry.Capabilities...)
	return ResourceBinding{
		SchemaVersion:    SchemaVersion,
		BundleID:         index.BundleID,
		BundleVersion:    index.BundleVersion,
		ResourceID:       entry.ResourceID,
		ResourceType:     entry.ResourceType,
		ResourceVersion:  entry.ResourceVersion,
		Capabilities:     capabilities,
		PlaintextBytes:   entry.PlaintextBytes,
		CipherAlgorithm:  entry.CipherAlgorithm,
		KeyWrapAlgorithm: entry.KeyWrapAlgorithm,
		KeyID:            entry.KeyID,
	}
}

func bindingBytes(binding ResourceBinding) ([]byte, error) {
	if err := validateBinding(binding, DefaultLimits()); err != nil {
		return nil, err
	}
	return json.Marshal(binding)
}

// BindingSHA256 returns the digest used as the RSA-OAEP label and identifies
// the exact AES-GCM authenticated-data contract.
func BindingSHA256(binding ResourceBinding) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	payload, err := bindingBytes(binding)
	if err != nil {
		return zero, err
	}
	defer wipe(payload)
	return sha256.Sum256(payload), nil
}

func validateBundleID(value string) error {
	if value == "" || len(value) > 255 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return fmt.Errorf("value is empty, oversized, or non-canonical")
	}
	if strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") {
		return fmt.Errorf("value has invalid structure")
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '-' {
			continue
		}
		return fmt.Errorf("value contains an unsupported character")
	}
	return nil
}

func validateOpaqueID(value string, maxBytes int, lowercase bool) error {
	if value == "" || len(value) > maxBytes || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return fmt.Errorf("value is empty, oversized, or non-canonical")
	}
	if lowercase && value != strings.ToLower(value) {
		return fmt.Errorf("value must be lowercase")
	}
	if strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") {
		return fmt.Errorf("value has invalid structure")
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '-' || character == '_' || character == ':' || character == '+' || character == '@' {
			continue
		}
		return fmt.Errorf("value contains a path, control, or unsupported character")
	}
	return nil
}

func decodeSHA256(value string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return digest, fmt.Errorf("invalid SHA-256 encoding")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return digest, fmt.Errorf("invalid SHA-256 encoding")
	}
	copy(digest[:], decoded)
	wipe(decoded)
	return digest, nil
}

func decodeCanonicalBase64(value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != value {
		wipe(decoded)
		return nil, fmt.Errorf("non-canonical base64")
	}
	return decoded, nil
}

func decodeStrictJSON(payload []byte, destination any) error {
	if err := rejectDuplicateJSONFields(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not permitted")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONFields(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not permitted")
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}
