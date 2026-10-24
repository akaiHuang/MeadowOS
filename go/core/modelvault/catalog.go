package modelvault

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type verifiedArtifact struct {
	entry        CatalogArtifact
	digest       [sha256.Size]byte
	chunkDigests [][sha256.Size]byte
}

type verifiedEntry struct {
	entry          CatalogEntry
	profileDigest  [sha256.Size]byte
	licenseDigest  [sha256.Size]byte
	manifestDigest [sha256.Size]byte
	origin         *url.URL
	artifacts      []verifiedArtifact
}

type VerifiedCatalog struct {
	sequence uint64
	digest   [sha256.Size]byte
	entries  []verifiedEntry
	vaultID  [sha256.Size]byte
}

func (catalog VerifiedCatalog) Sequence() uint64 { return catalog.sequence }

func (catalog VerifiedCatalog) Digest() [sha256.Size]byte { return catalog.digest }

func (catalog VerifiedCatalog) Entry(modelID, version string) (CatalogEntry, bool) {
	entry, ok := catalog.find(modelID, version)
	if !ok {
		return CatalogEntry{}, false
	}
	return cloneCatalogEntry(entry.entry), true
}

func (catalog VerifiedCatalog) find(modelID, version string) (verifiedEntry, bool) {
	for _, entry := range catalog.entries {
		if entry.entry.ModelID == modelID && entry.entry.Version == version {
			return cloneVerifiedEntry(entry), true
		}
	}
	return verifiedEntry{}, false
}

func verifyCatalogPayload(
	ctx context.Context,
	payload []byte,
	signature CatalogSignature,
	keyring Keyring,
	now time.Time,
) (Catalog, []verifiedEntry, [sha256.Size]byte, error) {
	if ctx == nil || len(payload) == 0 || len(payload) > maxCatalogBytes || now.IsZero() {
		return Catalog{}, nil, [sha256.Size]byte{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return Catalog{}, nil, [sha256.Size]byte{}, errors.Join(ErrCanceled, err)
	}
	if signature.Algorithm != "Ed25519" ||
		!validIdentifier(signature.KeyID) ||
		len(signature.Signature) != ed25519.SignatureSize {
		return Catalog{}, nil, [sha256.Size]byte{}, ErrCatalogRejected
	}
	key, ok := resolveVerificationKey(keyring, signature.KeyID, now)
	if !ok || !ed25519.Verify(key, payload, signature.Signature) {
		return Catalog{}, nil, [sha256.Size]byte{}, ErrCatalogRejected
	}
	var catalog Catalog
	if err := decodeCanonicalJSON(payload, &catalog); err != nil {
		return Catalog{}, nil, [sha256.Size]byte{}, errors.Join(ErrCatalogRejected, err)
	}
	entries, err := validateCatalog(catalog, now)
	if err != nil {
		return Catalog{}, nil, [sha256.Size]byte{}, err
	}
	return catalog, entries, sha256.Sum256(payload), nil
}

func resolveVerificationKey(keyring Keyring, keyID string, now time.Time) (ed25519.PublicKey, bool) {
	seen := make(map[string]struct{}, len(keyring.Keys))
	var selected ed25519.PublicKey
	for _, key := range keyring.Keys {
		if !validIdentifier(key.KeyID) || len(key.PublicKey) != ed25519.PublicKeySize ||
			key.NotBefore.IsZero() || key.NotAfter.IsZero() || !key.NotAfter.After(key.NotBefore) {
			return nil, false
		}
		if _, exists := seen[key.KeyID]; exists {
			return nil, false
		}
		seen[key.KeyID] = struct{}{}
		if key.KeyID == keyID {
			if key.Revoked || now.Before(key.NotBefore) || !now.Before(key.NotAfter) {
				return nil, false
			}
			selected = append(ed25519.PublicKey(nil), key.PublicKey...)
		}
	}
	return selected, len(selected) == ed25519.PublicKeySize
}

func validateCatalog(catalog Catalog, now time.Time) ([]verifiedEntry, error) {
	if catalog.SchemaVersion != CatalogSchemaVersion || catalog.Sequence == 0 ||
		catalog.IssuedAt <= 0 || catalog.ExpiresAt <= catalog.IssuedAt ||
		len(catalog.Entries) == 0 || len(catalog.Entries) > maxEntries {
		return nil, ErrCatalogRejected
	}
	issued := time.Unix(catalog.IssuedAt, 0)
	expires := time.Unix(catalog.ExpiresAt, 0)
	if now.Before(issued) || !now.Before(expires) {
		return nil, ErrCatalogRejected
	}
	entries := make([]verifiedEntry, 0, len(catalog.Entries))
	seen := make(map[string]struct{}, len(catalog.Entries))
	previous := ""
	for _, raw := range catalog.Entries {
		key := raw.ModelID + "\x00" + raw.Version
		if previous != "" && key <= previous {
			return nil, ErrCatalogRejected
		}
		previous = key
		if _, exists := seen[key]; exists {
			return nil, ErrCatalogRejected
		}
		seen[key] = struct{}{}
		entry, err := validateCatalogEntry(raw)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func validateCatalogEntry(raw CatalogEntry) (verifiedEntry, error) {
	if !validIdentifier(raw.ModelID) || !validIdentifier(raw.Version) ||
		!validIdentifier(raw.ProfileID) || !validIdentifier(raw.LicenseID) ||
		len(raw.Artifacts) == 0 || len(raw.Artifacts) > maxArtifacts {
		return verifiedEntry{}, ErrCatalogRejected
	}
	profile, ok := parseDigest(raw.ProfileDigestSHA256)
	if !ok {
		return verifiedEntry{}, ErrCatalogRejected
	}
	license, ok := parseDigest(raw.LicenseDigestSHA256)
	if !ok {
		return verifiedEntry{}, ErrCatalogRejected
	}
	manifest, ok := parseDigest(raw.ManifestDigestSHA256)
	if !ok {
		return verifiedEntry{}, ErrCatalogRejected
	}
	origin, err := validateOrigin(raw.SourceOrigin)
	if err != nil {
		return verifiedEntry{}, err
	}
	artifacts := make([]verifiedArtifact, 0, len(raw.Artifacts))
	ids := make(map[string]struct{}, len(raw.Artifacts))
	digests := make(map[[sha256.Size]byte]struct{}, len(raw.Artifacts))
	var tokenizer, modelConfig, weight bool
	for index, artifact := range raw.Artifacts {
		if artifact.Ordinal != uint32(index) || !validIdentifier(artifact.AssetID) ||
			artifact.Role < 1 || artifact.Role > 4 || artifact.Bytes == 0 ||
			artifact.Bytes > maxArtifactBytes || artifact.ChunkBytes == 0 ||
			artifact.ChunkBytes > maxChunkBytes ||
			!validSourcePathTemplate(artifact.SourcePathTemplate) {
			return verifiedEntry{}, ErrCatalogRejected
		}
		digest, ok := parseDigest(artifact.SHA256)
		if !ok {
			return verifiedEntry{}, ErrCatalogRejected
		}
		if _, exists := ids[artifact.AssetID]; exists {
			return verifiedEntry{}, ErrCatalogRejected
		}
		if _, exists := digests[digest]; exists {
			return verifiedEntry{}, ErrCatalogRejected
		}
		ids[artifact.AssetID] = struct{}{}
		digests[digest] = struct{}{}
		expectedChunks := (artifact.Bytes + uint64(artifact.ChunkBytes) - 1) / uint64(artifact.ChunkBytes)
		if expectedChunks == 0 || expectedChunks > maxArtifacts ||
			uint64(len(artifact.ChunkSHA256)) != expectedChunks {
			return verifiedEntry{}, ErrCatalogRejected
		}
		chunkDigests := make([][sha256.Size]byte, len(artifact.ChunkSHA256))
		for position, encoded := range artifact.ChunkSHA256 {
			parsed, valid := parseDigest(encoded)
			if !valid {
				return verifiedEntry{}, ErrCatalogRejected
			}
			chunkDigests[position] = parsed
		}
		artifacts = append(artifacts, verifiedArtifact{
			entry: cloneCatalogArtifact(artifact), digest: digest, chunkDigests: chunkDigests,
		})
		tokenizer = tokenizer || artifact.Role == 1
		modelConfig = modelConfig || artifact.Role == 3
		weight = weight || artifact.Role == 4
	}
	if !tokenizer || !modelConfig || !weight {
		return verifiedEntry{}, ErrCatalogRejected
	}
	return verifiedEntry{
		entry: cloneCatalogEntry(raw), profileDigest: profile, licenseDigest: license,
		manifestDigest: manifest, origin: origin, artifacts: artifacts,
	}, nil
}

func validateOrigin(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > maxTemplateBytes || !utf8.ValidString(raw) {
		return nil, ErrCatalogRejected
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return nil, ErrCatalogRejected
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return nil, ErrCatalogRejected
	}
	parsed.Path = ""
	return parsed, nil
}

func validSourcePathTemplate(value string) bool {
	if len(value) == 0 || len(value) > maxTemplateBytes || !utf8.ValidString(value) ||
		!strings.HasPrefix(value, "/") || strings.Contains(value, "\\") ||
		strings.Contains(value, "%") || strings.Contains(value, "//") ||
		strings.ContainsAny(value, "?#") {
		return false
	}
	replaced := strings.NewReplacer(
		"{model_id}", "model", "{profile_id}", "profile", "{version}", "version",
	).Replace(value)
	if strings.ContainsAny(replaced, "{}") || path.Clean(replaced) != replaced {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(replaced, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func decodeCanonicalJSON(payload []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON value")
	}
	canonical, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, canonical) {
		return fmt.Errorf("document is not canonical JSON")
	}
	return nil
}

func parseDigest(value string) ([sha256.Size]byte, bool) {
	var result [sha256.Size]byte
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return result, false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return result, false
	}
	copy(result[:], decoded)
	if subtle.ConstantTimeCompare(result[:], make([]byte, sha256.Size)) == 1 {
		return [sha256.Size]byte{}, false
	}
	return result, true
}

func validIdentifier(value string) bool {
	if len(value) == 0 || len(value) > maxIdentifierBytes || !utf8.ValidString(value) {
		return false
	}
	for index, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			(index > 0 && (character == '.' || character == '_' || character == '-')) {
			continue
		}
		return false
	}
	return value != "." && value != ".."
}

func cloneCatalogEntry(entry CatalogEntry) CatalogEntry {
	copyValue := entry
	copyValue.Artifacts = make([]CatalogArtifact, len(entry.Artifacts))
	for index, artifact := range entry.Artifacts {
		copyValue.Artifacts[index] = cloneCatalogArtifact(artifact)
	}
	return copyValue
}

func cloneCatalogArtifact(artifact CatalogArtifact) CatalogArtifact {
	copyValue := artifact
	copyValue.ChunkSHA256 = append([]string(nil), artifact.ChunkSHA256...)
	return copyValue
}

func cloneVerifiedEntry(entry verifiedEntry) verifiedEntry {
	copyValue := entry
	copyValue.entry = cloneCatalogEntry(entry.entry)
	copyValue.origin = &url.URL{}
	*copyValue.origin = *entry.origin
	copyValue.artifacts = make([]verifiedArtifact, len(entry.artifacts))
	for index, artifact := range entry.artifacts {
		copyValue.artifacts[index] = artifact
		copyValue.artifacts[index].entry = cloneCatalogArtifact(artifact.entry)
		copyValue.artifacts[index].chunkDigests = append([][sha256.Size]byte(nil), artifact.chunkDigests...)
	}
	return copyValue
}

func sortedEntries(entries []verifiedEntry) {
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].entry.ModelID == entries[right].entry.ModelID {
			return entries[left].entry.Version < entries[right].entry.Version
		}
		return entries[left].entry.ModelID < entries[right].entry.ModelID
	})
}
