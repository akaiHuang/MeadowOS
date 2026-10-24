package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Raven code-flow's active peer notice and archive use observed workspace
// state. This native seat has no git write, restoration or task-selection API.
var ravenWorkspaceRuns = struct {
	sync.Mutex
	active map[string]map[uint64]*ravenWorkspaceRun
}{active: map[string]map[uint64]*ravenWorkspaceRun{}}
var ravenWorkspaceSequence atomic.Uint64

type ravenWorkspaceRun struct {
	cwd    string
	id     uint64
	base   ravenWorkspaceManifest
	shared map[uint64]bool
}

type RavenWorkspaceManifest struct {
	Version             string   `json:"version"`
	Scope               string   `json:"scope"`
	Attribution         string   `json:"attribution"`
	SharedWith          int      `json:"sharedWith"`
	WorkspaceKind       string   `json:"workspaceKind"`
	RepositoryRoot      string   `json:"repositoryRoot,omitempty"`
	Branch              string   `json:"branch,omitempty"`
	BaseCommit          string   `json:"baseCommit,omitempty"`
	CommitsPastBase     int      `json:"commitsPastBase"`
	CommitIDs           []string `json:"commitIDs,omitempty"`
	WorkingTreeClean    bool     `json:"workingTreeClean"`
	UncommittedCount    int      `json:"uncommittedCount"`
	DiffStat            string   `json:"diffStat,omitempty"`
	Status              string   `json:"status"`
	Blockers            []string `json:"blockers,omitempty"`
	ReadyForIntegration bool     `json:"readyForIntegration"`
	GitAvailable        bool     `json:"git_available"`
	Commit              string   `json:"commit,omitempty"`
	ChangedPaths        []string `json:"changed_paths,omitempty"`
	StatusDigest        string   `json:"status_digest,omitempty"`
	Truncated           bool     `json:"truncated,omitempty"`
	Skipped             string   `json:"skipped,omitempty"`
}

type ravenWorkspaceManifest = RavenWorkspaceManifest

func ravenBeginWorkspace(ctx context.Context, cwd, sessionID string, observe ...bool) *ravenWorkspaceRun {
	if cwd == "" || ctx.Err() != nil {
		return nil
	}
	root, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil
	}
	run := &ravenWorkspaceRun{cwd: root, id: ravenWorkspaceSequence.Add(1), shared: map[uint64]bool{}}
	ravenWorkspaceRuns.Lock()
	if ravenWorkspaceRuns.active[root] == nil {
		ravenWorkspaceRuns.active[root] = map[uint64]*ravenWorkspaceRun{}
	}
	for id, peer := range ravenWorkspaceRuns.active[root] {
		run.shared[id] = true
		peer.shared[run.id] = true
	}
	ravenWorkspaceRuns.active[root][run.id] = run
	ravenWorkspaceRuns.Unlock()
	if len(observe) == 0 || observe[0] {
		run.base = ravenObserveWorkspace(ctx, root)
	}
	return run
}

func (r *ravenWorkspaceRun) sharedWith() int {
	if r == nil {
		return 0
	}
	ravenWorkspaceRuns.Lock()
	defer ravenWorkspaceRuns.Unlock()
	return len(r.shared)
}

func (r *ravenWorkspaceRun) close() {
	if r == nil {
		return
	}
	ravenWorkspaceRuns.Lock()
	delete(ravenWorkspaceRuns.active[r.cwd], r.id)
	if len(ravenWorkspaceRuns.active[r.cwd]) == 0 {
		delete(ravenWorkspaceRuns.active, r.cwd)
	}
	ravenWorkspaceRuns.Unlock()
}

func (r *ravenWorkspaceRun) peers() int {
	if r == nil {
		return 0
	}
	ravenWorkspaceRuns.Lock()
	defer ravenWorkspaceRuns.Unlock()
	return max(0, len(ravenWorkspaceRuns.active[r.cwd])-1)
}

func (r *ravenWorkspaceRun) concurrencyNotice() string {
	peers := r.peers()
	if peers == 0 {
		return ""
	}
	return fmt.Sprintf("# Workspace concurrency\n%d other native Meadow session(s) are currently working in this same directory; the tree is shared. Re-read a target before writing, change only this task's authorized files, and preserve unexplained changes. If a target changes unexpectedly, stop writing that file and report the conflict.", peers)
}

type ravenBoundedGitOutput struct {
	bytes     []byte
	truncated bool
}

func (o *ravenBoundedGitOutput) Write(bytes []byte) (int, error) {
	n := len(bytes)
	room := max(0, 16384-len(o.bytes))
	if len(bytes) > room {
		o.truncated = true
		bytes = bytes[:room]
	}
	o.bytes = append(o.bytes, bytes...)
	return n, nil
}

func ravenGitRead(ctx context.Context, cwd string, args ...string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	command := exec.CommandContext(bounded, "git", append([]string{"--no-optional-locks", "-C", cwd}, args...)...)
	output := &ravenBoundedGitOutput{}
	command.Stdout = output
	// stderr is not an observation or a prompt source; no credentials/config
	// diagnostics are exposed by the workspace metadata seat.
	err := command.Run()
	return string(output.bytes), output.truncated, err
}

func ravenObserveWorkspace(ctx context.Context, cwd string) ravenWorkspaceManifest {
	manifest := ravenWorkspaceManifest{Version: "raven_workspace_manifest.v1", Scope: "workspace", Attribution: "session", WorkspaceKind: "plain", Status: "unknown"}
	if ctx.Err() != nil {
		manifest.Skipped = "caller_cancelled"
		return manifest
	}
	commit, truncated, err := ravenGitRead(ctx, cwd, "rev-parse", "HEAD")
	if err != nil || truncated {
		manifest.Skipped = "git_unavailable_or_unborn"
		return manifest
	}
	manifest.GitAvailable = true
	manifest.WorkspaceKind = "repository"
	manifest.Commit = strings.TrimSpace(commit)
	manifest.Branch, _, err = ravenGitRead(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	manifest.Branch = strings.TrimSpace(manifest.Branch)
	if err != nil || manifest.Branch == "" {
		manifest.Blockers = append(manifest.Blockers, "branch could not be read")
	}
	manifest.RepositoryRoot, _, err = ravenGitRead(ctx, cwd, "rev-parse", "--show-toplevel")
	manifest.RepositoryRoot = strings.TrimSpace(manifest.RepositoryRoot)
	if err != nil {
		manifest.Blockers = append(manifest.Blockers, "repository root could not be read")
	}
	status, truncated, err := ravenGitRead(ctx, cwd, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".")
	manifest.Truncated = truncated
	if err != nil {
		manifest.Skipped = "status_unavailable"
		return manifest
	}
	manifest.StatusDigest = ravenMemoryDigest(status)
	manifest.ChangedPaths, manifest.UncommittedCount, err = ravenStatusPaths(status)
	if err != nil || truncated {
		manifest.Blockers = append(manifest.Blockers, "working-tree status is partial or invalid")
	}
	manifest.WorkingTreeClean = len(manifest.Blockers) == 0 && manifest.UncommittedCount == 0
	if len(manifest.Blockers) == 0 {
		manifest.Status = "observed"
	}
	return manifest
}

func ravenStatusPaths(status string) ([]string, int, error) {
	if status == "" {
		return nil, 0, nil
	}
	if !strings.HasSuffix(status, "\x00") {
		return nil, 0, fmt.Errorf("incomplete NUL status")
	}
	entries := strings.Split(strings.TrimSuffix(status, "\x00"), "\x00")
	paths := map[string]bool{}
	count := 0
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 || entry[2] != ' ' {
			return nil, 0, fmt.Errorf("invalid porcelain entry")
		}
		paths[entry[3:]] = true
		count++
		if strings.ContainsAny(entry[:2], "RC") {
			i++
			if i >= len(entries) || entries[i] == "" {
				return nil, 0, fmt.Errorf("missing rename source")
			}
			paths[entries[i]] = true
		}
	}
	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, count, nil
}

func (r *ravenWorkspaceRun) archive(ctx context.Context) ravenWorkspaceManifest {
	manifest := ravenObserveWorkspace(ctx, r.cwd)
	manifest.BaseCommit = r.base.Commit
	manifest.SharedWith = r.sharedWith()
	if manifest.SharedWith > 0 {
		manifest.Attribution = "shared"
	}
	if manifest.BaseCommit == "" || !manifest.GitAvailable {
		manifest.Blockers = append(manifest.Blockers, "base commit unavailable")
		return manifest
	}
	count, cut, err := ravenGitRead(ctx, r.cwd, "rev-list", "--count", manifest.BaseCommit+"..HEAD")
	manifest.CommitsPastBase, _ = strconv.Atoi(strings.TrimSpace(count))
	if err != nil || cut {
		manifest.Blockers = append(manifest.Blockers, "commits past base could not be read")
	}
	ids, cut, err := ravenGitRead(ctx, r.cwd, "log", "--format=%H", "-100", manifest.BaseCommit+"..HEAD")
	if err != nil || cut {
		manifest.Blockers = append(manifest.Blockers, "commit list could not be read")
	} else if strings.TrimSpace(ids) != "" {
		manifest.CommitIDs = strings.Fields(ids)
	}
	stat, cut, err := ravenGitRead(ctx, r.cwd, "diff", "--shortstat", manifest.BaseCommit, "--", ".")
	manifest.DiffStat = strings.TrimSpace(stat)
	if err != nil || cut {
		manifest.Blockers = append(manifest.Blockers, "diff stat could not be read")
	}
	if len(manifest.Blockers) > 0 {
		manifest.Status = "unknown"
		return manifest
	}
	if manifest.SharedWith > 0 {
		manifest.Status = "shared_workspace"
		manifest.Blockers = append(manifest.Blockers, "workspace shared during this run; commits cannot be attributed to this session")
	} else if manifest.UncommittedCount > 0 {
		manifest.Status = "needs_commit"
		manifest.Blockers = append(manifest.Blockers, "uncommitted changes")
	} else if manifest.CommitsPastBase == 0 {
		manifest.Status = "no_changes"
		manifest.Blockers = append(manifest.Blockers, "no commits past base")
	} else {
		manifest.Status = "ready_for_integration"
		manifest.ReadyForIntegration = true
	}
	return manifest
}

func ravenPersistWorkspaceManifest(ctx context.Context, root, sessionID, cwd string, manifest *RavenWorkspaceManifest) error {
	if root == "" || sessionID == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("private workspace archive root must be absolute")
	}
	scope := sha256.Sum256([]byte(sessionID + "\x00" + cwd))
	dir := filepath.Join(root, "workspace-manifests", hex.EncodeToString(scope[:]))
	for _, path := range []string{root, filepath.Join(root, "workspace-manifests"), dir} {
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("unsafe private workspace archive directory")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}
	bytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(bytes) > 128*1024 {
		return fmt.Errorf("private workspace manifest exceeds bounded metadata size")
	}
	file, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(bytes); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, "latest.json"))
}
