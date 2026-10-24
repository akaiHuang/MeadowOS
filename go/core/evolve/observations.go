package evolve

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

func (f *files) observeFile(raw string) (tools.WorkspaceFileState, error) {
	rel := strings.TrimPrefix(raw, "/workspace/")
	if raw == "/workspace" {
		rel = "."
	}
	rel, err := relativePath(rel)
	if err != nil || rel == "." {
		return tools.WorkspaceFileState{}, errors.New("file observation requires a workspace file")
	}
	state := tools.WorkspaceFileState{Path: "/workspace/" + rel}
	parts := strings.Split(rel, "/")
	for i := range parts {
		info, e := f.workspace.Lstat(strings.Join(parts[:i+1], "/"))
		if os.IsNotExist(e) {
			return state, nil
		}
		if e != nil {
			return state, e
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return state, errors.New("unsafe file observation target")
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && info.Mode().IsRegular() && stat.Nlink > 1 {
			return state, errors.New("hard-linked observation target")
		}
	}
	h, err := f.workspace.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return state, err
	}
	defer h.Close()
	before, err := h.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxFileBytes {
		return state, errors.New("observation requires a regular file at most 1 MiB")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(h, maxFileBytes+1))
	if err != nil {
		return state, err
	}
	after, err := h.Stat()
	if err != nil {
		return state, err
	}
	current, err := f.workspace.Lstat(rel)
	if err != nil {
		return state, err
	}
	if n > maxFileBytes || n != before.Size() || after.Size() != before.Size() || !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, current) || current.Size() != after.Size() || !current.ModTime().Equal(after.ModTime()) {
		return state, errors.New("file changed during observation")
	}
	state.Exists = true
	state.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return state, nil
}

func (f *files) snapshotFiles(ignore string) (map[string]tools.WorkspaceFileState, error) {
	states := map[string]tools.WorkspaceFileState{}
	count, total := 0, int64(0)
	err := fs.WalkDir(f.workspace.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == ignore {
			return fs.SkipDir
		}
		count++
		if count > 2048 {
			return errors.New("workspace observation exceeds file count bound")
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > 32*maxFileBytes {
			return errors.New("workspace observation exceeds byte bound")
		}
		state, err := f.observeFile(p)
		if err != nil {
			return err
		}
		if state.Exists {
			states[state.Path] = state
		}
		return nil
	})
	return states, err
}

// Observe all process/hook side effects even when the program exits nonzero.
// Paths and hashes come from confined IO, never from program-printed claims.
func (f *files) trackMutations(ignore string, call func() tools.Result) tools.Result {
	before, err := f.snapshotFiles(ignore)
	if err != nil {
		return failure(err)
	}
	result := call()
	after, err := f.snapshotFiles(ignore)
	changed := map[string]bool{}
	for _, p := range result.ChangedPaths {
		changed[p] = true
	}
	for p, state := range before {
		if next, ok := after[p]; err != nil || !ok || next.SHA256 != state.SHA256 {
			changed[p] = true
		}
	}
	for p, state := range after {
		if old, ok := before[p]; !ok || old.SHA256 != state.SHA256 {
			changed[p] = true
		}
	}
	result.ChangedPaths = nil
	for p := range changed {
		result.ChangedPaths = append(result.ChangedPaths, p)
	}
	sort.Strings(result.ChangedPaths)
	if len(changed) > 0 {
		result.NoChange = false
	}
	kept := result.FileObservations[:0]
	for _, obs := range result.FileObservations {
		if current, ok := after[obs.Path]; err == nil && ok && current.SHA256 == obs.SHA256 {
			kept = append(kept, obs)
		}
	}
	result.FileObservations = kept
	if err != nil {
		result.Err = errors.Join(result.Err, err)
		result.LLMContent += "\nWORKSPACE_OBSERVATION_FAILED: " + err.Error()
	}
	return result
}

func (f *files) validationTargets(args map[string]any) ([]tools.FileObservation, error) {
	raw, ok := args["validation_paths"]
	if !ok {
		return nil, nil
	}
	if text(args, "purpose") != "validation" {
		return nil, errors.New("validation_paths requires purpose=validation")
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, errors.New("validation_paths must be an array")
	}
	if len(values) < 1 || len(values) > 8 {
		return nil, errors.New("validation_paths requires 1..8 files")
	}
	seen := map[string]bool{}
	out := []tools.FileObservation{}
	for _, v := range values {
		p, ok := v.(string)
		if !ok {
			return nil, errors.New("validation path must be text")
		}
		state, err := f.observeFile(p)
		if err != nil {
			return nil, err
		}
		if !state.Exists || seen[state.Path] {
			return nil, errors.New("validation target must be an existing unique file")
		}
		seen[state.Path] = true
		out = append(out, tools.FileObservation{Path: state.Path, SHA256: state.SHA256})
	}
	return out, nil
}

func (f *files) attachValidation(result *tools.Result, before []tools.FileObservation) {
	if !result.ProcessExited {
		return
	}
	for _, old := range before {
		current, err := f.observeFile(old.Path)
		if err == nil && current.Exists && current.SHA256 == old.SHA256 {
			result.FileObservations = append(result.FileObservations, old)
		}
	}
}

func (s *sandbox) scratchName() string {
	if !inside(s.files.workspacePath, s.scratch) {
		return ""
	}
	rel, err := filepath.Rel(s.files.workspacePath, s.scratch)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}
