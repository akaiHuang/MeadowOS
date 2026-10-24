package harness

import (
	"github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"github.com/hey-meadow/meadow-code/go/core/hermes"
	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/rag"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

type FamilyProfile string

const (
	FamilyLFM25Research FamilyProfile = "lfm25-research"
	FamilyGemma12Agent  FamilyProfile = "gemma12-agent"
	FamilyQwen38Coding  FamilyProfile = "qwen38-coding"
	FamilyOrnithCoding  FamilyProfile = "ornith-coding"
	FamilyOffice35      FamilyProfile = "office35"
	FamilyMuseGlimmer   FamilyProfile = "muse-glimmer"
)

type FamilyRuntimeSpec struct {
	Binding           Binding
	Client            agent.KernelModel
	TraceObserver     agenttrace.Observer
	Workspace         *tools.Workspace
	SelfIteration     string
	SemanticPreflight string
	WorkState         string
	UseRouter         bool
	ModelIntent       bool
	IntentPredictor   hermes.SemanticIntentPredictor
	RAG               *rag.Store
	DocumentStore     *rag.DocumentStore
	Encyclopedia      *rag.EncyclopediaStore
	Cartridges        *rag.CartridgeStore
	CartridgeScope    rag.CartridgeScope
	Embedder          rag.Embedder
	RAGTopK           int
	RAGSourceIDs      []string
	SubtaskStateDir   string
}

func DefaultFamilyProfile(modelID string) FamilyProfile {
	if info, ok := modelmgr.ModelByName(modelID); ok {
		modelID = info.ServedName
	}
	switch modelID {
	case "Meadow Lightning⚡️":
		return FamilyLFM25Research
	case "Meadow 12B", "Meadow 26B":
		return FamilyGemma12Agent
	case "Meadow 27B":
		return FamilyQwen38Coding
	case "Meadow 30B":
		return FamilyMuseGlimmer
	case "Meadow Coder 35B":
		return FamilyOrnithCoding
	default:
		return ""
	}
}

func NewFamilyRuntime(profile FamilyProfile, spec FamilyRuntimeSpec, events Events) (agent.Runtime, error) {
	binding := spec.Binding
	parentSessionID := binding.ChatID
	if parentSessionID == "" {
		parentSessionID = binding.SessionID
	}
	hooks := hermesHooks(events)

	switch profile {
	case FamilyLFM25Research:
		return hermes.NewLFM25ResearchRuntime(hermes.LFM25ResearchSpec{
			Client: spec.Client, ModelID: binding.ModelID,
			SystemPrompt: binding.SystemPrompt, CWD: binding.CWD,
			ContextManager: binding.ContextManager, TraceObserver: spec.TraceObserver,
			Registry: binding.Registry, Workspace: spec.Workspace,
			RAG: spec.RAG, DocumentStore: spec.DocumentStore, Encyclopedia: spec.Encyclopedia,
			Cartridges: spec.Cartridges, CartridgeScope: spec.CartridgeScope,
			Embedder: spec.Embedder, RAGTopK: spec.RAGTopK, RAGSourceIDs: spec.RAGSourceIDs,
			ParentSessionID: parentSessionID, AskUser: binding.AskUser,
			Authorize: binding.Authorize, ModelIntent: spec.ModelIntent,
			RequireToolAccount: binding.RequireToolAccount,
		}, hooks)
	case FamilyGemma12Agent:
		return hermes.NewGemma12AgentRuntime(hermes.Gemma12AgentSpec{
			Client: spec.Client, ModelID: binding.ModelID,
			SelfIteration: spec.SelfIteration, WorkState: spec.WorkState,
			SystemPrompt: binding.SystemPrompt, CWD: binding.CWD,
			ContextManager: binding.ContextManager, TraceObserver: spec.TraceObserver,
			Registry: binding.Registry, Workspace: spec.Workspace,
			UseRouter: spec.UseRouter, ModelIntent: spec.ModelIntent,
			RAG: spec.RAG, DocumentStore: spec.DocumentStore,
			Cartridges: spec.Cartridges, CartridgeScope: spec.CartridgeScope,
			RAGTopK: spec.RAGTopK, RAGSourceIDs: spec.RAGSourceIDs,
			SubtaskStateDir: spec.SubtaskStateDir, ParentSessionID: parentSessionID,
			AskUser: binding.AskUser, Authorize: binding.Authorize,
			RequireToolAccount: binding.RequireToolAccount,
		}, hooks)
	case FamilyQwen38Coding:
		return hermes.NewQwen38CodingRuntime(hermes.Qwen38CodingSpec{
			Client: spec.Client, ModelID: binding.ModelID,
			SelfIteration: spec.SelfIteration, WorkState: spec.WorkState,
			SystemPrompt: binding.SystemPrompt, CWD: binding.CWD,
			ContextManager: binding.ContextManager, TraceObserver: spec.TraceObserver,
			Registry: binding.Registry, Workspace: spec.Workspace,
			UseRouter: spec.UseRouter, ModelIntent: spec.ModelIntent,
			IntentPredictor: spec.IntentPredictor,
			RAG:             spec.RAG, DocumentStore: spec.DocumentStore,
			Cartridges: spec.Cartridges, CartridgeScope: spec.CartridgeScope,
			RAGTopK: spec.RAGTopK, RAGSourceIDs: spec.RAGSourceIDs,
			SubtaskStateDir: spec.SubtaskStateDir, ParentSessionID: parentSessionID,
			AskUser: binding.AskUser, Authorize: binding.Authorize,
			RequireToolAccount: binding.RequireToolAccount,
		}, hooks)
	case FamilyOrnithCoding:
		return hermes.NewOrnithCodingRuntime(hermes.OrnithCodingSpec{
			Client: spec.Client, ModelID: binding.ModelID,
			IntentPredictor: spec.IntentPredictor,
			SelfIteration:   spec.SelfIteration, WorkState: spec.WorkState,
			SystemPrompt: binding.SystemPrompt, CWD: binding.CWD,
			ContextManager: binding.ContextManager, TraceObserver: spec.TraceObserver,
			PromptTokenLimit: binding.PromptTokenLimit,
			Registry:         binding.Registry, Workspace: spec.Workspace, UseRouter: spec.UseRouter,
			SubtaskStateDir: spec.SubtaskStateDir, ParentSessionID: parentSessionID,
			AskUser: binding.AskUser, Authorize: binding.Authorize,
			RequireToolAccount: binding.RequireToolAccount,
		}, hooks)
	case FamilyOffice35:
		return hermes.NewOffice35Runtime(hermes.Office35Spec{
			Client: spec.Client, ModelID: binding.ModelID,
			SelfIteration: spec.SelfIteration, SemanticPreflight: spec.SemanticPreflight,
			WorkState: spec.WorkState, SystemPrompt: binding.SystemPrompt, CWD: binding.CWD,
			ContextManager: binding.ContextManager, TraceObserver: spec.TraceObserver,
			Registry: binding.Registry, Workspace: spec.Workspace,
			UseRouter: spec.UseRouter, ModelIntent: spec.ModelIntent,
			RAG: spec.RAG, DocumentStore: spec.DocumentStore,
			Cartridges: spec.Cartridges, CartridgeScope: spec.CartridgeScope,
			RAGTopK: spec.RAGTopK, RAGSourceIDs: spec.RAGSourceIDs,
			SubtaskStateDir: spec.SubtaskStateDir, ParentSessionID: parentSessionID,
			AskUser: binding.AskUser, Authorize: binding.Authorize,
			RequireToolAccount: binding.RequireToolAccount,
		}, hooks)
	case FamilyMuseGlimmer:
		return hermes.NewMuseGlimmerRuntime(hermes.MuseGlimmerSpec{
			Client: spec.Client, ModelID: binding.ModelID,
			SelfIteration: spec.SelfIteration, WorkState: spec.WorkState,
			SystemPrompt: binding.SystemPrompt, CWD: binding.CWD,
			ContextManager: binding.ContextManager, TraceObserver: spec.TraceObserver,
			Registry: binding.Registry, Workspace: spec.Workspace,
			UseRouter: spec.UseRouter, ModelIntent: spec.ModelIntent,
			RAG: spec.RAG, DocumentStore: spec.DocumentStore,
			Cartridges: spec.Cartridges, CartridgeScope: spec.CartridgeScope,
			RAGTopK: spec.RAGTopK, RAGSourceIDs: spec.RAGSourceIDs,
			SubtaskStateDir: spec.SubtaskStateDir, ParentSessionID: parentSessionID,
			AskUser: binding.AskUser, Authorize: binding.Authorize,
			RequireToolAccount: binding.RequireToolAccount,
		}, hooks)
	default:
		return nil, nil
	}
}

func hermesHooks(events Events) hermes.Hooks {
	return hermes.Hooks{
		OnMessageBoundary: events.OnMessageBoundary,
		OnContent:         events.OnContent, OnThought: events.OnThought,
		OnProgress: events.OnProgress, OnToolDraft: events.OnToolDraft,
		OnToolDraftProgress: events.OnToolDraftProgress,
		OnToolCall:          events.OnToolCall, OnToolResult: events.OnToolResult,
		OnToolStart: events.OnToolStart, OnToolComplete: events.OnToolComplete,
		OnTurn: events.OnTurn, OnGuidance: events.OnGuidance,
	}
}
