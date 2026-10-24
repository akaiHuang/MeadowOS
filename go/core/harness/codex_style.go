package harness

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/rag"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/sessionrecovery"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	DefaultCodexStyleMaxTurns  = 48
	DefaultCodexStyleMaxTokens = 8192
	DefaultToolParallelism     = 4
)

type ToolHooks struct {
	OnToolCall     func(string, map[string]any)
	OnToolResult   func(string, tools.Result)
	OnToolStart    func(agent.ToolStart)
	OnToolComplete func(agent.ToolExecution)
}

type CodexStyleSpec struct {
	RuntimeProfile                  string
	BaseSystemPrompt                string
	MaxEmptyOutputContinuations     int
	RavenCheckpointDir              string
	RavenCuratorDir                 string
	Client                          agent.KernelModel
	ModelID                         string
	CWD                             string
	SystemPrompt                    string
	CacheSalt                       string
	ContextManager                  *agent.ContextManager
	TraceObserver                   agenttrace.Observer
	Registry                        *tools.Registry
	DisabledTools                   []string
	DocumentStore                   *rag.DocumentStore
	EvidenceIndex                   *rag.EvidenceIndex
	AttachmentIndex                 *rag.AttachmentIndex
	Embedder                        rag.Embedder
	DocumentIngestor                agent.DocumentIngestor
	Authorize                       agent.AuthorizeFunc
	RequireToolAccount              func(context.Context) error
	Hooks                           agent.KernelHooks
	ToolHooks                       ToolHooks
	MaxTurns                        int
	MaxTokens                       int
	ContextWindowTokens             int
	PromptTokenLimit                int
	EnableSequentialAdvisor         bool
	EnableEvidenceReconciliation    bool
	EnableMutationNarrationRecovery bool
	DisableRunHandoffReserve        bool
	HandoffReserve                  time.Duration
	HandoffMaxTokens                int
	DisableRevisionBoundEvidence    bool
	SessionID                       string
	Recovery                        sessionrecovery.Recorder
	ResumePlan                      *sessionrecovery.ResumePlan
	RecoveryRequired                bool
	RecoveryError                   error
	CoordinatorOnly                 bool
	Subtasks                        agent.SubtaskPolicy
	SubtaskStateDir                 string
}

type CodexStyleDependencies struct {
	RuntimeProfile               string
	CoordinatorOnly              bool
	Subtasks                     agent.SubtaskPolicy
	Client                       agent.KernelModel
	TraceObserver                agenttrace.Observer
	DocumentStore                *rag.DocumentStore
	AttachmentIndex              *rag.AttachmentIndex
	Embedder                     rag.Embedder
	MaxTurns                     int
	MaxTokens                    int
	EnableSequentialAdvisor      bool
	EnableEvidenceReconciliation bool
	DisableRunHandoffReserve     bool
	HandoffReserve               time.Duration
	HandoffMaxTokens             int
	DisableRevisionBoundEvidence bool
}

func (d CodexStyleDependencies) Bind(binding Binding, events Events) CodexStyleSpec {
	cacheSalt := binding.ChatID
	if cacheSalt == "" {
		cacheSalt = binding.SessionID
	}
	return CodexStyleSpec{
		RuntimeProfile:  d.RuntimeProfile,
		CoordinatorOnly: d.CoordinatorOnly,
		Subtasks:        d.Subtasks,
		Client:          d.Client, ModelID: binding.ModelID, CWD: binding.CWD,
		SystemPrompt: binding.SystemPrompt, CacheSalt: cacheSalt,
		ContextManager: binding.ContextManager,
		TraceObserver:  d.TraceObserver, Registry: binding.Registry, DisabledTools: binding.DisabledTools,
		DocumentStore: d.DocumentStore, Embedder: d.Embedder, AttachmentIndex: d.AttachmentIndex,
		Authorize: binding.Authorize, MaxTurns: d.MaxTurns, MaxTokens: d.MaxTokens,
		RequireToolAccount:           binding.RequireToolAccount,
		EnableSequentialAdvisor:      d.EnableSequentialAdvisor,
		EnableEvidenceReconciliation: d.EnableEvidenceReconciliation,
		DisableRunHandoffReserve:     d.DisableRunHandoffReserve,
		HandoffReserve:               d.HandoffReserve,
		HandoffMaxTokens:             d.HandoffMaxTokens,
		DisableRevisionBoundEvidence: d.DisableRevisionBoundEvidence,
		PromptTokenLimit:             binding.PromptTokenLimit,
		ContextWindowTokens:          binding.ContextWindowTokens,
		Hooks: agent.KernelHooks{
			OnMessageBoundary: events.OnMessageBoundary,
			OnContent:         events.OnContent, OnThought: events.OnThought,
			OnProgress: events.OnProgress, OnToolDraft: events.OnToolDraft,
			OnToolDraftProgress: events.OnToolDraftProgress,
			OnTurn:              events.OnTurn, OnGuidance: events.OnGuidance,
			OnCheckpoint: events.OnCheckpoint,
		},
		ToolHooks: ToolHooks{
			OnToolCall: events.OnToolCall, OnToolResult: events.OnToolResult,
			OnToolStart: events.OnToolStart, OnToolComplete: events.OnToolComplete,
		},
	}
}

func NewCodexStyleRuntime(spec CodexStyleSpec) (*agent.CodexStyleRuntime, error) {
	return agent.NewCodexStyleRuntime(codexStyleConfig(spec), spec.Hooks)
}

func codexStyleConfig(spec CodexStyleSpec) agent.CodexStyleConfig {
	maxTurns := spec.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultCodexStyleMaxTurns
	}
	maxTokens := spec.MaxTokens
	if maxTokens <= 0 {
		maxTokens = agent.DefaultCodexStyleOutputTokens(spec.Client, spec.ModelID)
	}
	promptTokenLimit := spec.PromptTokenLimit
	if spec.ContextWindowTokens > 0 {
		// The product's generic prompt bound reserves 8K. A native API may
		// advertise a larger output cap, so admit messages + tool schemas only
		// after reserving this request's actual output allowance. Never expand
		// an explicit prompt bound, or change the requested generation limit.
		effectiveOutput := maxTokens
		if capacity, ok := spec.Client.(interface{ OutputTokenLimit() int }); ok {
			if limit := capacity.OutputTokenLimit(); limit > 0 {
				effectiveOutput = min(effectiveOutput, limit)
			}
		}
		margin := modelmgr.RuntimeBudgetFor(spec.ContextWindowTokens).ProtocolMarginTokens
		availablePrompt := max(1, spec.ContextWindowTokens-effectiveOutput-margin)
		if promptTokenLimit <= 0 || promptTokenLimit > availablePrompt {
			promptTokenLimit = availablePrompt
		}
	}

	registry := spec.Registry
	if spec.DocumentStore != nil || spec.EvidenceIndex != nil {
		index := spec.EvidenceIndex
		if index == nil {
			index = rag.NewEvidenceIndex()
		}
		registry = rag.RegistryWithEvidenceIndex(registry, spec.DocumentStore, spec.Embedder, index)
	}
	if spec.AttachmentIndex != nil {
		registry = rag.RegistryWithAttachmentIndex(registry, spec.AttachmentIndex)
	}
	if len(spec.DisabledTools) > 0 {
		registry = registry.Excluding(spec.DisabledTools)
	}
	documentIngestor := spec.DocumentIngestor
	if documentIngestor == nil {
		documentIngestor = NewDocumentIngestor(spec.DocumentStore)
	}

	return agent.CodexStyleConfig{
		RuntimeProfile:              spec.RuntimeProfile,
		BaseSystemPrompt:            spec.BaseSystemPrompt,
		MaxEmptyOutputContinuations: spec.MaxEmptyOutputContinuations,
		RavenCheckpointDir:          spec.RavenCheckpointDir,
		RavenCuratorDir:             spec.RavenCuratorDir,
		Client:                      spec.Client,
		Executor: &agent.ToolExecutor{
			Registry:       registry,
			Authorize:      spec.Authorize,
			RequireAccount: spec.RequireToolAccount,
			Parallel:       true,
			MaxParallel:    DefaultToolParallelism,
			OnToolCall:     spec.ToolHooks.OnToolCall,
			OnToolResult:   spec.ToolHooks.OnToolResult,
			OnToolStart:    spec.ToolHooks.OnToolStart,
			OnToolComplete: spec.ToolHooks.OnToolComplete,
		},
		ModelID:                         spec.ModelID,
		CWD:                             spec.CWD,
		SystemPrompt:                    spec.SystemPrompt,
		CacheSalt:                       spec.CacheSalt,
		ContextManager:                  spec.ContextManager,
		DocumentIngestor:                documentIngestor,
		TraceObserver:                   spec.TraceObserver,
		MaxTurns:                        maxTurns,
		MaxTokens:                       maxTokens,
		PromptTokenLimit:                promptTokenLimit,
		EnableSequentialAdvisor:         spec.EnableSequentialAdvisor,
		EnableEvidenceReconciliation:    spec.EnableEvidenceReconciliation,
		EnableMutationNarrationRecovery: spec.EnableMutationNarrationRecovery,
		DisableRunHandoffReserve:        spec.DisableRunHandoffReserve,
		HandoffReserve:                  spec.HandoffReserve,
		HandoffMaxTokens:                spec.HandoffMaxTokens,
		DisableRevisionBoundEvidence:    spec.DisableRevisionBoundEvidence,
		SessionID:                       spec.SessionID, Recovery: spec.Recovery,
		ResumePlan: spec.ResumePlan, RecoveryRequired: spec.RecoveryRequired,
		RecoveryError:   spec.RecoveryError,
		CoordinatorOnly: spec.CoordinatorOnly,
		Subtasks:        spec.Subtasks, SubtaskStateDir: spec.SubtaskStateDir,
	}
}

type CodexStylePool struct {
	mu                sync.Mutex
	runtimes          map[string]*agent.CodexStyleRuntime
	runtimeProfiles   map[string]string
	evidenceIndexes   map[string]*rag.EvidenceIndex
	recoveries        map[string]*sessionrecovery.Store
	recoveryPlans     map[string]sessionrecovery.ResumePlan
	recoveryErrors    map[string]error
	recoveryAttempted map[string]bool
	closed            bool
}

func NewCodexStylePool() *CodexStylePool {
	return &CodexStylePool{
		runtimes:        make(map[string]*agent.CodexStyleRuntime),
		runtimeProfiles: make(map[string]string),
		evidenceIndexes: make(map[string]*rag.EvidenceIndex),
		recoveries:      make(map[string]*sessionrecovery.Store),
		recoveryPlans:   make(map[string]sessionrecovery.ResumePlan),
		recoveryErrors:  make(map[string]error), recoveryAttempted: make(map[string]bool),
	}
}

func (p *CodexStylePool) Runtime(key string, spec CodexStyleSpec) (*agent.CodexStyleRuntime, error) {
	if key == "" {
		return NewCodexStyleRuntime(spec)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("codex-style runtime pool is closed")
	}
	if spec.RecoveryRequired && spec.Recovery == nil {
		p.attachRecoveryLocked(key, &spec)
	}

	runtime := p.runtimes[key]
	// Runtime strategy state cannot cross profiles. Recovery keeps one journal
	// owner for the durable chat, even when an idle chat chooses a new profile.
	reusable := runtime != nil && p.runtimeProfiles[key] == spec.RuntimeProfile && runtime.MatchesBinding(spec.ModelID, spec.CWD)
	if spec.DocumentStore != nil || spec.EvidenceIndex != nil {
		index := p.evidenceIndexes[key]
		if !reusable || index == nil {
			index = rag.NewEvidenceIndex()
			p.evidenceIndexes[key] = index
		}
		spec.EvidenceIndex = index
	}
	config := codexStyleConfig(spec)
	if reusable {
		if err := runtime.Rebind(config, spec.Hooks); err != nil {
			return nil, err
		}
		return runtime, nil
	}

	runtime, err := agent.NewCodexStyleRuntime(config, spec.Hooks)
	if err != nil {
		return nil, err
	}
	p.runtimes[key] = runtime
	p.runtimeProfiles[key] = spec.RuntimeProfile
	return runtime, nil
}

// Close releases every session recovery writer owned by the pool. A pool is
// process-scoped, so closing it is terminal and idempotent; this prevents CLI
// sessions from retaining file descriptors and writer locks until process
// teardown happens to reclaim them.
func (p *CodexStylePool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	stores := make([]*sessionrecovery.Store, 0, len(p.recoveries))
	for _, store := range p.recoveries {
		if store != nil {
			stores = append(stores, store)
		}
	}
	p.runtimes = make(map[string]*agent.CodexStyleRuntime)
	p.runtimeProfiles = make(map[string]string)
	p.evidenceIndexes = make(map[string]*rag.EvidenceIndex)
	p.recoveries = make(map[string]*sessionrecovery.Store)
	p.recoveryPlans = make(map[string]sessionrecovery.ResumePlan)
	p.recoveryErrors = make(map[string]error)
	p.recoveryAttempted = make(map[string]bool)
	p.mu.Unlock()

	var closeErr error
	for _, store := range stores {
		closeErr = errors.Join(closeErr, store.Close())
	}
	return closeErr
}

func (p *CodexStylePool) RuntimeFor(
	binding Binding,
	dependencies CodexStyleDependencies,
	events Events,
) (*agent.CodexStyleRuntime, error) {
	key := binding.ChatID
	if key == "" {
		key = binding.SessionID
	}
	spec := dependencies.Bind(binding, events)
	spec.SessionID = key
	if spec.RuntimeProfile == agent.RuntimeProfileRavenCode && key != "" {
		identity := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
		spec.RavenCheckpointDir = filepath.Join(session.PrimaryDir(), "recovery", "raven-workspaces", identity)
		spec.RavenCuratorDir = filepath.Join(session.PrimaryDir(), "recovery", "raven-curator", identity)
	}
	if spec.Subtasks.Enabled && key != "" {
		identity := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
		spec.SubtaskStateDir = filepath.Join(session.PrimaryDir(), "subagents", identity)
	}
	spec.RecoveryRequired = true
	runtime, err := p.Runtime(key, spec)
	if err == nil && events.OnResumePlan != nil {
		p.mu.Lock()
		plan, ok := p.recoveryPlans[key]
		p.mu.Unlock()
		if ok {
			events.OnResumePlan(plan)
		}
	}
	return runtime, err
}

func (p *CodexStylePool) attachRecoveryLocked(key string, spec *CodexStyleSpec) {
	if store := p.recoveries[key]; store != nil {
		plan := p.recoveryPlans[key]
		spec.Recovery = store
		spec.ResumePlan = &plan
		return
	}
	if p.recoveryAttempted[key] {
		spec.RecoveryError = p.recoveryErrors[key]
		return
	}
	p.recoveryAttempted[key] = true
	root := filepath.Join(session.PrimaryDir(), "recovery", "harness")
	store, recovered, err := sessionrecovery.Open(root, key, sessionrecovery.DefaultOptions())
	if err != nil {
		p.recoveryErrors[key] = err
		spec.RecoveryError = err
		return
	}
	plan := sessionrecovery.BuildResumePlan(recovered)
	p.recoveries[key] = store
	p.recoveryPlans[key] = plan
	spec.Recovery = store
	spec.ResumePlan = &plan
}
