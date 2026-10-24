package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const maxToolArgumentIssues = 6

type toolArgumentIssue struct {
	Path     string `json:"path"`
	Problem  string `json:"problem"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

type toolArgumentError struct {
	Code           string              `json:"code"`
	Tool           string              `json:"tool"`
	Message        string              `json:"message"`
	RequiredAction string              `json:"required_action"`
	Issues         []toolArgumentIssue `json:"issues"`
	Required       []string            `json:"required,omitempty"`
	Allowed        []string            `json:"allowed,omitempty"`
	Received       []string            `json:"received,omitempty"`
}

func (e *toolArgumentError) Error() string {
	if e == nil {
		return "invalid tool arguments"
	}
	return e.Message
}

func (e *toolArgumentError) modelContent() string {
	payload, err := json.Marshal(e)
	if err != nil {
		return "TOOL_ERROR {\"code\":\"invalid_tool_arguments\",\"message\":\"Arguments do not match the advertised tool schema.\"}"
	}
	return "TOOL_ERROR " + string(payload)
}

func (e *toolArgumentError) displayContent() string {
	if e == nil {
		return "Tool arguments rejected."
	}
	details := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		detail := issue.Path + " " + issue.Problem
		if issue.Expected != "" {
			detail += "; expected " + issue.Expected
		}
		if issue.Actual != "" {
			detail += ", got " + issue.Actual
		}
		details = append(details, detail)
	}
	if len(details) == 0 {
		return e.Tool + " arguments rejected: " + e.Message
	}
	return e.Tool + " arguments rejected: " + strings.Join(details, "; ")
}

func decodeToolArguments(tool tools.Tool, raw string) (map[string]any, *toolArgumentError) {
	schema := tool.Schema()
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return map[string]any{}, newToolArgumentError(tool.Name(), schema, nil, []toolArgumentIssue{{
			Path: "$", Problem: "invalid_json", Expected: "object",
		}})
	}
	args, ok := value.(map[string]any)
	if !ok || args == nil {
		return map[string]any{}, newToolArgumentError(tool.Name(), schema, nil, []toolArgumentIssue{{
			Path: "$", Problem: "wrong_type", Expected: "object", Actual: jsonType(value),
		}})
	}
	if err := resolveToolArgumentReferences(tool, args); err != nil {
		return args, newToolArgumentError(tool.Name(), schema, args, []toolArgumentIssue{{
			Path: "$", Problem: "argument_resolution_failed", Expected: err.Error(),
		}})
	}
	// write_file itself remains non-destructive by default. This internal flag
	// is injected only when its Workspace confirms that the exact existing path
	// was authoritatively read during this run; the tool rechecks that invariant
	// immediately before replacing bytes.
	authorizePriorReadWriteFileOverwrite(tool, args)
	if normalizer, ok := tool.(tools.ArgumentNormalizer); ok {
		normalizer.NormalizeArguments(args)
	}
	normalizeStringifiedStructuredArguments(args, schema)
	normalizeCompatibleArguments(args, schema)
	issues := make([]toolArgumentIssue, 0, 2)
	validateSchemaValue(args, schema, "$", &issues)
	if len(issues) > 0 {
		return args, newToolArgumentError(tool.Name(), schema, args, issues)
	}
	if validator, ok := tool.(tools.ArgumentValidator); ok {
		if err := validator.ValidateArguments(args); err != nil {
			return args, newToolArgumentError(tool.Name(), schema, args, []toolArgumentIssue{{
				Path: "$", Problem: "invalid_constraints", Expected: err.Error(),
			}})
		}
	}
	return args, nil
}

func resolveToolArgumentReferences(tool tools.Tool, args map[string]any) error {
	// The path guard embeds tools.Tool, so optional interfaces are not promoted.
	// Resolve its underlying explicit references before its normalizer can select
	// a collision path. Execution still passes through the original guard.
	if guarded, ok := tool.(*artifactPathGuardTool); ok {
		return resolveToolArgumentReferences(guarded.Tool, args)
	}
	if resolver, ok := tool.(tools.ArgumentResolver); ok {
		return resolver.ResolveArguments(args)
	}
	return nil
}

// normalizeStringifiedStructuredArguments repairs only lossless wire-shape
// mismatches described by the advertised schema. A model may serialize an
// array/object as JSON text, or emit null for an empty nested array. Walk known
// properties recursively, decode only a single required structured type, and
// turn null into [] only when the array has no positive minItems constraint.
// Ambiguous unions, malformed JSON, and required non-empty arrays remain
// untouched so validation still rejects guessed or lossy repairs.
func normalizeStringifiedStructuredArguments(args map[string]any, schema map[string]any) {
	normalizeStructuredArgumentValue(args, schema)
}

func normalizeStructuredArgumentValue(value any, schema map[string]any) any {
	expected := schemaTypes(schema)
	if raw, ok := value.(string); ok && len(expected) == 1 &&
		(expected[0] == "array" || expected[0] == "object") {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			var decoded any
			if json.Unmarshal([]byte(raw), &decoded) == nil &&
				matchesAnyJSONType(decoded, expected) {
				value = decoded
			}
		}
	}
	if value == nil && len(expected) == 1 && expected[0] == "array" {
		if minimum, ok := schemaInteger(schema, "minItems"); !ok || minimum == 0 {
			return []any{}
		}
		return value
	}

	switch typed := value.(type) {
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		required := make(map[string]bool)
		for _, name := range schemaRequiredNames(schema) {
			required[name] = true
		}
		for name, property := range properties {
			propertySchema, ok := property.(map[string]any)
			if !ok {
				continue
			}
			child, exists := typed[name]
			if !exists {
				continue
			}
			// Model transports sometimes preserve an omitted optional field as
			// JSON null. If null is not part of that field's advertised type,
			// dropping it is lossless and restores the same object the model
			// intended. Required fields and nullable optionals remain untouched.
			if child == nil && !required[name] &&
				!matchesAnyJSONType(nil, schemaTypes(propertySchema)) {
				delete(typed, name)
				continue
			}
			typed[name] = normalizeStructuredArgumentValue(child, propertySchema)
		}
	case []any:
		itemSchema, _ := schema["items"].(map[string]any)
		for index, item := range typed {
			typed[index] = normalizeStructuredArgumentValue(item, itemSchema)
		}
	}
	return value
}

func newToolArgumentError(toolName string, schema, args map[string]any, issues []toolArgumentIssue) *toolArgumentError {
	allowed := schemaPropertyNames(schema)
	received := make([]string, 0, len(args))
	for key := range args {
		received = append(received, key)
	}
	sort.Strings(received)
	return &toolArgumentError{
		Code: "invalid_tool_arguments", Tool: toolName,
		Message:        "Arguments do not match the advertised tool schema. Send a corrected native tool call or choose another tool.",
		RequiredAction: "retry_native_tool_call_with_schema",
		Issues:         issues,
		Required:       schemaRequiredNames(schema),
		Allowed:        allowed,
		Received:       received,
	}
}

func validateSchemaValue(value any, schema map[string]any, path string, issues *[]toolArgumentIssue) {
	if len(*issues) >= maxToolArgumentIssues || len(schema) == 0 {
		return
	}
	expected := schemaTypes(schema)
	if len(expected) > 0 && !matchesAnyJSONType(value, expected) {
		*issues = append(*issues, toolArgumentIssue{
			Path: path, Problem: "wrong_type",
			Expected: strings.Join(expected, "|"), Actual: jsonType(value),
		})
		return
	}
	if enum := schemaEnum(schema); len(enum) > 0 && !enumContains(enum, value) {
		*issues = append(*issues, toolArgumentIssue{
			Path: path, Problem: "invalid_value",
			Expected: compactEnum(enum), Actual: fmt.Sprint(value),
		})
		return
	}
	switch typed := value.(type) {
	case string:
		length := utf8.RuneCountInString(typed)
		if minimum, ok := schemaInteger(schema, "minLength"); ok && length < minimum {
			*issues = append(*issues, toolArgumentIssue{
				Path: path, Problem: "too_short",
				Expected: fmt.Sprintf(">=%d characters", minimum), Actual: fmt.Sprintf("%d characters", length),
			})
			return
		}
		if maximum, ok := schemaInteger(schema, "maxLength"); ok && length > maximum {
			*issues = append(*issues, toolArgumentIssue{
				Path: path, Problem: "too_long",
				Expected: fmt.Sprintf("<=%d characters", maximum), Actual: fmt.Sprintf("%d characters", length),
			})
			return
		}
	case map[string]any:
		for _, required := range schemaRequiredNames(schema) {
			if _, ok := typed[required]; ok {
				continue
			}
			*issues = append(*issues, toolArgumentIssue{
				Path: path + "." + required, Problem: "required",
			})
			if len(*issues) >= maxToolArgumentIssues {
				return
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for _, name := range schemaPropertyNames(schema) {
			property := properties[name]
			child, exists := typed[name]
			childSchema, valid := property.(map[string]any)
			if exists && valid {
				validateSchemaValue(child, childSchema, path+"."+name, issues)
				if len(*issues) >= maxToolArgumentIssues {
					return
				}
			}
		}
	case []any:
		if minimum, ok := schemaInteger(schema, "minItems"); ok && len(typed) < minimum {
			*issues = append(*issues, toolArgumentIssue{
				Path: path, Problem: "too_few_items",
				Expected: fmt.Sprintf(">=%d", minimum), Actual: fmt.Sprint(len(typed)),
			})
			return
		}
		if maximum, ok := schemaInteger(schema, "maxItems"); ok && len(typed) > maximum {
			*issues = append(*issues, toolArgumentIssue{
				Path: path, Problem: "too_many_items",
				Expected: fmt.Sprintf("<=%d", maximum), Actual: fmt.Sprint(len(typed)),
			})
			return
		}
		itemSchema, _ := schema["items"].(map[string]any)
		for index, item := range typed {
			validateSchemaValue(item, itemSchema, fmt.Sprintf("%s[%d]", path, index), issues)
			if len(*issues) >= maxToolArgumentIssues {
				return
			}
		}
	}
}

func schemaRequiredNames(schema map[string]any) []string {
	var result []string
	switch required := schema["required"].(type) {
	case []string:
		result = append(result, required...)
	case []any:
		for _, item := range required {
			if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
				result = append(result, name)
			}
		}
	}
	return result
}

func schemaPropertyNames(schema map[string]any) []string {
	properties, _ := schema["properties"].(map[string]any)
	result := make([]string, 0, len(properties))
	for name := range properties {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func schemaTypes(schema map[string]any) []string {
	switch value := schema["type"].(type) {
	case string:
		return []string{value}
	case []string:
		return append([]string(nil), value...)
	case []any:
		result := make([]string, 0, len(value))
		for _, item := range value {
			if name, ok := item.(string); ok {
				result = append(result, name)
			}
		}
		return result
	default:
		return nil
	}
}

func schemaEnum(schema map[string]any) []any {
	switch value := schema["enum"].(type) {
	case []any:
		return append([]any(nil), value...)
	case []string:
		result := make([]any, len(value))
		for index := range value {
			result[index] = value[index]
		}
		return result
	default:
		return nil
	}
}

func schemaInteger(schema map[string]any, key string) (int, bool) {
	switch value := schema[key].(type) {
	case int:
		return value, true
	case float64:
		if math.Trunc(value) == value {
			return int(value), true
		}
	}
	return 0, false
}

func matchesAnyJSONType(value any, expected []string) bool {
	for _, kind := range expected {
		switch kind {
		case "object":
			_, ok := value.(map[string]any)
			if ok {
				return true
			}
		case "array":
			_, ok := value.([]any)
			if ok {
				return true
			}
		case "string":
			_, ok := value.(string)
			if ok {
				return true
			}
		case "boolean":
			_, ok := value.(bool)
			if ok {
				return true
			}
		case "number":
			if isJSONNumber(value, false) {
				return true
			}
		case "integer":
			if isJSONNumber(value, true) {
				return true
			}
		case "null":
			if value == nil {
				return true
			}
		}
	}
	return false
}

func isJSONNumber(value any, integer bool) bool {
	switch number := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		value := float64(number)
		return !integer || math.Trunc(value) == value
	case float64:
		return !integer || math.Trunc(number) == number
	default:
		return false
	}
}

func jsonType(value any) string {
	if value == nil {
		return "null"
	}
	switch typed := value.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if math.Trunc(typed) == typed {
			return "integer"
		}
		return "number"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "integer"
	case float32:
		if math.Trunc(float64(typed)) == float64(typed) {
			return "integer"
		}
		return "number"
	default:
		return reflect.TypeOf(value).String()
	}
}

func enumContains(enum []any, value any) bool {
	for _, candidate := range enum {
		if reflect.DeepEqual(candidate, value) {
			return true
		}
	}
	return false
}

func compactEnum(enum []any) string {
	values := make([]string, 0, len(enum))
	for _, item := range enum {
		values = append(values, fmt.Sprint(item))
	}
	return strings.Join(values, "|")
}

// normalizeCompatibleArguments canonicalizes only the unambiguous path aliases
// already accepted by workspace tools. It does not guess plural/list-shaped
// fields and refuses conflicting aliases.
func normalizeCompatibleArguments(args map[string]any, schema map[string]any) {
	properties, _ := schema["properties"].(map[string]any)
	if _, advertised := properties["file_path"]; !advertised {
		return
	}
	required := false
	for _, name := range schemaRequiredNames(schema) {
		required = required || name == "file_path"
	}
	if !required {
		return
	}
	if _, exists := args["file_path"]; exists {
		return
	}
	canonical := ""
	for _, alias := range []string{"path", "filename"} {
		value, ok := args[alias].(string)
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			continue
		}
		if canonical != "" && canonical != value {
			return
		}
		canonical = value
	}
	if canonical != "" {
		args["file_path"] = canonical
	}
}
