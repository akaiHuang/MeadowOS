package protectedbootstrap

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/cmd/meadow-model-seal/sealedasset"
	"github.com/hey-meadow/meadow-code/go/core/keybroker"
	"github.com/hey-meadow/meadow-code/go/core/nativeassettransport"
	"github.com/hey-meadow/meadow-code/go/core/nativeruntime"
	"github.com/hey-meadow/meadow-code/go/core/protectedmodelbridge"
)

var nativeModelLeases = struct {
	sync.Mutex
	active map[*Prepared]*nativeModelLease
}{active: make(map[*Prepared]*nativeModelLease)}

type nativeModelLeaseState uint8

const (
	nativeModelLeaseActive nativeModelLeaseState = iota + 1
	nativeModelLeaseExhausted
	nativeModelLeaseClosed
	nativeModelLeaseCanceled
	nativeModelLeaseFailed
)

type nativeModelLease struct {
	mu          sync.Mutex
	owner       *Prepared
	ctx         context.Context
	cancel      context.CancelFunc
	info        protectedmodelbridge.AssetInfo
	offset      uint64
	state       nativeModelLeaseState
	terminalErr error
	closeErr    error
}

var _ protectedmodelbridge.SequentialLease = (*nativeModelLease)(nil)

// AcquireNativeModelForLoad verifies that the process-local profile capability
// and native lifecycle request name the same signed model before granting the
// one sequential plaintext lease owned by Prepared.
func (p *Prepared) AcquireNativeModelForLoad(
	ctx context.Context,
	binding protectedmodelbridge.VerifiedProfileBinding,
	request *nativeruntime.ModelLoad,
) (protectedmodelbridge.SequentialLease, error) {
	if err := binding.MatchLoad(request); err != nil {
		return nil, err
	}
	return p.AcquireNativeModelAsset(ctx, binding)
}

// AcquireNativeModelForSwitch is the model-switch equivalent of
// AcquireNativeModelForLoad.
func (p *Prepared) AcquireNativeModelForSwitch(
	ctx context.Context,
	binding protectedmodelbridge.VerifiedProfileBinding,
	request *nativeruntime.ModelSwitch,
) (protectedmodelbridge.SequentialLease, error) {
	if err := binding.MatchSwitch(request); err != nil {
		return nil, err
	}
	return p.AcquireNativeModelAsset(ctx, binding)
}

// AcquireNativeModelAsset grants exclusive sequential use of Prepared's model
// reader. After this succeeds callers must not use ReadModel directly. The
// current Prepared contract does not retain ProfileDigest, so only a
// VerifiedProfileBinding minted from the same trusted productsecurity profile
// is accepted; status fields are never used to invent the missing digest.
func (p *Prepared) AcquireNativeModelAsset(
	ctx context.Context,
	verified protectedmodelbridge.VerifiedProfileBinding,
) (protectedmodelbridge.SequentialLease, error) {
	if p == nil || ctx == nil {
		return nil, protectedmodelbridge.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	binding := verified.Binding()
	if err := binding.Validate(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.modelReader == nil {
		return nil, protectedmodelbridge.ErrClosed
	}
	if p.status.Model.CanonicalID != binding.ModelID ||
		p.status.Model.Backend != binding.Backend ||
		p.status.ProfileRevision != binding.ProfileRevision {
		return nil, protectedmodelbridge.ErrBindingRejected
	}
	if p.modelInfo.PlaintextSize == 0 || p.modelInfo.ChunkSize == 0 {
		return nil, protectedmodelbridge.ErrIntegrity
	}

	nativeModelLeases.Lock()
	defer nativeModelLeases.Unlock()
	if _, exists := nativeModelLeases.active[p]; exists {
		return nil, protectedmodelbridge.ErrLeaseActive
	}

	preferred := p.modelInfo.ChunkSize
	if preferred > protectedmodelbridge.MaxChunkBytes {
		preferred = protectedmodelbridge.MaxChunkBytes
	}
	if uint64(preferred) > p.modelInfo.PlaintextSize {
		preferred = uint32(p.modelInfo.PlaintextSize)
	}
	leaseContext, cancel := context.WithCancel(ctx)
	lease := &nativeModelLease{
		owner:  p,
		ctx:    leaseContext,
		cancel: cancel,
		info: protectedmodelbridge.AssetInfo{
			AccessMode:          protectedmodelbridge.AccessSequentialAuthenticated,
			Binding:             binding,
			PlaintextSize:       p.modelInfo.PlaintextSize,
			PreferredChunkBytes: preferred,
			MaxChunkBytes:       protectedmodelbridge.MaxChunkBytes,
		},
		state: nativeModelLeaseActive,
	}
	nativeModelLeases.active[p] = lease
	return lease, nil
}

func (lease *nativeModelLease) Info() protectedmodelbridge.AssetInfo {
	if lease == nil {
		return protectedmodelbridge.AssetInfo{}
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.info
}

func (lease *nativeModelLease) ConsumedBytes() uint64 {
	if lease == nil {
		return 0
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.offset
}

func (lease *nativeModelLease) ConsumeNext(
	ctx context.Context,
	maxBytes uint32,
	consume protectedmodelbridge.ChunkConsumer,
) (uint32, error) {
	if lease == nil {
		return 0, protectedmodelbridge.ErrClosed
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()

	if terminal := lease.readTerminalErrorLocked(); terminal != nil {
		return 0, terminal
	}
	if ctx == nil || consume == nil || maxBytes == 0 ||
		maxBytes > protectedmodelbridge.MaxChunkBytes {
		return 0, lease.failLocked(protectedmodelbridge.ErrInvalidArgument)
	}
	if err := lease.operationContextErrorLocked(ctx); err != nil {
		return 0, lease.cancelLocked(err)
	}
	if lease.offset == lease.info.PlaintextSize {
		lease.state = nativeModelLeaseExhausted
		return 0, io.EOF
	}

	remaining := lease.info.PlaintextSize - lease.offset
	requested := uint64(maxBytes)
	if requested > remaining {
		requested = remaining
	}
	buffer := make([]byte, int(requested))
	read, readErr := lease.owner.ReadModel(buffer)
	if read < 0 || read > len(buffer) {
		clear(buffer)
		return 0, lease.failLocked(protectedmodelbridge.ErrIntegrity)
	}
	nextOffset := lease.offset + uint64(read)
	if nextOffset > lease.info.PlaintextSize {
		clear(buffer)
		return 0, lease.failLocked(protectedmodelbridge.ErrIntegrity)
	}
	if readErr != nil {
		clear(buffer)
		if errors.Is(readErr, io.EOF) && read == 0 && lease.offset == lease.info.PlaintextSize {
			lease.state = nativeModelLeaseExhausted
			return 0, io.EOF
		}
		return 0, lease.failLocked(mapModelReadError(readErr))
	}
	if read == 0 {
		clear(buffer)
		return 0, lease.failLocked(protectedmodelbridge.ErrNoProgress)
	}
	if err := lease.operationContextErrorLocked(ctx); err != nil {
		clear(buffer)
		return 0, lease.cancelLocked(err)
	}
	if err := consumeBorrowedChunk(ctx, consume, lease.offset, buffer, read); err != nil {
		return 0, lease.failLocked(err)
	}

	lease.offset = nextOffset
	if lease.offset == lease.info.PlaintextSize {
		lease.state = nativeModelLeaseExhausted
	}
	return uint32(read), nil
}

func (lease *nativeModelLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	switch lease.state {
	case nativeModelLeaseClosed, nativeModelLeaseCanceled, nativeModelLeaseFailed:
		return lease.closeErr
	default:
		return lease.finishLocked(nativeModelLeaseClosed, nil)
	}
}

func (lease *nativeModelLease) Cancel() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	switch lease.state {
	case nativeModelLeaseClosed, nativeModelLeaseCanceled, nativeModelLeaseFailed:
		return lease.closeErr
	default:
		return lease.finishLocked(nativeModelLeaseCanceled, protectedmodelbridge.ErrCanceled)
	}
}

func (lease *nativeModelLease) readTerminalErrorLocked() error {
	switch lease.state {
	case nativeModelLeaseExhausted:
		return io.EOF
	case nativeModelLeaseClosed:
		return protectedmodelbridge.ErrClosed
	case nativeModelLeaseCanceled:
		return protectedmodelbridge.ErrCanceled
	case nativeModelLeaseFailed:
		if lease.terminalErr != nil {
			return lease.terminalErr
		}
		return protectedmodelbridge.ErrIntegrity
	default:
		return nil
	}
}

func (lease *nativeModelLease) operationContextErrorLocked(ctx context.Context) error {
	if err := lease.ctx.Err(); err != nil {
		return errors.Join(protectedmodelbridge.ErrCanceled, err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(protectedmodelbridge.ErrCanceled, err)
	}
	return nil
}

func (lease *nativeModelLease) cancelLocked(cause error) error {
	closeErr := lease.finishLocked(nativeModelLeaseCanceled, cause)
	if closeErr != nil {
		return errors.Join(cause, closeErr)
	}
	return cause
}

func (lease *nativeModelLease) failLocked(cause error) error {
	closeErr := lease.finishLocked(nativeModelLeaseFailed, cause)
	if closeErr != nil {
		return errors.Join(cause, closeErr)
	}
	return cause
}

func (lease *nativeModelLease) finishLocked(state nativeModelLeaseState, cause error) error {
	lease.state = state
	lease.terminalErr = cause
	if lease.cancel != nil {
		lease.cancel()
		lease.cancel = nil
	}
	owner := lease.owner
	lease.owner = nil
	lease.closeErr = closePreparedNativeModel(owner)
	unregisterNativeModelLease(owner, lease)
	if lease.closeErr != nil {
		lease.state = nativeModelLeaseFailed
		if lease.terminalErr == nil {
			lease.terminalErr = lease.closeErr
		}
	}
	return lease.closeErr
}

func closePreparedNativeModel(prepared *Prepared) error {
	if prepared == nil {
		return nil
	}
	prepared.mu.Lock()
	if prepared.closed {
		done := prepared.closeDone
		prepared.mu.Unlock()
		if done != nil {
			<-done
		}
		prepared.mu.Lock()
		closeErr := prepared.closeErr
		prepared.mu.Unlock()
		if closeErr != nil {
			return protectedmodelbridge.ErrIntegrity
		}
		return nil
	}

	reader := prepared.modelReader
	wrapper := prepared.modelWrapper
	prepared.modelReader = nil
	prepared.modelWrapper = nil
	var readerErr error
	if reader != nil {
		readerErr = reader.Close()
	}
	if wrapper != nil {
		wrapper.Destroy()
	}
	prepared.mu.Unlock()
	if readerErr != nil {
		return protectedmodelbridge.ErrIntegrity
	}
	return nil
}

func unregisterNativeModelLease(prepared *Prepared, lease *nativeModelLease) {
	if prepared == nil {
		return
	}
	nativeModelLeases.Lock()
	if nativeModelLeases.active[prepared] == lease {
		delete(nativeModelLeases.active, prepared)
	}
	nativeModelLeases.Unlock()
}

func consumeBorrowedChunk(
	ctx context.Context,
	consume protectedmodelbridge.ChunkConsumer,
	offset uint64,
	buffer []byte,
	read int,
) (err error) {
	defer func() {
		clear(buffer)
		if recover() != nil {
			err = protectedmodelbridge.ErrConsumer
		}
	}()
	if consumeErr := consume(ctx, offset, buffer[:read]); consumeErr != nil {
		return protectedmodelbridge.ErrConsumer
	}
	return nil
}

func mapModelReadError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errors.Join(protectedmodelbridge.ErrCanceled, err)
	case errors.Is(err, ErrClosed):
		return protectedmodelbridge.ErrClosed
	default:
		return protectedmodelbridge.ErrIntegrity
	}
}

const maxSealingEvidenceBytes = 8 << 20

// ProtectedSealedAssetConfig is a process-local composition capability. The
// root is fixed once here; ResolveSignedAsset never accepts or derives a path.
type ProtectedSealedAssetConfig struct {
	ProtectedRoot string
	Manifest      sealedasset.VerifiedProductManifest
	KeyBroker     protectedmodelbridge.SealedModelKeyBroker
}

type sealedAssetRecord struct {
	sealedBytes  uint64
	sealedDigest [sha256.Size]byte
}

// ProtectedSealedAssetResolver resolves only ordinal filenames beneath one
// symlink-free protected root and one already-verified product manifest.
type ProtectedSealedAssetResolver struct {
	mu sync.Mutex

	root      *os.Root
	manifest  sealedasset.VerifiedProductManifest
	evidence  sealedasset.Evidence
	assets    map[uint32]sealedAssetRecord
	keyBroker protectedmodelbridge.SealedModelKeyBroker
	active    map[*protectedSealedLease]struct{}
	issued    map[uint32]struct{}
	completed map[uint32]struct{}
	closed    bool
	committed bool
}

var _ nativeassettransport.AssetResolver = (*ProtectedSealedAssetResolver)(nil)
var _ io.Closer = (*ProtectedSealedAssetResolver)(nil)

// NewProtectedSealedAssetResolver validates the fixed root and its sealing
// evidence without opening any plaintext asset.
func NewProtectedSealedAssetResolver(
	ctx context.Context,
	config ProtectedSealedAssetConfig,
) (*ProtectedSealedAssetResolver, error) {
	if ctx == nil || config.KeyBroker == nil {
		return nil, protectedmodelbridge.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manifest := config.Manifest.SignedManifest()
	if len(manifest.Assets) == 0 || config.Manifest.NativeManifest().RequiredAssetCount() == 0 {
		return nil, protectedmodelbridge.ErrSealedAssetRejected
	}
	root, err := openProtectedRoot(config.ProtectedRoot)
	if err != nil {
		return nil, protectedmodelbridge.ErrSealedRootRejected
	}
	evidence, err := readSealingEvidence(root)
	if err != nil {
		_ = root.Close()
		return nil, protectedmodelbridge.ErrSealingEvidence
	}
	assets, err := validateSealingEvidence(config.Manifest, evidence)
	if err != nil {
		_ = root.Close()
		return nil, protectedmodelbridge.ErrSealingEvidence
	}
	return &ProtectedSealedAssetResolver{
		root:      root,
		manifest:  config.Manifest,
		evidence:  evidence,
		assets:    assets,
		keyBroker: config.KeyBroker,
		active:    make(map[*protectedSealedLease]struct{}),
		issued:    make(map[uint32]struct{}),
		completed: make(map[uint32]struct{}),
	}, nil
}

// NewProtectedSealedAssetResolver binds the existing product keybroker to the
// sealed-asset KeyProvider contract. Packaging must provision the sealer
// master key under sealedModelKeyResource for this exact manifest.
func (p *Prepared) NewProtectedSealedAssetResolver(
	ctx context.Context,
	protectedRoot string,
	manifest sealedasset.VerifiedProductManifest,
) (*ProtectedSealedAssetResolver, error) {
	if p == nil || ctx == nil {
		return nil, protectedmodelbridge.ErrInvalidArgument
	}
	signed := manifest.SignedManifest()
	p.mu.Lock()
	if p.closed || p.broker == nil ||
		p.status.Model.CanonicalID != signed.ModelID ||
		p.status.Model.Backend != signed.Backend ||
		p.status.ProfileRevision != signed.ProfileRevision {
		p.mu.Unlock()
		return nil, protectedmodelbridge.ErrBindingRejected
	}
	broker := p.broker
	p.mu.Unlock()
	return NewProtectedSealedAssetResolver(ctx, ProtectedSealedAssetConfig{
		ProtectedRoot: protectedRoot,
		Manifest:      manifest,
		KeyBroker:     preparedSealedModelKeyBroker{broker: broker},
	})
}

type preparedSealedModelKeyBroker struct {
	broker *keybroker.Broker
}

func (broker preparedSealedModelKeyBroker) AcquireSealedModelKey(
	ctx context.Context,
	request protectedmodelbridge.SealedModelKeyRequest,
) (protectedmodelbridge.SealedModelKeyLease, error) {
	if ctx == nil || broker.broker == nil || request.Validate() != nil {
		return nil, protectedmodelbridge.ErrSealedKeyUnavailable
	}
	keys := broker.broker.ModelDataKeys()
	if keys == nil {
		return nil, protectedmodelbridge.ErrSealedKeyUnavailable
	}
	lease, err := keys.Current(ctx, sealedModelKeyResource(request))
	if err != nil || lease == nil {
		return nil, protectedmodelbridge.ErrSealedKeyUnavailable
	}
	return lease, nil
}

func sealedModelKeyResource(request protectedmodelbridge.SealedModelKeyRequest) string {
	return "sealed-model-assets:v1:" +
		hex.EncodeToString(request.Binding.ProfileDigest[:]) + ":" +
		strconv.FormatUint(request.Binding.ProfileRevision, 10) + ":" +
		hex.EncodeToString(request.AssetSetDigest[:]) + ":" +
		hex.EncodeToString(request.ManifestDigest[:]) + ":" +
		hex.EncodeToString(request.CanonicalManifestDigest[:])
}

func (resolver *ProtectedSealedAssetResolver) ResolveSignedAsset(
	ctx context.Context,
	asset nativeassettransport.SignedAsset,
) (protectedmodelbridge.SequentialLease, error) {
	if resolver == nil || ctx == nil {
		return nil, protectedmodelbridge.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.closed || resolver.root == nil {
		return nil, protectedmodelbridge.ErrClosed
	}
	manifest := resolver.manifest.SignedManifest()
	if int(asset.Ordinal) >= len(manifest.Assets) ||
		!signedAssetEqual(asset, manifest.Assets[asset.Ordinal]) {
		return nil, protectedmodelbridge.ErrSealedAssetRejected
	}
	if _, exists := resolver.issued[asset.Ordinal]; exists {
		return nil, protectedmodelbridge.ErrSealedAssetReplay
	}
	record, exists := resolver.assets[asset.Ordinal]
	if !exists {
		return nil, protectedmodelbridge.ErrSealingEvidence
	}
	file, info, err := openDirectProtectedFile(
		resolver.root, sealedasset.SealedAssetFilename(asset.Ordinal))
	if err != nil || uint64(info.Size()) != record.sealedBytes {
		if file != nil {
			_ = file.Close()
		}
		return nil, protectedmodelbridge.ErrSealedAssetRejected
	}
	reader := &sealedDigestReader{
		file:           file,
		hash:           sha256.New(),
		expectedBytes:  record.sealedBytes,
		expectedDigest: record.sealedDigest,
	}
	request := sealedModelKeyRequest(resolver.manifest, asset)
	lease, err := sealedasset.OpenLease(ctx, reader, resolver.manifest, asset.Ordinal,
		sealedKeyProvider{broker: resolver.keyBroker, request: request})
	if err != nil {
		if errors.Is(err, sealedasset.ErrKeyUnavailable) {
			return nil, protectedmodelbridge.ErrSealedKeyUnavailable
		}
		return nil, protectedmodelbridge.ErrSealedAssetRejected
	}
	wrapped := &protectedSealedLease{
		inner:   lease,
		owner:   resolver,
		ordinal: asset.Ordinal,
		size:    asset.PlaintextSize,
	}
	resolver.active[wrapped] = struct{}{}
	resolver.issued[asset.Ordinal] = struct{}{}
	return wrapped, nil
}

func sealedModelKeyRequest(
	manifest sealedasset.VerifiedProductManifest,
	asset nativeassettransport.SignedAsset,
) protectedmodelbridge.SealedModelKeyRequest {
	return protectedmodelbridge.SealedModelKeyRequest{
		Binding: protectedmodelbridge.ProfileBinding{
			ModelID:         asset.Binding.ModelID,
			ProfileDigest:   asset.Binding.ProfileDigest,
			ProfileRevision: asset.Binding.ProfileRevision,
			Backend:         asset.Binding.Backend,
		},
		ManifestDigest:          manifest.ManifestDigest(),
		AssetSetDigest:          manifest.AssetSetDigest(),
		CanonicalManifestDigest: manifest.CanonicalDigest(),
		AssetDigest:             asset.Binding.AssetDigest,
		Ordinal:                 asset.Ordinal,
		AssetID:                 asset.AssetID,
		Role:                    uint32(asset.Role),
		PlaintextSize:           asset.PlaintextSize,
	}
}

type sealedKeyProvider struct {
	broker  protectedmodelbridge.SealedModelKeyBroker
	request protectedmodelbridge.SealedModelKeyRequest
}

func (provider sealedKeyProvider) MasterKey(ctx context.Context) ([]byte, error) {
	if ctx == nil || provider.broker == nil || provider.request.Validate() != nil {
		return nil, sealedasset.ErrKeyUnavailable
	}
	lease, err := provider.broker.AcquireSealedModelKey(ctx, provider.request)
	if err != nil || lease == nil {
		return nil, sealedasset.ErrKeyUnavailable
	}
	defer lease.Destroy()
	var key []byte
	err = lease.WithBytes(func(value []byte) error {
		if len(value) != sealedasset.MasterKeyBytes {
			return sealedasset.ErrKeyUnavailable
		}
		key = make([]byte, len(value))
		copy(key, value)
		return nil
	})
	if err != nil || len(key) != sealedasset.MasterKeyBytes {
		clear(key)
		return nil, sealedasset.ErrKeyUnavailable
	}
	return key, nil
}

type sealedDigestReader struct {
	mu sync.Mutex

	file           *os.File
	hash           io.Writer
	sum            interface{ Sum([]byte) []byte }
	expectedBytes  uint64
	expectedDigest [sha256.Size]byte
	readBytes      uint64
	closed         bool
	closeErr       error
}

func (reader *sealedDigestReader) Read(destination []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed || reader.file == nil {
		return 0, os.ErrClosed
	}
	read, err := reader.file.Read(destination)
	if read > 0 {
		_, _ = reader.hash.Write(destination[:read])
		reader.readBytes += uint64(read)
	}
	return read, err
}

func (reader *sealedDigestReader) Close() error {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return reader.closeErr
	}
	reader.closed = true
	var actual []byte
	if hash, ok := reader.hash.(interface{ Sum([]byte) []byte }); ok {
		actual = hash.Sum(nil)
	}
	valid := reader.readBytes == reader.expectedBytes && len(actual) == sha256.Size &&
		subtle.ConstantTimeCompare(actual, reader.expectedDigest[:]) == 1
	clear(actual)
	if reader.file != nil {
		reader.closeErr = reader.file.Close()
		reader.file = nil
	}
	if !valid {
		reader.closeErr = errors.Join(protectedmodelbridge.ErrSealingEvidence, reader.closeErr)
	}
	return reader.closeErr
}

type protectedSealedLease struct {
	once sync.Once

	inner   protectedmodelbridge.SequentialLease
	owner   *ProtectedSealedAssetResolver
	ordinal uint32
	size    uint64
}

func (lease *protectedSealedLease) Info() protectedmodelbridge.AssetInfo {
	if lease == nil || lease.inner == nil {
		return protectedmodelbridge.AssetInfo{}
	}
	return lease.inner.Info()
}

func (lease *protectedSealedLease) ConsumedBytes() uint64 {
	if lease == nil || lease.inner == nil {
		return 0
	}
	return lease.inner.ConsumedBytes()
}

func (lease *protectedSealedLease) ConsumeNext(
	ctx context.Context,
	maximum uint32,
	consumer protectedmodelbridge.ChunkConsumer,
) (uint32, error) {
	if lease == nil || lease.inner == nil {
		return 0, protectedmodelbridge.ErrClosed
	}
	return lease.inner.ConsumeNext(ctx, maximum, consumer)
}

func (lease *protectedSealedLease) Close() error {
	if lease == nil || lease.inner == nil {
		return nil
	}
	err := lease.inner.Close()
	complete := err == nil && lease.inner.ConsumedBytes() == lease.size
	lease.finish(complete)
	return err
}

func (lease *protectedSealedLease) Cancel() error {
	if lease == nil || lease.inner == nil {
		return nil
	}
	err := lease.inner.Cancel()
	lease.finish(false)
	return err
}

func (lease *protectedSealedLease) finish(complete bool) {
	lease.once.Do(func() {
		if lease.owner != nil {
			lease.owner.finishLease(lease, complete)
		}
	})
}

func (resolver *ProtectedSealedAssetResolver) finishLease(
	lease *protectedSealedLease,
	complete bool,
) {
	resolver.mu.Lock()
	delete(resolver.active, lease)
	if complete {
		resolver.completed[lease.ordinal] = struct{}{}
	}
	resolver.mu.Unlock()
}

// ProtectedModelReady is minted only after authenticated transport terminal
// ACK evidence matches both the signed manifest and sealing evidence.
type ProtectedModelReady struct {
	binding         protectedmodelbridge.ProfileBinding
	manifestDigest  [sha256.Size]byte
	assetSetDigest  [sha256.Size]byte
	aggregateDigest [sha256.Size]byte
}

func (ready ProtectedModelReady) Binding() protectedmodelbridge.ProfileBinding {
	return ready.binding
}

func (ready ProtectedModelReady) ManifestDigest() [sha256.Size]byte {
	return ready.manifestDigest
}

func (ready ProtectedModelReady) AssetSetDigest() [sha256.Size]byte {
	return ready.assetSetDigest
}

func (ready ProtectedModelReady) AggregateDigest() [sha256.Size]byte {
	return ready.aggregateDigest
}

// CommitModelReady accepts only evidence exposed by Parent after aggregate
// terminal ACK. Calling it before Parent reaches ready fails closed.
func (resolver *ProtectedSealedAssetResolver) CommitModelReady(
	parent *nativeassettransport.Parent,
) (ProtectedModelReady, error) {
	if resolver == nil || parent == nil {
		return ProtectedModelReady{}, protectedmodelbridge.ErrInvalidArgument
	}
	terminal, ready := parent.Evidence()
	if !ready {
		return ProtectedModelReady{}, protectedmodelbridge.ErrModelNotReady
	}
	return resolver.commitTerminalEvidence(terminal)
}

func (resolver *ProtectedSealedAssetResolver) commitTerminalEvidence(
	terminal nativeassettransport.AggregateTerminalEvidence,
) (ProtectedModelReady, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.closed || resolver.committed || len(resolver.active) != 0 {
		return ProtectedModelReady{}, protectedmodelbridge.ErrModelNotReady
	}
	required := resolver.manifest.NativeManifest().RequiredAssetCount()
	if uint32(len(resolver.completed)) != required {
		return ProtectedModelReady{}, protectedmodelbridge.ErrModelNotReady
	}
	for ordinal := uint32(0); ordinal < required; ordinal++ {
		if _, complete := resolver.completed[ordinal]; !complete {
			return ProtectedModelReady{}, protectedmodelbridge.ErrModelNotReady
		}
	}
	if err := validateTerminalEvidence(resolver.manifest, terminal); err != nil {
		return ProtectedModelReady{}, err
	}
	manifest := resolver.manifest.SignedManifest()
	resolver.committed = true
	return ProtectedModelReady{
		binding: protectedmodelbridge.ProfileBinding{
			ModelID:         manifest.ModelID,
			ProfileDigest:   manifest.ProfileDigest,
			ProfileRevision: manifest.ProfileRevision,
			Backend:         manifest.Backend,
		},
		manifestDigest:  resolver.manifest.ManifestDigest(),
		assetSetDigest:  resolver.manifest.AssetSetDigest(),
		aggregateDigest: terminal.AggregateDigest,
	}, nil
}

func validateTerminalEvidence(
	verified sealedasset.VerifiedProductManifest,
	terminal nativeassettransport.AggregateTerminalEvidence,
) error {
	manifest := verified.SignedManifest()
	required := verified.NativeManifest().RequiredAssetCount()
	if required == 0 || terminal.RequiredAssets != required ||
		terminal.CompletedAssets != required || uint32(len(terminal.Assets)) != required ||
		!digestEqual(terminal.ManifestDigest, verified.ManifestDigest()) ||
		!digestEqual(terminal.CanonicalDigest, verified.CanonicalDigest()) ||
		zeroBytes(terminal.SessionID[:]) || zeroBytes(terminal.AggregateDigest[:]) {
		return protectedmodelbridge.ErrModelNotReady
	}
	var total uint64
	for ordinal := uint32(0); ordinal < required; ordinal++ {
		expected := manifest.Assets[ordinal]
		actual := terminal.Assets[ordinal]
		if actual.Ordinal != expected.Ordinal || actual.AssetID != expected.AssetID ||
			actual.Role != expected.Role || actual.PlaintextBytes != expected.PlaintextSize ||
			actual.ChunkCount == 0 || actual.ChunkCount > actual.PlaintextBytes ||
			!digestEqual(actual.AssetDigest, expected.Binding.AssetDigest) ||
			!digestEqual(actual.StreamDigest, expected.Binding.AssetDigest) ||
			zeroBytes(actual.EvidenceDigest[:]) || ^uint64(0)-total < actual.PlaintextBytes {
			return protectedmodelbridge.ErrModelNotReady
		}
		total += actual.PlaintextBytes
	}
	if total != terminal.TotalPlaintext {
		return protectedmodelbridge.ErrModelNotReady
	}
	return nil
}

func (resolver *ProtectedSealedAssetResolver) Close() error {
	if resolver == nil {
		return nil
	}
	resolver.mu.Lock()
	if resolver.closed {
		resolver.mu.Unlock()
		return nil
	}
	resolver.closed = true
	root := resolver.root
	resolver.root = nil
	active := make([]*protectedSealedLease, 0, len(resolver.active))
	for lease := range resolver.active {
		active = append(active, lease)
	}
	resolver.mu.Unlock()
	var closeErr error
	for _, lease := range active {
		closeErr = errors.Join(closeErr, lease.Cancel())
	}
	if root != nil {
		closeErr = errors.Join(closeErr, root.Close())
	}
	return closeErr
}

func openProtectedRoot(rootPath string) (*os.Root, error) {
	if rootPath == "" || strings.IndexByte(rootPath, 0) >= 0 ||
		!filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return nil, protectedmodelbridge.ErrSealedRootRejected
	}
	info, err := os.Lstat(rootPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		info.Mode().Perm()&0o077 != 0 {
		return nil, protectedmodelbridge.ErrSealedRootRejected
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, protectedmodelbridge.ErrSealedRootRejected
	}
	return root, nil
}

func openDirectProtectedFile(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	if root == nil || name == "" || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\\`) || strings.IndexByte(name, 0) >= 0 {
		return nil, nil, protectedmodelbridge.ErrSealedAssetRejected
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Mode().Perm()&0o077 != 0 {
		return nil, nil, protectedmodelbridge.ErrSealedAssetRejected
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, nil, protectedmodelbridge.ErrSealedAssetRejected
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, nil, protectedmodelbridge.ErrSealedAssetRejected
	}
	return file, after, nil
}

func readSealingEvidence(root *os.Root) (sealedasset.Evidence, error) {
	file, info, err := openDirectProtectedFile(root, sealedasset.EvidenceFilename)
	if err != nil || info.Size() > maxSealingEvidenceBytes {
		if file != nil {
			_ = file.Close()
		}
		return sealedasset.Evidence{}, protectedmodelbridge.ErrSealingEvidence
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxSealingEvidenceBytes+1))
	decoder.DisallowUnknownFields()
	var evidence sealedasset.Evidence
	if err := decoder.Decode(&evidence); err != nil {
		return sealedasset.Evidence{}, protectedmodelbridge.ErrSealingEvidence
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return sealedasset.Evidence{}, protectedmodelbridge.ErrSealingEvidence
	}
	return evidence, nil
}

func validateSealingEvidence(
	verified sealedasset.VerifiedProductManifest,
	evidence sealedasset.Evidence,
) (map[uint32]sealedAssetRecord, error) {
	manifest := verified.SignedManifest()
	assetSetDigest := verified.AssetSetDigest()
	manifestDigest := verified.ManifestDigest()
	canonicalDigest := verified.CanonicalDigest()
	if evidence.SchemaVersion != sealedasset.EvidenceSchemaVersion ||
		evidence.FormatVersion != sealedasset.FormatVersion ||
		evidence.ModelID != manifest.ModelID ||
		evidence.ProfileRevision != manifest.ProfileRevision ||
		evidence.Backend != uint32(manifest.Backend) ||
		evidence.ProfileDigest != hex.EncodeToString(manifest.ProfileDigest[:]) ||
		evidence.AssetSetDigest != hex.EncodeToString(assetSetDigest[:]) ||
		evidence.ManifestDigest != hex.EncodeToString(manifestDigest[:]) ||
		evidence.CanonicalManifestDigest != hex.EncodeToString(canonicalDigest[:]) ||
		len(evidence.Assets) != len(manifest.Assets) || !validNonzeroHexDigest(evidence.EvidenceDigest) {
		return nil, protectedmodelbridge.ErrSealingEvidence
	}
	assets := make(map[uint32]sealedAssetRecord, len(manifest.Assets))
	for index, expected := range manifest.Assets {
		actual := evidence.Assets[index]
		sealedDigest, ok := parseHexDigest(actual.SealedSHA256)
		if !ok || actual.Ordinal != expected.Ordinal || actual.AssetID != expected.AssetID ||
			actual.Role != signedAssetRoleName(expected.Role) || actual.Required != expected.Required ||
			actual.PlaintextBytes != expected.PlaintextSize ||
			actual.PlaintextSHA256 != hex.EncodeToString(expected.Binding.AssetDigest[:]) ||
			actual.ChunkBytes == 0 || actual.ChunkBytes > nativeassettransport.MaxChunkBytes ||
			actual.ChunkCount != expectedChunkCount(actual.PlaintextBytes, actual.ChunkBytes) ||
			actual.SealedBytes == 0 {
			return nil, protectedmodelbridge.ErrSealingEvidence
		}
		assets[expected.Ordinal] = sealedAssetRecord{
			sealedBytes:  actual.SealedBytes,
			sealedDigest: sealedDigest,
		}
	}
	return assets, nil
}

func expectedChunkCount(size uint64, chunkBytes uint32) uint64 {
	if size == 0 || chunkBytes == 0 {
		return 0
	}
	return (size + uint64(chunkBytes) - 1) / uint64(chunkBytes)
}

func signedAssetRoleName(role nativeassettransport.AssetRole) string {
	switch role {
	case nativeassettransport.AssetRoleTokenizer:
		return "tokenizer"
	case nativeassettransport.AssetRoleTokenizerConfig:
		return "tokenizer_config"
	case nativeassettransport.AssetRoleModelConfig:
		return "model_config"
	case nativeassettransport.AssetRoleWeight:
		return "weight"
	default:
		return ""
	}
}

func signedAssetEqual(left, right nativeassettransport.SignedAsset) bool {
	return left.Ordinal == right.Ordinal && left.AssetID == right.AssetID &&
		left.Role == right.Role && left.Required == right.Required &&
		left.PlaintextSize == right.PlaintextSize &&
		left.Binding.ModelID == right.Binding.ModelID &&
		left.Binding.Backend == right.Binding.Backend &&
		left.Binding.ProfileRevision == right.Binding.ProfileRevision &&
		digestEqual(left.Binding.ProfileDigest, right.Binding.ProfileDigest) &&
		digestEqual(left.Binding.AssetDigest, right.Binding.AssetDigest)
}

func digestEqual(left, right [sha256.Size]byte) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func zeroBytes(value []byte) bool {
	var combined byte
	for _, item := range value {
		combined |= item
	}
	return combined == 0
}

func validNonzeroHexDigest(value string) bool {
	digest, valid := parseHexDigest(value)
	return valid && !zeroBytes(digest[:])
}

func parseHexDigest(value string) ([sha256.Size]byte, bool) {
	var digest [sha256.Size]byte
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return digest, false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		clear(decoded)
		return digest, false
	}
	copy(digest[:], decoded)
	clear(decoded)
	return digest, true
}
