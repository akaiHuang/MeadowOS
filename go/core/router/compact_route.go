package router

import (
	"fmt"
	"regexp"
	"strings"
)

// CompactRoute is Meadow Call's small-model route-line contract:
// intent=<taxonomy_id>; tools=<tool_chain>; safety=<safety>; confirm=<bool>.
// It is deliberately smaller than a planner JSON object; runtime code expands
// it through deterministic tool maps and permission gates.
type CompactRoute struct {
	Intent  IntentID
	Tools   []string
	Safety  string
	Confirm bool
}

var compactToolSplitRe = regexp.MustCompile(`[>,|/\s]+`)

type IntentCodeSpec struct {
	Code    string
	Intent  IntentID
	Tools   []string
	Safety  string
	Confirm bool
}

var compactIntentCodeSpecs = map[string]IntentCodeSpec{
	"WA": {Code: "WA", Intent: IntentStaticWebArtifact, Tools: []string{ToolRead, ToolWrite, ToolShell}, Safety: "write_project_file"},
	"TS": {Code: "TS", Intent: IntentEngineeringFix, Tools: []string{ToolRead, ToolGrep, ToolEdit, ToolShell}, Safety: "modify_project_file"},
	"BR": {Code: "BR", Intent: IntentInternetResearch, Tools: []string{ToolWebSearch, ToolWebFetch, ToolWrite}, Safety: "network_fetch"},
	"DG": {Code: "DG", Intent: IntentDeleteGenerated, Tools: []string{ToolList, ToolAskUser, ToolShell}, Safety: "destructive_confirm", Confirm: true},
	"PR": {Code: "PR", Intent: IntentOfficeArtifact, Tools: []string{ToolRead, ToolWrite, ToolShell}, Safety: "write_project_file"},
	"SR": {Code: "SR", Intent: IntentOfficeArtifact, Tools: []string{ToolRead, ToolWrite, ToolShell}, Safety: "write_project_file"},
	"FW": {Code: "FW", Intent: IntentSimpleWrite, Tools: []string{ToolWrite, ToolShell}, Safety: "write_project_file"},
	"CF": {Code: "CF", Intent: IntentConfigArtifact, Tools: []string{ToolWrite, ToolShell}, Safety: "write_project_file"},
	"IP": {Code: "IP", Intent: IntentInspectOrSearch, Tools: []string{ToolList, ToolGlob, ToolGrep, ToolRead}, Safety: "read_only"},
	"GA": {Code: "GA", Intent: IntentGitAction, Tools: []string{ToolShell}, Safety: "execute_command"},
	"SH": {Code: "SH", Intent: IntentShellScript, Tools: []string{ToolWrite, ToolShell}, Safety: "write_project_file"},
	"CP": {Code: "CP", Intent: IntentSimpleCopy, Tools: []string{ToolRead, ToolWrite}, Safety: "write_project_file"},
	"AF": {Code: "AF", Intent: IntentSimpleAppend, Tools: []string{ToolRead, ToolWrite}, Safety: "modify_project_file"},
	"DC": {Code: "DC", Intent: IntentDirectoryCreate, Tools: []string{ToolShell}, Safety: "write_project_file"},
	"TP": {Code: "TP", Intent: IntentGeneralAction, Tools: []string{"todo_write"}, Safety: "read_only"},
	"AQ": {Code: "AQ", Intent: IntentGeneralAction, Tools: []string{ToolAskUser}, Safety: "interactive_confirm"},
	"GT": {Code: "GT", Intent: IntentEngineeringFix, Tools: []string{ToolRead, ToolWrite, ToolShell}, Safety: "write_project_file"},
	"RF": {Code: "RF", Intent: IntentEngineeringFix, Tools: []string{ToolRead, ToolGrep, ToolEdit, ToolShell}, Safety: "modify_project_file"},
	"DM": {Code: "DM", Intent: IntentEngineeringFix, Tools: []string{ToolShell}, Safety: "modify_project_file"},
	"ES": {Code: "ES", Intent: IntentShellScript, Tools: []string{ToolWrite, ToolShell}, Safety: "write_project_file"},
	"PK": {Code: "PK", Intent: IntentGitAction, Tools: []string{ToolShell}, Safety: "execute_command"},
	"CL": {Code: "CL", Intent: IntentOfficeArtifact, Tools: []string{ToolRead, ToolWrite, ToolShell}, Safety: "write_project_file"},
	"SS": {Code: "SS", Intent: IntentOfficeArtifact, Tools: []string{ToolRead, ToolWrite, ToolShell}, Safety: "write_project_file"},
	"MF": {Code: "MF", Intent: IntentOfficeArtifact, Tools: []string{ToolRead, ToolWrite, ToolShell}, Safety: "write_project_file"},
	"ED": {Code: "ED", Intent: IntentOfficeArtifact, Tools: []string{ToolRead, ToolWrite}, Safety: "write_project_file"},
	"LA": {Code: "LA", Intent: IntentInspectOrSearch, Tools: []string{ToolGrep, ToolRead, ToolWrite}, Safety: "read_only"},
	"AC": {Code: "AC", Intent: IntentShellScript, Tools: []string{ToolWrite, ToolShell}, Safety: "write_project_file"},
	"DI": {Code: "DI", Intent: IntentResearchArtifact, Tools: []string{ToolWebSearch, ToolWebFetch, ToolDownload}, Safety: "network_fetch"},
	"NO": {Code: "NO", Intent: IntentID("none"), Tools: nil, Safety: "none"},
}

// CompactRouteLine renders the deterministic router plan in the same compact
// shape the router LoRA is trained to emit. This is used for shadow telemetry
// and later for predictor A/B comparisons.
func CompactRouteLine(plan *Plan, tools []string) string {
	if plan == nil {
		return ""
	}
	return fmt.Sprintf(
		"intent=%s; tools=%s; safety=%s; confirm=%t",
		plan.ID,
		strings.Join(normalizeCompactTools(tools), ">"),
		compactSafety(plan.ID),
		compactRequiresConfirmation(plan.ID),
	)
}

// ParseCompactRouteLine extracts the first route-looking line from text and
// normalizes known model aliases into Meadow Go router/tool IDs.
func ParseCompactRouteLine(text string) (CompactRoute, bool) {
	line := firstCompactRouteLine(text)
	if line == "" {
		return CompactRoute{}, false
	}
	if spec, ok := CompactIntentCodeSpec(line); ok {
		return CompactRoute{
			Intent:  spec.Intent,
			Tools:   normalizeCompactTools(spec.Tools),
			Safety:  spec.Safety,
			Confirm: spec.Confirm,
		}, true
	}
	fields := map[string]string{}
	for _, part := range strings.Split(line, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	tools := normalizeCompactTools(splitCompactTools(firstNonEmptyCompactField(fields, "tools", "t", "letters", "tool_codes")))
	rawIntent := firstNonEmptyCompactField(fields, "intent", "i", "code")
	if rawIntent == "" {
		rawIntent = string(inferCompactIntentFromTools(tools))
	}
	if rawIntent == "" {
		return CompactRoute{}, false
	}
	return CompactRoute{
		Intent:  NormalizeCompactIntent(rawIntent),
		Tools:   tools,
		Safety:  firstNonEmptyCompactField(fields, "safety", "s"),
		Confirm: parseCompactBool(firstNonEmptyCompactField(fields, "confirm", "c")),
	}, true
}

func firstCompactRouteLine(text string) string {
	for _, stop := range []string{"<|im_end|>", "</s>"} {
		if idx := strings.Index(text, stop); idx >= 0 {
			text = text[:idx]
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if _, ok := CompactIntentCodeSpec(line); ok {
			return strings.ToUpper(line)
		}
		if strings.HasPrefix(line, "intent=") ||
			strings.HasPrefix(line, "i=") ||
			strings.HasPrefix(line, "code=") ||
			strings.HasPrefix(line, "tools=") ||
			strings.HasPrefix(line, "t=") ||
			strings.HasPrefix(line, "letters=") ||
			strings.HasPrefix(line, "tool_codes=") {
			return line
		}
	}
	text = strings.TrimSpace(text)
	if _, ok := CompactIntentCodeSpec(text); ok {
		return strings.ToUpper(text)
	}
	if strings.HasPrefix(text, "intent=") ||
		strings.HasPrefix(text, "i=") ||
		strings.HasPrefix(text, "code=") ||
		strings.HasPrefix(text, "tools=") ||
		strings.HasPrefix(text, "t=") ||
		strings.HasPrefix(text, "letters=") ||
		strings.HasPrefix(text, "tool_codes=") {
		return text
	}
	return ""
}

func parseCompactBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

func CompactIntentCodeSpec(code string) (IntentCodeSpec, bool) {
	key := strings.ToUpper(strings.TrimSpace(code))
	spec, ok := compactIntentCodeSpecs[key]
	if !ok {
		return IntentCodeSpec{}, false
	}
	return spec, true
}

func firstNonEmptyCompactField(fields map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(fields[key]); value != "" {
			return value
		}
	}
	return ""
}

func splitCompactTools(value string) []string {
	var out []string
	for _, item := range compactToolSplitRe.Split(value, -1) {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// NormalizeCompactIntent maps observed LoRA/taxonomy aliases into the Go
// router's current intent IDs. Unknown IDs are preserved for diagnostics.
func NormalizeCompactIntent(intent string) IntentID {
	raw := strings.TrimSpace(intent)
	if spec, ok := CompactIntentCodeSpec(raw); ok {
		return spec.Intent
	}
	key := IntentID(raw)
	switch key {
	case "simple_file_write", "write_file", "save_file", "note_write":
		return IntentSimpleWrite
	case "copy_file":
		return IntentSimpleCopy
	case "append_file":
		return IntentSimpleAppend
	case "create_directory", "directory_action":
		return IntentDirectoryCreate
	case "create_web_app", "static_web_app":
		return IntentStaticWebArtifact
	case "browser_research", "web_search":
		return IntentInternetResearch
	case "browser_research_artifact", "research_artifact", "shopping_research", "travel_itinerary", "outfit_research":
		return IntentResearchArtifact
	case "delete_generated_files", "delete_file", "cleanup_generated_files":
		return IntentDeleteGenerated
	case "generate_report", "slider_report", "office_artifact":
		return IntentOfficeArtifact
	case "refactor_code":
		return IntentEngineeringFix
	case "package_update", "dependency_management":
		return IntentEngineeringFix
	default:
		return key
	}
}

// NormalizeCompactTool maps compact-router subtool aliases into registered
// Meadow tool names. Unknown names are preserved for diagnostics.
func NormalizeCompactTool(tool string) string {
	raw := strings.TrimSpace(tool)
	key := strings.ToLower(raw)
	switch key {
	case "r", "read", "read_file":
		return ToolRead
	case "f", "file", "write", "write_file", "save":
		return ToolWrite
	case "e", "edit", "edit_file", "patch":
		return ToolEdit
	case "t", "ts", "typecheck", "test", "build", "run", "shell", "run_shell", "run_shell_command", "command":
		return ToolShell
	case "l", "list", "list_directory", "ls":
		return ToolList
	case "g", "grep", "grep_search", "search_files":
		return ToolGrep
	case "o", "glob":
		return ToolGlob
	case "q", "ask", "ask_user", "ask_user_question":
		return ToolAskUser
	case "y", "todo", "todo_write":
		return "todo_write"
	case "v", "visual", "visual_check", "browser_visual", "meadow_browser_visual_check":
		return ToolVisual
	case "s", "search", "web_search":
		return ToolWebSearch
	case "c", "crawl", "web_crawl":
		return ToolWebCrawl
	case "w", "fetch", "web_fetch":
		return ToolWebFetch
	case "b", "browse", "browse_page":
		return ToolBrowsePage
	case "d", "download", "image_download", "download_image", "download_image_file":
		return ToolDownload
	case "a", "artifact", "research", "research_artifact", "save_research_artifact", "render_research_artifact":
		return ToolResearch
	case "browser_research":
		return ToolWebSearch
	case "delete_file", "git_checkout", "git_commit", "git_status",
		"install_package", "npm_install", "package_install":
		return ToolShell
	case "todo_check", "todo_planning", "todo_write_file", "todo_write_web":
		return "todo_write"
	default:
		return raw
	}
}

func inferCompactIntentFromTools(tools []string) IntentID {
	if len(tools) == 0 {
		return ""
	}
	has := func(name string) bool {
		for _, tool := range tools {
			if tool == name {
				return true
			}
		}
		return false
	}
	hasAny := func(names ...string) bool {
		for _, name := range names {
			if has(name) {
				return true
			}
		}
		return false
	}
	hasWeb := hasAny(ToolWebSearch, ToolWebCrawl, ToolWebFetch, ToolBrowsePage)
	if has(ToolResearch) || (hasWeb && hasAny(ToolWrite, ToolDownload)) {
		return IntentResearchArtifact
	}
	if hasWeb {
		return IntentInternetResearch
	}
	if has(ToolVisual) || (has(ToolWrite) && hasAny(ToolEdit, ToolShell)) {
		return IntentStaticWebArtifact
	}
	if has(ToolWrite) {
		return IntentSimpleWrite
	}
	if has(ToolShell) {
		return IntentShellScript
	}
	return ""
}

func normalizeCompactTools(tools []string) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		normalized := NormalizeCompactTool(tool)
		if normalized == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1] == normalized {
			continue
		}
		out = append(out, normalized)
	}
	return out
}

func compactSafety(intent IntentID) string {
	switch intent {
	case IntentInspectOrSearch:
		return "read_only"
	case IntentInternetResearch, IntentResearchArtifact:
		return "network_fetch"
	case IntentDeleteGenerated:
		return "destructive_confirm"
	case IntentGitAction, IntentShellScript:
		return "execute_command"
	case IntentEngineeringFix, IntentSimpleAppend:
		return "modify_project_file"
	default:
		return "write_project_file"
	}
}

func compactRequiresConfirmation(intent IntentID) bool {
	return intent == IntentDeleteGenerated
}
