package rag

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

var researchLedgerID = regexp.MustCompile(`(?i)^[ck][0-9]+$`)

const (
	DocumentSearchToolName = "document_search"
	DocumentReadToolName   = "document_read"
)

// RegistryWithDocumentTools returns an independent registry view with local
// document search/read capabilities installed. CLI and Desktop use the same
// assembly path so indexed context never advertises tools that are absent from
// the model-visible registry.
func RegistryWithDocumentTools(base *tools.Registry, store *DocumentStore, embedder Embedder) *tools.Registry {
	registry := tools.NewRegistry()
	if base != nil {
		for _, name := range base.Names() {
			if tool, ok := base.Get(name); ok {
				registry.Register(tool)
			}
		}
	}
	if store != nil {
		registry.Register(&DocumentSearchTool{Store: store, Embedder: embedder})
		read := &DocumentReadTool{Store: store}
		registry.Register(read)
		registerSourceCompare(registry, read)
	}
	return registry
}

type DocumentSearchTool struct {
	Store       *DocumentStore
	Embedder    Embedder
	Evidence    *EvidenceIndex
	Attachments *AttachmentIndex
}

func (t *DocumentSearchTool) EvidenceSources(args map[string]any, result tools.Result) []tools.EvidenceSource {
	if sourceID, _ := args["source_id"].(string); strings.HasPrefix(sourceID, AttachmentDocumentPrefix) {
		return nil
	}
	if t.Evidence != nil {
		if sourceID, _ := args["source_id"].(string); isEvidenceDocumentID(sourceID) {
			return t.Evidence.sourcesForIDs([]string{sourceID})
		}
	}
	if sourceID, _ := args["source_id"].(string); strings.TrimSpace(sourceID) != "" {
		return documentEvidenceSources(t.Store, args)
	}
	sources := documentSearchEvidenceSources(t.Store, result)
	return append(sources, t.Evidence.sourcesFromSearchResult(result)...)
}

func (t *DocumentSearchTool) Name() string { return DocumentSearchToolName }
func (t *DocumentSearchTool) Description() string {
	return "Search ingested documents, scoped attachments and observed web fragments with exact source coordinates. Omit source_id for cross-source RAG; use an exact doc-*, attachdoc-* or webdoc-* handle to restrict a follow-up search."
}
func (t *DocumentSearchTool) Kind() tools.Kind                 { return tools.KindReadOnly }
func (t *DocumentSearchTool) ParallelSafe(map[string]any) bool { return true }
func (t *DocumentSearchTool) SubtaskSafe() bool                { return true }
func (t *DocumentSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source_id": map[string]any{"type": "string", "description": "Optional exact doc-*, attachdoc-* or webdoc-* handle from a tool result. Omit for cross-source RAG; coverage IDs such as C1 are not source IDs."},
			"query":     map[string]any{"type": "string", "description": "Words or facts to find in the source."},
			"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": maxDocumentSearchLimit},
		},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}

// NormalizeArguments repairs the one unambiguous small-model wire mistake:
// coverage IDs belong to the research ledger and can never identify a
// content-addressed document. Removing C1/K1 selects the advertised all-source
// semantic search instead of spending a recovery turn on an internal ID mixup.
func (t *DocumentSearchTool) NormalizeArguments(args map[string]any) {
	sourceID, _ := args["source_id"].(string)
	if researchLedgerID.MatchString(strings.TrimSpace(sourceID)) {
		delete(args, "source_id")
	}
}
func (t *DocumentSearchTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	sourceID, _ := args["source_id"].(string)
	query, _ := args["query"].(string)
	limit := defaultDocumentSearchLimit
	switch value := args["limit"].(type) {
	case int:
		limit = value
	case float64:
		limit = int(value)
	}
	if limit <= 0 {
		limit = defaultDocumentSearchLimit
	}
	limit = min(limit, maxDocumentSearchLimit)
	if strings.TrimSpace(query) == "" {
		return documentToolError("query is required")
	}
	if t == nil || (t.Store == nil && t.Evidence == nil && t.Attachments == nil) {
		return documentToolError("document store is unavailable")
	}
	var matches []DocumentMatch
	var attachmentMatches []DocumentMatch
	var attachmentMeta []byte
	if strings.HasPrefix(sourceID, AttachmentDocumentPrefix) && t.Attachments == nil {
		return documentToolError("unknown session attachment source_id %q", sourceID)
	}
	if t.Attachments != nil && (strings.TrimSpace(sourceID) == "" || strings.HasPrefix(sourceID, AttachmentDocumentPrefix)) {
		var err error
		attachmentMatches, attachmentMeta, err = t.Attachments.Search(ctx, sourceID, query, limit)
		if err != nil {
			return documentToolError("%v", err)
		}
	}
	if strings.TrimSpace(sourceID) == "" {
		// ToolExecutor attaches only sources observed by successful tools in this
		// run. When that set exists, constrain research RAG to it; otherwise keep
		// the general local-document behavior used by pasted-file workflows.
		observed := tools.EvidenceSourcesFromContext(ctx)
		if t.Store != nil && t.Attachments == nil && len(observed) > 0 {
			sourceNames := make([]string, 0, len(observed))
			for _, source := range observed {
				if strings.TrimSpace(source.URL) != "" {
					sourceNames = append(sourceNames, source.URL)
				}
			}
			matches = t.Store.SearchSourcesHybrid(ctx, query, limit, t.Embedder, sourceNames)
		} else if t.Store != nil {
			matches = t.Store.SearchAllHybrid(ctx, query, limit, t.Embedder)
		}
		if t.Evidence != nil {
			matches = append(matches, t.Evidence.store.SearchAllHybrid(ctx, query, limit, t.Embedder)...)
			sort.SliceStable(matches, func(i, j int) bool { return documentMatchRanksBefore(matches[i], matches[j]) })
			if len(matches) > limit {
				matches = matches[:limit]
			}
		}
	} else if !strings.HasPrefix(sourceID, AttachmentDocumentPrefix) {
		store := documentOperandStore(t.Store, t.Evidence, sourceID)
		if store == nil {
			return documentToolError("unknown source_id %q; omit source_id to search all ingested sources", sourceID)
		}
		if _, ok := store.Get(sourceID); !ok {
			return documentToolError("unknown source_id %q; omit source_id to search all ingested sources", sourceID)
		}
		matches = store.SearchHybrid(ctx, sourceID, query, limit, t.Embedder)
	}
	if len(attachmentMatches) > 0 {
		// Provider rank scores and local lexical scores are different scales.
		// Keep provider order rather than pretend they form one calibrated score.
		matches = append(attachmentMatches, matches...)
		if len(matches) > limit {
			matches = matches[:limit]
		}
	}
	if len(matches) == 0 {
		scope := "the local source collection"
		if strings.TrimSpace(sourceID) != "" {
			scope = sourceID
		}
		return tools.Result{
			LLMContent: attachmentRetrievalMetadata(attachmentMeta) + fmt.Sprintf("No exact source excerpts matched %q in %s.", query, scope),
			Display:    "文件索引沒有符合片段",
		}
	}
	var b strings.Builder
	operands := []tools.SourceOperand{}
	seenOperands := map[string]bool{}
	b.WriteString(attachmentRetrievalMetadata(attachmentMeta))
	for _, match := range matches {
		if !seenOperands[match.SourceID] {
			operands = append(operands, observedDocumentOperand(ctx, &DocumentReadTool{Store: t.Store, Evidence: t.Evidence, Attachments: t.Attachments}, match.SourceID)...)
			seenOperands[match.SourceID] = true
		}
		score := fmt.Sprintf("lexical=%d", match.Score)
		if match.HybridScore > 0 {
			score = fmt.Sprintf("hybrid=%.3f semantic=%.3f lexical=%d", match.HybridScore, match.Semantic, match.Score)
		}
		excerpt := documentSearchExcerptWithSource(t.Store, match.SourceID, match.Excerpt)
		if passage, ok := t.Evidence.Passage(match.SourceID); ok {
			excerpt = evidenceExcerptWithSource(passage, match.Excerpt)
		}
		if strings.HasPrefix(match.SourceID, AttachmentDocumentPrefix) {
			if _, receipt, ok := t.Attachments.Get(ctx, match.SourceID); ok {
				excerpt = attachmentExcerpt(receipt, match.Excerpt)
			}
			score = fmt.Sprintf("provider_rank_score=%.6f lexical=%d", match.HybridScore, match.Score)
		}
		fmt.Fprintf(&b, "[%s:L%d-L%d chunk=%s %s]\n%s\n\n",
			match.SourceID, match.StartLine, match.EndLine,
			match.Chunk.ID, score, excerpt)
	}
	return tools.Result{
		LLMContent:     strings.TrimSpace(b.String()),
		Display:        fmt.Sprintf("找到 %d 個有座標的原文片段", len(matches)),
		SourceOperands: operands,
	}
}

func documentSearchExcerptWithSource(store *DocumentStore, sourceID, excerpt string) string {
	if document, ok := store.Get(sourceID); ok {
		parsed, err := url.Parse(strings.TrimSpace(document.Name))
		if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
			return "source_url: " + document.Name + "\n" + excerpt
		}
	}
	return excerpt
}

type DocumentReadTool struct {
	Store       *DocumentStore
	MaxLines    int
	Evidence    *EvidenceIndex
	Attachments *AttachmentIndex
}

func (t *DocumentReadTool) EvidenceSources(args map[string]any, _ tools.Result) []tools.EvidenceSource {
	if sourceID, _ := args["source_id"].(string); strings.HasPrefix(sourceID, AttachmentDocumentPrefix) {
		return nil
	}
	if sourceID, _ := args["source_id"].(string); isEvidenceDocumentID(sourceID) {
		return t.Evidence.sourcesForIDs([]string{sourceID})
	}
	return documentEvidenceSources(t.Store, args)
}

func (t *DocumentReadTool) Name() string { return DocumentReadToolName }
func (t *DocumentReadTool) Description() string {
	return "Read one exact chunk or bounded line range from an ingested local document."
}
func (t *DocumentReadTool) Kind() tools.Kind                 { return tools.KindReadOnly }
func (t *DocumentReadTool) ParallelSafe(map[string]any) bool { return true }
func (t *DocumentReadTool) SubtaskSafe() bool                { return true }
func (t *DocumentReadTool) Schema() map[string]any {
	lineDescription := "Preferred exact end line returned by document_search."
	if t != nil && t.MaxLines > 0 {
		lineDescription = fmt.Sprintf("Preferred exact end line returned by document_search; read at most %d lines per call.", t.MaxLines)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source_id":  map[string]any{"type": "string"},
			"chunk_id":   map[string]any{"type": "string", "description": "Read the whole search-result chunk when surrounding context is required."},
			"start_line": map[string]any{"type": "integer", "minimum": 1, "description": "Preferred exact start line returned by document_search."},
			"end_line":   map[string]any{"type": "integer", "minimum": 1, "description": lineDescription},
		},
		"required":             []string{"source_id"},
		"additionalProperties": false,
	}
}
func (t *DocumentReadTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	sourceID, _ := args["source_id"].(string)
	chunkID, _ := args["chunk_id"].(string)
	if strings.TrimSpace(sourceID) == "" {
		return documentToolError("source_id is required")
	}
	if t == nil || (t.Store == nil && t.Evidence == nil && t.Attachments == nil) {
		return documentToolError("document store is unavailable")
	}
	if strings.HasPrefix(sourceID, AttachmentDocumentPrefix) {
		if t.Attachments == nil {
			return documentToolError("unknown session attachment source_id %q", sourceID)
		}
		text, err := t.Attachments.Read(ctx, args, t.MaxLines)
		if err != nil {
			return documentToolError("%v", err)
		}
		return tools.Result{LLMContent: text, Display: "讀取附件原文座標", SourceOperands: observedDocumentOperand(ctx, t, sourceID)}
	}
	store := documentOperandStore(t.Store, t.Evidence, sourceID)
	if store == nil {
		return documentToolError("unknown source_id %q in this session", sourceID)
	}
	decorate := func(text string) string {
		if passage, ok := t.Evidence.Passage(sourceID); ok {
			return evidenceExcerptWithSource(passage, text)
		}
		return text
	}
	if strings.TrimSpace(chunkID) != "" {
		chunk, err := store.ReadChunk(sourceID, chunkID)
		if err != nil {
			return documentToolError("%v", err)
		}
		return tools.Result{
			LLMContent: fmt.Sprintf("[%s:L%d-L%d chunk=%s sha256=%s]\n%s",
				sourceID, chunk.StartLine, chunk.EndLine, chunk.ID, chunk.SHA256, decorate(chunk.Text)),
			Display:        fmt.Sprintf("讀取原文 L%d-L%d", chunk.StartLine, chunk.EndLine),
			SourceOperands: observedDocumentOperand(ctx, t, sourceID),
		}
	}
	startLine, startOK := documentIntegerArg(args["start_line"])
	endLine, endOK := documentIntegerArg(args["end_line"])
	if !startOK || !endOK {
		return documentToolError("provide chunk_id or both start_line and end_line")
	}
	if t.MaxLines > 0 && endLine-startLine+1 > t.MaxLines {
		return documentToolError("line range L%d-L%d is too large; call document_search with a focused query, then read at most %d lines", startLine, endLine, t.MaxLines)
	}
	text, err := store.ReadLines(sourceID, startLine, endLine)
	if err != nil {
		return documentToolError("%v", err)
	}
	return tools.Result{
		LLMContent:     fmt.Sprintf("[%s:L%d-L%d]\n%s", sourceID, startLine, endLine, decorate(text)),
		Display:        fmt.Sprintf("讀取原文 L%d-L%d", startLine, endLine),
		SourceOperands: observedDocumentOperand(ctx, t, sourceID),
	}
}

func documentIntegerArg(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case float64:
		integer := int(typed)
		return integer, typed == float64(integer)
	default:
		return 0, false
	}
}

func documentToolError(format string, values ...any) tools.Result {
	message := fmt.Sprintf(format, values...)
	return tools.Result{LLMContent: "ERROR: " + message, Display: message, Err: fmt.Errorf("%s", message)}
}

func documentEvidenceSources(store *DocumentStore, args map[string]any) []tools.EvidenceSource {
	if store == nil {
		return nil
	}
	sourceID, _ := args["source_id"].(string)
	document, ok := store.Get(strings.TrimSpace(sourceID))
	if !ok {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(document.Name))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil
	}
	return []tools.EvidenceSource{{Title: parsed.Host, URL: document.Name}}
}

func documentSearchEvidenceSources(store *DocumentStore, result tools.Result) []tools.EvidenceSource {
	if store == nil {
		return nil
	}
	seen := make(map[string]bool)
	var sources []tools.EvidenceSource
	for _, match := range regexp.MustCompile(`\[(doc-[a-f0-9]+):L`).FindAllStringSubmatch(result.LLMContent, -1) {
		if len(match) < 2 || seen[match[1]] {
			continue
		}
		document, ok := store.Get(match[1])
		if !ok {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(document.Name))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			continue
		}
		seen[match[1]] = true
		sources = append(sources, tools.EvidenceSource{Title: parsed.Host, URL: document.Name})
	}
	return sources
}
