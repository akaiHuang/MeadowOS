package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const verificationCorrectToolName = "verification_correct"

// A correction is a model-declared test revision, not a passing observation or
// an independently verified explanation. Original calls and results stay intact.
type RunRecapCorrection struct {
	OriginalCallID    string   `json:"originalCallId"`
	ReplacementCallID string   `json:"replacementCallId"`
	CorrectionCallID  string   `json:"correctionCallId"`
	Tool              string   `json:"tool"`
	Reason            string   `json:"reason"`
	EvidenceCallIDs   []string `json:"evidenceCallIds"`
	Status            string   `json:"status"`
}

type verificationCorrectTool struct{}

func (*verificationCorrectTool) Name() string     { return verificationCorrectToolName }
func (*verificationCorrectTool) Kind() tools.Kind { return tools.KindReadOnly }
func (*verificationCorrectTool) Description() string {
	return "Explicitly record a model-declared correction to an erroneous expected value in a meadow_browser_interaction_check. First inspect independent tool evidence and execute a passing replacement with the same file, requirement, coverage and ordered steps; only the originally failed assert_state/assert_text expected value may change. Reference both call IDs and separate successful evidence calls from this run. Original failure stays recorded as superseded, never passed; the harness does not verify your rationale. Preserve the user's requirement; never weaken a test to pass. Suite/shell checks, changed operators/actions, removed checks, stale or unexecuted results are unsupported."
}
func (*verificationCorrectTool) Schema() map[string]any {
	id := func() map[string]any { return map[string]any{"type": "string", "minLength": 1, "maxLength": 256} }
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"original_call_id": id(), "replacement_call_id": id(),
			"reason":            map[string]any{"type": "string", "minLength": 1, "maxLength": 600, "description": "Concise explanation of the test defect and why the corrected expectation preserves the original user requirement. This is your judgment, not a harness-proven conclusion."},
			"evidence_call_ids": map[string]any{"type": "array", "items": id(), "minItems": 1, "maxItems": 8, "uniqueItems": true, "description": "Already executed successful tool calls supporting the correction, distinct from the failed and replacement checks."},
		}, "required": []string{"original_call_id", "replacement_call_id", "reason", "evidence_call_ids"},
	}
}
func (*verificationCorrectTool) Execute(context.Context, map[string]any) tools.Result {
	return tools.Result{LLMContent: "ERROR: verification_correct requires the current runtime execution ledger."}
}

type verificationCallObservation struct {
	Call               provider.ToolCall
	Execution          ToolExecution
	Sequence, Mutation int
}

// Exposure follows observed tool-protocol eligibility, never task semantics.
// An ordinary edit/shell/permission error must not alter the model's tool set.
func (l *codexStyleExecutionLedger) HasCorrectableVerificationFailure() bool {
	if l == nil {
		return false
	}
	for _, record := range l.failures {
		if record != nil && l.correctableVerificationOriginal(record.CallID) {
			return true
		}
	}
	return false
}

func (l *codexStyleExecutionLedger) correctableVerificationOriginal(callID string) bool {
	if l == nil {
		return false
	}
	observed := l.observations[callID]
	if observed == nil {
		return false
	}
	execution := observed.Execution
	record := l.failures[canonicalToolCallFingerprint(observed.Call)]
	if record == nil || record.CallID != callID || record.Recovered || record.Superseded || len(record.VerificationKeys) == 0 ||
		execution.Tool != codexStyleInteractionToolName || !execution.Executed || (execution.Status != ToolError && execution.Status != ToolOK) || codexStyleInteractionNeedsHumanReview(execution) {
		return false
	}
	path, _ := execution.Args["file_path"].(string)
	criterion, _ := execution.Args["requirement"].(string)
	coverage, _ := execution.Args["coverage"].([]any)
	steps, _ := execution.Args["steps"].([]any)
	if path == "" || strings.TrimSpace(criterion) == "" && len(coverage) == 0 || len(steps) == 0 {
		return false
	}
	evidence, err := interactionCorrectionReport(execution)
	if err != nil {
		return false
	}
	failures := 0
	for i, row := range evidence {
		if i >= len(steps) || row["step"] != float64(i+1) {
			return false
		}
		if row["status"] != "failed" {
			continue
		}
		step, ok := steps[i].(map[string]any)
		if !ok {
			return false
		}
		predicate, _, valid := correctionStateStep(step)
		_, actualObserved := row["actual"]
		if !valid || predicate["operator"] != "eq" || row["action"] != "assert_state" || row["operator"] != "eq" || !actualObserved {
			return false
		}
		failures++
	}
	return failures > 0
}

// References are immutable per-call receipts. Reused IDs are poisoned instead
// of silently referring to a later execution with the same ID/arguments.
func (l *codexStyleExecutionLedger) observeCorrectionReference(call provider.ToolCall, execution ToolExecution) {
	l.observationSequence++
	if execution.Executed && !execution.NoChange && !execution.ArtifactRolledBack && (len(execution.ChangedPaths) > 0 || execution.Status == ToolOK && execution.Effect == EffectWrite) {
		l.mutationSequence++
	}
	if call.ID == "" {
		return
	}
	if l.observations == nil {
		l.observations = map[string]*verificationCallObservation{}
	}
	if _, exists := l.observations[call.ID]; exists {
		l.observations[call.ID] = nil
		return
	}
	// Only the arguments need a deep copy: outputs are immutable strings and
	// media/private provider content is deliberately absent from this receipt.
	args, _ := json.Marshal(execution.Args)
	execution.Args = nil
	_ = json.Unmarshal(args, &execution.Args)
	execution.ContentParts = nil
	l.observations[call.ID] = &verificationCallObservation{Call: call, Execution: execution, Sequence: l.observationSequence, Mutation: l.mutationSequence}
}

func (l *codexStyleExecutionLedger) correctVerification(call provider.ToolCall) (string, error) {
	fail := func(message string) (string, error) {
		return "VERIFICATION_CORRECTION_REJECTED: " + message, fmt.Errorf("%s", message)
	}
	var args struct {
		Original    string   `json:"original_call_id"`
		Replacement string   `json:"replacement_call_id"`
		Reason      string   `json:"reason"`
		Evidence    []string `json:"evidence_call_ids"`
	}
	decoder := json.NewDecoder(strings.NewReader(call.Function.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return fail("invalid arguments: " + err.Error())
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fail("arguments must contain exactly one JSON object")
	}
	validID := func(id string) bool { return strings.TrimSpace(id) != "" && utf8.RuneCountInString(id) <= 256 }
	if !validID(args.Original) || !validID(args.Replacement) || !validID(call.ID) || strings.TrimSpace(args.Reason) == "" || utf8.RuneCountInString(args.Reason) > 600 || len(args.Evidence) < 1 || len(args.Evidence) > 8 {
		return fail("provide bounded call IDs, a concise reason (1-600 characters), and 1-8 independent evidence call IDs")
	}
	if l == nil {
		return fail("no current run ledger")
	}
	if _, exists := l.observations[call.ID]; exists {
		return fail("correction call ID must be unique in this run")
	}
	if l.observations == nil {
		l.observations = map[string]*verificationCallObservation{}
	}
	// Control receipts cannot serve as independent observation evidence.
	l.observations[call.ID] = nil
	original, replacement := l.observations[args.Original], l.observations[args.Replacement]
	if original == nil {
		return fail(fmt.Sprintf("original_call_id %q is unknown or ambiguous in this run", args.Original))
	}
	rejectReference := func(message string) (string, error) {
		candidates, _ := json.Marshal(l.correctionReplacementCandidates(original))
		return fail(message + "\neligible_replacement_call_ids: " + string(candidates) +
			"\nThese are recorded passing checks matching this original's structure and current recorded mutation state. Choose the relevant call ID and separate successful evidence IDs; the correction rationale remains your responsibility.")
	}
	if replacement == nil {
		return rejectReference(fmt.Sprintf("replacement_call_id %q is unknown or ambiguous in this run", args.Replacement))
	}
	a, b := original.Execution, replacement.Execution
	record := l.failures[canonicalToolCallFingerprint(original.Call)]
	if !l.correctableVerificationOriginal(args.Original) {
		return fail(fmt.Sprintf("original_call_id %q must be the current unresolved executed browser equality assertion failure with structured observation", args.Original))
	}
	switch a.Status {
	case ToolDenied, ToolBlocked, ToolConfirmationRequired, ToolCancelled:
		return fail("permission and cancellation boundaries cannot be corrected")
	}
	if a.Tool != codexStyleInteractionToolName || b.Tool != a.Tool {
		return rejectReference(fmt.Sprintf("replacement_call_id %q references tool %q; it must reference an executed passing %s call, not a source/evidence read", args.Replacement, codexStyleRecapOneLine(b.Tool, 80), codexStyleInteractionToolName))
	}
	if original.Sequence >= replacement.Sequence {
		return rejectReference(fmt.Sprintf("replacement_call_id %q occurred at observation %d, before or at original_call_id %q at observation %d. The passing replacement must occur after the selected original failure", args.Replacement, replacement.Sequence, args.Original, original.Sequence))
	}
	if replacement.Mutation != l.mutationSequence {
		return rejectReference(fmt.Sprintf("replacement_call_id %q is stale after a subsequently recorded workspace mutation", args.Replacement))
	}
	if !verificationRecoveryPassed(b) || codexStyleInteractionNeedsHumanReview(b) {
		return rejectReference(fmt.Sprintf("replacement_call_id %q is not an actually executed passing check (tool=%q, status=%q, executed=%t)", args.Replacement, codexStyleRecapOneLine(b.Tool, 80), b.Status, b.Executed))
	}
	seen := map[string]bool{}
	for _, id := range args.Evidence {
		observed := l.observations[id]
		if !validID(id) {
			return rejectReference("evidence_call_ids contains an empty or overlong ID; each ID must contain 1-256 characters")
		}
		if seen[id] || id == args.Original || id == args.Replacement {
			return rejectReference(fmt.Sprintf("evidence_call_ids contains duplicate reference %q or reuses original_call_id/replacement_call_id. Use distinct successful evidence calls, such as the source or requirement read IDs in recent_evidence_calls", id))
		}
		if observed == nil || !observed.Execution.Executed || observed.Execution.Status != ToolOK || observed.Execution.ValidationStatus == VerificationFailed || codexStyleInteractionNeedsHumanReview(observed.Execution) {
			return rejectReference(fmt.Sprintf("evidence_call_ids reference %q is unknown, ambiguous, unexecuted or unsuccessful; use an actual successful observation from this run", id))
		}
		seen[id] = true
	}
	if err := validateInteractionCorrection(a, b); err != nil {
		return rejectReference("replacement_call_id does not preserve the correction contract: " + err.Error())
	}
	correction := RunRecapCorrection{OriginalCallID: args.Original, ReplacementCallID: args.Replacement, CorrectionCallID: call.ID, Tool: a.Tool, Reason: strings.TrimSpace(args.Reason), EvidenceCallIDs: append([]string(nil), args.Evidence...), Status: "superseded"}
	record.Superseded = true
	l.corrections = append(l.corrections, correction)
	body, _ := json.Marshal(map[string]any{"status": "superseded", "correction": correction, "scope": "Model-declared test correction; rationale is not independently verified. The original failed observation is preserved, not a passing result."})
	return string(body), nil
}

// A bounded reference index, not a choice or automatic disposition. It uses
// precisely the existing structural/freshness checks and never changes state.
func (l *codexStyleExecutionLedger) correctionReplacementCandidates(original *verificationCallObservation) []string {
	ids := []string{}
	if l == nil || original == nil || !l.correctableVerificationOriginal(original.Call.ID) {
		return ids
	}
	var candidates []*verificationCallObservation
	for _, candidate := range l.observations {
		if candidate == nil || candidate.Sequence <= original.Sequence || candidate.Mutation != l.mutationSequence ||
			candidate.Execution.Tool != original.Execution.Tool || !verificationRecoveryPassed(candidate.Execution) || codexStyleInteractionNeedsHumanReview(candidate.Execution) ||
			strings.TrimSpace(candidate.Call.ID) == "" || utf8.RuneCountInString(candidate.Call.ID) > 256 {
			continue
		}
		if validateInteractionCorrection(original.Execution, candidate.Execution) == nil {
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Sequence > candidates[j].Sequence })
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}
	for _, candidate := range candidates {
		ids = append(ids, candidate.Call.ID)
	}
	return ids
}

func interactionCorrectionReport(execution ToolExecution) ([]map[string]any, error) {
	const marker = "\nbehavior_evidence: "
	i := strings.LastIndex(execution.LLMContent, marker)
	if i < 0 {
		return nil, fmt.Errorf("missing structured browser observation")
	}
	var report struct {
		Steps []map[string]any `json:"steps"`
	}
	if err := json.Unmarshal([]byte(execution.LLMContent[i+len(marker):]), &report); err != nil {
		return nil, fmt.Errorf("invalid structured browser observation")
	}
	return report.Steps, nil
}

func validateInteractionCorrection(original, replacement ToolExecution) error {
	fail := func() error {
		return fmt.Errorf("preserve the exact target, criterion, coverage and ordered steps; change only an originally observed failed state expectation")
	}
	a, b := original.Args, replacement.Args
	path, _ := a["file_path"].(string)
	if path == "" || b["file_path"] != path {
		return fail()
	}
	// Requiring the complete non-step argument map also preserves settle time,
	// target aliases and assertion-to-requirement coverage without inference.
	other := func(args map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range args {
			if k != "steps" {
				out[k] = v
			}
		}
		return out
	}
	if !reflect.DeepEqual(other(a), other(b)) {
		return fail()
	}
	criterion, _ := a["requirement"].(string)
	coverage, _ := a["coverage"].([]any)
	if strings.TrimSpace(criterion) == "" && len(coverage) == 0 {
		return fail()
	}
	as, aok := a["steps"].([]any)
	bs, bok := b["steps"].([]any)
	if !aok || !bok || len(as) == 0 || len(as) != len(bs) {
		return fail()
	}
	oldEvidence, err := interactionCorrectionReport(original)
	if err != nil {
		return err
	}
	newEvidence, err := interactionCorrectionReport(replacement)
	if err != nil {
		return err
	}
	if len(newEvidence) != len(bs) {
		return fmt.Errorf("replacement did not execute every ordered step")
	}
	failed := map[int]bool{}
	for i, row := range oldEvidence {
		if row["step"] != float64(i+1) {
			return fmt.Errorf("ambiguous original observation indexes")
		}
		if row["status"] == "failed" {
			if row["action"] != "assert_state" {
				return fmt.Errorf("only observed state assertion failures can be corrected")
			}
			if _, ok := row["actual"]; !ok {
				return fmt.Errorf("original failed state was not observed")
			}
			failed[i] = true
		}
	}
	if len(failed) == 0 {
		return fmt.Errorf("no observed failed state predicate")
	}
	changed := 0
	for i, raw := range as {
		before, ok := raw.(map[string]any)
		if !ok {
			return fail()
		}
		after, ok := bs[i].(map[string]any)
		if !ok {
			return fail()
		}
		row := newEvidence[i]
		if row["step"] != float64(i+1) || row["status"] != "passed" {
			return fmt.Errorf("replacement contains missing or nonpassing step evidence")
		}
		if reflect.DeepEqual(before, after) {
			continue
		}
		if !failed[i] {
			return fail()
		}
		x, xv, xok := correctionStateStep(before)
		y, yv, yok := correctionStateStep(after)
		if !xok || !yok || !reflect.DeepEqual(x, y) || reflect.TypeOf(xv) != reflect.TypeOf(yv) || reflect.DeepEqual(xv, yv) || row["action"] != "assert_state" {
			return fail()
		}
		if _, ok := row["actual"]; !ok {
			return fail()
		}
		// Disposition is deliberately limited to equality expectation mistakes.
		// Range/inequality threshold changes can weaken a criterion mechanically.
		if x["operator"] != "eq" {
			return fmt.Errorf("only equality expectation corrections are supported; range or operator changes must not weaken validation")
		}
		changed++
	}
	if changed != len(failed) {
		return fail()
	}
	return nil
}

func correctionStateStep(step map[string]any) (map[string]any, any, bool) {
	action, _ := step["action"].(string)
	if action != "assert_state" && action != "assert_text" {
		return nil, nil, false
	}
	out := map[string]any{}
	var expected any
	found := false
	for k, v := range step {
		switch k {
		case "value", "expected_text", "expected_number", "expected_boolean", "expected_null":
			if found {
				return nil, nil, false
			}
			expected = v
			if k == "expected_null" {
				if v != true {
					return nil, nil, false
				}
				expected = nil
			}
			found = true
		default:
			out[k] = v
		}
	}
	if !found {
		return nil, nil, false
	}
	if action == "assert_text" {
		out["action"] = "assert_state"
		out["source"] = "dom"
		out["path"] = "text"
		out["operator"] = "eq"
	}
	return out, expected, true
}

// Interpret declared shell validation's real process contract without adopting
// a task classifier or changing the raw tool result/status.
func codexStyleShellVerification(execution ToolExecution) VerificationObservation {
	if execution.Tool != "run_shell_command" || !execution.Executed || !codexStyleExecutionIsValidation(execution) {
		return VerificationObservation{}
	}
	code, hasCode := executionExitCode(execution.Display)
	failure := validationOutputHasFailureSignal(execution.LLMContent)
	status := VerificationUnverified
	if execution.Status != ToolOK || failure || hasCode && !expectedValidationExitCode(execution.Args, code, hasCode) {
		status = VerificationFailed
	} else if hasCode {
		status = VerificationPassed
	}
	return VerificationObservation{Validation: true, Status: status, ExitCode: code, HasExitCode: hasCode, FailureOutput: failure}
}
