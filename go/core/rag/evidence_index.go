package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

var evidenceDocumentHeader = regexp.MustCompile(`\[(webdoc-[a-f0-9]{64}):L`)

const (
	EvidenceDocumentPrefix     = "webdoc-"
	maxEvidenceIndexBytes      = 16 * 1024 * 1024
	maxEvidenceIndexDocuments  = 256
	maxEvidencePassageBytes    = 256 * 1024
	maxEvidencePassagesPerCall = 32
)

// EvidenceIndex owns only the returned source fragments observed by one native
// runtime. It is memory-only, survives same-session Rebind, and is never the
// shared attachment store or durable knowledge. An unobserved webdoc-* handle
// cannot fall back to that global store.
type EvidenceIndex struct {
	mu       sync.RWMutex
	store    *DocumentStore
	passages map[string]tools.EvidencePassage
	bytes    int
}

type EvidenceIndexReceipt struct {
	SourceID    string               `json:"source_id"`
	Source      tools.EvidenceSource `json:"source"`
	TextSHA256  string               `json:"text_sha256"`
	RetrievedAt string               `json:"retrieved_at,omitempty"`
	Section     string               `json:"section,omitempty"`
	Locator     json.RawMessage      `json:"provider_locator,omitempty"`
	Coverage    json.RawMessage      `json:"coverage,omitempty"`
	Scope       string               `json:"scope"`
	Bytes       int                  `json:"bytes"`
	StartLine   int                  `json:"start_line"`
	EndLine     int                  `json:"end_line"`
	Reused      bool                 `json:"reused"`
	CacheHit    bool                 `json:"provider_cache_hit,omitempty"`
}

func NewEvidenceIndex() *EvidenceIndex {
	return &EvidenceIndex{store: NewDocumentStore(""), passages: map[string]tools.EvidencePassage{}}
}

func evidenceDocumentID(passage tools.EvidencePassage) string {
	hash := sha256.Sum256([]byte(passage.Source.URL + "\x00" + passage.Source.SourceVersion + "\x00" + passage.TextSHA256 + "\x00" + string(passage.Locator)))
	return EvidenceDocumentPrefix + hex.EncodeToString(hash[:])
}

func prepareEvidencePassage(passage tools.EvidencePassage) (tools.EvidencePassage, error) {
	if passage.Source.ContentKind != "body" || passage.Source.FetchError != "" {
		return passage, fmt.Errorf("only successful extracted body fragments can be indexed")
	}
	if len(passage.Text) > maxEvidencePassageBytes || strings.TrimSpace(passage.Text) == "" {
		return passage, fmt.Errorf("source fragment is empty or exceeds 256 KiB")
	}
	if len(passage.Locator) > 4096 || len(passage.Coverage) > 8192 ||
		(len(passage.Locator) > 0 && !json.Valid(passage.Locator)) ||
		(len(passage.Coverage) > 0 && !json.Valid(passage.Coverage)) {
		return passage, fmt.Errorf("source locator/coverage metadata is invalid or oversized")
	}
	if len(passage.Section) > 2048 || len(passage.RetrievedAt) > 80 {
		return passage, fmt.Errorf("source section/retrieval metadata is oversized")
	}
	source, ok := tools.NormalizeEvidenceSource(passage.Source)
	if !ok {
		return passage, fmt.Errorf("source URL is invalid")
	}
	if passage.Source.SourceVersion != "" && source.SourceVersion == "" {
		return passage, fmt.Errorf("provider source version is invalid; original observation preserved")
	}
	if strings.TrimSpace(passage.Source.FetchedAt) != "" && source.FetchedAt == "" {
		return passage, fmt.Errorf("provider source timestamp is invalid; original observation preserved")
	}
	// The UI snippet is not the indexing operand. Keep the exact returned text
	// separate and bind its identity before newline normalization for chunking.
	passage.Source = source
	hash := sha256.Sum256([]byte(passage.Text))
	actualHash := hex.EncodeToString(hash[:])
	if passage.TextSHA256 != "" && passage.TextSHA256 != actualHash {
		return passage, fmt.Errorf("source fragment hash does not match returned text")
	}
	passage.TextSHA256 = actualHash
	if passage.Scope == "" || len(passage.Scope) > 80 {
		return passage, fmt.Errorf("source fragment scope is missing or oversized")
	}
	return tools.CloneEvidencePassages([]tools.EvidencePassage{passage})[0], nil
}

// Ingest atomically admits a bounded successful observation. A cancellation,
// malformed receipt or capacity failure publishes none of this call's docs.
func (index *EvidenceIndex) Ingest(ctx context.Context, incoming []tools.EvidencePassage) ([]EvidenceIndexReceipt, error) {
	if index == nil || index.store == nil {
		return nil, fmt.Errorf("source evidence index unavailable")
	}
	if len(incoming) > maxEvidencePassagesPerCall {
		return nil, fmt.Errorf("source observation exceeds 32 fragments")
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	newDocs := map[string]*Document{}
	newPassages := map[string]tools.EvidencePassage{}
	receipts := make([]EvidenceIndexReceipt, 0, len(incoming))
	addedBytes := 0
	for _, raw := range incoming {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		passage, err := prepareEvidencePassage(raw)
		if err != nil {
			return nil, err
		}
		id := evidenceDocumentID(passage)
		document, reused := index.store.Get(id)
		if document == nil {
			document = newDocs[id]
			reused = document != nil
		}
		if document == nil {
			text := NormalizeDocumentText(passage.Text)
			chunks, err := buildDocumentChunks(ctx, id, text, DefaultDocumentChunkTokens)
			if err != nil {
				return nil, err
			}
			hash := sha256.Sum256([]byte(text))
			document = &Document{Schema: 1, ID: id, Name: passage.Source.URL,
				SHA256: hex.EncodeToString(hash[:]), Bytes: len(text), Lines: lineCount(text),
				ApproxTokens: provider.EstimateTextTokens(text), Text: text, Chunks: chunks}
			newDocs[id] = document
			addedBytes += len(passage.Text)
		}
		// The source operand remains identical, while cache/coverage/receipt
		// metadata may change on a later successful observation of that operand.
		// Stage both so the returned manifest and future reads agree atomically.
		newPassages[id] = passage
		if len(index.passages)+len(newDocs) > maxEvidenceIndexDocuments || index.bytes+addedBytes > maxEvidenceIndexBytes {
			return nil, fmt.Errorf("session source index capacity reached; original tool observation is preserved")
		}
		receipts = append(receipts, EvidenceIndexReceipt{SourceID: id, Source: passage.Source,
			TextSHA256: passage.TextSHA256, RetrievedAt: passage.RetrievedAt, Section: passage.Section,
			Locator: append(json.RawMessage(nil), passage.Locator...), Coverage: append(json.RawMessage(nil), passage.Coverage...), Scope: passage.Scope,
			Bytes: len(passage.Text), StartLine: 1, EndLine: document.Lines, Reused: reused, CacheHit: passage.CacheHit})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(receipts)
	if err != nil || len(encoded) > maxEvidenceManifestBytes-1024 {
		return nil, fmt.Errorf("source-coordinate receipt exceeds 32 KiB; original observation is preserved")
	}
	index.store.mu.Lock()
	for id, document := range newDocs {
		index.store.docs[id] = document
	}
	index.store.mu.Unlock()
	for id, passage := range newPassages {
		index.passages[id] = passage
	}
	index.bytes += addedBytes
	return receipts, nil
}

func (index *EvidenceIndex) Passage(id string) (tools.EvidencePassage, bool) {
	if index == nil {
		return tools.EvidencePassage{}, false
	}
	index.mu.RLock()
	defer index.mu.RUnlock()
	passage, ok := index.passages[id]
	if !ok {
		return tools.EvidencePassage{}, false
	}
	return tools.CloneEvidencePassages([]tools.EvidencePassage{passage})[0], true
}

func (index *EvidenceIndex) Get(id string) (*Document, bool) {
	if index == nil {
		return nil, false
	}
	if _, ok := index.Passage(id); !ok {
		return nil, false
	}
	return index.store.Get(id)
}

func documentOperandStore(primary *DocumentStore, index *EvidenceIndex, id string) *DocumentStore {
	if !isEvidenceDocumentID(id) {
		return primary
	}
	if _, ok := index.Get(id); !ok {
		return nil
	}
	return index.store
}

func evidenceExcerptWithSource(passage tools.EvidencePassage, excerpt string) string {
	metadata := passage
	metadata.Text = ""
	encoded, _ := json.Marshal(metadata)
	return "source_url: " + passage.Source.URL + "\nsource_metadata: " + string(encoded) + "\n" + excerpt
}

func (index *EvidenceIndex) sourcesForIDs(ids []string) []tools.EvidenceSource {
	if index == nil {
		return nil
	}
	seen := map[string]bool{}
	sources := []tools.EvidenceSource{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if passage, ok := index.Passage(id); ok {
			sources = append(sources, passage.Source)
		}
	}
	return sources
}

func (index *EvidenceIndex) sourcesFromSearchResult(result tools.Result) []tools.EvidenceSource {
	if index == nil || result.Err != nil {
		return nil
	}
	ids := []string{}
	for _, match := range evidenceDocumentHeader.FindAllStringSubmatch(result.LLMContent, -1) {
		if len(match) > 1 {
			ids = append(ids, match[1])
		}
	}
	return index.sourcesForIDs(ids)
}
