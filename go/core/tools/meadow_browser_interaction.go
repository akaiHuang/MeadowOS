package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hey-meadow/meadow-code/go/core/browserruntime"
)

const (
	meadowBrowserInteractionMaxSteps        = 20
	meadowBrowserInteractionMaxSettleMS     = 10_000
	meadowBrowserInteractionMaxDependencies = 64
)

// MeadowBrowserInteractionCheck executes a small, declarative acceptance test
// against a generated local web artifact. Unlike the visual checker, this
// proves that user-visible controls cause the requested state changes.
type MeadowBrowserInteractionCheck struct {
	WS              *Workspace
	BaseURL         string
	HTTP            *http.Client
	StartLocal      func(context.Context, string) error
	RuntimeIdentity func(context.Context, string) error
	NewWorker       func(context.Context) (string, func(), error)
}

func (t *MeadowBrowserInteractionCheck) Name() string { return "meadow_browser_interaction_check" }
func (t *MeadowBrowserInteractionCheck) Description() string {
	return "驗證已儲存 HTML 的一個需求：提供簡短且固定的 requirement 與依序 steps，不需 coverage 索引；requirement 只放受測條件，操作說明與進度敘述留在工具參數外。由你選擇前置動作與預期結果；每次重新載入，動作之後必須有斷言。assert_text(selector,expected_text) 驗證文字；assert_state(source,path,operator,expected_number/expected_boolean/expected_text) 驗證真實型別。可用 dependencies 列出本地相依檔案以比對執行前後來源版本；未列檔案、遠端狀態與瀏覽器儲存不在版本範圍。需要動態控制項時先用 meadow_browser_inspect 的 setup_steps。多案例回歸用 meadow_browser_suite。只證明實際執行的檢查，不代表需求完整；未執行或觀察不可用不能當成程式錯誤。"
}
func (t *MeadowBrowserInteractionCheck) Kind() Kind                       { return KindExecute }
func (t *MeadowBrowserInteractionCheck) ParallelSafe(map[string]any) bool { return false }

// NormalizeArguments accepts common lossless model wire shapes without changing
// the requested interaction semantics. It repairs JSON-stringified steps, a
// known duplicated-colon typo, scalar assertion text that belongs in a
// one-element array, a missing key that is already encoded by KeyboardEvent.code,
// and coverage step numbers placed in the wrong column. Selectors,
// requirements, assertions, and execution ordering are never invented or
// changed.
func (t *MeadowBrowserInteractionCheck) NormalizeArguments(args map[string]any) {
	var steps []any
	switch raw := args["steps"].(type) {
	case string:
		if strings.TrimSpace(raw) == "" {
			return
		}
		raw = interactionActionDoubleColon.ReplaceAllString(raw, `${1}: "`)
		if json.Unmarshal([]byte(raw), &steps) != nil {
			return
		}
		args["steps"] = steps
	case []any:
		steps = raw
	default:
		return
	}

	for _, rawStep := range steps {
		step, ok := rawStep.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"contains", "contains_any", "not_contains"} {
			if value, ok := step[field].(string); ok && strings.TrimSpace(value) != "" {
				step[field] = []any{value}
			}
		}
		if strings.TrimSpace(strArg(step, "action")) != "press_key" || strings.TrimSpace(strArg(step, "key")) != "" {
			continue
		}
		if key := interactionKeyFromCode(strings.TrimSpace(strArg(step, "code")), sliceArg(step, "modifiers")); key != "" {
			step["key"] = key
		}
	}
	normalizeInteractionCoverageColumns(args["coverage"], steps)
}

var interactionActionDoubleColon = regexp.MustCompile(`("action"\s*):\s*"\s*:\s*"`)

func interactionKeyFromCode(code string, modifiers []string) string {
	switch code {
	case "Space":
		return "Space"
	case "Enter", "Return", "Tab", "Escape", "Backspace", "Delete",
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight":
		return code
	}
	if len(code) == 4 && strings.HasPrefix(code, "Key") {
		key := strings.ToLower(code[3:])
		for _, modifier := range modifiers {
			if modifier == "shift" {
				return strings.ToUpper(key)
			}
		}
		return key
	}
	if len(code) == 6 && strings.HasPrefix(code, "Digit") {
		return code[5:]
	}
	return ""
}

func normalizeInteractionCoverageColumns(raw any, steps []any) {
	rows, ok := raw.([]any)
	if !ok || len(rows) == 0 || len(steps) == 0 {
		return
	}
	for _, rawRow := range rows {
		row, ok := rawRow.(map[string]any)
		if !ok {
			continue
		}
		actionNumbers, actionsOK := interactionRawStepNumbers(row["action_steps"], len(steps))
		assertionNumbers, assertionsOK := interactionRawStepNumbers(row["assertion_steps"], len(steps))
		if !actionsOK || !assertionsOK {
			continue
		}
		allNumbers := append(append([]int(nil), actionNumbers...), assertionNumbers...)
		canonicalActions, canonicalAssertions := interactionClassifyStepNumbers(allNumbers, steps)
		if len(canonicalActions) == 0 || len(canonicalAssertions) == 0 ||
			interactionSameStepNumbers(actionNumbers, canonicalActions) &&
				interactionSameStepNumbers(assertionNumbers, canonicalAssertions) {
			continue
		}
		row["action_steps"] = interactionStepNumbersAny(canonicalActions)
		row["assertion_steps"] = interactionStepNumbersAny(canonicalAssertions)
	}
}

func interactionClassifyStepNumbers(numbers []int, steps []any) ([]int, []int) {
	actions := make([]int, 0, len(numbers))
	assertions := make([]int, 0, len(numbers))
	seen := make(map[int]bool, len(numbers))
	for _, number := range numbers {
		if seen[number] {
			continue
		}
		seen[number] = true
		step, ok := steps[number-1].(map[string]any)
		if !ok {
			continue
		}
		action := strings.TrimSpace(strArg(step, "action"))
		if interactionAction(action) {
			actions = append(actions, number)
		} else if interactionAssertion(action) {
			assertions = append(assertions, number)
		}
	}
	return actions, assertions
}

func interactionSameStepNumbers(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func interactionStepNumbersAny(numbers []int) []any {
	result := make([]any, 0, len(numbers))
	for _, number := range numbers {
		result = append(result, number)
	}
	return result
}

func interactionRawStepNumbers(raw any, stepCount int) ([]int, bool) {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil, false
	}
	result := make([]int, 0, len(items))
	for _, item := range items {
		var number int
		switch value := item.(type) {
		case int:
			number = value
		case float64:
			if value != float64(int(value)) {
				return nil, false
			}
			number = int(value)
		default:
			return nil, false
		}
		if number < 1 || number > stepCount {
			return nil, false
		}
		result = append(result, number)
	}
	return result, true
}

func interactionRawStepsAll(numbers []int, steps []any, predicate func(string) bool) bool {
	for _, number := range numbers {
		step, ok := steps[number-1].(map[string]any)
		if !ok || !predicate(strings.TrimSpace(strArg(step, "action"))) {
			return false
		}
	}
	return len(numbers) > 0
}

func (t *MeadowBrowserInteractionCheck) Schema() map[string]any {
	step := obj(map[string]any{
		"action": map[string]any{
			"type":        "string",
			"enum":        []string{"fill", "click", "context_click", "press_key", "advance_time", "observe_state", "assert_state", "assert_text", "assert_dom", "assert_game_state", "assert_canvas_pixels"},
			"description": "fill/click/context_click/press_key exercise browser event handlers. observe_state records a scalar baseline, assert_state tests exact values/bounds/changes. assert_dom checks rendered text; assert_game_state uses render_game_to_text(); advance_time calls advanceTime(ms); assert_canvas_pixels checks nonempty painting only.",
		},
		"selector": str("CSS selector for fill/click/context_click, optional focused element for press_key, or optional DOM scope for assert_dom."),
		"text":     str("Text to enter for fill."),
		"key":      str("KeyboardEvent.key for press_key, for example ArrowLeft, ArrowUp, w, Space, or Enter."),
		"code":     str("Optional KeyboardEvent.code for press_key; Meadow Browser derives it when omitted."),
		"modifiers": map[string]any{
			"type": "array", "items": map[string]any{
				"type": "string", "enum": []string{"shift", "cmd", "meta", "opt", "alt", "ctrl"},
			},
			"maxItems":    3,
			"description": "Optional press_key modifiers.",
		},
		"ms":               num("Milliseconds for advance_time. Must be 1-10000."),
		"source":           map[string]any{"type": "string", "enum": []string{"dom", "app"}, "description": "Required for observe_state/assert_state. dom reads rendered properties; app reads a synchronous read-only window.meadowTest.getState() JSON projection, not a success flag."},
		"path":             str("State path. dom: text/value/checked/count/visible/disabled, with selector required. app: dotted own-property path, e.g. player.x or raw grid for collection contracts; omit selector. Missing paths fail even for ne/null."),
		"operator":         map[string]any{"type": "string", "enum": []string{"eq", "ne", "gt", "gte", "lt", "lte", "changed", "unchanged", "rectangular", "unique", "all_eq", "all_gte", "all_lte"}, "description": "assert_state only. changed/unchanged require earlier same-source/path observation. App array contracts: rectangular value=positive integer width (every row string/array); unique omits value; all_eq/all_gte/all_lte compare every scalar item to value. Collections must be nonempty. Numeric comparisons never coerce strings. Other operators require value."},
		"value":            map[string]any{"description": "Legacy expected scalar. Prefer exactly one typed expected_* field; never combine. Omit for observe_state/changed/unchanged/unique."},
		"expected_number":  num("Expected JSON number, not a string. DOM text is always a string; use DOM count or a real numeric app path for numeric comparisons."),
		"expected_text":    str("Expected exact string. Alternative to value; do not combine expected fields."),
		"expected_boolean": map[string]any{"type": "boolean", "description": "Expected true/false, not a string. Alternative to value."},
		"expected_null":    map[string]any{"type": "boolean", "enum": []any{true}, "description": "Set true only when the expected value is JSON null. Omit for other expectations."},
		"contains": map[string]any{
			"type": "array", "items": map[string]any{"type": "string"},
			"description": "Every string must appear in rendered DOM for assert_dom or compact game state for assert_game_state.",
		},
		"contains_any": map[string]any{
			"type": "array", "items": map[string]any{"type": "string"},
			"description": "At least one string must appear in rendered DOM or compact game state.",
		},
		"not_contains": map[string]any{
			"type": "array", "items": map[string]any{"type": "string"},
			"description": "No listed string may appear in rendered DOM or compact game state.",
		},
		"changed_from_previous": map[string]any{
			"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 8,
			"description": "For assert_game_state, require at least one listed JSON path to differ from the previous assert_game_state observation. Choose paths that represent the user-requested behavior; missing paths do not count as a change.",
		},
		"unchanged_from_previous": map[string]any{
			"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 8,
			"description": "For assert_game_state, require every listed JSON path to equal the previous assert_game_state observation. Use this after advance_time to prove that paused gameplay stayed still; missing paths fail the assertion.",
		},
	}, "action")
	step["allOf"] = []any{
		map[string]any{"if": map[string]any{"properties": map[string]any{"action": map[string]any{"enum": []string{"observe_state", "assert_state"}}}}, "then": map[string]any{"required": []string{"source", "path"}}},
		map[string]any{"if": map[string]any{"properties": map[string]any{"source": map[string]any{"const": "dom"}}, "required": []string{"source"}}, "then": map[string]any{"required": []string{"selector"}}},
		map[string]any{"if": map[string]any{"properties": map[string]any{"action": map[string]any{"const": "assert_state"}}}, "then": map[string]any{"required": []string{"operator"}}},
	}
	step["description"] = interactionStateExamples
	coverage := obj(map[string]any{
		"requirement": str("One concise, stable criterion only. Use an exact user quote when explicit; otherwise label a necessary inferred acceptance criterion as derived. Keep its text unchanged when rerunning the same criterion. Do not include progress narration, prior tool calls, recovery discussion or manually escaped Unicode. Never claim inferred details were explicitly requested."),
		"action_steps": map[string]any{
			"type": "array", "minItems": 0, "maxItems": meadowBrowserInteractionMaxSteps,
			"items":       map[string]any{"type": "integer", "minimum": 1, "maximum": meadowBrowserInteractionMaxSteps},
			"description": "1-based fill/click/context_click/press_key/advance_time steps; empty only for initial assertions before any action. Never list assertions here.",
		},
		"assertion_steps": map[string]any{
			"type": "array", "minItems": 1, "maxItems": meadowBrowserInteractionMaxSteps,
			"items":       map[string]any{"type": "integer", "minimum": 1, "maximum": meadowBrowserInteractionMaxSteps},
			"description": "Only 1-based later assertion steps: assert_state/assert_dom/assert_game_state/assert_canvas_pixels. observe_state is not an assertion. Never list input actions here.",
		},
	}, "requirement", "action_steps", "assertion_steps")
	return obj(map[string]any{
		"file_path":    str("Saved LOCAL HTML path, e.g. /workspace/app.html. Never an http(s) or file URL. Do not start a server for this tool; it snapshots the saved artifact itself."),
		"dependencies": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": meadowBrowserInteractionMaxDependencies, "description": "Optional declared local companion files, relative to the HTML directory or explicit workspace/session paths. Source-byte freshness covers only these files and the entry document at the before/after boundaries; no undeclared dependency, loaded-byte, remote-state or persistent-storage guarantee."},
		"requirement":  str("The single concise, stable criterion this scenario tests. Quote explicit requirements or label a derived criterion; keep its text unchanged when rerunning that criterion. Put narration and recovery discussion outside arguments; send ordinary text, not manually escaped Unicode. Use this instead of coverage; Harness derives numeric indices, not expected outcomes."),
		"steps": map[string]any{
			"type": "array", "minItems": 1, "maxItems": meadowBrowserInteractionMaxSteps,
			"items":       step,
			"description": "Ordered checks with at least one assertion. Initial-only checks allowed. For changes, observe_state before an action then assert_state changed; prefer exact values when specified.",
		},
		"coverage": map[string]any{
			"type": "array", "minItems": 1, "maxItems": meadowBrowserInteractionMaxSteps,
			"items":       coverage,
			"description": "One row per requirement-derived criterion tested by this call. Every input action must be claimed by a row with a later assertion. This graph does not prove complete or semantically correct coverage.",
		},
		"settle_ms": num("Optional delay after each interaction, 0-10000 ms; default 120. Use this instead of a wait action."),
	}, "file_path", "steps")
}

func (t *MeadowBrowserInteractionCheck) Execute(ctx context.Context, args map[string]any) (result Result) {
	ctx = desktopBrowserPageContext(ctx, t.BaseURL)
	if problems := interactionStateFieldProblems(args["steps"]); len(problems) > 0 {
		return interactionInvalidCall(strings.Join(problems, "; "))
	}
	helper := &MeadowBrowserVisualCheck{WS: t.WS, BaseURL: t.BaseURL, HTTP: t.HTTP, StartLocal: t.StartLocal, RuntimeIdentity: t.RuntimeIdentity}
	target, localPath, err := helper.targetURL(args)
	if err != nil {
		return interactionInvalidCall(err.Error())
	}
	steps, err := interactionSteps(args["steps"])
	if err != nil {
		return interactionInvalidCall(err.Error())
	}
	rawCoverage, err := interactionCallCoverage(args, steps)
	if err != nil {
		return interactionInvalidCall(err.Error())
	}
	coverage, err := interactionCoverageRows(rawCoverage, steps)
	if err != nil {
		return interactionCoverageInvalidCall(err.Error(), rawCoverage, steps)
	}
	settle := 120
	if value, ok := intArg(args, "settle_ms"); ok {
		if value < 0 || value > meadowBrowserInteractionMaxSettleMS {
			return interactionInvalidCall("settle_ms must be between 0 and 10000")
		}
		settle = value
	}
	if err := validateInteractionStateOrder(steps); err != nil {
		return interactionInvalidCall(fmt.Sprintf("INTERACTION_CHECK_CALL_INVALID: %v", err))
	}
	inputPaths, err := interactionInputPaths(t.WS, localPath, args["dependencies"])
	if err != nil {
		return interactionInvalidCall(err.Error())
	}
	inputs, inputError := interactionInputSnapshots(ctx, t.WS, inputPaths)
	steps, selectorCorrections := groundInteractionIDSelectors(localPath, steps)
	observations := make([]map[string]any, 0, len(steps))
	clockVerified := false
	defer func() {
		if len(observations) == 0 {
			return
		}
		freshness := "unknown"
		if ctx.Err() != nil {
			inputError = "source recheck unavailable because the run was cancelled"
		}
		if inputError == "" && len(inputs) > 0 {
			after, afterError := interactionInputSnapshots(ctx, t.WS, inputPaths)
			if ctx.Err() != nil {
				inputError = "source recheck unavailable because the run was cancelled"
			} else if afterError != "" || !reflect.DeepEqual(inputs, after) {
				freshness = "stale"
				inputError = "declared source changed or became unavailable during execution"
			} else if ctx.Err() == nil {
				freshness = "current"
				result.FileObservations = append([]FileObservation(nil), inputs...)
			}
		}
		inputVersions := make(map[string]string, len(inputs))
		for _, input := range inputs {
			inputVersions[input.Path] = input.SHA256
		}
		result.BehaviorEvidence = map[string]any{
			"animation_clock_verified": clockVerified,
			"scope":                    "only executed checks; coverage is model-authored, not proof of complete requirements",
			"input_fidelity":           "browser event dispatch; not OS-trusted input",
			"state_fidelity":           "DOM properties or page-reported app state; not visual-motion proof",
			"inputs":                   inputVersions,
			"declared_input_count":     len(inputPaths),
			"inputs_freshness":         freshness,
			"input_scope":              "declared local entry/dependency source bytes at before/after boundaries only; not loaded-byte identity; undeclared files, remote state and persistent browser storage are untracked",
			"coverage":                 interactionExecutedCoverage(coverage, observations), "steps": observations,
		}
		if inputError != "" {
			result.BehaviorEvidence["inputs_error"] = inputError
		}
		report, _ := json.Marshal(result.BehaviorEvidence)
		result.LLMContent += "\nbehavior_evidence: " + string(report)
	}()
	base := helper.base()
	ownedWorker := t.NewWorker != nil || (t.BaseURL == "" && t.HTTP == nil && t.StartLocal == nil)
	if ownedWorker {
		factory := t.NewWorker
		if factory == nil {
			factory = func(ctx context.Context) (string, func(), error) { return startInteractionWorker(ctx, helper) }
		}
		workerBase, cleanup, err := factory(ctx)
		if err != nil {
			return errResultf("INTERACTION_ENVIRONMENT_UNAVAILABLE: %v. No application behavior was tested; do not modify game logic on this evidence.", err)
		}
		defer cleanup()
		base = workerBase
	}
	if ownedWorker {
		if err := helper.runtimeIdentity(ctx, base); err != nil {
			return errResultf("INTERACTION_ENVIRONMENT_UNAVAILABLE: owned browser identity: %w. Preserve source; no application behavior was tested", err)
		}
		if _, err := helper.health(ctx, base); err != nil {
			return errResultf("INTERACTION_ENVIRONMENT_UNAVAILABLE: owned browser stopped responding: %v. Preserve source; no application behavior was tested.", err)
		}
	} else if _, _, err := helper.ensureAvailable(ctx, base); err != nil {
		return browserRuntimeGuardResult(fmt.Errorf("Meadow Browser is unavailable at %s: %w", base, err))
	}
	navigationTarget, cleanupSnapshot, err := snapshotLocalWebArtifactTarget(localPath, target)
	if err != nil {
		return errResultf("prepare cache-safe Meadow Browser verification snapshot: %v", err)
	}
	defer cleanupSnapshot()
	diagnosticCursor := 0
	if baseline, baselineErr := helper.browserDiagnostics(ctx, base); baselineErr == nil {
		diagnosticCursor = baseline.LastID
	}
	if err := helper.postJSON(ctx, base+"/goto", map[string]any{
		"url": navigationTarget, "wait_until": "load", "wait_strategy": "paint", "settle_ms": 120,
	}, nil); err != nil {
		if browserruntime.Code(err) != "" {
			return errResultf("INTERACTION_ENVIRONMENT_UNAVAILABLE: navigation was not dispatched: %w", err)
		}
		if diagnostics, diagnosticsErr := helper.browserDiagnostics(ctx, base); diagnosticsErr == nil {
			diagnostics = diagnosticsSince(diagnostics, diagnosticCursor)
			if len(diagnostics.errors()) > 0 {
				return browserRuntimeErrorResult(helper.WS, localPath, diagnostics, "Meadow Browser navigation failed")
			}
		}
		return errResultf("Meadow Browser goto %s: %w", target, err)
	}
	if result := interactionBrowserDiagnostics(ctx, helper, base, localPath, diagnosticCursor, 0, "page_load", ""); result != nil {
		return *result
	}
	if ownedWorker {
		if err := verifyInteractionClock(ctx, helper, base); err != nil {
			if browserruntime.Code(err) != "" {
				return browserRuntimeGuardResult(err)
			}
			return errResult(&RecoveryError{Code: "browser_environment_unavailable", Message: fmt.Sprintf("INTERACTION_ENVIRONMENT_UNAVAILABLE: animation clock did not pass its liveness probe: %v. This is not an application assertion failure; preserve source and criteria.", err), RequiredAction: "restore_browser_clock_and_retry", PreserveDraft: true})
		}
		clockVerified = true
	}

	deterministicGameContract := interactionHasDeterministicGameContract(steps)
	var previousGameState any
	stateBaselines := make(map[string]any)
	for index, step := range steps {
		observation := map[string]any{"step": index + 1, "action": step.Action, "status": "attempted"}
		if step.Selector != "" {
			observation["selector"] = step.Selector
		}
		if step.Key != "" {
			observation["key"] = step.Key
		}
		observations = append(observations, observation)
		var stepErr error
		switch step.Action {
		case "fill":
			stepErr = helper.postJSON(ctx, base+"/fill", map[string]any{
				"selector": step.Selector, "text": step.Text,
			}, nil)
		case "click":
			stepErr = helper.postJSON(ctx, base+"/click", map[string]any{
				"selector": step.Selector,
			}, nil)
		case "context_click":
			stepErr = contextClickInteraction(ctx, helper, base, step.Selector)
		case "press_key":
			payload := map[string]any{"key": step.Key}
			if step.Code != "" {
				payload["code"] = step.Code
			}
			if step.Selector != "" {
				payload["selector"] = step.Selector
			}
			if len(step.Modifiers) > 0 {
				payload["modifiers"] = append([]string(nil), step.Modifiers...)
			}
			stepErr = helper.postJSON(ctx, base+"/press", payload, nil)
		case "advance_time":
			stepErr = advanceInteractionGameTime(ctx, helper, base, step.MS)
		case "observe_state", "assert_state":
			actual, readErr := readInteractionState(ctx, helper, base, step.State)
			observation["source"], observation["path"] = step.State.Source, step.State.Path
			if readErr != nil {
				observation["status"], observation["error"] = "unavailable", readErr.Error()
				if browserruntime.Code(readErr) != "" {
					return browserRuntimeGuardResult(readErr)
				}
				if failure := interactionBrowserDiagnostics(ctx, helper, base, localPath, diagnosticCursor, index+1, step.Action, step.Selector); failure != nil {
					return *failure
				}
				controls := renderedInteractionControlDiagnostic(ctx, helper, base)
				return errResult(&RecoveryError{
					Code:           "browser_observation_unavailable",
					Message:        fmt.Sprintf("INTERACTION_OBSERVATION_UNAVAILABLE at step %d: %v.%s This observation is unavailable, not a failed behavior assertion.", index+1, readErr, controls),
					RequiredAction: "ground_observation_preserve_criteria_and_retry",
					RequiredArgs: map[string]any{
						"step": index + 1, "source": step.State.Source, "selector": step.State.Selector, "path": step.State.Path,
						"repair_contract": "Inspect the saved event handler and current rendered controls. For app source, first confirm an actual synchronous read-only meadowTest.getState() projection exposes this path from real application state; a lexical variable alone is not exposed. Use an existing DOM observation when it measures the same criterion, or add a read-only projection without changing behavior. Never substitute constant success values, invented paths, or weaker assertions. Re-run the original criterion and affected regressions; no observation means no pass.",
					},
					Diagnostics: []map[string]any{{"kind": "observation_unavailable", "message": readErr.Error(), "source": step.State.Source, "path": step.State.Path, "step": index + 1}}, PreserveDraft: true,
				})
			}
			observation["actual"] = actual
			previous := stateBaselines[step.State.key()]
			if step.Action == "assert_state" {
				observation["operator"] = step.State.Operator
				if step.State.Operator == "changed" || step.State.Operator == "unchanged" {
					observation["previous"] = previous
				} else {
					observation["expected"] = step.State.Value
				}
				if !compareInteractionState(step.State, actual, previous) {
					stepErr = fmt.Errorf("state assertion %s %s failed; actual=%#v (%T) expected=%#v (%T) previous=%#v", step.State.Path, step.State.Operator, actual, actual, step.State.Value, step.State.Value, previous)
				}
			}
			if stepErr == nil {
				stateBaselines[step.State.key()] = actual
			}
		case "assert_dom":
			stepErr = assertInteractionDOM(ctx, helper, base, step)
		case "assert_game_state":
			var currentGameState any
			currentGameState, stepErr = assertInteractionGameState(ctx, helper, base, step, previousGameState)
			if stepErr == nil {
				previousGameState = currentGameState
			}
		case "assert_canvas_pixels":
			stepErr = assertInteractionCanvasPixels(ctx, helper, base, step.Selector)
		}
		if stepErr != nil {
			if browserruntime.Code(stepErr) != "" {
				observation["status"], observation["error"] = "unavailable", stepErr.Error()
				return errResultf("INTERACTION_ENVIRONMENT_UNAVAILABLE at step %d: browser request was not dispatched: %w. Preserve source and criteria", index+1, stepErr)
			}
			observation["status"], observation["error"] = "failed", stepErr.Error()
			if result := interactionBrowserDiagnostics(ctx, helper, base, localPath, diagnosticCursor, index+1, step.Action, step.Selector); result != nil {
				return *result
			}
			if strings.Contains(stepErr.Error(), "INPUT_UNAVAILABLE:") {
				observation["status"] = "unavailable"
				return errResult(&RecoveryError{Code: "browser_input_unavailable", Message: fmt.Sprintf("INPUT_UNAVAILABLE at step %d (%s): %v. No input was dispatched; later assertions were not run. This is an input precondition, not proof of a source defect.", index+1, step.Action, stepErr), RequiredAction: "inspect_visible_controls_and_establish_required_preconditions", RequiredArgs: map[string]any{"failed_step": index + 1, "selector": step.Selector, "input_dispatched": false}, PreserveDraft: true})
			}
			if (step.Action == "click" || step.Action == "fill" || step.Action == "press_key") && strings.Contains(strings.ToLower(stepErr.Error()), "selector not found") {
				renderedControls := renderedInteractionControlDiagnostic(ctx, helper, base)
				evidence := interactionFailureEvidence(ctx, helper, base, localPath, index+1, step.Action, step.Selector, stepErr)
				return Result{
					LLMContent: fmt.Sprintf(
						"INTERACTION_CHECK_CALL_INVALID at step %d (%s): validator selector %q did not match the rendered page.%s%s\n%s\nCorrect only this acceptance-test selector using a control that actually exists in the current rendered state, then rerun meadow_browser_interaction_check with all requested behavior assertions preserved. Do not edit the artifact solely to make an invented test selector exist.",
						index+1, step.Action, step.Selector, availableHTMLIDDiagnostic(localPath), renderedControls, evidence,
					),
					Display: fmt.Sprintf("interaction selector invalid at step %d", index+1),
					Err:     stepErr,
				}
			}
			diagnostic := localHTMLRuntimeDiagnostic(localPath)
			if diagnostic != "" {
				diagnostic = " Runtime diagnostic: " + diagnostic
			}
			if binding := interactionBindingDiagnostic(localPath, steps, index); binding != "" && step.State == nil {
				diagnostic += " Interaction binding diagnostic: " + binding
			}
			if step.Action == "assert_game_state" {
				diagnostic += interactionGameHookSourceDiagnostic(localPath)
			}
			recovery := "Repair the saved TypeScript/HTML source, rebuild the same HTML, then rerun this acceptance test."
			if step.Action == "assert_game_state" && !deterministicGameContract {
				evidence := interactionFailureEvidence(ctx, helper, base, localPath, index+1, step.Action, step.Selector, stepErr)
				return Result{
					LLMContent: fmt.Sprintf("INTERACTION_HUMAN_REVIEW_REQUIRED at step %d (%s): Meadow Browser cannot establish visual gameplay or motion from this state assertion. The saved artifact is preserved. Ask the human to play it and report the observed behavior; inspect source or add focused console logging only after that report.\n%s", index+1, step.Action, evidence),
					Display:    "gameplay needs human review",
				}
			}
			if step.Action == "assert_dom" {
				recovery = "Compare the expected and observed DOM text against the user's literal requirement. If the observed state already satisfies that requirement, correct only this acceptance assertion and rerun it; do not edit the artifact to satisfy invented formatting. If the observed state contradicts the requirement, repair the saved TypeScript/HTML source, rebuild the same HTML, then rerun the full acceptance test."
			}
			if step.Action == "assert_state" {
				recovery = "Compare the typed actual/expected/previous values with the requirement and its invariant. Inspect the event handler and state-to-render path; app observations come from meadowTest.getState(), not visual judgment. Repair a demonstrated source defect, or correct an independently demonstrated test/instrumentation defect. Never weaken criteria merely to pass. Rerun failed checks and affected regressions after the repair."
			}
			evidence := interactionFailureEvidence(ctx, helper, base, localPath, index+1, step.Action, step.Selector, stepErr)
			return Result{
				LLMContent: fmt.Sprintf("INTERACTION_CHECK_FAILED at step %d (%s): %v.%s\n%s\n%s", index+1, step.Action, stepErr, diagnostic, evidence, recovery),
				Display:    fmt.Sprintf("interaction check failed at step %d", index+1),
				Err:        stepErr,
			}
		}
		if settle > 0 && !interactionAssertion(step.Action) {
			timer := time.NewTimer(time.Duration(settle) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errResult(ctx.Err())
			case <-timer.C:
			}
		}
		if result := interactionBrowserDiagnostics(ctx, helper, base, localPath, diagnosticCursor, index+1, step.Action, step.Selector); result != nil {
			observation["status"] = "runtime_failed"
			return *result
		}
		observation["status"] = "passed"
	}
	content := fmt.Sprintf("Meadow Browser interaction acceptance passed for the supplied checks only: %s (%d ordered steps, %d model-authored requirement coverage rows). This does not establish untested requirements, visual quality, or overall task completion.", target, len(steps), len(coverage))
	if len(selectorCorrections) > 0 {
		content += " Validator selector grounding: " + strings.Join(selectorCorrections, "; ") + "."
	}
	return Result{
		LLMContent: content,
		Display:    "interaction acceptance passed",
	}
}

func interactionInputPaths(ws *Workspace, entry string, raw any) ([]string, error) {
	if ws == nil || entry == "" {
		return nil, nil
	}
	paths := []string{entry}
	if raw == nil {
		return paths, nil
	}
	var dependencies []string
	switch values := raw.(type) {
	case []string:
		dependencies = values
	case []any:
		for _, value := range values {
			path, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("dependencies must contain only local path strings")
			}
			dependencies = append(dependencies, path)
		}
	default:
		return nil, fmt.Errorf("dependencies must be a local path array")
	}
	if len(dependencies) > meadowBrowserInteractionMaxDependencies {
		return nil, fmt.Errorf("dependencies exceeds %d files", meadowBrowserInteractionMaxDependencies)
	}
	seen := map[string]bool{entry: true}
	for _, dependency := range dependencies {
		path, err := resolveBehaviorPath(ws, entry, dependency)
		if err != nil {
			return nil, fmt.Errorf("invalid declared dependency: %w", err)
		}
		if seen[path] {
			return nil, fmt.Errorf("dependencies repeats the same source %s", ws.ModelPath(path))
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths, nil
}

func interactionInputSnapshots(ctx context.Context, ws *Workspace, paths []string) ([]FileObservation, string) {
	if ws == nil || len(paths) == 0 {
		return nil, "declared local input snapshots unavailable"
	}
	inputs := make([]FileObservation, 0, len(paths))
	for _, path := range paths {
		if ctx.Err() != nil {
			return nil, "declared source observation cancelled"
		}
		input, err := ws.SnapshotValidationFile(path)
		if err != nil {
			// A subset of declared inputs never constitutes a current full receipt.
			return nil, "cannot snapshot declared source " + ws.ModelPath(path)
		}
		inputs = append(inputs, input)
	}
	return inputs, ""
}

func interactionGameHookSourceDiagnostic(localPath string) string {
	content, err := os.ReadFile(strings.TrimSpace(localPath))
	if err != nil || len(content) == 0 {
		return ""
	}
	lines := strings.Split(string(content), "\n")
	selected := make(map[int]bool)
	for index, line := range lines {
		if !strings.Contains(line, "render_game_to_text") && !strings.Contains(line, "advanceTime") {
			continue
		}
		start := index - 1
		if start < 0 {
			start = 0
		}
		end := index + 2
		if end >= len(lines) {
			end = len(lines) - 1
		}
		for lineIndex := start; lineIndex <= end; lineIndex++ {
			selected[lineIndex] = true
		}
	}
	if len(selected) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString(" Saved game hook source excerpt (edit these exact current bytes, not an unseen helper):\n")
	for index, line := range lines {
		if !selected[index] {
			continue
		}
		fmt.Fprintf(&out, "%d: %s\n", index+1, line)
		if out.Len() >= 1600 {
			break
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

// interactionHasDeterministicGameContract distinguishes a real machine-
// verifiable movement contract from a model-invented visual state assertion.
// When the call advances the application's own deterministic clock and later
// compares an exposed state path, a missing/broken hook is an artifact defect,
// not a reason to silently downgrade the validator to human review.
func interactionHasDeterministicGameContract(steps []interactionStep) bool {
	hasAdvance := false
	hasMovementComparison := false
	for _, step := range steps {
		switch step.Action {
		case "advance_time":
			hasAdvance = true
		case "assert_game_state":
			hasMovementComparison = hasMovementComparison || len(step.ChangedFromPrevious) > 0
		}
	}
	return hasAdvance && hasMovementComparison
}

func interactionBrowserDiagnostics(
	ctx context.Context,
	helper *MeadowBrowserVisualCheck,
	base string,
	localPath string,
	diagnosticCursor int,
	step int,
	action string,
	selector string,
) *Result {
	diagnostics, err := helper.browserDiagnostics(ctx, base)
	if err != nil {
		result := browserDiagnosticsUnavailableResult(err)
		return &result
	}
	diagnostics = diagnosticsSince(diagnostics, diagnosticCursor)
	if runtimeFailure := browserRuntimeFailure(diagnostics); runtimeFailure != "" {
		label := action
		if step > 0 {
			label = fmt.Sprintf("step %d (%s)", step, action)
		}
		result := browserRuntimeErrorResult(helper.WS, localPath, diagnostics, "INTERACTION_CHECK_FAILED after "+label)
		result.LLMContent += "\n" + interactionFailureEvidence(ctx, helper, base, localPath, step, action, selector, fmt.Errorf("%s", runtimeFailure))
		return &result
	}
	return nil
}

// interactionFailureEvidence gives the repair turn one bounded, concrete
// snapshot of what the browser actually saw. It never hides the original
// validator error when screenshot/DOM collection is itself unavailable.
func interactionFailureEvidence(
	ctx context.Context,
	helper *MeadowBrowserVisualCheck,
	base string,
	localPath string,
	step int,
	action string,
	selector string,
	cause error,
) string {
	var out strings.Builder
	fmt.Fprintf(&out, "browser_failure_evidence:\n- step: %d\n- action: %s\n- selector: %q\n- error: %v\n", step, action, selector, cause)

	shotPath, shotBytes, shotErr := helper.captureScreenshot(ctx, base, map[string]any{}, localPath)
	if shotErr != nil {
		fmt.Fprintf(&out, "- screenshot_error: %v\n", shotErr)
	} else {
		displayPath := shotPath
		if helper.WS != nil {
			displayPath = helper.WS.ModelPath(shotPath)
		}
		fmt.Fprintf(&out, "- human_review_screenshot_path: %s (%d bytes)\n", displayPath, shotBytes)
		fmt.Fprintln(&out, "- screenshot_policy: human review only; base repairs on the validator error and rendered DOM/game state")
	}

	var raw json.RawMessage
	if err := helper.postJSON(ctx, base+"/dom", map[string]any{"selector": "body", "max_depth": 8}, &raw); err != nil {
		fmt.Fprintf(&out, "- dom_error: %v", err)
		return strings.TrimSpace(out.String())
	}
	dom := strings.TrimSpace(interactionDOMSearchText(raw))
	if dom == "" {
		dom = strings.TrimSpace(string(raw))
	}
	const maxDOMChars = 4_000
	runes := []rune(dom)
	if len(runes) > maxDOMChars {
		dom = string(runes[:maxDOMChars]) + " …[truncated]"
	}
	fmt.Fprintf(&out, "- rendered_dom_excerpt: %s", interactionDOMExcerpt(dom))
	return strings.TrimSpace(out.String())
}

var simpleInteractionIDSelector = regexp.MustCompile(`^#([A-Za-z_][A-Za-z0-9_:\-.]*)$`)

// groundInteractionIDSelectors removes a common validator-only retry without
// weakening acceptance semantics. It only rewrites a simple #id selector when
// the saved HTML contains exactly one id with the same case-insensitive
// alphanumeric form (for example #submitButton -> #submit-button). Arbitrary fuzzy
// matches and ambiguous candidates remain untouched and fail normally.
func groundInteractionIDSelectors(localPath string, steps []interactionStep) ([]interactionStep, []string) {
	if strings.TrimSpace(localPath) == "" || len(steps) == 0 {
		return steps, nil
	}
	html, err := os.ReadFile(localPath)
	if err != nil {
		return steps, nil
	}
	exact := make(map[string]bool)
	byNormalized := make(map[string][]string)
	for _, match := range htmlIDAttribute.FindAllSubmatch(html, -1) {
		if len(match) < 2 {
			continue
		}
		id := strings.TrimSpace(string(match[1]))
		if id == "" || exact[id] {
			continue
		}
		exact[id] = true
		key := interactionIDAlias(id)
		if key != "" {
			byNormalized[key] = append(byNormalized[key], id)
		}
	}
	grounded := append([]interactionStep(nil), steps...)
	var corrections []string
	for index := range grounded {
		if grounded[index].State != nil {
			continue
		}
		match := simpleInteractionIDSelector.FindStringSubmatch(grounded[index].Selector)
		if len(match) < 2 || exact[match[1]] {
			continue
		}
		candidates := byNormalized[interactionIDAlias(match[1])]
		if len(candidates) != 1 {
			continue
		}
		before := grounded[index].Selector
		grounded[index].Selector = "#" + candidates[0]
		corrections = append(corrections, fmt.Sprintf("step %d %s -> %s", index+1, before, grounded[index].Selector))
	}
	return grounded, corrections
}

func normalizedInteractionID(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(id)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func interactionIDAlias(id string) string {
	switch normalizedInteractionID(id) {
	case "restart", "restartbtn", "reset", "resetbtn":
		return "restartresetbtn"
	default:
		return normalizedInteractionID(id)
	}
}

// interactionBindingDiagnostic correlates a failed rendered-DOM assertion
// with the action immediately before it. This covers a class of silent web
// failures that neither TypeScript nor the JavaScript console can report: a
// visible button exists but no code ever binds it. The diagnostic is evidence
// based (saved HTML + sibling TypeScript), not an invented repair.
func interactionBindingDiagnostic(localPath string, steps []interactionStep, failedIndex int) string {
	if strings.TrimSpace(localPath) == "" || failedIndex <= 0 || failedIndex >= len(steps) {
		return ""
	}
	var action *interactionStep
	for index := failedIndex - 1; index >= 0; index-- {
		if steps[index].Action == "click" || steps[index].Action == "fill" || steps[index].Action == "press_key" {
			candidate := steps[index]
			action = &candidate
			break
		}
	}
	if action == nil {
		return ""
	}
	prefix := fmt.Sprintf("The immediately preceding %s targeted %q, but the requested rendered state did not appear.", action.Action, action.Selector)
	if action.Action == "press_key" {
		prefix = fmt.Sprintf("The immediately preceding press_key dispatched key %q, but the requested game state did not appear.", action.Key)
		return prefix + " Inspect the saved keyboard listener, direction/state transition, and render_game_to_text() projection before changing the assertion."
	}
	match := simpleInteractionIDSelector.FindStringSubmatch(action.Selector)
	if len(match) < 2 {
		return prefix + " Inspect that selector's event binding and the handler's state-to-DOM render path before changing the assertion text."
	}
	id := match[1]
	sourcePath := strings.TrimSuffix(localPath, filepath.Ext(localPath)) + ".ts"
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		return prefix + " Inspect the saved source event binding and state-to-DOM render path before changing the assertion text."
	}
	sourceText := string(source)
	selectorReferenced := strings.Contains(sourceText, `"`+id+`"`) ||
		strings.Contains(sourceText, `'`+id+`'`) ||
		strings.Contains(sourceText, `"#`+id+`"`) ||
		strings.Contains(sourceText, `'#`+id+`'`)
	modelSource := "/session/" + filepath.Base(sourcePath)
	if !selectorReferenced {
		return fmt.Sprintf(
			"%s The saved sibling TypeScript %s never references control id %q, so there is no evidence this visible control is wired. Bind the existing element to its requested handler (for example with getElementById/querySelector plus addEventListener), update the rendered state in that handler, rebuild, and rerun the same interaction steps. Do not weaken or rename the acceptance assertion.",
			prefix, modelSource, id,
		)
	}
	if !strings.Contains(sourceText, "addEventListener") && !strings.Contains(sourceText, ".onclick") && !strings.Contains(sourceText, ".oninput") && !strings.Contains(sourceText, ".onchange") {
		return fmt.Sprintf(
			"%s The saved sibling TypeScript %s references id %q but contains no event-binding API. Bind that existing element to its requested handler, update rendered state, rebuild, and rerun the unchanged assertion.",
			prefix, modelSource, id,
		)
	}
	return prefix + " The control id is referenced in sibling TypeScript; inspect its specific listener and the handler's state-to-DOM render path before changing assertion text."
}

func availableHTMLIDDiagnostic(localPath string) string {
	if strings.TrimSpace(localPath) == "" {
		return ""
	}
	html, err := os.ReadFile(localPath)
	if err != nil {
		return ""
	}
	seen := make(map[string]bool)
	var selectors []string
	for _, match := range htmlIDAttribute.FindAllSubmatch(html, -1) {
		if len(match) < 2 {
			continue
		}
		id := strings.TrimSpace(string(match[1]))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		selectors = append(selectors, "#"+id)
		if len(selectors) >= 24 {
			break
		}
	}
	if len(selectors) == 0 {
		return ""
	}
	return " Available saved HTML id selectors: " + strings.Join(selectors, ", ") + "."
}

func localHTMLRuntimeDiagnostic(localPath string) string {
	if strings.TrimSpace(localPath) == "" {
		return ""
	}
	html, err := os.ReadFile(localPath)
	if err != nil {
		return "cannot read the local HTML: " + err.Error()
	}
	var issues []string
	for _, match := range regexp.MustCompile(`(?is)<script\b[^>]*\bsrc\s*=\s*["']([^"']+)["'][^>]*>`).FindAllSubmatch(html, -1) {
		if len(match) < 2 {
			continue
		}
		src := strings.TrimSpace(string(match[1]))
		withoutQuery := src
		if query := strings.IndexByte(withoutQuery, '?'); query >= 0 {
			withoutQuery = withoutQuery[:query]
		}
		if strings.HasPrefix(withoutQuery, "/session/") || strings.HasPrefix(withoutQuery, "/workspace/") {
			issues = append(issues, fmt.Sprintf("script src %q is a Meadow filesystem alias, not a browser URL; use the sibling filename %q", src, filepath.Base(withoutQuery)))
			continue
		}
		if strings.HasPrefix(withoutQuery, "/") || strings.Contains(withoutQuery, "://") {
			continue
		}
		resolved := filepath.Join(filepath.Dir(localPath), filepath.FromSlash(withoutQuery))
		if info, statErr := os.Stat(resolved); statErr != nil || info.IsDir() {
			issues = append(issues, fmt.Sprintf("script src %q does not resolve to a saved sibling file", src))
		}
	}
	return strings.Join(issues, "; ")
}

type interactionStep struct {
	State                 *interactionStateSpec
	Action                string
	Selector              string
	Text                  string
	Key                   string
	Code                  string
	Modifiers             []string
	Contains              []string
	ContainsAny           []string
	NotContains           []string
	ChangedFromPrevious   []string
	UnchangedFromPrevious []string
	MS                    int
}

type interactionCoverage struct {
	Requirement    string
	ActionSteps    []int
	AssertionSteps []int
}

// interactionCoverageRows validates only the model-authored evidence graph.
// It deliberately does not decide what the application should do: the model
// quotes the user's behavior and chooses the scenario, while Meadow checks
// that every claimed behavior names an executed action and a later assertion.
// This prevents a click-only smoke test from being counted as acceptance and
// keeps the contract reusable across games, forms, dashboards, and editors.
func interactionCoverageRows(raw any, steps []interactionStep) ([]interactionCoverage, error) {
	items, ok := raw.([]any)
	if !ok || len(items) < 1 || len(items) > meadowBrowserInteractionMaxSteps {
		return nil, fmt.Errorf("coverage must contain 1 to %d requirement rows", meadowBrowserInteractionMaxSteps)
	}
	rows := make([]interactionCoverage, 0, len(items))
	coveredActions := make(map[int]bool)
	seenRequirements := make(map[string]bool)
	for index, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("coverage row %d must be an object", index+1)
		}
		requirement := strings.TrimSpace(strArg(item, "requirement"))
		if requirement == "" {
			return nil, fmt.Errorf("coverage row %d requires an exact user requirement quote", index+1)
		}
		normalizedRequirement := strings.ToLower(strings.Join(strings.Fields(requirement), " "))
		if seenRequirements[normalizedRequirement] {
			return nil, fmt.Errorf("coverage row %d repeats requirement %q", index+1, requirement)
		}
		seenRequirements[normalizedRequirement] = true
		actionSteps, err := interactionStepNumbers(item["action_steps"], len(steps))
		if empty, ok := item["action_steps"].([]any); ok && len(empty) == 0 {
			actionSteps, err = nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("coverage row %d action_steps: %w", index+1, err)
		}
		assertionSteps, err := interactionStepNumbers(item["assertion_steps"], len(steps))
		if err != nil {
			return nil, fmt.Errorf("coverage row %d assertion_steps: %w", index+1, err)
		}
		latestAction := 0
		for _, stepNumber := range actionSteps {
			action := steps[stepNumber-1].Action
			if !interactionAction(action) {
				return nil, fmt.Errorf("coverage row %d step %d is %s, not an interaction action", index+1, stepNumber, action)
			}
			coveredActions[stepNumber] = true
			if stepNumber > latestAction {
				latestAction = stepNumber
			}
		}
		laterAssertion := false
		for _, stepNumber := range assertionSteps {
			action := steps[stepNumber-1].Action
			if !interactionAssertion(action) {
				return nil, fmt.Errorf("coverage row %d step %d is %s, not an assertion", index+1, stepNumber, action)
			}
			if len(actionSteps) == 0 {
				for before := 0; before < stepNumber; before++ {
					if interactionAction(steps[before].Action) {
						return nil, fmt.Errorf("coverage row %d initial assertion follows an action; bind the action explicitly", index+1)
					}
				}
			}
			if stepNumber > latestAction {
				laterAssertion = true
			}
		}
		if !laterAssertion {
			return nil, fmt.Errorf("coverage row %d has no assertion after its actions", index+1)
		}
		rows = append(rows, interactionCoverage{
			Requirement: requirement, ActionSteps: actionSteps, AssertionSteps: assertionSteps,
		})
	}
	for index, step := range steps {
		stepNumber := index + 1
		if interactionAction(step.Action) && !coveredActions[stepNumber] {
			return nil, fmt.Errorf("action step %d (%s) is not bound to a user requirement", stepNumber, step.Action)
		}
	}
	return rows, nil
}

func interactionStepNumbers(raw any, stepCount int) ([]int, error) {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil, fmt.Errorf("must be a non-empty integer array")
	}
	result := make([]int, 0, len(items))
	seen := make(map[int]bool)
	for _, item := range items {
		var value int
		switch typed := item.(type) {
		case int:
			value = typed
		case float64:
			if typed != float64(int(typed)) {
				return nil, fmt.Errorf("contains non-integer step %v", typed)
			}
			value = int(typed)
		default:
			return nil, fmt.Errorf("contains non-integer step %v", item)
		}
		if value < 1 || value > stepCount {
			return nil, fmt.Errorf("step %d is outside 1..%d", value, stepCount)
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result, nil
}

func interactionAction(action string) bool {
	switch action {
	case "fill", "click", "context_click", "press_key", "advance_time":
		return true
	default:
		return false
	}
}

func interactionAssertion(action string) bool {
	return action == "assert_dom" || action == "assert_game_state" || action == "assert_canvas_pixels" || action == "assert_state" || action == "assert_text"
}

func interactionSteps(raw any) ([]interactionStep, error) {
	return parseInteractionSteps(raw, true)
}

func parseInteractionSteps(raw any, requireAssertion bool) ([]interactionStep, error) {
	items, ok := raw.([]any)
	if !ok || len(items) < 1 || len(items) > meadowBrowserInteractionMaxSteps {
		return nil, fmt.Errorf("steps must contain 1 to %d ordered actions", meadowBrowserInteractionMaxSteps)
	}
	steps := make([]interactionStep, 0, len(items))
	hasAssertion := false
	for index, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("step %d must be an object", index+1)
		}
		step := interactionStep{
			Action: strings.TrimSpace(strArg(item, "action")), Selector: strings.TrimSpace(strArg(item, "selector")),
			Text: strArg(item, "text"), Key: strings.TrimSpace(strArg(item, "key")),
			Code: strings.TrimSpace(strArg(item, "code")), Modifiers: sliceArg(item, "modifiers"),
			Contains:    sliceArg(item, "contains"),
			ContainsAny: sliceArg(item, "contains_any"), NotContains: sliceArg(item, "not_contains"),
			ChangedFromPrevious:   sliceArg(item, "changed_from_previous"),
			UnchangedFromPrevious: sliceArg(item, "unchanged_from_previous"),
		}
		if value, ok := intArg(item, "ms"); ok {
			step.MS = value
		}
		// Keep execution defensive even when a caller bypasses Registry argument
		// normalization. KeyboardEvent.code already carries an exact, lossless
		// identity for these keys; deriving key here avoids turning a harmless
		// model wire shape such as key=" ", code="Space" into a recovery loop.
		if step.Action == "press_key" && step.Key == "" {
			step.Key = interactionKeyFromCode(step.Code, step.Modifiers)
		}
		switch step.Action {
		case "assert_text":
			for field := range item {
				if field != "action" && field != "selector" && field != "value" && field != "expected_text" {
					return nil, fmt.Errorf("step %d assert_text accepts only selector/value; use assert_state for other comparisons", index+1)
				}
			}
			expected, _, expectedErr := interactionExpectedValue(item)
			if expectedErr != nil {
				return nil, fmt.Errorf("step %d: %w", index+1, expectedErr)
			}
			value, ok := expected.(string)
			if !ok {
				return nil, fmt.Errorf("step %d assert_text requires a string value", index+1)
			}
			var stateErr error
			step.State, stateErr = parseInteractionState(map[string]any{"source": "dom", "selector": step.Selector, "path": "text", "operator": "eq", "value": value}, "assert_state")
			if stateErr != nil {
				return nil, fmt.Errorf("step %d: %w", index+1, stateErr)
			}
			step.Action, hasAssertion = "assert_state", true
		case "observe_state", "assert_state":
			var stateErr error
			step.State, stateErr = parseInteractionState(item, step.Action)
			if stateErr != nil {
				return nil, fmt.Errorf("step %d: %w", index+1, stateErr)
			}
			hasAssertion = hasAssertion || step.Action == "assert_state"
		case "fill":
			if step.Selector == "" {
				return nil, fmt.Errorf("step %d fill requires selector", index+1)
			}
		case "click", "context_click":
			if step.Selector == "" {
				return nil, fmt.Errorf("step %d %s requires selector", index+1, step.Action)
			}
		case "press_key":
			if step.Key == "" {
				return nil, fmt.Errorf("step %d press_key requires key", index+1)
			}
			if !supportedInteractionKey(step.Key) {
				return nil, fmt.Errorf("step %d press_key key %q is unsupported", index+1, step.Key)
			}
		case "advance_time":
			if step.MS < 1 || step.MS > meadowBrowserInteractionMaxSettleMS {
				return nil, fmt.Errorf("step %d advance_time requires ms between 1 and 10000", index+1)
			}
		case "assert_dom", "assert_game_state":
			hasAssertion = true
			if len(step.Contains)+len(step.ContainsAny)+len(step.NotContains)+len(step.ChangedFromPrevious)+len(step.UnchangedFromPrevious) == 0 {
				return nil, fmt.Errorf("step %d %s requires contains, contains_any, not_contains, changed_from_previous, or unchanged_from_previous", index+1, step.Action)
			}
			if step.Action != "assert_game_state" && (len(step.ChangedFromPrevious) > 0 || len(step.UnchangedFromPrevious) > 0) {
				return nil, fmt.Errorf("step %d state comparison is only valid for assert_game_state", index+1)
			}
		case "assert_canvas_pixels":
			hasAssertion = true
		default:
			return nil, fmt.Errorf("step %d has unsupported action %q", index+1, step.Action)
		}
		steps = append(steps, step)
	}
	if requireAssertion && !hasAssertion {
		return nil, fmt.Errorf("steps require at least one assertion; observations and inputs alone do not verify behavior")
	}
	return steps, nil
}

func contextClickInteraction(
	ctx context.Context,
	helper *MeadowBrowserVisualCheck,
	base string,
	selector string,
) error {
	encodedSelector, _ := json.Marshal(selector)
	code := `(function(){const el=document.querySelector(` + string(encodedSelector) + `);` +
		`if(!el)return {__meadow_missing:true,selector:` + string(encodedSelector) + `};` +
		`const event=new MouseEvent("contextmenu",{bubbles:true,cancelable:true,button:2,buttons:2,view:window});` +
		`return {__meadow_missing:false,dispatched:el.dispatchEvent(event)};})()`
	var raw json.RawMessage
	if err := helper.postJSON(ctx, base+"/js", map[string]any{"code": code}, &raw); err != nil {
		return fmt.Errorf("dispatch context click: %w", err)
	}
	wirePayload := string(raw)
	if strings.Contains(wirePayload, `__meadow_missing\":true`) || strings.Contains(wirePayload, `"__meadow_missing":true`) {
		return fmt.Errorf("selector not found for context click: %q", selector)
	}
	return nil
}

func supportedInteractionKey(key string) bool {
	if len([]rune(key)) == 1 {
		return true
	}
	switch key {
	case "Enter", "Return", "Tab", "Escape", "Esc", "Backspace", "Delete",
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", " ", "Space":
		return true
	default:
		return false
	}
}

func advanceInteractionGameTime(ctx context.Context, helper *MeadowBrowserVisualCheck, base string, milliseconds int) error {
	code := fmt.Sprintf(`(function(){const fn=window.advanceTime;if(typeof fn!=="function")return {__meadow_missing_advance_time_hook:true};try{fn(%d);return {__meadow_missing_advance_time_hook:false,advanced_ms:%d};}catch(error){return {__meadow_missing_advance_time_hook:false,__meadow_advance_time_error:String(error&&error.message||error)};}})()`, milliseconds, milliseconds)
	var raw json.RawMessage
	if err := helper.postJSON(ctx, base+"/js", map[string]any{"code": code}, &raw); err != nil {
		return fmt.Errorf("advance deterministic game time: %w", err)
	}
	payload := interactionDOMSearchText(raw)
	compact := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(string(raw) + payload)
	if strings.Contains(compact, `"__meadow_missing_advance_time_hook":true`) || strings.Contains(compact, `\"__meadow_missing_advance_time_hook\":true`) {
		return fmt.Errorf("window.advanceTime(ms) is missing; add the deterministic time hook so movement can be proven without relying on real-time animation timing")
	}
	if strings.Contains(payload, "__meadow_advance_time_error") {
		return fmt.Errorf("window.advanceTime(%d) failed: %s", milliseconds, interactionDOMExcerpt(payload))
	}
	return nil
}

func assertInteractionCanvasPixels(
	ctx context.Context,
	helper *MeadowBrowserVisualCheck,
	base string,
	selector string,
) error {
	if strings.TrimSpace(selector) == "" {
		selector = "canvas"
	}
	encodedSelector, _ := json.Marshal(selector)
	code := `(function(){const canvas=document.querySelector(` + string(encodedSelector) + `);` +
		`if(!(canvas instanceof HTMLCanvasElement))return {__meadow_canvas_missing:true};` +
		`const context=canvas.getContext("2d");if(!context)return {__meadow_canvas_context_missing:true};` +
		`try{const data=context.getImageData(0,0,canvas.width,canvas.height).data;let painted=0;` +
		`for(let i=3;i<data.length;i+=4){if(data[i]!==0)painted++;}` +
		`return {__meadow_canvas_missing:false,painted_pixels:painted,width:canvas.width,height:canvas.height};}` +
		`catch(error){return {__meadow_canvas_missing:false,__meadow_canvas_error:String(error&&error.message||error)};}})()`
	var raw json.RawMessage
	if err := helper.postJSON(ctx, base+"/js", map[string]any{"code": code}, &raw); err != nil {
		return fmt.Errorf("inspect canvas pixels: %w", err)
	}
	payload := interactionDOMSearchText(raw)
	compact := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(string(raw) + payload)
	if strings.Contains(compact, `"__meadow_canvas_missing":true`) || strings.Contains(compact, `\"__meadow_canvas_missing\":true`) {
		return fmt.Errorf("canvas selector %q was not found", selector)
	}
	if strings.Contains(compact, `"__meadow_canvas_context_missing":true`) || strings.Contains(compact, `\"__meadow_canvas_context_missing\":true`) {
		return fmt.Errorf("canvas selector %q does not expose a 2D drawing context", selector)
	}
	if strings.Contains(payload, "__meadow_canvas_error") {
		return fmt.Errorf("canvas pixel inspection failed: %s", interactionDOMExcerpt(payload))
	}
	painted, ok := interactionNumericField(raw, "painted_pixels")
	if !ok || painted < 8 {
		return fmt.Errorf("canvas selector %q has no meaningful painted pixels; initialize and draw the game before acceptance (observed painted_pixels=%d)", selector, painted)
	}
	return nil
}

func interactionNumericField(raw json.RawMessage, field string) (int, bool) {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return 0, false
	}
	var find func(any, int) (int, bool)
	find = func(value any, depth int) (int, bool) {
		if depth > 8 {
			return 0, false
		}
		switch typed := value.(type) {
		case map[string]any:
			if candidate, exists := typed[field]; exists {
				switch number := candidate.(type) {
				case float64:
					return int(number), true
				case json.Number:
					parsed, err := strconv.Atoi(number.String())
					return parsed, err == nil
				}
			}
			for _, child := range typed {
				if found, ok := find(child, depth+1); ok {
					return found, true
				}
			}
		case []any:
			for _, child := range typed {
				if found, ok := find(child, depth+1); ok {
					return found, true
				}
			}
		case string:
			var nested any
			if json.Unmarshal([]byte(typed), &nested) == nil {
				return find(nested, depth+1)
			}
		}
		return 0, false
	}
	return find(root, 0)
}

func assertInteractionGameState(ctx context.Context, helper *MeadowBrowserVisualCheck, base string, step interactionStep, previous any) (any, error) {
	code := `(function(){const fn=window.render_game_to_text;` +
		`if(typeof fn!=="function")return {__meadow_missing_game_state_hook:true};` +
		`try{const value=fn();return {__meadow_missing_game_state_hook:false,value:typeof value==="string"?value:JSON.stringify(value)};}` +
		`catch(error){return {__meadow_missing_game_state_hook:false,__meadow_game_state_error:String(error&&error.message||error)};}})()`
	var raw json.RawMessage
	if err := helper.postJSON(ctx, base+"/js", map[string]any{"code": code}, &raw); err != nil {
		return nil, fmt.Errorf("capture game state: %w", err)
	}
	wirePayload := string(raw)
	payload := interactionDOMSearchText(raw)
	if gameStateHookFlagIsTrue(wirePayload) || gameStateHookFlagIsTrue(payload) {
		return nil, fmt.Errorf("window.render_game_to_text() is missing; add a read-only compact state hook in the saved TypeScript so keyboard/canvas behavior can be verified without pixel guessing")
	}
	if strings.Contains(payload, "__meadow_game_state_error") {
		return nil, fmt.Errorf("window.render_game_to_text() failed: %s", interactionDOMExcerpt(payload))
	}
	if err := assertInteractionText(payload, "game state", step); err != nil {
		return nil, err
	}
	current, ok := interactionGameStateValue(raw)
	if len(step.ChangedFromPrevious) > 0 {
		if !ok {
			return nil, fmt.Errorf("render_game_to_text() did not return a JSON object or array; movement paths %q cannot be compared", step.ChangedFromPrevious)
		}
		if previous == nil {
			return nil, fmt.Errorf("changed_from_previous requires an earlier assert_game_state observation")
		}
		changed, observed := false, make([]string, 0, len(step.ChangedFromPrevious))
		for _, path := range step.ChangedFromPrevious {
			before, beforeOK := interactionGameStatePath(previous, path)
			after, afterOK := interactionGameStatePath(current, path)
			if !beforeOK || !afterOK {
				continue
			}
			observed = append(observed, path)
			if !reflect.DeepEqual(before, after) {
				changed = true
			}
		}
		if len(observed) == 0 {
			return nil, fmt.Errorf("game state exposes none of requested comparison paths %q; expose those paths in render_game_to_text()", step.ChangedFromPrevious)
		}
		if !changed {
			return nil, fmt.Errorf("game state movement paths %q did not change after the requested input and advanceTime; observed game state: %s", observed, interactionDOMExcerpt(payload))
		}
	}
	if len(step.UnchangedFromPrevious) > 0 {
		if !ok {
			return nil, fmt.Errorf("render_game_to_text() did not return a JSON object or array; paused-state paths %q cannot be compared", step.UnchangedFromPrevious)
		}
		if previous == nil {
			return nil, fmt.Errorf("unchanged_from_previous requires an earlier assert_game_state observation")
		}
		missing := make([]string, 0)
		changed := make([]string, 0)
		for _, path := range step.UnchangedFromPrevious {
			before, beforeOK := interactionGameStatePath(previous, path)
			after, afterOK := interactionGameStatePath(current, path)
			if !beforeOK || !afterOK {
				missing = append(missing, path)
				continue
			}
			if !reflect.DeepEqual(before, after) {
				changed = append(changed, path)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("game state exposes none or not all paused-state paths %q; include those paths in render_game_to_text()", missing)
		}
		if len(changed) > 0 {
			return nil, fmt.Errorf("game state paths %q changed while the game should remain paused; observed game state: %s", changed, interactionDOMExcerpt(payload))
		}
	}
	if !ok {
		return payload, nil
	}
	return current, nil
}

func interactionGameStateValue(raw json.RawMessage) (any, bool) {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return nil, false
	}
	var find func(any, int) (any, bool)
	find = func(value any, depth int) (any, bool) {
		if depth > 8 {
			return nil, false
		}
		switch typed := value.(type) {
		case string:
			var nested any
			if json.Unmarshal([]byte(typed), &nested) == nil {
				switch nested.(type) {
				case map[string]any, []any:
					// The native Meadow Browser wire format can double-wrap a
					// JavaScript string as {"result":"{\"v\":\"{...}\"}"}.
					// Recurse through that transport envelope before accepting the
					// decoded map as the game state itself. Otherwise comparisons look
					// for requested paths on the intermediate {v: ...} object and reject
					// an artifact whose reported state actually changed.
					if found, ok := find(nested, depth+1); ok {
						return found, true
					}
					return nested, true
				}
			}
		case map[string]any:
			for _, key := range []string{"value", "v", "result"} {
				if child, exists := typed[key]; exists {
					if found, ok := find(child, depth+1); ok {
						return found, true
					}
				}
			}
		case []any:
			for _, child := range typed {
				if found, ok := find(child, depth+1); ok {
					return found, true
				}
			}
		}
		return nil, false
	}
	return find(root, 0)
}

func interactionGameStatePath(root any, path string) (any, bool) {
	current := root
	for _, segment := range strings.Split(strings.TrimSpace(path), ".") {
		if segment == "" {
			return nil, false
		}
		switch typed := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = typed[segment]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, false
			}
			current = typed[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func gameStateHookFlagIsTrue(payload string) bool {
	compact := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(payload)
	return strings.Contains(compact, `"__meadow_missing_game_state_hook":true`) ||
		strings.Contains(compact, `\"__meadow_missing_game_state_hook\":true`)
}

func assertInteractionDOM(ctx context.Context, helper *MeadowBrowserVisualCheck, base string, step interactionStep) error {
	selector := "body"
	if step.Selector != "" {
		selector = step.Selector
	}
	encodedSelector, _ := json.Marshal(selector)
	code := `(function(){const el=document.querySelector(` + string(encodedSelector) + `);` +
		`if(!el)return {__meadow_missing:true,selector:` + string(encodedSelector) + `};` +
		`return {__meadow_missing:false,text:(typeof el.innerText==="string"?el.innerText:(el.textContent||""))};})()`
	var raw json.RawMessage
	if err := helper.postJSON(ctx, base+"/js", map[string]any{"code": code}, &raw); err != nil {
		return fmt.Errorf("capture rendered DOM: %w", err)
	}
	wirePayload := string(raw)
	if strings.Contains(wirePayload, `__meadow_missing\":true`) || strings.Contains(wirePayload, `"__meadow_missing":true`) {
		return fmt.Errorf("rendered DOM selector %q was not found", selector)
	}
	payload := interactionDOMSearchText(raw)
	return assertInteractionText(payload, "rendered DOM", step)
}

func assertInteractionText(payload string, label string, step interactionStep) error {
	for _, expected := range step.Contains {
		if !interactionTextContains(payload, expected) {
			return fmt.Errorf("%s does not contain %q; observed %s: %s", label, expected, label, interactionDOMExcerpt(payload))
		}
	}
	if len(step.ContainsAny) > 0 {
		matched := false
		for _, expected := range step.ContainsAny {
			if interactionTextContains(payload, expected) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s contains none of %q; observed %s: %s", label, step.ContainsAny, label, interactionDOMExcerpt(payload))
		}
	}
	for _, forbidden := range step.NotContains {
		if interactionTextContains(payload, forbidden) {
			return fmt.Errorf("%s still contains forbidden text %q; observed %s: %s", label, forbidden, label, interactionDOMExcerpt(payload))
		}
	}
	return nil
}

// renderedInteractionControlDiagnostic captures the controls that exist after
// the last successful interaction. Static source ids are insufficient for
// applications that rerender buttons, radio options, or inputs between steps.
// Returning observed tag/selector/type/value/text facts lets the model correct
// its acceptance call without guessing or rewriting a valid application.
func renderedInteractionControlDiagnostic(
	ctx context.Context,
	helper *MeadowBrowserVisualCheck,
	base string,
) string {
	code := `(function(){return Array.from(document.querySelectorAll('button,input,select,textarea,a[href],[role="button"]')).slice(0,24).map(function(el,index){return {index:index+1,tag:el.tagName.toLowerCase(),id:el.id||'',type:el.getAttribute('type')||'',name:el.getAttribute('name')||'',value:el.getAttribute('value')||'',text:(el.innerText||el.getAttribute('aria-label')||'').trim().slice(0,80)};});})()`
	var raw json.RawMessage
	if err := helper.postJSON(ctx, base+"/js", map[string]any{"code": code}, &raw); err != nil {
		return ""
	}
	observed := strings.TrimSpace(interactionDOMSearchText(raw))
	if observed == "" {
		return ""
	}
	if len(observed) > 1800 {
		observed = observed[:1800]
	}
	return " Current rendered interactive controls: " + observed + "."
}

// interactionDOMSearchText decodes the nested response envelopes returned by
// Meadow Browser before matching user-visible text. Depending on the browser
// transport, /js may return the evaluated value directly or as a JSON string
// inside one or more result/v envelopes. Matching the wire representation
// directly makes ordinary characters such as "/" appear as "\\/" and can
// incorrectly reject a page whose rendered DOM already satisfies the test.
//
// This only removes transport encoding. It does not normalize wording,
// whitespace, punctuation, or application values, so semantic assertions stay
// strict.
func interactionDOMSearchText(raw json.RawMessage) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	parts := make([]string, 0, 4)
	collectInteractionDOMText(value, 0, &parts)
	if len(parts) == 0 {
		return string(raw)
	}
	return strings.Join(parts, "\n")
}

func collectInteractionDOMText(value any, depth int, parts *[]string) {
	if depth > 6 {
		return
	}
	switch typed := value.(type) {
	case string:
		*parts = append(*parts, typed)
		var nested any
		if json.Valid([]byte(typed)) && json.Unmarshal([]byte(typed), &nested) == nil {
			collectInteractionDOMText(nested, depth+1, parts)
		}
	case map[string]any:
		// Prefer fields that carry evaluated DOM output. Fall back to every
		// value for forward-compatible Browser response envelopes.
		matched := false
		for _, key := range []string{"text", "html", "result", "v", "value"} {
			if child, ok := typed[key]; ok {
				collectInteractionDOMText(child, depth+1, parts)
				matched = true
			}
		}
		if !matched {
			for _, child := range typed {
				collectInteractionDOMText(child, depth+1, parts)
			}
		}
	case []any:
		for _, child := range typed {
			collectInteractionDOMText(child, depth+1, parts)
		}
	}
}

// interactionTextContains keeps exact text semantics while accepting two
// presentation-only equivalences: localized numeric grouping, isolated digit
// token spacing, and the full-width/half-width forms of common punctuation. A
// user requirement such as "10000" is satisfied by "10,000 元", a model tool
// argument such as "8 4 0" is satisfied by rendered "840", and "總額: 100" is
// satisfied by "總額：100". Words, values, signs, and their order still have
// to match.
// This prevents the acceptance harness from asking a model to rewrite correct
// application logic merely to change locale typography.
func interactionTextContains(observed, expected string) bool {
	if strings.Contains(observed, expected) {
		return true
	}
	return strings.Contains(
		normalizeInteractionPresentation(observed),
		normalizeInteractionPresentation(expected),
	)
}

func normalizeInteractionPresentation(value string) string {
	replacer := strings.NewReplacer(
		"：", ":", "；", ";", "（", "(", "）", ")", "／", "/",
	)
	return normalizePunctuationSpacing(normalizeSpacedDigitTokens(normalizeNumericGrouping(replacer.Replace(value))))
}

// normalizeSpacedDigitTokens joins only runs of at least three whitespace-
// separated single digits. Dense local models occasionally serialize one
// amount as "8 4 0" inside a native validator argument. Requiring three or
// more single-digit chunks avoids conflating ordinary pairs such as "1 2" or
// multi-digit values such as "10 20", which can represent distinct values.
func normalizeSpacedDigitTokens(value string) string {
	fields := strings.Fields(value)
	if len(fields) < 3 {
		return value
	}
	var normalized strings.Builder
	for index := 0; index < len(fields); {
		end := index
		for end < len(fields) {
			runes := []rune(fields[end])
			if len(runes) != 1 || !unicode.IsDigit(runes[0]) {
				break
			}
			end++
		}
		if end-index >= 3 {
			if normalized.Len() > 0 {
				normalized.WriteRune(' ')
			}
			for cursor := index; cursor < end; cursor++ {
				normalized.WriteString(fields[cursor])
			}
			index = end
			continue
		}
		if normalized.Len() > 0 {
			normalized.WriteRune(' ')
		}
		normalized.WriteString(fields[index])
		index++
	}
	return normalized.String()
}

func normalizePunctuationSpacing(value string) string {
	runes := []rune(value)
	var normalized strings.Builder
	normalized.Grow(len(value))
	for index, current := range runes {
		if unicode.IsSpace(current) {
			previousPunctuation := index > 0 && strings.ContainsRune(":;/()", runes[index-1])
			nextPunctuation := index+1 < len(runes) && strings.ContainsRune(":;/()", runes[index+1])
			if previousPunctuation || nextPunctuation {
				continue
			}
		}
		normalized.WriteRune(current)
	}
	return normalized.String()
}

func normalizeNumericGrouping(value string) string {
	runes := []rune(value)
	var normalized strings.Builder
	normalized.Grow(len(value))
	for index, current := range runes {
		if (current == ',' || current == '，' || current == '_') &&
			index > 0 && index+1 < len(runes) &&
			unicode.IsDigit(runes[index-1]) && unicode.IsDigit(runes[index+1]) {
			continue
		}
		normalized.WriteRune(current)
	}
	return normalized.String()
}

func interactionDOMExcerpt(payload string) string {
	payload = strings.Join(strings.Fields(strings.TrimSpace(payload)), " ")
	const maxRunes = 600
	runes := []rune(payload)
	if len(runes) > maxRunes {
		payload = string(runes[:maxRunes-1]) + "…"
	}
	return payload
}
