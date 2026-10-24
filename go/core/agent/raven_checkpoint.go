package agent

// Native adaptation of Raven-Code checkpoint.py at RavenCodeSourceCommit.
// A private, bounded content-addressed store substitutes for its shadow Git
// repository. It snapshots ordinary workspace files (including shell writes),
// never stages or changes the user's Git repository, and never restores files.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ravenCheckpointLimits struct {
	Files, Entries, Snapshots         int
	FileBytes, TotalBytes, StoreBytes int64
	Time                              time.Duration
}

var ravenCheckpointDefaultLimits = ravenCheckpointLimits{
	Files: 4096, Entries: 20000, Snapshots: 64,
	FileBytes: 4 << 20, TotalBytes: 64 << 20, StoreBytes: 128 << 20,
	Time: 20 * time.Second,
}

// Slow filesystem calls cannot be cancelled by Go on every filesystem. Two
// bounded worker seats let Stop release the caller while a stuck OS call is
// draining. Late workers observe cancellation before publishing a manifest;
// a saturated pool makes checkpointing unavailable instead of creating more
// goroutines or holding a model turn indefinitely.
var ravenCheckpointWorkers = make(chan struct{}, 2)

type ravenCheckpointFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

type ravenCheckpointReceipt struct {
	Version      int                   `json:"version"`
	SourceCommit string                `json:"source_commit"`
	Workspace    string                `json:"workspace"`
	SessionID    string                `json:"session_id"`
	ID           string                `json:"id,omitempty"`
	Available    bool                  `json:"available"`
	Status       string                `json:"status"`
	Label        string                `json:"label"`
	CapturedAt   time.Time             `json:"captured_at"`
	ChangedPaths []string              `json:"changed_paths,omitempty"`
	Files        []ravenCheckpointFile `json:"files,omitempty"`
}

func (r ravenCheckpointReceipt) Observation() string {
	if !r.Available {
		return "[Workspace checkpoint]\nUnavailable (" + r.Status + "). No complete filesystem snapshot or restoration is inferred. Existing workspace files are preserved."
	}
	changedPaths := r.ChangedPaths
	if len(changedPaths) > 100 {
		changedPaths = changedPaths[:100]
	}
	changedBytes, _ := json.Marshal(changedPaths)
	changed := string(changedBytes)
	if len(changed) > 12000 {
		changed = "changed-path list omitted because it exceeds the context bound; inspect the private manifest"
	}
	if len(r.ChangedPaths) == 0 {
		changed = "none observed between the complete captures"
	}
	return fmt.Sprintf("[Workspace checkpoint]\nPrivate filesystem snapshot sha256:%s; %d ordinary files; changed paths: %s. Snapshot scope excludes ignored files, symlinks, credentials, build/cache and native private state. This is recovery evidence, not validation or proof of tool ownership. No file has been restored; verify current bytes before using an older snapshot.", r.ID, len(r.Files), changed)
}

type ravenWorkspaceCheckpoint struct {
	mu                              sync.Mutex
	workspace, sessionID, directory string
	workspaceInfo, storeInfo        os.FileInfo
	limits                          ravenCheckpointLimits
	previous                        *ravenCheckpointReceipt
	baseline                        *ravenCheckpointReceipt
}

func newRavenWorkspaceCheckpoint(ctx context.Context, workspace, sessionID, storeRoot string) (*ravenWorkspaceCheckpoint, error) {
	return newRavenWorkspaceCheckpointWithLimits(ctx, workspace, sessionID, storeRoot, ravenCheckpointDefaultLimits)
}

func newRavenWorkspaceCheckpointWithLimits(ctx context.Context, workspace, sessionID, storeRoot string, limits ravenCheckpointLimits) (*ravenWorkspaceCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, limits.Time)
	defer cancel()
	if err := ravenCheckpointAdmitWorker(bounded); err != nil {
		return nil, err
	}
	type response struct {
		tracker *ravenWorkspaceCheckpoint
		err     error
	}
	done := make(chan response, 1)
	go func() {
		defer func() { <-ravenCheckpointWorkers }()
		tracker, err := ravenCheckpointInitialize(bounded, workspace, sessionID, storeRoot, limits)
		done <- response{tracker, err}
	}()
	select {
	case result := <-done:
		return result.tracker, result.err
	case <-bounded.Done():
		return nil, bounded.Err()
	}
}

func ravenCheckpointInitialize(ctx context.Context, workspace, sessionID, storeRoot string, limits ravenCheckpointLimits) (*ravenWorkspaceCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(storeRoot) == "" {
		return nil, errors.New("workspace checkpoints are disabled without explicit native state and session identity")
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, errors.New("checkpoint workspace is unavailable")
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(canonical)
	if err != nil || !info.IsDir() {
		return nil, errors.New("checkpoint workspace is not a directory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	if ravenCheckpointWithin(canonical, home) {
		return nil, errors.New("checkpoint refuses home directory or its ancestor")
	}
	storeRoot, err = ravenCheckpointFuturePath(storeRoot)
	if err != nil {
		return nil, err
	}
	if ravenCheckpointWithin(canonical, storeRoot) || ravenCheckpointWithin(storeRoot, canonical) {
		return nil, errors.New("checkpoint private store and workspace must not overlap")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ravenCheckpointPrivateDirectory(storeRoot); err != nil {
		return nil, err
	}
	identity := sha256.Sum256([]byte(canonical + "\x00" + sessionID))
	directory := filepath.Join(storeRoot, hex.EncodeToString(identity[:]))
	if err := ravenCheckpointPrivateDirectory(directory); err != nil {
		return nil, err
	}
	for _, name := range []string{"blobs", "snapshots"} {
		if err := ravenCheckpointPrivateDirectory(filepath.Join(directory, name)); err != nil {
			return nil, err
		}
	}
	storeInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	m := &ravenWorkspaceCheckpoint{workspace: canonical, sessionID: sessionID, directory: directory, workspaceInfo: info, storeInfo: storeInfo, limits: limits}
	root, err := m.openStore()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if data, err := ravenCheckpointReadPrivate(root, "latest.json", 2<<20); err == nil {
		var receipt ravenCheckpointReceipt
		if json.Unmarshal(data, &receipt) != nil || m.verifyReceipt(receipt) != nil {
			return nil, errors.New("saved workspace checkpoint identity or manifest is invalid")
		}
		seen := make(map[string]bool)
		for _, file := range receipt.Files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if seen[file.SHA256] {
				continue
			}
			seen[file.SHA256] = true
			data, err := ravenCheckpointReadPrivate(root, "blobs/"+file.SHA256, limits.FileBytes)
			if err != nil || int64(len(data)) != file.Bytes || ravenCheckpointHash(data) != file.SHA256 {
				return nil, errors.New("saved workspace checkpoint blob is unavailable or corrupt")
			}
		}
		m.previous = &receipt
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("saved workspace checkpoint is unavailable")
	}
	return m, nil
}

func (m *ravenWorkspaceCheckpoint) ResumeObservation() string {
	if m == nil || m.previous == nil {
		return ""
	}
	prefix := "[Prior workspace checkpoint]\n"
	if m.previous.Label == "running" {
		prefix += "The previous run has no final filesystem capture. Its baseline remains recoverable; modifications after that capture may be incomplete or made by another actor. Inspect the current files before continuing.\n"
	}
	return prefix + m.previous.Observation()
}

func (m *ravenWorkspaceCheckpoint) Capture(ctx context.Context, label string) (ravenCheckpointReceipt, error) {
	if m == nil {
		return ravenCheckpointReceipt{Status: "disabled"}, errors.New("workspace checkpoint is disabled")
	}
	if err := ctx.Err(); err != nil {
		return ravenCheckpointReceipt{Status: "cancelled"}, err
	}
	bounded, cancel := context.WithTimeout(ctx, m.limits.Time)
	defer cancel()
	if err := ravenCheckpointAdmitWorker(bounded); err != nil {
		return ravenCheckpointReceipt{Status: "worker_unavailable"}, err
	}
	type response struct {
		receipt ravenCheckpointReceipt
		err     error
	}
	done := make(chan response, 1)
	go func() {
		defer func() { <-ravenCheckpointWorkers }()
		receipt, err := m.capture(bounded, label)
		done <- response{receipt, err}
	}()
	select {
	case result := <-done:
		return result.receipt, result.err
	case <-bounded.Done():
		return ravenCheckpointReceipt{Status: "cancelled", Label: label}, bounded.Err()
	}
}

func ravenCheckpointAdmitWorker(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case ravenCheckpointWorkers <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return errors.New("workspace checkpoint worker capacity unavailable")
	}
}

func (m *ravenWorkspaceCheckpoint) capture(ctx context.Context, label string) (ravenCheckpointReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	receipt := ravenCheckpointReceipt{Version: 1, SourceCommit: RavenCodeSourceCommit, Workspace: m.workspace, SessionID: m.sessionID, Label: label, CapturedAt: time.Now().UTC(), Status: "unavailable"}
	if err := ctx.Err(); err != nil {
		receipt.Status = "cancelled"
		return receipt, err
	}
	bounded, cancel := context.WithTimeout(ctx, m.limits.Time)
	defer cancel()
	fail := func(status string, err error) (ravenCheckpointReceipt, error) {
		receipt.Status = status
		receipt.Available = false
		receipt.ID = ""
		receipt.Files = nil
		receipt.ChangedPaths = nil
		return receipt, err
	}
	workspaceRoot, err := ravenCheckpointOpenPinnedRoot(m.workspace, m.workspaceInfo, false)
	if err != nil {
		return fail("workspace_unavailable", err)
	}
	defer workspaceRoot.Close()
	store, err := m.openStore()
	if err != nil {
		return fail("store_unavailable", err)
	}
	defer store.Close()
	paths, err := m.scanPaths(bounded, workspaceRoot)
	if err != nil {
		return fail(ravenCheckpointErrorStatus(err), err)
	}
	ignored, err := m.ignoredPaths(bounded, paths)
	if err != nil {
		return fail("ignore_rules_unavailable", err)
	}
	files, contents, err := m.readFiles(bounded, workspaceRoot, paths, ignored)
	if err != nil {
		return fail(ravenCheckpointErrorStatus(err), err)
	}
	// A second metadata/list pass refuses concurrent renames, additions and
	// deletions rather than publishing a partial tree as authoritative.
	secondPaths, err := m.scanPaths(bounded, workspaceRoot)
	if err != nil || !ravenCheckpointSameStrings(paths, secondPaths) {
		return fail("workspace_changed_during_capture", errors.New("workspace changed during capture"))
	}
	secondIgnored, err := m.ignoredPaths(bounded, secondPaths)
	if err != nil {
		return fail("ignore_rules_unavailable", err)
	}
	for _, path := range paths {
		if ignored[path] != secondIgnored[path] {
			return fail("workspace_changed_during_capture", errors.New("workspace ignore rules changed during capture"))
		}
	}
	for _, file := range files {
		if err := bounded.Err(); err != nil {
			return fail("cancelled", err)
		}
		data, err := ravenCheckpointReadWorkspace(workspaceRoot, file.Path, m.limits.FileBytes)
		info, statErr := workspaceRoot.Lstat(file.Path)
		if err != nil || statErr != nil || uint32(info.Mode().Perm()) != file.Mode || ravenCheckpointHash(data) != file.SHA256 {
			return fail("workspace_changed_during_capture", errors.New("workspace changed during capture"))
		}
	}
	if err := m.checkStorage(bounded, store, contents); err != nil {
		return fail("storage_limit_or_unavailable", err)
	}
	receipt.Files = files
	receipt.ID = ravenCheckpointTreeID(files)
	receipt.Available = true
	receipt.Status = "captured"
	if m.baseline != nil {
		receipt.ChangedPaths = ravenCheckpointChanged(m.baseline.Files, files)
	}
	for hash, data := range contents {
		if err := bounded.Err(); err != nil {
			return fail("cancelled", err)
		}
		name := "blobs/" + hash
		if saved, err := ravenCheckpointReadPrivate(store, name, m.limits.FileBytes); err == nil {
			if !bytes.Equal(saved, data) {
				return fail("blob_integrity_error", errors.New("checkpoint blob is invalid"))
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail("blob_unavailable", err)
		} else if err := ravenCheckpointAtomicWrite(bounded, store, name, data); err != nil {
			return fail("blob_unavailable", err)
		}
	}
	encoded, err := json.Marshal(receipt)
	if err != nil || len(encoded) > 2<<20 {
		return fail("manifest_limit", errors.New("checkpoint manifest exceeds bound"))
	}
	// Each turn's immutable manifest includes its phase and capture time. Tree
	// SHA identifies bytes; receipt SHA identifies this recorded boundary.
	manifestID := ravenCheckpointHash(encoded)
	if err := ravenCheckpointAtomicWrite(bounded, store, "snapshots/"+manifestID+".json", encoded); err != nil {
		return fail("manifest_unavailable", err)
	}
	if err := ravenCheckpointAtomicWrite(bounded, store, "latest.json", encoded); err != nil {
		return fail("manifest_unavailable", err)
	}
	if m.baseline == nil {
		copy := receipt
		m.baseline = &copy
	}
	return receipt, nil
}

func (m *ravenWorkspaceCheckpoint) verifyReceipt(receipt ravenCheckpointReceipt) error {
	if receipt.Version != 1 || receipt.Workspace != m.workspace || receipt.SessionID != m.sessionID || receipt.SourceCommit != RavenCodeSourceCommit || !receipt.Available || receipt.ID != ravenCheckpointTreeID(receipt.Files) || len(receipt.Files) > m.limits.Files {
		return errors.New("invalid checkpoint identity")
	}
	var total int64
	previous := ""
	for _, file := range receipt.Files {
		if !ravenCheckpointSafePath(file.Path) || ravenCheckpointExcluded(file.Path, false) || file.Path <= previous || !ravenCheckpointValidHash(file.SHA256) || file.Bytes < 0 || file.Bytes > m.limits.FileBytes {
			return errors.New("invalid checkpoint file")
		}
		previous = file.Path
		total += file.Bytes
	}
	if total > m.limits.TotalBytes {
		return errors.New("checkpoint byte limit")
	}
	return nil
}

// ReadFile is a bounded, read-only extraction primitive for explicit recovery.
// It cannot write, delete or restore the workspace. Consumers must expose any
// undo separately and obtain the user's explicit choice of files/generation.
func (m *ravenWorkspaceCheckpoint) ReadFile(ctx context.Context, receipt ravenCheckpointReceipt, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.verifyReceipt(receipt); err != nil {
		return nil, err
	}
	for _, file := range receipt.Files {
		if file.Path != path {
			continue
		}
		root, err := m.openStore()
		if err != nil {
			return nil, err
		}
		defer root.Close()
		data, err := ravenCheckpointReadPrivate(root, "blobs/"+file.SHA256, m.limits.FileBytes)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != file.Bytes || ravenCheckpointHash(data) != file.SHA256 {
			return nil, errors.New("checkpoint blob integrity failure")
		}
		return data, ctx.Err()
	}
	return nil, os.ErrNotExist
}

func (m *ravenWorkspaceCheckpoint) scanPaths(ctx context.Context, root *os.Root) ([]string, error) {
	var paths []string
	entries := 0
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		before, err := root.Lstat(directory)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return errors.New("checkpoint directory changed during discovery")
		}
		dir, err := ravenCheckpointOpenFile(root, directory)
		if err != nil {
			return err
		}
		defer dir.Close()
		opened, err := dir.Stat()
		if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
			return errors.New("checkpoint directory changed during open")
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch, readErr := dir.ReadDir(128)
			for _, entry := range batch {
				entries++
				if entries > m.limits.Entries {
					return errors.New("checkpoint entry limit")
				}
				path := entry.Name()
				if directory != "." {
					path = directory + "/" + path
				}
				if len(path) > 4096 || !ravenCheckpointSafePath(path) {
					return errors.New("checkpoint path limit or unsupported name")
				}
				if ravenCheckpointExcluded(path, entry.IsDir()) || entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				if entry.IsDir() {
					if err := walk(path); err != nil {
						return err
					}
					continue
				}
				if !entry.Type().IsRegular() {
					continue
				}
				paths = append(paths, path)
				if len(paths) > m.limits.Files {
					return errors.New("checkpoint file limit")
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	err := walk(".")
	sort.Strings(paths)
	return paths, err
}

func (m *ravenWorkspaceCheckpoint) readFiles(ctx context.Context, root *os.Root, paths []string, ignored map[string]bool) ([]ravenCheckpointFile, map[string][]byte, error) {
	files := make([]ravenCheckpointFile, 0, len(paths))
	contents := make(map[string][]byte)
	var total int64
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if ignored[path] {
			continue
		}
		info, err := root.Lstat(path)
		if err != nil {
			return nil, nil, err
		}
		data, err := ravenCheckpointReadWorkspace(root, path, m.limits.FileBytes)
		if err != nil {
			return nil, nil, err
		}
		total += int64(len(data))
		if total > m.limits.TotalBytes {
			return nil, nil, errors.New("checkpoint byte limit")
		}
		hash := ravenCheckpointHash(data)
		files = append(files, ravenCheckpointFile{Path: path, SHA256: hash, Bytes: int64(len(data)), Mode: uint32(info.Mode().Perm())})
		contents[hash] = data
	}
	return files, contents, nil
}

func (m *ravenWorkspaceCheckpoint) ignoredPaths(ctx context.Context, paths []string) (map[string]bool, error) {
	result := make(map[string]bool)
	if len(paths) == 0 {
		return result, nil
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New("git ignore evaluator is unavailable")
	}
	// An existing repository supplies its own nested/ancestor ignore rules and
	// info/exclude. The command is read-only and never uses or changes its index.
	probe := exec.CommandContext(ctx, git, "-C", m.workspace, "rev-parse", "--show-toplevel")
	probe.Env = ravenCheckpointGitEnvironment()
	probe.Stdout = io.Discard
	probe.Stderr = io.Discard
	if probe.Run() == nil {
		return ravenCheckpointCheckIgnore(ctx, git, m.workspace, nil, paths)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Do not silently discard a user's info/exclude rules when a repository
	// exists but Git cannot read it. The isolated fallback is only for a
	// genuinely non-Git workspace.
	for directory := m.workspace; ; directory = filepath.Dir(directory) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(filepath.Join(directory, ".git")); err == nil {
			return nil, errors.New("workspace repository ignore rules are unavailable")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("workspace repository state is unavailable")
		}
		if filepath.Dir(directory) == directory {
			break
		}
	}
	shadow := filepath.Join(m.directory, "ignore.git")
	if _, err := os.Lstat(shadow); errors.Is(err, os.ErrNotExist) {
		cmd := exec.CommandContext(ctx, git, "init", "--bare", "--template=", shadow)
		cmd.Env = ravenCheckpointGitEnvironment()
		cmd.Dir = m.directory
		if err := cmd.Run(); err != nil {
			return nil, errors.New("isolated ignore evaluator initialization failed")
		}
		if err := filepath.WalkDir(shadow, func(path string, entry fs.DirEntry, err error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("ignore evaluator contains a symlink")
			}
			mode := os.FileMode(0o600)
			if entry.IsDir() {
				mode = 0o700
			}
			return os.Chmod(path, mode)
		}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if err := ravenCheckpointPrivateDirectory(shadow); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ravenCheckpointCheckIgnore(ctx, git, m.workspace, []string{"--git-dir=" + shadow, "--work-tree=" + m.workspace, "-c", "core.bare=false"}, paths)
}

func ravenCheckpointCheckIgnore(ctx context.Context, git, workspace string, prefix, paths []string) (map[string]bool, error) {
	result := make(map[string]bool)
	args := append(append([]string(nil), prefix...), "check-ignore", "--no-index", "-z", "--stdin")
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Env = ravenCheckpointGitEnvironment()
	cmd.Dir = workspace
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	output := &ravenCheckpointBoundedBuffer{max: 2 << 20}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, errors.New("workspace ignore rules unavailable")
		}
	}
	if output.exceeded {
		return nil, errors.New("ignore result exceeds checkpoint bound")
	}
	for _, path := range strings.Split(output.String(), "\x00") {
		if path != "" {
			result[path] = true
		}
	}
	return result, nil
}

func (m *ravenWorkspaceCheckpoint) checkStorage(ctx context.Context, root *os.Root, contents map[string][]byte) error {
	var total int64
	snapshots := 0
	entries := 0
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := ravenCheckpointOpenFile(root, directory)
		if err != nil {
			return err
		}
		defer dir.Close()
		info, err := dir.Stat()
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("private checkpoint directory is unsafe")
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch, readErr := dir.ReadDir(128)
			for _, entry := range batch {
				entries++
				if entries > m.limits.Entries {
					return errors.New("checkpoint retained entry limit")
				}
				path := entry.Name()
				if directory != "." {
					path = directory + "/" + path
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return errors.New("private checkpoint store contains a symlink")
				}
				if entry.IsDir() {
					if err := walk(path); err != nil {
						return err
					}
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
					return errors.New("private checkpoint file is unsafe")
				}
				total += info.Size()
				if strings.HasPrefix(path, "snapshots/") {
					snapshots++
				}
				if total > m.limits.StoreBytes || snapshots >= m.limits.Snapshots {
					return errors.New("checkpoint retained storage limit")
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	if err := walk("."); err != nil {
		return err
	}
	for hash, data := range contents {
		if _, err := root.Lstat("blobs/" + hash); errors.Is(err, os.ErrNotExist) {
			total += int64(len(data))
		} else if err != nil {
			return err
		}
	}
	if total+(2<<20) > m.limits.StoreBytes {
		return errors.New("checkpoint retained byte limit")
	}
	return nil
}

func ravenCheckpointReadWorkspace(root *os.Root, path string, maxBytes int64) ([]byte, error) {
	if !ravenCheckpointSafePath(path) {
		return nil, errors.New("checkpoint path escapes workspace")
	}
	components := strings.Split(path, "/")
	for i := range components {
		info, err := root.Lstat(strings.Join(components[:i+1], "/"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("checkpoint path is unavailable or symlinked")
		}
	}
	before, err := root.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxBytes {
		return nil, errors.New("checkpoint file unavailable or exceeds byte limit")
	}
	file, err := ravenCheckpointOpenFile(root, path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("checkpoint file changed during open")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes {
		return nil, errors.New("checkpoint file read exceeded bound")
	}
	after, err := root.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("checkpoint file changed during read")
	}
	return data, nil
}

func ravenCheckpointReadPrivate(root *os.Root, path string, maxBytes int64) ([]byte, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("checkpoint private file is unsafe")
	}
	return ravenCheckpointReadWorkspace(root, path, maxBytes)
}

func ravenCheckpointAtomicWrite(ctx context.Context, root *os.Root, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ravenCheckpointSafePath(path) {
		return errors.New("checkpoint storage path invalid")
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temp := ".capture-" + hex.EncodeToString(random[:])
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return root.Rename(temp, path)
}

func ravenCheckpointPrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("checkpoint private directory is unsafe")
	}
	return nil
}

func (m *ravenWorkspaceCheckpoint) openStore() (*os.Root, error) {
	return ravenCheckpointOpenPinnedRoot(m.directory, m.storeInfo, true)
}

func ravenCheckpointOpenPinnedRoot(path string, expected os.FileInfo, private bool) (*os.Root, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return nil, errors.New("checkpoint directory containment changed")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, expected) || (private && before.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("checkpoint directory identity changed")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		root.Close()
		return nil, errors.New("checkpoint directory changed during open")
	}
	return root, nil
}

func ravenCheckpointFuturePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ancestor := absolute
	var suffix []string
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		suffix = append(suffix, filepath.Base(ancestor))
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return "", errors.New("checkpoint state ancestor unavailable")
		}
		ancestor = next
	}
	canonical, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		canonical = filepath.Join(canonical, suffix[i])
	}
	return canonical, nil
}

func ravenCheckpointExcluded(path string, isDir bool) bool {
	for _, component := range strings.Split(strings.ToLower(path), "/") {
		switch component {
		case ".git", ".raven", ".qwen", ".codex", ".meadow", ".meadow-recovery", ".ssh", ".gnupg", ".aws", ".azure", ".config", "__pycache__", "node_modules", "dist", "build", "target", "out", ".next", ".nuxt", "venv", ".venv", ".tox", ".idea", ".vscode", "logs", ".eggs":
			return true
		}
	}
	name := strings.ToLower(filepath.Base(path))
	if name == ".env" || strings.HasPrefix(name, ".env.") || strings.HasPrefix(name, "id_rsa") || strings.HasPrefix(name, "id_dsa") || strings.HasPrefix(name, "id_ecdsa") || strings.HasPrefix(name, "id_ed25519") || strings.HasPrefix(name, "api-key") || strings.HasPrefix(name, "api_key") {
		return true
	}
	switch name {
	case "secrets.yaml", "secrets.yml", "secrets.json", "credentials", "credentials.json", "credentials.toml", ".ds_store", "thumbs.db":
		return true
	}
	for _, suffix := range []string{".key", ".pem", ".crt", ".p12", ".ppk", ".pyc", ".pyo", ".log", ".egg-info"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func ravenCheckpointWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func ravenCheckpointSafePath(path string) bool {
	return path != "" && path != "." && !strings.ContainsRune(path, '\x00') && !strings.ContainsRune(path, '\n') && !strings.Contains(path, "\\") && fs.ValidPath(path)
}
func ravenCheckpointHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func ravenCheckpointValidHash(hash string) bool {
	data, err := hex.DecodeString(hash)
	return err == nil && len(data) == sha256.Size && hash == strings.ToLower(hash)
}
func ravenCheckpointTreeID(files []ravenCheckpointFile) string {
	if files == nil {
		files = []ravenCheckpointFile{}
	}
	data, _ := json.Marshal(files)
	return ravenCheckpointHash(data)
}
func ravenCheckpointSameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func ravenCheckpointChanged(before, after []ravenCheckpointFile) []string {
	state := make(map[string]ravenCheckpointFile, len(before))
	for _, file := range before {
		state[file.Path] = file
	}
	var paths []string
	for _, file := range after {
		old, found := state[file.Path]
		if !found || old.SHA256 != file.SHA256 || old.Mode != file.Mode {
			paths = append(paths, file.Path)
		}
		delete(state, file.Path)
	}
	for path := range state {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
func ravenCheckpointErrorStatus(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	if strings.Contains(err.Error(), "limit") || strings.Contains(err.Error(), "bound") {
		return "capture_limit"
	}
	return "workspace_unavailable"
}

func ravenCheckpointGitEnvironment() []string {
	var result []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			result = append(result, value)
		}
	}
	return append(result, "GIT_TERMINAL_PROMPT=0")
}

type ravenCheckpointBoundedBuffer struct {
	bytes.Buffer
	max      int
	exceeded bool
}

func (b *ravenCheckpointBoundedBuffer) Write(data []byte) (int, error) {
	n := len(data)
	available := b.max - b.Len()
	if available < n {
		b.exceeded = true
		data = data[:max(available, 0)]
	}
	_, _ = b.Buffer.Write(data)
	return n, nil
}
