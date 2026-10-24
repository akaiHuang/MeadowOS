// Package agenttrace defines the behavior-neutral observation contract used to
// compare Meadow's legacy Engine with the extracted Go agent kernel.
package agenttrace

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

const SchemaVersion = 1

type Runtime string

const (
	RuntimeLegacy  Runtime = "legacy"
	RuntimeObserve Runtime = "observe"
	RuntimeShadow  Runtime = "shadow"
	RuntimeKernel  Runtime = "kernel"
)

func ParseRuntime(value string) (Runtime, error) {
	runtime := Runtime(strings.ToLower(strings.TrimSpace(value)))
	if runtime == "" {
		return RuntimeLegacy, nil
	}
	switch runtime {
	case RuntimeLegacy, RuntimeObserve, RuntimeShadow, RuntimeKernel:
		return runtime, nil
	default:
		return "", fmt.Errorf("unknown agent runtime %q (want legacy|observe|shadow|kernel)", value)
	}
}

type EventType string

const (
	EventRunStarted           EventType = "run.started"
	EventRunFinished          EventType = "run.finished"
	EventTurnStarted          EventType = "turn.started"
	EventTurnFinished         EventType = "turn.finished"
	EventModelRequested       EventType = "model.requested"
	EventModelFirstSignal     EventType = "model.first_signal"
	EventModelFinished        EventType = "model.finished"
	EventModelFailed          EventType = "model.failed"
	EventRouteSelected        EventType = "route.selected"
	EventToolRequested        EventType = "tool.requested"
	EventToolStarted          EventType = "tool.started"
	EventToolFinished         EventType = "tool.finished"
	EventArtifactValidated    EventType = "artifact.validated"
	EventPolicyAdmission      EventType = "policy.admission"
	EventPolicyNudge          EventType = "policy.nudge"
	EventVerificationStarted  EventType = "verification.started"
	EventVerificationFinished EventType = "verification.finished"
	EventFinalizationFinished EventType = "finalization.finished"
)

// Event is one sanitized orchestration event. Fields must contain metadata,
// counters and classifications only; prompts, secrets and file contents do not
// belong in the default trace.
type Event struct {
	SchemaVersion int            `json:"schema_version"`
	Sequence      int            `json:"sequence"`
	RunID         string         `json:"run_id"`
	Runtime       Runtime        `json:"runtime"`
	Type          EventType      `json:"type"`
	Time          string         `json:"time"`
	ElapsedMS     int64          `json:"elapsed_ms"`
	Turn          int            `json:"turn,omitempty"`
	Model         string         `json:"model,omitempty"`
	CWD           string         `json:"cwd,omitempty"`
	Fields        map[string]any `json:"fields,omitempty"`
}

type Flags struct {
	FalseComplete         bool `json:"false_complete"`
	DuplicateVerification bool `json:"duplicate_verification"`
	Contradiction         bool `json:"contradiction"`
}

type FinalizerShadow struct {
	StopReason      string `json:"stop_reason"`
	EvidenceSupport string `json:"evidence_support,omitempty"`
	ClaimSupported  bool   `json:"claim_supported"`
	CompletedTruth  bool   `json:"completed_truth"`
	Verification    string `json:"verification"`
	Reason          string `json:"reason"`
	LegacyAgreement bool   `json:"legacy_agreement"`
}

// Report is the stable, machine-readable outcome used by migration gates.
// Fields not yet supplied by the legacy Engine remain zero-valued rather than
// being inferred from model prose.
type Report struct {
	SchemaVersion           int              `json:"schema_version"`
	RunID                   string           `json:"run_id"`
	Runtime                 Runtime          `json:"runtime"`
	ModelLed                bool             `json:"model_led"`
	SelfIteration           bool             `json:"self_iteration"`
	Model                   string           `json:"model,omitempty"`
	CWD                     string           `json:"cwd,omitempty"`
	TaskCategory            string           `json:"task_category,omitempty"`
	RouteIntent             string           `json:"route_intent,omitempty"`
	RouteSelections         int              `json:"route_selections"`
	RouteToolCount          int              `json:"route_tool_count"`
	RoutePlannerFallbacks   int              `json:"route_planner_fallbacks"`
	RetrievalBytes          int              `json:"retrieval_bytes"`
	RetrievalSourceCount    int              `json:"retrieval_source_count"`
	RetrievalAttempts       int              `json:"retrieval_attempts"`
	RetrievalRefreshes      int              `json:"retrieval_refreshes"`
	RetrievalErrors         int              `json:"retrieval_errors"`
	StopReason              string           `json:"stop_reason"`
	EvidenceSupport         string           `json:"evidence_support,omitempty"`
	ClaimSupported          bool             `json:"claim_supported"`
	CompletedTruth          bool             `json:"completed_truth"`
	Turns                   int              `json:"turns"`
	ModelCalls              int              `json:"model_calls"`
	ToolCalls               int              `json:"tool_calls"`
	PromptTokens            int              `json:"prompt_tokens"`
	CompletionTokens        int              `json:"completion_tokens"`
	FirstPromptTokens       int              `json:"first_prompt_tokens"`
	FirstSignalMS           int64            `json:"first_signal_ms"`
	TotalLatencyMS          int64            `json:"total_latency_ms"`
	ContextMemoryBytes      int              `json:"context_memory_bytes"`
	GlobalMemoryIncluded    bool             `json:"global_memory_included"`
	ProjectMemoryIncluded   bool             `json:"project_memory_included"`
	HistoryCompacted        bool             `json:"history_compacted"`
	HistoryInputMessages    int              `json:"history_input_messages"`
	HistoryOutputMessages   int              `json:"history_output_messages"`
	HistoryDroppedMessages  int              `json:"history_dropped_messages"`
	HistoryInputBytes       int              `json:"history_input_bytes"`
	HistoryOutputBytes      int              `json:"history_output_bytes"`
	MalformedToolCalls      int              `json:"malformed_tool_calls"`
	RepairedToolCalls       int              `json:"repaired_tool_calls"`
	InvalidToolArguments    int              `json:"invalid_tool_arguments"`
	ToolFailures            int              `json:"tool_failures"`
	RepeatedToolCalls       int              `json:"repeated_tool_calls"`
	ArtifactValidations     int              `json:"artifact_validations"`
	ArtifactRepairs         int              `json:"artifact_repairs"`
	WebArtifactRepairs      int              `json:"web_artifact_repairs"`
	NudgeCounts             map[string]int   `json:"nudge_counts,omitempty"`
	NudgeDenials            int              `json:"nudge_denials"`
	TaskPlanItems           int              `json:"task_plan_items"`
	TaskPlanRestoredCalls   int              `json:"task_plan_restored_calls"`
	TaskPlanUpdates         int              `json:"task_plan_updates"`
	ControllerEnabled       bool             `json:"controller_enabled"`
	ControllerPhase         string           `json:"controller_phase,omitempty"`
	ControllerRecoveries    int              `json:"controller_recoveries"`
	SuccessfulToolCalls     int              `json:"successful_tool_calls"`
	SuccessfulInspections   int              `json:"successful_inspections"`
	SuccessfulMutations     int              `json:"successful_mutations"`
	SuccessfulEvidenceCalls int              `json:"successful_evidence_calls"`
	ConsecutiveToolFailures int              `json:"consecutive_tool_failures"`
	VerificationVerdict     string           `json:"verification_verdict,omitempty"`
	ValidatorCommand        string           `json:"validator_command,omitempty"`
	ValidatorSource         string           `json:"validator_source,omitempty"`
	ValidatorExitCode       int              `json:"validator_exit_code"`
	HasValidatorExitCode    bool             `json:"has_validator_exit_code,omitempty"`
	ChangedPaths            []string         `json:"changed_paths,omitempty"`
	ProducedDeliverables    []string         `json:"produced_deliverables,omitempty"`
	FinalizerShadow         *FinalizerShadow `json:"finalizer_shadow,omitempty"`
	FinalizerAuthoritative  bool             `json:"finalizer_authoritative"`
	EvidenceGateAuthority   bool             `json:"evidence_gate_authoritative"`
	FinalResponseComposed   bool             `json:"final_response_composed"`
	FinalResponseBytes      int              `json:"final_response_bytes"`
	Flags                   Flags            `json:"flags"`
	Error                   string           `json:"error,omitempty"`
}

// Observer receives immutable copies of events and the final report. An
// observer must be fail-open: telemetry failure must never change task behavior.
type Observer interface {
	Observe(Event)
	ObserveReport(Report)
}

type RunMeta struct {
	RunID   string
	Runtime Runtime
	Model   string
	CWD     string
}

type Finish struct {
	TaskCategory            string
	ModelLed                bool
	SelfIteration           bool
	StopReason              string
	EvidenceSupport         string
	ClaimSupported          bool
	CompletedTruth          bool
	Turns                   int
	ToolCalls               int
	FirstPromptTokens       int
	FirstSignalMS           int64
	ProducedDeliverables    []string
	VerificationVerdict     string
	ValidatorCommand        string
	ValidatorSource         string
	ValidatorExitCode       int
	HasValidatorExitCode    bool
	ChangedPaths            []string
	ControllerEnabled       bool
	ControllerPhase         string
	ControllerRecoveries    int
	SuccessfulToolCalls     int
	SuccessfulInspections   int
	SuccessfulMutations     int
	SuccessfulEvidenceCalls int
	ConsecutiveToolFailures int
	FinalizerShadow         *FinalizerShadow
	FinalizerAuthoritative  bool
	EvidenceGateAuthority   bool
	Flags                   Flags
	Error                   error
}

// Emitter owns monotonic event sequencing and report aggregation for one run.
type Emitter struct {
	mu       sync.Mutex
	observer Observer
	meta     RunMeta
	started  time.Time
	seq      int
	finished bool
	report   Report
}

func Start(observer Observer, meta RunMeta) *Emitter {
	if observer == nil {
		return nil
	}
	if meta.RunID == "" {
		meta.RunID = newRunID()
	}
	if meta.Runtime == "" {
		meta.Runtime = RuntimeLegacy
	}
	emitter := &Emitter{
		observer: observer,
		meta:     meta,
		started:  time.Now(),
		report: Report{
			SchemaVersion: SchemaVersion,
			RunID:         meta.RunID,
			Runtime:       meta.Runtime,
			Model:         meta.Model,
			CWD:           meta.CWD,
		},
	}
	emitter.Emit(EventRunStarted, 0, nil)
	return emitter
}

func (e *Emitter) Emit(kind EventType, turn int, fields map[string]any) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.finished {
		e.mu.Unlock()
		return
	}
	e.seq++
	e.aggregateLocked(kind, fields)
	event := e.eventLocked(kind, turn, fields)
	observer := e.observer
	e.mu.Unlock()
	notifyEvent(observer, event)
}

func (e *Emitter) Finish(f Finish) Report {
	if e == nil {
		return Report{}
	}
	e.mu.Lock()
	if e.finished {
		report := e.report
		e.mu.Unlock()
		return report
	}
	e.report.TaskCategory = f.TaskCategory
	e.report.ModelLed = f.ModelLed
	e.report.SelfIteration = f.SelfIteration
	e.report.StopReason = f.StopReason
	e.report.EvidenceSupport = f.EvidenceSupport
	e.report.ClaimSupported = f.ClaimSupported
	e.report.CompletedTruth = f.CompletedTruth
	e.report.Turns = f.Turns
	e.report.ToolCalls = f.ToolCalls
	e.report.FirstPromptTokens = f.FirstPromptTokens
	if f.FirstSignalMS > 0 {
		e.report.FirstSignalMS = f.FirstSignalMS
	}
	e.report.TotalLatencyMS = elapsedMS(e.started)
	e.report.ProducedDeliverables = append([]string(nil), f.ProducedDeliverables...)
	e.report.VerificationVerdict = f.VerificationVerdict
	e.report.ValidatorCommand = f.ValidatorCommand
	e.report.ValidatorSource = f.ValidatorSource
	e.report.ValidatorExitCode = f.ValidatorExitCode
	e.report.HasValidatorExitCode = f.HasValidatorExitCode
	e.report.ChangedPaths = append([]string(nil), f.ChangedPaths...)
	e.report.ControllerEnabled = f.ControllerEnabled
	e.report.ControllerPhase = f.ControllerPhase
	e.report.ControllerRecoveries = f.ControllerRecoveries
	e.report.SuccessfulToolCalls = f.SuccessfulToolCalls
	e.report.SuccessfulInspections = f.SuccessfulInspections
	e.report.SuccessfulMutations = f.SuccessfulMutations
	e.report.SuccessfulEvidenceCalls = f.SuccessfulEvidenceCalls
	e.report.ConsecutiveToolFailures = f.ConsecutiveToolFailures
	if f.FinalizerShadow != nil {
		shadow := *f.FinalizerShadow
		e.report.FinalizerShadow = &shadow
	}
	e.report.FinalizerAuthoritative = f.FinalizerAuthoritative
	e.report.EvidenceGateAuthority = f.EvidenceGateAuthority
	e.report.Flags = f.Flags
	if f.Error != nil {
		e.report.Error = f.Error.Error()
	}
	e.seq++
	event := e.eventLocked(EventRunFinished, f.Turns, map[string]any{
		"stop_reason":      f.StopReason,
		"evidence_support": f.EvidenceSupport,
		"claim_supported":  f.ClaimSupported,
		"completed_truth":  f.CompletedTruth,
	})
	e.finished = true
	report := e.report
	observer := e.observer
	e.mu.Unlock()

	notifyEvent(observer, event)
	notifyReport(observer, report)
	return report
}

func notifyEvent(observer Observer, event Event) {
	defer func() { _ = recover() }()
	observer.Observe(event)
}

func notifyReport(observer Observer, report Report) {
	defer func() { _ = recover() }()
	observer.ObserveReport(report)
}

func (e *Emitter) eventLocked(kind EventType, turn int, fields map[string]any) Event {
	return Event{
		SchemaVersion: SchemaVersion,
		Sequence:      e.seq,
		RunID:         e.meta.RunID,
		Runtime:       e.meta.Runtime,
		Type:          kind,
		Time:          time.Now().UTC().Format(time.RFC3339Nano),
		ElapsedMS:     elapsedMS(e.started),
		Turn:          turn,
		Model:         e.meta.Model,
		CWD:           e.meta.CWD,
		Fields:        cloneFields(fields),
	}
}

func (e *Emitter) aggregateLocked(kind EventType, fields map[string]any) {
	switch kind {
	case EventRouteSelected:
		e.report.RouteSelections++
		if e.report.RouteIntent == "" {
			e.report.RouteIntent = stringField(fields, "intent")
		}
		if e.report.RouteToolCount == 0 {
			e.report.RouteToolCount = listLen(fields["tools"])
		}
		if boolField(fields, "planner_fallback") {
			e.report.RoutePlannerFallbacks++
		}
		if value := intField(fields, "retrieval_bytes"); value > e.report.RetrievalBytes {
			e.report.RetrievalBytes = value
		}
		if value := intField(fields, "retrieval_source_count"); value > e.report.RetrievalSourceCount {
			e.report.RetrievalSourceCount = value
		}
		if boolField(fields, "retrieval_attempted") {
			e.report.RetrievalAttempts++
			if stringField(fields, "retrieval_phase") == "recovery" {
				e.report.RetrievalRefreshes++
			}
		}
		if stringField(fields, "retrieval_error") != "" {
			e.report.RetrievalErrors++
		}
		if value := intField(fields, "task_plan_items"); value > e.report.TaskPlanItems {
			e.report.TaskPlanItems = value
		}
		if value := intField(fields, "task_plan_restored_calls"); value > e.report.TaskPlanRestoredCalls {
			e.report.TaskPlanRestoredCalls = value
		}
	case EventModelRequested:
		e.report.ModelCalls++
		if e.report.ModelCalls == 1 {
			e.report.ContextMemoryBytes = intField(fields, "context_memory_bytes")
			e.report.GlobalMemoryIncluded = boolField(fields, "global_memory_included")
			e.report.ProjectMemoryIncluded = boolField(fields, "project_memory_included")
			e.report.HistoryCompacted = boolField(fields, "history_compacted")
			e.report.HistoryInputMessages = intField(fields, "history_input_messages")
			e.report.HistoryOutputMessages = intField(fields, "history_output_messages")
			e.report.HistoryDroppedMessages = intField(fields, "history_dropped_messages")
			e.report.HistoryInputBytes = intField(fields, "history_input_bytes")
			e.report.HistoryOutputBytes = intField(fields, "history_output_bytes")
		}
	case EventModelFirstSignal:
		if e.report.FirstSignalMS == 0 {
			e.report.FirstSignalMS = elapsedMS(e.started)
		}
	case EventModelFinished:
		e.report.PromptTokens += intField(fields, "prompt_tokens")
		e.report.CompletionTokens += intField(fields, "completion_tokens")
		e.report.RepairedToolCalls += intField(fields, "repaired_tool_calls")
	case EventModelFailed:
		e.report.PromptTokens += intField(fields, "prompt_tokens")
		e.report.CompletionTokens += intField(fields, "completion_tokens")
		if stringField(fields, "error_class") == "malformed_tool_call" {
			e.report.MalformedToolCalls++
		}
	case EventToolFinished:
		if status := stringField(fields, "status"); status != "" && status != "ok" {
			e.report.ToolFailures++
		}
		if stringField(fields, "error_class") == "invalid_tool_arguments" {
			e.report.InvalidToolArguments++
		}
		if stringField(fields, "tool") == "task_plan" && stringField(fields, "status") == "ok" {
			e.report.TaskPlanUpdates++
		}
	case EventArtifactValidated:
		e.report.ArtifactValidations++
		if stringField(fields, "status") == "repaired" {
			e.report.ArtifactRepairs++
			if stringField(fields, "capability") == "web_runtime_repair" {
				e.report.WebArtifactRepairs++
			}
		}
	case EventPolicyNudge:
		if stringField(fields, "kind") == "repeated_tool" {
			e.report.RepeatedToolCalls++
		}
		if boolField(fields, "allowed") {
			if e.report.NudgeCounts == nil {
				e.report.NudgeCounts = make(map[string]int)
			}
			if kind := stringField(fields, "kind"); kind != "" {
				e.report.NudgeCounts[kind]++
			}
		} else {
			e.report.NudgeDenials++
		}
	case EventFinalizationFinished:
		e.report.FinalResponseComposed = boolField(fields, "response_composed")
		e.report.FinalResponseBytes = intField(fields, "response_bytes")
	}
}

func cloneFields(fields map[string]any) map[string]any {
	if len(fields) == 0 {
		return nil
	}
	copy := make(map[string]any, len(fields))
	for key, value := range fields {
		copy[key] = value
	}
	return copy
}

func intField(fields map[string]any, key string) int {
	switch value := fields[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func stringField(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}

func listLen(value any) int {
	switch list := value.(type) {
	case []string:
		return len(list)
	case []any:
		return len(list)
	default:
		return 0
	}
}

func boolField(fields map[string]any, key string) bool {
	value, _ := fields[key].(bool)
	return value
}

func elapsedMS(start time.Time) int64 {
	value := time.Since(start).Round(time.Millisecond).Milliseconds()
	if value < 0 {
		return 0
	}
	return value
}

func newRunID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(data[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
