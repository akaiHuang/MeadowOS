package agent

import (
	"context"

	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/sessionrecovery"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// StopReason is the stable product-facing reason a run ended.
type StopReason string

const (
	StopCompleted    StopReason = "completed"
	StopMaxTurns     StopReason = "max_turns"
	StopLoopDetected StopReason = "loop_detected"
	// StopAuditInconclusive means the selected validator still passes and the
	// passing artifact was preserved, but the bounded independent requirement
	// audit could not produce contract-bound evidence. It is deliberately not a
	// completion state: product surfaces must not present it as a strict pass.
	StopAuditInconclusive StopReason = "audit_inconclusive"
	StopTokenLimit        StopReason = "token_limit"
	StopCancelled         StopReason = "cancelled"
	StopTimeBudget        StopReason = "time_budget"
	StopTokenBudget       StopReason = "token_budget"
	StopEmpty             StopReason = "empty"
	StopIncomplete        StopReason = "incomplete"
	StopNeedsInput        StopReason = "needs_input"
)

type LoopType string

const (
	LoopIdenticalToolCalls LoopType = "consecutive_identical_tool_calls"
	LoopContentChanting    LoopType = "chanting_identical_sentences"
	LoopFileReads          LoopType = "read_file_loop"
	LoopActionStagnation   LoopType = "action_stagnation"
)

// RunMode is an explicit product-surface instruction. It is intentionally
// separate from model intent routing: Auto lets the resident model decide,
// while Research is a user-owned switch that starts the shared source-grounded
// research loop without relying on keywords in the prompt.
type RunMode string

const (
	RunModeAuto     RunMode = ""
	RunModeResearch RunMode = "research"
)

// RunInput is the single execution input shared by CLI, Desktop/Serve and
// tests. User preserves multimodal message parts; History is copied by Engine
// before mutation.
type RunInput struct {
	User    provider.Message
	History []provider.Message
	Mode    RunMode
	// InterruptedTask means the immediately preceding foreground run was
	// cancelled before it could finish. The model, not a keyword router,
	// decides whether the newest user message continues that task or replaces
	// it; when it continues, the original output form and constraints remain in
	// force.
	InterruptedTask bool
	// ResearchContinuation is the last compact, source-grounded ledger for this
	// chat. Kernel restores it only when DetectConversationRevision confirms the
	// new user turn is a correction/retry, never for an unrelated new question.
	ResearchContinuation *ResearchStateSnapshot
	// IndependentTask keeps conversational history available while preventing
	// model-authored execution state (for example task_plan) from crossing a
	// product-level task boundary such as queue_next.
	IndependentTask bool
	// RecordedTaskPlan is a trusted in-process projection of the latest
	// committed successful plan/result pair, supplied by the session host when
	// raw history has been compacted. Never populate it from request JSON or
	// model prose. IndependentTask ignores it; it does not verify completion.
	RecordedTaskPlan *tools.TaskPlanState
	// Attachments carries authorized durable metadata only. ContentParts remains
	// the model payload; recovery never persists attachment bytes or URLs.
	Attachments []sessionrecovery.AttachmentMetadata
}

// RunResult is the stable outcome shared by every Meadow surface.
type RunResult struct {
	FinalText     string
	FinalResponse *FinalResponse
	Turns         int
	ToolCalls     int
	Stop          StopReason
	Loop          LoopType
	Messages      []provider.Message
	// Recap is Harness-owned execution truth derived from ToolExecution records.
	// It is intentionally separate from model prose so CLI and Desktop can render
	// the same completion summary without letting the model rewrite failures.
	Recap *RunRecap
	// RavenWorkspace is an observed workspace-level Git manifest. Shared
	// attribution and unknown facts prevent fabricated integration readiness.
	RavenWorkspace *RavenWorkspaceManifest

	FirstPromptTokens int
	FirstResponseMs   int64
	// GuidanceEvents includes durable lifecycle events that a product surface
	// must reconcile even if its live event listener loses the run-finish race.
	GuidanceEvents []GuidanceEvent
	// ResearchState lets CLI/Desktop preserve accepted evidence across a normal
	// follow-up correction without serializing raw page bodies into chat history.
	ResearchState *ResearchStateSnapshot
	// Escalation is an honest product-level handoff recommendation. It is set
	// only after the current model has stopped without completing the task; it
	// never changes the stop reason or silently starts a second model run.
	Escalation *ModelEscalation
	// ResumePlan is the deterministic projection recovered for this existing
	// session identity. RecoveryWarning is non-empty only for an explicit
	// content-only degradation; tool execution fails closed instead.
	ResumePlan      *sessionrecovery.ResumePlan
	RecoveryWarning string
}

// RecoveryStatus is the transport-neutral recovery state emitted by the
// shared Harness and forwarded unchanged by daemon-backed presentation clients.
type RecoveryStatus struct {
	SessionID          string                      `json:"sessionId,omitempty"`
	ResumePlan         *sessionrecovery.ResumePlan `json:"resumePlan,omitempty"`
	RecoveryWarning    string                      `json:"recoveryWarning,omitempty"`
	Degraded           bool                        `json:"degraded,omitempty"`
	MutationFailClosed bool                        `json:"mutationFailClosed,omitempty"`
}

// ModelEscalation lets CLI and Desktop present the same capability boundary.
// ResumePrompt is a generic continuation instruction, not a hidden solution;
// the receiving model still owns planning, research and synthesis.
type ModelEscalation struct {
	SourceModel      string `json:"sourceModel"`
	RecommendedModel string `json:"recommendedModel"`
	Reason           string `json:"reason"`
	ResumePrompt     string `json:"resumePrompt"`
}

// RunEvent reuses the versioned sanitized observation contract.
type RunEvent = agenttrace.Event

// GuidanceMode declares whether text should affect the active run or start a
// later independent run. KernelRuntime accepts only GuidanceSteerCurrent;
// product surfaces own GuidanceQueueNext so queued work cannot leak into the
// active model transcript.
type GuidanceMode string

const (
	GuidanceSteerCurrent GuidanceMode = "steer_current"
	GuidanceQueueNext    GuidanceMode = "queue_next"
)

// GuidanceDisposition is the observable lifecycle of interactive guidance.
type GuidanceDisposition string

const (
	GuidanceAccepted  GuidanceDisposition = "accepted"
	GuidanceApplied   GuidanceDisposition = "applied"
	GuidanceQueued    GuidanceDisposition = "queued"
	GuidanceStarted   GuidanceDisposition = "started"
	GuidanceRejected  GuidanceDisposition = "rejected"
	GuidanceCancelled GuidanceDisposition = "cancelled"
)

// Guidance is opaque user content plus transport intent. The harness must not
// infer task semantics from the text or attachments.
type Guidance struct {
	ID   string
	Text string
	// PreserveText is trusted in-process admission policy, not model input.
	// It keeps ordinary conversation whitespace while still rejecting emptiness.
	PreserveText bool `json:"-"`
	ContentParts []provider.MessageContentPart
	Mode         GuidanceMode
}

// GuidanceReceipt confirms where accepted text will be observed.
type GuidanceReceipt struct {
	ID            string
	Mode          GuidanceMode
	Disposition   GuidanceDisposition
	TargetTurn    int
	QueuePosition int
}

// GuidanceEvent reports when accepted guidance is actually applied or cannot
// be applied. Receipts are immediate; events close the asynchronous lifecycle.
type GuidanceEvent = GuidanceReceipt

// Runtime is the model-agnostic execution facade. The legacy Engine implements
// it now; the extracted kernel can replace that implementation without changing
// CLI, Desktop or Serve call sites.
type Runtime interface {
	RunInput(context.Context, RunInput) (*RunResult, error)
}
