package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ArtifactCheckpointSnapshot struct {
	ID                 string   `json:"id,omitempty"`
	Available          bool     `json:"available"`
	Paths              []string `json:"paths,omitempty"`
	EvidenceGeneration int      `json:"evidence_generation,omitempty"`
	AuditGeneration    int      `json:"audit_generation,omitempty"`
}

type ArtifactRestoreResult struct {
	CheckpointID string   `json:"checkpoint_id"`
	Restored     []string `json:"restored"`
}

type artifactCheckpointPath struct {
	modelPath string
	hostPath  string
	exists    bool
	mode      os.FileMode
	hash      string
	blobPath  string
}

type artifactCheckpoint struct {
	id                 string
	paths              map[string]artifactCheckpointPath
	evidenceGeneration int
	auditGeneration    int
}

// ArtifactCheckpointManager keeps the best selected-validator candidate
// outside the user's workspace. It restores only run-owned generations and
// refuses to overwrite bytes changed by another actor.
type ArtifactCheckpointManager struct {
	mu             sync.Mutex
	root           string
	resolveSession func(string) (string, error)
	tempDir        string
	best           *artifactCheckpoint
	owned          map[string]string
}

func NewArtifactCheckpointManager(workspaceRoot string) (*ArtifactCheckpointManager, error) {
	return NewArtifactCheckpointManagerWithResolver(workspaceRoot, nil)
}

func NewArtifactCheckpointManagerWithResolver(
	workspaceRoot string,
	resolveSession func(string) (string, error),
) (*ArtifactCheckpointManager, error) {
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("checkpoint workspace root: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	tempDir, err := os.MkdirTemp("", "meadow-artifact-checkpoint-")
	if err != nil {
		return nil, fmt.Errorf("create artifact checkpoint store: %w", err)
	}
	return &ArtifactCheckpointManager{
		root: root, resolveSession: resolveSession,
		tempDir: tempDir, owned: make(map[string]string),
	}, nil
}

func (m *ArtifactCheckpointManager) Close() error {
	if m == nil || m.tempDir == "" {
		return nil
	}
	return os.RemoveAll(m.tempDir)
}

func (m *ArtifactCheckpointManager) Capture(modelPaths []string, evidenceGeneration, auditGeneration int) (ArtifactCheckpointSnapshot, error) {
	if m == nil {
		return ArtifactCheckpointSnapshot{}, errors.New("artifact checkpoint manager is nil")
	}
	paths := uniqueSortedStrings(modelPaths)
	if len(paths) == 0 {
		return ArtifactCheckpointSnapshot{}, errors.New("artifact checkpoint requires at least one changed path")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	type captured struct {
		path  artifactCheckpointPath
		bytes []byte
	}
	capturedPaths := make([]captured, 0, len(paths))
	hasher := sha256.New()
	for _, modelPath := range paths {
		hostPath, err := m.resolve(modelPath)
		if err != nil {
			return ArtifactCheckpointSnapshot{}, err
		}
		state, data, err := readArtifactCheckpointState(hostPath, modelPath)
		if err != nil {
			return ArtifactCheckpointSnapshot{}, err
		}
		capturedPaths = append(capturedPaths, captured{path: state, bytes: data})
		_, _ = io.WriteString(hasher, modelPath+"\x00"+state.hash+"\x00")
	}
	_, _ = io.WriteString(hasher, fmt.Sprintf("evidence=%d\x00audit=%d", evidenceGeneration, auditGeneration))
	id := hex.EncodeToString(hasher.Sum(nil))
	dir := filepath.Join(m.tempDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ArtifactCheckpointSnapshot{}, fmt.Errorf("create checkpoint %s: %w", id, err)
	}
	checkpoint := &artifactCheckpoint{
		id: id, paths: make(map[string]artifactCheckpointPath, len(capturedPaths)),
		evidenceGeneration: evidenceGeneration, auditGeneration: auditGeneration,
	}
	for index, capturedPath := range capturedPaths {
		state := capturedPath.path
		if state.exists {
			state.blobPath = filepath.Join(dir, fmt.Sprintf("%04d.blob", index))
			if err := os.WriteFile(state.blobPath, capturedPath.bytes, 0o600); err != nil {
				return ArtifactCheckpointSnapshot{}, fmt.Errorf("write checkpoint blob: %w", err)
			}
		}
		checkpoint.paths[state.hostPath] = state
		m.owned[state.hostPath] = state.hash
	}
	m.best = checkpoint
	return m.snapshotLocked(), nil
}

// ObserveOwnedPaths records the exact bytes produced by the just-completed
// model mutation. Restore later compares against these hashes to distinguish a
// model-owned generation from a concurrent user edit.
func (m *ArtifactCheckpointManager) ObserveOwnedPaths(modelPaths []string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, modelPath := range uniqueSortedStrings(modelPaths) {
		hostPath, err := m.resolve(modelPath)
		if err != nil {
			return err
		}
		state, _, err := readArtifactCheckpointState(hostPath, modelPath)
		if err != nil {
			return err
		}
		m.owned[hostPath] = state.hash
	}
	return nil
}

func (m *ArtifactCheckpointManager) RestoreBest() (ArtifactRestoreResult, error) {
	if m == nil {
		return ArtifactRestoreResult{}, errors.New("artifact checkpoint manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.best == nil {
		return ArtifactRestoreResult{}, errors.New("no artifact checkpoint is available")
	}
	states := make([]artifactCheckpointPath, 0, len(m.best.paths))
	for _, state := range m.best.paths {
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].hostPath < states[j].hostPath })
	// Preflight every path before writing any bytes so one concurrent edit
	// cannot leave a partially restored workspace.
	for _, state := range states {
		current, _, err := readArtifactCheckpointState(state.hostPath, state.modelPath)
		if err != nil {
			return ArtifactRestoreResult{}, err
		}
		ownedHash, owned := m.owned[state.hostPath]
		if !owned || current.hash != ownedHash {
			return ArtifactRestoreResult{}, fmt.Errorf(
				"concurrent workspace change prevents rollback of %s", state.modelPath,
			)
		}
	}
	restored := make([]string, 0, len(states))
	for _, state := range states {
		if err := restoreArtifactCheckpointPath(state); err != nil {
			return ArtifactRestoreResult{}, err
		}
		m.owned[state.hostPath] = state.hash
		restored = append(restored, state.modelPath)
	}
	return ArtifactRestoreResult{CheckpointID: m.best.id, Restored: restored}, nil
}

func (m *ArtifactCheckpointManager) Snapshot() ArtifactCheckpointSnapshot {
	if m == nil {
		return ArtifactCheckpointSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *ArtifactCheckpointManager) snapshotLocked() ArtifactCheckpointSnapshot {
	if m.best == nil {
		return ArtifactCheckpointSnapshot{}
	}
	paths := make([]string, 0, len(m.best.paths))
	for _, state := range m.best.paths {
		paths = append(paths, state.modelPath)
	}
	sort.Strings(paths)
	return ArtifactCheckpointSnapshot{
		ID: m.best.id, Available: true, Paths: paths,
		EvidenceGeneration: m.best.evidenceGeneration, AuditGeneration: m.best.auditGeneration,
	}
}

func (m *ArtifactCheckpointManager) resolve(modelPath string) (string, error) {
	raw := strings.TrimSpace(strings.Trim(modelPath, `"'`))
	if m.resolveSession != nil &&
		(raw == "/session" || strings.HasPrefix(raw, "/session/")) {
		return m.resolveSession(raw)
	}
	if raw == "/workspace" {
		raw = "."
	} else if strings.HasPrefix(raw, "/workspace/") {
		raw = strings.TrimPrefix(raw, "/workspace/")
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(m.root, raw)
	}
	hostPath := filepath.Clean(raw)
	relative, err := filepath.Rel(m.root, hostPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("checkpoint path %q is outside workspace", modelPath)
	}
	return hostPath, nil
}

func readArtifactCheckpointState(hostPath, modelPath string) (artifactCheckpointPath, []byte, error) {
	state := artifactCheckpointPath{hostPath: hostPath, modelPath: modelPath}
	info, err := os.Lstat(hostPath)
	if os.IsNotExist(err) {
		state.hash = hashArtifactCheckpointState(false, 0, nil)
		return state, nil, nil
	}
	if err != nil {
		return state, nil, fmt.Errorf("read checkpoint state %s: %w", modelPath, err)
	}
	if !info.Mode().IsRegular() {
		return state, nil, fmt.Errorf("checkpoint path %s is not a regular file", modelPath)
	}
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return state, nil, fmt.Errorf("read checkpoint bytes %s: %w", modelPath, err)
	}
	state.exists = true
	state.mode = info.Mode().Perm()
	state.hash = hashArtifactCheckpointState(true, state.mode, data)
	return state, data, nil
}

func hashArtifactCheckpointState(exists bool, mode os.FileMode, data []byte) string {
	hasher := sha256.New()
	if exists {
		_, _ = io.WriteString(hasher, "file\x00"+mode.String()+"\x00")
		_, _ = hasher.Write(data)
	} else {
		_, _ = io.WriteString(hasher, "missing\x00")
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func restoreArtifactCheckpointPath(state artifactCheckpointPath) error {
	if !state.exists {
		if err := os.Remove(state.hostPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove rollback-created path %s: %w", state.modelPath, err)
		}
		return nil
	}
	data, err := os.ReadFile(state.blobPath)
	if err != nil {
		return fmt.Errorf("read rollback blob %s: %w", state.modelPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(state.hostPath), 0o755); err != nil {
		return fmt.Errorf("prepare rollback path %s: %w", state.modelPath, err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(state.hostPath), ".meadow-restore-*")
	if err != nil {
		return fmt.Errorf("create rollback temporary %s: %w", state.modelPath, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write rollback temporary %s: %w", state.modelPath, err)
	}
	if err := temporary.Chmod(state.mode); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set rollback mode %s: %w", state.modelPath, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close rollback temporary %s: %w", state.modelPath, err)
	}
	if err := os.Rename(temporaryPath, state.hostPath); err != nil {
		return fmt.Errorf("restore rollback path %s: %w", state.modelPath, err)
	}
	// Give restored source a distinct whole-second mtime as well as restored
	// bytes. Some validators (notably Python bytecode caches) key freshness on
	// second-resolution mtime plus size; same-sized rapid edits could otherwise
	// make the exact revalidation execute stale compiled content.
	validationMTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(state.hostPath, validationMTime, validationMTime); err != nil {
		return fmt.Errorf("refresh rollback timestamp %s: %w", state.modelPath, err)
	}
	return nil
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
