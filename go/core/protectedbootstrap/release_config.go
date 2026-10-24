package protectedbootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/keybroker"
	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
	"github.com/hey-meadow/meadow-code/go/core/resourcevault"
)

const (
	ReleaseConfigManifestSchemaVersion = 1
	maxReleaseConfigManifestBytes      = 1 << 20
	maxReleaseConfigSignatureBytes     = 64 << 10
)

// These roots are populated only by the protected release linker. They are
// deliberately private so runtime callers cannot replace release trust.
var (
	buildReleaseConfigPublicKeyB64 = ""
	buildReleaseConfigKeyID        = ""
)

// TrustedDeviceIdentity is implemented by the platform identity boundary. It
// must derive a stable binding from protected local platform state and must not
// use an environment variable, command-line value, user-selected file, URL or
// other caller-controlled material.
type TrustedDeviceIdentity interface {
	DeviceBindingSHA256(context.Context) ([sha256.Size]byte, error)
}

// ReleaseResourceInputs are already-open handles to fixed resources in the
// signed application bundle. No path or URL crosses this boundary. Ownership
// transfers to Config on success; on failure NewReleaseConfig closes all three.
type ReleaseResourceInputs struct {
	Index               io.ReadCloser
	Signature           io.ReadCloser
	PrivateKeyContainer io.ReadCloser
}

// ReleaseConfigRequest contains only authenticated release documents, opaque
// resource handles, and the trusted device identity boundary. Manifest and
// ManifestSignature are always consumed and closed by NewReleaseConfig.
type ReleaseConfigRequest struct {
	Manifest          io.ReadCloser
	ManifestSignature io.ReadCloser
	Resources         ReleaseResourceInputs
	DeviceIdentity    TrustedDeviceIdentity
}

type releaseConfigSignatureDocument struct {
	SchemaVersion   int    `json:"schema_version"`
	Algorithm       string `json:"algorithm"`
	KeyID           string `json:"key_id"`
	SignatureBase64 string `json:"signature_base64"`
}

type releaseConfigManifestDocument struct {
	SchemaVersion   int                     `json:"schema_version"`
	KeychainService string                  `json:"keychain_service"`
	License         releaseLicenseDocument  `json:"license"`
	Model           releaseModelDocument    `json:"model"`
	Resources       releaseResourceDocument `json:"resources"`
}

type releaseLicenseDocument struct {
	Product                      string            `json:"product"`
	Version                      uint64            `json:"version"`
	PublicKeys                   map[string]string `json:"public_keys"`
	RequiredCapabilities         []string          `json:"required_capabilities"`
	MaxOfflineGraceSeconds       int64             `json:"max_offline_grace_seconds"`
	FutureClockSkewSeconds        int64             `json:"future_clock_skew_seconds"`
	ClockRollbackToleranceSeconds int64             `json:"clock_rollback_tolerance_seconds"`
}

type releaseModelDocument struct {
	LogicalModelID string `json:"logical_model_id"`
	AssetID        string `json:"asset_id"`
}

type releaseResourceDocument struct {
	VerificationKeys    map[string]string    `json:"verification_keys"`
	ExpectedBundleID    string               `json:"expected_bundle_id"`
	ExpectedBundleVersion string             `json:"expected_bundle_version"`
	ExpectedSigningKeyID  string             `json:"expected_signing_key_id"`
	PrivateKeyID          string             `json:"private_key_id"`
	Limits                releaseLimitsDocument `json:"limits"`
}

type releaseLimitsDocument struct {
	IndexBytes      int64 `json:"index_bytes"`
	SignatureBytes  int64 `json:"signature_bytes"`
	PayloadBytes    int64 `json:"payload_bytes"`
	PlaintextBytes  int64 `json:"plaintext_bytes"`
	WrappedKeyBytes int   `json:"wrapped_key_bytes"`
	Resources       int   `json:"resources"`
}

type staticVerificationKeys struct {
	keys map[string]ed25519.PublicKey
}

func (provider *staticVerificationKeys) PublicKey(ctx context.Context, keyID string) (ed25519.PublicKey, error) {
	if provider == nil || ctx == nil || ctx.Err() != nil || !validIdentifier(keyID, 128) {
		return nil, ErrDenied
	}
	key, ok := provider.keys[keyID]
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, ErrDenied
	}
	result := make(ed25519.PublicKey, len(key))
	copy(result, key)
	return result, nil
}

// NewReleaseConfig creates the only production Config assembly boundary. The
// static policy is accepted only from an Ed25519-signed release manifest whose
// trust root was linked into this protected binary. The selected model is also
// rebound to the current App's signed model-pack profile. Licensing starts in
// stored mode; activation must have provisioned signed entitlement state before
// protected startup.
func NewReleaseConfig(ctx context.Context, request ReleaseConfigRequest) (Config, error) {
	resourcesOwned := true
	defer func() {
		if resourcesOwned {
			closeReleaseResources(request.Resources)
		}
	}()
	if ctx == nil || ctx.Err() != nil || request.DeviceIdentity == nil ||
		request.Manifest == nil || request.ManifestSignature == nil ||
		request.Resources.Index == nil || request.Resources.Signature == nil ||
		request.Resources.PrivateKeyContainer == nil {
		closeReleaseDocument(request.Manifest)
		closeReleaseDocument(request.ManifestSignature)
		return Config{}, denied(StageConfiguration)
	}
	manifestPayload, err := readReleaseDocument(request.Manifest, maxReleaseConfigManifestBytes)
	if err != nil {
		closeReleaseDocument(request.ManifestSignature)
		return Config{}, denied(StageConfiguration)
	}
	defer clear(manifestPayload)
	signaturePayload, err := readReleaseDocument(request.ManifestSignature, maxReleaseConfigSignatureBytes)
	if err != nil {
		return Config{}, denied(StageConfiguration)
	}
	defer clear(signaturePayload)
	if !productsecurity.Required() || productsecurity.VerifyCurrentBundle() != nil ||
		verifyReleaseConfigSignature(manifestPayload, signaturePayload) != nil {
		return Config{}, denied(StageProductSecurity)
	}
	var document releaseConfigManifestDocument
	if decodeStrictReleaseJSON(manifestPayload, &document) != nil ||
		document.SchemaVersion != ReleaseConfigManifestSchemaVersion {
		return Config{}, denied(StageConfiguration)
	}
	config, err := releaseConfigFromDocument(ctx, document, request)
	if err != nil {
		return Config{}, err
	}
	if validateConfig(ctx, config) != nil {
		return Config{}, denied(StageConfiguration)
	}
	resourcesOwned = false
	return config, nil
}

func releaseConfigFromDocument(
	ctx context.Context,
	document releaseConfigManifestDocument,
	request ReleaseConfigRequest,
) (Config, error) {
	licenseKeys, err := decodeReleasePublicKeys(document.License.PublicKeys, 128)
	if err != nil {
		return Config{}, denied(StageConfiguration)
	}
	resourceKeys, err := decodeReleasePublicKeys(document.Resources.VerificationKeys, 128)
	if err != nil {
		return Config{}, denied(StageConfiguration)
	}
	capabilities, err := copyReleaseCapabilities(document.License.RequiredCapabilities)
	if err != nil {
		return Config{}, denied(StageConfiguration)
	}
	maxOfflineGrace, ok := releaseDuration(document.License.MaxOfflineGraceSeconds)
	if !ok {
		return Config{}, denied(StageConfiguration)
	}
	futureClockSkew, ok := releaseDuration(document.License.FutureClockSkewSeconds)
	if !ok {
		return Config{}, denied(StageConfiguration)
	}
	clockRollbackTolerance, ok := releaseDuration(document.License.ClockRollbackToleranceSeconds)
	if !ok || !validReleaseLimits(document.Resources.Limits) {
		return Config{}, denied(StageConfiguration)
	}
	profile, err := productsecurity.ResolveCurrentModelProfile(document.Model.LogicalModelID)
	if err != nil || profile.LogicalModelID != document.Model.LogicalModelID ||
		profile.BundleIdentifier != document.Resources.ExpectedBundleID {
		return Config{}, denied(StageModelProfile)
	}
	if _, found := selectModelAsset(profile, document.Model.AssetID); !found {
		return Config{}, denied(StageModelProfile)
	}
	if _, found := resourceKeys[document.Resources.ExpectedSigningKeyID]; !found {
		return Config{}, denied(StageConfiguration)
	}
	digest, err := request.DeviceIdentity.DeviceBindingSHA256(ctx)
	if err != nil {
		return Config{}, denied(StageConfiguration)
	}
	if _, err := keybroker.DeviceBindingFromDigest(digest); err != nil {
		return Config{}, denied(StageConfiguration)
	}
	return Config{
		KeychainService:     document.KeychainService,
		DeviceBindingSHA256: digest,
		License: LicensePolicy{
			Product:                document.License.Product,
			Version:                document.License.Version,
			PublicKeys:             licenseKeys,
			RequiredCapabilities:   capabilities,
			MaxOfflineGrace:        maxOfflineGrace,
			FutureClockSkew:        futureClockSkew,
			ClockRollbackTolerance: clockRollbackTolerance,
			Mode:                   LicenseStored,
		},
		Model: ModelConfig{
			LogicalModelID: document.Model.LogicalModelID,
			AssetID:        document.Model.AssetID,
		},
		Resources: ResourceConfig{
			Index:               request.Resources.Index,
			Signature:           request.Resources.Signature,
			VerificationKeys:    &staticVerificationKeys{keys: resourceKeys},
			VaultConfig: resourcevault.Config{
				ExpectedBundleID:      document.Resources.ExpectedBundleID,
				ExpectedBundleVersion: document.Resources.ExpectedBundleVersion,
				ExpectedSigningKeyID:  document.Resources.ExpectedSigningKeyID,
				Limits: resourcevault.Limits{
					IndexBytes:      document.Resources.Limits.IndexBytes,
					SignatureBytes:  document.Resources.Limits.SignatureBytes,
					PayloadBytes:    document.Resources.Limits.PayloadBytes,
					PlaintextBytes:  document.Resources.Limits.PlaintextBytes,
					WrappedKeyBytes: document.Resources.Limits.WrappedKeyBytes,
					Resources:       document.Resources.Limits.Resources,
				},
			},
			PrivateKeyID:        document.Resources.PrivateKeyID,
			PrivateKeyContainer: request.Resources.PrivateKeyContainer,
		},
	}, nil
}

func verifyReleaseConfigSignature(manifest, encodedSignature []byte) error {
	if !validIdentifier(buildReleaseConfigKeyID, 128) || strings.TrimSpace(buildReleaseConfigPublicKeyB64) == "" {
		return ErrDenied
	}
	publicKey, err := base64.StdEncoding.DecodeString(buildReleaseConfigPublicKeyB64)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		clear(publicKey)
		return ErrDenied
	}
	defer clear(publicKey)
	var signature releaseConfigSignatureDocument
	if decodeStrictReleaseJSON(encodedSignature, &signature) != nil ||
		signature.SchemaVersion != ReleaseConfigManifestSchemaVersion ||
		signature.Algorithm != "ed25519" || signature.KeyID != buildReleaseConfigKeyID {
		return ErrDenied
	}
	decoded, err := base64.StdEncoding.DecodeString(signature.SignatureBase64)
	if err != nil || len(decoded) != ed25519.SignatureSize {
		clear(decoded)
		return ErrDenied
	}
	defer clear(decoded)
	if !ed25519.Verify(ed25519.PublicKey(publicKey), manifest, decoded) {
		return ErrDenied
	}
	return nil
}

func decodeReleasePublicKeys(encoded map[string]string, maxKeyIDBytes int) (map[string]ed25519.PublicKey, error) {
	if len(encoded) == 0 || len(encoded) > 32 {
		return nil, ErrDenied
	}
	keys := make(map[string]ed25519.PublicKey, len(encoded))
	for keyID, value := range encoded {
		if !validIdentifier(keyID, maxKeyIDBytes) || strings.TrimSpace(value) == "" {
			return nil, ErrDenied
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			clear(decoded)
			return nil, ErrDenied
		}
		key := make(ed25519.PublicKey, len(decoded))
		copy(key, decoded)
		clear(decoded)
		keys[keyID] = key
	}
	return keys, nil
}

func copyReleaseCapabilities(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 64 {
		return nil, ErrDenied
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validIdentifier(value, 128) {
			return nil, ErrDenied
		}
		if _, exists := seen[value]; exists {
			return nil, ErrDenied
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func releaseDuration(seconds int64) (time.Duration, bool) {
	const maxDurationSeconds = int64((1<<63)-1) / int64(time.Second)
	if seconds <= 0 || seconds > maxDurationSeconds {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func validReleaseLimits(limits releaseLimitsDocument) bool {
	return limits.IndexBytes > 0 && limits.SignatureBytes > 0 &&
		limits.PayloadBytes > 0 && limits.PlaintextBytes > 0 &&
		limits.WrappedKeyBytes > 0 && limits.Resources > 0
}

func readReleaseDocument(source io.ReadCloser, limit int64) ([]byte, error) {
	if source == nil || limit <= 0 {
		return nil, ErrDenied
	}
	value, readErr := io.ReadAll(io.LimitReader(source, limit+1))
	closeErr := source.Close()
	if readErr != nil || closeErr != nil || len(value) == 0 || int64(len(value)) > limit {
		clear(value)
		return nil, ErrDenied
	}
	return value, nil
}

func decodeStrictReleaseJSON(encoded []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return ErrDenied
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrDenied
	}
	return nil
}

func closeReleaseDocument(source io.ReadCloser) {
	if source != nil {
		_ = source.Close()
	}
}

func closeReleaseResources(resources ReleaseResourceInputs) {
	closeReleaseDocument(resources.Index)
	closeReleaseDocument(resources.Signature)
	closeReleaseDocument(resources.PrivateKeyContainer)
}

var _ resourcevault.VerificationKeyProvider = (*staticVerificationKeys)(nil)
