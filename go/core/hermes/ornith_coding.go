package hermes

import (
	"context"
	"errors"
	"strings"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"github.com/hey-meadow/meadow-code/go/core/prompt"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/router"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	OrnithCodingProfileID = "ornith-coding-agent-v1"

	// The product exposes a 128K workspace, but history grows on demand instead
	// of eagerly pre-filling it. Tool observations and persisted file payloads
	// remain independently bounded so long coding loops retain diagnostics.
	ornithHistoryMaxBytes             = 384 * 1024
	ornithToolResultMaxBytes          = 32 * 1024
	ornithDuplicateToolResultMinBytes = 512
	ornithMutationArgumentMaxBytes    = 1024
)

// OrnithCodingSpec is independent from the Qwen Office35 and Muse profiles.
// The Kernel provides transport, safety and evidence accounting; Ornith owns
// the plan, file selection, edits and repair strategy.
type OrnithCodingSpec struct {
	Client             agentcore.KernelModel
	ModelID            string
	IntentPredictor    SemanticIntentPredictor
	SelfIteration      string
	WorkState          string
	SystemPrompt       string
	CWD                string
	ContextManager     *agentcore.ContextManager
	PromptTokenLimit   int
	TraceObserver      agenttrace.Observer
	Registry           *tools.Registry
	Workspace          *tools.Workspace
	UseRouter          bool
	SubtaskStateDir    string
	ParentSessionID    string
	AskUser            tools.Asker
	Authorize          agentcore.AuthorizeFunc
	RequireToolAccount func(context.Context) error
}

// NewOrnithCodingRuntime builds the shared CLI/Desktop 35B agent runtime.
// The resident model owns conversation, research, coding and repair choices;
// the Harness supplies capabilities, safety, persistence and evidence state.
func NewOrnithCodingRuntime(spec OrnithCodingSpec, hooks Hooks) (agentcore.Runtime, error) {
	if spec.Client == nil {
		return nil, errors.New("Ornith Coding requires a model client")
	}
	if spec.Registry == nil || spec.Workspace == nil {
		return nil, errors.New("Ornith Coding requires a workspace tool registry")
	}
	if !isOrnithCodingModel(spec.ModelID) {
		return nil, errors.New("Ornith Coding profile cannot be applied to a non-Ornith model")
	}
	if !spec.UseRouter {
		return nil, errors.New("Ornith Coding requires its coding router")
	}
	if spec.ContextManager == nil {
		spec.ContextManager = agentcore.NewContextManager(agentcore.ContextPolicy{})
	}
	selfIteration, err := featureSwitch(spec.SelfIteration, true, "self iteration")
	if err != nil {
		return nil, err
	}
	if !selfIteration {
		return nil, errors.New("Ornith Coding requires self iteration and fresh validation")
	}
	workState, err := featureSwitch(spec.WorkState, true, "agent work state")
	if err != nil {
		return nil, err
	}

	registry := tools.OrnithCodingRegistry(spec.Registry, spec.Workspace)
	if registry == nil {
		return nil, errors.New("Ornith Coding could not build its coding registry")
	}
	registry.Register(&tools.AskUser{Ask: spec.AskUser})
	authorize := spec.Authorize
	if authorize == nil {
		authorize = safeProductAuthorizer
	}
	contextManager := spec.ContextManager.
		WithHistoryBounds(ornithHistoryMaxBytes, 12).
		WithToolResultBounds(ornithToolResultMaxBytes, ornithToolResultMaxBytes).
		WithDuplicateToolResultBounds(ornithDuplicateToolResultMinBytes).
		WithMutationArgumentBounds(ornithMutationArgumentMaxBytes)

	return agentcore.NewKernelRuntime(agentcore.KernelConfig{
		Client:         spec.Client,
		ModelID:        spec.ModelID,
		SystemPrompt:   prompt.ForOrnithCoding(spec.SystemPrompt),
		CWD:            spec.CWD,
		Workspace:      spec.Workspace,
		ContextManager: contextManager,
		// Experiment: one resident-model request owns both semantic selection
		// and the resulting direct response or native tool call.
		TurnRouter: ornithCodingRouter{
			artifactRoot: spec.Workspace.ArtifactModelRoot(),
			inner:        residentInlineSemanticRouter{},
		},
		TraceObserver:   spec.TraceObserver,
		AdmissionPolicy: ornithCodingAdmission,
		TaskProfile: func(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
			profile := ornithCodingTaskProfile(task, route)
			if router.IntentID(route.Intent) == router.IntentStaticWebArtifact {
				profile.RequiredArtifactRoot = spec.Workspace.ArtifactModelRoot()
				profile.RequiredArtifactPathArgs = map[string][]string{
					router.ToolWrite:       {"file_path"},
					router.ToolEdit:        {"file_path"},
					router.ToolWebBuild:    {"html_path", "source_path"},
					router.ToolVisual:      {"file_path"},
					router.ToolInteraction: {"file_path"},
				}
			}
			return profile
		},
		ModelLed:                  true,
		NativeAutoToolChoice:      true,
		FreshNarrowedToolRecovery: true,
		StreamLargeWriteDrafts:    true,
		IncludeStreamingUsage:     true,
		PromptTokenLimit:          spec.PromptTokenLimit,
		// Inline semantic routing deliberately starts without a classifier-owned
		// completion profile. A successful search or validator therefore cannot
		// prove that the model's current plan is complete. Keep the same resident
		// model in the loop for source opening, interaction checks, repairs and the
		// final user-facing synthesis instead of treating the first successful tool
		// as a terminal answer.
		VerifiedFastFinal:    false,
		DisableSelfIteration: false,
		WorkState: agentcore.WorkStatePolicy{
			Enabled: workState,
			Eligible: func(_ string, route agentcore.TurnRoute) bool {
				return route.Mode == string(router.TurnModeAgent)
			},
		},
		Research: agentcore.ResearchPolicy{Enabled: false},
		Subtasks: agentcore.SubtaskPolicy{
			Enabled: true, MaxTasks: 3, MaxConcurrent: 1, MaxTurns: 16, MaxDepth: 1,
		},
		MaxTurns:        48,
		SubtaskStateDir: spec.SubtaskStateDir,
		ParentSessionID: spec.ParentSessionID,
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
			plan := router.Classify(router.StripSystemReminder(task))
			repositoryValidation := false
			if plan != nil {
				switch plan.ID {
				case router.IntentEngineeringFix, router.IntentDelegatedLocalTask,
					router.IntentShellScript, router.IntentConfigArtifact:
					repositoryValidation = true
				}
			}
			return agentcore.ValidationRequest{
				CWD: spec.CWD, ExplicitCommand: router.NamedValidationCommand(task),
				RepositoryValidation: repositoryValidation, ShellTool: router.ToolShell,
			}
		},
		Deliverables: func(task string) []agentcore.Deliverable {
			return taskDeliverables(spec.Workspace, task)
		},
		MutationScope: router.TaskExplicitMutationScope,
	})
}

func ornithCodingAdmission(task string) error {
	if strings.TrimSpace(router.StripSystemReminder(task)) == "" {
		return errors.New("Ornith Coding requires a non-empty task")
	}
	// Semantic/tool selection belongs to the resident Ornith generation. Risky
	// mutations still pass through the registry authorizer, so admission only
	// rejects an empty turn instead of keyword-classifying the user's intent.
	return nil
}

func ornithCodingTaskProfile(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
	if profile, ok := residentInlineTaskProfile(task, route); ok {
		profile.MaxConsecutiveFailures = 3
		profile.ModelDirectedEvidenceRecovery = true
		return profile
	}
	// Follow-up validation requests often classify as engineering_fix because
	// they mention a previously repaired file.  When the requested evidence is
	// explicitly a TypeScript web build plus browser validation, retain the
	// static-web evidence contract even though no new mutation is required.
	// Otherwise a single glob/read can satisfy the generic engineering tool gate
	// and falsely complete without running the checks the user named.
	if router.IntentID(route.Intent) != router.IntentStaticWebArtifact &&
		explicitExistingArtifactValidation(task) && ornithStaticWebValidationTask(task) {
		route.Intent = string(router.IntentStaticWebArtifact)
	}
	profile := baseTaskProfile(task, route)
	profile.MaxConsecutiveFailures = 3
	switch router.IntentID(route.Intent) {
	case router.IntentEngineeringFix, router.IntentDelegatedLocalTask:
		profile.TaskStartPrompt = "CODING_LOOP_START: inspect the real repository and its local instructions, identify the smallest relevant surface, then use native tools to implement and run the repository's actual validation. Do not stop at a plan."
		profile.MutationRequiredPrompt = "CODING_MUTATION_REQUIRED: analysis alone cannot complete this engineering task. Apply the smallest evidence-grounded edit with a native file tool, then run a fresh relevant compiler, test, linter, or explicit validator."
		profile.MutationRecoveryPrompt = "CODING_TOOL_RECOVERY: the previous response described a change without persisting it. Reuse the grounded plan and issue one native edit/write call now; do not paste a replacement code block as prose."
	case router.IntentStaticWebArtifact:
		// Ornith was trained as a coding agent, so it owns the build/inspect/edit/
		// browser loop. The controller keeps the ordered evidence contract and
		// rejects false completion, but does not schedule validators or select a
		// repair tool on the model's behalf.
		profile.ForceRequiredEvidenceAfterMutations = 0
		profile.ForceRequiredEvidenceImmediately = false
		profile.ForceRequiredEvidenceAfterInspections = 0
		profile.AutoExecuteDeterministicEvidence = false
		profile.RequiredEvidenceRepairTools = nil
		profile.RepairPostActionInteractionImmediately = false
		profile.ReplayPostActionInteractionAfterRepair = false
		profile.ModelDirectedEvidenceRecovery = true
		interactionQuotes := ornithExplicitInteractionRequirementQuotes(task)
		profile.RequiredInteractionCoverageQuotes = interactionQuotes
		profile.RequireInteractionCoverageQuotes = len(interactionQuotes) > 0
		profile.ForceMutationToolAfterInspections = ""
		profile.MinInspectionsBeforeForcedMutation = 0
		if !explicitWebInteractionEvidenceTask(task) {
			removeOrnithRequiredEvidence(&profile, router.ToolInteraction)
		}
		if standaloneHTMLWithoutExplicitBuild(task) {
			removeOrnithRequiredEvidence(&profile, router.ToolWebBuild)
			removeOrnithRequiredEvidence(&profile, router.ToolRead)
			profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolVisual)
			if explicitWebInteractionEvidenceTask(task) {
				profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolInteraction)
			}
			if profile.RequiredEvidenceBindings == nil {
				profile.RequiredEvidenceBindings = make(map[string]agentcore.RequiredEvidenceBinding)
			}
			profile.RequiredEvidenceBindings[router.ToolVisual] = agentcore.RequiredEvidenceBinding{
				MutationPathArgs: []string{"file_path"},
			}
			if explicitWebInteractionEvidenceTask(task) {
				profile.RequiredEvidenceBindings[router.ToolInteraction] = agentcore.RequiredEvidenceBinding{
					MutationPathArgs: []string{"file_path"},
				}
			}
			profile.TaskStartPrompt += " WEB_ARCHITECTURE_OWNERSHIP: inspect the existing project and choose file boundaries, names, and stack from the task and repository. Keep a genuinely tiny artifact simple, but split independent concerns when that improves maintenance; do not follow a harness-selected component recipe. After the final write, run meadow_browser_visual_check for technical loading, console, resource, navigation, DOM, and runtime diagnostics. If the user explicitly requested still-image inspection or visual comparison, set include_model_image=true and reason from the attached pixels. Do not require automatic visual-motion or gameplay proof; preserve the artifact for human review unless the user supplied an explicit machine-verifiable interaction contract."
		}
		if ornithInteractiveGameTask(task) {
			if explicitWebInteractionEvidenceTask(task) {
				// Snake has a stable product contract. Let the model implement the game,
				// but do not make it invent a large validator JSON whose bookkeeping can
				// fail after the game is already playable. The harness runs this compact
				// plan once after the first persisted draft and returns only real runtime
				// failures to the model for repair.
				if args, ok := nemotron30DeterministicGameInteraction(task); ok {
					profile.AutoExecuteDeterministicEvidence = true
					profile.ForceRequiredEvidenceAfterMutations = 1
					profile.DeterministicRequiredEvidenceArgs = map[string]map[string]any{
						router.ToolInteraction: args,
					}
					profile.TaskStartPrompt += deterministicSnakeGameContract
				} else {
					profile.TaskStartPrompt += " GAME_INTERACTION_EVIDENCE: expose window.render_game_to_text() with compact machine-readable state and window.advanceTime(ms) with deterministic advancement, then author one concise interaction check covering the user's requested behaviors. Reuse that plan for repairs instead of expanding it after each failure."
				}
			} else {
				profile.TaskStartPrompt += " GAME_REVIEW_BOUNDARY: implement keyboard and visible controls, inspect source for movement logic, and use meadow_browser_visual_check only for technical faults. Do not add window.render_game_to_text() or coordinate assertions merely to satisfy the harness. Ask the human to review gameplay; if the human reports a failure, inspect source and add focused console logging only when useful."
			}
		}
		if explicitExistingArtifactValidation(task) || ornithContinuationValidationTask(task) {
			profile.RequiresMutation = false
			for tool, binding := range profile.RequiredEvidenceBindings {
				binding.AllowPreexistingPaths = true
				profile.RequiredEvidenceBindings[tool] = binding
			}
		} else if explicitExistingArtifactRepair(task) {
			for tool, binding := range profile.RequiredEvidenceBindings {
				binding.AllowPreexistingPaths = true
				if tool == router.ToolWebBuild {
					binding.RequireAnyMutationPath = true
				}
				profile.RequiredEvidenceBindings[tool] = binding
			}
		}
	}
	return profile
}

func ornithStaticWebValidationTask(task string) bool {
	text := strings.ToLower(task)
	hasWebSource := containsAnyText(text,
		"typescript", ".ts", ".html", "html", "web", "canvas", "網頁", "网页")
	hasBrowserEvidence := containsAnyText(text,
		"meadow browser", "visual", "interaction", "browser", "視覺", "视觉", "互動", "互动")
	return hasWebSource && hasBrowserEvidence
}

func ornithInteractiveGameTask(task string) bool {
	lower := strings.ToLower(task)
	for _, marker := range []string{
		"遊戲", "游戏", "game", "貪食蛇", "贪食蛇", "snake", "俄羅斯方塊", "俄罗斯方块",
		"tetris", "pac-man", "pacman", "pong", "breakout",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func ornithContinuationValidationTask(task string) bool {
	lower := strings.ToLower(strings.TrimSpace(task))
	hasContinuation := containsAnyText(lower,
		"continue", "resume", "finish the previous", "previous validation",
		"繼續", "继续", "接著", "接着", "恢復", "恢复", "剛才", "刚才", "上一輪", "上一轮")
	hasValidation := containsAnyText(lower,
		"validation", "verification", "acceptance", "test", "check", "interaction",
		"驗收", "验收", "驗證", "验证", "測試", "测试", "檢查", "检查", "互動", "互动")
	return hasContinuation && hasValidation
}

// ornithExplicitInteractionRequirementQuotes returns only literal acceptance
// items from a request that explicitly asks for per-item/coverage validation.
// It does not infer behavior, rewrite wording, or decide how to test it. This
// lets the model author a general interaction plan while the Harness can prove
// that no user-listed item silently disappeared.
func ornithExplicitInteractionRequirementQuotes(task string) []string {
	lower := strings.ToLower(task)
	if !containsAnyText(lower,
		"coverage", "each requirement", "every requirement", "each item",
		"逐項", "逐项", "每一項", "每一项", "每項", "每项", "下列") {
		return nil
	}
	pairs := [][2]string{{"「", "」"}, {"“", "”"}}
	var result []string
	seen := make(map[string]bool)
	for _, pair := range pairs {
		rest := task
		for {
			start := strings.Index(rest, pair[0])
			if start < 0 {
				break
			}
			rest = rest[start+len(pair[0]):]
			end := strings.Index(rest, pair[1])
			if end < 0 {
				break
			}
			quote := strings.TrimSpace(rest[:end])
			rest = rest[end+len(pair[1]):]
			if quote == "" || len([]rune(quote)) > 200 {
				continue
			}
			key := strings.ToLower(strings.Join(strings.Fields(quote), " "))
			if !seen[key] {
				seen[key] = true
				result = append(result, quote)
			}
		}
	}
	return result
}

type ornithCodingRouter struct {
	artifactRoot string
	inner        agentcore.TurnRouter
}

func (r ornithCodingRouter) Route(ctx context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	routeRequest := request
	continuedTask := ""
	explicitWebValidation := explicitExistingArtifactValidation(request.Task) &&
		ornithStaticWebValidationTask(request.Task)
	if ornithContinuationTask(request.Task) {
		continuedTask = previousOrnithCodingTask(request.Messages, request.Task)
		if continuedTask != "" {
			routeRequest.Task = continuedTask + "\n\nCurrent continuation request: " + request.Task
		}
	}
	intentRouter := r.inner
	if intentRouter == nil {
		intentRouter = advisoryRouter{artifactRoot: r.artifactRoot}
	}
	route, err := intentRouter.Route(ctx, routeRequest)
	if err != nil {
		return route, err
	}
	// Resident-inline routing deliberately avoids a separate classifier model,
	// but an imperative request to create an interactive game already carries a
	// deterministic artifact contract. Preserve that contract here so the
	// static-web mutation and browser-evidence gates are not replaced by the
	// generic inline rule of "one HTML visual check is enough". The model still
	// owns the implementation, tool arguments and any repair strategy.
	if router.IntentID(route.Intent) != router.IntentStaticWebArtifact &&
		ornithStaticWebCreationTask(routeRequest.Task) {
		route.Intent = string(router.IntentStaticWebArtifact)
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
	}
	if isConversationOnlyRoute(route) {
		return route, nil
	}
	if router.IntentID(route.Intent) == router.IntentStaticWebArtifact {
		allowed := map[string]bool{
			router.ToolWrite: true, router.ToolEdit: true, router.ToolRead: true,
			router.ToolGlob: true, router.ToolGrep: true, router.ToolList: true,
			router.ToolWebBuild: true, router.ToolVisual: true,
			router.ToolInteraction: true,
		}
		if router.NamedValidationCommand(request.Task) != "" {
			allowed[router.ToolShell] = true
		}
		route.Tools = selectTools(request.AvailableTools, allowed)
	}
	if router.IntentID(route.Intent) == router.IntentStaticWebArtifact &&
		standaloneHTMLWithoutExplicitBuild(request.Task) {
		guidance := "Choose the web artifact's architecture from the user's goal and the existing repository; the harness does not prescribe one file, multiple files, component names, a framework, or a build step. Run one Meadow Browser visual/runtime check after the final mutation."
		if explicitWebInteractionEvidenceTask(request.Task) {
			guidance += " Then run the required interaction check and finish immediately when both checks pass; do not invent or repeat another acceptance plan."
		} else {
			guidance += " Finish immediately when it passes."
		}
		route.Guidance = append(route.Guidance, guidance)
	}
	// Desktop may resume a cancelled task in a fresh chat that has the same
	// project workspace but no provider-message history.  A request such as
	// "continue fixing the current TypeScript game and rerun Meadow Browser"
	// still carries enough task-local evidence to retain the static-web tool and
	// validation contract.  Without this recovery, the generic engineering
	// profile can fast-final immediately after one edit and leave build/browser
	// evidence stale.  The synthetic prefix is classification-only; the model
	// continues to receive the user's original request unchanged.
	if router.IntentID(route.Intent) != router.IntentStaticWebArtifact &&
		(explicitWebValidation ||
			(ornithContinuationValidationTask(request.Task) && ornithStaticWebValidationTask(request.Task))) {
		// This is capability-family selection, not a repair oracle. The user has
		// explicitly named an existing TypeScript/browser validation surface, so
		// remove unrelated planning, delegation and free-form shell capabilities.
		// Ornith still decides whether the useful next action is read, edit, build,
		// visual or interaction validation, and authors every argument itself.
		route.Intent = string(router.IntentStaticWebArtifact)
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
		allowed := map[string]bool{
			router.ToolWrite: true, router.ToolEdit: true, router.ToolRead: true,
			router.ToolGlob: true, router.ToolGrep: true, router.ToolList: true,
			router.ToolWebBuild: true, router.ToolVisual: true,
			router.ToolInteraction: true,
		}
		if router.NamedValidationCommand(request.Task) != "" {
			allowed[router.ToolShell] = true
		}
		route.Tools = selectTools(request.AvailableTools, allowed)
		route.Guidance = append(route.Guidance,
			"This is validation-first work on an existing TypeScript web artifact. Choose the next capability from the current evidence; do not rewrite correct source merely to satisfy a stage. Build, visual, and interaction evidence must all be fresh after the final edit.",
		)
	}
	if continuedTask != "" {
		route.Guidance = append(route.Guidance,
			"This is a continuation of the prior coding task in the same conversation. Preserve its artifact and validation context; the newest user message changes the next objective but does not create an unrelated file task.",
		)
	}
	if route.Mode == string(router.TurnModeAgent) {
		route.Guidance = append(route.Guidance,
			"Own the engineering loop: inspect the repository, persist the smallest relevant change, run its real checks, and iterate from exact diagnostics until fresh validation passes or a concrete blocker remains.",
			"Do not weaken tests, invent command output, or report completion from a code block. Preserve unrelated user changes.",
		)
	}
	return route, nil
}

func standaloneHTMLWithoutExplicitBuild(task string) bool {
	text := strings.ToLower(strings.TrimSpace(task))
	// The shared HTML-only classifier already separates creation from
	// hypothetical discussion and recognizes ordinary mini-game wording. Let an
	// explicit single-file game stay build-free even when its acceptance text
	// also says "run browser validation", which NamedValidationCommand may
	// otherwise mistake for a repository build command.
	if htmlOnlyStaticWebTask(text) {
		return true
	}
	if explicitExistingArtifactValidation(task) || explicitExistingArtifactRepair(task) ||
		ornithContinuationValidationTask(task) || router.NamedValidationCommand(task) != "" {
		return false
	}
	return !containsAnyText(text,
		"typescript", "typescript_web_build", ".ts", "package.json",
		"npm run", "pnpm ", "yarn ", "bun ", "vite", "react", "vue", "svelte", "next.js",
		"compile", "compiler", "build command", "建置", "編譯", "编译")
}

func explicitWebInteractionEvidenceTask(task string) bool {
	text := strings.ToLower(strings.TrimSpace(task))
	return len(ornithExplicitInteractionRequirementQuotes(task)) > 0 || containsAnyText(text,
		"meadow_browser_interaction_check", "interaction check", "interaction test",
		"互動驗收", "互动验收", "互動測試", "互动测试", "互動檢查", "互动检查",
		"行為驗收", "行为验收")
}

func removeOrnithRequiredEvidence(profile *agentcore.TaskProfile, tool string) {
	if profile == nil || strings.TrimSpace(tool) == "" {
		return
	}
	kept := profile.RequiredEvidenceTools[:0]
	for _, name := range profile.RequiredEvidenceTools {
		if name != tool {
			kept = append(kept, name)
		}
	}
	profile.RequiredEvidenceTools = kept
	delete(profile.RequiredEvidenceFailureLimits, tool)
	delete(profile.RequiredEvidenceRepairTools, tool)
	delete(profile.RequiredEvidenceBindings, tool)
}

func previousOrnithCodingTask(messages []provider.Message, current string) string {
	current = strings.TrimSpace(current)
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Role != "user" {
			continue
		}
		candidate := strings.TrimSpace(provider.MessageText(message))
		if candidate == "" || candidate == current || strings.HasPrefix(candidate, "(") {
			continue
		}
		if ornithStaticWebCreationTask(candidate) {
			return candidate
		}
		plan := router.Classify(router.StripSystemReminder(candidate))
		if plan == nil {
			continue
		}
		switch plan.ID {
		case router.IntentStaticWebArtifact, router.IntentEngineeringFix,
			router.IntentDelegatedLocalTask, router.IntentShellScript,
			router.IntentConfigArtifact:
			return candidate
		}
	}
	return ""
}

func ornithContinuationTask(task string) bool {
	lower := strings.ToLower(strings.TrimSpace(task))
	return containsAnyText(lower,
		"continue", "resume", "keep going", "finish it", "finish the previous",
		"繼續", "继续", "接著", "接着", "繼續做", "继续做", "完成它", "做完",
		"恢復", "恢复", "剛才", "刚才", "上一輪", "上一轮")
}

func ornithStaticWebCreationTask(task string) bool {
	if !ornithInteractiveGameTask(task) {
		return false
	}
	// Questions about game rules remain chat. Long acceptance prompts may also
	// contain quoted examples or meta words that make the broad turn-mode
	// classifier conservative, even though they begin with an unambiguous local
	// creation request. Use the narrower effect predicate here so explicit game
	// builds keep their static-web evidence contract.
	stripped := router.StripSystemReminder(task)
	return router.ShouldRequireTool(stripped) && !router.IsArtifactDiscussionQuestion(stripped)
}

func isOrnithCodingModel(modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	return strings.Contains(modelID, "ornith") || strings.Contains(modelID, "meadow coder")
}
