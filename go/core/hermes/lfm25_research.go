package hermes

import (
	"context"
	"errors"
	"net/url"
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
	LFM25ResearchProfileID = "lfm25-research-v0"

	lfm25HistoryMaxBytes             = 12 * 1024
	lfm25ToolResultMaxBytes          = 4 * 1024
	lfm25DuplicateToolResultMinBytes = 256
	lfm25DocumentIngestionTokens     = 8_000
	lfm25DocumentReadMaxLines        = 120
)

var (
	lfm25KnowledgeQuestionRe = regexp.MustCompile(`(?i)(?:什麼|什么|為什麼|为什么|如何|怎麼|怎么|哪些|哪裡|哪里|何時|何时|誰|谁|多少|解釋|解释|比較|比较|介紹|介绍|查詢|查询|查證|查证|核實|核实|what|why|how|which|where|when|who|explain|compare|look up|verify|research)`)
	lfm25NoLookupTaskRe      = regexp.MustCompile(`(?i)(?:你好|嗨|哈囉|哈啰|謝謝|谢谢|晚安|早安|陪我聊|聊聊天|翻譯|翻译|改寫|改写|潤飾|润色|摘要以下|整理以下|寫一首|写一首|寫故事|写故事|幫我想|帮我想|冰箱|食材|購物清單|购物清单|算一下|計算|计算|hello|hi|thanks|chat with me|translate|rewrite|summarize the following|write (?:a )?(?:poem|story)|brainstorm|calculate)`)
	lfm25LocalDataRe         = regexp.MustCompile(`(?i)(?:以下|如下|這段|这段|上面|附件|貼上|贴上|我提供|given (?:text|data)|following (?:text|data)|attached)`)
	lfm25LocalPlanningRe     = regexp.MustCompile(`(?i)(?:幫我|帮我|請|请|替我).{0,24}(?:規劃|规划|計畫|计划|安排|排程|排序|縮成|缩成)|(?:今天|本週|本周|這週|这周).{0,24}(?:做不完|行程|待辦|待办|最重要)|(?:週|周|星期)[一二三四五六日天1-7].{0,12}(?:怎麼排|怎么排|安排|行程)|(?:重新|再).{0,12}(?:安排|規劃|规划|排程)`)
	lfm25PersonalContextRe   = regexp.MustCompile(`(?i)(?:我(?:明天|今天|現在|现在|剛|刚|最近|很|一直|要跟|想跟|需要跟)|我的(?:主管|同事|老師|老师|家人|朋友|考試|考试|工作|功課|功课|行程|計畫|计划)|緊張|紧张|難過|难过|沮喪|沮丧|害怕|拖延|睡不著|睡不着|打工|讀書計畫|读书计划)`)
	lfm25ContextFollowupRe   = regexp.MustCompile(`(?i)^(?:你剛剛|你刚刚|剛才|刚才|那|那麼|那么|可是|但是|所以|再|重新|回顧|回顾|接著|接着|明天|今天|現在|现在|只用|一句|簡短|简短|前面|上面|這個|这个|它|他|她|they\b|it\b|that\b|but\b|so\b|then\b|tomorrow\b)`)
	// Lightning is a research and conversational worker, not a file/coding
	// agent. Keep this boundary deliberately narrow: ordinary planning verbs
	// such as "幫我規劃讀書" must remain tool-free conversation, while an
	// explicit request to create/edit/run a software artifact is rejected
	// before a small model can narrate a mutation it cannot perform.
	lfm25UnsupportedChineseActionRe = regexp.MustCompile(`(?i)(?:(?:請|请|請你|请你|幫我|帮我|替我|直接)\s*)?(?:建立|创建|創建|開發|开发|製作|制作|寫入|写入|寫一個|写一个|修改|編輯|编辑|儲存|储存|保存|執行|执行|運行|运行).{0,64}(?:檔案|文件|程式|程序|遊戲|游戏|網站|网页|網頁|app\b|html\b|css\b|javascript\b|typescript\b|python\b|程式碼|代码|code\b|script\b|file\b|project\b|terminal\b|shell\b|command\b)`)
	lfm25UnsupportedEnglishActionRe = regexp.MustCompile(`(?i)(?:^|[.!?]\s*|\b(?:please|can you|could you|would you|help me(?: to)?|i need you to)\s+)(?:build|create|develop|implement|edit|save|write|run|execute)\b.{0,64}\b(?:app|html|css|javascript|typescript|python|code|script|file|project|terminal|shell|command)\b`)
)

func lfm25UnsupportedAction(task string) bool {
	stripped := strings.TrimSpace(router.StripSystemReminder(task))
	return lfm25UnsupportedChineseActionRe.MatchString(stripped) ||
		lfm25UnsupportedEnglishActionRe.MatchString(stripped)
}

// lfm25ConversationContinuation prevents a short context-dependent follow-up
// from being reclassified as open-web research merely because it contains a
// broad question word such as "怎麼". It is intentionally history-aware: a
// follow-up to an earlier factual research question still researches, while a
// correction inside personal planning or support remains tool-free.
func lfm25ConversationContinuation(task string, history []provider.Message) bool {
	stripped := strings.TrimSpace(router.StripSystemReminder(task))
	if stripped == "" || positiveNetworkRe.MatchString(stripped) || noNetworkRe.MatchString(stripped) {
		return false
	}
	revision := agentcore.DetectConversationRevision(stripped, history)
	if !revision.Detected && !lfm25ContextFollowupRe.MatchString(stripped) {
		return false
	}
	priorQuestion := revision.PriorQuestion
	if priorQuestion == "" {
		skippedCurrent := false
		for index := len(history) - 1; index >= 0; index-- {
			if history[index].Role == "user" {
				candidate := strings.TrimSpace(provider.MessageText(history[index]))
				if !skippedCurrent && candidate == stripped {
					skippedCurrent = true
					continue
				}
				priorQuestion = candidate
				if priorQuestion != "" {
					break
				}
			}
		}
	}
	if priorQuestion == "" || positiveNetworkRe.MatchString(priorQuestion) || lfm25ResearchEligible(priorQuestion) {
		return false
	}
	return lfm25ConversationEligible(priorQuestion)
}

// lfm25ResearchEligible makes the product distinction the 2.6B model cannot
// reliably make by itself: conversational/transform tasks stay tool-free;
// factual questions enter cache-first research even when the user did not know
// to say "search the web". A reviewed encyclopedia miss then falls through to
// the existing Search RAG + Meadow Browser loop.
func lfm25ResearchEligible(task string) bool {
	stripped := strings.TrimSpace(router.StripSystemReminder(task))
	if stripped == "" {
		return false
	}
	assessment := safety.Assess(stripped)
	if assessment.Category != safety.CategoryOrdinary {
		return false
	}
	if lfm25UnsupportedAction(stripped) {
		return false
	}
	// Personal planning is grounded in the conversation, not external facts.
	// Check it before broad research cues such as "今天", which otherwise turn
	// a simple schedule revision into an unnecessary web task.
	if lfm25LocalPlanningRe.MatchString(stripped) && !positiveNetworkRe.MatchString(stripped) {
		return false
	}
	if lfm25PersonalContextRe.MatchString(stripped) && !positiveNetworkRe.MatchString(stripped) {
		return false
	}
	if researchLoopEligible(task) {
		return true
	}
	if noNetworkRe.MatchString(stripped) || hasExplicitLocalInput(stripped) ||
		lfm25NoLookupTaskRe.MatchString(stripped) || lfm25LocalDataRe.MatchString(stripped) ||
		lfm25LocalPlanningRe.MatchString(stripped) {
		return false
	}
	return lfm25KnowledgeQuestionRe.MatchString(stripped)
}

func lfm25ConversationEligible(task string) bool {
	stripped := strings.TrimSpace(router.StripSystemReminder(task))
	if stripped == "" || lfm25UnsupportedAction(stripped) {
		return false
	}
	assessment := safety.Assess(stripped)
	if assessment.Category == safety.CategoryEmotional || assessment.Category == safety.CategoryAbuse ||
		assessment.Category == safety.CategorySafetySupport || assessment.GuardFinal {
		return true
	}
	// The general router treats words such as "規劃" as agentic because larger
	// Meadow models may act on a workspace. Lightning has no mutation tools;
	// every admitted non-research request is therefore ordinary conversation.
	return !lfm25ResearchEligible(stripped)
}

// LFM25ResearchSpec composes the small-model research worker shared by CLI HG
// and Desktop. It intentionally has no coding/mutation controller fields.
type LFM25ResearchSpec struct {
	Client             agentcore.KernelModel
	ModelID            string
	SystemPrompt       string
	CWD                string
	ContextManager     *agentcore.ContextManager
	TraceObserver      agenttrace.Observer
	Registry           *tools.Registry
	Workspace          *tools.Workspace
	RAG                *rag.Store
	DocumentStore      *rag.DocumentStore
	Encyclopedia       *rag.EncyclopediaStore
	Cartridges         *rag.CartridgeStore
	CartridgeScope     rag.CartridgeScope
	Embedder           rag.Embedder
	RAGTopK            int
	RAGSourceIDs       []string
	AskUser            tools.Asker
	Authorize          agentcore.AuthorizeFunc
	RequireToolAccount func(context.Context) error
	ParentSessionID    string
	ModelIntent        bool
}

func lfm25TaskProfile(task string, route agentcore.TurnRoute) agentcore.TaskProfile {
	profile := baseTaskProfile(task, route)
	profile.MaxConsecutiveFailures = 3
	// The ResearchStateLedger is the authoritative completion controller for
	// this profile. It already verifies discovery, opened sources, RAG cards,
	// grounded claims and final citations. The generic task controller sees
	// early rejected candidates as tool failures; leaving it enabled after
	// the ledger closes incorrectly rejects a valid cited synthesis as
	// "unfinished_after_tool_failure".
	if route.Intent == string(router.IntentInternetResearch) ||
		route.Intent == string(router.IntentResearchArtifact) {
		profile.Enabled = false
	}
	profile.EvidenceTools = []string{
		router.ToolWebFetch, "meadow_browser_dom", "pdf_extract", rag.EncyclopediaSearchToolName,
		rag.DocumentSearchToolName, rag.DocumentReadToolName,
	}
	return profile
}

// NewLFM25ResearchRuntime builds an evidence-first pipeline rather than a
// smaller copy of the general Agent harness.
func NewLFM25ResearchRuntime(spec LFM25ResearchSpec, hooks Hooks) (agentcore.Runtime, error) {
	if spec.Client == nil {
		return nil, errors.New("LFM2.5 research requires a model client")
	}
	if spec.Registry == nil || spec.Workspace == nil {
		return nil, errors.New("LFM2.5 research requires a workspace tool registry")
	}
	if spec.ContextManager == nil {
		spec.ContextManager = agentcore.NewContextManager(agentcore.ContextPolicy{})
	}
	registry := tools.LFM25ResearchRegistry(spec.Registry, spec.Workspace)
	registry.Register(&tools.ResearchPublish{WS: spec.Workspace})
	if spec.DocumentStore != nil {
		for _, name := range []string{router.ToolWebFetch, "meadow_browser_dom", "pdf_extract"} {
			if sourceTool, ok := registry.Get(name); ok {
				registry.Register(&rag.EvidenceIngestTool{
					Tool: sourceTool, Store: spec.DocumentStore,
					Cartridges: spec.Cartridges, Scope: spec.CartridgeScope,
				})
			}
		}
		registry.Register(&rag.DocumentSearchTool{Store: spec.DocumentStore, Embedder: spec.Embedder})
		registry.Register(&rag.DocumentReadTool{Store: spec.DocumentStore, MaxLines: lfm25DocumentReadMaxLines})
	}
	if spec.Encyclopedia != nil {
		registry.Register(&rag.EncyclopediaSearchTool{Store: spec.Encyclopedia})
	}
	registry.Register(&tools.AskUser{Ask: spec.AskUser})
	registerCartridgeMemoryTools(registry, spec.Cartridges, spec.CartridgeScope, spec.AskUser)
	authorize := spec.Authorize
	if authorize == nil {
		authorize = safeProductAuthorizer
	}
	contextManager := spec.ContextManager.
		WithoutMemory().
		WithHistoryBounds(lfm25HistoryMaxBytes, 4).
		WithToolResultBounds(lfm25ToolResultMaxBytes, lfm25ToolResultMaxBytes).
		WithDuplicateToolResultBounds(lfm25DuplicateToolResultMinBytes)

	var retriever agentcore.ContextRetriever
	if spec.RAG != nil {
		retriever = kernelRetriever{
			store: spec.RAG, topK: spec.RAGTopK, profile: LFM25ResearchProfileID,
			includeSourceIDs: append([]string(nil), spec.RAGSourceIDs...),
		}
	}
	retriever = withCartridgeRetriever(retriever, spec.Cartridges, LFM25ResearchProfileID, spec.CartridgeScope)
	if retriever != nil {
		retriever = lfm25RouteRetriever{delegate: retriever}
	}

	turnRouter := agentcore.TurnRouter(lfm25ResearchRouter{})
	if spec.ModelIntent {
		turnRouter = newModelIntentRouter(spec.Client, spec.ModelID, LFM25ResearchProfileID)
	}
	kernel, err := agentcore.NewKernelRuntime(agentcore.KernelConfig{
		Client:            spec.Client,
		ModelID:           spec.ModelID,
		SystemPrompt:      prompt.ForLFM25Research(spec.SystemPrompt),
		RouteSystemPrompt: lfm25RouteSystemPrompt,
		CWD:               spec.CWD,
		Workspace:         spec.Workspace,
		ContextManager:    contextManager,
		// Lightning uses the same model-owned intent contract as larger profiles.
		TurnRouter:       turnRouter,
		ContextRetriever: retriever,
		DocumentIngestor: documentIngestor{
			store: spec.DocumentStore, minTokens: lfm25DocumentIngestionTokens,
		},
		TraceObserver: spec.TraceObserver,
		ConversationSafety: func(task string, history []provider.Message) agentcore.ConversationSafetyDecision {
			prior := make([]string, 0, len(history))
			for _, message := range history {
				if message.Role == "user" {
					prior = append(prior, provider.MessageText(message))
				}
			}
			assessment := safety.AssessConversation(task, prior)
			guarded, guardedFinal := safety.GuardedResponse(assessment, "")
			if !guardedFinal {
				guarded = ""
			}
			return agentcore.ConversationSafetyDecision{
				Category: string(assessment.Category), Severity: string(assessment.Severity),
				Guidance: safety.Guidance(assessment), DisableTools: assessment.DisableTools,
				GuardedResponse: guarded, GuardModelText: safety.GuardModelPublicNumbers,
			}
		},
		TaskProfile:                lfm25TaskProfile,
		ModelLed:                   true,
		NativeAutoToolChoice:       true,
		NativeCheckpointToolChoice: true,
		FreshNarrowedToolRecovery:  true,
		IncludeStreamingUsage:      true,
		// Research must always return a model-authored, citation-bearing answer.
		// Artifact fast-final text cannot substitute for synthesis or source gates.
		VerifiedFastFinal: false,
		Research: agentcore.ResearchPolicy{
			Enabled: true, Eligible: lfm25ResearchEligible, ModelRouted: spec.ModelIntent,
			MinOpenedSources: 2, MaxSourceOpensPerTurn: 1, MaxOpenedSources: 6,
			RequireDiscovery:              true,
			RequireFinalCitations:         true,
			RequireRAGBeforeClaims:        true,
			RequireArticleSources:         true,
			AutoExecuteRAGSearch:          true,
			AutoOpenDiscoveryArticles:     true,
			AutoExecuteDiscoverySearch:    true,
			MaxDiscoveryAttemptsPerTurn:   2,
			RAGEvidenceCards:              true,
			AutoBindRAGEvidenceCards:      true,
			IncrementalOnly:               true,
			RequireAudienceProfile:        true,
			AutoExecuteEncyclopediaSearch: true,
			SearchRAGCards:                true,
			SingleInitialDiscoverySearch:  true,
			AllowedSourceURL:              lfm25AllowedResearchSourceURL,
			DiscoveryQuerySuffix:          lfm25DiscoveryQuerySuffix,
			PublishTools:                  []string{tools.ResearchPublishToolName},
		},
		Subtasks: agentcore.SubtaskPolicy{Enabled: false},
		MaxTurns: 18,
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
	})
	if err != nil {
		return nil, err
	}
	return &lfm25ResearchRuntime{KernelRuntime: kernel, modelID: spec.ModelID}, nil
}

type lfm25RouteRetriever struct {
	delegate agentcore.ContextRetriever
}

func (r lfm25RouteRetriever) Retrieve(ctx context.Context, request agentcore.RetrievalRequest) (agentcore.RetrievedContext, error) {
	// A prior verified artifact is useful evidence during research or repair, but
	// it is actively harmful on the tiny model's tool-free capability boundary:
	// the model sees an old file and narrates that it will inspect the workspace
	// instead of giving the user a fresh draft. The 30B continuation can recover
	// that project evidence after the explicit handoff.
	if request.Intent == "capability_guidance" || r.delegate == nil {
		return agentcore.RetrievedContext{}, nil
	}
	return r.delegate.Retrieve(ctx, request)
}

func lfm25RouteSystemPrompt(route agentcore.TurnRoute) string {
	if route.Intent != "capability_guidance" {
		return ""
	}
	// The broad research/agent prompt is deliberately culled for this one
	// tool-free boundary turn. On a 2.6B model, retaining the general agent
	// identity makes it narrate a future workspace inspection even though no
	// tools are available. This compact contract keeps authorship with the model
	// while requiring a useful present-tense answer before Desktop offers 30B.
	return `You are Meadow Lightning, a small local assistant. Answer the user's current request directly in this response.

If the user asks to create an app, file, or program, give useful material now: include a minimal self-contained runnable draft in a fenced code block and a short usage note. Do not begin with promises such as "I will create", "let me inspect", or "first I will check".

You have no file or execution tools in this turn. Never claim that you saved, ran, tested, or verified the draft. State that limitation in one short sentence after the useful answer. Do not emit tool-call syntax and do not refuse the request merely because it would benefit from a larger model.`
}

// lfm25ResearchRuntime preserves the shared Kernel and guidance interfaces
// while adding one product boundary: Lightning answers first, then offers a
// transparent handoff when the requested work needs file mutation, execution,
// or a longer agent loop. A capability boundary is product guidance, not a
// transport error.
type lfm25ResearchRuntime struct {
	*agentcore.KernelRuntime
	modelID string
}

func (r *lfm25ResearchRuntime) RunInput(ctx context.Context, input agentcore.RunInput) (*agentcore.RunResult, error) {
	result, err := r.KernelRuntime.RunInput(ctx, input)
	if result != nil {
		result.Escalation = lfm25Escalation(r.modelID, input, result)
	}
	return result, err
}

func lfm25Escalation(modelID string, input agentcore.RunInput, result *agentcore.RunResult) *agentcore.ModelEscalation {
	if result == nil || result.Stop == agentcore.StopCancelled || result.Stop == agentcore.StopNeedsInput {
		return nil
	}
	task := strings.TrimSpace(provider.MessageText(input.User))
	// For agent/file requests Lightning still gives the best useful chat answer
	// it can, but always offers the user a one-click continuation on 30B. This is
	// intentionally allowed after StopCompleted: the answer and the executable
	// task are different capability levels.
	if lfm25UnsupportedAction(task) {
		return &agentcore.ModelEscalation{
			SourceModel:      strings.TrimSpace(modelID),
			RecommendedModel: "Meadow 30B",
			Reason:           "outside_research_worker_scope",
			ResumePrompt:     "請接手上一個任務並實際建立、儲存與驗證需要的檔案。先重用 Lightning 已提供的內容與目前對話限制，不要從頭重複詢問。",
		}
	}
	if result.Stop == agentcore.StopCompleted {
		return nil
	}
	reason := "research_not_completed"
	return &agentcore.ModelEscalation{
		SourceModel:      strings.TrimSpace(modelID),
		RecommendedModel: "Meadow 30B",
		Reason:           reason,
		ResumePrompt:     "請接手並完成上一個未完成的任務。先重用對話中已收集的證據與來源，只補足缺口；重新規劃、查證並產出完整結果。",
	}
}

type lfm25ResearchRouter struct{}

func (lfm25ResearchRouter) Route(ctx context.Context, request agentcore.TurnRouteRequest) (agentcore.TurnRoute, error) {
	route, err := (advisoryRouter{}).Route(ctx, request)
	if err != nil {
		return route, err
	}
	if provider.EstimateTextTokens(request.Task) >= lfm25DocumentIngestionTokens {
		route.Mode = string(router.TurnModeAgent)
		route.DisableTools = false
		route.Tools = selectTools(request.AvailableTools, map[string]bool{
			rag.DocumentSearchToolName: true, rag.DocumentReadToolName: true,
			tools.AskUserToolName: true,
		})
		return route, nil
	}
	if conversationOnlyPreferenceActive(request.Task, request.Messages) {
		return applyConversationOnlyRoute(route), nil
	}
	assessment := safety.Assess(router.StripSystemReminder(request.Task))
	if assessment.Category == safety.CategoryEmotional || assessment.Category == safety.CategoryAbuse ||
		assessment.Category == safety.CategorySafetySupport {
		route.Mode = string(router.TurnModeChat)
		route.Intent = "conversation_support"
		route.Tools = nil
		route.DisableTools = true
		return route, nil
	}
	if lfm25ConversationContinuation(request.Task, request.Messages) {
		route.Mode = string(router.TurnModeChat)
		route.Intent = "conversation_revision"
		route.Tools = nil
		route.DisableTools = true
		route.Guidance = append(route.Guidance,
			"This is a context-dependent follow-up to a personal planning or support conversation, not a request for external research. Re-read the prior user facts and answer the newest request directly without browsing.",
		)
		return route, nil
	}
	if lfm25UnsupportedAction(request.Task) {
		route.Mode = string(router.TurnModeChat)
		route.Intent = "capability_guidance"
		// This boundary is a short user-visible answer, not a planning/tool
		// turn. LFM2.5 otherwise spends the whole response in its private
		// thinking channel and can finish with zero visible bytes. Final phase
		// explicitly closes thinking in ApplyLFM25SafetyOverrides so Lightning
		// actually returns the useful draft before Desktop offers 30B.
		route.SamplingPhase = provider.SamplingPhaseFinal
		route.Tools = nil
		route.DisableTools = true
		route.Guidance = append(route.Guidance,
			"Answer first with the most useful result you can produce without tools: a concise plan, a runnable code draft, or exact next steps. Do not refuse merely because the user requested a file or app.",
			"Return the useful material now. Do not begin with future-action narration such as 'I will create', 'let me check', or 'next I will'. For a coding request, include a minimal self-contained fenced code example plus one sentence explaining that it is a draft and has not been saved or run.",
			"You cannot write, run, or verify files in Meadow Lightning. Never claim that you created, saved, executed, or validated anything. The product will separately offer Meadow 30B for those actions, so do not emit tool-call syntax.",
		)
		return route, nil
	}
	if !lfm25ResearchEligible(request.Task) {
		if lfm25ConversationEligible(request.Task) {
			route.Mode = string(router.TurnModeChat)
			route.Intent = "general_conversation"
			route.Tools = nil
			route.DisableTools = true
			route.Guidance = append(route.Guidance,
				"Answer the user's current conversational or planning request directly. Do not emit tool-call syntax or claim to have changed files or external state.",
				"Before answering, re-read the user messages as a constraint ledger: preserve each fact, priority, prohibition, availability window, duration, and later correction with the same polarity. A weakness is not a prohibition; a priority is not something to avoid; a duration is not a clock-time offset. A later turn changes only the fields it explicitly revises.",
			)
		}
		return route, nil
	}
	route.Intent = string(router.IntentInternetResearch)
	route.Mode = string(router.TurnModeAgent)
	route.DisableTools = false
	allowedTools := map[string]bool{
		router.ToolWebSearch: true, router.ToolWebFetch: true,
		"meadow_browser_dom": true, "pdf_extract": true, rag.EncyclopediaSearchToolName: true, rag.DocumentSearchToolName: true,
		rag.DocumentReadToolName: true, tools.AskUserToolName: true,
	}
	if lfm25RequiresMeadowBrowserDOM(request.Task) {
		// An explicit product-tool instruction is part of the user's requested
		// evidence provenance, not a stylistic preference. Search can still
		// discover candidates, but actual remote pages must be opened through MB.
		delete(allowedTools, router.ToolWebFetch)
		delete(allowedTools, rag.EncyclopediaSearchToolName)
	}
	route.Tools = selectTools(request.AvailableTools, allowedTools)
	route.Guidance = append(route.Guidance,
		"Search results are candidates only. Open one source at a time, retain its exact URL, and do not answer factual claims from snippets alone.",
		"Use only the source types and source count already specified by the user; do not ask them to choose again.",
		"Keep the user's audience and concrete use case in every discovery query. A requested publisher, institution, or domain is only a provenance filter, not the subject of the answer; do not replace a household, student, or work task with an article about that institution itself.",
	)
	if lfm25RequiresMeadowBrowserDOM(request.Task) {
		route.Guidance = append(route.Guidance,
			"The user explicitly selected Meadow Browser. Use meadow_browser_dom for every remote article opening; web_fetch and reviewed encyclopedia entries do not satisfy this run's requested browsing evidence.",
		)
	}
	if lfm25RequiresTaiwanPublicInstitutionSources(request.Task) {
		route.Guidance = append(route.Guidance,
			"The user requires Taiwan government or public education institution articles. Focus discovery on official .gov.tw and .edu.tw pages and do not spend an opening slot on a blog, tutoring company, news outlet, forum, or commercial education site.",
		)
	}
	// A 2.6B model is more stable with deterministic extraction than broad
	// creative sampling. Sampling details remain a model-side decision.
	route.SamplingPhase = provider.SamplingPhasePrecision
	return route, nil
}

func lfm25RequiresMeadowBrowserDOM(task string) bool {
	lower := strings.ToLower(task)
	return strings.Contains(lower, "meadow browser") &&
		(strings.Contains(lower, "開啟") || strings.Contains(lower, "打開") ||
			strings.Contains(lower, "open") || strings.Contains(lower, "透過") ||
			strings.Contains(lower, "through") || strings.Contains(lower, "using"))
}

func lfm25RequiresTaiwanPublicInstitutionSources(task string) bool {
	lower := strings.ToLower(task)
	return (strings.Contains(task, "台灣") || strings.Contains(task, "台湾") || strings.Contains(lower, "taiwan")) &&
		(strings.Contains(task, "政府") || strings.Contains(task, "公立教育機構") ||
			strings.Contains(task, "公立教育机构") || strings.Contains(lower, "government") ||
			strings.Contains(lower, "public education institution"))
}

func lfm25AllowedResearchSourceURL(task, rawURL string) bool {
	if !lfm25RequiresTaiwanPublicInstitutionSources(task) {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	return host == "gov.tw" || strings.HasSuffix(host, ".gov.tw") ||
		host == "edu.tw" || strings.HasSuffix(host, ".edu.tw")
}

func lfm25DiscoveryQuerySuffix(task string) string {
	if lfm25RequiresTaiwanPublicInstitutionSources(task) {
		return "台灣 官方 gov.tw edu.tw 文章 指南"
	}
	card := agentcore.ClassifySearchRAGCard(task)
	switch card.Kind {
	case agentcore.SearchTaskHighRisk:
		return "official professional guidance"
	case agentcore.SearchTaskSoftwareGuide:
		return "official documentation"
	case agentcore.SearchTaskCurrentFact:
		return "official source"
	case agentcore.SearchTaskOpenResearch:
		if lfm25KnowledgeQuestionRe.MatchString(router.StripSystemReminder(task)) {
			return "official university educational article"
		}
	}
	return ""
}

func lfm25ResearchToolAllowed(name string) bool {
	name = strings.TrimSpace(name)
	switch name {
	case router.ToolWebSearch, router.ToolWebFetch, "meadow_browser_dom",
		"pdf_extract",
		rag.EncyclopediaSearchToolName, rag.DocumentSearchToolName, rag.DocumentReadToolName, tools.AskUserToolName,
		tools.ResearchPublishToolName:
		return true
	default:
		return false
	}
}
