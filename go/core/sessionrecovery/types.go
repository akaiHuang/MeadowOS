package sessionrecovery

import (
	"errors"
	"time"
)

const (
	JournalVersion        = "meadow.session-recovery.journal.v1"
	SnapshotVersion       = "meadow.session-recovery.snapshot.v1"
	JournalFileName       = "recovery.jsonl"
	SnapshotFileName      = "recovery.snapshot.json"
	EvidenceDirectoryName = "evidence"
	ZeroHash              = "0000000000000000000000000000000000000000000000000000000000000000"
)

var (
	ErrClosed             = errors.New("session recovery store is closed")
	ErrPoisoned           = errors.New("session recovery store has an uncertain write boundary")
	ErrCorruptJournal     = errors.New("session recovery journal is corrupt")
	ErrCorruptSnapshot    = errors.New("session recovery snapshot is corrupt")
	ErrCompactionRequired = errors.New("session recovery journal requires compaction")
	ErrStateLimit         = errors.New("session recovery state limit exceeded")
	ErrTurnActive         = errors.New("a different turn is already active")
	ErrTurnNotActive      = errors.New("the requested turn is not active")
	ErrToolCallConflict   = errors.New("tool call identity conflicts with durable state")
	ErrToolResultConflict = errors.New("tool result conflicts with durable state")
	ErrSameActionRetry    = errors.New("same mutation action retry requires explicit authorization")
	ErrSessionInUse       = errors.New("session recovery store already has an active writer")
	ErrPermissionConflict = errors.New("permission state conflicts with durable state")
	ErrGuidanceConflict   = errors.New("guidance state conflicts with durable state")
	ErrAttachmentConflict = errors.New("attachment metadata conflicts with durable state")
)

type EventKind string

const (
	EventTurn               EventKind = "turn"
	EventToolCall           EventKind = "tool_call"
	EventToolExecution      EventKind = "tool_execution"
	EventToolResult         EventKind = "tool_result"
	EventPermission         EventKind = "permission"
	EventGuidance           EventKind = "guidance"
	EventAttachment         EventKind = "attachment"
	EventRetryAuthorization EventKind = "retry_authorization"
	EventRecap              EventKind = "recap"
	EventTerminal           EventKind = "terminal"
)

type TurnStatus string

const (
	TurnRunning   TurnStatus = "running"
	TurnCompleted TurnStatus = "completed"
	TurnFailed    TurnStatus = "failed"
	TurnCancelled TurnStatus = "cancelled"
)

type ToolEffect string

const (
	ToolReadOnly ToolEffect = "read_only"
	ToolMutation ToolEffect = "mutation"
	ToolUnknown  ToolEffect = "unknown"
)

type ExecutionOutcome string

const (
	ExecutionSucceeded          ExecutionOutcome = "succeeded"
	ExecutionNoChange           ExecutionOutcome = "no_change"
	ExecutionFailedBeforeEffect ExecutionOutcome = "failed_before_effect"
	ExecutionEffectUnknown      ExecutionOutcome = "effect_unknown"
)

type PermissionDecision string

const (
	PermissionPending  PermissionDecision = "pending"
	PermissionApproved PermissionDecision = "approved"
	PermissionDenied   PermissionDecision = "denied"
)

type GuidanceStatus string

const (
	GuidanceQueued  GuidanceStatus = "queued"
	GuidanceApplied GuidanceStatus = "applied"
	GuidanceDropped GuidanceStatus = "dropped"
)

type TerminalOutcome string

const (
	TerminalCompleted TerminalOutcome = "completed"
	TerminalFailed    TerminalOutcome = "failed"
	TerminalCancelled TerminalOutcome = "cancelled"
)

type AttachmentClass string

const (
	AttachmentUserUpload        AttachmentClass = "user_upload"
	AttachmentGenerated         AttachmentClass = "generated"
	AttachmentWorkspaceArtifact AttachmentClass = "workspace_artifact"
)

// ContentReference is the only accepted representation of prompt-like or
// tool-produced content. Redacted references intentionally omit the digest and
// byte count so low-entropy secrets cannot be attacked offline.
type ContentReference struct {
	SHA256     string `json:"sha256,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	Summary    string `json:"summary,omitempty"`
	StorageRef string `json:"storage_ref,omitempty"`
	Sensitive  bool   `json:"sensitive,omitempty"`
	Redacted   bool   `json:"redacted,omitempty"`
	// Content is process-local hydrated evidence. It is never serialized into
	// the journal or snapshot; StorageRef is the durable authenticated locator.
	Content string `json:"-"`
}

type TurnCheckpoint struct {
	TurnID     string            `json:"turn_id"`
	Ordinal    uint64            `json:"ordinal"`
	Status     TurnStatus        `json:"status"`
	Prompt     *ContentReference `json:"prompt,omitempty"`
	Summary    string            `json:"summary,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
}

type ToolCallCheckpoint struct {
	CallID               string            `json:"call_id"`
	TurnID               string            `json:"turn_id"`
	Name                 string            `json:"name"`
	ActionDigest         string            `json:"action_digest"`
	InputVersion         string            `json:"input_version,omitempty"`
	Effect               ToolEffect        `json:"effect"`
	Arguments            *ContentReference `json:"arguments,omitempty"`
	RetryAuthorizationID string            `json:"retry_authorization_id,omitempty"`
	ObservedCallID       string            `json:"observed_call_id,omitempty"`
	PreparedAt           time.Time         `json:"prepared_at"`
}

type ToolExecutionCheckpoint struct {
	CallID        string           `json:"call_id"`
	Outcome       ExecutionOutcome `json:"outcome"`
	ProcessExited bool             `json:"process_exited,omitempty"`
	Summary       string           `json:"summary,omitempty"`
	At            time.Time        `json:"at"`
}

type ToolResultCheckpoint struct {
	CallID  string            `json:"call_id"`
	Name    string            `json:"name"`
	Output  *ContentReference `json:"output,omitempty"`
	Summary string            `json:"summary,omitempty"`
	At      time.Time         `json:"at"`
}

type PermissionCheckpoint struct {
	RequestID            string             `json:"request_id"`
	TurnID               string             `json:"turn_id,omitempty"`
	CallID               string             `json:"call_id,omitempty"`
	Scope                string             `json:"scope"`
	Decision             PermissionDecision `json:"decision"`
	Reason               string             `json:"reason,omitempty"`
	RetryAuthorizationID string             `json:"retry_authorization_id,omitempty"`
	RetryApprovedBy      string             `json:"retry_approved_by,omitempty"`
	RetryReason          string             `json:"retry_reason,omitempty"`
	At                   time.Time          `json:"at"`
}

type GuidanceCheckpoint struct {
	GuidanceID string            `json:"guidance_id"`
	TurnID     string            `json:"turn_id,omitempty"`
	Content    *ContentReference `json:"content,omitempty"`
	Status     GuidanceStatus    `json:"status"`
	Reason     string            `json:"reason,omitempty"`
	QueuedAt   time.Time         `json:"queued_at"`
	ResolvedAt *time.Time        `json:"resolved_at,omitempty"`
}

// AttachmentMetadata never contains attachment bytes. StorageRef must be a
// session-local relative path and is accepted only when Authorized is true.
type AttachmentMetadata struct {
	AttachmentID string          `json:"attachment_id"`
	Class        AttachmentClass `json:"class"`
	OriginalName string          `json:"original_name,omitempty"`
	MediaType    string          `json:"media_type,omitempty"`
	StorageRef   string          `json:"storage_ref,omitempty"`
	SHA256       string          `json:"sha256,omitempty"`
	Bytes        int64           `json:"bytes,omitempty"`
	Authorized   bool            `json:"authorized"`
	Available    bool            `json:"available"`
	AddedAt      time.Time       `json:"added_at"`
}

type RetryAuthorization struct {
	AuthorizationID    string    `json:"authorization_id"`
	ActionDigest       string    `json:"action_digest"`
	ApprovedBy         string    `json:"approved_by"`
	Reason             string    `json:"reason"`
	SourcePermissionID string    `json:"source_permission_id,omitempty"`
	SourceCallID       string    `json:"source_call_id,omitempty"`
	TurnID             string    `json:"turn_id,omitempty"`
	At                 time.Time `json:"at"`
	ConsumedBy         string    `json:"consumed_by,omitempty"`
}

type ActionGuard struct {
	ActionDigest            string     `json:"action_digest"`
	CallID                  string     `json:"call_id"`
	ToolName                string     `json:"tool_name"`
	Effect                  ToolEffect `json:"effect"`
	Reason                  string     `json:"reason"`
	At                      time.Time  `json:"at"`
	ResultRecorded          bool       `json:"result_recorded"`
	RetryBoundaryUnresolved bool       `json:"retry_boundary_unresolved,omitempty"`
}

type TerminalCheckpoint struct {
	Outcome TerminalOutcome `json:"outcome"`
	Summary string          `json:"summary,omitempty"`
	At      time.Time       `json:"at"`
}

type RecapCheckpoint struct {
	RunID      string            `json:"run_id"`
	Status     string            `json:"status"`
	StopReason string            `json:"stop_reason"`
	Summary    *ContentReference `json:"summary,omitempty"`
	At         time.Time         `json:"at"`
}

// Event has a closed payload set. Exactly one payload must be present and must
// match Kind. There is no arbitrary map or raw byte field.
type Event struct {
	ID                 string                   `json:"id"`
	Kind               EventKind                `json:"kind"`
	At                 time.Time                `json:"at"`
	Turn               *TurnCheckpoint          `json:"turn,omitempty"`
	ToolCall           *ToolCallCheckpoint      `json:"tool_call,omitempty"`
	ToolExecution      *ToolExecutionCheckpoint `json:"tool_execution,omitempty"`
	ToolResult         *ToolResultCheckpoint    `json:"tool_result,omitempty"`
	Permission         *PermissionCheckpoint    `json:"permission,omitempty"`
	Guidance           *GuidanceCheckpoint      `json:"guidance,omitempty"`
	Attachment         *AttachmentMetadata      `json:"attachment,omitempty"`
	RetryAuthorization *RetryAuthorization      `json:"retry_authorization,omitempty"`
	Recap              *RecapCheckpoint         `json:"recap,omitempty"`
	Terminal           *TerminalCheckpoint      `json:"terminal,omitempty"`
}

type JournalRecord struct {
	Version      string `json:"version"`
	SessionID    string `json:"session_id"`
	Sequence     uint64 `json:"sequence"`
	PreviousHash string `json:"previous_hash"`
	Event        Event  `json:"event"`
	Hash         string `json:"hash"`
}

type State struct {
	CurrentTurn         *TurnCheckpoint                    `json:"current_turn,omitempty"`
	RecentTurns         []TurnCheckpoint                   `json:"recent_turns,omitempty"`
	ToolCalls           map[string]ToolCallCheckpoint      `json:"tool_calls,omitempty"`
	ToolExecutions      map[string]ToolExecutionCheckpoint `json:"tool_executions,omitempty"`
	ToolResults         map[string]ToolResultCheckpoint    `json:"tool_results,omitempty"`
	PendingPermissions  map[string]PermissionCheckpoint    `json:"pending_permissions,omitempty"`
	QueuedGuidance      map[string]GuidanceCheckpoint      `json:"queued_guidance,omitempty"`
	Attachments         map[string]AttachmentMetadata      `json:"attachments,omitempty"`
	RetryAuthorizations map[string]RetryAuthorization      `json:"retry_authorizations,omitempty"`
	ActionGuards        map[string]ActionGuard             `json:"action_guards,omitempty"`
	RecentRecaps        []RecapCheckpoint                  `json:"recent_recaps,omitempty"`
	Terminal            *TerminalCheckpoint                `json:"terminal,omitempty"`
	LastSequence        uint64                             `json:"last_sequence"`
	LastHash            string                             `json:"last_hash"`
}

type Snapshot struct {
	Version         string    `json:"version"`
	SessionID       string    `json:"session_id"`
	ThroughSequence uint64    `json:"through_sequence"`
	ThroughHash     string    `json:"through_hash"`
	State           State     `json:"state"`
	CreatedAt       time.Time `json:"created_at"`
	Hash            string    `json:"hash"`
}

type Limits struct {
	MaxRecordBytes        int
	MaxJournalBytes       int64
	MaxJournalRecords     int
	MaxSnapshotBytes      int64
	MaxSummaryBytes       int
	MaxRecentTurns        int
	MaxToolResults        int
	MaxActionGuards       int
	MaxRecentRecaps       int
	MaxPendingPermissions int
	MaxQueuedGuidance     int
	MaxAttachments        int
	MaxEvidenceBytes      int64
}

type Redactor func(string) string

type Options struct {
	Limits   Limits
	Redactor Redactor
	Clock    func() time.Time
}

func DefaultOptions() Options {
	return Options{
		Limits: Limits{
			MaxRecordBytes:        256 << 10,
			MaxJournalBytes:       16 << 20,
			MaxJournalRecords:     20000,
			MaxSnapshotBytes:      8 << 20,
			MaxSummaryBytes:       2048,
			MaxRecentTurns:        32,
			MaxToolResults:        512,
			MaxActionGuards:       512,
			MaxRecentRecaps:       32,
			MaxPendingPermissions: 256,
			MaxQueuedGuidance:     256,
			MaxAttachments:        256,
			MaxEvidenceBytes:      64 << 20,
		},
		Clock: time.Now,
	}
}

type RecordResult struct {
	Applied   bool   `json:"applied"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Sequence  uint64 `json:"sequence"`
	Hash      string `json:"hash,omitempty"`
}

type Recovery struct {
	State           State `json:"state"`
	CrashDetected   bool  `json:"crash_detected"`
	SnapshotUsed    bool  `json:"snapshot_used"`
	ReplayedRecords int   `json:"replayed_records"`
	PhysicalRecords int   `json:"physical_records"`
	TrustedOffset   int64 `json:"trusted_offset"`
	FailureOffset   int64 `json:"failure_offset,omitempty"`
	Corrupt         bool  `json:"corrupt,omitempty"`
}

type BeginTurnRequest struct {
	TurnID  string
	Ordinal uint64
	Prompt  *ContentReference
	Summary string
}

type FinishTurnRequest struct {
	TurnID  string
	Status  TurnStatus
	Summary string
}

type ToolCallRequest struct {
	CallID       string
	TurnID       string
	Name         string
	ActionDigest string
	InputVersion string `json:"input_version,omitempty"`
	Effect       ToolEffect
	Arguments    *ContentReference
	// ObservedCallID is supplied only by a live runtime after returning this
	// completed result to the model and observing an intervening edit or a
	// trusted input-version change. The attestation cannot be inferred from
	// recovered history; its checkpoint is retained to validate journal replay.
	// It is not an operator retry authorization.
	ObservedCallID string
}

type ToolExecutionRequest struct {
	CallID        string
	Outcome       ExecutionOutcome
	ProcessExited bool
	Summary       string
}

type ToolResultRequest struct {
	CallID  string
	Name    string
	Output  *ContentReference
	Summary string
}

type PermissionRequest struct {
	RequestID string
	TurnID    string
	CallID    string
	Scope     string
	Reason    string
}

type PermissionResolution struct {
	RequestID            string
	Decision             PermissionDecision
	Reason               string
	RetryAuthorizationID string
	RetryApprovedBy      string
	RetryReason          string
}

type GuidanceRequest struct {
	GuidanceID string
	TurnID     string
	Content    *ContentReference
}

type GuidanceResolution struct {
	GuidanceID string
	Status     GuidanceStatus
	Reason     string
}

type RetryAuthorizationRequest struct {
	AuthorizationID string
	ActionDigest    string
	ApprovedBy      string
	Reason          string
}

type RecapRequest struct {
	RunID      string
	Status     string
	StopReason string
	Summary    *ContentReference
}

type RetryDecision struct {
	Allowed               bool
	RequiresAuthorization bool
	Guard                 *ActionGuard
	AuthorizationID       string
}

type ResumeMode string

const (
	ResumeNoAction        ResumeMode = "no_action"
	ResumeTurn            ResumeMode = "resume_turn"
	ResumeAwaitPermission ResumeMode = "await_permission"
	ResumeApplyGuidance   ResumeMode = "apply_guidance"
	ResumeManualReview    ResumeMode = "manual_review"
	ResumeBlockedCorrupt  ResumeMode = "blocked_corrupt"
)

type ResumePlan struct {
	Mode                 ResumeMode                `json:"mode"`
	CrashDetected        bool                      `json:"crash_detected"`
	TurnID               string                    `json:"turn_id,omitempty"`
	CompletedToolCallIDs []string                  `json:"completed_tool_call_ids,omitempty"`
	RetryableToolCalls   []ToolCallCheckpoint      `json:"retryable_tool_calls,omitempty"`
	ReviewRequired       []ActionGuard             `json:"review_required,omitempty"`
	PendingPermissions   []PermissionCheckpoint    `json:"pending_permissions,omitempty"`
	QueuedGuidance       []GuidanceCheckpoint      `json:"queued_guidance,omitempty"`
	Attachments          []AttachmentMetadata      `json:"attachments,omitempty"`
	OutputEvidence       []RecoveredOutputEvidence `json:"output_evidence,omitempty"`
	RecapEvidence        []RecoveredRecapEvidence  `json:"recap_evidence,omitempty"`
	NextSteps            []string                  `json:"next_steps,omitempty"`
}

type RecoveredOutputEvidence struct {
	CallID  string    `json:"call_id"`
	Name    string    `json:"name"`
	Content string    `json:"content"`
	SHA256  string    `json:"sha256"`
	Bytes   int64     `json:"bytes"`
	At      time.Time `json:"at"`
}

type RecoveredRecapEvidence struct {
	RunID      string    `json:"run_id"`
	Status     string    `json:"status"`
	StopReason string    `json:"stop_reason"`
	Content    string    `json:"content"`
	SHA256     string    `json:"sha256"`
	Bytes      int64     `json:"bytes"`
	At         time.Time `json:"at"`
}

// Recorder is the transport-neutral integration boundary intended for one
// later Harness integrator.
type Recorder interface {
	BeginTurn(BeginTurnRequest) (RecordResult, error)
	FinishTurn(FinishTurnRequest) (RecordResult, error)
	RecordToolCall(ToolCallRequest) (RecordResult, error)
	RecordToolExecution(ToolExecutionRequest) (RecordResult, error)
	RecordToolResult(ToolResultRequest) (RecordResult, error)
	RequestPermission(PermissionRequest) (RecordResult, error)
	ResolvePermission(PermissionResolution) (RecordResult, error)
	QueueGuidance(GuidanceRequest) (RecordResult, error)
	ResolveGuidance(GuidanceResolution) (RecordResult, error)
	RecordAttachment(AttachmentMetadata) (RecordResult, error)
	AuthorizeRetry(RetryAuthorizationRequest) (RecordResult, error)
	RecordRecap(RecapRequest) (RecordResult, error)
	CheckRetry(actionDigest string, effect ToolEffect, retryTurnID string) RetryDecision
	FinishSession(outcome TerminalOutcome, summary string) (RecordResult, error)
	State() State
	SaveSnapshot() error
	Compact(reason string) error
	Close() error
}
