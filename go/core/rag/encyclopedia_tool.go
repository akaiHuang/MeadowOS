package rag

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const EncyclopediaSearchToolName = "encyclopedia_search"

var encyclopediaSourceLine = regexp.MustCompile(`(?m)^source_url:\s*(https?://\S+)\s*$`)

// EncyclopediaSearchTool exposes only reviewed, persona-scoped knowledge with
// exact local coordinates and original web provenance. It is read-only and
// does not let the model invent a persona filter outside the supported set.
type EncyclopediaSearchTool struct{ Store *EncyclopediaStore }

func (t *EncyclopediaSearchTool) Name() string { return EncyclopediaSearchToolName }
func (t *EncyclopediaSearchTool) Description() string {
	return "Search Meadow's reviewed local encyclopedia by user need, audience, difficulty, and locale. Returns source-backed excerpts; use Meadow Browser when no reviewed result covers the need."
}
func (t *EncyclopediaSearchTool) Kind() tools.Kind { return tools.KindReadOnly }
func (t *EncyclopediaSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "One focused semantic information need from the user request."},
			"audience": map[string]any{
				"type": "string", "enum": []string{
					string(AudienceGeneral), string(AudienceHomemaker), string(AudienceMiddleSchool),
					string(AudienceHighSchool), string(AudienceUniversity), string(AudienceWhiteCollar),
					string(AudienceBlueCollar),
				},
			},
			"max_difficulty": map[string]any{
				"type": "string", "enum": []string{
					string(DifficultyBasic), string(DifficultyIntermediate), string(DifficultyAdvanced),
				},
			},
			"locale": map[string]any{"type": "string", "description": "Preferred BCP-47 locale, normally zh-TW."},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 6},
		},
		"required":             []string{"query", "audience", "max_difficulty", "locale"},
		"additionalProperties": false,
	}
}

func (t *EncyclopediaSearchTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	if t == nil || t.Store == nil {
		return documentToolError("encyclopedia store is unavailable")
	}
	query, _ := args["query"].(string)
	audience, _ := args["audience"].(string)
	difficulty, _ := args["max_difficulty"].(string)
	locale, _ := args["locale"].(string)
	limit := 4
	if value, ok := args["limit"].(float64); ok {
		limit = int(value)
	} else if value, ok := args["limit"].(int); ok {
		limit = value
	}
	if strings.TrimSpace(query) == "" {
		return documentToolError("query is required")
	}
	matches := t.Store.Search(ctx, KnowledgeQuery{
		Text: query, Audience: Audience(audience), MaxDifficulty: Difficulty(difficulty),
		Locale: strings.TrimSpace(locale), Limit: limit,
	})
	if len(matches) == 0 {
		message := "NO_REVIEWED_MATCH: Meadow encyclopedia has no reviewed evidence covering this need and audience. Continue with one focused Meadow Browser search; do not answer from memory alone."
		return tools.Result{LLMContent: message, Display: message}
	}
	var body strings.Builder
	for _, match := range matches {
		// One coordinate block must name exactly one provenance URL. A reviewed
		// entry with multiple references is useful administrative metadata, but
		// attributing the same excerpt to every reference would let a later claim
		// cite a source that did not actually contribute that passage. Curators
		// should split genuinely multi-source material into separate entries.
		source := match.Entry.Sources[0]
		fmt.Fprintf(&body, "[%s:L%d-L%d chunk=%s hybrid=%.3f]\nsource_url: %s\nsource_title: %s\n%s\n",
			match.Entry.DocumentID, match.Evidence.StartLine, match.Evidence.EndLine,
			match.Evidence.Chunk.ID, match.Score, source.URL, source.Title,
			match.Evidence.Excerpt)
		body.WriteByte('\n')
	}
	message := strings.TrimSpace(body.String())
	return tools.Result{LLMContent: message, Display: fmt.Sprintf("Found %d reviewed encyclopedia entries", len(matches))}
}

func (t *EncyclopediaSearchTool) EvidenceSources(_ map[string]any, toolResult tools.Result) []tools.EvidenceSource {
	seen := make(map[string]bool)
	var result []tools.EvidenceSource
	for _, match := range encyclopediaSourceLine.FindAllStringSubmatch(toolResult.LLMContent, -1) {
		if len(match) < 2 || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		result = append(result, tools.EvidenceSource{URL: match[1]})
	}
	return result
}
