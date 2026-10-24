package protectedbootstrap

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
	"github.com/hey-meadow/meadow-code/go/core/resourcevault"
)

var (
	ErrDenied      = errors.New("protected bootstrap denied")
	ErrClosed      = errors.New("protected bootstrap is closed")
	ErrCloseFailed = errors.New("protected bootstrap close failed")
)

type Stage string

const (
	StageConfiguration     Stage = "configuration"
	StagePublicBuild       Stage = "public_build"
	StageRuntimeIntegrity  Stage = "runtime_integrity"
	StageProductSecurity   Stage = "product_security"
	StageModelProfile      Stage = "model_profile"
	StageKeyBroker         Stage = "key_broker"
	StageLicensing         Stage = "licensing"
	StageModelKey          Stage = "model_key"
	StageModelContainer    Stage = "model_container"
	StageResourceKey       Stage = "resource_key"
	StageResourceUnwrapper Stage = "resource_unwrapper"
	StageResourceVault     Stage = "resource_vault"
	StageInputClose        Stage = "input_close"
)

// PrepareError intentionally withholds the underlying error because source
// contracts may include local paths or cryptographic details.
type PrepareError struct {
	Stage Stage
}

func (e *PrepareError) Error() string {
	if e == nil || e.Stage == "" {
		return ErrDenied.Error()
	}
	return ErrDenied.Error() + " at " + string(e.Stage)
}

func (e *PrepareError) Unwrap() error {
	return ErrDenied
}

type LicenseMode uint8

const (
	LicenseFresh LicenseMode = iota + 1
	LicenseStored
)

type LicensePolicy struct {
	Product                string
	Version                uint64
	PublicKeys             map[string]ed25519.PublicKey
	RequiredCapabilities   []string
	MaxOfflineGrace        time.Duration
	FutureClockSkew        time.Duration
	ClockRollbackTolerance time.Duration
	Mode                   LicenseMode
	EntitlementEnvelope    []byte
	RevocationEnvelope     []byte
}

type ModelConfig struct {
	LogicalModelID string
	AssetID        string
}

// ResourceConfig readers are consumed and closed by Prepare. Callers should
// use io.NopCloser for in-memory documents.
type ResourceConfig struct {
	Index               io.ReadCloser
	Signature           io.ReadCloser
	VerificationKeys    resourcevault.VerificationKeyProvider
	VaultConfig         resourcevault.Config
	PrivateKeyID        string
	PrivateKeyContainer io.ReadCloser
}

type Config struct {
	KeychainService     string
	DeviceBindingSHA256 [sha256.Size]byte
	License             LicensePolicy
	Model               ModelConfig
	Resources           ResourceConfig
}

type LicenseStatus struct {
	EffectiveExpiresAt time.Time
	InOfflineGrace     bool
	RevocationEpoch    uint64
}

type Status struct {
	Model              productsecurity.VerifiedModelStatusEvidence
	ProfileRevision    uint64
	ModelKeyVersion    uint64
	ResourceKeyVersion uint64
	License            LicenseStatus
}

type ModelInfo struct {
	PlaintextSize uint64
	ChunkSize     uint32
	ChunkCount    uint64
}

// ResourcePrivateKeyMetadata returns the authenticated metadata required on
// the modelprotection container that carries the vault's PKCS#8 RSA key.
func ResourcePrivateKeyMetadata(config resourcevault.Config, privateKeyID string) ([sha256.Size]byte, error) {
	if !validIdentifier(config.ExpectedBundleID, 255) ||
		!validIdentifier(config.ExpectedBundleVersion, 128) ||
		!validIdentifier(config.ExpectedSigningKeyID, 128) ||
		!validIdentifier(privateKeyID, 128) {
		return [sha256.Size]byte{}, ErrDenied
	}
	payload := "meadow/protectedbootstrap/resource-private-key/pkcs8/v1\x00" +
		config.ExpectedBundleID + "\x00" +
		config.ExpectedBundleVersion + "\x00" +
		config.ExpectedSigningKeyID + "\x00" +
		privateKeyID
	return sha256.Sum256([]byte(payload)), nil
}

func validIdentifier(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		value != strings.TrimSpace(value) || strings.IndexByte(value, 0) >= 0 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '.' || character == '-' || character == '_' ||
			character == ':' || character == '+' || character == '@' {
			continue
		}
		return false
	}
	return true
}

func denied(stage Stage) error {
	return &PrepareError{Stage: stage}
}
