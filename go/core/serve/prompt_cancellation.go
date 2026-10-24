package serve

import (
	"context"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// An admission exists before body decoding/attachment preparation, and survives
// a detached HTTP handler until the accepted prompt settles. Stop can therefore
// retire accepted work before its runtime cancellation function exists.
type promptAdmission struct {
	requestID   string
	generation  uint64
	cancelled   bool
	harnessMode string
}

func (sess *serveSession) beginPromptAdmission(requestID string) *promptAdmission {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.prunePromptCancellationsLocked(time.Now())
	ticket := &promptAdmission{requestID: requestID, generation: sess.cancelGeneration, harnessMode: sess.snapshotHarnessModeLocked()}
	_, ticket.cancelled = sess.cancelledPromptRequests[requestID]
	if sess.promptAdmissions == nil {
		sess.promptAdmissions = map[*promptAdmission]struct{}{}
	}
	sess.promptAdmissions[ticket] = struct{}{}
	return ticket
}

func (sess *serveSession) endPromptAdmission(ticket *promptAdmission) {
	sess.mu.Lock()
	delete(sess.promptAdmissions, ticket)
	sess.mu.Unlock()
}

func (sess *serveSession) promptAdmissionCancelled(ticket *promptAdmission) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return ticket.cancelled || ticket.generation != sess.cancelGeneration
}

// The cancellation check, queue resumption and run identity installation share
// one lock. Stop cannot land between a successful check and runtime admission.
func (sess *serveSession) admitForegroundPrompt(ticket *promptAdmission, runID string, cancel context.CancelFunc) (bool, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if ticket.cancelled || ticket.generation != sess.cancelGeneration {
		return false, nil
	}
	if sess.queuePaused {
		sess.queuePaused = false
		if err := sess.persistQueueLocked(); err != nil {
			sess.queuePaused = true
			return false, err
		}
	}
	mode := ticket.harnessMode
	if mode == "" {
		mode = sess.snapshotHarnessModeLocked()
	}
	sess.running = true
	sess.activeInferenceControls = snapshotSessionInferenceControlsLocked(sess)
	sess.admitHarnessModeLocked(mode)
	sess.cancelRun = cancel
	sess.activePromptRequestID = ticket.requestID
	sess.runReplayAfterEventID = sess.nextEventID
	sess.activeRunID = runID
	return true, nil
}

func (sess *serveSession) prunePromptCancellationsLocked(now time.Time) {
	for id, expires := range sess.cancelledPromptRequests {
		if !expires.After(now) {
			delete(sess.cancelledPromptRequests, id)
		}
	}
}

// A bounded request-ID tombstone handles transport reordering: Stop can reach
// the server before its already-issued prompt. Never evict a live tombstone to
// claim another cancellation succeeded; an overloaded caller receives an error.
func (sess *serveSession) rememberPromptCancellationLocked(id string) error {
	if id == "" {
		return nil
	}
	now := time.Now()
	sess.prunePromptCancellationsLocked(now)
	if sess.cancelledPromptRequests == nil {
		sess.cancelledPromptRequests = map[string]time.Time{}
	}
	if _, exists := sess.cancelledPromptRequests[id]; !exists && len(sess.cancelledPromptRequests) >= maxPromptRequestRecords {
		return errPromptRequestLimit
	}
	sess.cancelledPromptRequests[id] = now.Add(promptRequestCompletedRetention)
	return nil
}

// A Stop whose response raced completion may still pause that latest request's
// idle queue. It cannot claim a successor's queue, including a successor still
// decoding/preparing its payload rather than executing the model.
func (sess *serveSession) latestCompletedPromptOwnsIdleQueueLocked(id string) bool {
	if id == "" || sess.running || sess.activeRunID != "" {
		return false
	}
	record := sess.promptRequests[id]
	if record == nil || !record.completed {
		return false
	}
	for ticket := range sess.promptAdmissions {
		if ticket.requestID != id && !ticket.cancelled {
			return false
		}
	}
	for _, other := range sess.promptRequests {
		if other != nil && other.createdAt.After(record.createdAt) {
			return false
		}
	}
	return true
}

// Cancel cards while the same lock still owns the captured active run. A late
// Stop for an old request must not resolve a successor's cards or cancel it.
func (sess *serveSession) cancelRunCardsLocked() int {
	count := len(sess.permissions)
	for id, ch := range sess.permissions {
		delete(sess.permissions, id)
		select {
		case ch <- engine.DecisionDeny:
		default:
		}
		sess.broadcastLocked("permission_resolved", map[string]any{
			"sessionId": sess.id, "requestId": id, "cancelled": true,
			"outcome": map[string]any{"outcome": "cancelled", "optionId": "reject"},
		})
	}
	sess.locationRunID = ""
	sess.locationRunContext = nil
	sess.locationGrants = nil
	for id, pending := range sess.locationRequests {
		sess.resolveLocationLocked(id, pending, tools.LocationResult{Status: "cancelled"})
	}
	return count
}
