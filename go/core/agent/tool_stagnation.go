package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const toolStagnationWindow = 8
const semanticToolStagnationWindow = 12
const semanticToolStagnationLimit = 6

type toolStagnationObservation struct {
	outputHash string
	count      int
	sequence   int
}

// toolStagnationTracker detects a repeated call that keeps returning the same
// observable result, even when the model alternates it with another call. It
// never decides the next action; it only bounds a proven no-progress cycle.
type toolStagnationTracker struct {
	sequence  int
	byCall    map[string]toolStagnationObservation
	byIntent  map[string]toolStagnationObservation
	byFailure map[string]toolStagnationObservation
	byValidator map[string]toolStagnationObservation
}

func newToolStagnationTracker() *toolStagnationTracker {
	return &toolStagnationTracker{
		byCall:    make(map[string]toolStagnationObservation),
		byIntent:  make(map[string]toolStagnationObservation),
		byFailure: make(map[string]toolStagnationObservation),
		byValidator: make(map[string]toolStagnationObservation),
	}
}

func (t *toolStagnationTracker) Observe(call provider.ToolCall, execution ToolExecution) int {
	if t == nil {
		return 0
	}
	t.sequence++
	if execution.Status == ToolOK && execution.Effect == EffectWrite {
		clear(t.byCall)
		clear(t.byIntent)
		clear(t.byFailure)
		// An edit is progress, but it must not erase evidence that the same
		// invalid tool contract or browser validator keeps recurring after edits.
		return 0
	}
	callFingerprint := canonicalToolCallFingerprint(call)
	outputFingerprint := toolExecutionOutputFingerprint(execution)
	previous, ok := t.byCall[callFingerprint]
	if !ok || previous.outputHash != outputFingerprint ||
		t.sequence-previous.sequence > toolStagnationWindow {
		previous = toolStagnationObservation{outputHash: outputFingerprint, count: 1}
	} else {
		previous.count++
	}
	previous.sequence = t.sequence
	t.byCall[callFingerprint] = previous
	return previous.count
}

// ObserveValidatorFailure bounds edit -> browser failure -> edit loops. It is
// deliberately coarse: after three failures of the same validator class, the
// artifact is preserved for human review instead of spending more model turns.
func (t *toolStagnationTracker) ObserveValidatorFailure(execution ToolExecution) int {
	if t == nil {
		return 0
	}
	if execution.Tool != "meadow_browser_interaction_check" &&
		execution.Tool != "meadow_browser_visual_check" {
		return 0
	}
	if execution.Status == ToolOK {
		for signature := range t.byValidator {
			if strings.HasPrefix(signature, execution.Tool+"\x00") {
				delete(t.byValidator, signature)
			}
		}
		return 0
	}
	content := strings.ToLower(strings.TrimSpace(execution.LLMContent))
	class := ""
	switch {
	case strings.Contains(content, "(assert_dom)"):
		class = "assert_dom"
	case execution.Tool == "meadow_browser_interaction_check" && (
		strings.Contains(content, "interaction_coverage_invalid") ||
			strings.Contains(content, "interaction_check_call_invalid") ||
			strings.Contains(content, "settle_ms must be between") ||
			strings.Contains(content, "arguments rejected")):
		class = "validator_contract"
	case strings.Contains(content, "interaction_check_failed"):
		class = "interaction"
	case execution.Tool == "meadow_browser_visual_check":
		class = "technical_runtime"
	}
	if class == "" {
		return 0
	}
	signature := execution.Tool + "\x00" + class
	previous := t.byValidator[signature]
	if previous.count == 0 || t.sequence-previous.sequence > semanticToolStagnationWindow {
		previous = toolStagnationObservation{count: 1}
	} else {
		previous.count++
	}
	previous.sequence = t.sequence
	t.byValidator[signature] = previous
	return previous.count
}

// ObserveTypedFailure groups failures by their machine-readable recovery
// shape, not by the model's full arguments. This catches a second cosmetically
// different payload that still violates the same required fields/types. It is
// intentionally limited to TOOL_ERROR JSON; prose errors remain under the
// slower exact/semantic stagnation guards.
func (t *toolStagnationTracker) ObserveTypedFailure(
	call provider.ToolCall,
	execution ToolExecution,
) int {
	if t == nil || execution.Status == ToolOK {
		return 0
	}
	signature := typedToolFailureFingerprint(call.Function.Name, execution.LLMContent)
	if signature == "" {
		return 0
	}
	previous, ok := t.byFailure[signature]
	if !ok || t.sequence-previous.sequence > semanticToolStagnationWindow {
		previous = toolStagnationObservation{count: 1}
	} else {
		previous.count++
	}
	previous.sequence = t.sequence
	t.byFailure[signature] = previous
	return previous.count
}

func typedToolFailureFingerprint(toolName, content string) string {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "TOOL_ERROR ") {
		return ""
	}
	var payload struct {
		Code           string              `json:"code"`
		Path           string              `json:"path"`
		RequiredAction string              `json:"required_action"`
		Issues         []toolArgumentIssue `json:"issues"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(content, "TOOL_ERROR ")), &payload) != nil ||
		strings.TrimSpace(payload.Code) == "" {
		return ""
	}
	parts := []string{toolName, payload.Code, payload.Path, payload.RequiredAction}
	for _, issue := range payload.Issues {
		parts = append(parts, issue.Path, issue.Problem, issue.Expected)
	}
	return strings.Join(parts, "\x00")
}

func typedToolFailureCode(content string) string {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "TOOL_ERROR ") {
		return ""
	}
	var payload struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(content, "TOOL_ERROR ")), &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Code)
}

// ObserveSemanticIntent detects shell-command spelling changes that keep
// inspecting the same unchanged target. Exact-call detection remains the
// primary fast path; this larger threshold is reserved for a proven family of
// no-progress diagnostic variants.
func (t *toolStagnationTracker) ObserveSemanticIntent(
	call provider.ToolCall,
	execution ToolExecution,
) int {
	if t == nil || execution.Status == ToolOK && execution.Effect == EffectWrite {
		return 0
	}
	intent := semanticToolIntentFingerprint(call, execution)
	if intent == "" {
		return 0
	}
	previous, ok := t.byIntent[intent]
	if !ok || t.sequence-previous.sequence > semanticToolStagnationWindow {
		previous = toolStagnationObservation{count: 1}
	} else {
		previous.count++
	}
	previous.sequence = t.sequence
	t.byIntent[intent] = previous
	return previous.count
}

func canonicalToolCallFingerprint(call provider.ToolCall) string {
	arguments := strings.TrimSpace(call.Function.Arguments)
	var decoded any
	if json.Unmarshal([]byte(arguments), &decoded) == nil {
		if canonical, err := json.Marshal(decoded); err == nil {
			arguments = string(canonical)
		}
	}
	return call.Function.Name + "\x00" + arguments
}

var shellPathTokenRe = regexp.MustCompile(
	`(?:/workspace(?:/[A-Za-z0-9_.-]+)+|(?:\./|\.\./)[A-Za-z0-9_./-]+|[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+|[A-Za-z0-9_-]+\.[A-Za-z0-9_.-]+)`,
)

func semanticToolIntentFingerprint(call provider.ToolCall, execution ToolExecution) string {
	args := execution.Args
	if len(args) == 0 {
		_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
	}
	// Network research tools commonly vary max_chars, selectors, prompts, or
	// settle timing while reopening the same page. Allow several bounded
	// attempts, then group them as one semantic read intent so a successful
	// fetch cannot reset no-progress recovery forever. Different fetchers share
	// the bounded page-level budget: five attempts still allow a renderer
	// fallback, while changing tool names cannot extend the loop indefinitely.
	// Successful writes still reset the whole tracker.
	if execution.Effect != EffectWrite {
		rawURL, _ := args["url"].(string)
		if parsed, err := url.Parse(strings.TrimSpace(rawURL)); err == nil &&
			(parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
			parsed.Fragment = ""
			return strings.Join([]string{"external_read", parsed.String()}, "\x00")
		}
	}
	if execution.Tool != DefaultShellTool && call.Function.Name != DefaultShellTool {
		return ""
	}
	command, _ := args["command"].(string)
	command = strings.TrimSpace(command)
	script, _ := args["script"].(string)
	purpose, _ := args["purpose"].(string)
	family := shellCommandFamily(command, script)
	targets := shellCommandTargets(command + "\n" + script)

	if execution.Status != ToolOK {
		if family == "" {
			family = "unknown"
		}
		return strings.Join([]string{
			DefaultShellTool, "failure", family, strings.Join(targets, ","),
		}, "\x00")
	}
	if strings.EqualFold(strings.TrimSpace(purpose), "validation") {
		return ""
	}
	if family != "file_inspection" || len(targets) == 0 {
		return ""
	}
	return strings.Join([]string{
		DefaultShellTool, "inspection", family, strings.Join(targets, ","),
	}, "\x00")
}

func shellCommandFamily(command, script string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return ""
	}
	head := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
	if head == "env" {
		for len(fields) > 1 {
			fields = fields[1:]
			if !strings.Contains(fields[0], "=") {
				break
			}
		}
		if len(fields) == 0 {
			return ""
		}
		head = strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
	}
	switch head {
	case "cat", "head", "tail", "sed", "awk", "wc", "stat", "file",
		"xxd", "od", "hexdump", "strings":
		return "file_inspection"
	case "python", "python3":
		if strings.TrimSpace(script) != "" {
			return "python_script"
		}
		if strings.Contains(command, " -c ") {
			return "python_inline"
		}
		return "python"
	case "node":
		if strings.TrimSpace(script) != "" {
			return "node_script"
		}
		if strings.Contains(command, " -e ") {
			return "node_inline"
		}
		return "node"
	default:
		return head
	}
}

func shellCommandTargets(source string) []string {
	seen := map[string]bool{}
	for _, match := range shellPathTokenRe.FindAllString(source, -1) {
		target := strings.Trim(match, `"'(),:;`)
		target = strings.TrimPrefix(filepath.Clean(target), "/workspace/")
		if target == "." || target == "" {
			continue
		}
		seen[target] = true
	}
	result := make([]string, 0, len(seen))
	for target := range seen {
		result = append(result, target)
	}
	sort.Strings(result)
	return result
}

func toolExecutionOutputFingerprint(execution ToolExecution) string {
	payload := string(execution.Status) + "\x00" + strings.TrimSpace(execution.LLMContent)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}
