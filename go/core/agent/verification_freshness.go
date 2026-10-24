package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// CheckEvidence projects actual tool observations, not a task verdict. Current
// means only that every declared file still has the observed bytes. Test quality,
// undeclared dependencies, remote state and live document state are not proved.
type CheckEvidence struct {
	Freshness          string                   `json:"freshness"`
	Reason             string                   `json:"reason,omitempty"`
	Scope              string                   `json:"scope"`
	Inputs             []tools.FileObservation  `json:"inputs,omitempty"`
	DeclaredInputCount int                      `json:"declaredInputCount,omitempty"`
	Assertions         []CheckAssertionEvidence `json:"assertions,omitempty"`
	Criteria           []string                 `json:"criteria,omitempty"`
	ReportPath         string                   `json:"reportPath,omitempty"`
	ReusedReport       bool                     `json:"reusedReport,omitempty"`
	Document           *CheckDocumentEvidence   `json:"document,omitempty"`
}

type CheckAssertionEvidence struct {
	ScenarioID    string          `json:"scenarioId,omitempty"`
	Step          int             `json:"step,omitempty"`
	Action        string          `json:"action,omitempty"`
	Source        string          `json:"source,omitempty"`
	Path          string          `json:"path,omitempty"`
	Selector      string          `json:"selector,omitempty"`
	Operator      string          `json:"operator,omitempty"`
	Status        string          `json:"status,omitempty"`
	Expected      json.RawMessage `json:"expected,omitempty"`
	Actual        json.RawMessage `json:"actual,omitempty"`
	ValuesOmitted bool            `json:"valuesOmitted,omitempty"`
}

type CheckDocumentEvidence struct {
	PageID        string `json:"pageId,omitempty"`
	DocumentID    string `json:"documentId,omitempty"`
	URL           string `json:"url,omitempty"`
	Origin        string `json:"origin,omitempty"`
	Backend       string `json:"backend,omitempty"`
	FieldsOmitted bool   `json:"fieldsOmitted,omitempty"`
	// Pins identify the observed document, never attest its present live state.
	Freshness string `json:"freshness"`
}

// SetVerificationWorkspace enables revision-bound projection for this run.
// A nil workspace leaves freshness unknown rather than inventing file evidence.
func (l *codexStyleExecutionLedger) SetVerificationWorkspace(ws *tools.Workspace) {
	if l == nil {
		return
	}
	l.verificationWorkspace = ws
	l.revisionBoundEvidence = true
	l.invalidatedChecks = make(map[string]bool)
}

func (l *codexStyleExecutionLedger) SetVerificationContext(ctx context.Context) {
	if l != nil {
		l.verificationContext = ctx
	}
}

// Validation is also available in a read-only or Browser-only registry. Reuse
// those registered capabilities' actual workspace rather than requiring a
// writer solely to recheck already declared source hashes.
func codexStyleVerificationWorkspace(executor *ToolExecutor) *tools.Workspace {
	if executor == nil || executor.Registry == nil {
		return nil
	}
	for _, name := range []string{"write_file", "read_file", "run_shell_command", "meadow_browser_interaction_check", "meadow_browser_suite"} {
		tool, exists := executor.Registry.Get(name)
		if !exists {
			continue
		}
		var ws *tools.Workspace
		switch t := tool.(type) {
		case *tools.WriteFile:
			ws = t.WS
		case *tools.ReadFile:
			ws = t.WS
		case *tools.Shell:
			ws = t.WS
		case *tools.MeadowBrowserInteractionCheck:
			ws = t.WS
		case *tools.MeadowBrowserSuite:
			if t.Checker != nil {
				ws = t.Checker.WS
			}
		}
		if ws != nil {
			return ws
		}
	}
	return nil
}

func captureCheckEvidence(execution ToolExecution) *CheckEvidence {
	e := &CheckEvidence{Freshness: "unknown", Scope: "Only executed supplied checks; model-authored criteria are not proof of complete requirements. File freshness covers declared bytes only, not undeclared dependencies or test correctness."}
	if criterion, ok := execution.Args["requirement"].(string); ok && criterion != "" {
		e.Criteria = append(e.Criteria, codexStyleRecapOneLine(criterion, 300))
	}
	if rows, ok := execution.Args["coverage"].([]any); ok {
		for _, value := range rows {
			row, _ := value.(map[string]any)
			if criterion, ok := row["requirement"].(string); ok && len(e.Criteria) < 64 {
				e.Criteria = append(e.Criteria, codexStyleRecapOneLine(criterion, 300))
			}
		}
	}
	if !execution.Executed {
		e.Reason = "No fresh executed validator receipt."
		return e
	}
	inputs := make(map[string]string)
	for _, observation := range execution.FileObservations {
		if (execution.Tool == "run_shell_command" || execution.Args["purpose"] == "validation") && !execution.ProcessExited {
			continue
		}
		if validEvidenceDigest(observation.SHA256) && observation.Path != "" {
			inputs[observation.Path] = observation.SHA256
		}
	}
	if execution.Tool == "run_shell_command" || execution.ProcessExited && execution.Args["purpose"] == "validation" {
		e.Scope += " Shell observations describe unchanged declared targets at before/after normal-exit boundaries; they do not prove that the command read or correctly tested them."
		switch paths := execution.Args["validation_paths"].(type) {
		case []string:
			e.DeclaredInputCount = len(paths)
		case []any:
			e.DeclaredInputCount = len(paths)
		}
	}
	var report map[string]any
	switch execution.Tool {
	case "meadow_browser_suite":
		e.ReusedReport = execution.Args["operation"] == "status"
		e.Document = &CheckDocumentEvidence{Freshness: "unknown"}
		_ = json.Unmarshal([]byte(execution.LLMContent), &report)
		e.ReportPath, _ = report["report_path"].(string)
		if requirements, ok := report["requirements"].([]any); ok {
			for _, value := range requirements {
				row, _ := value.(map[string]any)
				if text, ok := row["text"].(string); ok && len(e.Criteria) < 64 {
					e.Criteria = append(e.Criteria, codexStyleRecapOneLine(text, 300))
				}
			}
		}
	case "meadow_browser_interaction_check":
		e.Document = &CheckDocumentEvidence{Freshness: "unknown"}
		report = execution.BehaviorEvidence
		if report == nil {
			const marker = "\nbehavior_evidence: "
			if index := strings.LastIndex(execution.LLMContent, marker); index >= 0 {
				_ = json.Unmarshal([]byte(execution.LLMContent[index+len(marker):]), &report)
			}
		}
	}
	if report != nil {
		if declared, ok := report["inputs"].(map[string]any); ok {
			e.DeclaredInputCount = len(declared)
			for path, value := range declared {
				if digest, ok := value.(string); ok && validEvidenceDigest(digest) && path != "" {
					inputs[path] = digest
				}
			}
		} else if declared, ok := report["inputs"].(map[string]string); ok {
			e.DeclaredInputCount = len(declared)
			for path, digest := range declared {
				if validEvidenceDigest(digest) && path != "" {
					inputs[path] = digest
				}
			}
		}
		switch count := report["declared_input_count"].(type) {
		case int:
			if count > e.DeclaredInputCount {
				e.DeclaredInputCount = count
			}
		case float64:
			if count > float64(e.DeclaredInputCount) && count <= 66 {
				e.DeclaredInputCount = int(count)
			}
		}
		e.Assertions = captureAssertionEvidence(report)
		e.Document = captureDocumentEvidence(report)
		if execution.Tool == "meadow_browser_suite" && report["freshness"] == "stale" || report["inputs_freshness"] == "stale" {
			e.Freshness, e.Reason = "stale", "Tool observed declared inputs change during the check."
		}
	}
	for path, digest := range inputs {
		e.Inputs = append(e.Inputs, tools.FileObservation{Path: path, SHA256: digest})
	}
	sort.Slice(e.Inputs, func(i, j int) bool { return e.Inputs[i].Path < e.Inputs[j].Path })
	if e.DeclaredInputCount == 0 && len(e.Inputs) > 0 {
		e.DeclaredInputCount = len(e.Inputs)
	}
	if len(e.Inputs) > 66 {
		e.Inputs = e.Inputs[:66]
		e.Freshness, e.Reason = "unknown", "Input receipt exceeded bounded provenance capacity."
	}
	if e.Freshness != "stale" {
		switch {
		case len(e.Inputs) == 0:
			e.Reason = "No declared stable input hashes; current source/document state is unverified."
		case len(e.Inputs) != e.DeclaredInputCount:
			e.Reason = "Only a subset of declared inputs has stable hash observations."
		case report != nil && report["inputs_freshness"] == "unknown":
			e.Reason = "Tool could not establish declared input stability."
		default:
			e.Freshness, e.Reason = "current", "Declared bytes matched the tool observation boundaries; live state and undeclared inputs remain untracked."
		}
	}
	return e
}

func validEvidenceDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == 32
}

func captureAssertionEvidence(report map[string]any) []CheckAssertionEvidence {
	var rows []map[string]any
	for _, field := range []string{"assertion_checks", "steps"} {
		switch values := report[field].(type) {
		case []map[string]any:
			rows = values
		case []any:
			for _, value := range values {
				if row, ok := value.(map[string]any); ok {
					rows = append(rows, row)
				}
			}
		}
		if len(rows) > 0 {
			break
		}
	}
	var out []CheckAssertionEvidence
	for _, row := range rows {
		action, _ := row["action"].(string)
		if !strings.HasPrefix(action, "assert_") || len(out) >= 20 {
			continue
		}
		str := func(key string) string { value, _ := row[key].(string); return codexStyleRecapOneLine(value, 160) }
		a := CheckAssertionEvidence{ScenarioID: str("scenario_id"), Action: action, Source: str("source"), Path: str("path"), Selector: str("selector"), Operator: str("operator"), Status: str("status")}
		if step, ok := row["step"].(float64); ok {
			a.Step = int(step)
		}
		if step, ok := row["step"].(int); ok {
			a.Step = step
		}
		for _, key := range []string{"expected", "actual"} {
			if value, present := row[key]; present {
				encoded, err := json.Marshal(value)
				if err != nil || len(encoded) > 1024 {
					a.ValuesOmitted = true
					continue
				}
				if key == "expected" {
					a.Expected = encoded
				} else {
					a.Actual = encoded
				}
			}
		}
		if row["actual_omitted"] != nil || row["actual_truncated"] != nil {
			a.ValuesOmitted = true
		}
		out = append(out, a)
	}
	return out
}

func captureDocumentEvidence(report map[string]any) *CheckDocumentEvidence {
	e := &CheckDocumentEvidence{Freshness: "unknown"}
	str := func(key string) string {
		value, _ := report[key].(string)
		if !utf8.ValidString(value) || len(value) > 4096 {
			e.FieldsOmitted = true
			return ""
		}
		return value
	}
	e.PageID, e.DocumentID, e.URL, e.Origin, e.Backend = str("page_id"), str("document_id"), str("page_url"), str("page_origin"), str("backend")
	return e
}

func (l *codexStyleExecutionLedger) refreshVerificationEvidence() {
	if l == nil || !l.revisionBoundEvidence {
		return
	}
	// Several criteria may share one artifact or fixture. Hash each path once
	// per projection rather than repeating file IO for every passing criterion.
	type snapshotResult struct {
		observation tools.FileObservation
		err         error
	}
	snapshots := make(map[string]snapshotResult)
	for _, key := range l.checkOrder {
		check := l.checks[key]
		e := check.Evidence
		if e == nil {
			continue
		}
		wasCurrent := e.Freshness == "current"
		cancelledRecheck := false
		if !l.invalidatedChecks[key] && e.Freshness == "current" {
			if l.verificationWorkspace == nil && l.verificationObserver == nil {
				e.Freshness, e.Reason = "unknown", "Workspace unavailable for current input recheck."
			} else {
				for _, previous := range e.Inputs {
					if l.verificationContext != nil && l.verificationContext.Err() != nil {
						e.Freshness, e.Reason = "unknown", "Current input recheck omitted after cancellation or deadline."
						cancelledRecheck = true
						break
					}
					result, exists := snapshots[previous.Path]
					if !exists {
						if l.verificationObserver != nil {
							state, err := l.verificationObserver.ObserveWorkspaceFileState(previous.Path)
							result.err = err
							if err == nil && !state.Exists {
								result.err = fmt.Errorf("declared input is absent")
							}
							result.observation = tools.FileObservation{Path: state.Path, SHA256: state.SHA256}
						} else {
							result.observation, result.err = l.verificationWorkspace.SnapshotValidationFile(previous.Path)
						}
						snapshots[previous.Path] = result
					}
					if result.err != nil {
						e.Freshness, e.Reason = "unknown", "Declared input unavailable for recheck: "+codexStyleRecapOneLine(previous.Path, 180)
						break
					}
					if result.observation != previous {
						e.Freshness, e.Reason = "stale", "Declared input changed: "+codexStyleRecapOneLine(previous.Path, 180)
						break
					}
				}
			}
		}
		if check.Status == "superseded" {
			continue
		}
		if e.Freshness == "stale" || e.Freshness == "unknown" && e.DeclaredInputCount > 0 {
			if !l.invalidatedChecks[key] && !cancelledRecheck && (wasCurrent || e.Freshness == "stale") {
				l.evidenceInvalidations++
			}
			l.invalidatedChecks[key] = true
			if check.Status == "passed" {
				check.ObservedStatus = "passed"
			}
			if check.ObservedStatus == "passed" {
				if e.Freshness == "stale" {
					check.Status = "stale"
				} else {
					check.Status = "unverified"
				}
			}
		}
		l.checks[key] = check
	}
}

func (l *codexStyleExecutionLedger) verificationCheckCanRecover(key string) bool {
	if !l.revisionBoundEvidence {
		return true
	}
	l.refreshVerificationEvidence()
	e := l.checks[key].Evidence
	// Missing undeclared provenance remains visibly unknown, never current. A
	// declared-but-incomplete receipt cannot discharge a previous failure.
	return e == nil || e.DeclaredInputCount == 0 && e.Freshness == "unknown" || e.Freshness == "current"
}

func (l *codexStyleExecutionLedger) hasInvalidatedPassingCheck() bool {
	for _, check := range l.checks {
		if check.ObservedStatus == "passed" && check.Status != "passed" && check.Status != "superseded" {
			return true
		}
	}
	return false
}

func (l *codexStyleExecutionLedger) staleVerificationFeedbackRows() []verificationFeedbackRow {
	var rows []verificationFeedbackRow
	for _, key := range l.checkOrder {
		check := l.checks[key]
		if check.ObservedStatus != "passed" || check.Status == "passed" || check.Status == "superseded" {
			continue
		}
		rows = append(rows, l.verificationFeedbackRow(key, check.Tool, fmt.Sprintf("Earlier pass has %s input evidence: %s", check.Evidence.Freshness, check.Evidence.Reason)))
	}
	return rows
}

// Replacing an invalidated pass requires an actual current execution with all
// earlier declared input paths and exact assertion/recovery identities. Merely
// weakening an expected value or omitting a dependency cannot supersede it.
func (l *codexStyleExecutionLedger) resolveFreshPassingChecks(call provider.ToolCall, execution ToolExecution) {
	if !l.revisionBoundEvidence {
		return
	}
	newKey := canonicalToolCallFingerprint(call)
	newEvidence := l.checks[newKey].Evidence
	if newEvidence == nil || newEvidence.Freshness != "current" {
		return
	}
	keys := make(map[string]bool)
	for _, key := range verificationRecoveryKeys(call, execution, true) {
		keys[key] = true
	}
	paths := make(map[string]bool)
	for _, input := range newEvidence.Inputs {
		paths[input.Path] = true
	}
	for _, key := range l.checkOrder {
		old := l.checks[key]
		if key == newKey || old.ObservedStatus != "passed" || old.Status == "passed" || old.Status == "superseded" || old.Evidence == nil || len(old.Evidence.Inputs) == 0 {
			continue
		}
		original := l.observations[old.CallID]
		if original == nil {
			continue
		}
		matched := true
		for _, input := range old.Evidence.Inputs {
			if !paths[input.Path] {
				matched = false
				break
			}
		}
		oldKeys := verificationRecoveryKeys(original.Call, original.Execution, true)
		if len(oldKeys) == 0 {
			continue
		}
		for _, identity := range oldKeys {
			if !keys[identity] {
				matched = false
				break
			}
		}
		if matched {
			old.Status, old.ReplacedByCallID = "superseded", call.ID
			l.checks[key] = old
		}
	}
}
