package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// These keys preserve model-authored obligations, not inferred task semantics.
// New diagnostics permit retries; only matching fresh checks discharge them.
func verificationRecoveryKeys(call provider.ToolCall, execution ToolExecution, passedOnly bool) []string {
	args := execution.Args
	if args == nil {
		_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
	}
	if execution.Tool == "meadow_browser_inspect" || (execution.Tool == "meadow_browser_suite" && args["operation"] == "inspect") {
		return nil
	}
	path, _ := args["file_path"].(string)
	prefix := execution.Tool + "\x00" + path + "\x00"
	fallback := "call\x00" + verificationRecoveryCallFingerprint(call)
	var keys []string
	if execution.Tool == "meadow_browser_interaction_check" && path != "" {
		if criterion, ok := args["requirement"].(string); ok {
			if criterion = strings.Join(strings.Fields(criterion), " "); criterion != "" {
				keys = append(keys, prefix+criterion)
			}
		}
		rows, _ := args["coverage"].([]any)
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			criterion, _ := row["requirement"].(string)
			if criterion = strings.Join(strings.Fields(criterion), " "); criterion != "" {
				keys = append(keys, prefix+criterion)
			}
		}
	} else if execution.Tool == "meadow_browser_suite" {
		path, _ = args["suite_path"].(string)
		if path != "" {
			fallback = "suite-input\x00" + path
		}
		var report struct {
			Freshness    string `json:"freshness"`
			Requirements []struct {
				ID     string `json:"id"`
				Text   string `json:"text"`
				Status string `json:"status"`
			} `json:"requirements"`
		}
		if path != "" && json.Unmarshal([]byte(execution.LLMContent), &report) == nil {
			for _, row := range report.Requirements {
				if row.ID != "" && (!passedOnly || report.Freshness == "current" && row.Status == "passed") {
					keys = append(keys, execution.Tool+"\x00"+path+"\x00"+row.ID+"\x00"+row.Text)
				}
			}
		}
	}
	if len(keys) == 0 && !passedOnly {
		keys = []string{fallback}
	}
	if passedOnly {
		// A schema-invalid saved suite has no parseable requirement IDs. A
		// later successful run of the exact invocation can clear that failure.
		keys = append(keys, fallback)
	}
	keys = append(keys, verificationStatePredicateKeys(execution, path, passedOnly)...)
	return keys
}

// Only this documented Shell default is equivalent for recovery. Keep raw
// call/check/duplicate identities intact, and preserve every other argument.
// Do not use execution-normalized arguments: they can rewrite command/script.
func verificationRecoveryCallFingerprint(call provider.ToolCall) string {
	if call.Function.Name != "run_shell_command" {
		return canonicalToolCallFingerprint(call)
	}
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
		return canonicalToolCallFingerprint(call)
	}
	var codes []json.RawMessage
	if json.Unmarshal(args["expected_exit_codes"], &codes) == nil && len(codes) == 1 {
		// Exact integer token only: float64 could round a tiny nonzero value
		// to zero and incorrectly widen the documented default equivalence.
		if strings.TrimSpace(string(codes[0])) == "0" {
			delete(args, "expected_exit_codes")
			encoded, err := json.Marshal(args)
			if err == nil {
				call.Function.Arguments = string(encoded)
			}
		}
	}
	return canonicalToolCallFingerprint(call)
}

// Only genuinely observed state assertions acquire an exact predicate identity.
// Format/selector/observation failures remain repairable. This does not infer
// task semantics or prove that a different predicate logically implies one.
func verificationStatePredicateKeys(execution ToolExecution, path string, passedOnly bool) []string {
	if !execution.Executed || path == "" {
		return nil
	}
	var steps []map[string]any
	switch execution.Tool {
	case "meadow_browser_interaction_check":
		const marker = "\nbehavior_evidence: "
		index := strings.LastIndex(execution.LLMContent, marker)
		if index < 0 {
			return nil
		}
		var report struct {
			Steps    []map[string]any `json:"steps"`
			Coverage []struct {
				Requirement string `json:"requirement"`
				Assertions  []int  `json:"assertion_steps"`
			} `json:"coverage"`
		}
		if json.Unmarshal([]byte(execution.LLMContent[index+len(marker):]), &report) != nil {
			return nil
		}
		for _, step := range report.Steps {
			if _, observed := step["actual"]; observed {
				number, _ := step["step"].(float64)
				var criteria []string
				for _, coverage := range report.Coverage {
					for _, index := range coverage.Assertions {
						if float64(index) == number {
							criteria = append(criteria, strings.Join(strings.Fields(coverage.Requirement), " "))
							break
						}
					}
				}
				sort.Strings(criteria)
				step["requirements"] = criteria
				steps = append(steps, step)
			}
		}
	case "meadow_browser_suite":
		var report struct {
			Freshness string           `json:"freshness"`
			Checks    []map[string]any `json:"assertion_checks"`
		}
		if json.Unmarshal([]byte(execution.LLMContent), &report) != nil || (passedOnly && report.Freshness != "current") {
			return nil
		}
		steps = report.Checks
	default:
		return nil
	}
	status := "failed"
	if passedOnly {
		status = "passed"
	}
	var keys []string
	for _, step := range steps {
		if step["action"] != "assert_state" || step["status"] != status || step["source"] == nil || step["path"] == nil || step["operator"] == nil {
			continue
		}
		predicate := map[string]any{}
		for _, key := range []string{"action", "source", "selector", "path", "operator", "expected", "scenario_id", "requirements"} {
			if value, ok := step[key]; ok {
				predicate[key] = value
			}
		}
		encoded, err := json.Marshal(predicate)
		if err == nil {
			keys = append(keys, fmt.Sprintf("state-predicate\x00%s\x00%s\x00%x", execution.Tool, path, sha256.Sum256(encoded)))
		}
	}
	return keys
}

func verificationRecoveryPassed(execution ToolExecution) bool {
	if execution.Tool == "meadow_browser_inspect" || (execution.Tool == "meadow_browser_suite" && execution.Args["operation"] == "inspect") {
		return false
	}
	if !execution.Executed || execution.Status != ToolOK || execution.ValidationStatus == VerificationFailed || execution.ValidationStatus == VerificationUnverified || execution.ValidationStatus == VerificationStale {
		return false
	}
	if execution.Tool != "meadow_browser_suite" {
		return true
	}
	var report struct {
		Freshness    string `json:"freshness"`
		Requirements []struct {
			Status string `json:"status"`
		} `json:"requirements"`
	}
	if json.Unmarshal([]byte(execution.LLMContent), &report) != nil || report.Freshness != "current" || len(report.Requirements) == 0 {
		return false
	}
	for _, row := range report.Requirements {
		if row.Status != "passed" {
			return false
		}
	}
	return true
}

func (l *codexStyleExecutionLedger) resolveVerified(keys []string) {
	passed := make(map[string]bool, len(keys))
	for _, key := range keys {
		passed[key] = true
	}
	for _, record := range l.failures {
		if record.Recovered || record.Superseded || len(record.VerificationKeys) == 0 {
			continue
		}
		pending := record.VerificationKeys[:0]
		for _, key := range record.VerificationKeys {
			if !passed[key] {
				pending = append(pending, key)
			}
		}
		record.VerificationKeys = pending
		if len(pending) == 0 {
			record.Recovered = true
			l.recovered++
		}
	}
}
