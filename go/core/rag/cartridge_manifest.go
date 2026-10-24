package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// CartridgeManifest is the portable, reviewable format used to install a
// knowledge cartridge. A manifest contains only curated method/domain
// knowledge; private memories and transient web cache entries must enter
// through their dedicated consent and evidence paths.
type CartridgeManifest struct {
	Schema      int               `json:"schema"`
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description,omitempty"`
	Version     string            `json:"version"`
	Records     []CartridgeRecord `json:"records"`
}

type CartridgeImportResult struct {
	ManifestID string   `json:"manifest_id"`
	Version    string   `json:"version"`
	RecordIDs  []string `json:"record_ids"`
}

func DecodeCartridgeManifest(payload []byte) (CartridgeManifest, error) {
	var manifest CartridgeManifest
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return CartridgeManifest{}, fmt.Errorf("decode cartridge manifest: %w", err)
	}
	if decoder.More() {
		return CartridgeManifest{}, errors.New("decode cartridge manifest: multiple JSON values")
	}
	manifest.ID = strings.TrimSpace(manifest.ID)
	manifest.Title = strings.TrimSpace(manifest.Title)
	manifest.Description = strings.TrimSpace(manifest.Description)
	manifest.Version = strings.TrimSpace(manifest.Version)
	if manifest.Schema != 1 {
		return CartridgeManifest{}, fmt.Errorf("unsupported cartridge manifest schema %d", manifest.Schema)
	}
	if !knowledgeEntryIDRe.MatchString(manifest.ID) || manifest.Title == "" || manifest.Version == "" {
		return CartridgeManifest{}, errors.New("cartridge manifest requires a safe id, title, and version")
	}
	if len(manifest.Records) == 0 {
		return CartridgeManifest{}, errors.New("cartridge manifest has no records")
	}
	seen := make(map[string]bool, len(manifest.Records))
	for index := range manifest.Records {
		record := normalizeCartridgeRecord(manifest.Records[index])
		if record.CartridgeID == "" {
			record.CartridgeID = manifest.ID
		}
		if record.CartridgeID != manifest.ID {
			return CartridgeManifest{}, fmt.Errorf("record %q belongs to cartridge %q, want %q", record.ID, record.CartridgeID, manifest.ID)
		}
		if record.Origin != CartridgeMethod && record.Origin != CartridgeDomain {
			return CartridgeManifest{}, fmt.Errorf("record %q uses private/transient origin %q", record.ID, record.Origin)
		}
		if seen[record.ID] {
			return CartridgeManifest{}, fmt.Errorf("duplicate record id %q", record.ID)
		}
		seen[record.ID] = true
		if err := ValidateCartridgeRecord(record); err != nil {
			return CartridgeManifest{}, fmt.Errorf("record %q: %w", record.ID, err)
		}
		manifest.Records[index] = record
	}
	return manifest, nil
}

func (s *CartridgeStore) ImportManifest(ctx context.Context, manifest CartridgeManifest) (CartridgeImportResult, error) {
	payload, err := json.Marshal(manifest)
	if err != nil {
		return CartridgeImportResult{}, err
	}
	validated, err := DecodeCartridgeManifest(payload)
	if err != nil {
		return CartridgeImportResult{}, err
	}
	result := CartridgeImportResult{ManifestID: validated.ID, Version: validated.Version}
	for _, record := range validated.Records {
		stored, upsertErr := s.Upsert(ctx, record)
		if upsertErr != nil {
			return CartridgeImportResult{}, fmt.Errorf("import record %q: %w", record.ID, upsertErr)
		}
		result.RecordIDs = append(result.RecordIDs, stored.ID)
	}
	return result, nil
}

func (s *CartridgeStore) ImportManifestFile(ctx context.Context, path string) (CartridgeImportResult, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return CartridgeImportResult{}, fmt.Errorf("read cartridge manifest: %w", err)
	}
	manifest, err := DecodeCartridgeManifest(payload)
	if err != nil {
		return CartridgeImportResult{}, err
	}
	return s.ImportManifest(ctx, manifest)
}
