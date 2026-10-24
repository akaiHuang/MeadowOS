package harness

import (
	"context"

	"github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/rag"
)

const DocumentIngestionThresholdTokens = 32_000

type DocumentIngestor struct {
	store *rag.DocumentStore
}

func NewDocumentIngestor(store *rag.DocumentStore) *DocumentIngestor {
	if store == nil {
		return nil
	}
	return &DocumentIngestor{store: store}
}

func (i *DocumentIngestor) Ingest(
	ctx context.Context,
	request agent.DocumentIngestionRequest,
	onProgress func(agent.DocumentIngestionProgress),
) (agent.DocumentIngestionResult, error) {
	if i == nil || i.store == nil || provider.EstimateTextTokens(request.Text) < DocumentIngestionThresholdTokens {
		return agent.DocumentIngestionResult{}, nil
	}
	doc, err := i.store.Ingest(ctx, request.SourceName, request.Text, func(progress rag.DocumentProgress) {
		if onProgress == nil {
			return
		}
		onProgress(agent.DocumentIngestionProgress{
			SourceID: progress.SourceID, SourceName: progress.SourceName,
			ProcessedBytes: progress.ProcessedBytes, TotalBytes: progress.TotalBytes,
			Processed: progress.Processed, Total: progress.Total,
			StartLine: progress.Chunk.StartLine, EndLine: progress.Chunk.EndLine,
			Note: progress.Chunk.Note, Reused: progress.Reused, Ready: progress.Ready,
		})
	})
	if err != nil {
		return agent.DocumentIngestionResult{}, err
	}
	return agent.DocumentIngestionResult{
		Used: true, SourceID: doc.ID, Chunks: len(doc.Chunks), SourceBytes: doc.Bytes,
		ModelContext: i.store.CompactDocumentContext(doc, request.Text),
	}, nil
}
