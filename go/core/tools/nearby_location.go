package tools

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// GetCurrentLocation asks the requesting foreground client, never the host IP.
// The frontend requests location directly; the OS/browser owns device consent.
// No subtask access is exposed, even when the parent owns a callback.
type GetCurrentLocation struct {
	Request LocationRequester
}

func (t *GetCurrentLocation) Name() string { return "get_current_location" }
func (t *GetCurrentLocation) Kind() Kind   { return KindInteract }
func (t *GetCurrentLocation) Description() string {
	return "Request approximate current-device location through the browser/OS permission prompt. When the user asks for nearby information without naming an area, call this tool directly; do not first ask where they are or ask for location permission in chat. If the user already named an area or landmark, use search_places.near instead. Pass the returned locationRef as search_places.location_ref; do not copy coordinates into search arguments or replace the approved location. Only if this tool returns denied or unavailable, ask for an area/address to continue. Never infer a device location from the server or retry a declined request."
}
func (t *GetCurrentLocation) Schema() map[string]any {
	return obj(map[string]any{"reason": str("Brief user-facing reason the location is needed.")}, "reason")
}
func (t *GetCurrentLocation) Execute(ctx context.Context, args map[string]any) Result {
	reason := strings.TrimSpace(strArg(args, "reason"))
	if reason == "" || len(reason) > 512 {
		return errResultf("reason must contain 1-512 bytes")
	}
	location := LocationResult{Status: "unavailable"}
	if t.Request != nil {
		// A foreground client has time to answer, while disconnected/headless
		// executions still have a finite cancellation boundary.
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		result, err := t.Request(requestCtx, reason)
		if err != nil {
			if ctx.Err() != nil {
				location.Status = "cancelled"
			}
		} else {
			var normalizeErr error
			location, normalizeErr = NormalizeLocationResult(result)
			if normalizeErr != nil {
				return errResult(normalizeErr)
			}
			if (location.Status == "granted" || location.Status == "manual") && len(result.LocationRef) <= 512 {
				// Request is a trusted runtime callback, not the browser's raw
				// response. Only it can attach an owning-session reference.
				location.LocationRef = strings.TrimSpace(result.LocationRef)
			}
		}
	}
	location.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(location)
	if err != nil {
		return errResult(err)
	}
	return Result{LLMContent: string(encoded), Display: "Location: " + location.Status}
}
