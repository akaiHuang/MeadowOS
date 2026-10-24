package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	desktopControlTimeout = 20 * time.Second
	desktopOutputLimit    = 256 * 1024
)

// DesktopControl acts only on the desktop capability supplied by the host.
// Neither its schema nor the helper request lets a model select another X
// server, replace the environment, or execute an arbitrary program.
type DesktopControl struct {
	WS       *Workspace
	Session  *DesktopSession
	ReadOnly bool
}

func NewDesktopControl(ws *Workspace, session *DesktopSession) *DesktopControl {
	return &DesktopControl{WS: ws, Session: session}
}

func (*DesktopControl) Name() string { return "desktop" }
func (t *DesktopControl) Description() string {
	text := "Observe the user's connected MeadowOS desktop: running applications, windows and accessibility text. Uses the host-bound desktop identity; never guess DISPLAY or connect to another desktop. status reports real capabilities; snapshot/windows report stable window_id, PID, title, geometry and focus. accessibility returns bounded nodes from a specific observed window. Tool receipts describe observed effects, not completion of the user's whole goal."
	if !t.ReadOnly {
		text += " launch/open starts a catalog application or opens an authorized local file/HTTP(S) URL in a visible app. Use observed window_id for focus, minimize, maximize, close or move_resize. accessibility_action/set_text use an observed node_id. click/move_pointer/scroll/drag/type_text/key send real desktop input; inspect the current window before acting and observe the result. A launched PID or dispatched input does not establish that the requested window or content appeared; observe it. screenshot saves the bound desktop to a new workspace PNG."
	}
	return text
}

var desktopReadActions = []string{"status", "snapshot", "windows", "applications", "accessibility"}
var desktopWriteActions = []string{"launch", "open", "focus", "close", "minimize", "maximize", "move_resize", "accessibility_action", "set_text", "click", "move_pointer", "scroll", "drag", "type_text", "key", "screenshot"}

func desktopActionReadOnly(action string) bool {
	for _, candidate := range desktopReadActions {
		if action == candidate {
			return true
		}
	}
	return false
}

func (t *DesktopControl) Kind() Kind {
	if t.ReadOnly {
		return KindReadOnly
	}
	return KindExecute
}
func (*DesktopControl) ClassifyCall(args map[string]any) Kind {
	action := strArg(args, "action")
	if desktopActionReadOnly(action) {
		return KindReadOnly
	}
	if action == "screenshot" {
		return KindEdit
	}
	return KindExecute
}
func (*DesktopControl) ParallelSafe(map[string]any) bool { return false }
func (*DesktopControl) CompleteObservation() bool        { return true }
func (t *DesktopControl) ToolWorkspace() *Workspace      { return t.WS }
func (t *DesktopControl) ForWorkspace(ws *Workspace) Tool {
	copy := *t
	copy.WS = ws
	if ws != nil {
		ws.SetDesktopSession(t.Session)
	}
	return &copy
}
func (t *DesktopControl) ReadOnlyView() *DesktopControl {
	copy := *t
	copy.ReadOnly = true
	return &copy
}
func (t *DesktopControl) SubtaskTool() Tool { return t.ReadOnlyView() }
func (t *DesktopControl) MutationPaths(args map[string]any) []string {
	if strArg(args, "action") == "screenshot" && strArg(args, "output_path") != "" {
		return []string{strArg(args, "output_path")}
	}
	return nil
}

func (t *DesktopControl) Schema() map[string]any {
	actions := append([]string(nil), desktopReadActions...)
	if !t.ReadOnly {
		actions = append(actions, desktopWriteActions...)
	}
	props := map[string]any{
		"action":    map[string]any{"type": "string", "enum": actions},
		"window_id": str("Stable window_id returned by snapshot/windows; required for window and accessibility actions. Never invent an ID."),
		"max_nodes": desktopIntegerSchema("Maximum accessibility nodes (1–400, default 160).", 1, 400),
		"max_depth": desktopIntegerSchema("Maximum accessibility depth (1–16, default 10).", 1, 16),
	}
	if !t.ReadOnly {
		props["application"] = str("Catalog desktop application ID from applications, or browser/chrome, files/finder, terminal, settings. Not a shell command.")
		props["target"] = str("Existing authorized local file/directory (/workspace or /session), or a full HTTP(S) URL. Optional for launch; required for open.")
		props["node_id"] = str("Exact accessibility node_id from the most recent accessibility result for this window.")
		props["action_name"] = str("Exact accessible action name from the observed node; alternative to action_index.")
		props["action_index"] = desktopIntegerSchema("Observed accessibility action index (0–31).", 0, 31)
		props["text"] = str("Literal text for type_text or set_text; at most 16384 UTF-8 bytes.")
		props["key"] = str("One X11 keysym combination, e.g. Return, Escape, ctrl+l, super+a. No shell syntax.")
		props["button"] = desktopIntegerSchema("Mouse button 1–3 (default 1).", 1, 3)
		props["enabled"] = boolean("For maximize: true maximizes (default), false restores the window.")
		props["count"] = desktopIntegerSchema("Number of clicks, 1–3 (default 1); use 2 for double-click.", 1, 3)
		props["x"] = desktopIntegerSchema("Observed desktop x coordinate.", 0, 32767)
		props["y"] = desktopIntegerSchema("Observed desktop y coordinate.", 0, 32767)
		props["to_x"] = desktopIntegerSchema("Drag destination desktop x coordinate.", 0, 32767)
		props["to_y"] = desktopIntegerSchema("Drag destination desktop y coordinate.", 0, 32767)
		props["delta_x"] = desktopIntegerSchema("Horizontal scroll steps, -20–20.", -20, 20)
		props["delta_y"] = desktopIntegerSchema("Vertical scroll steps, -20–20.", -20, 20)
		props["width"] = desktopIntegerSchema("Window width in pixels.", 64, 32767)
		props["height"] = desktopIntegerSchema("Window height in pixels.", 64, 32767)
		props["output_path"] = str("New PNG path in the authorized workspace/session, required for screenshot. Use inspect_image separately when visual model interpretation is needed.")
	}
	schema := obj(props, "action")
	schema["additionalProperties"] = false
	return schema
}

func desktopIntegerSchema(description string, minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "minimum": minimum, "maximum": maximum}
}

// prepare makes a fresh request. Passing the original argument map would let
// callers smuggle host-only context through a field ignored by JSON Schema.
func (t *DesktopControl) prepare(args map[string]any) (map[string]any, error) {
	if t == nil || t.WS == nil || t.Session == nil || strings.TrimSpace(t.Session.Display) == "" {
		return nil, fmt.Errorf("desktop_unavailable: no host-bound desktop session; do not select another display")
	}
	action, ok := args["action"].(string)
	if !ok || action == "" || strings.TrimSpace(action) != action {
		return nil, fmt.Errorf("desktop action is required")
	}
	known := desktopActionReadOnly(action)
	for _, candidate := range desktopWriteActions {
		known = known || action == candidate
	}
	if !known {
		return nil, fmt.Errorf("unknown desktop action %q", action)
	}
	if t.ReadOnly && !desktopActionReadOnly(action) {
		return nil, fmt.Errorf("desktop action %q requires a worker with desktop execution capability", action)
	}
	allowed := map[string]bool{"action": true}
	add := func(keys ...string) {
		for _, key := range keys {
			allowed[key] = true
		}
	}
	switch action {
	case "accessibility":
		add("window_id", "max_nodes", "max_depth")
	case "launch", "open":
		add("application", "target")
	case "focus", "close", "minimize":
		add("window_id")
	case "maximize":
		add("window_id", "enabled")
	case "move_resize":
		add("window_id", "x", "y", "width", "height")
	case "accessibility_action":
		add("window_id", "node_id", "action_name", "action_index")
	case "set_text":
		add("window_id", "node_id", "text")
	case "click":
		add("window_id", "x", "y", "button", "count")
	case "move_pointer":
		add("window_id", "x", "y")
	case "scroll":
		add("window_id", "delta_x", "delta_y")
	case "drag":
		add("window_id", "x", "y", "to_x", "to_y", "button")
	case "type_text":
		add("window_id", "text")
	case "key":
		add("window_id", "key")
	case "screenshot":
		add("output_path")
	}
	request := make(map[string]any, len(args))
	for key, value := range args {
		if !allowed[key] {
			return nil, fmt.Errorf("desktop action %q does not accept %q", action, key)
		}
		request[key] = value
	}
	for _, key := range []string{"application", "target", "node_id", "action_name", "text", "key", "output_path"} {
		if value, supplied := request[key]; supplied {
			text, valid := value.(string)
			if !valid || strings.ContainsRune(text, '\x00') || len(text) > 16384 {
				return nil, fmt.Errorf("desktop %s must be bounded text without NUL", key)
			}
		}
	}
	if value, supplied := request["enabled"]; supplied {
		if _, valid := value.(bool); !valid {
			return nil, fmt.Errorf("desktop enabled must be a boolean")
		}
	}
	if value, supplied := request["window_id"]; supplied {
		id, valid := value.(string)
		if !valid || len(id) > 20 {
			return nil, fmt.Errorf("window_id must be an observed hexadecimal or decimal string")
		}
		base := 10
		if strings.HasPrefix(id, "0x") {
			base, id = 16, id[2:]
		}
		n, err := strconv.ParseUint(id, base, 32)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("window_id must be an observed nonzero X11 window ID")
		}
	}
	for _, field := range []struct {
		key      string
		min, max int
	}{
		{"x", 0, 32767}, {"y", 0, 32767}, {"width", 64, 32767}, {"height", 64, 32767},
		{"to_x", 0, 32767}, {"to_y", 0, 32767}, {"delta_x", -20, 20}, {"delta_y", -20, 20},
		{"button", 1, 3}, {"count", 1, 3}, {"max_nodes", 1, 400}, {"max_depth", 1, 16}, {"action_index", 0, 31},
	} {
		if value, supplied := request[field.key]; supplied {
			number, err := desktopInteger(value, field.min, field.max)
			if err != nil {
				return nil, fmt.Errorf("desktop %s: %w", field.key, err)
			}
			request[field.key] = number
		}
	}
	require := func(keys ...string) error {
		for _, key := range keys {
			value, present := request[key]
			if !present || value == "" {
				return fmt.Errorf("desktop %s requires %s", action, key)
			}
		}
		return nil
	}
	switch action {
	case "focus", "close", "minimize", "maximize", "move_resize", "accessibility_action", "set_text", "click", "move_pointer", "scroll", "drag", "type_text", "key":
		if err := require("window_id"); err != nil {
			return nil, err
		}
	}
	switch action {
	case "move_resize":
		if err := require("x", "y", "width", "height"); err != nil {
			return nil, err
		}
	case "click", "move_pointer":
		if err := require("x", "y"); err != nil {
			return nil, err
		}
	case "drag":
		if err := require("x", "y", "to_x", "to_y"); err != nil {
			return nil, err
		}
	case "scroll":
		if request["delta_x"] == nil && request["delta_y"] == nil {
			return nil, fmt.Errorf("scroll requires delta_x or delta_y")
		}
	case "type_text", "set_text":
		if _, supplied := request["text"]; !supplied {
			return nil, fmt.Errorf("desktop %s requires text", action)
		}
		if action == "set_text" {
			if err := require("node_id"); err != nil {
				return nil, err
			}
		}
	case "key":
		if err := require("key"); err != nil {
			return nil, err
		}
		key := strArg(request, "key")
		if len(key) > 128 || strings.ContainsAny(key, " \t\r\n") {
			return nil, fmt.Errorf("key must be one bounded keysym combination")
		}
	case "accessibility_action":
		if err := require("node_id"); err != nil {
			return nil, err
		}
		_, hasIndex := request["action_index"]
		if (strArg(request, "action_name") != "") == hasIndex {
			return nil, fmt.Errorf("accessibility_action requires exactly one of action_name or action_index")
		}
	case "launch", "open":
		if action == "launch" && strArg(request, "application") == "" {
			return nil, fmt.Errorf("launch requires application")
		}
		if action == "open" && strArg(request, "target") == "" {
			return nil, fmt.Errorf("open requires target")
		}
		if target := strArg(request, "target"); target != "" {
			resolved, err := t.resolveTarget(target)
			if err != nil {
				return nil, err
			}
			request["target"] = resolved
		}
	case "screenshot":
		if err := require("output_path"); err != nil {
			return nil, err
		}
		path, err := t.WS.ResolveArtifact(strArg(request, "output_path"))
		if err != nil {
			return nil, err
		}
		if err := t.WS.CheckWritable(path); err != nil {
			return nil, err
		}
		if _, _, err := t.WS.validationFilePath(path); err != nil {
			return nil, err
		}
		if strings.ToLower(filepath.Ext(path)) != ".png" {
			return nil, fmt.Errorf("screenshot output_path must end in .png")
		}
		if _, err := os.Lstat(path); err == nil {
			return nil, fmt.Errorf("screenshot output_path already exists; choose a new path")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		request["output_path"] = path
	}
	return request, nil
}

func desktopInteger(value any, minimum, maximum int) (int, error) {
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case int:
		number = float64(value)
	case json.Number:
		var err error
		number, err = value.Float64()
		if err != nil {
			return 0, fmt.Errorf("integer required")
		}
	default:
		return 0, fmt.Errorf("integer required")
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < float64(minimum) || number > float64(maximum) {
		return 0, fmt.Errorf("integer must be between %d and %d", minimum, maximum)
	}
	return int(number), nil
}

func (t *DesktopControl) resolveTarget(target string) (string, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("invalid desktop target: %w", err)
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		if parsed.Host == "" {
			return "", fmt.Errorf("desktop URL requires a host")
		}
		return target, nil
	}
	if parsed.Scheme == "file" {
		if parsed.Host != "" && parsed.Host != "localhost" {
			return "", fmt.Errorf("file target must be local")
		}
		target = parsed.Path
	} else if parsed.Scheme != "" {
		return "", fmt.Errorf("desktop target supports only authorized paths and HTTP(S) URLs")
	}
	path, err := t.WS.Resolve(target)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("desktop target: %w", err)
	}
	return path, nil
}

type desktopBoundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *desktopBoundedOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *desktopBoundedOutput) Write(p []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		if remaining > 0 {
			_, _ = b.buffer.Write(p[:remaining])
		}
		b.exceeded = true
		return len(p), nil
	}
	return b.buffer.Write(p)
}

func (t *DesktopControl) Execute(ctx context.Context, args map[string]any) Result {
	request, err := t.prepare(args)
	if err != nil {
		return errResult(err)
	}
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	ctx, cancel := context.WithTimeout(ctx, desktopControlTimeout)
	defer cancel()
	t.Session.Lock()
	defer t.Session.Unlock()
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	if !filepath.IsAbs(t.Session.HelperPath) || !filepath.IsAbs(t.Session.PythonPath) {
		return errResultf("desktop_unavailable: installed desktop adapter is not configured")
	}
	input, err := json.Marshal(request)
	if err != nil {
		return errResult(err)
	}
	command := exec.CommandContext(ctx, t.Session.PythonPath, t.Session.HelperPath)
	command.Env = t.Session.Environment()
	command.Dir = t.WS.Root
	command.Stdin = bytes.NewReader(input)
	command.WaitDelay = 2 * time.Second
	output := &desktopBoundedOutput{limit: desktopOutputLimit}
	stderr := &desktopBoundedOutput{limit: 4096}
	command.Stdout, command.Stderr = output, stderr
	runErr := command.Run()
	if err := ctx.Err(); err != nil {
		return errResult(fmt.Errorf("desktop call interrupted: %w", err))
	}
	if output.exceeded {
		return errResultf("desktop adapter exceeded its bounded output contract")
	}
	var reply map[string]any
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	if err := decoder.Decode(&reply); err != nil {
		return errResultf("desktop adapter returned invalid JSON (%v)", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errResultf("desktop adapter returned multiple output records")
	}
	ok, valid := reply["ok"].(bool)
	if !valid || reply["action"] != request["action"] || reply["display"] != t.Session.Display {
		return errResultf("desktop adapter returned mismatched action or desktop identity")
	}
	content, err := json.Marshal(reply)
	if err != nil {
		return errResult(err)
	}
	result := Result{LLMContent: string(content), Display: "Desktop " + strArg(request, "action"), BehaviorEvidence: reply}
	if !ok || runErr != nil {
		message := "desktop adapter action failed"
		if details, valid := reply["error"].(map[string]any); valid {
			if reported, valid := details["message"].(string); valid && reported != "" {
				message = reported
			}
		}
		if runErr != nil && ok {
			message = "desktop adapter exited unsuccessfully after reporting an observation"
		}
		result.Err = fmt.Errorf("%s", message)
		result.Display = message
		return result
	}
	if request["action"] == "screenshot" {
		path := strArg(request, "output_path")
		observation, err := t.WS.SnapshotValidationFile(path)
		if err != nil {
			return errResultf("desktop screenshot was not saved as reported: %w", err)
		}
		screenshot, valid := reply["screenshot"].(map[string]any)
		if !valid || screenshot["path"] != path || screenshot["sha256"] != observation.SHA256 || screenshot["display"] != t.Session.Display {
			return errResultf("desktop screenshot metadata differs from the saved pixels or bound display")
		}
		result.ChangedPaths = []string{t.WS.ModelPath(path)}
		result.FileObservations = []FileObservation{observation}
		t.WS.NoteRead(path)
	}
	return result
}
