package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	locationResponseTimeout = 5 * time.Minute
	locationGrantLifetime   = 5 * time.Minute
)

type locationGrant struct {
	runID     string
	ctx       context.Context
	expiresAt time.Time
	result    tools.LocationResult
}

type locationRequest struct {
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId"`
	RequestID string `json:"requestId"`
	Reason    string `json:"reason"`
}

type pendingLocationRequest struct {
	locationRequest
	ctx context.Context
	ch  chan tools.LocationResult
}

func (sess *serveSession) bindLocationTool() {
	if sess.engine == nil || sess.engine.Registry == nil {
		return
	}
	registry := tools.NewRegistry()
	for _, name := range sess.engine.Registry.Names() {
		if tool, ok := sess.engine.Registry.Get(name); ok {
			registry.Register(tool)
		}
	}
	sess.engine.Registry = registry
	if _, ok := sess.engine.Registry.Get("get_current_location"); ok {
		// Replace the instance rather than changing a pointer another session's
		// registry could also hold. Session and device consent are never shared.
		sess.engine.Registry.Register(&tools.GetCurrentLocation{Request: sess.requestLocation})
	}
	if existing, ok := sess.engine.Registry.Get("search_places"); ok {
		if configured, ok := existing.(*tools.SearchPlaces); ok {
			instance := configured.NewSessionInstance()
			instance.ResolveLocation = sess.resolveLocation
			sess.engine.Registry.Register(instance)
		}
	}
}

func (sess *serveSession) beginLocationRun(ctx context.Context, foreground bool) string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	runID := sess.activeRunID
	sess.locationRunID = ""
	sess.locationRunContext = nil
	sess.locationGrants = nil
	if foreground && sess.effectiveApprovalChannel() != approvalChannelNone {
		sess.locationRunID = runID
		sess.locationRunContext = ctx
	}
	return runID
}

func (sess *serveSession) finishLocationRun(runID string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.locationRunID == runID {
		sess.locationRunID = ""
		sess.locationRunContext = nil
	}
	for ref, grant := range sess.locationGrants {
		if grant.runID == runID {
			delete(sess.locationGrants, ref)
		}
	}
	for id, pending := range sess.locationRequests {
		if pending.RunID == runID {
			sess.resolveLocationLocked(id, pending, tools.LocationResult{Status: "cancelled"})
		}
	}
}

func (sess *serveSession) requestLocation(ctx context.Context, reason string) (tools.LocationResult, error) {
	return sess.requestLocationWithTimeout(ctx, reason, locationResponseTimeout)
}

func (sess *serveSession) requestLocationWithTimeout(ctx context.Context, reason string, timeout time.Duration) (tools.LocationResult, error) {
	if err := ctx.Err(); err != nil {
		return tools.LocationResult{Status: "cancelled"}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sess.mu.Lock()
	runID := sess.activeRunID
	if runID == "" || sess.locationRunID != runID || sess.effectiveApprovalChannel() == approvalChannelNone || sess.locationAskedRunID == runID {
		sess.mu.Unlock()
		return tools.LocationResult{Status: "unavailable"}, nil
	}
	sess.locationAskedRunID = runID
	pending := &pendingLocationRequest{
		locationRequest: locationRequest{SessionID: sess.id, RunID: runID, RequestID: session.NewUUID(), Reason: strings.TrimSpace(reason)},
		ctx:             requestCtx, ch: make(chan tools.LocationResult, 1),
	}
	if sess.locationRequests == nil {
		sess.locationRequests = make(map[string]*pendingLocationRequest)
	}
	sess.locationRequests[pending.RequestID] = pending
	sess.broadcastLocked("location_request", pending.locationRequest)
	sess.mu.Unlock()
	return sess.awaitLocationResponse(ctx, pending)
}

func (sess *serveSession) awaitLocationResponse(ctx context.Context, pending *pendingLocationRequest) (tools.LocationResult, error) {
	select {
	case result := <-pending.ch:
		// A cancelled model run must not receive a grant that raced its stop.
		if err := ctx.Err(); err != nil {
			return tools.LocationResult{Status: "cancelled"}, err
		}
		return result, nil
	case <-pending.ctx.Done():
		status := "unavailable"
		if ctx.Err() != nil {
			status = "cancelled"
		}
		result := tools.LocationResult{Status: status}
		sess.mu.Lock()
		owned := sess.locationRequests[pending.RequestID] == pending
		if owned {
			sess.resolveLocationLocked(pending.RequestID, pending, result)
		}
		sess.mu.Unlock()
		if !owned {
			// A POST may have consumed the entry immediately before the timer
			// fired. Its already-published receipt owns the outcome, even when
			// select chose the timer among two ready channels.
			result = <-pending.ch
			if err := ctx.Err(); err != nil {
				return tools.LocationResult{Status: "cancelled"}, err
			}
		}
		return result, ctx.Err()
	}
}

// resolveLocationLocked consumes a request exactly once. Receipts intentionally
// omit coordinates; the private tool result carries the approved observation.
func (sess *serveSession) resolveLocationLocked(id string, pending *pendingLocationRequest, result tools.LocationResult) {
	delete(sess.locationRequests, id)
	result.LocationRef = ""
	if result.Status == "granted" || result.Status == "manual" {
		if pending.RunID == sess.activeRunID && pending.RunID == sess.locationRunID && sess.locationRunContext != nil && sess.locationRunContext.Err() == nil {
			// POST already checked the response deadline while owning this map
			// entry. Its accepted decision must not lose a grant merely because
			// the response timer fires between that check and this assignment.
			result.LocationRef = id
			if sess.locationGrants == nil {
				sess.locationGrants = map[string]locationGrant{}
			}
			sess.locationGrants[id] = locationGrant{runID: pending.RunID, ctx: sess.locationRunContext, expiresAt: time.Now().Add(locationGrantLifetime), result: cloneLocationResult(result)}
		} else {
			result = tools.LocationResult{Status: "cancelled"}
		}
	}
	pending.ch <- cloneLocationResult(result)
	sess.broadcastLocked("location_resolved", map[string]any{
		"sessionId": sess.id, "runId": pending.RunID, "requestId": id, "status": result.Status,
	})
}

func (sess *serveSession) cancelLocationRequests(status string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.locationRunID = ""
	sess.locationRunContext = nil
	sess.locationGrants = nil
	for id, pending := range sess.locationRequests {
		sess.resolveLocationLocked(id, pending, tools.LocationResult{Status: status})
	}
}

// Guidance replaces the model's current plan, not the foreground run. Revoke
// old location evidence and pending cards, but retain the run's ability to ask
// its client for a fresh observation. It cannot enable a background/ended run.
func (sess *serveSession) supersedeLocationRequests() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.locationGrants = nil
	for id, pending := range sess.locationRequests {
		sess.resolveLocationLocked(id, pending, tools.LocationResult{Status: "cancelled"})
	}
	// Explicit user guidance creates a new request opportunity. Within that
	// opportunity the existing once-only guard still prevents repeated prompts.
	sess.locationAskedRunID = ""
}

func cloneLocationResult(result tools.LocationResult) tools.LocationResult {
	if result.Latitude != nil {
		value := *result.Latitude
		result.Latitude = &value
	}
	if result.Longitude != nil {
		value := *result.Longitude
		result.Longitude = &value
	}
	return result
}

// resolveLocation resolves only an observation accepted in this active run.
// Model-authored coordinates and references restored from old history cannot
// become device-location evidence. The lookup exposes no cross-session state.
func (sess *serveSession) resolveLocation(ctx context.Context, ref string) (tools.LocationResult, error) {
	if err := ctx.Err(); err != nil {
		return tools.LocationResult{}, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	grant, ok := sess.locationGrants[ref]
	if !ok || ref == "" {
		return tools.LocationResult{}, errors.New("location reference is unavailable; use an explicit area or a current location observation")
	}
	if grant.runID == "" || grant.runID != sess.activeRunID || grant.runID != sess.locationRunID ||
		grant.ctx == nil || grant.ctx.Err() != nil || !time.Now().Before(grant.expiresAt) {
		delete(sess.locationGrants, ref)
		return tools.LocationResult{}, errors.New("location reference has expired or was cancelled; use an explicit area or a current location observation")
	}
	return cloneLocationResult(grant.result), nil
}

func (s *Server) handleLocationRequests(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	requests := []locationRequest{}
	sess.mu.Lock()
	for _, pending := range sess.locationRequests {
		if pending.ctx.Err() == nil && pending.RunID == sess.activeRunID && pending.RunID == sess.locationRunID {
			requests = append(requests, pending.locationRequest)
		}
	}
	sess.mu.Unlock()
	sort.Slice(requests, func(i, j int) bool { return requests[i].RequestID < requests[j].RequestID })
	writeJSON(w, map[string]any{"requests": requests})
}

func (s *Server) handleLocationResponse(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var response tools.LocationResult
	if err := decoder.Decode(&response); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid location response")
		return
	}
	if response.LocationRef != "" {
		writeErr(w, http.StatusBadRequest, "location references are assigned by the server")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid location response")
		return
	}
	response.ObservedAt = time.Now().UTC().Format(time.RFC3339)
	response, err := tools.NormalizeLocationResult(response)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid location response")
		return
	}
	id := r.PathValue("requestId")
	sess.mu.Lock()
	pending, ok := sess.locationRequests[id]
	if !ok {
		sess.mu.Unlock()
		writeErr(w, http.StatusNotFound, "unknown or already-resolved location request")
		return
	}
	if pending.ctx.Err() != nil || pending.RunID != sess.activeRunID || pending.RunID != sess.locationRunID {
		sess.resolveLocationLocked(id, pending, tools.LocationResult{Status: "cancelled"})
		sess.mu.Unlock()
		writeErr(w, http.StatusConflict, "location request is no longer active")
		return
	}
	sess.resolveLocationLocked(id, pending, response)
	sess.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}
