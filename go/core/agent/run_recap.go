package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// RunRecap is the transport-neutral end-of-run projection of the execution
// ledger. It is factual UI data, not another model-authored answer.
type RunRecap struct {
	Status            string               `json:"status"`
	StopReason        StopReason           `json:"stopReason"`
	Turns             int                  `json:"turns"`
	ToolCalls         int                  `json:"toolCalls"`
	SuccessfulTools   int                  `json:"successfulTools"`
	FailedTools       int                  `json:"failedTools"`
	RecoveredFailures int                  `json:"recoveredFailures"`
	DurationMS        int64                `json:"durationMs"`
	ChangedPaths      []string             `json:"changedPaths,omitempty"`
	Activities        []RunRecapActivity   `json:"activities,omitempty"`
	Checks            []RunRecapCheck      `json:"checks,omitempty"`
	Outstanding       []RunRecapIssue      `json:"outstanding,omitempty"`
	Corrections       []RunRecapCorrection `json:"corrections,omitempty"`
	TokenBudget       *RunTokenUsage       `json:"tokenBudget,omitempty"`
	Handoff           *RunHandoffReceipt   `json:"handoff,omitempty"`
	InvalidatedChecks int                  `json:"invalidatedChecks,omitempty"`
}

type RunRecapActivity struct {
	Effect Effect `json:"effect"`
	Count  int    `json:"count"`
}

type RunRecapCheck struct {
	Tool    string `json:"tool"`
	CallID  string `json:"callId"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
	// ObservedStatus retains the executed verdict when current input provenance
	// makes the projected Status stale/unverified. It is not a new execution.
	ObservedStatus   string         `json:"observedStatus,omitempty"`
	Evidence         *CheckEvidence `json:"evidence,omitempty"`
	ReplacedByCallID string         `json:"replacedByCallId,omitempty"`
}

type RunRecapIssue struct {
	Tool     string `json:"tool"`
	Status   string `json:"status"`
	Summary  string `json:"summary,omitempty"`
	Attempts int    `json:"attempts"`
}

type codexStyleFailureRecord struct {
	Tool             string
	Status           ToolStatus
	Summary          string
	Attempts         int
	Epoch            int
	Recovered        bool
	Superseded       bool
	CallID           string
	VerificationKeys []string
}

// codexStyleExecutionLedger provides the two control-plane features Claude's
// loop relies on: a failed call cannot be replayed against unchanged evidence,
// and completion UI is projected from executions rather than model prose.
type codexStyleExecutionLedger struct {
	evidenceEpoch         int
	failedTools           int
	recovered             int
	activities            map[Effect]int
	failures              map[string]*codexStyleFailureRecord
	failureOrder          []string
	checks                map[string]RunRecapCheck
	checkOrder            []string
	checkDetails          map[string]codexStyleVerificationDetails
	observations          map[string]*verificationCallObservation
	observationSequence   int
	mutationSequence      int
	corrections           []RunRecapCorrection
	verificationWorkspace *tools.Workspace
	verificationObserver  tools.WorkspaceFileStateObserver
	verificationContext   context.Context
	revisionBoundEvidence bool
	invalidatedChecks     map[string]bool
	evidenceInvalidations int
}

func newCodexStyleExecutionLedger() *codexStyleExecutionLedger {
	return &codexStyleExecutionLedger{
		activities:   make(map[Effect]int),
		failures:     make(map[string]*codexStyleFailureRecord),
		checks:       make(map[string]RunRecapCheck),
		checkDetails: make(map[string]codexStyleVerificationDetails),
	}
}

func (l *codexStyleExecutionLedger) NoteExternalEvidence() {
	if l == nil {
		return
	}
	l.resolveOutstanding()
	l.evidenceEpoch++
}

func (l *codexStyleExecutionLedger) ObserveToolSearch() {
	if l == nil {
		return
	}
	l.activities[EffectRead]++
	l.resolveOutstanding()
	l.evidenceEpoch++
}

func (l *codexStyleExecutionLedger) CanBatch(calls []provider.ToolCall) bool {
	if l == nil {
		return true
	}
	for _, call := range calls {
		if _, blocked := l.ReplayBlock(call); blocked {
			return false
		}
	}
	return true
}

func (l *codexStyleExecutionLedger) ReplayBlock(call provider.ToolCall) (string, bool) {
	if l == nil {
		return "", false
	}
	fingerprint := canonicalToolCallFingerprint(call)
	previous := l.failures[fingerprint]
	if previous == nil || previous.Recovered || previous.Superseded || previous.Epoch != l.evidenceEpoch {
		return "", false
	}
	message := "ERROR: this exact action already failed and no successful tool result or new user guidance has added evidence since then. Change the arguments, choose another relevant tool, gather a different bounded diagnostic, ask the user for required information, or report the concrete blocker."
	if previous.Summary != "" {
		message += " Previous failure: " + previous.Summary
	}
	return message, true
}

func (l *codexStyleExecutionLedger) Observe(call provider.ToolCall, execution ToolExecution) {
	if l == nil {
		return
	}
	l.observeCorrectionReference(call, execution)
	l.refreshVerificationEvidence()
	// Duplicate-success suppression reuses prior evidence. It is neither a
	// freshly passing check nor a new failure, and must not erase the old pass.
	if execution.Status == ToolOK && !execution.Executed {
		return
	}
	if execution.Effect != EffectNone {
		l.activities[execution.Effect]++
	}
	validation := codexStyleExecutionIsValidation(execution)
	if validation {
		l.observeCheck(call, execution)
	}
	// The browser interaction helper successfully ran, but explicitly declined
	// to claim gameplay or motion.  That is a pending human acceptance step, not
	// a passing validator and not recovery evidence for an earlier failure.
	if codexStyleInteractionNeedsHumanReview(execution) {
		return
	}
	if execution.Status == ToolOK {
		l.resolveOutstanding()
		l.evidenceEpoch++
		if !validation || verificationRecoveryPassed(execution) && l.verificationCheckCanRecover(canonicalToolCallFingerprint(call)) {
			if validation {
				l.resolveFreshPassingChecks(call, execution)
				l.resolveVerified(verificationRecoveryKeys(call, execution, true))
			}
			return
		}
	}
	if execution.Status == ToolCancelled {
		return
	}
	if execution.Status != ToolOK {
		l.failedTools++
	}
	fingerprint := canonicalToolCallFingerprint(call)
	record := l.failures[fingerprint]
	if record == nil {
		record = &codexStyleFailureRecord{Tool: execution.Tool}
		l.failures[fingerprint] = record
		l.failureOrder = append(l.failureOrder, fingerprint)
	}
	record.Status = execution.Status
	record.CallID = call.ID
	record.Superseded = false
	if validation {
		record.VerificationKeys = verificationRecoveryKeys(call, execution, false)
		if execution.Status == ToolOK {
			record.Status = ToolStatus("unverified")
		}
	}
	record.Summary = codexStyleRecapOneLine(toolExecutionRecapSummary(execution), 220)
	record.Attempts++
	record.Epoch = l.evidenceEpoch
	record.Recovered = false
}

func (l *codexStyleExecutionLedger) DecorateFailure(execution ToolExecution) ToolExecution {
	// Keep tool_result payloads faithful to the tool, as Claude Code does.
	// The ledger may classify outcomes for UI/recap and safety code may block an
	// unsafe replay, but the harness must not append hidden next-step coaching.
	return execution
}

func (l *codexStyleExecutionLedger) HasOutstandingFailure() bool {
	if l == nil {
		return false
	}
	l.refreshVerificationEvidence()
	if l.hasInvalidatedPassingCheck() {
		return true
	}
	for _, record := range l.failures {
		if !record.Recovered && !record.Superseded {
			return true
		}
	}
	return false
}

func (l *codexStyleExecutionLedger) Build(
	stop StopReason,
	turns int,
	toolCalls int,
	successfulTools int,
	changedPaths []string,
	durationMS int64,
) *RunRecap {
	if l == nil || toolCalls == 0 {
		return nil
	}
	recap := &RunRecap{
		Status:            codexStyleRecapStatus(stop, successfulTools, len(changedPaths), l.HasOutstandingFailure()),
		StopReason:        stop,
		Turns:             turns,
		ToolCalls:         toolCalls,
		SuccessfulTools:   successfulTools,
		FailedTools:       l.failedTools,
		RecoveredFailures: l.recovered,
		DurationMS:        durationMS,
		ChangedPaths:      codexStyleUniqueStrings(changedPaths),
		Corrections:       append([]RunRecapCorrection(nil), l.corrections...),
		InvalidatedChecks: l.evidenceInvalidations,
	}
	for _, effect := range []Effect{EffectRead, EffectWrite, EffectExecute, EffectInteract, EffectDestructive} {
		if count := l.activities[effect]; count > 0 {
			recap.Activities = append(recap.Activities, RunRecapActivity{Effect: effect, Count: count})
		}
	}
	for _, key := range l.checkOrder {
		recap.Checks = append(recap.Checks, l.checks[key])
	}
	for _, fingerprint := range l.failureOrder {
		record := l.failures[fingerprint]
		if record == nil || record.Recovered || record.Superseded {
			continue
		}
		recap.Outstanding = append(recap.Outstanding, RunRecapIssue{
			Tool: record.Tool, Status: string(record.Status), Summary: record.Summary, Attempts: record.Attempts,
		})
	}
	for _, key := range l.checkOrder {
		check := l.checks[key]
		if check.ObservedStatus == "passed" && check.Status != "passed" && check.Status != "superseded" {
			recap.Outstanding = append(recap.Outstanding, RunRecapIssue{Tool: check.Tool, Status: check.Status, Summary: "Previously passing check has " + check.Evidence.Freshness + " input evidence; " + check.Evidence.Reason, Attempts: 1})
		}
	}
	return recap
}

func (l *codexStyleExecutionLedger) resolveOutstanding() {
	resolved := 0
	for _, record := range l.failures {
		if !record.Recovered && !record.Superseded && len(record.VerificationKeys) == 0 {
			record.Recovered = true
			resolved++
		}
	}
	l.recovered += resolved
}

func (l *codexStyleExecutionLedger) observeCheck(call provider.ToolCall, execution ToolExecution) {
	key := canonicalToolCallFingerprint(call)
	if _, exists := l.checks[key]; !exists {
		l.checkOrder = append(l.checkOrder, key)
	}
	status := "failed"
	if codexStyleInteractionNeedsHumanReview(execution) {
		status = "needs_human_review"
	} else if verificationRecoveryPassed(execution) {
		status = "passed"
	} else if execution.Status == ToolOK && execution.ValidationStatus != VerificationFailed {
		status = "unverified"
	}
	l.checks[key] = RunRecapCheck{
		Tool: execution.Tool, CallID: execution.CallID, Status: status,
		Summary: codexStyleRecapOneLine(toolExecutionRecapSummary(execution), 180),
	}
	if l.revisionBoundEvidence {
		check := l.checks[key]
		check.Evidence = captureCheckEvidence(execution)
		l.checks[key] = check
		delete(l.invalidatedChecks, key)
		l.refreshVerificationEvidence()
	}
	l.observeVerificationDetails(key, call, execution, status)
}

func codexStyleInteractionNeedsHumanReview(execution ToolExecution) bool {
	return execution.Tool == "meadow_browser_interaction_check" &&
		strings.Contains(strings.ToUpper(execution.LLMContent), "INTERACTION_HUMAN_REVIEW_REQUIRED")
}

func codexStyleExecutionIsValidation(execution ToolExecution) bool {
	// Saving source (including attached parser diagnostics), bookkeeping and a
	// page's own WebMCP tool result are not native acceptance observations.
	if execution.Effect == EffectWrite || execution.Tool == "write_file" || execution.Tool == "edit" || execution.Tool == "edit_file" || execution.Tool == tools.TaskPlanToolName || execution.Tool == "todo_write" || execution.Tool == "meadow_browser_webmcp" {
		return false
	}
	if execution.Tool == "meadow_browser_inspect" || (execution.Tool == "meadow_browser_suite" && execution.Args["operation"] == "inspect") {
		return false
	}
	if codexStyleInteractionNeedsHumanReview(execution) {
		return true
	}
	// The interaction tool is itself a first-class acceptance contract. Its
	// transport reports assertion failures through ToolExecution.Status/Err, so
	// a successful execution belongs in the durable validation recap even when
	// the model omitted a generic purpose="validation" argument.
	if execution.Tool == codexStyleInteractionToolName || execution.Tool == "meadow_browser_suite" {
		return true
	}
	if execution.Validation {
		return true
	}
	purpose, _ := execution.Args["purpose"].(string)
	switch strings.ToLower(strings.TrimSpace(purpose)) {
	case "validation", "verification", "test":
		return true
	default:
		return false
	}
}

func toolExecutionRecapSummary(execution ToolExecution) string {
	if value := strings.TrimSpace(execution.Display); value != "" {
		return value
	}
	if execution.Err != nil {
		return execution.Err.Error()
	}
	return strings.TrimSpace(execution.LLMContent)
}

func codexStyleRecapOneLine(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return value
}

func codexStyleRecapStatus(stop StopReason, successfulTools, changedPaths int, outstanding bool) string {
	switch stop {
	case StopCancelled:
		return "cancelled"
	case StopNeedsInput:
		return "needs_input"
	case StopCompleted:
		if !outstanding {
			return "completed"
		}
	}
	if successfulTools > 0 || changedPaths > 0 {
		return "partial"
	}
	return "failed"
}

func (r RunRecap) String() string {
	return fmt.Sprintf("%s: %d tools, %d successful, %d failed", r.Status, r.ToolCalls, r.SuccessfulTools, r.FailedTools)
}

// GroundedTechnicalCompletionText returns a Harness-owned completion message
// when Meadow Browser proved only the technical page boundary. Model prose is
// deliberately not reused here: a load/runtime check cannot prove gameplay,
// motion, controls, or subjective visual quality.
func (r RunRecap) GroundedTechnicalCompletionText() string {
	if r.Status != "completed" || !r.hasPassedCheck("meadow_browser_visual_check") {
		return ""
	}
	lines := make([]string, 0, 3)
	if paths := codexStyleRecapPaths(r.ChangedPaths, 4); paths != "" {
		lines = append(lines, "完成並已儲存："+paths)
	} else {
		lines = append(lines, "技術處理已完成。")
	}
	lines = append(lines,
		"技術驗證通過：Meadow Browser 已確認頁面載入、必要結構／Canvas 與瀏覽器 runtime 診斷。",
		"待人工驗收：鍵盤／觸控互動、角色移動、玩法正確性與主觀視覺品質。",
	)
	return strings.Join(lines, "\n\n")
}

func (r RunRecap) hasPassedCheck(tool string) bool {
	for _, check := range r.Checks {
		if check.Tool == tool && check.Status == "passed" {
			return true
		}
	}
	return false
}

func codexStyleRecapPaths(paths []string, limit int) string {
	paths = codexStyleUniqueStrings(paths)
	if limit > 0 && len(paths) > limit {
		paths = paths[:limit]
	}
	quoted := make([]string, 0, len(paths))
	for _, path := range paths {
		if path = strings.TrimSpace(path); path != "" {
			quoted = append(quoted, "`"+path+"`")
		}
	}
	return strings.Join(quoted, "、")
}
