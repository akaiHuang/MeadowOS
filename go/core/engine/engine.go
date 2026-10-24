// Package engine is the M1 agent loop: model call → tool_calls → approval →
// execution → feed results back, with the local-Gemma-4 action-mode routing
// from the TS provider built in.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"github.com/hey-meadow/meadow-code/go/core/artifactvalidator"
	"github.com/hey-meadow/meadow-code/go/core/mempalace"
	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/rag"
	"github.com/hey-meadow/meadow-code/go/core/router"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	maxTurns      = 100
	mtplxMaxTurns = 24
)

// ModelCaller is the provider surface the engine needs; *provider.Client
// satisfies it, tests fake it.
type ModelCaller interface {
	ChatStream(ctx context.Context, req provider.ChatRequest, hooks *provider.StreamHooks) (*provider.StreamResult, error)
}

// Events are optional UI hooks; any field may be nil.
type Events struct {
	OnContent  func(delta string)
	OnProgress func(provider.InferenceProgress)
	// OnMessageBoundary marks one public response (start/end), not task
	// completion. The host may return its presentation ID on start.
	OnMessageBoundary func(state string) string
	// OnThought receives the model's thought-channel text — UIs typically
	// render it dimmed while waiting for the answer.
	OnThought func(delta string)
	// OnToolDraft streams tool-call argument progress (name, total bytes
	// so far) — the liveness signal while the model writes a large file.
	OnToolDraft         func(name string, totalBytes int)
	OnToolDraftProgress func(provider.ToolDraftProgress)
	OnToolCall          func(name string, args map[string]any)
	OnToolResult        func(name string, res tools.Result)
	OnToolStart         func(agentcore.ToolStart)
	OnToolComplete      func(agentcore.ToolExecution)
	OnTurn              func(turn int, usage *provider.Usage)
	OnRouteShadow       func(res RouteShadowResult)
	OnRAG               func(res RAGTrace)
}

// RAGTrace exposes retrieval metadata without leaking evidence into normal
// model output. Product gates use it to separate retrieval and fusion errors.
type RAGTrace struct {
	UserText string
	Intent   string
	Matches  []RAGTraceMatch
}

type RAGTraceMatch struct {
	Title       string
	Source      string
	Version     string
	Updated     string
	Score       int
	KeywordHits int
	IntentMatch bool
}

// RoutePredictor is the optional Meadow Call / router-LoRA surface. It returns
// one compact route hint; Meadow core still owns tool selection, approvals, and
// destructive gates.
type RoutePredictor interface {
	PredictRoute(ctx context.Context, userText string, availableToolNames []string) (string, error)
}

// RouteShadowResult is emitted when a compact route predictor is configured or
// a UI wants deterministic route telemetry. Agreement is diagnostic only.
type RouteShadowResult struct {
	UserText             string
	DeterministicIntent  string
	DeterministicTools   []string
	DeterministicCompact string
	PredictedRaw         string
	PredictedIntent      string
	PredictedTools       []string
	Agreement            bool
	Promoted             bool
	PromotionReason      string
	Err                  string
}

// Compatibility aliases keep existing callers building while the canonical
// runtime contract lives in core/agent.
type StopReason = agentcore.StopReason

const (
	StopCompleted         = agentcore.StopCompleted
	StopMaxTurns          = agentcore.StopMaxTurns
	StopLoopDetected      = agentcore.StopLoopDetected
	StopAuditInconclusive = agentcore.StopAuditInconclusive
	StopTokenLimit        = agentcore.StopTokenLimit
	StopCancelled         = agentcore.StopCancelled
	StopEmpty             = agentcore.StopEmpty
	StopIncomplete        = agentcore.StopIncomplete
	StopNeedsInput        = agentcore.StopNeedsInput
)

// StopMessage renders an honest, user-facing line for a non-completed run.
// deliverable is the task-named output file (may be ""); suggest26B adds the
// "try Meadow 26B" hint only when the run used a weaker model. Returns "" for
// StopCompleted (the caller prints the model's own summary in that case).
// For StopIncomplete/StopEmpty the model's own FinalText is confabulated or
// empty, so the caller should print THIS instead; for the other reasons the
// caller keeps the partial output and appends this line.
func StopMessage(stop StopReason, deliverable string, suggest26B bool) string {
	try26 := ""
	if suggest26B {
		try26 = "這類多步任務在 Meadow 26B 上更可靠——重試或改用 26B。"
	} else {
		try26 = "重試一次，或把任務拆小一點。"
	}
	switch stop {
	case StopNeedsInput:
		return "需要你先確認一個會影響結果的選擇，才能安全繼續。"
	case StopIncomplete:
		what := "要求的檔案"
		if deliverable != "" {
			what = deliverable
		}
		return "⚠ 我處理了這個任務，但沒能產出 " + what + "。" + try26
	case StopEmpty:
		return "⚠ 這次沒有任何產出。換個說法再試一次。" + ifStr(suggest26B, "或改用 Meadow 26B。", "")
	case StopMaxTurns:
		return "⚠ 步驟太多、已達上限。把任務拆小一點再試。"
	case StopLoopDetected:
		return "⚠ 偵測到重複迴圈、已停止。" + ifStr(suggest26B, "換個說法或改用 Meadow 26B。", "換個說法再試。")
	case StopAuditInconclusive:
		return "⚠ 指定驗收已通過，但獨立需求審核未能收斂；已保留通過版本，尚未宣稱完整完成。"
	case StopTokenLimit:
		return "⚠ 模型輸出或對話長度已達上限，任務尚未完成。請重試或開新對話。"
	case StopCancelled:
		return "已取消。"
	default:
		return ""
	}
}

func ifStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

type RunResult = agentcore.RunResult

// Engine wires provider, tools, approval, and routing together.
type Engine struct {
	Client             ModelCaller
	Registry           *tools.Registry
	RequireToolAccount func(context.Context) error
	Mode               ApprovalMode
	Confirm            Confirmer
	Events             Events
	// TraceObserver records sanitized orchestration events and a final run
	// report. It is behavior-neutral and fail-open; nil disables observation.
	TraceObserver agenttrace.Observer
	// AgentRuntime labels the migration path in events/reports. Kernel is not
	// promoted by Engine itself; CLI rejects it until the execution gate lands.
	AgentRuntime agenttrace.Runtime

	SystemPrompt string
	// ContextManager owns task-scoped memory injection. It is deliberately
	// separate from prompt.Build so CLI, Desktop and Serve use one policy.
	ContextManager *agentcore.ContextManager
	// ArtifactValidators owns post-write syntax checks and optional mechanical
	// repair capabilities. Nil uses the default capability registry.
	ArtifactValidators *artifactvalidator.Registry
	ModelID            string
	CWD                string
	// UseRouter enables the local-Gemma-4 three-state request rewriting
	// (minimal chat / action mode / final summary).
	UseRouter bool
	// RoutePredictor runs a compact route model. In default/shadow mode it is
	// queried whenever routing telemetry is emitted. In "auto" mode it is only
	// queried for ambiguous action turns or explicit user follow-up/escalation.
	RoutePredictor RoutePredictor
	// RoutePredictorMode controls predictor wakeup. Empty keeps the legacy
	// always-shadow behavior when a predictor is configured; "auto" avoids
	// waking the second MLX process when the deterministic router is already
	// specific enough.
	RoutePredictorMode string
	// RoutePredictorPromotion is an opt-in experiment. Empty/off keeps pure
	// shadow mode. "safe" lets low-risk predictor hints narrow a deterministic
	// general_action route, but never bypasses approvals or destructive gates.
	RoutePredictorPromotion string
	// SessionTokenLimit ends the run when the prompt grows past it. 0 = off.
	SessionTokenLimit int
	// ContextWindowTokens is the physical input/output capacity reported by the
	// live runtime. It is separate from the smaller prompt admission limit.
	ContextWindowTokens int
	// RAG retrieves a proven template for a create intent and injects it
	// into the first action prompt as a reference the model adapts. nil = off.
	RAG *rag.Store
	// RAGTopK controls compact evidence retrieval for the thin small-model path.
	// Values below 1 use three evidence chunks.
	RAGTopK int
	// MaterializeRAG lets Core write an exact verified RAG artifact before the
	// model turn, then gives the model only the compact tool result. The legacy
	// MEADOW_RAG_MATERIALIZE=1 switch remains as a back-compatible fallback.
	MaterializeRAG bool
	// DisableRAGMaterialize is the fair benchmark guard: it wins over both the
	// field above and the legacy environment switch.
	DisableRAGMaterialize bool
	// ThinHarness keeps deterministic routing and validators, omits MemPalace and
	// full RAG artifacts, but may inject a bounded compact Top-K evidence block.
	ThinHarness bool
	// MemPalace is the long-term memory client: lessons learned across runs +
	// per-task max_tok ceilings, injected on the first action turn. Fail-open,
	// so a nil client (or a down shim) simply adds nothing. nil = off.
	MemPalace mempalace.Client
	// PrefillProfile gates experimental request-shaping profiles that reduce
	// cold prefill tokens before the request reaches the local model. Empty uses
	// MEADOW_PREFILL_PROFILE; empty/off keeps current behavior.
	PrefillProfile string
	// TwoPass runs a short hidden Task Brief pass before the normal agent loop.
	// The brief is injected into outgoing model requests for this run only; it is
	// not appended to canonical history or persisted chat logs.
	TwoPass bool

	// approveAlways remembers DecisionApproveAlways per exact tool name. The
	// legacy approveAllAlways field remains for resumed-session compatibility,
	// but is intentionally ignored: approving web_fetch must never silently
	// authorize a later shell command or file mutation.
	approveAlways    map[string]bool
	approveAllAlways bool

	// steers queues user guidance injected at the next turn boundary —
	// mid-task steering without cancelling the run.
	steerMu sync.Mutex
	steers  []string

	// runText is the current Run's original user ask — the stable anchor
	// for intent classification and RAG retrieval (nudges are synthetic
	// user messages and must not re-classify the plan mid-run).
	runText string
	// lastTaskType is the most recent action plan id (the MemPalace task_type),
	// set in buildRequest so the self-repair loop can key a written lesson to
	// the task that produced the mistake.
	lastTaskType string
	// twoPassBrief holds the hidden Task Brief for the current run. It is cleared
	// at the end of RunMessage.
	twoPassBrief string
	// lastSavedArtifactPath is the sanitized path of the latest successful
	// write_file in this run. Empty visual_check calls reuse it instead of
	// failing with "file_path or url is required".
	lastSavedArtifactPath string
}

var _ agentcore.Runtime = (*Engine)(nil)

// Steer queues a user message for injection before the next model turn.
// Safe to call from any goroutine while Run is in flight; queued text is
// also picked up at the start of the next Run if the current one finishes
// first.
func (e *Engine) Steer(text string) {
	e.steerMu.Lock()
	e.steers = append(e.steers, text)
	e.steerMu.Unlock()
}

func (e *Engine) drainSteers() []string {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	out := e.steers
	e.steers = nil
	return out
}

// Run executes one user prompt to completion. history carries prior turns
// (resume); the returned Messages include the new turns.
// runIntentRe matches a task that explicitly asks to RUN/EXECUTE something —
// "then run it", "execute the script", "跑它", "執行". Used to push a model
// that wrote a file but reported a guessed result instead of actually running
// it (the 12B "wrote add.py, said 4, never ran it" failure).
var runIntentRe = regexp.MustCompile(`(?i)\b(run|execute)\b[^.]{0,30}\b(it|them|the\s+\w+|with\s+\w+)|然後[^。]{0,4}(跑|執行)|再[^。]{0,3}(跑|執行)|並[^。]{0,3}(跑|執行)|(跑|執行)(它|起來|看|一)`)
var postWriteValidationRe = regexp.MustCompile(`(?i)\b(test|build|validate|verify|lint|typecheck|tsc|pytest|vitest|jest|npm|pnpm|yarn|node|python|run|execute)\b|測試|测试|驗證|验证|執行|执行|跑(一下|測試|测试|看看|起來)?`)

func (e *Engine) Run(ctx context.Context, userText string, history []provider.Message) (*RunResult, error) {
	return e.RunInput(ctx, agentcore.RunInput{
		User: provider.Message{Role: "user", Content: userText}, History: history,
	})
}

func (e *Engine) RunInput(ctx context.Context, input agentcore.RunInput) (*RunResult, error) {
	return e.RunMessage(ctx, input.User, input.History)
}

func (e *Engine) RunMessage(ctx context.Context, userMsg provider.Message, history []provider.Message) (out *RunResult, runErr error) {
	if e.approveAlways == nil {
		e.approveAlways = map[string]bool{}
	}
	if userMsg.Role == "" {
		userMsg.Role = "user"
	}
	if strings.TrimSpace(userMsg.Content) == "" {
		userMsg.Content = provider.MessageText(userMsg)
	}
	userText := provider.MessageText(userMsg)
	e.runText = userText
	e.lastTaskType = ""
	e.twoPassBrief = ""
	verifier := e.newVerifier(userText)
	defer func() { e.twoPassBrief = "" }()
	messages := append(append([]provider.Message{}, history...), userMsg)
	res := &RunResult{}
	// Tool-produced image pixels are run-scoped provider evidence. Keep them in
	// the live loop so a visual inspection may be followed by another tool and a
	// final comparison, but never return them through durable/UI history.
	defer func() {
		if out != nil {
			out.Messages = provider.StripToolResultMedia(out.Messages)
		}
	}()
	trace := agenttrace.Start(e.TraceObserver, agenttrace.RunMeta{
		Runtime: e.AgentRuntime,
		Model:   e.ModelID,
		CWD:     e.CWD,
	})
	ctx = trace.Context(ctx)
	evidence := agentcore.NewEvidenceLedger()
	activeTurn := 0
	// Consolidation runs after finalization (defer order is LIFO), so an
	// authoritative kernel verdict cannot be learned as a false success.
	defer e.consolidate(ctx, res)
	defer func() {
		var legacyStop string
		var deliverables []agentcore.Deliverable
		if out != nil {
			legacyStop = string(out.Stop)
			target := router.TaskTargetFile(userText)
			if target != "" && !isDeleteTask(userText) {
				deliverables = []agentcore.Deliverable{{Path: target, Exists: fileOnDisk(e.CWD, target)}}
			}
		}
		snapshot := evidence.Snapshot()
		validationDecision := verifier.Decision(snapshot)
		finalDecision := agentcore.Finalize(agentcore.FinalizeInput{
			LegacyStop: agentcore.StopReason(legacyStop), RunError: runErr, Evidence: snapshot,
			Deliverables: deliverables, Validation: &validationDecision,
		})
		authoritative := agentcore.FinalizerIsAuthoritative(string(e.AgentRuntime))
		if authoritative && out != nil && runErr == nil {
			out.Stop = StopReason(finalDecision.StopReason)
			finalResponse := agentcore.ComposeFinalResponse(agentcore.FinalResponseInput{
				Decision: finalDecision, ModelText: out.FinalText, Evidence: snapshot,
				ValidatorCommand: verifier.Command(),
			})
			out.FinalResponse = &finalResponse
			out.FinalText = finalResponse.Text
			out.Messages = agentcore.ReplaceTerminalAssistant(out.Messages, finalResponse.Text)
		}
		if trace == nil {
			return
		}
		if activeTurn > 0 {
			trace.Emit(agenttrace.EventTurnFinished, activeTurn, map[string]any{"status": "run_finished"})
		}
		finish := agenttrace.Finish{
			TaskCategory: e.lastTaskType, Error: runErr,
			VerificationVerdict: string(finalDecision.Verification),
			ValidatorCommand:    validationDecision.Command, ValidatorSource: string(validationDecision.Source),
			ValidatorExitCode: snapshot.ValidatorExitCode, HasValidatorExitCode: snapshot.HasValidatorExitCode,
			ChangedPaths: snapshot.ChangedPaths, FinalizerAuthoritative: authoritative,
			FinalizerShadow: &agenttrace.FinalizerShadow{
				StopReason: string(finalDecision.StopReason), EvidenceSupport: string(finalDecision.EvidenceSupport),
				ClaimSupported: finalDecision.ClaimSupported, CompletedTruth: finalDecision.CompletedTruth,
				Verification: string(finalDecision.Verification), Reason: finalDecision.Reason,
				LegacyAgreement: finalDecision.LegacyAgreement,
			},
		}
		finish.EvidenceGateAuthority = authoritative
		finish.Flags.DuplicateVerification = snapshot.DuplicateValidations > 0
		if out != nil {
			finish.StopReason = string(out.Stop)
			finish.Turns = out.Turns
			finish.ToolCalls = out.ToolCalls
			finish.FirstPromptTokens = out.FirstPromptTokens
			finish.FirstSignalMS = out.FirstResponseMs
			target := router.TaskTargetFile(userText)
			falseComplete := out.Stop == StopCompleted && target != "" && !isDeleteTask(userText) && !fileOnDisk(e.CWD, target)
			finish.Flags.FalseComplete = falseComplete
			if authoritative {
				finish.CompletedTruth = finalDecision.CompletedTruth
			} else {
				finish.CompletedTruth = out.Stop == StopCompleted && !falseComplete
			}
			if target != "" && fileOnDisk(e.CWD, target) {
				finish.ProducedDeliverables = []string{target}
			}
		}
		trace.Emit(agenttrace.EventFinalizationFinished, finish.Turns, map[string]any{
			"stop_reason":       finish.StopReason,
			"completed_truth":   finish.CompletedTruth,
			"deliverable_count": len(finish.ProducedDeliverables),
			"shadow_agreement":  finish.FinalizerShadow == nil || finish.FinalizerShadow.LegacyAgreement,
			"authoritative":     authoritative,
		})
		trace.Finish(finish)
	}()

	// Harness-driven Bash: a high-confidence, determinable, SAFE shell op
	// (mkdir / list / git status) is run by the harness directly. The weak
	// model never sees it — no tool-call drift, no prefill of the
	// system+tools prompt, instant. Anything ambiguous or destructive returns
	// ok=false and falls through to the normal model loop below.
	if e.UseRouter && len(history) == 0 {
		if cmd, ok := router.HarnessCommand(userText); ok {
			if _, found := e.Registry.Get("run_shell_command"); found {
				if e.Events.OnRouteShadow != nil || e.RoutePredictor != nil {
					if plan, selected := router.SelectIntentTools(userText, e.Registry.Names()); plan != nil {
						e.emitRouteShadow(ctx, userText, plan, selected)
					}
				}
				args := map[string]any{"command": cmd}
				callID := "meadow-harness-shell-1"
				call := provider.ToolCall{
					ID:   callID,
					Type: "function",
					Function: provider.FunctionCall{
						Name:      "run_shell_command",
						Arguments: provider.MarshalArgs(args),
					},
				}
				trace.Emit(agenttrace.EventToolRequested, 0, map[string]any{"tool": "run_shell_command", "source": "harness"})
				executor := agentcore.ToolExecutor{
					Registry:       e.Registry,
					RequireAccount: e.RequireToolAccount,
					Authorize:      e.authorizeToolCall,
					BeforeExecute: func(name string, toolArgs map[string]any) {
						e.backupBeforeMutate(name, toolArgs)
					},
					OnToolCall: e.Events.OnToolCall,
					OnToolStart: func(start agentcore.ToolStart) {
						if e.Events.OnToolStart != nil {
							e.Events.OnToolStart(start)
						}
						trace.Emit(agenttrace.EventToolStarted, 0, map[string]any{
							"tool": start.Tool, "source": "harness", "effect": string(agentcore.EffectForKind(start.Kind)),
						})
					},
					OnToolResult:   e.Events.OnToolResult,
					OnToolComplete: e.Events.OnToolComplete,
				}
				execution := executor.Execute(ctx, call, 0)
				if evidence != nil {
					evidence.Record(agentcore.ToolObservation{
						Tool: "run_shell_command", Status: execution.Status,
						MutatesFile: execution.Effect == agentcore.EffectWrite,
						DurationMS:  execution.DurationMS,
					})
				}
				trace.Emit(agenttrace.EventToolFinished, 0, map[string]any{
					"tool": "run_shell_command", "source": "harness", "status": string(execution.Status),
					"effect": string(execution.Effect), "executed": execution.Executed,
					"duration_ms": execution.DurationMS,
				})
				if !execution.Executed {
					final := strings.TrimSpace(execution.LLMContent)
					if final == "" {
						final = "這個操作需要明確授權，因此尚未執行。"
					}
					messages = append(messages, provider.Message{Role: "assistant", Content: final})
					res.Stop = StopIncomplete
					res.FinalText = final
					res.ToolCalls = 1
					res.Loop = "harness-shell"
					res.Messages = messages
					return res, nil
				}
				final := "已直接執行 `" + cmd + "`"
				if strings.TrimSpace(execution.Display) != "" {
					final += "\n" + execution.Display
				}
				messages = append(messages, provider.Message{Role: "assistant", Content: final})
				res.Stop = StopCompleted
				res.FinalText = final
				res.ToolCalls = 1
				res.Loop = "harness-shell"
				res.Messages = messages
				return res, nil
			}
		}
	}
	// Backend scaffolding: for a static-web "create" intent the Go backend
	// pre-creates the skeleton file (structure + empty style/script with
	// placeholders) so the model spends no tokens on the path or structure
	// and only edits to fill it — eliminating the whole class of path/name
	// failures and the unreliable model-writes-skeleton stage.
	if seeded, ok := e.maybeMaterializeRAG(ctx, userText, len(history) == 0); ok {
		messages = append(messages, seeded...)
		res.ToolCalls++
	} else if note := e.maybeScaffold(ctx, userText, len(history) == 0); note != "" {
		messages = append(messages, provider.Message{Role: "user", Content: note})
	}
	if e.TwoPass {
		e.twoPassBrief = e.runTwoPassBrief(ctx, userMsg, history, trace)
	}
	detector := newLoopDetector()
	// The 26B sometimes burns a whole turn inside the thought channel and
	// produces neither text nor a tool call (observed live: 280 tokens of
	// musing, zero output). One nudge gets it moving again; a second empty
	// turn ends the run for real.
	nudges := agentcore.DefaultNudgePolicy()
	// Self-repair loop: a write_file/edit on a code file is syntax-checked;
	// a failure is fed back so the model edit-fixes it, and the run is not
	// allowed to finish while a written file still has a syntax error
	// (capped, so a model that cannot fix it eventually stops). This is the
	// read-back-verify-repair loop that makes the difference for local
	// models that cannot one-shot a correct artifact.
	pendingSyntaxErr := ""
	pendingSyntaxFile := ""
	// A run that wrote/edited a file did produce output (on disk); it must
	// not report StopEmpty just because the closing turn had no prose, or
	// the desktop shows "⚠ 沒有產出" over a real file.
	producedFiles := false
	// "asked to run it but never ran it" guard: track whether a shell command
	// actually executed this run, so a task that wanted execution but only got
	// a guessed answer gets pushed to really run it.
	ranShell := false
	pendingValidationFailure := false
	conversation := agentcore.NewConversationLoop(e.turnLimit(), agentcore.RetryPolicy{
		agentcore.RetryMalformedToolCall: 1,
		agentcore.RetryEmptyOutput:       1,
	})

	for {
		turn, ok := conversation.BeginTurn(ctx)
		if !ok {
			break
		}
		if trace != nil {
			if activeTurn > 0 {
				trace.Emit(agenttrace.EventTurnFinished, activeTurn, map[string]any{"status": "advanced"})
			}
			activeTurn = turn
			trace.Emit(agenttrace.EventTurnStarted, turn, nil)
		}
		res.Turns = turn

		// Turn-boundary steering: guidance typed while the previous turn
		// ran lands here as fresh user messages the model sees next.
		for _, s := range e.drainSteers() {
			messages = append(messages, provider.Message{Role: "user", Content: s})
		}
		for _, notification := range e.Registry.DrainNotifications() {
			messages = append(messages, provider.Message{Role: "user", Content: notification})
		}

		req := e.buildRequestWithVerifier(ctx, messages, verifier)
		modelProfile, _ := provider.ProfileForModel(e.ModelID)
		req.Messages = provider.FilterPersistedHistoryImages(req.Messages, modelProfile.Multimodal)
		req.Messages = provider.PrepareToolResultMedia(req.Messages, modelProfile.Multimodal)
		contextSelection := agentcore.ContextSelection{}
		if e.ContextManager != nil {
			if e.ThinHarness {
				req.Messages, contextSelection = e.ContextManager.Compact(req.Messages)
			} else {
				req.Messages, contextSelection = e.ContextManager.Apply(req.Messages, e.runText)
			}
		}
		trace.Emit(agenttrace.EventRouteSelected, turn, map[string]any{
			"intent": e.lastTaskType,
			"tools":  requestToolNames(req),
		})
		turnStart := time.Now()
		var firstSignal sync.Once
		markFirstResponse := func(signal string) {
			firstSignal.Do(func() {
				trace.Emit(agenttrace.EventModelFirstSignal, turn, map[string]any{
					"signal": signal, "latency_ms": time.Since(turnStart).Round(time.Millisecond).Milliseconds(),
				})
			})
			if turn != 1 || res.FirstResponseMs > 0 {
				return
			}
			ms := time.Since(turnStart).Round(time.Millisecond).Milliseconds()
			if ms < 1 {
				ms = 1
			}
			res.FirstResponseMs = ms
		}
		trace.Emit(agenttrace.EventModelRequested, turn, map[string]any{
			"message_count":            len(req.Messages),
			"tool_count":               len(req.Tools),
			"max_tokens":               traceMaxTokens(req.MaxTokens),
			"context_memory_bytes":     contextSelection.Bytes,
			"global_memory_included":   contextSelection.GlobalIncluded,
			"project_memory_included":  contextSelection.ProjectIncluded,
			"global_memory_reason":     contextSelection.GlobalReason,
			"history_compacted":        contextSelection.HistoryCompacted,
			"history_input_messages":   contextSelection.HistoryInputMessages,
			"history_output_messages":  contextSelection.HistoryOutputMessages,
			"history_dropped_messages": contextSelection.HistoryDroppedMessages,
			"history_input_bytes":      contextSelection.HistoryInputBytes,
			"history_output_bytes":     contextSelection.HistoryOutputBytes,
		})
		publicStarted := false
		publicMessageID := ""
		var publicText strings.Builder
		emitPublicContent := func(delta string) {
			if delta == "" && e.Events.OnMessageBoundary != nil {
				return
			}
			if delta != "" && !publicStarted {
				publicStarted = true
				if e.Events.OnMessageBoundary != nil {
					publicMessageID = e.Events.OnMessageBoundary("start")
				}
			}
			if e.Events.OnMessageBoundary != nil {
				publicText.WriteString(delta)
			}
			if e.Events.OnContent != nil {
				e.Events.OnContent(delta)
			}
		}
		stream, err := e.Client.ChatStream(ctx, req, &provider.StreamHooks{
			OnProgress: e.Events.OnProgress,
			OnDelta: func(delta string, thought bool) {
				if delta != "" {
					markFirstResponse(ifStr(thought, "thought", "content"))
				}
				if thought {
					if !provider.SuppressThoughtDisplay(e.ModelID) && e.Events.OnThought != nil {
						e.Events.OnThought(delta)
					}
					return
				}
				emitPublicContent(delta)
			},
			OnToolDraft: func(name string, totalBytes int) {
				if totalBytes > 0 {
					markFirstResponse("tool_draft")
				}
				if e.Events.OnToolDraft != nil {
					e.Events.OnToolDraft(name, totalBytes)
				}
			},
			OnToolDraftProgress: e.Events.OnToolDraftProgress,
		})
		if e.Events.OnMessageBoundary != nil && !publicStarted && err == nil && stream != nil && stream.Content != "" {
			// Some adapters return public text without streaming it. Only opted-in
			// hosts receive this exact-content fallback; legacy hooks stay unchanged.
			emitPublicContent(stream.Content)
		}
		if publicStarted && e.Events.OnMessageBoundary != nil {
			// Close partial responses before the existing retry/error handling.
			e.Events.OnMessageBoundary("end")
		}
		if e.Events.OnMessageBoundary != nil && publicStarted && (err != nil || stream == nil) {
			// Keep only bytes already shown publicly so reconnect/retry replay
			// retains this message. Failed tool drafts are never accepted here.
			messages = append(messages, provider.Message{
				Role: "assistant", Content: publicText.String(), MessageID: publicMessageID,
			})
		}
		if err != nil {
			trace.Emit(agenttrace.EventModelFailed, turn, map[string]any{
				"error_class": traceModelErrorClass(err),
				"duration_ms": time.Since(turnStart).Round(time.Millisecond).Milliseconds(),
			})
			if errors.Is(err, provider.ErrMalformedToolCall) &&
				conversation.AllowRetry(agentcore.RetryMalformedToolCall) {
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: "(上一輪的工具呼叫格式未完成。只重試一次：使用 API tool/function-call channel 重送完整且有效的工具參數。)",
				})
				continue
			}
			if errors.Is(err, provider.ErrThoughtBurn) && conversation.AllowRetry(agentcore.RetryEmptyOutput) {
				// 思考燒盡＝空輪的早斷版：不失敗整個 run，用更硬的
				// 指令 nudge 一次（資深路徑：見 resume 失敗歷史污染案）。
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: "(你上一輪只輸出思考、沒有任何正文或工具呼叫。不要使用思考通道：直接呼叫下一個需要的工具，或用一句話回答。)",
				})
				continue
			}
			if errors.Is(err, provider.ErrThoughtBurn) {
				res.Stop = StopEmpty
				res.Messages = messages
				return res, nil
			}
			res.Messages = messages
			return res, fmt.Errorf("turn %d: %w", turn, err)
		}
		if stream == nil {
			res.Messages = messages
			return res, fmt.Errorf("turn %d: model returned no result", turn)
		}
		if turn == 1 && stream.Usage != nil {
			res.FirstPromptTokens = stream.Usage.PromptTokens
		}
		conversation.ResetRetry(agentcore.RetryMalformedToolCall)
		if turn == 1 && res.FirstResponseMs == 0 &&
			(stream.Content != "" || len(stream.ToolCalls) > 0) {
			markFirstResponse("completion")
		}
		modelFields := map[string]any{
			"finish_reason":      stream.FinishReason,
			"content_bytes":      len(stream.Content),
			"tool_call_count":    len(stream.ToolCalls),
			"protocol_leak":      stream.ProtocolLeak,
			"protocol_recovered": stream.ProtocolRecovered,
			"duration_ms":        time.Since(turnStart).Round(time.Millisecond).Milliseconds(),
		}
		if stream.Repaired {
			modelFields["repaired_tool_calls"] = len(stream.ToolCalls)
		}
		if stream.Usage != nil {
			modelFields["prompt_tokens"] = stream.Usage.PromptTokens
			modelFields["completion_tokens"] = stream.Usage.CompletionTokens
		}
		trace.Emit(agenttrace.EventModelFinished, turn, modelFields)
		if e.Events.OnTurn != nil {
			e.Events.OnTurn(turn, stream.Usage)
		}

		// A forced validator turn has one acceptable action. Preserve an exact
		// Agent Kernel call, but discard a missing, wrong-name, or wrong-command
		// draft so the deterministic Core fallback below can run the selected
		// command instead of executing a hallucinated tool.
		if forcedSingleToolName(req.ToolChoice) == router.ToolShell {
			if verifier.Command() != "" && !verifier.AcceptsOnlyCall(stream.ToolCalls) {
				stream.ToolCalls = nil
			}
		}

		transcriptContent := stream.Content
		if e.Events.OnMessageBoundary != nil && publicText.Len() > 0 {
			// The provider may normalize its completed result. Replay this public
			// identity with exactly the bytes already shown to the opted-in host.
			transcriptContent = publicText.String()
		}
		assistant := provider.Message{
			Role: "assistant", Content: transcriptContent, ToolCalls: stream.ToolCalls, MessageID: publicMessageID,
		}
		messages = append(messages, assistant)

		// A valid tool call is concrete progress. Some OpenAI-compatible local
		// servers co-emit the edit body in content as well as in tool arguments;
		// running prose-chant detection on that repeated code can terminate just
		// before the repair executes. AddToolCall below owns loop detection for
		// tool turns and resets content history.
		if len(stream.ToolCalls) == 0 {
			if lt := detector.AddContent(stream.Content); lt != "" {
				res.Stop, res.Loop, res.FinalText = StopLoopDetected, lt, stream.Content
				res.Messages = messages
				return res, nil
			}
		}

		if len(stream.ToolCalls) == 0 {
			if notifications := e.Registry.DrainNotifications(); len(notifications) > 0 {
				for _, notification := range notifications {
					messages = append(messages, provider.Message{Role: "user", Content: notification})
				}
				continue
			}
			// 自我修復：上一個寫入的程式檔還有語法錯誤，模型卻想結束 →
			// 強制它用 edit 修；次數由通用 bounded nudge policy 管理。
			if pendingSyntaxErr != "" && allowNudge(trace, turn, nudges, agentcore.NudgeSyntaxRepair) {
				messages = append(messages, provider.Message{
					Role: "user", Content: agentcore.ComposeNudge(agentcore.NudgeInput{
						Kind: agentcore.NudgeSyntaxRepair, SyntaxFile: pendingSyntaxFile, SyntaxError: pendingSyntaxErr,
					}),
				})
				continue
			}
			// The Agent Kernel gets first chance to emit an exact validator tool
			// call. If it returns text/empty output on a forced engineering
			// validation turn, Core already has a repository-derived command and
			// can execute it deterministically. This is a controller fallback, not
			// a fabricated result: the normal shell tool runs and its real exit
			// status re-enters the repair/stop loop.
			if forcedSingleToolName(req.ToolChoice) == router.ToolShell {
				if call, ok := verifier.NextFallbackCall(turn, 3); ok {
					command := verifier.Command()
					messages = append(messages, provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{call}})
					output, loopType, verification, execution := e.executeCall(ctx, call, detector, trace, evidence, verifier, turn)
					messages = append(messages, provider.Message{
						Role: "tool", ToolCallID: call.ID, Content: output,
						ContentParts: append([]provider.MessageContentPart(nil), execution.ContentParts...),
					})
					res.ToolCalls++
					ranShell = ranShell || execution.Executed
					pendingValidationFailure = verification.Validation && verification.Status == agentcore.VerificationFailed
					if loopType != "" {
						res.Stop, res.Loop, res.Messages = StopLoopDetected, loopType, messages
						return res, nil
					}
					if verifier.Decision(evidence.Snapshot()).Fresh && e.thinEngineeringValidationComplete(messages) {
						res.Stop = StopCompleted
						res.FinalText = "Validation passed: `" + command + "`."
						res.Messages = messages
						return res, nil
					}
					continue
				}
			}
			if fail := lastToolFailure(messages); fail != "" {
				if deliverable := router.TaskTargetFile(e.runText); deliverable != "" &&
					fileOnDisk(e.CWD, deliverable) && ignorableToolArgumentFailure(fail) {
					res.Stop = StopCompleted
					res.FinalText = strings.TrimSpace(stream.Content)
					if res.FinalText == "" {
						res.FinalText = "Created " + deliverable + "."
					}
					res.Messages = messages
					return res, nil
				}
				if allowNudge(trace, turn, nudges, agentcore.NudgeToolError) {
					namedCmd := router.NamedValidationCommand(e.runText)
					messages = append(messages, provider.Message{
						Role: "user", Content: agentcore.ComposeNudge(agentcore.NudgeInput{
							Kind: agentcore.NudgeToolError, Failure: firstLine(fail), ValidationCommand: namedCmd,
						}),
					})
					continue
				}
				res.Stop = StopIncomplete
				res.FinalText = stream.Content
				res.Messages = messages
				return res, nil
			}
			if snapshot := evidence.Snapshot(); agentcore.LastMutationFailed(snapshot) {
				if allowNudge(trace, turn, nudges, agentcore.NudgeEvidenceStatus) {
					messages = append(messages, provider.Message{
						Role: "user", Content: agentcore.ComposeNudge(agentcore.NudgeInput{
							Kind: agentcore.NudgeEvidenceStatus, Verification: agentcore.VerificationUnverified,
							ChangedPaths: snapshot.ChangedPaths, Failure: "last_mutation_failed",
						}),
					})
					continue
				}
				res.Stop = StopIncomplete
				res.FinalText = stream.Content
				res.Messages = messages
				return res, nil
			}
			if pendingValidationFailure {
				if allowNudge(trace, turn, nudges, agentcore.NudgeValidationFailure) {
					namedCmd := router.NamedValidationCommand(e.runText)
					messages = append(messages, provider.Message{
						Role: "user", Content: agentcore.ComposeNudge(agentcore.NudgeInput{
							Kind: agentcore.NudgeValidationFailure, ValidationCommand: namedCmd,
						}),
					})
					continue
				}
				res.Stop = StopIncomplete
				res.FinalText = stream.Content
				res.Messages = messages
				return res, nil
			}
			// 文字交付：staged 計畫還缺檔，但模型把內容印在對話裡（高 temp
			// 的主要失敗模式）。先試 harness 抽取——把內容抽出來自己寫檔，
			// 工具調用就不再依賴模型發出正確的 tool call；抽不到才退回 nudge。
			deleteTask := isDeleteTask(e.runText)
			if e.UseRouter && strings.TrimSpace(stream.Content) != "" {
				if missing := router.UnfinishedStagedFiles(e.runText, messages, e.CWD); len(missing) > 0 {
					if note, ok := e.saveChattedArtifact(ctx, trace, turn, missing[0], stream.Content, &producedFiles, &pendingSyntaxErr, &pendingSyntaxFile); ok {
						messages = append(messages, provider.Message{Role: "user", Content: note})
						continue
					}
					if allowNudge(trace, turn, nudges, agentcore.NudgeStagedDeliverable) {
						messages = append(messages, provider.Message{
							Role: "user", Content: agentcore.ComposeNudge(agentcore.NudgeInput{
								Kind: agentcore.NudgeStagedDeliverable, Targets: missing,
							}),
						})
						continue
					}
				} else if target := router.TaskTargetFile(e.runText); !deleteTask && target != "" && !fileOnDisk(e.CWD, target) {
					// Not a staged plan, but the task named a file the model only
					// printed in chat — save it so a weak model that skipped the
					// write_file call still delivers the artifact it produced.
					//
					// Exception: a compute task whose model never ran a tool. Its
					// numbers came from (unreliable) mental arithmetic, so saving
					// the chatted artifact would present WRONG totals as a finished
					// deliverable (observed: a 12B printed report.html with bad
					// sums into chat). That breaks the honest-finishing promise —
					// skip the save and nudge it to actually compute, else the run
					// finishes StopIncomplete rather than silently wrong.
					chatSaveOK := !(router.NeedsComputation(e.runText) && !ranShell)
					if chatSaveOK {
						if note, ok := e.saveChattedArtifact(ctx, trace, turn, target, stream.Content, &producedFiles, &pendingSyntaxErr, &pendingSyntaxFile); ok {
							messages = append(messages, provider.Message{Role: "user", Content: note})
							continue
						}
					}
					// The task named a file we cannot extract and it is not on disk
					// — the run is finishing without the requested deliverable (seen:
					// a 12B computed totals into an intermediate CSV but never wrote
					// report.html). Nudge it to produce the named file once or twice.
					if allowNudge(trace, turn, nudges, agentcore.NudgeNamedDeliverable) {
						nudge := agentcore.ComposeNudge(agentcore.NudgeInput{
							Kind: agentcore.NudgeNamedDeliverable, Target: target,
							NeedsComputation: router.NeedsComputation(e.runText) && !ranShell,
						})
						messages = append(messages, provider.Message{Role: "user", Content: nudge})
						continue
					}
				}
			}
			if forced := forcedSingleToolName(req.ToolChoice); forced != "" && len(req.Tools) > 0 &&
				allowNudge(trace, turn, nudges, agentcore.NudgeForcedTool) {
				messages = append(messages, provider.Message{
					Role: "user", Content: agentcore.ComposeNudge(agentcore.NudgeInput{
						Kind: agentcore.NudgeForcedTool, ToolName: forced, Task: e.runText,
					}),
				})
				continue
			}
			if strings.TrimSpace(stream.Content) == "" && conversation.AllowRetry(agentcore.RetryEmptyOutput) {
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: "(你上一輪沒有任何輸出。繼續執行：呼叫下一個需要的工具，或用一句話總結結果。)",
				})
				continue
			}
			// "asked to run it but never ran it": the task wanted execution but
			// the model finished with a guessed answer and never called the
			// shell (the 12B "wrote add.py, said 4, never ran it" failure).
			if runIntentRe.MatchString(e.runText) && !ranShell &&
				allowNudge(trace, turn, nudges, agentcore.NudgeRunIntent) {
				messages = append(messages, provider.Message{
					Role: "user", Content: agentcore.ComposeNudge(agentcore.NudgeInput{Kind: agentcore.NudgeRunIntent}),
				})
				continue
			}
			// The task named a deliverable that is not on disk: the model finished
			// (often with a confabulated "I generated report.html") but produced
			// nothing — a weak 12B failure mode on multi-step tasks. Mark it
			// incomplete (exit 1), never a false ✓ completed.
			deliverable := router.TaskTargetFile(e.runText)
			switch {
			case strings.TrimSpace(stream.Content) == "" && !producedFiles:
				res.Stop = StopEmpty // 真空手而歸（沒寫檔也沒話）：誠實標示
			case !deleteTask && deliverable != "" && !fileOnDisk(e.CWD, deliverable):
				// 任務指名的交付檔不在磁碟上（即使寫了中間檔如 calc.py，或模型
				// 用客套話宣稱「已產出 report.html」）：誠實標示未完成，不讓幻覺
				// 成功冒充 ✓ 完成。
				res.Stop = StopIncomplete
			default:
				res.Stop = StopCompleted
			}
			res.FinalText = stream.Content
			res.Messages = messages
			return res, nil
		}
		conversation.ResetRetry(agentcore.RetryEmptyOutput)
		nudges.Reset(agentcore.NudgeToolError, agentcore.NudgeForcedTool)

		wroteNamedDeliverable := false
		wroteStaticWebDeliverable := false
		staticWebDeliverable := ""
		for _, tc := range stream.ToolCalls {
			staticWebVisualCandidate := ""
			plan := router.Classify(e.runText)
			if plan != nil && plan.ID == router.IntentEngineeringFix && !requestAllowsTool(req, tc.Function.Name) {
				allowed := requestToolNames(req)
				execution := e.rejectCall(tc, agentcore.ToolBlocked,
					fmt.Sprintf("ERROR: tool %s is not available at this workflow stage; choose one of: %s", tc.Function.Name, strings.Join(allowed, ", ")),
					trace, evidence, turn)
				messages = append(messages, provider.Message{
					Role: "tool", ToolCallID: tc.ID, Content: execution.LLMContent,
				})
				res.ToolCalls++
				continue
			}
			tc = e.fillMissingPath(tc) // weak models omit file_path; backfill from the task
			if check := verifier.CheckToolCall(tc); !check.Accepted {
				pendingValidationFailure = true
				execution := e.rejectCall(tc, agentcore.ToolBlocked, check.Error, trace, evidence, turn)
				messages = append(messages, provider.Message{
					Role: "tool", ToolCallID: tc.ID, Content: execution.LLMContent,
				})
				res.ToolCalls++
				continue
			} else {
				tc = check.Call
			}
			output, loopType, verification, execution := e.executeCall(ctx, tc, detector, trace, evidence, verifier, turn)
			if tc.Function.Name == "run_shell_command" {
				ranShell = ranShell || execution.Executed
				if verification.Validation {
					pendingValidationFailure = verification.Status == agentcore.VerificationFailed
				} else if execution.Status != agentcore.ToolOK {
					pendingValidationFailure = true
				} else {
					pendingValidationFailure = false
					nudges.Reset(agentcore.NudgeValidationFailure)
				}
			}
			// Self-repair: syntax-check a freshly written/edited code file.
			// On failure, fold the error into the tool result (the model
			// reacts to errors it can see) and remember it so a turn that
			// tries to end without fixing gets nudged below.
			if name := tc.Function.Name; execution.Executed && execution.Status == agentcore.ToolOK &&
				(name == "write_file" || name == "edit") {
				if fp := toolFilePath(tc); fp != "" {
					validation := e.validateArtifact(fp)
					abs := validation.Path
					fileExists := false
					if _, statErr := os.Stat(abs); statErr == nil {
						producedFiles = true // a file exists on disk now
						fileExists = true
					}
					e.emitArtifactValidation(trace, turn, abs, validation)
					agentcore.RecordArtifactValidation(
						evidence, turn, tc.ID, canonicalEvidencePath(e.CWD, abs), validation,
					)
					if deliverable := router.TaskTargetFile(e.runText); deliverable != "" &&
						sameTaskPath(e.CWD, fp, deliverable) && fileOnDisk(e.CWD, deliverable) {
						wroteNamedDeliverable = true
					}
					hadSyntaxErr := pendingSyntaxErr
					validAfterWrite := false
					if !fileExists {
						// The tool may have rejected an escaping or invalid path. Do
						// not let ValidateSyntax's "missing file means no validator"
						// behavior turn a failed write into a completed web artifact.
					} else {
						switch validation.Status {
						case artifactvalidator.StatusRepaired:
							output += "\n(系統套用 artifact repair " + artifactRepairSummary(validation) + "，檢查已通過。)"
							pendingSyntaxErr, pendingSyntaxFile = "", ""
							validAfterWrite = true
						case artifactvalidator.StatusFailed:
							output += "\n⚠️ 語法檢查失敗：" + validation.Error +
								"\n用 edit 修正這個錯誤（不要重寫整個檔案），修好才算完成。"
							if e.MemPalace != nil {
								if hints := e.MemPalace.GetLessons(ctx, e.lastTaskType, validation.Error+" "+e.runText); len(hints) > 0 {
									output += "\n💡 過去相似情況學到的：" + hints[0]
								}
							}
							pendingSyntaxErr = validation.Error
							pendingSyntaxFile = filepath.Base(abs)
						default:
							validAfterWrite = true
							// The model just repaired a syntax error it had made — record
							// it as a MemPalace lesson for future runs.
							if hadSyntaxErr != "" && e.MemPalace != nil && writesLessonFor(e.lastTaskType) {
								e.MemPalace.WriteLesson(ctx, e.lastTaskType,
									"做「"+e.lastTaskType+"」類任務寫程式時曾因語法錯誤需回頭修（例："+
										firstLine(hadSyntaxErr)+"）。產出前先自我檢查括號/引號/標籤收尾完整再結束。")
							}
							pendingSyntaxErr, pendingSyntaxFile = "", ""
						}
					}
					if validAfterWrite && isHTMLPath(abs) {
						switch {
						case e.shouldAutoStaticWebVisualCheck(e.runText):
							staticWebVisualCandidate = abs
						case shouldStopAfterStaticWebDeliverableWrite(e.ModelID, e.runText):
							wroteStaticWebDeliverable = true
							staticWebDeliverable = filepath.Base(abs)
						}
					}
				}
			}
			messages = append(messages, provider.Message{
				Role: "tool", ToolCallID: tc.ID, Content: output,
				ContentParts: append([]provider.MessageContentPart(nil), execution.ContentParts...),
			})
			res.ToolCalls++
			if staticWebVisualCandidate != "" && pendingSyntaxErr == "" {
				visualOutput, visualLoop := e.executeAutoStaticWebVisualCheck(ctx, detector, &messages, staticWebVisualCandidate, trace, evidence, verifier, turn)
				res.ToolCalls++
				if visualLoop != "" {
					res.Stop, res.Loop = StopLoopDetected, visualLoop
					res.Messages = messages
					return res, nil
				}
				if strings.HasPrefix(strings.TrimSpace(visualOutput), "ERROR:") {
					pendingValidationFailure = true
				} else {
					wroteStaticWebDeliverable = true
					staticWebDeliverable = filepath.Base(staticWebVisualCandidate)
				}
			}
			if loopType != "" {
				res.Stop, res.Loop = StopLoopDetected, loopType
				res.Messages = messages
				return res, nil
			}
			if tc.Function.Name == router.ToolShell && verifier.Decision(evidence.Snapshot()).Fresh && e.thinEngineeringValidationComplete(messages) {
				res.Stop = StopCompleted
				res.FinalText = "Validation passed: `" + verifier.Command() + "`."
				res.Messages = messages
				return res, nil
			}
		}
		if wroteNamedDeliverable && pendingSyntaxErr == "" && shouldStopAfterNamedDeliverableWrite(e.ModelID, e.runText) {
			deliverable := router.TaskTargetFile(e.runText)
			res.Stop = StopCompleted
			res.FinalText = "Created " + deliverable + "."
			res.Messages = messages
			return res, nil
		}
		if wroteStaticWebDeliverable && pendingSyntaxErr == "" {
			if staticWebDeliverable == "" {
				staticWebDeliverable = "HTML artifact"
			}
			res.Stop = StopCompleted
			res.FinalText = "Created " + staticWebDeliverable + "."
			res.Messages = messages
			return res, nil
		}

		if e.SessionTokenLimit > 0 && stream.Usage != nil &&
			stream.Usage.PromptTokens > e.SessionTokenLimit {
			res.Stop = StopTokenLimit
			res.Messages = messages
			return res, nil
		}
	}
	if conversation.Snapshot().StopReason == agentcore.LoopCancelled {
		res.Stop = StopCancelled
	} else {
		res.Stop = StopMaxTurns
	}
	res.Messages = messages
	return res, nil
}

func (e *Engine) turnLimit() int {
	if provider.IsMTPLXModel(e.ModelID) {
		return mtplxMaxTurns
	}
	return maxTurns
}

func requestToolNames(req provider.ChatRequest) []string {
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if name := strings.TrimSpace(tool.Function.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func requestAllowsTool(req provider.ChatRequest, name string) bool {
	for _, allowed := range requestToolNames(req) {
		if allowed == name {
			return true
		}
	}
	return false
}

func (e *Engine) thinEngineeringValidationComplete(messages []provider.Message) bool {
	if !e.ThinHarness {
		return false
	}
	plan := router.Classify(e.runText)
	return plan != nil && plan.ID == router.IntentEngineeringFix && router.IsActionComplete(plan, messages)
}

// maybeMaterializeRAG turns an exact example-template hit into a deterministic
// workspace seed. Large examples belong in host memory, not in a 2B model's
// prompt: the model receives only the compact write result and continues with
// validation or focused repair. The experiment flag keeps this behavior opt-in
// until more task families have promotion evidence.
func (e *Engine) maybeMaterializeRAG(ctx context.Context, userText string, fresh bool) ([]provider.Message, bool) {
	if e.requireToolAccount(ctx) != nil {
		return nil, false
	}
	materialize := !e.DisableRAGMaterialize && (e.MaterializeRAG || os.Getenv("MEADOW_RAG_MATERIALIZE") == "1")
	if !materialize || !e.UseRouter || !fresh || e.RAG == nil {
		return nil, false
	}
	plan := router.Classify(router.StripSystemReminder(userText))
	if plan == nil || plan.ID != router.IntentStaticWebArtifact {
		return nil, false
	}
	tmpl := e.RAG.Best(userText, string(plan.ID))
	if tmpl == nil || strings.TrimSpace(tmpl.Content) == "" {
		return nil, false
	}
	target := router.TaskTargetFile(userText)
	if target == "" || !isHTMLPath(target) {
		return nil, false
	}
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(e.CWD, abs)
	}
	if _, err := os.Stat(abs); err == nil {
		return nil, false
	}
	wf, ok := e.Registry.Get("write_file")
	if !ok {
		return nil, false
	}
	execArgs := map[string]any{"file_path": target, "content": tmpl.Content}
	callID := "meadow-rag-materialize-1"
	if e.Events.OnToolCall != nil {
		e.Events.OnToolCall("write_file", map[string]any{"file_path": target, "rag_template": tmpl.Title})
	}
	if e.requireToolAccount(ctx) != nil {
		return nil, false
	}
	if e.Events.OnToolStart != nil {
		e.Events.OnToolStart(agentcore.ToolStart{
			CallID: callID, Tool: "write_file",
			Args: map[string]any{"file_path": target, "rag_template": tmpl.Title},
			Kind: tools.KindEdit,
		})
	}
	started := time.Now()
	if e.requireToolAccount(ctx) != nil {
		return nil, false
	}
	result := wf.Execute(ctx, execArgs)
	if e.Events.OnToolResult != nil {
		e.Events.OnToolResult("write_file", result)
	}
	if e.Events.OnToolComplete != nil {
		durationMS := time.Since(started).Round(time.Millisecond).Milliseconds()
		if durationMS < 1 {
			durationMS = 1
		}
		status := agentcore.ToolOK
		if result.Err != nil {
			status = agentcore.ToolError
		}
		e.Events.OnToolComplete(agentcore.ToolExecution{
			CallID: callID, Tool: "write_file", Args: execArgs, Kind: tools.KindEdit,
			Effect: agentcore.EffectWrite, Status: status, Executed: true,
			LLMContent: result.LLMContent, Display: result.Display, Diff: result.Diff,
			Err: result.Err, DurationMS: durationMS,
		})
	}
	if result.Err != nil {
		return nil, false
	}
	compactArgs, _ := json.Marshal(map[string]any{"file_path": target})
	toolResult := "Core RAG materialized the verified template \"" + tmpl.Title + "\". " + result.LLMContent
	note := "(Core RAG 已把命中的已驗證範例直接寫入 " + target + "，完整範例沒有塞進模型上下文。" +
		"不要重寫檔案；繼續原始任務，先執行指定 validator，只有失敗時才 read/edit。原始任務：" + userText + ")"
	return []provider.Message{
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID:   callID,
				Type: "function",
				Function: provider.FunctionCall{
					Name:      "write_file",
					Arguments: string(compactArgs),
				},
			}},
		},
		{Role: "tool", ToolCallID: callID, Content: toolResult},
		{Role: "user", Content: note},
	}, true
}

// executeCall runs the approval gate and the tool; the returned string is
// always what the model should see (results and denials alike).
// maybeScaffold pre-creates the static-web skeleton on disk for a fresh
// "create a game/site" request, returning the note that tells the model to
// edit-fill it (or "" when not applicable). fresh is false on resume, where
// files may already exist. The skeleton is written via the write_file tool
// so the workspace marks it read (edit's prior-read check then passes) and
// approval is bypassed — this is the backend setting up, not the model.
func (e *Engine) maybeScaffold(ctx context.Context, userText string, fresh bool) string {
	if e.requireToolAccount(ctx) != nil {
		return ""
	}
	// Off by default: a FIXED skeleton forces the model's JS to conform to the
	// backend's HTML structure (canvas id, score element), but local models
	// generate JS for their own imagined structure → id mismatch → a
	// syntactically-valid but broken game. One-shot write (the model writes
	// HTML+JS together, internally consistent) is 4/4 on 12B and richer, so
	// it stays the default. Kept behind a flag for the future orchestrator,
	// where the plan would scaffold BOTH structure and logic coherently.
	if os.Getenv("MEADOW_SCAFFOLD") != "1" {
		return ""
	}
	if !e.UseRouter || !fresh {
		return ""
	}
	plan := router.Classify(router.StripSystemReminder(userText))
	if plan == nil || plan.ID != router.IntentStaticWebArtifact {
		return ""
	}
	name, skeleton := router.StaticWebScaffold(userText)
	abs := name
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(e.CWD, abs)
	}
	if _, err := os.Stat(abs); err == nil {
		return "" // 已存在，不覆蓋
	}
	wf, ok := e.Registry.Get("write_file")
	if !ok {
		return ""
	}
	if e.requireToolAccount(ctx) != nil {
		return ""
	}
	if res := wf.Execute(ctx, map[string]any{"file_path": name, "content": skeleton}); res.Err != nil {
		return ""
	}
	res := fmt.Sprintf("(系統已自動建立 %s，內含空的 <style>（佔位 %s）與 <script>（佔位 %s）。"+
		"請用 edit 工具把佔位換成完整內容：先把 %s 換成完整的程式/遊戲邏輯，"+
		"再把 %s 換成樣式。需要時可調整 <body> 結構。不要重新建立或重寫整個檔案。)",
		name, router.StylePlaceholder, router.ScriptPlaceholder,
		router.ScriptPlaceholder, router.StylePlaceholder)
	return res
}

// hasAnyToolCall reports whether the model has made any tool call yet — used
// to inject the RAG template only on the first action turn.
func hasAnyToolCall(msgs []provider.Message) bool {
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// writesLessonFor limits MemPalace write-back to the tasks that produce a code
// artifact, where a syntax-repair lesson is actually reusable: web pages, code
// fixes, config files, and plain code writes. Shell / copy / mkdir tasks never
// produce a syntax-checked file, so recording a lesson for them would just fill
// long-term memory with generic reminders (the G3 "web-task limited" tradeoff,
// widened to all code artifacts since the syntax gate already excludes non-code).
func writesLessonFor(taskType string) bool {
	switch taskType {
	case string(router.IntentStaticWebArtifact),
		string(router.IntentEngineeringFix),
		string(router.IntentConfigArtifact),
		string(router.IntentSimpleWrite):
		return true
	}
	return false
}

// consolidate writes a MemPalace lesson from how a finished run ended (F1):
// a failure to avoid next time, or a non-trivial success worth keeping. Gated to
// code-artifact tasks and conservative (runLesson returns "" for trivial
// outcomes) so memory does not fill with noise; fail-open.
func (e *Engine) consolidate(ctx context.Context, res *RunResult) {
	if e.MemPalace == nil || e.lastTaskType == "" || !writesLessonFor(e.lastTaskType) {
		return
	}
	if lesson := runLesson(res.Stop, res.Turns, res.ToolCalls); lesson != "" {
		e.MemPalace.WriteLesson(ctx, e.lastTaskType, lesson)
	}
}

// runLesson maps a finished run to a concise, reusable lesson, or "" when the
// outcome taught nothing worth recording (the common case).
func runLesson(stop StopReason, turns, toolCalls int) string {
	switch stop {
	case StopLoopDetected:
		return "上次同類任務陷入重複迴圈。每一輪結束時用一句話總結已完成的步驟，避免重做。"
	case StopEmpty:
		return "上次同類任務多次提示後仍零產出（卡住）。要產檔就直接用 write_file 寫入，不要只在對話裡描述內容。"
	case StopCompleted:
		// Only a genuinely multi-step success is worth a note; a one-shot
		// completion taught nothing reusable.
		if turns >= 3 && toolCalls >= 3 {
			return "這類任務需要多步迭代才完成（讀→改→驗）。用 edit 分階段修，不要一次重寫整個檔案。"
		}
	}
	return ""
}

// combineMemory folds the proven approach (RAG) and the lessons learned
// (MemPalace) into ONE memory block the small model reads as a single context,
// instead of two separate prepends (F4). No fuzzy dedup — that risks dropping
// independent lessons; just an ordered join with RAG leading for prefix-cache
// stability. Returns "" when there is nothing to inject.
func combineMemory(tmpl *rag.Template, lessons []string) string {
	ragPart := rag.Inject(tmpl)                              // "" when tmpl is nil
	memPart := mempalace.FormatForSystemPrompt(lessons, nil) // "" when no lessons
	switch {
	case ragPart != "" && memPart != "":
		return ragPart + "\n\n" + memPart
	case ragPart != "":
		return ragPart
	default:
		return memPart
	}
}

func (e *Engine) validateArtifact(path string) artifactvalidator.Result {
	registry := e.ArtifactValidators
	if registry == nil {
		registry = artifactvalidator.DefaultRegistry()
	}
	// Repair is fail-closed. A missing or newly introduced intent must never
	// silently gain permission to rewrite source after a model edit.
	var capabilities []artifactvalidator.Capability
	if plan := router.Classify(router.StripSystemReminder(e.runText)); plan != nil {
		switch plan.ID {
		case router.IntentSimpleWrite, router.IntentShellScript, router.IntentConfigArtifact,
			router.IntentOfficeArtifact:
			capabilities = []artifactvalidator.Capability{artifactvalidator.CapabilityMechanicalRepair}
		case router.IntentEngineeringFix:
			// SWE source changes are model-owned: validate and return the native
			// syntax error, but never rewrite project code deterministically.
			capabilities = nil
		case router.IntentStaticWebArtifact:
			capabilities = []artifactvalidator.Capability{
				artifactvalidator.CapabilityMechanicalRepair,
				artifactvalidator.CapabilityWebRuntimeRepair,
			}
		}
	}
	resolved, err := resolveWorkspacePhysicalPath(e.CWD, path)
	if err != nil {
		return artifactvalidator.Result{
			Status: artifactvalidator.StatusFailed, Path: path,
			ValidatorID: "workspace_path", Error: err.Error(), EnabledCapabilities: capabilities,
		}
	}
	return registry.Validate(artifactvalidator.Request{Path: resolved, Capabilities: capabilities})
}

func (e *Engine) emitArtifactValidation(trace *agenttrace.Emitter, turn int, path string, result artifactvalidator.Result) {
	enabled := make([]string, 0, len(result.EnabledCapabilities))
	for _, capability := range result.EnabledCapabilities {
		enabled = append(enabled, string(capability))
	}
	trace.Emit(agenttrace.EventArtifactValidated, turn, map[string]any{
		"path":                 canonicalEvidencePath(e.CWD, path),
		"status":               string(result.Status),
		"validator_id":         result.ValidatorID,
		"repair_id":            result.RepairID,
		"capability":           string(result.Capability),
		"enabled_capabilities": enabled,
	})
}

func artifactRepairSummary(result artifactvalidator.Result) string {
	summary := result.RepairID
	if summary == "" {
		summary = string(result.Capability)
	}
	if result.Detail != "" {
		if summary != "" {
			summary += ": "
		}
		summary += result.Detail
	}
	return summary
}

func allowNudge(trace *agenttrace.Emitter, turn int, policy *agentcore.NudgePolicy, kind agentcore.NudgeKind) bool {
	decision := policy.Try(kind)
	trace.Emit(agenttrace.EventPolicyNudge, turn, map[string]any{
		"kind":    string(decision.Kind),
		"allowed": decision.Allowed,
		"attempt": decision.Attempt,
		"limit":   decision.Limit,
	})
	return decision.Allowed
}

// saveChattedArtifact extracts a fenced artifact from chatted content and writes
// it to target — capturing a file a weak model only PRINTED instead of writing
// (a common 12B/26B failure mode). Returns the note to show the model + ok=false
// when nothing is extractable or the write fails. The pointers set the caller's
// loop state (producedFiles + a pending syntax error to repair).
func (e *Engine) saveChattedArtifact(ctx context.Context, trace *agenttrace.Emitter, turn int, target, content string, producedFiles *bool, pendingSyntaxErr, pendingSyntaxFile *string) (string, bool) {
	if e.requireToolAccount(ctx) != nil {
		return "", false
	}
	body, found := router.ExtractArtifact(content)
	if !found {
		return "", false
	}
	wf, has := e.Registry.Get("write_file")
	if !has {
		return "", false
	}
	if e.requireToolAccount(ctx) != nil {
		return "", false
	}
	if r := wf.Execute(ctx, map[string]any{"file_path": target, "content": body}); r.Err != nil {
		return "", false
	}
	*producedFiles = true
	note := "(偵測到你把內容寫在對話裡，系統已自動存成 " + target + "。)"
	validation := e.validateArtifact(target)
	abs := validation.Path
	e.emitArtifactValidation(trace, turn, abs, validation)
	switch validation.Status {
	case artifactvalidator.StatusRepaired:
		note = "(已自動存成 " + target + "，並套用 artifact repair " + artifactRepairSummary(validation) + "，檢查已通過。)"
	case artifactvalidator.StatusFailed:
		note = "(已自動存成 " + target + "，但語法有誤：" + validation.Error + " 用 edit 修正，不要重寫整個檔案。)"
		*pendingSyntaxErr, *pendingSyntaxFile = validation.Error, filepath.Base(abs)
	}
	return note, true
}

// fillMissingPath backfills a write_file call's file_path from the task when the
// model produced content but omitted the path (observed live: a 12B called
// write_file(content=…) with no file_path → "path is empty" → the artifact lost).
// Only fires when there is content to write and the task actually names a file.
// Empty meadow_browser_visual_check calls reuse the latest saved artifact.
func (e *Engine) fillMissingPath(tc provider.ToolCall) provider.ToolCall {
	switch tc.Function.Name {
	case "write_file":
		if toolFilePath(tc) != "" {
			return tc
		}
		target := router.TaskTargetFile(e.runText)
		if target == "" {
			return tc
		}
		var args map[string]any
		if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
			return tc
		}
		if c, ok := args["content"].(string); !ok || strings.TrimSpace(c) == "" {
			return tc // nothing to write — don't invent a path
		}
		args["file_path"] = target
		if b, err := json.Marshal(args); err == nil {
			tc.Function.Arguments = string(b)
		}
		return tc
	case "meadow_browser_visual_check":
		if visualCheckHasTarget(tc) {
			return tc
		}
		target := strings.TrimSpace(e.lastSavedArtifactPath)
		if target == "" {
			target = router.TaskTargetFile(e.runText)
		}
		if target == "" {
			return tc
		}
		args := map[string]any{}
		if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
			args = map[string]any{}
		}
		args["file_path"] = target
		if b, err := json.Marshal(args); err == nil {
			tc.Function.Arguments = string(b)
		}
		return tc
	default:
		return tc
	}
}

func visualCheckHasTarget(tc provider.ToolCall) bool {
	if tools.SanitizeToolPath(toolFilePath(tc)) != "" {
		return true
	}
	var args struct {
		URL string `json:"url"`
	}
	if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
		return false
	}
	return strings.TrimSpace(args.URL) != ""
}

// fileOnDisk reports whether name (resolved against cwd) already exists.
func fileOnDisk(cwd, name string) bool {
	abs, err := resolveWorkspacePhysicalPath(cwd, name)
	if err != nil {
		return false
	}
	_, err = os.Stat(abs)
	return err == nil
}

func resolveWorkspacePhysicalPath(cwd, name string) (string, error) {
	workspace, err := tools.NewWorkspace(cwd)
	if err != nil {
		return "", err
	}
	return workspace.Resolve(name)
}

func forcedSingleToolName(toolChoice any) string {
	obj, ok := toolChoice.(map[string]any)
	if !ok {
		return ""
	}
	fn, ok := obj["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return strings.TrimSpace(name)
}

func isDeleteTask(userText string) bool {
	plan := router.Classify(router.StripSystemReminder(userText))
	return plan != nil && plan.ID == router.IntentDeleteGenerated
}

// shouldRetrieveMemory is a cheap metacognition gate (F5): skip memory retrieval
// for the trivial / mechanical intents where injected knowledge only adds prompt
// noise (make a directory, run a shell line, copy or append a file). Every other
// intent can plausibly benefit, so the default is to retrieve — a miss costs more
// than a little noise.
func shouldRetrieveMemory(intent router.IntentID) bool {
	switch intent {
	case router.IntentDirectoryCreate,
		router.IntentShellScript,
		router.IntentSimpleCopy,
		router.IntentSimpleAppend,
		router.IntentDeleteGenerated:
		return false
	}
	return true
}

// firstLine returns the first non-empty line of s, rune-capped so a multi-line
// compiler error collapses to a concise one-line lesson.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if r := []rune(t); len(r) > 80 {
			return string(r[:80])
		}
		return t
	}
	return ""
}

func lastToolFailure(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		switch msgs[i].Role {
		case "tool":
			t := strings.TrimSpace(msgs[i].Content)
			if strings.HasPrefix(t, "ERROR:") || strings.Contains(t, "(exit code ") {
				return t
			}
			return ""
		case "user":
			return ""
		}
	}
	return ""
}

func ignorableToolArgumentFailure(msg string) bool {
	t := strings.ToLower(msg)
	return strings.Contains(t, "parameter is required") ||
		strings.Contains(t, "path is empty")
}

// injectRAGSystem puts the template reference at the VERY FRONT of the
// system content. Order is load-bearing: the template is static, so leading
// with it gives every request that uses it an identical token prefix —
// vllm's prefix cache (enabled + disk-persisted in our launch config) then
// pays the template's prefill once and reuses the KV afterwards. Measured:
// this server config prefills at only ~16-19 tok/s, so a 5KB template
// appended at a varying position cost ~2 minutes on every request.
func injectRAGSystem(msgs []provider.Message, inject string) []provider.Message {
	if inject == "" {
		return msgs
	}
	if len(msgs) > 0 && msgs[0].Role == "system" {
		msgs[0].Content = inject + "\n\n" + msgs[0].Content
		return msgs
	}
	return append([]provider.Message{{Role: "system", Content: inject}}, msgs...)
}

// toolFilePath pulls the target path from a write_file/edit call, tolerating
// the path/filename aliases and keeping only the first line (a model that
// glued a comment onto the filename is sanitized the same way pathArg is).
func toolFilePath(tc provider.ToolCall) string {
	var args struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Filename string `json:"filename"`
	}
	if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
		return ""
	}
	p := args.FilePath
	if p == "" {
		p = args.Path
	}
	if p == "" {
		p = args.Filename
	}
	if i := strings.IndexAny(p, "\n\r\x00"); i >= 0 {
		p = p[:i]
	}
	return normalizeToolPath(p)
}

func normalizeToolPath(p string) string {
	p = tools.SanitizeToolPath(p)
	if p == "" {
		return ""
	}
	slash := strings.ReplaceAll(p, `\`, `/`)
	for _, prefix := range []string{"/home/user/", "/Users/user/", "/mnt/data/"} {
		if strings.HasPrefix(slash, prefix) {
			return filepath.Base(slash)
		}
	}
	if len(slash) >= 3 && slash[1] == ':' && slash[2] == '/' && isASCIIAlpha(slash[0]) {
		return filepath.Base(slash)
	}
	return slash
}

// canonicalEvidencePath gives one stable identity to relative, /workspace and
// physical paths for the same file. Reports stay independent of temporary
// checkout names, and a later edit reliably invalidates an earlier pass.
func canonicalEvidencePath(cwd, path string) string {
	path = normalizeToolPath(path)
	if path == "" || strings.TrimSpace(cwd) == "" {
		return path
	}
	slash := filepath.ToSlash(path)
	for _, alias := range []string{"/workspace", "/workdir"} {
		if slash == alias {
			path = "."
			break
		}
		if strings.HasPrefix(slash, alias+"/") {
			path = strings.TrimPrefix(slash, alias+"/")
			break
		}
	}
	root := filepath.Clean(cwd)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	physical := filepath.Clean(path)
	if !filepath.IsAbs(physical) {
		physical = filepath.Join(root, physical)
	}
	if resolved, err := filepath.EvalSymlinks(physical); err == nil {
		physical = resolved
	} else if parent, err := filepath.EvalSymlinks(filepath.Dir(physical)); err == nil {
		physical = filepath.Join(parent, filepath.Base(physical))
	}
	if rel, err := filepath.Rel(root, physical); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if rel == "." {
			return "/workspace"
		}
		return filepath.ToSlash(filepath.Join("/workspace", rel))
	}
	return filepath.Clean(physical)
}

func sameTaskPath(cwd, a, b string) bool {
	left := canonicalEvidencePath(cwd, a)
	right := canonicalEvidencePath(cwd, b)
	if left == "" || right == "" {
		return false
	}
	return left == right
}

func shouldStopAfterNamedDeliverableWrite(modelID, userText string) bool {
	if !provider.IsQwythosModel(modelID) {
		return false
	}
	if router.TaskTargetFile(userText) == "" {
		return false
	}
	if plan := router.Classify(router.StripSystemReminder(userText)); plan != nil &&
		plan.ID == router.IntentStaticWebArtifact &&
		os.Getenv("MEADOW_SKIP_STATIC_WEB_VISUAL_CHECK") != "1" {
		return false
	}
	return router.NamedValidationCommand(userText) == "" &&
		!router.NeedsComputation(userText) &&
		!runIntentRe.MatchString(userText) &&
		!postWriteValidationRe.MatchString(userText)
}

func shouldStopAfterStaticWebDeliverableWrite(modelID, userText string) bool {
	if os.Getenv("MEADOW_SKIP_STATIC_WEB_VISUAL_CHECK") != "1" {
		return false
	}
	if !provider.IsQwythosModel(modelID) {
		return false
	}
	plan := router.Classify(router.StripSystemReminder(userText))
	if plan == nil || plan.ID != router.IntentStaticWebArtifact {
		return false
	}
	return router.NamedValidationCommand(userText) == "" &&
		!router.NeedsComputation(userText) &&
		!runIntentRe.MatchString(userText) &&
		!postWriteValidationRe.MatchString(userText)
}

func (e *Engine) shouldAutoStaticWebVisualCheck(userText string) bool {
	if os.Getenv("MEADOW_SKIP_STATIC_WEB_VISUAL_CHECK") == "1" {
		return false
	}
	if e.Registry == nil {
		return false
	}
	if _, ok := e.Registry.Get(router.ToolVisual); !ok {
		return false
	}
	if router.NamedValidationCommand(userText) != "" {
		return false
	}
	plan := router.Classify(router.StripSystemReminder(userText))
	return plan != nil && plan.ID == router.IntentStaticWebArtifact
}

func (e *Engine) executeAutoStaticWebVisualCheck(ctx context.Context, detector *loopDetector, messages *[]provider.Message, abs string, trace *agenttrace.Emitter, evidence *agentcore.EvidenceLedger, verifier *agentcore.Verifier, turn int) (string, LoopType) {
	filePath := filepath.Base(abs)
	if e.CWD != "" {
		if rel, err := filepath.Rel(e.CWD, abs); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
			filePath = rel
		}
	}
	args := autoStaticWebVisualArgs(filePath)
	tc := provider.ToolCall{
		ID:   "auto_static_web_visual",
		Type: "function",
		Function: provider.FunctionCall{
			Name:      router.ToolVisual,
			Arguments: args,
		},
	}
	*messages = append(*messages, provider.Message{
		Role:      "assistant",
		ToolCalls: []provider.ToolCall{tc},
	})
	output, loopType, _, execution := e.executeCall(ctx, tc, detector, trace, evidence, verifier, turn)
	*messages = append(*messages, provider.Message{
		Role:         "tool",
		ToolCallID:   tc.ID,
		Content:      output,
		ContentParts: append([]provider.MessageContentPart(nil), execution.ContentParts...),
	})
	return output, loopType
}

// autoStaticWebVisualArgs intentionally omits require_canvas. The Browser tool
// derives that requirement from the saved artifact: canvas/Three.js pages must
// expose a canvas, while ordinary DOM/CSS pages are validated for loading and
// runtime diagnostics without being forced into an irrelevant repair loop.
func autoStaticWebVisualArgs(filePath string) string {
	return provider.MarshalArgs(map[string]any{"file_path": filePath})
}

func isHTMLPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm")
}

func isASCIIAlpha(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

func (e *Engine) newVerifier(userText string) *agentcore.Verifier {
	plan := router.Classify(router.StripSystemReminder(userText))
	return agentcore.NewVerifier(agentcore.ValidationRequest{
		CWD:                  e.CWD,
		ExplicitCommand:      router.NamedValidationCommand(userText),
		RepositoryValidation: plan != nil && plan.ID == router.IntentEngineeringFix,
		ShellTool:            router.ToolShell,
	})
}

func (e *Engine) executeCall(ctx context.Context, tc provider.ToolCall, detector *loopDetector, trace *agenttrace.Emitter, evidence *agentcore.EvidenceLedger, verifier *agentcore.Verifier, turn int) (output string, loopType LoopType, verification agentcore.VerificationObservation, execution agentcore.ToolExecution) {
	loopType = detector.AddToolCall(tc.Function.Name, tc.Function.Arguments)
	check := verifier.CheckToolCall(tc)
	validation := check.Validation && check.Accepted
	trace.Emit(agenttrace.EventToolRequested, turn, map[string]any{
		"tool": tc.Function.Name, "call_id": tc.ID, "source": "model",
	})
	executor := agentcore.ToolExecutor{
		Registry:       e.Registry,
		RequireAccount: e.RequireToolAccount,
		Authorize:      e.authorizeToolCall,
		BeforeExecute: func(name string, args map[string]any) {
			e.backupBeforeMutate(name, args)
		},
		OnToolCall: e.Events.OnToolCall,
		OnToolStart: func(start agentcore.ToolStart) {
			if e.Events.OnToolStart != nil {
				e.Events.OnToolStart(start)
			}
			if validation {
				trace.Emit(agenttrace.EventVerificationStarted, turn, map[string]any{
					"tool": start.Tool, "call_id": start.CallID,
					"validator": verifier.Command(), "source": string(verifier.Source()),
				})
			}
			trace.Emit(agenttrace.EventToolStarted, turn, map[string]any{
				"tool": start.Tool, "call_id": start.CallID, "effect": string(agentcore.EffectForKind(start.Kind)),
			})
		},
		OnToolResult:   e.Events.OnToolResult,
		OnToolComplete: e.Events.OnToolComplete,
	}
	execution = executor.Execute(ctx, tc, turn)
	verification = verifier.Observe(execution)
	trace.Emit(agenttrace.EventToolFinished, turn, map[string]any{
		"tool": tc.Function.Name, "call_id": tc.ID, "status": string(execution.Status),
		"effect": string(execution.Effect), "executed": execution.Executed, "duration_ms": execution.DurationMS,
	})
	if verification.Validation && execution.Executed {
		trace.Emit(agenttrace.EventVerificationFinished, turn, map[string]any{
			"tool": tc.Function.Name, "call_id": tc.ID, "status": string(execution.Status),
			"verdict": string(verification.Status),
		})
	}
	exitCode, hasExitCode := traceToolExecutionExitCode(execution)
	if verification.Validation {
		exitCode, hasExitCode = verification.ExitCode, verification.HasExitCode
	}
	if evidence != nil {
		evidence.Record(agentcore.ToolObservation{
			Turn: turn, CallID: tc.ID, Tool: tc.Function.Name,
			Status: execution.Status, Path: canonicalEvidencePath(e.CWD, toolFilePath(tc)),
			MutatesFile: execution.Effect == agentcore.EffectWrite,
			Validation:  verification.Validation, ValidatorID: verification.ValidatorID,
			ExitCode: exitCode, HasExitCode: hasExitCode, Verification: verification.Status,
			DurationMS: execution.DurationMS,
		})
	}
	if execution.Status == agentcore.ToolOK && tc.Function.Name == "write_file" {
		if p := tools.SanitizeToolPath(toolFilePath(tc)); p != "" {
			e.lastSavedArtifactPath = p
		}
	}
	return execution.LLMContent, loopType, verification, execution
}

func (e *Engine) rejectCall(tc provider.ToolCall, status agentcore.ToolStatus, content string, trace *agenttrace.Emitter, evidence *agentcore.EvidenceLedger, turn int) agentcore.ToolExecution {
	trace.Emit(agenttrace.EventToolRequested, turn, map[string]any{
		"tool": tc.Function.Name, "call_id": tc.ID, "source": "model",
	})
	executor := agentcore.ToolExecutor{
		Registry: e.Registry, OnToolCall: e.Events.OnToolCall,
		OnToolComplete: e.Events.OnToolComplete,
	}
	execution := executor.Reject(tc, turn, status, content)
	trace.Emit(agenttrace.EventToolFinished, turn, map[string]any{
		"tool": tc.Function.Name, "call_id": tc.ID, "status": string(execution.Status),
		"effect": string(execution.Effect), "executed": false, "duration_ms": execution.DurationMS,
	})
	if evidence != nil {
		evidence.Record(agentcore.ToolObservation{
			Turn: turn, CallID: tc.ID, Tool: tc.Function.Name,
			Status: execution.Status, Path: canonicalEvidencePath(e.CWD, toolFilePath(tc)),
			MutatesFile: execution.Effect == agentcore.EffectWrite,
			DurationMS:  execution.DurationMS,
		})
	}
	return execution
}

func (e *Engine) authorizeToolCall(ctx context.Context, name string, args map[string]any, kind tools.Kind) agentcore.Authorization {
	if kind != tools.KindReadOnly && kind != tools.KindInteract && !modelmgr.AllowsGeneralTaskExecution(e.ModelID) {
		return agentcore.Authorization{
			Status:     agentcore.ToolBlocked,
			LLMContent: modelmgr.ExecutionBlockedMessage(e.ModelID),
		}
	}
	switch classify(e.Mode, kind) {
	case verdictPlanBlocked:
		return agentcore.Authorization{
			Status:     agentcore.ToolBlocked,
			LLMContent: "ERROR: plan mode blocked this non-read-only tool call. Present the plan to the user instead.",
		}
	case verdictNeedsConfirmation:
		// Destructive calls are always one-shot approvals. A prior "always"
		// decision for the same shell tool may cover ordinary execution, but it
		// must never authorize a later delete/reset/wipe without a fresh prompt.
		if kind == tools.KindDestructive || !e.approveAlways[name] {
			if e.Confirm == nil {
				return agentcore.Authorization{
					Status:     agentcore.ToolConfirmationRequired,
					LLMContent: "ERROR: this call requires user confirmation, and the session is non-interactive. It was not executed.",
				}
			}
			decision, err := e.Confirm(ctx, ConfirmRequest{ToolName: name, Args: args, Kind: kind})
			if err != nil || decision == DecisionDeny {
				return agentcore.Authorization{
					Status:     agentcore.ToolDenied,
					LLMContent: "User declined this tool call. Ask for direction or try another approach.",
				}
			}
			if decision == DecisionApproveAlways && kind != tools.KindDestructive {
				e.approveAlways[name] = true
			}
		}
	}
	return agentcore.Authorization{Allowed: true, Status: agentcore.ToolOK}
}

func (e *Engine) requireToolAccount(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.RequireToolAccount != nil {
		return e.RequireToolAccount(ctx)
	}
	return nil
}

func traceToolStatus(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func traceMaxTokens(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func traceToolExitCode(result tools.Result) (int, bool) {
	var code int
	if _, err := fmt.Sscanf(strings.TrimSpace(result.Display), "exit %d", &code); err == nil {
		return code, true
	}
	return 0, false
}

func traceToolExecutionExitCode(execution agentcore.ToolExecution) (int, bool) {
	return traceToolExitCode(tools.Result{Display: execution.Display})
}

func traceModelErrorClass(err error) string {
	switch {
	case errors.Is(err, provider.ErrMalformedToolCall):
		return "malformed_tool_call"
	case errors.Is(err, provider.ErrThoughtBurn):
		return "thought_burn"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "provider_error"
	}
}

// backupBeforeMutate snapshots an existing file before edit/write_file
// overwrites it, so a weak model's botched edit is always recoverable.
// Backups land in .meadow/backups/<base>.<ns>.bak; it is best-effort and
// never blocks the edit. New files (nothing to lose) are skipped.
func (e *Engine) backupBeforeMutate(name string, args map[string]any) {
	if name != "edit" && name != "write_file" {
		return
	}
	fp := ""
	for _, k := range []string{"file_path", "path", "filename"} {
		if s, ok := args[k].(string); ok && strings.TrimSpace(s) != "" {
			fp = s
			break
		}
	}
	if fp == "" {
		return
	}
	abs, resolveErr := resolveWorkspacePhysicalPath(e.CWD, fp)
	if resolveErr != nil {
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return // new file, or a directory — nothing to back up
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return
	}
	dir := filepath.Join(e.CWD, ".meadow", "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s.%d.bak", filepath.Base(abs), time.Now().UnixNano()))
	_ = os.WriteFile(dst, data, 0o644)
}

// buildRequest assembles one turn's request, applying the three-state
// local-Gemma-4 routing when enabled. The canonical history is never
// mutated — only the outgoing request is rewritten, exactly like the TS
// provider's buildRequest.
func (e *Engine) buildRequest(ctx context.Context, messages []provider.Message) provider.ChatRequest {
	return e.buildRequestWithVerifier(ctx, messages, e.newVerifier(e.runText))
}

func (e *Engine) buildRequestWithVerifier(ctx context.Context, messages []provider.Message, verifier *agentcore.Verifier) provider.ChatRequest {
	allTools := e.toolSchemas(e.Registry.Names())
	base := provider.ChatRequest{Model: e.ModelID, Messages: e.withSystem(messages), Tools: allTools}

	if !e.UseRouter || !provider.IsMeadowLocalModel(e.ModelID) {
		return base
	}
	// Intent/RAG anchor to the RUN's original ask, not the latest user-role
	// message: nudges (empty-turn, thought-burn, staged) are synthetic
	// user messages, and classifying THEM hijacked the plan mid-run — the
	// template stopped injecting and the intent could flip entirely.
	userText := router.StripSystemReminder(e.runText)
	if userText == "" {
		userText = router.StripSystemReminder(lastUserText(messages))
	}
	hasToolHistory := false
	for _, m := range messages {
		if m.Role == "tool" {
			hasToolHistory = true
			break
		}
	}

	if !hasToolHistory && router.ShouldUseMinimalChat(userText) {
		return localSafety(provider.ChatRequest{
			Model:    e.ModelID,
			Messages: router.MinimalChatMessages(messages, e.ModelID, e.CWD),
		})
	}
	if !router.ShouldRequireTool(userText) {
		return localSafety(base)
	}
	plan, selected := router.SelectIntentTools(userText, e.Registry.Names())
	if plan == nil {
		return localSafety(base)
	}
	if !hasToolHistory {
		shadow, promotedPlan, promotedTools := e.evaluateRouteShadow(ctx, userText, plan, selected, true)
		if shadow.Promoted {
			plan, selected = promotedPlan, promotedTools
		}
		if e.Events.OnRouteShadow != nil {
			e.Events.OnRouteShadow(shadow)
		}
	}
	if router.IsActionComplete(plan, messages) {
		mt := 128
		return localSafety(provider.ChatRequest{
			Model:     e.ModelID,
			Messages:  router.FinalMessages(messages, e.ModelID, plan),
			MaxTokens: &mt,
		})
	}
	turnTools := router.SelectTurnTools(plan, selected, messages, e.CWD)
	// Open-ended SWE prompts usually ask for focused tests without spelling out
	// a command. Once the workflow reaches validation, turn repository structure
	// into an exact command. This gives the Agent Kernel adapter the same explicit
	// contract it was trained on and keeps semantic command invention out of it.
	if plan.ID == router.IntentEngineeringFix && len(turnTools) == 1 && turnTools[0] == router.ToolShell && verifier.Source() == agentcore.ValidationSourceRepository {
		userText += "\n\nRepository validator selected by Meadow Core. Run exactly `" + verifier.Command() + "`."
	}
	actionMsgs := router.ActionMessages(messages, e.ModelID, e.CWD, turnTools, plan, userText)
	e.lastTaskType = string(plan.ID) // MemPalace task_type for this turn's self-repair
	maxTokOverride := 0
	// On the FIRST action turn (before any tool call), hand a weaker local model
	// what it lacks — done once so the big blocks are not re-sent every turn:
	//   RAG       = proven STRUCTURE for this create intent (template).
	//   MemPalace = LESSONS learned across runs + a per-task max_tok ceiling.
	// Memory retrieval is metacognition-gated (F5): a trivial/mechanical task
	// gains nothing from injected knowledge, so skip the retrieval (and its prompt
	// noise) for those. When we do retrieve, RAG (the proven approach) and
	// MemPalace (lessons learned) are COMBINED into one block (F4) the small model
	// reads as a single memory context — RAG leads so the larger static template
	// keeps the prefix-cache position. Both fail-open when unset.
	if !hasAnyToolCall(messages) && shouldRetrieveMemory(plan.ID) {
		taskType := string(plan.ID)
		if e.ThinHarness {
			if e.RAG != nil {
				topK := e.RAGTopK
				if topK < 1 {
					topK = 3
				}
				matches := e.RAG.Search(userText, taskType, topK)
				e.emitRAGTrace(userText, taskType, matches)
				if compact := rag.CompactEvidence(matches, 2200); compact != "" {
					actionMsgs = injectRAGSystem(actionMsgs, compact)
				}
			}
		} else {
			var lessons []string
			if e.MemPalace != nil {
				// userText (the run's original ask) is the semantic query, so the shim
				// recalls by MEANING across task_types, not just an exact-key lookup.
				lessons = e.MemPalace.GetLessons(ctx, taskType, userText)
				maxTokOverride = e.MemPalace.GetMaxTokOverride(ctx, taskType)
			}
			var tmpl *rag.Template
			if e.RAG != nil {
				matches := e.RAG.Search(userText, taskType, 1)
				e.emitRAGTrace(userText, taskType, matches)
				if len(matches) > 0 {
					tmpl = matches[0].Template
				}
			}
			if combined := e.combineMemoryForPrefill(tmpl, lessons); combined != "" {
				actionMsgs = injectRAGSystem(actionMsgs, combined)
			}
		}
	}
	actionMsgs = e.shapeActionMessagesForPrefill(actionMsgs)
	pf := false
	req := provider.ChatRequest{
		Model:             e.ModelID,
		Messages:          actionMsgs,
		Tools:             e.toolSchemas(turnTools),
		ToolChoice:        router.ToolChoice(turnTools),
		ParallelToolCalls: &pf,
	}
	if maxTokOverride > 0 {
		req.MaxTokens = &maxTokOverride
	}
	return localSafety(req)
}

func (e *Engine) emitRAGTrace(userText, intent string, matches []rag.Match) {
	if e.Events.OnRAG == nil {
		return
	}
	trace := RAGTrace{UserText: userText, Intent: intent}
	for _, match := range matches {
		if match.Template == nil {
			continue
		}
		trace.Matches = append(trace.Matches, RAGTraceMatch{
			Title:       match.Template.Title,
			Source:      match.Template.Source,
			Version:     match.Template.Version,
			Updated:     match.Template.Updated,
			Score:       match.Score,
			KeywordHits: match.KeywordHits,
			IntentMatch: match.IntentMatch,
		})
	}
	e.Events.OnRAG(trace)
}

func (e *Engine) emitRouteShadow(ctx context.Context, userText string, plan *router.Plan, selected []string) {
	if e.RoutePredictor == nil && e.Events.OnRouteShadow == nil {
		return
	}
	rec, _, _ := e.evaluateRouteShadow(ctx, userText, plan, selected, false)
	if e.Events.OnRouteShadow != nil {
		e.Events.OnRouteShadow(rec)
	}
}

func (e *Engine) evaluateRouteShadow(ctx context.Context, userText string, plan *router.Plan, selected []string, allowPromotion bool) (RouteShadowResult, *router.Plan, []string) {
	rec := RouteShadowResult{
		UserText:             userText,
		DeterministicIntent:  string(plan.ID),
		DeterministicTools:   append([]string{}, selected...),
		DeterministicCompact: router.CompactRouteLine(plan, selected),
	}
	promotedPlan := plan
	promotedTools := append([]string{}, selected...)
	if e.RoutePredictor != nil {
		if skip, reason := e.shouldSkipRoutePredictor(userText, plan, allowPromotion); skip {
			rec.PromotionReason = reason
			return rec, promotedPlan, promotedTools
		}
		// Semantic routing is an advisory preflight, not the user's actual work.
		// Bound it independently so a cold or unhealthy predictor cannot hold the
		// resident coding model behind a minute-long first-token gate. On timeout
		// the existing fail-open path retains the deterministic capability plan.
		predictCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		raw, err := e.RoutePredictor.PredictRoute(predictCtx, userText, e.Registry.Names())
		cancel()
		rec.PredictedRaw = raw
		if err != nil {
			rec.Err = err.Error()
		} else if parsed, ok := router.ParseCompactRouteLine(raw); ok {
			rec.PredictedIntent = string(parsed.Intent)
			rec.PredictedTools = append([]string{}, parsed.Tools...)
			rec.Agreement = rec.PredictedIntent == rec.DeterministicIntent &&
				sameStringSlice(rec.PredictedTools, rec.DeterministicTools)
			if allowPromotion {
				if pPlan, pTools, reason := e.safePromotedRoute(parsed, plan, userText); pPlan != nil {
					rec.Promoted = true
					rec.PromotionReason = reason
					promotedPlan = pPlan
					promotedTools = pTools
				} else if reason != "" {
					rec.PromotionReason = reason
				}
			}
		} else {
			rec.Err = "compact route did not parse"
		}
	}
	return rec, promotedPlan, promotedTools
}

var routePredictorEscalationRe = regexp.MustCompile(`(?i)你沒有|你没|沒用工具|没用工具|沒有用工具|沒有存|没存|沒存|存下來|存下来|儲存|储存|保存|存成|再試|再试|重來|重来|真的|務必|\b(you did not|you didn't|use the tool|save it|save this|try again|really)\b`)

func (e *Engine) shouldSkipRoutePredictor(userText string, plan *router.Plan, allowPromotion bool) (bool, string) {
	if !strings.EqualFold(strings.TrimSpace(e.RoutePredictorMode), "auto") {
		return false, ""
	}
	if !allowPromotion {
		return true, "auto_skipped_shadow_only"
	}
	// Auto mode is a recovery aid for ambiguous/general requests, not authority
	// to reinterpret a route that the deterministic parser already identified.
	// Wake it for explicit escalation and the one known URL+artifact conflict;
	// otherwise keep the specific route and avoid an extra model preflight.
	if routePredictorEscalationRe.MatchString(userText) ||
		plan == nil || plan.ID == router.IntentGeneralAction ||
		routePredictorArtifactConflict(userText, plan) {
		return false, ""
	}
	return true, "auto_skipped_specific_deterministic"
}

func routePredictorArtifactConflict(userText string, plan *router.Plan) bool {
	if plan == nil || plan.ID != router.IntentInternetResearch {
		return false
	}
	if router.TaskTargetFile(userText) == "" {
		return false
	}
	lower := strings.ToLower(userText)
	return strings.Contains(lower, ".html") ||
		strings.Contains(lower, "three.js") ||
		strings.Contains(lower, "cdn") ||
		strings.Contains(lower, "網頁") ||
		strings.Contains(lower, "网页")
}

func (e *Engine) safePromotedRoute(route router.CompactRoute, deterministic *router.Plan, userText string) (*router.Plan, []string, string) {
	if !strings.EqualFold(strings.TrimSpace(e.RoutePredictorPromotion), "safe") {
		return nil, nil, "promotion_disabled"
	}
	if deterministic == nil {
		return nil, nil, "missing_deterministic_plan"
	}
	if route.Confirm || route.Intent == router.IntentDeleteGenerated {
		return nil, nil, "confirm_required"
	}
	if deterministic.ID == router.IntentDeleteGenerated {
		return nil, nil, "deterministic_destructive_boundary"
	}
	artifactConflict := routePredictorArtifactConflict(userText, deterministic) &&
		route.Intent == router.IntentStaticWebArtifact
	if route.Intent != deterministic.ID && deterministic.ID != router.IntentGeneralAction && !artifactConflict {
		return nil, nil, "deterministic_specific"
	}
	if (route.Intent == router.IntentInternetResearch || route.Intent == router.IntentResearchArtifact) &&
		!router.HasExplicitWebResearchCue(userText) {
		return nil, nil, "network_hint_without_web_cue"
	}
	if route.Intent == deterministic.ID {
		selected := intersectRegistered(deterministic.Tools, e.Registry.Names())
		if len(selected) == 0 {
			return nil, nil, "no_tools_available"
		}
		return deterministic, selected, "safe_intent_agreement"
	}
	plan := predictorSafePlan(route.Intent)
	if plan == nil {
		return nil, nil, "unsafe_intent"
	}
	selected := intersectRegistered(plan.Tools, e.Registry.Names())
	if len(selected) == 0 {
		return nil, nil, "no_tools_available"
	}
	if artifactConflict {
		return plan, selected, "safe_artifact_conflict_hint"
	}
	return plan, selected, "safe_general_action_hint"
}

func predictorSafePlan(intent router.IntentID) *router.Plan {
	switch intent {
	case router.IntentSimpleWrite:
		return &router.Plan{
			ID:    router.IntentSimpleWrite,
			Label: "predictor simple file write",
			Tools: []string{router.ToolWrite},
			Guidance: []string{
				"Predictor route hint: create one local file directly.",
				"First step: call write_file with an absolute file_path under the current working directory.",
				"Do not answer in prose before the required tool call.",
			},
		}
	case router.IntentStaticWebArtifact:
		return &router.Plan{
			ID:    router.IntentStaticWebArtifact,
			Label: "predictor static web artifact",
			Tools: []string{router.ToolWrite, router.ToolEdit, router.ToolRead, router.ToolShell, router.ToolVisual, router.ToolList},
			Guidance: []string{
				"Predictor route hint: create a static web artifact.",
				"First step: call write_file for a complete single-file HTML artifact when possible.",
				"Validation step: run a named TS/build/check command when provided, then use Meadow Browser for deterministic DOM, canvas, console, runtime, and network diagnostics.",
				"Screenshots are human-review evidence by default. If the user explicitly asks to inspect or compare an image or screenshot, set meadow_browser_visual_check.include_model_image=true and reason from the attached pixels; a still image cannot prove motion or interaction.",
				"Do not answer in prose before the required tool call.",
			},
		}
	case router.IntentConfigArtifact:
		return &router.Plan{
			ID:    router.IntentConfigArtifact,
			Label: "predictor config artifact",
			Tools: []string{router.ToolWrite, router.ToolShell, router.ToolRead},
			Guidance: []string{
				"Predictor route hint: create or update a local configuration artifact.",
				"First step: call write_file for a new config file, or read_file before changing an existing file.",
				"Do not answer in prose before the required tool call.",
			},
		}
	case router.IntentInternetResearch:
		return &router.Plan{
			ID:    router.IntentInternetResearch,
			Label: "predictor internet research",
			Tools: []string{router.ToolWebSearch, router.ToolWebFetch, router.ToolBrowsePage},
			Guidance: []string{
				"Predictor route hint: collect current or website-backed information from the web.",
				"Use web_search for discovery, web_fetch for exact source pages, and browse_page for JS-heavy pages.",
				"Return source URLs in the final answer.",
			},
		}
	case router.IntentSimpleCopy:
		return &router.Plan{
			ID:    router.IntentSimpleCopy,
			Label: "predictor file copy",
			Tools: []string{router.ToolRead, router.ToolWrite},
			Guidance: []string{
				"Predictor route hint: copy one local file to another.",
				"First step: call read_file for the source file.",
				"Second step: call write_file for the destination with exactly the read content.",
			},
		}
	default:
		return nil
	}
}

func intersectRegistered(want []string, available []string) []string {
	registered := map[string]bool{}
	for _, name := range available {
		registered[name] = true
	}
	out := make([]string, 0, len(want))
	for _, name := range want {
		if registered[name] {
			out = append(out, name)
		}
	}
	return out
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// localSafety ports the TS applyLocalGemma4SafetyOverrides ceilings. Local
// Gemma 4 served with no sampling params decodes at the server default
// temperature, where mid-size checkpoints (12B) can spiral into endless
// thought-channel loops (M1-gate s1: 866 chunks, zero tool calls). Action
// turns clamp to 0.1, chat turns to 0.2; completions default to the local
// provider's served limit unless the caller already set a tighter limit.
func localSafety(req provider.ChatRequest) provider.ChatRequest {
	if req.Temperature == nil {
		t := 0.2
		if len(req.Tools) > 0 {
			t = 0.1
		}
		req.Temperature = &t
	}
	if req.MaxTokens == nil {
		mt := provider.LocalModelMaxTokens(req.Model)
		req.MaxTokens = &mt
	}
	if req.RepetitionPenalty == nil {
		// vllm-mlx 預設 1.12：對自然語言無害，但對長 HTML/JS/JSON 是
		// 毒藥——重複懲罰打壓必要的重複標籤與語法，讓 staged write 的
		// 大檔生成品質崩潰或燒滿截斷（live 五輪 1/5 過的高嫌疑因子）。
		rp := 1.0
		req.RepetitionPenalty = &rp
	}
	return req
}

const twoPassBriefSystemPrompt = `You are Meadow Code's private first-pass task briefer.
Return a concise JSON object only. Do not call tools. Do not solve the task.
Extract the user's actual deliverable, likely files, required tools, acceptance checks, and common failure traps.
Keep it compact and operational so a second-pass local coding agent can execute it.`

func (e *Engine) runTwoPassBrief(ctx context.Context, userMsg provider.Message, history []provider.Message, trace *agenttrace.Emitter) string {
	if e.Client == nil {
		return ""
	}
	// Harness status is telemetry, never model-supplied thought content.
	trace.Emit("agent.status", 0, map[string]any{"phase": "two_pass_brief", "status": "started"})
	msgs := []provider.Message{{Role: "system", Content: twoPassBriefSystemPrompt}}
	if len(history) > 0 {
		msgs = append(msgs, provider.Message{
			Role:    "system",
			Content: "Conversation context is available in the next pass. Use it only to identify constraints; do not restate the full history.",
		})
	}
	msgs = append(msgs, userMsg)
	mt := 900
	temp := 0.1
	req := provider.ChatRequest{
		Model:       e.ModelID,
		Messages:    msgs,
		MaxTokens:   &mt,
		Temperature: &temp,
	}
	stream, err := e.Client.ChatStream(ctx, req, nil)
	if err != nil || stream == nil {
		trace.Emit("agent.status", 0, map[string]any{"phase": "two_pass_brief", "status": "failed"})
		return ""
	}
	brief := strings.TrimSpace(provider.StripThoughtChannel(stream.Content))
	if brief == "" {
		return ""
	}
	if len([]rune(brief)) > 6000 {
		r := []rune(brief)
		brief = string(r[:6000])
	}
	trace.Emit("agent.status", 0, map[string]any{"phase": "two_pass_brief", "status": "completed"})
	return brief
}

func (e *Engine) withSystem(messages []provider.Message) []provider.Message {
	system := strings.TrimSpace(e.SystemPrompt)
	if brief := strings.TrimSpace(e.twoPassBrief); brief != "" {
		block := "【Private two-pass task brief】\n" +
			"Use this advisory brief to execute the user's original request. If the brief conflicts with the user request, the user request wins.\n" +
			brief
		if system == "" {
			system = block
		} else {
			system = system + "\n\n" + block
		}
	}
	if system == "" {
		return messages
	}
	return append([]provider.Message{{Role: "system", Content: system}}, messages...)
}

func (e *Engine) toolSchemas(names []string) []provider.Tool {
	var out []provider.Tool
	for _, name := range names {
		t, ok := e.Registry.Get(name)
		if !ok {
			continue
		}
		out = append(out, provider.Tool{
			Type: "function",
			Function: provider.ToolSchema{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Schema(),
			},
		})
	}
	return out
}

func lastUserText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}
