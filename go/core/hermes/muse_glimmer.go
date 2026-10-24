package hermes

import (
	"context"
	"errors"
	"strings"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"github.com/hey-meadow/meadow-code/go/core/prompt"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/rag"
	"github.com/hey-meadow/meadow-code/go/core/router"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	MuseGlimmerProfileID = "muse-glimmer-agent-v0"

	museHistoryMaxBytes             = 20 * 1024
	museToolResultMaxBytes          = 6 * 1024
	museDuplicateToolResultMinBytes = 512
	museMutationArgumentMaxBytes    = 512

	nemotron30HistoryMaxBytes             = 384 * 1024
	nemotron30ToolResultMaxBytes          = 32 * 1024
	nemotron30DuplicateToolResultMinBytes = 512
	nemotron30MutationArgumentMaxBytes    = 24 * 1024
)

// MuseGlimmerSpec is deliberately independent from Office35Spec. The shared
// types below are kernel contracts, not Qwen behavior.
type MuseGlimmerSpec struct {
	Client             agentcore.KernelModel
	ModelID            string
	SelfIteration      string
	WorkState          string
	SystemPrompt       string
	CWD                string
	ContextManager     *agentcore.ContextManager
	TraceObserver      agenttrace.Observer
	Registry           *tools.Registry
	Workspace          *tools.Workspace
	UseRouter          bool
	ModelIntent        bool
	RAG                *rag.Store
	DocumentStore      *rag.DocumentStore
	Cartridges         *rag.CartridgeStore
	CartridgeScope     rag.CartridgeScope
	RAGTopK            int
	RAGSourceIDs       []string
	SubtaskStateDir    string
	ParentSessionID    string
	AskUser            tools.Asker
	Authorize          agentcore.AuthorizeFunc
	RequireToolAccount func(context.Context) error
}

// NewMuseGlimmerRuntime builds the isolated Muse experiment pipeline. It does
// not use Qwen's semantic preflight, registry, prompt, sampling assumptions, or
// parallel-tool policy.
func NewMuseGlimmerRuntime(spec MuseGlimmerSpec, hooks Hooks) (agentcore.Runtime, error) {
	if spec.Client == nil {
		return nil, errors.New("Muse Glimmer requires a model client")
	}
	if spec.Registry == nil || spec.Workspace == nil {
		return nil, errors.New("Muse Glimmer requires a workspace tool registry")
	}
	if !spec.UseRouter {
		return nil, errors.New("Muse Glimmer requires its dedicated router")
	}
	if spec.ContextManager == nil {
		spec.ContextManager = agentcore.NewContextManager(agentcore.ContextPolicy{})
	}
	selfIteration, err := featureSwitch(spec.SelfIteration, true, "self iteration")
	if err != nil {
		return nil, err
	}
	if !selfIteration {
		return nil, errors.New("Muse Glimmer evaluation requires self iteration")
	}
	workState, err := featureSwitch(spec.WorkState, false, "agent work state")
	if err != nil {
		return nil, err
	}

	registry := tools.MuseGlimmerAgentRegistry(spec.Registry, spec.Workspace)
	if spec.DocumentStore != nil {
		registerEvidenceIngestTools(registry, spec.DocumentStore, spec.Cartridges, spec.CartridgeScope,
			"web_fetch", "meadow_browser_dom", "pdf_extract")
		registry.Register(&rag.DocumentSearchTool{Store: spec.DocumentStore})
		registry.Register(&rag.DocumentReadTool{Store: spec.DocumentStore})
	}
	registry.Register(&tools.ResearchPublish{WS: spec.Workspace})
	registry.Register(&tools.AskUser{Ask: spec.AskUser})
	registerCartridgeMemoryTools(registry, spec.Cartridges, spec.CartridgeScope, spec.AskUser)
	authorize := spec.Authorize
	if authorize == nil {
		authorize = safeProductAuthorizer
	}
	isNemotron := provider.IsNemotronModel(spec.ModelID)
	profileID := MuseGlimmerProfileID
	systemPrompt := prompt.ForMuseGlimmerAgent(spec.SystemPrompt)
	var routeSystemPrompt func(agentcore.TurnRoute) string
	maxTurns := 24
	contextManager := spec.ContextManager.
		WithHistoryBounds(museHistoryMaxBytes, 6).
		WithToolResultBounds(museToolResultMaxBytes, museToolResultMaxBytes).
		WithDuplicateToolResultBounds(museDuplicateToolResultMinBytes).
		WithMutationArgumentBounds(museMutationArgumentMaxBytes)
	if isNemotron {
		profileID = "nemotron-30b-agent-v1"
		systemPrompt = prompt.ForNemotron30Agent(spec.SystemPrompt)
		routeSystemPrompt = nemotron30RouteSystemPrompt
		// Live Desktop acceptance showed that this checkpoint can keep issuing
		// syntactically valid but semantically stagnant read/edit cycles after a
		// failed browser contract. Sixteen turns still leave room for write,
		// visual check, interaction check and several grounded repairs without a
		// five-minute 300K-token loop.
		maxTurns = 16
		contextManager = spec.ContextManager.
			WithHistoryBounds(nemotron30HistoryMaxBytes, 12).
			WithToolResultBounds(nemotron30ToolResultMaxBytes, nemotron30ToolResultMaxBytes).
			WithDuplicateToolResultBounds(nemotron30DuplicateToolResultMinBytes).
			WithMutationArgumentBounds(nemotron30MutationArgumentMaxBytes)
	}

	var retriever agentcore.ContextRetriever
	if spec.RAG != nil {
		retriever = kernelRetriever{
			store: spec.RAG, topK: spec.RAGTopK, profile: profileID,
			includeSourceIDs: append([]string(nil), spec.RAGSourceIDs...),
		}
	}
	retriever = withCartridgeRetriever(retriever, spec.Cartridges, profileID, spec.CartridgeScope)

	turnRouter := agentcore.TurnRouter(museGlimmerRouter{artifactRoot: spec.Workspace.ArtifactModelRoot()})
	if spec.ModelIntent {
		turnRouter = newModelIntentRouter(spec.Client, spec.ModelID, profileID)
	}
	if isNemotron {
		turnRouter = nemotron30Router{inner: turnRouter}
	}
	kernel, err := agentcore.NewKernelRuntime(agentcore.KernelConfig{
		Client:            spec.Client,
		ModelID:           spec.ModelID,
		SystemPrompt:      systemPrompt,
		RouteSystemPrompt: routeSystemPrompt,
		CWD:               spec.CWD,
		Workspace:         spec.Workspace,
		ContextManager:    contextManager,
		// The resident model, not a keyword table, owns user-intent selection.
		TurnRouter:       turnRouter,
		ContextRetriever: retriever,
		DocumentIngestor: documentIngestor{store: spec.DocumentStore},
		TraceObserver:    spec.TraceObserver,
		AdmissionPolicy:  productAdmission,
		TaskProfile: func(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
			profile := museGlimmerTaskProfile(task, route)
			if isNemotron {
				profile = nemotron30TaskProfile(task, route)
			}
			if router.IntentID(route.Intent) == router.IntentStaticWebArtifact {
				profile.RequiredArtifactRoot = spec.Workspace.ArtifactModelRoot()
				profile.RequiredArtifactPathArgs = map[string][]string{
					router.ToolWrite: {"file_path"}, tools.WriteFilesToolName: {"first_file_path", "second_file_path"},
					router.ToolEdit:     {"file_path"},
					router.ToolWebBuild: {"html_path", "source_path"},
					router.ToolVisual:   {"file_path"}, router.ToolInteraction: {"file_path"},
				}
				if !explicitExistingArtifactValidation(task) && !explicitExistingArtifactRepair(task) {
					if isNemotron {
						profile.InitialTool = router.ToolWrite
					} else {
						profile.InitialTool = tools.WriteFilesToolName
					}
				}
			}
			return profile
		},
		ModelLed:                   true,
		NativeAutoToolChoice:       true,
		NativeCheckpointToolChoice: true,
		FreshNarrowedToolRecovery:  true,
		IncludeStreamingUsage:      true,
		VerifiedFastFinal:          true,
		DisableSelfIteration:       false,
		SemanticContract: agentcore.SemanticContractPolicy{
			Enabled: false,
		},
		WorkState: agentcore.WorkStatePolicy{
			Enabled: workState,
			Eligible: func(_ string, route agentcore.TurnRoute) bool {
				return route.Mode == string(router.TurnModeAgent)
			},
		},
		Research:        productResearchPolicy(spec.ModelIntent, 2, 6, false),
		Subtasks:        agentcore.SubtaskPolicy{Enabled: false},
		MaxTurns:        maxTurns,
		SubtaskStateDir: spec.SubtaskStateDir, ParentSessionID: spec.ParentSessionID,
		Executor: &agentcore.ToolExecutor{
			RequireAccount: spec.RequireToolAccount,
			Registry:       registry, Authorize: authorize, Parallel: false, MaxParallel: 1,
			OnToolCall: hooks.OnToolCall, OnToolResult: hooks.OnToolResult,
			OnToolStart: hooks.OnToolStart, OnToolComplete: hooks.OnToolComplete,
		},
		Hooks: agentcore.KernelHooks{
			OnMessageBoundary: hooks.OnMessageBoundary,
			OnContent:         hooks.OnContent, OnThought: hooks.OnThought,
			OnProgress: hooks.OnProgress, OnToolDraft: hooks.OnToolDraft,
			OnToolDraftProgress: hooks.OnToolDraftProgress,
			OnTurn:              hooks.OnTurn, OnGuidance: hooks.OnGuidance,
			OnCheckpoint: hooks.OnCheckpoint,
		},
		ValidationPolicy: func(task string) agentcore.ValidationRequest {
			return agentcore.ValidationRequest{
				CWD: spec.CWD, ExplicitCommand: router.NamedValidationCommand(task),
				RepositoryValidation: false, ShellTool: router.ToolShell,
			}
		},
		Deliverables: func(task string) []agentcore.Deliverable {
			return taskDeliverables(spec.Workspace, task)
		},
		MutationScope: router.TaskExplicitMutationScope,
	})
	if err != nil {
		return nil, err
	}
	if isNemotron {
		return &nemotron30Runtime{KernelRuntime: kernel, modelID: spec.ModelID}, nil
	}
	return kernel, nil
}

// nemotron30Runtime turns the model/role boundary into a product handoff event.
// Desktop renders the event as one explicit action and, after the user accepts
// it, switches to the coding model and resumes the existing request. The 30B
// checkpoint still owns multimodal office work; interaction-heavy programming
// no longer burns a long agent loop before admitting that it needs 35B.
type nemotron30Runtime struct {
	*agentcore.KernelRuntime
	modelID string
}

func (r *nemotron30Runtime) RunInput(ctx context.Context, input agentcore.RunInput) (*agentcore.RunResult, error) {
	result, err := r.KernelRuntime.RunInput(ctx, input)
	if result != nil {
		result.Escalation = nemotron30Escalation(r.modelID, input, result)
	}
	return result, err
}

func nemotron30Escalation(modelID string, input agentcore.RunInput, result *agentcore.RunResult) *agentcore.ModelEscalation {
	if result == nil || result.Stop == agentcore.StopCancelled || result.Stop == agentcore.StopNeedsInput ||
		!nemotron30NeedsCodingHandoff(provider.MessageText(input.User)) {
		return nil
	}
	originalTask := strings.TrimSpace(provider.MessageText(input.User))
	const maxResumeTaskRunes = 1200
	if runes := []rune(originalTask); len(runes) > maxResumeTaskRunes {
		originalTask = string(runes[:maxResumeTaskRunes]) + "…"
	}
	return &agentcore.ModelEscalation{
		SourceModel:      strings.TrimSpace(modelID),
		RecommendedModel: "Meadow Coder 35B",
		Reason:           "interaction_heavy_coding",
		ResumePrompt:     "請接手並完成下列互動式程式任務。保留指定檔名，實際建立、執行並只用右側 Meadow Browser 完成互動驗收；不要只提供程式碼草稿、不要另開瀏覽器，也不要重複詢問需求。\n\n原始需求：\n" + originalTask,
	}
}

func nemotron30NeedsCodingHandoff(task string) bool {
	return ornithInteractiveGameTask(strings.TrimSpace(task))
}

func nemotron30TaskProfile(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
	if strings.TrimSpace(route.Intent) == "coding_model_handoff" {
		// The handoff is a terminal product capability boundary, not an unfinished
		// artifact attempt. Disable the generic deliverable controller so it cannot
		// turn a tool-free refusal into repeated "write the missing file" nudges.
		return agentcore.TaskProfile{Intent: route.Intent}
	}
	profile := museGlimmerTaskProfile(task, route)
	if router.IntentID(route.Intent) != router.IntentStaticWebArtifact ||
		!standaloneHTMLWithoutExplicitBuild(task) {
		return profile
	}

	// The shipped Meadow 30B is Nemotron MoE, not the retired dense Muse
	// checkpoint.  A small standalone game should therefore stay one browser-
	// ready HTML file instead of entering Muse's forced two-file TypeScript loop.
	profile.InitialTool = router.ToolWrite
	profile.InitialToolUntilMutations = 1
	profile.ForceRequiredEvidenceAfterMutations = 1
	profile.RequiredEvidenceTools = withoutMuseTool(profile.RequiredEvidenceTools, router.ToolWebBuild)
	profile.RequiredEvidenceTools = withoutMuseTool(profile.RequiredEvidenceTools, router.ToolRead)
	profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolVisual)
	requiresInteraction := explicitWebInteractionEvidenceTask(task) || ornithInteractiveGameTask(task)
	if requiresInteraction {
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolInteraction)
	}
	profile.EvidenceTools = withoutMuseTool(profile.EvidenceTools, router.ToolWebBuild)
	delete(profile.RequiredEvidenceRepairTools, router.ToolWebBuild)
	delete(profile.RequiredEvidenceRepairTools, router.ToolInteraction)
	delete(profile.RequiredEvidenceFailureLimits, router.ToolWebBuild)
	if profile.RequiredEvidenceFailureLimits == nil {
		profile.RequiredEvidenceFailureLimits = make(map[string]int)
	}
	// Keep long-task self-iteration, but stop a small model from spending the
	// entire context on the same semantic validator defect. Four fresh failures
	// still allow initial diagnosis plus three materially different repairs.
	profile.RequiredEvidenceFailureLimits[router.ToolInteraction] = 4
	delete(profile.RequiredEvidenceBindings, router.ToolWebBuild)
	if profile.RequiredEvidenceBindings == nil {
		profile.RequiredEvidenceBindings = make(map[string]agentcore.RequiredEvidenceBinding)
	}
	for _, tool := range []string{router.ToolVisual, router.ToolInteraction} {
		profile.RequiredEvidenceBindings[tool] = agentcore.RequiredEvidenceBinding{
			MutationPathArgs: []string{"file_path"},
		}
	}
	interactionQuotes := ornithExplicitInteractionRequirementQuotes(task)
	profile.RequiredInteractionCoverageQuotes = interactionQuotes
	profile.RequireInteractionCoverageQuotes = len(interactionQuotes) > 0
	if len(interactionQuotes) == 0 {
		if args, ok := nemotron30DeterministicGameInteraction(task); ok {
			profile.DeterministicRequiredEvidenceArgs = map[string]map[string]any{
				router.ToolInteraction: args,
			}
		}
	}
	profile.TaskStartPrompt = "NEMOTRON_WEB_SAVE_REQUIRED: create the requested standalone page or game now with one native write_file call. Before writing, audit every requested visible control, keyboard mapping, state transition, and test hook; implement all of them in that first complete draft. Named start, pause/resume, and restart controls must be visible, separately wired, and have stable ids. Save one complete self-contained HTML file with CSS and JavaScript inline under /workspace. Do not create TypeScript or invoke a build. Do not paste code as prose."
	if ornithInteractiveGameTask(task) {
		profile.TaskStartPrompt += deterministicSnakeGameContract
	}
	profile.MutationRequiredPrompt = "NEMOTRON_WEB_MUTATION_REQUIRED: persist the complete self-contained HTML with write_file now; a code block is not a saved artifact."
	profile.MutationRecoveryPrompt = "NEMOTRON_WEB_RECOVERY: reuse the existing draft and issue one corrected native write_file call for the standalone HTML now."
	profile.ModelDirectedEvidenceRecovery = true
	profile.RepairPostActionInteractionImmediately = false
	profile.ReplayPostActionInteractionAfterRepair = false
	return profile
}

const deterministicSnakeGameContract = ` GAME_ACCEPTANCE_REQUIRED: expose window.render_game_to_text() with compact JSON state and window.advanceTime(ms) with deterministic tick advancement. The state JSON must include running and paused booleans, numeric score, and head:{x,y}; direction may be a name or vector because acceptance proves actual coordinate movement rather than an internal representation. Use stable ids startBtn and pauseBtn plus restartBtn or resetBtn; pauseBtn must toggle pause/resume. Use one tick function for both the timer and advanceTime, and make advanceTime a no-op while paused. Keep the snake, food, movement step, collision bounds, and canvas in one consistent coordinate system. The tick must update state and redraw the canvas. Before the script ends, initialize every array/object read by Start, paint the snake and food once, and leave all three controls visible so Start never reads undefined state and the initial canvas is not blank. After saving, run both meadow_browser_visual_check and meadow_browser_interaction_check; verify that the game starts, accepts its documented movement keys, visibly advances, pauses without movement, resumes, and restarts. A clean page load alone is not completion.`

// nemotron30DeterministicGameInteraction is host-authored only for the
// recognized Snake product contract. The profile already requires stable
// controls and state hooks for Snake, so asking a 30B model to rediscover the
// same long validator JSON after writing the game adds failure surface without
// adding task semantics. Other games keep model-authored interaction plans.
func nemotron30DeterministicGameInteraction(task string) (map[string]any, bool) {
	lower := strings.ToLower(task)
	if !containsAnyText(lower, "貪食蛇", "贪食蛇", "snake") {
		return nil, false
	}
	steps := []any{
		map[string]any{"action": "click", "selector": "#startBtn"},
		map[string]any{"action": "assert_game_state", "contains": []any{`"running":true`, `"paused":false`}},
		map[string]any{"action": "assert_canvas_pixels", "selector": "canvas"},
		map[string]any{"action": "press_key", "key": "ArrowDown"},
		map[string]any{"action": "advance_time", "ms": 240},
		map[string]any{"action": "assert_game_state", "contains": []any{`"running":true`}, "changed_from_previous": []any{"head.y"}, "unchanged_from_previous": []any{"head.x"}},
		map[string]any{"action": "press_key", "key": "d"},
		map[string]any{"action": "advance_time", "ms": 240},
		map[string]any{"action": "assert_game_state", "contains": []any{`"running":true`}, "changed_from_previous": []any{"head.x"}, "unchanged_from_previous": []any{"head.y"}},
		map[string]any{"action": "click", "selector": "#pauseBtn"},
		map[string]any{"action": "assert_game_state", "contains": []any{`"paused":true`}},
		map[string]any{"action": "advance_time", "ms": 240},
		map[string]any{"action": "assert_game_state", "contains": []any{`"paused":true`}, "unchanged_from_previous": []any{"head"}},
		map[string]any{"action": "click", "selector": "#pauseBtn"},
		map[string]any{"action": "assert_game_state", "contains": []any{`"running":true`, `"paused":false`}},
		map[string]any{"action": "advance_time", "ms": 240},
		map[string]any{"action": "assert_game_state", "contains": []any{`"running":true`}, "changed_from_previous": []any{"head"}},
		map[string]any{"action": "click", "selector": "#restartBtn"},
		map[string]any{"action": "assert_game_state", "contains": []any{`"score":0`}, "changed_from_previous": []any{"head"}},
	}
	coverage := []any{
		map[string]any{"requirement": "start", "action_steps": []any{1}, "assertion_steps": []any{2, 3}},
		map[string]any{"requirement": "arrow movement", "action_steps": []any{4}, "assertion_steps": []any{6}},
		map[string]any{"requirement": "WASD movement", "action_steps": []any{7}, "assertion_steps": []any{9}},
		map[string]any{"requirement": "pause", "action_steps": []any{10}, "assertion_steps": []any{11, 13}},
		map[string]any{"requirement": "resume", "action_steps": []any{14}, "assertion_steps": []any{15, 17}},
		map[string]any{"requirement": "restart", "action_steps": []any{18}, "assertion_steps": []any{19}},
	}
	return map[string]any{"steps": steps, "coverage": coverage, "settle_ms": 0}, true
}

func nemotron30RouteSystemPrompt(route agentcore.TurnRoute) string {
	if strings.TrimSpace(route.Intent) != "coding_model_handoff" {
		return ""
	}
	return `You are the Meadow 30B capability boundary. This request has already
been classified as interaction-heavy Coding and must be handed to Meadow Coder
35B. Reply in the user's language with one short sentence only: say to switch
to Meadow Coder 35B, that the original request will be preserved, and that no
file has been created yet. Do not draft code, JSON, Markdown, or tool syntax.`
}

func withoutMuseTool(names []string, removed string) []string {
	result := names[:0]
	for _, name := range names {
		if name != removed {
			result = append(result, name)
		}
	}
	return result
}

type nemotron30Router struct {
	inner agentcore.TurnRouter
}

func (r nemotron30Router) Route(ctx context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	if r.inner == nil {
		return agentcore.TurnRoute{}, errors.New("Nemotron 30B router requires an inner router")
	}
	route, err := r.inner.Route(ctx, request)
	if err != nil {
		return route, err
	}
	applyQwen38ModelLedStaticWeb(&route, request.AvailableTools, "", request.Task)
	if nemotron30NeedsCodingHandoff(request.Task) {
		// Meadow 30B is the multimodal/office product. The current shipped
		// Nemotron checkpoint has only 3B active parameters and live acceptance
		// shows it can emit valid native tools while repeatedly applying semantic
		// no-op edits to an interaction failure. Hand off before mutation so the
		// user's workspace cannot be replaced by a known-unverified draft.
		route.Intent = "coding_model_handoff"
		route.Mode = string(router.TurnModeChat)
		route.Tools = nil
		route.DisableTools = true
		route.Guidance = nil
		return route, nil
	}
	if router.IntentID(route.Intent) == router.IntentStaticWebArtifact &&
		(explicitWebInteractionEvidenceTask(request.Task) || ornithInteractiveGameTask(request.Task)) {
		if containsTool(request.AvailableTools, router.ToolInteraction) {
			route.Tools = appendUniqueHermesTool(route.Tools, router.ToolInteraction)
		}
		route.Guidance = append(route.Guidance,
			`For meadow_browser_interaction_check, use assert_game_state for internal game fields such as state, direction, snake coordinates, and score; assert_dom only searches visible text. A typical pair is {"action":"click","selector":"#startBtn"} followed by {"action":"assert_game_state","contains":["\"state\":\"running\""]}. steps must be an array. When the user lists multiple quoted behaviors, preserve every exact quote as its own coverage row, with 1-based action_steps and later assertion_steps; do not test only the first item. text is a scalar used only by fill, and fill always requires a CSS selector. A validator-schema error is not a source defect; neither is a selector, coverage, or assertion-format error. Correct the validator arguments instead of editing valid HTML.`,
		)
	}
	return route, nil
}

func museGlimmerTaskProfile(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
	profile := baseTaskProfile(task, route)
	if localPDFHTMLDocumentTask(task) {
		profile.RequiresInspection = true
		profile.RequiresMutation = true
		profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, "pdf_extract")
		profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, router.ToolVisual)
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, "pdf_extract")
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolVisual)
	}
	if router.IntentID(route.Intent) == router.IntentOfficeArtifact &&
		router.TaskRequiresExecutableValidation(task) {
		// Dense Muse can usually author a computed office artifact in one shell
		// call, but without an explicit acceptance capability it tends to spend
		// several expensive turns rereading the same correct JSON. Require a real
		// fail-capable command result for tasks that explicitly ask for computation
		// or executable verification. The model still owns the checker and all task
		// semantics; the profile only refuses to treat inspection as proof.
		profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, router.ToolShell)
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolShell)
		profile.ForceRequiredEvidenceAfterMutations = 1
	}
	if router.IntentID(route.Intent) == router.IntentStaticWebArtifact {
		// Muse is reliable at authoring the files but can spend several expensive
		// decode turns restating build and visual calls the controller already
		// selected. The controller may auto-run only stages whose paths are uniquely
		// bound to artifacts mutated in this run. Interaction assertions remain
		// model-owned because they encode the user's requested behavior.
		profile.AutoExecuteDeterministicEvidence = true
		profile.RepairPostActionInteractionImmediately = true
		profile.ReplayPostActionInteractionAfterRepair = true
		// Muse emits one native tool call per response. A transactional write_files
		// call lets it save the HTML shell and TypeScript source together without
		// giving the Kernel any task-specific code or semantics.
		profile.InitialToolUntilMutations = 2
		profile.TaskStartPrompt = "MUSE_MULTI_FILE_SAVE_REQUIRED: create the requested app now with one native write_files call containing exactly two complete entries: a small HTML shell that loads its sibling compiled .js file, and a sibling TypeScript source containing all interactive logic. Use matching basenames, preserve every user requirement, and do not emit prose. Meadow will commit both files atomically, compile them, and then request interaction assertions."
	}
	if router.IntentID(route.Intent) == router.IntentStaticWebArtifact && explicitExistingArtifactValidation(task) {
		profile.RequiresMutation = false
		profile.ForceRequiredEvidenceAfterMutations = 0
		if explicitExistingArtifactConditionalRepair(task) {
			profile.ForceRequiredEvidenceImmediately = false
			profile.ForceRequiredEvidenceAfterInspections = 2
		} else {
			profile.ForceRequiredEvidenceImmediately = true
		}
		for tool, binding := range profile.RequiredEvidenceBindings {
			binding.AllowPreexistingPaths = true
			profile.RequiredEvidenceBindings[tool] = binding
		}
	}
	return profile
}

type museGlimmerRouter struct {
	artifactRoot string
}

func (r museGlimmerRouter) Route(ctx context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	route, err := (advisoryRouter{artifactRoot: r.artifactRoot}).Route(ctx, request)
	if err != nil {
		return route, err
	}
	if isConversationOnlyRoute(route) {
		return route, nil
	}
	route = narrowMuseGlimmerRoute(route, request.AvailableTools, request.Task)
	if localPDFHTMLDocumentTask(request.Task) {
		route.Intent = string(router.IntentOfficeArtifact)
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
		route.Tools = selectTools(request.AvailableTools, map[string]bool{
			router.ToolRead: true, router.ToolWrite: true, router.ToolEdit: true,
			"pdf_extract": true, rag.DocumentSearchToolName: true, rag.DocumentReadToolName: true,
			router.ToolVisual: true, tools.TaskPlanToolName: true,
		})
	}
	if backendApplicationTask(request.Task) {
		route.Intent = string(router.IntentEngineeringFix)
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
		route.Tools = selectTools(request.AvailableTools, map[string]bool{
			router.ToolRead: true, router.ToolWrite: true, router.ToolEdit: true,
			router.ToolShell: true, "process": true, router.ToolGlob: true,
			router.ToolGrep: true, router.ToolList: true, tools.TaskPlanToolName: true,
		})
	}
	if researchHTMLFollowupTask(request.Task) {
		route.Intent = string(router.IntentOfficeArtifact)
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
		route.Tools = selectTools(request.AvailableTools, map[string]bool{
			rag.DocumentSearchToolName: true, rag.DocumentReadToolName: true,
			router.ToolWrite: true, router.ToolEdit: true, router.ToolVisual: true,
		})
		route.Guidance = append(route.Guidance, "Reuse the already indexed research evidence and original source URLs; do not replace them with a new search unless evidence is missing.")
		return route, nil
	}
	// "Meadow Browser" is also the local HTML acceptance surface. Once the
	// advisory router has classified an explicit create/repair request as a
	// static-web artifact, that validator name must not promote the task into
	// internet research. Otherwise a local Snake request receives only search,
	// fetch, PDF and DOM tools and can never write the requested file.
	if router.IntentID(route.Intent) == router.IntentStaticWebArtifact {
		return route, nil
	}
	if !productResearchEligible(request.Task) {
		return route, nil
	}
	plan := router.Classify(router.StripSystemReminder(request.Task))
	if plan != nil && plan.ID == router.IntentResearchArtifact {
		route.Intent = string(router.IntentResearchArtifact)
	} else {
		route.Intent = string(router.IntentInternetResearch)
	}
	route.Mode = string(router.TurnModeAgent)
	route.DisableTools = false
	route.Tools = selectTools(request.AvailableTools, map[string]bool{
		router.ToolWebSearch: true, router.ToolWebFetch: true,
		"meadow_browser_dom": true, "pdf_extract": true,
		rag.DocumentSearchToolName: true, rag.DocumentReadToolName: true,
		router.ToolRead: true, router.ToolWrite: true, router.ToolEdit: true,
		router.ToolPresentation: true, tools.TaskPlanToolName: true,
	})
	// The experiment serializes source acquisition. Two independent source
	// requests may be emitted together, but execution order remains stable.
	if strings.Contains(strings.ToLower(request.Task), "two sources") ||
		strings.Contains(request.Task, "兩個來源") || strings.Contains(request.Task, "二個來源") {
		route.Guidance = append(route.Guidance, "The user already fixed the source count. Do not ask which sources to use; select exactly the named source types and proceed.")
	}
	return route, nil
}

// narrowMuseGlimmerRoute keeps the dense 30B model's hot prompt proportional
// to the job. Muse's sliding-window layers cannot reuse the shared attention KV
// pool, so presenting unrelated browser, research, PDF, presentation, and web
// build schemas on every local CSV/JSON turn creates a large, repeated prefill
// tax. This map is intent-level capability routing only: it does not select a
// next action, inspect task answers, or force a workflow.
func narrowMuseGlimmerRoute(route agentcore.TurnRoute, available []string, task string) agentcore.TurnRoute {
	switch router.IntentID(route.Intent) {
	case router.IntentOfficeArtifact:
		allowed := map[string]bool{
			router.ToolRead: true, router.ToolWrite: true, router.ToolEdit: true,
			router.ToolShell: true, router.ToolGlob: true, router.ToolList: true,
		}
		targets := router.TaskTargetFiles(task)
		if router.TaskRequiresPresentationArtifact(task) {
			allowed[router.ToolPresentation] = true
		}
		// A single-output data transform has no useful plan fan-out: Muse creates
		// one task_plan entry and never updates it, paying a full dense-model turn
		// before doing the work. Keep planning available for genuinely composite
		// or unnamed office work where it can still coordinate deliverables.
		if len(targets) != 1 || router.TaskRequiresPresentationArtifact(task) {
			allowed[tools.TaskPlanToolName] = true
		}
		if strings.Contains(strings.ToLower(task), ".pdf") {
			allowed["pdf_extract"] = true
		}
		route.Tools = selectTools(available, allowed)
	case router.IntentEngineeringFix, router.IntentDelegatedLocalTask:
		route.Tools = selectTools(available, map[string]bool{
			router.ToolRead: true, router.ToolWrite: true, router.ToolEdit: true,
			router.ToolShell: true, "process": true, router.ToolGlob: true,
			router.ToolGrep: true, router.ToolList: true, tools.TaskPlanToolName: true,
		})
	case router.IntentInspectOrSearch:
		route.Tools = selectTools(available, map[string]bool{
			router.ToolRead: true, router.ToolGlob: true,
			router.ToolGrep: true, router.ToolList: true, "pdf_extract": true,
		})
	}
	return route
}
