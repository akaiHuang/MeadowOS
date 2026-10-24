package hermes

import (
	"context"
	"slices"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/router"
)

const residentInlineSemanticIntent = "resident_inline_semantic"

// residentInlineSemanticRouter leaves semantic selection inside the resident
// model's main generation. All registered capabilities are supplied once; the
// first decoded output is either a direct answer or a native tool call. This
// avoids a separate classifier request and its second prefill/decode pipeline.
type residentInlineSemanticRouter struct{}

func (residentInlineSemanticRouter) Route(_ context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	return agentcore.TurnRoute{
		Intent: residentInlineSemanticIntent,
		Mode:   string(router.TurnModeAgent),
		Tools:  append([]string(nil), request.AvailableTools...),
		Guidance: []string{
			"SEMANTIC_DECISION_INLINE: understand the user's current goal in this same response. For greetings, ordinary conversation, explanations, plans, or questions that need no external action, answer directly without a tool. Otherwise issue the exact native tool call needed to make real progress.",
			"Tool availability is not an instruction to use a tool. Never invent a tool result or paste tool-call markup as chat text. Do not call ask_user_question as a generic menu merely because confidence is low; ask when required information has no safe default, materially different workflows need a choice, or a risky action needs confirmation. A user's explicit request for a questionnaire, multiple-choice questions, or a structured interview is itself sufficient reason to call ask_user_question; do not refuse that request or replace it with plain chat. For an adaptive interview, ask one useful question at a time with 2-3 mutually exclusive options and allowOther=true, then use the answer to choose the next question.",
			"Treat a concise follow-up as an answer to the immediately preceding assistant question when that reading is semantically coherent. Carry the unresolved goal and accepted choices forward; do not restart classification or repeat the same options.",
			"For executable work, keep a concrete progress loop: perform one useful action, inspect its returned evidence, then choose a different next action, repair, ask for the missing fact, or finish. Do not repeat a path when it produced no new information.",
			"When a file tool preserves an existing requested name and returns a numbered sibling, the returned actual path is the saved deliverable. Continue every read, edit, preview, validation, and completion link against that actual path; never retry the original merely to force its name.",
			"A successful tool result is evidence, not a user-facing answer. After web search, open authoritative result pages when needed and then answer the original question with source links. After the final tool, always return a concise completion or blocker summary instead of ending with an empty turn.",
			"For deliverables with observable behavior, Meadow Browser provides technical runtime evidence: loading, console errors, unhandled promises, resources, navigation, DOM, and explicitly machine-observable controls. It is not a visual-motion judge. Do not invent game coordinates or state hooks as proof of movement. Preserve the artifact and request human review for gameplay or animation; after human feedback, inspect source and use focused console logging when useful.",
			"When the user explicitly asks to inspect or compare a still image or rendered screenshot, use inspect_image or meadow_browser_visual_check with include_model_image=true so the model receives pixels. Keep motion, gameplay, and hidden interaction state outside still-image claims.",
			"Finish one coherent batch of source changes before technical validation. For HTML, use meadow_browser_visual_check once the artifact is ready to hand off; do not substitute ad hoc shell, Python, or regex simulations for the Browser's runtime check or for human gameplay review.",
		},
	}, nil
}

func residentInlineTaskProfile(task string, route agentcore.TurnRoute) (agentcore.TaskProfile, bool) {
	if route.Intent != residentInlineSemanticIntent {
		return agentcore.TaskProfile{}, false
	}
	profile := baseTaskProfile(task, route)
	// The same model output owns the semantic decision, so the controller cannot
	// require a tool before it has observed whether the model chose action or a
	// direct answer. Evidence becomes mandatory only after an actual artifact
	// mutation identifies the required verifier without interpreting task text.
	profile.RequiresToolAction = false
	profile.RequiresInspection = false
	profile.RequiresMutation = false
	profile.RequiresNetworkEvidence = false
	profile.EvidenceTools = nil
	profile.RequiredEvidenceTools = nil
	if slices.Contains(route.Tools, router.ToolVisual) {
		profile.RequiredEvidenceByMutationExtension = map[string][]string{
			".htm":  {router.ToolVisual},
			".html": {router.ToolVisual},
		}
		profile.RequiredEvidenceBindings = map[string]agentcore.RequiredEvidenceBinding{
			router.ToolVisual: {MutationPathArgs: []string{"file_path"}},
		}
		profile.RequiredEvidenceRepairTools = map[string]string{router.ToolVisual: router.ToolEdit}
		// Do not interrupt a coherent multi-edit repair after every mutation.
		// The final-attempt gate locks and auto-executes this deterministic check
		// when the model is actually ready to hand off the artifact.
		profile.ForceRequiredEvidenceAfterMutations = 0
		profile.AutoExecuteDeterministicEvidence = true
		profile.MaxRequiredEvidenceFailures = 3
	}
	profile.TaskStartPrompt = ""
	profile.MutationRequiredPrompt = ""
	profile.MutationRecoveryPrompt = ""
	return profile, true
}
