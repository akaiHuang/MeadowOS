package serve

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	promptRequestIDHeader           = "X-Meadow-Prompt-Request-ID"
	maxPromptRequestRecords         = 128
	promptRequestCompletedRetention = 30 * time.Minute
)

var errPromptRequestConflict = errors.New("prompt request ID was reused with different content")
var errPromptRequestLimit = errors.New("too many active prompt requests")
var errPromptReplayUnavailable = errors.New("the original prompt result is no longer available")

type promptReplayOnlyContextKey struct{}

// WithPromptReplayOnly constrains an already-started, server-authorized delivery
// to the retained Core result for requestID. It is an in-process capability:
// request headers and JSON fields cannot enable it. A missing record must never
// establish a new turn because the original delivery may already be settled.
func WithPromptReplayOnly(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, promptReplayOnlyContextKey{}, requestID)
}

// IsPromptReplayOnly lets in-process wrappers preserve the original delivery's
// bookkeeping. Core still validates the context's request ID against the actual
// request and requires a matching retained record before returning any result.
// Wire fields cannot create this context marker.
func IsPromptReplayOnly(ctx context.Context) bool {
	_, replayOnly := promptReplayOnlyRequestID(ctx)
	return replayOnly
}

func promptReplayOnlyRequestID(ctx context.Context) (string, bool) {
	requestID, ok := ctx.Value(promptReplayOnlyContextKey{}).(string)
	return requestID, ok
}

// promptRequestResult is the complete HTTP-visible result of an idempotent
// prompt request. Keeping validation errors as well as successful turns lets a
// concurrent replay terminate deterministically instead of waiting forever
// when the first request is rejected before the model starts.
type promptRequestResult struct {
	status       int
	stopReason   string
	checkpointID string
	errorMessage string
	errorCode    string
}

type promptRequestRecord struct {
	fingerprint [sha256.Size]byte
	done        chan struct{}
	createdAt   time.Time
	completedAt time.Time
	completed   bool
	result      promptRequestResult
}

func validPromptRequestID(value string) bool {
	if len(value) < 8 || len(value) > 160 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '-' || char == '_' || char == '.' || char == ':' {
			continue
		}
		return false
	}
	return true
}

// claimPromptRequest atomically establishes ownership of a request ID. The
// map is conversation-scoped, so identical IDs in independent conversations
// never collide. A matching replay receives the existing record and joins its
// completion; a mismatched replay is rejected before attachment persistence or
// model execution can happen.
func (s *serveSession) claimPromptRequest(requestID string, fingerprint [sha256.Size]byte) (*promptRequestRecord, bool, error) {
	return s.claimPromptRequestMode(requestID, fingerprint, false)
}

func (s *serveSession) replayPromptRequest(requestID string, fingerprint [sha256.Size]byte) (*promptRequestRecord, bool, error) {
	return s.claimPromptRequestMode(requestID, fingerprint, true)
}

func (s *serveSession) claimPromptRequestMode(requestID string, fingerprint [sha256.Size]byte, replayOnly bool) (*promptRequestRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.promptRequests[requestID]; existing != nil {
		if existing.fingerprint != fingerprint {
			return nil, false, errPromptRequestConflict
		}
		return existing, true, nil
	}
	if replayOnly {
		return nil, false, errPromptReplayUnavailable
	}
	if s.promptRequests == nil {
		s.promptRequests = make(map[string]*promptRequestRecord)
	}
	now := time.Now()
	s.prunePromptRequestsLocked(now)
	for len(s.promptRequests) >= maxPromptRequestRecords {
		if !s.evictOldestCompletedPromptRequestLocked() {
			return nil, false, errPromptRequestLimit
		}
	}
	record := &promptRequestRecord{
		fingerprint: fingerprint,
		done:        make(chan struct{}),
		createdAt:   now,
	}
	s.promptRequests[requestID] = record
	return record, false, nil
}

func (s *serveSession) prunePromptRequestsLocked(now time.Time) {
	for requestID, record := range s.promptRequests {
		if record == nil || (record.completed && !record.completedAt.IsZero() && now.Sub(record.completedAt) >= promptRequestCompletedRetention) {
			delete(s.promptRequests, requestID)
		}
	}
}

func (s *serveSession) evictOldestCompletedPromptRequestLocked() bool {
	oldestID := ""
	var oldestAt time.Time
	for requestID, record := range s.promptRequests {
		if record == nil {
			delete(s.promptRequests, requestID)
			return true
		}
		if !record.completed {
			continue
		}
		completedAt := record.completedAt
		if completedAt.IsZero() {
			completedAt = record.createdAt
		}
		if oldestID == "" || completedAt.Before(oldestAt) {
			oldestID = requestID
			oldestAt = completedAt
		}
	}
	if oldestID == "" {
		return false
	}
	delete(s.promptRequests, oldestID)
	return true
}

func (s *serveSession) completePromptRequest(record *promptRequestRecord, result promptRequestResult) {
	if record == nil {
		return
	}
	s.mu.Lock()
	if !record.completed {
		record.result = result
		record.completed = true
		record.completedAt = time.Now()
		close(record.done)
	}
	s.mu.Unlock()
}

func waitPromptRequest(w http.ResponseWriter, r *http.Request, record *promptRequestRecord) {
	select {
	case <-record.done:
		w.Header().Set("Idempotency-Replayed", "true")
		writePromptRequestResult(w, record.result)
	case <-r.Context().Done():
		// Like an original prompt request, a detached replay does not cancel the
		// already-running turn.
	}
}

func writePromptRequestResult(w http.ResponseWriter, result promptRequestResult) {
	if result.status >= http.StatusBadRequest {
		if result.errorCode != "" {
			writeCodedErr(w, result.status, result.errorCode, result.errorMessage)
			return
		}
		writeErr(w, result.status, result.errorMessage)
		return
	}
	writeJSON(w, map[string]any{
		"stopReason":   result.stopReason,
		"checkpointId": result.checkpointID,
	})
}
