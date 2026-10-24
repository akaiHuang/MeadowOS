package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	shellMaxValidationPaths     = 8
	shellMaxValidationFileBytes = 8 * 1024 * 1024
)

// FileObservation is a content snapshot for an explicitly declared validation
// target. Path is a canonical model alias; no private host path is exported.
// A pair of equal snapshots describes only the two observation boundaries.
type FileObservation struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// SnapshotValidationFile takes a bounded snapshot of a regular file within the
// claimed workspace or session namespace. Unlike Resolve, this contract has no
// training-path aliases, basename recovery, or ambient temporary-dir access.
// Callers must compare both Path and SHA256 when rechecking an observation.
func (w *Workspace) SnapshotValidationFile(raw string) (FileObservation, error) {
	path, root, err := w.validationFilePath(raw)
	if err != nil {
		return FileObservation{}, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return FileObservation{}, err
	}
	// Root.Open rejects symlink escapes even if a parent changes between the
	// preceding resolution and open. Hashing is bounded; this is not a shell
	// sandbox and does not claim the command itself is confined.
	rootDir, err := os.OpenRoot(root)
	if err != nil {
		return FileObservation{}, fmt.Errorf("open validation root: %w", err)
	}
	defer rootDir.Close()
	info, err := rootDir.Stat(rel)
	if err != nil {
		return FileObservation{}, fmt.Errorf("stat validation file %q: %w", raw, err)
	}
	if !info.Mode().IsRegular() {
		return FileObservation{}, fmt.Errorf("validation file %q must be a regular file", raw)
	}
	// Nonblocking open also prevents a concurrent regular-file-to-FIFO swap
	// from stalling before the post-open type check.
	file, err := rootDir.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return FileObservation{}, fmt.Errorf("open validation file %q: %w", raw, err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return FileObservation{}, err
	}
	if !before.Mode().IsRegular() {
		return FileObservation{}, fmt.Errorf("validation file %q must be a regular file", raw)
	}
	if before.Size() > shellMaxValidationFileBytes {
		return FileObservation{}, fmt.Errorf("validation file %q exceeds 8 MiB", raw)
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(file, shellMaxValidationFileBytes+1))
	if err != nil {
		return FileObservation{}, fmt.Errorf("hash validation file %q: %w", raw, err)
	}
	after, err := file.Stat()
	if err != nil {
		return FileObservation{}, err
	}
	current, err := rootDir.Stat(rel)
	if err != nil {
		return FileObservation{}, fmt.Errorf("recheck validation file %q: %w", raw, err)
	}
	currentPath, currentRoot, pathErr := w.validationFilePath(raw)
	if n > shellMaxValidationFileBytes || n != before.Size() ||
		after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
		!os.SameFile(before, current) || current.Size() != after.Size() ||
		!current.ModTime().Equal(after.ModTime()) || pathErr != nil || currentPath != path || currentRoot != root {
		return FileObservation{}, fmt.Errorf("validation file %q changed during observation", raw)
	}
	return FileObservation{Path: w.ModelPath(path), SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}

func (w *Workspace) validationFilePath(raw string) (string, string, error) {
	if w == nil || strings.TrimSpace(w.Root) == "" {
		return "", "", fmt.Errorf("validation file requires a configured workspace")
	}
	path := strings.TrimSpace(raw)
	if path == "" || strings.ContainsAny(path, "\x00\\") {
		return "", "", fmt.Errorf("validation file path must be a nonempty slash-separated path")
	}
	for _, mount := range []struct{ alias, root, label string }{
		{"/workspace", w.Root, "workspace"},
		{"/session", w.SessionRoot, "session artifact"},
	} {
		if rel, ok := mountedRelative(path, mount.alias); ok {
			if mount.root == "" {
				return "", "", fmt.Errorf("session artifact root is not configured")
			}
			resolved, err := resolveMountedPath(raw, mount.root, rel, mount.label)
			return resolved, mount.root, err
		}
	}
	if !filepath.IsAbs(path) {
		resolved, err := resolveMountedPath(raw, w.Root, path, "workspace")
		return resolved, w.Root, err
	}
	// Preserve parent components until the namespace is selected. An absolute
	// workspace path cannot traverse to a sibling session or temp root.
	root, label, ok := w.narrowestNamespace(normalizePathSpellingKeepParents(path), false)
	if !ok {
		return "", "", fmt.Errorf("validation file %q is outside the workspace and session roots", raw)
	}
	resolved, err := resolveCanonicalWithinRoot(raw, path, root, label)
	return resolved, root, err
}

type validationSnapshot struct {
	declaredPath string
	observation  FileObservation
}

func (t *Shell) prepareValidationObservations(args map[string]any) ([]validationSnapshot, error) {
	raw, declared := args["validation_paths"]
	if !declared {
		return nil, nil
	}
	if strArg(args, "purpose") != "validation" || boolArg(args, "background") ||
		boolArg(args, ReadOnlyWorkspaceArgument) {
		return nil, fmt.Errorf("validation_paths requires purpose=validation and foreground execution in the actual workspace")
	}
	var paths []string
	switch values := raw.(type) {
	case []string:
		paths = values
	case []any:
		for _, value := range values {
			path, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("validation_paths must contain only strings")
			}
			paths = append(paths, path)
		}
	default:
		return nil, fmt.Errorf("validation_paths must be an array of paths")
	}
	if len(paths) == 0 || len(paths) > shellMaxValidationPaths {
		return nil, fmt.Errorf("validation_paths must contain between 1 and %d unique files", shellMaxValidationPaths)
	}
	observations := make([]validationSnapshot, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		observation, err := t.WS.SnapshotValidationFile(path)
		if err != nil {
			return nil, err
		}
		if seen[observation.Path] {
			return nil, fmt.Errorf("validation_paths repeats the same file %s", observation.Path)
		}
		seen[observation.Path] = true
		observations = append(observations, validationSnapshot{declaredPath: path, observation: observation})
	}
	return observations, nil
}

func (t *Shell) attachValidationObservations(result *Result, before []validationSnapshot) {
	if len(before) == 0 {
		return
	}
	if result.ProcessExited {
		for _, previous := range before {
			current, err := t.WS.SnapshotValidationFile(previous.declaredPath)
			if err == nil && current == previous.observation {
				result.FileObservations = append(result.FileObservations, current)
			}
		}
	}
	if len(result.FileObservations) == 0 {
		result.LLMContent += "\nvalidation_file_observations: none (requires normal process exit and unchanged target bytes at both boundaries)"
		return
	}
	payload, _ := json.Marshal(result.FileObservations)
	result.LLMContent += "\nvalidation_file_observations: " + string(payload) +
		"\nModel-declared targets with unchanged bytes at the before/after boundaries; not proof of correctness, file access, or no intermediate changes."
}
