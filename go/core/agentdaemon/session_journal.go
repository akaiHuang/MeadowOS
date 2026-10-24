package agentdaemon

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/checkpoint"
	"github.com/hey-meadow/meadow-code/go/core/serve"
)

const maxPromptResponseBytes = 1 << 20

type sessionJournal struct {
	root   string
	mu     sync.Mutex
	active map[string]string
}

func (journal *sessionJournal) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sessionID, action, ok := journalSessionRoute(request.URL.Path)
		if !ok {
			next.ServeHTTP(writer, request)
			return
		}
		switch {
		case request.Method == http.MethodPost && action == "prompt":
			journal.handlePrompt(writer, request, next, sessionID)
		case request.Method == http.MethodGet && action == "events":
			runID, err := journal.activeRun(sessionID)
			if err != nil {
				writeError(writer, http.StatusServiceUnavailable, err.Error())
				return
			}
			wrapped := &checkpointEventWriter{ResponseWriter: writer, journal: journal, sessionID: sessionID, runID: runID}
			next.ServeHTTP(wrapped, request)
		case request.Method == http.MethodPost && action == "cancel":
			if err := journal.cancelActive(sessionID); err != nil {
				writeError(writer, http.StatusServiceUnavailable, err.Error())
				return
			}
			next.ServeHTTP(writer, request)
		default:
			next.ServeHTTP(writer, request)
		}
	})
}

func (journal *sessionJournal) handlePrompt(writer http.ResponseWriter, request *http.Request, next http.Handler, sessionID string) {
	if serve.IsPromptReplayOnly(request.Context()) {
		// A server-authorized transport retry belongs to the original durable
		// run. Core either returns that exact result or fails closed; starting
		// another journal run would block an active retry and leave a dangling
		// run when an old result has been evicted. The outer account and
		// configuration gates still apply before this wrapper.
		next.ServeHTTP(writer, request)
		return
	}
	runID, err := journal.beginRun(sessionID)
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	buffered := newJournalResponse()
	next.ServeHTTP(buffered, request)
	if buffered.err != nil {
		_ = journal.failRun(sessionID, runID, buffered.err.Error())
		writeError(writer, http.StatusServiceUnavailable, "daemon response exceeded checkpoint boundary")
		return
	}
	if buffered.body.Len() != 0 && buffered.status >= 200 && buffered.status < 300 {
		var response struct {
			StopReason   string `json:"stopReason"`
			CheckpointID string `json:"checkpointId"`
		}
		if json.Unmarshal(buffered.body.Bytes(), &response) == nil && strings.TrimSpace(response.StopReason) != "" {
			if err := journal.finishRun(sessionID, runID, response.StopReason, response.CheckpointID); err != nil {
				writeError(writer, http.StatusServiceUnavailable, err.Error())
				return
			}
		}
	}
	buffered.commit(writer)
}

func (journal *sessionJournal) beginRun(sessionID string) (string, error) {
	if !validJournalIdentifier(sessionID) {
		return "", errors.New("session id is not safe for checkpoint storage")
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if existing, err := journal.activeRunLocked(sessionID); err != nil {
		return "", err
	} else if existing != "" {
		return "", errors.New("session already has an incomplete durable run")
	}
	random, err := randomJournalID()
	if err != nil {
		return "", err
	}
	runID := fmt.Sprintf("run-%020d-%s", time.Now().UTC().UnixNano(), random)
	event := checkpoint.Event{
		ID:   "turn-start-" + random,
		Kind: checkpoint.EventTurn,
		At:   time.Now().UTC(),
		Turn: &checkpoint.TurnState{TurnID: runID, Status: checkpoint.TurnRunning, Summary: "daemon prompt accepted"},
	}
	if err := journal.appendLocked(sessionID, runID, event); err != nil {
		return "", err
	}
	journal.active[sessionID] = runID
	return runID, nil
}

func (journal *sessionJournal) cancelActive(sessionID string) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	runID, err := journal.activeRunLocked(sessionID)
	if err != nil || runID == "" {
		return err
	}
	return journal.withStoreLocked(sessionID, runID, func(store *checkpoint.Store, state checkpoint.State) error {
		if state.Cancellation != nil || state.Terminal != nil {
			return nil
		}
		random, randomErr := randomJournalID()
		if randomErr != nil {
			return randomErr
		}
		_, appendErr := store.Append(checkpoint.Event{
			ID:           "cancel-" + random,
			Kind:         checkpoint.EventCancel,
			At:           time.Now().UTC(),
			Cancellation: &checkpoint.CancellationState{Source: "daemon", Reason: "session cancel requested"},
		})
		return appendErr
	})
}

func (journal *sessionJournal) finishRun(sessionID, runID, stopReason, checkpointID string) error {
	outcome := checkpoint.TerminalCompleted
	turnStatus := checkpoint.TurnCompleted
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "cancelled", "canceled":
		outcome = checkpoint.TerminalCancelled
		turnStatus = checkpoint.TurnCancelled
	case "error", "failed", "failure":
		outcome = checkpoint.TerminalFailed
		turnStatus = checkpoint.TurnFailed
	}
	return journal.terminalRun(sessionID, runID, outcome, turnStatus, stableCheckpointSummary(checkpointID, stopReason), time.Now().UTC(), "response")
}

func (journal *sessionJournal) failRun(sessionID, runID, summary string) error {
	return journal.terminalRun(sessionID, runID, checkpoint.TerminalFailed, checkpoint.TurnFailed, summary, time.Now().UTC(), "failure")
}

func (journal *sessionJournal) terminalRun(sessionID, runID string, outcome checkpoint.TerminalOutcome, turnStatus checkpoint.TurnStatus, summary string, at time.Time, eventSuffix string) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	err := journal.withStoreLocked(sessionID, runID, func(store *checkpoint.Store, state checkpoint.State) error {
		if state.Terminal != nil {
			if state.Terminal.Outcome == outcome {
				return nil
			}
			return checkpoint.ErrTerminalAlreadySet
		}
		if state.CurrentTurn == nil || state.CurrentTurn.TurnID != runID {
			return errors.New("checkpoint current turn does not match active run")
		}
		turnEvent := checkpoint.Event{
			ID:   "turn-terminal-" + eventSuffix,
			Kind: checkpoint.EventTurn,
			At:   at,
			Turn: &checkpoint.TurnState{TurnID: runID, Status: turnStatus, Summary: checkpointSummary(summary)},
		}
		if _, appendErr := store.Append(turnEvent); appendErr != nil {
			return appendErr
		}
		_, appendErr := store.Append(checkpoint.Event{
			ID:       "terminal-" + eventSuffix,
			Kind:     checkpoint.EventTerminal,
			At:       at,
			Terminal: &checkpoint.TerminalState{Outcome: outcome, Summary: checkpointSummary(summary), At: at},
		})
		return appendErr
	})
	if err == nil {
		delete(journal.active, sessionID)
	}
	return err
}

func (journal *sessionJournal) appendSSE(sessionID, runID string, eventID int64, eventType string, data json.RawMessage) error {
	if runID == "" || eventID <= 0 {
		return nil
	}
	stableTime := time.Unix(0, eventID).UTC()
	var payload map[string]any
	if len(data) != 0 && json.Unmarshal(data, &payload) != nil {
		return errors.New("daemon SSE event contains invalid JSON")
	}
	switch eventType {
	case "stable_checkpoint":
		checkpointID, _ := payload["checkpointId"].(string)
		if !validJournalIdentifier(checkpointID) {
			return errors.New("daemon stable checkpoint identity is invalid")
		}
		return journal.recordStableCheckpoint(sessionID, runID, checkpointID, stableTime, eventID)
	case "turn_completed":
		stopReason, _ := payload["stopReason"].(string)
		checkpointID, _ := payload["checkpointId"].(string)
		outcome := checkpoint.TerminalCompleted
		status := checkpoint.TurnCompleted
		if strings.EqualFold(stopReason, "cancelled") || strings.EqualFold(stopReason, "canceled") {
			outcome = checkpoint.TerminalCancelled
			status = checkpoint.TurnCancelled
		} else if strings.EqualFold(stopReason, "error") || strings.EqualFold(stopReason, "failed") {
			outcome = checkpoint.TerminalFailed
			status = checkpoint.TurnFailed
		}
		return journal.terminalRun(sessionID, runID, outcome, status, stableCheckpointSummary(checkpointID, stopReason), stableTime, "sse-"+strconv.FormatInt(eventID, 10))
	case "stream_error":
		message, _ := payload["message"].(string)
		return journal.terminalRun(sessionID, runID, checkpoint.TerminalFailed, checkpoint.TurnFailed, message, stableTime, "sse-"+strconv.FormatInt(eventID, 10))
	default:
		return nil
	}
}

func (journal *sessionJournal) recordStableCheckpoint(sessionID, runID, checkpointID string, at time.Time, eventID int64) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.withStoreLocked(sessionID, runID, func(store *checkpoint.Store, state checkpoint.State) error {
		if state.Terminal != nil {
			return nil
		}
		if state.CurrentTurn == nil || state.CurrentTurn.TurnID != runID {
			return errors.New("checkpoint current turn does not match active run")
		}
		_, err := store.Append(checkpoint.Event{
			ID: "stable-" + strconv.FormatInt(eventID, 10), Kind: checkpoint.EventTurn, At: at,
			Turn: &checkpoint.TurnState{TurnID: runID, Status: checkpoint.TurnRunning, Summary: stableCheckpointSummary(checkpointID, "running")},
		})
		return err
	})
}

func (journal *sessionJournal) activeRun(sessionID string) (string, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.activeRunLocked(sessionID)
}

func (journal *sessionJournal) activeRunLocked(sessionID string) (string, error) {
	if runID := journal.active[sessionID]; runID != "" {
		return runID, nil
	}
	result, runID, err := journal.recoverLatestLocked(sessionID)
	if err != nil || runID == "" || result.State.Terminal != nil {
		return "", err
	}
	journal.active[sessionID] = runID
	return runID, nil
}

func (journal *sessionJournal) recoverLatestLocked(sessionID string) (checkpoint.RecoveryResult, string, error) {
	if !validJournalIdentifier(sessionID) {
		return checkpoint.RecoveryResult{}, "", errors.New("invalid checkpoint session id")
	}
	directory := filepath.Join(journal.root, sessionID)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return checkpoint.RecoveryResult{}, "", nil
	}
	if err != nil {
		return checkpoint.RecoveryResult{}, "", fmt.Errorf("read checkpoint session: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return checkpoint.RecoveryResult{}, "", errors.New("checkpoint session contains a symlink")
		}
		if entry.IsDir() && validJournalIdentifier(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) == 0 {
		return checkpoint.RecoveryResult{}, "", nil
	}
	runID := names[0]
	result, err := checkpoint.Recover(filepath.Join(directory, runID), runID, checkpoint.DefaultOptions())
	return result, runID, err
}

func (journal *sessionJournal) Recovery(sessionID string) (checkpoint.RecoveryResult, string, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.recoverLatestLocked(sessionID)
}

func (journal *sessionJournal) appendLocked(sessionID, runID string, event checkpoint.Event) error {
	return journal.withStoreLocked(sessionID, runID, func(store *checkpoint.Store, _ checkpoint.State) error {
		_, err := store.Append(event)
		return err
	})
}

func (journal *sessionJournal) withStoreLocked(sessionID, runID string, action func(*checkpoint.Store, checkpoint.State) error) error {
	if !validJournalIdentifier(sessionID) || !validJournalIdentifier(runID) {
		return errors.New("unsafe checkpoint identifier")
	}
	directory := filepath.Join(journal.root, sessionID, runID)
	store, recovery, err := checkpoint.Open(directory, runID, checkpoint.DefaultOptions())
	if err != nil {
		return err
	}
	actionErr := action(store, recovery.State)
	if actionErr == nil {
		_, actionErr = store.SaveSnapshot()
	}
	return errors.Join(actionErr, store.Close())
}

func (server *serverHandler) handleCheckpointRecovery(writer http.ResponseWriter, request *http.Request) {
	result, runID, err := server.journal.Recovery(request.PathValue("session"))
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, err.Error())
		return
	}
	if runID == "" {
		writeError(writer, http.StatusNotFound, "no durable checkpoint for session")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"run_id": runID, "stable_checkpoint_id": recoveryStableCheckpointID(result.State), "recovery": result})
}

type checkpointEventWriter struct {
	http.ResponseWriter
	journal   *sessionJournal
	sessionID string
	runID     string
	pending   bytes.Buffer
}

func (writer *checkpointEventWriter) Write(payload []byte) (int, error) {
	writer.pending.Write(payload)
	for {
		raw := writer.pending.String()
		boundary := strings.Index(raw, "\n\n")
		if boundary < 0 {
			break
		}
		frame := raw[:boundary]
		writer.pending.Next(boundary + 2)
		if err := writer.persistFrame(frame); err != nil {
			return 0, err
		}
	}
	return writer.ResponseWriter.Write(payload)
}

func (writer *checkpointEventWriter) persistFrame(frame string) error {
	var eventID int64
	var eventType string
	var data json.RawMessage
	for _, line := range strings.Split(frame, "\n") {
		switch {
		case strings.HasPrefix(line, "id: "):
			eventID, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64)
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			data = append(data[:0], strings.TrimSpace(strings.TrimPrefix(line, "data: "))...)
		}
	}
	return writer.journal.appendSSE(writer.sessionID, writer.runID, eventID, eventType, data)
}

func (writer *checkpointEventWriter) Flush() {
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *checkpointEventWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

type journalResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	err    error
}

func newJournalResponse() *journalResponse {
	return &journalResponse{header: make(http.Header), status: http.StatusOK}
}

func (response *journalResponse) Header() http.Header { return response.header }

func (response *journalResponse) WriteHeader(status int) {
	if response.status == http.StatusOK {
		response.status = status
	}
}

func (response *journalResponse) Write(payload []byte) (int, error) {
	if response.body.Len()+len(payload) > maxPromptResponseBytes {
		response.err = errors.New("prompt response exceeds journal boundary")
		return 0, response.err
	}
	return response.body.Write(payload)
}

func (response *journalResponse) commit(writer http.ResponseWriter) {
	for name, values := range response.header {
		writer.Header()[name] = append([]string(nil), values...)
	}
	writer.WriteHeader(response.status)
	_, _ = io.Copy(writer, &response.body)
}

func journalSessionRoute(path string) (string, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "session" || !validJournalIdentifier(parts[1]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func validJournalIdentifier(value string) bool {
	if value == "" || len(value) > 160 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func randomJournalID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate checkpoint id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func checkpointSummary(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

func stableCheckpointSummary(checkpointID, detail string) string {
	checkpointID = strings.TrimSpace(checkpointID)
	detail = strings.TrimSpace(detail)
	if checkpointID == "" {
		return checkpointSummary(detail)
	}
	return checkpointSummary("stable_checkpoint=" + checkpointID + "; " + detail)
}

func recoveryStableCheckpointID(state checkpoint.State) string {
	summary := ""
	if state.Terminal != nil {
		summary = state.Terminal.Summary
	}
	if summary == "" && state.CurrentTurn != nil {
		summary = state.CurrentTurn.Summary
	}
	const prefix = "stable_checkpoint="
	start := strings.Index(summary, prefix)
	if start < 0 {
		return ""
	}
	value := summary[start+len(prefix):]
	if end := strings.IndexByte(value, ';'); end >= 0 {
		value = value[:end]
	}
	value = strings.TrimSpace(value)
	if !validJournalIdentifier(value) {
		return ""
	}
	return value
}
