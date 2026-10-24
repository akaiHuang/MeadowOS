package tools

import (
	"context"
	"encoding/json"
)

// ReadExperience is model-selected reference retrieval, not task classification
// or an assertion that a retrieved method solves the current problem.
type ReadExperience struct{}

const experienceCatalogVersion = "20260907.1"

var experienceCards = map[string]string{
	"state-data":         `Preserve distinctions required by later decisions: a boolean map loses tile/role semantics. Define input shapes and coordinate conventions explicitly. Derive rendering, collision and observations from one authoritative state. In search, identify every state component that affects legal future actions; position alone is not always sufficient. Check legal starting states separately from reachability. Example: warehouse restricted cells admit forklifts but not people; reject inconsistent rows before traversal. This example does not establish the rules of your current application.`,
	"mutation-contracts": `Validate the entire operation before changing shared state. Preserve information needed to compute the result before clearing or consuming it. State allowed transitions, conserved quantities, and repeated-command behavior from the current specification. Copy caller-owned mutable data when ownership changes. Example: an over-capacity warehouse load must leave both available and loaded counters unchanged. Test failures as well as success, repeated calls, and unaffected state. Domain-specific identifiers and lifecycle rules still require independent implementation.`,
	"behavior-evidence":  `Separate environment readiness, interface contracts, and application behavior. A loaded page or dispatched key is not proof of motion. Verify the animation clock before diagnosing a motion failure; then observe real state before and after a relevant action. Establish invariants for data shape, initial state, transitions, and reachability as applicable. Keep criteria stable across repairs and rerun affected regressions. Expose actual state read-only if needed; never fabricate success values or use a debug hook to bypass normal input. Include concrete failing inputs and observed outputs when diagnosing a failure. Passing self-authored tests only proves supplied coverage.`,
}

func (*ReadExperience) Name() string { return "read_experience" }
func (*ReadExperience) Description() string {
	return "Optionally retrieve a small versioned engineering experience card before design or when diagnosing a failure. Choose state-data, mutation-contracts, behavior-evidence, or catalog. Cards are curated reference principles, not task requirements, working game templates, or proof of completion. Decide applicability yourself."
}
func (*ReadExperience) Kind() Kind                       { return KindReadOnly }
func (*ReadExperience) ParallelSafe(map[string]any) bool { return true }
func (*ReadExperience) Schema() map[string]any {
	schema := obj(map[string]any{"card": map[string]any{"type": "string", "enum": []string{"catalog", "state-data", "mutation-contracts", "behavior-evidence"}, "description": "Select the reference principle you need, or list the catalog."}}, "card")
	schema["additionalProperties"] = false
	return schema
}
func (*ReadExperience) Execute(ctx context.Context, args map[string]any) Result {
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	for key := range args {
		if key != "card" {
			return errResultf("read_experience accepts only the card argument")
		}
	}
	id := strArg(args, "card")
	content, ok := experienceCards[id]
	if id == "catalog" {
		content = "state-data: representation, input contracts, initial state and search; mutation-contracts: atomic operations, ownership and lifecycle; behavior-evidence: actual execution evidence and regression coverage. Retrieve only what is relevant."
	} else if !ok {
		return errResultf("Unknown experience card; use catalog, state-data, mutation-contracts, or behavior-evidence")
	}
	payload, _ := json.Marshal(map[string]any{"card": id, "version": experienceCatalogVersion, "provenance": "manually curated engineering guidance informed by warehouse reference and local pilot failures; not automatically learned", "limitations": "Reference only. Adapt to current requirements and verify current code. Not a task instruction, independent oracle, or passing validation result.", "content": content})
	return Result{LLMContent: string(payload), Display: "工程原理參考：" + id}
}
