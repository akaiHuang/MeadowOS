package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/rag"
	"github.com/hey-meadow/meadow-code/go/core/router"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// modelIntentRouter is the product intent control plane. Unlike the legacy
// advisoryRouter it does not decide from regular expressions in the user's
// wording. The resident model reads the current request together with recent
// conversation context and emits one compact, auditable route contract.
//
// Safety, authorization, workspace confinement and post-action validation stay
// deterministic. They verify observable effects; they do not infer intent.
type modelIntentRouter struct {
	client  agentcore.KernelModel
	modelID string
	profile string

	mu    sync.Mutex
	cache map[string]agentcore.TurnRoute
}

// SemanticIntentPredictor is the compact semantic route contract shared by
// Desktop's lightweight MLX predictor and Kernel runtimes. Keeping it here as
// an interface avoids coupling Hermes to the legacy Engine implementation.
type SemanticIntentPredictor interface {
	PredictRoute(ctx context.Context, userText string, availableToolNames []string) (string, error)
}

type predictorIntentRouter struct {
	predictor SemanticIntentPredictor
}

func newPredictorIntentRouter(predictor SemanticIntentPredictor) agentcore.TurnRouter {
	return predictorIntentRouter{predictor: predictor}
}

func (r predictorIntentRouter) Route(ctx context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	if r.predictor == nil {
		return unclassifiedSemanticRoute(request, "The independent semantic intent predictor is unavailable. Infer the user's intent from the full conversation in this main turn."), nil
	}
	predictCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	raw, err := r.predictor.PredictRoute(predictCtx, request.Task, request.AvailableTools)
	cancel()
	if err != nil {
		return unclassifiedSemanticRoute(request, "The independent semantic intent predictor timed out or was unavailable. Infer the user's intent from the full conversation in this main turn."), nil
	}
	parsed, ok := router.ParseCompactRouteLine(raw)
	if !ok {
		parsed, ok = compactRouteFromIntentCode(raw)
	}
	if !ok {
		return unclassifiedSemanticRoute(request, "The independent semantic intent predictor returned no valid route. Infer the user's intent from the full conversation in this main turn."), nil
	}
	return routeFromModelIntent(parsed, request.AvailableTools), nil
}

func compactRouteFromIntentCode(raw string) (router.CompactRoute, bool) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	intentByCode := map[string]router.IntentID{
		"WA": router.IntentStaticWebArtifact,
		"TS": router.IntentEngineeringFix,
		"BR": router.IntentInternetResearch,
		"DG": router.IntentDeleteGenerated,
		"PR": router.IntentOfficeArtifact,
		"SR": router.IntentOfficeArtifact,
		"FW": router.IntentSimpleWrite,
		"CF": router.IntentConfigArtifact,
		"IP": router.IntentInspectOrSearch,
		"GA": router.IntentGitAction,
		"SH": router.IntentShellScript,
		"CP": router.IntentSimpleCopy,
		"AF": router.IntentSimpleAppend,
		"DC": router.IntentDirectoryCreate,
		"TP": router.IntentGeneralAction,
		"AQ": "general_conversation",
		"GT": router.IntentEngineeringFix,
		"RF": router.IntentEngineeringFix,
		"DM": router.IntentEngineeringFix,
		"ES": router.IntentGeneralAction,
		"PK": router.IntentGeneralAction,
		"CL": router.IntentOfficeArtifact,
		"SS": router.IntentOfficeArtifact,
		"MF": router.IntentOfficeArtifact,
		"ED": router.IntentOfficeArtifact,
		"LA": router.IntentInspectOrSearch,
		"AC": router.IntentGeneralAction,
		"DI": router.IntentResearchArtifact,
		"NO": "general_conversation",
	}
	intent, ok := intentByCode[code]
	if !ok {
		return router.CompactRoute{}, false
	}
	return router.CompactRoute{Intent: intent}, true
}

func unclassifiedSemanticRoute(request agentcore.TurnRouteRequest, guidance string) agentcore.TurnRoute {
	return agentcore.TurnRoute{
		Intent: "model_unclassified", Mode: string(router.TurnModeAgent),
		Tools: append([]string(nil), request.AvailableTools...),
		Guidance: []string{guidance},
	}
}

const modelIntentToolName = "classify_intent"

type modelIntentToolArgs struct {
	Intent  string   `json:"intent"`
	Tools   []string `json:"tools"`
	Safety  string   `json:"safety"`
	Confirm bool     `json:"confirm"`
}

func newModelIntentRouter(client agentcore.KernelModel, modelID, profile string) *modelIntentRouter {
	return &modelIntentRouter{
		client: client, modelID: strings.TrimSpace(modelID), profile: strings.TrimSpace(profile),
		cache: make(map[string]agentcore.TurnRoute),
	}
}

func (r *modelIntentRouter) Route(ctx context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	key := modelIntentRouteKey(request)
	r.mu.Lock()
	if cached, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return cloneTurnRoute(cached), nil
	}
	r.mu.Unlock()

	maxTokens := modelIntentMaxTokens(r.profile)
	temperature := 0.0
	topP := 0.1
	thinking := false
	parallelToolCalls := false
	chatRequest := provider.ChatRequest{
		Model: r.modelID,
		Messages: []provider.Message{
			{Role: "system", Content: modelIntentNumericSystemPrompt(r.profile)},
			{Role: "user", Content: modelIntentConversation(request)},
		},
		MaxTokens:         &maxTokens,
		Temperature:       &temperature,
		TopP:              &topP,
		EnableThinking:    &thinking,
		SamplingPhase:     provider.SamplingPhasePrecision,
		ParallelToolCalls: &parallelToolCalls,
	}
	toolContract := modelIntentUsesToolContract(r.profile)
	if toolContract {
		// LFM2.5 currently reasons about a forced function correctly but its
		// backend may serialize the decision as prose. Keep its native contract
		// and recover the exact enum from model output below.
		maxTokens = 96
		chatRequest.Messages[0].Content = modelIntentSystemPrompt(r.profile, request.AvailableTools)
		chatRequest.Tools = []provider.Tool{modelIntentTool(request.AvailableTools)}
		chatRequest.ToolChoice = map[string]any{
			"type":     "function",
			"function": map[string]any{"name": modelIntentToolName},
		}
	}
	// The resident model owns semantic intent, but this classifier request must
	// never monopolize the inference server and prevent the real turn from
	// starting. A timeout fails open below, where the same resident model still
	// receives the full conversation and available capability set.
	routeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	result, err := r.client.ChatStream(routeCtx, chatRequest, nil)
	cancel()
	if err != nil {
		// A classifier outage must not turn Meadow into an if/else router or
		// silently disable tools. Fail open and let the main model own the turn.
		return unclassifiedSemanticRoute(request, "The intent preflight was unavailable. Infer the user's current intent from the full conversation; use no tool unless an external action is actually needed."), nil
	}
	var parsed router.CompactRoute
	var ok bool
	if toolContract {
		parsed, ok = modelIntentRouteFromResult(result)
	} else {
		parsed, ok = modelIntentRouteFromNumericResult(result)
	}
	if !ok {
		return unclassifiedSemanticRoute(request, "The intent preflight did not return a valid route. Infer the user's current intent from context; answer directly when no external action is required."), nil
	}
	route := routeFromModelIntent(parsed, request.AvailableTools)
	r.mu.Lock()
	if len(r.cache) >= 32 {
		clear(r.cache)
	}
	r.cache[key] = cloneTurnRoute(route)
	r.mu.Unlock()
	return route, nil
}

func modelIntentUsesToolContract(profile string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(profile)), "lfm25")
}

func modelIntentMaxTokens(profile string) int {
	// Muse Glimmer emits a short internal reasoning prelude even with thinking
	// disabled. Eight tokens therefore truncates before its one-digit answer and
	// turns a valid semantic decision into model_unclassified. This is a
	// model-output contract, not a user-keyword rule.
	if strings.Contains(strings.ToLower(strings.TrimSpace(profile)), "muse-glimmer") {
		return 256
	}
	return 8
}

func modelIntentRouteFromNumericResult(result *provider.StreamResult) (router.CompactRoute, bool) {
	if result == nil {
		return router.CompactRoute{}, false
	}
	value := strings.TrimSpace(result.Content)
	if len(value) != 1 || value[0] < '0' || value[0] > '9' {
		return router.CompactRoute{}, false
	}
	intents := [...]router.IntentID{
		"general_conversation", router.IntentInspectOrSearch,
		router.IntentInternetResearch, router.IntentResearchArtifact,
		router.IntentSimpleWrite, router.IntentStaticWebArtifact,
		router.IntentEngineeringFix, router.IntentOfficeArtifact,
		router.IntentGitAction, router.IntentGeneralAction,
	}
	return router.CompactRoute{Intent: intents[int(value[0]-'0')]}, true
}

func modelIntentTool(available []string) provider.Tool {
	toolNames := append([]string(nil), available...)
	sort.Strings(toolNames)
	return provider.Tool{
		Type: "function",
		Function: provider.ToolSchema{
			Name:        modelIntentToolName,
			Description: "Classify the user's requested outcome from the full recent conversation. This records intent only and performs no external action.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"intent": map[string]any{
						"type": "string",
						"enum": []string{
							"general_conversation", "inspect_or_search", "internet_research",
							"research_artifact", "simple_write", "static_web_artifact",
							"engineering_fix", "office_artifact", "git_action", "general_action",
						},
					},
					"tools": map[string]any{
						"type":        "array",
						"description": "Only exact available tool names genuinely needed for the requested outcome. Empty for conversation.",
						"items":       map[string]any{"type": "string", "enum": toolNames},
					},
					"safety": map[string]any{
						"type": "string",
						"enum": []string{"none", "read_only", "network_fetch", "write_project_file", "modify_project_file", "execute_command"},
					},
					"confirm": map[string]any{"type": "boolean"},
				},
				"required":             []string{"intent", "tools", "safety", "confirm"},
				"additionalProperties": false,
			},
		},
	}
}

func modelIntentRouteFromResult(result *provider.StreamResult) (router.CompactRoute, bool) {
	if result == nil {
		return router.CompactRoute{}, false
	}
	for _, call := range result.ToolCalls {
		if strings.TrimSpace(call.Function.Name) != modelIntentToolName {
			continue
		}
		var args modelIntentToolArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			continue
		}
		intent := router.NormalizeCompactIntent(args.Intent)
		if strings.TrimSpace(string(intent)) == "" {
			continue
		}
		return router.CompactRoute{
			Intent: intent, Tools: args.Tools, Safety: args.Safety, Confirm: args.Confirm,
		}, true
	}
	// Text parsing remains only as a protocol-repair path for backends that
	// serialize the requested function call as text. It is not a keyword router.
	if parsed, ok := router.ParseCompactRouteLine(result.Content); ok {
		return parsed, true
	}
	// Some small models understand the semantic classification correctly but
	// narrate the requested enum value instead of emitting a native tool call.
	// Recover only an exact intent ID from the model's output. Deliberately never
	// inspect the user's request here: the model still owns the intent decision.
	return modelIntentRouteFromNarration(result.Content)
}

func modelIntentRouteFromNarration(content string) (router.CompactRoute, bool) {
	normalized := strings.ToLower(strings.TrimSpace(content))
	if normalized == "" {
		return router.CompactRoute{}, false
	}
	intents := []router.IntentID{
		"general_conversation", router.IntentInspectOrSearch,
		router.IntentInternetResearch, router.IntentResearchArtifact,
		router.IntentSimpleWrite, router.IntentStaticWebArtifact,
		router.IntentEngineeringFix, router.IntentOfficeArtifact,
		router.IntentGitAction, router.IntentGeneralAction,
	}
	var matched router.IntentID
	for _, intent := range intents {
		needle := string(intent)
		if !strings.Contains(normalized, needle) {
			continue
		}
		if matched != "" && matched != intent {
			// If the model discussed multiple candidates without choosing one, do
			// not guess which candidate it meant.
			return router.CompactRoute{}, false
		}
		matched = intent
	}
	if matched == "" {
		return router.CompactRoute{}, false
	}
	return router.CompactRoute{Intent: matched}, true
}

func modelIntentSystemPrompt(profile string, available []string) string {
	toolsList := append([]string(nil), available...)
	sort.Strings(toolsList)
	return strings.TrimSpace(fmt.Sprintf(`You are Meadow's intent control plane for profile %s.
Read meaning and conversation context, not isolated keywords. A later correction overrides an earlier plan. A repeated request usually means the prior answer was unsatisfactory.

Choose exactly one intent:
- general_conversation: chat, advice, emotional support, brainstorming, explanation, or a direct answer that needs no external action.
- inspect_or_search: inspect local files without changing them.
- internet_research: current/external facts that require network evidence, answer in chat.
- research_artifact: network research plus a saved report/page/deck.
- simple_write: save supplied or simple content to a file.
- static_web_artifact: create or change an interactive local web page/game.
- engineering_fix: inspect/change a codebase, backend, tests, build, or debug.
- office_artifact: create/transform a document, PDF-derived notes, spreadsheet, presentation, or structured office file.
- git_action: explicit git/repository operation.
- general_action: another local action that genuinely needs tools, including an explicit request to save or search Meadow memory.

If the user says they only want to chat, asks for your own advice, declines search, or asks to cancel the pending action, choose general_conversation even when earlier messages involved tools.
Do not infer that a tool is needed merely because a noun resembles a file, website, search topic, or command. Judge the requested outcome.
Available tool names: %s

Call the classify_intent function exactly once. Do not answer the user and do not perform the requested work in this preflight.`, profile, strings.Join(toolsList, ", ")))
}

func modelIntentNumericSystemPrompt(profile string) string {
	if strings.Contains(strings.ToLower(strings.TrimSpace(profile)), "muse-glimmer") {
		// Muse spends part of every completion on an internal reasoning prelude,
		// even when thinking is disabled. A concise classifier contract leaves
		// enough of its bounded output budget for the actual one-digit decision.
		return `You only classify intent; never solve the task. Return one ASCII digit, no words. 0=chat/advice/no action; 1=read local; 2=web research chat; 3=web research plus saved artifact; 4=write a simple file; 5=interactive web page/game; 6=change/build/test/debug code; 7=document/PDF/sheet/slides; 8=git; 9=other tool action including saving/searching Meadow memory. Classify the latest request by meaning and context. Later corrections override earlier requests. Output the digit now.`
	}
	return strings.TrimSpace(fmt.Sprintf(`You are Meadow's semantic intent classifier for profile %s.
Read the current request together with recent conversation. A later correction overrides an earlier plan. Judge the desired outcome; never classify from isolated words.
Reply with exactly one digit and nothing else:
0 chat, advice, planning, explanation, or a direct answer needing no external action
1 inspect local files without changing them
2 research current or external facts and answer in chat
3 research current or external facts and save a report, page, or deck
4 save supplied or simple content to a file
5 create or change an interactive local web page or game
6 inspect, change, build, test, or debug a codebase or backend
7 create or transform an office document, PDF notes, spreadsheet, or presentation
8 explicit git or repository operation
9 another local action that genuinely needs tools, including explicitly saving or searching Meadow memory`, profile))
}

func modelIntentConversation(request agentcore.TurnRouteRequest) string {
	const maxMessageRunes = 1200
	var parts []string
	current := strings.TrimSpace(router.StripSystemReminder(request.Task))
	start := len(request.Messages) - 6
	if start < 0 {
		start = 0
	}
	lastUserIndex := -1
	for index := len(request.Messages) - 1; index >= start; index-- {
		if request.Messages[index].Role == "user" {
			lastUserIndex = index
			break
		}
	}
	for index := start; index < len(request.Messages); index++ {
		message := request.Messages[index]
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		messageText := strings.TrimSpace(provider.MessageText(message))
		// The Desktop and CLI include the current user request both in Task and
		// as the last conversation message. Keep the explicit current-request
		// label below, but do not make the classifier read the same request twice.
		if index == lastUserIndex && message.Role == "user" && messageText == current {
			continue
		}
		text := []rune(messageText)
		if len(text) > maxMessageRunes {
			text = text[len(text)-maxMessageRunes:]
		}
		if len(text) > 0 {
			parts = append(parts, message.Role+": "+string(text))
		}
	}
	parts = append(parts, "current_user_request: "+current)
	return strings.Join(parts, "\n")
}

func modelIntentRouteKey(request agentcore.TurnRouteRequest) string {
	// Tool/assistant messages grow during one Kernel run, while the sequence of
	// user messages stays stable. Include that user sequence so an identical
	// follow-up such as "continue" is reclassified when it appears again later
	// in the session, without paying a classifier call on every internal loop.
	parts := []string{strings.TrimSpace(request.Task)}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		parts = append(parts, strings.TrimSpace(provider.MessageText(message)))
	}
	return strings.Join(parts, "\x1f")
}

func cloneTurnRoute(route agentcore.TurnRoute) agentcore.TurnRoute {
	route.Tools = append([]string(nil), route.Tools...)
	route.Guidance = append([]string(nil), route.Guidance...)
	return route
}

func routeFromModelIntent(parsed router.CompactRoute, available []string) agentcore.TurnRoute {
	intent := router.NormalizeCompactIntent(string(parsed.Intent))
	route := agentcore.TurnRoute{Intent: string(intent)}
	if intent == "general_conversation" || intent == "conversation" || intent == "none" {
		route.Intent = "general_conversation"
		route.Mode = string(router.TurnModeChat)
		route.DisableTools = true
		return route
	}

	allowed := modelIntentCapabilityFamily(intent)
	for _, name := range parsed.Tools {
		allowed[router.NormalizeCompactTool(name)] = true
	}
	route.Tools = selectTools(available, allowed)
	switch intent {
	case router.IntentInspectOrSearch:
		route.Mode = string(router.TurnModeExplore)
	case router.IntentInternetResearch, router.IntentResearchArtifact,
		router.IntentSimpleWrite, router.IntentSimpleCopy, router.IntentSimpleAppend,
		router.IntentDirectoryCreate, router.IntentShellScript, router.IntentConfigArtifact,
		router.IntentDeleteGenerated, router.IntentStaticWebArtifact,
		router.IntentEngineeringFix, router.IntentOfficeArtifact, router.IntentGitAction,
		router.IntentDelegatedLocalTask, router.IntentGeneralAction:
		route.Mode = string(router.TurnModeAgent)
	default:
		route.Mode = string(router.TurnModeAgent)
		// Unknown model intents fail open. This is deliberately not sent to the
		// legacy regexp router.
		route.Tools = append([]string(nil), available...)
	}
	return route
}

func modelIntentCapabilityFamily(intent router.IntentID) map[string]bool {
	set := func(names ...string) map[string]bool {
		out := make(map[string]bool, len(names))
		for _, name := range names {
			out[name] = true
		}
		return out
	}
	read := []string{router.ToolRead, router.ToolGlob, router.ToolGrep, router.ToolList,
		"pdf_extract", rag.DocumentSearchToolName, rag.DocumentReadToolName,
		"memory_search", tools.AskUserToolName}
	write := []string{router.ToolWrite, tools.WriteFilesToolName, router.ToolEdit,
		"memory_remember"}
	research := []string{router.ToolWebSearch, router.ToolWebFetch, router.ToolWebCrawl,
		router.ToolBrowsePage, "meadow_browser_dom", "pdf_extract", "encyclopedia_search",
		rag.DocumentSearchToolName, rag.DocumentReadToolName, tools.AskUserToolName}
	switch intent {
	case router.IntentInspectOrSearch:
		return set(read...)
	case router.IntentInternetResearch:
		return set(research...)
	case router.IntentResearchArtifact:
		return set(append(research, append(write, router.ToolVisual, "presentation_artifact", tools.TaskPlanToolName)...)...)
	case router.IntentSimpleWrite, router.IntentSimpleCopy, router.IntentSimpleAppend:
		return set(append(read, write...)...)
	case router.IntentStaticWebArtifact:
		return set(append(append(read, write...), router.ToolWebBuild, router.ToolVisual,
			router.ToolInteraction, router.ToolShell, "process", tools.TaskPlanToolName)...)
	case router.IntentEngineeringFix:
		return set(append(append(read, write...), router.ToolShell, "process", router.ToolWebBuild,
			router.ToolVisual, router.ToolInteraction, tools.TaskPlanToolName)...)
	case router.IntentOfficeArtifact:
		return set(append(append(read, write...), router.ToolShell, "presentation_artifact",
			router.ToolVisual, tools.TaskPlanToolName)...)
	case router.IntentGitAction, router.IntentShellScript, router.IntentConfigArtifact,
		router.IntentDirectoryCreate, router.IntentDeleteGenerated:
		return set(append(append(read, write...), router.ToolShell, "process", tools.AskUserToolName)...)
	case router.IntentGeneralAction:
		return set("memory_search", "memory_remember", tools.AskUserToolName)
	default:
		return map[string]bool{}
	}
}
