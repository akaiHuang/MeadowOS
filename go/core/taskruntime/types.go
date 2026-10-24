// Package taskruntime owns durable, model-agnostic background task state.
//
// It deliberately does not understand user intent or decide what work should
// happen. Models create and update tasks through tools; the runtime only
// persists lifecycle state, enforces generic execution bounds, and publishes
// observable events that a host can use to wake an idle agent.
package taskruntime

import "time"

const (
	MaxScheduledTasks = 50
	MinLoopInterval   = time.Minute
	DefaultLoopTTL    = 7 * 24 * time.Hour
	SummaryLimit      = 4_000
)

type Kind string

const (
	KindLoop Kind = "loop"
	KindOnce Kind = "once"
)

type Status string

const (
	StatusScheduled Status = "scheduled"
	StatusRunning   Status = "running"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
	StatusExpired   Status = "expired"
)

// Task is the durable, serializable state of one background unit. Prompt and
// prior summaries are model context; status fields are execution facts.
type Task struct {
	ID           string     `json:"id"`
	Kind         Kind       `json:"kind"`
	Prompt       string     `json:"prompt"`
	Interval     string     `json:"interval"`
	IntervalSecs int64      `json:"intervalSecs"`
	Status       Status     `json:"status"`
	Durable      bool       `json:"durable"`
	Paused       bool       `json:"paused,omitempty"`
	OwnerSession string     `json:"ownerSession,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	NextRunAt    time.Time  `json:"nextRunAt"`
	ExpiresAt    time.Time  `json:"expiresAt"`
	LastRunAt    *time.Time `json:"lastRunAt,omitempty"`
	LastEndedAt  *time.Time `json:"lastEndedAt,omitempty"`
	ActiveRunID  string     `json:"activeRunId,omitempty"`
	Iteration    int        `json:"iteration"`
	LastSummary  string     `json:"lastSummary,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	// NativeOwner binds durable delivery to the originating conversation. Nil
	// retains the historical CLI loop contract.
	NativeOwner  *ScheduleOwner `json:"nativeOwner,omitempty"`
	OriginatedAt time.Time      `json:"originatedAt,omitempty"`
	MaxRuns      int            `json:"maxRuns,omitempty"`
}

// ScheduleOwner is host-issued authority; tool arguments cannot select it.
type ScheduleOwner struct {
	ChatID       string `json:"chatId"`
	WorkspaceCWD string `json:"workspaceCwd"`
	ConnectionID string `json:"connectionId"`
}

type CreateLoopRequest struct {
	Prompt          string
	Interval        string
	FireImmediately bool
	Durable         bool
	OwnerSession    string
}

type UpdateLoopRequest struct {
	ID       string
	Prompt   *string
	Interval *string
}

type EventType string

const (
	EventCreated EventType = "created"
	EventUpdated EventType = "updated"
	EventDeleted EventType = "deleted"
	EventDue     EventType = "due"
	EventPaused  EventType = "paused"
	EventResumed EventType = "resumed"
	EventEnded   EventType = "ended"
)

// Event is a host-facing lifecycle fact. EventDue is the only event that asks
// a host to run a model turn; it still carries no prescribed next action.
type Event struct {
	Type  EventType `json:"type"`
	Task  Task      `json:"task"`
	RunID string    `json:"runId,omitempty"`
}
