package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	subtaskStateVersion     = 1
	subtaskStateMaxBytes    = 8 << 20
	subtaskUntrackedMax     = 100 << 20
	subtaskUntrackedMaxFile = 20 << 20
)

var subagentIDPattern = regexp.MustCompile(`^sa_[a-zA-Z0-9_-]{4,80}$`)

type subtaskState struct {
	Version         int                  `json:"version"`
	SubagentID      string               `json:"subagent_id"`
	ParentCWD       string               `json:"parent_cwd,omitempty"`
	ParentSessionID string               `json:"parent_session_id,omitempty"`
	ModelID         string               `json:"model_id"`
	Spec            tools.SubtaskSpec    `json:"spec"`
	Status          string               `json:"status"`
	CWD             string               `json:"cwd"`
	WorktreePath    string               `json:"worktree_path,omitempty"`
	SnapshotRef     string               `json:"snapshot_ref,omitempty"`
	ResumedFrom     string               `json:"resumed_from,omitempty"`
	History         []provider.Message   `json:"history,omitempty"`
	Outcome         tools.SubtaskOutcome `json:"outcome,omitempty"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
}

type subtaskStore struct {
	dir string
	mu  sync.Mutex
}

func newSubtaskStore(dir string) *subtaskStore {
	return &subtaskStore{dir: strings.TrimSpace(dir)}
}

func (s *subtaskStore) enabled() bool {
	return s != nil && s.dir != ""
}

func (s *subtaskStore) load(id string) (subtaskState, bool, error) {
	if !s.enabled() {
		return subtaskState{}, false, nil
	}
	path, err := s.path(id)
	if err != nil {
		return subtaskState{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return subtaskState{}, false, nil
	}
	if err != nil {
		return subtaskState{}, false, fmt.Errorf("read subagent state: %w", err)
	}
	if len(data) > subtaskStateMaxBytes {
		return subtaskState{}, false, fmt.Errorf("subagent state %s exceeds %d bytes", id, subtaskStateMaxBytes)
	}
	var state subtaskState
	if err := json.Unmarshal(data, &state); err != nil {
		return subtaskState{}, false, fmt.Errorf("parse subagent state: %w", err)
	}
	if state.Version != 0 && state.Version != subtaskStateVersion {
		return subtaskState{}, false, fmt.Errorf("unsupported subagent state version %d", state.Version)
	}
	if state.SubagentID != id {
		return subtaskState{}, false, fmt.Errorf("subagent state identity mismatch for %s", id)
	}
	return state, true, nil
}

func (s *subtaskStore) save(state subtaskState) error {
	if !s.enabled() {
		return nil
	}
	path, err := s.path(state.SubagentID)
	if err != nil {
		return err
	}
	state.Version = subtaskStateVersion
	state.UpdatedAt = time.Now().UTC()
	if state.CreatedAt.IsZero() {
		state.CreatedAt = state.UpdatedAt
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode subagent state: %w", err)
	}
	if len(data) > subtaskStateMaxBytes {
		return fmt.Errorf("subagent state %s exceeds %d bytes", state.SubagentID, subtaskStateMaxBytes)
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create subagent state directory: %w", err)
	}
	temp, err := os.CreateTemp(s.dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create subagent state temp file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("commit subagent state: %w", err)
	}
	return nil
}

func (s *subtaskStore) path(id string) (string, error) {
	if !subagentIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid subagent_id %q", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

type subtaskWorktree struct {
	CWD         string
	Path        string
	SnapshotRef string
}

func createSubtaskWorktree(
	ctx context.Context,
	parentCWD, baseDir, subagentID string,
) (subtaskWorktree, error) {
	if !subagentIDPattern.MatchString(subagentID) {
		return subtaskWorktree{}, fmt.Errorf("invalid subagent_id %q", subagentID)
	}
	root, err := gitText(ctx, parentCWD, "rev-parse", "--show-toplevel")
	if err != nil {
		return subtaskWorktree{}, fmt.Errorf("worktree isolation requires a git repository: %w", err)
	}
	root = filepath.Clean(root)
	canonicalParent := filepath.Clean(parentCWD)
	if resolved, resolveErr := filepath.EvalSymlinks(canonicalParent); resolveErr == nil {
		canonicalParent = resolved
	}
	relativeCWD, err := filepath.Rel(root, canonicalParent)
	if err != nil || relativeCWD == ".." || strings.HasPrefix(relativeCWD, ".."+string(filepath.Separator)) {
		return subtaskWorktree{}, errors.New("current directory is outside the git repository root")
	}
	if strings.TrimSpace(baseDir) == "" {
		baseDir = filepath.Join(os.TempDir(), "meadow-subagent-worktrees")
	}
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return subtaskWorktree{}, fmt.Errorf("create subagent worktree base: %w", err)
	}
	destination := filepath.Join(baseDir, subagentID)
	if info, statErr := os.Stat(destination); statErr == nil && info.IsDir() {
		if _, gitErr := gitText(ctx, destination, "rev-parse", "--show-toplevel"); gitErr == nil {
			return subtaskWorktree{
				CWD: filepath.Join(destination, relativeCWD), Path: destination,
			}, nil
		}
		return subtaskWorktree{}, fmt.Errorf("subagent worktree destination already exists: %s", destination)
	}

	snapshotRef, err := gitText(ctx, root, "stash", "create", "meadow-subagent-"+subagentID)
	if err != nil {
		return subtaskWorktree{}, fmt.Errorf("snapshot tracked working tree: %w", err)
	}
	if snapshotRef == "" {
		snapshotRef, err = gitText(ctx, root, "rev-parse", "HEAD")
		if err != nil {
			return subtaskWorktree{}, fmt.Errorf("resolve worktree HEAD: %w", err)
		}
	}
	if _, err := gitCombined(
		ctx, root, nil, "worktree", "add", "--detach", destination, snapshotRef,
	); err != nil {
		return subtaskWorktree{}, fmt.Errorf("create isolated git worktree: %w", err)
	}
	if err := copySubtaskUntracked(ctx, root, destination); err != nil {
		_ = removeSubtaskWorktree(context.Background(), root, destination)
		return subtaskWorktree{}, err
	}
	childCWD := filepath.Join(destination, relativeCWD)
	if info, err := os.Stat(childCWD); err != nil || !info.IsDir() {
		_ = removeSubtaskWorktree(context.Background(), root, destination)
		return subtaskWorktree{}, fmt.Errorf("isolated child cwd does not exist: %s", childCWD)
	}
	return subtaskWorktree{
		CWD: childCWD, Path: destination, SnapshotRef: snapshotRef,
	}, nil
}

func removeSubtaskWorktree(ctx context.Context, repoRoot, worktreePath string) error {
	_, err := gitCombined(ctx, repoRoot, nil, "worktree", "remove", "--force", worktreePath)
	return err
}

func copySubtaskUntracked(ctx context.Context, sourceRoot, destination string) error {
	output, err := gitCombined(
		ctx, sourceRoot, nil, "ls-files", "--others", "--exclude-standard", "-z",
	)
	if err != nil {
		return fmt.Errorf("list untracked files for subagent snapshot: %w", err)
	}
	var total int64
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		relative := filepath.Clean(string(raw))
		if filepath.IsAbs(relative) || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe untracked path %q", relative)
		}
		source := filepath.Join(sourceRoot, relative)
		target := filepath.Join(destination, relative)
		info, err := os.Lstat(source)
		if err != nil {
			return fmt.Errorf("inspect untracked file %s: %w", relative, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(source)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if info.Size() > subtaskUntrackedMaxFile {
				return fmt.Errorf("untracked file %s exceeds %d bytes", relative, subtaskUntrackedMaxFile)
			}
			total += info.Size()
			if total > subtaskUntrackedMax {
				return fmt.Errorf("untracked snapshot exceeds %d bytes", subtaskUntrackedMax)
			}
			if err := copyRegularFile(source, target, info.Mode().Perm()); err != nil {
				return fmt.Errorf("copy untracked file %s: %w", relative, err)
			}
		}
	}
	return nil
}

func copyRegularFile(source, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func gitText(ctx context.Context, dir string, args ...string) (string, error) {
	output, err := gitCombined(ctx, dir, nil, args...)
	return strings.TrimSpace(string(output)), err
}

func gitCombined(
	ctx context.Context,
	dir string,
	stdin []byte,
	args ...string,
) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// A resumed lineage may have multiple child IDs pointing to one worktree.
// Serialize those writers, including separate parent invocations, while still
// allowing independent worktrees and read-only research to run concurrently.
var subtaskWorkspaces = struct {
	sync.Mutex
	leases map[string]*subtaskWorkspaceLease
}{leases: make(map[string]*subtaskWorkspaceLease)}

type subtaskWorkspaceLease struct {
	gate chan struct{}
	refs int
}

func acquireSubtaskWorkspace(ctx context.Context, path string) (func(), error) {
	path = filepath.Clean(path)
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		path = canonical
	}
	subtaskWorkspaces.Lock()
	lease := subtaskWorkspaces.leases[path]
	if lease == nil {
		lease = &subtaskWorkspaceLease{gate: make(chan struct{}, 1)}
		subtaskWorkspaces.leases[path] = lease
	}
	lease.refs++
	subtaskWorkspaces.Unlock()
	releaseRef := func() {
		subtaskWorkspaces.Lock()
		lease.refs--
		if lease.refs == 0 {
			delete(subtaskWorkspaces.leases, path)
		}
		subtaskWorkspaces.Unlock()
	}
	select {
	case lease.gate <- struct{}{}:
		return func() { <-lease.gate; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}
