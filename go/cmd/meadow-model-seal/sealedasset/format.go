package sealedasset

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"runtime"

	"github.com/hey-meadow/meadow-code/go/core/nativeassettransport"
)

var sealedMagic = [8]byte{'M', 'D', 'W', 'S', 'E', 'A', 'L', '1'}

const (
	algorithmAES256GCM = 1
	preambleBytes      = 60
	headerAADKind      = 1
	chunkAADKind       = 2
)

type assetIdentity struct {
	modelID         string
	profileRevision uint64
	backend         uint32
	profileDigest   [sha256.Size]byte
	assetSetDigest  [sha256.Size]byte
	manifestDigest  [sha256.Size]byte
	canonicalDigest [sha256.Size]byte
	asset           nativeassettransport.SignedAsset
}

type fileMetadata struct {
	SchemaVersion           int    `json:"schema_version"`
	FormatVersion           int    `json:"format_version"`
	ModelID                 string `json:"model_id"`
	ProfileRevision         uint64 `json:"profile_revision"`
	Backend                 uint32 `json:"backend"`
	ProfileDigest           string `json:"profile_digest"`
	AssetSetDigest          string `json:"asset_set_digest"`
	ManifestDigest          string `json:"manifest_digest"`
	CanonicalManifestDigest string `json:"canonical_manifest_digest"`
	Ordinal                 uint32 `json:"ordinal"`
	AssetID                 string `json:"asset_id"`
	Role                    string `json:"role"`
	Required                bool   `json:"required"`
	PlaintextBytes          uint64 `json:"plaintext_bytes"`
	PlaintextSHA256         string `json:"plaintext_sha256"`
	ChunkBytes              uint32 `json:"chunk_bytes"`
	ChunkCount              uint64 `json:"chunk_count"`
}

func identityFor(verified VerifiedProductManifest, asset nativeassettransport.SignedAsset) assetIdentity {
	manifest := verified.SignedManifest()
	return assetIdentity{
		modelID:         manifest.ModelID,
		profileRevision: manifest.ProfileRevision,
		backend:         uint32(manifest.Backend),
		profileDigest:   manifest.ProfileDigest,
		assetSetDigest:  verified.assetSetDigest,
		manifestDigest:  verified.manifestDigest,
		canonicalDigest: verified.canonicalDigest,
		asset:           asset,
	}
}

func metadataFor(identity assetIdentity, chunkBytes uint32) fileMetadata {
	return fileMetadata{
		SchemaVersion:           ManifestSchemaVersion,
		FormatVersion:           FormatVersion,
		ModelID:                 identity.modelID,
		ProfileRevision:         identity.profileRevision,
		Backend:                 identity.backend,
		ProfileDigest:           hex.EncodeToString(identity.profileDigest[:]),
		AssetSetDigest:          hex.EncodeToString(identity.assetSetDigest[:]),
		ManifestDigest:          hex.EncodeToString(identity.manifestDigest[:]),
		CanonicalManifestDigest: hex.EncodeToString(identity.canonicalDigest[:]),
		Ordinal:                 identity.asset.Ordinal,
		AssetID:                 identity.asset.AssetID,
		Role:                    roleName(identity.asset.Role),
		Required:                identity.asset.Required,
		PlaintextBytes:          identity.asset.PlaintextSize,
		PlaintextSHA256:         hex.EncodeToString(identity.asset.Binding.AssetDigest[:]),
		ChunkBytes:              chunkBytes,
		ChunkCount:              chunkCount(identity.asset.PlaintextSize, chunkBytes),
	}
}

func identityBytes(identity assetIdentity) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("meadow-sealed-asset-identity-v1\x00")
	writeString(&buffer, identity.modelID)
	writeU64(&buffer, identity.profileRevision)
	writeU32(&buffer, identity.backend)
	buffer.Write(identity.profileDigest[:])
	buffer.Write(identity.assetSetDigest[:])
	buffer.Write(identity.manifestDigest[:])
	buffer.Write(identity.canonicalDigest[:])
	writeString(&buffer, identity.asset.AssetID)
	writeU32(&buffer, uint32(identity.asset.Role))
	writeU32(&buffer, identity.asset.Ordinal)
	writeU64(&buffer, identity.asset.PlaintextSize)
	buffer.Write(identity.asset.Binding.AssetDigest[:])
	return buffer.Bytes()
}

func aad(identity assetIdentity, kind uint32, index, size uint64) []byte {
	context := identityBytes(identity)
	var buffer bytes.Buffer
	buffer.WriteString("meadow-sealed-asset-aad-v1\x00")
	buffer.Write(context)
	writeU32(&buffer, kind)
	writeU64(&buffer, index)
	writeU64(&buffer, size)
	zero(context)
	return buffer.Bytes()
}

func deriveKey(master, salt []byte, label string, identity assetIdentity) []byte {
	extract := hmac.New(sha256.New, salt)
	_, _ = extract.Write(master)
	prk := extract.Sum(nil)
	context := identityBytes(identity)
	expand := hmac.New(sha256.New, prk)
	_, _ = expand.Write([]byte("meadow-sealed-asset-key-v1\x00"))
	_, _ = expand.Write([]byte(label))
	_, _ = expand.Write(context)
	_, _ = expand.Write([]byte{1})
	key := expand.Sum(nil)
	zero(prk)
	zero(context)
	return key[:MasterKeyBytes]
}

func nonceFor(base [12]byte, index uint64) [12]byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], index)
	for offset := 0; offset < len(encoded); offset++ {
		base[4+offset] ^= encoded[offset]
	}
	zero(encoded[:])
	return base
}

func chunkCount(size uint64, chunkBytes uint32) uint64 {
	return (size + uint64(chunkBytes) - 1) / uint64(chunkBytes)
}

func writeString(buffer *bytes.Buffer, value string) {
	writeU32(buffer, uint32(len(value)))
	buffer.WriteString(value)
}

func writeU32(buffer *bytes.Buffer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	buffer.Write(encoded[:])
}

func writeU64(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	buffer.Write(encoded[:])
}

func encodePreamble(salt [32]byte, baseNonce [12]byte, metadataBytes uint32) [preambleBytes]byte {
	var output [preambleBytes]byte
	copy(output[0:8], sealedMagic[:])
	binary.BigEndian.PutUint16(output[8:10], FormatVersion)
	binary.BigEndian.PutUint16(output[10:12], algorithmAES256GCM)
	copy(output[12:44], salt[:])
	copy(output[44:56], baseNonce[:])
	binary.BigEndian.PutUint32(output[56:60], metadataBytes)
	return output
}

func zero(value []byte) {
	clear(value)
	runtime.KeepAlive(value)
}

func evidenceDigest(evidence Evidence) (string, error) {
	evidence.EvidenceDigest = ""
	payload, err := json.Marshal(evidence)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	zero(payload)
	return hex.EncodeToString(digest[:]), nil
}

func SealedAssetFilename(ordinal uint32) string {
	return "asset-" + sixDigits(ordinal) + ".sealed"
}

func sixDigits(value uint32) string {
	const digits = "0123456789"
	var output [6]byte
	for index := len(output) - 1; index >= 0; index-- {
		output[index] = digits[value%10]
		value /= 10
	}
	return string(output[:])
}
