package cli

import (
	"fmt"
	"strings"

	"github.com/hey-meadow/meadow-code/go/cli/theme"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const confirmPreviewLines = 14

// renderConfirm draws the pending tool-call confirmation box:
// header, a content preview (diff for edit, head of content for write,
// the command for shell), and the available decision key hints.
func renderConfirm(req engine.ConfirmRequest, st theme.Styles, width int) string {
	var b strings.Builder
	get := func(k string) string { v, _ := req.Args[k].(string); return v }

	b.WriteString(st.Warn.Render("⚒ ") + st.Secondary.Render(req.ToolName))
	switch req.ToolName {
	case "edit":
		b.WriteString(" " + st.Dim.Render(get("file_path")) + "\n")
		for _, line := range previewLines(get("old_string")) {
			b.WriteString(st.DiffRemove.Render("- "+line) + "\n")
		}
		for _, line := range previewLines(get("new_string")) {
			b.WriteString(st.DiffAdd.Render("+ "+line) + "\n")
		}
	case "write_file":
		content := get("content")
		lines := strings.Split(content, "\n")
		b.WriteString(" " + st.Dim.Render(get("file_path")) +
			st.Dim.Render(fmt.Sprintf("  (%d 行)", len(lines))) + "\n")
		for _, line := range previewLines(content) {
			b.WriteString(st.DiffAdd.Render("+ "+line) + "\n")
		}
	case "run_shell_command":
		b.WriteString("\n" + st.Bright.Render("$ "+get("command")) + "\n")
		if script := get("script"); script != "" {
			b.WriteString(st.Dim.Render("  script:") + "\n")
			for _, line := range previewLines(script) {
				b.WriteString(st.Bright.Render("  "+line) + "\n")
			}
		}
		if d := get("directory"); d != "" {
			b.WriteString(st.Dim.Render("  in "+d) + "\n")
		}
	case "web_fetch":
		b.WriteString("\n" + st.Bright.Render(get("url")) + "\n")
	case "web_search":
		b.WriteString("\n" + st.Bright.Render("search: "+get("query")) + "\n")
	default:
		b.WriteString("\n")
		for k, v := range req.Args {
			b.WriteString(st.Dim.Render(fmt.Sprintf("%s: %v", k, oneLine(fmt.Sprintf("%v", v), 70))) + "\n")
		}
	}

	b.WriteString("\n" + st.Dim.Render("允許這次操作？ ") +
		st.Success.Render("[y] 允許") + "  ")
	if req.Kind != tools.KindDestructive {
		b.WriteString(st.Warn.Render("[a] 一律允許") + "  ")
	}
	b.WriteString(st.Error.Render("[n] 拒絕"))

	box := st.ToolBox.Width(min(width-2, 100))
	return box.Render(b.String())
}

func previewLines(s string) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > confirmPreviewLines {
		kept := lines[:confirmPreviewLines]
		kept = append(kept, fmt.Sprintf("… (+%d 行)", len(lines)-confirmPreviewLines))
		return kept
	}
	return lines
}
