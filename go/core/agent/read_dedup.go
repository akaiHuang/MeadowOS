package agent

import (
	"fmt"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const unchangedReadMessage = "Resource unchanged since last read. The content from the earlier read result in this conversation is still current; use it instead of reading the same resource again."

type readDedupDecision uint8

const (
	readExecute readDedupDecision = iota
	readUnchanged
	readBlocked
)

type readDedupEntry struct {
	identity tools.ReadIdentity
	hits     int
}

// readDedupState is scoped to one RunInput. A resumed chat gets a fresh state:
// the transcript may contain old tool output, but the current model context is
// the only safe authority for replacing a new read with a lightweight stub.
type readDedupState struct {
	mu      sync.Mutex
	entries map[string]readDedupEntry
}

func newReadDedupState() *readDedupState {
	return &readDedupState{entries: map[string]readDedupEntry{}}
}

func (s *readDedupState) before(identity tools.ReadIdentity) readDedupDecision {
	if s == nil || identity.Key == "" || identity.Version == "" {
		return readExecute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[identity.Key]
	if !ok || entry.identity.Version != identity.Version {
		return readExecute
	}
	entry.hits++
	entry.identity = identity
	s.entries[identity.Key] = entry
	if entry.hits >= 2 {
		return readBlocked
	}
	return readUnchanged
}

func (s *readDedupState) record(identity tools.ReadIdentity) {
	if s == nil || identity.Key == "" || identity.Version == "" {
		return
	}
	s.mu.Lock()
	s.entries[identity.Key] = readDedupEntry{identity: identity}
	s.mu.Unlock()
}

func (s *readDedupState) resetHits() {
	if s == nil {
		return
	}
	s.mu.Lock()
	for key, entry := range s.entries {
		entry.hits = 0
		s.entries[key] = entry
	}
	s.mu.Unlock()
}

func (s *readDedupState) clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.entries = map[string]readDedupEntry{}
	s.mu.Unlock()
}

func unchangedReadResult(identity tools.ReadIdentity) tools.Result {
	return tools.Result{
		LLMContent: unchangedReadMessage,
		Display:    "Skipped unchanged read of " + identity.Path,
	}
}

func blockedReadResult(identity tools.ReadIdentity) tools.Result {
	return tools.Result{
		LLMContent: fmt.Sprintf(
			"BLOCKED: this exact resource has already been read and has not changed. Stop calling the same read tool for %s; use the earlier content, make progress with another tool, or answer the user.",
			identity.Path,
		),
		Display: "Blocked repeated unchanged read of " + identity.Path,
	}
}
