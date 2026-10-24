package tools

import (
	"fmt"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
	diffContextLines = 3
	diffMaxLines     = 80
)

type displayDiffLine struct {
	prefix byte
	text   string
}

// boundedUnifiedDiff returns a compact line-oriented mutation preview. It is
// deliberately a display artifact rather than a model message: large writes
// must not inflate the next prompt, and unchanged regions are collapsed.
func boundedUnifiedDiff(path, before, after string) string {
	if before == after {
		return ""
	}
	dmp := diffmatchpatch.New()
	left, right, lines := dmp.DiffLinesToRunes(before, after)
	diffs := dmp.DiffMainRunes(left, right, false)
	diffs = dmp.DiffCharsToLines(diffs, lines)

	var expanded []displayDiffLine
	for _, diff := range diffs {
		prefix := byte(' ')
		switch diff.Type {
		case diffmatchpatch.DiffDelete:
			prefix = '-'
		case diffmatchpatch.DiffInsert:
			prefix = '+'
		}
		for _, line := range splitDiffLines(diff.Text) {
			expanded = append(expanded, displayDiffLine{prefix: prefix, text: line})
		}
	}
	visible := selectDiffContext(expanded, diffContextLines, diffMaxLines)
	if len(visible) == 0 {
		return ""
	}

	var out strings.Builder
	displayPath := strings.TrimPrefix(strings.TrimPrefix(path, "/workspace/"), "/")
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", displayPath, displayPath)
	out.WriteString("@@\n")
	for _, line := range visible {
		if line.prefix == 0 {
			out.WriteString("…\n")
			continue
		}
		out.WriteByte(line.prefix)
		out.WriteString(line.text)
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n")
}

func splitDiffLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func selectDiffContext(lines []displayDiffLine, context, limit int) []displayDiffLine {
	selected := make([]bool, len(lines))
	for index, line := range lines {
		if line.prefix == ' ' {
			continue
		}
		start := max(0, index-context)
		end := min(len(lines), index+context+1)
		for i := start; i < end; i++ {
			selected[i] = true
		}
	}
	result := make([]displayDiffLine, 0, min(len(lines), limit))
	omitted := false
	for index, line := range lines {
		if !selected[index] {
			omitted = len(result) > 0
			continue
		}
		if omitted {
			result = append(result, displayDiffLine{})
			omitted = false
		}
		result = append(result, line)
		if len(result) >= limit {
			result = append(result, displayDiffLine{})
			break
		}
	}
	return result
}
