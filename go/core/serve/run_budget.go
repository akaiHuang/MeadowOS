package serve

import (
	"context"
	"fmt"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
)

func validateRunBudgets(timeout, tokens *int64) error {
	if timeout != nil && (*timeout < 1 || *timeout > agentcore.MaxRunTimeBudget.Milliseconds()) {
		return fmt.Errorf("options.runTimeoutMs must be an integer between 1 and 86400000")
	}
	if tokens != nil && (*tokens < 1 || *tokens > agentcore.MaxRunTokenBudget) {
		return fmt.Errorf("options.runTokenLimit must be an integer between 1 and 100000000")
	}
	return nil
}

// Admission owns fresh budgets, excluding queue/session-lock waiting. Omitted
// options retain unbounded execution; provider generation caps are unchanged.
func newRunBudgetContext(timeout, tokens *int64) (context.Context, context.CancelFunc, error) {
	if err := validateRunBudgets(timeout, tokens); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if timeout != nil {
		cancel()
		ctx, cancel, _ = agentcore.WithRunTimeBudget(context.Background(), time.Duration(*timeout)*time.Millisecond)
	}
	if tokens != nil {
		ctx, _ = agentcore.WithRunTokenBudget(ctx, *tokens)
	}
	return ctx, cancel, nil
}

func isRunBudgetStop(stop string) bool {
	return stop == string(agentcore.StopTimeBudget) || stop == string(agentcore.StopTokenBudget)
}

func (sess *serveSession) pauseQueueForBudget(stop string) error {
	if !isRunBudgetStop(stop) {
		return nil
	}
	return sess.persistResourceQueuePause()
}

// Pause resource-bound successors from the factual handoff receipt. An
// ordinary incomplete task or tool failure does not imply a spent budget.
func (sess *serveSession) pauseQueueForRunHandoff(recap *agentcore.RunRecap) error {
	if recap == nil || recap.Handoff == nil {
		return nil
	}
	switch recap.Handoff.Reason {
	case "work_time_reserve", "work_token_reserve", "model_turn_limit", "advisor_work_reserve":
		return sess.persistResourceQueuePause()
	default:
		return nil
	}
}

func (sess *serveSession) persistResourceQueuePause() error {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.cancelGeneration++
	cutoff := time.Now()
	sess.wakeCancelledBefore = &cutoff
	sess.queuePaused = true
	return sess.persistQueueLocked()
}

func (sess *serveSession) queueIsPaused() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.queuePaused
}
