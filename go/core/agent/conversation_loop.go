package agent

import "context"

type LoopStopReason string

const (
	LoopRunning   LoopStopReason = ""
	LoopCancelled LoopStopReason = "cancelled"
	LoopMaxTurns  LoopStopReason = "max_turns"
)

type RetryClass string

const (
	RetryMalformedToolCall RetryClass = "malformed_tool_call"
	RetryEmptyOutput       RetryClass = "empty_output"
	RetryOutputTruncated   RetryClass = "output_truncated"
)

type RetryPolicy map[RetryClass]int

type ConversationLoop struct {
	maxTurns int
	turn     int
	stop     LoopStopReason
	limits   RetryPolicy
	used     map[RetryClass]int
}

type LoopSnapshot struct {
	Turn       int
	MaxTurns   int
	StopReason LoopStopReason
	RetryUsed  map[RetryClass]int
}

func NewConversationLoop(maxTurns int, policy RetryPolicy) *ConversationLoop {
	if maxTurns < 1 {
		maxTurns = 1
	}
	limits := make(RetryPolicy, len(policy))
	for class, limit := range policy {
		if limit > 0 {
			limits[class] = limit
		}
	}
	return &ConversationLoop{
		maxTurns: maxTurns,
		limits:   limits,
		used:     make(map[RetryClass]int),
	}
}

// BeginTurn is the only place that advances the model-call budget. A retry is
// therefore still a real turn, matching provider cost and observable traces.
func (l *ConversationLoop) BeginTurn(ctx context.Context) (int, bool) {
	if l == nil || l.stop != LoopRunning {
		return 0, false
	}
	if ctx != nil && ctx.Err() != nil {
		l.stop = LoopCancelled
		return 0, false
	}
	if l.turn >= l.maxTurns {
		l.stop = LoopMaxTurns
		return 0, false
	}
	l.turn++
	return l.turn, true
}

// AllowRetry consumes one bounded retry for a generic transport/output class.
func (l *ConversationLoop) AllowRetry(class RetryClass) bool {
	if l == nil || l.stop != LoopRunning {
		return false
	}
	limit := l.limits[class]
	if limit <= 0 || l.used[class] >= limit {
		return false
	}
	l.used[class]++
	return true
}

// ResetRetry starts a new consecutive-failure window after observable progress.
func (l *ConversationLoop) ResetRetry(class RetryClass) {
	if l != nil {
		delete(l.used, class)
	}
}

func (l *ConversationLoop) Stop(reason LoopStopReason) {
	if l != nil && l.stop == LoopRunning && reason != LoopRunning {
		l.stop = reason
	}
}

func (l *ConversationLoop) Snapshot() LoopSnapshot {
	if l == nil {
		return LoopSnapshot{}
	}
	retries := make(map[RetryClass]int, len(l.used))
	for class, count := range l.used {
		retries[class] = count
	}
	return LoopSnapshot{
		Turn: l.turn, MaxTurns: l.maxTurns, StopReason: l.stop, RetryUsed: retries,
	}
}
