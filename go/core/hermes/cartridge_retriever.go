package hermes

import (
	"context"
	"errors"
	"strings"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/rag"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// cartridgeRetriever mounts only the records already permitted by the runtime
// scope. Cross-session memory is intentionally disabled here; product surfaces
// must use an explicit, user-approved recall action for that wider boundary.
type cartridgeRetriever struct {
	store   *rag.CartridgeStore
	profile string
	scope   rag.CartridgeScope
	locale  string
}

func (r cartridgeRetriever) Retrieve(ctx context.Context, request agentcore.RetrievalRequest) (agentcore.RetrievedContext, error) {
	if r.store == nil || request.Phase != agentcore.RetrievalPhaseInitial {
		return agentcore.RetrievedContext{}, nil
	}
	matches := r.store.Search(ctx, rag.CartridgeQuery{
		Text: request.Task, Profile: r.profile, Locale: r.locale,
		Scope: r.scope, AllowCrossSession: false, Limit: 4, MaxTokens: 900,
	})
	result := agentcore.RetrievedContext{Text: rag.CompactCartridgeEvidence(matches, 2400)}
	for _, match := range matches {
		result.Sources = append(result.Sources, agentcore.RetrievalSource{
			ID: "cartridge:" + match.Record.ID, Title: match.Record.Title,
			Source: string(match.Record.Origin), Version: match.Record.UpdatedAt,
			Score: int(match.Score * 1000),
		})
	}
	return result, nil
}

type combinedRetriever struct {
	retrievers []agentcore.ContextRetriever
}

func combineRetrievers(retrievers ...agentcore.ContextRetriever) agentcore.ContextRetriever {
	compact := make([]agentcore.ContextRetriever, 0, len(retrievers))
	for _, retriever := range retrievers {
		if retriever != nil {
			compact = append(compact, retriever)
		}
	}
	if len(compact) == 0 {
		return nil
	}
	if len(compact) == 1 {
		return compact[0]
	}
	return combinedRetriever{retrievers: compact}
}

func (r combinedRetriever) Retrieve(ctx context.Context, request agentcore.RetrievalRequest) (agentcore.RetrievedContext, error) {
	var texts []string
	var sources []agentcore.RetrievalSource
	var failures []error
	seen := make(map[string]bool)
	for _, retriever := range r.retrievers {
		candidate, err := retriever.Retrieve(ctx, request)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if text := strings.TrimSpace(candidate.Text); text != "" {
			texts = append(texts, text)
		}
		for _, source := range candidate.Sources {
			key := strings.TrimSpace(source.ID)
			if key == "" {
				key = source.Source + "\x00" + source.Title
			}
			if !seen[key] {
				seen[key] = true
				sources = append(sources, source)
			}
		}
	}
	if len(texts) > 0 || len(sources) > 0 {
		return agentcore.RetrievedContext{Text: strings.Join(texts, "\n\n"), Sources: sources}, nil
	}
	return agentcore.RetrievedContext{}, errors.Join(failures...)
}

func withCartridgeRetriever(
	base agentcore.ContextRetriever,
	store *rag.CartridgeStore,
	profile string,
	scope rag.CartridgeScope,
) agentcore.ContextRetriever {
	if store == nil {
		return base
	}
	return combineRetrievers(base, cartridgeRetriever{
		store: store, profile: profile, scope: scope, locale: "zh-TW",
	})
}

// registerCartridgeMemoryTools exposes memory as an explicit, auditable
// capability. Current-session retrieval stays automatic; these tools are for
// an intentional save or a user-approved wider recall.
func registerCartridgeMemoryTools(
	registry interface{ Register(tools.Tool) },
	store *rag.CartridgeStore,
	scope rag.CartridgeScope,
	ask tools.Asker,
) {
	if registry == nil || store == nil {
		return
	}
	registry.Register(&rag.MemorySearchTool{Store: store, Scope: scope, Ask: ask})
	registry.Register(&rag.MemoryRememberTool{Store: store, Scope: scope, Ask: ask})
}
