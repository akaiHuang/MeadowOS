package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/reflow/wordwrap"

	"github.com/hey-meadow/meadow-code/go/cli/theme"
)

// activityAnchor stores one stable viewport line for the live activity block.
// View expands it at frame time and layout reserves the extra rendered rows,
// so animation ticks never rebuild markdown, enter scrollback, or change
// persisted chat history.
const activityAnchor = "__MEADOW_LIVE_ACTIVITY_ANCHOR__"

// entryKind tags one transcript entry.
type entryKind int

const (
	entryUser entryKind = iota
	entryAssistant
	entryTool
	entrySystem
	// entryBanner is pre-colored art rendered verbatim (startup meadow).
	entryBanner
	// entryThought is a completed turn's musing, kept in history dimmed.
	entryThought
	// entrySteer is user guidance injected mid-run (next turn boundary).
	entrySteer
	// entryQueued is an independent user task waiting for the active run.
	entryQueued
)

type entry struct {
	kind entryKind
	text string // user text / assistant markdown / system notice
	// interactive guidance / queued-task fields
	guidanceID          string
	guidanceDisposition string
	guidanceTargetTurn  int
	queuePosition       int
	// tool fields
	toolID         string
	toolName       string
	toolSummary    string
	toolDetail     string
	toolDiff       string
	toolOK         bool
	toolDone       bool
	toolStarted    time.Time
	toolDurationMS int64
}

// renderTranscript builds the scrollback content for the viewport.
// thinking is the in-flight thought-channel text, rendered dimmed so it
// reads as background musing rather than the answer.
func renderTranscript(entries []entry, streaming, thinking string, st theme.Styles, md *glamour.TermRenderer, width int) string {
	return renderTranscriptWithActivity(entries, streaming, thinking, st, md, width, false)
}

func renderTranscriptWithActivity(
	entries []entry,
	streaming, thinking string,
	st theme.Styles,
	md *glamour.TermRenderer,
	width int,
	showActivity bool,
) string {
	var b strings.Builder
	activityAfter := -1
	if showActivity {
		for i := range entries {
			if entries[i].kind == entryUser || entries[i].kind == entrySteer || entries[i].kind == entryQueued {
				activityAfter = i
			}
		}
	}
	for i := range entries {
		e := &entries[i]
		switch e.kind {
		case entryBanner:
			b.WriteString(e.text + "\n")
		case entryUser:
			b.WriteString(st.Prompt.Render("› ") + st.Bright.Render(wrapTerminalText(e.text, width-4)) + "\n")
			if i == activityAfter {
				b.WriteString(activityAnchor + "\n")
			}
			b.WriteString("\n")
		case entrySteer:
			state := "⤷ 已收到，等待安全邊界"
			switch e.guidanceDisposition {
			case "applied":
				state = fmt.Sprintf("⤷ 已套用至第 %d 輪", e.guidanceTargetTurn)
			case "cancelled":
				state = "⤷ 本輪已結束，未套用"
			case "rejected":
				state = "⤷ 未套用"
			}
			b.WriteString(st.Prompt.Render("› ") + st.Bright.Render(wrapTerminalText(e.text, width-4)) +
				st.Dim.Render("  "+state) + "\n")
			if i == activityAfter {
				b.WriteString(activityAnchor + "\n")
			}
			b.WriteString("\n")
		case entryQueued:
			state := fmt.Sprintf("⏳ 已排入下一題 · #%d", e.queuePosition)
			if e.guidanceDisposition == "started" {
				state = "▶ 已從佇列開始執行"
			} else if e.guidanceDisposition == "cancelled" {
				state = "⊘ 已從佇列取消"
			}
			b.WriteString(st.Prompt.Render("› ") + st.Bright.Render(wrapTerminalText(e.text, width-4)) +
				st.Dim.Render("  "+state) + "\n")
			if i == activityAfter {
				b.WriteString(activityAnchor + "\n")
			}
			b.WriteString("\n")
		case entryAssistant:
			b.WriteString(renderMarkdown(e.text, st, md) + "\n")
		case entryTool:
			b.WriteString(renderToolLine(*e, st, width) + "\n")
			if strings.TrimSpace(e.toolDiff) != "" {
				b.WriteString(renderToolDiff(e.toolDiff, st, width) + "\n")
			}
		case entrySystem:
			b.WriteString(st.Dim.Render(wrapTerminalText(e.text, width-2)) + "\n\n")
		case entryThought:
			b.WriteString(st.Dim.Render("· 思考 ·") + "\n" +
				st.Dim.Italic(true).Render(indentLines(wrapTerminalText(strings.TrimSpace(e.text), width-4))) + "\n\n")
		}
	}
	if thinking != "" && streaming == "" {
		b.WriteString(st.Dim.Render("· 思考中 ·") + "\n" +
			st.Dim.Italic(true).Render(indentLines(wrapTerminalText(strings.TrimSpace(thinking), width-4))) + "\n")
	}
	if streaming != "" {
		// Raw text while streaming; glamour pass happens when the turn ends.
		b.WriteString(st.Text.Render(wrapTerminalText(strings.TrimRight(streaming, "\n"), width-2)) + "\n")
	}
	return b.String()
}

func wrapTerminalText(text string, width int) string {
	if width < 12 {
		width = 12
	}
	lines := strings.Split(text, "\n")
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		for _, softLine := range strings.Split(wordwrap.String(line, width), "\n") {
			wrapped = append(wrapped, hardWrapDisplayLine(softLine, width)...)
		}
	}
	return strings.Join(wrapped, "\n")
}

func hardWrapDisplayLine(line string, width int) []string {
	if runewidth.StringWidth(line) <= width {
		return []string{line}
	}
	result := make([]string, 0, 2)
	var current strings.Builder
	currentWidth := 0
	for _, value := range line {
		cellWidth := runewidth.RuneWidth(value)
		if cellWidth < 0 {
			cellWidth = 1
		}
		if currentWidth > 0 && currentWidth+cellWidth > width {
			result = append(result, current.String())
			current.Reset()
			currentWidth = 0
		}
		current.WriteRune(value)
		currentWidth += cellWidth
	}
	result = append(result, current.String())
	return result
}

func indentLines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

func renderToolLine(e entry, st theme.Styles, width int) string {
	marker := st.Warn.Render("⚒")
	if e.toolDone {
		if e.toolOK {
			marker = st.Success.Render("✓")
		} else if softUnavailableTool(e) {
			marker = st.Warn.Render("↷")
		} else {
			marker = st.Error.Render("✗")
		}
	}
	summary := e.toolSummary
	if detail := strings.TrimSpace(e.toolDetail); detail != "" && detail != strings.TrimSpace(summary) {
		if summary != "" {
			summary += " — "
		}
		summary += detail
	}
	duration := ""
	if e.toolDone && e.toolDurationMS > 0 {
		duration = fmt.Sprintf(" · %s", formatToolDuration(time.Duration(e.toolDurationMS)*time.Millisecond))
	} else if !e.toolStarted.IsZero() {
		duration = fmt.Sprintf(" · %s", formatToolDuration(time.Since(e.toolStarted)))
	}
	if max := width - len(e.toolName) - len(duration) - 8; max > 10 && len(summary) > max {
		summary = summary[:max] + "…"
	}
	summary += duration
	return fmt.Sprintf("%s %s %s", marker, st.Secondary.Render(e.toolName), st.Dim.Render(summary))
}

func softUnavailableTool(e entry) bool {
	if e.toolName != "meadow_browser_visual_check" {
		return false
	}
	text := strings.ToLower(e.toolSummary + " " + e.toolDetail)
	return strings.Contains(text, "browser unavailable") ||
		strings.Contains(text, "browser 未啟動") ||
		strings.Contains(text, "browser 無法連接")
}

func renderToolDiff(diff string, st theme.Styles, width int) string {
	var out strings.Builder
	out.WriteString(st.Dim.Render("  review diff") + "\n")
	for _, line := range strings.Split(strings.TrimSpace(diff), "\n") {
		line = oneLine(line, max(width-4, 20))
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@"):
			out.WriteString(st.Dim.Render("  "+line) + "\n")
		case strings.HasPrefix(line, "+"):
			out.WriteString(st.Success.Render("  "+line) + "\n")
		case strings.HasPrefix(line, "-"):
			out.WriteString(st.Error.Render("  "+line) + "\n")
		default:
			out.WriteString(st.Dim.Render("  "+line) + "\n")
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

func formatToolDuration(duration time.Duration) string {
	if duration < time.Second {
		return fmt.Sprintf("%dms", duration.Round(time.Millisecond).Milliseconds())
	}
	return fmt.Sprintf("%.1fs", duration.Seconds())
}

func renderMarkdown(text string, st theme.Styles, md *glamour.TermRenderer) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if md != nil {
		if out, err := md.Render(text); err == nil {
			return strings.Trim(out, "\n") + "\n"
		}
	}
	return st.Text.Render(text) + "\n"
}

// newMarkdownRenderer builds a glamour renderer for the current width,
// tinting headings/links with the theme accent so assistant output has
// visible structure instead of one flat text color.
func newMarkdownRenderer(width int, th theme.Theme) *glamour.TermRenderer {
	cfg := styles.DarkStyleConfig
	accent := string(th.Accent)
	secondary := string(th.Secondary)
	cfg.H1.Color = &accent
	cfg.H1.BackgroundColor = nil
	cfg.H2.Color = &accent
	cfg.H3.Color = &accent
	cfg.H4.Color = &secondary
	cfg.Link.Color = &accent
	cfg.Strong.Color = &secondary
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(min(width, 100)),
		glamour.WithEmoji(),
	)
	if err != nil {
		return nil
	}
	return r
}

// toolCallSummary compacts tool args into the one-line transcript form.
func toolCallSummary(name string, args map[string]any) string {
	get := func(k string) string { v, _ := args[k].(string); return v }
	switch name {
	case "read_file", "write_file", "edit":
		return shortPath(get("file_path"))
	case "run_shell_command":
		return oneLine(get("command"), 60)
	case "glob", "grep_search":
		return get("pattern")
	case "list_directory":
		return shortPath(get("path"))
	case "web_fetch":
		return oneLine(get("url"), 60)
	case "web_search":
		return oneLine(get("query"), 60)
	default:
		return ""
	}
}

func shortPath(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(p), "/")
	if len(parts) <= 3 {
		return p
	}
	return "…/" + strings.Join(parts[len(parts)-2:], "/")
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1fKB", float64(n)/1024)
}
