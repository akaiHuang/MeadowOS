package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	readDefaultLineLimit = 2000
	readMaxBytes         = 256 * 1024
)

// ReadFile is intentionally a UTF-8 text-file tool. Image attachments use the
// provider's native MLX-VLM message path instead of being decoded as file text.
type ReadFile struct {
	WS       *Workspace
	MaxBytes int
}

func (t *ReadFile) Name() string { return "read_file" }
func (t *ReadFile) Description() string {
	return "Read a local UTF-8 text file. For PNG or JPEG pixels, use inspect_image instead."
}
func (t *ReadFile) Kind() Kind                       { return KindReadOnly }
func (t *ReadFile) ParallelSafe(map[string]any) bool { return true }
func (t *ReadFile) SubtaskSafe() bool                { return true }

func (t *ReadFile) NormalizeArguments(args map[string]any) {
	for _, key := range []string{"offset", "limit"} {
		raw, ok := args[key].(string)
		if !ok {
			continue
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(raw))
		if err == nil {
			args[key] = float64(parsed)
		}
	}
}

func (t *ReadFile) Schema() map[string]any {
	return obj(map[string]any{
		"file_path": str("Absolute path to the file. /workspace is the selected project root itself: /workspace/README.md reads its root README.md. Append only subdirectories observed in the workspace."),
		"offset":    num("Optional 1-based line number to start reading from."),
		"limit":     num("Optional max number of lines to read."),
	}, "file_path")
}

// ReadIdentity lets the agent tool boundary deduplicate an unchanged file
// region without baking read_file-specific path rules into the agent loop.
func (t *ReadFile) ReadIdentity(args map[string]any) (ReadIdentity, error) {
	path, err := t.resolvePath(pathArg(args))
	if err != nil {
		return ReadIdentity{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return ReadIdentity{}, err
	}
	if info.IsDir() {
		return ReadIdentity{}, fmt.Errorf("%s is a directory; use list_directory", path)
	}
	start, limit := readWindow(args)
	return ReadIdentity{
		Key:  path + "\x00" + strconv.Itoa(start) + "\x00" + strconv.Itoa(limit),
		Path: t.WS.ModelPath(path),
		Version: strconv.FormatInt(info.ModTime().UnixNano(), 10) +
			"\x00" + strconv.FormatInt(info.Size(), 10),
	}, nil
}

func (t *ReadFile) Execute(ctx context.Context, args map[string]any) Result {
	path, err := t.resolvePath(pathArg(args))
	if err != nil {
		return errResult(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return errResult(err)
	}
	if info.IsDir() {
		return errResultf("%s is a directory; use list_directory", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return errResult(err)
	}
	if !utf8.Valid(b) {
		hint := ""
		switch strings.ToLower(filepath.Ext(path)) {
		case ".png", ".jpg", ".jpeg":
			hint = "; use inspect_image to attach its pixels to a vision-capable model"
		}
		return errResultf("%s is not a UTF-8 text file (%d bytes)%s", path, len(b), hint)
	}
	t.WS.NoteRead(path)

	lines := strings.Split(string(b), "\n")
	total := len(lines)

	start, limit := readWindow(args)
	if start > total {
		return errResultf("offset %d is past the end of the file (%d lines)", start, total)
	}
	end := start + limit - 1
	if end > total {
		end = total
	}
	section := strings.Join(lines[start-1:end], "\n")

	// Byte cap protects the context window from minified one-liner monsters.
	// Model-family registries may lower it without changing the default tool.
	truncatedBytes := false
	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = readMaxBytes
	}
	if len(section) > maxBytes {
		section = validUTF8Prefix(section, maxBytes)
		truncatedBytes = true
	}

	if start == 1 && end == total && !truncatedBytes {
		return Result{
			LLMContent: section,
			Display:    fmt.Sprintf("Read %d lines from %s", total, t.WS.ModelPath(path)),
		}
	}
	header := fmt.Sprintf("Showing lines %d-%d of %d total lines.", start, end, total)
	if truncatedBytes {
		header += fmt.Sprintf(" Output truncated at %dKB; use a narrower offset/limit window to continue.", maxBytes/1024)
	}
	return Result{
		LLMContent: header + "\n\n---\n\n" + section,
		Display:    fmt.Sprintf("Read lines %d-%d of %d from %s", start, end, total, t.WS.ModelPath(path)),
	}
}

// resolvePath preserves Workspace.Resolve for non-workspace locations, while
// making read_file tolerant of casing drift inside the authorized workspace.
// Resolution is component-by-component so the returned path uses the
// filesystem's canonical spelling. Symlinks and ambiguous case-fold matches
// fail closed rather than changing the authorization boundary.
func (t *ReadFile) resolvePath(raw string) (string, error) {
	if t == nil || t.WS == nil {
		return "", fmt.Errorf("workspace is not configured")
	}
	// Keep the legacy model spelling on the same namespace path as every other
	// file tool. Passing it through workspaceReadRelative would strip the
	// /workspace mount first, misroute reads to project/session, and let a
	// /workspace/session/../ traversal escape the intended SessionRoot.
	slash := strings.ReplaceAll(strings.TrimSpace(strings.Trim(raw, `"'`)), `\`, "/")
	if t.WS.SessionRoot != "" &&
		(slash == "/workspace/session" || strings.HasPrefix(slash, "/workspace/session/")) {
		return t.WS.Resolve(raw)
	}
	root, err := canonicalCasePath(t.WS.Root)
	if err != nil {
		return "", fmt.Errorf("canonical workspace root: %w", err)
	}
	rel, workspacePath, err := workspaceReadRelative(root, raw)
	if err != nil {
		return "", err
	}
	if !workspacePath {
		return t.WS.Resolve(raw)
	}
	return resolveCaseInsensitiveComponents(root, rel)
}

func workspaceReadRelative(root, raw string) (string, bool, error) {
	raw = strings.TrimSpace(strings.Trim(raw, `"'`))
	if raw == "" {
		return "", true, fmt.Errorf("path is empty")
	}
	normalized := normalizeModelPath(raw)
	if normalized == "" {
		return "", true, fmt.Errorf("path is empty")
	}
	if !filepath.IsAbs(normalized) {
		rel := filepath.Clean(normalized)
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", true, fmt.Errorf("path %q escapes the workspace root", raw)
		}
		return rel, true, nil
	}

	rootVolume, rootParts := splitAbsolutePath(root)
	pathVolume, pathParts := splitAbsolutePath(filepath.Clean(normalized))
	if !strings.EqualFold(rootVolume, pathVolume) || len(pathParts) < len(rootParts) {
		return "", false, nil
	}
	for index := range rootParts {
		if !strings.EqualFold(rootParts[index], pathParts[index]) {
			return "", false, nil
		}
	}
	if len(pathParts) == len(rootParts) {
		return ".", true, nil
	}
	return filepath.Join(pathParts[len(rootParts):]...), true, nil
}

func canonicalCasePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	volume, parts := splitAbsolutePath(filepath.Clean(abs))
	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	for _, part := range parts {
		match, err := uniqueCaseInsensitiveChild(current, part)
		if err != nil {
			if !os.IsPermission(err) {
				return "", err
			}
			// Sandboxed members may inspect the configured root's ancestors
			// without enumerating them (for example /Users). Use only the
			// exact configured component in that case; never broaden access or
			// guess a case-fold match outside the authorized workspace.
			candidate := filepath.Join(current, part)
			info, statErr := os.Lstat(candidate)
			if statErr != nil {
				return "", statErr
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("workspace root component %q is a symlink", part)
			}
			current = candidate
			continue
		}
		if match.Type()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("workspace root component %q is a symlink", match.Name())
		}
		current = filepath.Join(current, match.Name())
	}
	return filepath.Clean(current), nil
}

func resolveCaseInsensitiveComponents(root, rel string) (string, error) {
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace root", rel)
	}
	current := root
	if rel == "." {
		return current, nil
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("path component %q is not allowed", component)
		}
		match, err := uniqueCaseInsensitiveChild(current, component)
		if err != nil {
			return "", err
		}
		if match.Type()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path component %q is a symlink", match.Name())
		}
		current = filepath.Join(current, match.Name())
		if !containsRoot(root, current) {
			return "", fmt.Errorf("resolved path escapes the workspace root")
		}
	}
	return current, nil
}

func uniqueCaseInsensitiveChild(parent, requested string) (os.DirEntry, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	var match os.DirEntry
	for _, entry := range entries {
		if !strings.EqualFold(entry.Name(), requested) {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("path component %q is ambiguous in %q", requested, parent)
		}
		match = entry
	}
	if match == nil {
		return nil, fmt.Errorf("path component %q does not exist in %q", requested, parent)
	}
	return match, nil
}

func splitAbsolutePath(path string) (string, []string) {
	volume := filepath.VolumeName(path)
	remainder := strings.TrimPrefix(path, volume)
	remainder = strings.Trim(remainder, string(filepath.Separator))
	if remainder == "" {
		return volume, nil
	}
	return volume, strings.Split(remainder, string(filepath.Separator))
}

func validUTF8Prefix(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func readWindow(args map[string]any) (start, limit int) {
	start = 1
	if v, ok := intArg(args, "offset"); ok && v > 0 {
		start = v
	}
	limit = readDefaultLineLimit
	if v, ok := intArg(args, "limit"); ok && v > 0 {
		limit = v
	}
	return start, limit
}
