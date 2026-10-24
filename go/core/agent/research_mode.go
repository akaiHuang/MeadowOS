package agent

import (
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// explicitResearchToolNames is a capability boundary, not a task program. The
// model still authors the need analysis, search query, source choices, claims,
// citations and final document. The Kernel only prevents an explicitly
// selected research run from drifting into unrelated mutation or shell tools.
var explicitResearchToolNames = map[string]bool{
	"web_search":                  true,
	"web_fetch":                   true,
	"meadow_browser_dom":          true,
	"pdf_extract":                 true,
	"document_search":             true,
	"document_read":               true,
	"encyclopedia_search":         true,
	ResearchEvidenceToolName:      true,
	ResearchCoverageToolName:      true,
	ResearchClaimsToolName:        true,
	tools.ResearchPublishToolName: true,
}

func explicitResearchRoute(route TurnRoute, registry *tools.Registry) (TurnRoute, *tools.Registry) {
	if registry == nil {
		return route, registry
	}
	names := make([]string, 0, len(explicitResearchToolNames))
	for _, name := range registry.Names() {
		if explicitResearchToolNames[name] {
			names = append(names, name)
		}
	}
	route.Intent = "internet_research"
	route.Mode = "agent"
	route.DisableTools = false
	route.Tools = names
	route.Guidance = compactGuidance(append(route.Guidance,
		strings.TrimSpace(`Explicit research mode is active. First define the information need, then issue one bounded discovery search, open only the most relevant pages with Meadow Browser/PDF extraction, retrieve the indexed evidence semantically, and publish cited Markdown plus an HTML document. Add slides only when requested. Never invent missing facts; user guidance may redirect or stop research.`),
	))
	return route, registry.Select(names)
}
