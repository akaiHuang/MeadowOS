package taskruntime

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
	"strconv"
	"strings"
	"sync"
	"time"
)

const persistedVersion = 1

type persistedState struct {
	Version int    `json:"version"`
	Tasks   []Task `json:"tasks"`
}

// Scheduler owns loop state and emits due occurrences. It is safe for tool
// calls, the TUI, and its timer goroutine to use concurrently.
type Scheduler struct {
	mu     sync.Mutex
	path   string
	now    func() time.Time
	tasks  map[string]Task
	order  []string
	wake   chan struct{}
	events chan Event
	done   chan struct{}
	lock   *schedulerFileLock
	closed bool
}

func Open(path string) (*Scheduler, error) {
	return openWithClock(path, time.Now)
}

func openWithClock(path string, now func() time.Time) (*Scheduler, error) {
	s := &Scheduler{
		path: path, now: now, tasks: map[string]Task{},
		wake: make(chan struct{}, 1), events: make(chan Event, 64),
		done: make(chan struct{}),
	}
	if strings.TrimSpace(path) != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create task scheduler directory: %w", err)
		}
		lock, err := acquireSchedulerFileLock(path + ".lock")
		if err != nil {
			return nil, err
		}
		s.lock = lock
	}
	if err := s.load(); err != nil {
		_ = releaseSchedulerFileLock(s.lock)
		return nil, err
	}
	return s, nil
}

func (s *Scheduler) Events() <-chan Event { return s.events }

// Close releases the cross-process single-writer lease. The lease is backed
// by an OS advisory lock, so an ungraceful process exit releases it as well.
func (s *Scheduler) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	lock := s.lock
	s.lock = nil
	s.mu.Unlock()
	s.signal()
	return releaseSchedulerFileLock(lock)
}

// Run waits until the next persisted deadline. Due occurrences are claimed
// before publication, which provides a generic no-overlap guarantee.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		task, runID, wait, err := s.claimNextDue()
		if err != nil {
			wait = time.Second
		}
		if task != nil {
			select {
			case s.events <- Event{Type: EventDue, Task: *task, RunID: runID}:
			case <-ctx.Done():
				return
			case <-s.done:
				return
			}
			continue
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return
		case <-s.done:
			stopTimer(timer)
			return
		case <-s.wake:
			stopTimer(timer)
		case <-timer.C:
		}
	}
}

func (s *Scheduler) CreateLoop(request CreateLoopRequest) (Task, error) {
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.Prompt == "" {
		return Task{}, errors.New("prompt is required")
	}
	interval, canonical, err := ParseInterval(request.Interval)
	if err != nil {
		return Task{}, err
	}

	s.mu.Lock()
	if err := s.ensureOpenLocked(); err != nil {
		s.mu.Unlock()
		return Task{}, err
	}
	if len(s.tasks) >= MaxScheduledTasks {
		s.mu.Unlock()
		return Task{}, fmt.Errorf("maximum of %d scheduled tasks reached", MaxScheduledTasks)
	}
	beforeTasks, beforeOrder := s.copyStateLocked()
	now := s.now().UTC()
	next := now.Add(interval)
	if request.FireImmediately {
		next = now
	}
	task := Task{
		ID: newTaskID(), Kind: KindLoop, Prompt: request.Prompt,
		Interval: canonical, IntervalSecs: int64(interval / time.Second),
		Status: StatusScheduled, Durable: request.Durable,
		OwnerSession: request.OwnerSession, CreatedAt: now, UpdatedAt: now,
		NextRunAt: next, ExpiresAt: now.Add(DefaultLoopTTL),
	}
	s.tasks[task.ID] = task
	s.order = append(s.order, task.ID)
	if err := s.persistLocked(); err != nil {
		s.restoreStateLocked(beforeTasks, beforeOrder)
		s.mu.Unlock()
		return Task{}, err
	}
	s.mu.Unlock()
	s.signal()
	s.emit(Event{Type: EventCreated, Task: task})
	return task, nil
}

// UpdateLoop changes only model-authored schedule fields. It preserves the
// schedule phase, but clears the prior summary when the prompt changes so old
// task context cannot leak into a new objective.
func (s *Scheduler) UpdateLoop(request UpdateLoopRequest) (Task, error) {
	s.mu.Lock()
	if err := s.ensureOpenLocked(); err != nil {
		s.mu.Unlock()
		return Task{}, err
	}
	task, ok := s.tasks[request.ID]
	if !ok {
		s.mu.Unlock()
		return Task{}, fmt.Errorf("no scheduled task with id %s", request.ID)
	}
	if request.Prompt == nil && request.Interval == nil {
		s.mu.Unlock()
		return Task{}, errors.New("nothing to update")
	}
	beforeTasks, beforeOrder := s.copyStateLocked()
	if request.Prompt != nil {
		prompt := strings.TrimSpace(*request.Prompt)
		if prompt == "" {
			s.mu.Unlock()
			return Task{}, errors.New("prompt cannot be empty")
		}
		if prompt != task.Prompt {
			task.Prompt = prompt
			task.LastSummary = ""
			task.LastError = ""
			task.Iteration = 0
		}
	}
	if request.Interval != nil {
		duration, canonical, err := ParseInterval(*request.Interval)
		if err != nil {
			s.mu.Unlock()
			return Task{}, err
		}
		task.Interval = canonical
		task.IntervalSecs = int64(duration / time.Second)
		anchor := task.CreatedAt
		if task.LastRunAt != nil {
			anchor = *task.LastRunAt
		}
		task.NextRunAt = anchor.Add(duration)
	}
	task.UpdatedAt = s.now().UTC()
	s.tasks[task.ID] = task
	if err := s.persistLocked(); err != nil {
		s.restoreStateLocked(beforeTasks, beforeOrder)
		s.mu.Unlock()
		return Task{}, err
	}
	s.mu.Unlock()
	s.signal()
	s.emit(Event{Type: EventUpdated, Task: task})
	return task, nil
}

func (s *Scheduler) Pause(id string) (Task, error) {
	return s.setPaused(id, true)
}

func (s *Scheduler) Resume(id string) (Task, error) {
	return s.setPaused(id, false)
}

func (s *Scheduler) setPaused(id string, paused bool) (Task, error) {
	s.mu.Lock()
	if err := s.ensureOpenLocked(); err != nil {
		s.mu.Unlock()
		return Task{}, err
	}
	task, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return Task{}, fmt.Errorf("no scheduled task with id %s", id)
	}
	beforeTasks, beforeOrder := s.copyStateLocked()
	task.Paused = paused
	if task.Status != StatusRunning {
		if paused {
			task.Status = StatusPaused
		} else {
			task.Status = StatusScheduled
			task.NextRunAt = s.now().UTC().Add(time.Duration(task.IntervalSecs) * time.Second)
		}
	}
	task.UpdatedAt = s.now().UTC()
	s.tasks[id] = task
	if err := s.persistLocked(); err != nil {
		s.restoreStateLocked(beforeTasks, beforeOrder)
		s.mu.Unlock()
		return Task{}, err
	}
	s.mu.Unlock()
	s.signal()
	eventType := EventPaused
	if !paused {
		eventType = EventResumed
	}
	s.emit(Event{Type: eventType, Task: task})
	return task, nil
}

func (s *Scheduler) Delete(id string) (Task, bool, error) {
	s.mu.Lock()
	if err := s.ensureOpenLocked(); err != nil {
		s.mu.Unlock()
		return Task{}, false, err
	}
	task, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return Task{}, false, nil
	}
	beforeTasks, beforeOrder := s.copyStateLocked()
	delete(s.tasks, id)
	for i, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	if err := s.persistLocked(); err != nil {
		s.restoreStateLocked(beforeTasks, beforeOrder)
		s.mu.Unlock()
		return Task{}, false, err
	}
	s.mu.Unlock()
	s.signal()
	s.emit(Event{Type: EventDeleted, Task: task})
	return task, true, nil
}

func (s *Scheduler) Get(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	return task, ok
}

func (s *Scheduler) List() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Task, 0, len(s.order))
	for _, id := range s.order {
		if task, ok := s.tasks[id]; ok {
			result = append(result, task)
		}
	}
	return result
}

// Complete records observable output from one claimed occurrence. The model
// remains responsible for deciding whether the objective itself is complete;
// this method only advances the recurring schedule.
func (s *Scheduler) Complete(id, runID, summary, errorText string) (Task, error) {
	s.mu.Lock()
	if err := s.ensureOpenLocked(); err != nil {
		s.mu.Unlock()
		return Task{}, err
	}
	task, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return Task{}, fmt.Errorf("no scheduled task with id %s", id)
	}
	if task.ActiveRunID != runID {
		s.mu.Unlock()
		return Task{}, fmt.Errorf("task %s run %s is not active", id, runID)
	}
	beforeTasks, beforeOrder := s.copyStateLocked()
	now := s.now().UTC()
	task.ActiveRunID = ""
	task.LastEndedAt = &now
	task.LastSummary = truncate(summary, SummaryLimit)
	task.LastError = truncate(errorText, 1_000)
	task.Iteration++
	task.UpdatedAt = now
	if task.Paused {
		task.Status = StatusPaused
	} else if task.Kind == KindOnce || (task.MaxRuns > 0 && task.Iteration >= task.MaxRuns) {
		task.Status = StatusCompleted
	} else {
		task.Status = StatusScheduled
	}

	if !task.ExpiresAt.IsZero() && !now.Before(task.ExpiresAt) && task.NativeOwner == nil {
		delete(s.tasks, id)
		for i, existing := range s.order {
			if existing == id {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	} else {
		if !task.ExpiresAt.IsZero() && !now.Before(task.ExpiresAt) && task.Status == StatusScheduled {
			task.Status = StatusExpired
		}
		s.tasks[id] = task
	}
	if err := s.persistLocked(); err != nil {
		s.restoreStateLocked(beforeTasks, beforeOrder)
		s.mu.Unlock()
		return Task{}, err
	}
	s.mu.Unlock()
	s.signal()
	s.emit(Event{Type: EventEnded, Task: task, RunID: runID})
	return task, nil
}

func (s *Scheduler) claimNextDue() (*Task, string, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, "", 24 * time.Hour, err
	}

	now := s.now().UTC()
	var selected *Task
	var next time.Time
	expired := false
	for _, id := range s.order {
		task, ok := s.tasks[id]
		if !ok {
			continue
		}
		if !task.ExpiresAt.IsZero() && !now.Before(task.ExpiresAt) && task.ActiveRunID == "" {
			if task.NativeOwner != nil {
				if task.Status != StatusExpired && task.Status != StatusCompleted {
					task.Status = StatusExpired
					task.UpdatedAt = now
					s.tasks[id] = task
					expired = true
				}
			} else {
				delete(s.tasks, id)
				expired = true
			}
			continue
		}
		if task.Paused || task.Status == StatusRunning || task.Status == StatusCompleted || task.Status == StatusExpired {
			continue
		}
		if !task.NextRunAt.After(now) {
			copy := task
			selected = &copy
			break
		}
		if next.IsZero() || task.NextRunAt.Before(next) {
			next = task.NextRunAt
		}
	}
	if expired {
		filtered := s.order[:0]
		for _, id := range s.order {
			if _, ok := s.tasks[id]; ok {
				filtered = append(filtered, id)
			}
		}
		s.order = filtered
		if err := s.persistLocked(); err != nil {
			return nil, "", time.Second, err
		}
	}
	if selected == nil {
		if next.IsZero() {
			return nil, "", 24 * time.Hour, nil
		}
		wait := next.Sub(now)
		if wait < 0 {
			wait = 0
		}
		return nil, "", wait, nil
	}

	beforeTasks, beforeOrder := s.copyStateLocked()
	task := *selected
	runID := newRunID()
	task.Status = StatusRunning
	task.ActiveRunID = runID
	task.LastRunAt = &now
	task.NextRunAt = now.Add(time.Duration(task.IntervalSecs) * time.Second)
	task.UpdatedAt = now
	s.tasks[task.ID] = task
	if err := s.persistLocked(); err != nil {
		s.restoreStateLocked(beforeTasks, beforeOrder)
		return nil, "", time.Second, err
	}
	return &task, runID, 0, nil
}

func (s *Scheduler) load() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read task scheduler: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse task scheduler: %w", err)
	}
	if state.Version != 0 && state.Version != persistedVersion {
		return fmt.Errorf("unsupported task scheduler version %d", state.Version)
	}
	now := s.now().UTC()
	recovered := false
	for _, task := range state.Tasks {
		if task.ID == "" || (task.Kind != KindLoop && task.Kind != KindOnce) || !task.Durable {
			continue
		}
		if !task.ExpiresAt.IsZero() && !now.Before(task.ExpiresAt) && task.ActiveRunID == "" {
			if task.NativeOwner == nil {
				recovered = true
				continue
			}
			if task.Status != StatusCompleted && task.Status != StatusExpired {
				task.Status = StatusExpired
				task.UpdatedAt = now
				recovered = true
			}
		}
		if task.Status == StatusRunning && task.NativeOwner == nil {
			task.Status = StatusScheduled
			task.ActiveRunID = ""
			task.LastError = "previous execution ended before completion"
			recovered = true
		}
		s.tasks[task.ID] = task
		s.order = append(s.order, task.ID)
	}
	if recovered {
		return s.persistLocked()
	}
	return nil
}

func (s *Scheduler) persistLocked() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	state := persistedState{Version: persistedVersion, Tasks: make([]Task, 0, len(s.order))}
	for _, id := range s.order {
		task, ok := s.tasks[id]
		if ok && task.Durable {
			state.Tasks = append(state.Tasks, task)
		}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task scheduler: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create task scheduler directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create task scheduler temp file: %w", err)
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
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("commit task scheduler: %w", err)
	}
	return nil
}

func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) emit(event Event) {
	select {
	case s.events <- event:
	default:
		// Lifecycle state remains durable and listable if a UI is briefly slow.
		// Due events are emitted by Run with backpressure and never use this path.
	}
}

func (s *Scheduler) copyStateLocked() (map[string]Task, []string) {
	tasks := make(map[string]Task, len(s.tasks))
	for id, task := range s.tasks {
		tasks[id] = task
	}
	return tasks, append([]string(nil), s.order...)
}

func (s *Scheduler) restoreStateLocked(tasks map[string]Task, order []string) {
	s.tasks = tasks
	s.order = order
}

func (s *Scheduler) ensureOpenLocked() error {
	if s.closed {
		return errors.New("task scheduler is closed")
	}
	return nil
}

func ParseInterval(value string) (time.Duration, string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, "", errors.New("interval is required")
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
		if err != nil || days <= 0 || days > int64((1<<63-1)/(24*time.Hour)) {
			return 0, "", fmt.Errorf("invalid interval %q", value)
		}
		duration := time.Duration(days) * 24 * time.Hour
		return validateInterval(duration, fmt.Sprintf("%dd", days))
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, "", fmt.Errorf("invalid interval %q: use 60s, 5m, 2h, or 1d", value)
	}
	return validateInterval(duration, canonicalInterval(duration))
}

func validateInterval(duration time.Duration, canonical string) (time.Duration, string, error) {
	if duration < MinLoopInterval {
		return 0, "", fmt.Errorf("interval must be at least %s", MinLoopInterval)
	}
	return duration, canonical, nil
}

func canonicalInterval(duration time.Duration) string {
	if duration%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", duration/(24*time.Hour))
	}
	return duration.String()
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) <= max {
		return value
	}
	return string([]rune(value)[:max]) + "…"
}

func newTaskID() string { return randomID("loop_") }
func newRunID() string  { return randomID("run_") }

func randomID(prefix string) string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return prefix + hex.EncodeToString(raw[:])
	}
	return prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func stopTimer(timer *time.Timer) {
	if timer == nil || !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// SortByNextRun is useful to hosts that want a task pane ordered by urgency.
func SortByNextRun(tasks []Task) {
	sort.SliceStable(tasks, func(i, j int) bool {
		return tasks[i].NextRunAt.Before(tasks[j].NextRunAt)
	})
}
