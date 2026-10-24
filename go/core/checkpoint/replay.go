package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

func newState() State {
	return State{
		ToolCalls: make(map[string]ToolCallState),
		ToolResults: make(map[string]ToolResultState),
		Permissions: make(map[string]PermissionState),
		IdempotencyKeys: make(map[string]IdempotencyRecord),
		AppliedEvents: make(map[string]string),
	}
}

func cloneState(state State) State {
	cloned := state
	if state.CurrentTurn != nil {
		value := *state.CurrentTurn
		value.Prompt = cloneReference(state.CurrentTurn.Prompt)
		cloned.CurrentTurn = &value
	}
	if state.Cancellation != nil {
		value := *state.Cancellation
		cloned.Cancellation = &value
	}
	if state.Generation != nil {
		value := *state.Generation
		value.Prompt = cloneReference(state.Generation.Prompt)
		value.Output = cloneReference(state.Generation.Output)
		cloned.Generation = &value
	}
	if state.Terminal != nil {
		value := *state.Terminal
		cloned.Terminal = &value
	}
	cloned.TaskProgress = cloneTaskProgress(state.TaskProgress)
	if state.LastAppliedEvent != nil {
		value := *state.LastAppliedEvent
		cloned.LastAppliedEvent = &value
	}
	cloned.ToolCalls = make(map[string]ToolCallState, len(state.ToolCalls))
	for key, value := range state.ToolCalls {
		value.Arguments = cloneReference(value.Arguments)
		cloned.ToolCalls[key] = value
	}
	cloned.ToolResults = make(map[string]ToolResultState, len(state.ToolResults))
	for key, value := range state.ToolResults {
		value.Output = cloneReference(value.Output)
		cloned.ToolResults[key] = value
	}
	cloned.Permissions = make(map[string]PermissionState, len(state.Permissions))
	for key, value := range state.Permissions {
		cloned.Permissions[key] = value
	}
	cloned.AppliedEvents = make(map[string]string, len(state.AppliedEvents))
	for key, value := range state.AppliedEvents {
		cloned.AppliedEvents[key] = value
	}
	cloned.IdempotencyKeys = make(map[string]IdempotencyRecord, len(state.IdempotencyKeys))
	for key, value := range state.IdempotencyKeys {
		cloned.IdempotencyKeys[key] = value
	}
	return cloned
}

func cloneReference(reference *ContentReference) *ContentReference {
	if reference == nil {
		return nil
	}
	value := *reference
	return &value
}

// Apply replays one already-sanitized event. It returns false for an exact
// duplicate event ID. Reusing an ID with different content is an error.
func Apply(state *State, event Event) (bool, error) {
	if state == nil {
		return false, errorsNew("checkpoint replay state is nil")
	}
	if err := validateEvent(event); err != nil {
		return false, err
	}
	ensureStateMaps(state)
	digest, err := eventIdentityDigest(event)
	if err != nil {
		return false, err
	}
	if previous, exists := state.AppliedEvents[event.ID]; exists {
		if previous != digest {
			return false, fmt.Errorf("%w: %s", ErrEventIDConflict, event.ID)
		}
		return false, nil
	}
	if state.Terminal != nil {
		return false, ErrTerminalAlreadySet
	}
	if event.Kind == EventTaskProgress {
		decision, replayErr := EvaluateTaskProgressEvent(*state, event)
		if replayErr != nil {
			return false, replayErr
		}
		if decision == ProgressReplayDuplicate {
			return false, nil
		}
	}

	switch event.Kind {
	case EventTurn:
		if err := applyTurn(state, *event.Turn); err != nil {
			return false, err
		}
	case EventToolCall:
		if err := applyToolCall(state, *event.ToolCall); err != nil {
			return false, err
		}
	case EventToolResult:
		if err := applyToolResult(state, *event.ToolResult); err != nil {
			return false, err
		}
	case EventCancel:
		if state.Cancellation != nil {
			return false, errorsNew("cancellation state is already set")
		}
		value := *event.Cancellation
		state.Cancellation = &value
	case EventModelGeneration:
		if err := applyGeneration(state, *event.Generation); err != nil {
			return false, err
		}
	case EventPermission:
		if err := applyPermission(state, *event.Permission); err != nil {
			return false, err
		}
	case EventTerminal:
		value := *event.Terminal
		value.At = event.At
		state.Terminal = &value
	case EventTaskProgress:
		applyTaskProgress(state, *event.TaskProgress, digest, event)
	default:
		return false, fmt.Errorf("unknown checkpoint event kind %q", event.Kind)
	}
	state.AppliedEvents[event.ID] = digest
	state.LastAppliedEvent = &AppliedEventCursor{
		EventID: event.ID,
		Kind: event.Kind,
		Digest: digest,
		IdempotencyKey: taskProgressIdempotencyKey(event),
		ProgressRevision: state.ProgressRevision,
		At: event.At,
	}
	return true, nil
}

func ensureStateMaps(state *State) {
	if state.ToolCalls == nil {
		state.ToolCalls = make(map[string]ToolCallState)
	}
	if state.ToolResults == nil {
		state.ToolResults = make(map[string]ToolResultState)
	}
	if state.Permissions == nil {
		state.Permissions = make(map[string]PermissionState)
	}
	if state.AppliedEvents == nil {
		state.AppliedEvents = make(map[string]string)
	}
	if state.IdempotencyKeys == nil {
		state.IdempotencyKeys = make(map[string]IdempotencyRecord)
	}
}

func applyTurn(state *State, next TurnState) error {
	current := state.CurrentTurn
	if current == nil {
		value := next
		value.Prompt = cloneReference(next.Prompt)
		state.CurrentTurn = &value
		return nil
	}
	if current.TurnID != next.TurnID {
		if !isTurnTerminal(current.Status) {
			return fmt.Errorf("cannot replace active turn %s with %s", current.TurnID, next.TurnID)
		}
		value := next
		value.Prompt = cloneReference(next.Prompt)
		state.CurrentTurn = &value
		return nil
	}
	if !sameReference(current.Prompt, next.Prompt) {
		return errorsNew("turn prompt reference changed during replay")
	}
	if !validTurnTransition(current.Status, next.Status) {
		return fmt.Errorf("invalid turn transition %s -> %s", current.Status, next.Status)
	}
	value := next
	value.Prompt = cloneReference(next.Prompt)
	state.CurrentTurn = &value
	return nil
}

func applyToolCall(state *State, next ToolCallState) error {
	current, exists := state.ToolCalls[next.CallID]
	if !exists {
		next.Arguments = cloneReference(next.Arguments)
		state.ToolCalls[next.CallID] = next
		return nil
	}
	if _, done := state.ToolResults[next.CallID]; done {
		return fmt.Errorf("tool call %s already has a result", next.CallID)
	}
	if current.Name != next.Name || current.TurnID != next.TurnID || !sameReference(current.Arguments, next.Arguments) {
		return fmt.Errorf("tool call %s immutable fields changed", next.CallID)
	}
	if !validToolCallTransition(current.Status, next.Status) {
		return fmt.Errorf("invalid tool call transition %s -> %s", current.Status, next.Status)
	}
	next.Arguments = cloneReference(next.Arguments)
	state.ToolCalls[next.CallID] = next
	return nil
}

func applyToolResult(state *State, result ToolResultState) error {
	call, exists := state.ToolCalls[result.CallID]
	if !exists {
		return fmt.Errorf("tool result references unknown call %s", result.CallID)
	}
	if _, exists := state.ToolResults[result.CallID]; exists {
		return fmt.Errorf("tool call %s already has a result", result.CallID)
	}
	if isToolCallTerminal(call.Status) {
		return fmt.Errorf("tool call %s is already terminal", result.CallID)
	}
	switch result.Status {
	case ToolResultSucceeded:
		call.Status = ToolCallCompleted
	case ToolResultFailed:
		call.Status = ToolCallFailed
	case ToolResultCancelled:
		call.Status = ToolCallCancelled
	}
	result.Output = cloneReference(result.Output)
	state.ToolCalls[result.CallID] = call
	state.ToolResults[result.CallID] = result
	return nil
}

func applyGeneration(state *State, next ModelGenerationState) error {
	current := state.Generation
	if current == nil || current.GenerationID != next.GenerationID {
		if current != nil && !isGenerationTerminal(current.Status) {
			return fmt.Errorf("cannot replace active generation %s with %s", current.GenerationID, next.GenerationID)
		}
		value := next
		value.Prompt = cloneReference(next.Prompt)
		value.Output = cloneReference(next.Output)
		state.Generation = &value
		return nil
	}
	if current.TurnID != next.TurnID || current.Model != next.Model || !sameReference(current.Prompt, next.Prompt) {
		return errorsNew("model generation immutable fields changed")
	}
	if !validGenerationTransition(current.Status, next.Status) {
		return fmt.Errorf("invalid generation transition %s -> %s", current.Status, next.Status)
	}
	value := next
	value.Prompt = cloneReference(next.Prompt)
	value.Output = cloneReference(next.Output)
	state.Generation = &value
	return nil
}

func applyPermission(state *State, next PermissionState) error {
	current, exists := state.Permissions[next.RequestID]
	if !exists {
		state.Permissions[next.RequestID] = next
		return nil
	}
	if current.TurnID != next.TurnID || current.Scope != next.Scope {
		return fmt.Errorf("permission %s immutable fields changed", next.RequestID)
	}
	if current.Decision != PermissionPending {
		return fmt.Errorf("permission %s is already decided", next.RequestID)
	}
	if next.Decision == PermissionPending {
		return fmt.Errorf("permission %s repeated pending state", next.RequestID)
	}
	state.Permissions[next.RequestID] = next
	return nil
}

func validateEvent(event Event) error {
	if event.ID == "" {
		return errorsNew("checkpoint event id is required")
	}
	if event.At.IsZero() {
		return errorsNew("checkpoint event time is required")
	}
	payloads := 0
	for _, present := range []bool{
		event.Turn != nil,
		event.ToolCall != nil,
		event.ToolResult != nil,
		event.Cancellation != nil,
		event.Generation != nil,
		event.Permission != nil,
		event.Terminal != nil,
		event.TaskProgress != nil,
	} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return fmt.Errorf("checkpoint event must have exactly one payload, got %d", payloads)
	}
	switch event.Kind {
	case EventTurn:
		if event.Turn == nil || !validTurnStatus(event.Turn.Status) || event.Turn.TurnID == "" {
			return errorsNew("invalid turn event")
		}
		return validateReference(event.Turn.Prompt)
	case EventToolCall:
		if event.ToolCall == nil || event.ToolCall.CallID == "" || event.ToolCall.Name == "" || !validToolCallStatus(event.ToolCall.Status) {
			return errorsNew("invalid tool call event")
		}
		return validateReference(event.ToolCall.Arguments)
	case EventToolResult:
		if event.ToolResult == nil || event.ToolResult.CallID == "" || !validToolResultStatus(event.ToolResult.Status) {
			return errorsNew("invalid tool result event")
		}
		return validateReference(event.ToolResult.Output)
	case EventCancel:
		if event.Cancellation == nil {
			return errorsNew("invalid cancellation event")
		}
	case EventModelGeneration:
		if event.Generation == nil || event.Generation.GenerationID == "" || !validGenerationStatus(event.Generation.Status) || event.Generation.PromptTokens < 0 || event.Generation.CompletionTokens < 0 {
			return errorsNew("invalid model generation event")
		}
		if err := validateReference(event.Generation.Prompt); err != nil {
			return err
		}
		return validateReference(event.Generation.Output)
	case EventPermission:
		if event.Permission == nil || event.Permission.RequestID == "" || event.Permission.Scope == "" || !validPermissionDecision(event.Permission.Decision) {
			return errorsNew("invalid permission event")
		}
	case EventTerminal:
		if event.Terminal == nil || !validTerminalOutcome(event.Terminal.Outcome) {
			return errorsNew("invalid terminal event")
		}
	case EventTaskProgress:
		return validateTaskProgressUpdate(event.TaskProgress)
	default:
		return fmt.Errorf("unknown checkpoint event kind %q", event.Kind)
	}
	return nil
}

func validateReference(reference *ContentReference) error {
	if reference == nil {
		return nil
	}
	if reference.Bytes < 0 || !validHash(reference.SHA256) {
		return errorsNew("invalid content reference")
	}
	if reference.Sensitive && reference.Summary != sensitiveContentMarker {
		return errorsNew("sensitive content reference has a non-redacted summary")
	}
	return nil
}

func validateState(state State, options Options) error {
	for eventID, digest := range state.AppliedEvents {
		if _, err := sanitizeIdentifier("applied event id", eventID, options.Redactor); err != nil {
			return err
		}
		if !validHash(digest) {
			return fmt.Errorf("applied event %s has invalid digest", eventID)
		}
	}
	for key, record := range state.IdempotencyKeys {
		if _, err := sanitizeIdentifier("idempotency key", key, options.Redactor); err != nil {
			return err
		}
		if key != record.Key || record.EventID == "" || !validHash(record.MutationDigest) || record.ProgressRevision == 0 {
			return fmt.Errorf("snapshot has invalid idempotency record %s", key)
		}
	}
	if state.TaskProgress != nil {
		if state.ProgressRevision == 0 {
			return errorsNew("snapshot task progress has no revision")
		}
		if err := validateTaskProgressState(*state.TaskProgress); err != nil {
			return err
		}
	} else if state.ProgressRevision != 0 || len(state.IdempotencyKeys) != 0 {
		return errorsNew("snapshot has progress metadata without task progress")
	}
	if state.LastAppliedEvent != nil {
		if err := validateAppliedEventCursor(*state.LastAppliedEvent); err != nil {
			return err
		}
		if digest, ok := state.AppliedEvents[state.LastAppliedEvent.EventID]; !ok || digest != state.LastAppliedEvent.Digest {
			return errorsNew("snapshot last applied event is outside applied event set")
		}
	}
	if state.CurrentTurn != nil {
		if !validTurnStatus(state.CurrentTurn.Status) || state.CurrentTurn.TurnID == "" {
			return errorsNew("snapshot has invalid current turn")
		}
		if err := validateReference(state.CurrentTurn.Prompt); err != nil {
			return err
		}
	}
	for callID, call := range state.ToolCalls {
		if callID != call.CallID || call.Name == "" || !validToolCallStatus(call.Status) {
			return fmt.Errorf("snapshot has invalid tool call %s", callID)
		}
		if err := validateReference(call.Arguments); err != nil {
			return err
		}
	}
	for callID, result := range state.ToolResults {
		call, exists := state.ToolCalls[callID]
		if !exists || callID != result.CallID || !validToolResultStatus(result.Status) || !isToolCallTerminal(call.Status) {
			return fmt.Errorf("snapshot has invalid tool result %s", callID)
		}
		if err := validateReference(result.Output); err != nil {
			return err
		}
	}
	if state.Generation != nil {
		if state.Generation.GenerationID == "" || !validGenerationStatus(state.Generation.Status) || state.Generation.PromptTokens < 0 || state.Generation.CompletionTokens < 0 {
			return errorsNew("snapshot has invalid generation state")
		}
		if err := validateReference(state.Generation.Prompt); err != nil {
			return err
		}
		if err := validateReference(state.Generation.Output); err != nil {
			return err
		}
	}
	for requestID, permission := range state.Permissions {
		if requestID != permission.RequestID || permission.Scope == "" || !validPermissionDecision(permission.Decision) {
			return fmt.Errorf("snapshot has invalid permission %s", requestID)
		}
	}
	if state.Terminal != nil && (!validTerminalOutcome(state.Terminal.Outcome) || state.Terminal.At.IsZero()) {
		return errorsNew("snapshot has invalid terminal state")
	}
	return validateStateRedaction(state, options)
}

func validateStateRedaction(state State, options Options) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	redacted, changed := applyRedactors(string(encoded), options.Redactor)
	if changed && redacted != string(encoded) {
		return errorsNew("snapshot contains unredacted credential-like data")
	}
	if len(encoded) > int(options.Limits.MaxSnapshotBytes) {
		return ErrRecoveryLimit
	}
	return nil
}

func eventIdentityDigest(event Event) (string, error) {
	identity := event
	identity.At = time.Time{}
	if identity.Terminal != nil {
		value := *identity.Terminal
		value.At = time.Time{}
		identity.Terminal = &value
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode event identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:]), nil
}

func sameReference(left, right *ContentReference) bool {
	return reflect.DeepEqual(left, right)
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validTurnStatus(status TurnStatus) bool {
	switch status {
	case TurnPending, TurnRunning, TurnWaitingTool, TurnWaitingPermission, TurnCompleted, TurnCancelled, TurnFailed:
		return true
	default:
		return false
	}
}

func validTurnTransition(from, to TurnStatus) bool {
	if isTurnTerminal(from) {
		return false
	}
	if from == to {
		return true
	}
	switch from {
	case TurnPending:
		return to == TurnRunning || to == TurnWaitingPermission || to == TurnCancelled || to == TurnFailed
	case TurnRunning:
		return to == TurnWaitingTool || to == TurnWaitingPermission || isTurnTerminal(to)
	case TurnWaitingTool, TurnWaitingPermission:
		return to == TurnRunning || to == TurnWaitingTool || to == TurnWaitingPermission || isTurnTerminal(to)
	default:
		return false
	}
}

func isTurnTerminal(status TurnStatus) bool {
	return status == TurnCompleted || status == TurnCancelled || status == TurnFailed
}

func validToolCallStatus(status ToolCallStatus) bool {
	switch status {
	case ToolCallRequested, ToolCallRunning, ToolCallCompleted, ToolCallFailed, ToolCallCancelled:
		return true
	default:
		return false
	}
}

func validToolCallTransition(from, to ToolCallStatus) bool {
	if isToolCallTerminal(from) {
		return false
	}
	return from == to || (from == ToolCallRequested && to == ToolCallRunning)
}

func isToolCallTerminal(status ToolCallStatus) bool {
	return status == ToolCallCompleted || status == ToolCallFailed || status == ToolCallCancelled
}

func validToolResultStatus(status ToolResultStatus) bool {
	return status == ToolResultSucceeded || status == ToolResultFailed || status == ToolResultCancelled
}

func validGenerationStatus(status GenerationStatus) bool {
	switch status {
	case GenerationQueued, GenerationRunning, GenerationCompleted, GenerationFailed, GenerationCancelled:
		return true
	default:
		return false
	}
}

func validGenerationTransition(from, to GenerationStatus) bool {
	if isGenerationTerminal(from) {
		return false
	}
	if from == to {
		return true
	}
	if from == GenerationQueued {
		return to == GenerationRunning || isGenerationTerminal(to)
	}
	return from == GenerationRunning && isGenerationTerminal(to)
}

func isGenerationTerminal(status GenerationStatus) bool {
	return status == GenerationCompleted || status == GenerationFailed || status == GenerationCancelled
}

func validPermissionDecision(decision PermissionDecision) bool {
	return decision == PermissionPending || decision == PermissionApproved || decision == PermissionDenied
}

func validTerminalOutcome(outcome TerminalOutcome) bool {
	return outcome == TerminalCompleted || outcome == TerminalFailed || outcome == TerminalCancelled
}

func errorsNew(message string) error {
	return fmt.Errorf("%s", message)
}
