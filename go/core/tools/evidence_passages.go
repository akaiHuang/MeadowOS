package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// EvidencePassage describes only text actually returned by a successful source
// tool. Local line coordinates refer to this fragment, not the original page.
// SourceVersion belongs to the provider's extracted source; TextSHA256 belongs
// to this exact returned text. A ranking score is never a confidence score.
type EvidencePassage struct {
	Source      EvidenceSource  `json:"source"`
	Text        string          `json:"text"`
	TextSHA256  string          `json:"text_sha256"`
	RetrievedAt string          `json:"retrieved_at,omitempty"`
	Section     string          `json:"section,omitempty"`
	Locator     json.RawMessage `json:"provider_locator,omitempty"`
	Coverage    json.RawMessage `json:"coverage,omitempty"`
	Scope       string          `json:"scope"`
	CacheHit    bool            `json:"cache_hit,omitempty"`
}

type EvidencePassageProvider interface {
	EvidencePassages(args map[string]any, result Result) []EvidencePassage
}

func exactEvidencePassage(source EvidenceSource, text, scope string) EvidencePassage {
	hash := sha256.Sum256([]byte(text))
	return EvidencePassage{Source: source, Text: text, Scope: scope, TextSHA256: hex.EncodeToString(hash[:])}
}

func CloneEvidencePassages(passages []EvidencePassage) []EvidencePassage {
	copyPassages := append([]EvidencePassage(nil), passages...)
	for index := range copyPassages {
		copyPassages[index].Locator = append(json.RawMessage(nil), copyPassages[index].Locator...)
		copyPassages[index].Coverage = append(json.RawMessage(nil), copyPassages[index].Coverage...)
	}
	return copyPassages
}

func successfulSourcePassages(result Result) []EvidencePassage {
	if result.Err != nil {
		return nil
	}
	return CloneEvidencePassages(result.SourcePassages)
}

func (*WebSearch) EvidencePassages(_ map[string]any, result Result) []EvidencePassage {
	return successfulSourcePassages(result)
}
func (*WebFetch) EvidencePassages(_ map[string]any, result Result) []EvidencePassage {
	return successfulSourcePassages(result)
}
func (*MeadowBrowserDOM) EvidencePassages(_ map[string]any, result Result) []EvidencePassage {
	return successfulSourcePassages(result)
}
func (*PDFExtract) EvidencePassages(_ map[string]any, result Result) []EvidencePassage {
	return successfulSourcePassages(result)
}
