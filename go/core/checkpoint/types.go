package checkpoint

import (
	"errors"
	"fmt"
	"time"
)

const (
	JournalFileName = "journal.jsonl"
	SnapshotFileName = "snapshot.json"
	WriterLockName = "writer.lock"

	JournalVersion = "meadow.checkpoint.journal.v1"
	SnapshotVersion = "meadow.checkpoint.snapshot.v1"
	ZeroHash = "0000000000000000000000000000000000000000000000000000000000000000"
)

var (
	ErrClosed = errors.New("checkpoint store is closed")
	ErrPoisoned = errors.New("checkpoint store is poisoned")
	ErrWriterLocked = errors.New("checkpoint directory already has a writer")
	ErrDurabilityUncertain = errors.New("checkpoint journal durability is uncertain")
	ErrCorruptCheckpoint = errors.New("checkpoint data is corrupt")
	ErrRecoveryLimit = errors.New("checkpoint recovery limit exceeded")
	ErrEventIDConflict = errors.New("checkpoint event id was reused with different content")
	ErrIdempotencyConflict = errors.New("checkpoint idempotency key was reused with different content")
	ErrStaleProgressRevision = errors.New("checkpoint task progress revision is stale")
	ErrReplayRejected = errors.New("checkpoint task progress replay token does not match")
	ErrTerminalAlreadySet = errors.New("checkpoint terminal state is already set")
	ErrSensitiveIdentifier = errors.New("checkpoint identifier contains credential-like data")
)

type EventKind string

const (
	EventTurn EventKind = "turn"
	EventToolCall EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventCancel EventKind = "cancel"
	EventModelGeneration EventKind = "model_generation"
	EventPermission EventKind = "permission"
	EventTerminal EventKind = "terminal"
	EventTaskProgress EventKind = "task_progress"
)

type TurnStatus string

const (
	TurnPending TurnStatus = "pending"
	TurnRunning TurnStatus = "running"
	TurnWaitingTool TurnStatus = "waiting_tool"
	TurnWaitingPermission TurnStatus = "waiting_permission"
	TurnCompleted TurnStatus = "completed"
	TurnCancelled TurnStatus = "cancelled"
	TurnFailed TurnStatus = "failed"
)

type ToolCallStatus string

const (
	ToolCallRequested ToolCallStatus = "requested"
	ToolCallRunning ToolCallStatus = "running"
	ToolCallCompleted ToolCallStatus = "completed"
	ToolCallFailed ToolCallStatus = "failed"
	ToolCallCancelled ToolCallStatus = "cancelled"
)

type ToolResultStatus string

const (
	ToolResultSucceeded ToolResultStatus = "succeeded"
	ToolResultFailed ToolResultStatus = "failed"
	ToolResultCancelled ToolResultStatus = "cancelled"
)

type GenerationStatus string

const (
	GenerationQueued GenerationStatus = "queued"
	GenerationRunning GenerationStatus = "running"
	GenerationCompleted GenerationStatus = "completed"
	GenerationFailed GenerationStatus = "failed"
	GenerationCancelled GenerationStatus = "cancelled"
)

type PermissionDecision string

const (
	PermissionPending PermissionDecision = "pending"
	PermissionApproved PermissionDecision = "approved"
	PermissionDenied PermissionDecision = "denied"
)

type TerminalOutcome string

const (
	TerminalCompleted TerminalOutcome = "completed"
	TerminalFailed TerminalOutcome = "failed"
	TerminalCancelled TerminalOutcome = "cancelled"
)

// ContentReference is the only persisted representation of prompt-like or
// potentially sensitive content. SHA256 and Bytes describe the original
// content. Summary must be caller-authored and is redacted and bounded before
// persistence. Sensitive forces Summary to a fixed redaction marker.
type ContentReference struct {
	SHA256 string `json:"sha256"`
	Bytes int64 `json:"bytes"`
	Summary string `json:"summary,omitempty"`
	Sensitive bool `json:"sensitive,omitempty"`
	Redacted bool `json:"redacted,omitempty"`
}

type TurnState struct {
	TurnID string `json:"turn_id"`
	Status TurnStatus `json:"status"`
	Prompt *ContentReference `json:"prompt,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type ToolCallState struct {
	CallID string `json:"call_id"`
	TurnID string `json:"turn_id,omitempty"`
	Name string `json:"name"`
	Status ToolCallStatus `json:"status"`
	Arguments *ContentReference `json:"arguments,omitempty"`
}

type ToolResultState struct {
	CallID string `json:"call_id"`
	Status ToolResultStatus `json:"status"`
	Output *ContentReference `json:"output,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type CancellationState struct {
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type ModelGenerationState struct {
	GenerationID string `json:"generation_id"`
	TurnID string `json:"turn_id,omitempty"`
	Model string `json:"model,omitempty"`
	Status GenerationStatus `json:"status"`
	Prompt *ContentReference `json:"prompt,omitempty"`
	Output *ContentReference `json:"output,omitempty"`
	PromptTokens int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
}

type PermissionState struct {
	RequestID string `json:"request_id"`
	TurnID string `json:"turn_id,omitempty"`
	Scope string `json:"scope"`
	Decision PermissionDecision `json:"decision"`
	Reason string `json:"reason,omitempty"`
}

type TerminalState struct {
	Outcome TerminalOutcome `json:"outcome"`
	Summary string `json:"summary,omitempty"`
	At time.Time `json:"at"`
}

// Event has a closed payload set. Exactly one payload must be present and it
// must match Kind. There is deliberately no raw prompt or arbitrary map field.
type Event struct {
	ID string `json:"id"`
	Kind EventKind `json:"kind"`
	At time.Time `json:"at"`
	Turn *TurnState `json:"turn,omitempty"`
	ToolCall *ToolCallState `json:"tool_call,omitempty"`
	ToolResult *ToolResultState `json:"tool_result,omitempty"`
	Cancellation *CancellationState `json:"cancellation,omitempty"`
	Generation *ModelGenerationState `json:"generation,omitempty"`
	Permission *PermissionState `json:"permission,omitempty"`
	Terminal *TerminalState `json:"terminal,omitempty"`
	TaskProgress *TaskProgressUpdate `json:"task_progress,omitempty"`
}

type State struct {
	CurrentTurn *TurnState `json:"current_turn,omitempty"`
	ToolCalls map[string]ToolCallState `json:"tool_calls,omitempty"`
	ToolResults map[string]ToolResultState `json:"tool_results,omitempty"`
	Cancellation *CancellationState `json:"cancellation,omitempty"`
	Generation *ModelGenerationState `json:"generation,omitempty"`
	Permissions map[string]PermissionState `json:"permissions,omitempty"`
	Terminal *TerminalState `json:"terminal,omitempty"`
	TaskProgress *TaskProgressState `json:"task_progress,omitempty"`
	ProgressRevision uint64 `json:"progress_revision,omitempty"`
	IdempotencyKeys map[string]IdempotencyRecord `json:"idempotency_keys,omitempty"`
	LastAppliedEvent *AppliedEventCursor `json:"last_applied_event,omitempty"`
	// AppliedEvents maps event IDs to identity digests. Keeping the digest,
	// rather than only the ID, makes replay idempotent while failing closed if
	// an ID is reused for different content.
	AppliedEvents map[string]string `json:"applied_events,omitempty"`
}

type JournalRecord struct {
	Version string `json:"version"`
	TaskID string `json:"task_id"`
	Sequence uint64 `json:"sequence"`
	PreviousHash string `json:"previous_hash"`
	Event Event `json:"event"`
	Hash string `json:"hash,omitempty"`
}

type Snapshot struct {
	Version string `json:"version"`
	TaskID string `json:"task_id"`
	JournalOffset int64 `json:"journal_offset"`
	ThroughSequence uint64 `json:"through_sequence"`
	ThroughHash string `json:"through_hash"`
	State State `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	Hash string `json:"hash,omitempty"`
}

type Limits struct {
	MaxTailBytes int64
	MaxRecords int
	MaxRecordBytes int
	MaxSnapshotBytes int64
	MaxSummaryBytes int
}

type Options struct {
	Limits Limits
	// Redactor is an additional application redactor. The built-in credential
	// redactor always runs first and cannot be disabled.
	Redactor Redactor
	Clock func() time.Time
}

func DefaultOptions() Options {
	return Options{
		Limits: Limits{
			MaxTailBytes: 64 << 20,
			MaxRecords: 100000,
			MaxRecordBytes: 1 << 20,
			MaxSnapshotBytes: 32 << 20,
			MaxSummaryBytes: 4096,
		},
		Clock: time.Now,
	}
}

type AppendResult struct {
	Applied bool
	Sequence uint64
	Hash string
}

type RecoveryBoundary struct {
	SnapshotUsed bool
	LastGoodOffset int64
	LastSequence uint64
	LastHash string
	FailureOffset int64
}

type RecoveryResult struct {
	State State
	Boundary RecoveryBoundary
	ReplayedRecords int
	DuplicateRecords int
}

type RecoveryFailureKind string

const (
	RecoverySnapshotCorrupt RecoveryFailureKind = "snapshot_corrupt"
	RecoveryJournalCorrupt RecoveryFailureKind = "journal_corrupt"
	RecoveryBoundExceeded RecoveryFailureKind = "recovery_bound_exceeded"
)

// RecoveryError reports the exact trusted boundary without repairing or
// truncating data. Callers must not continue from FailureOffset.
type RecoveryError struct {
	Kind RecoveryFailureKind
	Path string
	Boundary RecoveryBoundary
	Cause error
}

func (e *RecoveryError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("checkpoint recovery failed (%s) at byte %d; last trusted boundary is byte %d sequence %d hash %s: %v",
		e.Kind, e.Boundary.FailureOffset, e.Boundary.LastGoodOffset,
		e.Boundary.LastSequence, e.Boundary.LastHash, e.Cause)
}

func (e *RecoveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
