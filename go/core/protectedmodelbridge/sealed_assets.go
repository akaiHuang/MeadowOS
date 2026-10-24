package protectedmodelbridge

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
)

const SealedModelMasterKeyBytes = 32

var (
	ErrSealedRootRejected   = errors.New("protected model bridge: protected root rejected")
	ErrSealingEvidence      = errors.New("protected model bridge: sealing evidence rejected")
	ErrSealedAssetRejected  = errors.New("protected model bridge: sealed asset rejected")
	ErrSealedAssetReplay    = errors.New("protected model bridge: sealed asset replay")
	ErrSealedKeyUnavailable = errors.New("protected model bridge: sealed model key unavailable")
	ErrModelNotReady        = errors.New("protected model bridge: terminal model evidence unavailable")
)

// SealedModelKeyRequest binds a product key lease to one verified manifest and
// one signed asset. The broker may use the manifest-level fields to select the
// shared master key; per-asset key separation is performed by the sealer.
type SealedModelKeyRequest struct {
	Binding                 ProfileBinding
	ManifestDigest          [sha256.Size]byte
	AssetSetDigest          [sha256.Size]byte
	CanonicalManifestDigest [sha256.Size]byte
	AssetDigest             [sha256.Size]byte
	Ordinal                 uint32
	AssetID                 string
	Role                    uint32
	PlaintextSize           uint64
}

func (request SealedModelKeyRequest) Validate() error {
	if err := request.Binding.Validate(); err != nil ||
		isZeroDigest(request.ManifestDigest) ||
		isZeroDigest(request.AssetSetDigest) ||
		isZeroDigest(request.CanonicalManifestDigest) ||
		isZeroDigest(request.AssetDigest) ||
		request.AssetID == "" || len(request.AssetID) > 192 ||
		request.AssetID != strings.TrimSpace(request.AssetID) ||
		strings.IndexByte(request.AssetID, 0) >= 0 ||
		request.Role == 0 || request.PlaintextSize == 0 {
		return ErrSealedAssetRejected
	}
	return nil
}

// SealedModelKeyLease is satisfied by keybroker.Lease. WithBytes lends a
// temporary view; Destroy must erase the retained broker copy.
type SealedModelKeyLease interface {
	WithBytes(func([]byte) error) error
	Destroy()
}

// SealedModelKeyBroker resolves a process-local product key capability. It
// must not derive secrets from environment variables, paths, or caller input.
type SealedModelKeyBroker interface {
	AcquireSealedModelKey(context.Context, SealedModelKeyRequest) (SealedModelKeyLease, error)
}

type SealedModelKeyBrokerFunc func(context.Context, SealedModelKeyRequest) (SealedModelKeyLease, error)

func (acquire SealedModelKeyBrokerFunc) AcquireSealedModelKey(
	ctx context.Context,
	request SealedModelKeyRequest,
) (SealedModelKeyLease, error) {
	if acquire == nil {
		return nil, ErrSealedKeyUnavailable
	}
	return acquire(ctx, request)
}
