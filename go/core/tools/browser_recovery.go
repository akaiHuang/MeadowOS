package tools

import (
	"fmt"
	"strings"
)

const interactionStateExamples = `Action examples (syntax only; choose selectors/values from your own requirements and observed source): {"action":"assert_text","selector":"#total","value":"0"}; {"action":"observe_state","source":"dom","selector":"#total","path":"text"}; {"action":"assert_state","source":"dom","selector":"#total","path":"text","operator":"eq","value":"0"}; {"action":"assert_state","source":"app","path":"player.x","operator":"changed"}. changed/unchanged require an earlier observation of the same source/selector/path and omit value. app requires an actual read-only meadowTest.getState() projection. path is a property, NOT a selector or prose.`

func interactionInvalidCall(problem string) Result {
	requiredArgs := map[string]any{"steps_executed": 0, "examples": interactionStateExamples, "source_edit_required": false}
	if problem == interactionMissingRequirement {
		requiredArgs = map[string]any{"steps_executed": 0, "source_edit_required": false,
			"required_parameter": "requirement", "repair_contract": interactionMissingRequirement}
	}
	return errResult(&RecoveryError{
		Code: "browser_check_not_executed", Message: "Browser check NOT EXECUTED; steps_executed=0. " + problem,
		RequiredAction: "correct_validator_arguments_preserve_requirements_and_retry",
		RequiredArgs:   requiredArgs,
		Diagnostics:    []map[string]any{{"kind": "argument_validation", "message": problem}}, PreserveDraft: true,
	})
}

// Describe the submitted plan, never manufacture a requirement-to-step mapping.
// Invalid references remain invalid and no browser action has been executed.
func interactionCoverageInvalidCall(problem string, raw any, steps []interactionStep) Result {
	catalog := make([]map[string]any, 0, len(steps))
	actions, assertions := []int{}, []int{}
	for i, step := range steps {
		item := map[string]any{"step": i + 1, "action": step.Action}
		if step.Selector != "" {
			item["selector"] = step.Selector
		}
		if step.Key != "" {
			item["key"] = step.Key
		}
		if step.State != nil {
			item["source"], item["path"] = step.State.Source, step.State.Path
		}
		catalog = append(catalog, item)
		if interactionAction(step.Action) {
			actions = append(actions, i+1)
		}
		if interactionAssertion(step.Action) {
			assertions = append(assertions, i+1)
		}
	}
	invalid := []string{}
	truncated := false
	rows, _ := raw.([]any)
	if len(rows) > meadowBrowserInteractionMaxSteps {
		rows = rows[:meadowBrowserInteractionMaxSteps]
		truncated = true
	}
	for i, row := range rows {
		item, _ := row.(map[string]any)
		for _, field := range []string{"action_steps", "assertion_steps"} {
			refs, _ := item[field].([]any)
			if len(refs) > meadowBrowserInteractionMaxSteps {
				refs = refs[:meadowBrowserInteractionMaxSteps]
				truncated = true
			}
			for _, ref := range refs {
				if _, err := interactionStepNumbers([]any{ref}, len(steps)); err != nil {
					if len(invalid) < 40 {
						invalid = append(invalid, fmt.Sprintf("coverage row %d %s: %v", i+1, field, err))
					} else {
						truncated = true
					}
				}
			}
		}
	}
	message := "INTERACTION_COVERAGE_INVALID: " + problem
	return errResult(&RecoveryError{
		Code: "browser_check_not_executed", Message: "Browser check NOT EXECUTED; steps_executed=0. " + message,
		RequiredAction: "correct_validator_arguments_preserve_requirements_and_retry",
		RequiredArgs: map[string]any{
			"steps_executed": 0, "source_edit_required": false, "step_count": len(steps),
			"submitted_step_catalog": catalog, "valid_action_steps": actions, "valid_assertion_steps": assertions,
			"invalid_references": invalid, "invalid_references_truncated": truncated,
			"repair_contract": "Use only the submitted 1-based step numbers; choose which actions and later assertions establish each original criterion. An initial assertion before any action uses action_steps:[] and its actual assertion_steps. observe_state is not an assertion. Preserve requirements and expected outcomes; do not drop rows or invent missing actions to make the graph pass. A saved meadow_browser_suite scenario links requirement_ids to its own steps without numeric coverage indices.",
		}, Diagnostics: []map[string]any{{"kind": "argument_validation", "message": message}}, PreserveDraft: true,
	})
}

// Collect independent state-field failures before any browser work. This does
// not normalize selectors, infer source/value, or change assertion semantics.
// The authoritative parser still enforces limits, ordering and scalar types.
func interactionStateFieldProblems(raw any) []string {
	items, ok := raw.([]any)
	if !ok || len(items) > meadowBrowserInteractionMaxSteps {
		return nil
	}
	var problems []string
	for i, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		action := strArg(item, "action")
		if action != "observe_state" && action != "assert_state" {
			continue
		}
		add := func(field, expected string) {
			value := fmt.Sprint(item[field])
			if len(value) > 100 {
				value = value[:100] + "…"
			}
			problems = append(problems, fmt.Sprintf("step %d %s: %s received %q; %s", i+1, action, field, value, expected))
		}
		source, path, op := strArg(item, "source"), strArg(item, "path"), strArg(item, "operator")
		if source != "dom" && source != "app" {
			add("source", "required: dom or app (separate from path)")
		}
		if path == "" {
			add("path", "required property: DOM text/value/checked/count/visible/disabled, or app dotted own-property path")
		}
		if source == "dom" {
			if strings.TrimSpace(strArg(item, "selector")) == "" {
				add("selector", "required observed CSS selector for dom")
			}
			if path != "" {
				switch path {
				case "text", "value", "checked", "count", "visible", "disabled":
				default:
					add("path", "dom path must be text/value/checked/count/visible/disabled; CSS belongs in selector")
				}
			}
		}
		if source == "app" && strArg(item, "selector") != "" {
			add("selector", "omit for app source")
		}
		_, valuePresent, expectedErr := interactionExpectedValue(item)
		if expectedErr != nil {
			problems = append(problems, fmt.Sprintf("step %d: %v", i+1, expectedErr))
			continue
		}
		if action == "observe_state" {
			if op != "" {
				add("operator", "omit for observation")
			}
			if valuePresent {
				add("value", "omit for observation")
			}
		} else {
			switch op {
			case "changed", "unchanged", "unique":
				if valuePresent {
					add("value", "omit; compares earlier observation")
				}
			case "eq", "ne", "gt", "gte", "lt", "lte", "rectangular", "all_eq", "all_gte", "all_lte":
				if !valuePresent {
					add("value", "required JSON scalar, explicit null allowed")
				}
			default:
				add("operator", "required: eq/ne/gt/gte/lt/lte/changed/unchanged/rectangular/unique/all_eq/all_gte/all_lte")
			}
		}
	}
	return problems
}
