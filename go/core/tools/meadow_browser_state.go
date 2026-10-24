package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// A state step reads one scalar, never evaluates model-supplied JavaScript.
// App hooks remain page-reported evidence, not an independent visual oracle.
type interactionStateSpec struct {
	Source   string `json:"source"`
	Selector string `json:"selector,omitempty"`
	Path     string `json:"path"`
	Operator string `json:"operator,omitempty"`
	Value    any    `json:"value"`
}

func (s interactionStateSpec) key() string {
	return s.Source + "\x00" + s.Selector + "\x00" + s.Path
}

func validateInteractionStateOrder(steps []interactionStep) error {
	seen := make(map[string]bool)
	for index, step := range steps {
		if step.State == nil {
			continue
		}
		if (step.State.Operator == "changed" || step.State.Operator == "unchanged") && !seen[step.State.key()] {
			return fmt.Errorf("step %d %s requires an earlier observation of the same source/selector/path", index+1, step.State.Operator)
		}
		seen[step.State.key()] = true
	}
	return nil
}

func interactionExecutedCoverage(rows []interactionCoverage, steps []map[string]any) []map[string]any {
	report := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		status := "passed"
		for _, number := range append(append([]int(nil), row.ActionSteps...), row.AssertionSteps...) {
			if number > len(steps) || steps[number-1]["status"] != "passed" {
				status = "not_verified"
				break
			}
		}
		report = append(report, map[string]any{"requirement": row.Requirement, "action_steps": row.ActionSteps, "assertion_steps": row.AssertionSteps, "status": status})
	}
	return report
}

func parseInteractionState(item map[string]any, action string) (*interactionStateSpec, error) {
	s := &interactionStateSpec{Source: strArg(item, "source"), Selector: strArg(item, "selector"), Path: strArg(item, "path"), Operator: strArg(item, "operator")}
	if s.Source != "dom" && s.Source != "app" {
		return nil, fmt.Errorf("source must be dom or app")
	}
	if s.Path == "" || len(s.Path) > 256 || len(s.Selector) > 512 {
		return nil, fmt.Errorf("path is required (max 256 bytes); selector max 512 bytes")
	}
	if s.Source == "dom" {
		if strings.TrimSpace(s.Selector) == "" {
			return nil, fmt.Errorf("dom source requires an observed selector")
		}
		switch s.Path {
		case "text", "value", "checked", "count", "visible", "disabled":
		default:
			return nil, fmt.Errorf("dom path must be text/value/checked/count/visible/disabled")
		}
	} else {
		if s.Selector != "" {
			return nil, fmt.Errorf("app source does not accept selector")
		}
		for _, part := range strings.Split(s.Path, ".") {
			if part == "" || part == "__proto__" || part == "prototype" || part == "constructor" {
				return nil, fmt.Errorf("invalid own-property state path")
			}
		}
	}
	value, present, expectedErr := interactionExpectedValue(item)
	if expectedErr != nil {
		return nil, expectedErr
	}
	if action == "observe_state" {
		if s.Operator != "" || present {
			return nil, fmt.Errorf("observe_state records a baseline, not an assertion; omit operator/value")
		}
		return s, nil
	}
	switch s.Operator {
	case "unique":
		if present || s.Source != "app" {
			return nil, fmt.Errorf("unique requires app array and no value")
		}
	case "changed", "unchanged":
		if present {
			return nil, fmt.Errorf("changed/unchanged compare the prior observation; omit value")
		}
	case "eq", "ne", "gt", "gte", "lt", "lte", "rectangular", "all_eq", "all_gte", "all_lte":
		if !present {
			return nil, fmt.Errorf("operator %s requires value (explicit null is allowed)", s.Operator)
		}
		// Normalize Go caller numeric types through JSON without coercing strings.
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > 2048 || json.Unmarshal(encoded, &s.Value) != nil || !interactionStateScalar(s.Value) {
			return nil, fmt.Errorf("value must be a finite JSON scalar, max 2048 bytes")
		}
		if s.Operator != "eq" && s.Operator != "ne" && s.Operator != "all_eq" {
			if _, ok := s.Value.(float64); !ok {
				return nil, fmt.Errorf("numeric comparison requires a numeric value")
			}
			if s.Source == "dom" {
				switch s.Path {
				case "text", "checked", "disabled", "visible":
					return nil, fmt.Errorf("DOM %s has a fixed nonnumeric type; ordered numeric comparison cannot measure it. Preserve the criterion: use an appropriate typed observation (for example real numeric app state), not a weaker assertion or a coerced string", s.Path)
				}
			}
		}
		if collectionStateOperator(s.Operator) && s.Source != "app" {
			return nil, fmt.Errorf("collection contracts require app source")
		}
		if s.Operator == "rectangular" {
			width := s.Value.(float64)
			if width < 1 || width > 4096 || math.Trunc(width) != width {
				return nil, fmt.Errorf("rectangular value must be an integer width 1..4096")
			}
		}
	default:
		return nil, fmt.Errorf("operator must be eq/ne/gt/gte/lt/lte/changed/unchanged")
	}
	return s, nil
}

func interactionStateScalar(v any) bool {
	switch x := v.(type) {
	case nil, string, bool:
		return true
	case float64:
		return !math.IsNaN(x) && !math.IsInf(x, 0)
	default:
		return false
	}
}

func readInteractionState(ctx context.Context, helper *MeadowBrowserVisualCheck, base string, s *interactionStateSpec) (any, error) {
	params, _ := json.Marshal(s)
	code := `(()=>{` + strictObservationJSONJS + `const s=` + string(params) + `;
const result=(found,value,error)=>({__meadow_observation:{found,value,error:error||""}});
try {
 let value;
 if(s.source==="dom") {
  const nodes=document.querySelectorAll(s.selector);
  if(s.path==="count") value=nodes.length;
  else {
   if(nodes.length!==1) return result(false,null,"selector must match exactly one element; observed "+nodes.length);
   const el=nodes[0];
   if(s.path==="text") value=el.innerText;
   else if(s.path==="visible") {const r=el.getBoundingClientRect(),c=getComputedStyle(el);value=!!(r.width&&r.height&&c.visibility!=="hidden"&&c.display!=="none");}
   else {if(!(s.path in el))return result(false,null,"DOM property unavailable");value=el[s.path];}
  }
 } else {
  if(!window.meadowTest||typeof window.meadowTest.getState!=="function") return result(false,null,"read-only window.meadowTest.getState() hook unavailable");
  value=window.meadowTest.getState();
  if(value&&typeof value.then==="function") return result(false,null,"state hook must be synchronous JSON");
  for(const part of s.path.split(".")) {
   if(value===null||typeof value!=="object"||!Object.prototype.hasOwnProperty.call(value,part))return result(false,null,"state path missing: "+s.path);
   value=value[part];
  }
 }
 if(["rectangular","unique","all_eq","all_gte","all_lte"].includes(s.operator)) {
  if(!Array.isArray(value))return result(false,null,"collection contract requires an array");
  const encoded=meadowJSON(value);if(encoded.length>6000)return result(false,null,"collection exceeds 6000 characters");
  return result(true,encoded,"");
 }
 if(value!==null&&!["string","number","boolean"].includes(typeof value))return result(false,null,"observed value must be a JSON scalar");
 if(typeof value==="number"&&!Number.isFinite(value))return result(false,null,"observed number is nonfinite");
 if(JSON.stringify(value).length>2048)return result(false,null,"observation exceeds 2048 characters");
 return result(true,value,"");
}catch(error){return result(false,null,String(error&&error.message||error));}})()`
	var raw json.RawMessage
	if err := helper.postJSON(ctx, base+"/js", map[string]any{"code": code}, &raw); err != nil {
		return nil, fmt.Errorf("observation transport: %w", err)
	}
	value, err := decodeInteractionState(raw)
	if err != nil || !collectionStateOperator(s.Operator) {
		return value, err
	}
	encoded, ok := value.(string)
	var values []any
	if !ok || len(encoded) > 24000 || json.Unmarshal([]byte(encoded), &values) != nil || values == nil {
		return nil, fmt.Errorf("invalid collection observation")
	}
	return values, nil
}

func decodeInteractionState(raw json.RawMessage) (any, error) {
	var current any
	if len(raw) > 16384 || json.Unmarshal(raw, &current) != nil {
		return nil, fmt.Errorf("invalid or oversized observation response")
	}
	// Decode only transport envelopes, never search arbitrary application fields
	// for something that happens to look like a successful observation.
	for depth := 0; depth < 8; depth++ {
		switch v := current.(type) {
		case string:
			if json.Unmarshal([]byte(v), &current) != nil {
				return nil, fmt.Errorf("invalid observation envelope")
			}
		case map[string]any:
			if observation, ok := v["__meadow_observation"].(map[string]any); ok {
				value, present := observation["value"]
				if observation["found"] != true || !present || !interactionStateScalar(value) || observation["error"] != "" {
					return nil, fmt.Errorf("observation unavailable: %v", observation["error"])
				}
				encoded, _ := json.Marshal(value)
				if len(encoded) > 8192 {
					return nil, fmt.Errorf("oversized observed value")
				}
				return value, nil
			}
			if len(v) != 1 {
				return nil, fmt.Errorf("unknown observation envelope")
			}
			found := false
			for _, name := range []string{"result", "v", "value"} {
				if child, exists := v[name]; exists {
					current, found = child, true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("missing observation envelope")
			}
		default:
			return nil, fmt.Errorf("missing observation envelope")
		}
	}
	return nil, fmt.Errorf("observation envelope nesting exceeded")
}

func compareInteractionState(s *interactionStateSpec, actual, previous any) bool {
	if collectionStateOperator(s.Operator) {
		values, ok := actual.([]any)
		if !ok || len(values) == 0 {
			return false
		}
		seen := map[string]bool{}
		for _, v := range values {
			switch s.Operator {
			case "rectangular":
				width := -1
				switch row := v.(type) {
				case []any:
					width = len(row)
				case string:
					width = len([]rune(row))
				}
				if float64(width) != s.Value {
					return false
				}
			case "unique":
				if !interactionStateScalar(v) {
					return false
				}
				b, err := json.Marshal(v)
				if err != nil || seen[string(b)] {
					return false
				}
				seen[string(b)] = true
			case "all_eq", "all_gte", "all_lte":
				if !interactionStateScalar(v) {
					return false
				}
				inner := *s
				inner.Operator = strings.TrimPrefix(s.Operator, "all_")
				if !compareInteractionState(&inner, v, nil) {
					return false
				}
			}
		}
		return true
	}
	switch s.Operator {
	case "eq":
		return reflect.DeepEqual(actual, s.Value)
	case "ne":
		return !reflect.DeepEqual(actual, s.Value)
	case "changed":
		return !reflect.DeepEqual(actual, previous)
	case "unchanged":
		return reflect.DeepEqual(actual, previous)
	}
	a, aOK := actual.(float64)
	b, bOK := s.Value.(float64)
	if !aOK || !bOK {
		return false
	}
	switch s.Operator {
	case "gt":
		return a > b
	case "gte":
		return a >= b
	case "lt":
		return a < b
	case "lte":
		return a <= b
	}
	return false
}

func collectionStateOperator(op string) bool {
	return op == "rectangular" || op == "unique" || op == "all_eq" || op == "all_gte" || op == "all_lte"
}
