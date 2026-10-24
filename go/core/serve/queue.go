package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	harnesscore "github.com/hey-meadow/meadow-code/go/core/harness"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

const maxQueueItems = 64

var (
	errQueueCommitUncertain = errors.New("queue commit reached an uncertain durability boundary")
	queueSyncFile           = func(file *os.File) error { return file.Sync() }
	queueSyncDirectory      = func(file *os.File) error { return file.Sync() }
)

type queuedPrompt struct {
	ID            string            `json:"id"`
	RequestID     string            `json:"requestId,omitempty"`
	Text          string            `json:"text"`
	Prompt        []map[string]any  `json:"prompt"`
	TwoPass       bool              `json:"twoPass,omitempty"`
	Mode          agentcore.RunMode `json:"mode,omitempty"`
	RunTimeoutMs  *int64            `json:"runTimeoutMs,omitempty"`
	RunTokenLimit *int64            `json:"runTokenLimit,omitempty"`
	// Nil is a legacy queue entry; new entries freeze the resolved mode.
	HarnessMode          *string    `json:"harnessMode,omitempty"`
	HarnessModeSource    string     `json:"harnessModeSource,omitempty"`
	Status               string     `json:"status"`
	CreatedAt            time.Time  `json:"createdAt"`
	StartedAt            *time.Time `json:"startedAt,omitempty"`
	Delivery             string     `json:"delivery,omitempty"`
	AttachmentRefs       []string   `json:"attachmentRefs,omitempty"`
	CheckpointID         string     `json:"checkpointId,omitempty"`
	BackgroundCompletion bool       `json:"backgroundCompletion,omitempty"`
	WakeID               string     `json:"wakeId,omitempty"`
}

type queueDiskState struct {
	Version              int                                 `json:"version"`
	Items                []*queuedPrompt                     `json:"items"`
	Paused               bool                                `json:"paused,omitempty"`
	WakeCancelledBefore  *time.Time                          `json:"wakeCancelledBefore,omitempty"`
	ConversationReceipts map[string]conversationInboxReceipt `json:"conversationReceipts,omitempty"`
}

type queueItemView struct {
	ID                   string     `json:"id"`
	RequestID            string     `json:"requestId,omitempty"`
	Text                 string     `json:"text"`
	Status               string     `json:"status"`
	Position             int        `json:"position"`
	Attachments          int        `json:"attachments"`
	TwoPass              bool       `json:"twoPass"`
	CreatedAt            time.Time  `json:"createdAt"`
	StartedAt            *time.Time `json:"startedAt,omitempty"`
	HarnessMode          *string    `json:"harnessMode,omitempty"`
	HarnessModeSource    string     `json:"harnessModeSource,omitempty"`
	BackgroundCompletion bool       `json:"backgroundCompletion,omitempty"`
}

func queueChatID(sess *serveSession) string {
	if strings.TrimSpace(sess.queueKey) != "" {
		return sess.queueKey
	}
	return sess.id
}

func (sess *serveSession) queuePath() string {
	key := sess.queueKey
	if key == "" {
		key = sess.id
	}
	return filepath.Join(
		session.PrimaryDir(), "projects", session.ProjectDirName(sess.cwd),
		"queues", session.ProjectDirName(key)+".json",
	)
}

func (sess *serveSession) loadQueue() error {
	b, err := os.ReadFile(sess.queuePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var state queueDiskState
	if err := json.Unmarshal(b, &state); err != nil {
		return err
	}
	sess.queuePaused = state.Paused
	sess.wakeCancelledBefore = state.WakeCancelledBefore
	sess.conversationReceipts = state.ConversationReceipts
	normalized := false
	for _, item := range state.Items {
		if item == nil || strings.TrimSpace(item.ID) == "" || len(item.Prompt) == 0 {
			continue
		}
		if item.RequestID != "" && !validPromptRequestID(item.RequestID) {
			return fmt.Errorf("invalid queued prompt request id")
		}
		if item.HarnessMode != nil {
			if _, err := harnesscore.ParseMode(*item.HarnessMode); err != nil || *item.HarnessMode == "" {
				return fmt.Errorf("invalid queued harness mode")
			}
		} else {
			// Pre-selector pending work has no evidence of Raven admission.
			// Freeze its original model-family path before a newer preference
			// or deployment override can relabel it during promotion.
			mode := sess.modelFamilyHarnessModeLocked()
			item.HarnessMode = &mode
			item.HarnessModeSource = "legacy-model-family"
			normalized = true
		}
		// A process interruption cannot prove whether a running request reached a
		// terminal state. Requeue it explicitly instead of silently dropping it.
		if isGuidanceDelivery(item.Delivery) && (item.Status == "guiding" || item.Status == "applied") {
			chatPath, pathErr := session.ChatPath(sess.cwd, sess.queueKey)
			if pathErr != nil {
				return pathErr
			}
			committed, committedErr := session.HasGuidanceCheckpoint(chatPath, item.ID)
			if committedErr != nil {
				return committedErr
			}
			if committed {
				normalized = true
				continue
			}
		}
		if item.BackgroundCompletion && item.WakeID != "" && (item.Status == "running" || item.Status == "cancelling" || item.Status == "finishing" || item.Status == "interrupted") {
			// A dispatched automatic turn may already have made side effects.
			// Reopening/restarting never silently replays that turn.
			item.Status = "interrupted"
			item.StartedAt = nil
			normalized = true
		} else if item.Status == "running" || item.Status == "cancelling" || item.Status == "finishing" || item.Status == "guiding" || item.Status == "applied" || item.Status == "interrupted" {
			item.Status = "pending"
			item.StartedAt = nil
			normalized = true
		}
		if item.Status == "" {
			item.Status = "pending"
		}
		sess.queuedPrompts = append(sess.queuedPrompts, item)
	}
	if normalized {
		return sess.persistQueueLocked()
	}
	return nil
}

func (sess *serveSession) persistQueueLocked() error {
	if sess.queuePersistenceErr != nil {
		return sess.queuePersistenceErr
	}
	state := queueDiskState{Version: 2, Items: sess.queuedPrompts, Paused: sess.queuePaused, WakeCancelledBefore: sess.wakeCancelledBefore, ConversationReceipts: sess.conversationReceipts}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := sess.queuePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	payload := append(b, '\n')
	written, writeErr := file.Write(payload)
	if writeErr == nil && written != len(payload) {
		writeErr = fmt.Errorf("short queue write: %d/%d", written, len(payload))
	}
	if writeErr == nil {
		writeErr = queueSyncFile(file)
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return errors.Join(writeErr, closeErr)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = queueSyncDirectory(directory)
		err = errors.Join(err, directory.Close())
	}
	if err != nil {
		sess.queuePersistenceErr = fmt.Errorf("%w: %v", errQueueCommitUncertain, err)
		return sess.queuePersistenceErr
	}
	return nil
}

func queueAttachments(prompt []map[string]any) int {
	n := 0
	for _, block := range prompt {
		switch kind, _ := block["type"].(string); kind {
		case "image", "image_url", "input_image", "file", "input_file", "attachment_collection":
			n++
		}
	}
	return n
}

func queuePromptImages(prompt []map[string]any) []string {
	images := make([]string, 0)
	for _, block := range prompt {
		if url := imageURLFromPromptBlock(block); url != "" {
			images = append(images, url)
		}
	}
	return images
}

func queuePromptPDFs(prompt []map[string]any) []map[string]any {
	files := make([]map[string]any, 0)
	for _, block := range prompt {
		if name, format, ok := promptDocumentBlock(block); ok {
			file := block["file"].(map[string]any)
			files = append(files, map[string]any{"kind": "document", "name": name, "type": format.MediaType, "path": stringValue(file["file_path"])})
			continue
		}
		if name, path, ok := promptPDFBlock(block); ok {
			files = append(files, map[string]any{
				"kind": "pdf", "name": name, "type": "application/pdf", "path": path,
			})
		}
	}
	return files
}

func queueViews(items []*queuedPrompt) []queueItemView {
	views := make([]queueItemView, 0, len(items))
	position := 0
	for _, item := range items {
		if item == nil {
			continue
		}
		pos := 0
		if item.Status == "pending" {
			position++
			pos = position
		}
		views = append(views, queueItemView{
			ID: item.ID, RequestID: item.RequestID, Text: item.Text, Status: item.Status, Position: pos,
			Attachments: queueAttachments(item.Prompt), TwoPass: item.TwoPass,
			CreatedAt: item.CreatedAt, StartedAt: item.StartedAt,
			HarnessMode: item.HarnessMode, HarnessModeSource: item.HarnessModeSource,
			BackgroundCompletion: item.BackgroundCompletion,
		})
	}
	return views
}

func (sess *serveSession) queueSnapshot() []queueItemView {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return queueViews(sess.queuedPrompts)
}

func (sess *serveSession) broadcastQueue() {
	sess.broadcast("queue_changed", map[string]any{
		"sessionId": sess.id, "items": sess.queueSnapshot(), "paused": sess.queueIsPaused(),
	})
}

func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	writeJSON(w, map[string]any{"sessionId": sess.id, "items": sess.queueSnapshot(), "paused": sess.queueIsPaused()})
}

func (s *Server) handleQueueEnqueue(w http.ResponseWriter, r *http.Request) {
	if s.ConversationInboxEnabled && !isConversationInboxAdmission(r.Context()) {
		s.handleConversationInbox(w, r)
		return
	}
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	requestID := r.Header.Get(promptRequestIDHeader)
	if requestID != "" && !validPromptRequestID(requestID) {
		writeErr(w, http.StatusBadRequest, "invalid prompt request id")
		return
	}
	sess.mu.Lock()
	acceptedCancelGeneration := sess.cancelGeneration
	acceptedHarnessMode := sess.snapshotHarnessModeLocked()
	sess.mu.Unlock()
	var body struct {
		Prompt  []map[string]any `json:"prompt"`
		Options struct {
			TwoPass                  bool   `json:"twoPass"`
			Mode                     string `json:"mode"`
			RunTimeoutMs             *int64 `json:"runTimeoutMs"`
			RunTokenLimit            *int64 `json:"runTokenLimit"`
			EvoResumeReflectionRunID string `json:"evoResumeReflectionRunId"`
		} `json:"options"`
	}
	if err := decodeBoundedJSONBody(w, r, maxPromptRequestBytes, &body); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "queued prompt request is too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "prompt must be a non-empty array of content blocks")
		return
	}
	if len(body.Prompt) == 0 {
		writeErr(w, http.StatusBadRequest, "prompt must be a non-empty array of content blocks")
		return
	}
	if body.Options.EvoResumeReflectionRunID != "" {
		writeErr(w, http.StatusConflict, "回顧恢復不能排入一般任務佇列；請等候目前任務結束後點選重試回顧")
		return
	}
	if err := validateRunBudgets(body.Options.RunTimeoutMs, body.Options.RunTokenLimit); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	materializedPrompt, err := sess.prepareIncomingAttachments(r.Context(), body.Prompt)
	if err != nil {
		writeCodedErr(w, http.StatusUnprocessableEntity, attachmentUnreadableCode, err.Error())
		return
	}
	body.Prompt = materializedPrompt
	defer cleanupPromptPDFTemps(body.Prompt)
	msg, err := sess.promptBlocks(body.Prompt)
	if err != nil {
		writeCodedErr(w, http.StatusUnprocessableEntity, attachmentUnreadableCode, err.Error())
		return
	}
	text := userMessageDisplayText(msg)
	if !messageHasUserContent(msg) {
		writeErr(w, http.StatusBadRequest, "prompt has no user content")
		return
	}
	runMode := agentcore.RunModeAuto
	switch strings.TrimSpace(body.Options.Mode) {
	case "", "auto":
	case string(agentcore.RunModeResearch):
		runMode = agentcore.RunModeResearch
	default:
		writeErr(w, http.StatusBadRequest, "options.mode must be auto or research")
		return
	}
	displayText := strings.TrimSpace(text)
	if displayText == "" {
		displayText = fmt.Sprintf("%d 張圖片", len(queuePromptImages(body.Prompt)))
	}
	item := &queuedPrompt{
		ID: session.NewUUID(), RequestID: requestID, Text: displayText, Prompt: body.Prompt,
		TwoPass: body.Options.TwoPass, Mode: runMode, Status: "pending", CreatedAt: time.Now().UTC(),
		RunTimeoutMs: body.Options.RunTimeoutMs, RunTokenLimit: body.Options.RunTokenLimit,
	}
	if isConversationInboxAdmission(r.Context()) {
		item.Delivery = "conversation"
	}
	chatID := queueChatID(sess)
	durablePrompt, err := persistQueuedPromptPDFs(sess.cwd, chatID, body.Prompt)
	if err != nil {
		writeCodedErr(w, http.StatusUnprocessableEntity, attachmentUnreadableCode, err.Error())
		return
	}
	item.Prompt = durablePrompt
	attachmentRefs := append([]string(nil), item.AttachmentRefs...)
	var attachmentErr error
	if len(attachmentRefs) == 0 {
		attachmentRefs, attachmentErr = persistPromptAttachments(sess.cwd, chatID, msg)
	}
	if attachmentErr != nil {
		writeErr(w, http.StatusUnprocessableEntity, attachmentErr.Error())
		return
	}
	item.AttachmentRefs = attachmentRefs
	sess.mu.Lock()
	if len(sess.queuedPrompts) >= maxQueueItems {
		sess.mu.Unlock()
		writeErr(w, http.StatusConflict, fmt.Sprintf("queue limit reached (%d)", maxQueueItems))
		return
	}
	sess.queuedPrompts = append(sess.queuedPrompts, item)
	if err = rememberConversationReceiptLocked(sess, r.Context(), item); err != nil {
		sess.queuedPrompts = sess.queuedPrompts[:len(sess.queuedPrompts)-1]
		sess.mu.Unlock()
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	item.HarnessMode = &acceptedHarnessMode
	wasPaused := sess.queuePaused
	if acceptedCancelGeneration == sess.cancelGeneration {
		sess.queuePaused = false
	}
	err = sess.persistQueueLocked()
	if err != nil {
		sess.queuedPrompts = sess.queuedPrompts[:len(sess.queuedPrompts)-1]
		forgetConversationReceiptLocked(sess, r.Context(), item)
		sess.queuePaused = wasPaused
		sess.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := queueViews(sess.queuedPrompts)
	itemView := queueViews([]*queuedPrompt{item})[0]
	sess.mu.Unlock()
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	if taskErr := sess.recordBackgroundTask(backgroundTaskRecord{
		ID: item.ID, Kind: "queued_prompt", Title: item.Text,
		Status: "queued", CreatedAt: item.CreatedAt,
	}); taskErr != nil {
		sess.mu.Lock()
		for i, queued := range sess.queuedPrompts {
			if queued.ID == item.ID {
				sess.queuedPrompts = append(sess.queuedPrompts[:i], sess.queuedPrompts[i+1:]...)
				break
			}
		}
		if acceptedCancelGeneration == sess.cancelGeneration {
			sess.queuePaused = wasPaused
		}
		forgetConversationReceiptLocked(sess, r.Context(), item)
		queueErr := sess.persistQueueLocked()
		sess.mu.Unlock()
		if queueErr != nil {
			sess.broadcast("work_center_error", map[string]any{
				"sessionId": sess.id, "operation": "rollback_queue", "message": queueErr.Error(),
			})
		}
		writeErr(w, http.StatusInternalServerError, taskErr.Error())
		return
	}
	if isConversationInboxAdmission(r.Context()) {
		s.dispatchConversationInbox(sess, item, msg)
		writeJSON(w, conversationReceiptView(sess, item.RequestID, false))
	} else {
		s.startQueuePump(sess)
		writeJSON(w, map[string]any{"item": itemView, "items": views})
	}
}

func takePendingQueueItem(sess *serveSession, id string) (*queuedPrompt, int, []queueItemView, int, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	idx := -1
	for i, item := range sess.queuedPrompts {
		if item.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, -1, nil, http.StatusNotFound, errors.New("unknown queued prompt")
	}
	item := sess.queuedPrompts[idx]
	if item.Status != "pending" {
		return nil, -1, nil, http.StatusConflict, errors.New("queued prompt is already running")
	}
	sess.queuedPrompts = append(sess.queuedPrompts[:idx], sess.queuedPrompts[idx+1:]...)
	if err := sess.persistQueueLocked(); err != nil {
		sess.queuedPrompts = append(sess.queuedPrompts, nil)
		copy(sess.queuedPrompts[idx+1:], sess.queuedPrompts[idx:])
		sess.queuedPrompts[idx] = item
		return nil, -1, nil, http.StatusInternalServerError, err
	}
	return item, idx, queueViews(sess.queuedPrompts), http.StatusOK, nil
}

func restorePendingQueueItem(sess *serveSession, item *queuedPrompt, idx int) error {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if idx < 0 || idx > len(sess.queuedPrompts) {
		idx = len(sess.queuedPrompts)
	}
	sess.queuedPrompts = append(sess.queuedPrompts, nil)
	copy(sess.queuedPrompts[idx+1:], sess.queuedPrompts[idx:])
	sess.queuedPrompts[idx] = item
	return sess.persistQueueLocked()
}

func beginQueueGuidance(sess *serveSession, id string) (*queuedPrompt, []queueItemView, int, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for _, item := range sess.queuedPrompts {
		if item.ID != id {
			continue
		}
		if item.Status != "pending" {
			return nil, nil, http.StatusConflict, errors.New("queued prompt is already running")
		}
		item.Status = "guiding"
		item.Delivery = "guidance"
		if err := sess.persistQueueLocked(); err != nil {
			item.Status = "pending"
			return nil, nil, http.StatusInternalServerError, err
		}
		return item, queueViews(sess.queuedPrompts), http.StatusOK, nil
	}
	return nil, nil, http.StatusNotFound, errors.New("unknown queued prompt")
}

func restoreGuidanceQueueItem(sess *serveSession, id string) error {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for _, item := range sess.queuedPrompts {
		if item.ID == id && item.Status == "guiding" {
			item.Status = "pending"
			item.StartedAt = nil
			return sess.persistQueueLocked()
		}
	}
	return nil
}

func (s *Server) handleQueueGuide(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	item, views, status, err := beginQueueGuidance(sess, r.PathValue("queueId"))
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	preparedPrompt, err := sess.prepareQueuedAttachments(r.Context(), item.Prompt)
	if err != nil {
		_ = restoreGuidanceQueueItem(sess, item.ID)
		writeCodedErr(w, http.StatusUnprocessableEntity, attachmentUnreadableCode, err.Error())
		return
	}
	defer cleanupPromptPDFTemps(preparedPrompt)
	msg, err := sess.promptBlocks(preparedPrompt)
	if err != nil {
		_ = restoreGuidanceQueueItem(sess, item.ID)
		writeCodedErr(w, http.StatusUnprocessableEntity, attachmentUnreadableCode, err.Error())
		return
	}
	// Publish the in-flight state before handing control to the runtime. It may
	// apply and durably commit the guidance synchronously inside SubmitGuidance;
	// broadcasting this earlier snapshot afterward would resurrect its UI row.
	_ = sess.recordBackgroundTask(backgroundTaskRecord{
		ID: item.ID, Kind: "queued_prompt", Title: item.Text, Status: "guiding",
	})
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	receipt, err := submitSessionGuidance(sess, msg, item.ID)
	if err != nil {
		if restoreErr := restoreGuidanceQueueItem(sess, item.ID); restoreErr != nil {
			sess.broadcast("work_center_error", map[string]any{
				"sessionId": sess.id, "operation": "restore_guidance_queue", "message": restoreErr.Error(),
			})
		}
		_ = sess.recordBackgroundTask(backgroundTaskRecord{
			ID: item.ID, Kind: "queued_prompt", Title: item.Text, Status: "queued",
		})
		sess.broadcastQueue()
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	publishSessionGuidance(sess, msg)
	writeJSON(w, map[string]any{
		"id": receipt.ID, "mode": receipt.Mode, "disposition": receipt.Disposition,
		"targetTurn": receipt.TargetTurn, "queuePosition": receipt.QueuePosition,
		"items": sess.queueSnapshot(),
	})
}

// settleGuidanceQueue keeps accepted steering durable until the runtime says it
// was actually applied. If the current turn finishes first, the item returns to
// the normal queue instead of disappearing.
func (s *Server) settleGuidanceQueue(sess *serveSession, event agentcore.GuidanceEvent) {
	// accepted/queued/started are progress receipts, not terminal lifecycle
	// events. In particular, treating accepted as failure would move a guiding
	// item back to pending, making an old guidance row reappear on the user's
	// next send and allowing it to execute twice.
	switch event.Disposition {
	case agentcore.GuidanceAccepted, agentcore.GuidanceQueued, agentcore.GuidanceStarted:
		return
	case agentcore.GuidanceApplied, agentcore.GuidanceRejected, agentcore.GuidanceCancelled:
		// Terminal lifecycle events are settled below.
	default:
		return
	}
	sess.mu.Lock()
	var item *queuedPrompt
	for _, candidate := range sess.queuedPrompts {
		if candidate.ID == event.ID && candidate.Status == "guiding" {
			item = candidate
			break
		}
	}
	if item == nil {
		sess.mu.Unlock()
		return
	}
	applied := event.Disposition == agentcore.GuidanceApplied
	if applied {
		item.Status = "applied"
		if sess.appliedGuidance == nil {
			sess.appliedGuidance = map[string]bool{}
		}
		sess.appliedGuidance[item.ID] = true
	} else {
		item.Status = "pending"
		item.StartedAt = nil
	}
	persistErr := sess.persistQueueLocked()
	views := queueViews(sess.queuedPrompts)
	sess.mu.Unlock()
	if persistErr != nil {
		sess.broadcast("work_center_error", map[string]any{
			"sessionId": sess.id, "operation": "settle_guidance_queue", "message": persistErr.Error(),
		})
	}
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	if applied {
		return
	}
	_ = sess.recordBackgroundTask(backgroundTaskRecord{
		ID: item.ID, Kind: "queued_prompt", Title: item.Text, Status: "queued",
	})
	s.startQueuePump(sess)
}

// commitAppliedGuidance uses the chat transcript as the commit record. Queue
// deletion happens afterward and replay is idempotent by guidance ID.
func (s *Server) commitAppliedGuidance(sess *serveSession, checkpointID string) error {
	if sess.chat == nil {
		return errors.New("durable chat is unavailable for applied guidance")
	}
	sess.mu.Lock()
	items := make([]*queuedPrompt, 0)
	for _, item := range sess.queuedPrompts {
		if item != nil && item.Status == "applied" && sess.appliedGuidance[item.ID] {
			copyItem := *item
			items = append(items, &copyItem)
		}
	}
	sess.mu.Unlock()
	if len(items) == 0 {
		return nil
	}
	for _, item := range items {
		requestID := ""
		text := promptDisplayText(item.Prompt)
		if item.Delivery == "conversation" {
			requestID = item.RequestID
			text = conversationPromptText(item.Prompt)
		}
		if err := sess.chat.GuidanceCheckpointWithRequestID(item.ID, checkpointID, text, item.AttachmentRefs, requestID); err != nil {
			return err
		}
	}
	sess.mu.Lock()
	committed := map[string]bool{}
	for _, item := range items {
		committed[item.ID] = true
	}
	filtered := sess.queuedPrompts[:0]
	for _, item := range sess.queuedPrompts {
		if item != nil && committed[item.ID] {
			delete(sess.appliedGuidance, item.ID)
			continue
		}
		filtered = append(filtered, item)
	}
	sess.queuedPrompts = filtered
	err := sess.persistQueueLocked()
	views := queueViews(sess.queuedPrompts)
	sess.mu.Unlock()
	if err != nil {
		return err
	}
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	for _, item := range items {
		finishedAt := time.Now().UTC()
		_ = sess.recordBackgroundTask(backgroundTaskRecord{ID: item.ID, Kind: "queued_prompt", Title: item.Text, Status: "completed", FinishedAt: &finishedAt})
	}
	return nil
}

func (sess *serveSession) prepareRecoveredGuidance(message provider.Message) (provider.Message, []string, error) {
	return sess.prepareRecoveredGuidanceContext(context.Background(), message)
}

func (sess *serveSession) prepareRecoveredGuidanceContext(ctx context.Context, message provider.Message) (provider.Message, []string, error) {
	type pendingGuidance struct {
		item   *queuedPrompt
		prompt []map[string]any
	}
	var pending []pendingGuidance
	sess.mu.Lock()
	for _, item := range sess.queuedPrompts {
		if item == nil || !isGuidanceDelivery(item.Delivery) || item.Status != "pending" {
			continue
		}
		prompt := make([]map[string]any, len(item.Prompt))
		for i, block := range item.Prompt {
			prompt[i] = clonePromptBlock(block)
		}
		pending = append(pending, pendingGuidance{item: item, prompt: prompt})
	}
	sess.mu.Unlock()
	prepared := map[*queuedPrompt]provider.Message{}
	for _, pendingItem := range pending {
		preparedPrompt, err := sess.prepareQueuedAttachments(ctx, pendingItem.prompt)
		if err != nil {
			return message, nil, err
		}
		guidance, err := sess.promptBlocks(preparedPrompt)
		cleanupPromptPDFTemps(preparedPrompt)
		if err != nil {
			return message, nil, err
		}
		if err := ctx.Err(); err != nil {
			return message, nil, err
		}
		prepared[pendingItem.item] = guidance
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return message, nil, err
	}
	ids := make([]string, 0)
	items := make([]*queuedPrompt, 0)
	// Cancellation/editing may occur during extraction and indexing. Only
	// still-pending identical queue objects are admitted, in current order.
	for _, item := range sess.queuedPrompts {
		guidance, ok := prepared[item]
		if !ok || !isGuidanceDelivery(item.Delivery) || item.Status != "pending" {
			continue
		}
		text := provider.MessageText(guidance)
		if item.Delivery != "conversation" {
			text = strings.TrimSpace(text)
		}
		if strings.TrimSpace(text) != "" {
			message.Content += "\n\nRecovered guidance:\n" + text
		}
		message.ContentParts = append(message.ContentParts, guidance.ContentParts...)
		items = append(items, item)
		ids = append(ids, item.ID)
	}
	if len(ids) > 0 {
		if sess.appliedGuidance == nil {
			sess.appliedGuidance = map[string]bool{}
		}
		for _, item := range items {
			item.Status = "applied"
			sess.appliedGuidance[item.ID] = true
		}
		if err := sess.persistQueueLocked(); err != nil {
			return message, nil, err
		}
	}
	return message, ids, nil
}

func (sess *serveSession) restoreRecoveredGuidance(ids []string) {
	if len(ids) == 0 {
		return
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	sess.mu.Lock()
	for _, item := range sess.queuedPrompts {
		if item != nil && set[item.ID] && item.Status == "applied" {
			item.Status = "pending"
			delete(sess.appliedGuidance, item.ID)
		}
	}
	_ = sess.persistQueueLocked()
	sess.mu.Unlock()
}

func (s *Server) handleQueueEdit(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	item, _, views, status, err := takePendingQueueItem(sess, r.PathValue("queueId"))
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	finishedAt := time.Now().UTC()
	_ = sess.recordBackgroundTask(backgroundTaskRecord{
		ID: item.ID, Kind: "queued_prompt", Title: item.Text,
		Status: "cancelled", FinishedAt: &finishedAt,
	})
	writeJSON(w, map[string]any{
		"draft": map[string]any{
			"text":   promptDisplayText(item.Prompt),
			"images": queuePromptImages(item.Prompt),
			"files":  queuePromptPDFs(item.Prompt),
		},
		"items": views,
	})
}

func (s *Server) handleQueueMove(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	var body struct {
		Direction     int      `json:"direction"`
		BeforeID      *string  `json:"beforeId"`
		ExpectedOrder []string `json:"expectedOrder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.BeforeID == nil && body.Direction != -1 && body.Direction != 1) {
		writeErr(w, http.StatusBadRequest, "direction must be -1 or 1")
		return
	}
	id := r.PathValue("queueId")
	sess.mu.Lock()
	if body.BeforeID != nil {
		previous := append([]*queuedPrompt(nil), sess.queuedPrompts...)
		if err := reorderPendingPrompts(sess.queuedPrompts, id, *body.BeforeID, body.ExpectedOrder); err != nil {
			sess.mu.Unlock()
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if err := sess.persistQueueLocked(); err != nil {
			sess.queuedPrompts = previous
			sess.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		views := queueViews(sess.queuedPrompts)
		sess.mu.Unlock()
		sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
		writeJSON(w, map[string]any{"items": views})
		return
	}
	idx := -1
	for i, item := range sess.queuedPrompts {
		if item.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		sess.mu.Unlock()
		writeErr(w, http.StatusNotFound, "unknown queued prompt")
		return
	}
	item := sess.queuedPrompts[idx]
	target := idx + body.Direction
	if item.Status != "pending" || target < 0 || target >= len(sess.queuedPrompts) || sess.queuedPrompts[target].Status != "pending" {
		sess.mu.Unlock()
		writeErr(w, http.StatusConflict, "queued prompt cannot move to that position")
		return
	}
	sess.queuedPrompts[idx], sess.queuedPrompts[target] = sess.queuedPrompts[target], sess.queuedPrompts[idx]
	err := sess.persistQueueLocked()
	if err != nil {
		sess.queuedPrompts[idx], sess.queuedPrompts[target] = sess.queuedPrompts[target], sess.queuedPrompts[idx]
		sess.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := queueViews(sess.queuedPrompts)
	sess.mu.Unlock()
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	writeJSON(w, map[string]any{"items": views})
}

// Dragging changes pending order atomically. Preserve all nonpending slots and
// reject a drop made against a queue that changed while the pointer was held.
func reorderPendingPrompts(items []*queuedPrompt, id, beforeID string, expected []string) error {
	var pending []*queuedPrompt
	var moved *queuedPrompt
	for _, item := range items {
		if item != nil && item.Status == "pending" {
			pending = append(pending, item)
			if item.ID == id {
				moved = item
			}
		}
	}
	if len(expected) != len(pending) {
		return fmt.Errorf("queue changed during drag; try again")
	}
	for i, item := range pending {
		if item.ID != expected[i] {
			return fmt.Errorf("queue changed during drag; try again")
		}
	}
	if moved == nil || beforeID == id {
		return fmt.Errorf("dragged prompt must still be pending")
	}
	ordered := make([]*queuedPrompt, 0, len(pending))
	inserted := false
	for _, item := range pending {
		if item == moved {
			continue
		}
		if item.ID == beforeID {
			ordered = append(ordered, moved)
			inserted = true
		}
		ordered = append(ordered, item)
	}
	if beforeID == "" {
		ordered = append(ordered, moved)
	} else if !inserted {
		return fmt.Errorf("drop target must still be pending")
	}
	j := 0
	for i, item := range items {
		if item != nil && item.Status == "pending" {
			items[i] = ordered[j]
			j++
		}
	}
	return nil
}

func (s *Server) handleQueueCancel(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	id := r.PathValue("queueId")
	sess.mu.Lock()
	idx := -1
	for i, item := range sess.queuedPrompts {
		if item.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		sess.mu.Unlock()
		writeErr(w, http.StatusNotFound, "unknown queued prompt")
		return
	}
	item := sess.queuedPrompts[idx]
	if item.Status == "running" || item.Status == "cancelling" {
		previousStatus := item.Status
		previousCutoff := sess.wakeCancelledBefore
		cutoff := time.Now()
		sess.wakeCancelledBefore = &cutoff
		item.Status = "cancelling"
		cancel := sess.cancelRun
		if err := sess.persistQueueLocked(); err != nil {
			item.Status = previousStatus
			sess.wakeCancelledBefore = previousCutoff
			sess.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		views := queueViews(sess.queuedPrompts)
		sess.mu.Unlock()
		sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
		// Publish the intermediate state before invoking cancel. The runtime may
		// return immediately; recording "cancelling" afterward could otherwise
		// overwrite the stronger terminal "cancelled" record.
		sess.recordBackgroundTask(backgroundTaskRecord{
			ID: item.ID, Kind: "queued_prompt", Title: item.Text, Status: "cancelling",
		})
		if cancel != nil {
			cancel()
		}
		writeJSON(w, map[string]any{"cancelled": false, "cancelling": true, "items": views})
		return
	}
	if item.WakeID != "" {
		if err := s.setWakeState(sess, item.WakeID, "cancelled", "", "cancelled"); err != nil {
			sess.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	sess.queuedPrompts = append(sess.queuedPrompts[:idx], sess.queuedPrompts[idx+1:]...)
	err := sess.persistQueueLocked()
	if err != nil {
		sess.queuedPrompts = append(sess.queuedPrompts, nil)
		copy(sess.queuedPrompts[idx+1:], sess.queuedPrompts[idx:])
		sess.queuedPrompts[idx] = item
		sess.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := queueViews(sess.queuedPrompts)
	sess.mu.Unlock()
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	finishedAt := time.Now().UTC()
	sess.recordBackgroundTask(backgroundTaskRecord{
		ID: item.ID, Kind: "queued_prompt", Title: item.Text,
		Status: "cancelled", FinishedAt: &finishedAt,
	})
	writeJSON(w, map[string]any{"cancelled": true, "items": views})
}

func (s *Server) startQueuePump(sess *serveSession) {
	if sess == nil {
		return
	}
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	if s.configurationLeaseActiveLocked(time.Now()) {
		return
	}
	sess.mu.Lock()
	if sess.queuePump || !sess.hasRunnableQueuedPromptLocked() {
		sess.mu.Unlock()
		return
	}
	sess.queuePump = true
	sess.mu.Unlock()
	go s.runQueuePump(sess)
}

func (s *Server) runQueuePump(sess *serveSession) {
	restart := true
	defer func() {
		sess.mu.Lock()
		sess.queuePump = false
		more := restart && sess.hasRunnableQueuedPromptLocked()
		sess.mu.Unlock()
		if more {
			s.startQueuePump(sess)
		}
	}()
	for {
		sess.promptMu.Lock()
		item, err := sess.nextQueuedPrompt()
		if err != nil {
			restart = false
			sess.promptMu.Unlock()
			sess.broadcast("work_center_error", map[string]any{
				"sessionId": sess.id, "operation": "start_queue", "message": err.Error(),
			})
			return
		}
		if item == nil {
			sess.promptMu.Unlock()
			return
		}
		// Promotion to running is already durable. Publish that state before chat
		// persistence/model startup so the composer immediately removes the sent
		// queue row; the formal user bubble and Work Center own it from here.
		sess.broadcastQueue()
		s.runQueuedPrompt(sess, item)
		sess.promptMu.Unlock()
	}
}

func (sess *serveSession) nextQueuedPrompt() (*queuedPrompt, error) {
	// Inbox admission also writes the Work Center before it commits or rolls
	// back its receipt. An already-running pump must not execute a visible row
	// in the interval between those writes, then let a failed admission forget
	// its identity. This lock is never held for model/tool execution.
	if sess.conversationInboxEnabled {
		sess.conversationInboxMu.Lock()
		defer sess.conversationInboxMu.Unlock()
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.queuePaused {
		return nil, nil
	}
	for _, item := range sess.queuedPrompts {
		if item.Status != "pending" {
			continue
		}
		now := time.Now().UTC()
		item.Status = "running"
		item.StartedAt = &now
		if err := sess.persistQueueLocked(); err != nil {
			item.Status = "pending"
			item.StartedAt = nil
			return nil, err
		}
		sess.activeInferenceControls = snapshotSessionInferenceControlsLocked(sess)
		mode := sess.snapshotHarnessModeLocked()
		if item.HarnessMode != nil {
			mode = *item.HarnessMode
		}
		sess.admitHarnessModeLocked(mode)
		return item, nil
	}
	return nil, nil
}

func (sess *serveSession) hasRunnableQueuedPromptLocked() bool {
	if sess.queuePaused {
		return false
	}
	for _, item := range sess.queuedPrompts {
		if item.Status == "pending" {
			return true
		}
	}
	return false
}

func (s *Server) runQueuedPrompt(sess *serveSession, item *queuedPrompt) {
	turnStarted := time.Now()
	if item.BackgroundCompletion {
		s.configurationMu.Lock()
		if s.wakeClosed {
			s.configurationMu.Unlock()
			s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, context.Canceled)
			return
		}
		s.wakeWorkers.Add(1)
		s.configurationMu.Unlock()
		defer s.wakeWorkers.Done()
	}
	defer func() {
		sess.mu.Lock()
		sess.activeInferenceControls = nil
		sess.activeHarnessMode = nil
		sess.mu.Unlock()
	}()
	ctx, cancel, budgetErr := newRunBudgetContext(item.RunTimeoutMs, item.RunTokenLimit)
	if budgetErr != nil {
		s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, budgetErr)
		return
	}
	if item.BackgroundCompletion {
		ctx = context.WithValue(ctx, backgroundCompletionContextKey{}, true)
		stop := context.AfterFunc(s.backgroundWakeContext(), cancel)
		defer stop()
		if s.BackgroundWakeAdmission != nil {
			if err := s.BackgroundWakeAdmission(ctx, sess.id); err != nil {
				cancel()
				s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, err)
				return
			}
		}
	}
	// Install cancellation ownership before chat persistence or any other
	// work. handleQueueCancel can observe "running" immediately after
	// nextQueuedPrompt; without this atomic handoff it could mark the item
	// cancelling while cancelRun was still nil, then the task would execute.
	sess.mu.Lock()
	if wakeErr := s.beginDurableWakeRunLocked(sess, item); wakeErr != nil {
		sess.mu.Unlock()
		cancel()
		s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, wakeErr)
		return
	}
	if item.Status == "cancelling" {
		for i, queued := range sess.queuedPrompts {
			if queued.ID == item.ID {
				sess.queuedPrompts = append(sess.queuedPrompts[:i], sess.queuedPrompts[i+1:]...)
				break
			}
		}
		persistErr := sess.persistQueueLocked()
		views := queueViews(sess.queuedPrompts)
		sess.mu.Unlock()
		if persistErr != nil {
			sess.broadcast("work_center_error", map[string]any{
				"sessionId": sess.id, "operation": "finish_cancel", "message": persistErr.Error(),
			})
		}
		cancel()
		finishedAt := time.Now().UTC()
		sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
		sess.recordBackgroundTask(backgroundTaskRecord{
			ID: item.ID, Kind: "queued_prompt", Title: item.Text,
			Status: "cancelled", StopReason: "cancelled", FinishedAt: &finishedAt,
		})
		return
	}
	sess.cancelRun = cancel
	if sess.activeInferenceControls == nil {
		// Direct/internal callers may not use the persisted queue promotion.
		sess.activeInferenceControls = snapshotSessionInferenceControlsLocked(sess)
	}
	if sess.activeHarnessMode == nil {
		mode := sess.snapshotHarnessModeLocked()
		if item.HarnessMode != nil {
			mode = *item.HarnessMode
		}
		sess.admitHarnessModeLocked(mode)
	}
	sess.mu.Unlock()
	defer cancel()

	preparedPrompt, err := sess.prepareQueuedAttachments(ctx, item.Prompt)
	if err != nil {
		s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, err)
		return
	}
	defer cleanupPromptPDFTemps(preparedPrompt)
	msg, err := sess.promptBlocks(preparedPrompt)
	if err != nil {
		s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, err)
		return
	}
	text := userMessageDisplayText(msg)
	if item.Delivery == "conversation" {
		text = conversationUserDisplayText(msg)
	}
	chatID := queueChatID(sess)
	attachmentRefs := append([]string(nil), item.AttachmentRefs...)
	var attachmentErr error
	if len(attachmentRefs) == 0 {
		attachmentRefs, attachmentErr = persistPromptAttachments(sess.cwd, chatID, msg)
	}
	if attachmentErr != nil {
		// Keep the queue record honest: an invalid persisted attachment is a
		// failed queued turn, not a successful text-only fallback.
		s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, attachmentErr)
		return
	}
	if sess.chat == nil {
		cw, chatErr := sess.openBoundChatWriter(chatID)
		if chatErr != nil {
			s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, chatErr)
			return
		}
		sess.chat = cw
	}
	runMessages := append(append([]provider.Message(nil), sess.history...), msg)
	runCheckpointID := session.StableCheckpointIdentity(chatID, runMessages)
	var inputWriteErr error
	if item.BackgroundCompletion {
		inputWriteErr = sess.chat.BackgroundObservation(text)
	} else {
		inputWriteErr = sess.chat.UserWithRequestID(text, attachmentRefs, item.RequestID)
	}
	if inputWriteErr == nil {
		inputWriteErr = persistAttachmentCollectionContext(sess.chat, item.Prompt)
	}
	if chatErr := inputWriteErr; chatErr != nil {
		s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, chatErr)
		return
	}
	if chatErr := sess.persistHarnessRunStart(runCheckpointID); chatErr != nil {
		s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, chatErr)
		return
	}
	sess.mu.Lock()
	mark := len(sess.history)
	sess.turnMarks = append(sess.turnMarks, mark)
	sess.activeRunCheckpointID = runCheckpointID
	sess.stableCheckpointErr = nil
	views := queueViews(sess.queuedPrompts)
	sess.mu.Unlock()
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	sess.recordBackgroundTask(backgroundTaskRecord{
		ID: item.ID, Kind: "queued_prompt", Title: item.Text,
		Status: "running", StartedAt: item.StartedAt,
	})

	// Only a real queued user admission may update the trusted restore policy.
	// Automatic observations must retain the previously authorized charter.
	if !item.BackgroundCompletion && s.BackgroundWakeEnabled {
		if err := s.refreshWakeOwnerForUserAdmission(sess); err != nil {
			s.finishQueuedPromptEarlyFailure(sess, item, turnStarted, err)
			return
		}
	}
	stop := s.runPromptMessageWithRequestID(ctx, sess, msg, mark, item.TwoPass, item.Mode, false, item.RequestID)
	// Freeze cancellation before terminal persistence, just as foreground
	// turns do. A Stop after this boundary pauses successors without rewriting
	// the already-settled queued result on disk or in its terminal event.
	sess.mu.Lock()
	sess.cancelRun = nil
	if stop != "error" && stop != "token_limit" && (item.Status == "cancelling" || errors.Is(context.Cause(ctx), context.Canceled)) {
		stop = "cancelled"
	}
	item.Status = "finishing"
	sess.mu.Unlock()
	if err := sess.pauseQueueForBudget(stop); err != nil {
		stop = "error"
		sess.broadcast("work_center_error", map[string]any{"sessionId": sess.id, "operation": "pause_queue", "message": err.Error()})
	}
	sess.mu.Lock()
	runID := sess.activeRunID
	checkpointID := sess.stableCheckpointID
	checkpointErr := sess.stableCheckpointErr
	sess.mu.Unlock()
	if checkpointErr != nil {
		stop = "error"
	}
	if checkpointErr == nil {
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
	if err := s.finishDurableWakeRun(sess, item, runID, stop); err != nil {
		stop = "error"
		sess.broadcast("work_center_error", map[string]any{"operation": "finish_background_wake", "message": err.Error()})
	}
	sess.mu.Lock()
	sess.cancelRun = nil
	if sess.activeRunID == runID {
		sess.activeRunID = ""
	}
	sess.activeRunCheckpointID = ""
	sess.activeHarnessMode = nil
	for i, queued := range sess.queuedPrompts {
		if queued.ID != item.ID {
			continue
		}
		if stop == "error" {
			queued.Status = "interrupted"
			queued.StartedAt = nil
		} else {
			sess.queuedPrompts = append(sess.queuedPrompts[:i], sess.queuedPrompts[i+1:]...)
		}
		break
	}
	persistErr := sess.persistQueueLocked()
	views = queueViews(sess.queuedPrompts)
	sess.mu.Unlock()
	if persistErr != nil {
		sess.broadcast("work_center_error", map[string]any{
			"sessionId": sess.id, "operation": "finish_queue", "message": persistErr.Error(),
		})
	}
	status := "completed"
	if stop != "end_turn" && stop != "completed" {
		status = stop
	}
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	finishedAt := time.Now().UTC()
	sess.recordBackgroundTask(backgroundTaskRecord{
		ID: item.ID, Kind: "queued_prompt", Title: item.Text,
		Status: status, StopReason: stop, FinishedAt: &finishedAt,
	})
	sess.broadcast("turn_completed", map[string]any{
		"sessionId": sess.id, "origin": "queue", "taskId": item.ID,
		"runId":      runID,
		"stopReason": stop, "elapsedMs": time.Since(turnStarted).Milliseconds(),
	})
	sess.broadcast("run_state", map[string]any{
		"sessionId": sess.id, "chatId": sess.queueKey, "runId": runID,
		"status": "completed", "stopReason": stop,
	})
}

// finishQueuedPromptEarlyFailure closes the state transition that starts in
// nextQueuedPrompt.  Queue preparation, attachment persistence and chat setup
// all happen after the item has become durable "running" state, so every
// failure in that interval must remove the item, release cancellation
// ownership and publish a terminal event just like the normal completion path.
func (s *Server) finishQueuedPromptEarlyFailure(sess *serveSession, item *queuedPrompt, started time.Time, cause error) {
	if sess == nil || item == nil || cause == nil {
		return
	}
	wakeErr := s.finishDurableWakeRun(sess, item, "", "error")
	sess.mu.Lock()
	sess.cancelRun = nil
	sess.activeRunCheckpointID = ""
	for i, queued := range sess.queuedPrompts {
		if queued.ID == item.ID {
			if wakeErr != nil {
				queued.Status = "interrupted"
			} else {
				sess.queuedPrompts = append(sess.queuedPrompts[:i], sess.queuedPrompts[i+1:]...)
			}
			break
		}
	}
	persistErr := errors.Join(wakeErr, sess.persistQueueLocked())
	views := queueViews(sess.queuedPrompts)
	sess.mu.Unlock()

	if persistErr != nil {
		sess.broadcast("work_center_error", map[string]any{
			"sessionId": sess.id, "operation": "finish_queue", "message": persistErr.Error(),
		})
	}
	sess.broadcast("stream_error", map[string]any{"sessionId": sess.id, "message": cause.Error()})
	sess.broadcast("queue_changed", map[string]any{"sessionId": sess.id, "items": views})
	finishedAt := time.Now().UTC()
	sess.recordBackgroundTask(backgroundTaskRecord{
		ID: item.ID, Kind: "queued_prompt", Title: item.Text,
		Status: "failed", StopReason: cause.Error(), FinishedAt: &finishedAt,
	})
	sess.broadcast("turn_completed", map[string]any{
		"sessionId": sess.id, "origin": "queue", "taskId": item.ID,
		"stopReason": "error", "elapsedMs": time.Since(started).Milliseconds(),
	})
}
