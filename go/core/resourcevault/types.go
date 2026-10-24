package resourcevault

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
)

const (
	SchemaVersion = 1

	SignatureAlgorithmEd25519 = "Ed25519"
	CipherAlgorithmAES256GCM   = "AES-256-GCM"
	KeyWrapAlgorithmRSAOAEP256 = "RSA-OAEP-SHA256"
)

const (
	hardMaxIndexBytes      int64 = 4 << 20
	hardMaxSignatureBytes  int64 = 16 << 10
	hardMaxPayloadBytes    int64 = 64 << 20
	hardMaxPlaintextBytes  int64 = 32 << 20
	hardMaxWrappedKeyBytes       = 16 << 10
	hardMaxResources              = 4096
	maxCapabilitiesPerResource   = 64
)

var (
	ErrClosed       = errors.New("resource vault plaintext is closed")
	ErrNotFound     = errors.New("resource vault resource not found")
	ErrUnauthorized = errors.New("resource vault capability is not authorized")
)

type ResourceType string

const (
	ResourceTypePrompt     ResourceType = "prompt"
	ResourceTypePolicy     ResourceType = "policy"
	ResourceTypeCapability ResourceType = "capability"
)

type CipherAlgorithm string
type KeyWrapAlgorithm string

// Index is the exact document covered by the detached Signature document.
// Resources must be sorted by resource_id and capabilities must be sorted.
type Index struct {
	SchemaVersion int             `json:"schema_version"`
	BundleID      string          `json:"bundle_id"`
	BundleVersion string          `json:"bundle_version"`
	Resources     []ResourceEntry `json:"resources"`
}

// ResourceEntry binds an encrypted payload to one bundle and one capability
// set. It intentionally contains no filesystem path.
type ResourceEntry struct {
	ResourceID       string           `json:"resource_id"`
	ResourceType     ResourceType     `json:"resource_type"`
	ResourceVersion  string           `json:"resource_version"`
	Capabilities     []string         `json:"capabilities"`
	PlaintextBytes   int64            `json:"plaintext_bytes"`
	PayloadBytes     int64            `json:"payload_bytes"`
	PayloadSHA256    string           `json:"payload_sha256"`
	CipherAlgorithm  CipherAlgorithm  `json:"cipher_algorithm"`
	KeyWrapAlgorithm KeyWrapAlgorithm `json:"key_wrap_algorithm"`
	KeyID            string           `json:"key_id"`
}

// Signature is a detached signature over the exact bytes of an Index JSON
// document. SignatureBase64 uses canonical padded standard base64.
type Signature struct {
	SchemaVersion   int    `json:"schema_version"`
	Algorithm       string `json:"algorithm"`
	KeyID           string `json:"key_id"`
	SignatureBase64 string `json:"signature_base64"`
}

type encryptedPayload struct {
	SchemaVersion    int    `json:"schema_version"`
	NonceBase64      string `json:"nonce_base64"`
	WrappedKeyBase64 string `json:"wrapped_key_base64"`
	CiphertextBase64 string `json:"ciphertext_base64"`
}

// ResourceBinding is the canonical authenticated-data contract used by both
// the offline packager and runtime vault.
type ResourceBinding struct {
	SchemaVersion    int              `json:"schema_version"`
	BundleID         string           `json:"bundle_id"`
	BundleVersion    string           `json:"bundle_version"`
	ResourceID       string           `json:"resource_id"`
	ResourceType     ResourceType     `json:"resource_type"`
	ResourceVersion  string           `json:"resource_version"`
	Capabilities     []string         `json:"capabilities"`
	PlaintextBytes   int64            `json:"plaintext_bytes"`
	CipherAlgorithm  CipherAlgorithm  `json:"cipher_algorithm"`
	KeyWrapAlgorithm KeyWrapAlgorithm `json:"key_wrap_algorithm"`
	KeyID            string           `json:"key_id"`
}

// VerificationKeyProvider returns trusted Ed25519 public keys. Public keys may
// be shipped with a product; signing private keys must remain in offline or HSM
// infrastructure.
type VerificationKeyProvider interface {
	PublicKey(context.Context, string) (ed25519.PublicKey, error)
}

// KeyUnwrapRequest carries an externally wrapped data key. For
// RSA-OAEP-SHA256, BindingSHA256 is the OAEP label used when wrapping and
// unwrapping the key.
type KeyUnwrapRequest struct {
	KeyID           string
	Algorithm       KeyWrapAlgorithm
	BindingSHA256   [sha256.Size]byte
	WrappedKey      []byte
}

// KeyUnwrapper is the runtime trust boundary. Implementations must return a
// fresh mutable byte slice, must not retain request buffers, and must not expose
// a process-wide fixed key. The vault wipes the returned slice after use.
type KeyUnwrapper interface {
	UnwrapKey(context.Context, KeyUnwrapRequest) ([]byte, error)
}

type Limits struct {
	IndexBytes      int64
	SignatureBytes  int64
	PayloadBytes    int64
	PlaintextBytes  int64
	WrappedKeyBytes int
	Resources       int
}

func DefaultLimits() Limits {
	return Limits{
		IndexBytes:      hardMaxIndexBytes,
		SignatureBytes:  hardMaxSignatureBytes,
		PayloadBytes:    hardMaxPayloadBytes,
		PlaintextBytes:  hardMaxPlaintextBytes,
		WrappedKeyBytes: hardMaxWrappedKeyBytes,
		Resources:       hardMaxResources,
	}
}

// Config pins a vault to the current product bundle and expected release
// signing key. Limits may only lower the package hard limits; zero selects the
// default for that field.
type Config struct {
	ExpectedBundleID      string
	ExpectedBundleVersion string
	ExpectedSigningKeyID  string
	Limits                Limits
}

// ResourceRequest prevents silent resource-version downgrade and requires a
// capability explicitly authorized by the signed index.
type ResourceRequest struct {
	ResourceID      string
	ResourceVersion string
	Capability      string
}

type ResourceMetadata struct {
	ResourceID      string
	ResourceType    ResourceType
	ResourceVersion string
	Capabilities    []string
	PlaintextBytes  int64
}
