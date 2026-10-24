package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// BackgroundWakeRequest is an internal trusted-tool delivery, not a public
// prompt endpoint. ID is stable across retries of one occurrence. OriginatedAt
// names the original operation/create/resume admission, never completion time.
type BackgroundWakeRequest struct {
	ID           string    `json:"id"`
	Source       string    `json:"source"`
	ChatID       string    `json:"chatId"`
	WorkspaceCWD string    `json:"workspaceCwd"`
	ConnectionID string    `json:"connectionId"`
	OriginatedAt time.Time `json:"originatedAt"`
	Observation  string    `json:"observation"`
}

type BackgroundWakeReceipt struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	QueueID        string `json:"queueId,omitempty"`
	RunID          string `json:"runId,omitempty"`
	StopReason     string `json:"stopReason,omitempty"`
	RepairRequired bool   `json:"repairRequired,omitempty"`
}

type durableWakeRecord struct {
	Request    BackgroundWakeRequest `json:"request"`
	Status     string                `json:"status"`
	QueueID    string                `json:"queueId"`
	RunID      string                `json:"runId,omitempty"`
	StopReason string                `json:"stopReason,omitempty"`
	CreatedAt  time.Time             `json:"createdAt"`
	UpdatedAt  time.Time             `json:"updatedAt"`
}

// This owner charter is written by serve before exposing model tools. A due
// event cannot invent a more privileged approval channel/toolset after restart.
type durableWakeDocument struct {
	Version         int                           `json:"version"`
	ChatID          string                        `json:"chatId"`
	WorkspaceCWD    string                        `json:"workspaceCwd"`
	ConnectionID    string                        `json:"connectionId"`
	ModelID         string                        `json:"modelId"`
	EngineMode      engine.ApprovalMode           `json:"engineMode"`
	ApprovalChannel sessionApprovalChannel        `json:"approvalChannel"`
	DisabledTools   []string                      `json:"disabledTools,omitempty"`
	Events          map[string]*durableWakeRecord `json:"events"`
}

func wakeHash(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func wakeRecordKey(req BackgroundWakeRequest) string { return wakeHash(req.Source, req.ID) }
func wakeStoreDirectory() string {
	return filepath.Join(session.PrimaryDir(), "background-wakes", "v1")
}
func wakeDocumentPath(req BackgroundWakeRequest) string {
	return filepath.Join(wakeStoreDirectory(), wakeHash(req.ConnectionID, req.WorkspaceCWD, req.ChatID)+".json")
}
func wakeSessionRequest(sess *serveSession) BackgroundWakeRequest {
	return BackgroundWakeRequest{ChatID: queueChatID(sess), WorkspaceCWD: sess.cwd, ConnectionID: sess.connectionID}
}

func validateWakeRequest(req BackgroundWakeRequest, cancel bool) error {
	if strings.TrimSpace(req.ID) == "" || len(req.ID) > 256 || strings.TrimSpace(req.Source) == "" || len(req.Source) > 80 || req.ChatID == "" || !filepath.IsAbs(req.WorkspaceCWD) {
		return errors.New("invalid_background_wake_identity")
	}
	if _, err := session.ChatPath(req.WorkspaceCWD, req.ChatID); err != nil {
		return err
	}
	if !cancel && (req.OriginatedAt.IsZero() || strings.TrimSpace(req.Observation) == "" || len(req.Observation) > 256<<10) {
		return errors.New("invalid_background_wake_observation")
	}
	return nil
}

func loadWakeDocument(path string) (*durableWakeDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 16<<20 {
		return nil, errors.New("background_wake_journal_too_large")
	}
	var doc durableWakeDocument
	if err = json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Version != 1 || doc.ChatID == "" || !filepath.IsAbs(doc.WorkspaceCWD) || doc.ModelID == "" || (doc.ApprovalChannel != approvalChannelInteractive && doc.ApprovalChannel != approvalChannelNone) {
		return nil, errors.New("invalid_background_wake_owner")
	}
	if doc.Events == nil {
		doc.Events = map[string]*durableWakeRecord{}
	}
	if wakeDocumentPath(BackgroundWakeRequest{ChatID: doc.ChatID, WorkspaceCWD: doc.WorkspaceCWD, ConnectionID: doc.ConnectionID}) != path {
		return nil, errors.New("background_wake_owner_mismatch")
	}
	for key, record := range doc.Events {
		if record == nil || key != wakeRecordKey(record.Request) || record.QueueID != "wake_"+key[:32] || record.Request.ChatID != doc.ChatID || record.Request.WorkspaceCWD != doc.WorkspaceCWD || record.Request.ConnectionID != doc.ConnectionID {
			return nil, errors.New("invalid_background_wake_event")
		}
		switch record.Status {
		case "pending", "accepted", "running", "completed", "cancelled", "interrupted":
		default:
			return nil, errors.New("invalid_background_wake_status")
		}
		if err := validateWakeRequest(record.Request, record.Status == "cancelled"); err != nil {
			return nil, err
		}
	}
	return &doc, nil
}

func persistWakeDocument(path string, doc *durableWakeDocument) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("background_wake_journal_capacity")
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".wake-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func (s *Server) ensureWakeOwner(sess *serveSession) error {
	return s.withWakeOwnerPolicy(sess, false)
}

// Refresh is reserved for admitted user input. Completion/recovery paths only
// validate this charter and cannot silently authorize a policy change.
func (s *Server) refreshWakeOwnerForUserAdmission(sess *serveSession) error {
	return s.withWakeOwnerPolicy(sess, true)
}

func (s *Server) withWakeOwnerPolicy(sess *serveSession, refresh bool) error {
	if sess == nil {
		return errors.New("background_wake_owner_unavailable")
	}
	sess.approvalModeMu.Lock()
	defer sess.approvalModeMu.Unlock()
	return s.saveWakeOwner(sess, refresh)
}

// Caller holds approvalModeMu; keep the existing session/store lock order.
func (s *Server) saveWakeOwner(sess *serveSession, refresh bool) error {
	if sess == nil || sess.engine == nil {
		return errors.New("background_wake_owner_unavailable")
	}
	sess.mu.Lock()
	modelID, engineMode, approval := sess.engine.ModelID, sess.engine.Mode, sess.effectiveApprovalChannel()
	disabled := append([]string(nil), sess.disabledTools...)
	req := wakeSessionRequest(sess)
	sess.mu.Unlock()
	if _, err := engine.ParseApprovalMode(string(engineMode)); err != nil {
		return err
	}
	if req.ConnectionID != s.CurrentConnectionID() {
		return errors.New("background_wake_connection_unavailable")
	}
	path := wakeDocumentPath(req)
	s.wakeStoreMu.Lock()
	defer s.wakeStoreMu.Unlock()
	doc, err := loadWakeDocument(path)
	if err == nil {
		if doc.ChatID != req.ChatID || doc.WorkspaceCWD != req.WorkspaceCWD || doc.ConnectionID != req.ConnectionID {
			return errors.New("background_wake_owner_mismatch")
		}
		if refresh {
			doc.ModelID, doc.EngineMode, doc.ApprovalChannel, doc.DisabledTools = modelID, engineMode, approval, disabled
			return persistWakeDocument(path, doc)
		}
		if doc.ModelID != modelID || doc.EngineMode != engineMode || doc.ApprovalChannel != approval || !sameWakeStrings(doc.DisabledTools, disabled) {
			return errors.New("background_wake_owner_configuration_changed")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	doc = &durableWakeDocument{Version: 1, ChatID: req.ChatID, WorkspaceCWD: req.WorkspaceCWD, ConnectionID: req.ConnectionID, ModelID: modelID, EngineMode: engineMode, ApprovalChannel: approval, DisabledTools: disabled, Events: map[string]*durableWakeRecord{}}
	return persistWakeDocument(path, doc)
}
func sameWakeStrings(a, b []string) bool {
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	return strings.Join(aa, "\x00") == strings.Join(bb, "\x00")
}
func wakeReceipt(rec *durableWakeRecord) BackgroundWakeReceipt {
	return BackgroundWakeReceipt{ID: rec.Request.ID, Status: rec.Status, QueueID: rec.QueueID, RunID: rec.RunID, StopReason: rec.StopReason, RepairRequired: rec.Status == "interrupted"}
}

func (s *Server) recordBackgroundWake(req BackgroundWakeRequest, cancel bool) (BackgroundWakeReceipt, error) {
	if err := validateWakeRequest(req, cancel); err != nil {
		return BackgroundWakeReceipt{}, err
	}
	s.wakeStoreMu.Lock()
	defer s.wakeStoreMu.Unlock()
	path := wakeDocumentPath(req)
	doc, err := loadWakeDocument(path)
	if err != nil {
		return BackgroundWakeReceipt{}, fmt.Errorf("background_wake_owner_unavailable: %w", err)
	}
	key := wakeRecordKey(req)
	now := time.Now().UTC()
	if existing := doc.Events[key]; existing != nil {
		if existing.Request.ID != req.ID || existing.Request.Source != req.Source {
			return BackgroundWakeReceipt{}, errors.New("background_wake_identity_conflict")
		}
		if cancel {
			if existing.Status == "pending" || existing.Status == "accepted" {
				existing.Status = "cancelled"
				existing.StopReason = "cancelled"
				existing.UpdatedAt = now
				if err = persistWakeDocument(path, doc); err != nil {
					return BackgroundWakeReceipt{}, err
				}
			}
			return wakeReceipt(existing), nil
		}
		if existing.Status != "cancelled" && (!existing.Request.OriginatedAt.Equal(req.OriginatedAt) || existing.Request.Observation != req.Observation) {
			return BackgroundWakeReceipt{}, errors.New("background_wake_payload_conflict")
		}
		return wakeReceipt(existing), nil
	}
	record := &durableWakeRecord{Request: req, Status: "pending", QueueID: "wake_" + key[:32], CreatedAt: now, UpdatedAt: now}
	if cancel {
		record.Status = "cancelled"
		record.StopReason = "cancelled"
	}
	doc.Events[key] = record
	if err = persistWakeDocument(path, doc); err != nil {
		return BackgroundWakeReceipt{}, err
	}
	return wakeReceipt(record), nil
}

// EnqueueBackgroundWake returns only after the event is durable. Repeating the
// same occurrence returns its existing receipt; it never creates another turn.
func (s *Server) EnqueueBackgroundWake(ctx context.Context, req BackgroundWakeRequest) (BackgroundWakeReceipt, error) {
	if err := ctx.Err(); err != nil {
		return BackgroundWakeReceipt{}, err
	}
	if !s.BackgroundWakeEnabled {
		return BackgroundWakeReceipt{}, errors.New("background_wake_unavailable")
	}
	if owner := s.findWakeSession(req); owner != nil {
		if err := s.ensureWakeOwner(owner); err != nil {
			return BackgroundWakeReceipt{}, err
		}
	}
	receipt, err := s.recordBackgroundWake(req, false)
	if err != nil {
		return receipt, err
	}
	if owner := s.findWakeSession(req); owner != nil {
		s.startBackgroundWatcher(owner)
		s.signalWake(owner)
	}
	s.startWakeRestorer()
	return receipt, nil
}

func (s *Server) CancelBackgroundWake(ctx context.Context, req BackgroundWakeRequest) (BackgroundWakeReceipt, error) {
	if err := ctx.Err(); err != nil {
		return BackgroundWakeReceipt{}, err
	}
	if owner := s.findWakeSession(req); owner != nil {
		if err := s.ensureWakeOwner(owner); err != nil {
			return BackgroundWakeReceipt{}, err
		}
	}
	receipt, err := s.recordBackgroundWake(req, true)
	if err != nil {
		return receipt, err
	}
	if owner := s.findWakeSession(req); owner != nil {
		s.signalWake(owner)
	}
	return receipt, nil
}

func (s *Server) findWakeSession(req BackgroundWakeRequest) *serveSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, candidate := range s.sessions {
		if candidate.cwd == req.WorkspaceCWD && queueChatID(candidate) == req.ChatID && candidate.connectionID == req.ConnectionID {
			return candidate
		}
	}
	return nil
}
func (s *Server) signalWake(sess *serveSession) {
	sess.mu.Lock()
	signal := sess.wakeSignal
	sess.mu.Unlock()
	if signal != nil {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
}

func (s *Server) wakeRecords(sess *serveSession) ([]*durableWakeRecord, error) {
	s.wakeStoreMu.Lock()
	defer s.wakeStoreMu.Unlock()
	doc, err := loadWakeDocument(wakeDocumentPath(wakeSessionRequest(sess)))
	if err != nil {
		return nil, err
	}
	result := make([]*durableWakeRecord, 0, len(doc.Events))
	for _, record := range doc.Events {
		copy := *record
		result = append(result, &copy)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (s *Server) setWakeState(sess *serveSession, key, status, runID, stop string) error {
	s.wakeStoreMu.Lock()
	defer s.wakeStoreMu.Unlock()
	path := wakeDocumentPath(wakeSessionRequest(sess))
	doc, err := loadWakeDocument(path)
	if err != nil {
		return err
	}
	record := doc.Events[key]
	if record == nil {
		return errors.New("background_wake_event_missing")
	}
	// Terminal receipt is authoritative even if a prepared/running queue row
	// survived a crash or an earlier watcher snapshot is delivered much later.
	if record.Status == "completed" || record.Status == "cancelled" || record.Status == "interrupted" {
		if status == record.Status {
			return nil
		}
		return errors.New("background_wake_already_terminal")
	}
	record.Status = status
	record.RunID = runID
	record.StopReason = stop
	record.UpdatedAt = time.Now().UTC()
	if err = persistWakeDocument(path, doc); err != nil {
		return err
	}
	return nil
}

func wakeStatusView(sess *serveSession, record *durableWakeRecord) map[string]any {
	return map[string]any{"sessionId": sess.id, "chatId": queueChatID(sess), "wakeId": wakeRecordKey(record.Request), "source": record.Request.Source, "status": record.Status, "repairRequired": record.Status == "interrupted", "actor": "runtime"}
}
func (s *Server) publishWakeStatus(sess *serveSession, record *durableWakeRecord) {
	sess.broadcast("background_wake_status", wakeStatusView(sess, record))
}
func wakeStatusSnapshots(sess *serveSession) []map[string]any {
	doc, err := loadWakeDocument(wakeDocumentPath(wakeSessionRequest(sess)))
	if err != nil {
		return nil
	}
	records := make([]*durableWakeRecord, 0, len(doc.Events))
	for _, record := range doc.Events {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].UpdatedAt.After(records[j].UpdatedAt) })
	if len(records) > 64 {
		records = records[:64]
	}
	views := make([]map[string]any, 0, len(records))
	for _, record := range records {
		views = append(views, wakeStatusView(sess, record))
	}
	return views
}

// RestoreBackgroundWakes starts a metadata-only retry owner. It does not create
// a model request itself. The ordinary per-chat worker owns all turn admission.
func (s *Server) RestoreBackgroundWakes(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.BackgroundWakeEnabled {
		return nil
	}
	s.startWakeRestorer()
	return s.restoreWakeOwners(ctx)
}
func (s *Server) startWakeRestorer() {
	ctx := s.backgroundWakeContext()
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	if s.wakeClosed || s.wakeRestorerStarted {
		return
	}
	s.wakeRestorerStarted = true
	s.wakeWorkers.Add(1)
	go func() {
		defer s.wakeWorkers.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = s.restoreWakeOwners(ctx)
			}
		}
	}()
}
func (s *Server) restoreWakeOwners(ctx context.Context) error {
	s.wakeRestoreMu.Lock()
	defer s.wakeRestoreMu.Unlock()
	paths, err := filepath.Glob(filepath.Join(wakeStoreDirectory(), "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.wakeStoreMu.Lock()
		doc, readErr := loadWakeDocument(path)
		s.wakeStoreMu.Unlock()
		if readErr != nil {
			continue
		}
		active := false
		for _, record := range doc.Events {
			if record.Status == "pending" || record.Status == "accepted" || record.Status == "running" {
				active = true
				break
			}
		}
		if !active && s.ConversationInboxEnabled {
			active = tools.HostedBackgroundTasksNeedRecovery(nativeWorkerPath(BackgroundWakeRequest{ChatID: doc.ChatID, WorkspaceCWD: doc.WorkspaceCWD, ConnectionID: doc.ConnectionID}))
		}
		if !active {
			continue
		}
		req := BackgroundWakeRequest{ChatID: doc.ChatID, WorkspaceCWD: doc.WorkspaceCWD, ConnectionID: doc.ConnectionID}
		if owner := s.findWakeSession(req); owner != nil {
			s.startBackgroundWatcher(owner)
			s.signalWake(owner)
			continue
		}
		if doc.ConnectionID != s.CurrentConnectionID() {
			continue
		}
		owner, restoreErr := s.restoreWakeSession(ctx, doc)
		if restoreErr != nil {
			if !errors.Is(restoreErr, errWakeRecoveryRequired) {
				continue
			}
			s.wakeStoreMu.Lock()
			current, loadErr := loadWakeDocument(path)
			if loadErr == nil {
				for _, record := range current.Events {
					if record.Status == "pending" || record.Status == "accepted" || record.Status == "running" {
						record.Status = "interrupted"
						record.StopReason = "original_chat_recovery_required"
						record.UpdatedAt = time.Now().UTC()
					}
				}
				_ = persistWakeDocument(path, current)
			}
			s.wakeStoreMu.Unlock()
			continue
		}
		s.startBackgroundWatcher(owner)
		s.signalWake(owner)
	}
	return nil
}

// Called while sess.mu is held immediately before cancellation ownership is
// installed. This is the last tombstone/cutoff check before any model execution.
func (s *Server) beginDurableWakeRunLocked(sess *serveSession, item *queuedPrompt) error {
	if item.WakeID == "" {
		return nil
	}
	s.wakeStoreMu.Lock()
	defer s.wakeStoreMu.Unlock()
	path := wakeDocumentPath(wakeSessionRequest(sess))
	doc, err := loadWakeDocument(path)
	if err != nil {
		return err
	}
	record := doc.Events[item.WakeID]
	if record == nil || record.QueueID != item.ID {
		return errors.New("background_wake_queue_mismatch")
	}
	if record.Status == "cancelled" || (sess.wakeCancelledBefore != nil && !record.Request.OriginatedAt.After(*sess.wakeCancelledBefore)) {
		record.Status = "cancelled"
		record.StopReason = "cancelled"
		record.UpdatedAt = time.Now().UTC()
		if err = persistWakeDocument(path, doc); err != nil {
			return err
		}
		item.Status = "cancelling"
		return nil
	}
	if record.Status != "accepted" {
		return errors.New("background_wake_not_accepted")
	}
	record.Status = "running"
	record.UpdatedAt = time.Now().UTC()
	if err = persistWakeDocument(path, doc); err != nil {
		return err
	}
	// No raw observation is presented as a human message.
	go s.publishWakeStatus(sess, record)
	return nil
}
func (s *Server) finishDurableWakeRun(sess *serveSession, item *queuedPrompt, runID, stop string) error {
	if item.WakeID == "" {
		return nil
	}
	status := "interrupted"
	switch stop {
	case "end_turn", "completed":
		status = "completed"
	case "cancelled":
		status = "cancelled"
	}
	if err := s.setWakeState(sess, item.WakeID, status, runID, stop); err != nil {
		return err
	}
	records, err := s.wakeRecords(sess)
	if err != nil {
		return err
	}
	for _, record := range records {
		if wakeRecordKey(record.Request) == item.WakeID {
			s.publishWakeStatus(sess, record)
			break
		}
	}
	return nil
}
