// Package protectedmodelbridge defines the process-local contract between the
// protected bootstrap and a native model loader. It intentionally contains no
// filesystem path, URL, socket address, or serializable plaintext handle.
package protectedmodelbridge

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/nativeruntime"
)

// MaxChunkBytes is the largest plaintext view the bridge may lend to a native
// consumer in one call. A consumer needing random access must add an
// authenticated, bounded ReadAt primitive below modelprotection; buffering the
// complete plaintext asset is not a legal substitute.
const MaxChunkBytes uint32 = 4 << 20

var (
	ErrInvalidArgument = errors.New("protected model bridge: invalid argument")
	ErrBindingRejected = errors.New("protected model bridge: profile binding rejected")
	ErrLeaseActive     = errors.New("protected model bridge: model lease already active")
	ErrClosed          = errors.New("protected model bridge: lease closed")
	ErrCanceled        = errors.New("protected model bridge: lease canceled")
	ErrIntegrity       = errors.New("protected model bridge: model integrity failure")
	ErrConsumer        = errors.New("protected model bridge: native consumer failed")
	ErrNoProgress      = errors.New("protected model bridge: model source made no progress")
	ErrNativeAssetConsumerUnavailable = errors.New("protected model bridge: native asset consumer is unavailable")
	ErrNativeAssetSwitchUnavailable   = errors.New("protected model bridge: no verified asset lease is available for model switch")
)

type AccessMode uint8

const (
	// AccessSequentialAuthenticated means offsets are monotonic and each chunk
	// is exposed only after modelprotection authenticates it. ReadAt is not
	// available in the current modelprotection contract.
	AccessSequentialAuthenticated AccessMode = iota + 1
)

// ProfileBinding is the signed-profile identity associated with one native
// model lifecycle request. ProfileDigest is the signed profile digest, not a
// digest invented from status fields or plaintext produced by this bridge.
type ProfileBinding struct {
	ModelID         string
	ProfileDigest   [sha256.Size]byte
	ProfileRevision uint64
	Backend         nativeruntime.Backend
}

func (binding ProfileBinding) Validate() error {
	if binding.ModelID == "" || len(binding.ModelID) > 192 ||
		binding.ModelID != strings.TrimSpace(binding.ModelID) ||
		strings.IndexByte(binding.ModelID, 0) >= 0 ||
		binding.ProfileRevision == 0 || binding.Backend == 0 ||
		isZeroDigest(binding.ProfileDigest) {
		return ErrBindingRejected
	}
	return nil
}

// ProfileBindingVerifier is the minimum seam needed because Prepared retains
// profile revision and status evidence but not the signed profile digest. An
// implementation must verify binding against the same trusted
// productsecurity profile selected for Prepare. Returning nil based only on a
// native request or caller-provided digest violates this contract.
type ProfileBindingVerifier interface {
	VerifyProfileBinding(context.Context, ProfileBinding) error
}

type ProfileBindingVerifierFunc func(context.Context, ProfileBinding) error

func (verify ProfileBindingVerifierFunc) VerifyProfileBinding(ctx context.Context, binding ProfileBinding) error {
	if verify == nil {
		return ErrBindingRejected
	}
	return verify(ctx, binding)
}

// VerifiedProfileBinding is a process-local capability. Its zero value is
// invalid, and its fields can only be populated through VerifyProfileBinding.
type VerifiedProfileBinding struct {
	binding ProfileBinding
}

func VerifyProfileBinding(
	ctx context.Context,
	binding ProfileBinding,
	verifier ProfileBindingVerifier,
) (VerifiedProfileBinding, error) {
	if ctx == nil || verifier == nil {
		return VerifiedProfileBinding{}, ErrInvalidArgument
	}
	if err := binding.Validate(); err != nil {
		return VerifiedProfileBinding{}, err
	}
	if err := ctx.Err(); err != nil {
		return VerifiedProfileBinding{}, err
	}
	if err := verifier.VerifyProfileBinding(ctx, binding); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return VerifiedProfileBinding{}, contextErr
		}
		return VerifiedProfileBinding{}, ErrBindingRejected
	}
	if err := ctx.Err(); err != nil {
		return VerifiedProfileBinding{}, err
	}
	return VerifiedProfileBinding{binding: binding}, nil
}

func (verified VerifiedProfileBinding) Binding() ProfileBinding {
	return verified.binding
}

// MatchLoad binds an asset capability to exactly one validated native load
// identity. MemoryLimitBytes remains a native policy value, but must be
// non-zero before an asset lease is granted.
func (verified VerifiedProfileBinding) MatchLoad(request *nativeruntime.ModelLoad) error {
	if request == nil || request.MemoryLimitBytes == 0 {
		return ErrBindingRejected
	}
	return verified.matchNative(request.ModelID, request.ProfileDigest, request.Backend)
}

// MatchSwitch applies the same binding to a model switch and also rejects an
// unbound generation.
func (verified VerifiedProfileBinding) MatchSwitch(request *nativeruntime.ModelSwitch) error {
	if request == nil || request.MemoryLimitBytes == 0 || request.ExpectedGeneration == 0 {
		return ErrBindingRejected
	}
	return verified.matchNative(request.ModelID, request.ProfileDigest, request.Backend)
}

func (verified VerifiedProfileBinding) matchNative(
	modelID string,
	digest [sha256.Size]byte,
	backend nativeruntime.Backend,
) error {
	binding := verified.binding
	if err := binding.Validate(); err != nil ||
		binding.ModelID != modelID || binding.Backend != backend ||
		subtle.ConstantTimeCompare(binding.ProfileDigest[:], digest[:]) != 1 {
		return ErrBindingRejected
	}
	return nil
}

type AssetInfo struct {
	AccessMode          AccessMode
	Binding             ProfileBinding
	PlaintextSize       uint64
	PreferredChunkBytes uint32
	MaxChunkBytes       uint32
}

// ChunkConsumer receives a borrowed plaintext view at the supplied monotonic
// offset. The view is zeroed before ConsumeNext returns and must not be retained
// or used asynchronously. A native loader that retains bytes must copy them
// synchronously into memory whose lifetime and zeroization it owns.
type ChunkConsumer func(context.Context, uint64, []byte) error

// SequentialLease is the only currently legal model access mode. Close drains
// and authenticates unread encrypted chunks before releasing model secrets;
// Cancel revokes future callbacks and performs the same fail-closed release.
type SequentialLease interface {
	Info() AssetInfo
	ConsumedBytes() uint64
	ConsumeNext(context.Context, uint32, ChunkConsumer) (uint32, error)
	Cancel() error
	io.Closer
}

// NativeAssetRuntime is the exact entry point a protected in-process native
// loader must provide. Load and Switch must synchronously consume the supplied
// lease before reporting ModelReady. They must never derive a path, URL or
// complete Go plaintext buffer from it. On success the implementation owns all
// native copies and their later zeroization; on error the caller cancels the
// lease. Cancel must stop further callbacks before returning, and Close must be
// idempotent.
type NativeAssetRuntime interface {
	Load(context.Context, nativeruntime.ModelLoad, SequentialLease) (*nativeruntime.ModelStatePayload, error)
	Switch(context.Context, nativeruntime.ModelSwitch, SequentialLease) (*nativeruntime.ModelStatePayload, error)
	Cancel(context.Context) error
	Close() error
}

func isZeroDigest(digest [sha256.Size]byte) bool {
	var nonzero byte
	for _, value := range digest {
		nonzero |= value
	}
	return nonzero == 0
}
