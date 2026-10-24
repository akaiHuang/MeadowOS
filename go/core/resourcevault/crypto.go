package resourcevault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
)

// SealPayload is intended for an offline packager. dataKey must be a fresh
// random 32-byte AES key, and wrappedKey must be that key wrapped externally
// with RSA-OAEP-SHA256 using BindingSHA256(binding) as the OAEP label. The
// caller remains responsible for wiping plaintext, dataKey, and wrappedKey.
func SealPayload(binding ResourceBinding, plaintext []byte, dataKey []byte, wrappedKey []byte) (ResourceEntry, []byte, error) {
	limits := DefaultLimits()
	if err := validateBinding(binding, limits); err != nil {
		return ResourceEntry{}, nil, err
	}
	if int64(len(plaintext)) != binding.PlaintextBytes {
		return ResourceEntry{}, nil, fmt.Errorf("plaintext length does not match resource binding")
	}
	if len(dataKey) != 32 {
		return ResourceEntry{}, nil, fmt.Errorf("AES-256-GCM data key must be 32 bytes")
	}
	if len(wrappedKey) == 0 || len(wrappedKey) > limits.WrappedKeyBytes {
		return ResourceEntry{}, nil, fmt.Errorf("wrapped key size is outside the permitted range")
	}
	aad, err := bindingBytes(binding)
	if err != nil {
		return ResourceEntry{}, nil, err
	}
	defer wipe(aad)
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return ResourceEntry{}, nil, fmt.Errorf("construct AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return ResourceEntry{}, nil, fmt.Errorf("construct AES-GCM: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	defer wipe(nonce)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return ResourceEntry{}, nil, fmt.Errorf("generate AES-GCM nonce: %w", err)
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	defer wipe(ciphertext)
	document := encryptedPayload{
		SchemaVersion:    SchemaVersion,
		NonceBase64:      base64.StdEncoding.EncodeToString(nonce),
		WrappedKeyBase64: base64.StdEncoding.EncodeToString(wrappedKey),
		CiphertextBase64: base64.StdEncoding.EncodeToString(ciphertext),
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return ResourceEntry{}, nil, fmt.Errorf("encode encrypted resource payload: %w", err)
	}
	if int64(len(payload)) > limits.PayloadBytes {
		wipe(payload)
		return ResourceEntry{}, nil, fmt.Errorf("encrypted resource payload exceeds hard size limit")
	}
	digest := sha256.Sum256(payload)
	entry := ResourceEntry{
		ResourceID:       binding.ResourceID,
		ResourceType:     binding.ResourceType,
		ResourceVersion:  binding.ResourceVersion,
		Capabilities:     append([]string(nil), binding.Capabilities...),
		PlaintextBytes:   binding.PlaintextBytes,
		PayloadBytes:     int64(len(payload)),
		PayloadSHA256:    hex.EncodeToString(digest[:]),
		CipherAlgorithm:  binding.CipherAlgorithm,
		KeyWrapAlgorithm: binding.KeyWrapAlgorithm,
		KeyID:            binding.KeyID,
	}
	return entry, payload, nil
}

func decryptPayload(raw []byte, verified verifiedResource, dataKey []byte, limits Limits) ([]byte, error) {
	var document encryptedPayload
	if err := decodeStrictJSON(raw, &document); err != nil {
		return nil, fmt.Errorf("decode encrypted resource payload: %w", err)
	}
	if document.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("encrypted resource payload schema_version must be %d", SchemaVersion)
	}
	nonce, err := decodeCanonicalBase64(document.NonceBase64)
	if err != nil {
		return nil, fmt.Errorf("encrypted resource nonce is malformed")
	}
	defer wipe(nonce)
	ciphertext, err := decodeCanonicalBase64(document.CiphertextBase64)
	if err != nil {
		return nil, fmt.Errorf("encrypted resource ciphertext is malformed")
	}
	defer wipe(ciphertext)
	if len(dataKey) != 32 {
		return nil, fmt.Errorf("key unwrapper returned an invalid AES-256 data key")
	}
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, fmt.Errorf("construct AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("construct AES-GCM: %w", err)
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("encrypted resource nonce has invalid size")
	}
	if verified.entry.PlaintextBytes > limits.PlaintextBytes || verified.entry.PlaintextBytes > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("resource plaintext size cannot be represented safely")
	}
	expectedCiphertextBytes := verified.entry.PlaintextBytes + int64(aead.Overhead())
	if int64(len(ciphertext)) != expectedCiphertextBytes {
		return nil, fmt.Errorf("encrypted resource ciphertext size mismatch")
	}
	backing := make([]byte, 0, int(verified.entry.PlaintextBytes))
	plaintext, err := aead.Open(backing, nonce, ciphertext, verified.aad)
	if err != nil {
		wipe(backing[:cap(backing)])
		return nil, fmt.Errorf("authenticate encrypted resource payload: %w", err)
	}
	if int64(len(plaintext)) != verified.entry.PlaintextBytes {
		wipe(plaintext)
		return nil, fmt.Errorf("decrypted resource size mismatch")
	}
	return plaintext, nil
}

func extractWrappedKey(raw []byte, maxBytes int) ([]byte, error) {
	var document encryptedPayload
	if err := decodeStrictJSON(raw, &document); err != nil {
		return nil, fmt.Errorf("decode encrypted resource payload: %w", err)
	}
	if document.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("encrypted resource payload schema_version must be %d", SchemaVersion)
	}
	wrappedKey, err := decodeCanonicalBase64(document.WrappedKeyBase64)
	if err != nil || len(wrappedKey) == 0 || len(wrappedKey) > maxBytes {
		wipe(wrappedKey)
		return nil, fmt.Errorf("encrypted resource wrapped key is malformed")
	}
	return wrappedKey, nil
}

func digestMatches(payload []byte, expected [sha256.Size]byte) bool {
	actual := sha256.Sum256(payload)
	return subtle.ConstantTimeCompare(actual[:], expected[:]) == 1
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil || limit <= 0 || limit >= int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("invalid bounded reader")
	}
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		wipe(payload)
		return nil, err
	}
	if int64(len(payload)) > limit {
		wipe(payload)
		return nil, fmt.Errorf("document exceeds permitted size")
	}
	return payload, nil
}

func wipe(buffer []byte) {
	clear(buffer)
	runtime.KeepAlive(buffer)
}
