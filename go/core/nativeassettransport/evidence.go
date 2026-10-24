package nativeassettransport

import (
	"crypto/sha256"
	"encoding/binary"
)

type AssetTerminalEvidence struct {
	Ordinal        uint32
	AssetID        string
	Role           AssetRole
	PlaintextBytes uint64
	ChunkCount     uint64
	AssetDigest    [sha256.Size]byte
	StreamDigest   [sha256.Size]byte
	EvidenceDigest [sha256.Size]byte
}

type AggregateTerminalEvidence struct {
	SessionID       [SessionIDBytes]byte
	ManifestDigest  [sha256.Size]byte
	CanonicalDigest [sha256.Size]byte
	RequiredAssets  uint32
	CompletedAssets uint32
	TotalPlaintext  uint64
	Assets          []AssetTerminalEvidence
	AggregateDigest [sha256.Size]byte
}

func assetEvidenceDigest(
	manifest VerifiedManifest,
	session [SessionIDBytes]byte,
	asset SignedAsset,
	total, chunks uint64,
	streamDigest [sha256.Size]byte,
) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write(assetEvidenceLabel)
	_, _ = hash.Write(manifest.manifest.ManifestDigest[:])
	_, _ = hash.Write(manifest.canonicalDigest[:])
	_, _ = hash.Write(session[:])
	writeHashUint32(hash, asset.Ordinal)
	writeHashUint32(hash, uint32(asset.Role))
	writeHashString(hash, asset.AssetID)
	writeHashUint64(hash, total)
	writeHashUint64(hash, chunks)
	_, _ = hash.Write(asset.Binding.AssetDigest[:])
	_, _ = hash.Write(streamDigest[:])
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func aggregateEvidence(
	manifest VerifiedManifest,
	session [SessionIDBytes]byte,
	assets []AssetTerminalEvidence,
) AggregateTerminalEvidence {
	evidence := AggregateTerminalEvidence{
		SessionID:       session,
		ManifestDigest:  manifest.manifest.ManifestDigest,
		CanonicalDigest: manifest.canonicalDigest,
		RequiredAssets:  manifest.requiredCount,
		CompletedAssets: uint32(len(assets)),
		Assets:          append([]AssetTerminalEvidence(nil), assets...),
	}
	hash := sha256.New()
	_, _ = hash.Write(aggregateEvidenceLabel)
	_, _ = hash.Write(evidence.ManifestDigest[:])
	_, _ = hash.Write(evidence.CanonicalDigest[:])
	_, _ = hash.Write(session[:])
	writeHashUint32(hash, evidence.RequiredAssets)
	writeHashUint32(hash, evidence.CompletedAssets)
	for _, asset := range assets {
		evidence.TotalPlaintext += asset.PlaintextBytes
		_, _ = hash.Write(asset.EvidenceDigest[:])
	}
	writeHashUint64(hash, evidence.TotalPlaintext)
	copy(evidence.AggregateDigest[:], hash.Sum(nil))
	return evidence
}

func encodeAggregateTerminal(evidence AggregateTerminalEvidence) [112]byte {
	var payload [112]byte
	binary.BigEndian.PutUint32(payload[0:4], evidence.RequiredAssets)
	binary.BigEndian.PutUint32(payload[4:8], evidence.CompletedAssets)
	binary.BigEndian.PutUint64(payload[8:16], evidence.TotalPlaintext)
	copy(payload[16:48], evidence.ManifestDigest[:])
	copy(payload[48:80], evidence.CanonicalDigest[:])
	copy(payload[80:112], evidence.AggregateDigest[:])
	return payload
}
