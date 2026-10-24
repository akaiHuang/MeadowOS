package modelvault

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/nativeassettransport"
)

const (
	CatalogSchemaVersion = 1
	StateSchemaVersion   = 1
	SealerContractV1     = 1

	maxCatalogBytes        = 4 * 1024 * 1024
	maxEntries             = 512
	maxArtifacts           = 4096
	maxIdentifierBytes     = 192
	maxTemplateBytes       = 1024
	maxLicenseTextBytes    = 4096
	maxManifestBytes       = 8 * 1024 * 1024
	maxArtifactBytes       = uint64(1) << 50
	maxChunkBytes          = 4 * 1024 * 1024
	maximumRedirects       = 3
	stateFilename          = "state.json"
	lockFilename           = ".vault.lock"
	stagingDirectory       = "staging"
	modelsDirectory        = "models"
	sealedManifestName     = "signed-manifest.bin"
	installedDirectoryMode = 0o500
)

var (
	ErrUnsupportedPlatform = errors.New("model vault is unavailable on this platform")
	ErrInvalidArgument     = errors.New("model vault argument is invalid")
	ErrUnavailable         = errors.New("model vault trust seam is unavailable")
	ErrCatalogRejected     = errors.New("signed model catalog rejected")
	ErrCatalogReplay       = errors.New("signed model catalog replay rejected")
	ErrCatalogRollback     = errors.New("signed model catalog rollback rejected")
	ErrLicenseRejected     = errors.New("model license acceptance rejected")
	ErrDownloadRejected    = errors.New("model artifact download rejected")
	ErrImportRejected      = errors.New("model import rejected")
	ErrSealRejected        = errors.New("model sealing rejected")
	ErrAlreadyInstalled    = errors.New("model version is already installed")
	ErrNotInstalled        = errors.New("model version is not installed")
	ErrRemovalRejected     = errors.New("model removal authorization rejected")
	ErrCanceled            = errors.New("model vault operation canceled")
)

// Clock is mandatory so release code explicitly supplies its trusted wall
// clock boundary. Tests can inject a fixed instant without changing catalog
// semantics.
type Clock interface {
	Now() time.Time
}

type ClockFunc func() time.Time

func (clock ClockFunc) Now() time.Time {
	if clock == nil {
		return time.Time{}
	}
	return clock()
}

type VerificationKey struct {
	KeyID     string
	PublicKey ed25519.PublicKey
	NotBefore time.Time
	NotAfter  time.Time
	Revoked   bool
}

type Keyring struct {
	Keys []VerificationKey
}

type CatalogSignature struct {
	Algorithm string
	KeyID     string
	Signature []byte
}

type CatalogSequenceAdvance struct {
	BundleID      string
	Sequence      uint64
	CatalogDigest [sha256.Size]byte
}

type CatalogSequenceQuery struct {
	BundleID string
}

type CatalogSequenceState struct {
	Sequence      uint64
	CatalogDigest [sha256.Size]byte
}

// SequenceGuard is a mandatory production seam backed by monotonic protected
// state such as the Keychain or an entitled helper. CurrentCatalog must return
// the persisted bundle-scoped sequence and digest. AdvanceCatalog must
// atomically persist a strictly newer sequence and reject rollback or a
// same-sequence digest fork. The local state file is a recoverable shadow and
// is not treated as tamper proof.
type SequenceGuard interface {
	CurrentCatalog(context.Context, CatalogSequenceQuery) (CatalogSequenceState, error)
	AdvanceCatalog(context.Context, CatalogSequenceAdvance) error
}

type SequenceGuardFuncs struct {
	Current func(context.Context, CatalogSequenceQuery) (CatalogSequenceState, error)
	Advance func(context.Context, CatalogSequenceAdvance) error
}

func (guard SequenceGuardFuncs) CurrentCatalog(
	ctx context.Context,
	query CatalogSequenceQuery,
) (CatalogSequenceState, error) {
	if guard.Current == nil {
		return CatalogSequenceState{}, ErrUnavailable
	}
	return guard.Current(ctx, query)
}

func (guard SequenceGuardFuncs) AdvanceCatalog(
	ctx context.Context,
	request CatalogSequenceAdvance,
) error {
	if guard.Advance == nil {
		return ErrUnavailable
	}
	return guard.Advance(ctx, request)
}

type Catalog struct {
	SchemaVersion uint32         `json:"schema_version"`
	Sequence      uint64         `json:"sequence"`
	IssuedAt      int64          `json:"issued_at_epoch_seconds"`
	ExpiresAt     int64          `json:"expires_at_epoch_seconds"`
	Entries       []CatalogEntry `json:"entries"`
}

type CatalogEntry struct {
	ModelID              string            `json:"model_id"`
	Version              string            `json:"version"`
	ProfileID            string            `json:"profile_id"`
	ProfileDigestSHA256  string            `json:"profile_digest_sha256"`
	LicenseID            string            `json:"license_id"`
	LicenseDigestSHA256  string            `json:"license_digest_sha256"`
	SourceOrigin         string            `json:"source_origin"`
	Artifacts            []CatalogArtifact `json:"artifacts"`
	ManifestDigestSHA256 string            `json:"manifest_digest_sha256"`
}

type CatalogArtifact struct {
	Ordinal            uint32                         `json:"ordinal"`
	AssetID            string                         `json:"asset_id"`
	Role               nativeassettransport.AssetRole `json:"role"`
	SourcePathTemplate string                         `json:"source_path_template"`
	Bytes              uint64                         `json:"bytes"`
	SHA256             string                         `json:"sha256"`
	ChunkBytes         uint32                         `json:"chunk_bytes"`
	ChunkSHA256        []string                       `json:"chunk_sha256"`
}

type LicenseBinding struct {
	ModelID       string
	Version       string
	ProfileID     string
	LicenseID     string
	LicenseDigest [sha256.Size]byte
}

type LicenseAcceptance struct {
	LicenseID     string
	LicenseDigest [sha256.Size]byte
	AcceptedAt    time.Time
	Subject       string
}

type LicenseVerifier interface {
	VerifyLicenseAcceptance(context.Context, LicenseBinding, LicenseAcceptance) error
}

type LicenseVerifierFunc func(context.Context, LicenseBinding, LicenseAcceptance) error

func (verify LicenseVerifierFunc) VerifyLicenseAcceptance(
	ctx context.Context,
	binding LicenseBinding,
	acceptance LicenseAcceptance,
) error {
	if verify == nil {
		return ErrUnavailable
	}
	return verify(ctx, binding, acceptance)
}

type ArtifactDescriptor struct {
	Ordinal uint32
	AssetID string
	Role    nativeassettransport.AssetRole
	Bytes   uint64
	Digest  [sha256.Size]byte
}

type ArtifactSource interface {
	OpenArtifact(context.Context, uint32) (io.ReadCloser, ArtifactDescriptor, error)
}

type SealRequest struct {
	ContractVersion uint32
	ModelID         string
	Version         string
	ProfileID       string
	ProfileDigest   [sha256.Size]byte
	LicenseID       string
	LicenseDigest   [sha256.Size]byte
	Artifacts       []ArtifactDescriptor
}

type SealedAssetOutput struct {
	Ordinal uint32
	AssetID string
	Role    nativeassettransport.AssetRole
	Bytes   uint64
	Digest  [sha256.Size]byte
	Content io.ReadCloser
}

type SealResult struct {
	SignedManifest []byte
	ManifestDigest [sha256.Size]byte
	Assets         []SealedAssetOutput
}

// ProductionSealer must consume ArtifactSource synchronously. It must not
// retain readers or plaintext. The vault verifies all outputs independently
// and removes the plaintext staging tree after Seal returns.
type ProductionSealer interface {
	Seal(context.Context, SealRequest, ArtifactSource) (SealResult, error)
}

type ProductionSealerFunc func(context.Context, SealRequest, ArtifactSource) (SealResult, error)

func (seal ProductionSealerFunc) Seal(
	ctx context.Context,
	request SealRequest,
	source ArtifactSource,
) (SealResult, error) {
	if seal == nil {
		return SealResult{}, ErrUnavailable
	}
	return seal(ctx, request, source)
}

type ImportMetadata struct {
	ModelID       string
	Version       string
	ProfileID     string
	ProfileDigest [sha256.Size]byte
	LicenseID     string
	LicenseDigest [sha256.Size]byte
	Artifacts     []ArtifactDescriptor
}

// UserImportSource is deliberately path-free. The client can present metadata
// and sequential readers only; it cannot hand a filesystem path to the vault
// or runtime.
type UserImportSource interface {
	Metadata(context.Context) (ImportMetadata, error)
	OpenArtifact(context.Context, uint32) (io.ReadCloser, ArtifactDescriptor, error)
}

type RemovalRequest struct {
	ModelID        string
	Version        string
	ProfileDigest  [sha256.Size]byte
	ManifestDigest [sha256.Size]byte
}

type RemovalAuthorizer interface {
	AuthorizeRemoval(context.Context, RemovalRequest) error
}

type RemovalAuthorizerFunc func(context.Context, RemovalRequest) error

func (authorize RemovalAuthorizerFunc) AuthorizeRemoval(
	ctx context.Context,
	request RemovalRequest,
) error {
	if authorize == nil {
		return ErrUnavailable
	}
	return authorize(ctx, request)
}

type InstalledAsset struct {
	Ordinal uint32
	AssetID string
	Role    nativeassettransport.AssetRole
	Bytes   uint64
	Digest  [sha256.Size]byte
}

type InstalledModelHandle struct {
	ModelID        string
	Version        string
	ProfileID      string
	ProfileDigest  [sha256.Size]byte
	LicenseID      string
	ManifestDigest [sha256.Size]byte
	InstalledAt    time.Time
	Assets         []InstalledAsset
	generation     string
}

type ModelStatus uint32

const (
	ModelStatusUnavailable ModelStatus = 0
	ModelStatusInstalled   ModelStatus = 1
)

type Config struct {
	BundleID          string
	Keyring           Keyring
	Clock             Clock
	SequenceGuard     SequenceGuard
	Transport         http.RoundTripper
	Sealer            ProductionSealer
	LicenseVerifier   LicenseVerifier
	RemovalAuthorizer RemovalAuthorizer
}
