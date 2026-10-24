package nativeassettransport

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"hash"
	"strings"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/nativeruntime"
	"github.com/hey-meadow/meadow-code/go/core/protectedmodelbridge"
)

const (
	MaxAssets        = 4096
	MaxAssetIDBytes  = 192
	MaxManifestBytes = 1 << 20
)

type AssetRole uint32

const (
	AssetRoleTokenizer       AssetRole = 1
	AssetRoleTokenizerConfig AssetRole = 2
	AssetRoleModelConfig     AssetRole = 3
	AssetRoleWeight          AssetRole = 4
)

type Binding struct {
	ModelID         string
	ProfileDigest   [sha256.Size]byte
	AssetDigest     [sha256.Size]byte
	Backend         nativeruntime.Backend
	ProfileRevision uint64
}

func (binding Binding) Validate() error {
	if !validIdentifier(binding.ModelID, MaxAssetIDBytes) ||
		binding.Backend == 0 || binding.ProfileRevision == 0 ||
		zeroDigest(binding.ProfileDigest) || zeroDigest(binding.AssetDigest) {
		return ErrBindingRejected
	}
	return nil
}

type SignedAsset struct {
	Ordinal       uint32
	AssetID       string
	Role          AssetRole
	Required      bool
	PlaintextSize uint64
	Binding       Binding
}

type SignedManifest struct {
	ModelID         string
	ProfileDigest   [sha256.Size]byte
	Backend         nativeruntime.Backend
	ProfileRevision uint64
	ManifestDigest  [sha256.Size]byte
	Assets          []SignedAsset
}

type ManifestVerifier interface {
	VerifySignedAssetManifest(context.Context, SignedManifest) error
}

type ManifestVerifierFunc func(context.Context, SignedManifest) error

func (verify ManifestVerifierFunc) VerifySignedAssetManifest(
	ctx context.Context,
	manifest SignedManifest,
) error {
	if verify == nil {
		return ErrBindingRejected
	}
	return verify(ctx, manifest)
}

type VerifiedManifest struct {
	manifest        SignedManifest
	canonicalDigest [sha256.Size]byte
	requiredCount   uint32
}

func VerifySignedManifest(
	ctx context.Context,
	manifest SignedManifest,
	verifier ManifestVerifier,
) (VerifiedManifest, error) {
	if ctx == nil || verifier == nil {
		return VerifiedManifest{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return VerifiedManifest{}, errors.Join(ErrCanceled, err)
	}
	required, err := validateManifest(manifest)
	if err != nil {
		return VerifiedManifest{}, err
	}
	copyForVerification := cloneManifest(manifest)
	if err := verifier.VerifySignedAssetManifest(ctx, copyForVerification); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return VerifiedManifest{}, errors.Join(ErrCanceled, contextErr)
		}
		return VerifiedManifest{}, ErrBindingRejected
	}
	if err := ctx.Err(); err != nil {
		return VerifiedManifest{}, errors.Join(ErrCanceled, err)
	}
	immutable := cloneManifest(manifest)
	return VerifiedManifest{
		manifest:        immutable,
		canonicalDigest: canonicalManifestDigest(immutable),
		requiredCount:   required,
	}, nil
}

func (verified VerifiedManifest) Manifest() SignedManifest {
	return cloneManifest(verified.manifest)
}

func (verified VerifiedManifest) CanonicalDigest() [sha256.Size]byte {
	return verified.canonicalDigest
}

func (verified VerifiedManifest) RequiredAssetCount() uint32 {
	return verified.requiredCount
}

type AssetResolver interface {
	ResolveSignedAsset(context.Context, SignedAsset) (protectedmodelbridge.SequentialLease, error)
}

type AssetResolverFunc func(context.Context, SignedAsset) (protectedmodelbridge.SequentialLease, error)

func (resolve AssetResolverFunc) ResolveSignedAsset(
	ctx context.Context,
	asset SignedAsset,
) (protectedmodelbridge.SequentialLease, error) {
	if resolve == nil {
		return nil, ErrUnavailable
	}
	return resolve(ctx, asset)
}

func validateManifest(manifest SignedManifest) (uint32, error) {
	if !validIdentifier(manifest.ModelID, MaxAssetIDBytes) ||
		manifest.Backend == 0 || manifest.ProfileRevision == 0 ||
		zeroDigest(manifest.ProfileDigest) || zeroDigest(manifest.ManifestDigest) ||
		len(manifest.Assets) == 0 || len(manifest.Assets) > MaxAssets {
		return 0, ErrBindingRejected
	}
	seenIDs := make(map[string]struct{}, len(manifest.Assets))
	seenDigests := make(map[[sha256.Size]byte]struct{}, len(manifest.Assets))
	var required uint32
	var tokenizer bool
	var weight bool
	optionalSeen := false
	for index, asset := range manifest.Assets {
		if asset.Ordinal != uint32(index) ||
			!validIdentifier(asset.AssetID, MaxAssetIDBytes) ||
			asset.Role < AssetRoleTokenizer || asset.Role > AssetRoleWeight ||
			asset.PlaintextSize == 0 || asset.Binding.Validate() != nil ||
			asset.Binding.ModelID != manifest.ModelID ||
			asset.Binding.Backend != manifest.Backend ||
			asset.Binding.ProfileRevision != manifest.ProfileRevision ||
			subtle.ConstantTimeCompare(asset.Binding.ProfileDigest[:], manifest.ProfileDigest[:]) != 1 {
			return 0, ErrBindingRejected
		}
		if _, exists := seenIDs[asset.AssetID]; exists {
			return 0, ErrBindingRejected
		}
		if _, exists := seenDigests[asset.Binding.AssetDigest]; exists {
			return 0, ErrBindingRejected
		}
		seenIDs[asset.AssetID] = struct{}{}
		seenDigests[asset.Binding.AssetDigest] = struct{}{}
		if asset.Required {
			if optionalSeen {
				return 0, ErrBindingRejected
			}
			required++
			tokenizer = tokenizer || asset.Role == AssetRoleTokenizer
			weight = weight || asset.Role == AssetRoleWeight
		} else {
			optionalSeen = true
		}
	}
	if required == 0 || !tokenizer || !weight {
		return 0, ErrIncompleteManifest
	}
	return required, nil
}

func (asset SignedAsset) matchLease(info protectedmodelbridge.AssetInfo) bool {
	leaseBinding := info.Binding
	return info.AccessMode == protectedmodelbridge.AccessSequentialAuthenticated &&
		info.PlaintextSize == asset.PlaintextSize && info.MaxChunkBytes > 0 &&
		info.MaxChunkBytes <= MaxChunkBytes && leaseBinding.Validate() == nil &&
		asset.Binding.ModelID == leaseBinding.ModelID &&
		asset.Binding.Backend == leaseBinding.Backend &&
		asset.Binding.ProfileRevision == leaseBinding.ProfileRevision &&
		subtle.ConstantTimeCompare(asset.Binding.ProfileDigest[:], leaseBinding.ProfileDigest[:]) == 1
}

func cloneManifest(manifest SignedManifest) SignedManifest {
	cloned := manifest
	cloned.Assets = append([]SignedAsset(nil), manifest.Assets...)
	return cloned
}

func canonicalManifestDigest(manifest SignedManifest) [sha256.Size]byte {
	digest := sha256.New()
	writeHashBytes(digest, []byte("meadow-native-asset-manifest-v2\x00"))
	writeHashString(digest, manifest.ModelID)
	writeHashBytes(digest, manifest.ProfileDigest[:])
	writeHashUint32(digest, uint32(manifest.Backend))
	writeHashUint64(digest, manifest.ProfileRevision)
	writeHashBytes(digest, manifest.ManifestDigest[:])
	writeHashUint32(digest, uint32(len(manifest.Assets)))
	for _, asset := range manifest.Assets {
		writeHashUint32(digest, asset.Ordinal)
		writeHashString(digest, asset.AssetID)
		writeHashUint32(digest, uint32(asset.Role))
		if asset.Required {
			writeHashUint32(digest, 1)
		} else {
			writeHashUint32(digest, 0)
		}
		writeHashUint64(digest, asset.PlaintextSize)
		writeHashBytes(digest, asset.Binding.AssetDigest[:])
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

func validIdentifier(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) &&
		value == strings.TrimSpace(value) && strings.IndexByte(value, 0) < 0
}

func writeHashString(digest hash.Hash, value string) {
	writeHashUint32(digest, uint32(len(value)))
	writeHashBytes(digest, []byte(value))
}

func writeHashBytes(digest hash.Hash, value []byte) {
	_, _ = digest.Write(value)
}

func writeHashUint32(digest hash.Hash, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	writeHashBytes(digest, encoded[:])
}

func writeHashUint64(digest hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	writeHashBytes(digest, encoded[:])
}

func zeroDigest(digest [sha256.Size]byte) bool {
	var value byte
	for _, part := range digest {
		value |= part
	}
	return value == 0
}
