package agent

import "context"

// DocumentIngestionRequest keeps the source text opaque to the Kernel. The
// implementation owns chunking/retrieval; the Kernel only swaps in the
// returned bounded model context after a complete, successful ingest.
type DocumentIngestionRequest struct {
	Text       string
	SourceName string
}

type DocumentIngestionProgress struct {
	SourceID       string
	SourceName     string
	ProcessedBytes int
	TotalBytes     int
	Processed      int
	Total          int
	StartLine      int
	EndLine        int
	Note           string
	Reused         bool
	Ready          bool
}

type DocumentIngestionResult struct {
	Used         bool
	SourceID     string
	ModelContext string
	Chunks       int
	SourceBytes  int
}

type DocumentIngestor interface {
	Ingest(
		context.Context,
		DocumentIngestionRequest,
		func(DocumentIngestionProgress),
	) (DocumentIngestionResult, error)
}
