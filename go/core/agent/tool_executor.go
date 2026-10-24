package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/sessionrecovery"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

type Effect string

const (
	EffectNone        Effect = "none"
	EffectRead        Effect = "read"
	EffectWrite       Effect = "workspace_write"
	EffectExecute     Effect = "execute"
	EffectInteract    Effect = "interact"
	EffectDestructive Effect = "destructive"
)

const ToolCancelled ToolStatus = "cancelled"

type Authorization struct {
	Allowed    bool
	Status     ToolStatus
	LLMContent string
}

type AuthorizeFunc func(context.Context, string, map[string]any, tools.Kind) Authorization

type ToolCheckpointCall struct {
	Turn         int
	CallID       string
	Tool         string
	Args         map[string]any
	RawArguments string
	Kind         tools.Kind
	Effect       Effect
	InputVersion string
}

type PermissionLifecycle struct {
	Turn          int
	CallID        string
	Tool          string
	Kind          tools.Kind
	Decision      PermissionDecision
	Authorization Authorization
}

type PermissionDecision string

const (
	PermissionRequested PermissionDecision = "requested"
	PermissionResolved  PermissionDecision = "resolved"
)

type ToolStart struct {
	Turn   int
	CallID string
	Tool   string
	Args   map[string]any
	Kind   tools.Kind
}

type ToolExecution struct {
	Turn   int
	CallID string
	Tool   string
	Args   map[string]any
	// RawArguments binds a typed lifecycle result to the original assistant
	// call before normalization. Only its digest is persisted by session code.
	RawArguments string `json:"-"`
	Kind         tools.Kind
	Status       ToolStatus
	Effect       Effect
	InputVersion string
	Executed     bool
	LLMContent   string
	// ContentParts is transient provider-only evidence returned to the model on
	// the next inference request. Lifecycle callbacks deliberately receive a
	// redacted copy so data URLs cannot enter UI events or recovery journals.
	ContentParts []provider.MessageContentPart
	// TaskPlanState is successful executed model bookkeeping, not task evidence.
	// UI consumers must additionally match the committed transcript boundary.
	TaskPlanState *tools.TaskPlanState
	// BehaviorEvidence contains typed browser execution observations. Its
	// presence alone does not mean assertions passed or an artifact is current.
	BehaviorEvidence map[string]any
	Display          string
	Diff             string
	ChangedPaths     []string
	// EvidenceSources are authoritative URLs observed by this successful tool
	// call. They are copied onto the lifecycle event so UI/server consumers do
	// not need to reverse-engineer URLs from display text or tool arguments.
	EvidenceSources []tools.EvidenceSource
	// Places are typed provider observations, never inferred from model prose.
	Places        *tools.PlaceCollection
	NoChange      bool
	ProcessExited bool
	// FileObservations bind model-declared foreground validation targets to
	// unchanged file bytes at the command boundaries. They do not attest that
	// the command tested those bytes correctly or that the task is complete.
	FileObservations []tools.FileObservation
	// ArtifactRolledBack reports that a structurally-invalid write executed but
	// was restored to its exact pre-call bytes before control returned to the
	// model. It is not a verified checkpoint or a successful mutation.
	ArtifactRolledBack bool
	Err                error
	// Validation metadata is attached by KernelRuntime after Verifier observes
	// the executed call. Keeping it with the execution lets later policy gates
	// distinguish a credible non-zero discrepancy from a checker that printed a
	// failure verdict while incorrectly returning success.
	Validation              bool
	ValidationStatus        VerificationStatus
	ValidationExitCode      int
	ValidationHasExitCode   bool
	ValidationFailureOutput bool
	// ReturnControl is copied from tools.Result for async-start tools.
	ReturnControl bool
	DurationMS    int64
}

// ToolExecutor is the model-agnostic tool boundary. It guarantees one result
// for every input call and preserves input order in ExecuteBatch.
type ToolExecutor struct {
	Registry  *tools.Registry
	Authorize AuthorizeFunc
	// RequireAccount is independent of tool approval and must remain live across
	// model calls, permission waits and delegated workers. Nil is an ungated host.
	RequireAccount func(context.Context) error
	// Parallel enables bounded concurrent execution only for calls whose tool
	// explicitly implements tools.Parallelizable. Results remain in model
	// emission order and non-safe calls are sequential barriers.
	Parallel      bool
	MaxParallel   int
	BeforeExecute func(name string, args map[string]any)
	OnToolCall    func(name string, args map[string]any)
	OnToolStart   func(ToolStart)
	OnToolResult  func(name string, result tools.Result)
	// BeforeToolCall must durably checkpoint before authorization or execution.
	// Returning an error blocks the call before any tool side effect.
	BeforeToolCall func(ToolCheckpointCall) error
	// OnPermission records the pending and resolved permission lifecycle.
	// Returning an error blocks execution even when authorization was approved.
	OnPermission func(PermissionLifecycle) error
	// AfterToolComplete persists execution and model-facing result boundaries.
	AfterToolComplete func(ToolExecution) error
	// OnToolComplete is the stable, lossless lifecycle callback. Unlike the
	// legacy name-only result callback it carries call identity, status and
	// duration, so parallel/repeated calls cannot be associated with the wrong
	// UI row.
	OnToolComplete func(ToolExecution)
	readDedup      *readDedupState
	callbackMu     *sync.Mutex
	evidenceState  *evidenceSourceState
	// evidenceContextFilter narrows the run-scoped evidence made visible to
	// retrieval/publishing tools. Network tools are recorded optimistically by
	// the executor, while the research ledger may later reject a portal, search
	// page, or structurally weak article. Keeping the filter on the per-run clone
	// prevents rejected pages and documents from older sessions entering the
	// current semantic RAG query.
	evidenceContextFilter func([]tools.EvidenceSource) []tools.EvidenceSource
}

type evidenceSourceState struct {
	mu           sync.Mutex
	sources      []tools.EvidenceSource
	operands     []tools.SourceOperand
	limitedCalls map[string]int
}

// recoverableCheckpointBlock converts a guarded mutation replay into a normal
// model-facing tool result. Durable recovery must still prevent the replay,
// but that safety decision is not a broken stream or a corrupt checkpoint.
// Returning the observation to the same Agent loop lets the model inspect the
// current state with read-only tools and choose a different action, matching
// the tool-result recovery boundary used by mature coding harnesses.
func recoverableCheckpointBlock(err error) (llmContent, display string, ok bool) {
	if !errors.Is(err, sessionrecovery.ErrSameActionRetry) {
		return "", "", false
	}
	return strings.TrimSpace(`SAME_MUTATION_RETRY_BLOCKED: the previous identical mutation may already have taken effect, so it was not executed again.
This call produced no fresh state or source-version evidence. An earlier result does not prove the current state or that the current input was loaded.
Inspect the current state with a read-only tool first. Then use the observed result to finish, choose a different action, or ask the user only when the external effect cannot be determined. Do not repeat the unchanged mutation automatically.`),
		"已阻止重複執行，請先檢查目前狀態", true
}

// NewRun returns a callback-compatible executor with state isolated to one
// agent request. The base executor remains immutable and can be reused by a
// long-lived CLI runtime.
func (e *ToolExecutor) NewRun() *ToolExecutor {
	if e == nil {
		return nil
	}
	clone := *e
	clone.readDedup = newReadDedupState()
	clone.callbackMu = &sync.Mutex{}
	clone.evidenceState = &evidenceSourceState{limitedCalls: map[string]int{}}
	if clone.MaxParallel <= 0 {
		clone.MaxParallel = 4
	}
	return &clone
}

// ResetReadDedup is called after context compaction because the earlier full
// read may no longer be present in model context.
func (e *ToolExecutor) ResetReadDedup() {
	if e != nil {
		e.readDedup.clear()
	}
}

// SetEvidenceContextFilter binds retrieval context to the orchestration state
// that has actually accepted evidence. It is deliberately not a tool-specific
// answer rule: the callback only selects observed provenance URLs.
func (e *ToolExecutor) SetEvidenceContextFilter(filter func([]tools.EvidenceSource) []tools.EvidenceSource) {
	if e != nil {
		e.evidenceContextFilter = filter
	}
}

// HandlesRepeatFeedback reports whether a repeated call will receive a
// tool-level unchanged/block response. The Kernel grants those calls one
// extra turn so it does not terminate before the model sees the feedback.
func (e *ToolExecutor) HandlesRepeatFeedback(call provider.ToolCall) bool {
	if e == nil || e.Registry == nil {
		return false
	}
	tool, ok := e.Registry.Get(call.Function.Name)
	if !ok {
		return false
	}
	_, ok = tool.(tools.DeduplicableRead)
	return ok
}

func (e *ToolExecutor) Execute(ctx context.Context, call provider.ToolCall, turn int) (result ToolExecution) {
	started := time.Now()
	checkpointStarted := false
	result = ToolExecution{
		Turn: turn, CallID: call.ID, Tool: call.Function.Name,
		RawArguments: call.Function.Arguments,
		Status:       ToolError, Effect: EffectNone,
	}
	defer func() {
		result.DurationMS = time.Since(started).Round(time.Millisecond).Milliseconds()
		if result.DurationMS < 1 {
			result.DurationMS = 1
		}
		if checkpointStarted && e.AfterToolComplete != nil {
			if checkpointErr := e.AfterToolComplete(withoutToolExecutionMedia(result)); checkpointErr != nil {
				result.Status = ToolError
				result.LLMContent = "RECOVERY_CHECKPOINT_FAILED: tool execution stopped because its durable result boundary could not be recorded."
				result.Display = "recovery checkpoint failed"
				result.Err = fmt.Errorf("%w: %v", ErrRecoveryCheckpoint, checkpointErr)
				result.ReturnControl = true
				result.TaskPlanState = nil
				result.BehaviorEvidence = nil
			}
		}
		e.emitToolComplete(result)
	}()

	if e == nil || e.Registry == nil {
		result.Status = ToolUnknown
		result.LLMContent = "ERROR: unknown tool " + call.Function.Name
		return result
	}
	if err := e.requireAccount(ctx); err != nil {
		return accountBlockedExecution(result, err)
	}
	tool, ok := e.Registry.Get(call.Function.Name)
	if !ok {
		if e.BeforeToolCall != nil {
			if checkpointErr := e.BeforeToolCall(ToolCheckpointCall{Turn: turn, CallID: call.ID, Tool: call.Function.Name, RawArguments: call.Function.Arguments}); checkpointErr != nil {
				if llmContent, display, recoverable := recoverableCheckpointBlock(checkpointErr); recoverable {
					result.Status = ToolBlocked
					result.LLMContent = llmContent
					result.Display = display
					return result
				}
				result.Status = ToolBlocked
				result.LLMContent = "RECOVERY_CHECKPOINT_FAILED: tool call was blocked before execution."
				result.Err = fmt.Errorf("%w: %v", ErrRecoveryCheckpoint, checkpointErr)
				return result
			}
			checkpointStarted = true
		}
		result.Status = ToolUnknown
		result.LLMContent = "ERROR: unknown tool " + call.Function.Name
		return result
	}

	args, argumentError := decodeToolArguments(tool, call.Function.Arguments)
	result.Args = args
	e.emitToolCall(call.Function.Name, args)

	if ctx.Err() != nil {
		result.Status = ToolCancelled
		result.LLMContent = "Tool execution cancelled before it started."
		result.Err = ctx.Err()
		return result
	}

	result.Kind = tools.CallKind(tool, args)
	result.Effect = effectForToolCall(tool, args, result.Kind)
	var inputVersionError error
	if argumentError == nil {
		if err := e.requireAccount(ctx); err != nil {
			return accountBlockedExecution(result, err)
		}
		result.InputVersion, inputVersionError = executionInputVersion(ctx, tool, args, result.Kind)
	}
	if err := e.requireAccount(ctx); err != nil {
		return accountBlockedExecution(result, err)
	}
	if e.BeforeToolCall != nil {
		if checkpointErr := e.BeforeToolCall(ToolCheckpointCall{
			Turn: turn, CallID: call.ID, Tool: call.Function.Name, Args: args,
			RawArguments: call.Function.Arguments, Kind: result.Kind, Effect: result.Effect, InputVersion: result.InputVersion,
		}); checkpointErr != nil {
			if llmContent, display, recoverable := recoverableCheckpointBlock(checkpointErr); recoverable {
				result.Status = ToolBlocked
				result.LLMContent = llmContent
				result.Display = display
				return result
			}
			result.Status = ToolBlocked
			result.LLMContent = "RECOVERY_CHECKPOINT_FAILED: tool call was blocked before execution."
			result.Display = "recovery checkpoint failed"
			result.Err = fmt.Errorf("%w: %v", ErrRecoveryCheckpoint, checkpointErr)
			return result
		}
		checkpointStarted = true
	}
	if argumentError != nil {
		result.Status = ToolError
		result.LLMContent = argumentError.modelContent()
		result.Display = argumentError.displayContent()
		result.Err = argumentError
		return result
	}
	if inputVersionError != nil {
		result.Status = ToolError
		result.LLMContent = "Tool input identity could not be verified: " + inputVersionError.Error()
		result.Err = inputVersionError
		return result
	}
	if e.Authorize != nil {
		if e.OnPermission != nil {
			if checkpointErr := e.OnPermission(PermissionLifecycle{
				Turn: turn, CallID: call.ID, Tool: call.Function.Name,
				Kind: result.Kind, Decision: PermissionRequested,
			}); checkpointErr != nil {
				result.Status = ToolBlocked
				result.LLMContent = "RECOVERY_CHECKPOINT_FAILED: permission request was not durably recorded."
				result.Err = fmt.Errorf("%w: %v", ErrRecoveryCheckpoint, checkpointErr)
				return result
			}
		}
		authorization := e.Authorize(ctx, call.Function.Name, args, result.Kind)
		if e.OnPermission != nil {
			if checkpointErr := e.OnPermission(PermissionLifecycle{
				Turn: turn, CallID: call.ID, Tool: call.Function.Name,
				Kind: result.Kind, Decision: PermissionResolved, Authorization: authorization,
			}); checkpointErr != nil {
				result.Status = ToolBlocked
				result.LLMContent = "RECOVERY_CHECKPOINT_FAILED: permission resolution was not durably recorded."
				result.Err = fmt.Errorf("%w: %v", ErrRecoveryCheckpoint, checkpointErr)
				return result
			}
		}
		if !authorization.Allowed {
			result.Status = authorization.Status
			if result.Status == "" {
				result.Status = ToolBlocked
			}
			result.LLMContent = authorization.LLMContent
			return result
		}
	}

	// Authorization and durable journal callbacks may have consumed the run
	// deadline. Recheck before advertising or starting a tool operation.
	if err := ctx.Err(); err != nil {
		result.Status = ToolCancelled
		result.LLMContent = "Tool execution cancelled before it started."
		result.Err = err
		return result
	}
	if err := e.requireAccount(ctx); err != nil {
		return accountBlockedExecution(result, err)
	}

	if bounded, ok := tool.(tools.RunCallLimitProvider); ok && bounded.RunCallLimit() > 0 {
		if !e.consumeLimitedCall(call.Function.Name, bounded.RunCallLimit()) {
			result.Status = ToolBlocked
			result.LLMContent = "RUN_TOOL_LIMIT_REACHED: this bounded comparison has no further executions in this run. Use the existing result, disclose unchecked sources, and deliver within the remaining limits."
			result.Display = "本輪來源比對次數已用完；保留原結果與未查核部分"
			return result
		}
	}
	readIdentity, deduplicable := e.readIdentity(tool, args)
	if deduplicable {
		switch e.readDedup.before(readIdentity) {
		case readUnchanged:
			return e.syntheticReadResult(result, call.Function.Name, unchangedReadResult(readIdentity), ToolOK)
		case readBlocked:
			return e.syntheticReadResult(result, call.Function.Name, blockedReadResult(readIdentity), ToolBlocked)
		}
	} else if e.readDedup != nil {
		// Another tool means the model made progress, so a later unchanged
		// read starts a fresh warning budget while retaining its cached version.
		e.readDedup.resetHits()
	}
	if e.BeforeExecute != nil {
		e.BeforeExecute(call.Function.Name, args)
	}
	evidenceSources := e.evidenceSourceSnapshot()
	if e.evidenceContextFilter != nil {
		evidenceSources = e.evidenceContextFilter(evidenceSources)
	}
	toolContext := tools.ContextWithEvidenceSources(ctx, evidenceSources)
	toolContext = tools.ContextWithSourceOperands(toolContext, e.sourceOperandSnapshot())
	if result.InputVersion != "" {
		toolContext = tools.WithExecutionInputVersion(toolContext, result.InputVersion)
	}
	// BeforeExecute/evidence callbacks are also synchronous and may block.
	if err := ctx.Err(); err != nil {
		result.Status = ToolCancelled
		result.LLMContent = "Tool execution cancelled before it started."
		result.Err = err
		return result
	}
	if err := e.requireAccount(ctx); err != nil {
		return accountBlockedExecution(result, err)
	}
	e.emitToolStart(ToolStart{Turn: turn, CallID: call.ID, Tool: call.Function.Name, Args: args, Kind: result.Kind})
	// A lifecycle callback may block too; admission is checked at the actual
	// tool boundary, without claiming to revoke an operation already running.
	if err := ctx.Err(); err != nil {
		result.Status = ToolCancelled
		result.LLMContent = "Tool execution cancelled before it started."
		result.Err = err
		return result
	}
	if err := e.requireAccount(ctx); err != nil {
		return accountBlockedExecution(result, err)
	}
	toolResult := tool.Execute(toolContext, args)
	result.Executed = true
	result.LLMContent = toolResult.LLMContent
	result.ContentParts = cloneMessageContentParts(toolResult.ContentParts)
	result.Display = toolResult.Display
	result.Diff = toolResult.Diff
	result.ChangedPaths = append([]string(nil), toolResult.ChangedPaths...)
	result.NoChange = toolResult.NoChange
	result.ProcessExited = toolResult.ProcessExited
	result.FileObservations = append([]tools.FileObservation(nil), toolResult.FileObservations...)
	result.BehaviorEvidence = cloneToolBehaviorEvidence(toolResult.BehaviorEvidence)
	if result.NoChange {
		result.Effect = EffectNone
	}
	if len(result.ChangedPaths) > 0 {
		result.Effect = EffectWrite
	}
	result.Err = toolResult.Err
	result.ReturnControl = toolResult.ReturnControl
	if toolResult.Err != nil {
		result.Status = ToolError
		if result.Kind == tools.KindEdit && e.readDedup != nil {
			// A failed edit commonly means the model's exact-match view is stale
			// (for example, old_string no longer matches).  The tool explicitly
			// asks the model to re-read, so retaining the previous dedup entry
			// would suppress the only authoritative recovery action.  Clear all
			// cached regions conservatively: edit tools may also fail after a
			// partial or indirect workspace change.
			e.readDedup.clear()
		}
	} else {
		result.Status = ToolOK
		if _, isPlanStateTool := tool.(tools.TaskPlanStateTool); isPlanStateTool {
			result.TaskPlanState = tools.CloneTaskPlanState(toolResult.TaskPlanState)
		}
		result.Places = tools.ClonePlaceCollection(toolResult.Places)
		result.EvidenceSources = e.recordEvidenceSources(tool, args, toolResult)
		e.recordSourceOperands(toolResult.SourceOperands)
		if deduplicable {
			if current, ok := e.readIdentity(tool, args); ok {
				readIdentity = current
			}
			e.readDedup.record(readIdentity)
		} else if result.Effect == EffectWrite && e.readDedup != nil {
			// A successful workspace mutation can invalidate any cached region.
			// Clearing all entries is intentionally conservative and generic.
			e.readDedup.clear()
		}
	}
	if strings.TrimSpace(result.LLMContent) == "" {
		result.LLMContent = "(no output)"
	}
	e.emitToolResult(call.Function.Name, toolResult)
	return result
}

func (e *ToolExecutor) requireAccount(ctx context.Context) error {
	if e != nil && e.RequireAccount != nil {
		return e.RequireAccount(ctx)
	}
	return nil
}

func accountBlockedExecution(result ToolExecution, err error) ToolExecution {
	result.Status = ToolBlocked
	result.Effect = EffectNone
	result.Err = err
	result.LLMContent = "ACCOUNT_ADMISSION_DENIED: tool execution requires an authenticated Meadow account."
	result.Display = "Tool execution requires an authenticated Meadow account."
	return result
}

// A JSON copy bounds structured lifecycle data and avoids aliasing a tool's
// mutable maps. Unsupported/oversized data is omitted, never promoted to proof.
func cloneToolBehaviorEvidence(evidence map[string]any) map[string]any {
	if evidence == nil {
		return nil
	}
	encoded, err := json.Marshal(evidence)
	if err != nil || len(encoded) > 64<<10 {
		return nil
	}
	var cloned map[string]any
	if json.Unmarshal(encoded, &cloned) != nil {
		return nil
	}
	return cloned
}

func (e *ToolExecutor) evidenceSourceSnapshot() []tools.EvidenceSource {
	if e == nil || e.evidenceState == nil {
		return nil
	}
	e.evidenceState.mu.Lock()
	defer e.evidenceState.mu.Unlock()
	return append([]tools.EvidenceSource(nil), e.evidenceState.sources...)
}

func (e *ToolExecutor) consumeLimitedCall(name string, limit int) bool {
	if e == nil || e.evidenceState == nil {
		return false
	}
	e.evidenceState.mu.Lock()
	defer e.evidenceState.mu.Unlock()
	if e.evidenceState.limitedCalls == nil {
		e.evidenceState.limitedCalls = map[string]int{}
	}
	if e.evidenceState.limitedCalls[name] >= limit {
		return false
	}
	e.evidenceState.limitedCalls[name]++
	return true
}

func (e *ToolExecutor) sourceOperandSnapshot() []tools.SourceOperand {
	if e == nil || e.evidenceState == nil {
		return nil
	}
	e.evidenceState.mu.Lock()
	defer e.evidenceState.mu.Unlock()
	return append([]tools.SourceOperand(nil), e.evidenceState.operands...)
}

func (e *ToolExecutor) recordSourceOperands(incoming []tools.SourceOperand) {
	if e == nil || e.evidenceState == nil {
		return
	}
	e.evidenceState.mu.Lock()
	defer e.evidenceState.mu.Unlock()
	for _, source := range incoming {
		if source.SourceID == "" || len(source.SourceID) > 160 || len(source.TextSHA256) != 64 {
			continue
		}
		if _, err := hex.DecodeString(source.TextSHA256); err != nil {
			continue
		}
		found := false
		for i, previous := range e.evidenceState.operands {
			if previous.SourceID == source.SourceID {
				e.evidenceState.operands[i] = source
				found = true
				break
			}
		}
		if !found && len(e.evidenceState.operands) < 512 {
			e.evidenceState.operands = append(e.evidenceState.operands, source)
		}
	}
}

func (e *ToolExecutor) recordEvidenceSources(tool tools.Tool, args map[string]any, result tools.Result) []tools.EvidenceSource {
	provider, ok := tool.(tools.EvidenceSourceProvider)
	if !ok || e == nil || e.evidenceState == nil {
		return nil
	}
	incoming := provider.EvidenceSources(args, result)
	if len(incoming) == 0 {
		return nil
	}
	accepted := make([]tools.EvidenceSource, 0, len(incoming))
	e.evidenceState.mu.Lock()
	defer e.evidenceState.mu.Unlock()
	for _, source := range incoming {
		source.URL = strings.TrimSpace(source.URL)
		source.Title = strings.TrimSpace(source.Title)
		if source.URL == "" || (!strings.HasPrefix(source.URL, "https://") && !strings.HasPrefix(source.URL, "http://")) {
			continue
		}
		accepted = append(accepted, source)
		found := false
		for index := range e.evidenceState.sources {
			if e.evidenceState.sources[index].URL != source.URL {
				continue
			}
			e.evidenceState.sources[index] = mergeObservedEvidenceSource(e.evidenceState.sources[index], source)
			found = true
			break
		}
		if !found {
			e.evidenceState.sources = append(e.evidenceState.sources, source)
		}
	}
	return accepted
}

// mergeObservedEvidenceSource updates receipt quality and identity only. A
// successful body read with a known source version supersedes an earlier body
// receipt; discovery/error/identity-only observations cannot downgrade it.
// Missing body text is retained only for the SAME known source version, never
// copied into a receipt for a newly observed version.
func mergeObservedEvidenceSource(previous, incoming tools.EvidenceSource) tools.EvidenceSource {
	if incoming.ContentKind != "body" || incoming.FetchError != "" {
		if previous.Title == "" {
			previous.Title = incoming.Title
		}
		if previous.ThumbnailURL == "" {
			previous.ThumbnailURL = incoming.ThumbnailURL
		}
		return previous
	}
	if previous.ContentKind == "body" && previous.SourceVersion != "" && incoming.SourceVersion == "" {
		// An unversioned observation cannot relabel the earlier versioned receipt.
		if previous.Title == "" {
			previous.Title = incoming.Title
		}
		return previous
	}
	if previous.ContentKind == "body" && incoming.SourceVersion == "" && incoming.Snippet == "" {
		// A cached identity receipt without body text or a version cannot prove
		// that the earlier text belongs to a newly observed receipt.
		return previous
	}
	if incoming.Title == "" {
		incoming.Title = previous.Title
	}
	if incoming.ThumbnailURL == "" {
		incoming.ThumbnailURL = previous.ThumbnailURL
	}
	if incoming.SourceVersion != "" && incoming.SourceVersion == previous.SourceVersion {
		if incoming.Snippet == "" {
			incoming.Snippet, incoming.SnippetTruncated = previous.Snippet, previous.SnippetTruncated
		}
		if incoming.FetchedAt == "" {
			incoming.FetchedAt = previous.FetchedAt
		}
	}
	return incoming
}

// Reject reports a call that the orchestration boundary deliberately did not
// execute. It uses the same lossless lifecycle as Execute so the UI, trace and
// evidence ledger cannot mistake a blocked call for missing activity.
//
// Reject does not choose an alternative tool or prescribe a recovery plan.
// Those decisions remain model-owned.
func (e *ToolExecutor) Reject(call provider.ToolCall, turn int, status ToolStatus, content string) (result ToolExecution) {
	started := time.Now()
	result = ToolExecution{
		Turn: turn, CallID: call.ID, Tool: call.Function.Name,
		RawArguments: call.Function.Arguments,
		Status:       status, Effect: EffectNone, LLMContent: strings.TrimSpace(content),
	}
	if result.Status == "" {
		result.Status = ToolBlocked
	}
	if result.LLMContent == "" {
		result.LLMContent = "ERROR: tool call was not executed."
	}
	defer func() {
		result.DurationMS = time.Since(started).Round(time.Millisecond).Milliseconds()
		if result.DurationMS < 1 {
			result.DurationMS = 1
		}
		e.emitToolComplete(result)
	}()

	args := map[string]any{}
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
		args = map[string]any{}
	}
	result.Args = args
	e.emitToolCall(call.Function.Name, args)
	if e != nil && e.Registry != nil {
		if tool, ok := e.Registry.Get(call.Function.Name); ok {
			result.Kind = tools.CallKind(tool, args)
			result.Effect = effectForToolCall(tool, args, result.Kind)
		}
	}
	return result
}

func (e *ToolExecutor) readIdentity(tool tools.Tool, args map[string]any) (tools.ReadIdentity, bool) {
	if e == nil || e.readDedup == nil {
		return tools.ReadIdentity{}, false
	}
	provider, ok := tool.(tools.DeduplicableRead)
	if !ok {
		return tools.ReadIdentity{}, false
	}
	identity, err := provider.ReadIdentity(args)
	if err != nil {
		return tools.ReadIdentity{}, false
	}
	return identity, true
}

func (e *ToolExecutor) syntheticReadResult(result ToolExecution, name string, toolResult tools.Result, status ToolStatus) ToolExecution {
	result.Status = status
	result.Executed = false
	result.LLMContent = toolResult.LLMContent
	result.ContentParts = cloneMessageContentParts(toolResult.ContentParts)
	result.Display = toolResult.Display
	result.Diff = toolResult.Diff
	e.emitToolResult(name, toolResult)
	return result
}

func (e *ToolExecutor) ExecuteBatch(ctx context.Context, calls []provider.ToolCall, turn int) []ToolExecution {
	results := make([]ToolExecution, len(calls))
	if e == nil || !e.Parallel || len(calls) < 2 {
		for index, call := range calls {
			results[index] = e.Execute(ctx, call, turn)
		}
		return results
	}
	if e.callbackMu == nil {
		e.callbackMu = &sync.Mutex{}
	}
	maxParallel := e.MaxParallel
	if maxParallel <= 0 {
		maxParallel = 4
	}
	for start := 0; start < len(calls); {
		if !e.callParallelSafe(calls[start]) {
			results[start] = e.Execute(ctx, calls[start], turn)
			start++
			continue
		}
		end := start + 1
		for end < len(calls) && e.callParallelSafe(calls[end]) {
			end++
		}
		if end-start == 1 {
			results[start] = e.Execute(ctx, calls[start], turn)
			start = end
			continue
		}
		semaphore := make(chan struct{}, maxParallel)
		var group sync.WaitGroup
		for index := start; index < end; index++ {
			index := index
			group.Add(1)
			go func() {
				defer group.Done()
				semaphore <- struct{}{}
				defer func() { <-semaphore }()
				results[index] = e.Execute(ctx, calls[index], turn)
			}()
		}
		group.Wait()
		start = end
	}
	return results
}

func (e *ToolExecutor) callParallelSafe(call provider.ToolCall) bool {
	if e == nil || e.Registry == nil {
		return false
	}
	tool, ok := e.Registry.Get(call.Function.Name)
	if !ok {
		return false
	}
	args := map[string]any{}
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
		args = map[string]any{}
	}
	return tools.ParallelSafe(tool, args)
}

func (e *ToolExecutor) withCallbackLock(fn func()) {
	if fn == nil {
		return
	}
	if e.callbackMu == nil {
		fn()
		return
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	fn()
}

func (e *ToolExecutor) emitToolCall(name string, args map[string]any) {
	if e.OnToolCall != nil {
		e.withCallbackLock(func() { e.OnToolCall(name, args) })
	}
}

func (e *ToolExecutor) emitToolStart(start ToolStart) {
	if e.OnToolStart != nil {
		e.withCallbackLock(func() { e.OnToolStart(start) })
	}
}

func (e *ToolExecutor) emitToolResult(name string, result tools.Result) {
	if e.OnToolResult != nil {
		result.ContentParts = nil
		e.withCallbackLock(func() { e.OnToolResult(name, result) })
	}
}

func (e *ToolExecutor) emitToolComplete(result ToolExecution) {
	if e != nil && e.OnToolComplete != nil {
		result = withoutToolExecutionMedia(result)
		e.withCallbackLock(func() { e.OnToolComplete(result) })
	}
}

func withoutToolExecutionMedia(result ToolExecution) ToolExecution {
	result.ContentParts = nil
	result.TaskPlanState = tools.CloneTaskPlanState(result.TaskPlanState)
	result.BehaviorEvidence = cloneToolBehaviorEvidence(result.BehaviorEvidence)
	return result
}

func cloneMessageContentParts(parts []provider.MessageContentPart) []provider.MessageContentPart {
	if len(parts) == 0 {
		return nil
	}
	cloned := append([]provider.MessageContentPart(nil), parts...)
	for index := range cloned {
		if cloned[index].ImageURL != nil {
			imageURL := *cloned[index].ImageURL
			cloned[index].ImageURL = &imageURL
		}
	}
	return cloned
}

func effectForToolCall(tool tools.Tool, args map[string]any, kind tools.Kind) Effect {
	if kind == tools.KindExecute {
		if provider, ok := tool.(tools.ExecutionEffectProvider); ok && provider.ToolExecutionEffect(args) == "read" {
			return EffectRead
		}
	}
	return EffectForKind(kind)
}

func executionInputVersion(ctx context.Context, tool tools.Tool, args map[string]any, kind tools.Kind) (string, error) {
	if kind != tools.KindExecute {
		return "", nil
	}
	versioner, ok := tool.(tools.ExecutionInputVersionProvider)
	if !ok {
		return "", nil
	}
	version, err := versioner.ToolExecutionInputVersion(ctx, args)
	if err != nil || version == "" {
		return "", err
	}
	digest, err := hex.DecodeString(version)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != version {
		return "", fmt.Errorf("trusted tool returned an invalid input version")
	}
	return version, nil
}

func (e *ToolExecutor) inputVersionForCall(ctx context.Context, call provider.ToolCall) string {
	if e == nil || e.Registry == nil {
		return ""
	}
	if err := e.requireAccount(ctx); err != nil {
		return ""
	}
	tool, ok := e.Registry.Get(call.Function.Name)
	if !ok {
		return ""
	}
	if _, ok := tool.(tools.ExecutionInputVersionProvider); !ok {
		return ""
	}
	args, argumentError := decodeToolArguments(tool, call.Function.Arguments)
	if argumentError != nil {
		return ""
	}
	version, _ := executionInputVersion(ctx, tool, args, tools.CallKind(tool, args))
	return version
}

// hasReadExecutionEffect resolves the same trusted, argument-aware effect used
// at execution. These reads observe live state and cannot reuse a prior action's
// success. Kind and all authorization, validation and recovery checks still run
// in Execute; neither an unregistered tool nor invalid arguments gains an
// exemption from the runtime's repeat guards.
func (e *ToolExecutor) hasReadExecutionEffect(call provider.ToolCall) bool {
	if e == nil || e.Registry == nil {
		return false
	}
	tool, ok := e.Registry.Get(call.Function.Name)
	if !ok {
		return false
	}
	if _, ok := tool.(tools.ExecutionEffectProvider); !ok {
		return false
	}
	args, argumentError := decodeToolArguments(tool, call.Function.Arguments)
	if argumentError != nil {
		return false
	}
	kind := tools.CallKind(tool, args)
	return kind == tools.KindExecute && effectForToolCall(tool, args, kind) == EffectRead
}

func EffectForKind(kind tools.Kind) Effect {
	switch kind {
	case tools.KindReadOnly:
		return EffectRead
	case tools.KindEdit:
		return EffectWrite
	case tools.KindExecute:
		return EffectExecute
	case tools.KindInteract:
		return EffectInteract
	case tools.KindDestructive:
		return EffectDestructive
	default:
		return EffectNone
	}
}
