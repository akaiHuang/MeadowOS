package sessionrecovery

import "sort"

// BuildResumePlan is deterministic and side-effect free. It never turns a
// possibly-executed mutation into an automatic retry.
func BuildResumePlan(recovery Recovery) ResumePlan {
	plan := ResumePlan{CrashDetected: recovery.CrashDetected}
	if recovery.Corrupt {
		plan.Mode = ResumeBlockedCorrupt
		plan.NextSteps = []string{"Stop at the trusted journal boundary and require operator repair or quarantine."}
		return plan
	}
	state := recovery.State
	pendingCallIDs := make(map[string]struct{}, len(state.PendingPermissions))
	if state.CurrentTurn != nil {
		plan.TurnID = state.CurrentTurn.TurnID
	}
	for _, permission := range state.PendingPermissions {
		plan.PendingPermissions = append(plan.PendingPermissions, permission)
		if permission.CallID != "" {
			pendingCallIDs[permission.CallID] = struct{}{}
		}
	}
	sort.Slice(plan.PendingPermissions, func(i, j int) bool {
		return plan.PendingPermissions[i].At.Before(plan.PendingPermissions[j].At)
	})
	for _, guidance := range state.QueuedGuidance {
		plan.QueuedGuidance = append(plan.QueuedGuidance, guidance)
	}
	sort.Slice(plan.QueuedGuidance, func(i, j int) bool {
		return plan.QueuedGuidance[i].QueuedAt.Before(plan.QueuedGuidance[j].QueuedAt)
	})
	for _, attachment := range state.Attachments {
		if attachment.Authorized && attachment.Available {
			plan.Attachments = append(plan.Attachments, attachment)
		}
	}
	sort.Slice(plan.Attachments, func(i, j int) bool {
		return plan.Attachments[i].AddedAt.Before(plan.Attachments[j].AddedAt)
	})
	for _, result := range state.ToolResults {
		if result.Output == nil || result.Output.Content == "" {
			continue
		}
		plan.OutputEvidence = append(plan.OutputEvidence, RecoveredOutputEvidence{
			CallID: result.CallID, Name: result.Name, Content: result.Output.Content,
			SHA256: result.Output.SHA256, Bytes: result.Output.Bytes, At: result.At,
		})
	}
	sort.Slice(plan.OutputEvidence, func(i, j int) bool {
		if plan.OutputEvidence[i].At.Equal(plan.OutputEvidence[j].At) {
			return plan.OutputEvidence[i].CallID < plan.OutputEvidence[j].CallID
		}
		return plan.OutputEvidence[i].At.Before(plan.OutputEvidence[j].At)
	})
	for _, recap := range state.RecentRecaps {
		if recap.Summary == nil || recap.Summary.Content == "" {
			continue
		}
		plan.RecapEvidence = append(plan.RecapEvidence, RecoveredRecapEvidence{
			RunID: recap.RunID, Status: recap.Status, StopReason: recap.StopReason,
			Content: recap.Summary.Content, SHA256: recap.Summary.SHA256,
			Bytes: recap.Summary.Bytes, At: recap.At,
		})
	}
	if state.CurrentTurn != nil {
		for callID, call := range state.ToolCalls {
			if call.TurnID != state.CurrentTurn.TurnID {
				continue
			}
			execution := state.ToolExecutions[callID]
			if execution.Outcome == ExecutionEffectUnknown {
				if guard, guarded := state.ActionGuards[call.ActionDigest]; guarded && guard.CallID == callID {
					if _, awaitingPermission := pendingCallIDs[callID]; awaitingPermission {
						continue
					}
					if authorization, exists := state.RetryAuthorizations[call.ActionDigest]; exists &&
						retryAuthorizationMatchesGuard(&state, authorization, guard, call.TurnID) {
						continue
					}
					plan.ReviewRequired = append(plan.ReviewRequired, guard)
				}
				continue
			}
			if _, completed := state.ToolResults[callID]; completed {
				plan.CompletedToolCallIDs = append(plan.CompletedToolCallIDs, callID)
				continue
			}
			if call.Effect == ToolReadOnly {
				plan.RetryableToolCalls = append(plan.RetryableToolCalls, call)
				continue
			}
			if guard, guarded := state.ActionGuards[call.ActionDigest]; guarded && guard.CallID == callID {
				if _, awaitingPermission := pendingCallIDs[callID]; awaitingPermission {
					continue
				}
				if authorization, exists := state.RetryAuthorizations[call.ActionDigest]; exists &&
					retryAuthorizationMatchesGuard(&state, authorization, guard, call.TurnID) {
					continue
				}
				plan.ReviewRequired = append(plan.ReviewRequired, guard)
			}
		}
	}
	sort.Strings(plan.CompletedToolCallIDs)
	sort.Slice(plan.RetryableToolCalls, func(i, j int) bool {
		return plan.RetryableToolCalls[i].PreparedAt.Before(plan.RetryableToolCalls[j].PreparedAt)
	})
	sort.Slice(plan.ReviewRequired, func(i, j int) bool {
		return plan.ReviewRequired[i].At.Before(plan.ReviewRequired[j].At)
	})

	switch {
	case len(plan.ReviewRequired) > 0:
		plan.Mode = ResumeManualReview
		plan.NextSteps = []string{
			"Inspect external state for every guarded mutation before continuing.",
			"Record an explicit retry authorization only when repeating the same action is safe.",
			"Reuse durable tool results and do not synthesize replacements for missing mutation results.",
		}
	case len(plan.PendingPermissions) > 0:
		plan.Mode = ResumeAwaitPermission
		plan.NextSteps = []string{"Restore the pending permission UI and wait for an explicit decision."}
	case recovery.CrashDetected:
		plan.Mode = ResumeTurn
		plan.NextSteps = []string{
			"Restore the current turn from the newest trusted checkpoint.",
			"Inject queued guidance in original order.",
			"Reuse completed tool results and retry only listed read-only calls.",
		}
	case len(plan.QueuedGuidance) > 0:
		plan.Mode = ResumeApplyGuidance
		plan.NextSteps = []string{"Apply queued guidance in original order before the next model turn."}
	default:
		plan.Mode = ResumeNoAction
	}
	return plan
}
