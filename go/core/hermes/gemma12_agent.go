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
	Gemma12AgentProfileID = "gemma12-general-agent-v0"

	gemma12HistoryMaxBytes             = 96 * 1024
	gemma12ToolResultMaxBytes          = 16 * 1024
	gemma12DuplicateToolResultMinBytes = 512
	gemma12MutationArgumentMaxBytes    = 768
)

// Gemma12AgentSpec is independent from every Qwen and Muse profile. The
// shared Kernel supplies safety, durable diagnostics and completion truth;
// Gemma owns task interpretation, actions and repair strategy.
type Gemma12AgentSpec struct {
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

func NewGemma12AgentRuntime(spec Gemma12AgentSpec, hooks Hooks) (agentcore.Runtime, error) {
	if spec.Client == nil {
		return nil, errors.New("Gemma 12B Agent requires a model client")
	}
	if spec.Registry == nil || spec.Workspace == nil {
		return nil, errors.New("Gemma 12B Agent requires a workspace tool registry")
	}
	if !provider.IsGemma12Model(spec.ModelID) {
		return nil, errors.New("Gemma 12B Agent profile cannot be applied to a different model family")
	}
	if !spec.UseRouter {
		return nil, errors.New("Gemma 12B Agent requires its capability router")
	}
	if spec.ContextManager == nil {
		spec.ContextManager = agentcore.NewContextManager(agentcore.ContextPolicy{})
	}
	selfIteration, err := featureSwitch(spec.SelfIteration, true, "self iteration")
	if err != nil {
		return nil, err
	}
	if !selfIteration {
		return nil, errors.New("Gemma 12B Agent requires self iteration and fresh validation")
	}
	workState, err := featureSwitch(spec.WorkState, true, "agent work state")
	if err != nil {
		return nil, err
	}

	registry := tools.Gemma12AgentRegistry(spec.Registry, spec.Workspace)
	if registry == nil {
		return nil, errors.New("Gemma 12B Agent could not build its registry")
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
		WithHistoryBounds(gemma12HistoryMaxBytes, 10).
		WithToolResultBounds(gemma12ToolResultMaxBytes, gemma12ToolResultMaxBytes).
		WithDuplicateToolResultBounds(gemma12DuplicateToolResultMinBytes).
		WithMutationArgumentBounds(gemma12MutationArgumentMaxBytes)
	var retriever agentcore.ContextRetriever
	if spec.RAG != nil {
		retriever = kernelRetriever{
			store: spec.RAG, topK: spec.RAGTopK, profile: Gemma12AgentProfileID,
			includeSourceIDs: append([]string(nil), spec.RAGSourceIDs...),
		}
	}
	retriever = withCartridgeRetriever(retriever, spec.Cartridges, Gemma12AgentProfileID, spec.CartridgeScope)

	turnRouter := agentcore.TurnRouter(gemma12AgentRouter{artifactRoot: spec.Workspace.ArtifactModelRoot()})
	if spec.ModelIntent {
		turnRouter = newModelIntentRouter(spec.Client, spec.ModelID, Gemma12AgentProfileID)
	}
	return agentcore.NewKernelRuntime(agentcore.KernelConfig{
		Client:         spec.Client,
		ModelID:        spec.ModelID,
		SystemPrompt:   prompt.ForGemma12Agent(spec.SystemPrompt),
		CWD:            spec.CWD,
		Workspace:      spec.Workspace,
		ContextManager: contextManager,
		// The resident model, not a keyword table, owns user-intent selection.
		TurnRouter:       turnRouter,
		ContextRetriever: retriever,
		DocumentIngestor: documentIngestor{store: spec.DocumentStore},
		TraceObserver:    spec.TraceObserver,
		AdmissionPolicy:  productAdmission,
		TaskProfile: func(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
			profile := gemma12AgentTaskProfile(task, route)
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
		ModelLed:                   true,
		NativeAutoToolChoice:       true,
		NativeLockedTaskToolChoice: true,
		FreshNarrowedToolRecovery:  true,
		IncludeStreamingUsage:      true,
		VerifiedFastFinal:          true,
		WorkState: agentcore.WorkStatePolicy{
			Enabled: workState,
			Eligible: func(_ string, route agentcore.TurnRoute) bool {
				return route.Mode == string(router.TurnModeAgent)
			},
		},
		Research:        productResearchPolicy(spec.ModelIntent, 1, 6, true),
		Subtasks:        agentcore.SubtaskPolicy{Enabled: false},
		MaxTurns:        56,
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
			return agentcore.ValidationRequest{
				CWD: spec.CWD, ExplicitCommand: router.NamedValidationCommand(task),
				RepositoryValidation: backendApplicationTask(task), ShellTool: router.ToolShell,
			}
		},
		Deliverables: func(task string) []agentcore.Deliverable {
			return taskDeliverables(spec.Workspace, task)
		},
		MutationScope: router.TaskExplicitMutationScope,
	})
}

func gemma12AgentTaskProfile(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
	profile := baseTaskProfile(task, route)
	profile.MaxConsecutiveFailures = 5
	profile.ModelDirectedEvidenceRecovery = true
	profile.AutoExecuteDeterministicEvidence = false
	if router.IntentID(route.Intent) == router.IntentStaticWebArtifact &&
		htmlOnlyStaticWebRoute(task, route) {
		// Gemma's one-file path keeps the compact, recoverable long write, but a
		// read-back alone cannot prove that the saved page loads. Bind one real
		// browser runtime check to the mutated HTML; behavioral assertions remain
		// opt-in because ordinary gameplay still needs human review.
		removeOrnithRequiredEvidence(&profile, router.ToolRead)
		profile.InitialTool = router.ToolWrite
		profile.InitialToolUntilMutations = 1
		profile.ForceRequiredEvidenceAfterMutations = 1
		profile.AutoExecuteDeterministicEvidence = true
		profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, router.ToolVisual)
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolVisual)
		if profile.RequiredEvidenceBindings == nil {
			profile.RequiredEvidenceBindings = make(map[string]agentcore.RequiredEvidenceBinding)
		}
		profile.RequiredEvidenceBindings[router.ToolVisual] = agentcore.RequiredEvidenceBinding{
			MutationPathArgs: []string{"file_path"},
		}
		if explicitWebInteractionEvidenceTask(task) {
			profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, router.ToolInteraction)
			profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolInteraction)
			profile.RequiredEvidenceBindings[router.ToolInteraction] = agentcore.RequiredEvidenceBinding{
				MutationPathArgs: []string{"file_path"},
			}
			quotes := ornithExplicitInteractionRequirementQuotes(task)
			profile.RequiredInteractionCoverageQuotes = quotes
			profile.RequireInteractionCoverageQuotes = len(quotes) > 0
		}
		profile.TaskStartPrompt += " After the final saved write, run meadow_browser_visual_check on that HTML. Add meadow_browser_interaction_check only when the user explicitly requested machine-verifiable interaction acceptance."
	}
	if localPDFHTMLDocumentTask(task) {
		profile.RequiresInspection = true
		profile.RequiresMutation = true
		profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, "pdf_extract")
		profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, router.ToolVisual)
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, "pdf_extract")
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolVisual)
	}
	return profile
}

type gemma12AgentRouter struct {
	artifactRoot string
}

func (r gemma12AgentRouter) Route(ctx context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	route, err := (advisoryRouter{artifactRoot: r.artifactRoot}).Route(ctx, request)
	if err != nil {
		return route, err
	}
	if isConversationOnlyRoute(route) {
		return route, nil
	}
	if router.IntentID(route.Intent) == router.IntentStaticWebArtifact &&
		htmlOnlyStaticWebRoute(request.Task, route) {
		allowed := map[string]bool{
			router.ToolWrite: true, router.ToolEdit: true, router.ToolRead: true,
			router.ToolGlob: true, router.ToolGrep: true, router.ToolList: true,
			tools.TaskPlanToolName: true, router.ToolVisual: true,
		}
		if explicitWebInteractionEvidenceTask(request.Task) {
			allowed[router.ToolInteraction] = true
		}
		route.Tools = selectTools(request.AvailableTools, allowed)
		route.Guidance = append(route.Guidance,
			"Create one self-contained browser-ready HTML file with inline CSS and JavaScript. After the final write, run meadow_browser_visual_check. Add interaction assertions only when the user explicitly requested them.",
		)
	}
	if localPDFHTMLDocumentTask(request.Task) {
		route.Intent = string(router.IntentOfficeArtifact)
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
		route.Tools = selectTools(request.AvailableTools, map[string]bool{
			router.ToolRead: true, router.ToolWrite: true, router.ToolEdit: true,
			"pdf_extract": true, rag.DocumentSearchToolName: true, rag.DocumentReadToolName: true,
			router.ToolVisual: true, tools.TaskPlanToolName: true,
		})
		route.Guidance = append(route.Guidance,
			"Read the named local PDF with page coordinates, write the requested HTML notes, and visually validate the saved HTML. Preserve every sourced number and qualifier; mark anything unavailable as unconfirmed.",
		)
	}
	if backendApplicationTask(request.Task) {
		route.Intent = string(router.IntentEngineeringFix)
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
		route.Tools = selectTools(request.AvailableTools, map[string]bool{
			router.ToolRead: true, router.ToolWrite: true, router.ToolEdit: true,
			router.ToolGlob: true, router.ToolGrep: true, router.ToolList: true,
			router.ToolShell: true, "process": true, tools.TaskPlanToolName: true,
		})
	}
	if productResearchEligible(request.Task) {
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
			router.ToolWrite: true, router.ToolEdit: true, router.ToolVisual: true,
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
	}
	return route, nil
}

func localPDFHTMLDocumentTask(task string) bool {
	lower := strings.ToLower(task)
	return strings.Contains(lower, ".pdf") && strings.Contains(lower, "html") &&
		!containsAnyText(lower, "slider", "slide", "簡報", "简报", "上一頁", "下一頁")
}

func backendApplicationTask(task string) bool {
	lower := strings.ToLower(task)
	return containsAnyText(lower, "node.js", "nodejs", "sqlite", "mongodb", "mongo db") &&
		containsAnyText(lower, "網站", "网站", "web site", "website", "server", "服務", "服务")
}
