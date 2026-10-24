package sealedasset

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/nativeassettransport"
)

const (
	ManifestSchemaVersion = 1
	EvidenceSchemaVersion = 1
	FormatVersion         = 1
	MasterKeyBytes        = 32
	DefaultChunkBytes     = 1 << 20
	EvidenceFilename      = "sealed-model.json"
	maxSignatureBytes     = 64 << 10
	maxEncryptedMetadata  = 1 << 20
)

var (
	ErrInvalidArgument   = errors.New("model seal: invalid argument")
	ErrManifestRejected  = errors.New("model seal: signed manifest rejected")
	ErrKeyUnavailable    = errors.New("model seal: master key unavailable")
	ErrAssetRejected     = errors.New("model seal: asset rejected")
	ErrIntegrity         = errors.New("model seal: integrity failure")
	ErrOutputUnavailable = errors.New("model seal: atomic output unavailable")
	ErrClosed            = errors.New("model seal: lease closed")
	ErrCanceled          = errors.New("model seal: lease canceled")
)

// KeyProvider transfers ownership of one exact 32-byte master key to the
// caller. The caller always clears the returned slice.
type KeyProvider interface {
	MasterKey(context.Context) ([]byte, error)
}

type KeyProviderFunc func(context.Context) ([]byte, error)

func (provide KeyProviderFunc) MasterKey(ctx context.Context) ([]byte, error) {
	if provide == nil {
		return nil, ErrKeyUnavailable
	}
	return provide(ctx)
}

// FDKeyProvider owns the inherited descriptor and consumes it exactly once.
// The descriptor must contain exactly 32 bytes and then EOF.
type FDKeyProvider struct {
	mu   sync.Mutex
	file *os.File
	used bool
}

func NewFDKeyProvider(fd int) (*FDKeyProvider, error) {
	if fd < 3 {
		return nil, ErrKeyUnavailable
	}
	file := os.NewFile(uintptr(fd), "meadow-model-seal-master-key")
	if file == nil {
		return nil, ErrKeyUnavailable
	}
	return &FDKeyProvider{file: file}, nil
}

func (provider *FDKeyProvider) MasterKey(ctx context.Context) ([]byte, error) {
	if provider == nil || ctx == nil {
		return nil, ErrKeyUnavailable
	}
	provider.mu.Lock()
	if provider.used || provider.file == nil {
		provider.mu.Unlock()
		return nil, ErrKeyUnavailable
	}
	provider.used = true
	file := provider.file
	provider.file = nil
	provider.mu.Unlock()
	defer file.Close()
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(ErrKeyUnavailable, err)
	}
	key := make([]byte, MasterKeyBytes)
	if _, err := io.ReadFull(file, key); err != nil {
		zero(key)
		return nil, ErrKeyUnavailable
	}
	var trailing [1]byte
	count, err := file.Read(trailing[:])
	zero(trailing[:])
	if count != 0 || err != io.EOF {
		zero(key)
		return nil, ErrKeyUnavailable
	}
	if err := ctx.Err(); err != nil {
		zero(key)
		return nil, errors.Join(ErrKeyUnavailable, err)
	}
	return key, nil
}

type ManifestSignatureVerifier interface {
	VerifyManifestSignature(payload, signature []byte) error
}

type Ed25519ManifestVerifier struct {
	PublicKey     ed25519.PublicKey
	ExpectedKeyID string
}

type signatureDocument struct {
	SchemaVersion   int    `json:"schema_version"`
	Algorithm       string `json:"algorithm"`
	KeyID           string `json:"key_id"`
	SignatureBase64 string `json:"signature_base64"`
}

func (verifier Ed25519ManifestVerifier) VerifyManifestSignature(payload, signature []byte) error {
	if len(verifier.PublicKey) != ed25519.PublicKeySize || len(signature) == 0 || len(signature) > maxSignatureBytes {
		return ErrManifestRejected
	}
	var document signatureDocument
	if err := decodeStrictJSON(signature, &document); err != nil ||
		document.SchemaVersion != 1 || document.Algorithm != "Ed25519" ||
		document.KeyID == "" ||
		(verifier.ExpectedKeyID != "" && document.KeyID != verifier.ExpectedKeyID) {
		return ErrManifestRejected
	}
	decoded, err := base64.StdEncoding.DecodeString(document.SignatureBase64)
	if err != nil || len(decoded) != ed25519.SignatureSize {
		zero(decoded)
		return ErrManifestRejected
	}
	valid := ed25519.Verify(verifier.PublicKey, payload, decoded)
	zero(decoded)
	if !valid {
		return ErrManifestRejected
	}
	return nil
}

type ManifestAssetDocument struct {
	Ordinal         uint32 `json:"ordinal"`
	AssetID         string `json:"asset_id"`
	Role            string `json:"role"`
	Required        bool   `json:"required"`
	PlaintextSize   uint64 `json:"plaintext_size"`
	PlaintextSHA256 string `json:"plaintext_sha256"`
}

type ProductAssetManifestDocument struct {
	SchemaVersion   int                     `json:"schema_version"`
	ModelID         string                  `json:"model_id"`
	ProfileRevision uint64                  `json:"profile_revision"`
	Backend         uint32                  `json:"backend"`
	ProfileDigest   string                  `json:"profile_digest"`
	AssetSetDigest  string                  `json:"asset_set_digest"`
	Assets          []ManifestAssetDocument `json:"assets"`
}

type VerifiedProductManifest struct {
	native          nativeassettransport.VerifiedManifest
	assetSetDigest  [sha256.Size]byte
	manifestDigest  [sha256.Size]byte
	canonicalDigest [sha256.Size]byte
}

func (verified VerifiedProductManifest) NativeManifest() nativeassettransport.VerifiedManifest {
	return verified.native
}

func (verified VerifiedProductManifest) SignedManifest() nativeassettransport.SignedManifest {
	return verified.native.Manifest()
}

func (verified VerifiedProductManifest) AssetSetDigest() [sha256.Size]byte {
	return verified.assetSetDigest
}

func (verified VerifiedProductManifest) ManifestDigest() [sha256.Size]byte {
	return verified.manifestDigest
}

func (verified VerifiedProductManifest) CanonicalDigest() [sha256.Size]byte {
	return verified.canonicalDigest
}

type SealRequest struct {
	ManifestPath          string
	ManifestSignaturePath string
	AssetPaths            map[string]string
	OutputDirectory       string
	ChunkBytes            uint32
	SignatureVerifier     ManifestSignatureVerifier
	Keys                  KeyProvider
	Random                io.Reader
}

type AssetEvidence struct {
	Ordinal         uint32 `json:"ordinal"`
	AssetID         string `json:"asset_id"`
	Role            string `json:"role"`
	Required        bool   `json:"required"`
	PlaintextBytes  uint64 `json:"plaintext_bytes"`
	PlaintextSHA256 string `json:"plaintext_sha256"`
	ChunkBytes      uint32 `json:"chunk_bytes"`
	ChunkCount      uint64 `json:"chunk_count"`
	SealedBytes     uint64 `json:"sealed_bytes"`
	SealedSHA256    string `json:"sealed_sha256"`
}

type Evidence struct {
	SchemaVersion           int             `json:"schema_version"`
	FormatVersion           int             `json:"format_version"`
	ModelID                 string          `json:"model_id"`
	ProfileRevision         uint64          `json:"profile_revision"`
	Backend                 uint32          `json:"backend"`
	ProfileDigest           string          `json:"profile_digest"`
	AssetSetDigest          string          `json:"asset_set_digest"`
	ManifestDigest          string          `json:"manifest_digest"`
	CanonicalManifestDigest string          `json:"canonical_manifest_digest"`
	Assets                  []AssetEvidence `json:"assets"`
	EvidenceDigest          string          `json:"evidence_digest"`
}

func decodeStrictJSON(payload []byte, destination any) error {
	decoder := json.NewDecoder(newSliceReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
