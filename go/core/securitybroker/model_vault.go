package securitybroker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/modelvault"
	"github.com/hey-meadow/meadow-code/go/core/nativeassettransport"
	"github.com/hey-meadow/meadow-code/go/core/protectedmodelbridge"
)

const (
	modelVaultSealChunkBytes = 64 * 1024
	modelVaultMaxChunkBytes  = 1024 * 1024
	modelVaultHeaderMaxBytes = 4096
	modelVaultFrameBytes     = 9
)

var (
	ErrModelVaultUnavailable    = errors.New("security broker model vault is unavailable")
	ErrModelVaultBinding        = errors.New("security broker model vault binding rejected")
	ErrModelVaultAuthentication = errors.New("security broker model vault authentication failed")
	ErrRawModelKeyForbidden     = errors.New("raw model key access is forbidden in production")
)

var modelVaultStreamMagic = [8]byte{'M', 'V', 'S', 'B', 'G', 'O', '0', '2'}

// ModelVaultProductBinding is immutable product evidence supplied by protected
// composition. SignedManifest is returned byte-for-byte by ProductionSealer;
// the broker never invents or rewrites signed product metadata.
type ModelVaultProductBinding struct {
	Version        string
	SignedManifest []byte
	Manifest       nativeassettransport.SignedManifest
}

// OpaqueSealedAssetOpenRequest transfers ownership of Sealed to the returned
// lease. No key or filesystem path crosses this API.
type OpaqueSealedAssetOpenRequest struct {
	Product      ModelVaultProductBinding
	Asset        nativeassettransport.SignedAsset
	Sealed       io.ReadCloser
	SealedBytes  uint64
	SealedDigest [sha256.Size]byte
}

type modelVaultAssetBinding struct {
	modelID         string
	version         string
	profileDigest   [sha256.Size]byte
	profileRevision uint64
	backend         uint32
	manifestDigest  [sha256.Size]byte
	ordinal         uint32
	assetID         string
	role            uint32
	plaintextBytes  uint64
	plaintextDigest [sha256.Size]byte
}

type modelVaultBackend interface {
	BeginSeal(context.Context, modelVaultAssetBinding) ([]byte, uint64, [sha256.Size]byte, error)
	SealChunk(context.Context, modelVaultAssetBinding, []byte, uint64, bool, []byte) ([]byte, error)
	OpenChunk(context.Context, modelVaultAssetBinding, []byte, uint64, bool, []byte) ([]byte, error)
	Close() error
}

type modelVaultBackendFactory func() (modelVaultBackend, error)

type modelVaultProductionSealer struct {
	product ModelVaultProductBinding
	newBack modelVaultBackendFactory
}

type modelVaultDestroyFunc func(context.Context, modelvault.RemovalRequest) error

// ModelVaultProductKeyDestroyer is path-free and structurally implements
// modelvaultaccess.ModelKeyDestroyer without importing commercial policy into
// this lower-level package. The policy must call it only after its confirmed,
// one-use removal grant has been consumed.
type ModelVaultProductKeyDestroyer struct {
	destroy modelVaultDestroyFunc
}

func NewModelVaultProductKeyDestroyer() ModelVaultProductKeyDestroyer {
	return ModelVaultProductKeyDestroyer{destroy: destroyPlatformModelVaultProductKeys}
}

func (destroyer ModelVaultProductKeyDestroyer) DestroyModelKey(
	ctx context.Context,
	request modelvault.RemovalRequest,
) error {
	if ctx == nil || destroyer.destroy == nil || ctx.Err() != nil ||
		!validVaultIdentifier(request.ModelID) || !validVaultIdentifier(request.Version) ||
		zeroVaultDigest(request.ProfileDigest) || zeroVaultDigest(request.ManifestDigest) {
		return ErrModelVaultBinding
	}
	if err := destroyer.destroy(ctx, request); err != nil {
		return errors.Join(ErrModelVaultAuthentication, err)
	}
	return nil
}

// NewModelVaultProductionSealer creates the opaque production sealer. The
// signed manifest must already have passed product verification.
func NewModelVaultProductionSealer(product ModelVaultProductBinding) (modelvault.ProductionSealer, error) {
	return newModelVaultProductionSealer(product, newPlatformModelVaultBackend)
}

func newModelVaultProductionSealer(
	product ModelVaultProductBinding,
	factory modelVaultBackendFactory,
) (*modelVaultProductionSealer, error) {
	if err := validateProductBinding(product, true); err != nil || factory == nil {
		return nil, ErrModelVaultBinding
	}
	product.SignedManifest = append([]byte(nil), product.SignedManifest...)
	product.Manifest.Assets = append([]nativeassettransport.SignedAsset(nil), product.Manifest.Assets...)
	return &modelVaultProductionSealer{product: product, newBack: factory}, nil
}

func validateProductBinding(product ModelVaultProductBinding, requireBytes bool) error {
	manifest := product.Manifest
	if !validVaultIdentifier(product.Version) || !validVaultIdentifier(manifest.ModelID) ||
		manifest.ProfileRevision == 0 || manifest.Backend == 0 ||
		zeroVaultDigest(manifest.ProfileDigest) || zeroVaultDigest(manifest.ManifestDigest) ||
		len(manifest.Assets) == 0 {
		return ErrModelVaultBinding
	}
	if requireBytes && (len(product.SignedManifest) == 0 ||
		sha256.Sum256(product.SignedManifest) != manifest.ManifestDigest) {
		return ErrModelVaultBinding
	}
	for index := range manifest.Assets {
		asset := manifest.Assets[index]
		if asset.Ordinal != uint32(index) || !validVaultIdentifier(asset.AssetID) ||
			asset.PlaintextSize == 0 || asset.Binding.ModelID != manifest.ModelID ||
			asset.Binding.ProfileDigest != manifest.ProfileDigest ||
			asset.Binding.ProfileRevision != manifest.ProfileRevision ||
			asset.Binding.Backend != manifest.Backend || zeroVaultDigest(asset.Binding.AssetDigest) {
			return ErrModelVaultBinding
		}
	}
	return nil
}

func validVaultIdentifier(value string) bool {
	return value != "" && len(value) <= 192 && value == strings.TrimSpace(value) &&
		!strings.ContainsRune(value, 0)
}

func zeroVaultDigest(value [sha256.Size]byte) bool {
	var combined byte
	for _, item := range value {
		combined |= item
	}
	return combined == 0
}

func bindingForAsset(
	product ModelVaultProductBinding,
	asset nativeassettransport.SignedAsset,
) (modelVaultAssetBinding, error) {
	if err := validateProductBinding(product, false); err != nil ||
		int(asset.Ordinal) >= len(product.Manifest.Assets) ||
		!signedAssetEqual(asset, product.Manifest.Assets[asset.Ordinal]) {
		return modelVaultAssetBinding{}, ErrModelVaultBinding
	}
	return modelVaultAssetBinding{
		modelID: product.Manifest.ModelID, version: product.Version,
		profileDigest:   product.Manifest.ProfileDigest,
		profileRevision: product.Manifest.ProfileRevision,
		backend:         uint32(product.Manifest.Backend), manifestDigest: product.Manifest.ManifestDigest,
		ordinal: asset.Ordinal, assetID: asset.AssetID, role: uint32(asset.Role),
		plaintextBytes: asset.PlaintextSize, plaintextDigest: asset.Binding.AssetDigest,
	}, nil
}

func signedAssetEqual(left, right nativeassettransport.SignedAsset) bool {
	return left.Ordinal == right.Ordinal && left.AssetID == right.AssetID &&
		left.Role == right.Role && left.Required == right.Required &&
		left.PlaintextSize == right.PlaintextSize && left.Binding == right.Binding
}

func (sealer *modelVaultProductionSealer) Seal(
	ctx context.Context,
	request modelvault.SealRequest,
	source modelvault.ArtifactSource,
) (result modelvault.SealResult, err error) {
	if ctx == nil || source == nil || request.ContractVersion != modelvault.SealerContractV1 ||
		request.ModelID != sealer.product.Manifest.ModelID || request.Version != sealer.product.Version ||
		request.ProfileDigest != sealer.product.Manifest.ProfileDigest ||
		len(request.Artifacts) != len(sealer.product.Manifest.Assets) {
		return result, ErrModelVaultBinding
	}
	result.SignedManifest = append([]byte(nil), sealer.product.SignedManifest...)
	result.ManifestDigest = sha256.Sum256(result.SignedManifest)
	defer func() {
		if err == nil {
			return
		}
		for index := range result.Assets {
			if result.Assets[index].Content != nil {
				_ = result.Assets[index].Content.Close()
				result.Assets[index].Content = nil
			}
		}
	}()
	var catalogSequence uint64
	var catalogDigest [sha256.Size]byte
	for index := range request.Artifacts {
		expected := request.Artifacts[index]
		signed := sealer.product.Manifest.Assets[index]
		if expected.Ordinal != signed.Ordinal || expected.AssetID != signed.AssetID ||
			expected.Role != signed.Role || expected.Bytes != signed.PlaintextSize ||
			expected.Digest != signed.Binding.AssetDigest {
			return result, ErrModelVaultBinding
		}
		sealed, sequence, digest, sealErr := sealer.sealAsset(ctx, source, expected, signed)
		if sealErr != nil {
			return result, sealErr
		}
		if index == 0 {
			catalogSequence, catalogDigest = sequence, digest
		} else if catalogSequence != sequence ||
			subtle.ConstantTimeCompare(catalogDigest[:], digest[:]) != 1 {
			_ = sealed.Content.Close()
			return result, ErrModelVaultAuthentication
		}
		result.Assets = append(result.Assets, sealed)
	}
	return result, nil
}

func (sealer *modelVaultProductionSealer) sealAsset(
	ctx context.Context,
	source modelvault.ArtifactSource,
	descriptor modelvault.ArtifactDescriptor,
	signed nativeassettransport.SignedAsset,
) (modelvault.SealedAssetOutput, uint64, [sha256.Size]byte, error) {
	var zeroDigest [sha256.Size]byte
	reader, actual, err := source.OpenArtifact(ctx, descriptor.Ordinal)
	if err != nil || reader == nil || actual != descriptor {
		if reader != nil {
			_ = reader.Close()
		}
		return modelvault.SealedAssetOutput{}, 0, zeroDigest, ErrModelVaultBinding
	}
	defer reader.Close()
	binding, err := bindingForAsset(sealer.product, signed)
	if err != nil {
		return modelvault.SealedAssetOutput{}, 0, zeroDigest, err
	}
	backend, err := sealer.newBack()
	if err != nil {
		return modelvault.SealedAssetOutput{}, 0, zeroDigest,
			errors.Join(ErrModelVaultUnavailable, err)
	}
	defer backend.Close()
	header, sequence, catalogDigest, err := backend.BeginSeal(ctx, binding)
	if err != nil || len(header) == 0 || len(header) > modelVaultHeaderMaxBytes ||
		sequence == 0 || zeroVaultDigest(catalogDigest) {
		zeroBytes(header)
		return modelvault.SealedAssetOutput{}, 0, zeroDigest,
			errors.Join(ErrModelVaultAuthentication, err)
	}
	file, err := anonymousCiphertextFile()
	if err != nil {
		zeroBytes(header)
		return modelvault.SealedAssetOutput{}, 0, zeroDigest, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()
	sealedHash := sha256.New()
	writer := io.MultiWriter(file, sealedHash)
	if _, err = writer.Write(modelVaultStreamMagic[:]); err == nil {
		var headerLength [4]byte
		binary.BigEndian.PutUint32(headerLength[:], uint32(len(header)))
		_, err = writer.Write(headerLength[:])
	}
	if err == nil {
		_, err = writer.Write(header)
	}
	plainHash := sha256.New()
	remaining := descriptor.Bytes
	buffer := make([]byte, modelVaultSealChunkBytes)
	defer zeroBytes(buffer)
	for chunkIndex := uint64(0); err == nil && remaining > 0; chunkIndex++ {
		if contextErr := ctx.Err(); contextErr != nil {
			err = contextErr
			break
		}
		length := uint64(len(buffer))
		if remaining < length {
			length = remaining
		}
		chunk := buffer[:int(length)]
		if _, err = io.ReadFull(reader, chunk); err != nil {
			break
		}
		_, _ = plainHash.Write(chunk)
		final := length == remaining
		sealed, sealErr := backend.SealChunk(ctx, binding, header, chunkIndex, final, chunk)
		zeroBytes(chunk)
		err = sealErr
		if err != nil {
			zeroBytes(sealed)
			break
		}
		if len(sealed) == 0 || len(sealed) > modelVaultMaxChunkBytes+128 {
			zeroBytes(sealed)
			err = ErrModelVaultAuthentication
			break
		}
		var frame [modelVaultFrameBytes]byte
		binary.BigEndian.PutUint32(frame[0:4], uint32(chunkIndex))
		if final {
			frame[4] = 1
		}
		binary.BigEndian.PutUint32(frame[5:9], uint32(len(sealed)))
		if _, err = writer.Write(frame[:]); err == nil {
			_, err = writer.Write(sealed)
		}
		zeroBytes(sealed)
		remaining -= length
	}
	var extra [1]byte
	if err == nil {
		count, readErr := reader.Read(extra[:])
		if count != 0 || !errors.Is(readErr, io.EOF) ||
			subtle.ConstantTimeCompare(plainHash.Sum(nil), descriptor.Digest[:]) != 1 {
			err = ErrModelVaultAuthentication
		}
	}
	zeroBytes(header)
	if err != nil {
		return modelvault.SealedAssetOutput{}, 0, zeroDigest, err
	}
	position, err := file.Seek(0, io.SeekCurrent)
	if err != nil || position <= 0 {
		return modelvault.SealedAssetOutput{}, 0, zeroDigest, ErrModelVaultUnavailable
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return modelvault.SealedAssetOutput{}, 0, zeroDigest, err
	}
	ok = true
	var digest [sha256.Size]byte
	copy(digest[:], sealedHash.Sum(nil))
	return modelvault.SealedAssetOutput{
		Ordinal: descriptor.Ordinal, AssetID: descriptor.AssetID, Role: descriptor.Role,
		Bytes: uint64(position), Digest: digest, Content: file,
	}, sequence, catalogDigest, nil
}

func anonymousCiphertextFile() (*os.File, error) {
	file, err := os.CreateTemp("", ".meadow-model-vault-ciphertext-*")
	if err != nil {
		return nil, err
	}
	name := file.Name()
	if err = file.Chmod(0o600); err == nil {
		err = os.Remove(name)
	}
	if err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return nil, err
	}
	return file, nil
}

// OpenOpaqueSealedAsset returns the only production access mode: an
// authenticated sequential lease backed by Security Broker chunk operations.
func OpenOpaqueSealedAsset(
	ctx context.Context,
	request OpaqueSealedAssetOpenRequest,
) (protectedmodelbridge.SequentialLease, error) {
	return openOpaqueSealedAsset(ctx, request, newPlatformModelVaultBackend)
}

type modelVaultLease struct {
	mu sync.Mutex

	backend modelVaultBackend
	binding modelVaultAssetBinding
	header  []byte
	reader  io.ReadCloser

	expectedSealedBytes  uint64
	expectedSealedDigest [sha256.Size]byte
	sealedHash           hash.Hash
	plainHash            hash.Hash
	sealedRead           uint64
	chunkIndex           uint64
	offset               uint64
	pending              []byte
	pendingOffset        int
	finalSeen            bool
	terminal             bool
	terminalErr          error
	info                 protectedmodelbridge.AssetInfo
}

func openOpaqueSealedAsset(
	ctx context.Context,
	request OpaqueSealedAssetOpenRequest,
	factory modelVaultBackendFactory,
) (protectedmodelbridge.SequentialLease, error) {
	if ctx == nil || ctx.Err() != nil || request.Sealed == nil || request.SealedBytes == 0 ||
		zeroVaultDigest(request.SealedDigest) || factory == nil {
		if request.Sealed != nil {
			_ = request.Sealed.Close()
		}
		return nil, ErrModelVaultBinding
	}
	binding, err := bindingForAsset(request.Product, request.Asset)
	if err != nil {
		_ = request.Sealed.Close()
		return nil, err
	}
	backend, err := factory()
	if err != nil {
		_ = request.Sealed.Close()
		return nil, errors.Join(ErrModelVaultUnavailable, err)
	}
	lease := &modelVaultLease{
		backend: backend, binding: binding, reader: request.Sealed,
		expectedSealedBytes: request.SealedBytes, expectedSealedDigest: request.SealedDigest,
		sealedHash: sha256.New(), plainHash: sha256.New(),
		info: protectedmodelbridge.AssetInfo{
			AccessMode: protectedmodelbridge.AccessSequentialAuthenticated,
			Binding: protectedmodelbridge.ProfileBinding{
				ModelID: binding.modelID, ProfileDigest: binding.profileDigest,
				ProfileRevision: binding.profileRevision, Backend: request.Product.Manifest.Backend,
			},
			PlaintextSize:       binding.plaintextBytes,
			PreferredChunkBytes: modelVaultSealChunkBytes,
			MaxChunkBytes:       modelVaultMaxChunkBytes,
		},
	}
	if err = lease.readHeader(); err != nil {
		_ = lease.finish(err, false)
		return nil, err
	}
	return lease, nil
}

func (lease *modelVaultLease) Info() protectedmodelbridge.AssetInfo { return lease.info }

func (lease *modelVaultLease) ConsumedBytes() uint64 {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.offset
}

func (lease *modelVaultLease) readSealed(target []byte) error {
	if lease.sealedRead+uint64(len(target)) > lease.expectedSealedBytes {
		return ErrModelVaultAuthentication
	}
	if _, err := io.ReadFull(lease.reader, target); err != nil {
		return ErrModelVaultAuthentication
	}
	lease.sealedRead += uint64(len(target))
	_, _ = lease.sealedHash.Write(target)
	return nil
}

func (lease *modelVaultLease) readHeader() error {
	var prefix [12]byte
	if err := lease.readSealed(prefix[:]); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(prefix[:8], modelVaultStreamMagic[:]) != 1 {
		return ErrModelVaultAuthentication
	}
	length := binary.BigEndian.Uint32(prefix[8:12])
	if length == 0 || length > modelVaultHeaderMaxBytes {
		return ErrModelVaultAuthentication
	}
	lease.header = make([]byte, int(length))
	return lease.readSealed(lease.header)
}

func (lease *modelVaultLease) nextPlaintext(ctx context.Context) error {
	if lease.finalSeen {
		return io.EOF
	}
	var frame [modelVaultFrameBytes]byte
	if err := lease.readSealed(frame[:]); err != nil {
		return err
	}
	if uint64(binary.BigEndian.Uint32(frame[0:4])) != lease.chunkIndex || frame[4] > 1 {
		return ErrModelVaultAuthentication
	}
	length := binary.BigEndian.Uint32(frame[5:9])
	if length == 0 || length > modelVaultMaxChunkBytes+128 {
		return ErrModelVaultAuthentication
	}
	sealed := make([]byte, int(length))
	if err := lease.readSealed(sealed); err != nil {
		zeroBytes(sealed)
		return err
	}
	final := frame[4] == 1
	plaintext, err := lease.backend.OpenChunk(
		ctx, lease.binding, lease.header, lease.chunkIndex, final, sealed)
	zeroBytes(sealed)
	if err != nil || len(plaintext) == 0 || len(plaintext) > modelVaultMaxChunkBytes ||
		lease.offset+uint64(len(plaintext)) > lease.binding.plaintextBytes {
		zeroBytes(plaintext)
		return ErrModelVaultAuthentication
	}
	_, _ = lease.plainHash.Write(plaintext)
	lease.pending, lease.pendingOffset = plaintext, 0
	lease.chunkIndex++
	if final {
		lease.finalSeen = true
		if lease.offset+uint64(len(plaintext)) != lease.binding.plaintextBytes {
			return ErrModelVaultAuthentication
		}
	}
	return nil
}

func (lease *modelVaultLease) ConsumeNext(
	ctx context.Context,
	maximum uint32,
	consume protectedmodelbridge.ChunkConsumer,
) (uint32, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if ctx == nil || consume == nil || maximum == 0 || maximum > modelVaultMaxChunkBytes {
		return 0, protectedmodelbridge.ErrInvalidArgument
	}
	if lease.terminal {
		return 0, lease.terminalErr
	}
	if err := ctx.Err(); err != nil {
		_ = lease.finish(err, true)
		return 0, err
	}
	if lease.pendingOffset == len(lease.pending) {
		zeroBytes(lease.pending)
		lease.pending = nil
		lease.pendingOffset = 0
		if lease.finalSeen {
			if err := lease.finish(nil, false); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		if err := lease.nextPlaintext(ctx); err != nil {
			_ = lease.finish(err, false)
			return 0, err
		}
	}
	count := len(lease.pending) - lease.pendingOffset
	if count > int(maximum) {
		count = int(maximum)
	}
	chunk := lease.pending[lease.pendingOffset : lease.pendingOffset+count]
	offset := lease.offset
	if err := consume(ctx, offset, chunk); err != nil {
		zeroBytes(chunk)
		lease.pendingOffset += count
		lease.offset += uint64(count)
		_ = lease.finish(err, true)
		return 0, err
	}
	zeroBytes(chunk)
	lease.pendingOffset += count
	lease.offset += uint64(count)
	return uint32(count), nil
}

func (lease *modelVaultLease) drain() error {
	zeroBytes(lease.pending)
	lease.offset += uint64(len(lease.pending) - lease.pendingOffset)
	lease.pending = nil
	lease.pendingOffset = 0
	for !lease.finalSeen {
		if err := lease.nextPlaintext(context.Background()); err != nil {
			return err
		}
		lease.offset += uint64(len(lease.pending))
		zeroBytes(lease.pending)
		lease.pending = nil
		lease.pendingOffset = 0
	}
	return nil
}

func (lease *modelVaultLease) finish(cause error, drain bool) error {
	if lease.terminal {
		return lease.terminalErr
	}
	if drain {
		if err := lease.drain(); err != nil {
			cause = errors.Join(cause, err)
		}
	}
	if cause == nil && (!lease.finalSeen || lease.offset != lease.binding.plaintextBytes ||
		subtle.ConstantTimeCompare(lease.plainHash.Sum(nil), lease.binding.plaintextDigest[:]) != 1) {
		cause = ErrModelVaultAuthentication
	}
	if cause == nil {
		var extra [1]byte
		count, err := lease.reader.Read(extra[:])
		if count != 0 || !errors.Is(err, io.EOF) || lease.sealedRead != lease.expectedSealedBytes ||
			subtle.ConstantTimeCompare(lease.sealedHash.Sum(nil), lease.expectedSealedDigest[:]) != 1 {
			cause = ErrModelVaultAuthentication
		}
	}
	zeroBytes(lease.pending)
	zeroBytes(lease.header)
	closeErr := lease.reader.Close()
	backendErr := lease.backend.Close()
	if cause == nil {
		cause = errors.Join(closeErr, backendErr)
	}
	lease.terminal, lease.terminalErr = true, cause
	return cause
}

func (lease *modelVaultLease) Cancel() error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.finish(context.Canceled, true)
}

func (lease *modelVaultLease) Close() error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.finish(nil, true)
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// FailClosedSealedModelKeyBroker is the only legacy WithBytes adapter. It
// deliberately cannot acquire a key; production callers must use the opaque
// sealed-asset API above.
type FailClosedSealedModelKeyBroker struct{}

func (FailClosedSealedModelKeyBroker) AcquireSealedModelKey(
	context.Context,
	protectedmodelbridge.SealedModelKeyRequest,
) (protectedmodelbridge.SealedModelKeyLease, error) {
	return nil, ErrRawModelKeyForbidden
}
