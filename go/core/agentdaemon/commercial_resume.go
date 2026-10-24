package agentdaemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/serve"
)

// Durable quota ownership uses the original public conversation and chat ID.
// A resumed Core handle belongs only to this daemon lifetime; never restore it
// from disk or let a client nominate an arbitrary alias.
type commercialResumedSession struct {
	principal string
	chatID    string
	liveID    string
}

func (bridge *commercialBridge) lockSessionResume(conversation string) func() {
	bridge.resumeMu.Lock()
	if bridge.resumeLocks == nil {
		bridge.resumeLocks = make(map[string]*sync.Mutex)
	}
	lock := bridge.resumeLocks[conversation]
	if lock == nil {
		lock = &sync.Mutex{}
		bridge.resumeLocks[conversation] = lock
	}
	bridge.resumeMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (bridge *commercialBridge) resumedInternal(conversation, principal, original string) string {
	bridge.resumeMu.Lock()
	defer bridge.resumeMu.Unlock()
	resumed, ok := bridge.resumedSessions[conversation]
	if ok && resumed.chatID == original && (principal == "" || resumed.principal == principal) {
		return resumed.liveID
	}
	return original
}

func (bridge *commercialBridge) internalSession(conversation, principal string) (string, bool) {
	internal, ok := bridge.store.internalSession(conversation, principal)
	if !ok {
		return "", false
	}
	return bridge.resumedInternal(conversation, principal, internal), true
}

func (store *commercialBindingStore) resumeBinding(chatID, cwd, principal string) (*commercialBinding, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var match *commercialBinding
	for _, binding := range store.bindings {
		if principal == "" || binding.PrincipalKey != principal || binding.Scope != "new_conversation" || binding.WorkspaceCWD != cwd ||
			binding.InternalSessionID == "" || (binding.InternalSessionID != chatID && binding.ConversationID != chatID) {
			continue
		}
		if match != nil {
			return nil, false
		}
		copy := *binding
		match = &copy
	}
	return match, match != nil
}

func (bridge *commercialBridge) handleSessionResume(writer http.ResponseWriter, request *http.Request, next http.Handler) {
	principal, _, err := bridge.principalAccess(request.Context())
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	var body struct {
		CWD                 string `json:"cwd"`
		ChatID              string `json:"chatId"`
		ContinueInterrupted *bool  `json:"continueInterrupted,omitempty"`
		ApprovalChannel     string `json:"approvalChannel,omitempty"`
	}
	if decodeJSON(request, &body, true) != nil || !validCommercialID(body.ChatID) || body.CWD == "" {
		writeCommercialRequestError(writer)
		return
	}
	binding, ok := bridge.store.resumeBinding(body.ChatID, body.CWD, principal)
	if !ok {
		writeCommercialConflict(writer, "unknown_conversation")
		return
	}
	unlock := bridge.lockSessionResume(binding.ConversationID)
	defer unlock()
	if request.Context().Err() != nil {
		writeCommercialError(writer, request.Context().Err())
		return
	}
	current, _, err := bridge.principalAccess(request.Context())
	if err != nil || current != principal {
		writeCommercialConflict(writer, "authorization_binding_mismatch")
		return
	}
	bridge.resumeMu.Lock()
	previous, reused := bridge.resumedSessions[binding.ConversationID]
	bridge.resumeMu.Unlock()
	preferred := binding.InternalSessionID
	if reused {
		if previous.principal != principal || previous.chatID != binding.InternalSessionID {
			writeCommercialConflict(writer, "authorization_binding_mismatch")
			return
		}
		preferred = previous.liveID
	}
	// The original durable ID may have no live handle after daemon restart.
	// Once this daemon publishes a handle, resuming must reuse it even while
	// idle: a concurrent prompt must never lose its controls/cancel target.
	request = request.WithContext(serve.WithPreferredResumeSessionID(request.Context(), preferred, !reused))
	body.ChatID = binding.InternalSessionID
	encoded, err := json.Marshal(body)
	if err != nil {
		writeCommercialRequestError(writer)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(encoded))
	request.ContentLength = int64(len(encoded))
	buffer := newCommercialResponse()
	next.ServeHTTP(buffer, request)
	if buffer.err != nil {
		writeCommercialError(writer, buffer.err)
		return
	}
	if buffer.status < 200 || buffer.status >= 300 {
		buffer.commit(writer)
		return
	}
	var value map[string]json.RawMessage
	var liveID, chatID string
	if json.Unmarshal(buffer.body.Bytes(), &value) != nil || json.Unmarshal(value["sessionId"], &liveID) != nil ||
		json.Unmarshal(value["chatId"], &chatID) != nil || !validCommercialID(liveID) || chatID != binding.InternalSessionID || (reused && liveID != preferred) {
		writeCommercialGatewayError(writer)
		return
	}
	current, _, err = bridge.principalAccess(request.Context())
	if err != nil || current != principal {
		writeCommercialConflict(writer, "authorization_binding_mismatch")
		return
	}
	if currentBinding, ok := bridge.store.resumeBinding(chatID, body.CWD, principal); !ok || currentBinding.AuthorizationID != binding.AuthorizationID {
		writeCommercialConflict(writer, "unknown_conversation")
		return
	}
	bridge.resumeMu.Lock()
	if bridge.resumedSessions == nil {
		bridge.resumedSessions = make(map[string]commercialResumedSession)
	}
	bridge.resumedSessions[binding.ConversationID] = commercialResumedSession{principal: principal, chatID: chatID, liveID: liveID}
	bridge.resumeMu.Unlock()
	value["sessionId"], _ = json.Marshal(binding.ConversationID)
	buffer.body.Reset()
	_ = json.NewEncoder(&buffer.body).Encode(value)
	buffer.header.Del("Content-Length")
	buffer.commit(writer)
}
