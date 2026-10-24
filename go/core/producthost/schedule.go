package producthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/serve"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/taskruntime"
)

type scheduledWakeHost interface {
	EnqueueBackgroundWake(context.Context, serve.BackgroundWakeRequest) (serve.BackgroundWakeReceipt, error)
	CancelBackgroundWake(context.Context, serve.BackgroundWakeRequest) (serve.BackgroundWakeReceipt, error)
	RestoreBackgroundWakes(context.Context) error
}

// nativeScheduleService owns timers and occurrence delivery only. The serve
// owner controls conversation restore, permissions, billing and model turns.
type nativeScheduleService struct {
	scheduler *taskruntime.Scheduler
	host      scheduledWakeHost
	mu        sync.Mutex
	cancel    context.CancelFunc
	closed    bool
	workers   sync.WaitGroup
}

func newNativeScheduleService(root string, host scheduledWakeHost) (*nativeScheduleService, error) {
	if strings.TrimSpace(root) == "" {
		root = session.PrimaryDir()
	}
	scheduler, err := taskruntime.Open(filepath.Join(root, "native-schedules.json"))
	if err != nil {
		return nil, fmt.Errorf("open native schedules: %w", err)
	}
	return &nativeScheduleService{scheduler: scheduler, host: host}, nil
}

func (s *nativeScheduleService) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("native schedule service is closed")
	}
	if s.cancel != nil {
		return nil
	}
	if err := s.host.RestoreBackgroundWakes(ctx); err != nil {
		return fmt.Errorf("restore durable wake ownership: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.workers.Add(2)
	go func() { defer s.workers.Done(); s.scheduler.Run(runCtx) }()
	go func() {
		defer s.workers.Done()
		// Running native claims retain their exact occurrence ID on restart.
		s.deliverPending(runCtx)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case event := <-s.scheduler.Events():
				if event.Type == taskruntime.EventDue {
					s.deliver(runCtx, event.Task)
				}
			case <-ticker.C:
				s.deliverPending(runCtx)
			}
		}
	}()
	return nil
}

func (s *nativeScheduleService) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.workers.Wait()
	return s.scheduler.Close()
}

func scheduleWakeRequest(task taskruntime.Task) serve.BackgroundWakeRequest {
	observation, _ := json.Marshal(map[string]any{
		"kind": "scheduled_continuation", "schedule_id": task.ID,
		"occurrence_id": task.ActiveRunID, "scheduled_at": task.LastRunAt,
		"objective": task.Prompt,
	})
	owner := task.NativeOwner
	return serve.BackgroundWakeRequest{
		ID:     "schedule:" + task.ID + ":" + task.ActiveRunID,
		Source: "scheduled_continuation", ChatID: owner.ChatID,
		WorkspaceCWD: owner.WorkspaceCWD, ConnectionID: owner.ConnectionID,
		OriginatedAt: task.OriginatedAt, Observation: string(observation),
	}
}

func (s *nativeScheduleService) Cancel(ctx context.Context, task taskruntime.Task) (string, error) {
	if task.NativeOwner == nil || task.ActiveRunID == "" {
		return "not_started", nil
	}
	request := scheduleWakeRequest(task)
	request.Observation = ""
	receipt, err := s.host.CancelBackgroundWake(ctx, request)
	if err != nil {
		return receipt.Status, err
	}
	switch receipt.Status {
	case "cancelled", "completed", "interrupted", "running":
		return receipt.Status, nil
	default:
		return receipt.Status, fmt.Errorf("wake cancellation was not confirmed: %s", receipt.Status)
	}
}

func (s *nativeScheduleService) deliverPending(ctx context.Context) {
	for _, task := range s.scheduler.List() {
		if ctx.Err() != nil {
			return
		}
		if task.NativeOwner != nil && task.ActiveRunID != "" {
			s.deliver(ctx, task)
		}
	}
}

func (s *nativeScheduleService) deliver(ctx context.Context, candidate taskruntime.Task) {
	task, ok := s.scheduler.Get(candidate.ID)
	if !ok || task.NativeOwner == nil || task.ActiveRunID == "" || task.ActiveRunID != candidate.ActiveRunID {
		return
	}
	request := scheduleWakeRequest(task)
	var receipt serve.BackgroundWakeReceipt
	var err error
	if task.Paused {
		request.Observation = ""
		receipt, err = s.host.CancelBackgroundWake(ctx, request)
	} else {
		receipt, err = s.host.EnqueueBackgroundWake(ctx, request)
	}
	if err != nil {
		if ctx.Err() == nil {
			_ = s.scheduler.RecordDeliveryError(task.ID, task.ActiveRunID, err.Error())
		}
		return
	}
	switch receipt.Status {
	case "completed":
		_, _ = s.scheduler.Complete(task.ID, task.ActiveRunID, "Scheduled agent turn settled.", "")
	case "cancelled", "interrupted":
		// A Stop cutoff or uncertain interrupted turn must not be revived by
		// another recurring occurrence. Explicit user/model resume is separate.
		if _, err := s.scheduler.Pause(task.ID); err != nil {
			return
		}
		_, _ = s.scheduler.Complete(task.ID, task.ActiveRunID, "", "wake "+receipt.Status+": "+receipt.StopReason)
	case "pending", "accepted", "running":
		// Keep the active occurrence until the model turn actually settles.
	default:
		_ = s.scheduler.RecordDeliveryError(task.ID, task.ActiveRunID, "unknown durable wake receipt: "+receipt.Status)
	}
}
