package agent

import "encoding/json"

// Keep declared acceptance separate from opaque argument truncation. This only
// projects submitted schema fields; it does not infer criteria from task prose.
func compactAdvisorValidationContract(arguments string) json.RawMessage {
	var args struct {
		Coverage []struct {
			Requirement string `json:"requirement"`
		} `json:"coverage"`
		Steps []map[string]any `json:"steps"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil || len(args.Coverage) == 0 {
		return nil
	}
	type contract struct {
		Requirements        []string         `json:"model_authored_requirements"`
		Steps               []map[string]any `json:"submitted_steps"`
		OmittedRequirements int              `json:"omitted_requirements"`
		OmittedSteps        int              `json:"omitted_steps"`
		TruncatedFields     bool             `json:"truncated_fields"`
	}
	c := contract{}
	bounded := func(s string, limit int) string {
		if len(s) > limit {
			c.TruncatedFields = true
		}
		return compactEvidenceField(s, limit)
	}
	for i, row := range args.Coverage {
		if i >= 4 {
			c.OmittedRequirements++
			continue
		}
		c.Requirements = append(c.Requirements, bounded(row.Requirement, 384))
	}
	for i, step := range args.Steps {
		if i >= 15 && i != len(args.Steps)-1 {
			c.OmittedSteps++
			continue
		}
		out := map[string]any{"step": i + 1}
		for _, key := range []string{"action", "key", "selector", "source", "path", "operator", "value"} {
			value, exists := step[key]
			if !exists {
				continue
			}
			switch value := value.(type) {
			case string:
				out[key] = bounded(value, 96)
			case float64, bool:
				out[key] = value
			case nil:
				out[key] = nil
			}
		}
		c.Steps = append(c.Steps, out)
	}
	for {
		encoded, err := json.Marshal(c)
		if err != nil {
			return nil
		}
		if len(encoded) <= 1400 {
			return encoded
		}
		// Preserve the initial and final submitted steps when limiting a long
		// sequence. Omission is explicit, never interpreted as full coverage.
		if len(c.Steps) > 2 {
			c.Steps = append(c.Steps[:len(c.Steps)-2], c.Steps[len(c.Steps)-1])
			c.OmittedSteps++
		} else if len(c.Requirements) > 1 {
			c.Requirements = c.Requirements[:len(c.Requirements)-1]
			c.OmittedRequirements++
		} else if len(c.Steps) > 0 {
			c.OmittedSteps += len(c.Steps)
			c.Steps = nil
		} else if len(c.Requirements) == 1 && len(c.Requirements[0]) > 96 {
			// JSON escaping can expand one byte into six. Preserve an explicit
			// truncated contract even for control/HTML-heavy requirements.
			c.Requirements[0] = bounded(c.Requirements[0], 96)
		} else {
			return nil
		}
	}
}
