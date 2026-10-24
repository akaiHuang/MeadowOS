package rag

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var knowledgeEntryIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

type Audience string

const (
	AudienceGeneral      Audience = "general"
	AudienceHomemaker    Audience = "homemaker"
	AudienceMiddleSchool Audience = "middle_school"
	AudienceHighSchool   Audience = "high_school"
	AudienceUniversity   Audience = "university"
	AudienceWhiteCollar  Audience = "white_collar"
	AudienceBlueCollar   Audience = "blue_collar"
)

type Difficulty string

const (
	DifficultyBasic        Difficulty = "basic"
	DifficultyIntermediate Difficulty = "intermediate"
	DifficultyAdvanced     Difficulty = "advanced"
)

type KnowledgeSource struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Publisher   string `json:"publisher"`
	Kind        string `json:"kind"`
	RetrievedAt string `json:"retrieved_at"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	License     string `json:"license,omitempty"`
	StartLine   int    `json:"start_line,omitempty"`
	EndLine     int    `json:"end_line,omitempty"`
}

// KnowledgeEntry is the canonical encyclopedia unit. Body remains extractive
// source material; Summary is model-authored only after evidence validation.
// Audience and difficulty are retrieval filters, never hidden prompt guesses.
type KnowledgeEntry struct {
	Schema       int               `json:"schema"`
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Domain       string            `json:"domain"`
	Topics       []string          `json:"topics"`
	Audiences    []Audience        `json:"audiences"`
	Difficulty   Difficulty        `json:"difficulty"`
	Locale       string            `json:"locale"`
	Summary      string            `json:"summary"`
	Body         string            `json:"body"`
	ContentMode  string            `json:"content_mode,omitempty"`
	Sources      []KnowledgeSource `json:"sources"`
	SafetyLevel  string            `json:"safety_level"`
	ReviewStatus string            `json:"review_status"`
	ReviewedAt   string            `json:"reviewed_at,omitempty"`
	ExpiresAt    string            `json:"expires_at,omitempty"`
	DocumentID   string            `json:"document_id"`
}

type KnowledgeQuery struct {
	Text          string
	Audience      Audience
	MaxDifficulty Difficulty
	Locale        string
	Limit         int
}

type KnowledgeMatch struct {
	Entry    KnowledgeEntry
	Evidence DocumentMatch
	Score    float64
}

type KnowledgeImportReport struct {
	Accepted int      `json:"accepted"`
	Rejected int      `json:"rejected"`
	IDs      []string `json:"ids,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

type KnowledgeStats struct {
	Total      int            `json:"total"`
	Reviewed   int            `json:"reviewed"`
	Draft      int            `json:"draft"`
	Expired    int            `json:"expired"`
	Audiences  map[string]int `json:"audiences"`
	Difficulty map[string]int `json:"difficulty"`
}

type EncyclopediaStore struct {
	dir       string
	documents *DocumentStore
	embedder  Embedder
	mu        sync.RWMutex
	entries   map[string]KnowledgeEntry
	vectors   *vectorCache
}

func NewEncyclopediaStore(dir string, documents *DocumentStore, embedder Embedder) *EncyclopediaStore {
	if documents == nil {
		documents = NewDocumentStore(filepath.Join(strings.TrimSpace(dir), "documents"))
	}
	store := &EncyclopediaStore{
		dir: strings.TrimSpace(dir), documents: documents, embedder: embedder,
		entries: make(map[string]KnowledgeEntry),
		vectors: newVectorCache(),
	}
	// Constructor remains fail-open for existing call sites; product startup can
	// call Load explicitly and surface corruption. Every valid entry is still
	// available if one sibling JSON file is damaged.
	_ = store.Load(context.Background())
	return store
}

// Load restores reviewed encyclopedia entries after process restart and
// verifies their content-addressed document indexes. Missing indexes are
// rebuilt from the persisted extractive body; malformed entries are returned
// as one joined error rather than silently becoming citable.
func (s *EncyclopediaStore) Load(ctx context.Context) error {
	if s == nil || s.dir == "" {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(s.dir, "entries", "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	var failures []error
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, readErr := os.ReadFile(path)
		if readErr != nil {
			failures = append(failures, fmt.Errorf("read %s: %w", path, readErr))
			continue
		}
		var entry KnowledgeEntry
		if decodeErr := json.Unmarshal(payload, &entry); decodeErr != nil {
			failures = append(failures, fmt.Errorf("decode %s: %w", path, decodeErr))
			continue
		}
		entry = normalizeKnowledgeEntry(entry)
		if validateErr := validateKnowledgeEntry(entry); validateErr != nil {
			failures = append(failures, fmt.Errorf("validate %s: %w", path, validateErr))
			continue
		}
		expectedID := DocumentID(entry.Body)
		if entry.DocumentID != "" && entry.DocumentID != expectedID {
			failures = append(failures, fmt.Errorf("validate %s: document_id does not match body", path))
			continue
		}
		if _, ok := s.documents.Get(expectedID); !ok {
			document, ingestErr := s.documents.Ingest(ctx, entry.ID, entry.Body, nil)
			if ingestErr != nil {
				failures = append(failures, fmt.Errorf("rebuild %s: %w", path, ingestErr))
				continue
			}
			entry.DocumentID = document.ID
		} else {
			entry.DocumentID = expectedID
		}
		s.mu.Lock()
		s.entries[entry.ID] = entry
		s.mu.Unlock()
	}
	return errors.Join(failures...)
}

func (s *EncyclopediaStore) Ingest(ctx context.Context, entry KnowledgeEntry) (KnowledgeEntry, error) {
	if s == nil {
		return KnowledgeEntry{}, fmt.Errorf("encyclopedia store is nil")
	}
	entry = normalizeKnowledgeEntry(entry)
	if err := validateKnowledgeEntry(entry); err != nil {
		return KnowledgeEntry{}, err
	}
	document, err := s.documents.Ingest(ctx, entry.ID, entry.Body, nil)
	if err != nil {
		return KnowledgeEntry{}, err
	}
	entry.Schema = 1
	entry.DocumentID = document.ID
	s.mu.Lock()
	s.entries[entry.ID] = entry
	s.mu.Unlock()
	if err := s.persistEntry(entry); err != nil {
		return KnowledgeEntry{}, err
	}
	return entry, nil
}

// ImportJSONL is the administrative bulk-ingestion boundary. Every line is an
// independently validated canonical entry, so one malformed record cannot
// make its siblings citable or corrupt an existing reviewed entry.
func (s *EncyclopediaStore) ImportJSONL(ctx context.Context, reader io.Reader) (KnowledgeImportReport, error) {
	var report KnowledgeImportReport
	if s == nil || reader == nil {
		return report, fmt.Errorf("encyclopedia store and JSONL reader are required")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return report, err
		}
		payload := strings.TrimSpace(scanner.Text())
		if payload == "" {
			continue
		}
		var entry KnowledgeEntry
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			report.Rejected++
			report.Errors = append(report.Errors, fmt.Sprintf("line %d: decode: %v", line, err))
			continue
		}
		stored, err := s.Ingest(ctx, entry)
		if err != nil {
			report.Rejected++
			report.Errors = append(report.Errors, fmt.Sprintf("line %d id=%q: %v", line, entry.ID, err))
			continue
		}
		report.Accepted++
		report.IDs = append(report.IDs, stored.ID)
	}
	if err := scanner.Err(); err != nil {
		return report, err
	}
	return report, nil
}

func ValidateKnowledgeEntry(entry KnowledgeEntry) error {
	return validateKnowledgeEntry(normalizeKnowledgeEntry(entry))
}

func (s *EncyclopediaStore) Stats(now time.Time) KnowledgeStats {
	stats := KnowledgeStats{Audiences: make(map[string]int), Difficulty: make(map[string]int)}
	if s == nil {
		return stats
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, entry := range s.entries {
		stats.Total++
		if entry.ReviewStatus == "reviewed" {
			stats.Reviewed++
		} else {
			stats.Draft++
		}
		if knowledgeEntryExpired(entry, now) {
			stats.Expired++
		}
		for _, audience := range entry.Audiences {
			stats.Audiences[string(audience)]++
		}
		stats.Difficulty[string(entry.Difficulty)]++
	}
	return stats
}

func (s *EncyclopediaStore) Search(ctx context.Context, query KnowledgeQuery) []KnowledgeMatch {
	if s == nil || strings.TrimSpace(query.Text) == "" {
		return nil
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 12 {
		limit = 12
	}
	s.mu.RLock()
	entries := make([]KnowledgeEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		if knowledgeEntryInScope(entry, query) {
			entries = append(entries, entry)
		}
	}
	s.mu.RUnlock()
	if len(entries) == 0 {
		return nil
	}
	semantic := make(map[string]float64, len(entries))
	if s.embedder != nil {
		keys := make([]string, 0, len(entries))
		texts := make([]string, 0, len(entries))
		for _, entry := range entries {
			text := knowledgeEmbeddingText(entry)
			digest := sha256.Sum256([]byte(text))
			keys = append(keys, entry.ID+":"+hex.EncodeToString(digest[:]))
			texts = append(texts, text)
		}
		if queryVector, vectors, err := s.vectors.embedQueryAndItems(ctx, s.embedder, query.Text, keys, texts); err == nil {
			for index, entry := range entries {
				semantic[entry.ID] = cosineFloat32(queryVector, vectors[index])
			}
		}
	}
	terms := documentTerms(query.Text)
	result := make([]KnowledgeMatch, 0, len(entries))
	for _, entry := range entries {
		lexical := documentMatchScore(knowledgeEmbeddingText(entry), query.Text, terms)
		sem := semantic[entry.ID]
		if sem < 0.35 && lexical <= 0 {
			continue
		}
		chunkMatches := s.documents.SearchHybrid(ctx, entry.DocumentID, query.Text, 1, s.embedder)
		if len(chunkMatches) == 0 {
			chunkMatches = s.documents.Search(entry.DocumentID, "", 1)
		}
		if len(chunkMatches) == 0 {
			continue
		}
		lexicalNormalized := float64(min(lexical, 12)) / 12.0
		score := 0.7*sem + 0.3*lexicalNormalized
		if knowledgeAudienceContains(entry.Audiences, query.Audience) {
			score += 0.08
		}
		result = append(result, KnowledgeMatch{Entry: entry, Evidence: chunkMatches[0], Score: score})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		return result[i].Entry.ID < result[j].Entry.ID
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func normalizeKnowledgeEntry(entry KnowledgeEntry) KnowledgeEntry {
	entry.ID = strings.TrimSpace(entry.ID)
	entry.Title = strings.TrimSpace(entry.Title)
	entry.Domain = strings.TrimSpace(entry.Domain)
	entry.Locale = strings.TrimSpace(entry.Locale)
	entry.Summary = strings.TrimSpace(entry.Summary)
	entry.Body = NormalizeDocumentText(entry.Body)
	entry.ContentMode = strings.TrimSpace(entry.ContentMode)
	if entry.ContentMode == "" {
		entry.ContentMode = "excerpt"
	}
	entry.SafetyLevel = strings.TrimSpace(entry.SafetyLevel)
	entry.ReviewStatus = strings.TrimSpace(entry.ReviewStatus)
	entry.Topics = uniqueDocumentStrings(entry.Topics)
	seenAudience := make(map[Audience]bool)
	audiences := make([]Audience, 0, len(entry.Audiences))
	for _, audience := range entry.Audiences {
		if audience != "" && !seenAudience[audience] {
			seenAudience[audience] = true
			audiences = append(audiences, audience)
		}
	}
	entry.Audiences = audiences
	for index := range entry.Sources {
		entry.Sources[index].Title = strings.TrimSpace(entry.Sources[index].Title)
		entry.Sources[index].URL = strings.TrimSpace(entry.Sources[index].URL)
		entry.Sources[index].Publisher = strings.TrimSpace(entry.Sources[index].Publisher)
		entry.Sources[index].Kind = strings.TrimSpace(entry.Sources[index].Kind)
	}
	return entry
}

func validateKnowledgeEntry(entry KnowledgeEntry) error {
	if entry.ID == "" || entry.Title == "" || entry.Domain == "" || strings.TrimSpace(entry.Body) == "" {
		return fmt.Errorf("knowledge entry requires id, title, domain, and body")
	}
	if !knowledgeEntryIDRe.MatchString(entry.ID) {
		return fmt.Errorf("knowledge entry id must be a safe lowercase slug")
	}
	if len(entry.Audiences) == 0 || !validKnowledgeDifficulty(entry.Difficulty) {
		return fmt.Errorf("knowledge entry requires a supported audience and difficulty")
	}
	if entry.Locale == "" || len(entry.Sources) == 0 {
		return fmt.Errorf("knowledge entry requires locale and at least one source")
	}
	if entry.ContentMode != "excerpt" && entry.ContentMode != "full_text" {
		return fmt.Errorf("content_mode must be excerpt or full_text")
	}
	if entry.ReviewStatus != "draft" && entry.ReviewStatus != "reviewed" {
		return fmt.Errorf("review_status must be draft or reviewed")
	}
	for _, audience := range entry.Audiences {
		if !validKnowledgeAudience(audience) {
			return fmt.Errorf("unsupported audience %q", audience)
		}
	}
	for _, source := range entry.Sources {
		parsed, err := url.Parse(source.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("knowledge source must be an http(s) URL: %q", source.URL)
		}
		if source.Title == "" || source.Publisher == "" || source.RetrievedAt == "" {
			return fmt.Errorf("knowledge source requires title, publisher, and retrieved_at")
		}
		if source.Kind != "primary" && source.Kind != "official" && source.Kind != "secondary" {
			return fmt.Errorf("unsupported knowledge source kind %q", source.Kind)
		}
		if _, err := time.Parse("2006-01-02", source.RetrievedAt); err != nil {
			return fmt.Errorf("retrieved_at must be YYYY-MM-DD")
		}
		if (source.StartLine > 0 || source.EndLine > 0) && (source.StartLine < 1 || source.EndLine < source.StartLine) {
			return fmt.Errorf("knowledge source line coordinates must be a positive ordered range")
		}
		if entry.ContentMode == "full_text" && strings.TrimSpace(source.License) == "" {
			return fmt.Errorf("full_text knowledge requires a source license")
		}
	}
	if entry.SafetyLevel == "high" && entry.ReviewStatus != "reviewed" {
		return fmt.Errorf("high-safety knowledge must be human reviewed before publication")
	}
	for field, value := range map[string]string{
		"reviewed_at": entry.ReviewedAt, "expires_at": entry.ExpiresAt,
	} {
		if value != "" {
			if _, err := time.Parse("2006-01-02", value); err != nil {
				return fmt.Errorf("%s must be YYYY-MM-DD", field)
			}
		}
	}
	return nil
}

func knowledgeEntryInScope(entry KnowledgeEntry, query KnowledgeQuery) bool {
	if entry.ReviewStatus != "reviewed" || knowledgeEntryExpired(entry, time.Now()) {
		return false
	}
	if query.Audience != "" && query.Audience != AudienceGeneral &&
		!knowledgeAudienceContains(entry.Audiences, query.Audience) &&
		!knowledgeAudienceContains(entry.Audiences, AudienceGeneral) {
		return false
	}
	if query.Locale != "" && entry.Locale != query.Locale && !strings.HasPrefix(entry.Locale, strings.Split(query.Locale, "-")[0]) {
		return false
	}
	return difficultyRank(entry.Difficulty) <= difficultyRank(query.MaxDifficulty) || query.MaxDifficulty == ""
}

func knowledgeEntryExpired(entry KnowledgeEntry, now time.Time) bool {
	if strings.TrimSpace(entry.ExpiresAt) == "" {
		return false
	}
	expires, err := time.Parse("2006-01-02", entry.ExpiresAt)
	if err != nil {
		return true
	}
	return expires.Before(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC))
}

func knowledgeEmbeddingText(entry KnowledgeEntry) string {
	return entry.Title + "\n" + entry.Domain + "\n" + strings.Join(entry.Topics, " ") + "\n" + entry.Summary + "\n" + truncateRunes(entry.Body, 2000)
}

func validKnowledgeAudience(value Audience) bool {
	switch value {
	case AudienceGeneral, AudienceHomemaker, AudienceMiddleSchool, AudienceHighSchool,
		AudienceUniversity, AudienceWhiteCollar, AudienceBlueCollar:
		return true
	default:
		return false
	}
}

func validKnowledgeDifficulty(value Difficulty) bool {
	return value == DifficultyBasic || value == DifficultyIntermediate || value == DifficultyAdvanced
}

func difficultyRank(value Difficulty) int {
	switch value {
	case DifficultyBasic:
		return 1
	case DifficultyIntermediate:
		return 2
	case DifficultyAdvanced:
		return 3
	default:
		return 3
	}
}

func knowledgeAudienceContains(values []Audience, target Audience) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func uniqueDocumentStrings(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func (s *EncyclopediaStore) persistEntry(entry KnowledgeEntry) error {
	if s.dir == "" {
		return nil
	}
	dir := filepath.Join(s.dir, "entries")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, entry.ID+".json"), append(payload, '\n'), 0o600)
}
