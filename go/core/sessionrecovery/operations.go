package sessionrecovery

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

func (s *Store) BeginTurn(request BeginTurnRequest) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireIdentifier("turn id", request.TurnID); err != nil || request.Ordinal == 0 {
		return RecordResult{}, errors.New("turn id and positive ordinal are required")
	}
	prompt, err := sanitizeReference(request.Prompt, s.options)
	if err != nil {
		return RecordResult{}, err
	}
	prompt = metadataOnlyReference(prompt)
	if s.state.CurrentTurn != nil {
		if s.state.CurrentTurn.TurnID == request.TurnID && s.state.CurrentTurn.Ordinal == request.Ordinal && sameReference(s.state.CurrentTurn.Prompt, prompt) {
			return duplicateResult(s.state), nil
		}
		return RecordResult{}, ErrTurnActive
	}
	id, err := newEventID("turn")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventTurn, At: now, Turn: &TurnCheckpoint{
		TurnID: request.TurnID, Ordinal: request.Ordinal, Status: TurnRunning,
		Prompt: prompt, Summary: sanitizeText(request.Summary, s.options), StartedAt: now,
	}})
}

func (s *Store) FinishTurn(request FinishTurnRequest) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.CurrentTurn == nil || s.state.CurrentTurn.TurnID != request.TurnID {
		return RecordResult{}, ErrTurnNotActive
	}
	if request.Status != TurnCompleted && request.Status != TurnFailed && request.Status != TurnCancelled {
		return RecordResult{}, errors.New("turn terminal status is invalid")
	}
	id, err := newEventID("turn")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	turn := *s.state.CurrentTurn
	turn.Status = request.Status
	turn.Summary = sanitizeText(request.Summary, s.options)
	turn.FinishedAt = &now
	return s.appendEventLocked(Event{ID: id, Kind: EventTurn, At: now, Turn: &turn})
}

func (s *Store) RecordToolCall(request ToolCallRequest) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireIdentifier("call id", request.CallID); err != nil {
		return RecordResult{}, err
	}
	if err := requireIdentifier("turn id", request.TurnID); err != nil {
		return RecordResult{}, err
	}
	if !validLabel(request.Name) || !validHash(request.ActionDigest) {
		return RecordResult{}, errors.New("tool name or action digest is invalid")
	}
	if request.InputVersion != "" && !validHash(request.InputVersion) {
		return RecordResult{}, errors.New("tool input version is invalid")
	}
	if request.Effect != ToolReadOnly && request.Effect != ToolMutation && request.Effect != ToolUnknown {
		return RecordResult{}, errors.New("tool effect is invalid")
	}
	if s.state.CurrentTurn == nil || s.state.CurrentTurn.TurnID != request.TurnID {
		return RecordResult{}, ErrTurnNotActive
	}
	arguments, err := sanitizeReference(request.Arguments, s.options)
	if err != nil {
		return RecordResult{}, err
	}
	arguments = metadataOnlyReference(arguments)
	if previous, exists := s.state.ToolCalls[request.CallID]; exists {
		if previous.TurnID == request.TurnID && previous.Name == request.Name && previous.ActionDigest == request.ActionDigest && previous.InputVersion == request.InputVersion && previous.Effect == request.Effect && sameReference(previous.Arguments, arguments) {
			if previous.Effect != ToolReadOnly {
				return RecordResult{}, ErrSameActionRetry
			}
			return duplicateResult(s.state), nil
		}
		return RecordResult{}, ErrToolCallConflict
	}
	retryAuthorizationID := ""
	if request.ObservedCallID != "" && !observedRepeatMatchesGuard(&s.state, request.ObservedCallID, request.ActionDigest, request.TurnID, request.Name, request.Effect, request.InputVersion) {
		return RecordResult{}, ErrSameActionRetry
	}
	if request.Effect != ToolReadOnly {
		if guard, guarded := s.state.ActionGuards[request.ActionDigest]; guarded {
			if request.ObservedCallID == "" {
				authorization, exists := s.state.RetryAuthorizations[request.ActionDigest]
				if !exists || !retryAuthorizationMatchesGuard(&s.state, authorization, guard, request.TurnID) {
					return RecordResult{}, ErrSameActionRetry
				}
				retryAuthorizationID = authorization.AuthorizationID
			}
		}
	}
	id, err := newEventID("tool-call")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventToolCall, At: now, ToolCall: &ToolCallCheckpoint{
		CallID: request.CallID, TurnID: request.TurnID, Name: request.Name,
		ActionDigest: request.ActionDigest, InputVersion: request.InputVersion, Effect: request.Effect, Arguments: arguments,
		RetryAuthorizationID: retryAuthorizationID, PreparedAt: now,
		ObservedCallID: request.ObservedCallID,
	}})
}

func (s *Store) RecordToolExecution(request ToolExecutionRequest) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.ToolCalls[request.CallID]; !exists {
		return RecordResult{}, errors.New("tool execution requires a durable tool call")
	}
	if previous, exists := s.state.ToolExecutions[request.CallID]; exists {
		if previous.Outcome == request.Outcome && previous.ProcessExited == request.ProcessExited && previous.Summary == sanitizeText(request.Summary, s.options) {
			return duplicateResult(s.state), nil
		}
		return RecordResult{}, ErrToolCallConflict
	}
	switch request.Outcome {
	case ExecutionSucceeded, ExecutionNoChange, ExecutionFailedBeforeEffect, ExecutionEffectUnknown:
	default:
		return RecordResult{}, errors.New("tool execution outcome is invalid")
	}
	id, err := newEventID("tool-execution")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventToolExecution, At: now, ToolExecution: &ToolExecutionCheckpoint{
		CallID: request.CallID, Outcome: request.Outcome,
		ProcessExited: request.ProcessExited,
		Summary:       sanitizeText(request.Summary, s.options), At: now,
	}})
}

func (s *Store) RecordToolResult(request ToolResultRequest) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validIdentifier(request.CallID) || !validLabel(request.Name) {
		return RecordResult{}, errors.New("tool result identity is invalid")
	}
	output, err := sanitizeReference(request.Output, s.options)
	if err != nil {
		return RecordResult{}, err
	}
	output, err = s.persistEvidenceLocked(output)
	if err != nil {
		return RecordResult{}, err
	}
	summary := sanitizeText(request.Summary, s.options)
	if previous, exists := s.state.ToolResults[request.CallID]; exists {
		if previous.Name == request.Name && previous.Summary == summary && sameReference(previous.Output, output) {
			return duplicateResult(s.state), nil
		}
		return RecordResult{}, ErrToolResultConflict
	}
	call, exists := s.state.ToolCalls[request.CallID]
	if !exists || call.Name != request.Name {
		return RecordResult{}, ErrToolResultConflict
	}
	id, err := newEventID("tool-result")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventToolResult, At: now, ToolResult: &ToolResultCheckpoint{
		CallID: request.CallID, Name: request.Name, Output: output, Summary: summary, At: now,
	}})
}

func (s *Store) RequestPermission(request PermissionRequest) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validIdentifier(request.RequestID) || !validLabel(request.Scope) {
		return RecordResult{}, errors.New("permission request identity or scope is invalid")
	}
	if request.TurnID != "" && !validIdentifier(request.TurnID) {
		return RecordResult{}, errors.New("permission turn id is invalid")
	}
	if request.CallID != "" && !validIdentifier(request.CallID) {
		return RecordResult{}, errors.New("permission call id is invalid")
	}
	reason := sanitizeText(request.Reason, s.options)
	if previous, exists := s.state.PendingPermissions[request.RequestID]; exists {
		if previous.TurnID == request.TurnID && previous.CallID == request.CallID && previous.Scope == request.Scope && previous.Reason == reason {
			return duplicateResult(s.state), nil
		}
		return RecordResult{}, ErrPermissionConflict
	}
	if len(s.state.PendingPermissions) >= s.options.Limits.MaxPendingPermissions {
		return RecordResult{}, ErrStateLimit
	}
	id, err := newEventID("permission")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventPermission, At: now, Permission: &PermissionCheckpoint{
		RequestID: request.RequestID, TurnID: request.TurnID, CallID: request.CallID,
		Scope: request.Scope, Decision: PermissionPending, Reason: reason, At: now,
	}})
}

func (s *Store) ResolvePermission(resolution PermissionResolution) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, exists := s.state.PendingPermissions[resolution.RequestID]
	if !exists {
		return RecordResult{}, ErrPermissionConflict
	}
	if resolution.Decision != PermissionApproved && resolution.Decision != PermissionDenied {
		return RecordResult{}, errors.New("permission resolution must approve or deny")
	}
	retryReason := sanitizeText(resolution.RetryReason, s.options)
	if resolution.RetryAuthorizationID != "" {
		if resolution.Decision != PermissionApproved || !validIdentifier(resolution.RetryAuthorizationID) ||
			!validIdentifier(resolution.RetryApprovedBy) || strings.TrimSpace(retryReason) == "" {
			return RecordResult{}, errors.New("retry authorization from permission is invalid")
		}
		call, callExists := s.state.ToolCalls[previous.CallID]
		guard, guarded := s.state.ActionGuards[call.ActionDigest]
		if !callExists || !guarded || guard.CallID != call.CallID || call.TurnID != previous.TurnID {
			return RecordResult{}, errors.New("retry authorization has no matching pending guarded action")
		}
		if authorization, exists := s.state.RetryAuthorizations[call.ActionDigest]; exists && authorization.ConsumedBy == "" {
			return RecordResult{}, ErrSameActionRetry
		}
	} else if resolution.RetryApprovedBy != "" || resolution.RetryReason != "" {
		return RecordResult{}, errors.New("retry authorization id is required")
	}
	id, err := newEventID("permission")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	previous.Decision = resolution.Decision
	previous.Reason = sanitizeText(resolution.Reason, s.options)
	previous.RetryAuthorizationID = resolution.RetryAuthorizationID
	previous.RetryApprovedBy = resolution.RetryApprovedBy
	previous.RetryReason = retryReason
	previous.At = now
	return s.appendEventLocked(Event{ID: id, Kind: EventPermission, At: now, Permission: &previous})
}

func (s *Store) QueueGuidance(request GuidanceRequest) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validIdentifier(request.GuidanceID) || (request.TurnID != "" && !validIdentifier(request.TurnID)) {
		return RecordResult{}, errors.New("guidance identity is invalid")
	}
	content, err := sanitizeReference(request.Content, s.options)
	if err != nil {
		return RecordResult{}, err
	}
	content = metadataOnlyReference(content)
	if previous, exists := s.state.QueuedGuidance[request.GuidanceID]; exists {
		if previous.TurnID == request.TurnID && sameReference(previous.Content, content) {
			return duplicateResult(s.state), nil
		}
		return RecordResult{}, ErrGuidanceConflict
	}
	if len(s.state.QueuedGuidance) >= s.options.Limits.MaxQueuedGuidance {
		return RecordResult{}, ErrStateLimit
	}
	id, err := newEventID("guidance")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventGuidance, At: now, Guidance: &GuidanceCheckpoint{
		GuidanceID: request.GuidanceID, TurnID: request.TurnID, Content: content,
		Status: GuidanceQueued, QueuedAt: now,
	}})
}

func (s *Store) ResolveGuidance(resolution GuidanceResolution) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, exists := s.state.QueuedGuidance[resolution.GuidanceID]
	if !exists {
		return RecordResult{}, ErrGuidanceConflict
	}
	if resolution.Status != GuidanceApplied && resolution.Status != GuidanceDropped {
		return RecordResult{}, errors.New("guidance resolution must apply or drop")
	}
	id, err := newEventID("guidance")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	previous.Status = resolution.Status
	previous.Reason = sanitizeText(resolution.Reason, s.options)
	previous.ResolvedAt = &now
	return s.appendEventLocked(Event{ID: id, Kind: EventGuidance, At: now, Guidance: &previous})
}

func (s *Store) RecordAttachment(metadata AttachmentMetadata) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clean, err := sanitizeAttachment(metadata, s.options)
	if err != nil {
		return RecordResult{}, err
	}
	if clean.AddedAt.IsZero() {
		clean.AddedAt = s.options.Clock().UTC()
	} else {
		clean.AddedAt = clean.AddedAt.UTC()
	}
	if previous, exists := s.state.Attachments[clean.AttachmentID]; exists {
		if reflect.DeepEqual(previous, clean) {
			return duplicateResult(s.state), nil
		}
		immutableSame := previous.Class == clean.Class && previous.OriginalName == clean.OriginalName &&
			previous.MediaType == clean.MediaType && previous.StorageRef == clean.StorageRef &&
			previous.SHA256 == clean.SHA256 && previous.Bytes == clean.Bytes && previous.Authorized == clean.Authorized
		if !immutableSame || (previous.Available == false && clean.Available == true) {
			return RecordResult{}, ErrAttachmentConflict
		}
	} else if len(s.state.Attachments) >= s.options.Limits.MaxAttachments {
		return RecordResult{}, ErrStateLimit
	}
	id, err := newEventID("attachment")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventAttachment, At: now, Attachment: &clean})
}

func (s *Store) AuthorizeRetry(request RetryAuthorizationRequest) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validIdentifier(request.AuthorizationID) || !validHash(request.ActionDigest) || !validIdentifier(request.ApprovedBy) {
		return RecordResult{}, errors.New("retry authorization identity is invalid")
	}
	guard, guarded := s.state.ActionGuards[request.ActionDigest]
	if !guarded {
		return RecordResult{}, errors.New("retry authorization has no matching action guard")
	}
	call, callExists := s.state.ToolCalls[guard.CallID]
	if !callExists {
		return RecordResult{}, errors.New("retry authorization guard has no matching tool call")
	}
	reason := sanitizeText(request.Reason, s.options)
	if strings.TrimSpace(reason) == "" {
		return RecordResult{}, errors.New("retry authorization reason is required")
	}
	if previous, exists := s.state.RetryAuthorizations[request.ActionDigest]; exists && previous.ConsumedBy == "" {
		if previous.AuthorizationID == request.AuthorizationID && previous.ApprovedBy == request.ApprovedBy && previous.Reason == reason {
			return duplicateResult(s.state), nil
		}
		return RecordResult{}, ErrSameActionRetry
	}
	id, err := newEventID("retry")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventRetryAuthorization, At: now, RetryAuthorization: &RetryAuthorization{
		AuthorizationID: request.AuthorizationID, ActionDigest: request.ActionDigest,
		ApprovedBy: request.ApprovedBy, Reason: reason,
		SourceCallID: guard.CallID, TurnID: call.TurnID, At: now,
	}})
}

func (s *Store) RecordRecap(request RecapRequest) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validIdentifier(request.RunID) || !validLabel(request.Status) || !validLabel(request.StopReason) {
		return RecordResult{}, errors.New("recap identity or status is invalid")
	}
	summary, err := sanitizeReference(request.Summary, s.options)
	if err != nil {
		return RecordResult{}, err
	}
	summary, err = s.persistEvidenceLocked(summary)
	if err != nil {
		return RecordResult{}, err
	}
	for _, previous := range s.state.RecentRecaps {
		if previous.RunID != request.RunID {
			continue
		}
		if previous.Status == request.Status && previous.StopReason == request.StopReason && sameReference(previous.Summary, summary) {
			return duplicateResult(s.state), nil
		}
		return RecordResult{}, errors.New("recap conflicts with durable state")
	}
	id, err := newEventID("recap")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventRecap, At: now, Recap: &RecapCheckpoint{
		RunID: request.RunID, Status: request.Status, StopReason: request.StopReason,
		Summary: summary, At: now,
	}})
}

func (s *Store) CheckRetry(actionDigest string, effect ToolEffect, retryTurnID string) RetryDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if effect == ToolReadOnly {
		return RetryDecision{Allowed: true}
	}
	guard, exists := s.state.ActionGuards[actionDigest]
	if !exists {
		return RetryDecision{Allowed: true}
	}
	decision := RetryDecision{RequiresAuthorization: true, Guard: &guard}
	if authorization, exists := s.state.RetryAuthorizations[actionDigest]; exists &&
		retryAuthorizationMatchesGuard(&s.state, authorization, guard, retryTurnID) {
		decision.Allowed = true
		decision.RequiresAuthorization = false
		decision.AuthorizationID = authorization.AuthorizationID
	}
	return decision
}

func (s *Store) FinishSession(outcome TerminalOutcome, summary string) (RecordResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch outcome {
	case TerminalCompleted, TerminalFailed, TerminalCancelled:
	default:
		return RecordResult{}, errors.New("terminal outcome is invalid")
	}
	if s.state.Terminal != nil {
		if s.state.Terminal.Outcome == outcome && s.state.Terminal.Summary == sanitizeText(summary, s.options) {
			return duplicateResult(s.state), nil
		}
		return RecordResult{}, fmt.Errorf("session is already terminal: %s", s.state.Terminal.Outcome)
	}
	id, err := newEventID("terminal")
	if err != nil {
		return RecordResult{}, err
	}
	now := s.options.Clock().UTC()
	return s.appendEventLocked(Event{ID: id, Kind: EventTerminal, At: now, Terminal: &TerminalCheckpoint{
		Outcome: outcome, Summary: sanitizeText(summary, s.options), At: now,
	}})
}

func duplicateResult(state State) RecordResult {
	return RecordResult{Duplicate: true, Sequence: state.LastSequence, Hash: state.LastHash}
}
