// Package producthost owns the compiled product composition shared by the
// local Agent daemon, CLI and Desktop shells. Presentation clients must not
// reconstruct this graph or carry proprietary prompts and policy.
package producthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"github.com/hey-meadow/meadow-code/go/core/deskevo"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/evolve"
	harnesscore "github.com/hey-meadow/meadow-code/go/core/harness"
	"github.com/hey-meadow/meadow-code/go/core/memoryservices"
	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/prompt"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/rag"
	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
	"github.com/hey-meadow/meadow-code/go/core/serve"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/taskruntime"
	"github.com/hey-meadow/meadow-code/go/core/telemetry"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const defaultModelID = "Meadow Coder 35B"

// configurationHandler carries the session owner's admission boundary through
// the product composition so outer daemon routes share the same idle lease.
type configurationHandler struct {
	http.Handler
	server    *serve.Server
	schedules *nativeScheduleService
}

func (h *configurationHandler) WithConfigurationAdmission(next http.Handler) http.Handler {
	return h.server.WithConfigurationAdmission(next)
}

func (h *configurationHandler) ApplyConfigurationLease(leaseID string, apply func() (string, error)) error {
	return h.server.ApplyConfigurationLease(leaseID, apply)
}

func (h *configurationHandler) WithSessionConfigurationAdmission(next http.Handler) http.Handler {
	return h.server.WithSessionConfigurationAdmission(next)
}

func (h *configurationHandler) Close() error {
	var scheduleErr error
	if h.schedules != nil {
		scheduleErr = h.schedules.Close()
	}
	return errors.Join(scheduleErr, h.server.Close())
}

func (h *configurationHandler) StartBackgroundServices(ctx context.Context) error {
	if h.schedules == nil {
		return nil
	}
	return h.schedules.Start(ctx)
}

type Config struct {
	WorkspaceRoot         string
	ModelID               string
	ConnectionID          string
	ApprovalMode          engine.ApprovalMode
	UseRouter             bool
	HarnessMode           string
	StateRoot             string
	ArtifactRetrievalMode ArtifactRetrievalMode
	HarnessPolicyFile     string
	ConversationProfile   string
}

func ConfigFromEnvironment(workspaceRoot string) (Config, error) {
	mode, err := engine.ParseApprovalMode(strings.TrimSpace(os.Getenv("MEADOW_APPROVAL_MODE")))
	if err != nil {
		return Config{}, err
	}
	return Config{
		WorkspaceRoot:       workspaceRoot,
		ModelID:             strings.TrimSpace(os.Getenv("MEADOW_MODEL_ID")),
		ApprovalMode:        mode,
		UseRouter:           strings.TrimSpace(os.Getenv("MEADOW_USE_ROUTER")) == "1",
		HarnessMode:         strings.TrimSpace(os.Getenv("MEADOW_HARNESS_MODE")),
		StateRoot:           strings.TrimSpace(os.Getenv("MEADOW_HOME")),
		HarnessPolicyFile:   strings.TrimSpace(os.Getenv("MEADOW_HARNESS_POLICY_FILE")),
		ConversationProfile: strings.TrimSpace(os.Getenv("MEADOW_CONVERSATION_PROFILE")),
	}, nil
}

type Options struct {
	TraceObserver     agenttrace.Observer
	RuntimeControl    runtimecontrol.RuntimeControl
	NewProviderClient func(modelID string) agentcore.KernelModel
	// RequireToolAccount revalidates the trusted account before each tool starts.
	RequireToolAccount func(context.Context) error
	// BrowserClientFactory is an explicit Desktop-owned transport capability.
	// It binds Browser requests to the live session and its workspace; absent
	// capabilities retain the CLI's separately configured Browser surface.
	BrowserClientFactory func(sessionID, workspaceRoot string) *http.Client
	// ModelAllowed checks catalog identity without activating a provider.
	ModelAllowed func(modelID string) bool
	// BackgroundWakeAdmission authorizes an automatic model turn, independently
	// of tool authorization. Paid hosts must supply their prompt credit gate.
	BackgroundWakeAdmission func(context.Context, string) error
}

// NewHandler returns the complete local Agent HTTP contract. The caller owns
// transport authentication and process lifecycle so the same composition can
// later sit behind macOS XPC without changing the Harness.
func NewHandler(config Config, options Options) (http.Handler, error) {
	conversationProfile, err := parseConversationProfile(config.ConversationProfile)
	if err != nil {
		return nil, err
	}
	var desktopSession *tools.DesktopSession
	if conversationProfile == conversationProfileMeadowOS && runtime.GOOS == "linux" {
		// Freeze launch-time ownership. Missing desktop capability remains an
		// observable unavailable tool rather than guessing another X display.
		desktopSession, _ = tools.DesktopSessionFromEnvironment()
	}
	// Freeze the explicit deployment override when creating the host. The
	// selected model still resolves auto separately for every runtime binding.
	retrievalSetting := string(config.ArtifactRetrievalMode)
	if retrievalSetting == "" {
		retrievalSetting = os.Getenv("MEADOW_ARTIFACT_RETRIEVAL")
	}
	retrievalMode, err := parseArtifactRetrievalMode(retrievalSetting)
	if err != nil {
		return nil, err
	}
	policyPath := config.HarnessPolicyFile
	if policyPath == "" {
		policyPath = strings.TrimSpace(os.Getenv("MEADOW_HARNESS_POLICY_FILE"))
	}
	policyRegistry, err := LoadHarnessPolicyRegistry(policyPath)
	if err != nil {
		return nil, err
	}
	if options.RuntimeControl == nil {
		return nil, fmt.Errorf("runtime control is required")
	}
	if options.NewProviderClient == nil {
		return nil, fmt.Errorf("provider client factory is required")
	}
	workspaceRoot := strings.TrimSpace(config.WorkspaceRoot)
	if workspaceRoot == "" {
		return nil, fmt.Errorf("workspace root is required")
	}
	absoluteRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root is not a directory: %s", absoluteRoot)
	}

	modelID := strings.TrimSpace(config.ModelID)
	if modelID == "" {
		modelID = defaultModelID
	}
	mode := config.ApprovalMode
	if mode == "" {
		mode = engine.ModeDefault
	}
	dependencies, err := loadRuntimeDependencies(config.StateRoot)
	if err != nil {
		return nil, err
	}
	dependencies.traceObserver = telemetry.Combine(dependencies.traceObserver, options.TraceObserver)
	defaultClient := options.NewProviderClient(modelID)
	if defaultClient == nil {
		return nil, fmt.Errorf("provider client factory returned nil")
	}
	type runtimeBinding struct {
		modelID             string
		contextWindowTokens int
		maxOutputTokens     int
	}
	activeRuntimeBinding := func() runtimeBinding {
		// An external endpoint has configured capabilities, not a resident
		// process whose model lifecycle belongs to this Desktop.
		if external, ok := options.RuntimeControl.(interface{ ConfiguredModel() (string, int, int) }); ok {
			selected, contextWindow, maxOutput := external.ConfiguredModel()
			return runtimeBinding{modelID: selected, contextWindowTokens: contextWindow, maxOutputTokens: maxOutput}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		status, available, statusErr := options.RuntimeControl.Status(ctx)
		if statusErr == nil && available {
			if resident, ready := status.ResidentModel(); ready && resident != "" {
				return runtimeBinding{
					modelID:         resident,
					maxOutputTokens: status.MaxOutputTokens,
					contextWindowTokens: modelmgr.ResolveRuntimeContextWindowTokens(
						resident, status.ContextWindowTokens,
					),
				}
			}
		}
		return runtimeBinding{
			modelID:             modelID,
			contextWindowTokens: modelmgr.ResolveRuntimeContextWindowTokens(modelID, 0),
		}
	}

	server := serve.NewServer(absoluteRoot, func(cwd string) (*engine.Engine, error) {
		workspace, err := tools.NewWorkspace(cwd)
		if err != nil {
			return nil, err
		}
		active := activeRuntimeBinding()
		residentModelID := active.modelID
		contextBudget := modelmgr.RuntimeBudgetFor(active.contextWindowTokens)
		client := options.NewProviderClient(residentModelID)
		if client == nil {
			return nil, fmt.Errorf("provider client factory returned nil for model %q", residentModelID)
		}
		bindOutputTokenLimit(client, active.maxOutputTokens)
		familyProfile := harnesscore.DefaultFamilyProfile(residentModelID)
		workspace.SetDesktopSession(desktopSession)
		registry := productRegistry(workspace)
		if conversationProfile == conversationProfileMeadowOS && runtime.GOOS == "linux" {
			registry.Register(tools.NewDesktopControl(workspace, desktopSession))
			if options.BrowserClientFactory == nil && strings.TrimSpace(os.Getenv("MEADOW_BROWSER_BASE")) == "" &&
				strings.TrimSpace(os.Getenv("MEADOW_BROWSER_URL")) == "" && strings.TrimSpace(os.Getenv("MEADOW_BROWSER_BIN")) == "" {
				// The macOS embedded Browser is not a Linux desktop driver. Keep
				// its validators out of discovery when no runtime was composed.
				var unavailable []string
				for _, name := range registry.Names() {
					if strings.HasPrefix(name, "meadow_browser_") {
						unavailable = append(unavailable, name)
					}
				}
				registry = registry.Excluding(unavailable)
			}
		}
		registry = rag.RegistryWithArtifactLibrary(registry, dependencies.artifacts)
		tools.RegisterMemoryServices(registry, workspace, dependencies.memoryServices)
		if options.BrowserClientFactory != nil {
			browserClient := options.BrowserClientFactory("", cwd)
			if browserClient == nil {
				return nil, fmt.Errorf("Desktop Browser transport capability is unavailable")
			}
			tools.ConfigureDesktopBrowserTransport(registry, browserClient)
		}
		return &engine.Engine{
			Client:             client,
			Registry:           registry,
			RequireToolAccount: options.RequireToolAccount,
			Mode:               mode,
			TraceObserver:      dependencies.traceObserver,
			ContextManager: productContextManagerFor(cwd, registry).
				WithHistoryTokenBounds(contextBudget.MessageLimitTokens),
			SystemPrompt: productConversationPrompt(conversationProfile, prompt.Options{
				CWD:       cwd,
				IsGitRepo: isGitRepository(cwd),
				Platform:  runtime.GOOS + " " + runtime.GOARCH,
			}),
			ModelID:             residentModelID,
			CWD:                 cwd,
			ContextWindowTokens: contextBudget.WindowTokens,
			SessionTokenLimit:   contextBudget.PromptLimitTokens,
			UseRouter:           familyRouterEnabled(config.UseRouter, familyProfile),
			RAG:                 dependencies.templates,
			RAGTopK:             3,
		}, nil
	})
	server.ResumeModelAllowed = options.ModelAllowed
	server.RequireToolAccount = options.RequireToolAccount
	server.ConnectionID = strings.TrimSpace(config.ConnectionID)
	if server.ConnectionID != "" && server.ResumeModelAllowed == nil {
		server.ResumeModelAllowed = func(saved string) bool { return saved == modelID }
	}
	server.DefaultHarnessMode = config.HarnessMode
	server.ConversationInboxEnabled = conversationProfile == conversationProfileMeadowOS
	server.ResolveHarnessMode = func(modelID, override string) string {
		return productConversationHarnessMode(conversationProfile, modelID, override)
	}
	server.BackgroundWakeEnabled = true
	server.BackgroundWakeAdmission = options.BackgroundWakeAdmission
	server.IndexAttachmentDocuments = func(ctx context.Context, cwd, chatID string, inputs []rag.AttachmentInput) ([]rag.AttachmentReceipt, error) {
		index, err := dependencies.attachments.Session(cwd, chatID)
		if err != nil {
			return nil, err
		}
		return index.Ingest(ctx, inputs)
	}
	// Controls describe the current resident backend capacity. Use the existing
	// passive runtime status boundary; reading controls never activates a model.
	server.InferenceOutputTokenLimit = func(modelID string) int {
		active := activeRuntimeBinding()
		if active.modelID != modelID {
			return 0
		}
		return active.maxOutputTokens
	}
	runtimePool := harnesscore.NewCodexStylePool()
	var schedules *nativeScheduleService
	server.NewRuntime = func(binding harnesscore.Binding, events harnesscore.Events) (agentcore.Runtime, error) {
		// A same-named model on another endpoint cannot inherit this chat or its
		// cached runtime. Check before constructing a new provider or capability.
		if binding.ConnectionID != server.CurrentConnectionID() {
			return nil, session.ErrConnectionMismatch
		}
		if external, ok := options.RuntimeControl.(runtimecontrol.ExternalConnection); ok {
			connection, err := external.Connection()
			if err != nil {
				return nil, err
			}
			if binding.ConnectionID == "" || binding.ConnectionID != connection.ID {
				return nil, session.ErrConnectionMismatch
			}
		}
		if strings.TrimSpace(binding.ModelID) == "" {
			binding.ModelID = activeRuntimeBinding().modelID
		}
		if binding.ContextWindowTokens <= 0 {
			binding.ContextWindowTokens = modelmgr.ResolveRuntimeContextWindowTokens(binding.ModelID, 0)
		}
		contextBudget := modelmgr.RuntimeBudgetFor(binding.ContextWindowTokens)
		if binding.PromptTokenLimit <= 0 {
			binding.PromptTokenLimit = contextBudget.PromptLimitTokens
		}
		binding = bindingWithProductContext(binding)
		if schedules != nil {
			chatID := binding.ChatID
			if chatID == "" {
				chatID = binding.SessionID
			}
			tools.RegisterNativeSchedulerTools(binding.Registry, schedules.scheduler, taskruntime.ScheduleOwner{
				ChatID: chatID, WorkspaceCWD: binding.CWD, ConnectionID: binding.ConnectionID,
			}, schedules.Cancel)
		}
		if dependencies.memoryServices != nil {
			workspace, workspaceErr := tools.NewWorkspace(binding.CWD)
			if workspaceErr != nil {
				return nil, workspaceErr
			}
			tools.RegisterMemoryServices(binding.Registry, workspace, dependencies.memoryServices)
			binding.ContextManager = binding.ContextManager.WithCompleteObservationTools(binding.Registry.CompleteObservationNames())
		}
		if options.BrowserClientFactory != nil {
			browserClient := options.BrowserClientFactory(binding.SessionID, binding.CWD)
			if browserClient == nil {
				return nil, fmt.Errorf("Desktop Browser transport capability is unavailable")
			}
			tools.ConfigureDesktopBrowserTransport(binding.Registry, browserClient)
		}
		if err := bindProductArtifactWorkspace(binding); err != nil {
			return nil, err
		}
		binding.ContextManager = binding.ContextManager.WithHistoryTokenBounds(contextBudget.MessageLimitTokens)
		client := options.NewProviderClient(binding.ModelID)
		if client == nil {
			return nil, fmt.Errorf("provider client factory returned nil for model %q", binding.ModelID)
		}
		// A limit advertised for another resident model is not this binding's
		// capability. Only configure the fresh client from a matching live
		// runtime; an unavailable handshake remains explicitly unknown.
		active := activeRuntimeBinding()
		if active.modelID == binding.ModelID {
			bindOutputTokenLimit(client, active.maxOutputTokens)
		}
		modeOverride := binding.HarnessMode
		if modeOverride == "" {
			modeOverride = config.HarnessMode
		}
		mode := harnesscore.Mode(productConversationHarnessMode(conversationProfile, binding.ModelID, modeOverride))
		if conversationProfile == conversationProfileMeadowOS && mode != harnesscore.ModeCodexStyle && mode != harnesscore.ModeRavenCode {
			return nil, fmt.Errorf("unsupported_meadow_os_coordinator_harness: %s", mode)
		}

		if mode == harnesscore.ModeEvo {
			executable, err := os.Executable()
			if err != nil {
				return nil, err
			}
			resources := filepath.Clean(filepath.Join(filepath.Dir(executable), "../Resources"))
			return deskevo.New(deskevo.Config{StateRoot: config.StateRoot, Python: filepath.Join(resources, "evo/python/bin/python3.12"), Node: filepath.Join(resources, "source-diagnostics/node"), Worker: filepath.Join(resources, "evo/bridge/desktop_worker.py"), Upstream: filepath.Join(resources, "evo/upstream"), Binding: binding, Dependencies: evolve.NativeDependencies{Client: client, Authorize: binding.Authorize, RequireToolAccount: binding.RequireToolAccount, AskUser: binding.AskUser, Events: events, Observer: dependencies.traceObserver}})
		}
		if mode == harnesscore.ModeCodexStyle || mode == harnesscore.ModeRavenCode {
			policy := policyRegistry.Resolve(binding.ModelID)
			// An explicit deployment retrieval override retains precedence.
			if retrievalMode != ArtifactRetrievalAuto {
				policy.ArtifactRetrieval = retrievalMode
			}
			runtimeProfile := ""
			if mode == harnesscore.ModeRavenCode {
				runtimeProfile = agentcore.RuntimeProfileRavenCode
				binding.SystemPrompt = prompt.ForRavenCode(binding.SystemPrompt)
			} else if conversationProfile == conversationProfileMeadowOS {
				binding.SystemPrompt = prompt.ForModelLed(binding.SystemPrompt)
			} else {
				binding.SystemPrompt = productCodexStyleSystemPrompt(binding.ModelID, binding.SystemPrompt)
			}
			binding = bindingWithArtifactRetrieval(binding, policy.ArtifactRetrieval)
			chatID := binding.ChatID
			if chatID == "" {
				chatID = binding.SessionID
			}
			attachments, attachmentErr := dependencies.attachments.Session(binding.CWD, chatID)
			if attachmentErr != nil {
				return nil, attachmentErr
			}
			return runtimePool.RuntimeFor(binding, harnesscore.CodexStyleDependencies{
				RuntimeProfile:  runtimeProfile,
				CoordinatorOnly: conversationProfile == conversationProfileMeadowOS,
				Subtasks:        agentcore.SubtaskPolicy{Enabled: true, MaxTasks: 3, MaxConcurrent: 2, MaxTurns: 12, MaxDepth: 1},
				Client:          client,
				TraceObserver:   WithHarnessPolicyObservation(dependencies.traceObserver, policy),
				DocumentStore:   attachments.PrimaryStore(),
				AttachmentIndex: attachments,
				// Re-present bounded, declared failure evidence for every model;
				// the model still selects repairs, rechecks and its final handoff.
				// Sequential advisor reviews remain opt-in.
				EnableEvidenceReconciliation: true,
				DisableRunHandoffReserve:     !policy.RunHandoffReserveEnabled,
				HandoffReserve:               time.Duration(policy.HandoffReserveMs) * time.Millisecond,
				HandoffMaxTokens:             policy.HandoffMaxOutputTokens,
				DisableRevisionBoundEvidence: !policy.RevisionBoundEvidenceEnabled,
			}, events)
		}
		workspace, err := tools.NewWorkspace(binding.CWD)
		if err != nil {
			return nil, err
		}
		familyProfile := harnesscore.DefaultFamilyProfile(binding.ModelID)
		return harnesscore.NewFamilyRuntime(
			familyProfile,
			harnesscore.FamilyRuntimeSpec{
				Binding:       binding,
				Client:        client,
				TraceObserver: dependencies.traceObserver,
				Workspace:     workspace,
				UseRouter:     familyRouterEnabled(config.UseRouter, familyProfile),
				RAG:           dependencies.templates,
				DocumentStore: dependencies.documents,
				Encyclopedia:  dependencies.encyclopedia,
				Cartridges:    dependencies.cartridges,
				RAGTopK:       3,
			},
			events,
		)
	}
	api := http.NewServeMux()
	api.Handle("/", server.Handler())
	api.Handle("POST /agent/model", server.WithConfigurationAdmission(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Model) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "model is required"})
			return
		}
		result, err := options.RuntimeControl.Activate(r.Context(), body.Model)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	})))
	api.HandleFunc("GET /agent/model", func(w http.ResponseWriter, r *http.Request) {
		status, ok, err := options.RuntimeControl.Status(r.Context())
		w.Header().Set("Content-Type", "application/json")
		if err != nil || !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"ready": false, "error": runtimeErrorText(err)})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "status": status})
	})
	api.HandleFunc("GET /agent/memory-services", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"configured": dependencies.memoryServices != nil, "configuration_error": dependencies.memoryServicesError, "scope": "Native opt-in configuration; no service availability or recall is inferred"})
	})
	schedules, err = newNativeScheduleService(config.StateRoot, server)
	if err != nil {
		return nil, err
	}
	return &configurationHandler{Handler: api, server: server, schedules: schedules}, nil
}

// familyRouterEnabled keeps the product composition valid for model families
// whose runtime contract requires a dedicated task router. MEADOW_USE_ROUTER
// remains an opt-in for optional routers, but a selected Muse/Nemotron runtime
// must never be constructed without the router that defines its tool surface
// and self-iteration policy.
func familyRouterEnabled(configured bool, profile harnesscore.FamilyProfile) bool {
	return configured || profile == harnesscore.FamilyMuseGlimmer
}

func productCodexStyleSystemPrompt(modelID, base string) string {
	// The provider already resolves API aliases and artifact-suffixed IDs.
	// Select instructions from that profile without rewriting the wire ID.
	if provider.IsOrnithModel(modelID) {
		return prompt.ForCodexStyleCoder(base)
	}
	switch strings.TrimSpace(modelID) {
	case "Meadow 26B":
		return prompt.ForGemma12Agent(base)
	case "Meadow 30B":
		return prompt.ForNemotron30Agent(base)
	default:
		return prompt.ForModelLed(base)
	}
}

// productRegistry keeps the prompt and the actually exposed Desktop/agentd
// capability surface in lockstep. DefaultRegistry intentionally stays small;
// the compiled product also needs its office, browser and static-web validators.
// Register only missing tools so a caller-provided browser instance (with its
// configured helper URL/client) is never replaced.
func productRegistry(workspace *tools.Workspace) *tools.Registry {
	registry := tools.DefaultRegistry(workspace, nil)
	ensure := func(tool tools.Tool) {
		if _, ok := registry.Get(tool.Name()); !ok {
			registry.Register(tool)
		}
	}
	ensure(&tools.TypeScriptWebBuild{WS: workspace})
	ensure(&tools.CodeOutline{WS: workspace})
	ensure(&tools.SystemInfo{WS: workspace})
	plan := tools.NewTaskPlan()
	ensure(plan)
	// Historical and current schemas expose one model-owned state. Separate
	// instances would disagree after mixed alias calls and reset revision.
	registry.Register(tools.NewLegacyTodoAlias(plan))
	ensure(&tools.ReadExperience{})
	ensure(&tools.MeadowBrowserInteractionCheck{WS: workspace})
	if checker, ok := registry.Get("meadow_browser_interaction_check"); ok {
		if native, ok := checker.(*tools.MeadowBrowserInteractionCheck); ok {
			ensure(&tools.MeadowBrowserSuite{Checker: native})
			ensure(&tools.MeadowBrowserInspect{Checker: native})
			ensure(&tools.MeadowBrowserSuiteEdit{WS: workspace})
		}
	}
	ensure(&tools.MeadowBrowserWebMCP{WS: workspace})
	ensure(&tools.MeadowBrowserDOM{WS: workspace})
	ensure(&tools.PDFExtract{WS: workspace})
	ensure(&tools.PresentationTemplates{})
	ensure(&tools.PresentationArtifact{WS: workspace})
	if artifact, ok := registry.Get("presentation_artifact"); ok {
		if native, ok := artifact.(*tools.PresentationArtifact); ok {
			ensure(&tools.PresentationFromMarkdown{Artifact: native})
		}
	}
	return registry
}

func bindProductArtifactWorkspace(binding harnesscore.Binding) error {
	chatID := strings.TrimSpace(binding.ChatID)
	if chatID == "" {
		chatID = strings.TrimSpace(binding.SessionID)
	}
	if chatID == "" || binding.Registry == nil {
		return nil
	}
	tool, ok := binding.Registry.Get("presentation_artifact")
	if !ok {
		return nil
	}
	presentation, ok := tool.(*tools.PresentationArtifact)
	if !ok || presentation.WS == nil {
		return nil
	}
	root, err := session.ArtifactsDir(binding.CWD, chatID)
	if err != nil {
		return err
	}
	return presentation.WS.SetSessionRoot(root)
}

// productContextManagerFor owns the memory/compaction policy at the product
// composition boundary. Desktop is a presentation-only reverse proxy, so the
// manager must be constructed in agentd where Engine and Harness bindings are
// created. Always returning a manager keeps context compaction available even
// when neither global nor project memory exists.
func productContextManagerFor(projectRoot string, registry *tools.Registry) *agentcore.ContextManager {
	memory := session.LoadUserMemoryBundle(projectRoot)
	return agentcore.NewContextManager(agentcore.ContextPolicy{
		GlobalMemory:  memory.Global,
		ProjectMemory: memory.Project,
	}).
		// Shell/DOM/browser output can be paged again from its authoritative
		// source. Keep one useful head/tail excerpt instead of allowing an
		// accidental broad listing to occupy the rest of the session.
		WithToolResultBounds(8*1024, 16*1024).
		WithCompleteObservationTools(registry.CompleteObservationNames()).
		WithDuplicateToolResultBounds(512).
		// Persisted artifacts remain exact on disk; subsequent model turns need
		// only their path and digest until an edit explicitly reads them back.
		WithMutationArgumentBounds(1024)
}

// bindingWithProductContext is a defensive boundary for runtime factories.
// Serve normally forwards Engine.ContextManager, but resumed/imported session
// paths must not be able to create a Desktop Harness binding without one.
func bindingWithProductContext(binding harnesscore.Binding) harnesscore.Binding {
	if binding.ContextManager == nil {
		binding.ContextManager = productContextManagerFor(binding.CWD, binding.Registry)
	}
	if binding.Registry != nil {
		// Resumed/imported bindings may carry a manager from another registry.
		// Derive current capabilities even when that manager already exists;
		// replacement revokes stale exemptions without changing other ceilings.
		binding.ContextManager = binding.ContextManager.WithCompleteObservationTools(binding.Registry.CompleteObservationNames())
	}
	return binding
}

type runtimeDependencies struct {
	memoryServices      *memoryservices.Client
	memoryServicesError string
	traceObserver       agenttrace.Observer
	templates           *rag.Store
	documents           *rag.DocumentStore
	encyclopedia        *rag.EncyclopediaStore
	cartridges          *rag.CartridgeStore
	attachments         *rag.AttachmentIndexManager
	artifacts           *rag.ArtifactLibrary
}

func loadRuntimeDependencies(configuredRoot string) (runtimeDependencies, error) {
	root := strings.TrimSpace(configuredRoot)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return runtimeDependencies{}, fmt.Errorf("resolve Meadow state directory: %w", err)
		}
		root = filepath.Join(home, ".meadow")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return runtimeDependencies{}, fmt.Errorf("resolve Meadow state directory: %w", err)
	}
	if err := secureDirectory(absoluteRoot); err != nil {
		return runtimeDependencies{}, err
	}
	templatesDir := filepath.Join(absoluteRoot, "templates")
	documentsDir := filepath.Join(absoluteRoot, "documents")
	encyclopediaDir := filepath.Join(absoluteRoot, "encyclopedia")
	cartridgesDir := filepath.Join(absoluteRoot, "cartridges")
	traceDir := filepath.Join(absoluteRoot, "traces")
	for _, directory := range []string{templatesDir, documentsDir, encyclopediaDir, cartridgesDir, traceDir} {
		if err := secureDirectory(directory); err != nil {
			return runtimeDependencies{}, err
		}
	}
	documents := rag.NewDocumentStore(documentsDir)
	var knowledge rag.AttachmentKnowledgeService
	if client, err := tools.NewMeadowKnowledgeClient(); err == nil {
		knowledge = client
	}
	artifacts, err := rag.NewArtifactLibrary(absoluteRoot, knowledge)
	if err != nil {
		return runtimeDependencies{}, fmt.Errorf("open artifact library: %w", err)
	}
	traceObserver, err := agenttrace.NewDirectoryObserver(traceDir)
	if err != nil {
		return runtimeDependencies{}, fmt.Errorf("open Meadow trace store: %w", err)
	}
	memoryClient, memoryErr := memoryservices.Load(absoluteRoot)
	memoryErrorText := ""
	if memoryErr != nil {
		memoryErrorText = memoryErr.Error()
	}
	return runtimeDependencies{
		memoryServices:      memoryClient,
		memoryServicesError: memoryErrorText,
		traceObserver:       traceObserver,
		artifacts:           artifacts,
		templates:           rag.Load(templatesDir),
		documents:           documents,
		encyclopedia:        rag.NewEncyclopediaStore(encyclopediaDir, documents, nil),
		cartridges:          rag.NewCartridgeStore(cartridgesDir, documents, nil),
		attachments:         rag.NewAttachmentIndexManager(filepath.Join(absoluteRoot, "attachment-rag"), knowledge),
	}, nil
}

// Configure capacity before publishing a client to a session/runtime. Generic
// test providers and transports without this capability keep their own limits.
func bindOutputTokenLimit(client agentcore.KernelModel, limit int) {
	if configurable, ok := client.(interface{ SetOutputTokenLimit(int) }); ok && limit > 0 {
		configurable.SetOutputTokenLimit(limit)
	}
}

func secureDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create private Meadow directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("protect private Meadow directory: %w", err)
	}
	return nil
}

func isGitRepository(root string) bool {
	info, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}

func runtimeErrorText(err error) string {
	if err == nil {
		return "Meadow Runtime is not ready"
	}
	return err.Error()
}
