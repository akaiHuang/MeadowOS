package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Workspace owns path resolution and the prior-read ledger shared by the
// file tools. Tools accept both absolute paths (the schemas ask for them)
// and relative ones (the local model emits them anyway) — relative resolves
// against the root.
type Workspace struct {
	Root        string
	SessionRoot string

	mu             sync.Mutex
	readAt         map[string]time.Time // path -> mtime observed at last read/write
	protectedFiles map[string]struct{}  // host-declared immutable files
	desktop        *DesktopSession      // immutable explicit OS-host capability
}

// ModelPath returns a short, stable absolute alias for paths inside the
// workspace. Small local models copy paths from tool results into later calls;
// exposing a long temporary checkout path wastes tokens and can be truncated
// mid-argument. Resolve accepts /workspace, so the alias is executable rather
// than presentation-only.
func (w *Workspace) ModelPath(path string) string {
	clean := filepath.Clean(path)
	if sessionRoot := filepath.Clean(w.SessionRoot); w.SessionRoot != "" {
		if clean == sessionRoot {
			return "/session"
		}
		if rel, err := filepath.Rel(sessionRoot, clean); err == nil && rel != "." &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(filepath.Join("/session", rel))
		}
	}
	root := filepath.Clean(w.Root)
	if clean == root {
		return "/workspace"
	}
	if rel, err := filepath.Rel(root, clean); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(filepath.Join("/workspace", rel))
	}
	return path
}

// SetSessionRoot mounts one durable per-chat artifact directory at /session.
// The project remains available at /workspace, so generated standalone output
// is isolated without breaking repository inspection and repair tasks.
func (w *Workspace) SetSessionRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("session artifact root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("create session artifact root: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	w.SessionRoot = filepath.Clean(abs)
	return nil
}

// ArtifactModelRoot is the model-visible destination for new standalone
// artifacts. Older/non-session callers retain the /workspace behavior.
func (w *Workspace) ArtifactModelRoot() string {
	if w != nil && strings.TrimSpace(w.SessionRoot) != "" {
		return "/session"
	}
	return "/workspace"
}

// ArtifactHostRoot is the host-side companion of ArtifactModelRoot.
func (w *Workspace) ArtifactHostRoot() string {
	if w != nil && strings.TrimSpace(w.SessionRoot) != "" {
		return w.SessionRoot
	}
	if w == nil {
		return ""
	}
	return w.Root
}

// ResolveArtifact resolves a newly requested standalone output. Explicit
// /workspace or host-absolute paths remain authoritative; a bare filename is
// placed in the session mount when one is configured.
func (w *Workspace) ResolveArtifact(path string) (string, error) {
	raw := strings.TrimSpace(strings.Trim(path, `"'`))
	if raw == "" {
		return "", fmt.Errorf("path is empty")
	}
	slash := strings.ReplaceAll(raw, `\`, "/")
	if w.SessionRoot != "" && !filepath.IsAbs(raw) &&
		!strings.HasPrefix(slash, "/workspace/") && slash != "/workspace" {
		rel, err := cleanMountedRelative(slash)
		if err != nil {
			return "", fmt.Errorf("artifact path %q escapes the session artifact root", path)
		}
		return w.Resolve("/session/" + filepath.ToSlash(rel))
	}
	return w.Resolve(raw)
}

func NewWorkspace(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	return &Workspace{
		Root: abs, readAt: map[string]time.Time{},
		protectedFiles: map[string]struct{}{},
	}, nil
}

// ProtectFiles installs a host-owned immutable-file boundary. The model may
// still read these files, but file tools reject writes and the shell restores
// any attempted replacement before returning control. Paths must name existing
// regular files inside the workspace; this is an execution capability, not
// natural-language instruction parsing.
func (w *Workspace) ProtectFiles(paths []string) error {
	resolved := make([]string, 0, len(paths))
	for _, raw := range paths {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		path, err := w.Resolve(raw)
		if err != nil {
			return fmt.Errorf("protect file %q: %w", raw, err)
		}
		if !w.contains(path) {
			return fmt.Errorf("protect file %q: path must be inside workspace", raw)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("protect file %q: %w", raw, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("protect file %q: only regular files are supported", raw)
		}
		resolved = append(resolved, path)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, path := range resolved {
		w.protectedFiles[path] = struct{}{}
	}
	return nil
}

// CheckWritable rejects a mutation outside the authorized roots or of a
// host-protected file. Callers that derive child paths from a resolved
// directory (research/presentation exporters, for example) rely on this second
// boundary check because a pre-existing child may itself be a symlink.
func (w *Workspace) CheckWritable(path string) error {
	resolved, declared, err := w.resolveDeclaredAbsolutePath(path, path)
	if err != nil {
		return err
	}
	if !declared {
		return fmt.Errorf("writable path %q is outside the authorized workspace roots", path)
	}
	w.mu.Lock()
	_, protected := w.protectedFiles[resolved]
	w.mu.Unlock()
	if protected {
		return fmt.Errorf("protected file is read-only: %s", w.ModelPath(resolved))
	}
	return nil
}

type protectedFileState struct {
	data    []byte
	mode    os.FileMode
	modTime time.Time
}

func (w *Workspace) snapshotProtectedFiles() (map[string]protectedFileState, error) {
	w.mu.Lock()
	paths := make([]string, 0, len(w.protectedFiles))
	for path := range w.protectedFiles {
		paths = append(paths, path)
	}
	w.mu.Unlock()
	snapshot := make(map[string]protectedFileState, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("snapshot protected file %s: %w", w.ModelPath(path), err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("snapshot protected file %s: %w", w.ModelPath(path), err)
		}
		snapshot[path] = protectedFileState{
			data: data, mode: info.Mode().Perm(), modTime: info.ModTime(),
		}
	}
	return snapshot, nil
}

func (w *Workspace) restoreProtectedFiles(snapshot map[string]protectedFileState) ([]string, error) {
	var changed []string
	for path, before := range snapshot {
		current, readErr := os.ReadFile(path)
		info, statErr := os.Lstat(path)
		unchanged := readErr == nil && statErr == nil && info.Mode().IsRegular() &&
			info.Mode().Perm() == before.mode && string(current) == string(before.data)
		if unchanged {
			continue
		}
		changed = append(changed, w.ModelPath(path))
		if statErr == nil && !info.Mode().IsRegular() {
			if err := os.RemoveAll(path); err != nil {
				return changed, fmt.Errorf("restore protected file %s: %w", w.ModelPath(path), err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return changed, fmt.Errorf("restore protected file %s: %w", w.ModelPath(path), err)
		}
		if err := os.WriteFile(path, before.data, before.mode); err != nil {
			return changed, fmt.Errorf("restore protected file %s: %w", w.ModelPath(path), err)
		}
		if err := os.Chmod(path, before.mode); err != nil {
			return changed, fmt.Errorf("restore protected file %s mode: %w", w.ModelPath(path), err)
		}
		if err := os.Chtimes(path, before.modTime, before.modTime); err != nil {
			return changed, fmt.Errorf("restore protected file %s timestamp: %w", w.ModelPath(path), err)
		}
		w.NoteRead(path)
	}
	sort.Strings(changed)
	return changed, nil
}

// Resolve turns a model-supplied path into a cleaned absolute path and
// rejects escapes from the workspace root. The temp dir is also allowed —
// scratch output is a normal agent pattern.
func (w *Workspace) Resolve(p string) (string, error) {
	raw := strings.TrimSpace(strings.Trim(p, `"'`))
	if raw == "" {
		return "", fmt.Errorf("path is empty")
	}
	slash := strings.ReplaceAll(raw, `\`, "/")
	// Qwen commonly expands the documented /session mount into the legacy
	// looking /workspace/session path. Treat that spelling as a read/resolve
	// alias when a session root exists. Artifact mutation guards still require
	// the canonical /session spelling, so writes remain explicit and auditable.
	if strings.TrimSpace(w.SessionRoot) != "" &&
		(slash == "/workspace/session" || strings.HasPrefix(slash, "/workspace/session/")) {
		slash = "/session" + strings.TrimPrefix(slash, "/workspace/session")
	}
	if rel, mounted := mountedRelative(slash, "/session"); mounted {
		if strings.TrimSpace(w.SessionRoot) == "" {
			return "", fmt.Errorf("session artifact root is not configured")
		}
		return resolveMountedPath(p, w.SessionRoot, rel, "session artifact")
	}
	for _, mount := range []string{"/workspace", "/workdir"} {
		if rel, mounted := mountedRelative(slash, mount); mounted {
			return resolveMountedPath(p, w.Root, rel, "workspace")
		}
	}
	// Common paths copied from model training data are documented aliases for a
	// workspace basename, including a missing file that is about to be created.
	for _, prefix := range []string{"/home/user/", "/Users/user/", "/mnt/data/"} {
		if strings.HasPrefix(slash, prefix) {
			return resolveMountedPath(p, w.Root, filepath.Base(filepath.FromSlash(slash)), "workspace")
		}
	}
	if len(slash) >= 3 && slash[1] == ':' && slash[2] == '/' && isASCIIAlpha(slash[0]) {
		return resolveMountedPath(p, w.Root, filepath.Base(filepath.FromSlash(slash)), "workspace")
	}
	hostSpelling := filepath.FromSlash(slash)
	hostPath := filepath.Clean(hostSpelling)
	if !filepath.IsAbs(hostPath) {
		return resolveMountedPath(p, w.Root, slash, "workspace")
	}
	// Host-absolute paths inherit the narrowest declared namespace. This check
	// happens before following path symlinks so a workspace path cannot promote
	// itself to /session or to the broader scratch-temp capability. Mapping the
	// platform temp spelling first also preserves /tmp ↔ /private/tmp aliases.
	if resolved, declared, err := w.resolveDeclaredAbsolutePath(p, hostSpelling); declared {
		return resolved, err
	}
	// Weak models invent absolute paths from training data (e.g. a 12B reading
	// "/home/user/data/sales.csv" for a file that is really in the workspace).
	// When the escaping path's basename names a file that EXISTS at the
	// workspace root, redirect to it rather than hard-failing — this recovers a
	// read/overwrite of a real in-workspace file without ever creating a file at
	// a surprising location (a missing basename still rejects).
	if cand := filepath.Join(w.Root, filepath.Base(hostPath)); cand != w.Root {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return resolveCanonicalWithinRoot(p, cand, w.Root, "workspace basename")
		}
	}
	return "", fmt.Errorf("path %q is outside the workspace root %q", hostPath, w.Root)
}

func mountedRelative(path, mount string) (string, bool) {
	if path == mount {
		return ".", true
	}
	if strings.HasPrefix(path, mount+"/") {
		return strings.TrimPrefix(path, mount+"/"), true
	}
	return "", false
}

func cleanMountedRelative(raw string) (string, error) {
	// Repeated separators after a model mount still name a child of that mount;
	// they must not be reinterpreted as a new host-absolute namespace.
	raw = strings.TrimLeft(strings.ReplaceAll(raw, `\`, "/"), "/")
	if raw == "" {
		return ".", nil
	}
	rel := filepath.Clean(filepath.FromSlash(raw))
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("relative path escapes its mount")
	}
	return rel, nil
}

func resolveMountedPath(original, root, rawRelative, label string) (string, error) {
	rel, err := cleanMountedRelative(rawRelative)
	if err != nil {
		return "", fmt.Errorf("path %q escapes the %s root", original, label)
	}
	return resolveCanonicalWithinRoot(original, filepath.Join(root, rel), root, label)
}

func resolveCanonicalWithinRoot(original, candidate, root, label string) (string, error) {
	candidate = filepath.Clean(candidate)
	if !containsRoot(root, candidate) {
		return "", fmt.Errorf("path %q escapes the %s root", original, label)
	}
	resolved, err := canonicalPathAllowMissing(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve %s path %q: %w", label, original, err)
	}
	if !containsRoot(root, resolved) {
		return "", fmt.Errorf("path %q resolves outside the %s root", original, label)
	}
	return resolved, nil
}

func pathClaimsRootSpelling(root, path string) bool {
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return filepath.IsAbs(path)
	}
	root = strings.TrimSuffix(root, string(filepath.Separator))
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func (w *Workspace) narrowestNamespace(path string, includeTemp bool) (string, string, bool) {
	type namespace struct {
		root  string
		label string
	}
	candidates := []namespace{{root: w.Root, label: "workspace"}}
	if w.SessionRoot != "" {
		candidates = append(candidates, namespace{root: w.SessionRoot, label: "session artifact"})
	}
	if includeTemp {
		candidates = append(candidates, namespace{root: canonicalTempDir(), label: "temporary"})
	}
	best := namespace{}
	for _, candidate := range candidates {
		if !pathClaimsRootSpelling(candidate.root, path) {
			continue
		}
		if best.root == "" || len(filepath.Clean(candidate.root)) > len(filepath.Clean(best.root)) {
			best = candidate
		}
	}
	return best.root, best.label, best.root != ""
}

func (w *Workspace) resolveDeclaredAbsolutePath(original, path string) (string, bool, error) {
	path = normalizePathSpellingKeepParents(path)
	if root, label, ok := w.narrowestNamespace(path, false); ok {
		resolved, err := resolveCanonicalWithinRoot(original, filepath.Clean(path), root, label)
		return resolved, true, err
	}
	candidate, tempClaimed := canonicalTempCandidate(path)
	if !tempClaimed {
		return "", false, nil
	}
	root, label, ok := w.narrowestNamespace(candidate, true)
	if !ok {
		return "", true, fmt.Errorf("path %q is outside the authorized workspace roots", original)
	}
	resolved, err := resolveCanonicalWithinRoot(original, candidate, root, label)
	return resolved, true, err
}

// normalizePathSpellingKeepParents removes redundant separators and harmless
// dot components for namespace matching while deliberately retaining `..`.
// filepath.Clean cannot be used until after a namespace has been selected,
// otherwise an aliased workspace traversal can masquerade as a direct temp
// path before the workspace boundary check runs.
func normalizePathSpellingKeepParents(path string) string {
	separator := string(filepath.Separator)
	volume := filepath.VolumeName(path)
	remainder := strings.TrimPrefix(path, volume)
	absolute := strings.HasPrefix(remainder, separator)
	parts := strings.FieldsFunc(remainder, func(r rune) bool {
		return r == rune(filepath.Separator)
	})
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "." {
			kept = append(kept, part)
		}
	}
	result := volume
	if absolute {
		result += separator
	}
	result += strings.Join(kept, separator)
	if result == "" && absolute {
		return separator
	}
	return result
}

func canonicalTempCandidate(path string) (string, bool) {
	canonical := canonicalTempDir()
	for _, spelling := range []string{filepath.Clean(os.TempDir()), canonical} {
		if !pathClaimsRootSpelling(spelling, path) {
			continue
		}
		// Replace only the namespace prefix. Do not filepath.Rel/Join here: those
		// clean `..` before narrowestNamespace can see that an aliased workspace
		// path tried to traverse into a temp sibling.
		return canonical + strings.TrimPrefix(path, spelling), true
	}
	return "", false
}

func normalizeModelPath(p string) string {
	p = strings.TrimSpace(strings.Trim(p, `"'`))
	if p == "" {
		return ""
	}
	slash := strings.ReplaceAll(p, `\`, `/`)
	for _, prefix := range []string{"/workspace/", "/workdir/"} {
		if strings.HasPrefix(slash, prefix) {
			return strings.TrimPrefix(slash, prefix)
		}
	}
	for _, prefix := range []string{"/home/user/", "/Users/user/", "/mnt/data/"} {
		if strings.HasPrefix(slash, prefix) {
			return filepath.Base(slash)
		}
	}
	if slash == "/workspace" || slash == "/workdir" {
		return "."
	}
	if len(slash) >= 3 && slash[1] == ':' && slash[2] == '/' && isASCIIAlpha(slash[0]) {
		return filepath.Base(slash)
	}
	return slash
}

func isASCIIAlpha(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

func (w *Workspace) contains(p string) bool {
	resolved, err := canonicalPathAllowMissing(p)
	return err == nil && (containsRoot(w.Root, resolved) ||
		(w.SessionRoot != "" && containsRoot(w.SessionRoot, resolved)))
}

func containsRoot(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if root == "." || root == "" {
		return false
	}
	return candidate == root || strings.HasPrefix(candidate, root+string(filepath.Separator))
}

func insideTempDir(p string) bool {
	resolved, err := canonicalPathAllowMissing(p)
	if err != nil {
		return false
	}
	return containsRoot(canonicalTempDir(), resolved)
}

// canonicalPathAllowMissing resolves every existing path component, then
// appends any missing suffix. Resolving only filepath.Dir(path) is insufficient:
// the immediate parent may itself be missing below a symlinked ancestor.
func canonicalPathAllowMissing(path string) (string, error) {
	current := filepath.Clean(path)
	if !filepath.IsAbs(current) {
		return "", fmt.Errorf("path must be absolute: %q", path)
	}
	missing := make([]string, 0, 4)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		// If Lstat succeeds but EvalSymlinks fails, current is commonly a
		// dangling/cyclic symlink. Do not reinterpret it as an ordinary missing
		// target because a later open could follow it outside the trusted root.
		if _, lstatErr := os.Lstat(current); lstatErr == nil {
			return "", err
		} else if !os.IsNotExist(lstatErr) {
			return "", lstatErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func canonicalTempDir() string {
	tmp := filepath.Clean(os.TempDir())
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		return filepath.Clean(resolved)
	}
	return tmp
}

// NoteRead records the file's current mtime after a successful read or
// write, satisfying the prior-read requirement for later mutations.
func (w *Workspace) NoteRead(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	w.mu.Lock()
	w.readAt[path] = info.ModTime()
	w.mu.Unlock()
}

// CheckPriorRead enforces the TS prior-read rule for mutating an existing
// file: it must have been read this session, and must not have changed on
// disk since. New files pass.
func (w *Workspace) CheckPriorRead(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	w.mu.Lock()
	seen, ok := w.readAt[path]
	// Most paths resolve to one stable spelling. ReadFile additionally repairs
	// component casing so weak local models can recover from e.g. `Meadow` vs
	// `meadow` on a case-insensitive macOS volume. In that case a successful
	// read and a later edit can name the same inode with different host-path
	// strings. Keep the fast exact lookup, then compare file identity rather
	// than weakening the ledger with unconditional lower-casing (which would
	// conflate distinct files on case-sensitive volumes).
	readAtSnapshot := make(map[string]time.Time)
	if !ok {
		readAtSnapshot = make(map[string]time.Time, len(w.readAt))
		for observedPath, observedAt := range w.readAt {
			readAtSnapshot[observedPath] = observedAt
		}
	}
	w.mu.Unlock()
	if !ok {
		for observedPath, observedAt := range readAtSnapshot {
			observedInfo, statErr := os.Stat(observedPath)
			if statErr != nil || !os.SameFile(info, observedInfo) {
				continue
			}
			seen, ok = observedAt, true
			// Cache the equivalent spelling so repeated model retries remain on
			// the constant-time path without changing authorization semantics.
			w.mu.Lock()
			w.readAt[path] = observedAt
			w.mu.Unlock()
			break
		}
	}
	if !ok {
		modelPath := w.ModelPath(path)
		return &RecoveryError{
			Code:           "prior_read_required",
			Message:        "Existing file must be read before overwrite; the supplied path is valid.",
			Path:           modelPath,
			RequiredAction: "read_file",
			RequiredArgs:   map[string]any{"file_path": modelPath},
			PreserveDraft:  true,
		}
	}
	if !info.ModTime().Equal(seen) {
		modelPath := w.ModelPath(path)
		return &RecoveryError{
			Code:           "prior_read_stale",
			Message:        "Existing file changed after the previous read; refresh the observation before overwrite.",
			Path:           modelPath,
			RequiredAction: "read_file",
			RequiredArgs:   map[string]any{"file_path": modelPath},
			PreserveDraft:  true,
		}
	}
	return nil
}
