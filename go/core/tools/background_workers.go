package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type backgroundTaskContextKey struct{}

func BackgroundTaskID(ctx context.Context) string {
	id, _ := ctx.Value(backgroundTaskContextKey{}).(string)
	return id
}

type WorkerMessageReceipt struct {
	ID         string    `json:"message_id"`
	Text       string    `json:"message"`
	Status     string    `json:"status"`
	TargetTurn int       `json:"target_turn,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
type BackgroundWorkerSnapshot struct {
	SubagentID      string                 `json:"subagent_id"`
	Label           string                 `json:"id"`
	Goal            string                 `json:"goal"`
	Status          string                 `json:"status"`
	CancelRequested bool                   `json:"cancel_requested,omitempty"`
	Messages        []WorkerMessageReceipt `json:"messages,omitempty"`
	Outcome         *SubtaskOutcome        `json:"outcome,omitempty"`
}
type WorkerSteer func(WorkerMessageReceipt) (int, error)
type backgroundWorkerControl struct {
	mu     sync.Mutex
	steer  WorkerSteer
	cancel context.CancelFunc
}

func backgroundWorkerTerminal(status string) bool {
	switch status {
	case "completed", "cancelled", "failed", "incomplete", "interrupted":
		return true
	}
	return false
}
func copyBackgroundWorkers(workers map[string]*BackgroundWorkerSnapshot) []BackgroundWorkerSnapshot {
	result := make([]BackgroundWorkerSnapshot, 0, len(workers))
	for _, worker := range workers {
		copy := *worker
		copy.Messages = append([]WorkerMessageReceipt(nil), worker.Messages...)
		if worker.Outcome != nil {
			outcome := *worker.Outcome
			copy.Outcome = &outcome
		}
		result = append(result, copy)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SubagentID < result[j].SubagentID })
	return result
}

// ConfigureHosted is called by the trusted session owner before exposing model
// tools. The slot channel is shared by every chat on this Agent, never per batch.
func (b *BackgroundTaskBroker) ConfigureHosted(sink func(Notification) error, admit func(context.Context) error, slots chan struct{}) {
	b.mu.Lock()
	b.completionSink = sink
	b.hostAdmission = admit
	b.workerSlots = slots
	b.mu.Unlock()
	for _, event := range b.PendingNotifications() {
		if sink != nil {
			_ = sink(event)
		}
	}
}
func (b *BackgroundTaskBroker) Hosted() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.hosted }
func (b *BackgroundTaskBroker) PendingNotifications() []Notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Notification(nil), b.notifications...)
}
func (b *BackgroundTaskBroker) HasActive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, task := range b.tasks {
		if task.active() {
			return true
		}
	}
	return false
}
func (b *BackgroundTaskBroker) CancelAll() error {
	b.mu.Lock()
	var ids []string
	for id, task := range b.tasks {
		if task.active() {
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	var errs []error
	for _, id := range ids {
		_, err := b.Cancel(id)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
func (b *BackgroundTaskBroker) StartDurableContext(ctx context.Context, origin time.Time, kind, description string, input any, run BackgroundTaskRunner) (BackgroundTaskSnapshot, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return BackgroundTaskSnapshot{}, err
	}
	if run == nil {
		return BackgroundTaskSnapshot{}, errors.New("background runner unavailable")
	}
	return b.startWithContext(ctx, origin, kind, description, payload, true, run)
}
func (b *BackgroundTaskBroker) worker(taskID, id string) (*backgroundTask, *BackgroundWorkerSnapshot, error) {
	task, err := b.get(taskID)
	if err != nil {
		return nil, nil, err
	}
	task.mu.Lock()
	worker := task.workers[id]
	task.mu.Unlock()
	if worker == nil {
		return nil, nil, errors.New("worker does not belong to this task")
	}
	return task, worker, nil
}
func (b *BackgroundTaskBroker) BeginWorker(ctx context.Context, id string) (func(), error) {
	task, worker, err := b.worker(BackgroundTaskID(ctx), id)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	slots := b.workerSlots
	b.mu.Unlock()
	release := func() {}
	if slots != nil {
		select {
		case slots <- struct{}{}:
			release = func() { <-slots }
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b.controlAdmission.Lock()
	defer b.controlAdmission.Unlock()
	task.mu.Lock()
	if task.status != "running" || worker.CancelRequested || backgroundWorkerTerminal(worker.Status) {
		task.mu.Unlock()
		release()
		return nil, context.Canceled
	}
	worker.Status = "running"
	task.mu.Unlock()
	if err = b.persist(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
func (b *BackgroundTaskBroker) AttachWorker(ctx context.Context, id string, steer WorkerSteer, cancel context.CancelFunc) error {
	task, worker, err := b.worker(BackgroundTaskID(ctx), id)
	if err != nil {
		return err
	}
	controller := &backgroundWorkerControl{steer: steer, cancel: cancel}
	b.mu.Lock()
	b.workerControls[id] = controller
	b.mu.Unlock()
	b.controlAdmission.Lock()
	task.mu.Lock()
	requested := worker.CancelRequested
	task.mu.Unlock()
	b.controlAdmission.Unlock()
	if requested {
		cancel()
	}
	return nil
}
func (b *BackgroundTaskBroker) FinishWorker(ctx context.Context, id string, outcome SubtaskOutcome) error {
	b.controlAdmission.Lock()
	defer b.controlAdmission.Unlock()
	task, worker, err := b.worker(BackgroundTaskID(ctx), id)
	if err != nil {
		return err
	}
	task.mu.Lock()
	worker.Status = outcome.Status
	copy := outcome
	worker.Outcome = &copy
	for i := range worker.Messages {
		if worker.Messages[i].Status == "accepted" || worker.Messages[i].Status == "delivered" {
			worker.Messages[i].Status = "cancelled"
		}
	}
	task.mu.Unlock()
	b.mu.Lock()
	delete(b.workerControls, id)
	b.mu.Unlock()
	return b.persist()
}

// SteerWorker persists raw model-authored content before acknowledging it. It
// does not interpret whether the user's message amends a task or starts another.
func (b *BackgroundTaskBroker) SteerWorker(taskID, id, messageID, text string) (WorkerMessageReceipt, error) {
	if strings.TrimSpace(text) == "" || len(text) > 8000 {
		return WorkerMessageReceipt{}, errors.New("worker message must contain 1..8000 bytes")
	}
	if messageID == "" {
		messageID = "wm_" + newNotificationID()
	}
	if len(messageID) > 128 {
		return WorkerMessageReceipt{}, errors.New("worker message_id too long")
	}
	task, worker, err := b.worker(taskID, id)
	if err != nil {
		return WorkerMessageReceipt{}, err
	}
	message, err := func() (WorkerMessageReceipt, error) {
		b.controlAdmission.Lock()
		defer b.controlAdmission.Unlock()
		task.mu.Lock()
		for _, existing := range worker.Messages {
			if existing.ID == messageID {
				task.mu.Unlock()
				if existing.Text != text {
					return existing, errors.New("worker_message_id_payload_conflict")
				}
				if err := b.persist(); err != nil {
					return WorkerMessageReceipt{}, err
				}
				return existing, nil
			}
		}
		if task.status != "running" || backgroundWorkerTerminal(worker.Status) || worker.CancelRequested {
			status := worker.Status
			task.mu.Unlock()
			return WorkerMessageReceipt{}, fmt.Errorf("worker is %s; no active worker was steered", status)
		}
		if len(worker.Messages) >= 32 {
			task.mu.Unlock()
			return WorkerMessageReceipt{}, errors.New("worker message capacity reached")
		}
		message := WorkerMessageReceipt{ID: messageID, Text: text, Status: "accepted", CreatedAt: time.Now().UTC()}
		worker.Messages = append(worker.Messages, message)
		task.mu.Unlock()
		if err := b.persist(); err != nil {
			task.mu.Lock()
			worker.Messages = worker.Messages[:len(worker.Messages)-1]
			task.mu.Unlock()
			return WorkerMessageReceipt{}, err
		}
		return message, nil
	}()
	if err != nil {
		return message, err
	}
	b.FlushWorkerMessages(taskID, id)
	task.mu.Lock()
	defer task.mu.Unlock()
	for _, current := range worker.Messages {
		if current.ID == messageID {
			return current, nil
		}
	}
	return message, nil
}
func (b *BackgroundTaskBroker) PendingWorkerMessages(taskID, id string) []WorkerMessageReceipt {
	b.controlAdmission.Lock()
	defer b.controlAdmission.Unlock()
	return b.pendingWorkerMessages(taskID, id)
}
func (b *BackgroundTaskBroker) pendingWorkerMessages(taskID, id string) []WorkerMessageReceipt {
	task, worker, err := b.worker(taskID, id)
	if err != nil {
		return nil
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	var result []WorkerMessageReceipt
	for _, message := range worker.Messages {
		if message.Status == "accepted" {
			result = append(result, message)
		}
	}
	return result
}
func (b *BackgroundTaskBroker) FlushWorkerMessages(taskID, id string) {
	b.mu.Lock()
	control := b.workerControls[id]
	b.mu.Unlock()
	if control == nil {
		return
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	for _, message := range b.PendingWorkerMessages(taskID, id) {
		turn, err := control.steer(message)
		if err != nil {
			return
		}
		_ = b.SetWorkerMessageStatus(taskID, id, message.ID, "delivered", turn)
	}
}
func (b *BackgroundTaskBroker) SetWorkerMessageStatus(taskID, id, messageID, status string, turn int) error {
	b.controlAdmission.Lock()
	defer b.controlAdmission.Unlock()
	task, worker, err := b.worker(taskID, id)
	if err != nil {
		return err
	}
	task.mu.Lock()
	found := false
	for i := range worker.Messages {
		current := &worker.Messages[i]
		if current.ID == messageID {
			found = true
			if current.Status == "applied" || current.Status == "cancelled" {
				break
			}
			current.Status = status
			current.TargetTurn = turn
			break
		}
	}
	task.mu.Unlock()
	if !found {
		return errors.New("worker message missing")
	}
	return b.persist()
}
func (b *BackgroundTaskBroker) CancelWorker(taskID, id string) (BackgroundWorkerSnapshot, error) {
	b.controlAdmission.Lock()
	defer b.controlAdmission.Unlock()
	task, worker, err := b.worker(taskID, id)
	if err != nil {
		return BackgroundWorkerSnapshot{}, err
	}
	task.mu.Lock()
	if backgroundWorkerTerminal(worker.Status) {
		copy := *worker
		task.mu.Unlock()
		return copy, nil
	}
	previousStatus, previousCancel := worker.Status, worker.CancelRequested
	worker.CancelRequested = true
	worker.Status = "cancelling"
	copy := *worker
	copy.Messages = append([]WorkerMessageReceipt(nil), worker.Messages...)
	task.mu.Unlock()
	if err = b.persist(); err != nil {
		task.mu.Lock()
		worker.Status = previousStatus
		worker.CancelRequested = previousCancel
		task.mu.Unlock()
		return BackgroundWorkerSnapshot{}, err
	}
	b.mu.Lock()
	control := b.workerControls[id]
	b.mu.Unlock()
	if control != nil {
		control.cancel()
	}
	return copy, nil
}

// Recovery inspection never starts a runner or opens a model connection.
func HostedBackgroundTasksNeedRecovery(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 16<<20 {
		return false
	}
	var data persistedBackgroundTasks
	if json.Unmarshal(raw, &data) != nil {
		return false
	}
	for _, task := range data.Tasks {
		if task.Status == "running" || task.Status == "recovering" || task.NotificationPending {
			return true
		}
	}
	return false
}

type backgroundOwnerContextKey struct{}
type backgroundWorkerContextKey struct{}
type BackgroundWorkerOwner struct {
	ChatID       string `json:"chat_id"`
	WorkspaceCWD string `json:"workspace_cwd"`
	ConnectionID string `json:"connection_id"`
	ModelID      string `json:"model_id"`
	ApprovalMode string `json:"approval_mode"`
}

func (b *BackgroundTaskBroker) BindOwner(owner BackgroundWorkerOwner) error {
	b.mu.Lock()
	if b.owner != nil && (b.owner.ChatID != owner.ChatID || b.owner.WorkspaceCWD != owner.WorkspaceCWD || b.owner.ConnectionID != owner.ConnectionID) {
		b.mu.Unlock()
		return errors.New("background_worker_owner_mismatch")
	}
	if b.owner == nil && len(b.tasks) != 0 {
		b.mu.Unlock()
		return errors.New("background_worker_owner_missing")
	}
	copy := owner
	b.owner = &copy
	b.mu.Unlock()
	return b.persist()
}
func BackgroundOwner(ctx context.Context) (BackgroundWorkerOwner, bool) {
	owner, ok := ctx.Value(backgroundOwnerContextKey{}).(*BackgroundWorkerOwner)
	if !ok || owner == nil {
		return BackgroundWorkerOwner{}, false
	}
	return *owner, true
}
func WithBackgroundWorkerID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, backgroundWorkerContextKey{}, id)
}
func BackgroundWorkerID(ctx context.Context) string {
	id, _ := ctx.Value(backgroundWorkerContextKey{}).(string)
	return id
}

func WithBackgroundOwner(ctx context.Context, owner BackgroundWorkerOwner) context.Context {
	copy := owner
	return context.WithValue(ctx, backgroundOwnerContextKey{}, &copy)
}

// Reserve initial content once, before the child runtime starts. It must not
// also be delivered through SubmitGuidance when another message arrives.
func (b *BackgroundTaskBroker) TakeInitialWorkerMessages(taskID, id string) ([]WorkerMessageReceipt, error) {
	b.controlAdmission.Lock()
	defer b.controlAdmission.Unlock()
	task, worker, err := b.worker(taskID, id)
	if err != nil {
		return nil, err
	}
	task.mu.Lock()
	var result []WorkerMessageReceipt
	for i := range worker.Messages {
		if worker.Messages[i].Status == "accepted" {
			result = append(result, worker.Messages[i])
			worker.Messages[i].Status = "delivered"
		}
	}
	task.mu.Unlock()
	if len(result) == 0 {
		return result, nil
	}
	if err = b.persist(); err != nil {
		task.mu.Lock()
		ids := map[string]bool{}
		for _, message := range result {
			ids[message.ID] = true
		}
		for i := range worker.Messages {
			if ids[worker.Messages[i].ID] {
				worker.Messages[i].Status = "accepted"
			}
		}
		task.mu.Unlock()
		return nil, err
	}
	return result, nil
}
