package taskruntime

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ScheduleRequest contains only model-authored timing and objective data.
// Owner authority is supplied separately by the native host.
type ScheduleRequest struct {
	Prompt    string
	Interval  string
	RunAt     time.Time
	After     time.Duration
	MaxRuns   int
	ExpiresAt time.Time
}

func (o ScheduleOwner) Valid() bool {
	return strings.TrimSpace(o.ChatID) != "" && filepath.IsAbs(o.WorkspaceCWD)
}

func OwnsSchedule(task Task, owner ScheduleOwner) bool {
	return task.NativeOwner != nil && *task.NativeOwner == owner
}

func (s *Scheduler) CreateSchedule(owner ScheduleOwner, request ScheduleRequest) (Task, error) {
	if !owner.Valid() {
		return Task{}, errors.New("native schedule requires a bound chat and absolute workspace")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return Task{}, err
	}
	if len(s.tasks) >= MaxScheduledTasks {
		return Task{}, fmt.Errorf("maximum of %d scheduled tasks reached", MaxScheduledTasks)
	}
	now := s.now().UTC()
	task, err := nativeScheduleFields(request, now)
	if err != nil {
		return Task{}, err
	}
	task.ID, task.CreatedAt, task.UpdatedAt = randomID("schedule_"), now, now
	task.OriginatedAt, task.NativeOwner = now, &owner
	task.OwnerSession, task.Durable, task.Status = owner.ChatID, true, StatusScheduled
	s.tasks[task.ID] = task
	s.order = append(s.order, task.ID)
	if err := s.persistLocked(); err != nil {
		delete(s.tasks, task.ID)
		s.order = s.order[:len(s.order)-1]
		return Task{}, err
	}
	s.signal()
	s.emit(Event{Type: EventCreated, Task: task})
	return task, nil
}

func nativeScheduleFields(request ScheduleRequest, now time.Time) (Task, error) {
	task := Task{Prompt: strings.TrimSpace(request.Prompt), MaxRuns: request.MaxRuns, ExpiresAt: request.ExpiresAt.UTC()}
	if task.Prompt == "" {
		return Task{}, errors.New("prompt is required")
	}
	if len(task.Prompt) > 32_768 {
		return Task{}, errors.New("prompt exceeds 32768 bytes")
	}
	if task.MaxRuns < 0 || task.MaxRuns > 1000 {
		return Task{}, errors.New("max_runs must be between 1 and 1000 when supplied")
	}
	modes := 0
	if request.Interval != "" {
		modes++
	}
	if !request.RunAt.IsZero() {
		modes++
	}
	if request.After != 0 {
		modes++
	}
	if modes != 1 {
		return Task{}, errors.New("provide exactly one of interval, run_at, or after_seconds")
	}
	if request.Interval != "" {
		duration, canonical, err := ParseInterval(request.Interval)
		if err != nil {
			return Task{}, err
		}
		if task.MaxRuns == 0 && task.ExpiresAt.IsZero() {
			return Task{}, errors.New("recurring schedules require max_runs or expires_at")
		}
		task.Kind, task.Interval, task.IntervalSecs = KindLoop, canonical, int64(duration/time.Second)
		task.NextRunAt = now.Add(duration)
	} else {
		task.Kind, task.MaxRuns = KindOnce, 1
		task.NextRunAt = request.RunAt.UTC()
		if request.After != 0 {
			if request.After < time.Second {
				return Task{}, errors.New("after_seconds must be at least one second")
			}
			task.NextRunAt = now.Add(request.After)
		}
		if !task.NextRunAt.After(now) {
			return Task{}, errors.New("run_at must be in the future")
		}
	}
	if !task.ExpiresAt.IsZero() && !task.ExpiresAt.After(task.NextRunAt) {
		return Task{}, errors.New("expires_at must be after the first scheduled occurrence")
	}
	return task, nil
}

// ReplaceSchedule updates one idle schedule atomically, preserving ownership.
// Updating an active occurrence would change its immutable wake payload.
func (s *Scheduler) ReplaceSchedule(owner ScheduleOwner, id string, request ScheduleRequest) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return Task{}, err
	}
	before, ok := s.tasks[id]
	if !ok || !OwnsSchedule(before, owner) {
		return Task{}, errors.New("schedule not found in this chat")
	}
	if before.ActiveRunID != "" {
		return Task{}, errors.New("pause and settle the active occurrence before updating this schedule")
	}
	now := s.now().UTC()
	task, err := nativeScheduleFields(request, now)
	if err != nil {
		return Task{}, err
	}
	task.ID, task.CreatedAt, task.UpdatedAt = id, before.CreatedAt, now
	task.OriginatedAt, task.NativeOwner = now, before.NativeOwner
	task.OwnerSession, task.Durable, task.Status = owner.ChatID, true, StatusScheduled
	task.Paused = before.Paused
	if task.Paused {
		task.Status = StatusPaused
	}
	s.tasks[id] = task
	if err := s.persistLocked(); err != nil {
		s.tasks[id] = before
		return Task{}, err
	}
	s.signal()
	s.emit(Event{Type: EventUpdated, Task: task})
	return task, nil
}

func (s *Scheduler) ResumeSchedule(owner ScheduleOwner, id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return Task{}, err
	}
	before, ok := s.tasks[id]
	if !ok || !OwnsSchedule(before, owner) {
		return Task{}, errors.New("schedule not found in this chat")
	}
	if before.ActiveRunID != "" {
		return Task{}, errors.New("the prior occurrence has not settled; it cannot be resumed or replayed")
	}
	if before.Status == StatusCompleted || (before.MaxRuns > 0 && before.Iteration >= before.MaxRuns) {
		return Task{}, errors.New("schedule is complete; create a new schedule to authorize more work")
	}
	now := s.now().UTC()
	if !before.ExpiresAt.IsZero() && !now.Before(before.ExpiresAt) {
		return Task{}, errors.New("schedule has expired")
	}
	task := before
	task.Paused, task.Status, task.UpdatedAt, task.OriginatedAt = false, StatusScheduled, now, now
	if task.Kind == KindLoop {
		task.NextRunAt = now.Add(time.Duration(task.IntervalSecs) * time.Second)
	}
	s.tasks[id] = task
	if err := s.persistLocked(); err != nil {
		s.tasks[id] = before
		return Task{}, err
	}
	s.signal()
	s.emit(Event{Type: EventResumed, Task: task})
	return task, nil
}

// RecordDeliveryError leaves the occurrence claim intact for an idempotent
// admission retry. It records transport evidence, never an assistant answer.
func (s *Scheduler) RecordDeliveryError(id, runID, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return err
	}
	task, ok := s.tasks[id]
	if !ok || task.ActiveRunID != runID {
		return errors.New("schedule occurrence is no longer active")
	}
	message = truncate(message, 1000)
	if task.LastError == message {
		return nil
	}
	before := task
	task.LastError, task.UpdatedAt = message, s.now().UTC()
	s.tasks[id] = task
	if err := s.persistLocked(); err != nil {
		s.tasks[id] = before
		return err
	}
	return nil
}
