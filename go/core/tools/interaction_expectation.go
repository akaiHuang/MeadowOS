package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Typed wire fields are alternatives, never guesses/coercions of expected data.
func interactionExpectedValue(item map[string]any) (any, bool, error) {
	value, present := item["value"]
	for _, key := range []string{"expected_number", "expected_text", "expected_boolean", "expected_null"} {
		v, ok := item[key]
		if !ok {
			continue
		}
		if present {
			return nil, false, fmt.Errorf("choose exactly one expected field or legacy value; conflicting %s", key)
		}
		present = true
		switch key {
		case "expected_text":
			if _, ok := v.(string); !ok {
				return nil, false, fmt.Errorf("expected_text must be a JSON string")
			}
		case "expected_boolean":
			if _, ok := v.(bool); !ok {
				return nil, false, fmt.Errorf("expected_boolean must be a JSON boolean")
			}
		case "expected_null":
			if v != true {
				return nil, false, fmt.Errorf("expected_null must be true; use expected_boolean for false")
			}
			v = nil
		case "expected_number":
			data, err := json.Marshal(v)
			var n any
			if err != nil || json.Unmarshal(data, &n) != nil {
				return nil, false, fmt.Errorf("expected_number must be finite")
			}
			if _, ok := n.(float64); !ok {
				return nil, false, fmt.Errorf("expected_number must be a JSON number, not a string")
			}
			v = n
		}
		value = v
	}
	return value, present, nil
}

// One model-authored criterion owns exactly the submitted scenario. Only the
// numeric graph is generated; validation still requires post-action assertions.
const interactionMissingRequirement = "缺少 requirement：請補上描述本次驗收條件的單一字串，保留原 steps、動作與預期值即可重試；不需要建立 coverage 索引。不要修改程式來修正這個工具參數問題"

func interactionCallCoverage(args map[string]any, steps []interactionStep) (any, error) {
	raw, present := args["requirement"]
	if !present {
		if _, legacyPresent := args["coverage"]; !legacyPresent {
			return nil, fmt.Errorf("%s", interactionMissingRequirement)
		}
		return args["coverage"], nil
	}
	if _, conflict := args["coverage"]; conflict {
		return nil, fmt.Errorf("use requirement OR coverage, not both")
	}
	requirement, ok := raw.(string)
	if !ok || strings.TrimSpace(requirement) == "" || len(requirement) > 4096 {
		return nil, fmt.Errorf("requirement must contain 1..4096 bytes")
	}
	actions, assertions := []any{}, []any{}
	for i, step := range steps {
		if interactionAction(step.Action) {
			actions = append(actions, i+1)
		}
		if interactionAssertion(step.Action) {
			assertions = append(assertions, i+1)
		}
	}
	return []any{map[string]any{"requirement": requirement, "action_steps": actions, "assertion_steps": assertions}}, nil
}
