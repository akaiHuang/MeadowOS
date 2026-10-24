package tools

import (
	"context"
	"fmt"
	"strings"
)

// These fields are derived from successful tool receipts, never from the
// presentation author's source arguments. They identify observed fragments,
// not verified claims or complete original pages.
type presentationSourceObservation struct {
	Status                   string `json:"status"`
	Scope                    string `json:"scope"`
	OriginalPageCompleteness string `json:"original_page_completeness"`
	ContentKind              string `json:"content_kind,omitempty"`
	FetchedAt                string `json:"fetched_at,omitempty"`
	SourceVersion            string `json:"source_version,omitempty"`
	Snippet                  string `json:"snippet,omitempty"`
	SnippetTruncated         bool   `json:"snippet_truncated,omitempty"`
	InputTruncated           bool   `json:"input_truncated,omitempty"`
	EvidenceTruncated        bool   `json:"evidence_truncated,omitempty"`
	FetchError               string `json:"fetch_error,omitempty"`
}

func validatePresentationSourceLineage(ctx context.Context, slides []presentationSlideInput, sources []presentationSourceInput) error {
	ledger := map[string]bool{}
	for _, source := range sources {
		ledger[strings.TrimSpace(source.URL)] = true
	}
	observed := map[string]bool{}
	for _, source := range EvidenceSourcesFromContext(ctx) {
		if normalized, ok := NormalizeEvidenceSource(source); ok {
			observed[normalized.URL] = true
		}
	}
	for index, slide := range slides {
		for _, raw := range slide.SourceURLs {
			url := strings.TrimSpace(raw)
			if !ledger[url] {
				return &RecoveryError{Code: "source_ledger_incomplete", Message: fmt.Sprintf("slide %d cites a URL missing from the supplied source ledger", index+1), RequiredAction: "retry_presentation_with_complete_sources", PreserveDraft: true}
			}
			if len(observed) > 0 && !observed[url] {
				return &RecoveryError{Code: "source_not_observed", Message: fmt.Sprintf("slide %d cites a URL not present in this run's successful source observations; open the source before citing it", index+1), RequiredAction: "observe_cited_source", PreserveDraft: true}
			}
		}
	}
	return nil
}

func annotatePresentationSourceObservations(ctx context.Context, sources []presentationSource) {
	observed := map[string]EvidenceSource{}
	for _, source := range EvidenceSourcesFromContext(ctx) {
		if normalized, ok := NormalizeEvidenceSource(source); ok {
			observed[normalized.URL] = normalized
		}
	}
	for index := range sources {
		receipt, ok := observed[sources[index].URL]
		if !ok && sources[index].Observation != nil {
			// An omitted ledger preserves its prior stored receipts. This field
			// is not accepted from model arguments or asserted as current proof.
			continue
		}
		observation := &presentationSourceObservation{Status: "not_observed_in_current_run", Scope: "returned_fragment", OriginalPageCompleteness: "not_verified"}
		if ok {
			observation.Status = "observed_tool_receipt"
			if receipt.ContentKind == "snippet" || receipt.FetchError != "" {
				observation.Status = "discovery_only"
			}
			observation.ContentKind, observation.FetchedAt, observation.SourceVersion = receipt.ContentKind, receipt.FetchedAt, receipt.SourceVersion
			observation.Snippet, observation.SnippetTruncated = receipt.Snippet, receipt.SnippetTruncated
			observation.InputTruncated, observation.EvidenceTruncated, observation.FetchError = receipt.InputTruncated, receipt.EvidenceTruncated, receipt.FetchError
		}
		sources[index].Observation = observation
	}
}
