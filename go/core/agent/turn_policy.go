package agent

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// TurnRouteRequest is the model-agnostic input to a toolset planner. Messages
// are copied by the caller; implementations must not mutate them.
type TurnRouteRequest struct {
	Task           string
	Turn           int
	CWD            string
	Messages       []provider.Message
	AvailableTools []string
}

// TurnRoute is the complete route contract consumed by KernelRuntime. Tools is
// an allowlist, not merely a schema hint. Nil/empty means use all registered
// tools so planner failures can fail open without silently disabling actions.
type TurnRoute struct {
	Intent       string
	Mode         string
	Tools        []string
	DisableTools bool
	Guidance     []string
	// SamplingPhase is an optional route-level precision envelope. Lifecycle
	// recovery/audit phases remain higher priority in KernelRuntime.
	SamplingPhase provider.SamplingPhase
}

type TurnRouter interface {
	Route(context.Context, TurnRouteRequest) (TurnRoute, error)
}

type RetrievalRequest struct {
	Task             string
	Intent           string
	Turn             int
	Phase            string
	Reason           string
	Procedure        string
	FailureSignature string
	ExcludeSourceIDs []string
}

type RetrievalSource struct {
	ID      string
	Title   string
	Source  string
	Version string
	Updated string
	Score   int
}

// RetrievedContext keeps model-visible evidence separate from source metadata
// used by traces and product gates.
type RetrievedContext struct {
	Text    string
	Sources []RetrievalSource
}

type ContextRetriever interface {
	Retrieve(context.Context, RetrievalRequest) (RetrievedContext, error)
}

const (
	RetrievalPhaseInitial  = "initial"
	RetrievalPhaseRecovery = "recovery"

	RetrievalProcedureFailureMutation  = "failure_mutation"
	RetrievalProcedureIndependentAudit = "independent_audit"
	RetrievalProcedureNativeTool       = "native_tool"
)

func retrievalSourceID(source RetrievalSource) string {
	if value := strings.TrimSpace(source.ID); value != "" {
		return value
	}
	return strings.Join([]string{
		strings.TrimSpace(source.Source),
		strings.TrimSpace(source.Title),
		strings.TrimSpace(source.Version),
	}, "|")
}

func acceptNewRetrievedContext(candidate RetrievedContext, seen map[string]bool, phase string) RetrievedContext {
	if strings.TrimSpace(candidate.Text) == "" {
		return RetrievedContext{}
	}
	if len(candidate.Sources) == 0 {
		// Initial legacy retrievers may not yet expose provenance. A recovery
		// refresh must have IDs so the kernel can enforce non-repetition.
		if phase == RetrievalPhaseRecovery {
			return RetrievedContext{}
		}
		return candidate
	}
	for _, source := range candidate.Sources {
		id := retrievalSourceID(source)
		if id == "" || seen[id] {
			return RetrievedContext{}
		}
	}
	return candidate
}

func mergeRetrievedContexts(values ...RetrievedContext) RetrievedContext {
	result := RetrievedContext{}
	for _, value := range values {
		if text := strings.TrimSpace(value.Text); text != "" {
			if result.Text != "" {
				result.Text += "\n\n"
			}
			result.Text += text
		}
		result.Sources = append(result.Sources, value.Sources...)
	}
	return result
}

// retainRecoveryRetrievedContexts keeps the newest recovery procedure plus as
// many earlier procedure batches as fit in the prompt budget. Recovery is
// iterative: a later strategy-switch card supplements the validator procedure
// that motivated it; it must not silently replace that still-applicable card.
// Keeping whole batches avoids presenting a model with a truncated procedure.
func retainRecoveryRetrievedContexts(current []RetrievedContext, candidate RetrievedContext) []RetrievedContext {
	if strings.TrimSpace(candidate.Text) == "" {
		return current
	}
	const maxRunes = 12_000
	values := make([]RetrievedContext, 0, len(current)+1)
	values = append(values, candidate)
	values = append(values, current...)

	retained := make([]RetrievedContext, 0, len(values))
	used := 0
	for _, value := range values {
		text := strings.TrimSpace(value.Text)
		if text == "" {
			continue
		}
		cost := utf8.RuneCountInString(text)
		if len(retained) > 0 {
			cost += 2 // mergeRetrievedContexts inserts two newlines.
		}
		if used+cost > maxRunes {
			continue
		}
		value.Text = text
		retained = append(retained, value)
		used += cost
	}
	return retained
}

func retrievalSourceIDs(seen map[string]bool) []string {
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func retrievedSourceIDs(sources []RetrievalSource) []string {
	result := make([]string, 0, len(sources))
	for _, source := range sources {
		if id := retrievalSourceID(source); id != "" {
			result = append(result, id)
		}
	}
	return result
}

func resolveTurnRoute(ctx context.Context, planner TurnRouter, request TurnRouteRequest, registry *tools.Registry) (TurnRoute, *tools.Registry, error) {
	all := registry.Names()
	route := TurnRoute{Intent: "kernel_generic", Tools: all}
	if planner == nil {
		return route, registry, nil
	}
	planned, err := planner.Route(ctx, request)
	if err != nil {
		return route, registry, err
	}
	if strings.TrimSpace(planned.Intent) != "" {
		route.Intent = strings.TrimSpace(planned.Intent)
	}
	route.Mode = strings.TrimSpace(planned.Mode)
	route.Guidance = compactGuidance(planned.Guidance)
	route.SamplingPhase = planned.SamplingPhase
	if planned.DisableTools {
		route.Tools = nil
		route.DisableTools = true
		return route, registry.Select(nil), nil
	}
	if len(planned.Tools) == 0 {
		return route, registry, nil
	}
	selected := registry.Select(uniqueStrings(planned.Tools))
	if len(selected.Names()) == 0 {
		return route, registry, nil
	}
	route.Tools = selected.Names()
	return route, selected, nil
}

func compactGuidance(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
		if len(result) == 8 {
			break
		}
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func boundedRetrievedText(value string) string {
	const maxRunes = 12_000
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes-1]) + "…"
}
