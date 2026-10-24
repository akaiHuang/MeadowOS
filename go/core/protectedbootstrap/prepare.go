package protectedbootstrap

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strconv"

	"github.com/hey-meadow/meadow-code/go/core/keybroker"
	"github.com/hey-meadow/meadow-code/go/core/licensing"
	"github.com/hey-meadow/meadow-code/go/core/modelprotection"
	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
	"github.com/hey-meadow/meadow-code/go/core/resourcevault"
	"github.com/hey-meadow/meadow-code/go/core/runtimeintegrity"
)

const (
	maxLicenseEnvelopeBytes = 4 << 20
	maxPrivateKeyBytes      = 64 << 10
)

type readerOnly struct {
	io.Reader
}

type ownedInputs struct {
	readers []io.ReadCloser
	closed  bool
}

func (inputs *ownedInputs) Close() error {
	if inputs == nil || inputs.closed {
		return nil
	}
	inputs.closed = true
	failed := false
	for index := len(inputs.readers) - 1; index >= 0; index-- {
		if inputs.readers[index] != nil {
			if err := inputs.readers[index].Close(); err != nil {
				failed = true
			}
		}
	}
	if failed {
		return ErrCloseFailed
	}
	return nil
}

// Prepare transfers ownership of all ResourceConfig readers and returns a
// lifecycle only after every protection boundary succeeds.
func Prepare(ctx context.Context, config Config) (*Prepared, error) {
	inputs := &ownedInputs{readers: []io.ReadCloser{
		config.Resources.Index,
		config.Resources.Signature,
		config.Resources.PrivateKeyContainer,
	}}
	defer inputs.Close()
	prepared := newPrepared()
	complete := false
	defer func() {
		if !complete {
			_ = prepared.Close()
		}
	}()

	if err := validateConfig(ctx, config); err != nil {
		return nil, denied(StageConfiguration)
	}
	if !productsecurity.Required() {
		return nil, denied(StagePublicBuild)
	}
	integrity, err := runtimeintegrity.Verify()
	if err != nil {
		return nil, denied(StageRuntimeIntegrity)
	}
	if err := productsecurity.VerifyCurrentBundle(); err != nil {
		return nil, denied(StageProductSecurity)
	}
	profile, err := productsecurity.ResolveCurrentModelProfile(config.Model.LogicalModelID)
	if err != nil {
		return nil, denied(StageModelProfile)
	}
	if profile.BundleIdentifier != integrity.Application.BundleIdentifier ||
		profile.BundleIdentifier != config.Resources.VaultConfig.ExpectedBundleID {
		return nil, denied(StageModelProfile)
	}
	asset, ok := selectModelAsset(profile, config.Model.AssetID)
	if !ok {
		return nil, denied(StageModelProfile)
	}

	binding, err := keybroker.DeviceBindingFromDigest(config.DeviceBindingSHA256)
	if err != nil {
		return nil, denied(StageConfiguration)
	}
	broker, err := keybroker.NewKeychain(config.KeychainService, binding)
	if err != nil {
		return nil, denied(StageKeyBroker)
	}
	prepared.broker = broker

	licenseStore := newLicenseStateStore(
		broker.LicenseDeviceState(),
		licenseStateResource(config.License.Product, config.License.Version),
	)
	verifier, err := licensing.NewLocalVerifier(licensing.LocalVerifierConfig{
		Product:                config.License.Product,
		Version:                config.License.Version,
		PublicKeys:             config.License.PublicKeys,
		RequiredCapabilities:   config.License.RequiredCapabilities,
		MaxOfflineGrace:        config.License.MaxOfflineGrace,
		FutureClockSkew:        config.License.FutureClockSkew,
		ClockRollbackTolerance: config.License.ClockRollbackTolerance,
		SecureStore:            licenseStore,
		AntiRollbackStore:      licenseStore,
	})
	if err != nil {
		return nil, denied(StageLicensing)
	}
	licenseRequest := licensing.LocalVerifyRequest{DeviceBinding: licensing.DeviceBinding{
		Algorithm: licensing.DeviceBindingSHA256,
		Digest:    base64.RawURLEncoding.EncodeToString(config.DeviceBindingSHA256[:]),
	}}
	var licenseResult licensing.LocalVerificationResult
	switch config.License.Mode {
	case LicenseFresh:
		licenseResult, err = verifier.Verify(
			ctx,
			config.License.EntitlementEnvelope,
			config.License.RevocationEnvelope,
			licenseRequest,
		)
	case LicenseStored:
		licenseResult, err = verifier.VerifyStored(ctx, licenseRequest)
	default:
		err = ErrDenied
	}
	if err != nil {
		return nil, denied(StageLicensing)
	}

	modelFile, err := openVerifiedModelAsset(integrity.Application.ContentsPath, asset)
	if err != nil {
		return nil, denied(StageModelContainer)
	}
	modelKeyLease, err := broker.ModelDataKeys().Current(ctx, modelKeyResource(profile, asset))
	if err != nil {
		_ = modelFile.Close()
		return nil, denied(StageModelKey)
	}
	modelKeyVersion := modelKeyLease.Version()
	modelWrapper, err := wrapperFromLease(modelKeyLease)
	if err != nil {
		_ = modelFile.Close()
		return nil, denied(StageModelKey)
	}
	prepared.modelWrapper = modelWrapper
	modelReader, err := modelprotection.NewReader(ctx, modelFile, modelWrapper)
	if err != nil {
		_ = modelFile.Close()
		return nil, denied(StageModelContainer)
	}
	prepared.modelReader = modelReader
	modelInfo := modelReader.Info()
	prepared.modelInfo = ModelInfo{
		PlaintextSize: modelInfo.PlaintextSize,
		ChunkSize:     modelInfo.ChunkSize,
		ChunkCount:    modelInfo.ChunkCount,
	}

	metadata, err := ResourcePrivateKeyMetadata(config.Resources.VaultConfig, config.Resources.PrivateKeyID)
	if err != nil {
		return nil, denied(StageResourceUnwrapper)
	}
	resourceKeyLease, err := broker.ResourceVaultKeys().Current(
		ctx,
		resourceKeyResource(metadata),
	)
	if err != nil {
		return nil, denied(StageResourceKey)
	}
	resourceKeyVersion := resourceKeyLease.Version()
	resourceWrapper, err := wrapperFromLease(resourceKeyLease)
	if err != nil {
		return nil, denied(StageResourceKey)
	}
	privateKey, err := loadResourcePrivateKey(
		ctx,
		readerOnly{Reader: config.Resources.PrivateKeyContainer},
		resourceWrapper,
		metadata,
	)
	resourceWrapper.Destroy()
	if err != nil {
		return nil, denied(StageResourceUnwrapper)
	}
	unwrapper, err := newRSAKeyUnwrapper(privateKey, config.Resources.PrivateKeyID)
	if err != nil {
		wipeRSAKey(privateKey)
		return nil, denied(StageResourceUnwrapper)
	}
	prepared.vaultUnwrapper = unwrapper
	vault, err := resourcevault.New(
		ctx,
		readerOnly{Reader: config.Resources.Index},
		readerOnly{Reader: config.Resources.Signature},
		config.Resources.VerificationKeys,
		unwrapper,
		config.Resources.VaultConfig,
	)
	if err != nil {
		return nil, denied(StageResourceVault)
	}
	prepared.vault = vault
	if err := inputs.Close(); err != nil {
		return nil, denied(StageInputClose)
	}

	prepared.status = Status{
		Model:              profile.StatusEvidence(),
		ProfileRevision:    profile.ProfileRevision,
		ModelKeyVersion:    uint64(modelKeyVersion),
		ResourceKeyVersion: uint64(resourceKeyVersion),
		License: LicenseStatus{
			EffectiveExpiresAt: licenseResult.EffectiveExpiresAt,
			InOfflineGrace:     licenseResult.InOfflineGrace,
			RevocationEpoch:    licenseResult.RevocationEpoch,
		},
	}
	complete = true
	return prepared, nil
}

func validateConfig(ctx context.Context, config Config) error {
	if ctx == nil || ctx.Err() != nil ||
		!validIdentifier(config.KeychainService, 255) ||
		!validIdentifier(config.License.Product, 255) ||
		config.License.Version == 0 ||
		len(config.License.PublicKeys) == 0 ||
		len(config.License.RequiredCapabilities) == 0 ||
		!validIdentifier(config.Model.LogicalModelID, 192) ||
		!validIdentifier(config.Model.AssetID, 192) ||
		config.Resources.Index == nil ||
		config.Resources.Signature == nil ||
		config.Resources.PrivateKeyContainer == nil ||
		config.Resources.VerificationKeys == nil ||
		!validIdentifier(config.Resources.PrivateKeyID, 128) {
		return ErrDenied
	}
	if _, err := keybroker.DeviceBindingFromDigest(config.DeviceBindingSHA256); err != nil {
		return ErrDenied
	}
	switch config.License.Mode {
	case LicenseFresh:
		if len(config.License.EntitlementEnvelope) == 0 ||
			len(config.License.EntitlementEnvelope) > maxLicenseEnvelopeBytes ||
			len(config.License.RevocationEnvelope) == 0 ||
			len(config.License.RevocationEnvelope) > maxLicenseEnvelopeBytes {
			return ErrDenied
		}
	case LicenseStored:
		if len(config.License.EntitlementEnvelope) != 0 ||
			len(config.License.RevocationEnvelope) != 0 {
			return ErrDenied
		}
	default:
		return ErrDenied
	}
	if _, err := ResourcePrivateKeyMetadata(config.Resources.VaultConfig, config.Resources.PrivateKeyID); err != nil {
		return ErrDenied
	}
	return nil
}

func selectModelAsset(profile productsecurity.VerifiedModelProfile, assetID string) (productsecurity.VerifiedModelAsset, bool) {
	for _, asset := range profile.Assets {
		if asset.ID == assetID {
			return asset, true
		}
	}
	return productsecurity.VerifiedModelAsset{}, false
}

func wrapperFromLease(lease *keybroker.Lease) (*modelprotection.AESGCMKeyWrapper, error) {
	if lease == nil {
		return nil, ErrDenied
	}
	defer lease.Destroy()
	var wrapper *modelprotection.AESGCMKeyWrapper
	err := lease.WithBytes(func(key []byte) error {
		created, createErr := modelprotection.NewAESGCMKeyWrapper(key)
		if createErr != nil {
			return createErr
		}
		wrapper = created
		return nil
	})
	if err != nil || wrapper == nil {
		return nil, ErrDenied
	}
	return wrapper, nil
}

func loadResourcePrivateKey(
	ctx context.Context,
	source io.Reader,
	wrapper *modelprotection.AESGCMKeyWrapper,
	expectedMetadata [sha256.Size]byte,
) (*rsa.PrivateKey, error) {
	reader, err := modelprotection.NewReader(ctx, source, wrapper)
	if err != nil {
		return nil, ErrDenied
	}
	info := reader.Info()
	if len(info.Metadata) != len(expectedMetadata) ||
		subtle.ConstantTimeCompare(info.Metadata, expectedMetadata[:]) != 1 ||
		info.PlaintextSize == 0 || info.PlaintextSize > maxPrivateKeyBytes {
		_ = reader.Close()
		return nil, ErrDenied
	}
	encoded, readErr := io.ReadAll(io.LimitReader(reader, maxPrivateKeyBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(encoded) == 0 ||
		len(encoded) > maxPrivateKeyBytes || uint64(len(encoded)) != info.PlaintextSize {
		clear(encoded)
		return nil, ErrDenied
	}
	defer clear(encoded)
	parsed, err := x509.ParsePKCS8PrivateKey(encoded)
	if err != nil {
		return nil, ErrDenied
	}
	privateKey, ok := parsed.(*rsa.PrivateKey)
	if !ok || privateKey.N == nil || privateKey.N.BitLen() < 2048 ||
		privateKey.N.BitLen() > 8192 || privateKey.E != 65537 {
		if ok {
			wipeRSAKey(privateKey)
		}
		return nil, ErrDenied
	}
	if err := privateKey.Validate(); err != nil {
		wipeRSAKey(privateKey)
		return nil, ErrDenied
	}
	privateKey.Precompute()
	return privateKey, nil
}

func modelKeyResource(profile productsecurity.VerifiedModelProfile, asset productsecurity.VerifiedModelAsset) string {
	return "model-data:" +
		hex.EncodeToString(profile.ProfileDigest[:]) + ":" +
		strconv.FormatUint(profile.ProfileRevision, 10) + ":" +
		hex.EncodeToString(asset.SHA256[:])
}

func resourceKeyResource(metadata [sha256.Size]byte) string {
	return "resource-vault:" + hex.EncodeToString(metadata[:])
}

func licenseStateResource(product string, version uint64) string {
	digest := sha256.Sum256([]byte(product + "\x00" + strconv.FormatUint(version, 10)))
	return "license-device:" + hex.EncodeToString(digest[:])
}

var _ = errors.Is
