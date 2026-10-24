package serve

import (
	"context"
	"errors"
	"strings"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

type backgroundCompletionContextKey struct{}

func (s *Server) backgroundWakeContext() context.Context {
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	if s.wakeContext == nil {
		s.wakeContext, s.wakeCancel = context.WithCancel(context.Background())
		if s.wakeClosed {
			s.wakeCancel()
		}
	}
	return s.wakeContext
}

// Close stops only this host's completion listeners and their automatic turns.
// A disconnected SSE reader is deliberately not this lifecycle boundary.
func (s *Server) Close() error {
	s.configurationMu.Lock()
	s.wakeClosed = true
	if s.wakeCancel != nil {
		s.wakeCancel()
	}
	s.configurationMu.Unlock()
	workerErr := s.closeNativeWorkers()
	s.wakeWorkers.Wait()
	return workerErr
}

// The durable listener journals process facts before the lossy channel hint.
func (s *Server) startBackgroundWatcher(sess *serveSession) {
	if !s.BackgroundWakeEnabled || sess == nil || sess.engine == nil {
		return
	}
	if err := s.ensureWakeOwner(sess); err != nil {
		sess.broadcast("background_wake_blocked", map[string]any{"message": err.Error()})
		return
	}
	ctx := s.backgroundWakeContext()
	s.configurationMu.Lock()
	if s.wakeClosed {
		s.configurationMu.Unlock()
		return
	}
	sess.mu.Lock()
	if sess.wakeStarted || sess.engine.Registry == nil {
		sess.mu.Unlock()
		s.configurationMu.Unlock()
		return
	}
	broker, brokerErr := s.nativeWorkerBrokerLocked(sess)
	if brokerErr != nil {
		sess.mu.Unlock()
		s.configurationMu.Unlock()
		sess.broadcast("background_wake_blocked", map[string]any{"message": brokerErr.Error()})
		return
	}
	sess.wakeStarted = true
	sess.wakeSignal = make(chan struct{}, 1)
	registry := sess.engine.Registry
	sess.mu.Unlock()
	if manager, ok := tools.FindProcessManager(registry); ok {
		manager.SetCompletionSink(func(event tools.Notification) error {
			err := s.persistCompletion(sess, event)
			sess.mu.Lock()
			sess.wakeSourceRetry = sess.wakeSourceRetry || err != nil
			sess.mu.Unlock()
			s.signalWake(sess)
			return err
		})
	}
	s.wakeWorkers.Add(1)
	s.configurationMu.Unlock()
	s.bindNativeWorkerSink(sess, broker)
	go func() {
		defer s.wakeWorkers.Done()
		events := registry.WatchNotifications(ctx)
		ticker := time.NewTicker(100 * time.Millisecond)
		ticker.Stop()
		defer ticker.Stop()
		s.signalWake(sess)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					events = nil
				} else {
					if err := s.persistCompletion(sess, event); err != nil {
						sess.mu.Lock()
						sess.wakeSourceRetry = true
						sess.mu.Unlock()
					}
				}
			case <-sess.wakeSignal:
			case <-ticker.C:
			}
			sess.mu.Lock()
			retry := sess.wakeSourceRetry
			sess.wakeSourceRetry = false
			sess.mu.Unlock()
			if retry {
				if manager, ok := tools.FindProcessManager(registry); ok {
					failed := false
					for _, event := range manager.PendingNotifications() {
						if s.persistCompletion(sess, event) != nil {
							failed = true
						}
					}
					sess.mu.Lock()
					sess.wakeSourceRetry = sess.wakeSourceRetry || failed
					sess.mu.Unlock()
				}
				if broker != nil {
					failed := false
					for _, event := range broker.PendingNotifications() {
						if s.persistCompletion(sess, event) != nil {
							failed = true
						}
					}
					sess.mu.Lock()
					sess.wakeSourceRetry = sess.wakeSourceRetry || failed
					sess.mu.Unlock()
				}
			}
			records, err := s.wakeRecords(sess)
			if err != nil {
				ticker.Reset(100 * time.Millisecond)
				continue
			}
			active := retry
			for _, record := range records {
				if record.Status == "pending" || record.Status == "accepted" || record.Status == "running" {
					active = true
				}
				s.tryDurableWake(ctx, sess, registry, record)
			}
			if active {
				ticker.Reset(100 * time.Millisecond)
			} else {
				ticker.Stop()
			}
		}
	}()
}
func (s *Server) persistCompletion(sess *serveSession, n tools.Notification) error {
	if n.StartedAt == nil || strings.TrimSpace(n.Content) == "" {
		return errors.New("background_completion_provenance_unavailable")
	}
	req := wakeSessionRequest(sess)
	req.ID = n.ID
	req.Source = n.Source
	req.OriginatedAt = *n.StartedAt
	req.Observation = n.Content
	_, err := s.recordBackgroundWake(req, false)
	return err
}
func (s *Server) tryBackgroundWake(ctx context.Context, sess *serveSession, registry *tools.Registry, event tools.Notification) bool {
	if err := s.ensureWakeOwner(sess); err != nil {
		return false
	}
	if err := s.persistCompletion(sess, event); err != nil {
		return false
	}
	req := wakeSessionRequest(sess)
	req.Source = event.Source
	req.ID = event.ID
	records, err := s.wakeRecords(sess)
	if err != nil {
		return false
	}
	for _, record := range records {
		if wakeRecordKey(record.Request) == wakeRecordKey(req) {
			return s.tryDurableWake(ctx, sess, registry, record)
		}
	}
	return false
}

// The prepared queue row cannot execute. accepted is durable before pending is
// published, and all retries reconcile one fixed queue identity.
func (s *Server) tryDurableWake(ctx context.Context, sess *serveSession, registry *tools.Registry, record *durableWakeRecord) bool {
	if ctx.Err() != nil || !sess.promptMu.TryLock() {
		return false
	}
	defer sess.promptMu.Unlock()
	key := wakeRecordKey(record.Request)
	// A channel/store snapshot may predate model completion. Refresh only after
	// owning the execution boundary; terminal receipts can never be downgraded.
	records, err := s.wakeRecords(sess)
	if err != nil {
		return false
	}
	var authoritative *durableWakeRecord
	for _, candidate := range records {
		if wakeRecordKey(candidate.Request) == key {
			authoritative = candidate
			break
		}
	}
	if authoritative == nil {
		return false
	}
	record = authoritative
	s.configurationMu.Lock()
	sess.mu.Lock()
	var existing *queuedPrompt
	for _, item := range sess.queuedPrompts {
		if item.ID == record.QueueID {
			existing = item
			break
		}
	}
	if record.Status == "cancelled" || record.Status == "completed" || record.Status == "interrupted" {
		// Terminal outbox precedes queue deletion. On a crash in between, its
		// receipt settles the leftover row; it must not block later authorized work.
		if existing != nil {
			for i, item := range sess.queuedPrompts {
				if item == existing {
					sess.queuedPrompts = append(sess.queuedPrompts[:i], sess.queuedPrompts[i+1:]...)
					break
				}
			}
			if err := sess.persistQueueLocked(); err != nil {
				sess.queuePaused = true
			}
		}
		sess.mu.Unlock()
		s.configurationMu.Unlock()
		return true
	}
	if cutoff := sess.wakeCancelledBefore; cutoff != nil && !record.Request.OriginatedAt.After(*cutoff) {
		sess.mu.Unlock()
		s.configurationMu.Unlock()
		if s.setWakeState(sess, key, "cancelled", "", "cancelled") == nil {
			record.Status = "cancelled"
			s.publishWakeStatus(sess, record)
		}
		return true
	}
	if record.Status == "running" || (existing != nil && existing.Status == "interrupted") {
		sess.mu.Unlock()
		s.configurationMu.Unlock()
		if s.setWakeState(sess, key, "interrupted", record.RunID, "model_execution_interrupted") == nil {
			record.Status = "interrupted"
			s.publishWakeStatus(sess, record)
		}
		return true
	}
	blocked := s.wakeClosed || s.configurationLeaseActiveLocked(time.Now()) || sess.connectionID != s.ConnectionID || sess.queuePaused || sess.running || sess.activeRuntime != nil || sess.cancelRun != nil || sess.warmCancel != nil || len(sess.promptAdmissions) != 0 || sess.stableCheckpointErr != nil || sess.queuePump
	for _, item := range sess.queuedPrompts {
		if item != existing {
			blocked = true
		}
	}
	if blocked {
		sess.mu.Unlock()
		s.configurationMu.Unlock()
		return false
	}
	generation := sess.cancelGeneration
	s.configurationAdmissions++
	sess.mu.Unlock()
	s.configurationMu.Unlock()
	defer s.endConfigurationAdmission()
	// A changed live policy must be explicitly admitted by the user before
	// automatic work can adopt it. Keep this occurrence pending and retryable.
	if err := s.ensureWakeOwner(sess); err != nil {
		return false
	}
	if s.BackgroundWakeAdmission != nil {
		if err := s.BackgroundWakeAdmission(ctx, sess.id); err != nil {
			sess.broadcast("background_wake_blocked", map[string]any{"sessionId": sess.id, "source": record.Request.Source, "notificationId": record.Request.ID, "wakeId": key, "message": err.Error()})
			return false
		}
	}
	sess.mu.Lock()
	if ctx.Err() != nil || sess.queuePaused || sess.cancelGeneration != generation || len(sess.promptAdmissions) != 0 {
		sess.mu.Unlock()
		return false
	}
	if existing == nil {
		if record.Status == "accepted" {
			sess.mu.Unlock()
			_ = s.setWakeState(sess, key, "interrupted", record.RunID, "accepted_queue_missing")
			return true
		}
		mode := sess.snapshotHarnessModeLocked()
		prompt := "[MEADOW RUNTIME OBSERVATION — observable tool state]\n" + record.Request.Observation + "\n\nReview this observation in the context of the user's objective. Choose necessary follow-up tools, verify state, and report the outcome."
		existing = &queuedPrompt{ID: record.QueueID, Text: "背景工作", Prompt: []map[string]any{{"type": "text", "text": prompt}}, Mode: agentcore.RunModeAuto, Status: "wake_prepared", CreatedAt: record.CreatedAt, HarnessMode: &mode, BackgroundCompletion: true, WakeID: key}
		sess.queuedPrompts = append(sess.queuedPrompts, existing)
		if err := sess.persistQueueLocked(); err != nil {
			sess.queuePaused = true
			sess.mu.Unlock()
			return false
		}
	}
	if err := s.setWakeState(sess, key, "accepted", "", ""); err != nil {
		sess.mu.Unlock()
		return false
	}
	existing.Status = "pending"
	if err := sess.persistQueueLocked(); err != nil {
		existing.Status = "wake_prepared"
		sess.queuePaused = true
		sess.mu.Unlock()
		return false
	}
	sess.mu.Unlock()
	registry.ClaimNotification(tools.Notification{Source: record.Request.Source, ID: record.Request.ID})
	record.Status = "accepted"
	s.publishWakeStatus(sess, record)
	sess.broadcast("background_wake", map[string]any{"sessionId": sess.id, "source": record.Request.Source, "notificationId": record.Request.ID, "wakeId": key, "taskId": existing.ID})
	sess.broadcastQueue()
	go s.startQueuePump(sess)
	return true
}
