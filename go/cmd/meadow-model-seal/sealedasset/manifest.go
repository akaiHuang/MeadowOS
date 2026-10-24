package sealedasset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/hey-meadow/meadow-code/go/core/nativeassettransport"
	"github.com/hey-meadow/meadow-code/go/core/nativeruntime"
)

func newSliceReader(payload []byte) *bytes.Reader { return bytes.NewReader(payload) }

func LoadVerifiedManifestFiles(
	ctx context.Context,
	manifestPath, signaturePath string,
	verifier ManifestSignatureVerifier,
) (VerifiedProductManifest, error) {
	if ctx == nil || manifestPath == "" || signaturePath == "" || verifier == nil {
		return VerifiedProductManifest{}, ErrInvalidArgument
	}
	payload, err := readBoundedRegularFile(manifestPath, nativeassettransport.MaxManifestBytes)
	if err != nil {
		return VerifiedProductManifest{}, ErrManifestRejected
	}
	signature, err := readBoundedRegularFile(signaturePath, maxSignatureBytes)
	if err != nil {
		zero(payload)
		return VerifiedProductManifest{}, ErrManifestRejected
	}
	defer zero(payload)
	defer zero(signature)
	return VerifyManifestPayload(ctx, payload, signature, verifier)
}

func VerifyManifestPayload(
	ctx context.Context,
	payload, signature []byte,
	verifier ManifestSignatureVerifier,
) (VerifiedProductManifest, error) {
	if ctx == nil || verifier == nil || len(payload) == 0 || len(payload) > nativeassettransport.MaxManifestBytes {
		return VerifiedProductManifest{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return VerifiedProductManifest{}, errors.Join(ErrManifestRejected, err)
	}
	if err := verifier.VerifyManifestSignature(payload, signature); err != nil {
		return VerifiedProductManifest{}, ErrManifestRejected
	}
	var document ProductAssetManifestDocument
	if err := decodeStrictJSON(payload, &document); err != nil || document.SchemaVersion != ManifestSchemaVersion {
		return VerifiedProductManifest{}, ErrManifestRejected
	}
	profileDigest, err := canonicalDigest(document.ProfileDigest)
	if err != nil {
		return VerifiedProductManifest{}, ErrManifestRejected
	}
	assetSetDigest, err := canonicalDigest(document.AssetSetDigest)
	if err != nil {
		return VerifiedProductManifest{}, ErrManifestRejected
	}
	if document.Backend != uint32(nativeruntime.BackendSGLang) &&
		document.Backend != uint32(nativeruntime.BackendMTPLX) {
		return VerifiedProductManifest{}, ErrManifestRejected
	}
	manifestDigest := sha256.Sum256(payload)
	signed := nativeassettransport.SignedManifest{
		ModelID:         document.ModelID,
		ProfileDigest:   profileDigest,
		Backend:         nativeruntime.Backend(document.Backend),
		ProfileRevision: document.ProfileRevision,
		ManifestDigest:  manifestDigest,
		Assets:          make([]nativeassettransport.SignedAsset, 0, len(document.Assets)),
	}
	for _, input := range document.Assets {
		digest, digestErr := canonicalDigest(input.PlaintextSHA256)
		role, roleErr := parseRole(input.Role)
		if digestErr != nil || roleErr != nil {
			return VerifiedProductManifest{}, ErrManifestRejected
		}
		signed.Assets = append(signed.Assets, nativeassettransport.SignedAsset{
			Ordinal:       input.Ordinal,
			AssetID:       input.AssetID,
			Role:          role,
			Required:      input.Required,
			PlaintextSize: input.PlaintextSize,
			Binding: nativeassettransport.Binding{
				ModelID:         document.ModelID,
				ProfileDigest:   profileDigest,
				AssetDigest:     digest,
				Backend:         nativeruntime.Backend(document.Backend),
				ProfileRevision: document.ProfileRevision,
			},
		})
	}
	verified, err := nativeassettransport.VerifySignedManifest(
		ctx,
		signed,
		nativeassettransport.ManifestVerifierFunc(func(_ context.Context, candidate nativeassettransport.SignedManifest) error {
			if !signedManifestEqual(candidate, signed) {
				return ErrManifestRejected
			}
			return nil
		}),
	)
	if err != nil {
		return VerifiedProductManifest{}, ErrManifestRejected
	}
	return VerifiedProductManifest{
		native:          verified,
		assetSetDigest:  assetSetDigest,
		manifestDigest:  manifestDigest,
		canonicalDigest: verified.CanonicalDigest(),
	}, nil
}

func readBoundedRegularFile(filePath string, maximum int) ([]byte, error) {
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > int64(maximum) {
		return nil, ErrManifestRejected
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, ErrManifestRejected
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil || len(payload) == 0 || len(payload) > maximum {
		zero(payload)
		return nil, ErrManifestRejected
	}
	return payload, nil
}

func canonicalDigest(value string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if len(value) != sha256.Size*2 {
		return digest, ErrManifestRejected
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return digest, ErrManifestRejected
			}
		}
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		zero(decoded)
		return digest, ErrManifestRejected
	}
	copy(digest[:], decoded)
	zero(decoded)
	return digest, nil
}

func parseRole(value string) (nativeassettransport.AssetRole, error) {
	switch value {
	case "tokenizer":
		return nativeassettransport.AssetRoleTokenizer, nil
	case "tokenizer_config":
		return nativeassettransport.AssetRoleTokenizerConfig, nil
	case "model_config":
		return nativeassettransport.AssetRoleModelConfig, nil
	case "weight":
		return nativeassettransport.AssetRoleWeight, nil
	default:
		return 0, ErrManifestRejected
	}
}

func roleName(role nativeassettransport.AssetRole) string {
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

func signedManifestEqual(left, right nativeassettransport.SignedManifest) bool {
	if left.ModelID != right.ModelID || left.Backend != right.Backend ||
		left.ProfileRevision != right.ProfileRevision || len(left.Assets) != len(right.Assets) ||
		subtle.ConstantTimeCompare(left.ProfileDigest[:], right.ProfileDigest[:]) != 1 ||
		subtle.ConstantTimeCompare(left.ManifestDigest[:], right.ManifestDigest[:]) != 1 {
		return false
	}
	for index := range left.Assets {
		l, r := left.Assets[index], right.Assets[index]
		if l.Ordinal != r.Ordinal || l.AssetID != r.AssetID || l.Role != r.Role ||
			l.Required != r.Required || l.PlaintextSize != r.PlaintextSize ||
			l.Binding.ModelID != r.Binding.ModelID || l.Binding.Backend != r.Binding.Backend ||
			l.Binding.ProfileRevision != r.Binding.ProfileRevision ||
			subtle.ConstantTimeCompare(l.Binding.ProfileDigest[:], r.Binding.ProfileDigest[:]) != 1 ||
			subtle.ConstantTimeCompare(l.Binding.AssetDigest[:], r.Binding.AssetDigest[:]) != 1 {
			return false
		}
	}
	return true
}
