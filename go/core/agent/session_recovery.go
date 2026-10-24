package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/sessionrecovery"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

var ErrRecoveryCheckpoint = errors.New("durable session recovery checkpoint failed")

type sessionRecoveryBridge struct {
	mu                  sync.Mutex
	recorder            sessionrecovery.Recorder
	plan                *sessionrecovery.ResumePlan
	required            bool
	openError           error
	sessionID           string
	runID               string
	activeTurnID        string
	resumedTurn         bool
	mutationObserved    bool
	workspaceRevision   uint64
	completedShellCalls map[string]observedShellCall
	completedInputCalls map[string]observedInputCall
	warnings            []string
}

type observedShellCall struct {
	turn              int
	workspaceRevision uint64
	observed          bool
}

type observedInputCall struct {
	turn         int
	inputVersion string
	outputHash   [sha256.Size]byte
	observed     bool
}

func newSessionRecoveryBridge(config CodexStyleConfig) *sessionRecoveryBridge {
	seed := fmt.Sprintf("%s\x00%d", config.SessionID, time.Now().UnixNano())
	digest := sha256.Sum256([]byte(seed))
	return &sessionRecoveryBridge{
		recorder: config.Recovery, plan: config.ResumePlan, required: config.RecoveryRequired,
		openError: config.RecoveryError, sessionID: config.SessionID,
		runID: "run-" + hex.EncodeToString(digest[:12]),
	}
}

func (b *sessionRecoveryBridge) enabled() bool { return b != nil && b.required }

func (b *sessionRecoveryBridge) initialWarning() string {
	if !b.enabled() || b.recorder != nil {
		return ""
	}
	if b.openError != nil {
		return "Session recovery is unavailable: " + b.openError.Error()
	}
	return "Session recovery is unavailable for this run."
}

func (b *sessionRecoveryBridge) noteWarning(err error) {
	if b == nil || err == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.warnings = append(b.warnings, err.Error())
}

func (b *sessionRecoveryBridge) warning() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	values := append([]string(nil), b.warnings...)
	if initial := b.initialWarning(); initial != "" {
		values = append(values, initial)
	}
	sort.Strings(values)
	values = recoveryUniqueStrings(values)
	return strings.Join(values, "; ")
}

func (b *sessionRecoveryBridge) requireRecorder() (sessionrecovery.Recorder, error) {
	if !b.enabled() {
		return nil, nil
	}
	if b.recorder == nil {
		if b.openError != nil {
			return nil, fmt.Errorf("%w: %v", ErrRecoveryCheckpoint, b.openError)
		}
		return nil, ErrRecoveryCheckpoint
	}
	return b.recorder, nil
}

func (b *sessionRecoveryBridge) beginTurn(turn int, prompt string) error {
	recorder, err := b.requireRecorder()
	if err != nil || recorder == nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.activeTurnID != "" {
		if _, finishErr := recorder.FinishTurn(sessionrecovery.FinishTurnRequest{
			TurnID: b.activeTurnID, Status: sessionrecovery.TurnCompleted,
			Summary: "advanced to the next safe model boundary",
		}); finishErr != nil {
			return finishErr
		}
		b.activeTurnID = ""
	}
	state := recorder.State()
	if !b.resumedTurn && state.CurrentTurn != nil {
		b.resumedTurn = true
		b.activeTurnID = state.CurrentTurn.TurnID
		return nil
	}
	ordinal := uint64(len(state.RecentTurns) + 1)
	// The turn identity must survive a process restart. Do not include the
	// process-local run nonce: the durable session, ordinal and prompt digest
	// are the stable boundary shared by CLI, serve and Harness recovery.
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%x", b.sessionID, ordinal, sha256.Sum256([]byte(prompt)))))
	turnID := "turn-" + hex.EncodeToString(digest[:12])
	promptReference := sessionrecovery.ReferenceAuto([]byte(prompt), "model request checkpoint")
	if _, err := recorder.BeginTurn(sessionrecovery.BeginTurnRequest{
		TurnID: turnID, Ordinal: ordinal, Prompt: &promptReference,
		Summary: "safe model boundary",
	}); err != nil {
		return err
	}
	b.activeTurnID = turnID
	return nil
}

func (b *sessionRecoveryBridge) finishActive(status sessionrecovery.TurnStatus, summary string) error {
	recorder, err := b.requireRecorder()
	if err != nil || recorder == nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.activeTurnID == "" {
		return nil
	}
	_, err = recorder.FinishTurn(sessionrecovery.FinishTurnRequest{
		TurnID: b.activeTurnID, Status: status, Summary: summary,
	})
	if err == nil {
		b.activeTurnID = ""
	}
	return err
}

func (b *sessionRecoveryBridge) beforeToolCall(call ToolCheckpointCall) error {
	recorder, err := b.requireRecorder()
	if err != nil || recorder == nil {
		return err
	}
	arguments := sessionrecovery.ReferenceAuto([]byte(call.RawArguments), "tool arguments for "+call.Tool)
	effect := recoveryToolCallEffect(call.Tool, call.Kind, call.Effect)
	actionArguments := []byte(call.RawArguments)
	legacyActionArguments := actionArguments
	// The executor has already applied the tool's lossless public argument
	// normalizers before this checkpoint. Identify the action by those values so
	// aliases such as "true" and true cannot bypass a guard for the same
	// operation; retain raw bytes only as the legacy upgrade alias below.
	if call.Args != nil {
		if normalized, marshalErr := json.Marshal(call.Args); marshalErr == nil {
			actionArguments = normalized
		}
	}
	if recoveryValidationTracksWorkspaceRevision(call) {
		b.mu.Lock()
		revision := b.workspaceRevision
		b.mu.Unlock()
		// A conventional validator is an observation of one concrete workspace
		// revision. Running the same validator after a successful edit is not a
		// replay of the earlier action: its input bytes have changed. Keep the
		// revision process-local so a crash still fails closed and requires the
		// durable retry authorization for any pre-crash action guard.
		if json.Valid(actionArguments) {
			actionArguments, _ = json.Marshal(struct {
				WorkspaceRevision uint64          `json:"workspace_revision"`
				Arguments         json.RawMessage `json:"arguments"`
			}{WorkspaceRevision: revision, Arguments: json.RawMessage(actionArguments)})
		} else {
			actionArguments = []byte(fmt.Sprintf("workspace-revision=%d\x00%s", revision, call.RawArguments))
		}
		legacyActionArguments = []byte(fmt.Sprintf("workspace-revision=%d\x00%s", revision, call.RawArguments))
	}
	actionDigest := sessionrecovery.DigestAction(call.Tool, actionArguments)
	// Journals created before canonical JSON identities used the exact model
	// wire bytes. Check that legacy alias as well so upgrading cannot turn an
	// already guarded exact retry into a fresh action. An existing one-shot
	// authorization may still be consumed on its original digest.
	if effect != sessionrecovery.ToolReadOnly {
		legacyDigest := sessionrecovery.DigestActionLegacy(call.Tool, legacyActionArguments)
		if legacyDigest != actionDigest {
			legacyDecision := recorder.CheckRetry(legacyDigest, effect, b.currentTurnID())
			if !legacyDecision.Allowed {
				return sessionrecovery.ErrSameActionRetry
			}
			if legacyDecision.AuthorizationID != "" {
				actionDigest = legacyDigest
			}
		}
	}
	observedCallID := ""
	if call.InputVersion != "" {
		state := recorder.State()
		if guard, exists := state.ActionGuards[actionDigest]; exists {
			b.mu.Lock()
			prior, completedHere := b.completedInputCalls[guard.CallID]
			if completedHere && prior.observed && prior.turn < call.Turn && prior.inputVersion != call.InputVersion {
				observedCallID = guard.CallID
			}
			b.mu.Unlock()
		}
	}
	if recoveryForegroundShell(call.Tool, call.Kind, call.Args) {
		state := recorder.State()
		if guard, exists := state.ActionGuards[actionDigest]; exists {
			b.mu.Lock()
			prior, completedHere := b.completedShellCalls[guard.CallID]
			if completedHere && prior.observed && prior.turn < call.Turn && prior.workspaceRevision < b.workspaceRevision {
				observedCallID = guard.CallID
			}
			b.mu.Unlock()
		}
	}
	_, err = recorder.RecordToolCall(sessionrecovery.ToolCallRequest{
		CallID: call.CallID, TurnID: b.currentTurnID(), Name: call.Tool,
		ActionDigest: actionDigest,
		Effect:       effect, Arguments: &arguments,
		ObservedCallID: observedCallID,
		InputVersion:   call.InputVersion,
	})
	return err
}

func (b *sessionRecoveryBridge) permission(event PermissionLifecycle) error {
	recorder, err := b.requireRecorder()
	if err != nil || recorder == nil {
		return err
	}
	requestID := "permission-" + event.CallID
	if event.Decision == PermissionRequested {
		_, err = recorder.RequestPermission(sessionrecovery.PermissionRequest{
			RequestID: requestID, TurnID: b.currentTurnID(), CallID: event.CallID,
			Scope: recoveryPermissionScope(event.Kind), Reason: "tool authorization required",
		})
		return err
	}
	decision := sessionrecovery.PermissionDenied
	if event.Authorization.Allowed {
		decision = sessionrecovery.PermissionApproved
	}
	_, err = recorder.ResolvePermission(sessionrecovery.PermissionResolution{
		RequestID: requestID, Decision: decision, Reason: "authorization decision recorded",
	})
	return err
}

func (b *sessionRecoveryBridge) afterToolComplete(execution ToolExecution) error {
	recorder, err := b.requireRecorder()
	if err != nil || recorder == nil {
		return err
	}
	outcome := sessionrecovery.ExecutionFailedBeforeEffect
	if execution.Executed {
		switch {
		case execution.NoChange || execution.Effect == EffectNone:
			outcome = sessionrecovery.ExecutionNoChange
		case execution.Status == ToolOK:
			outcome = sessionrecovery.ExecutionSucceeded
		case recoveryFailureBeforeMutation(execution):
			// The tool function ran, but this typed contract is emitted before
			// its first write. Durable recovery must clear the prepared-action
			// guard so the model can satisfy the stated prerequisite and retry
			// the exact saved draft once. Treating this as effect-unknown makes
			// the safe edit -> read_file -> same edit sequence impossible.
			outcome = sessionrecovery.ExecutionFailedBeforeEffect
		case recoveryToolCallEffect(execution.Tool, execution.Kind, execution.Effect) == sessionrecovery.ToolReadOnly:
			outcome = sessionrecovery.ExecutionFailedBeforeEffect
		default:
			outcome = sessionrecovery.ExecutionEffectUnknown
		}
	}
	if recoveryToolCallEffect(execution.Tool, execution.Kind, execution.Effect) != sessionrecovery.ToolReadOnly &&
		(outcome == sessionrecovery.ExecutionSucceeded || outcome == sessionrecovery.ExecutionEffectUnknown) {
		b.mu.Lock()
		b.mutationObserved = true
		b.mu.Unlock()
	}
	if _, err = recorder.RecordToolExecution(sessionrecovery.ToolExecutionRequest{
		CallID: execution.CallID, Outcome: outcome,
		ProcessExited: execution.ProcessExited,
		Summary:       "tool execution " + string(execution.Status),
	}); err != nil {
		return err
	}
	output := sessionrecovery.ReferenceAuto([]byte(execution.LLMContent), "model-facing tool result")
	_, err = recorder.RecordToolResult(sessionrecovery.ToolResultRequest{
		CallID: execution.CallID, Name: execution.Tool, Output: &output,
		Summary: "tool result " + string(execution.Status),
	})
	if err == nil && execution.Executed && execution.Status == ToolOK && execution.Effect == EffectWrite && !execution.NoChange {
		b.mu.Lock()
		b.workspaceRevision++
		b.mu.Unlock()
	}
	if err == nil && execution.Executed && execution.Status == ToolOK && execution.InputVersion != "" {
		b.mu.Lock()
		if b.completedInputCalls == nil {
			b.completedInputCalls = make(map[string]observedInputCall)
		}
		b.completedInputCalls[execution.CallID] = observedInputCall{turn: execution.Turn, inputVersion: execution.InputVersion, outputHash: sha256.Sum256([]byte(execution.LLMContent))}
		b.mu.Unlock()
	}
	if err == nil && execution.Executed && execution.ProcessExited &&
		recoveryForegroundShell(execution.Tool, execution.Kind, execution.Args) &&
		(execution.Status == ToolOK || execution.Status == ToolError) {
		b.mu.Lock()
		if b.completedShellCalls == nil {
			b.completedShellCalls = make(map[string]observedShellCall)
		}
		// Record the revision after this command's own writes. Only a later
		// successful edit can make another invocation eligible.
		b.completedShellCalls[execution.CallID] = observedShellCall{
			turn: execution.Turn, workspaceRevision: b.workspaceRevision,
		}
		b.mu.Unlock()
	}
	return err
}

func recoveryForegroundShell(name string, kind tools.Kind, args map[string]any) bool {
	if name != DefaultShellTool || kind != tools.KindExecute || args == nil {
		return false
	}
	background, _ := args["background"].(bool)
	pty, _ := args["pty"].(bool)
	return !background && !pty
}

// observeModelRequest records only actual model-facing results from this live
// run. Recovered journal entries cannot grant this permission, nor can a tool
// argument claim that its previous execution was observed or safe to repeat.
func (b *sessionRecoveryBridge) observeModelRequest(messages []provider.Message) {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, message := range messages {
		if message.Role != "tool" {
			continue
		}
		if prior, exists := b.completedShellCalls[message.ToolCallID]; exists {
			prior.observed = true
			b.completedShellCalls[message.ToolCallID] = prior
		}
		if prior, exists := b.completedInputCalls[message.ToolCallID]; exists && prior.outputHash == sha256.Sum256([]byte(message.Content)) {
			prior.observed = true
			b.completedInputCalls[message.ToolCallID] = prior
		}
	}
}

// recoveryFailureBeforeMutation is deliberately an allowlist. RecoveryError
// is also used by workflows whose failure may occur after an earlier output
// was saved (for example a PDF export), so type identity alone is not proof of
// a zero-effect failure. These codes are emitted by file tools before their
// atomic write boundary and are therefore safe to retry without an operator
// authorization after the model satisfies the reported prerequisite.
func recoveryFailureBeforeMutation(execution ToolExecution) bool {
	if !execution.Executed || execution.Status == ToolOK || execution.Err == nil {
		return false
	}
	var recovery *tools.RecoveryError
	if !errors.As(execution.Err, &recovery) || recovery == nil {
		return false
	}
	switch strings.TrimSpace(recovery.Code) {
	case "prior_read_required", "prior_read_stale", "edit_file_not_found",
		"edit_no_change", "edit_old_string_not_found", "FILE_EXISTS":
		return true
	default:
		return false
	}
}

func recoveryValidationTracksWorkspaceRevision(call ToolCheckpointCall) bool {
	if strings.TrimSpace(call.Tool) != DefaultShellTool || call.Args == nil || call.Kind != tools.KindExecute {
		return false
	}
	purpose, _ := call.Args["purpose"].(string)
	if !strings.EqualFold(strings.TrimSpace(purpose), "validation") {
		return false
	}
	if background, _ := call.Args["background"].(bool); background {
		return false
	}
	if script, _ := call.Args["script"].(string); strings.TrimSpace(script) != "" {
		return false
	}
	command, _ := call.Args["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "\r\n`") || strings.Contains(command, "$(") {
		return false
	}
	// A trailing stderr-to-stdout merge only changes presentation. Strip it
	// before rejecting all other redirection and shell composition.
	command = stripValidatorOutputMerge(command)
	if strings.ContainsAny(command, "<>") || strings.Contains(command, "||") {
		return false
	}
	body := command
	separator := ""
	switch {
	case strings.Count(command, "&&") == 1:
		separator = "&&"
	case strings.Count(command, ";") == 1:
		separator = ";"
	case strings.Contains(command, "&&") || strings.Contains(command, ";"):
		return false
	}
	if separator != "" {
		parts := strings.SplitN(command, separator, 2)
		if len(parts) != 2 || !recoveryWorkspaceCDHop(parts[0]) {
			return false
		}
		body = strings.TrimSpace(parts[1])
	}
	if body == "" || strings.ContainsAny(body, ";&|<>") ||
		strings.ContainsAny(body, "`") || strings.Contains(body, "$(") {
		return false
	}
	return looksLikeValidationCommand(body)
}

func recoveryWorkspaceCDHop(command string) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) != 2 || fields[0] != "cd" {
		return false
	}
	return strings.Trim(fields[1], `"'`) == "/workspace"
}

func (b *sessionRecoveryBridge) recordSyntheticTool(call provider.ToolCall, turn int, output string) error {
	return b.recordSyntheticToolOutcome(call, turn, tools.KindReadOnly, ToolOK, true, output)
}

func (b *sessionRecoveryBridge) recordSyntheticToolOutcome(
	call provider.ToolCall,
	turn int,
	kind tools.Kind,
	status ToolStatus,
	executed bool,
	output string,
) error {
	checkpoint := ToolCheckpointCall{
		Turn: turn, CallID: call.ID, Tool: call.Function.Name,
		RawArguments: call.Function.Arguments, Kind: kind,
	}
	if err := b.beforeToolCall(checkpoint); err != nil {
		return err
	}
	return b.afterToolComplete(ToolExecution{
		Turn: turn, CallID: call.ID, Tool: call.Function.Name,
		Kind: kind, Status: status, Effect: EffectForKind(kind),
		Executed: executed, LLMContent: output,
	})
}

func (b *sessionRecoveryBridge) queueGuidance(guidance Guidance) error {
	recorder, err := b.requireRecorder()
	if err != nil || recorder == nil {
		return err
	}
	content := sessionrecovery.ReferenceAuto([]byte(guidance.Text), "queued guidance")
	_, err = recorder.QueueGuidance(sessionrecovery.GuidanceRequest{
		GuidanceID: guidance.ID, TurnID: b.currentTurnID(), Content: &content,
	})
	return err
}

func (b *sessionRecoveryBridge) applyGuidance(guidance Guidance) error {
	recorder, err := b.requireRecorder()
	if err != nil || recorder == nil {
		return err
	}
	_, err = recorder.ResolveGuidance(sessionrecovery.GuidanceResolution{
		GuidanceID: guidance.ID, Status: sessionrecovery.GuidanceApplied,
		Reason: "injected at safe model boundary",
	})
	return err
}

func (b *sessionRecoveryBridge) recordAttachments(input RunInput) error {
	recorder, err := b.requireRecorder()
	if err != nil || recorder == nil {
		return err
	}
	for _, metadata := range input.Attachments {
		if _, err := recorder.RecordAttachment(metadata); err != nil {
			return err
		}
	}
	for _, part := range input.User.ContentParts {
		if part.ImageURL == nil {
			continue
		}
		digest := sha256.Sum256([]byte(part.ImageURL.URL))
		mediaType := "image"
		if parsed, parseErr := url.Parse(part.ImageURL.URL); parseErr == nil && parsed.Scheme == "data" {
			if boundary := strings.Index(parsed.Opaque, ";"); boundary > 0 {
				mediaType = parsed.Opaque[:boundary]
			}
		}
		if _, err := recorder.RecordAttachment(sessionrecovery.AttachmentMetadata{
			AttachmentID: "attachment-" + hex.EncodeToString(digest[:12]),
			Class:        sessionrecovery.AttachmentUserUpload,
			OriginalName: "image", MediaType: mediaType,
			Authorized: false, Available: false,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *sessionRecoveryBridge) recordRecap(recap *RunRecap, stop StopReason) error {
	if recap == nil {
		return nil
	}
	recorder, err := b.requireRecorder()
	if err != nil || recorder == nil {
		return err
	}
	encoded, err := json.Marshal(recap)
	if err != nil {
		return err
	}
	reference := sessionrecovery.ReferenceAuto(encoded, recap.String())
	_, err = recorder.RecordRecap(sessionrecovery.RecapRequest{
		RunID: b.runID, Status: recap.Status, StopReason: string(stop), Summary: &reference,
	})
	return err
}

func (b *sessionRecoveryBridge) mustFailClosed() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mutationObserved
}

func (b *sessionRecoveryBridge) currentTurnID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeTurnID
}

func (b *sessionRecoveryBridge) resumeInstruction() string {
	if b == nil || b.plan == nil || b.plan.Mode == sessionrecovery.ResumeNoAction {
		return ""
	}
	plan := b.plan
	var lines []string
	lines = append(lines, "Durable recovery plan mode: "+string(plan.Mode)+".")
	if len(plan.CompletedToolCallIDs) > 0 {
		lines = append(lines, "Reuse these completed tool call results; do not replay them: "+strings.Join(plan.CompletedToolCallIDs, ", ")+".")
	}
	if len(plan.ReviewRequired) > 0 {
		lines = append(lines, "One or more mutation effects require external-state review. Do not retry the same action without explicit authorization.")
	}
	if len(plan.PendingPermissions) > 0 {
		lines = append(lines, "Wait for the restored pending permission decisions before protected execution.")
	}
	return strings.Join(lines, " ")
}

func recoveryToolEffect(kind tools.Kind) sessionrecovery.ToolEffect {
	if kind == tools.KindReadOnly {
		return sessionrecovery.ToolReadOnly
	}
	if kind == "" {
		return sessionrecovery.ToolUnknown
	}
	return sessionrecovery.ToolMutation
}

func recoveryToolCallEffect(tool string, kind tools.Kind, effect Effect) sessionrecovery.ToolEffect {
	if kind == tools.KindExecute && effect == EffectRead {
		return sessionrecovery.ToolReadOnly
	}
	return recoveryToolEffectFor(tool, kind)
}

func recoveryToolEffectFor(tool string, kind tools.Kind) sessionrecovery.ToolEffect {
	// Browser validators execute a local browser contract, but they do not
	// mutate the workspace artifact. Treating a visual or interaction failure
	// as an effect-unknown mutation prevents the normal create -> validate ->
	// repair -> revalidate loop and produces a false explicit-authorization
	// checkpoint. Browser state is disposable evidence; the saved artifact is
	// the authoritative durable state.
	switch strings.TrimSpace(tool) {
	case "meadow_browser_dom", "meadow_browser_visual_check", "meadow_browser_interaction_check", "meadow_browser_suite", "meadow_browser_inspect":
		// Suite run/status share the local validation contract. Suite reports are
		// reproducible evidence, not the user's application mutation; edits to
		// the suite or artifact must be followed by the same verification call.
		// inspect shares the disposable local-page contract, including setup.
		// This is checkpoint retry classification, not an authorization downgrade
		// or a claim that arbitrary page JavaScript has no network side effects.
		return sessionrecovery.ToolReadOnly
	}
	return recoveryToolEffect(kind)
}

func recoveryPermissionScope(kind tools.Kind) string {
	value := strings.TrimSpace(string(kind))
	if value == "" {
		return "unknown"
	}
	return value
}

func recoveryUniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
