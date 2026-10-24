package sessionrecovery

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

func newState() State {
	return State{
		ToolCalls:           make(map[string]ToolCallCheckpoint),
		ToolExecutions:      make(map[string]ToolExecutionCheckpoint),
		ToolResults:         make(map[string]ToolResultCheckpoint),
		PendingPermissions:  make(map[string]PermissionCheckpoint),
		QueuedGuidance:      make(map[string]GuidanceCheckpoint),
		Attachments:         make(map[string]AttachmentMetadata),
		RetryAuthorizations: make(map[string]RetryAuthorization),
		ActionGuards:        make(map[string]ActionGuard),
		LastHash:            ZeroHash,
	}
}

func cloneState(source State) State {
	clone := source
	if source.CurrentTurn != nil {
		value := *source.CurrentTurn
		if source.CurrentTurn.Prompt != nil {
			prompt := *source.CurrentTurn.Prompt
			value.Prompt = &prompt
		}
		clone.CurrentTurn = &value
	}
	clone.RecentTurns = append([]TurnCheckpoint(nil), source.RecentTurns...)
	for index := range clone.RecentTurns {
		clone.RecentTurns[index].Prompt = cloneReference(clone.RecentTurns[index].Prompt)
	}
	clone.ToolCalls = cloneMap(source.ToolCalls)
	for key, value := range clone.ToolCalls {
		value.Arguments = cloneReference(value.Arguments)
		clone.ToolCalls[key] = value
	}
	clone.ToolExecutions = cloneMap(source.ToolExecutions)
	clone.ToolResults = cloneMap(source.ToolResults)
	for key, value := range clone.ToolResults {
		value.Output = cloneReference(value.Output)
		clone.ToolResults[key] = value
	}
	clone.PendingPermissions = cloneMap(source.PendingPermissions)
	clone.QueuedGuidance = cloneMap(source.QueuedGuidance)
	for key, value := range clone.QueuedGuidance {
		value.Content = cloneReference(value.Content)
		clone.QueuedGuidance[key] = value
	}
	clone.Attachments = cloneMap(source.Attachments)
	clone.RetryAuthorizations = cloneMap(source.RetryAuthorizations)
	clone.ActionGuards = cloneMap(source.ActionGuards)
	clone.RecentRecaps = append([]RecapCheckpoint(nil), source.RecentRecaps...)
	for index := range clone.RecentRecaps {
		clone.RecentRecaps[index].Summary = cloneReference(clone.RecentRecaps[index].Summary)
	}
	if source.Terminal != nil {
		terminal := *source.Terminal
		clone.Terminal = &terminal
	}
	return clone
}

func cloneReference(source *ContentReference) *ContentReference {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	clone := make(map[K]V, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func retryAuthorizationMatchesGuard(state *State, authorization RetryAuthorization, guard ActionGuard, retryTurnID string) bool {
	if authorization.ConsumedBy != "" || authorization.ActionDigest != guard.ActionDigest {
		return false
	}
	if authorization.SourceCallID != "" && authorization.SourceCallID != guard.CallID {
		return false
	}
	if authorization.TurnID == "" {
		source, exists := state.ToolCalls[guard.CallID]
		return exists && source.TurnID == retryTurnID
	}
	source, exists := state.ToolCalls[guard.CallID]
	return exists && source.TurnID == authorization.TurnID && retryTurnID == authorization.TurnID
}

// The live runtime separately attests a fresh model decision after observing
// the result and a relevant change. Bind that decision to the exact durable
// result/guard; a changed input alone cannot clear a prior unknown effect.
func observedRepeatMatchesGuard(state *State, sourceID, digest, turnID, name string, effect ToolEffect, inputVersion string) bool {
	guard, guarded := state.ActionGuards[digest]
	source, hasCall := state.ToolCalls[sourceID]
	execution, hasExecution := state.ToolExecutions[sourceID]
	_, hasResult := state.ToolResults[sourceID]
	if !(effect == ToolMutation && guarded && guard.CallID == sourceID && guard.ResultRecorded && !guard.RetryBoundaryUnresolved &&
		hasCall && source.TurnID != turnID && source.ActionDigest == digest && source.Name == name && source.Effect == effect &&
		hasExecution && hasResult) {
		return false
	}
	if source.InputVersion != "" || inputVersion != "" {
		return execution.Outcome == ExecutionSucceeded && validHash(source.InputVersion) &&
			validHash(inputVersion) && source.InputVersion != inputVersion
	}
	// Preserve the existing unversioned foreground-process repeat contract.
	// Process exit can be observed even when the command reports a nonzero status.
	return execution.ProcessExited && (execution.Outcome == ExecutionSucceeded || execution.Outcome == ExecutionEffectUnknown)
}

func applyEvent(state *State, event Event) error {
	if err := validateEvent(event); err != nil {
		return err
	}
	switch event.Kind {
	case EventTurn:
		turn := *event.Turn
		if turn.Status == TurnRunning {
			if state.CurrentTurn != nil && state.CurrentTurn.TurnID != turn.TurnID {
				return ErrTurnActive
			}
			state.CurrentTurn = &turn
			state.Terminal = nil
			return nil
		}
		if state.CurrentTurn == nil || state.CurrentTurn.TurnID != turn.TurnID {
			return ErrTurnNotActive
		}
		state.RecentTurns = append(state.RecentTurns, turn)
		state.CurrentTurn = nil
	case EventToolCall:
		call := *event.ToolCall
		if previous, exists := state.ToolCalls[call.CallID]; exists {
			if previous.Name == call.Name && previous.ActionDigest == call.ActionDigest && previous.InputVersion == call.InputVersion && previous.TurnID == call.TurnID {
				return nil
			}
			return ErrToolCallConflict
		}
		if _, guarded := state.ActionGuards[call.ActionDigest]; guarded && call.Effect != ToolReadOnly &&
			call.ObservedCallID == "" && call.RetryAuthorizationID == "" {
			return ErrSameActionRetry
		}
		if call.ObservedCallID != "" && (call.RetryAuthorizationID != "" ||
			!observedRepeatMatchesGuard(state, call.ObservedCallID, call.ActionDigest, call.TurnID, call.Name, call.Effect, call.InputVersion)) {
			return ErrSameActionRetry
		}
		if call.RetryAuthorizationID != "" {
			authorization, exists := state.RetryAuthorizations[call.ActionDigest]
			guard, guarded := state.ActionGuards[call.ActionDigest]
			if !exists || !guarded || authorization.AuthorizationID != call.RetryAuthorizationID ||
				!retryAuthorizationMatchesGuard(state, authorization, guard, call.TurnID) {
				return ErrSameActionRetry
			}
			// Older journals did not persist the source binding. Bind it to the
			// exact guard being consumed so a later crash cannot let this one-shot
			// authorization drift to another prepared action.
			if authorization.SourceCallID == "" {
				authorization.SourceCallID = guard.CallID
			}
			if authorization.TurnID == "" {
				authorization.TurnID = call.TurnID
			}
			authorization.ConsumedBy = call.CallID
			state.RetryAuthorizations[call.ActionDigest] = authorization
		}
		state.ToolCalls[call.CallID] = call
		if call.Effect != ToolReadOnly {
			reason := "mutation was prepared; execution may have crossed the crash boundary"
			if call.RetryAuthorizationID != "" {
				reason = "authorized mutation retry was prepared; its effect is unknown until a durable outcome is recorded"
			}
			state.ActionGuards[call.ActionDigest] = ActionGuard{
				ActionDigest: call.ActionDigest,
				CallID:       call.CallID,
				ToolName:     call.Name,
				Effect:       call.Effect,
				Reason:       reason,
				At:           call.PreparedAt,
			}
		}
	case EventToolExecution:
		execution := *event.ToolExecution
		if _, exists := state.ToolExecutions[execution.CallID]; exists {
			return ErrToolCallConflict
		}
		call, exists := state.ToolCalls[execution.CallID]
		if !exists {
			return errors.New("tool execution has no durable call")
		}
		state.ToolExecutions[execution.CallID] = execution
		if call.Effect != ToolReadOnly {
			switch execution.Outcome {
			case ExecutionNoChange, ExecutionFailedBeforeEffect:
				if call.RetryAuthorizationID == "" && call.ObservedCallID == "" {
					delete(state.ActionGuards, call.ActionDigest)
				} else {
					authorization := state.RetryAuthorizations[call.ActionDigest]
					sourceID := authorization.SourceCallID
					if call.ObservedCallID != "" {
						sourceID = call.ObservedCallID
					}
					if guard, ok := actionGuardForCall(state, sourceID); ok {
						guard.Reason = "authorized retry stopped before effect; the prior mutation guard remains active"
						guard.RetryBoundaryUnresolved = true
						state.ActionGuards[call.ActionDigest] = guard
					} else if guard, exists := state.ActionGuards[call.ActionDigest]; exists {
						// Missing legacy source metadata must fail closed. Keeping the
						// prepared retry guarded is safer than erasing the boundary.
						guard.Reason = "authorized retry stopped before effect; prior guard source is unavailable and review is required"
						guard.RetryBoundaryUnresolved = true
						state.ActionGuards[call.ActionDigest] = guard
					}
				}
			case ExecutionSucceeded:
				guard := state.ActionGuards[call.ActionDigest]
				guard.CallID = call.CallID
				guard.ToolName = call.Name
				guard.Effect = call.Effect
				guard.At = execution.At
				guard.ResultRecorded = false
				guard.RetryBoundaryUnresolved = false
				guard.Reason = "mutation execution succeeded and must not be repeated implicitly"
				state.ActionGuards[call.ActionDigest] = guard
			case ExecutionEffectUnknown:
				guard := state.ActionGuards[call.ActionDigest]
				guard.CallID = call.CallID
				guard.ToolName = call.Name
				guard.Effect = call.Effect
				guard.At = execution.At
				guard.ResultRecorded = false
				guard.RetryBoundaryUnresolved = false
				guard.Reason = "mutation effect is unknown and requires review before retry"
				state.ActionGuards[call.ActionDigest] = guard
			}
		}
	case EventToolResult:
		result := *event.ToolResult
		if previous, exists := state.ToolResults[result.CallID]; exists {
			if previous.Name == result.Name && previous.Summary == result.Summary && sameReference(previous.Output, result.Output) {
				return nil
			}
			return ErrToolResultConflict
		}
		call, exists := state.ToolCalls[result.CallID]
		if !exists || call.Name != result.Name {
			return ErrToolResultConflict
		}
		state.ToolResults[result.CallID] = result
		if guard, exists := state.ActionGuards[call.ActionDigest]; exists {
			guard.ResultRecorded = true
			state.ActionGuards[call.ActionDigest] = guard
		}
	case EventPermission:
		permission := *event.Permission
		if permission.Decision == PermissionPending {
			if _, exists := state.PendingPermissions[permission.RequestID]; exists {
				return ErrPermissionConflict
			}
			state.PendingPermissions[permission.RequestID] = permission
		} else {
			pending, exists := state.PendingPermissions[permission.RequestID]
			if !exists || pending.TurnID != permission.TurnID || pending.CallID != permission.CallID || pending.Scope != permission.Scope {
				return ErrPermissionConflict
			}
			if permission.RetryAuthorizationID != "" {
				if permission.Decision != PermissionApproved || !validIdentifier(permission.RetryAuthorizationID) ||
					!validIdentifier(permission.RetryApprovedBy) || strings.TrimSpace(permission.RetryReason) == "" {
					return ErrPermissionConflict
				}
				call, callExists := state.ToolCalls[pending.CallID]
				guard, guarded := state.ActionGuards[call.ActionDigest]
				if !callExists || !guarded || guard.CallID != call.CallID || call.TurnID != pending.TurnID {
					return ErrPermissionConflict
				}
				if previous, exists := state.RetryAuthorizations[call.ActionDigest]; exists && previous.ConsumedBy == "" {
					return ErrSameActionRetry
				}
				state.RetryAuthorizations[call.ActionDigest] = RetryAuthorization{
					AuthorizationID:    permission.RetryAuthorizationID,
					ActionDigest:       call.ActionDigest,
					ApprovedBy:         permission.RetryApprovedBy,
					Reason:             permission.RetryReason,
					SourcePermissionID: permission.RequestID,
					SourceCallID:       call.CallID,
					TurnID:             call.TurnID,
					At:                 permission.At,
				}
			} else if permission.RetryApprovedBy != "" || permission.RetryReason != "" {
				return ErrPermissionConflict
			}
			delete(state.PendingPermissions, permission.RequestID)
		}
	case EventGuidance:
		guidance := *event.Guidance
		if guidance.Status == GuidanceQueued {
			if _, exists := state.QueuedGuidance[guidance.GuidanceID]; exists {
				return ErrGuidanceConflict
			}
			state.QueuedGuidance[guidance.GuidanceID] = guidance
		} else {
			if _, exists := state.QueuedGuidance[guidance.GuidanceID]; !exists {
				return ErrGuidanceConflict
			}
			delete(state.QueuedGuidance, guidance.GuidanceID)
		}
	case EventAttachment:
		state.Attachments[event.Attachment.AttachmentID] = *event.Attachment
	case EventRetryAuthorization:
		authorization := *event.RetryAuthorization
		guard, guarded := state.ActionGuards[authorization.ActionDigest]
		if !guarded || !validIdentifier(authorization.AuthorizationID) ||
			!validIdentifier(authorization.ApprovedBy) || strings.TrimSpace(authorization.Reason) == "" ||
			(authorization.SourceCallID != "" && authorization.SourceCallID != guard.CallID) {
			return ErrSameActionRetry
		}
		if authorization.TurnID != "" {
			call, exists := state.ToolCalls[guard.CallID]
			if !exists || call.TurnID != authorization.TurnID {
				return ErrSameActionRetry
			}
		}
		if previous, exists := state.RetryAuthorizations[authorization.ActionDigest]; exists && previous.ConsumedBy == "" {
			return ErrSameActionRetry
		}
		state.RetryAuthorizations[authorization.ActionDigest] = authorization
	case EventRecap:
		state.RecentRecaps = append(state.RecentRecaps, *event.Recap)
	case EventTerminal:
		terminal := *event.Terminal
		state.Terminal = &terminal
	}
	return nil
}

func actionGuardForCall(state *State, callID string) (ActionGuard, bool) {
	call, exists := state.ToolCalls[callID]
	if !exists || call.Effect == ToolReadOnly {
		return ActionGuard{}, false
	}
	at := call.PreparedAt
	reason := "prior mutation remains guarded"
	if execution, ok := state.ToolExecutions[callID]; ok {
		if !execution.At.IsZero() {
			at = execution.At
		}
		switch execution.Outcome {
		case ExecutionSucceeded:
			reason = "mutation execution succeeded and must not be repeated implicitly"
		case ExecutionEffectUnknown:
			reason = "mutation effect is unknown and requires review before retry"
		}
	}
	_, resultRecorded := state.ToolResults[callID]
	return ActionGuard{
		ActionDigest:   call.ActionDigest,
		CallID:         call.CallID,
		ToolName:       call.Name,
		Effect:         call.Effect,
		Reason:         reason,
		At:             at,
		ResultRecorded: resultRecorded,
	}, true
}

// reconstructUnknownEffectGuards migrates snapshots written by builds that
// incorrectly treated a model-facing ToolResult as proof that an
// effect-unknown mutation was complete. The call and execution checkpoints are
// sufficient to restore the fail-closed boundary without replaying anything.
func reconstructUnknownEffectGuards(state *State) {
	existing := make(map[string]struct{}, len(state.ActionGuards))
	for digest := range state.ActionGuards {
		existing[digest] = struct{}{}
	}
	candidates := map[string]ActionGuard{}
	callIDs := make([]string, 0, len(state.ToolExecutions))
	for callID := range state.ToolExecutions {
		callIDs = append(callIDs, callID)
	}
	sort.Strings(callIDs)
	for _, callID := range callIDs {
		execution := state.ToolExecutions[callID]
		if execution.Outcome != ExecutionEffectUnknown {
			continue
		}
		guard, ok := actionGuardForCall(state, callID)
		if !ok {
			continue
		}
		if _, guarded := existing[guard.ActionDigest]; guarded {
			continue
		}
		guard.Reason = "reconstructed effect-unknown mutation guard from durable call and execution checkpoints"
		if previous, exists := candidates[guard.ActionDigest]; !exists || previous.At.Before(guard.At) ||
			(previous.At.Equal(guard.At) && previous.CallID < guard.CallID) {
			candidates[guard.ActionDigest] = guard
		}
	}
	for digest, guard := range candidates {
		state.ActionGuards[digest] = guard
	}
}

func validateEvent(event Event) error {
	if !validIdentifier(event.ID) || event.At.IsZero() {
		return errors.New("event identity or timestamp is invalid")
	}
	payloads := 0
	for _, present := range []bool{
		event.Turn != nil, event.ToolCall != nil, event.ToolExecution != nil,
		event.ToolResult != nil, event.Permission != nil, event.Guidance != nil,
		event.Attachment != nil, event.RetryAuthorization != nil, event.Terminal != nil,
		event.Recap != nil,
	} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return errors.New("event must contain exactly one payload")
	}
	valid := (event.Kind == EventTurn && event.Turn != nil) ||
		(event.Kind == EventToolCall && event.ToolCall != nil) ||
		(event.Kind == EventToolExecution && event.ToolExecution != nil) ||
		(event.Kind == EventToolResult && event.ToolResult != nil) ||
		(event.Kind == EventPermission && event.Permission != nil) ||
		(event.Kind == EventGuidance && event.Guidance != nil) ||
		(event.Kind == EventAttachment && event.Attachment != nil) ||
		(event.Kind == EventRetryAuthorization && event.RetryAuthorization != nil) ||
		(event.Kind == EventRecap && event.Recap != nil) ||
		(event.Kind == EventTerminal && event.Terminal != nil)
	if !valid {
		return errors.New("event kind does not match payload")
	}
	if event.ToolCall != nil && event.ToolCall.InputVersion != "" && !validHash(event.ToolCall.InputVersion) {
		return errors.New("tool input version is invalid")
	}
	return nil
}

func boundedState(source State, limits Limits) (State, error) {
	state := cloneState(source)
	if len(state.RecentTurns) > limits.MaxRecentTurns {
		state.RecentTurns = append([]TurnCheckpoint(nil), state.RecentTurns[len(state.RecentTurns)-limits.MaxRecentTurns:]...)
	}
	if len(state.RecentRecaps) > limits.MaxRecentRecaps {
		state.RecentRecaps = append([]RecapCheckpoint(nil), state.RecentRecaps[len(state.RecentRecaps)-limits.MaxRecentRecaps:]...)
	}
	if len(state.PendingPermissions) > limits.MaxPendingPermissions || len(state.QueuedGuidance) > limits.MaxQueuedGuidance || len(state.Attachments) > limits.MaxAttachments {
		return State{}, ErrStateLimit
	}
	resultIDs := newestToolResultIDs(state.ToolResults, limits.MaxToolResults)
	for callID := range state.ToolResults {
		if _, keep := resultIDs[callID]; !keep {
			delete(state.ToolResults, callID)
		}
	}
	for callID, call := range state.ToolCalls {
		_, hasResult := state.ToolResults[callID]
		current := state.CurrentTurn != nil && call.TurnID == state.CurrentTurn.TurnID
		_, guarded := state.ActionGuards[call.ActionDigest]
		if !hasResult && !current && !guarded {
			delete(state.ToolCalls, callID)
			delete(state.ToolExecutions, callID)
		}
	}
	if len(state.ActionGuards) > limits.MaxActionGuards {
		unresolved := make(map[string]ActionGuard)
		var resolved []ActionGuard
		for digest, guard := range state.ActionGuards {
			execution := state.ToolExecutions[guard.CallID]
			if guard.RetryBoundaryUnresolved || !guard.ResultRecorded || execution.Outcome == ExecutionEffectUnknown {
				unresolved[digest] = guard
			} else {
				resolved = append(resolved, guard)
			}
		}
		if len(unresolved) > limits.MaxActionGuards {
			return State{}, fmt.Errorf("%w: unresolved action guards exceed limit", ErrStateLimit)
		}
		sort.Slice(resolved, func(i, j int) bool { return resolved[i].At.After(resolved[j].At) })
		for index := 0; index < len(resolved) && len(unresolved) < limits.MaxActionGuards; index++ {
			unresolved[resolved[index].ActionDigest] = resolved[index]
		}
		state.ActionGuards = unresolved
	}
	for digest, authorization := range state.RetryAuthorizations {
		if authorization.ConsumedBy != "" {
			delete(state.RetryAuthorizations, digest)
		}
	}
	return state, nil
}

func newestToolResultIDs(results map[string]ToolResultCheckpoint, limit int) map[string]struct{} {
	values := make([]ToolResultCheckpoint, 0, len(results))
	for _, result := range results {
		values = append(values, result)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].At.After(values[j].At) })
	if len(values) > limit {
		values = values[:limit]
	}
	ids := make(map[string]struct{}, len(values))
	for _, result := range values {
		ids[result.CallID] = struct{}{}
	}
	return ids
}
