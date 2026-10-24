package productsecurity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/nativeruntime"
)

const (
	ModelPackManifestSchemaVersion = 1
	ModelProfileSchemaVersion = 1
	ModelPackIndexContentsRelativePath = "Resources/manifests/model-packs.json"
	ModelPackRootContentsRelativePath = "Resources/models"
	ModelPackProfileFilename = "meadow-profile.json"
	maxModelProfileBytes = 8 * 1024 * 1024
)

type modelPackIndexDocument struct {
	SchemaVersion int `json:"schema_version"`
	BundleIdentifier string `json:"bundle_identifier"`
	Profiles []modelPackIndexEntry `json:"profiles"`
}

type modelPackIndexEntry struct {
	ProfileID string `json:"profile_id"`
	LogicalModelID string `json:"logical_model_id"`
	ProfileBytes int64 `json:"profile_bytes"`
	ProfileSHA256 string `json:"profile_sha256"`
}

type modelProfileDocument struct {
	SchemaVersion int `json:"schema_version"`
	ProfileID string `json:"profile_id"`
	ProfileRevision uint64 `json:"profile_revision"`
	LogicalModelID string `json:"logical_model_id"`
	ArtifactName string `json:"artifact_name"`
	Architecture string `json:"architecture"`
	ModelType string `json:"model_type"`
	Backend nativeruntime.Backend `json:"backend"`
	CapabilityMask nativeruntime.CapabilityMask `json:"capability_mask"`
	Multimodal bool `json:"multimodal"`
	MemoryLimitBytes uint64 `json:"memory_limit_bytes"`
	DeclaredResidentBytes uint64 `json:"declared_resident_bytes"`
	PeakLoadBytes uint64 `json:"peak_load_bytes"`
	Quantization modelQuantizationDocument `json:"quantization"`
	Files []modelAssetDocument `json:"files"`
}

type modelQuantizationDocument struct {
	Method string `json:"method"`
	Bits int `json:"bits"`
	GroupSize int `json:"group_size"`
}

type modelAssetDocument struct {
	ID string `json:"id"`
	Path string `json:"path"`
	Bytes int64 `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// VerifiedModelQuantization is signed model identity evidence. It is safe for
// lifecycle status because it contains no loader path or implementation detail.
type VerifiedModelQuantization struct {
	Method string
	Bits int
	GroupSize int
}

// VerifiedModelAsset is an immutable file authorized by a signed profile. The
// relative path is rooted beneath the current App's fixed Resources/models
// directory; no caller-provided root participates in resolution.
type VerifiedModelAsset struct {
	ID string
	ContentsRelativePath string
	Bytes int64
	SHA256 [sha256.Size]byte
}

// VerifiedModelStatusEvidence is the non-secret subset needed to prove runtime
// identity. ArtifactIdentity is an opaque signed-profile identity, not a local
// filesystem path. Runtime and Core PIDs must come from the live launcher and
// must never be fabricated from this document.
type VerifiedModelStatusEvidence struct {
	CanonicalID string
	ArtifactIdentity string
	ArtifactName string
	Architecture string
	ModelType string
	Backend nativeruntime.Backend
	Multimodal bool
	Quantization VerifiedModelQuantization
}

// VerifiedModelProfile is returned only after the index signature, current App
// binding, profile digest, schema, metadata and every selected model asset have
// been verified. It cannot carry argv, environment, URL or an arbitrary root.
type VerifiedModelProfile struct {
	BundleIdentifier string
	ProfileID string
	ProfileRevision uint64
	ProfileDigest [sha256.Size]byte
	LogicalModelID string
	ArtifactIdentity string
	ArtifactName string
	Architecture string
	ModelType string
	Backend nativeruntime.Backend
	CapabilityMask nativeruntime.CapabilityMask
	Multimodal bool
	MemoryLimitBytes uint64
	DeclaredResidentBytes uint64
	PeakLoadBytes uint64
	Quantization VerifiedModelQuantization
	ProfileContentsRelativePath string
	Assets []VerifiedModelAsset
}

// StatusEvidence returns signed metadata suitable for runtime identity status.
// The caller must add the actual runtime/Core PID evidence independently.
func (profile VerifiedModelProfile) StatusEvidence() VerifiedModelStatusEvidence {
	return VerifiedModelStatusEvidence{
		CanonicalID: profile.LogicalModelID,
		ArtifactIdentity: profile.ArtifactIdentity,
		ArtifactName: profile.ArtifactName,
		Architecture: profile.Architecture,
		ModelType: profile.ModelType,
		Backend: profile.Backend,
		Multimodal: profile.Multimodal,
		Quantization: profile.Quantization,
	}
}

// ResolveCurrentModelProfile resolves a canonical logical model ID exclusively
// from the signed immutable model-pack index inside the current App bundle.
func ResolveCurrentModelProfile(logicalModelID string) (VerifiedModelProfile, error) {
	executable, err := os.Executable()
	if err != nil {
		return VerifiedModelProfile{}, fmt.Errorf("resolve executable: %w", err)
	}
	contents, err := appContentsForExecutable(executable)
	if err != nil {
		return VerifiedModelProfile{}, err
	}
	return resolveModelProfileAtContents(contents, logicalModelID)
}

func resolveModelProfileAtContents(contents string, logicalModelID string) (VerifiedModelProfile, error) {
	requested := strings.TrimSpace(logicalModelID)
	if err := validateLogicalModelID(requested); err != nil {
		return VerifiedModelProfile{}, fmt.Errorf("logical model id is invalid: %w", err)
	}
	payload, err := readVerifiedModelPackIndex(contents)
	if err != nil {
		return VerifiedModelProfile{}, err
	}
	var index modelPackIndexDocument
	if err := decodeStrictJSON(payload, &index); err != nil {
		return VerifiedModelProfile{}, fmt.Errorf("decode signed model-pack index: %w", err)
	}
	if index.SchemaVersion != ModelPackManifestSchemaVersion {
		return VerifiedModelProfile{}, fmt.Errorf("model-pack index schema_version must be %d", ModelPackManifestSchemaVersion)
	}
	bundleIdentifier := strings.TrimSpace(index.BundleIdentifier)
	if err := validateBundleIdentifier(bundleIdentifier); err != nil {
		return VerifiedModelProfile{}, fmt.Errorf("model-pack bundle_identifier is invalid: %w", err)
	}
	currentBundleIdentifier, err := readBundleIdentifier(filepath.Join(contents, "Info.plist"))
	if err != nil {
		return VerifiedModelProfile{}, err
	}
	if currentBundleIdentifier != bundleIdentifier {
		return VerifiedModelProfile{}, fmt.Errorf("signed model-pack bundle_identifier does not match current App Info.plist")
	}
	if len(index.Profiles) == 0 {
		return VerifiedModelProfile{}, fmt.Errorf("signed model-pack index contains no profiles")
	}
	profileIDs := make(map[string]struct{}, len(index.Profiles))
	logicalIDs := make(map[string]struct{}, len(index.Profiles))
	match := -1
	for position, entry := range index.Profiles {
		if err := validateProfileIndexEntry(entry); err != nil {
			return VerifiedModelProfile{}, fmt.Errorf("signed model-pack profile entry %d is invalid: %w", position, err)
		}
		if _, exists := profileIDs[entry.ProfileID]; exists {
			return VerifiedModelProfile{}, fmt.Errorf("signed model-pack index contains duplicate profile_id %q", entry.ProfileID)
		}
		profileIDs[entry.ProfileID] = struct{}{}
		if _, exists := logicalIDs[entry.LogicalModelID]; exists {
			return VerifiedModelProfile{}, fmt.Errorf("signed model-pack index contains duplicate logical_model_id %q", entry.LogicalModelID)
		}
		logicalIDs[entry.LogicalModelID] = struct{}{}
		if entry.LogicalModelID == requested {
			match = position
		}
	}
	if match < 0 {
		return VerifiedModelProfile{}, fmt.Errorf("signed model-pack index does not authorize logical model %q", requested)
	}
	return verifySelectedModelProfile(contents, bundleIdentifier, index.Profiles[match])
}

func readVerifiedModelPackIndex(contents string) ([]byte, error) {
	manifestPath, err := directContentsFile(contents, ModelPackIndexContentsRelativePath)
	if err != nil {
		return nil, fmt.Errorf("inspect signed model-pack index: %w", err)
	}
	signaturePath, err := directContentsFile(contents, ModelPackIndexContentsRelativePath+".sig")
	if err != nil {
		return nil, fmt.Errorf("inspect model-pack index signature: %w", err)
	}
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read signed model-pack index: %w", err)
	}
	signaturePayload, err := os.ReadFile(signaturePath)
	if err != nil {
		return nil, fmt.Errorf("read model-pack index signature: %w", err)
	}
	var signature signatureDocument
	if err := decodeStrictJSON(signaturePayload, &signature); err != nil {
		return nil, fmt.Errorf("decode model-pack index signature: %w", err)
	}
	if signature.SchemaVersion != 1 || signature.Algorithm != "Ed25519" {
		return nil, fmt.Errorf("unsupported model-pack signature contract")
	}
	if expected := strings.TrimSpace(BuildKeyID); expected != "" && signature.KeyID != expected {
		return nil, fmt.Errorf("model-pack signature key id mismatch")
	}
	publicKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(BuildPublicKeyB64))
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("protected build has an invalid release public key")
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(signature.SignatureBase64)
	if err != nil || len(signatureBytes) != ed25519.SignatureSize {
		return nil, fmt.Errorf("model-pack index signature is malformed")
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signatureBytes) {
		return nil, fmt.Errorf("model-pack index signature verification failed")
	}
	return payload, nil
}

func verifySelectedModelProfile(contents string, bundleIdentifier string, entry modelPackIndexEntry) (VerifiedModelProfile, error) {
	profileRelativePath := filepath.ToSlash(filepath.Join(ModelPackRootContentsRelativePath, entry.ProfileID, ModelPackProfileFilename))
	profilePath, err := directContentsFile(contents, profileRelativePath)
	if err != nil {
		return VerifiedModelProfile{}, fmt.Errorf("inspect signed model profile: %w", err)
	}
	info, err := os.Stat(profilePath)
	if err != nil {
		return VerifiedModelProfile{}, fmt.Errorf("inspect signed model profile: %w", err)
	}
	if entry.ProfileBytes <= 0 || entry.ProfileBytes > maxModelProfileBytes || info.Size() != entry.ProfileBytes {
		return VerifiedModelProfile{}, fmt.Errorf("signed model profile size mismatch")
	}
	profilePayload, err := os.ReadFile(profilePath)
	if err != nil {
		return VerifiedModelProfile{}, fmt.Errorf("read signed model profile: %w", err)
	}
	profileDigest, err := decodeSHA256(entry.ProfileSHA256, "signed model profile")
	if err != nil {
		return VerifiedModelProfile{}, err
	}
	actualProfileDigest := sha256.Sum256(profilePayload)
	if actualProfileDigest != profileDigest {
		return VerifiedModelProfile{}, fmt.Errorf("signed model profile integrity verification failed")
	}
	var document modelProfileDocument
	if err := decodeStrictJSON(profilePayload, &document); err != nil {
		return VerifiedModelProfile{}, fmt.Errorf("decode signed model profile: %w", err)
	}
	if err := validateModelProfile(document, entry); err != nil {
		return VerifiedModelProfile{}, err
	}
	assets, err := verifyModelAssets(contents, entry.ProfileID, document.Files)
	if err != nil {
		return VerifiedModelProfile{}, err
	}
	digestHex := hex.EncodeToString(profileDigest[:])
	return VerifiedModelProfile{
		BundleIdentifier: bundleIdentifier,
		ProfileID: document.ProfileID,
		ProfileRevision: document.ProfileRevision,
		ProfileDigest: profileDigest,
		LogicalModelID: document.LogicalModelID,
		ArtifactIdentity: "meadow-profile:"+document.ProfileID+"@sha256:"+digestHex,
		ArtifactName: document.ArtifactName,
		Architecture: document.Architecture,
		ModelType: document.ModelType,
		Backend: document.Backend,
		CapabilityMask: document.CapabilityMask,
		Multimodal: document.Multimodal,
		MemoryLimitBytes: document.MemoryLimitBytes,
		DeclaredResidentBytes: document.DeclaredResidentBytes,
		PeakLoadBytes: document.PeakLoadBytes,
		Quantization: VerifiedModelQuantization{
			Method: document.Quantization.Method,
			Bits: document.Quantization.Bits,
			GroupSize: document.Quantization.GroupSize,
		},
		ProfileContentsRelativePath: profileRelativePath,
		Assets: assets,
	}, nil
}

func validateProfileIndexEntry(entry modelPackIndexEntry) error {
	if err := validateProfileID(entry.ProfileID); err != nil {
		return err
	}
	if err := validateLogicalModelID(entry.LogicalModelID); err != nil {
		return err
	}
	if entry.ProfileBytes <= 0 || entry.ProfileBytes > maxModelProfileBytes {
		return fmt.Errorf("profile_bytes is outside the permitted range")
	}
	_, err := decodeSHA256(entry.ProfileSHA256, "profile_sha256")
	return err
}

func validateModelProfile(document modelProfileDocument, entry modelPackIndexEntry) error {
	if document.SchemaVersion != ModelProfileSchemaVersion {
		return fmt.Errorf("model profile schema_version must be %d", ModelProfileSchemaVersion)
	}
	if document.ProfileID != entry.ProfileID || document.LogicalModelID != entry.LogicalModelID {
		return fmt.Errorf("model profile identity does not match signed index")
	}
	if document.ProfileRevision == 0 {
		return fmt.Errorf("model profile revision must be positive")
	}
	if strings.TrimSpace(document.ArtifactName) == "" || strings.TrimSpace(document.Architecture) == "" || strings.TrimSpace(document.ModelType) == "" {
		return fmt.Errorf("model profile status identity metadata is incomplete")
	}
	switch document.Backend {
	case nativeruntime.BackendBaseline, nativeruntime.BackendSGLang, nativeruntime.BackendMTPLX:
	default:
		return fmt.Errorf("model profile backend %d is not explicit", document.Backend)
	}
	if document.CapabilityMask == 0 || document.CapabilityMask&^nativeruntime.KnownCapabilities != 0 {
		return fmt.Errorf("model profile capability_mask is invalid")
	}
	if !document.CapabilityMask.Has(nativeruntime.CapabilityText) {
		return fmt.Errorf("model profile must authorize text inference")
	}
	if document.Multimodal && !document.CapabilityMask.Has(nativeruntime.CapabilityVision) {
		return fmt.Errorf("multimodal model profile must authorize vision")
	}
	if !document.Multimodal && document.CapabilityMask.Has(nativeruntime.CapabilityVision) {
		return fmt.Errorf("text-only model profile cannot authorize vision")
	}
	if document.Backend == nativeruntime.BackendSGLang && !document.CapabilityMask.Has(nativeruntime.CapabilityBackendSGLang) {
		return fmt.Errorf("SGLang model profile is missing its backend capability")
	}
	if document.Backend == nativeruntime.BackendMTPLX && !document.CapabilityMask.Has(nativeruntime.CapabilityBackendMTPLX) {
		return fmt.Errorf("MTPLX model profile is missing its backend capability")
	}
	if document.MemoryLimitBytes == 0 || document.DeclaredResidentBytes == 0 || document.PeakLoadBytes == 0 || document.DeclaredResidentBytes > document.PeakLoadBytes || document.PeakLoadBytes > document.MemoryLimitBytes {
		return fmt.Errorf("model profile memory bounds are invalid")
	}
	method := strings.TrimSpace(document.Quantization.Method)
	if method == "" || document.Quantization.Bits <= 0 || document.Quantization.Bits > 16 || document.Quantization.GroupSize <= 0 {
		return fmt.Errorf("model profile quantization evidence is invalid")
	}
	if len(document.Files) == 0 {
		return fmt.Errorf("model profile contains no immutable assets")
	}
	return nil
}

func verifyModelAssets(contents string, profileID string, documents []modelAssetDocument) ([]VerifiedModelAsset, error) {
	assetIDs := make(map[string]struct{}, len(documents))
	assetPaths := make(map[string]struct{}, len(documents))
	assets := make([]VerifiedModelAsset, 0, len(documents))
	for position, document := range documents {
		if err := validateAssetID(document.ID); err != nil {
			return nil, fmt.Errorf("model asset %d id is invalid: %w", position, err)
		}
		if _, exists := assetIDs[document.ID]; exists {
			return nil, fmt.Errorf("model profile contains duplicate asset id %q", document.ID)
		}
		assetIDs[document.ID] = struct{}{}
		relativeAssetPath, err := cleanRelativeModelAssetPath(document.Path)
		if err != nil {
			return nil, fmt.Errorf("model asset %q path is invalid: %w", document.ID, err)
		}
		if relativeAssetPath == ModelPackProfileFilename {
			return nil, fmt.Errorf("model asset list cannot include its profile document")
		}
		if _, exists := assetPaths[relativeAssetPath]; exists {
			return nil, fmt.Errorf("model profile contains duplicate asset path %q", relativeAssetPath)
		}
		assetPaths[relativeAssetPath] = struct{}{}
		if document.Bytes <= 0 {
			return nil, fmt.Errorf("model asset %q size must be positive", document.ID)
		}
		digest, err := decodeSHA256(document.SHA256, "model asset "+document.ID)
		if err != nil {
			return nil, err
		}
		contentsRelativePath := filepath.ToSlash(filepath.Join(ModelPackRootContentsRelativePath, profileID, filepath.FromSlash(relativeAssetPath)))
		assetPath, err := directContentsFile(contents, contentsRelativePath)
		if err != nil {
			return nil, fmt.Errorf("inspect model asset %q: %w", document.ID, err)
		}
		info, err := os.Stat(assetPath)
		if err != nil || info.Size() != document.Bytes {
			return nil, fmt.Errorf("model asset %q size mismatch", document.ID)
		}
		actual, err := sha256File(assetPath)
		if err != nil {
			return nil, fmt.Errorf("hash model asset %q: %w", document.ID, err)
		}
		if actual != hex.EncodeToString(digest[:]) {
			return nil, fmt.Errorf("model asset %q integrity verification failed", document.ID)
		}
		assets = append(assets, VerifiedModelAsset{
			ID: document.ID,
			ContentsRelativePath: contentsRelativePath,
			Bytes: document.Bytes,
			SHA256: digest,
		})
	}
	return assets, nil
}

func directContentsFile(contents string, contentsRelativePath string) (string, error) {
	relative := filepath.Clean(filepath.FromSlash(strings.TrimSpace(contentsRelativePath)))
	if relative == "." || relative == "" || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe Contents-relative path")
	}
	current := contents
	segments := strings.Split(filepath.ToSlash(relative), "/")
	for index, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("invalid path segment")
		}
		current = filepath.Join(current, filepath.FromSlash(segment))
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path contains a symbolic link")
		}
		if index < len(segments)-1 && !info.IsDir() {
			return "", fmt.Errorf("path parent is not a directory")
		}
		if index == len(segments)-1 && !info.Mode().IsRegular() {
			return "", fmt.Errorf("target is not a regular file")
		}
	}
	return current, nil
}

func cleanRelativeModelAssetPath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.Contains(trimmed, "\\") {
		return "", fmt.Errorf("asset path is empty or non-canonical")
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(trimmed)))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") || cleaned != trimmed {
		return "", fmt.Errorf("asset path must be a canonical relative path")
	}
	return cleaned, nil
}

func validateLogicalModelID(value string) error {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return fmt.Errorf("logical model id is empty, oversized or non-canonical")
	}
	if strings.ContainsAny(value, "/\\\x00\r\n") || value == "." || value == ".." {
		return fmt.Errorf("logical model id contains a path or control character")
	}
	return nil
}

func validateProfileID(value string) error {
	if value == "" || len(value) > 96 || value != strings.ToLower(value) || value != strings.TrimSpace(value) {
		return fmt.Errorf("profile_id is empty, oversized or non-canonical")
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '.') {
			continue
		}
		return fmt.Errorf("profile_id contains an unsupported character")
	}
	if strings.HasSuffix(value, ".") || strings.HasSuffix(value, "-") || strings.Contains(value, "..") {
		return fmt.Errorf("profile_id structure is invalid")
	}
	return nil
}

func validateAssetID(value string) error {
	if value == "" || len(value) > 96 || value != strings.TrimSpace(value) {
		return fmt.Errorf("asset id is empty, oversized or non-canonical")
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return fmt.Errorf("asset id contains an unsupported character")
	}
	return nil
}

func decodeSHA256(value string, label string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	decoded, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(value)))
	if err != nil || len(decoded) != sha256.Size {
		return digest, fmt.Errorf("%s SHA-256 is malformed", label)
	}
	copy(digest[:], decoded)
	return digest, nil
}
