package provider

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// RepairTextToolCalls recovers tool calls that a model emitted as PLAIN TEXT
// instead of native tool_calls. vllm-mlx's gemma4 parser converts the model's
// native control-token output into wire tool_calls, but weaker / local /
// non-gemma models sometimes print an unquoted JSON blob or a <tool_call> tag
// (Qwen/Hermes) in assistant content. Without repair the engine sees zero tool
// calls and ends the turn with prose. Markdown code and quoted blocks remain
// literal examples; they must never become executable calls.
//
// It only promotes a parsed call whose name is in allowed (the registered tool
// names), so ordinary prose that happens to contain a JSON object is never
// hijacked. Callers should run it only when native tool_calls came back empty.
// Returns the recovered calls (in source order, ids "repair_N") and the content
// with the matched spans removed.
func RepairTextToolCalls(content string, allowed []string) ([]ToolCall, string) {
	if strings.TrimSpace(content) == "" || len(allowed) == 0 {
		return nil, content
	}
	allow := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		allow[n] = true
	}

	type hit struct {
		start, end int
		call       ToolCall
	}
	var hits []hit
	literals := toolProtocolLiteralRanges(content)
	covered := func(s, e int) bool {
		for _, h := range hits {
			if s < h.end && e > h.start {
				return true
			}
		}
		return false
	}
	add := func(start, end int, jsonStr string) {
		if covered(start, end) || toolProtocolLiteralAt(literals, start) {
			return
		}
		if call, ok := parseToolCallJSON(jsonStr, allow); ok {
			hits = append(hits, hit{start, end, call})
		}
	}

	// 1. <tool_call>...</tool_call> (Qwen / Hermes style). Capture everything
	//    between the tags and let the JSON parser handle nesting.
	for _, m := range tagToolCallRe.FindAllStringSubmatchIndex(content, -1) {
		add(m[0], m[1], content[m[2]:m[3]])
	}
	// 2. bare top-level balanced JSON objects that mention "name".
	for _, span := range findBalancedJSONObjects(content) {
		sub := content[span[0]:span[1]]
		if strings.Contains(sub, `"name"`) {
			add(span[0], span[1], sub)
		}
	}
	// 3. Qwen transcript-echo style: <|tool_call>call:name{"arg": ...}
	//    <tool_call|>. Some Qwen/Qwythos turns print the harness transcript
	//    instead of native tool_calls; promote the call and drop any fake
	//    tool_response text that followed it.
	for _, m := range transcriptCallRe.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if !allow[name] {
			continue
		}
		openBrace := m[4]
		closeBrace := matchJSONBrace(content, openBrace)
		// Gemma 4 can surface its native call grammar as assistant text when
		// the serving template does not promote it to wire tool_calls:
		//
		//   <|tool_call>call:write_file{content:<|"|>...<|"|>,file_path:<|"|>/workspace/a.html<|"|>}<tool_call|>
		//
		// The marker-delimited content can contain arbitrary JSON braces, so a
		// normal balanced-JSON scan cannot find the outer closing brace. The
		// protocol terminator is authoritative; use the last brace before it.
		tagEnd := strings.Index(content[openBrace:], "<tool_call|>")
		if tagEnd >= 0 {
			if candidate := strings.LastIndex(content[openBrace:openBrace+tagEnd], "}"); candidate >= 0 {
				closeBrace = openBrace + candidate
			}
		}
		if closeBrace < 0 {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(content[openBrace:closeBrace+1]), &args); err != nil {
			var ok bool
			args, ok = parseGemmaNativeArguments(content[openBrace+1 : closeBrace])
			if !ok {
				continue
			}
		}
		if payload, err := json.Marshal(map[string]any{"name": name, "arguments": args}); err == nil {
			end := closeBrace + 1
			if tagEnd := strings.Index(content[end:], "<tool_call|>"); tagEnd >= 0 {
				end += tagEnd + len("<tool_call|>")
			}
			if respEnd := strings.Index(content[end:], "<tool_response|>"); respEnd >= 0 {
				end += respEnd + len("<tool_response|>")
			}
			add(m[0], end, string(payload))
		}
	}
	// 4. Qwen CLI-display echo style:
	//
	//      [Calling run_shell_command({"command":"python3 ...","purpose":"validation"})]
	//
	//    Some Qwen turns echo the harness's human-readable tool display rather
	//    than the native channel. The payload is still a complete JSON object,
	//    so promote it only when the function name is registered and the entire
	//    parenthesized argument parses as one object.
	for _, m := range directCallingRe.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if !allow[name] {
			continue
		}
		openParen := m[1] - 1
		closeParen := matchParen(content, openParen)
		if closeParen < 0 {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(content[openParen+1:closeParen])), &args); err != nil {
			continue
		}
		payload, err := json.Marshal(map[string]any{"name": name, "arguments": args})
		if err != nil {
			continue
		}
		end := closeParen + 1
		for end < len(content) && (content[end] == ' ' || content[end] == '\t') {
			end++
		}
		if end < len(content) && content[end] == ']' {
			end++
		}
		add(m[0], end, string(payload))
	}
	// 5. Python-call / signature-echo style: name(key="val", key=val). Local
	//    Qwen-family models echo the tool *signature* shown in the system prompt
	//    (write_file(file_path, content)) instead of emitting the native format,
	//    so the call arrives as text like write_file(file_path="x", content="y")
	//    — often inside a stray <tool_call> tag with no JSON for branch 1 to find.
	//    Scan for an allowed name + a quote-aware balanced (...) and synthesize the
	//    {"name","arguments"} object the JSON parser already understands. Empty or
	//    no-keyword arg lists are skipped so a bare signature mention is not a call.
	for _, m := range pyCallNameRe.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if !allow[name] {
			continue
		}
		openParen := m[1] - 1
		closeParen := matchParen(content, openParen)
		if closeParen < 0 {
			continue
		}
		kwargs := parseKwargs(content[openParen+1 : closeParen])
		if len(kwargs) == 0 {
			continue
		}
		if payload, err := json.Marshal(map[string]any{"name": name, "arguments": kwargs}); err == nil {
			add(m[2], closeParen+1, string(payload))
		}
	}
	// 6. Qwen function-tag style. Some Qwen chat templates stream a tool call
	//    as angle-bracket fields instead of JSON:
	//
	//      <tool_name>run_shell_command</tool_name>
	//      <parameter=command>go test ./...</parameter>
	//      <parameter=timeout>30000</parameter>
	//      </tool_call>
	//
	//    In practice the model may omit </tool_name> and </parameter>, and may
	//    prefix the whole block with "[". Parameter starts still delimit values
	//    unambiguously, so recover both forms while retaining the allowlist gate.
	for _, block := range findTaggedToolCalls(content) {
		if !allow[block.name] || covered(block.start, block.end) {
			continue
		}
		// A malformed Gemma HTML write can expose a content parameter but put
		// file_path in a trailing named tag. Let the narrow complete-document
		// recovery below inspect it; do not cover that span with a guaranteed
		// schema-invalid content-only call.
		if block.name == "write_file" {
			if _, hasPath := block.arguments["file_path"]; !hasPath {
				continue
			}
		}
		if payload, err := json.Marshal(map[string]any{
			"name":      block.name,
			"arguments": block.arguments,
		}); err == nil {
			add(block.start, block.end, string(payload))
		}
	}
	// 7. Gemma 4 attribute-tag write style. With textual live-editor transport,
	//    Gemma can turn the requested parameter envelope into:
	//
	//      <parameter name="content" value="<!doctype html>...</html></parameter>
	//      file_path="/workspace/game.html"
	//
	//    Quotes inside HTML make this invalid XML/JSON, but the structural
	//    boundaries are still unambiguous. Recover only a complete, balanced
	//    HTML document targeting a normalized workspace .html path. No partial
	//    document or other tool is promoted to an executable mutation.
	for _, block := range findGemmaAttributeHTMLWrites(content) {
		if !allow[block.name] || covered(block.start, block.end) {
			continue
		}
		if payload, err := json.Marshal(map[string]any{
			"name":      block.name,
			"arguments": block.arguments,
		}); err == nil {
			add(block.start, block.end, string(payload))
		}
	}

	if len(hits) == 0 {
		return nil, content
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].start < hits[j].start })

	// Strip matched spans back-to-front so indices stay valid.
	cleaned := content
	for i := len(hits) - 1; i >= 0; i-- {
		cleaned = cleaned[:hits[i].start] + cleaned[hits[i].end:]
	}
	calls := make([]ToolCall, 0, len(hits))
	for i, h := range hits {
		c := h.call
		c.ID = fmt.Sprintf("repair_%d", i)
		c.Type = "function"
		calls = append(calls, c)
	}
	return calls, strings.TrimSpace(cleaned)
}

const gemmaNativeStringMarker = `<|"|>`

// parseGemmaNativeArguments parses Gemma's marker-delimited native argument
// object. It is deliberately strict: keys must be identifiers and every value
// must be either a marker-delimited string or valid JSON. The registered-tool
// allowlist and the executor's schema validation remain the authority before
// any recovered call can run.
func parseGemmaNativeArguments(raw string) (map[string]any, bool) {
	arguments := make(map[string]any)
	for cursor := 0; ; {
		for cursor < len(raw) && (raw[cursor] == ' ' || raw[cursor] == '\t' || raw[cursor] == '\r' || raw[cursor] == '\n' || raw[cursor] == ',') {
			cursor++
		}
		if cursor == len(raw) {
			return arguments, len(arguments) > 0
		}
		keyStart := cursor
		for cursor < len(raw) && ((raw[cursor] >= 'a' && raw[cursor] <= 'z') || (raw[cursor] >= 'A' && raw[cursor] <= 'Z') || (raw[cursor] >= '0' && raw[cursor] <= '9') || raw[cursor] == '_') {
			cursor++
		}
		if cursor == keyStart {
			return nil, false
		}
		key := raw[keyStart:cursor]
		for cursor < len(raw) && (raw[cursor] == ' ' || raw[cursor] == '\t') {
			cursor++
		}
		if cursor >= len(raw) || raw[cursor] != ':' {
			return nil, false
		}
		cursor++
		for cursor < len(raw) && (raw[cursor] == ' ' || raw[cursor] == '\t') {
			cursor++
		}
		if strings.HasPrefix(raw[cursor:], gemmaNativeStringMarker) {
			cursor += len(gemmaNativeStringMarker)
			end := strings.Index(raw[cursor:], gemmaNativeStringMarker)
			if end < 0 {
				return nil, false
			}
			arguments[key] = raw[cursor : cursor+end]
			cursor += end + len(gemmaNativeStringMarker)
			continue
		}

		valueStart := cursor
		depth := 0
		inString := false
		escaped := false
		for cursor < len(raw) {
			char := raw[cursor]
			if inString {
				if escaped {
					escaped = false
				} else if char == '\\' {
					escaped = true
				} else if char == '"' {
					inString = false
				}
				cursor++
				continue
			}
			switch char {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				if depth == 0 {
					return nil, false
				}
				depth--
			case ',':
				if depth == 0 {
					goto valueComplete
				}
			}
			cursor++
		}
	valueComplete:
		encoded := strings.TrimSpace(raw[valueStart:cursor])
		if encoded == "" || inString || depth != 0 {
			return nil, false
		}
		var value any
		if err := json.Unmarshal([]byte(encoded), &value); err != nil {
			return nil, false
		}
		arguments[key] = value
	}
}

var (
	tagToolCallRe        = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>`)
	transcriptCallRe     = regexp.MustCompile(`(?s)(?:<\|?tool_call\|?>)?\s*call:(\w+)\s*(\{)`)
	directCallingRe      = regexp.MustCompile(`(?i)\[\s*calling\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	callingNamedToolRe   = regexp.MustCompile(`(?i)\[\s*calling\s+tool\s*:\s*([A-Za-z_][A-Za-z0-9_]*)`)
	invokeToolNameRe     = regexp.MustCompile(`(?i)<invoke\s+name\s*=\s*["']?([A-Za-z_][A-Za-z0-9_]*)`)
	pyCallNameRe         = regexp.MustCompile(`(\w+)\s*\(`)
	taggedToolNameRe     = regexp.MustCompile(`<tool_name>\s*([A-Za-z_][A-Za-z0-9_]*)`)
	taggedFunctionNameRe = regexp.MustCompile(`<function=\s*([A-Za-z_][A-Za-z0-9_]*)\s*>`)
	// Diagnostic only: a damaged function opener is not enough to reconstruct
	// arguments or authorize execution. Retain the named tool for a native retry.
	malformedFunctionNameRe = regexp.MustCompile(`<tool_call>\s*<?function\s*=\s*([A-Za-z_][A-Za-z0-9_]*)(?:\s|>|<|$)`)
	taggedToolBodyStartRe   = regexp.MustCompile(`^\s*(?:<?function\s*=|<tool_name>|\{|[A-Za-z_][A-Za-z0-9_]*\s*\()`)
	taggedLegacyCallStartRe = regexp.MustCompile(`^(?:<function=\s*[A-Za-z_][A-Za-z0-9_]*\s*>|<tool_name>\s*[A-Za-z_][A-Za-z0-9_]*(?:\s*</tool_name>)?)\s*<parameter(?:=|\s+name)`)
	taggedParamRe           = regexp.MustCompile(`<parameter(?:=|\s+name\s*=\s*["']?)([A-Za-z_][A-Za-z0-9_]*)["']?\s*>`)
	gemmaAttributeContentRe = regexp.MustCompile(`<parameter\s+name\s*=\s*["']content["']\s+value\s*=\s*["']`)
	gemmaNamedContentRe     = regexp.MustCompile(`<parameter\s+name\s*=\s*["']content["']\s*>`)
	gemmaAttributePathRe    = regexp.MustCompile(`(?:^|[\s>])file_path\s*=\s*["']([^"']+)["']`)
	gemmaNamedPathRe        = regexp.MustCompile(`</?parameter\s+name\s*=\s*["']file_path["']\s*>\s*([^<\r\n]+)`)
	callingToolRe           = regexp.MustCompile(`(?i)(?:^|\n)\[\s*calling\s+(?:tool\s*:|[A-Za-z_][A-Za-z0-9_]*\s*\()`)
)

// RepairTextToolCallsWithSchemas adds one deliberately narrow Qwen fallback to
// the ordinary tagged/native-like repair: the entire response may be a JSON
// array of {"tool":"registered_name", ...arguments...} objects. Every object
// must satisfy the registered tool's schema. Prose-wrapped examples,
// mixed data arrays, unknown fields, and incomplete required arguments remain
// ordinary assistant content instead of being executed.
func RepairTextToolCallsWithSchemas(content string, tools []Tool, model string) ([]ToolCall, string) {
	allowed := toolNames(tools)
	if calls, cleaned := RepairTextToolCalls(content, allowed); len(calls) > 0 {
		calls = coerceRecoveredToolCallArguments(calls, tools)
		if IsGemma12Model(model) {
			calls = sanitizeGemma12RecoveredCalls(calls)
		}
		return calls, cleaned
	}
	profile, ok := ProfileForModel(model)
	if !ok || (profile.Family != "qwen3" && profile.Family != "ornith") {
		return nil, content
	}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" || !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return nil, content
	}
	var envelopes []map[string]any
	if err := json.Unmarshal([]byte(trimmed), &envelopes); err != nil || len(envelopes) == 0 {
		return nil, content
	}
	schemas := make(map[string]ToolSchema, len(tools))
	for _, tool := range tools {
		if tool.Function.Name != "" {
			schemas[tool.Function.Name] = tool.Function
		}
	}
	calls := make([]ToolCall, 0, len(envelopes))
	for index, envelope := range envelopes {
		name, _ := envelope["tool"].(string)
		schema, ok := schemas[name]
		if !ok {
			return nil, content
		}
		args, ok := qwenEnvelopeArguments(envelope, schema)
		if !ok {
			return nil, content
		}
		calls = append(calls, ToolCall{
			ID:   fmt.Sprintf("repair_%d", index),
			Type: "function",
			Function: FunctionCall{
				Name: name, Arguments: MarshalArgs(args),
			},
		})
	}
	return calls, ""
}

// coerceRecoveredToolCallArguments applies the advertised JSON schema to tool
// parameters. Qwen transports may carry tagged or native scalar parameters as
// text, so losslessly normalizing booleans and numbers keeps both transports
// behaviorally identical without weakening executor validation.
func coerceRecoveredToolCallArguments(calls []ToolCall, tools []Tool) []ToolCall {
	schemas := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		if tool.Function.Name != "" {
			schemas[tool.Function.Name] = tool.Function.Parameters
		}
	}
	for index := range calls {
		schema := schemas[calls[index].Function.Name]
		if len(schema) == 0 {
			continue
		}
		var arguments any
		if json.Unmarshal([]byte(calls[index].Function.Arguments), &arguments) != nil {
			continue
		}
		arguments = recoverEmbeddedWriteFilePath(calls[index].Function.Name, arguments)
		arguments = coerceRecoveredSchemaValue(arguments, schema)
		if encoded, err := json.Marshal(arguments); err == nil {
			calls[index].Function.Arguments = string(encoded)
		}
	}
	return calls
}

type toolCallSchemaFailure struct {
	toolName        string
	reason          ToolCallRecoveryReason
	missingRequired []string
	argumentBytes   int
}

// firstToolCallSchemaFailure validates the exact post-coercion arguments that
// would otherwise reach the executor. Repair may normalize lossless scalar
// encodings, but it must never promote a missing-required or schema-invalid
// long call into an executable success.
func firstToolCallSchemaFailure(calls []ToolCall, tools []Tool) *toolCallSchemaFailure {
	schemas := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		if tool.Function.Name != "" && len(tool.Function.Parameters) > 0 {
			schemas[tool.Function.Name] = tool.Function.Parameters
		}
	}
	for _, call := range calls {
		schema := schemas[call.Function.Name]
		if len(schema) == 0 {
			// Unknown tools remain executor-owned. Provider validation only has
			// authority over the schemas actually advertised on this request.
			continue
		}
		failure := &toolCallSchemaFailure{
			toolName: call.Function.Name, argumentBytes: len(call.Function.Arguments),
		}
		var arguments any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
			failure.reason = ToolCallRecoverySchemaInvalid
			return failure
		}
		if object, ok := arguments.(map[string]any); ok {
			for _, key := range schemaRequiredKeys(schema["required"]) {
				value, present := object[key]
				if !present || (key == "file_path" && strings.TrimSpace(fmt.Sprint(value)) == "") {
					failure.missingRequired = append(failure.missingRequired, key)
				}
			}
		}
		if len(failure.missingRequired) > 0 {
			failure.reason = ToolCallRecoveryMissingRequired
			return failure
		}
		if !jsonValueMatchesSchema(arguments, schema) {
			failure.reason = ToolCallRecoverySchemaInvalid
			return failure
		}
	}
	return nil
}

var embeddedWriteFilePathSuffix = regexp.MustCompile(
	"(?s)^(.*?)(?:`|<\\|\"\\|>)\\s*,\\s*file_path\\s*:\\s*\"([^\"\\r\\n<>]+)\"\\s*}\\s*(?:<tool_call\\|>|<\\|tool_call\\|>)?\\s*$",
)

// recoverEmbeddedWriteFilePath repairs one observed Gemma/Qwen parser defect:
// the native tool parser sometimes closes the content parameter too late and
// returns the tagged call's trailing `,file_path:"..."}<tool_call|>` as part of
// content. Only the exact, terminal write_file suffix is accepted. Incomplete
// suffixes and all other tools remain schema failures instead of guessing a
// destination.
func recoverEmbeddedWriteFilePath(toolName string, value any) any {
	if toolName != "write_file" {
		return value
	}
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if path, exists := object["file_path"]; exists && strings.TrimSpace(fmt.Sprint(path)) != "" {
		return value
	}
	content, ok := object["content"].(string)
	if !ok {
		return value
	}
	match := embeddedWriteFilePathSuffix.FindStringSubmatch(content)
	if len(match) != 3 || strings.TrimSpace(match[2]) == "" {
		return value
	}
	object["content"] = match[1]
	object["file_path"] = strings.TrimSpace(match[2])
	return object
}

func coerceRecoveredSchemaValue(value any, schema map[string]any) any {
	typeName, _ := schema["type"].(string)
	switch typeName {
	case "boolean":
		if text, ok := value.(string); ok {
			if parsed, err := strconv.ParseBool(strings.TrimSpace(text)); err == nil {
				return parsed
			}
		}
	case "number":
		if text, ok := value.(string); ok {
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
				return parsed
			}
		}
	case "integer":
		if text, ok := value.(string); ok {
			if parsed, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
				return parsed
			}
		}
	case "array":
		if text, ok := value.(string); ok {
			var decoded []any
			if json.Unmarshal([]byte(strings.TrimSpace(text)), &decoded) == nil {
				value = decoded
			}
		}
		items, ok := value.([]any)
		itemSchema, hasItems := schema["items"].(map[string]any)
		if ok && hasItems {
			for index := range items {
				items[index] = coerceRecoveredSchemaValue(items[index], itemSchema)
			}
			return items
		}
	case "object":
		if text, ok := value.(string); ok {
			var decoded map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(text)), &decoded) == nil {
				value = decoded
			}
		}
		object, ok := value.(map[string]any)
		properties, hasProperties := schema["properties"].(map[string]any)
		if ok && hasProperties {
			for key, item := range object {
				if property, declared := properties[key]; declared {
					if propertySchema, ok := property.(map[string]any); ok {
						object[key] = coerceRecoveredSchemaValue(item, propertySchema)
					}
				}
			}
			return object
		}
	}
	return value
}

// sanitizeGemma12RecoveredCalls removes multimodal boundary tokens that the
// current unified Gemma checkpoint can accidentally emit inside a
// marker-delimited text tool argument. These are tokenizer control tokens,
// not user text, and letting them reach write_file turns otherwise valid
// TypeScript into an immediate syntax error. Keep this scoped to recovered
// Meadow 12B calls; native wire tool calls and other model families are left
// byte-for-byte unchanged.
func sanitizeGemma12RecoveredCalls(calls []ToolCall) []ToolCall {
	for index := range calls {
		var arguments map[string]any
		if err := json.Unmarshal([]byte(calls[index].Function.Arguments), &arguments); err != nil {
			continue
		}
		for key, value := range arguments {
			arguments[key] = sanitizeGemma12RecoveredValue(value)
		}
		calls[index].Function.Arguments = MarshalArgs(arguments)
	}
	return calls
}

func sanitizeGemma12RecoveredValue(value any) any {
	switch typed := value.(type) {
	case string:
		for _, token := range []string{
			"<|image>", "<image|>", "<|image|>",
			"<|audio>", "<audio|>", "<|audio|>",
			"<|video>", "<video|>", "<|video|>",
		} {
			typed = strings.ReplaceAll(typed, token, "")
		}
		return typed
	case []any:
		for index := range typed {
			typed[index] = sanitizeGemma12RecoveredValue(typed[index])
		}
		return typed
	case map[string]any:
		for key, nested := range typed {
			typed[key] = sanitizeGemma12RecoveredValue(nested)
		}
		return typed
	default:
		return value
	}
}

func qwenEnvelopeArguments(envelope map[string]any, schema ToolSchema) (map[string]any, bool) {
	if len(envelope) == 2 {
		if nested, present := envelope["parameters"]; present {
			args, ok := nested.(map[string]any)
			if !ok || !jsonValueMatchesSchema(args, schema.Parameters) {
				return nil, false
			}
			return args, true
		}
	}
	properties, ok := schema.Parameters["properties"].(map[string]any)
	if !ok || len(properties) == 0 {
		return nil, false
	}
	for key := range envelope {
		if key == "tool" {
			continue
		}
		if _, allowed := properties[key]; !allowed {
			return nil, false
		}
	}
	args := make(map[string]any, len(envelope)-1)
	for key, value := range envelope {
		if key != "tool" {
			args[key] = value
		}
	}
	if len(args) == 0 || !jsonValueMatchesSchema(args, schema.Parameters) {
		return nil, false
	}
	return args, true
}

func jsonValueMatchesSchema(value any, schema map[string]any) bool {
	if len(schema) == 0 {
		return true
	}
	if rawEnum, exists := schema["enum"]; exists && !jsonValueMatchesEnum(value, rawEnum) {
		return false
	}
	typeName, _ := schema["type"].(string)
	switch typeName {
	case "":
		return true
	case "string":
		text, ok := value.(string)
		if !ok {
			return false
		}
		length := utf8.RuneCountInString(text)
		if minimum, present := schemaInteger(schema["minLength"]); present && length < minimum {
			return false
		}
		if maximum, present := schemaInteger(schema["maxLength"]); present && length > maximum {
			return false
		}
		return true
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && !math.IsNaN(number) && !math.IsInf(number, 0) && math.Trunc(number) == number
	case "array":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		if minimum, present := schemaInteger(schema["minItems"]); present && len(items) < minimum {
			return false
		}
		if maximum, present := schemaInteger(schema["maxItems"]); present && len(items) > maximum {
			return false
		}
		itemSchema, hasItems := schema["items"].(map[string]any)
		if !hasItems {
			return true
		}
		for _, item := range items {
			if !jsonValueMatchesSchema(item, itemSchema) {
				return false
			}
		}
		return true
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for _, key := range schemaRequiredKeys(schema["required"]) {
			if _, present := object[key]; !present {
				return false
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		additionalAllowed, _ := schema["additionalProperties"].(bool)
		for key, item := range object {
			property, declared := properties[key]
			if !declared {
				if schema["additionalProperties"] != nil && !additionalAllowed {
					return false
				}
				continue
			}
			propertySchema, ok := property.(map[string]any)
			if ok && !jsonValueMatchesSchema(item, propertySchema) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func schemaInteger(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int32:
		return int(number), true
	case int64:
		return int(number), true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
			return 0, false
		}
		return int(number), true
	default:
		return 0, false
	}
}

func schemaRequiredKeys(raw any) []string {
	switch values := raw.(type) {
	case []string:
		return values
	case []any:
		keys := make([]string, 0, len(values))
		for _, value := range values {
			if key, ok := value.(string); ok && key != "" {
				keys = append(keys, key)
			}
		}
		return keys
	default:
		return nil
	}
}

func jsonValueMatchesEnum(value, rawEnum any) bool {
	enum, ok := rawEnum.([]any)
	if !ok {
		switch values := rawEnum.(type) {
		case []string:
			for _, candidate := range values {
				if candidate == value {
					return true
				}
			}
		}
		return false
	}
	for _, candidate := range enum {
		if reflect.DeepEqual(candidate, value) {
			return true
		}
	}
	return false
}

var repairedCallNamespace atomic.Uint64

func uniqueRepairRequestID() string {
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return fmt.Sprintf("local-%d-%x", time.Now().UnixNano(), repairedCallNamespace.Add(1))
	}
	return "local-" + hex.EncodeToString(entropy[:])
}

func namespaceRepairedToolCalls(calls []ToolCall, requestID string) []ToolCall {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		// A process-local counter resets on Core restart and collides with
		// durable call ids such as repair_local-1_0 from an earlier write.
		requestID = uniqueRepairRequestID()
	}
	for index := range calls {
		calls[index].ID = fmt.Sprintf("repair_%s_%d", requestID, index)
	}
	return calls
}

var nativeProtocolMarkers = []string{
	"<|tool_call_start|>", "<|tool_call_end|>",
	"<|tool_call>", "<tool_call>", "<|tool_response>", "<tool_response>",
	"<tool_call|>", "</tool_call>", "<tool_response|>", "</tool_response>",
	"<tool_name>", "<parameter=", "<parameter name=", "<invoke name",
	"[Calling tool", "[calling tool",
	"[Writing file:", "[writing file:",
	"[Writing file with content:", "[writing file with content:",
	"[Editing file:", "[editing file:",
	"[Editing file with content:", "[editing file with content:",
}

var (
	mutationClaimArtifactRe = regexp.MustCompile(`(?i)(?:[\w.@~-]+/)*[\w.@~-]+\.(?:html?|css|m?js|tsx?|jsx?|json|ya?ml|toml|md|txt|csv|py|go|rs|sh|svg)\b|/workspace/\S+|(?:檔案|文件|file)\b`)
	// Some Qwen-family responses use ordinary prose rather than the bracketed
	// display protocol, for example "我已經將上面的程式碼儲存為 snake.html".
	// Requiring both a completed-action phrase and an artifact reference keeps
	// explanatory prose out while preventing an unsupported save claim from
	// bypassing the native mutation recovery gate.
	mutationCompletedClaimRe = regexp.MustCompile(`(?i)(?:我|我們|我们|系統|系统|\bi\b|\bwe\b).{0,32}(?:已經|已经|已|成功|have|has|successfully).{0,80}(?:儲存|储存|保存|存成|存為|存为|存到|建立|創建|创建|新增|寫入|写入|寫到|写到|修改|更新|saved|created|written|wrote|updated|modified)|(?:已經|已经|已|成功|successfully).{0,80}(?:完成|儲存|储存|保存|存成|存為|存为|存到|建立|創建|创建|新增|寫入|写入|寫到|写到|修改|更新|saved|created|written|wrote|updated|modified)`)
	mutationClaimClauseRe    = regexp.MustCompile(`[。！？!?;；\n]+`)
	mutationConditionalRe    = regexp.MustCompile(`(?i)^\s*(?:如果|若|假如|假設|假设|if\b|when\b)`)
	completeArtifactFenceRe  = regexp.MustCompile("(?is)```(?:html?|tsx?|jsx?|javascript|typescript)?\\s*\\n(.{400,}?)```")
	truncatedHTMLWriteRe     = regexp.MustCompile(`(?is)<tool_call>\s*<function=\s*write_file\s*>\s*<parameter=file_path>\s*([^<]+?)\s*</parameter>\s*<parameter=content>`)
)

// LooksLikeMutationNarration recognizes both the bracketed display protocol
// and an explicit completed file-mutation claim. The latter is intentionally
// gated by a filename/file reference so ordinary discussion about editing is
// not mistaken for an attempted tool action.
func LooksLikeMutationNarration(content string) bool {
	lower := strings.ToLower(content)
	if strings.Contains(lower, "[writing file:") ||
		strings.Contains(lower, "[writing file with content:") ||
		strings.Contains(lower, "[editing file:") ||
		strings.Contains(lower, "[editing file with content:") {
		return true
	}
	for _, clause := range mutationClaimClauseRe.Split(content, -1) {
		if mutationConditionalRe.MatchString(clause) {
			continue
		}
		if mutationClaimArtifactRe.MatchString(clause) &&
			mutationCompletedClaimRe.MatchString(clause) {
			return true
		}
	}
	return false
}

// LooksLikeCompleteArtifactDraft catches the other false-save shape: a model
// answers an execution task with a complete code artifact but never emits a
// native write call. The provider applies this only when write_file is in the
// request tool surface, so ordinary chat-mode code examples remain prose.
func LooksLikeCompleteArtifactDraft(content string) bool {
	if completeArtifactFenceRe.MatchString(content) {
		return true
	}
	trimmed := strings.TrimSpace(content)
	lower := strings.ToLower(trimmed)
	return len(trimmed) >= 400 &&
		(strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html")) &&
		strings.Contains(lower, "</html>")
}

// RecoverCompleteTruncatedHTMLWrite handles one narrow local-model failure:
// the token limit can arrive after a complete HTML document but before Qwen
// closes the surrounding parameter/function/tool tags. Persisting arbitrary
// partial output would be unsafe, so recovery requires the exact streamed
// write_file envelope, a workspace HTML path, and a structurally complete
// document. The returned call still passes through the normal tool schema and
// executor validation before any filesystem mutation occurs.
func RecoverCompleteTruncatedHTMLWrite(content string) (ToolCall, bool) {
	match := truncatedHTMLWriteRe.FindStringSubmatchIndex(content)
	if len(match) != 4 || toolProtocolLiteralAt(toolProtocolLiteralRanges(content), match[0]) {
		return ToolCall{}, false
	}
	filePath := strings.TrimSpace(content[match[2]:match[3]])
	if !isRecoverableWorkspaceHTMLPath(filePath) {
		return ToolCall{}, false
	}

	rawHTML := content[match[1]:]
	lowerHTML := strings.ToLower(rawHTML)
	closeIndex := strings.LastIndex(lowerHTML, "</html>")
	if closeIndex < 0 {
		return ToolCall{}, false
	}
	htmlEnd := closeIndex + len("</html>")
	html := strings.TrimSpace(rawHTML[:htmlEnd])
	if !isCompleteRecoverableHTML(html) || !isTruncatedToolEnvelopeSuffix(rawHTML[htmlEnd:]) {
		return ToolCall{}, false
	}

	return ToolCall{
		Type: "function",
		Function: FunctionCall{
			Name: "write_file",
			Arguments: MarshalArgs(map[string]any{
				"file_path": filePath,
				"content":   html,
			}),
		},
	}, true
}

// RecoverCompleteTruncatedHTMLToolCall is the native-tool counterpart to
// RecoverCompleteTruncatedHTMLWrite. The streaming parser may close a missing
// outer JSON brace for diagnostics; that reconstructed call is executable only
// when both required arguments are present and the HTML document itself ended
// at a conservative, structurally complete boundary.
func RecoverCompleteTruncatedHTMLToolCall(call ToolCall) (ToolCall, bool) {
	if call.Function.Name != "write_file" {
		return ToolCall{}, false
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
		return ToolCall{}, false
	}
	// Never normalize an unknown field away. The recovery helper has no schema
	// parameter, so its only safe authority is the exact public write_file pair.
	if len(arguments) != 2 {
		return ToolCall{}, false
	}
	filePath, pathOK := arguments["file_path"].(string)
	html, contentOK := arguments["content"].(string)
	if !pathOK || !contentOK || !isRecoverableWorkspaceHTMLPath(filePath) || !isCompleteRecoverableHTML(html) {
		return ToolCall{}, false
	}
	call.Function.Arguments = MarshalArgs(map[string]any{"file_path": filePath, "content": html})
	return call, true
}

func isRecoverableWorkspaceHTMLPath(filePath string) bool {
	if !utf8.ValidString(filePath) || strings.ContainsRune(filePath, '\\') {
		return false
	}
	for _, char := range filePath {
		if unicode.IsControl(char) {
			return false
		}
	}
	trimmed := strings.TrimSpace(filePath)
	lower := strings.ToLower(trimmed)
	return trimmed == filePath && path.Clean(trimmed) == trimmed &&
		strings.HasPrefix(trimmed, "/workspace/") && strings.HasSuffix(lower, ".html")
}

func isCompleteRecoverableHTML(html string) bool {
	trimmed := strings.TrimSpace(html)
	lower := strings.ToLower(trimmed)
	if !LooksLikeCompleteArtifactDraft(trimmed) || !strings.HasSuffix(lower, "</html>") ||
		strings.Count(lower, "<html") != 1 || strings.Count(lower, "</html>") != 1 {
		return false
	}
	// These raw-text/container tags are the practical truncation boundary for
	// generated single-file games. A missing close must not be mistaken for a
	// complete document merely because JavaScript happened to contain the text
	// "</html>".
	for _, tag := range []string{"head", "body", "style", "script"} {
		if strings.Count(lower, "<"+tag) != strings.Count(lower, "</"+tag+">") {
			return false
		}
	}
	return true
}

func isTruncatedToolEnvelopeSuffix(suffix string) bool {
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(suffix)
	if compact == "" {
		return true
	}
	const closingEnvelope = "</parameter></function></tool_call>"
	return strings.HasPrefix(closingEnvelope, compact)
}

// NativeToolProtocolStreamFilter quarantines protocol narration while a
// native tool-capable request is streaming. Qwen can emit fragments such as
// "[Calling tool" immediately before a canonical tool_calls delta; cleaning
// only after the stream ends is too late because CLI/Desktop observers have
// already rendered those fragments. Partial marker prefixes are held across
// delta boundaries and ordinary text is released immediately.
type NativeToolProtocolStreamFilter struct {
	enabled    bool
	pending    string
	suppressed bool
}

func NewNativeToolProtocolStreamFilter(enabled bool) *NativeToolProtocolStreamFilter {
	return &NativeToolProtocolStreamFilter{enabled: enabled}
}

func (f *NativeToolProtocolStreamFilter) Feed(delta string) string {
	if f == nil || !f.enabled || delta == "" {
		return delta
	}
	if f.suppressed {
		return ""
	}
	text := f.pending + delta
	f.pending = ""
	first := -1
	for _, marker := range nativeProtocolMarkers {
		if index := strings.Index(text, marker); index >= 0 && (first < 0 || index < first) {
			first = index
		}
	}
	if first >= 0 {
		f.suppressed = true
		return text[:first]
	}
	keep := longestPartialTail(text, nativeProtocolMarkers...)
	if keep != "" {
		f.pending = keep
		return text[:len(text)-len(keep)]
	}
	return text
}

func (f *NativeToolProtocolStreamFilter) Flush() string {
	if f == nil || !f.enabled || f.suppressed {
		return ""
	}
	text := f.pending
	f.pending = ""
	return text
}

func (f *NativeToolProtocolStreamFilter) Suppressed() bool {
	return f != nil && f.suppressed
}

// LooksLikeIncompleteToolCall identifies a response that entered a known
// tool-call protocol and stopped before a registered call could be
// reconstructed. It scans the complete response because a large content
// argument can put the opening marker far beyond a fixed-size tail. Whenever a
// tool name is present, it must match this request's allowlist. The function
// deliberately does not guess missing arguments; callers can give the model one
// bounded fresh retry instead of persisting protocol fragments as prose.
func LooksLikeIncompleteToolCall(content string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(maskToolProtocolLiterals(content))
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	// Repeated empty markers are much more likely to be quoted examples or
	// degeneration than one actionable call. Keep the historical false-positive
	// guard while allowing a single protocol body to be arbitrarily long.
	if strings.Count(lower, "<tool_call>")+strings.Count(lower, "<|tool_call") > 4 {
		return false
	}
	if toolName, identified := firstIncompleteProtocolToolName(trimmed); identified {
		for _, registered := range allowed {
			if toolName == registered {
				return true
			}
		}
		return false
	}
	if callingToolRe.MatchString(trimmed) {
		return true
	}
	prefixCandidate := strings.TrimLeft(lower, "[ \t\r\n")
	for _, prefix := range []string{
		"<tool_call",
		"<|tool_call",
		"<function=",
		"<tool_name>",
		"<parameter=",
		"<parameter name=",
		"<invoke name",
		"calling tool",
	} {
		if strings.HasPrefix(prefixCandidate, prefix) {
			return true
		}
	}
	for _, marker := range []string{
		"\n<tool_call",
		"\n<|tool_call",
		"\n<function=",
		"\n<tool_name>",
		"\n<parameter=",
		"\n<parameter name=",
		"\n[<invoke name",
		"\n[calling tool",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func firstIncompleteProtocolToolName(content string) (string, bool) {
	content = maskToolProtocolLiterals(content)
	bestStart := len(content) + 1
	bestName := ""
	consider := func(expression *regexp.Regexp) {
		match := expression.FindStringSubmatchIndex(content)
		if len(match) < 4 || match[0] >= bestStart {
			return
		}
		bestStart = match[0]
		bestName = content[match[2]:match[3]]
	}
	consider(taggedFunctionNameRe)
	consider(malformedFunctionNameRe)
	consider(taggedToolNameRe)
	consider(transcriptCallRe)
	consider(directCallingRe)
	consider(callingNamedToolRe)
	consider(invokeToolNameRe)
	return bestName, bestName != ""
}

// Markdown literals are observations/examples, not an executable transport.
// Keep byte offsets so an actual call can still contain arbitrary source text,
// including Markdown fences inside its string arguments. Only the call's start
// is excluded; its argument bytes are never rewritten before schema validation.
func toolProtocolLiteralRanges(content string) [][2]int {
	var ranges [][2]int
	lineEnd := func(start int) int {
		if offset := strings.IndexByte(content[start:], '\n'); offset >= 0 {
			return start + offset + 1
		}
		return len(content)
	}
	fenceWidth := func(line string) (byte, int) {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) > 3 || len(trimmed) == 0 || (trimmed[0] != '`' && trimmed[0] != '~') {
			return 0, 0
		}
		width := 0
		for width < len(trimmed) && trimmed[width] == trimmed[0] {
			width++
		}
		return trimmed[0], width
	}
	for start := 0; start < len(content); {
		if start == 0 || content[start-1] == '\n' {
			end := lineEnd(start)
			line := content[start:end]
			if fence, width := fenceWidth(line); width >= 3 {
				for end < len(content) {
					next := lineEnd(end)
					closing := content[end:next]
					closingFence, closingWidth := fenceWidth(closing)
					end = next
					if closingFence == fence && closingWidth >= width && strings.TrimSpace(strings.TrimLeft(closing, " ")[closingWidth:]) == "" {
						break
					}
				}
				ranges = append(ranges, [2]int{start, end})
				start = end
				continue
			}
			trimmed := strings.TrimLeft(line, " ")
			indentColumns := 0
			for index := 0; index < len(line) && indentColumns < 4; index++ {
				if line[index] == ' ' {
					indentColumns++
				} else if line[index] == '\t' {
					indentColumns += 4 - indentColumns%4
				} else {
					break
				}
			}
			if indentColumns >= 4 ||
				(len(line)-len(trimmed) <= 3 && strings.HasPrefix(trimmed, ">")) {
				ranges = append(ranges, [2]int{start, end})
				start = end
				continue
			}
		}
		// Once an unquoted transport envelope starts, its body is tool data.
		// Unclosed Markdown inside a write argument must not conceal a later call.
		protocolClose := ""
		if strings.HasPrefix(content[start:], "<tool_call>") && taggedToolBodyStartRe.MatchString(content[start+len("<tool_call>"):]) {
			protocolClose = "</tool_call>"
		} else if strings.HasPrefix(content[start:], "<function=") && taggedLegacyCallStartRe.MatchString(content[start:]) {
			protocolClose = "</function>"
			if !strings.Contains(content[start:], protocolClose) {
				protocolClose = "</tool_call>"
			}
		} else if strings.HasPrefix(content[start:], "<tool_name>") && taggedLegacyCallStartRe.MatchString(content[start:]) {
			protocolClose = "</tool_call>"
		}
		if protocolClose != "" {
			if end := strings.Index(content[start:], protocolClose); end >= 0 {
				start += end + len(protocolClose)
				continue
			}
			break
		}
		if content[start] == '`' {
			backslashes := 0
			for previous := start - 1; previous >= 0 && content[previous] == '\\'; previous-- {
				backslashes++
			}
			if backslashes%2 == 1 {
				start++
				continue
			}
			width := 1
			for start+width < len(content) && content[start+width] == '`' {
				width++
			}
			next := start + width
			for cursor := next; cursor < len(content); {
				if content[cursor] != '`' {
					cursor++
					continue
				}
				closingWidth := 1
				for cursor+closingWidth < len(content) && content[cursor+closingWidth] == '`' {
					closingWidth++
				}
				if closingWidth == width {
					next = cursor + closingWidth
					ranges = append(ranges, [2]int{start, next})
					break
				}
				cursor += closingWidth
			}
			start = next
			continue
		}
		start++
	}
	return ranges
}

func toolProtocolLiteralAt(ranges [][2]int, offset int) bool {
	for _, span := range ranges {
		if offset >= span[0] && offset < span[1] {
			return true
		}
	}
	return false
}

func maskToolProtocolLiterals(content string) string {
	ranges := toolProtocolLiteralRanges(content)
	if len(ranges) == 0 {
		return content
	}
	masked := []byte(content)
	for _, span := range ranges {
		for index := span[0]; index < span[1]; index++ {
			if masked[index] != '\n' && masked[index] != '\r' {
				masked[index] = ' '
			}
		}
	}
	return string(masked)
}

// HasUnquotedToolProtocol classifies leaked transport syntax without repairing,
// executing, removing or rewriting any answer. It is suitable for a tools-none
// boundary: even an unknown/incomplete tool name cannot be a valid public
// delivery there. Existing Markdown literal handling keeps code/quoted examples
// from being treated as attempted protocol.
func HasUnquotedToolProtocol(content string) bool {
	masked := strings.ToLower(maskToolProtocolLiterals(content))
	for _, marker := range nativeProtocolMarkers {
		if strings.Contains(masked, strings.ToLower(marker)) {
			return true
		}
	}
	// Older Qwen-style output can omit the outer tool_call envelope entirely.
	return strings.Contains(masked, "<function=")
}

// CleanNativeToolContent keeps only genuine prose that preceded leaked tool
// protocol markup. Once canonical ToolCalls exist, everything from the first
// protocol marker onward is a duplicate or fabricated transcript and must not
// be persisted into the next model turn.
func CleanNativeToolContent(content string) (string, bool) {
	first := -1
	for _, marker := range nativeProtocolMarkers {
		if index := strings.Index(content, marker); index >= 0 && (first < 0 || index < first) {
			first = index
		}
	}
	if first < 0 {
		return content, false
	}
	return strings.TrimSpace(content[:first]), true
}

// matchParen returns the index of the ')' that closes the '(' at open, scanning
// quote-aware (both " and ') so parens inside string literals don't unbalance
// it. Returns -1 if no matching close is found (an unterminated call is skipped
// rather than guessed).
func matchParen(s string, open int) int {
	depth := 0
	inStr := false
	var q byte
	esc := false
	for i := open; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == q:
				inStr = false
			}
			continue
		}
		switch ch {
		case '"', '\'':
			inStr = true
			q = ch
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func matchJSONBrace(s string, open int) int {
	if open < 0 || open >= len(s) || s[open] != '{' {
		return -1
	}
	depth := 0
	inStr := false
	esc := false
	for i := open; i < len(s); i++ {
		ch := s[i]
		if inStr {
			if esc {
				esc = false
			} else if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

type taggedToolCall struct {
	start, end int
	name       string
	arguments  map[string]any
}

// findGemmaAttributeHTMLWrites recognizes one complete malformed-but-common
// Gemma write_file envelope. It deliberately does not attempt general XML
// recovery: only complete standalone HTML written under /workspace is safe to
// promote through the normal schema and executor boundary.
func findGemmaAttributeHTMLWrites(content string) []taggedToolCall {
	var calls []taggedToolCall
	searchFrom := 0
	for searchFrom < len(content) {
		remainder := content[searchFrom:]
		functionLoc := taggedFunctionNameRe.FindStringSubmatchIndex(remainder)
		if functionLoc == nil {
			break
		}
		fieldsStart := searchFrom + functionLoc[1]
		name := content[searchFrom+functionLoc[2] : searchFrom+functionLoc[3]]
		if name != "write_file" {
			searchFrom = fieldsStart
			continue
		}
		start := searchFrom + functionLoc[0]
		if prefix := content[searchFrom:start]; strings.HasSuffix(strings.TrimSpace(prefix), "<tool_call>") {
			if marker := strings.LastIndex(prefix, "<tool_call>"); marker >= 0 {
				start = searchFrom + marker
			}
		}
		closeRel := strings.Index(content[fieldsStart:], "</tool_call>")
		if closeRel < 0 {
			break
		}
		blockEnd := fieldsStart + closeRel + len("</tool_call>")
		block := content[fieldsStart:blockEnd]
		contentLoc := gemmaAttributeContentRe.FindStringIndex(block)
		if contentLoc == nil {
			contentLoc = gemmaNamedContentRe.FindStringIndex(block)
		}
		if contentLoc == nil {
			searchFrom = blockEnd
			continue
		}
		valueStart := contentLoc[1]
		rawHTML := block[valueStart:]
		lowerHTML := strings.ToLower(rawHTML)
		htmlClose := strings.LastIndex(lowerHTML, "</html>")
		if htmlClose < 0 {
			searchFrom = blockEnd
			continue
		}
		htmlEnd := htmlClose + len("</html>")
		html := strings.TrimSpace(rawHTML[:htmlEnd])
		pathArea := rawHTML[htmlEnd:]
		pathLoc := gemmaAttributePathRe.FindStringSubmatchIndex(pathArea)
		if len(pathLoc) != 4 {
			pathLoc = gemmaNamedPathRe.FindStringSubmatchIndex(pathArea)
		}
		if len(pathLoc) != 4 {
			searchFrom = blockEnd
			continue
		}
		filePath := pathArea[pathLoc[2]:pathLoc[3]]
		if !isRecoverableWorkspaceHTMLPath(filePath) || !isCompleteRecoverableHTML(html) {
			searchFrom = blockEnd
			continue
		}
		calls = append(calls, taggedToolCall{
			start: start,
			end:   blockEnd,
			name:  "write_file",
			arguments: map[string]any{
				"file_path": filePath,
				"content":   html,
			},
		})
		searchFrom = blockEnd
	}
	return calls
}

// findTaggedToolCalls extracts Qwen's field-tag tool syntax. It intentionally
// requires a closing </tool_call> so an incomplete streamed call is never
// executed, and it returns only blocks containing at least one parameter.
func findTaggedToolCalls(content string) []taggedToolCall {
	var calls []taggedToolCall
	searchFrom := 0
	for searchFrom < len(content) {
		remainder := content[searchFrom:]
		nameLoc := taggedToolNameRe.FindStringSubmatchIndex(remainder)
		functionLoc := taggedFunctionNameRe.FindStringSubmatchIndex(remainder)
		if functionLoc != nil && (nameLoc == nil || functionLoc[0] < nameLoc[0]) {
			nameLoc = functionLoc
		}
		if nameLoc == nil {
			break
		}
		start := searchFrom + nameLoc[0]
		name := content[searchFrom+nameLoc[2] : searchFrom+nameLoc[3]]
		fieldsStart := searchFrom + nameLoc[1]
		if prefix := content[searchFrom:start]; strings.HasSuffix(strings.TrimSpace(prefix), "<tool_call>") {
			if marker := strings.LastIndex(prefix, "<tool_call>"); marker >= 0 {
				start = searchFrom + marker
			}
		}
		closeRel := strings.Index(content[fieldsStart:], "</tool_call>")
		nextNameRel := strings.Index(content[fieldsStart:], "<tool_name>")
		nextFunctionRel := strings.Index(content[fieldsStart:], "<function=")
		if nextFunctionRel >= 0 && (nextNameRel < 0 || nextFunctionRel < nextNameRel) {
			nextNameRel = nextFunctionRel
		}
		if nextNameRel >= 0 && (closeRel < 0 || nextNameRel < closeRel) {
			searchFrom = fieldsStart + nextNameRel
			continue
		}
		if closeRel < 0 {
			break
		}
		blockEnd := fieldsStart + closeRel + len("</tool_call>")
		block := content[fieldsStart : blockEnd-len("</tool_call>")]
		params := taggedParamRe.FindAllStringSubmatchIndex(block, -1)
		if len(params) == 0 {
			searchFrom = blockEnd
			continue
		}

		arguments := make(map[string]any, len(params))
		for i, param := range params {
			valueEnd := len(block)
			if i+1 < len(params) {
				valueEnd = params[i+1][0]
			}
			key := block[param[2]:param[3]]
			value := strings.TrimSpace(block[param[1]:valueEnd])
			// The last parameter is followed by both </parameter> and
			// </function> or </function=write_file>. Strip the outer function
			// marker first, then the parameter marker, otherwise those
			// closers become part of file_path / content and are persisted.
			value = stripTaggedProtocolSuffix(value)
			if isTaggedPathKey(key) {
				value = sanitizeRepairedPath(value)
			}
			arguments[key] = taggedParameterValue(key, value)
		}
		if start > 0 && content[start-1] == '[' {
			start--
		}
		if blockEnd < len(content) && content[blockEnd] == ']' {
			blockEnd++
		}
		calls = append(calls, taggedToolCall{
			start: start, end: blockEnd, name: name, arguments: arguments,
		})
		searchFrom = blockEnd
	}
	return calls
}

var taggedFunctionCloseRe = regexp.MustCompile(`(?i)</function(?:\s*=\s*[A-Za-z_][A-Za-z0-9_]*)?>\s*$`)

func stripTaggedProtocolSuffix(value string) string {
	for {
		trimmed := strings.TrimSpace(value)
		next := strings.TrimSpace(taggedFunctionCloseRe.ReplaceAllString(trimmed, ""))
		next = strings.TrimSpace(strings.TrimSuffix(next, "</tool_call>"))
		next = strings.TrimSpace(strings.TrimSuffix(next, "</parameter>"))
		next = strings.TrimSpace(strings.TrimSuffix(next, "</function>"))
		if next == trimmed {
			return next
		}
		value = next
	}
}

func isTaggedPathKey(key string) bool {
	switch key {
	case "file_path", "path", "filename", "file", "screenshot_path":
		return true
	}
	return false
}

func sanitizeRepairedPath(value string) string {
	value = strings.TrimSpace(strings.Trim(value, `"'`))
	if value == "" {
		return ""
	}
	if i := strings.IndexAny(value, "\n\r\x00"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	if i := strings.Index(value, "<"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return value
}

func taggedParameterValue(key, value string) any {
	switch key {
	case "timeout", "timeout_ms", "line_start", "line_end", "depth",
		"max_results", "max_output_chars":
		if integer, err := strconv.ParseInt(value, 10, 64); err == nil {
			return integer
		}
	case "recursive", "include_hidden", "case_sensitive":
		if boolean, err := strconv.ParseBool(value); err == nil {
			return boolean
		}
	}
	// Large file content is deliberately raw and may itself start with `{` or
	// `[`. Other structured tool arguments use ordinary JSON inside their
	// parameter envelope so arrays/objects/bools/numbers keep their schema type.
	if key != "content" {
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") ||
			trimmed == "true" || trimmed == "false" || trimmed == "null" {
			var decoded any
			if json.Unmarshal([]byte(trimmed), &decoded) == nil {
				return decoded
			}
		}
	}
	return value
}

// parseKwargs turns key="val", key='val', key=bareword (comma-separated at the
// top level) into an arguments map. Quote- and bracket-aware so commas, parens
// or '=' inside a value never split it — load-bearing for content="" holding
// code or prose.
func parseKwargs(inner string) map[string]any {
	out := map[string]any{}
	for _, part := range splitTopLevel(inner, ',') {
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(part[:eq])
		if key == "" || strings.ContainsAny(key, " \t\"'") {
			continue
		}
		out[key] = parseKwargVal(strings.TrimSpace(part[eq+1:]))
	}
	return out
}

// splitTopLevel splits on sep only at bracket depth 0 and outside string
// literals, so structured values survive intact.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth, start := 0, 0
	inStr := false
	var q byte
	esc := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == q:
				inStr = false
			}
			continue
		}
		switch ch {
		case '"', '\'':
			inStr = true
			q = ch
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// parseKwargVal unquotes a "..."/'...' literal (loose escape handling) or keeps
// a bareword, typing true/false/null and plain numbers so the arguments JSON
// matches what the model meant.
func parseKwargVal(val string) any {
	if len(val) >= 2 {
		if c := val[0]; (c == '"' || c == '\'') && val[len(val)-1] == c {
			return unquoteLoose(val[1 : len(val)-1])
		}
	}
	switch val {
	case "true":
		return true
	case "false":
		return false
	case "null", "None", "":
		return nil
	}
	if n, err := strconv.ParseFloat(val, 64); err == nil {
		return n
	}
	return val
}

// unquoteLoose applies the common backslash escapes without the strictness of
// strconv.Unquote, which rejects the mixed/partial escaping local models emit.
func unquoteLoose(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	esc := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if esc {
			switch ch {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"', '\'', '\\', '/':
				b.WriteByte(ch)
			default:
				b.WriteByte('\\')
				b.WriteByte(ch)
			}
			esc = false
			continue
		}
		if ch == '\\' {
			esc = true
			continue
		}
		b.WriteByte(ch)
	}
	if esc {
		b.WriteByte('\\')
	}
	return b.String()
}

// parseToolCallJSON parses one candidate JSON object into a ToolCall, accepting
// the common shapes models emit: {"name","arguments"} (arguments may be an
// object or a JSON-encoded string) and {"name","parameters"} (Gemma style).
// Returns ok=false unless a non-empty name in allow is found.
func parseToolCallJSON(jsonStr string, allow map[string]bool) (ToolCall, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonStr)), &m); err != nil {
		return ToolCall{}, false
	}
	name, _ := m["name"].(string)
	if name == "" {
		return ToolCall{}, false
	}
	if !allow[name] {
		return ToolCall{}, false
	}

	var raw any
	for _, key := range []string{"arguments", "parameters", "args", "input"} {
		if v, ok := m[key]; ok {
			raw = v
			break
		}
	}
	var argsStr string
	switch v := raw.(type) {
	case nil:
		argsStr = "{}"
	case string:
		// Double-encoded args: trust it if it parses as JSON, else wrap.
		if json.Valid([]byte(v)) {
			argsStr = v
		} else {
			argsStr = MarshalArgs(map[string]any{"value": v})
		}
	case map[string]any:
		argsStr = MarshalArgs(v)
	default:
		if b, err := json.Marshal(v); err == nil {
			argsStr = string(b)
		} else {
			argsStr = "{}"
		}
	}
	return ToolCall{Function: FunctionCall{Name: name, Arguments: argsStr}}, true
}

// findBalancedJSONObjects returns [start,end) byte spans of every top-level
// brace-balanced {...} region in s, honoring string literals and escapes so
// braces inside strings don't break balancing.
func findBalancedJSONObjects(s string) [][2]int {
	var spans [][2]int
	depth := 0
	start := -1
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr {
			if esc {
				esc = false
			} else if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					spans = append(spans, [2]int{start, i + 1})
					start = -1
				}
			}
		}
	}
	return spans
}

// toolNames extracts the registered tool names from a request's tool list —
// the allowlist that gates tool-call-repair.
func toolNames(tools []Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Function.Name != "" {
			names = append(names, t.Function.Name)
		}
	}
	return names
}

func toolSchemasHaveName(tools []Tool, name string) bool {
	for _, tool := range tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

// UsesStreamedToolTransport is the single request-shape decision for tagged
// tool streaming. When true, ChatStream keeps Tools only as the local repair
// authority and removes them from the OpenAI wire payload; the model-facing
// transport instruction already contains the same schemas.
func UsesStreamedToolTransport(request ChatRequest) bool {
	mode := strings.TrimSpace(request.StreamedToolDraft)
	return mode != "" && (mode == "*" || toolSchemasHaveName(request.Tools, mode))
}
