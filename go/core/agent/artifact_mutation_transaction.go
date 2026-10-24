package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ArtifactMutationRollback identifies one rejected model mutation that was
// restored to its exact pre-call state. It is intentionally separate from an
// ArtifactCheckpointSnapshot: a pre-call image is a safety fallback, not a
// verified solution candidate.
type ArtifactMutationRollback struct {
	CallID   string   `json:"call_id"`
	Restored []string `json:"restored"`
	Drafts   []string `json:"drafts,omitempty"`
}

type artifactMutationTransaction struct {
	dir   string
	paths map[string]artifactCheckpointPath
	owned map[string]string
}

// ArtifactMutationTransactionManager makes structurally-invalid file writes
// transactional. Begin captures the pre-call bytes, ObserveOwned records the
// exact model-produced generation, and Rollback refuses to overwrite bytes
// that changed after the tool returned.
//
// This manager does not decide task correctness and never promotes a snapshot
// to "best". Repository validators and ArtifactCheckpointManager remain the
// authority for verified solution checkpoints.
type ArtifactMutationTransactionManager struct {
	mu             sync.Mutex
	root           string
	resolveSession func(string) (string, error)
	tempDir        string
	pending        map[string]*artifactMutationTransaction
	baseline       map[string]artifactCheckpointPath
}

func NewArtifactMutationTransactionManager(workspaceRoot string) (*ArtifactMutationTransactionManager, error) {
	return NewArtifactMutationTransactionManagerWithResolver(workspaceRoot, nil)
}

func NewArtifactMutationTransactionManagerWithResolver(
	workspaceRoot string,
	resolveSession func(string) (string, error),
) (*ArtifactMutationTransactionManager, error) {
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("mutation transaction workspace root: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	tempDir, err := os.MkdirTemp("", "meadow-artifact-mutation-")
	if err != nil {
		return nil, fmt.Errorf("create mutation transaction store: %w", err)
	}
	return &ArtifactMutationTransactionManager{
		root: root, resolveSession: resolveSession, tempDir: tempDir,
		pending:  make(map[string]*artifactMutationTransaction),
		baseline: make(map[string]artifactCheckpointPath),
	}, nil
}

func (m *ArtifactMutationTransactionManager) Close() error {
	if m == nil || m.tempDir == "" {
		return nil
	}
	return os.RemoveAll(m.tempDir)
}

// Begin captures the exact state immediately before a single model mutation.
func (m *ArtifactMutationTransactionManager) Begin(callID string, modelPaths []string) error {
	if m == nil {
		return errors.New("artifact mutation transaction manager is nil")
	}
	if callID == "" {
		return errors.New("artifact mutation transaction requires a call id")
	}
	paths := uniqueSortedStrings(modelPaths)
	if len(paths) == 0 {
		return errors.New("artifact mutation transaction requires at least one path")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.discardLocked(callID)
	dir, err := os.MkdirTemp(m.tempDir, "call-")
	if err != nil {
		return fmt.Errorf("create mutation snapshot for %s: %w", callID, err)
	}
	transaction := &artifactMutationTransaction{
		dir: dir, paths: make(map[string]artifactCheckpointPath, len(paths)),
		owned: make(map[string]string, len(paths)),
	}
	for index, modelPath := range paths {
		hostPath, resolveErr := m.resolve(modelPath)
		if resolveErr != nil {
			_ = os.RemoveAll(dir)
			return resolveErr
		}
		state, data, readErr := readArtifactCheckpointState(hostPath, modelPath)
		if readErr != nil {
			_ = os.RemoveAll(dir)
			return readErr
		}
		if _, recorded := m.baseline[hostPath]; !recorded {
			baseline := state
			baseline.blobPath = ""
			m.baseline[hostPath] = baseline
		}
		if state.exists {
			state.blobPath = filepath.Join(dir, fmt.Sprintf("%04d.blob", index))
			if writeErr := os.WriteFile(state.blobPath, data, 0o600); writeErr != nil {
				_ = os.RemoveAll(dir)
				return fmt.Errorf("write mutation snapshot for %s: %w", modelPath, writeErr)
			}
		}
		transaction.paths[hostPath] = state
	}
	m.pending[callID] = transaction
	return nil
}

// NetChangedPaths filters the append-only mutation history down to paths whose
// current bytes differ from their first pre-mutation state in this run. Paths
// not owned by this manager are preserved because another tool may have
// produced them. Read errors also fail open to the historical path so evidence
// is never hidden merely because reconciliation could not inspect a file.
func (m *ArtifactMutationTransactionManager) NetChangedPaths(modelPaths []string) []string {
	paths := uniqueSortedStrings(modelPaths)
	if m == nil || len(paths) == 0 {
		return paths
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	net := make([]string, 0, len(paths))
	for _, modelPath := range paths {
		hostPath, err := m.resolve(modelPath)
		if err != nil {
			net = append(net, modelPath)
			continue
		}
		baseline, tracked := m.baseline[hostPath]
		if !tracked {
			net = append(net, modelPath)
			continue
		}
		current, _, err := readArtifactCheckpointState(hostPath, modelPath)
		if err != nil || current.hash != baseline.hash {
			net = append(net, modelPath)
		}
	}
	return net
}

// ObserveOwned binds the transaction to the exact bytes left by the tool.
// Rollback is allowed only while those bytes remain unchanged.
func (m *ArtifactMutationTransactionManager) ObserveOwned(callID string, modelPaths []string) error {
	if m == nil {
		return errors.New("artifact mutation transaction manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	transaction, ok := m.pending[callID]
	if !ok {
		return errors.New("no artifact mutation transaction is available")
	}
	matched := 0
	for _, modelPath := range uniqueSortedStrings(modelPaths) {
		hostPath, err := m.resolve(modelPath)
		if err != nil {
			return err
		}
		if _, captured := transaction.paths[hostPath]; !captured {
			continue
		}
		state, _, err := readArtifactCheckpointState(hostPath, modelPath)
		if err != nil {
			return err
		}
		transaction.owned[hostPath] = state.hash
		matched++
	}
	if matched == 0 {
		return errors.New("mutation result did not include the captured artifact path")
	}
	return nil
}

func (m *ArtifactMutationTransactionManager) resolve(modelPath string) (string, error) {
	raw := strings.TrimSpace(strings.Trim(modelPath, `"'`))
	if m.resolveSession != nil &&
		(raw == "/session" || strings.HasPrefix(raw, "/session/")) {
		return m.resolveSession(raw)
	}
	return resolveArtifactTransactionPath(m.root, modelPath)
}

// Rollback restores one invalid mutation. Every captured path is preflighted
// before any write, so an external edit cannot cause a partial rollback.
func (m *ArtifactMutationTransactionManager) Rollback(callID string) (ArtifactMutationRollback, bool, error) {
	if m == nil {
		return ArtifactMutationRollback{}, false, errors.New("artifact mutation transaction manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	transaction, ok := m.pending[callID]
	if !ok {
		return ArtifactMutationRollback{}, false, nil
	}
	defer m.discardLocked(callID)

	states := make([]artifactCheckpointPath, 0, len(transaction.paths))
	for _, state := range transaction.paths {
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].hostPath < states[j].hostPath })
	for _, state := range states {
		ownedHash, owned := transaction.owned[state.hostPath]
		if !owned {
			return ArtifactMutationRollback{}, true, fmt.Errorf(
				"mutation result for %s was not bound before rollback", state.modelPath,
			)
		}
		current, _, err := readArtifactCheckpointState(state.hostPath, state.modelPath)
		if err != nil {
			return ArtifactMutationRollback{}, true, err
		}
		if current.hash != ownedHash {
			return ArtifactMutationRollback{}, true, fmt.Errorf(
				"concurrent workspace change prevents mutation rollback of %s", state.modelPath,
			)
		}
	}
	drafts, err := m.preserveInvalidDrafts(callID, states)
	if err != nil {
		return ArtifactMutationRollback{}, true, err
	}
	restored := make([]string, 0, len(states))
	for _, state := range states {
		if err := restoreArtifactCheckpointPath(state); err != nil {
			return ArtifactMutationRollback{}, true, err
		}
		restored = append(restored, state.modelPath)
	}
	return ArtifactMutationRollback{CallID: callID, Restored: restored, Drafts: drafts}, true, nil
}

// preserveInvalidDrafts keeps the exact rejected generation in the workspace
// before rollback. A newly-created invalid artifact otherwise disappears when
// its pre-call state (non-existent) is restored, leaving the repair turn with
// nothing to read. Drafts are deliberately outside the deliverable path and
// are never completion evidence; they are only recoverable model input.
func (m *ArtifactMutationTransactionManager) preserveInvalidDrafts(
	callID string,
	states []artifactCheckpointPath,
) ([]string, error) {
	if len(states) == 0 {
		return nil, nil
	}
	segment := safeMutationDraftSegment(callID)
	drafts := make([]string, 0, len(states))
	for _, state := range states {
		current, data, err := readArtifactCheckpointState(state.hostPath, state.modelPath)
		if err != nil {
			return nil, fmt.Errorf("read rejected artifact %s for draft preservation: %w", state.modelPath, err)
		}
		if !current.exists {
			continue
		}
		relative, err := filepath.Rel(m.root, state.hostPath)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("preserve rejected artifact %s outside workspace", state.modelPath)
		}
		draftRelative := filepath.Join(".meadow", "drafts", segment, relative)
		draftHost := filepath.Join(m.root, draftRelative)
		if err := os.MkdirAll(filepath.Dir(draftHost), 0o700); err != nil {
			return nil, fmt.Errorf("create rejected artifact draft directory: %w", err)
		}
		if err := os.WriteFile(draftHost, data, 0o600); err != nil {
			return nil, fmt.Errorf("preserve rejected artifact draft: %w", err)
		}
		drafts = append(drafts, "/workspace/"+filepath.ToSlash(draftRelative))
	}
	return drafts, nil
}

func safeMutationDraftSegment(callID string) string {
	segment := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, strings.TrimSpace(callID))
	segment = strings.Trim(segment, "-")
	if segment == "" {
		return "rejected-call"
	}
	return segment
}

func (m *ArtifactMutationTransactionManager) Discard(callID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.discardLocked(callID)
}

func (m *ArtifactMutationTransactionManager) discardLocked(callID string) {
	transaction, ok := m.pending[callID]
	if !ok {
		return
	}
	delete(m.pending, callID)
	_ = os.RemoveAll(transaction.dir)
}

func resolveArtifactTransactionPath(root, modelPath string) (string, error) {
	raw := strings.TrimSpace(strings.Trim(modelPath, `"'`))
	if raw == "/workspace" {
		raw = "."
	} else if strings.HasPrefix(raw, "/workspace/") {
		raw = strings.TrimPrefix(raw, "/workspace/")
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(root, raw)
	}
	hostPath := filepath.Clean(raw)
	relative, err := filepath.Rel(root, hostPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("mutation transaction path %q is outside workspace", modelPath)
	}
	return hostPath, nil
}
