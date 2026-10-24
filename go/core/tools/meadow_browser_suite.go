package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// MeadowBrowserSuite executes model-authored regression scenarios. It neither
// creates requirements nor chooses repairs or the model's next action.
type MeadowBrowserSuite struct {
	Checker *MeadowBrowserInteractionCheck
}

func (t *MeadowBrowserSuite) Name() string                     { return "meadow_browser_suite" }
func (t *MeadowBrowserSuite) Kind() Kind                       { return KindExecute }
func (t *MeadowBrowserSuite) ParallelSafe(map[string]any) bool { return false }
func (t *MeadowBrowserSuite) Description() string {
	return `Run or inspect a saved local-web regression suite. Author the JSON with write_file: {"version":1,"file_path":"app.html","dependencies":["app.js"],"requirements":[{"id":"r1","text":"initial value is 0","origin":"explicit"}],"scenarios":[{"id":"initial","requirement_ids":["r1"],"steps":[{"action":"assert_text","selector":"#count","value":"0"}]}]}. Steps use meadow_browser_interaction_check actions; no coverage indices required. Paths are relative to suite directory or explicit workspace/session paths. List every local dependency; undeclared files and remote state are NOT freshness-checked. Each scenario reloads the page but does not clear persistent browser storage. Keep requirements across repairs; changed suite bytes invalidate old evidence. Results persist beside suite in .meadow-suite-evidence; status reads latest run and rechecks declared file hashes. Passing supplied tests is not proof of complete requirements or overall task success.`
}
func (t *MeadowBrowserSuite) Schema() map[string]any {
	return obj(map[string]any{
		"suite_path":   str("Saved suite JSON path. Max 256 KiB; 64 requirements, 32 scenarios, 20 steps per scenario, 64 dependency files."),
		"scenario_id":  str("inspect-only: include this scenario's exact current steps. Omit for metadata and scenario-ID overview."),
		"operation":    map[string]any{"type": "string", "enum": []string{"run", "status", "inspect"}, "description": "inspect reads suite, source hash, step schema and preflight errors without executing tests; use meadow_browser_suite_edit for scenario repairs."},
		"scenario_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 32, "description": "Optional run subset for diagnosis. Other scenarios remain untested; results never silently merge across runs."},
	}, "suite_path", "operation")
}

type behaviorRequirement struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Origin string `json:"origin"`
}
type behaviorScenario struct {
	ID             string           `json:"id"`
	RequirementIDs []string         `json:"requirement_ids"`
	Steps          []map[string]any `json:"steps"`
}
type behaviorSuite struct {
	Version      int                   `json:"version"`
	FilePath     string                `json:"file_path"`
	Dependencies []string              `json:"dependencies,omitempty"`
	Requirements []behaviorRequirement `json:"requirements"`
	Scenarios    []behaviorScenario    `json:"scenarios"`
	SettleMS     *int                  `json:"settle_ms,omitempty"`
}
type behaviorScenarioResult struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	Diagnostic string         `json:"diagnostic,omitempty"`
	Evidence   map[string]any `json:"evidence,omitempty"`
}
type behaviorRequirementResult struct {
	behaviorRequirement
	Status string `json:"status"`
}
type behaviorSuiteReport struct {
	Version         int                         `json:"version"`
	Scope           string                      `json:"scope"`
	CreatedAt       string                      `json:"created_at"`
	Freshness       string                      `json:"freshness"`
	FreshnessError  string                      `json:"freshness_error,omitempty"`
	Inputs          map[string]string           `json:"inputs"`
	Requirements    []behaviorRequirementResult `json:"requirements"`
	Scenarios       []behaviorScenarioResult    `json:"scenarios"`
	ReportPath      string                      `json:"report_path,omitempty"`
	AssertionChecks []map[string]any            `json:"assertion_checks,omitempty"`
}

func (t *MeadowBrowserSuite) Execute(ctx context.Context, args map[string]any) Result {
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	if t.Checker == nil || t.Checker.WS == nil {
		return errResultf("suite workspace unavailable")
	}
	op := strArg(args, "operation")
	if _, exists := args["scenario_id"]; exists && op != "inspect" {
		return errResultf("scenario_id is inspect-only; run subsets use scenario_ids")
	}
	if op != "run" && op != "status" && op != "inspect" {
		return errResultf("operation must be run or status")
	}
	ws := t.Checker.WS
	path, _, err := ws.validationFilePath(strArg(args, "suite_path"))
	if err != nil {
		return errResult(err)
	}
	raw, err := readBehaviorFile(path, 256*1024)
	if err != nil {
		return errResult(err)
	}
	var suite behaviorSuite
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&suite); err != nil {
		return errResultf("SUITE_INVALID: %v", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errResultf("SUITE_INVALID: trailing JSON")
	}
	if op == "inspect" {
		if _, exists := args["scenario_ids"]; exists {
			return errResultf("scenario_ids is run-only")
		}
		ws.NoteRead(path)
		validation := ""
		if err := validateBehaviorSuite(suite); err != nil {
			validation = err.Error()
		}
		// Keep current metadata/error ahead of syntax reference, and avoid burying
		// the actual faulty values behind a long repeated schema in model context.
		view := suite
		view.Scenarios = append([]behaviorScenario(nil), suite.Scenarios...)
		selected := strArg(args, "scenario_id")
		found := selected == ""
		for i := range view.Scenarios {
			if view.Scenarios[i].ID == selected {
				found = true
			} else {
				view.Scenarios[i].Steps = nil
			}
		}
		if !found {
			return errResultf("unknown scenario_id for inspection")
		}
		payload := struct {
			Error     string        `json:"preflight_error"`
			Hash      string        `json:"sha256"`
			Suite     behaviorSuite `json:"suite"`
			Scope     string        `json:"scope"`
			Reference any           `json:"step_reference"`
			Repair    string        `json:"repair"`
		}{validation, fmt.Sprintf("%x", sha256.Sum256(raw)), view, "inspection only; no tests executed or passed; omitted steps need inspect with scenario_id", map[string]any{"actions": []string{"click(selector)", "press_key(key)", "fill(selector,text)", "observe_state(source,path,selector for dom)", "assert_text(selector,value string)", "assert_state(source,path,operator,value; selector for dom)", "assert_dom(contains string array)"}, "operators": []string{"eq", "ne", "gt", "gte", "lt", "lte", "changed", "unchanged", "rectangular", "unique", "all_eq", "all_gte", "all_lte"}, "types": "use real JSON numbers/booleans, not strings; changed/unchanged need earlier same-source/path observe_state; unique omits value; rectangular expects integer width; app requires actual meadowTest.getState; settle_ms uses real time"}, "For scenario steps use meadow_browser_suite_edit with current sha256 and scenario_id. For requirement metadata use exact edit of the named field; scenario editing cannot change metadata. Preserve IDs, text and criteria."}
		data, _ := json.Marshal(payload)
		return Result{LLMContent: string(data), Display: "已讀取驗收案例（尚未執行）"}
	}
	if err := validateBehaviorSuiteMetadata(suite); err != nil {
		return errResultf("SUITE_INVALID: %v. Inspect the suite for its current schema/hash. Scenario step errors can use meadow_browser_suite_edit; requirement metadata errors need an exact edit of the named field (scenario editing cannot change metadata). Preserve requirement text and acceptance criteria.", err)
	}
	// The evidence directory is separate from served mutable files, never a caller
	// supplied report destination. Preserve old runs instead of overwriting them.
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(path)))
	dir := filepath.Join(filepath.Dir(path), ".meadow-suite-evidence", key[:16])
	latest := filepath.Join(dir, "latest.json")
	if op == "status" {
		if _, err := resolveBehaviorPath(ws, path, latest); err != nil {
			return errResult(err)
		}
		if _, exists := args["scenario_ids"]; exists {
			return errResultf("scenario_ids is run-only")
		}
		data, err := readBehaviorFile(latest, 4*1024*1024)
		if os.IsNotExist(err) {
			return Result{LLMContent: "No suite run recorded. All requirements are untested.", Display: "suite untested"}
		}
		if err != nil {
			return errResult(err)
		}
		var report behaviorSuiteReport
		if json.Unmarshal(data, &report) != nil || report.Version != 1 {
			return errResultf("invalid saved suite evidence")
		}
		inputs, hashErr := behaviorInputs(ws, path, raw, suite)
		if hashErr != nil || !reflect.DeepEqual(inputs, report.Inputs) {
			report.Freshness = "stale"
			if hashErr != nil {
				report.FreshnessError = hashErr.Error()
			}
			for i := range report.Requirements {
				report.Requirements[i].Status = "stale"
			}
			// Retain historical requirements, including ones removed by a later edit.
			return behaviorReportResult(report, false)
		}
		return behaviorReportResult(report, false)
	}
	selected := map[string]bool{}
	if value, exists := args["scenario_ids"]; exists {
		items, ok := value.([]any)
		if !ok || len(items) == 0 || len(items) > 32 {
			return errResultf("scenario_ids must be a nonempty string array")
		}
		for _, item := range items {
			id, ok := item.(string)
			if !ok || selected[id] {
				return errResultf("scenario_ids must be unique strings")
			}
			selected[id] = true
		}
		for id := range selected {
			found := false
			for _, s := range suite.Scenarios {
				found = found || s.ID == id
			}
			if !found {
				return errResultf("unknown scenario %q", id)
			}
		}
	}
	inputs, err := behaviorInputs(ws, path, raw, suite)
	if err != nil {
		return errResult(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := ws.CheckWritable(latest); err != nil {
		return errResult(err)
	}
	if _, aliasesInput := inputs[ws.ModelPath(latest)]; aliasesInput {
		return errResultf("evidence output cannot be a suite input")
	}
	artifact, err := resolveBehaviorPath(ws, path, suite.FilePath)
	if err != nil {
		return errResult(err)
	}
	dependencyPaths := make([]any, 0, len(suite.Dependencies))
	seenDependencyPaths := map[string]bool{artifact: true}
	for _, dependency := range suite.Dependencies {
		dependencyPath, err := resolveBehaviorPath(ws, path, dependency)
		if err != nil {
			return errResult(err)
		}
		if !seenDependencyPaths[dependencyPath] {
			dependencyPaths = append(dependencyPaths, dependencyPath)
			seenDependencyPaths[dependencyPath] = true
		}
	}
	report := behaviorSuiteReport{Version: 1, Scope: "supplied model-authored checks only; freshness covers declared local files, not undeclared dependencies, remote state or semantic completeness; navigation resets the page, not persistent storage; local records are not tamper-proof", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Freshness: "current", Inputs: inputs}
	for _, s := range suite.Scenarios {
		r := behaviorScenarioResult{ID: s.ID, Status: "untested"}
		if (len(selected) == 0 || selected[s.ID]) && ctx.Err() == nil {
			if err := validateBehaviorScenario(suite, s); err != nil {
				r.Status = "invalid"
				r.Diagnostic = "測試未執行；這是案例格式錯誤，不是產品行為失敗：" + err.Error()
				report.Scenarios = append(report.Scenarios, r)
				continue
			}
			call := behaviorScenarioArgs(artifact, s, suite.SettleMS)
			if len(dependencyPaths) > 0 {
				call["dependencies"] = dependencyPaths
			}
			result := t.Checker.Execute(ctx, call)
			r.Evidence = result.BehaviorEvidence
			r.Status = behaviorExecutionStatus(result, len(s.Steps))
			if r.Status == "passed" && !behaviorScenarioSourceReceiptMatches(result, inputs, 1+len(dependencyPaths)) {
				r.Status = "unavailable"
			}
			if r.Status != "passed" {
				r.Diagnostic = result.LLMContent
				if r.Status == "unavailable" && result.BehaviorEvidence["inputs_freshness"] != "current" {
					r.Diagnostic = "Scenario source-byte receipt is unavailable or stale; no current source-bound acceptance is established. " + r.Diagnostic
				}
				if len(r.Diagnostic) > 8192 {
					r.Diagnostic = r.Diagnostic[:8192] + " [truncated]"
				}
			}
		}
		report.Scenarios = append(report.Scenarios, r)
	}
	for _, requirement := range suite.Requirements {
		status := "untested"
		matched := 0
		allPassed := true
		for i, s := range suite.Scenarios {
			for _, id := range s.RequirementIDs {
				if id != requirement.ID {
					continue
				}
				matched++
				state := report.Scenarios[i].Status
				if state != "passed" {
					allPassed = false
				}
				if state == "failed" {
					status = "failed"
				} else if state == "invalid" && status != "failed" {
					status = "invalid"
				} else if state == "unavailable" && status != "failed" && status != "invalid" {
					status = "unavailable"
				}
			}
		}
		if matched > 0 && allPassed {
			status = "passed"
		}
		report.Requirements = append(report.Requirements, behaviorRequirementResult{requirement, status})
	}
	// Re-read suite as well as dependencies to detect edits during execution.
	afterRaw, afterErr := readBehaviorFile(path, 256*1024)
	after, hashErr := behaviorInputs(ws, path, afterRaw, suite)
	if afterErr != nil || hashErr != nil || !reflect.DeepEqual(inputs, after) {
		report.Freshness = "stale"
		report.FreshnessError = "suite or declared input changed/unavailable during execution"
		for i := range report.Requirements {
			report.Requirements[i].Status = "stale"
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errResult(err)
	}
	// Revalidate derived paths after directory creation, including symlinks.
	if err := ws.CheckWritable(latest); err != nil {
		return errResult(err)
	}
	file, err := os.CreateTemp(dir, "run-*.json")
	if err != nil {
		return errResult(err)
	}
	report.ReportPath = ws.ModelPath(file.Name())
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		_, err = file.Write(encoded)
	}
	closeErr := file.Close()
	if err != nil {
		return errResult(err)
	}
	if closeErr != nil {
		return errResult(closeErr)
	}
	tmp, err := os.CreateTemp(dir, ".latest-*")
	if err != nil {
		return errResult(err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(encoded)
	closeErr = tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = ws.CheckWritable(latest)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), latest)
	}
	if err != nil {
		return errResult(err)
	}
	result := behaviorReportResult(report, true)
	if ctx.Err() != nil {
		result.Err = ctx.Err()
	}
	return result
}

func readBehaviorFile(path string, limit int64) ([]byte, error) {
	// Refuse FIFOs/devices before open; O_NONBLOCK also closes the stat/open
	// replacement race for FIFOs. A model-authored path must not hang the run.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("suite input must be a regular file: %s", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("suite input must be a regular file: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("suite input exceeds %d bytes: %s", limit, path)
	}
	return data, nil
}
func resolveBehaviorPath(ws *Workspace, suitePath, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("empty suite input path")
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(filepath.Dir(suitePath), raw)
	}
	path, _, err := ws.validationFilePath(raw)
	if err != nil {
		return "", err
	}
	// Strict declared identities never use generic read-tool basename recovery.
	// Explicit outside spellings must not be rebound to a same-name local file.
	return path, nil
}
func behaviorInputs(ws *Workspace, path string, raw []byte, s behaviorSuite) (map[string]string, error) {
	inputs := map[string]string{ws.ModelPath(path): fmt.Sprintf("%x", sha256.Sum256(raw))}
	var total int64
	for _, name := range append([]string{s.FilePath}, s.Dependencies...) {
		p, err := resolveBehaviorPath(ws, path, name)
		if err != nil {
			return nil, err
		}
		data, err := readBehaviorFile(p, 32*1024*1024)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > 128*1024*1024 {
			return nil, fmt.Errorf("suite inputs exceed 128 MiB")
		}
		inputs[ws.ModelPath(p)] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	return inputs, nil
}
func validateBehaviorSuite(s behaviorSuite) error {
	if err := validateBehaviorSuiteMetadata(s); err != nil {
		return err
	}
	for _, scenario := range s.Scenarios {
		if err := validateBehaviorScenario(s, scenario); err != nil {
			return err
		}
	}
	return nil
}

func validateBehaviorSuiteMetadata(s behaviorSuite) error {
	if s.Version != 1 || strings.TrimSpace(s.FilePath) == "" {
		return fmt.Errorf("version must be 1 and file_path required")
	}
	if len(s.Requirements) < 1 || len(s.Requirements) > 64 || len(s.Scenarios) < 1 || len(s.Scenarios) > 32 || len(s.Dependencies) > 64 {
		return fmt.Errorf("requires 1-64 requirements, 1-32 scenarios and <=64 dependencies")
	}
	if s.SettleMS != nil && (*s.SettleMS < 0 || *s.SettleMS > 10000) {
		return fmt.Errorf("settle_ms outside 0..10000")
	}
	reqs := map[string]bool{}
	for i, r := range s.Requirements {
		if strings.TrimSpace(r.ID) == "" || len(r.ID) > 128 || reqs[r.ID] {
			return fmt.Errorf("requirements[%d].id=%q must be unique, nonempty and <=128 bytes", i, truncateSuiteDiagnostic(r.ID))
		}
		if strings.TrimSpace(r.Text) == "" || len(r.Text) > 4096 {
			return fmt.Errorf("requirements[%d] id=%q: text must contain 1..4096 bytes", i, r.ID)
		}
		if r.Origin != "explicit" && r.Origin != "derived" {
			return fmt.Errorf("requirements[%d] id=%q: origin=%q is invalid; allowed origin values: explicit, derived. This is the origin field, not an ID or JSON parsing failure; preserve text and ID", i, r.ID, truncateSuiteDiagnostic(r.Origin))
		}
		reqs[r.ID] = true
	}
	seen := map[string]bool{}
	for _, scenario := range s.Scenarios {
		if strings.TrimSpace(scenario.ID) == "" || len(scenario.ID) > 128 || seen[scenario.ID] || len(scenario.RequirementIDs) == 0 || len(scenario.RequirementIDs) > 64 {
			return fmt.Errorf("scenarios need unique IDs and requirement_ids")
		}
		seen[scenario.ID] = true
		ids := map[string]bool{}
		for _, id := range scenario.RequirementIDs {
			if !reqs[id] || ids[id] {
				return fmt.Errorf("scenario %s has unknown/duplicate requirement %s", scenario.ID, id)
			}
			ids[id] = true
		}
	}
	return nil
}

func validateBehaviorScenario(s behaviorSuite, scenario behaviorScenario) error {
	properties := (&MeadowBrowserInteractionCheck{}).Schema()["properties"].(map[string]any)["steps"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	for _, step := range scenario.Steps {
		for key := range step {
			if _, ok := properties[key]; !ok {
				return fmt.Errorf("scenario %s unknown step field %q", scenario.ID, key)
			}
		}
	}
	args := behaviorScenarioArgs(s.FilePath, scenario, s.SettleMS)
	steps, err := interactionSteps(args["steps"])
	if err != nil {
		return fmt.Errorf("scenario %s: %w", scenario.ID, err)
	}
	if err := validateInteractionStateOrder(steps); err != nil {
		return fmt.Errorf("scenario %s: %w", scenario.ID, err)
	}
	if _, err := interactionCoverageRows(args["coverage"], steps); err != nil {
		return fmt.Errorf("scenario %s: %w", scenario.ID, err)
	}
	return nil
}

func truncateSuiteDiagnostic(value string) string {
	if len(value) > 128 {
		return value[:128] + "..."
	}
	return value
}
func behaviorScenarioArgs(artifact string, s behaviorScenario, settle *int) map[string]any {
	raw := make([]any, len(s.Steps))
	actions, assertions := []any{}, []any{}
	for i, step := range s.Steps {
		raw[i] = step
		action := strArg(step, "action")
		if interactionAction(action) {
			actions = append(actions, i+1)
		}
		if interactionAssertion(action) || action == "assert_text" {
			assertions = append(assertions, i+1)
		}
	}
	args := map[string]any{"file_path": artifact, "steps": raw, "coverage": []any{map[string]any{"requirement": s.ID, "action_steps": actions, "assertion_steps": assertions}}}
	if settle != nil {
		args["settle_ms"] = *settle
	}
	return args
}
func behaviorExecutionStatus(r Result, wantSteps int) string {
	steps, ok := r.BehaviorEvidence["steps"].([]map[string]any)
	if !ok {
		return "unavailable"
	}
	for _, step := range steps {
		status := step["status"]
		if status == "runtime_failed" {
			return "failed"
		}
		if status == "unavailable" || status == "attempted" {
			return "unavailable"
		}
		if status == "failed" {
			if step["action"] == "assert_state" {
				return "failed"
			}
			return "unavailable" // legacy/tool diagnostics alone do not classify source defects
		}
	}
	if r.Err != nil || len(steps) != wantSteps {
		return "unavailable"
	}
	if freshness, present := r.BehaviorEvidence["inputs_freshness"]; present && freshness != "current" {
		return "unavailable"
	}
	for _, step := range steps {
		if step["status"] != "passed" {
			return "unavailable"
		}
	}
	return "passed"
}

func behaviorScenarioSourceReceiptMatches(result Result, expected map[string]string, count int) bool {
	if result.BehaviorEvidence["inputs_freshness"] != "current" || len(result.FileObservations) != count {
		return false
	}
	seen := make(map[string]bool, count)
	for _, input := range result.FileObservations {
		if seen[input.Path] || expected[input.Path] != input.SHA256 {
			return false
		}
		seen[input.Path] = true
	}
	return true
}

func behaviorReportResult(report behaviorSuiteReport, run bool) Result {
	// Full step evidence lives in the immutable report; compact model feedback
	// retains counterexamples but avoids replaying every successful observation.
	// Preserve executed state predicates before removing detailed evidence.
	// A later weaker predicate is not a recovery of an earlier failed one.
	if report.AssertionChecks == nil {
		for _, scenario := range report.Scenarios {
			encoded, _ := json.Marshal(scenario.Evidence["steps"])
			var steps []map[string]any
			if json.Unmarshal(encoded, &steps) != nil {
				continue
			}
			for _, step := range steps {
				if step["action"] != "assert_state" || (step["status"] != "passed" && step["status"] != "failed") {
					continue
				}
				if _, observed := step["actual"]; !observed {
					continue
				}
				fact := map[string]any{"scenario_id": scenario.ID}
				for _, key := range []string{"action", "status", "source", "selector", "path", "operator", "expected", "previous"} {
					if value, ok := step[key]; ok {
						fact[key] = value
					}
				}
				actual, err := json.Marshal(step["actual"])
				if err == nil && len(actual) <= 2048 {
					fact["actual"] = step["actual"]
				} else if err == nil {
					excerpt := actual[:2048]
					for !utf8.Valid(excerpt) {
						excerpt = excerpt[:len(excerpt)-1]
					}
					fact["actual_truncated"] = true
					fact["actual_json_excerpt"] = string(excerpt)
					fact["actual_encoded_bytes"] = len(actual)
				}
				report.AssertionChecks = append(report.AssertionChecks, fact)
			}
		}
	}
	for i := range report.Scenarios {
		report.Scenarios[i].Evidence = nil
	}
	data, _ := json.Marshal(report)
	r := Result{LLMContent: string(data), Display: "behavior suite " + report.Freshness}
	if run {
		for _, s := range report.Scenarios {
			if s.Status == "failed" || s.Status == "unavailable" || s.Status == "invalid" {
				r.Err = fmt.Errorf("suite contains failed/unavailable/invalid checks")
				break
			}
		}
		if report.Freshness != "current" {
			r.Err = fmt.Errorf("suite evidence is stale")
		}
	}
	return r
}
