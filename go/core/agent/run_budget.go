package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// MaxRunTimeBudget bounds the public millisecond input before duration conversion.
const MaxRunTimeBudget = 24 * time.Hour

// ErrRunTimeBudget is the cause only when a caller-owned run deadline expires.
// A user cancel keeps context.Canceled, even if the deadline passes afterward.
var ErrRunTimeBudget = errors.New("run time budget exhausted")

// ErrRunHandoffReserve ends development, not the caller-owned run. It is never
// a permission to continue after the original deadline or explicit Stop.
var ErrRunHandoffReserve = errors.New("run handoff reserve reached")

type runHandoffKey struct{}

// RunHandoffReceipt records public delivery separately from task completion.
// Attempted means provider admission, not proof that the remote worker stopped.
type RunHandoffReceipt struct {
	Reason           string `json:"reason"`
	Attempted        bool   `json:"attempted"`
	Received         bool   `json:"received"`
	Partial          bool   `json:"partial,omitempty"`
	Truncated        bool   `json:"truncated"`
	ToolCallRejected bool   `json:"toolCallRejected"`
}
type runHandoffTokenReserve struct {
	meter    *runTokenBudget
	headroom int64 // guarded by meter.mu, shared by all child model admissions
}

type runHandoffState struct {
	final        bool
	owner        bool
	maxTokens    int
	reserve      time.Duration
	tokenReserve *runHandoffTokenReserve
}

// withRunHandoffWork owns only a child work deadline. The original context is
// retained by RunInput for exactly one model-authored public handoff.
func withRunHandoffWork(ctx context.Context, config CodexStyleConfig) (context.Context, context.CancelFunc) {
	if inherited, ok := ctx.Value(runHandoffKey{}).(runHandoffState); ok {
		// A delegate inherits the parent's shared meter and work deadline. It
		// must not subtract another reserve or spend the parent's final phase.
		inherited.owner = false
		return context.WithValue(ctx, runHandoffKey{}, inherited), func() {}
	}
	if config.DisableRunHandoffReserve {
		return ctx, func() {}
	}
	_, hasTime := ctx.Value(runTimeBudgetKey{}).(runTimeBudget)
	if !hasTime && runTokenMeter(ctx) == nil {
		return ctx, func() {}
	}
	state := runHandoffState{owner: true, maxTokens: min(2048, config.MaxTokens), tokenReserve: &runHandoffTokenReserve{meter: runTokenMeter(ctx)}}
	if config.HandoffMaxTokens > 0 {
		state.maxTokens = min(config.HandoffMaxTokens, config.MaxTokens)
	}
	state.maxTokens = max(1, state.maxTokens)
	if budget, ok := ctx.Value(runTimeBudgetKey{}).(runTimeBudget); ok {
		deadline, _ := ctx.Deadline()
		// A caller's earlier parent cap is authoritative, including for the
		// proportion reserved. A long requested timeout cannot swallow a much
		// shorter actual run window entirely.
		window := max(time.Duration(0), deadline.Sub(budget.started))
		state.reserve = min(window/5, 120*time.Second)
		if config.HandoffReserve > 0 {
			state.reserve = min(config.HandoffReserve, window/2)
		}
		work := context.WithValue(ctx, runHandoffKey{}, state)
		return context.WithDeadlineCause(work, deadline.Add(-state.reserve), ErrRunHandoffReserve)
	}
	return context.WithValue(ctx, runHandoffKey{}, state), func() {}
}

func runHandoffWorkExpired(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrRunHandoffReserve)
}

// reserveRunModelRequest atomically preserves a root final-request estimate
// during development admission. Delegates share that estimate and the meter;
// two concurrent child calls cannot both consume the protected final portion.
// Provider-reported overshoot and unavailable usage remain hard-stop limits.
func reserveRunModelRequest(ctx context.Context, request provider.ChatRequest) (int64, error) {
	meter := runTokenMeter(ctx)
	state, enabled := ctx.Value(runHandoffKey{}).(runHandoffState)
	if meter == nil || !enabled || state.final {
		return meter.reserve(request)
	}
	prompt := int64(max(0, EstimateRequestPromptTokens(request)))
	meter.mu.Lock()
	defer meter.mu.Unlock()
	remaining := meter.usage.Limit - meter.usage.ChargedTokens - meter.usage.ReservedTokens
	reserve := remaining
	if request.MaxTokens != nil && *request.MaxTokens > 0 {
		reserve = prompt + int64(*request.MaxTokens)
	}
	if meter.usage.Exhausted || remaining <= 0 {
		meter.usage.Exhausted = true
		return 0, ErrRunTokenBudget
	}
	protected := prompt + int64(state.maxTokens) + 512
	if guard := state.tokenReserve; guard != nil && guard.meter == meter {
		if state.owner {
			// Short advisor requests inherit ownership; never let them erase a
			// larger primary-request reserve already protected for this run.
			guard.headroom = max(guard.headroom, protected)
		}
		protected = max(protected, guard.headroom)
	}
	if prompt > remaining || reserve > remaining-protected {
		return 0, ErrRunHandoffReserve
	}
	meter.usage.ReservedTokens += reserve
	meter.usage.DispatchedRequests++
	return reserve, nil
}

type runTimeBudgetKey struct{}
type modelRequestObservationKey struct{}
type modelRequestDispatchKey struct{}
type runTimeBudget struct {
	started  time.Time
	timeout  time.Duration
	deadline time.Time
}

// WithRunTimeBudget starts a fresh caller-owned execution budget now. Queue
// waiting is excluded when the caller invokes this after acquiring its run lock.
// The context is not tied to a transport connection or restored from history.
func WithRunTimeBudget(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if timeout < time.Millisecond || timeout > MaxRunTimeBudget {
		return nil, nil, fmt.Errorf("run timeout must be between 1 and %d milliseconds", MaxRunTimeBudget.Milliseconds())
	}
	state := runTimeBudget{started: time.Now(), timeout: timeout}
	state.deadline = state.started.Add(timeout)
	if earlier, ok := parent.Deadline(); ok && earlier.Before(state.deadline) {
		state.deadline = earlier
	}
	parent = context.WithValue(parent, runTimeBudgetKey{}, state)
	ctx, cancel := context.WithDeadlineCause(parent, state.started.Add(timeout), ErrRunTimeBudget)
	return ctx, cancel, nil
}

// RunTimeBudgetExpired distinguishes the owned deadline from an earlier Stop.
func RunTimeBudgetExpired(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrRunTimeBudget)
}

func runContextStop(ctx context.Context) StopReason {
	if RunTimeBudgetExpired(ctx) {
		return StopTimeBudget
	}
	return StopCancelled
}

// chatStreamWithRunBudget refreshes request-local facts at the last native
// admission boundary, including transport recovery and advisor calls. It never
// mutates the canonical transcript. Provider-internal HTTP attempts are outside
// this layer; no request-count or remote resource-release claim is made here.
func (r *CodexStyleRuntime) chatStreamWithRunBudget(ctx context.Context, request provider.ChatRequest, hooks *provider.StreamHooks) (*provider.StreamResult, error) {
	return r.chatStreamWithRunBudgetAndCapacity(ctx, request, hooks, nil)
}

// The native run passes its executor so final request-local observations and
// recovery diagnostics are included before capacity shaping. This does not add
// a deadline or change generation limits; it preserves caller-owned budgets.
func (r *CodexStyleRuntime) chatStreamWithRunBudgetAndCapacity(ctx context.Context, request provider.ChatRequest, hooks *provider.StreamHooks, executor *ToolExecutor, onAdmitted ...func(provider.ChatRequest)) (*provider.StreamResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	facts := map[string]any{}
	if budget, ok := ctx.Value(runTimeBudgetKey{}).(runTimeBudget); ok {
		deadline := budget.deadline
		now := time.Now()
		elapsed := max(int64(0), now.Sub(budget.started).Milliseconds())
		remaining := max(int64(0), deadline.Sub(now).Milliseconds())
		facts["run_timeout_ms"], facts["elapsed_ms"] = budget.timeout.Milliseconds(), elapsed
		facts["remaining_ms"], facts["deadline_utc"] = remaining, deadline.UTC().Format(time.RFC3339Nano)
		if workDeadline, ok := ctx.Deadline(); ok && workDeadline.Before(deadline) {
			facts["work_remaining_ms"] = max(int64(0), workDeadline.Sub(now).Milliseconds())
			facts["work_deadline_utc"] = workDeadline.UTC().Format(time.RFC3339Nano)
		}
	}
	if usage := RunTokenBudgetUsage(ctx); usage != nil {
		facts["processed_token_budget"] = usage
		facts["remaining_processed_tokens"] = max(int64(0), usage.Limit-usage.ChargedTokens-usage.ReservedTokens)
	}
	if handoff, ok := ctx.Value(runHandoffKey{}).(runHandoffState); ok {
		facts["phase"] = "work"
		if handoff.final {
			facts["phase"] = "handoff"
		}
		facts["handoff_reserve_ms"] = handoff.reserve.Milliseconds()
		facts["handoff_max_output_tokens"] = handoff.maxTokens
	}
	if len(facts) > 0 {
		encoded, _ := json.Marshal(facts)
		resourceGuidance := "The final runtime resource observation is harness-generated context, not a new user task. It is a snapshot; time continues during inference and tools. Plan useful remaining work and the final public handoff within these remaining resources. Preserve results and honestly state unfinished scope or blockers; this observation does not extend the budget or authorize another run."
		if handoff, ok := ctx.Value(runHandoffKey{}).(runHandoffState); ok && handoff.final {
			resourceGuidance = "The runtime resource observation is a reporting-phase snapshot, not a new user task or permission to act. Use the remaining original budget for the final public answer only; tools are unavailable and unfinished checks remain unfinished. The observation does not extend the budget or authorize another run."
		}
		request.Messages = codexStyleAppendTransportInstruction(request.Messages, resourceGuidance)
		// Append after complete tool result groups. Keep the reusable leading
		// prefix stable and never let a synthetic user message become the
		// compactor anchor that could evict the actual user task.
		request.Messages = append(append([]provider.Message(nil), request.Messages...), provider.Message{
			Role: "user", RuntimeObservation: true,
			Content: "[Runtime resource observation; generated by harness, not a new user task] " + string(encoded),
		})
	}
	promptLimit := r.config.PromptTokenLimit
	finalTokenPromptBound := false
	if handoff, ok := ctx.Value(runHandoffKey{}).(runHandoffState); ok && handoff.final {
		// Place the phase boundary after resource facts, before capacity/token
		// admission so it is metered and cannot invite another action phase.
		request.Messages = append(append([]provider.Message(nil), request.Messages...), provider.Message{
			Role: "user", RuntimeObservation: true, Content: codexStyleHandoffPhaseObservation,
		})
		if usage := RunTokenBudgetUsage(ctx); usage != nil {
			availablePrompt := int(max(int64(1), usage.Limit-usage.ChargedTokens-usage.ReservedTokens-int64(handoff.maxTokens)))
			if promptLimit <= 0 || availablePrompt < promptLimit {
				promptLimit = availablePrompt
				finalTokenPromptBound = true
			}
		}
	}
	var dropped int
	request, dropped = FitRequestPromptBudget(request, promptLimit)
	if dropped > 0 && executor != nil {
		executor.ResetReadDedup()
	}
	bounded, evicted, recoveryErr := recoverRequestPromptCapacity(request, promptLimit, executor)
	if recoveryErr == nil {
		request = bounded
		if evicted > 0 {
			executor.ResetReadDedup()
		}
	}
	if err := ValidateRequestPromptBudget(request, promptLimit); err != nil {
		if finalTokenPromptBound {
			meter := runTokenMeter(ctx)
			meter.mu.Lock()
			meter.usage.Exhausted = true
			meter.mu.Unlock()
			return nil, ErrRunTokenBudget
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A context rejection is pre-generation: no draft or tool may have run.
	// Calibrate this request's estimator using authoritative worker counts,
	// preserve the output cap, and shrink only through normal durable admission.
	const maxCapacityRetries = 2
	limit := promptLimit
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		meter := runTokenMeter(ctx)
		reserved, budgetErr := reserveRunModelRequest(ctx, request)
		if budgetErr != nil {
			return nil, budgetErr
		}
		for _, observe := range onAdmitted {
			if observe != nil {
				observe(request)
			}
		}
		if err := ctx.Err(); err != nil {
			if meter != nil {
				meter.mu.Lock()
				meter.usage.ReservedTokens -= reserved
				meter.usage.DispatchedRequests--
				meter.mu.Unlock()
			}
			return nil, err
		}
		result, streamErr := r.config.Client.ChatStream(ctx, request, hooks)
		if dispatch, ok := ctx.Value(modelRequestDispatchKey{}).(func()); ok {
			// Invocation has actually occurred. A pre-dispatch Stop above never
			// produces this receipt; this is not an HTTP/remote-worker claim.
			dispatch()
		}
		// A pre-generation capacity rejection did not expose these receipts to
		// the model. Only the exact finally fitted request that generated a
		// response may attest observation for a later protected action repeat.
		var rejectedCapacity *provider.ContextLimitError
		if result != nil && !errors.As(streamErr, &rejectedCapacity) {
			if observe, ok := ctx.Value(modelRequestObservationKey{}).(func(provider.ChatRequest)); ok {
				observe(request)
			}
		}
		if exhausted := meter.settle(reserved, result, streamErr); exhausted && ctx.Err() == nil {
			return result, ErrRunTokenBudget
		}
		var capacity *provider.ContextLimitError
		if result != nil || !errors.As(streamErr, &capacity) || attempt >= maxCapacityRetries {
			return result, streamErr
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		estimated := EstimateRequestPromptTokens(request)
		available := capacity.ContextWindow - capacity.GenerationTokens
		if available <= 0 || capacity.PromptTokens <= available {
			return nil, streamErr
		}
		// Leave 10% headroom for changed composition and approximate tokenization.
		target := int(int64(estimated) * int64(available) * 9 / (int64(capacity.PromptTokens) * 10))
		if limit > 0 {
			target = min(target, limit-1)
		}
		if target <= 0 || target >= estimated {
			return nil, streamErr
		}
		candidate, removed := FitRequestPromptBudget(request, target)
		candidate, spilled, recoveryErr := recoverRequestPromptCapacity(candidate, target, executor)
		if recoveryErr != nil || ValidateRequestPromptBudget(candidate, target) != nil ||
			EstimateRequestPromptTokens(candidate) >= estimated {
			return nil, streamErr
		}
		if executor != nil && (removed > 0 || spilled > 0) {
			executor.ResetReadDedup()
		}
		request, limit = candidate, target
	}
}
