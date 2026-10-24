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
	Qwen38CodingProfileID = "qwen38-coding-agent-v0"

	qwen38HistoryMaxBytes             = 384 * 1024
	qwen38ToolResultMaxBytes          = 32 * 1024
	qwen38DuplicateToolResultMinBytes = 512
	qwen38MutationArgumentMaxBytes    = 24 * 1024
)

// Qwen38CodingSpec is independent from Office35, Ornith and Muse. The Kernel
// supplies safety and evidence accounting; Qwen3.8 chooses the files, tools,
// implementation and repair strategy.
type Qwen38CodingSpec struct {
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
	IntentPredictor    SemanticIntentPredictor
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

// NewQwen38CodingRuntime builds the shared CLI/Desktop Qwen3.8 coding runtime.
// It deliberately avoids Office35's deterministic tool scheduler. The profile
// narrows capabilities and rejects unsupported completion, but does not choose
// the model's next edit or validator.
func NewQwen38CodingRuntime(spec Qwen38CodingSpec, hooks Hooks) (agentcore.Runtime, error) {
	if spec.Client == nil {
		return nil, errors.New("Qwen3.8 Coding requires a model client")
	}
	if spec.Registry == nil || spec.Workspace == nil {
		return nil, errors.New("Qwen3.8 Coding requires a workspace tool registry")
	}
	if !provider.IsQwen38Model(spec.ModelID) {
		return nil, errors.New("Qwen3.8 Coding profile cannot be applied to a different model family")
	}
	if !spec.UseRouter {
		return nil, errors.New("Qwen3.8 Coding requires its coding router")
	}
	if !spec.ModelIntent {
		return nil, errors.New("Qwen3.8 Coding requires its semantic intent router")
	}
	if spec.ContextManager == nil {
		spec.ContextManager = agentcore.NewContextManager(agentcore.ContextPolicy{})
	}
	selfIteration, err := featureSwitch(spec.SelfIteration, true, "self iteration")
	if err != nil {
		return nil, err
	}
	if !selfIteration {
		return nil, errors.New("Qwen3.8 Coding requires self iteration and fresh validation")
	}
	workState, err := featureSwitch(spec.WorkState, true, "agent work state")
	if err != nil {
		return nil, err
	}

	registry := tools.Qwen38CodingRegistry(spec.Registry, spec.Workspace)
	if registry == nil {
		return nil, errors.New("Qwen3.8 Coding could not build its coding registry")
	}
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
	contextManager := spec.ContextManager.
		WithHistoryBounds(qwen38HistoryMaxBytes, 12).
		WithToolResultBounds(qwen38ToolResultMaxBytes, qwen38ToolResultMaxBytes).
		WithDuplicateToolResultBounds(qwen38DuplicateToolResultMinBytes).
		WithMutationArgumentBounds(qwen38MutationArgumentMaxBytes)
	var retriever agentcore.ContextRetriever
	if spec.RAG != nil {
		retriever = kernelRetriever{
			store: spec.RAG, topK: spec.RAGTopK, profile: Qwen38CodingProfileID,
			includeSourceIDs: append([]string(nil), spec.RAGSourceIDs...),
		}
	}
	retriever = withCartridgeRetriever(retriever, spec.Cartridges, Qwen38CodingProfileID, spec.CartridgeScope)

	// Experiment: the resident model chooses direct response vs native tool in
	// its main generation. Do not spend a separate model request on intent.
	turnRouter := agentcore.TurnRouter(residentInlineSemanticRouter{})
	return agentcore.NewKernelRuntime(agentcore.KernelConfig{
		Client:         spec.Client,
		ModelID:        spec.ModelID,
		SystemPrompt:   prompt.ForQwen38Coding(spec.SystemPrompt),
		CWD:            spec.CWD,
		Workspace:      spec.Workspace,
		ContextManager: contextManager,
		// The resident model, not a keyword table, owns user-intent selection.
		TurnRouter:       turnRouter,
		ContextRetriever: retriever,
		DocumentIngestor: documentIngestor{store: spec.DocumentStore},
		TraceObserver:    spec.TraceObserver,
		AdmissionPolicy:  qwen38SemanticAdmission,
		TaskProfile: func(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
			profile := qwen38CodingTaskProfile(task, route)
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
		VerifiedFastFinal:         true,
		WorkState: agentcore.WorkStatePolicy{
			Enabled: workState,
			Eligible: func(_ string, route agentcore.TurnRoute) bool {
				return route.Mode == string(router.TurnModeAgent)
			},
		},
		Research: productResearchPolicy(spec.ModelIntent, 2, 8, false),
		Subtasks: agentcore.SubtaskPolicy{
			Enabled: true, MaxTasks: 3, MaxConcurrent: 1, MaxTurns: 12, MaxDepth: 1,
		},
		MaxTurns:        40,
		SubtaskStateDir: spec.SubtaskStateDir,
		ParentSessionID: spec.ParentSessionID,
		Executor: &agentcore.ToolExecutor{
			RequireAccount: spec.RequireToolAccount,
			Registry:       registry, Authorize: authorize,
			Parallel: provider.QwenParallelToolCallsEnabled(spec.ModelID), MaxParallel: 2,
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
				ShellTool: router.ToolShell,
			}
		},
		RouteValidationPolicy: func(task string, route agentcore.TurnRoute) agentcore.ValidationRequest {
			intent := router.IntentID(route.Intent)
			return agentcore.ValidationRequest{
				CWD: spec.CWD, ExplicitCommand: router.NamedValidationCommand(task),
				RepositoryValidation: intent == router.IntentEngineeringFix ||
					intent == router.IntentDelegatedLocalTask,
				ShellTool: router.ToolShell,
			}
		},
		Deliverables: func(task string) []agentcore.Deliverable {
			return taskDeliverables(spec.Workspace, task)
		},
		MutationScope: router.TaskExplicitMutationScope,
	})
}

func qwen38SemanticAdmission(task string) error {
	if strings.TrimSpace(router.StripSystemReminder(task)) == "" {
		return errors.New("Qwen3.8 Coding requires a non-empty task")
	}
	return nil
}

func qwen38CodingTaskProfile(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
	if profile, ok := residentInlineTaskProfile(task, route); ok {
		profile.MaxConsecutiveFailures = 5
		profile.ModelDirectedEvidenceRecovery = true
		return profile
	}
	profile := baseTaskProfile(task, route)
	profile.MaxConsecutiveFailures = 5
	profile.ModelDirectedEvidenceRecovery = true
	if localPDFHTMLDocumentTask(task) {
		profile.RequiresInspection = true
		profile.RequiresMutation = true
		profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, "pdf_extract")
		profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, router.ToolVisual)
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, "pdf_extract")
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolVisual)
	}
	if router.IntentID(route.Intent) != router.IntentStaticWebArtifact {
		return profile
	}
	// The model chooses HTML-only vs TypeScript. Do not inherit Office35's
	// mandatory build/browser evidence or TS-split prompts from keywords.
	profile.RequiredEvidenceRepairTools = nil
	profile.ForceRequiredEvidenceAfterMutations = 0
	profile.ForceRequiredEvidenceImmediately = false
	profile.ForceRequiredEvidenceAfterInspections = 0
	profile.AutoExecuteDeterministicEvidence = false
	profile.RepairPostActionInteractionImmediately = false
	profile.ReplayPostActionInteractionAfterRepair = false
	profile.ModelDirectedEvidenceRecovery = true
	profile.ForceMutationToolAfterInspections = ""
	profile.MinInspectionsBeforeForcedMutation = 0
	// A fresh static-web artifact has one unambiguous first capability: save
	// source bytes. Keep the implementation, filenames, stack, and content
	// model-authored, but do not ask a dense local model to choose that first
	// action from the complete coding/research registry. The controller releases
	// this narrow surface immediately after the first successful mutation.
	profile.InitialTool = router.ToolWrite
	profile.InitialToolUntilMutations = 1
	if localPDFHTMLDocumentTask(task) {
		kept := make([]string, 0, 2)
		for _, tool := range profile.RequiredEvidenceTools {
			if tool == "pdf_extract" || tool == router.ToolVisual {
				kept = appendUniqueHermesTool(kept, tool)
			}
		}
		profile.RequiredEvidenceTools = kept
	} else {
		profile.RequiredEvidenceTools = nil
	}
	profile.TaskStartPrompt = qwen38WebStartPrompt
	profile.MutationRequiredPrompt = qwen38WebMutationPrompt
	profile.MutationRecoveryPrompt = qwen38WebRecoveryPrompt
	return profile
}

func researchHTMLFollowupTask(task string) bool {
	lower := strings.ToLower(task)
	return strings.Contains(lower, "html") && containsAnyText(lower,
		"剛才查", "刚才查", "剛剛查", "刚刚查", "prior research", "previous research", "already researched")
}

const (
	qwen38WebStartPrompt      = "WEB_SAVE_REQUIRED: persist a working page or game with write_file now. Choose the simplest structure that satisfies the user. A fenced code block is not a saved file. /workspace is the project root."
	qwen38WebMutationPrompt   = "WEB_MUTATION_REQUIRED: write the first missing file now. Choose the stack yourself; a chat code block is not a saved file."
	qwen38WebRecoveryPrompt   = "WEB_RECOVERY: a draft exists in chat but is not saved. Reuse it and send one write_file now."
	modelLedStaticWebGuidance = "Decide the implementation from the request. A working file on disk completes a small page or game. /workspace is already the project folder; write /workspace/index.html or another name directly under /workspace and do not nest another game directory. TypeScript build and Meadow Browser stay available if you choose them."
)

// htmlPreferenceRouter keeps the inner router in charge of intent and tools.
// On a static-web intent it only adds a decompose-and-write hint; it does not
// inspect game names or force HTML vs TypeScript.
type htmlPreferenceRouter struct {
	inner agentcore.TurnRouter
}

func (r htmlPreferenceRouter) Route(ctx context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	if r.inner == nil {
		return agentcore.TurnRoute{}, errors.New("html preference router requires an inner router")
	}
	route, err := r.inner.Route(ctx, request)
	if err != nil {
		return route, err
	}
	applyQwen38ModelLedStaticWeb(&route, request.AvailableTools, "", request.Task)
	return route, nil
}

func applyQwen38ModelLedStaticWeb(route *agentcore.TurnRoute, available []string, artifactRoot, task string) {
	if route == nil || router.IntentID(route.Intent) != router.IntentStaticWebArtifact {
		return
	}
	kept := route.Guidance[:0]
	for _, line := range route.Guidance {
		if strings.Contains(line, "TypeScript is mandatory") ||
			strings.Contains(line, "typescript_web_build") ||
			strings.Contains(line, "sibling .ts") ||
			strings.Contains(line, "sibling TypeScript") {
			continue
		}
		kept = append(kept, line)
	}
	route.Guidance = kept
	if !guidanceContains(route.Guidance, "Decide the implementation from the request") {
		route.Guidance = append(route.Guidance, modelLedStaticWebGuidance)
	}
	if len(available) == 0 {
		return
	}
	allowed := map[string]bool{}
	for _, name := range []string{
		router.ToolWrite, router.ToolEdit, router.ToolRead,
		router.ToolGlob, router.ToolGrep, router.ToolList,
		tools.TaskPlanToolName, router.ToolVisual,
	} {
		allowed[name] = true
	}
	if !standaloneHTMLWithoutExplicitBuild(task) {
		allowed[router.ToolWebBuild] = true
	}
	if explicitWebInteractionEvidenceTask(task) {
		allowed[router.ToolInteraction] = true
	}
	if selected := selectTools(available, allowed); len(selected) > 0 {
		route.Tools = selected
	}
	_ = artifactRoot
}

func guidanceContains(lines []string, needle string) bool {
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
