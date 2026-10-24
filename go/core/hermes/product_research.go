package hermes

import (
	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// productResearchPolicy is the shared cross-surface research contract for the
// general 12B, coding 27B and dense 30B products. Model-specific routers may
// still activate it automatically, but an explicit RunModeResearch bypasses
// intent heuristics and enters this same evidence loop.
func productResearchPolicy(modelRouted bool, maxPerTurn, maxSources int, incremental bool) agentcore.ResearchPolicy {
	return agentcore.ResearchPolicy{
		Enabled:                      true,
		Eligible:                     productResearchEligible,
		ModelRouted:                  modelRouted,
		MinOpenedSources:             2,
		MaxSourceOpensPerTurn:        maxPerTurn,
		MaxOpenedSources:             maxSources,
		RequireDiscovery:             true,
		RequireFinalCitations:        true,
		IncrementalOnly:              incremental,
		RequireRAGBeforeClaims:       true,
		RequireArticleSources:        true,
		AutoExecuteRAGSearch:         true,
		AutoOpenDiscoveryArticles:    true,
		AutoExecuteDiscoverySearch:   true,
		MaxDiscoveryAttemptsPerTurn:  2,
		RAGEvidenceCards:             true,
		AutoBindRAGEvidenceCards:     incremental,
		SearchRAGCards:               true,
		SingleInitialDiscoverySearch: true,
		PublishTools:                 []string{tools.ResearchPublishToolName},
	}
}
