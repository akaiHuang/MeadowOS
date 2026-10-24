package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// A scoped structural patch: the model supplies only replacement steps, never
// replacement requirements or a verdict. Hash and prior-read guard stale edits.
type MeadowBrowserSuiteEdit struct{ WS *Workspace }

func (*MeadowBrowserSuiteEdit) Name() string                     { return "meadow_browser_suite_edit" }
func (*MeadowBrowserSuiteEdit) Kind() Kind                       { return KindEdit }
func (*MeadowBrowserSuiteEdit) ParallelSafe(map[string]any) bool { return false }
func (*MeadowBrowserSuiteEdit) Description() string {
	return "Replace one saved suite scenario's steps by exact scenario_id, preserving requirements and every other scenario. First meadow_browser_suite operation=inspect to obtain expected_sha256 and schema. No old_string needed. All changed steps must validate; other existing scenario errors are reported for later repair. Editing does not pass any tests; rerun suite afterward."
}
func (*MeadowBrowserSuiteEdit) Schema() map[string]any {
	return obj(map[string]any{"file_path": str("Existing suite JSON path"), "expected_sha256": str("Exact SHA256 from suite inspect"), "scenario_id": str("Existing unique scenario ID"), "steps": (&MeadowBrowserInteractionCheck{}).Schema()["properties"].(map[string]any)["steps"]}, "file_path", "expected_sha256", "scenario_id", "steps")
}
func decodeBehaviorSuite(raw []byte) (behaviorSuite, error) {
	var suite behaviorSuite
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&suite); err != nil {
		return suite, err
	}
	if d.Decode(new(any)) != io.EOF {
		return suite, fmt.Errorf("trailing JSON")
	}
	return suite, nil
}
func (t *MeadowBrowserSuiteEdit) Execute(ctx context.Context, args map[string]any) Result {
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	if t.WS == nil {
		return errResultf("suite workspace unavailable")
	}
	for k := range args {
		if k != "file_path" && k != "expected_sha256" && k != "scenario_id" && k != "steps" {
			return errResultf("unknown suite edit argument")
		}
	}
	path, err := t.WS.Resolve(strArg(args, "file_path"))
	if err != nil {
		return errResult(err)
	}
	path, err = resolveBehaviorPath(t.WS, path, path)
	if err != nil {
		return errResult(err)
	}
	if err = t.WS.CheckWritable(path); err != nil {
		return errResult(err)
	}
	if err = t.WS.CheckPriorRead(path); err != nil {
		return errResult(err)
	}
	raw, err := readBehaviorFile(path, 256*1024)
	if err != nil {
		return errResult(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	if strArg(args, "expected_sha256") != hash {
		return errResultf("SUITE_STALE: inspect current suite and retry; no bytes changed")
	}
	s, err := decodeBehaviorSuite(raw)
	if err != nil {
		return errResult(err)
	}
	found := -1
	for i, scenario := range s.Scenarios {
		if scenario.ID == strArg(args, "scenario_id") {
			if found >= 0 {
				return errResultf("ambiguous scenario ID")
			}
			found = i
		}
	}
	if found < 0 {
		return errResultf("unknown scenario ID")
	}
	stepRaw, err := json.Marshal(args["steps"])
	if err != nil {
		return errResult(err)
	}
	var steps []map[string]any
	if json.Unmarshal(stepRaw, &steps) != nil || len(steps) == 0 || len(steps) > 20 {
		return errResultf("steps must contain 1..20 objects")
	}
	original := s.Scenarios[found]
	replacement := original
	replacement.Steps = steps
	// Validate all metadata/IDs while allowing existing unrelated invalid steps.
	check := s
	check.Scenarios = append([]behaviorScenario(nil), s.Scenarios...)
	for i := range check.Scenarios {
		check.Scenarios[i].Steps = []map[string]any{{"action": "assert_text", "selector": "body", "value": "metadata validation only"}}
	}
	check.Scenarios[found] = replacement
	if err = validateBehaviorSuite(check); err != nil {
		return errResultf("SUITE_PATCH_INVALID: %v; no bytes changed", err)
	}
	s.Scenarios[found] = replacement
	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return errResult(err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > 256*1024 {
		return errResultf("suite exceeds 256 KiB")
	}
	if err = ctx.Err(); err != nil {
		return errResult(err)
	}
	current, err := readBehaviorFile(path, 256*1024)
	if err != nil {
		return errResult(err)
	}
	if !bytes.Equal(current, raw) {
		return errResultf("SUITE_STALE: changed during edit; no bytes changed")
	}
	if err = t.WS.CheckPriorRead(path); err != nil {
		return errResult(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return errResult(err)
	}
	if bytes.Equal(encoded, raw) {
		return Result{LLMContent: "Suite unchanged; no tests executed.", NoChange: true}
	}
	if err = atomicWriteFile(path, encoded, info.Mode().Perm()); err != nil {
		return errResult(err)
	}
	t.WS.NoteRead(path)
	remaining := "none"
	if err = validateBehaviorSuite(s); err != nil {
		remaining = err.Error()
	}
	return Result{LLMContent: fmt.Sprintf("Updated scenario %s in %s. Requirements and other scenarios preserved. Remaining preflight error: %s. No tests executed; rerun suite. sha256=%x", original.ID, t.WS.ModelPath(path), remaining, sha256.Sum256(encoded)), Display: "已更新驗收案例 " + original.ID, ChangedPaths: []string{path}, Diff: boundedUnifiedDiff(t.WS.ModelPath(path), string(raw), string(encoded))}
}
