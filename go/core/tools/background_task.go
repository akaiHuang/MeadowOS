package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	BackgroundTaskToolName     = "task_output"
	maxRunningBackgroundTasks  = 20
	backgroundTaskStateVersion = 1
)

var (
	ErrBackgroundBrokerShutdown = errors.New("background task broker is shutting down")
	errBackgroundTaskCancelled  = errors.New("background task was cancelled")
)

type BackgroundTaskSnapshot struct {
	TaskID        string                     `json:"task_id"`
	Kind          string                     `json:"kind"`
	Description   string                     `json:"description,omitempty"`
	Status        string                     `json:"status"`
	Output        string                     `json:"output,omitempty"`
	Error         string                     `json:"error,omitempty"`
	StartedAt     string                     `json:"started_at"`
	EndedAt       string                     `json:"ended_at,omitempty"`
	Recoverable   bool                       `json:"recoverable,omitempty"`
	RecoveryCount int                        `json:"recovery_count,omitempty"`
	Workers       []BackgroundWorkerSnapshot `json:"workers,omitempty"`
	// A settled background execution is not acceptance of the user's objective.
	// The parent model still owns comparing the results with that objective.
	CompletionScope          string `json:"completion_scope,omitempty"`
	WorkerResultStatus       string `json:"worker_result_status,omitempty"`
	RequiresParentAcceptance bool   `json:"requires_parent_acceptance,omitempty"`
	CompletionGuidance       string `json:"completion_guidance,omitempty"`
}

type backgroundTask struct {
	id          string
	kind        string
	description string
	input       json.RawMessage
	durable     bool
	startedAt   time.Time
	cancel      context.CancelCauseFunc
	done        chan struct{}
	doneOnce    sync.Once

	mu                  sync.Mutex
	status              string
	output              string
	errText             string
	endedAt             time.Time
	recoveryCount       int
	notificationPending bool
	workers             map[string]*BackgroundWorkerSnapshot
	ownerContext        context.Context
	owner               *BackgroundWorkerOwner
}

func (t *backgroundTask) snapshot() BackgroundTaskSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	snapshot := BackgroundTaskSnapshot{
		TaskID: t.id, Kind: t.kind, Description: t.description,
		Status: t.status, Output: t.output, Error: t.errText,
		StartedAt:   t.startedAt.UTC().Format(time.RFC3339Nano),
		Recoverable: t.durable, RecoveryCount: t.recoveryCount, Workers: copyBackgroundWorkers(t.workers),
	}
	if !t.endedAt.IsZero() {
		snapshot.EndedAt = t.endedAt.UTC().Format(time.RFC3339Nano)
		snapshot.CompletionScope = "background_execution_only"
		snapshot.CompletionGuidance = "This receipt does not verify the user's goal. Reconcile worker outcomes and supporting observations with the latest user request; incomplete, failed, cancelled or interrupted work remains unfinished. Decide whether to continue, obtain missing evidence, or report the limitation."
		if t.kind == "subtask" {
			snapshot.RequiresParentAcceptance = true
			snapshot.WorkerResultStatus = backgroundWorkerResultStatus(t.workers)
			// Earlier versions stored the wrapper's normal return as completed
			// even for an incomplete child. Project the committed child facts
			// without rewriting the original historical settlement on disk.
			if snapshot.Status == "completed" && snapshot.WorkerResultStatus != "" {
				snapshot.Status = snapshot.WorkerResultStatus
			}
		}
	}
	return snapshot
}

func (t *backgroundTask) active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status == "running" || t.status == "recovering" || t.status == "cancelling"
}

type persistedBackgroundTask struct {
	BackgroundTaskSnapshot
	Input               json.RawMessage        `json:"input,omitempty"`
	NotificationPending bool                   `json:"notification_pending,omitempty"`
	Owner               *BackgroundWorkerOwner `json:"owner,omitempty"`
}

type persistedBackgroundTasks struct {
	Version int                       `json:"version"`
	Tasks   []persistedBackgroundTask `json:"tasks"`
	Owner   *BackgroundWorkerOwner    `json:"owner,omitempty"`
}

// BackgroundTaskRunner restarts durable work from its persisted model-authored
// input. The broker never interprets the input or invents a next action.
type BackgroundTaskRunner func(context.Context, json.RawMessage) (any, error)

// BackgroundTaskBroker owns non-process asynchronous agent work. Durable tasks
// persist their model-authored input and terminal result, allowing a new CLI
// process to resume interrupted work after the matching runner is registered.
type BackgroundTaskBroker struct {
	mu                sync.Mutex
	persistMu         sync.Mutex
	controlAdmission  sync.Mutex
	tasks             map[string]*backgroundTask
	runners           map[string]BackgroundTaskRunner
	notifications     []Notification
	notificationTasks map[string]string
	events            chan Notification
	path              string
	closed            bool
	wg                sync.WaitGroup
	hosted            bool
	completionSink    func(Notification) error
	hostAdmission     func(context.Context) error
	workerSlots       chan struct{}
	owner             *BackgroundWorkerOwner
	workerControls    map[string]*backgroundWorkerControl
}

func NewBackgroundTaskBroker() *BackgroundTaskBroker {
	broker, _ := OpenBackgroundTaskBroker("")
	return broker
}

func OpenBackgroundTaskBroker(path string) (*BackgroundTaskBroker, error) {
	return openBackgroundTaskBroker(path, false)
}
func OpenHostedBackgroundTaskBroker(path string) (*BackgroundTaskBroker, error) {
	return openBackgroundTaskBroker(path, true)
}
func openBackgroundTaskBroker(path string, hosted bool) (*BackgroundTaskBroker, error) {
	broker := &BackgroundTaskBroker{
		tasks: map[string]*backgroundTask{}, runners: map[string]BackgroundTaskRunner{},
		notificationTasks: map[string]string{}, events: make(chan Notification, 256),
		path: strings.TrimSpace(path), hosted: hosted, workerControls: map[string]*backgroundWorkerControl{},
	}
	if err := broker.load(); err != nil {
		return nil, err
	}
	broker.restorePendingNotifications()
	return broker, nil
}

func RegisterBackgroundTaskBroker(registry *Registry) *BackgroundTaskBroker {
	if registry == nil {
		return nil
	}
	if existing, ok := FindBackgroundTaskBroker(registry); ok {
		return existing
	}
	broker := NewBackgroundTaskBroker()
	registry.Register(broker)
	return broker
}

func RegisterPersistentBackgroundTaskBroker(
	registry *Registry,
	path string,
) (*BackgroundTaskBroker, error) {
	if registry == nil {
		return nil, errors.New("background task registry is required")
	}
	if existing, ok := FindBackgroundTaskBroker(registry); ok {
		return existing, nil
	}
	broker, err := OpenBackgroundTaskBroker(path)
	if err != nil {
		return nil, err
	}
	registry.Register(broker)
	return broker, nil
}

func FindBackgroundTaskBroker(registry *Registry) (*BackgroundTaskBroker, bool) {
	if registry == nil {
		return nil, false
	}
	tool, ok := registry.Get(BackgroundTaskToolName)
	if !ok {
		return nil, false
	}
	broker, ok := tool.(*BackgroundTaskBroker)
	return broker, ok
}

func (b *BackgroundTaskBroker) Start(
	kind, description string,
	run func(context.Context) (any, error),
) (BackgroundTaskSnapshot, error) {
	if run == nil {
		return BackgroundTaskSnapshot{}, errors.New("background task runner is required")
	}
	return b.start(kind, description, nil, false, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return run(ctx)
	})
}

func (b *BackgroundTaskBroker) StartDurable(
	kind, description string,
	input any,
	run BackgroundTaskRunner,
) (BackgroundTaskSnapshot, error) {
	if run == nil {
		return BackgroundTaskSnapshot{}, errors.New("background task runner is required")
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return BackgroundTaskSnapshot{}, fmt.Errorf("encode durable background task input: %w", err)
	}
	return b.start(kind, description, payload, true, run)
}

func (b *BackgroundTaskBroker) start(kind, description string, input json.RawMessage, durable bool, run BackgroundTaskRunner) (BackgroundTaskSnapshot, error) {
	return b.startWithContext(context.Background(), time.Now(), kind, description, input, durable, run)
}
func (b *BackgroundTaskBroker) startWithContext(ctx context.Context, origin time.Time, kind, description string, input json.RawMessage, durable bool, run BackgroundTaskRunner) (BackgroundTaskSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return BackgroundTaskSnapshot{}, err
	}
	b.mu.Lock()
	admit := b.hostAdmission
	b.mu.Unlock()
	if admit != nil {
		if err := admit(ctx); err != nil {
			return BackgroundTaskSnapshot{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return BackgroundTaskSnapshot{}, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return BackgroundTaskSnapshot{}, errors.New("background task broker is closed")
	}
	running := 0
	for _, task := range b.tasks {
		if task.active() {
			running++
		}
	}
	if running >= maxRunningBackgroundTasks {
		b.mu.Unlock()
		return BackgroundTaskSnapshot{}, fmt.Errorf(
			"maximum of %d running background tasks reached",
			maxRunningBackgroundTasks,
		)
	}

	task := &backgroundTask{
		id: newBackgroundTaskID(), kind: strings.TrimSpace(kind),
		description: strings.TrimSpace(description), input: append(json.RawMessage(nil), input...),
		durable: durable, startedAt: origin, done: make(chan struct{}), status: "running", ownerContext: ctx, workers: map[string]*BackgroundWorkerSnapshot{},
	}
	if owner, ok := BackgroundOwner(ctx); ok {
		copy := owner
		task.owner = &copy
	} else {
		task.owner = b.owner
	}
	if b.hosted && kind == "subtask" {
		var specs []SubtaskSpec
		if err := json.Unmarshal(input, &specs); err != nil {
			b.mu.Unlock()
			return BackgroundTaskSnapshot{}, err
		}
		if len(specs) == 0 || len(specs) > 8 {
			b.mu.Unlock()
			return BackgroundTaskSnapshot{}, errors.New("invalid hosted worker count")
		}
		seen := map[string]bool{}
		for _, spec := range specs {
			if spec.AgentID == "" || seen[spec.AgentID] {
				b.mu.Unlock()
				return BackgroundTaskSnapshot{}, errors.New("invalid hosted worker identity")
			}
			seen[spec.AgentID] = true
			task.workers[spec.AgentID] = &BackgroundWorkerSnapshot{SubagentID: spec.AgentID, Label: spec.ID, Goal: spec.Goal, Status: "queued"}
		}
	}
	b.tasks[task.id] = task
	if durable {
		b.runners[task.kind] = run
	}
	b.mu.Unlock()
	if err := b.persist(); err != nil {
		b.mu.Lock()
		delete(b.tasks, task.id)
		b.mu.Unlock()
		return BackgroundTaskSnapshot{}, err
	}
	if !b.launch(task, run) {
		return task.snapshot(), errors.New("background task host closed before launch")
	}
	return task.snapshot(), nil
}

// RegisterRunner binds a durable task kind to executable behavior and resumes
// every interrupted instance of that kind. The persisted input remains the
// authority; the runner only executes it.
func (b *BackgroundTaskBroker) RegisterRunner(kind string, run BackgroundTaskRunner) error {
	kind = strings.TrimSpace(kind)
	if kind == "" || run == nil {
		return errors.New("durable background task kind and runner are required")
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return errors.New("background task broker is closed")
	}
	b.runners[kind] = run
	var recovering []*backgroundTask
	for _, task := range b.tasks {
		task.mu.Lock()
		if !b.hosted && task.kind == kind && task.durable && task.status == "recovering" {
			task.status = "running"
			task.errText = ""
			task.endedAt = time.Time{}
			task.recoveryCount++
			recovering = append(recovering, task)
		}
		task.mu.Unlock()
	}
	b.mu.Unlock()
	if len(recovering) > 0 {
		if err := b.persist(); err != nil {
			for _, task := range recovering {
				task.mu.Lock()
				task.status = "recovering"
				task.errText = "recovery state could not be persisted"
				if task.recoveryCount > 0 {
					task.recoveryCount--
				}
				task.mu.Unlock()
			}
			return err
		}
		for _, task := range recovering {
			b.launch(task, run)
		}
	}
	return nil
}

func (b *BackgroundTaskBroker) launch(task *backgroundTask, run BackgroundTaskRunner) bool {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	base := task.ownerContext
	if base == nil {
		base = context.Background()
	}
	detached := context.WithoutCancel(base)
	var deadlineCancel context.CancelFunc
	if deadline, ok := base.Deadline(); ok {
		detached, deadlineCancel = context.WithDeadline(detached, deadline)
	}
	ctx, cancel := context.WithCancelCause(context.WithValue(context.WithValue(detached, backgroundOwnerContextKey{}, task.owner), backgroundTaskContextKey{}, task.id))
	task.mu.Lock()
	task.cancel = cancel
	task.mu.Unlock()
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		if deadlineCancel != nil {
			defer deadlineCancel()
		}
		output, runErr := run(ctx, append(json.RawMessage(nil), task.input...))
		b.finish(task, ctx, output, runErr)
	}()
	return true
}

func (b *BackgroundTaskBroker) finish(
	task *backgroundTask,
	ctx context.Context,
	output any,
	runErr error,
) {
	defer task.doneOnce.Do(func() { close(task.done) })
	payload := ""
	if output != nil {
		encoded, err := json.Marshal(output)
		if err == nil {
			payload = string(encoded)
		} else {
			runErr = err
		}
	}
	cause := context.Cause(ctx)
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()

	task.mu.Lock()
	if task.durable && closed && errors.Is(cause, ErrBackgroundBrokerShutdown) {
		task.status = "recovering"
		if b.hosted {
			task.status = "interrupted"
		}
		task.errText = "previous Meadow process stopped before completion; recovery is pending"
		task.output = ""
		task.endedAt = time.Time{}
		task.mu.Unlock()
		_ = b.persist()
		return
	}
	status := "completed"
	errorText := ""
	if runErr != nil {
		status = "failed"
		errorText = runErr.Error()
	}
	if ctx.Err() != nil {
		status = "cancelled"
		errorText = causeString(cause)
	}
	task.output = truncateBackgroundOutput(payload)
	task.errText = errorText
	task.endedAt = time.Now()
	task.notificationPending = true
	for _, worker := range task.workers {
		if !backgroundWorkerTerminal(worker.Status) {
			worker.Status = "cancelled"
		}
	}
	// The Go runner may return normally after a bounded child run stopped
	// incomplete. Preserve that child outcome instead of advertising a successful
	// batch solely because its transport/function returned without an error.
	if status == "completed" && task.kind == "subtask" {
		if childStatus := backgroundWorkerResultStatus(task.workers); childStatus != "" {
			status = childStatus
		}
	}
	task.status = status
	task.mu.Unlock()
	if err := b.persist(); err != nil {
		task.mu.Lock()
		task.errText = errors.Join(runErr, err).Error()
		task.mu.Unlock()
	}
	b.publishCompletion(task)
}

func causeString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Child run status is execution evidence, not semantic goal verification. A
// failed or missing child outcome cannot be promoted by a completed sibling.
func backgroundWorkerResultStatus(workers map[string]*BackgroundWorkerSnapshot) string {
	if len(workers) == 0 {
		return ""
	}
	status, priority := "completed", 0
	for _, worker := range workers {
		current := worker.Status
		if worker.Outcome != nil {
			current = worker.Outcome.Status
		} else if current == "completed" {
			// A legacy or partial record can carry a terminal label without the
			// worker's committed outcome. It is not evidence of successful work.
			current = "incomplete"
		}
		rank := 0
		switch current {
		case "completed":
		case "cancelled":
			rank = 2
		case "interrupted":
			rank = 3
		case "failed":
			rank = 4
		default:
			current, rank = "incomplete", 1
		}
		if rank > priority {
			status, priority = current, rank
		}
	}
	return status
}

func (b *BackgroundTaskBroker) get(id string) (*backgroundTask, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	task, ok := b.tasks[id]
	if !ok {
		return nil, fmt.Errorf("unknown task_id %q", id)
	}
	return task, nil
}

func (b *BackgroundTaskBroker) Poll(id string) (BackgroundTaskSnapshot, error) {
	task, err := b.get(id)
	if err != nil {
		return BackgroundTaskSnapshot{}, err
	}
	return task.snapshot(), nil
}

func (b *BackgroundTaskBroker) Wait(
	ctx context.Context,
	id string,
	timeout time.Duration,
) (BackgroundTaskSnapshot, error) {
	task, err := b.get(id)
	if err != nil {
		return BackgroundTaskSnapshot{}, err
	}
	if !task.active() {
		return task.snapshot(), nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-task.done:
		return task.snapshot(), nil
	case <-ctx.Done():
		return task.snapshot(), ctx.Err()
	case <-timer.C:
		return task.snapshot(), nil
	}
}

func (b *BackgroundTaskBroker) Cancel(id string) (BackgroundTaskSnapshot, error) {
	task, err := b.get(id)
	if err != nil {
		return BackgroundTaskSnapshot{}, err
	}
	task.mu.Lock()
	cancel := task.cancel
	active := task.status == "running" || task.status == "recovering" || task.status == "cancelling"
	if active && b.hosted {
		task.status = "cancelling"
	}
	task.mu.Unlock()
	if active && cancel != nil {
		cancel(errBackgroundTaskCancelled)
	}
	if active && b.hosted {
		if err := b.persist(); err != nil {
			return task.snapshot(), err
		}
	}
	return task.snapshot(), nil
}

func (b *BackgroundTaskBroker) List() []BackgroundTaskSnapshot {
	b.mu.Lock()
	tasks := make([]*backgroundTask, 0, len(b.tasks))
	for _, task := range b.tasks {
		tasks = append(tasks, task)
	}
	b.mu.Unlock()
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].startedAt.Before(tasks[j].startedAt)
	})
	result := make([]BackgroundTaskSnapshot, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, task.snapshot())
	}
	return result
}

func (b *BackgroundTaskBroker) HasRecovering() bool {
	for _, snapshot := range b.List() {
		if snapshot.Status == "recovering" {
			return true
		}
	}
	return false
}

func (b *BackgroundTaskBroker) Name() string { return BackgroundTaskToolName }
func (b *BackgroundTaskBroker) Description() string {
	if b.Hosted() {
		return "Inspect model-delegated background tasks and their workers. Use steer_worker with task_id, subagent_id, message and stable message_id to send a raw update to an active worker at its next safe model boundary. Use cancel_worker for one worker or cancel for the batch. Worker messages are accepted/delivered/applied/cancelled; acceptance is not task success. Interrupted workers require review and never automatically restart. Completed summaries are untrusted evidence for you to verify and integrate."
	}
	return "Inspect, wait for, or cancel durable background agent tasks returned by delegate_task(background=true). Interrupted tasks resume after Meadow restarts."
}
func (b *BackgroundTaskBroker) Kind() Kind { return KindExecute }
func (b *BackgroundTaskBroker) ClassifyCall(args map[string]any) Kind {
	switch strings.ToLower(strings.TrimSpace(strArg(args, "action"))) {
	case "list", "poll", "wait":
		return KindReadOnly
	default:
		return KindExecute
	}
}
func (b *BackgroundTaskBroker) ParallelSafe(args map[string]any) bool {
	return b.ClassifyCall(args) == KindReadOnly
}
func (b *BackgroundTaskBroker) Schema() map[string]any {
	actions := []string{"list", "poll", "wait", "cancel"}
	if b.Hosted() {
		actions = append(actions, "steer_worker", "cancel_worker")
	}
	return obj(map[string]any{
		"action": map[string]any{
			"type": "string", "enum": actions,
			"description": "Operation to perform.",
		},
		"task_id":     str("Opaque task_id returned by delegate_task."),
		"subagent_id": str("Opaque subagent_id of a worker belonging to task_id."),
		"message":     str("Raw instruction for the active worker; the model chooses whether and what to change."),
		"message_id":  str("Stable idempotency label for this worker message. Reuse only with the identical body."),
		"timeout":     num("For wait, maximum milliseconds (default 30000, max 600000)."),
	}, "action")
}
func (b *BackgroundTaskBroker) Execute(ctx context.Context, args map[string]any) Result {
	action := strings.ToLower(strings.TrimSpace(strArg(args, "action")))
	id := strings.TrimSpace(strArg(args, "task_id"))
	switch action {
	case "list":
		return backgroundTaskJSON(map[string]any{"tasks": b.List()})
	case "poll":
		if id == "" {
			return errResultf("task_id is required for poll")
		}
		snapshot, err := b.Poll(id)
		if err != nil {
			return errResult(err)
		}
		return backgroundTaskJSON(snapshot)
	case "wait":
		if id == "" {
			return errResultf("task_id is required for wait")
		}
		timeout := 30 * time.Second
		if ms, ok := intArg(args, "timeout"); ok && ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
		snapshot, err := b.Wait(ctx, id, timeout)
		if err != nil {
			return errResult(err)
		}
		return backgroundTaskJSON(snapshot)
	case "steer_worker":
		if !b.Hosted() {
			return errResultf("active worker control is not configured")
		}
		receipt, err := b.SteerWorker(id, strArg(args, "subagent_id"), strArg(args, "message_id"), strArg(args, "message"))
		if err != nil {
			return errResult(err)
		}
		return backgroundTaskJSON(receipt)
	case "cancel_worker":
		if !b.Hosted() {
			return errResultf("active worker control is not configured")
		}
		receipt, err := b.CancelWorker(id, strArg(args, "subagent_id"))
		if err != nil {
			return errResult(err)
		}
		return backgroundTaskJSON(receipt)
	case "cancel":
		if id == "" {
			return errResultf("task_id is required for cancel")
		}
		snapshot, err := b.Cancel(id)
		if err != nil {
			return errResult(err)
		}
		return backgroundTaskJSON(snapshot)
	default:
		return errResultf("unknown action %q (list|poll|wait|cancel)", action)
	}
}

func (b *BackgroundTaskBroker) publishCompletion(task *backgroundTask) {
	snapshot := task.snapshot()
	// Completion is a bounded reference/receipt. Full mailbox bodies and worker
	// history remain queryable through task_output; they cannot overflow wake.
	for i := range snapshot.Workers {
		snapshot.Workers[i].Goal = ""
		snapshot.Workers[i].Messages = nil
		if outcome := snapshot.Workers[i].Outcome; outcome != nil {
			// Keep terminal status, stop reason and error in the wake itself. The
			// full model-authored summary remains in the durable task_output result.
			outcome.Summary = ""
			if len(outcome.Error) > 1024 {
				outcome.Error = strings.ToValidUTF8(outcome.Error[:1024], "") + " [error truncated]"
			}
		}
	}
	if len(snapshot.Description) > 512 {
		snapshot.Description = snapshot.Description[:512]
	}
	encoded, _ := json.Marshal(snapshot)
	if len(encoded) > 192<<10 {
		snapshot.Output = ""
		encoded, _ = json.Marshal(map[string]any{"task_id": snapshot.TaskID, "kind": snapshot.Kind, "status": snapshot.Status,
			"completion_scope": snapshot.CompletionScope, "worker_result_status": snapshot.WorkerResultStatus,
			"requires_parent_acceptance": snapshot.RequiresParentAcceptance, "workers": snapshot.Workers,
			"completion_guidance": snapshot.CompletionGuidance,
			"started_at":          snapshot.StartedAt, "ended_at": snapshot.EndedAt,
			"details": "Full result remains available through task_output poll using this task_id."})
	}
	notification := Notification{Source: BackgroundTaskToolName, ID: "completion:" + task.id, StartedAt: &task.startedAt,
		Content: "[BACKGROUND AGENT TASK UPDATE — observable state]\n" + string(encoded)}
	b.mu.Lock()
	sink := b.completionSink
	if b.closed && sink == nil {
		b.mu.Unlock()
		return
	}
	exists := false
	for _, current := range b.notifications {
		if current.ID == notification.ID {
			exists = true
			break
		}
	}
	if !exists {
		b.notifications = append(b.notifications, notification)
		b.notificationTasks[notification.ID] = task.id
	}
	b.mu.Unlock()
	if sink != nil {
		_ = sink(notification)
	}
	b.mu.Lock()
	select {
	case b.events <- notification:
	default:
	}
	b.mu.Unlock()
}

func (b *BackgroundTaskBroker) restorePendingNotifications() {
	b.mu.Lock()
	tasks := make([]*backgroundTask, 0, len(b.tasks))
	for _, task := range b.tasks {
		task.mu.Lock()
		pending := task.notificationPending
		task.mu.Unlock()
		if pending {
			tasks = append(tasks, task)
		}
	}
	b.mu.Unlock()
	for _, task := range tasks {
		b.publishCompletion(task)
	}
}

func (b *BackgroundTaskBroker) DrainNotifications() []string {
	b.mu.Lock()
	if b.completionSink != nil {
		b.mu.Unlock()
		return nil
	}
	notifications := append([]Notification(nil), b.notifications...)
	b.notifications = nil
	taskIDs := make([]string, 0, len(notifications))
	for _, notification := range notifications {
		if taskID := b.notificationTasks[notification.ID]; taskID != "" {
			taskIDs = append(taskIDs, taskID)
			delete(b.notificationTasks, notification.ID)
		}
	}
	b.mu.Unlock()
	b.clearNotificationPending(taskIDs)
	result := make([]string, 0, len(notifications))
	for _, notification := range notifications {
		result = append(result, notification.Content)
	}
	return result
}

func (b *BackgroundTaskBroker) NotificationEvents() <-chan Notification {
	return b.events
}

func (b *BackgroundTaskBroker) ClaimNotification(id string) (string, bool) {
	b.mu.Lock()
	for i, notification := range b.notifications {
		if notification.ID != id {
			continue
		}
		b.notifications = append(b.notifications[:i], b.notifications[i+1:]...)
		taskID := b.notificationTasks[id]
		delete(b.notificationTasks, id)
		b.mu.Unlock()
		b.clearNotificationPending([]string{taskID})
		return notification.Content, true
	}
	b.mu.Unlock()
	return "", false
}

func (b *BackgroundTaskBroker) clearNotificationPending(taskIDs []string) {
	changed := false
	for _, id := range taskIDs {
		if id == "" {
			continue
		}
		task, err := b.get(id)
		if err != nil {
			continue
		}
		task.mu.Lock()
		if task.notificationPending {
			task.notificationPending = false
			changed = true
		}
		task.mu.Unlock()
	}
	if changed {
		_ = b.persist()
	}
}

func (b *BackgroundTaskBroker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	tasks := make([]*backgroundTask, 0, len(b.tasks))
	for _, task := range b.tasks {
		tasks = append(tasks, task)
	}
	b.mu.Unlock()

	for _, task := range tasks {
		task.mu.Lock()
		if task.status != "running" && task.status != "cancelling" {
			task.mu.Unlock()
			continue
		}
		cancel := task.cancel
		if task.durable {
			task.status = "recovering"
			if b.hosted {
				task.status = "interrupted"
			}
			task.errText = "previous Meadow process stopped before completion; recovery is pending"
			task.output = ""
			task.endedAt = time.Time{}
		}
		task.mu.Unlock()
		if cancel != nil {
			cancel(ErrBackgroundBrokerShutdown)
		}
	}
	persistErr := b.persist()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if b.hosted {
			return errors.Join(persistErr, errors.New("background_workers_shutdown_timeout"))
		}
	}
	return persistErr
}

func (b *BackgroundTaskBroker) load() error {
	if b.path == "" {
		return nil
	}
	data, err := os.ReadFile(b.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read background task state: %w", err)
	}
	if len(data) > 16<<20 {
		return errors.New("background task state exceeds capacity")
	}
	var state persistedBackgroundTasks
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse background task state: %w", err)
	}
	b.owner = state.Owner
	if state.Version != 0 && state.Version != backgroundTaskStateVersion {
		return fmt.Errorf("unsupported background task state version %d", state.Version)
	}
	recovered := false
	for _, record := range state.Tasks {
		startedAt, err := time.Parse(time.RFC3339Nano, record.StartedAt)
		if err != nil {
			continue
		}
		task := &backgroundTask{
			id: record.TaskID, kind: record.Kind, description: record.Description,
			input: append(json.RawMessage(nil), record.Input...), durable: record.Recoverable,
			startedAt: startedAt, done: make(chan struct{}), status: record.Status,
			output: record.Output, errText: record.Error,
			recoveryCount:       record.RecoveryCount,
			notificationPending: record.NotificationPending, owner: record.Owner,
		}
		task.workers = map[string]*BackgroundWorkerSnapshot{}
		for _, worker := range record.Workers {
			copy := worker
			task.workers[worker.SubagentID] = &copy
		}
		if record.EndedAt != "" {
			task.endedAt, _ = time.Parse(time.RFC3339Nano, record.EndedAt)
		}
		if task.durable && (task.status == "running" || task.status == "recovering" || task.status == "cancelling") {
			task.status = "recovering"
			if b.hosted {
				task.status = "interrupted"
				task.notificationPending = false
				for _, worker := range task.workers {
					if !backgroundWorkerTerminal(worker.Status) {
						worker.Status = "interrupted"
					}
					for i := range worker.Messages {
						if worker.Messages[i].Status != "applied" {
							worker.Messages[i].Status = "cancelled"
						}
					}
				}
				task.doneOnce.Do(func() { close(task.done) })
			}
			task.errText = "previous Meadow process stopped before completion; recovery is pending"
			task.endedAt = time.Time{}
			recovered = true
		} else {
			task.doneOnce.Do(func() { close(task.done) })
		}
		b.tasks[task.id] = task
	}
	if recovered {
		return b.persist()
	}
	return nil
}

func (b *BackgroundTaskBroker) persist() error {
	if b == nil || b.path == "" {
		return nil
	}
	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	tasks := make([]*backgroundTask, 0, len(b.tasks))
	for _, task := range b.tasks {
		tasks = append(tasks, task)
	}
	b.mu.Unlock()
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].startedAt.Before(tasks[j].startedAt)
	})
	state := persistedBackgroundTasks{
		Version: backgroundTaskStateVersion, Owner: b.owner,
		Tasks: make([]persistedBackgroundTask, 0, len(tasks)),
	}
	for _, task := range tasks {
		task.mu.Lock()
		record := persistedBackgroundTask{
			BackgroundTaskSnapshot: BackgroundTaskSnapshot{
				TaskID: task.id, Kind: task.kind, Description: task.description,
				Status: task.status, Output: task.output, Error: task.errText,
				StartedAt:   task.startedAt.UTC().Format(time.RFC3339Nano),
				Recoverable: task.durable, RecoveryCount: task.recoveryCount, Workers: copyBackgroundWorkers(task.workers),
			},
			Input:               append(json.RawMessage(nil), task.input...),
			NotificationPending: task.notificationPending, Owner: task.owner,
		}
		if !task.endedAt.IsZero() {
			record.EndedAt = task.endedAt.UTC().Format(time.RFC3339Nano)
		}
		task.mu.Unlock()
		if record.Recoverable || record.NotificationPending {
			state.Tasks = append(state.Tasks, record)
		}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode background task state: %w", err)
	}
	if len(data) > 16<<20 {
		return errors.New("background task state exceeds capacity")
	}
	data = append(data, '\n')
	dir := filepath.Dir(b.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create background task directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(b.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create background task temp file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, b.path); err != nil {
		return fmt.Errorf("commit background task state: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func backgroundTaskJSON(value any) Result {
	payload, err := json.Marshal(value)
	if err != nil {
		return errResult(err)
	}
	return Result{LLMContent: string(payload), Display: string(payload)}
}

func newBackgroundTaskID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return "task_" + hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("task_%d", time.Now().UnixNano())
}

func truncateBackgroundOutput(value string) string {
	const limit = 32 * 1024
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n[output truncated]"
}
