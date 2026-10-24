package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const WriteFilesToolName = "write_files"

// WriteFiles transactionally creates or replaces two to four sibling artifacts.
// It is a general file transaction, not a web-task oracle: the model still
// chooses every path and byte. The Muse profile uses it because Muse emits one
// native tool call per response and otherwise spends one full decode per file.
type WriteFiles struct {
	WS                     *Workspace
	MinFiles               int
	MaxFiles               int
	DropCompiledJSSiblings bool
	// FlatPairArguments advertises two source files as four scalar ATEM
	// parameters instead of one nested JSON array. Muse Glimmer is reliable at
	// scalar recipient parameters but occasionally serializes a files[] schema
	// as nested ATEM tags, which turns an otherwise valid source mutation into a
	// full dense-model retry. The model still chooses every path and byte; this
	// only changes its wire contract. Keep this disabled for non-Muse registries.
	FlatPairArguments bool
}

func (t *WriteFiles) Name() string { return WriteFilesToolName }
func (t *WriteFiles) Description() string {
	if t != nil && t.FlatPairArguments {
		return "Transactionally write exactly two complete source files in one call using four scalar parameters. Do not include generated build outputs."
	}
	minFiles, maxFiles := t.fileLimits()
	generated := ""
	if t != nil && t.DropCompiledJSSiblings {
		generated = " When matching TypeScript and JavaScript are supplied, JavaScript is treated as generated output and omitted."
	}
	if minFiles == maxFiles {
		return fmt.Sprintf("Transactionally write exactly %d complete local files in one call.%s", minFiles, generated)
	}
	return fmt.Sprintf("Transactionally write %d to %d complete local files in one call.%s", minFiles, maxFiles, generated)
}
func (t *WriteFiles) Kind() Kind { return KindEdit }

func (t *WriteFiles) Schema() map[string]any {
	if t != nil && t.FlatPairArguments {
		return obj(map[string]any{
			"first_file_path":     str("Absolute path to the first source file."),
			"first_file_content":  str("Complete content of the first source file."),
			"second_file_path":    str("Absolute path to the second source file."),
			"second_file_content": str("Complete content of the second source file. Do not provide generated build output."),
		}, "first_file_path", "first_file_content", "second_file_path", "second_file_content")
	}
	minFiles, maxFiles := t.fileLimits()
	entry := obj(map[string]any{
		"file_path": str("Absolute path to one file."),
		"content":   str("Complete content for that file."),
	}, "file_path", "content")
	return obj(map[string]any{
		"files": map[string]any{
			"type": "array", "minItems": minFiles, "maxItems": maxFiles,
			"description": fmt.Sprintf("%d to %d complete files committed as one transaction.", minFiles, maxFiles),
			"items":       entry,
		},
	}, "files")
}

func (t *WriteFiles) MutationPaths(args map[string]any) []string {
	items := t.argumentItems(args)
	items = t.withoutCompiledJSSiblings(items)
	paths := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		path, _ := item["file_path"].(string)
		path = strings.TrimSpace(path)
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	return paths
}

type writeFilesEntry struct {
	modelPath string
	hostPath  string
	content   string
	before    []byte
	existed   bool
	changed   bool
}

func (t *WriteFiles) Execute(ctx context.Context, args map[string]any) Result {
	entries, err := t.prepare(args)
	if err != nil {
		return errResult(err)
	}
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}

	changed := make([]*writeFilesEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.changed {
			changed = append(changed, entry)
		}
	}
	if len(changed) == 0 {
		paths := make([]string, 0, len(entries))
		for _, entry := range entries {
			t.WS.NoteRead(entry.hostPath)
			paths = append(paths, entry.modelPath)
		}
		msg := fmt.Sprintf("Unchanged %d files: %s", len(paths), strings.Join(paths, ", "))
		result := Result{LLMContent: msg, Display: msg, NoChange: true}
		for _, entry := range entries {
			result = appendSourceDiagnostics(ctx, result, entry.modelPath, entry.content)
		}
		return result
	}

	written := make([]*writeFilesEntry, 0, len(changed))
	for _, entry := range changed {
		if err := ctx.Err(); err != nil {
			return rollbackWriteFiles(written, err)
		}
		if err := os.MkdirAll(filepath.Dir(entry.hostPath), 0o755); err != nil {
			return rollbackWriteFiles(written, err)
		}
		if err := os.WriteFile(entry.hostPath, []byte(entry.content), 0o644); err != nil {
			return rollbackWriteFiles(written, err)
		}
		written = append(written, entry)
	}

	paths := make([]string, 0, len(changed))
	diffs := make([]string, 0, len(changed))
	for _, entry := range entries {
		t.WS.NoteRead(entry.hostPath)
		if !entry.changed {
			continue
		}
		paths = append(paths, entry.modelPath)
		diffs = append(diffs, boundedUnifiedDiff(entry.modelPath, string(entry.before), entry.content))
	}
	msg := fmt.Sprintf("Created or updated %d files: %s", len(paths), strings.Join(paths, ", "))
	result := Result{
		LLMContent: msg, Display: msg, Diff: strings.Join(diffs, "\n"),
		ChangedPaths: paths,
	}
	for _, entry := range entries {
		result = appendSourceDiagnostics(ctx, result, entry.modelPath, entry.content)
	}
	return result
}

func (t *WriteFiles) prepare(args map[string]any) ([]*writeFilesEntry, error) {
	if t == nil || t.WS == nil {
		return nil, fmt.Errorf("workspace is required")
	}
	minFiles, maxFiles := t.fileLimits()
	items := t.argumentItems(args)
	ok := items != nil
	items = t.withoutCompiledJSSiblings(items)
	if !ok || len(items) < minFiles || len(items) > maxFiles {
		if minFiles == maxFiles {
			return nil, fmt.Errorf("files must contain exactly %d entries", minFiles)
		}
		return nil, fmt.Errorf("files must contain %d to %d entries", minFiles, maxFiles)
	}
	entries := make([]*writeFilesEntry, 0, len(items))
	seen := make(map[string]bool, len(items))
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("files[%d] must be an object", index)
		}
		modelPath, _ := item["file_path"].(string)
		modelPath = strings.TrimSpace(modelPath)
		if modelPath == "" {
			return nil, fmt.Errorf("files[%d].file_path is required", index)
		}
		content, ok := item["content"].(string)
		if !ok {
			return nil, fmt.Errorf("files[%d].content is required", index)
		}
		if containsCompactedToolResult(content) {
			return nil, fmt.Errorf("files[%d].content contains a compacted tool-result marker; re-read the exact file before writing", index)
		}
		hostPath, err := t.WS.Resolve(modelPath)
		if err != nil {
			return nil, err
		}
		if seen[hostPath] {
			return nil, fmt.Errorf("duplicate file path in transaction: %s", modelPath)
		}
		seen[hostPath] = true
		if err := t.WS.CheckWritable(hostPath); err != nil {
			return nil, err
		}
		if info, statErr := os.Stat(hostPath); statErr == nil && info.IsDir() {
			return nil, fmt.Errorf("%s is a directory", hostPath)
		}
		if err := t.WS.CheckPriorRead(hostPath); err != nil {
			return nil, err
		}
		before, readErr := os.ReadFile(hostPath)
		existed := readErr == nil
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, readErr
		}
		entries = append(entries, &writeFilesEntry{
			modelPath: t.WS.ModelPath(hostPath), hostPath: hostPath,
			content: content, before: before, existed: existed,
			changed: !existed || string(before) != content,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].modelPath < entries[j].modelPath })
	return entries, nil
}

func (t *WriteFiles) argumentItems(args map[string]any) []any {
	if t != nil && t.FlatPairArguments {
		firstPath, firstPathOK := args["first_file_path"].(string)
		firstContent, firstContentOK := args["first_file_content"].(string)
		secondPath, secondPathOK := args["second_file_path"].(string)
		secondContent, secondContentOK := args["second_file_content"].(string)
		if !firstPathOK || !firstContentOK || !secondPathOK || !secondContentOK {
			return nil
		}
		return []any{
			map[string]any{"file_path": firstPath, "content": firstContent},
			map[string]any{"file_path": secondPath, "content": secondContent},
		}
	}
	items, _ := args["files"].([]any)
	return items
}

func (t *WriteFiles) withoutCompiledJSSiblings(items []any) []any {
	if t == nil || !t.DropCompiledJSSiblings || len(items) < 2 {
		return items
	}
	typescriptRoots := make(map[string]bool, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		path, _ := item["file_path"].(string)
		path = strings.TrimSpace(path)
		if strings.EqualFold(filepath.Ext(path), ".ts") {
			typescriptRoots[strings.TrimSuffix(filepath.Clean(path), filepath.Ext(path))] = true
		}
	}
	if len(typescriptRoots) == 0 {
		return items
	}
	filtered := make([]any, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		path, _ := item["file_path"].(string)
		path = strings.TrimSpace(path)
		ext := filepath.Ext(path)
		if strings.EqualFold(ext, ".js") && typescriptRoots[strings.TrimSuffix(filepath.Clean(path), ext)] {
			continue
		}
		filtered = append(filtered, raw)
	}
	return filtered
}

func (t *WriteFiles) fileLimits() (int, int) {
	minFiles, maxFiles := 2, 4
	if t != nil && t.MinFiles > 0 {
		minFiles = t.MinFiles
	}
	if t != nil && t.MaxFiles > 0 {
		maxFiles = t.MaxFiles
	}
	if maxFiles < minFiles {
		maxFiles = minFiles
	}
	return minFiles, maxFiles
}

func rollbackWriteFiles(written []*writeFilesEntry, cause error) Result {
	restored := make([]string, 0, len(written))
	for index := len(written) - 1; index >= 0; index-- {
		entry := written[index]
		var err error
		if entry.existed {
			err = os.WriteFile(entry.hostPath, entry.before, 0o644)
		} else {
			err = os.Remove(entry.hostPath)
		}
		if err != nil {
			return errResultf("write_files failed: %v; rollback of %s also failed: %v", cause, entry.modelPath, err)
		}
		restored = append(restored, entry.modelPath)
	}
	if len(restored) > 0 {
		return errResultf("write_files failed and rolled back %s: %v", strings.Join(restored, ", "), cause)
	}
	return errResult(cause)
}
