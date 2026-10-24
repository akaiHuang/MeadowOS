// Session resume + chat persistence (M6: the desktop sidebar opens a
// stored chat as a live session, and serve-mode turns land in ~/.meadow
// chats exactly like the CLI's — same JSONL format, same files, so
// GET /sessions and the TS CLI's /resume both see them).
package serve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/sessionrecovery"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

func loadSharedRecovery(chatID string) (sessionrecovery.ResumePlan, sessionrecovery.State, error) {
	plan, state, _, err := loadSharedRecoveryEvidence(chatID)
	return plan, state, err
}

func loadSharedRecoveryEvidence(chatID string) (sessionrecovery.ResumePlan, sessionrecovery.State, bool, error) {
	root := filepath.Join(session.PrimaryDir(), "recovery", "harness")
	recovered, err := sessionrecovery.Recover(root, chatID, sessionrecovery.DefaultOptions())
	plan := sessionrecovery.BuildResumePlan(recovered)
	if err != nil && !recovered.Corrupt {
		return plan, recovered.State, false, err
	}
	return plan, recovered.State, err == nil && !recovered.Corrupt, nil
}

// handleResume attaches to an existing live session for the durable chat when
// one exists, otherwise it opens a new live session preloaded from storage.
//
// Contract: POST /session/resume {cwd, chatId} →
// {sessionId, chatId, messages:[{role:"user"|"assistant", content}]};
// 404 when the chat does not exist. The loaded history becomes the
// engine's context (the next prompt's model call sees every old turn) and
// new turns append to the original chat file — sessionId is the fresh
// live handle, chatId stays the durable thread identity.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.wakeRestoreMu.Lock()
	defer s.wakeRestoreMu.Unlock()
	var body struct {
		CWD                 string `json:"cwd"`
		ChatID              string `json:"chatId"`
		ContinueInterrupted *bool  `json:"continueInterrupted,omitempty"`
		ApprovalChannel     string `json:"approvalChannel"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	channel, err := parseApprovalChannel(body.ApprovalChannel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cwd := body.CWD
	if cwd == "" {
		cwd = s.WorkspaceCwd
	}
	if !strings.HasPrefix(cwd, "/") {
		writeErr(w, http.StatusBadRequest, "cwd must be absolute")
		return
	}
	if strings.TrimSpace(body.ChatID) == "" {
		writeErr(w, http.StatusBadRequest, "chatId required")
		return
	}
	// The API historically treated POST /session/resume as an explicit request
	// to continue a crashed turn. Keep that contract for callers which omit the
	// new field, while allowing Desktop thread navigation to open a transcript
	// passively. Merely viewing an old chat must not invoke a model, replay a
	// failed tool, start a persisted queue, or change resident-model lifecycle.
	continueInterrupted := body.ContinueInterrupted == nil || *body.ContinueInterrupted

	infos, err := session.ListSessions(cwd)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := ""
	for _, si := range infos {
		if si.SessionID == body.ChatID {
			path = si.Path
			break
		}
	}
	if path == "" {
		writeErr(w, http.StatusNotFound, "unknown chat: "+body.ChatID)
		return
	}
	if err := session.CheckConnectionID(path, s.ConnectionID); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	// A durable scheduler may not have any due occurrence yet. Its original
	// chat still has exactly one owner across repeated passive navigation.
	wakeOwner, ownerErr := loadWakeDocument(wakeDocumentPath(BackgroundWakeRequest{ChatID: body.ChatID, WorkspaceCWD: cwd, ConnectionID: s.ConnectionID}))
	hasWakeOwner := s.BackgroundWakeEnabled && ownerErr == nil
	savedModel, err := session.LoadLastAssistantModel(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.ConnectionID != "" && (savedModel == "" || s.ResumeModelAllowed == nil || !s.ResumeModelAllowed(savedModel)) {
		writeErr(w, http.StatusConflict, "session_model_binding_unavailable: select the original model connection or start a new chat")
		return
	}
	history, err := session.LoadHistory(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	journalRecovery, err := session.LoadRunRecovery(path)
	harnessRecoveryStatus := "journal"
	if errors.Is(err, session.ErrInvalidHarnessRunIdentity) && !continueInterrupted {
		// Passive history inspection never dispatches a runner. Keep malformed
		// identity visible without guessing its mode or rewriting the journal.
		harnessRecoveryStatus = "unavailable"
		journalRecovery = session.RunRecovery{}
	} else if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	} else if journalRecovery.HarnessMode == "" {
		harnessRecoveryStatus = "legacy-model-family"
	}
	resumePlan, recoveryState, recoveryEvidenceTrusted, err := loadSharedRecoveryEvidence(body.ChatID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Sprintf("load durable recovery plan: %v", err))
		return
	}
	displayHistory, err := session.LoadDisplayHistory(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	displayMessages, err := session.LoadDisplayMessages(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	decorateDisplayAttachments(cwd, body.ChatID, displayMessages)

	// A chat can keep running after Desktop switches away and closes its SSE.
	// Reusing the original live session is essential: its event ring, runtime,
	// cancellation handle and partial output are all owned by that session.
	s.mu.Lock()
	var attached *serveSession
	if preferred, hasPreferred := preferredResumeSessionID(r.Context()); hasPreferred {
		candidate := s.sessions[preferred.id]
		if (candidate == nil && !preferred.allowMissing) || (candidate != nil && (candidate.cwd != cwd || candidate.queueKey != body.ChatID)) {
			s.mu.Unlock()
			writeErr(w, http.StatusConflict, "preferred_resume_session_unavailable")
			return
		}
		// The commercial binding owns this live handle even while idle. A
		// repeated resume must not replace it while a prompt is being admitted.
		attached = candidate
	}
	if attached == nil {
		for _, candidate := range s.sessions {
			candidate.mu.Lock()
			isRunning := candidate.running || candidate.activeHarnessMode != nil || candidate.wakeStarted || (hasWakeOwner && candidate.connectionID == wakeOwner.ConnectionID)
			candidate.mu.Unlock()
			if candidate.cwd == cwd && candidate.queueKey == body.ChatID && isRunning {
				attached = candidate
				break
			}
		}
	}
	s.mu.Unlock()
	if attached != nil {
		// An idle background owner can finish between the first file read and the
		// SSE cursor snapshot. Pin its execution boundary while refreshing transcript.
		if attached.promptMu.TryLock() {
			defer attached.promptMu.Unlock()
			displayHistory, err = session.LoadDisplayHistory(path)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			displayMessages, err = session.LoadDisplayMessages(path)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			decorateDisplayAttachments(cwd, body.ChatID, displayMessages)
		}
		if attached.effectiveApprovalChannel() != channel {
			writeErr(w, http.StatusConflict, "approval_channel_mismatch")
			return
		}
		attached.mu.Lock()
		running := attached.running || attached.activeHarnessMode != nil
		replayAfter := attached.runReplayAfterEventID
		lastEventID := attached.nextEventID
		attached.mu.Unlock()
		writeResumeResponse(w, attached, body.ChatID, displayHistory, displayMessages, true, running, replayAfter, lastEventID, &resumePlan, recoveryEvidenceTrusted)
		return
	}
	// A saved approval is still awaiting a human decision. Do not recreate a
	// card with no recipient, wait indefinitely, or fabricate a user vote.
	// The caller can reopen interactively without losing the durable request.
	if continueInterrupted && channel == approvalChannelNone && len(resumePlan.PendingPermissions) > 0 {
		writeErr(w, http.StatusConflict, "approval_channel_unavailable")
		return
	}

	eng, err := s.NewEngine(cwd)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hasWakeOwner {
		if _, modeErr := engine.ParseApprovalMode(string(wakeOwner.EngineMode)); modeErr != nil || channel != wakeOwner.ApprovalChannel {
			writeErr(w, http.StatusConflict, "background_wake_owner_configuration_changed")
			return
		}
		eng.Mode = wakeOwner.EngineMode
		eng.Registry = eng.Registry.Excluding(wakeOwner.DisabledTools)
	}
	// Rebuild turn boundaries from user-role indices. Safe assumption for
	// resume: stored chats carry exactly one user record per turn — steers
	// and nudges are never persisted (see persistTurn) — so unlike the live
	// history, user role == turn start with no mid-turn pollution.
	var turnMarks []int
	for i, m := range history {
		if m.Role == "user" {
			turnMarks = append(turnMarks, i)
		}
	}

	// Continue the original chat file (append); new turns keep the thread.
	chat, err := session.OpenChatWriter(cwd, body.ChatID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lastPromptTokens, err := session.LoadLastPromptTokens(path)
	if err != nil {
		_ = chat.Close()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess := &serveSession{
		id: session.NewUUID(), cwd: cwd, engine: eng,
		connectionID:             s.ConnectionID,
		indexAttachmentDocuments: s.IndexAttachmentDocuments,
		subscribers:              map[chan event]struct{}{},
		permissions:              map[string]chan engine.Decision{},
		restoredPermissions:      map[string]sessionrecovery.PermissionCheckpoint{},
		restoredPermissionCalls:  map[string]sessionrecovery.ToolCallCheckpoint{},
		restoredApprovalActions:  map[string]int{},
		approveAlways:            map[string]bool{},
		approvalChannel:          channel,
		userInputs:               map[string]pendingUserInput{},
		history:                  history,
		turnMarks:                turnMarks,
		chat:                     chat,
		lastSeen:                 time.Now(),
		queueKey:                 body.ChatID,
		thinkingMode:             provider.ThinkingAuto,
		reasoningEffort:          provider.ReasoningEffortMedium,
		lastPromptTokens:         lastPromptTokens,
		recoveryPlan:             &resumePlan,
		appliedGuidance:          map[string]bool{},
		defaultHarnessMode:       s.DefaultHarnessMode,
		conversationInboxEnabled: s.ConversationInboxEnabled,
		resolveHarnessMode:       s.ResolveHarnessMode,
		harnessRecoveryStatus:    harnessRecoveryStatus,
	}
	if hasWakeOwner {
		sess.disabledTools = append([]string(nil), wakeOwner.DisabledTools...)
	}
	sess.restoreTaskPlanSnapshot(displayHistory)
	restoreSessionHarnessControlsLocked(sess)
	if savedModel != "" && s.ResumeModelAllowed != nil && s.ResumeModelAllowed(savedModel) {
		bindSessionModelLocked(sess, savedModel)
	} else {
		// Legacy or unavailable historical models retain the actual current
		// engine binding, which the response exposes instead of a stale label.
		restoreSessionInferenceControlsLocked(sess)
	}
	if err := sess.loadQueue(); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("load background queue: %v", err))
		return
	}
	if err := sess.loadWorkCenter(); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("load work center: %v", err))
		return
	}
	recoveryCandidate := journalRecovery.Interrupted || resumePlan.Mode != sessionrecovery.ResumeNoAction
	recoveryNeeded := continueInterrupted && recoveryCandidate
	// Historical runs without execution metadata retain the original model
	// family path. A newly saved preference or deployment override must never
	// silently migrate an interrupted legacy run into Raven.
	recoveredHarnessMode := sess.modelFamilyHarnessModeLocked()
	if journalRecovery.HarnessMode != "" {
		recoveredHarnessMode = journalRecovery.HarnessMode
	}
	// Same Asker wiring as handleCreateSession: questions broadcast as
	// SSE, answers arrive over the user-input endpoint.
	if t, ok := eng.Registry.Get("ask_user_question"); ok {
		if ask, isAsk := t.(*tools.AskUser); isAsk {
			ask.Ask = sess.askViaEvents
		}
	}
	sess.bindLocationTool()
	sess.bindAttachmentImageTools()
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	if recoveryNeeded {
		restorePendingPermissionCards(sess, resumePlan, recoveryState)
	}
	recoveryBlocked := resumePlan.Mode == sessionrecovery.ResumeManualReview || resumePlan.Mode == sessionrecovery.ResumeBlockedCorrupt

	if recoveryBlocked {
		if recoveryNeeded {
			recoveryPlanCallback(sess)(resumePlan)
			sess.broadcast("run_state", map[string]any{"sessionId": sess.id, "chatId": sess.queueKey, "status": "blocked", "resumeMode": resumePlan.Mode})
		}
	} else if recoveryNeeded {
		ctx, cancel := context.WithCancel(context.Background())
		sess.mu.Lock()
		sess.running = true
		sess.activeInferenceControls = snapshotSessionInferenceControlsLocked(sess)
		sess.admitHarnessModeLocked(recoveredHarnessMode)
		sess.cancelRun = cancel
		sess.mu.Unlock()
		go s.resumeInterruptedRun(sess, ctx, cancel)
	} else if continueInterrupted {
		s.warmPrefixCacheOnOpen(sess)
		go s.startQueuePump(sess)
	}

	writeResumeResponse(w, sess, body.ChatID, displayHistory, displayMessages, false, recoveryNeeded && !recoveryBlocked, sess.nextEventID, sess.nextEventID, &resumePlan, recoveryEvidenceTrusted)
}

func restorePendingPermissionCards(sess *serveSession, plan sessionrecovery.ResumePlan, state sessionrecovery.State) {
	for _, permission := range plan.PendingPermissions {
		ch := make(chan engine.Decision, 1)
		sess.permissions[permission.RequestID] = ch
		sess.restoredPermissions[permission.RequestID] = permission
		if call, ok := state.ToolCalls[permission.CallID]; ok {
			sess.restoredPermissionCalls[permission.RequestID] = call
		}
		options := []map[string]any{{"optionId": "approve", "name": "允許", "kind": "allow_once"}, {"optionId": "reject", "name": "拒絕", "kind": "reject_once"}}
		title := permission.Scope
		if call, ok := state.ToolCalls[permission.CallID]; ok && call.Name != "" {
			title = call.Name
		}
		sess.broadcast("permission_request", map[string]any{
			"requestId": permission.RequestID, "sessionId": sess.id, "restored": true,
			"toolCall": map[string]any{"title": title, "kind": permission.Scope, "callId": permission.CallID},
			"options":  options,
		})
	}
	// A user approval may itself have reached the durable journal just before
	// another process crash. Rehydrate only unconsumed authorizations created by
	// the atomic restored-permission path; general/operator retry grants do not
	// bypass the normal Desktop confirmation UI.
	for digest, authorization := range state.RetryAuthorizations {
		guard, guarded := state.ActionGuards[digest]
		call, callExists := state.ToolCalls[authorization.SourceCallID]
		if authorization.ConsumedBy != "" || authorization.SourcePermissionID == "" ||
			authorization.SourceCallID == "" || authorization.TurnID == "" ||
			!strings.HasPrefix(authorization.ApprovedBy, "user-session:") || !guarded ||
			guard.CallID != authorization.SourceCallID || !callExists ||
			call.TurnID != authorization.TurnID || plan.TurnID != authorization.TurnID {
			continue
		}
		sess.restoredApprovalActions[digest] = 1
	}
}

func (sess *serveSession) awaitRestoredPermissions(ctx context.Context) error {
	sess.mu.Lock()
	ids := make([]string, 0, len(sess.restoredPermissions))
	channels := map[string]chan engine.Decision{}
	for id := range sess.restoredPermissions {
		ids = append(ids, id)
		channels[id] = sess.permissions[id]
	}
	sess.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	sort.Strings(ids)
	for _, id := range ids {
		ch := channels[id]
		if ch == nil {
			continue
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		// Stop resolves outstanding cards before cancelling the same owned
		// context. A ready card cannot make a cancelled recovery runnable.
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func resolveRestoredPermission(sess *serveSession, requestID string, decision engine.Decision) error {
	sess.mu.Lock()
	_, restored := sess.restoredPermissions[requestID]
	call := sess.restoredPermissionCalls[requestID]
	sess.mu.Unlock()
	if !restored {
		return nil
	}
	root := filepath.Join(session.PrimaryDir(), "recovery", "harness")
	store, _, err := sessionrecovery.Open(root, sess.queueKey, sessionrecovery.DefaultOptions())
	if err != nil {
		return err
	}
	durableDecision := sessionrecovery.PermissionDenied
	if decision == engine.DecisionApprove || decision == engine.DecisionApproveAlways {
		durableDecision = sessionrecovery.PermissionApproved
	}
	resolution := sessionrecovery.PermissionResolution{
		RequestID: requestID, Decision: durableDecision, Reason: "restored permission decision",
	}
	if durableDecision == sessionrecovery.PermissionApproved && call.ActionDigest != "" {
		identity := sha256.Sum256([]byte(sess.queueKey + "\x00" + requestID + "\x00" + call.CallID + "\x00" + call.ActionDigest))
		resolution.RetryAuthorizationID = fmt.Sprintf("retry-%x", identity[:12])
		resolution.RetryApprovedBy = "user-session:" + sess.id
		resolution.RetryReason = "user explicitly approved the restored permission request"
	}
	_, err = store.ResolvePermission(resolution)
	if err == nil {
		err = store.SaveSnapshot()
	}
	err = errors.Join(err, store.Close())
	if err != nil {
		return err
	}
	sess.mu.Lock()
	delete(sess.restoredPermissions, requestID)
	delete(sess.restoredPermissionCalls, requestID)
	if durableDecision == sessionrecovery.PermissionApproved && call.ActionDigest != "" {
		sess.restoredApprovalActions[call.ActionDigest]++
	}
	sess.mu.Unlock()
	return nil
}

func resumePlanInstruction(plan *sessionrecovery.ResumePlan) string {
	if plan == nil {
		return ""
	}
	parts := []string{"Durable resume decision: " + string(plan.Mode) + "."}
	if len(plan.CompletedToolCallIDs) > 0 {
		parts = append(parts, "Reuse completed tool results and do not replay call IDs: "+strings.Join(plan.CompletedToolCallIDs, ", ")+".")
	}
	if len(plan.RetryableToolCalls) > 0 {
		parts = append(parts, "Only explicitly listed read-only calls may be retried.")
	}
	if len(plan.ReviewRequired) > 0 {
		parts = append(parts, "Mutation effects require manual external-state review; do not retry them.")
	}
	return strings.Join(parts, " ")
}

func (s *Server) resumeInterruptedRun(sess *serveSession, ctx context.Context, cancel context.CancelFunc) {
	turnStarted := time.Now()
	sess.promptMu.Lock()
	defer sess.promptMu.Unlock()
	defer cancel()
	stop := "error"
	var guidanceIDs []string
	// Admission installs cancellation ownership before this goroutine starts.
	// Every exit settles that ownership before another turn acquires promptMu.
	defer func() {
		stop = sess.settleForegroundExecution(ctx, stop)
		sess.mu.Lock()
		runID := sess.activeRunID
		checkpointID := sess.stableCheckpointID
		checkpointErr := sess.stableCheckpointErr
		sess.mu.Unlock()
		if checkpointErr != nil {
			stop = "error"
		}
		if stop == "error" {
			sess.restoreRecoveredGuidance(guidanceIDs)
		}
		if checkpointErr == nil && sess.chat != nil {
			var terminalErr error
			if stop == "error" || isRunBudgetStop(stop) {
				terminalErr = sess.chat.RunInterrupted(stop, checkpointID)
			} else {
				terminalErr = sess.chat.RunFinishedCheckpoint(stop, checkpointID)
			}
			if terminalErr != nil {
				stop = "error"
				sess.broadcast("stream_error", map[string]any{"message": terminalErr.Error()})
			}
		}
		sess.mu.Lock()
		sess.cancelRun = nil
		if sess.activeRunID == runID {
			sess.activeRunID = ""
		}
		sess.activeRunCheckpointID = ""
		sess.running = false
		sess.activeInferenceControls = nil
		sess.activeHarnessMode = nil
		sess.mu.Unlock()
		sess.broadcast("turn_completed", map[string]any{
			"sessionId": sess.id, "origin": "recovery", "stopReason": stop, "checkpointId": checkpointID,
			"runId":     runID,
			"elapsedMs": time.Since(turnStarted).Milliseconds(),
		})
		sess.broadcast("run_state", map[string]any{
			"sessionId": sess.id, "chatId": sess.queueKey, "runId": runID, "status": "completed", "stopReason": stop,
		})
		if stop == "end_turn" || stop == "completed" {
			go s.startQueuePump(sess)
		}
	}()
	if err := sess.awaitRestoredPermissions(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			stop = "cancelled"
		}
		return
	}
	sess.mu.Lock()
	mark := len(sess.history)
	sess.turnMarks = append(sess.turnMarks, mark)
	sess.mu.Unlock()
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			stop = "cancelled"
		}
		return
	}
	sess.broadcast("run_state", map[string]any{
		"sessionId": sess.id, "chatId": sess.queueKey, "status": "running", "recovered": true,
	})
	msg := provider.Message{Role: "user", Content: "Continue the interrupted task from the newest stable transcript checkpoint. This is request-level recovery, not token-level continuation. Inspect current external state, do not repeat completed tool actions, and finish the original request. " + resumePlanInstruction(sess.recoveryPlan)}
	var guidanceErr error
	msg, guidanceIDs, guidanceErr = sess.prepareRecoveredGuidanceContext(ctx, msg)
	if guidanceErr != nil {
		if errors.Is(guidanceErr, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
			stop = "cancelled"
			return
		}
		sess.failStableCheckpoint(guidanceErr)
		return
	}
	runCheckpointID := session.StableCheckpointIdentity(sess.queueKey, append(append([]provider.Message(nil), sess.history...), msg))
	sess.mu.Lock()
	sess.activeRunCheckpointID = runCheckpointID
	sess.stableCheckpointErr = nil
	sess.mu.Unlock()
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			stop = "cancelled"
		}
		return
	}
	stop = s.runPromptMessage(ctx, sess, msg, mark, false, agentcore.RunModeAuto, false)
}

// writeResumeResponse returns dialogue plus the live attachment and replay
// facts Desktop needs to restore an in-flight task after a chat switch.
func writeResumeResponse(w http.ResponseWriter, sess *serveSession, chatID string, history []provider.Message, messages []session.DisplayMessage, attached, running bool, replayAfterEventID, lastEventID int64, evidencePlan *sessionrecovery.ResumePlan, evidenceTrusted bool) {
	sess.mu.Lock()
	checkpointID := sess.stableCheckpointID
	runID := sess.activeRunID
	modelID := sess.engine.ModelID
	harnessRecoveryStatus := sess.harnessRecoveryStatus
	plan := sess.recoveryPlan
	sess.mu.Unlock()
	restoreLiveThoughtState(messages, runID, attached && running)
	outputEvidence, recapEvidence, evidenceVerified := verifiedResumeEvidence(evidencePlan, evidenceTrusted)
	// Evidence has one presentation location. Keep the resume decision's
	// control metadata, but do not duplicate hydrated content in the payload.
	var presentationPlan *sessionrecovery.ResumePlan
	if plan != nil {
		planCopy := *plan
		planCopy.OutputEvidence = nil
		planCopy.RecapEvidence = nil
		presentationPlan = &planCopy
	}
	writeJSON(w, map[string]any{
		"sessionId":    sess.id,
		"modelId":      modelID,
		"connectionId": sess.connectionID,
		"chatId":       chatID,
		"runId":        runID,
		"messages":     messages,
		"attached":     attached, "running": running,
		"approvalChannel":               sess.effectiveApprovalChannel(),
		"harnessRecoveryIdentityStatus": harnessRecoveryStatus,
		"replayAfterEventId":            replayAfterEventID,
		"lastEventId":                   lastEventID,
		"backgroundWakes":               wakeStatusSnapshots(sess),
		"checkpointId":                  checkpointID,
		"resumeDecision":                presentationPlan,
		"output_evidence":               outputEvidence,
		"recap_evidence":                recapEvidence,
		"evidence_verified":             evidenceVerified,
		// Files the stored chat wrote — the desktop right panel re-seeds
		// its output list from this (live SSE only covers new turns).
		"files": historyFiles(history),
		// Observable tool facts restore Progress/Browser/Expert panels for
		// historical conversations. They do not expose hidden reasoning and do
		// not promote a recorded result into a new validation verdict.
		"tools": historyTools(history),
	})
}

const maxResumePresentationEvidenceBytes int64 = 64 << 20

func verifiedResumeEvidence(plan *sessionrecovery.ResumePlan, trusted bool) ([]sessionrecovery.RecoveredOutputEvidence, []sessionrecovery.RecoveredRecapEvidence, bool) {
	outputs := make([]sessionrecovery.RecoveredOutputEvidence, 0)
	recaps := make([]sessionrecovery.RecoveredRecapEvidence, 0)
	if !trusted || plan == nil || plan.Mode == sessionrecovery.ResumeBlockedCorrupt {
		return outputs, recaps, false
	}
	validContent := func(content, digest string, bytes int64) bool {
		if content == "" || bytes <= 0 || bytes > maxResumePresentationEvidenceBytes || int64(len([]byte(content))) != bytes {
			return false
		}
		sum := sha256.Sum256([]byte(content))
		return digest == fmt.Sprintf("%x", sum[:])
	}
	for _, evidence := range plan.OutputEvidence {
		if strings.TrimSpace(evidence.CallID) == "" || strings.TrimSpace(evidence.Name) == "" || evidence.At.IsZero() ||
			!validContent(evidence.Content, evidence.SHA256, evidence.Bytes) {
			return []sessionrecovery.RecoveredOutputEvidence{}, []sessionrecovery.RecoveredRecapEvidence{}, false
		}
		outputs = append(outputs, evidence)
	}
	for _, evidence := range plan.RecapEvidence {
		if strings.TrimSpace(evidence.RunID) == "" || strings.TrimSpace(evidence.Status) == "" || evidence.At.IsZero() ||
			!validContent(evidence.Content, evidence.SHA256, evidence.Bytes) {
			return []sessionrecovery.RecoveredOutputEvidence{}, []sessionrecovery.RecoveredRecapEvidence{}, false
		}
		recaps = append(recaps, evidence)
	}
	return outputs, recaps, true
}

// persistTurn appends one turn's new assistant/tool messages to the chat
// log (TS JSONL via session.ChatWriter). from is the turn's history mark.
// User-role messages inside the slice are steers or empty-turn nudges and are
// skipped here: primary prompts are written at prompt start, while accepted
// queued guidance is written immediately as its own durable guidance record.
// Best-effort by design: a failed append never fails the turn.
func (sess *serveSession) persistTurn(model string, from int, lastUsage *provider.Usage) error {
	if from < 0 || from > len(sess.history) {
		return fmt.Errorf("turn history boundary %d is outside transcript length %d", from, len(sess.history))
	}
	_, err := sess.persistMessages(model, sess.history, persistedOutputCount(sess.history[:from]), lastUsage)
	return err
}

// Only assistant/tool messages are appended by persistMessages. User prompts
// and guidance have their own durable write boundary; system messages are
// runtime context and are not persisted. Counting just these outputs makes the
// cursor stable across system injection/removal and repeated checkpoints,
// without comparing or deduplicating message text (which can repeat legally).
func persistedOutputCount(messages []provider.Message) int {
	count := 0
	for _, message := range messages {
		if message.Role == "assistant" || message.Role == "tool" {
			count++
		}
	}
	return count
}

// Keep confirmed prefix progress even if a later append or marker fails. A
// prior failure poisons further attempts in the same run: a failed Write/Sync
// may have committed bytes, so the failing record itself cannot be retried
// safely just because its successful prefix is known.
func persistCheckpointOutputBoundary(cursor *int, priorErr error, appendOutputs func(int) (int, error), commitMarker func() error) error {
	if priorErr != nil {
		return priorErr
	}
	next, err := appendOutputs(*cursor)
	*cursor = next
	if err != nil {
		return err
	}
	if commitMarker != nil {
		return commitMarker()
	}
	return nil
}

func (sess *serveSession) checkpointPersistenceError() error {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.stableCheckpointErr
}

// from and the returned cursor count assistant/tool outputs, not raw message
// positions. A shortened output transcript fails closed rather than silently
// replaying earlier messages or skipping a new tool result.
func (sess *serveSession) persistMessages(model string, msgs []provider.Message, from int, lastUsage *provider.Usage) (int, error) {
	if sess.chat == nil {
		return from, fmt.Errorf("durable chat writer is unavailable")
	}
	outputCount := persistedOutputCount(msgs)
	if from < 0 || from > outputCount {
		return from, fmt.Errorf("stable checkpoint output boundary %d is outside output count %d", from, outputCount)
	}
	lastAssistant := -1
	ordinal := 0
	for i, m := range msgs {
		if m.Role != "assistant" && m.Role != "tool" {
			continue
		}
		if ordinal >= from && m.Role == "assistant" {
			lastAssistant = i
		}
		ordinal++
	}
	next := 0
	for i, m := range msgs {
		if m.Role != "assistant" && m.Role != "tool" {
			continue
		}
		if next < from {
			next++
			continue
		}
		var err error
		switch m.Role {
		case "assistant":
			var messageUsage *provider.Usage
			if i == lastAssistant {
				messageUsage = lastUsage
			}
			err = sess.chat.AssistantMessage(model, m, messageUsage)
		case "tool":
			name := ""
			for _, prev := range msgs {
				for _, tc := range prev.ToolCalls {
					if tc.ID == m.ToolCallID {
						name = tc.Function.Name
					}
				}
			}
			err = sess.chat.ToolResult(m.ToolCallID, name, m.Content)
		}
		if err != nil {
			return next, err
		}
		next++
	}
	return next, nil
}

// A durable wake restores only a settled original chat. Interrupted model/tool
// execution remains repair-visible; restoring an observation never authorizes
// replaying a request whose effects may already have happened.
var errWakeRecoveryRequired = errors.New("original_chat_recovery_required")

func (s *Server) restoreWakeSession(ctx context.Context, doc *durableWakeDocument) (*serveSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.beginConfigurationAdmission() {
		return nil, errConfigurationLeaseActive
	}
	defer s.endConfigurationAdmission()
	if doc.ConnectionID != s.CurrentConnectionID() {
		return nil, errors.New("background_wake_connection_unavailable")
	}
	path, err := session.ChatPath(doc.WorkspaceCWD, doc.ChatID)
	if err != nil {
		return nil, errWakeRecoveryRequired
	}
	if err = session.CheckConnectionID(path, doc.ConnectionID); err != nil {
		return nil, err
	}
	savedModel, err := session.LoadLastAssistantModel(path)
	if err != nil {
		return nil, err
	}
	if savedModel != "" && savedModel != doc.ModelID {
		return nil, errWakeRecoveryRequired
	}
	if s.ResumeModelAllowed != nil && !s.ResumeModelAllowed(doc.ModelID) {
		return nil, errors.New("background_wake_model_unavailable")
	}
	if doc.ConnectionID != "" && s.ResumeModelAllowed == nil {
		return nil, errors.New("background_wake_model_binding_unavailable")
	}
	recovery, err := session.LoadRunRecovery(path)
	if err != nil {
		return nil, errWakeRecoveryRequired
	}
	plan, _, _, err := loadSharedRecoveryEvidence(doc.ChatID)
	if err != nil {
		return nil, err
	}
	if recovery.Interrupted || plan.Mode != sessionrecovery.ResumeNoAction || len(plan.PendingPermissions) > 0 {
		return nil, errWakeRecoveryRequired
	}
	history, err := session.LoadHistory(path)
	if err != nil {
		return nil, err
	}
	candidate, err := s.buildSession(doc.WorkspaceCWD, doc.ApprovalChannel, doc.DisabledTools)
	if err != nil {
		return nil, err
	}
	// Restore the latest explicitly admitted user policy, never a factory
	// default that may be broader than the original chat's saved charter.
	if _, modeErr := engine.ParseApprovalMode(string(doc.EngineMode)); modeErr != nil {
		return nil, errors.New("background_wake_approval_policy_unavailable")
	}
	candidate.engine.Mode = doc.EngineMode
	candidate.queueKey = doc.ChatID
	candidate.history = history
	candidate.queuedPrompts = nil

	for i, msg := range history {
		if msg.Role == "user" {
			candidate.turnMarks = append(candidate.turnMarks, i)
		}
	}
	bindSessionModelLocked(candidate, doc.ModelID)
	if err = candidate.loadQueue(); err != nil {
		return nil, err
	}
	if err = candidate.loadWorkCenter(); err != nil {
		return nil, err
	}
	candidate.lastPromptTokens, err = session.LoadLastPromptTokens(path)
	if err != nil {
		return nil, err
	}
	candidate.chat, err = candidate.openBoundChatWriter(doc.ChatID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, owner := range s.sessions {
		if owner.cwd == doc.WorkspaceCWD && queueChatID(owner) == doc.ChatID && owner.connectionID == doc.ConnectionID {
			s.mu.Unlock()
			_ = candidate.chat.Close()
			return owner, nil
		}
	}
	s.sessions[candidate.id] = candidate
	s.mu.Unlock()
	return candidate, nil
}
