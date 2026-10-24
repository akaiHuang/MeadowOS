// Package hermes owns product runtime profiles shared by the CLI and Desktop.
// It deliberately excludes UI/session transport and Meadow Core lifecycle.
package hermes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"github.com/hey-meadow/meadow-code/go/core/prompt"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/rag"
	"github.com/hey-meadow/meadow-code/go/core/router"
	"github.com/hey-meadow/meadow-code/go/core/safety"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	Office35ProfileID = "qwen35-office-v1"

	office35HistoryMaxBytes             = 24 * 1024
	office35ToolResultMaxBytes          = 8 * 1024
	office35DuplicateToolResultMinBytes = 512
	office35MutationArgumentMaxBytes    = 512
	documentIngestionTokens             = 32_000
)

// Office35Spec is the model-profile composition shared by the CLI and Desktop.
// Product surfaces own approval, user interaction, persistence, and event
// rendering; this package owns the model-facing runtime behavior only.
type Office35Spec struct {
	Client             agentcore.KernelModel
	ModelID            string
	SelfIteration      string
	SemanticPreflight  string
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

// Hooks are observable runtime facts. They never select model actions.
type Hooks struct {
	OnMessageBoundary   func(string) string
	OnContent           func(string)
	OnThought           func(string)
	OnProgress          func(provider.InferenceProgress)
	OnToolDraft         func(string, int)
	OnToolDraftProgress func(provider.ToolDraftProgress)
	OnToolCall          func(string, map[string]any)
	OnToolResult        func(string, tools.Result)
	OnToolStart         func(agentcore.ToolStart)
	OnToolComplete      func(agentcore.ToolExecution)
	OnTurn              func(int, *provider.Usage)
	OnGuidance          func(agentcore.GuidanceEvent)
	OnCheckpoint        func([]provider.Message)
}

// NewOffice35Runtime builds the single Qwen 35B product profile used by every
// Meadow surface. Keeping this constructor outside cmd/meadow prevents the
// Desktop from silently drifting back to the legacy Engine.
func NewOffice35Runtime(spec Office35Spec, hooks Hooks) (agentcore.Runtime, error) {
	if spec.Client == nil {
		return nil, errors.New("Hermes Office35 requires a model client")
	}
	if spec.Registry == nil || spec.Workspace == nil {
		return nil, errors.New("Hermes Office35 requires a workspace tool registry")
	}
	if !spec.UseRouter {
		return nil, errors.New("Hermes Office35 requires its shared router; disabling it would bypass product task profiles")
	}
	if spec.ContextManager == nil {
		spec.ContextManager = agentcore.NewContextManager(agentcore.ContextPolicy{})
	}
	selfIteration, err := featureSwitch(spec.SelfIteration, true, "self iteration")
	if err != nil {
		return nil, err
	}
	if !selfIteration {
		return nil, errors.New("Hermes Office35 requires self iteration; disabling it would bypass evidence and completion guards")
	}
	semanticPreflight, err := featureSwitch(spec.SemanticPreflight, true, "semantic preflight")
	if err != nil {
		return nil, err
	}
	workState, err := featureSwitch(spec.WorkState, false, "agent work state")
	if err != nil {
		return nil, err
	}

	registry := tools.Qwen35OfficeRegistry(spec.Registry, spec.Workspace)
	if spec.DocumentStore != nil {
		registerEvidenceIngestTools(registry, spec.DocumentStore, spec.Cartridges, spec.CartridgeScope,
			"web_fetch", "meadow_browser_dom", "pdf_extract")
		registry.Register(&rag.DocumentSearchTool{Store: spec.DocumentStore})
		registry.Register(&rag.DocumentReadTool{Store: spec.DocumentStore})
	}
	registry.Register(&tools.AskUser{Ask: spec.AskUser})
	registerCartridgeMemoryTools(registry, spec.Cartridges, spec.CartridgeScope, spec.AskUser)

	var turnRouter agentcore.TurnRouter
	if spec.UseRouter {
		turnRouter = office35AdvisoryRouter{artifactRoot: spec.Workspace.ArtifactModelRoot()}
		if spec.ModelIntent {
			turnRouter = newModelIntentRouter(spec.Client, spec.ModelID, Office35ProfileID)
		}
	}
	var retriever agentcore.ContextRetriever
	if spec.RAG != nil {
		retriever = kernelRetriever{
			store: spec.RAG, topK: spec.RAGTopK, profile: Office35ProfileID,
			includeSourceIDs: append([]string(nil), spec.RAGSourceIDs...),
		}
	}
	retriever = withCartridgeRetriever(retriever, spec.Cartridges, Office35ProfileID, spec.CartridgeScope)
	authorize := spec.Authorize
	if authorize == nil {
		authorize = safeProductAuthorizer
	}
	contextManager := spec.ContextManager.
		WithHistoryBounds(office35HistoryMaxBytes, 6).
		WithToolResultBounds(office35ToolResultMaxBytes, office35ToolResultMaxBytes).
		WithDuplicateToolResultBounds(office35DuplicateToolResultMinBytes).
		WithMutationArgumentBounds(office35MutationArgumentMaxBytes)

	return agentcore.NewKernelRuntime(agentcore.KernelConfig{
		Client:            spec.Client,
		ModelID:           spec.ModelID,
		SystemPrompt:      prompt.ForQwen35Office(spec.SystemPrompt),
		RouteSystemPrompt: office35RouteSystemPrompt,
		CWD:               spec.CWD,
		Workspace:         spec.Workspace,
		ContextManager:    contextManager,
		TurnRouter:        turnRouter,
		ContextRetriever:  retriever,
		DocumentIngestor:  documentIngestor{store: spec.DocumentStore},
		TraceObserver:     spec.TraceObserver,
		AdmissionPolicy:   productAdmission,
		TaskProfile: func(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
			profile := office35TaskProfile(task, route)
			if router.IntentID(route.Intent) == router.IntentStaticWebArtifact {
				profile.RequiredArtifactRoot = spec.Workspace.ArtifactModelRoot()
				profile.RequiredArtifactPathArgs = map[string][]string{
					router.ToolWrite:    {"file_path"},
					router.ToolEdit:     {"file_path"},
					router.ToolWebBuild: {"html_path", "source_path"},
					router.ToolVisual:   {"file_path"},
				}
			}
			return profile
		},
		ModelLed:                        true,
		DisableSelfIteration:            !selfIteration,
		PreserveRequirementAuditHistory: false,
		SemanticContract: agentcore.SemanticContractPolicy{
			Enabled:  semanticPreflight,
			Eligible: office35SemanticPreflightEligible,
		},
		WorkState: agentcore.WorkStatePolicy{
			Enabled: workState,
			Eligible: func(_ string, route agentcore.TurnRoute) bool {
				return route.Mode == string(router.TurnModeAgent)
			},
		},
		Research: agentcore.ResearchPolicy{
			Enabled:               true,
			Eligible:              researchLoopEligible,
			ModelRouted:           spec.ModelIntent,
			MinOpenedSources:      2,
			MaxSourceOpensPerTurn: 4,
			MaxOpenedSources:      12,
			RequireDiscovery:      true,
			RequireFinalCitations: true,
		},
		Subtasks: agentcore.SubtaskPolicy{
			Enabled: true, MaxTasks: 3, MaxConcurrent: 2, MaxTurns: 12, MaxDepth: 1,
		},
		MaxTurns:        48,
		SubtaskStateDir: spec.SubtaskStateDir,
		ParentSessionID: spec.ParentSessionID,
		Executor: &agentcore.ToolExecutor{
			RequireAccount: spec.RequireToolAccount,
			Registry:       registry, Authorize: authorize,
			Parallel: provider.QwenParallelToolCallsEnabled(spec.ModelID), MaxParallel: 4,
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
}

func office35RouteSystemPrompt(route agentcore.TurnRoute) string {
	if router.IntentID(route.Intent) == router.IntentSimpleWrite &&
		route.Mode == string(router.TurnModeAgent) {
		return prompt.ForQwen35LiteralWrite()
	}
	return ""
}

func featureSwitch(requested string, auto bool, name string) (bool, error) {
	switch strings.TrimSpace(strings.ToLower(requested)) {
	case "", "auto":
		return auto, nil
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("unknown %s mode %q; supported: auto, on, off", name, requested)
	}
}

func safeProductAuthorizer(_ context.Context, _ string, _ map[string]any, kind tools.Kind) agentcore.Authorization {
	if kind == tools.KindDestructive {
		return agentcore.Authorization{
			Status:     agentcore.ToolConfirmationRequired,
			LLMContent: "ERROR: destructive calls require interactive user confirmation.",
		}
	}
	return agentcore.Authorization{Allowed: true, Status: agentcore.ToolOK}
}

func registerEvidenceIngestTools(
	registry *tools.Registry,
	store *rag.DocumentStore,
	cartridges *rag.CartridgeStore,
	scope rag.CartridgeScope,
	names ...string,
) {
	if registry == nil || store == nil {
		return
	}
	for _, name := range names {
		source, ok := registry.Get(name)
		if !ok {
			continue
		}
		registry.Register(&rag.EvidenceIngestTool{
			Tool: source, Store: store, Cartridges: cartridges, Scope: scope,
		})
	}
}

func productAdmission(task string) error {
	plan := router.Classify(router.StripSystemReminder(task))
	if plan == nil {
		return errors.New("kernel product controller requires a non-empty task")
	}
	if plan.ID == router.IntentDeleteGenerated {
		return errors.New("kernel product controller cannot run deletion without interactive confirmation")
	}
	return nil
}

func taskDeliverables(workspace *tools.Workspace, task string) []agentcore.Deliverable {
	targets := router.TaskDeliverableFiles(task)
	standaloneArtifact := false
	if plan := router.Classify(router.StripSystemReminder(task)); plan != nil {
		standaloneArtifact = plan.ID == router.IntentStaticWebArtifact
	}
	deliverables := make([]agentcore.Deliverable, 0, len(targets))
	for _, target := range targets {
		var resolved string
		var err error
		if standaloneArtifact {
			resolved, err = workspace.ResolveArtifact(target)
		} else {
			resolved, err = workspace.Resolve(target)
		}
		if err != nil {
			deliverables = append(deliverables, agentcore.Deliverable{Path: target})
			continue
		}
		_, statErr := os.Stat(resolved)
		deliverables = append(deliverables, agentcore.Deliverable{
			Path: workspace.ModelPath(resolved), Exists: statErr == nil,
		})
	}
	return deliverables
}

var noNetworkRe = regexp.MustCompile(`(?i)\b(?:do not|don't|must not|without|no need to)\s+(?:use\s+)?(?:the\s+)?(?:external\s+)?(?:web|internet|online|network|web search|search(?:\s+the)?\s+web)\b|(?:不得|不要|禁止|無需|无需|不必|不可|不能)(?:使用|進行|进行)?(?:外部)?(?:上網|上网|搜尋網路|搜索网络|網路搜尋|网络搜索|網路|网络)`)
var positiveNetworkRe = regexp.MustCompile(`(?i)\b(?:web_search|web_fetch|web_crawl|meadow[_\s]+browser(?:[_\s]+dom)?|search\s+(?:the\s+)?web|web\s+search|internet\s+research|online\s+research)\b|上網查|上网查|上網研究|上网研究|網路研究|网络研究|搜尋網路|搜索网络`)
var localMeadowBrowserNameRe = regexp.MustCompile(`(?i)\bmeadow\s+browser\b`)
var conversationOnlyPreferenceRe = regexp.MustCompile(`(?i)(?:只(?:是|想)?(?:要)?(?:閒聊|闲聊|聊天|聊聊)|(?:閒聊|闲聊|聊天|聊聊)(?:一下)?(?:就好|而已)|不用(?:搜尋|搜索|查資料|查资料)|不要(?:搜尋|搜索|查資料|查资料)|陪我聊|just\s+(?:chat|talk)|only\s+(?:want\s+to\s+)?(?:chat|talk)|chat\s+only|(?:no|do\s+not|don't)\s+(?:web\s+)?search)`)
var conversationSearchChoiceRe = regexp.MustCompile(`(?i)(?:幫我|帮我|請|请)?(?:開始|继续|繼續)?(?:搜尋|搜索|查證|查证|查資料|查资料)|(?:開始|继续|繼續)?(?:上網|上网)(?:查|找)|(?:please\s+)?(?:search|research|verify)\b`)

// conversationOnlyPreferenceActive is a shared CLI/Desktop routing fact. A
// user's explicit request to chat survives short follow-ups until they clearly
// opt into research or ask for an executable/artifact action. This prevents an
// ordinary recommendation question from silently starting a long tool loop.
func conversationOnlyPreferenceActive(task string, messages []provider.Message) bool {
	current := strings.TrimSpace(router.StripSystemReminder(task))
	if current == "" || positiveNetworkRe.MatchString(current) || lfm25UnsupportedAction(current) {
		return false
	}
	// A negative search instruction contains the word "search" too. Honor the
	// explicit conversation-only choice before considering a positive search
	// choice, and do so for the current first turn rather than history only.
	if conversationOnlyPreferenceRe.MatchString(current) {
		return true
	}
	if conversationSearchChoiceRe.MatchString(current) {
		return false
	}
	skippedCurrent := false
	seenUsers := 0
	for index := len(messages) - 1; index >= 0 && seenUsers < 10; index-- {
		if messages[index].Role != "user" {
			continue
		}
		candidate := strings.TrimSpace(router.StripSystemReminder(provider.MessageText(messages[index])))
		if candidate == "" {
			continue
		}
		if !skippedCurrent && candidate == current {
			skippedCurrent = true
			continue
		}
		seenUsers++
		if positiveNetworkRe.MatchString(candidate) || lfm25UnsupportedAction(candidate) {
			return false
		}
		if conversationOnlyPreferenceRe.MatchString(candidate) {
			return true
		}
		if conversationSearchChoiceRe.MatchString(candidate) {
			return false
		}
	}
	return false
}

func applyConversationOnlyRoute(route agentcore.TurnRoute) agentcore.TurnRoute {
	route.Mode = string(router.TurnModeChat)
	route.Intent = "conversation_preference"
	route.Tools = nil
	route.DisableTools = true
	route.Guidance = append(route.Guidance,
		"The user previously chose conversation only. Answer the current message directly and do not start tools, web search, research, file work, or a background task.",
		"If current information may benefit from external verification, end with two concise choices: 「先聊聊」 and 「幫我搜尋查證」. Do not start research until the user explicitly chooses the second option.",
	)
	return route
}

func isConversationOnlyRoute(route agentcore.TurnRoute) bool {
	return (route.Intent == "conversation_preference" || route.Intent == "conversation_support") && route.DisableTools
}

func emotionalConversationRoute(task string) (agentcore.TurnRoute, bool) {
	assessment := safety.Assess(router.StripSystemReminder(task))
	if assessment.Category != safety.CategoryEmotional && assessment.Category != safety.CategoryAbuse &&
		assessment.Category != safety.CategorySafetySupport {
		return agentcore.TurnRoute{}, false
	}
	route := applyConversationOnlyRoute(agentcore.TurnRoute{})
	route.Intent = "conversation_support"
	route.Guidance = append(route.Guidance,
		"Respond empathetically before presenting choices. Do not treat distress or a request to talk as permission to launch a task.",
	)
	return route, true
}

var (
	localInputDirRe       = regexp.MustCompile("(?:^|[\\s`\"'(])input/")
	localWorkspaceInputRe = regexp.MustCompile(`(?is)(?:讀取|查看|檢查|參考|依照|根據|載入|分析|整理|從|read|inspect|open|load|from|using|with|according\s+to)[^.!?\n。！？]{0,64}(?:/workspace/)`)
)

// hasExplicitLocalInput distinguishes source material from a requested output.
// A bare `/workspace/report.md` after 建立/create is a deliverable, not evidence
// that must be read before the task can finish. Treating every workspace path
// as input made simple creation tasks loop after a successful write.
func hasExplicitLocalInput(task string) bool {
	return localInputDirRe.MatchString(task) || localWorkspaceInputRe.MatchString(task)
}

func researchLoopEligible(task string) bool {
	stripped := router.StripSystemReminder(task)
	if localStaticWebAcceptanceTask(stripped) {
		return false
	}
	explicitNetwork := positiveNetworkRe.MatchString(stripped)
	if noNetworkRe.MatchString(stripped) || (hasExplicitLocalInput(stripped) && !explicitNetwork) {
		return false
	}
	if router.ClassifyTurnMode(stripped) != router.TurnModeAgent {
		return false
	}
	if explicitNetwork {
		return true
	}
	plan := router.Classify(stripped)
	return plan != nil && (plan.ID == router.IntentInternetResearch || plan.ID == router.IntentResearchArtifact)
}

// productResearchEligible lets an explicit user request such as "請上網查"
// enter the evidence loop even when the final response is a chat/table instead
// of a saved artifact. A negative network instruction and local-input-only task
// still fail closed. This is intent/capability routing, not semantic search
// planning.
func productResearchEligible(task string) bool {
	stripped := router.StripSystemReminder(task)
	if localStaticWebAcceptanceTask(stripped) {
		return false
	}
	if noNetworkRe.MatchString(stripped) || (hasExplicitLocalInput(stripped) && !positiveNetworkRe.MatchString(stripped)) {
		return false
	}
	return positiveNetworkRe.MatchString(stripped) || researchLoopEligible(stripped)
}

// localStaticWebAcceptanceTask distinguishes the embedded Meadow Browser
// validator from a request to browse the public web. The product name alone is
// not network intent when the task is already an explicit local HTML artifact;
// explicit web_search/web_fetch/web_crawl wording still enables research.
func localStaticWebAcceptanceTask(task string) bool {
	plan := router.Classify(task)
	if plan == nil || plan.ID != router.IntentStaticWebArtifact {
		return false
	}
	withoutLocalValidator := localMeadowBrowserNameRe.ReplaceAllString(task, "")
	return !positiveNetworkRe.MatchString(withoutLocalValidator)
}

func baseTaskProfile(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
	profile := agentcore.TaskProfile{
		Enabled: true, Intent: route.Intent, RequiresToolAction: true,
		MaxConsecutiveFailures: 5, AuthoritativeTask: task,
	}
	if route.Mode == string(router.TurnModeChat) {
		profile.RequiresToolAction = false
		return profile
	}
	switch router.IntentID(route.Intent) {
	case router.IntentEngineeringFix, router.IntentDelegatedLocalTask:
		profile.RequiresInspection = true
		profile.RequiresMutation = true
	case router.IntentInspectOrSearch:
		profile.RequiresInspection = true
	case router.IntentInternetResearch:
		profile.RequiresNetworkEvidence = true
		profile.EvidenceTools = networkEvidenceTools()
	case router.IntentResearchArtifact:
		profile.RequiresNetworkEvidence = true
		profile.RequiresMutation = true
		profile.EvidenceTools = networkEvidenceTools()
	case router.IntentSimpleWrite, router.IntentSimpleCopy, router.IntentShellScript,
		router.IntentConfigArtifact, router.IntentStaticWebArtifact:
		profile.RequiresMutation = true
		if router.IntentID(route.Intent) == router.IntentSimpleWrite {
			// A successful write proves bytes reached disk, but it does not prove
			// the agent checked the saved artifact it is about to claim. Make a
			// same-path read-back the universal simple-write completion contract.
			// This is effect validation, not a keyword-derived intent decision.
			profile.RequiredEvidenceTools = append(profile.RequiredEvidenceTools, router.ToolRead)
			profile.RequiredEvidenceBindings = map[string]agentcore.RequiredEvidenceBinding{
				router.ToolRead: {MutationPathArgs: []string{"file_path"}},
			}
			profile.ForceRequiredEvidenceAfterMutations = 1
			// Do not spend another 27B turn asking the model to retype
			// read_file on the path it just wrote.
			profile.AutoExecuteDeterministicEvidence = true
		}
		if router.IntentID(route.Intent) == router.IntentStaticWebArtifact {
			if htmlOnlyStaticWebRoute(task, route) {
				// A one-file HTML game deliberately has no TypeScript source, so
				// forcing the typed build/browser evidence chain can only select a
				// tool that the route did not expose. Prove the saved bytes with a
				// deterministic read-back and let an external browser acceptance
				// runner provide UI evidence when that capability is available.
				profile.RequiredEvidenceTools = append(profile.RequiredEvidenceTools, router.ToolRead)
				profile.RequiredEvidenceBindings = map[string]agentcore.RequiredEvidenceBinding{
					router.ToolRead: {MutationPathArgs: []string{"file_path"}},
				}
				profile.ForceRequiredEvidenceAfterMutations = 1
				profile.AutoExecuteDeterministicEvidence = true
				profile.TaskStartPrompt = "STATIC_WEB_SAVE_REQUIRED: create the requested self-contained HTML game now with one native write_file call. Keep CSS and JavaScript inline; do not create TypeScript or call typescript_web_build."
				profile.MutationRequiredPrompt = "STATIC_WEB_MUTATION_REQUIRED: prose and code blocks are not saved files. Send one native write_file/edit call for the requested self-contained HTML file now."
				profile.MutationRecoveryPrompt = "MUTATION_PROTOCOL_RECOVERY: preserve any complete HTML draft already present and send exactly one native write_file or edit API call now."
				if strings.Contains(strings.ToLower(task), ".pdf") {
					profile.RequiresInspection = true
					profile.EvidenceTools = append(profile.EvidenceTools, "pdf_extract")
					profile.RequiredEvidenceTools = append([]string{"pdf_extract"}, profile.RequiredEvidenceTools...)
				}
			} else {
				profile.RequireInteractionCoverageQuotes = true
				if strings.Contains(strings.ToLower(task), ".pdf") {
					// A passing page build proves only that the presentation runs. It
					// cannot prove the model read the authoritative PDF, so retain a
					// separate source-evidence stage before build/browser validation.
					profile.RequiresInspection = true
					profile.EvidenceTools = append(profile.EvidenceTools, "pdf_extract")
					profile.RequiredEvidenceTools = append(profile.RequiredEvidenceTools, "pdf_extract")
				}
				// Interactive pages use TypeScript as the source-of-truth code gate,
				// then Meadow Browser for independent visual and behavioral evidence.
				// A screenshot cannot prove that filters, persistence, reset, or game
				// controls work, so delivery requires a declarative interaction check.
				profile.EvidenceTools = append(profile.EvidenceTools, router.ToolWebBuild, router.ToolVisual, router.ToolInteraction)
				profile.RequiredEvidenceTools = append(profile.RequiredEvidenceTools, router.ToolWebBuild, router.ToolVisual, router.ToolInteraction)
				profile.MaxRequiredEvidenceFailures = 2
				profile.RequiredEvidenceFailureLimits = map[string]int{
					router.ToolWebBuild:    4,
					router.ToolVisual:      2,
					router.ToolInteraction: 4,
				}
				profile.RequiredEvidenceRepairTools = map[string]string{
					router.ToolWebBuild:    router.ToolEdit,
					router.ToolVisual:      router.ToolEdit,
					router.ToolInteraction: router.ToolEdit,
				}
				profile.RequiredEvidenceBindings = map[string]agentcore.RequiredEvidenceBinding{
					router.ToolWebBuild: {
						MutationPathArgs: []string{"html_path", "source_path"},
					},
					router.ToolVisual: {
						MutationPathArgs: []string{"file_path"},
						MatchArg:         "file_path",
						MatchPriorTool:   router.ToolWebBuild,
						MatchPriorArg:    "html_path",
					},
					router.ToolInteraction: {
						MutationPathArgs: []string{"file_path"},
						MatchArg:         "file_path",
						MatchPriorTool:   router.ToolWebBuild,
						MatchPriorArg:    "html_path",
					},
				}
				// The static app requires at least an HTML shell and a TypeScript
				// source. After two successful mutations, stop accepting endless
				// rewrites and explicitly advance to build, then visual evidence.
				profile.ForceRequiredEvidenceAfterMutations = 2
				profile.TaskStartPrompt = "STATIC_WEB_SAVE_REQUIRED: the previous response may contain a useful draft, but no saved interactive app exists yet. Reuse that draft as untrusted source material. Send one native write_file call now for the first missing artifact: an HTML shell that loads a sibling compiled .js file, or the sibling .ts source containing the application logic. Do not paste code as prose."
				profile.MutationRequiredPrompt = "STATIC_WEB_MUTATION_REQUIRED: prose and code blocks are not saved files. Reuse any complete draft already in this conversation, split it into a small HTML shell plus a sibling TypeScript source, and send one native write_file/edit call for the first missing file now. After both files exist, call typescript_web_build, repair diagnostics, call meadow_browser_visual_check, then prove requested controls with meadow_browser_interaction_check fill/click actions and DOM assertions."
				profile.MutationRecoveryPrompt = "MUTATION_PROTOCOL_RECOVERY: the previous response contained or claimed a file draft, but nothing was saved. Preserve and reuse that draft instead of rewriting from memory. Send exactly one native write_file or edit API call now for the first missing HTML shell or sibling TypeScript source. The HTML must load the compiled sibling .js file and interactive logic belongs in .ts. Do not paste code as prose and do not validate in the same response."
			}
		}
	case router.IntentOfficeArtifact:
		if len(router.TaskTargetFiles(task)) > 0 || router.TaskTargetFile(task) != "" {
			profile.RequiresMutation = true
		}
	case router.IntentGeneralAction:
		profile.RequiresToolAction = router.ShouldRequireTool(task)
	}
	// A typed presentation has no user-authored filename: the tool owns its
	// generated directory. Track the successful native publisher call as a
	// distinct completion obligation so a passing calculator/JSON validator
	// cannot silently satisfy the requested deck as well.
	if router.TaskRequiresPresentationArtifact(task) {
		profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, router.ToolPresentation)
		profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolPresentation)
		if router.IntentID(route.Intent) == router.IntentOfficeArtifact &&
			router.TaskRequiresExecutableValidation(task) {
			// A calculator/checker commonly runs before the deck is published. Make
			// the final executable acceptance check a distinct ordered stage after
			// presentation_artifact, so deck creation cannot stale the only proof and
			// trigger a false fast-final/incomplete downgrade.
			profile.EvidenceTools = appendUniqueHermesTool(profile.EvidenceTools, router.ToolShell)
			profile.RequiredEvidenceTools = appendUniqueHermesTool(profile.RequiredEvidenceTools, router.ToolShell)
			profile.ForceRequiredEvidenceAfterMutations = 1
		}
	}
	// Validation-first follow-ups must not inherit a synthetic mutation gate
	// merely because the router classified the surrounding coding context as an
	// engineering fix.  A user who says "verify the existing artifact and only
	// edit if it fails" is asking for fresh evidence first; forcing a write after
	// successful validation both wastes turns and risks damaging correct work.
	if explicitExistingArtifactValidation(task) {
		profile.RequiresMutation = false
	}
	return profile
}

func appendUniqueHermesTool(values []string, name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return values
	}
	for _, value := range values {
		if strings.TrimSpace(value) == name {
			return values
		}
	}
	return append(values, name)
}

func networkEvidenceTools() []string {
	return []string{
		router.ToolWebSearch, router.ToolWebFetch, router.ToolWebCrawl,
		router.ToolBrowsePage, "meadow_browser_dom",
	}
}

func office35TaskProfile(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
	profile := baseTaskProfile(task, route)
	localWithoutNetwork := hasExplicitLocalInput(task) && !positiveNetworkRe.MatchString(task)
	if noNetworkRe.MatchString(task) || localWithoutNetwork {
		profile.RequiresNetworkEvidence = false
		if len(router.TaskTargetFiles(task)) > 0 || router.TaskTargetFile(task) != "" {
			profile.RequiresInspection = true
			profile.RequiresMutation = true
			profile.EvidenceTools = append(profile.EvidenceTools, "pdf_extract", "meadow_browser_dom")
		}
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
	} else if router.IntentID(route.Intent) == router.IntentStaticWebArtifact && explicitExistingArtifactRepair(task) {
		// A follow-up repair already has both HTML and TypeScript. One real
		// source mutation is enough to advance back into build -> visual ->
		// interaction; keeping the new-artifact threshold of two lets a small
		// model reread both files forever while waiting for a synthetic second
		// write.
		profile.ForceRequiredEvidenceAfterMutations = 1
		profile.ForceMutationToolAfterInspections = router.ToolEdit
		profile.MinInspectionsBeforeForcedMutation = 2
		for tool, binding := range profile.RequiredEvidenceBindings {
			binding.AllowPreexistingPaths = true
			if tool == router.ToolWebBuild {
				binding.RequireAnyMutationPath = true
			}
			profile.RequiredEvidenceBindings[tool] = binding
		}
	}
	return profile
}

func explicitExistingArtifactRepair(task string) bool {
	text := strings.ToLower(strings.TrimSpace(task))
	hasExisting := containsAnyText(text,
		"/session/", "existing", "current file", "same chat", "previous file",
		"既有", "已存在", "現有", "现有", "同一聊天", "剛才", "刚才", "上一輪", "上一轮")
	if !hasExisting {
		// A bare "continue" is frequently part of the product being built (for
		// example pause/continue controls in a game or timer). Treat it as
		// existing-artifact context only when it is attached to an explicit
		// follow-up mutation verb. This prevents a new artifact specification
		// containing both "continue" and "fix compiler errors" from being
		// misclassified as a one-mutation repair.
		hasExisting = containsAnyText(text,
			"continue fixing", "continue to fix", "continue editing", "continue to edit",
			"continue modifying", "continue to modify", "continue updating", "continue to update",
			"繼續修", "继续修", "繼續改", "继续改", "繼續編輯", "继续编辑",
			"接著修", "接着修", "接著改", "接着改")
	}
	hasMutation := containsAnyText(text,
		"repair", "fix", "modify", "update", "improve", "change", "add",
		"修復", "修复", "修改", "更新", "改善", "變更", "变更", "增加", "新增")
	return hasExisting && hasMutation && !explicitExistingArtifactValidation(task)
}

// explicitExistingArtifactValidation recognizes validation-first work on an
// existing artifact. The user may either forbid changes or explicitly make
// repair conditional on a failed compiler/validator. Both cases must run the
// existing evidence first; only a concrete failure may open the edit stage.
// Ordinary create/fix tasks stay on the mutation-bound anti-false-complete path.
func explicitExistingArtifactValidation(task string) bool {
	text := strings.ToLower(strings.TrimSpace(task))
	hasValidation := containsAnyText(text,
		"validate", "verify", "compile", "recompile", "test", "check", "acceptance",
		"驗證", "验证", "編譯", "编译", "測試", "测试", "檢查", "检查", "驗收", "验收")
	hasExisting := containsAnyText(text,
		"/session/", "existing", "current file", "same chat", "previous file",
		"already fixed", "currently fixed",
		"既有", "已存在", "目前已存在", "現有", "现有", "已修正", "已修復", "已修复",
		"同一聊天", "剛才", "刚才", "上一輪", "上一轮")
	hasNoMutation := containsAnyText(text,
		"do not rewrite", "don't rewrite", "do not modify", "without modifying", "no changes",
		"不要重寫", "不要重写", "不要修改", "不用修改", "不需修改", "不要變更", "不要变更")
	hasConditionalRepair := explicitExistingArtifactConditionalRepair(task)
	return hasValidation && hasExisting && (hasNoMutation || hasConditionalRepair)
}

func explicitExistingArtifactConditionalRepair(task string) bool {
	text := strings.ToLower(strings.TrimSpace(task))
	return containsAnyText(text,
		"if it fails", "if validation fails", "if the build fails", "if there are errors", "fix any errors",
		"only modify if", "modify only if", "only edit if", "edit only if",
		"若有錯", "若有错", "有錯就修", "有错就修", "如果有錯", "如果有错",
		"驗證失敗就", "验证失败就", "編譯失敗就", "编译失败就",
		"失敗才修改", "失败才修改", "失敗才修", "失败才修")
}

func htmlOnlyStaticWebTask(task string) bool {
	lower := strings.ToLower(task)
	if explicitTypeScriptWebTask(lower) {
		return false
	}
	rejectedTS := containsAnyText(lower,
		"不要 typescript", "不要 ts", "不要type",
		"no typescript", "without typescript", "not typescript")
	explicitHTMLOnly := containsAnyText(lower,
		"html版本", "html 版本", "單檔 html", "单档 html",
		"一個html", "一个html", "html版", "單檔", "单档")
	hasGame := containsAnyText(lower,
		"遊戲", "游戏", "game", "貪食蛇", "贪食蛇", "snake",
		"pac-man", "pacman", "pac man", "小精靈", "小精灵",
		"tetris", "俄羅斯方塊", "俄罗斯方块", "2048", "pong", "breakout")
	creating := containsAnyText(lower,
		"做一個", "做个", "做個", "幫我做", "帮我做",
		"製作", "制作", "建立", "寫一個", "写一个",
		"create", "make a", "make an", "build a", "implement",
		"直接實作", "直接实作")
	if rejectedTS && (explicitHTMLOnly || hasGame ||
		strings.Contains(lower, "index.html") || strings.Contains(lower, "write_file")) {
		return true
	}
	if explicitHTMLOnly && hasGame {
		return true
	}
	// Casual "做一個Pacman" is still an HTML mini-game. Do not require the
	// magic words「html版本」; only an explicit TypeScript ask leaves this path.
	return creating && hasGame
}

func unfinishedInteractiveWebFollowUp(request agentcore.TurnRouteRequest) bool {
	task := strings.TrimSpace(router.StripSystemReminder(request.Task))
	lowerTask := strings.ToLower(task)
	if explicitTypeScriptWebTask(lowerTask) {
		return false
	}
	if !router.IsProgressOrContinueFollowUp(task) && !implementPlannedWebFollowUp(task) {
		return false
	}
	blob := lowerTask
	for _, message := range request.Messages {
		blob += "\n" + strings.ToLower(message.Content)
		for _, call := range message.ToolCalls {
			blob += "\n" + strings.ToLower(call.Function.Name)
		}
	}
	return containsAnyText(blob,
		"html", "貪食蛇", "贪食蛇", "snake", "pac-man", "pacman", "pac man",
		"遊戲", "游戏", "game",
		"task_plan", "write_file", "index.html",
	)
}

func implementPlannedWebFollowUp(task string) bool {
	t := strings.TrimSpace(router.StripSystemReminder(task))
	if t == "" || len([]rune(t)) > 80 {
		return false
	}
	lower := strings.ToLower(t)
	return containsAnyText(lower,
		"從step", "从step", "step1", "step 1", "步驟1", "步骤1",
		"開始實作", "开始实作", "開始幫我從", "开始帮我从",
		"照計劃做", "照计划做", "照計畫做",
		"從第一步", "从第一步",
	)
}

func explicitTypeScriptWebTask(lowerTask string) bool {
	if containsAnyText(lowerTask, "不要 typescript", "不要 ts", "不要type",
		"no typescript", "without typescript", "not typescript") {
		return false
	}
	return containsAnyText(lowerTask, "typescript", ".ts", "用 ts")
}

func htmlOnlyStaticWebRequest(request agentcore.TurnRouteRequest) bool {
	return htmlOnlyStaticWebTask(request.Task) || unfinishedInteractiveWebFollowUp(request)
}

func htmlOnlyStaticWebRoute(task string, route agentcore.TurnRoute) bool {
	if htmlOnlyStaticWebTask(task) {
		return true
	}
	for _, line := range route.Guidance {
		if strings.Contains(line, "self-contained HTML") {
			return true
		}
	}
	return false
}

func containsAnyText(text string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(text, candidate) {
			return true
		}
	}
	return false
}

type advisoryRouter struct {
	artifactRoot string
}

func (r advisoryRouter) Route(_ context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	task := router.StripSystemReminder(request.Task)
	if route, ok := emotionalConversationRoute(task); ok {
		return route, nil
	}
	if conversationOnlyPreferenceActive(task, request.Messages) {
		return applyConversationOnlyRoute(agentcore.TurnRoute{}), nil
	}
	plan := router.Classify(task)
	if plan == nil {
		if unfinishedInteractiveWebFollowUp(request) {
			plan = &router.Plan{ID: router.IntentStaticWebArtifact}
		} else {
			return agentcore.TurnRoute{}, nil
		}
	}
	mode := router.ClassifyTurnMode(task)
	route := agentcore.TurnRoute{Intent: string(plan.ID), Mode: string(mode)}
	// A very large pasted document is replaced by the ingestion kernel with a
	// source-coordinate index. Even when the wording sounds conversational,
	// disabling tools here would strand the model with an index it cannot read.
	if provider.EstimateTextTokens(task) >= documentIngestionTokens &&
		containsTool(request.AvailableTools, rag.DocumentSearchToolName) {
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
		route.Tools = selectTools(request.AvailableTools, map[string]bool{
			rag.DocumentSearchToolName: true,
			rag.DocumentReadToolName:   true,
			router.ToolAskUser:         true,
		})
		return route, nil
	}
	// An artifact intent is itself an execution boundary. Do not let a future
	// wording regression downgrade "make a game/page/file" to chat merely
	// because the user did not invent a filename first.
	switch plan.ID {
	case router.IntentSimpleWrite, router.IntentSimpleCopy, router.IntentShellScript,
		router.IntentConfigArtifact, router.IntentStaticWebArtifact,
		router.IntentOfficeArtifact, router.IntentResearchArtifact,
		router.IntentEngineeringFix, router.IntentDelegatedLocalTask:
		mode = router.TurnModeAgent
		route.Mode = string(mode)
	}
	if unfinishedInteractiveWebFollowUp(request) && plan.ID != router.IntentStaticWebArtifact {
		plan = &router.Plan{ID: router.IntentStaticWebArtifact}
		route.Intent = string(plan.ID)
		route.Mode = string(router.TurnModeAgent)
		mode = router.TurnModeAgent
	}
	if plan.ID == router.IntentStaticWebArtifact {
		artifactRoot := strings.TrimSpace(r.artifactRoot)
		if artifactRoot == "" {
			artifactRoot = "/workspace"
		}
		htmlOnly := htmlOnlyStaticWebRequest(request)
		deliverable := "choose a sensible `.html` filename and a sibling `.ts` source filename"
		if htmlOnly {
			deliverable = "create one self-contained `.html` file"
		}
		if name := router.TaskTargetFile(task); name != "" {
			if htmlOnly {
				deliverable = "create `" + name + "`"
			} else {
				stem := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
				deliverable = "create `" + name + "` plus sibling `" + stem + ".ts`"
			}
		}
		if htmlOnly {
			route.Guidance = append(route.Guidance,
				"Concrete deliverable: "+deliverable+" as `"+artifactRoot+"/index.html` (or another `.html` name directly under `"+artifactRoot+"`). `"+artifactRoot+"` is already the project folder; do not create a nested game directory such as `"+artifactRoot+"/snake/` or `"+artifactRoot+"/pacman/`.",
				"The user asked for an HTML game. Write one self-contained HTML file with inline CSS/JS. Do not create TypeScript, do not call typescript_web_build, and do not wait for Meadow Browser unless the file is already saved.",
				"This is an interactive web artifact, not an office document. Do not export or convert it to PDF.",
			)
		} else {
			route.Guidance = append(route.Guidance,
				"Concrete deliverable: "+deliverable+" under `"+artifactRoot+"` with native write_file/edit calls. Use full `"+artifactRoot+"/...` paths. Prose or a code block is not a saved artifact; if a previous response already contains a useful draft, reuse it when writing instead of regenerating it.",
				"TypeScript is mandatory for new interactive page/game logic. Keep HTML as a small shell, put application logic in the sibling .ts source, and load the compiled sibling .js with a script src attribute. Do not hide the app logic in an inline script.",
				"After both files are saved, call typescript_web_build. Treat every compiler diagnostic as repair feedback: edit the saved .ts or HTML and rerun the same build until it passes.",
				"Only after a passing TypeScript build, call meadow_browser_visual_check on the saved HTML, then call meadow_browser_interaction_check with ordered fill/click actions and rendered DOM assertions that exercise the user-requested controls. Completion requires successful build, visual, and interaction evidence in that order.",
				"After any later edit to TypeScript or HTML, the compiled page and prior browser evidence are stale. Restart the evidence chain at typescript_web_build, then visual check, then interaction check; never test stale JavaScript.",
				"This is an interactive web artifact, not an office document. Do not export or convert it to PDF unless the user explicitly asked for PDF or a shareable document.",
				"If the TypeScript compiler or Meadow Browser is unavailable, preserve the saved source files and report the exact unavailable verification; do not substitute PDF conversion or invent a different checker.",
			)
		}
		allowed := map[string]bool{
			router.ToolWrite: true, router.ToolEdit: true, router.ToolRead: true,
			router.ToolGlob: true, router.ToolGrep: true, router.ToolList: true,
		}
		if htmlOnly {
			allowed[tools.TaskPlanToolName] = true
		} else {
			allowed[router.ToolWebBuild] = true
			allowed[router.ToolVisual] = true
			allowed[router.ToolInteraction] = true
		}
		// A PDF-backed HTML slider is still an interactive web artifact, but it
		// also needs the source reader. Exposing pdf_extract here is a capability
		// decision only; the model remains responsible for what to extract and how
		// to present it.
		if strings.Contains(strings.ToLower(task), ".pdf") {
			allowed["pdf_extract"] = true
			allowed[rag.DocumentSearchToolName] = true
			allowed[rag.DocumentReadToolName] = true
		}
		// Shell is useful only when the user/repository supplied the exact
		// validator.  Leaving a free-form shell in the ordinary one-file route
		// made Qwen reinterpret HTML preview as the office HTML-to-PDF pipeline.
		if router.NamedValidationCommand(task) != "" {
			allowed[router.ToolShell] = true
		}
		route.Tools = selectTools(request.AvailableTools, allowed)
	}
	if plan.ID == router.IntentOfficeArtifact && router.TaskRequiresPresentationArtifact(task) {
		route.Guidance = append(route.Guidance,
			"The requested presentation is a separate typed deliverable. Call presentation_artifact with the final structured slides; a JSON file, calculation script, or passing shell validator does not create or replace the deck.",
		)
	}
	// A literal one-file creation is the agent equivalent of a renderer's
	// potentially-visible set: only the capability that can advance the current
	// frame belongs on the hot path.  Exposing the complete office/research
	// registry here added thousands of schema tokens and gave small local models
	// attractive but irrelevant alternatives (search, shell, presentation, ...).
	// Keep richer read/edit/validation surfaces on their own classified routes.
	if plan.ID == router.IntentSimpleWrite {
		allowed := map[string]bool{router.ToolWrite: true}
		for _, name := range plan.Tools {
			allowed[name] = true
		}
		if !allowed[router.ToolRead] {
			route.Guidance = append(route.Guidance,
				"Create the requested file with one native write_file call. Preserve literal names, dates, URLs, amounts, and unknown fields exactly as supplied; do not invent workflow steps or unstated facts.",
			)
		}
		route.Tools = selectTools(request.AvailableTools, allowed)
		route.SamplingPhase = provider.SamplingPhasePrecision
	}
	switch mode {
	case router.TurnModeChat:
		if plan.ID == router.IntentStaticWebArtifact || unfinishedInteractiveWebFollowUp(request) {
			route.Mode = string(router.TurnModeAgent)
			route.DisableTools = false
		} else {
			route.DisableTools = true
		}
	case router.TurnModeExplore, router.TurnModePlan:
		route.Tools = readOnlyTools(request.AvailableTools, mode == router.TurnModePlan)
	}
	if plan.ID == router.IntentDelegatedLocalTask {
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
		route.Tools = delegatedLocalTools(request.AvailableTools)
	}
	return route, nil
}

func office35SemanticPreflightEligible(task string) bool {
	task = router.StripSystemReminder(task)
	if router.ClassifyTurnMode(task) != router.TurnModeAgent {
		return false
	}
	// A self-contained page/game request has safe, reversible defaults for
	// styling and filename.  Blocking the first write to ask about aesthetics
	// caused Qwen to lose the pending mutation and then claim an unsaved file.
	// Material ambiguity can still be expressed explicitly by the user in a
	// follow-up; it must not prevent creating the requested local artifact.
	plan := router.Classify(task)
	// Clear single-file creation already has a deterministic, reversible next
	// action.  A separate draft+critic contract costs two or three model calls
	// and can introduce assumptions that were absent from the user's literals.
	// Complex office, repair, and research tasks retain semantic preflight.
	return plan == nil || (plan.ID != router.IntentStaticWebArtifact && plan.ID != router.IntentSimpleWrite)
}

type office35AdvisoryRouter struct {
	artifactRoot string
}

func (r office35AdvisoryRouter) Route(ctx context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	route, err := (advisoryRouter{artifactRoot: r.artifactRoot}).Route(ctx, request)
	if err != nil || isConversationOnlyRoute(route) || !researchLoopEligible(request.Task) {
		return route, err
	}
	plan := router.Classify(router.StripSystemReminder(request.Task))
	if plan != nil && plan.ID == router.IntentResearchArtifact {
		route.Intent = string(router.IntentResearchArtifact)
	} else {
		route.Intent = string(router.IntentInternetResearch)
	}
	route.Mode = string(router.TurnModeAgent)
	route.DisableTools = false
	return route, nil
}

func readOnlyTools(available []string, includePlan bool) []string {
	allowed := map[string]bool{
		router.ToolRead: true, router.ToolGlob: true, router.ToolGrep: true,
		router.ToolList: true, "pdf_extract": true, router.ToolAskUser: true,
		rag.DocumentSearchToolName: true, rag.DocumentReadToolName: true,
	}
	if includePlan {
		allowed[tools.TaskPlanToolName] = true
	}
	return selectTools(available, allowed)
}

func delegatedLocalTools(available []string) []string {
	allowed := map[string]bool{
		router.ToolRead: true, router.ToolWrite: true, router.ToolEdit: true,
		router.ToolShell: true, "process": true, router.ToolGlob: true,
		router.ToolGrep: true, router.ToolList: true, router.ToolVisual: true,
		"pdf_extract": true, "presentation_artifact": true,
		rag.DocumentSearchToolName: true, rag.DocumentReadToolName: true,
		tools.TaskPlanToolName: true, tools.AskUserToolName: true,
	}
	return selectTools(available, allowed)
}

func selectTools(available []string, allowed map[string]bool) []string {
	selected := make([]string, 0, len(available))
	for _, name := range available {
		if allowed[name] {
			selected = append(selected, name)
		}
	}
	return selected
}

func containsTool(available []string, target string) bool {
	for _, name := range available {
		if name == target {
			return true
		}
	}
	return false
}

type documentIngestor struct {
	store     *rag.DocumentStore
	minTokens int
}

func (i documentIngestor) Ingest(
	ctx context.Context,
	request agentcore.DocumentIngestionRequest,
	onProgress func(agentcore.DocumentIngestionProgress),
) (agentcore.DocumentIngestionResult, error) {
	minTokens := i.minTokens
	if minTokens <= 0 {
		minTokens = documentIngestionTokens
	}
	if i.store == nil || provider.EstimateTextTokens(request.Text) < minTokens {
		return agentcore.DocumentIngestionResult{}, nil
	}
	doc, err := i.store.Ingest(ctx, request.SourceName, request.Text, func(progress rag.DocumentProgress) {
		if onProgress != nil {
			onProgress(agentcore.DocumentIngestionProgress{
				SourceID: progress.SourceID, SourceName: progress.SourceName,
				ProcessedBytes: progress.ProcessedBytes, TotalBytes: progress.TotalBytes,
				Processed: progress.Processed, Total: progress.Total,
				StartLine: progress.Chunk.StartLine, EndLine: progress.Chunk.EndLine,
				Note: progress.Chunk.Note, Reused: progress.Reused, Ready: progress.Ready,
			})
		}
	})
	if err != nil {
		return agentcore.DocumentIngestionResult{}, err
	}
	return agentcore.DocumentIngestionResult{
		Used: true, SourceID: doc.ID, Chunks: len(doc.Chunks), SourceBytes: doc.Bytes,
		ModelContext: i.store.CompactDocumentContext(doc, request.Text),
	}, nil
}

type kernelRetriever struct {
	store            *rag.Store
	topK             int
	profile          string
	includeSourceIDs []string
}

func (r kernelRetriever) Retrieve(_ context.Context, request agentcore.RetrievalRequest) (agentcore.RetrievedContext, error) {
	if r.store == nil {
		return agentcore.RetrievedContext{}, nil
	}
	if request.Phase != agentcore.RetrievalPhaseRecovery && request.Intent == string(router.IntentDelegatedLocalTask) {
		return agentcore.RetrievedContext{}, nil
	}
	topK := r.topK
	if topK < 1 {
		topK = 3
	}
	priorityQuery, failureQuery := "", ""
	if request.Phase == agentcore.RetrievalPhaseRecovery {
		priorityQuery = "recovery_reason: " + request.Reason
		failureQuery = "failure_signature: " + request.FailureSignature
	}
	includeSourceIDs := append([]string(nil), r.includeSourceIDs...)
	directSourceIDs := recoverySourceIDs(r.profile, request.Procedure)
	if len(directSourceIDs) > 0 {
		if len(includeSourceIDs) > 0 {
			directSourceIDs = intersectSourceIDs(directSourceIDs, includeSourceIDs)
			if len(directSourceIDs) == 0 {
				return agentcore.RetrievedContext{}, nil
			}
		}
		includeSourceIDs = append([]string(nil), directSourceIDs...)
	}
	matches := r.store.SearchRequest(rag.SearchRequest{
		Text: request.Task, PriorityText: priorityQuery, FailureText: failureQuery,
		Intent: request.Intent, Profile: r.profile, Phase: request.Phase,
		RequireProfile: request.Phase == agentcore.RetrievalPhaseRecovery && r.profile != "",
		Limit:          topK, IncludeSourceIDs: includeSourceIDs, DirectSourceIDs: directSourceIDs,
		ExcludeSourceIDs: request.ExcludeSourceIDs,
	})
	result := agentcore.RetrievedContext{Text: rag.CompactEvidence(matches, 2200)}
	for _, match := range matches {
		if match.Template != nil {
			result.Sources = append(result.Sources, agentcore.RetrievalSource{
				ID: rag.TemplateID(match.Template), Title: match.Template.Title,
				Source: match.Template.Source, Version: match.Template.Version,
				Updated: match.Template.Updated, Score: match.Score,
			})
		}
	}
	return result, nil
}

func recoverySourceIDs(profile, procedure string) []string {
	if profile != Office35ProfileID {
		return nil
	}
	switch strings.TrimSpace(procedure) {
	case agentcore.RetrievalProcedureFailureMutation:
		return []string{"qwen35-strategy-switch-recovery-v1"}
	case agentcore.RetrievalProcedureIndependentAudit:
		return []string{"qwen35-independent-validator-recovery-v1"}
	case agentcore.RetrievalProcedureNativeTool:
		return []string{"qwen35-native-tool-recovery-v1"}
	default:
		return nil
	}
}

func intersectSourceIDs(left, right []string) []string {
	allowed := make(map[string]bool, len(right))
	for _, id := range right {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = true
		}
	}
	result := make([]string, 0, len(left))
	for _, id := range left {
		if id = strings.TrimSpace(id); id != "" && allowed[id] {
			result = append(result, id)
		}
	}
	return result
}
