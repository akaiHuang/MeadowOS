package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

const maxConversationReceipts = 1024
const conversationReceiptRetention = 30 * time.Minute

type conversationInboxReceipt struct {
	MessageID   string    `json:"messageId"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"createdAt"`
}
type conversationAdmission struct{ fingerprint string }
type conversationAdmissionKey struct{}

func isConversationInboxAdmission(ctx context.Context) bool {
	_, ok := ctx.Value(conversationAdmissionKey{}).(conversationAdmission)
	return ok
}
func isGuidanceDelivery(delivery string) bool {
	return delivery == "guidance" || delivery == "conversation"
}

func conversationPromptText(prompt []map[string]any) string {
	var parts []string
	for _, block := range prompt {
		if block["type"] == "text" {
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func conversationUserDisplayText(msg provider.Message) string {
	text := provider.MessageText(msg)
	for _, part := range msg.ContentParts {
		if part.AttachmentKind == "document_text" || part.AttachmentKind == "attachment_collection" {
			text = strings.ReplaceAll(text, part.Text, "")
		}
	}
	return text
}

// This boundary makes no task-semantic choice. The same immutable user message
// reaches the coordinator model, which chooses whether to answer, revise a
// worker or delegate new work. The ordinary queue is also its durable inbox.
func (s *Server) handleConversationInbox(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	requestID := r.Header.Get(promptRequestIDHeader)
	if requestID == "" {
		requestID = session.NewUUID()
	}
	if !validPromptRequestID(requestID) {
		writeErr(w, http.StatusBadRequest, "invalid prompt request id")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPromptRequestBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read conversation message")
		return
	}
	if int64(len(body)) > maxPromptRequestBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "conversation message is too large")
		return
	}
	fingerprint := sha256.Sum256(body)
	encoded := hex.EncodeToString(fingerprint[:])
	// Serialize admission, not inference. A second delivery cannot enter the
	// runtime twice while its first queue transaction is still being committed.
	sess.conversationInboxMu.Lock()
	defer sess.conversationInboxMu.Unlock()
	sess.mu.Lock()
	existing, duplicate := sess.conversationReceipts[requestID]
	sess.mu.Unlock()
	if duplicate {
		if existing.Fingerprint != encoded {
			writeErr(w, http.StatusConflict, errPromptRequestConflict.Error())
			return
		}
		writeJSON(w, conversationReceiptView(sess, requestID, true))
		return
	}
	copyRequest := r.Clone(context.WithValue(r.Context(), conversationAdmissionKey{}, conversationAdmission{fingerprint: encoded}))
	copyRequest.Header = r.Header.Clone()
	copyRequest.Header.Set(promptRequestIDHeader, requestID)
	copyRequest.Body = io.NopCloser(bytes.NewReader(body))
	s.handleQueueEnqueue(w, copyRequest)
}

// Called in the same lock/atomic queue transaction as the accepted prompt.
// Retain settled identities for the same bounded retry window as prompt IDs;
// never evict an identity whose message still belongs to the pending inbox.
func rememberConversationReceiptLocked(sess *serveSession, ctx context.Context, item *queuedPrompt) error {
	admission, ok := ctx.Value(conversationAdmissionKey{}).(conversationAdmission)
	if !ok {
		return nil
	}
	if sess.conversationReceipts == nil {
		sess.conversationReceipts = map[string]conversationInboxReceipt{}
	}
	active := map[string]bool{}
	for _, queued := range sess.queuedPrompts {
		active[queued.ID] = true
	}
	for requestID, receipt := range sess.conversationReceipts {
		if !active[receipt.MessageID] && time.Since(receipt.CreatedAt) > conversationReceiptRetention {
			delete(sess.conversationReceipts, requestID)
		}
	}
	if len(sess.conversationReceipts) >= maxConversationReceipts {
		return errors.New("too many retained conversation deliveries; wait for the retry window to expire")
	}
	sess.conversationReceipts[item.RequestID] = conversationInboxReceipt{MessageID: item.ID, Fingerprint: admission.fingerprint, CreatedAt: item.CreatedAt}
	return nil
}
func forgetConversationReceiptLocked(sess *serveSession, ctx context.Context, item *queuedPrompt) {
	if isConversationInboxAdmission(ctx) {
		delete(sess.conversationReceipts, item.RequestID)
	}
}
func conversationReceiptView(sess *serveSession, requestID string, duplicate bool) map[string]any {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	receipt := sess.conversationReceipts[requestID]
	return map[string]any{"requestId": requestID, "messageId": receipt.MessageID, "disposition": "accepted", "delivery": "conversation", "duplicate": duplicate, "items": queueViews(sess.queuedPrompts)}
}

func (s *Server) dispatchConversationInbox(sess *serveSession, item *queuedPrompt, msg provider.Message) {
	sess.mu.Lock()
	submitter, capable := sess.activeRuntime.(interface {
		SubmitGuidance(agentcore.Guidance) (agentcore.GuidanceReceipt, error)
	})
	if !capable || item.Status != "pending" {
		sess.mu.Unlock()
		s.startQueuePump(sess)
		return
	}
	item.Status = "guiding"
	if err := sess.persistQueueLocked(); err != nil {
		item.Status = "pending"
		sess.queuePaused = true
		sess.mu.Unlock()
		sess.broadcast("work_center_error", map[string]any{"operation": "admit_conversation", "message": err.Error()})
		return
	}
	sess.mu.Unlock()
	sess.broadcastQueue()
	// This durable inbox row is an explicit human admission even when it joins
	// an existing run. Persist its current trusted policy before the runtime can
	// expose the new message to tools; automatic completion wakes only validate
	// that policy and must never refresh it themselves.
	if s.BackgroundWakeEnabled {
		if err := s.refreshWakeOwnerForUserAdmission(sess); err != nil {
			sess.mu.Lock()
			if item.Status == "guiding" {
				item.Status = "pending"
				item.StartedAt = nil
			}
			sess.queuePaused = true
			queueErr := sess.persistQueueLocked()
			sess.mu.Unlock()
			sess.broadcast("work_center_error", map[string]any{"sessionId": sess.id, "operation": "refresh_conversation_policy", "message": errors.Join(err, queueErr).Error()})
			sess.broadcastQueue()
			return
		}
	}
	_, err := submitter.SubmitGuidance(agentcore.Guidance{ID: item.ID, Text: provider.MessageText(msg), ContentParts: msg.ContentParts, Mode: agentcore.GuidanceSteerCurrent, PreserveText: true})
	if err != nil {
		if restoreErr := restoreGuidanceQueueItem(sess, item.ID); restoreErr != nil {
			sess.broadcast("work_center_error", map[string]any{"operation": "restore_conversation", "message": restoreErr.Error()})
		}
		sess.broadcastQueue()
		s.startQueuePump(sess)
		return
	}
	// This is a new human message, not a declaration that the old task or its
	// pending questions are superseded. Only the model may make that decision.
	update := map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": conversationUserDisplayText(msg)}, "requestId": item.RequestID, "messageId": item.ID, "_steer": true}
	if attachments := messageAttachmentPayload(msg); len(attachments) > 0 {
		update["attachments"] = attachments
	}
	sess.broadcast("session_update", map[string]any{"sessionId": sess.id, "update": update})
}
