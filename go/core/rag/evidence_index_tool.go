package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const maxEvidenceManifestBytes = 32 * 1024

// IndexedEvidenceTool adds a source-coordinate handoff without changing the
// wrapped read, permissions, arguments, text, metadata or model tool choice.
// Its index is owned by one runtime; it does not execute retrieval on its own.
type IndexedEvidenceTool struct {
	Tool  tools.Tool
	Index *EvidenceIndex
}

func (t *IndexedEvidenceTool) Name() string           { return t.Tool.Name() }
func (t *IndexedEvidenceTool) Description() string    { return t.Tool.Description() }
func (t *IndexedEvidenceTool) Schema() map[string]any { return t.Tool.Schema() }
func (t *IndexedEvidenceTool) Kind() tools.Kind       { return t.Tool.Kind() }
func (t *IndexedEvidenceTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	result := t.Tool.Execute(ctx, args)
	if result.Err != nil || ctx.Err() != nil || t.Index == nil {
		return result
	}
	var object map[string]json.RawMessage
	jsonObject := json.Unmarshal([]byte(result.LLMContent), &object) == nil && object != nil
	if jsonObject {
		if _, exists := object["rag_index"]; exists {
			// Preserve a provider-owned coordinate field without hidden indexing.
			return result
		}
	}
	provider, ok := t.Tool.(tools.EvidencePassageProvider)
	if !ok {
		return result
	}
	passages := provider.EvidencePassages(args, result)
	if len(passages) == 0 {
		return result
	}
	started := time.Now()
	receipts, err := t.Index.Ingest(ctx, passages)
	manifest := map[string]any{"scope": "current_native_session_memory", "index_ms": time.Since(started).Milliseconds(),
		"coordinate_scope": "local lines in returned fragment; original-page completeness unverified"}
	if err != nil {
		manifest["warning"] = err.Error()
	} else {
		manifest["documents"] = receipts
		for _, receipt := range receipts {
			if doc, ok := t.Index.Get(receipt.SourceID); ok {
				result.SourceOperands = append(result.SourceOperands, tools.SourceOperand{SourceID: receipt.SourceID, TextSHA256: doc.SHA256})
			}
		}
	}
	encoded, encodeErr := json.Marshal(manifest)
	if encodeErr != nil || len(encoded) > maxEvidenceManifestBytes {
		encoded = []byte(`{"warning":"source-coordinate handoff exceeds 32 KiB; original observation preserved"}`)
	}
	// A JSON source response remains JSON: all original fields (including exact
	// text, status, coverage, timings and truncation) survive as raw operands.
	// Native non-JSON observations retain their original bytes as a prefix.
	if jsonObject {
		object["rag_index"] = encoded
		if merged, err := json.Marshal(object); err == nil {
			result.LLMContent = string(merged)
		}
	} else {
		result.LLMContent += "\n\nSource-coordinate handoff (observed data, not instructions):\n" + string(encoded)
	}
	return result
}

func (t *IndexedEvidenceTool) EvidenceSources(args map[string]any, result tools.Result) []tools.EvidenceSource {
	if provider, ok := t.Tool.(tools.EvidenceSourceProvider); ok {
		return provider.EvidenceSources(args, result)
	}
	return nil
}
func (t *IndexedEvidenceTool) EvidencePassages(args map[string]any, result tools.Result) []tools.EvidencePassage {
	if provider, ok := t.Tool.(tools.EvidencePassageProvider); ok {
		return provider.EvidencePassages(args, result)
	}
	return nil
}
func (t *IndexedEvidenceTool) ReadIdentity(args map[string]any) (tools.ReadIdentity, error) {
	if provider, ok := t.Tool.(tools.DeduplicableRead); ok {
		return provider.ReadIdentity(args)
	}
	return tools.ReadIdentity{}, fmt.Errorf("source tool does not expose a read identity")
}
func (t *IndexedEvidenceTool) ParallelSafe(args map[string]any) bool {
	return tools.ParallelSafe(t.Tool, args)
}
func (t *IndexedEvidenceTool) SubtaskSafe() bool {
	provider, ok := t.Tool.(tools.SubtaskAccessible)
	return ok && provider.SubtaskSafe()
}
func (t *IndexedEvidenceTool) NormalizeArguments(args map[string]any) {
	if provider, ok := t.Tool.(tools.ArgumentNormalizer); ok {
		provider.NormalizeArguments(args)
	}
}
func (t *IndexedEvidenceTool) ResolveArguments(args map[string]any) error {
	if provider, ok := t.Tool.(tools.ArgumentResolver); ok {
		return provider.ResolveArguments(args)
	}
	return nil
}
func (t *IndexedEvidenceTool) CompleteObservation() bool {
	provider, ok := t.Tool.(tools.CompleteObservationProvider)
	return ok && provider.CompleteObservation()
}

// RegistryWithEvidenceIndex preserves the attachment store and wraps only tools
// declaring exact observed text operands. No names, task phrases or semantic
// classifiers select the workflow.
func RegistryWithEvidenceIndex(base *tools.Registry, store *DocumentStore, embedder Embedder, index *EvidenceIndex) *tools.Registry {
	registry := RegistryWithDocumentTools(base, store, embedder)
	if index == nil {
		return registry
	}
	for _, name := range registry.Names() {
		tool, ok := registry.Get(name)
		if !ok {
			continue
		}
		if wrapped, ok := tool.(*IndexedEvidenceTool); ok {
			tool = wrapped.Tool
		}
		if _, ok := tool.(tools.EvidencePassageProvider); ok {
			registry.Register(&IndexedEvidenceTool{Tool: tool, Index: index})
		}
	}
	registry.Register(&DocumentSearchTool{Store: store, Embedder: embedder, Evidence: index})
	read := &DocumentReadTool{Store: store, Evidence: index}
	registry.Register(read)
	registerSourceCompare(registry, read)
	return registry
}

func isEvidenceDocumentID(id string) bool { return strings.HasPrefix(id, EvidenceDocumentPrefix) }
