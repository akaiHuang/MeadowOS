package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Edit is the edit tool: exact-string replacement with the TS matching
// rules — old_string must match exactly once unless replace_all, and an
// empty old_string only ever creates a brand-new file.
type Edit struct {
	WS *Workspace
}

func (t *Edit) Name() string { return "edit" }
func (t *Edit) Description() string {
	return "Make one minimal exact replacement in a local file. Normally copy only 1-3 unique contiguous old lines from the latest read; do not resend the unchanged file."
}
func (t *Edit) Kind() Kind { return KindEdit }

// NormalizeArguments repairs the lossless scalar shape produced by streamed
// tagged tool calls. The tagged transport carries parameter bodies as text, so
// a model-authored <parameter=replace_all>true</parameter> may otherwise fail a
// boolean schema check even though the value is unambiguous. Other spellings
// remain untouched and are rejected by the normal schema validator.
func (t *Edit) NormalizeArguments(args map[string]any) {
	raw, ok := args["replace_all"].(string)
	if !ok {
		return
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		args["replace_all"] = true
	case "false":
		args["replace_all"] = false
	}
}

func (t *Edit) Schema() map[string]any {
	return obj(map[string]any{
		"file_path":   str("Absolute path to the file."),
		"old_string":  str("Smallest exact unique snippet copied from the latest read, normally 1-3 contiguous lines. Never leave empty for an existing file."),
		"new_string":  str("Only the replacement for old_string. It is written verbatim, including indentation. It must differ; do not include the rest of the unchanged file."),
		"replace_all": boolean("Replace every matching occurrence."),
	}, "file_path", "old_string", "new_string")
}

func (t *Edit) Execute(ctx context.Context, args map[string]any) Result {
	path, err := t.WS.Resolve(pathArg(args))
	if err != nil {
		return errResult(err)
	}
	if err := t.WS.CheckWritable(path); err != nil {
		return errResult(err)
	}
	oldStr := strArg(args, "old_string")
	newStr := strArg(args, "new_string")
	if containsCompactedToolResult(newStr) {
		return errResultf("new_string contains a compacted tool-result marker; re-read the exact file range before editing")
	}

	_, statErr := os.Stat(path)
	exists := statErr == nil

	if oldStr == "" {
		if exists {
			return errResultf("old_string is empty but %s already exists; provide exact existing text in old_string to edit this file. write_file creates a new file and preserves conflicting existing files", path)
		}
		// Empty old_string on a missing file = create it with new_string,
		// matching the TS edit tool.
		return (&WriteFile{WS: t.WS}).Execute(ctx, map[string]any{
			"file_path": path,
			"content":   newStr,
		})
	}
	if !exists {
		modelPath := t.WS.ModelPath(path)
		return errResult(&RecoveryError{
			Code:           "edit_file_not_found",
			Message:        "cannot edit " + modelPath + " because the file does not exist; create that exact file with write_file and the complete generated content",
			Path:           modelPath,
			RequiredAction: "call_write_file_with_complete_content",
			RequiredArgs: map[string]any{
				"file_path": modelPath,
				"content":   "complete file content",
			},
			PreserveDraft: true,
		})
	}
	if oldStr == newStr {
		modelPath := t.WS.ModelPath(path)
		snippet := []rune(oldStr)
		if len(snippet) > 240 {
			snippet = append(snippet[:240], []rune("…[truncated]")...)
		}
		return errResult(&RecoveryError{
			Code:           "edit_no_change",
			Message:        "old_string and new_string are identical in " + modelPath + "; diff is empty; no repair was applied. Compare the proposed replacement with the original diagnostic. Reuse a current read; read again only if source context is missing or stale.",
			Path:           modelPath,
			RequiredAction: "compare_empty_diff_then_retry_edit_with_changed_new_string",
			RequiredArgs: map[string]any{
				"new_string_must_differ":              true,
				"reuse_validator_error":               true,
				"diff_empty":                          true,
				"submitted_snippet_not_disk_evidence": string(snippet),
			},
			PreserveDraft: true,
		})
	}
	if err := t.WS.CheckPriorRead(path); err != nil {
		return errResult(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return errResult(err)
	}
	content := string(b)
	n := strings.Count(content, oldStr)
	if n == 0 {
		// A unique source match does not establish the replacement's intended
		// indentation or scope. Return the observation without writing, so the
		// model can choose the replacement and resubmit an exact edit.
		if candidate, delta, ok := uniqueUniformIndentCandidate(content, oldStr); ok {
			return errResult(editIndentationRecovery(t.WS.ModelPath(path), content, candidate, delta))
		}
	}
	if n == 0 {
		modelPath := t.WS.ModelPath(path)
		return errResult(&RecoveryError{
			Code:           "edit_old_string_not_found",
			Message:        "old_string was not found in " + modelPath + "; retry with the smallest exact 1-3 contiguous lines copied from the last read",
			Path:           modelPath,
			RequiredAction: "retry_edit_with_small_exact_old_string",
			RequiredArgs: map[string]any{
				"old_string_max_lines": 3,
				"reuse_last_read":      true,
			},
			PreserveDraft: true,
		})
	}

	var updated string
	var replaced int
	if boolArg(args, "replace_all") {
		updated = strings.ReplaceAll(content, oldStr, newStr)
		replaced = n
	} else {
		if n > 1 {
			return errResultf("old_string matches %d times in %s; add surrounding context to make it unique or set replace_all", n, path)
		}
		updated = strings.Replace(content, oldStr, newStr, 1)
		replaced = 1
	}

	if err := atomicWriteFile(path, []byte(updated), 0o644); err != nil {
		return errResult(err)
	}
	t.WS.NoteRead(path)
	msg := fmt.Sprintf("Replaced %d occurrence(s) in %s", replaced, t.WS.ModelPath(path))
	return appendSourceDiagnostics(ctx, Result{
		LLMContent: msg, Display: msg,
		Diff: boundedUnifiedDiff(t.WS.ModelPath(path), content, updated),
	}, t.WS.ModelPath(path), updated)
}

const (
	editIndentCandidateMaxBytes = 4096
	editIndentCandidateMaxLines = 32
)

func editIndentationRecovery(modelPath, content, candidate string, delta int) *RecoveryError {
	lineStart := strings.Count(content[:strings.Index(content, candidate)], "\n") + 1
	lineCount := strings.Count(strings.TrimSuffix(candidate, "\n"), "\n") + 1
	recovery := &RecoveryError{
		Code:           "edit_indentation_mismatch",
		Message:        "old_string differs from one unique source block only by indentation in " + modelPath + "; no edit was applied. Choose the intended replacement indentation and retry with exact old_string. new_string is written verbatim, never automatically reindented.",
		Path:           modelPath,
		RequiredAction: "retry_edit_with_exact_old_string_and_intended_new_string",
		RequiredArgs: map[string]any{
			"file_path":              modelPath,
			"indentation_delta":      delta,
			"candidate_line_start":   lineStart,
			"candidate_line_count":   lineCount,
			"file_unchanged":         true,
			"new_string_is_verbatim": true,
		},
		PreserveDraft: true,
	}
	if len(candidate) <= editIndentCandidateMaxBytes && lineCount <= editIndentCandidateMaxLines {
		recovery.RequiredArgs["old_string"] = candidate
	} else {
		// Do not truncate a value presented as an exact replacement target.
		// The model can read the located range and choose a smaller exact edit.
		recovery.RequiredAction = "read_file_then_retry_small_exact_edit"
		recovery.RequiredArgs["offset"] = lineStart
		recovery.RequiredArgs["limit"] = min(lineCount, editIndentCandidateMaxLines)
		recovery.Message += " The candidate is too large to include; read the indicated range and choose a smaller exact edit."
	}
	return recovery
}

// uniqueUniformIndentCandidate locates a line-aligned source observation only.
// It never transforms replacement text. Single-line candidates, one-line
// evidence, tabs, partial matches and multiple candidate windows are rejected.
func uniqueUniformIndentCandidate(content, oldString string) (string, int, bool) {
	if !strings.Contains(oldString, "\n") || strings.Contains(oldString, "\r") {
		return "", 0, false
	}
	oldTrailingNewline := strings.HasSuffix(oldString, "\n")
	oldCore := strings.TrimSuffix(oldString, "\n")
	oldLines := strings.Split(oldCore, "\n")
	if len(oldLines) < 2 {
		return "", 0, false
	}
	nonblank := 0
	for _, line := range oldLines {
		if strings.TrimSpace(line) != "" {
			nonblank++
		}
	}
	if nonblank < 2 {
		return "", 0, false
	}

	contentLines := strings.Split(content, "\n")
	type match struct {
		old   string
		delta int
	}
	matches := make([]match, 0, 2)
	for start := 0; start+len(oldLines) <= len(contentLines); start++ {
		window := contentLines[start : start+len(oldLines)]
		delta, ok := uniformASCIISpaceIndentDelta(oldLines, window)
		if !ok || delta == 0 {
			continue
		}
		groundedOld := strings.Join(window, "\n")
		if oldTrailingNewline {
			groundedOld += "\n"
		}
		if !strings.Contains(content, groundedOld) {
			continue
		}
		matches = append(matches, match{old: groundedOld, delta: delta})
		if len(matches) > 1 {
			return "", 0, false
		}
	}
	if len(matches) != 1 {
		return "", 0, false
	}
	if strings.Count(content, matches[0].old) != 1 {
		return "", 0, false
	}
	return matches[0].old, matches[0].delta, true
}

func uniformASCIISpaceIndentDelta(expected, actual []string) (int, bool) {
	if len(expected) != len(actual) {
		return 0, false
	}
	delta := 0
	hasDelta := false
	for index := range expected {
		expectedLine := expected[index]
		actualLine := actual[index]
		if strings.TrimSpace(expectedLine) == "" || strings.TrimSpace(actualLine) == "" {
			if expectedLine != actualLine {
				return 0, false
			}
			continue
		}
		expectedIndent, expectedOK := asciiSpaceIndent(expectedLine)
		actualIndent, actualOK := asciiSpaceIndent(actualLine)
		if !expectedOK || !actualOK || expectedLine[expectedIndent:] != actualLine[actualIndent:] {
			return 0, false
		}
		lineDelta := actualIndent - expectedIndent
		if !hasDelta {
			delta = lineDelta
			hasDelta = true
		} else if lineDelta != delta {
			return 0, false
		}
	}
	return delta, hasDelta
}

func asciiSpaceIndent(line string) (int, bool) {
	indent := 0
	for indent < len(line) {
		switch line[indent] {
		case ' ':
			indent++
		case '\t':
			return 0, false
		default:
			return indent, true
		}
	}
	return indent, true
}
