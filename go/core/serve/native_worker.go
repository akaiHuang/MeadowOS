package serve

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

func nativeWorkerPath(req BackgroundWakeRequest) string {
	return filepath.Join(session.PrimaryDir(), "background-workers", "v1", wakeHash(req.ConnectionID, req.WorkspaceCWD, req.ChatID)+".json")
}

// configurationMu and sess.mu are held. The registry is copied before adding
// a session-owned broker, and is immutable once the runtime observes it.
func (s *Server) nativeWorkerBrokerLocked(sess *serveSession) (*tools.BackgroundTaskBroker, error) {
	if !s.ConversationInboxEnabled {
		return nil, nil
	}
	if sess.workerBroker != nil {
		return sess.workerBroker, nil
	}
	broker, err := tools.OpenHostedBackgroundTaskBroker(nativeWorkerPath(wakeSessionRequest(sess)))
	if err != nil {
		return nil, err
	}
	if err = broker.BindOwner(tools.BackgroundWorkerOwner{ChatID: queueChatID(sess), WorkspaceCWD: sess.cwd, ConnectionID: sess.connectionID, ModelID: sess.engine.ModelID, ApprovalMode: string(sess.engine.Mode)}); err != nil {
		return nil, err
	}
	if s.workerSlots == nil {
		s.workerSlots = make(chan struct{}, 2)
	}
	registry := sess.engine.Registry.Select(sess.engine.Registry.Names())
	registry.Register(broker)
	sess.engine.Registry = registry
	sess.workerBroker = broker
	broker.ConfigureHosted(nil, func(ctx context.Context) error {
		if s.BackgroundWakeAdmission != nil {
			return s.BackgroundWakeAdmission(ctx, sess.id)
		}
		return ctx.Err()
	}, s.workerSlots)
	return broker, nil
}
func (s *Server) bindNativeWorkerSink(sess *serveSession, broker *tools.BackgroundTaskBroker) {
	if broker == nil {
		return
	}
	broker.ConfigureHosted(func(event tools.Notification) error {
		err := s.persistCompletion(sess, event)
		sess.mu.Lock()
		sess.wakeSourceRetry = sess.wakeSourceRetry || err != nil
		sess.mu.Unlock()
		s.signalWake(sess)
		return err
	}, func(ctx context.Context) error {
		if s.BackgroundWakeAdmission != nil {
			return s.BackgroundWakeAdmission(ctx, sess.id)
		}
		return ctx.Err()
	}, s.workerSlots)
}
func (s *Server) closeNativeWorkers() error {
	s.mu.Lock()
	var brokers []*tools.BackgroundTaskBroker
	for _, sess := range s.sessions {
		sess.mu.Lock()
		if sess.workerBroker != nil {
			brokers = append(brokers, sess.workerBroker)
		}
		sess.mu.Unlock()
	}
	s.mu.Unlock()
	var errs []error
	for _, broker := range brokers {
		errs = append(errs, broker.Close())
	}
	return errors.Join(errs...)
}

// Worker permissions are fresh, explicitly attributed cards. They never consume
// a foreground run's restored approval or borrow its recovery checkpoint.
func (sess *serveSession) confirmWorkerTool(ctx context.Context, req engine.ConfirmRequest) (engine.Decision, error) {
	if sess.effectiveApprovalChannel() == approvalChannelNone {
		return engine.DecisionDeny, errApprovalChannelUnavailable
	}
	id := "worker-permission-" + session.NewUUID()
	ch := make(chan engine.Decision, 1)
	sess.mu.Lock()
	sess.permissions[id] = ch
	sess.mu.Unlock()
	defer func() { sess.mu.Lock(); delete(sess.permissions, id); sess.mu.Unlock() }()
	options := []map[string]any{{"optionId": "approve", "name": "允許", "kind": "allow_once"}, {"optionId": "reject", "name": "拒絕", "kind": "reject_once"}}
	if req.Kind != tools.KindDestructive {
		options = append(options, map[string]any{"optionId": "approve_always", "name": "一律允許", "kind": "allow_always"})
	}
	sess.broadcast("permission_request", map[string]any{"sessionId": sess.id, "requestId": id, "backgroundTaskId": tools.BackgroundTaskID(ctx), "subagentId": tools.BackgroundWorkerID(ctx), "actor": "worker", "toolCall": map[string]any{"title": req.ToolName, "kind": string(req.Kind), "rawInput": req.Args}, "options": options})
	select {
	case decision := <-ch:
		return decision, nil
	case <-ctx.Done():
		sess.cancelPermission(id, ch)
		return engine.DecisionDeny, ctx.Err()
	}
}

func (s *Server) withNativeWorkerOwner(ctx context.Context, sess *serveSession) context.Context {
	if !s.ConversationInboxEnabled {
		return ctx
	}
	sess.mu.Lock()
	owner := tools.BackgroundWorkerOwner{ChatID: queueChatID(sess), WorkspaceCWD: sess.cwd, ConnectionID: sess.connectionID, ModelID: sess.engine.ModelID, ApprovalMode: string(sess.engine.Mode)}
	sess.mu.Unlock()
	return tools.WithBackgroundOwner(ctx, owner)
}

// A detached child cannot gain authority from a later broader foreground mode,
// and a user's current restriction also applies to already-running children.
func intersectWorkerApprovalMode(admitted, current engine.ApprovalMode) engine.ApprovalMode {
	rank := func(mode engine.ApprovalMode) int {
		switch mode {
		case engine.ModePlan:
			return 0
		case engine.ModeAutoEdit:
			return 2
		case engine.ModeYolo:
			return 3
		case engine.ModeFullAccess:
			return 4
		default:
			return 1
		}
	}
	a, b := rank(admitted), rank(current)
	if b < a {
		a = b
	}
	return []engine.ApprovalMode{engine.ModePlan, engine.ModeDefault, engine.ModeAutoEdit, engine.ModeYolo, engine.ModeFullAccess}[a]
}
