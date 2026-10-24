package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func (index *AttachmentIndex) Search(ctx context.Context, sourceID, query string, limit int) ([]DocumentMatch, json.RawMessage, error) {
	if index == nil {
		return nil, nil, nil
	}
	release, err := index.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	if sourceID != "" && index.manifest.Documents[sourceID] == nil {
		return nil, nil, fmt.Errorf("unknown session attachment source_id %q", sourceID)
	}
	if limit <= 0 {
		limit = defaultDocumentSearchLimit
	}
	limit = min(limit, maxDocumentSearchLimit)
	meta := map[string]any{"scope": "current_chat_attachments", "coordinate_scope": attachmentCoordinateScope, "score_is_confidence": false, "embedding_policy": "provider_request_local; actual ranking capability is reported in provider metadata"}
	var matches []DocumentMatch
	if index.service != nil && index.manifest.Collection != "" {
		response, serviceErr := index.service.Search(ctx, index.manifest.Collection, query, 20)
		if serviceErr == nil {
			meta["provider"] = response.Meta
			ignored := 0
			for _, hit := range response.Results {
				if hit.Collection != index.manifest.Collection {
					ignored++
					continue
				}
				var ownerID string
				var remote attachmentRemoteChunk
				for id, record := range index.manifest.Documents {
					if sourceID != "" && id != sourceID {
						continue
					}
					for _, candidate := range record.Remote {
						if candidate.RecordID == hit.ID {
							ownerID, remote = id, candidate
							break
						}
					}
					if ownerID != "" {
						break
					}
				}
				if ownerID == "" || hit.SourceVersion != remote.Version || hit.Revision != remote.Revision {
					ignored++
					continue
				}
				record := index.manifest.Documents[ownerID]
				doc, ok := index.store.Get(record.DocumentID)
				if !ok {
					ignored++
					continue
				}
				var chunk DocumentChunk
				for _, candidate := range doc.Chunks {
					if candidate.ID == remote.ChunkID {
						chunk = candidate
						break
					}
				}
				var coverage struct {
					Start     int    `json:"start_char"`
					End       int    `json:"end_char"`
					Unit      string `json:"character_unit"`
					Rewritten bool   `json:"text_rewritten"`
				}
				runes := []rune(chunk.Text)
				if json.Unmarshal(hit.Coverage, &coverage) != nil || coverage.Unit != "unicode_code_points" || coverage.Rewritten || coverage.Start < 0 || coverage.End <= coverage.Start || coverage.End > len(runes) || string(runes[coverage.Start:coverage.End]) != hit.Text {
					ignored++
					continue
				}
				start := chunk.StartByte + len(string(runes[:coverage.Start]))
				end := chunk.StartByte + len(string(runes[:coverage.End]))
				lineStarts := documentLineStarts(doc.Text)
				var score float64
				_ = json.Unmarshal(hit.Score, &score)
				chunk.ID = strings.Replace(chunk.ID, doc.ID, ownerID, 1)
				matches = append(matches, DocumentMatch{SourceID: ownerID, Chunk: chunk, HybridScore: score, Excerpt: hit.Text, StartLine: lineForByte(lineStarts, start), EndLine: lineForByte(lineStarts, max(start, end-1))})
				if len(matches) >= limit {
					break
				}
			}
			meta["unregistered_or_changed_records_ignored"] = ignored
			seen := map[string]bool{}
			for _, match := range matches {
				seen[match.SourceID+"\x00"+match.Chunk.ID] = true
			}
			local, localErr := index.localMatches(ctx, sourceID, query, limit, true, seen)
			if localErr != nil {
				return nil, nil, localErr
			}
			if len(local) > 0 {
				meta["local_lexical_supplement"] = true
				meta["ranking_degraded"] = true
				matches = append(matches, local...)
				if len(matches) > limit {
					matches = matches[:limit]
				}
			}
			if sourceID != "" {
				meta["source_filter"] = "provider top 20 records plus scoped lexical supplement; no exhaustive dense coverage claim"
			}
			encoded, _ := json.Marshal(meta)
			return matches, encoded, nil
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		meta["ranking_degraded"] = true
		meta["ranking_error"] = "retrieval_service_unavailable"
	} else {
		meta["ranking_degraded"] = true
		meta["ranking_error"] = "remote_index_not_ready"
	}
	meta["ranking"] = "native_lexical_fallback"
	matches, err = index.localMatches(ctx, sourceID, query, limit, false, nil)
	if err != nil {
		return nil, nil, err
	}
	encoded, _ := json.Marshal(meta)
	return matches, encoded, nil
}

func (index *AttachmentIndex) localMatches(ctx context.Context, sourceID, query string, limit int, unindexedOnly bool, seen map[string]bool) ([]DocumentMatch, error) {
	var matches []DocumentMatch
	for id, record := range index.manifest.Documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if sourceID != "" && sourceID != id {
			continue
		}
		if unindexedOnly && sourceID == "" && record.Receipt.IndexState == "ready" {
			continue
		}
		indexed := map[string]bool{}
		for _, remote := range record.Remote {
			indexed[remote.ChunkID] = true
		}
		doc, ok := index.store.Get(record.DocumentID)
		if !ok {
			continue
		}
		terms := documentTerms(query)
		for _, chunk := range doc.Chunks {
			if unindexedOnly && sourceID == "" && indexed[chunk.ID] {
				continue
			}
			score := documentMatchScore(chunk.Text, query, terms)
			if len(terms) > 0 && score <= 0 {
				continue
			}
			excerpt, start, end := boundedDocumentExcerpt(chunk.Text, terms)
			match := DocumentMatch{SourceID: record.DocumentID, Chunk: chunk, Score: score, Excerpt: excerpt, StartLine: chunk.StartLine + start, EndLine: chunk.StartLine + end}
			match.SourceID = id
			match.Chunk.ID = strings.Replace(match.Chunk.ID, record.DocumentID, id, 1)
			if seen[id+"\x00"+match.Chunk.ID] {
				continue
			}
			matches = append(matches, match)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return documentMatchRanksBefore(matches[i], matches[j]) })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return matches, nil
}

func (index *AttachmentIndex) Read(ctx context.Context, args map[string]any, maxLines int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	id, _ := args["source_id"].(string)
	doc, receipt, ok := index.Get(ctx, id)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("unknown session attachment source_id %q", id)
	}
	if chunkID, _ := args["chunk_id"].(string); chunkID != "" {
		for _, chunk := range doc.Chunks {
			if chunk.ID == chunkID {
				return fmt.Sprintf("[%s:L%d-L%d chunk=%s sha256=%s]\n%s", id, chunk.StartLine, chunk.EndLine, chunk.ID, chunk.SHA256, attachmentExcerpt(receipt, chunk.Text)), nil
			}
		}
		return "", fmt.Errorf("unknown attachment chunk_id %q", chunkID)
	}
	start, firstOK := documentIntegerArg(args["start_line"])
	end, lastOK := documentIntegerArg(args["end_line"])
	if !firstOK || !lastOK || start < 1 || end < start || end > doc.Lines {
		return "", fmt.Errorf("provide chunk_id or a valid bounded extraction line range")
	}
	if maxLines > 0 && end-start+1 > maxLines {
		return "", fmt.Errorf("read at most %d extraction lines per call", maxLines)
	}
	lines := strings.Split(doc.Text, "\n")
	return fmt.Sprintf("[%s:L%d-L%d]\n%s", id, start, end, attachmentExcerpt(receipt, strings.Join(lines[start-1:end], "\n"))), nil
}
