package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const subtaskSummaryMaxBytes = 16 * 1024

// SubtaskPolicy bounds a model-invoked delegate_task tool. Enabled never means
// auto-delegate: the runtime merely exposes the tool and the parent model owns
// whether, when and how to split work.
type SubtaskPolicy struct {
	Enabled       bool
	MaxTasks      int
	MaxConcurrent int
	MaxTurns      int
	Depth         int
	MaxDepth      int
}

func normalizeSubtaskPolicy(policy SubtaskPolicy) SubtaskPolicy {
	if policy.MaxTasks <= 0 {
		policy.MaxTasks = 3
	}
	if policy.MaxTasks > 8 {
		policy.MaxTasks = 8
	}
	if policy.MaxConcurrent <= 0 {
		policy.MaxConcurrent = 2
	}
	if policy.MaxConcurrent > policy.MaxTasks {
		policy.MaxConcurrent = policy.MaxTasks
	}
	if policy.MaxTurns <= 0 {
		policy.MaxTurns = 12
	}
	if policy.MaxTurns > 48 {
		policy.MaxTurns = 48
	}
	if policy.MaxDepth <= 0 {
		policy.MaxDepth = 1
	}
	return policy
}

func (k *KernelRuntime) installSubtaskTool() error {
	policy := normalizeSubtaskPolicy(k.config.Subtasks)
	k.config.Subtasks = policy
	if !policy.Enabled || !k.config.ModelLed || policy.Depth >= policy.MaxDepth || k.config.Executor == nil || k.config.Executor.Registry == nil {
		return nil
	}
	// Own a registry view so adding delegate_task never mutates the caller's
	// base registry or the legacy 12B/26B surfaces that may share it.
	registry := k.config.Executor.Registry.Select(k.config.Executor.Registry.Names())
	executor := *k.config.Executor
	executor.Registry = registry
	k.config.Executor = &executor
	delegate := tools.NewDelegateTask(policy.MaxTasks, k.runSubtasks)
	// Freeze the child-visible registry before attaching the durable broker:
	// AttachBroker may immediately launch recovered workers on another
	// goroutine, and Registry is intentionally immutable during execution.
	registry.Register(delegate)
	if broker, ok := tools.FindBackgroundTaskBroker(registry); ok {
		if err := delegate.AttachBroker(broker); err != nil {
			return err
		}
	}
	return nil
}

// subtaskRunner shares durable child identity, isolated worktrees and bounded
// fan-out between the legacy and native model-led loops. NewChild selects the
// loop; this layer never selects a task, model, tool or workflow.
type subtaskRunner struct {
	Policy                                                SubtaskPolicy
	Broker                                                *tools.BackgroundTaskBroker
	AllowSharedWorkspace                                  bool
	Executor                                              *ToolExecutor
	CWD, ModelID, StateDir, ParentSessionID, SystemPrompt string
	NewChild                                              func(*ToolExecutor, string, string, string, KernelHooks, SubtaskPolicy) (Runtime, error)
}

func (k *KernelRuntime) subtaskRunner() subtaskRunner {
	return subtaskRunner{
		Policy: k.config.Subtasks, Executor: k.config.Executor,
		CWD: k.config.CWD, ModelID: k.config.ModelID,
		StateDir: k.config.SubtaskStateDir, ParentSessionID: k.config.ParentSessionID,
		SystemPrompt: k.config.SystemPrompt,
		NewChild: func(executor *ToolExecutor, cwd, _ string, prompt string, hooks KernelHooks, policy SubtaskPolicy) (Runtime, error) {
			config := k.config
			config.CWD, config.Executor, config.SystemPrompt = cwd, executor, prompt
			config.MaxTurns = policy.MaxTurns
			config.AdmissionPolicy, config.ValidationPolicy = nil, nil
			config.Deliverables, config.ArtifactCapabilities = nil, nil
			config.Hooks = hooks
			config.Subtasks.Depth++
			return NewKernelRuntime(config)
		},
	}
}

func (k *KernelRuntime) runSubtasks(ctx context.Context, specs []tools.SubtaskSpec) []tools.SubtaskOutcome {
	return k.subtaskRunner().run(ctx, specs)
}

func (k *KernelRuntime) runOneSubtask(ctx context.Context, spec tools.SubtaskSpec, policy SubtaskPolicy) tools.SubtaskOutcome {
	return k.subtaskRunner().runOne(ctx, spec, policy)
}

func (r subtaskRunner) run(ctx context.Context, specs []tools.SubtaskSpec) []tools.SubtaskOutcome {
	policy := normalizeSubtaskPolicy(r.Policy)
	outcomes := make([]tools.SubtaskOutcome, len(specs))
	jobs := make(chan int)
	workers := min(policy.MaxConcurrent, len(specs))
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				outcomes[index] = r.runOne(ctx, specs[index], policy)
			}
		}()
	}
	for index := range specs {
		select {
		case jobs <- index:
		case <-ctx.Done():
			outcomes[index] = tools.SubtaskOutcome{ID: specs[index].ID, Status: "cancelled", Error: ctx.Err().Error()}
		}
	}
	close(jobs)
	group.Wait()
	for index := range outcomes {
		if outcomes[index].ID == "" {
			message := "subtask cancelled"
			if cause := context.Cause(ctx); cause != nil {
				message = cause.Error()
			}
			outcomes[index] = tools.SubtaskOutcome{ID: specs[index].ID, Status: "cancelled", Error: message}
		}
	}
	return outcomes
}

func (r subtaskRunner) runOne(ctx context.Context, spec tools.SubtaskSpec, policy SubtaskPolicy) (outcome tools.SubtaskOutcome) {
	broker := r.Broker
	taskID := tools.BackgroundTaskID(ctx)
	hosted := broker != nil && broker.Hosted() && taskID != ""
	if hosted {
		ctx = tools.WithBackgroundWorkerID(ctx, spec.AgentID)
		defer func() { _ = broker.FinishWorker(ctx, spec.AgentID, outcome) }()
		release, err := broker.BeginWorker(ctx, spec.AgentID)
		if err != nil {
			return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "cancelled", Error: err.Error()}
		}
		defer release()
	}
	if err := r.Executor.requireAccount(ctx); err != nil {
		return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: "delegated worker requires an authenticated Meadow account"}
	}
	if spec.AgentID == "" {
		return tools.SubtaskOutcome{
			ID: spec.ID, Status: "failed",
			Error: "delegated worker is missing its runtime-assigned subagent_id",
		}
	}
	if err := ctx.Err(); err != nil {
		return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "cancelled", Error: err.Error()}
	}
	store := newSubtaskStore(r.StateDir)
	state, existing, err := store.load(spec.AgentID)
	if err != nil {
		return tools.SubtaskOutcome{
			ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: err.Error(),
		}
	}
	if existing && state.ParentSessionID != r.ParentSessionID {
		return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: "subagent belongs to a different parent session"}
	}
	if existing && state.ParentCWD != "" && state.ParentCWD != r.CWD {
		return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: "subagent belongs to a different parent workspace"}
	}
	recovering := existing && (state.Status == "running" || state.Status == "interrupted")
	if existing && !recovering {
		return tools.SubtaskOutcome{
			ID: spec.ID, SubagentID: spec.AgentID, Status: "failed",
			Error: fmt.Sprintf("subagent_id %s already has terminal state %s", spec.AgentID, state.Status),
		}
	}

	childCWD := r.CWD
	worktreePath := ""
	snapshotRef := ""
	resumedFrom := strings.TrimSpace(spec.ResumeFrom)
	var history []provider.Message
	if recovering {
		spec.Isolation = state.Spec.Isolation
		childCWD = state.CWD
		worktreePath = state.WorktreePath
		snapshotRef = state.SnapshotRef
		resumedFrom = state.ResumedFrom
		history = append([]provider.Message(nil), state.History...)
		state.Status = "running"
		state.Spec = spec
	} else {
		if resumedFrom != "" {
			source, found, loadErr := store.load(resumedFrom)
			if loadErr != nil {
				return tools.SubtaskOutcome{
					ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: loadErr.Error(),
				}
			}
			if !found {
				return tools.SubtaskOutcome{
					ID: spec.ID, SubagentID: spec.AgentID, Status: "failed",
					Error: fmt.Sprintf("unknown resume_from subagent_id %s", resumedFrom),
				}
			}
			if source.Status == "running" || source.Status == "interrupted" {
				return tools.SubtaskOutcome{
					ID: spec.ID, SubagentID: spec.AgentID, Status: "failed",
					Error: fmt.Sprintf("cannot resume active subagent %s", resumedFrom),
				}
			}
			if r.ParentSessionID != source.ParentSessionID {
				return tools.SubtaskOutcome{
					ID: spec.ID, SubagentID: spec.AgentID, Status: "failed",
					Error: fmt.Sprintf("subagent %s belongs to a different parent session", resumedFrom),
				}
			}
			if source.ParentCWD != "" && source.ParentCWD != r.CWD {
				return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: "subagent belongs to a different parent workspace"}
			}
			spec.Isolation = source.Spec.Isolation
			childCWD = source.CWD
			worktreePath = source.WorktreePath
			snapshotRef = source.SnapshotRef
			history = append([]provider.Message(nil), source.History...)
		} else if spec.Isolation == "worktree" {
			if err := r.Executor.requireAccount(ctx); err != nil {
				return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: "delegated worker requires an authenticated Meadow account"}
			}
			baseDir := ""
			if store.enabled() {
				baseDir = filepath.Join(filepath.Dir(store.dir), "worktrees")
			}
			worktree, worktreeErr := createSubtaskWorktree(
				ctx, r.CWD, baseDir, spec.AgentID,
			)
			if worktreeErr != nil {
				return tools.SubtaskOutcome{
					ID: spec.ID, SubagentID: spec.AgentID, Status: "failed",
					Error: worktreeErr.Error(),
				}
			}
			childCWD = worktree.CWD
			worktreePath = worktree.Path
			snapshotRef = worktree.SnapshotRef
		}
		state = subtaskState{
			SubagentID: spec.AgentID, ParentSessionID: r.ParentSessionID, ParentCWD: r.CWD,
			ModelID: r.ModelID, Spec: spec, Status: "running",
			CWD: childCWD, WorktreePath: worktreePath, SnapshotRef: snapshotRef,
			ResumedFrom: resumedFrom, History: history,
		}
	}
	if worktreePath != "" {
		release, leaseErr := acquireSubtaskWorkspace(ctx, worktreePath)
		if leaseErr != nil {
			return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "cancelled", Error: leaseErr.Error()}
		}
		defer release()
	}
	if state.ParentSessionID == "" {
		state.ParentSessionID = r.ParentSessionID
	}
	if state.ModelID == "" {
		state.ModelID = r.ModelID
	}
	if state.CWD == "" {
		state.CWD = childCWD
	}
	if err := store.save(state); err != nil {
		return tools.SubtaskOutcome{
			ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: err.Error(),
		}
	}

	childRegistry := tools.SubtaskRegistry(r.Executor.Registry)
	sharedWorkspace := spec.Isolation == "workspace"
	if sharedWorkspace {
		if !r.AllowSharedWorkspace || !hosted || childCWD != r.CWD || worktreePath != "" {
			return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: "shared workspace capability unavailable"}
		}
		childRegistry = tools.SharedWorkspaceSubtaskRegistry(r.Executor.Registry)
	}
	if worktreePath != "" {
		workspace, workspaceErr := tools.NewWorkspace(childCWD)
		if workspaceErr != nil {
			return tools.SubtaskOutcome{
				ID: spec.ID, SubagentID: spec.AgentID, Status: "failed",
				Error: workspaceErr.Error(), WorktreePath: worktreePath,
			}
		}
		var registryErr error
		childRegistry, registryErr = tools.InheritedWorktreeSubtaskRegistry(r.Executor.Registry, workspace)
		if registryErr != nil {
			return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: registryErr.Error(), WorktreePath: worktreePath}
		}
		defer childRegistry.Close()
	}
	// Child execution inherits only the authority boundary and resource limits.
	// Parent UI/recovery callbacks belong to a different transcript.
	childExecutor := &ToolExecutor{
		Registry: childRegistry, Authorize: r.Executor.Authorize,
		RequireAccount: r.Executor.RequireAccount,
		Parallel:       true, MaxParallel: r.Executor.MaxParallel,
	}
	childCtx, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	var checkpointErr error
	initialMessages := []tools.WorkerMessageReceipt{}
	if hosted {
		initialMessages, err = broker.TakeInitialWorkerMessages(taskID, spec.AgentID)
		if err != nil {
			return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: err.Error()}
		}
	}
	pendingApplied := map[string]GuidanceReceipt{}
	childHooks := KernelHooks{OnCheckpoint: func(messages []provider.Message) {
		state.History = append([]provider.Message(nil), messages...)
		state.Status = "running"
		if saveErr := store.save(state); saveErr != nil && checkpointErr == nil {
			checkpointErr = saveErr
			cancelChild()
		}
		if hosted && checkpointErr == nil {
			for _, message := range initialMessages {
				if err := broker.SetWorkerMessageStatus(taskID, spec.AgentID, message.ID, "applied", 1); err != nil {
					checkpointErr = errors.Join(checkpointErr, err)
					cancelChild()
				}
			}
			initialMessages = nil
			for id, receipt := range pendingApplied {
				if err := broker.SetWorkerMessageStatus(taskID, spec.AgentID, id, "applied", receipt.TargetTurn); err != nil {
					checkpointErr = errors.Join(checkpointErr, err)
					cancelChild()
				} else {
					delete(pendingApplied, id)
				}
			}
			if checkpointErr == nil {
				broker.FlushWorkerMessages(taskID, spec.AgentID)
			}
		}
	}}
	if hosted {
		childHooks.OnGuidance = func(receipt GuidanceReceipt) {
			if receipt.Disposition == GuidanceApplied {
				pendingApplied[receipt.ID] = receipt
				return
			}
			if err := broker.SetWorkerMessageStatus(taskID, spec.AgentID, receipt.ID, string(receipt.Disposition), receipt.TargetTurn); err != nil {
				checkpointErr = errors.Join(checkpointErr, err)
				cancelChild()
			}
		}
	}
	childPrompt := strings.TrimSpace(r.SystemPrompt) + `

# Isolated delegated worker
- Solve only the delegated goal below. You do not inherit the parent conversation.
- Return a concise summary with concrete paths, URLs, observations, uncertainty, and what the parent should verify.
- You may use task_plan if useful, but you decide whether it is needed.`
	if sharedWorkspace {
		childPrompt += "\n- You use the original shared user workspace with the parent's exact tool/account/permission limits. Changes affect that actual workspace. Coordinate your bounded delegated scope; do not touch unrelated work or claim Git isolation. Session control, scheduling and delegation stay with the primary companion."
	} else if worktreePath == "" {
		childPrompt += `
- Your tools are read-only inspection and research tools. Do not claim to have changed files or external state.`
	} else {
		childPrompt += fmt.Sprintf(`
- You are working in a writable isolated git worktree at %s.
- You may edit files and run verification there. Never claim the parent checkout changed.
- Report changed files, validation evidence, and the worktree path so the parent can inspect or integrate it.`, worktreePath)
	}
	child, err := r.NewChild(childExecutor, childCWD, spec.AgentID, childPrompt, childHooks, policy)
	if err != nil {
		return tools.SubtaskOutcome{
			ID: spec.ID, SubagentID: spec.AgentID, Status: "failed",
			Error: err.Error(), WorktreePath: worktreePath, ResumedFrom: resumedFrom,
		}
	}
	if hosted {
		err = broker.AttachWorker(ctx, spec.AgentID, func(message tools.WorkerMessageReceipt) (int, error) {
			receiver, ok := child.(interface {
				SubmitGuidance(Guidance) (GuidanceReceipt, error)
			})
			if !ok {
				return 0, errors.New("worker does not accept live guidance")
			}
			receipt, err := receiver.SubmitGuidance(Guidance{ID: message.ID, Text: message.Text, Mode: GuidanceSteerCurrent, PreserveText: true})
			return receipt.TargetTurn, err
		}, cancelChild)
		if err != nil {
			return tools.SubtaskOutcome{ID: spec.ID, SubagentID: spec.AgentID, Status: "failed", Error: err.Error()}
		}
	}
	if closer, ok := child.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	content := spec.Goal
	if spec.Context != "" {
		content += "\n\nContext supplied by the parent model:\n" + spec.Context
	}
	if recovering {
		content = "Continue the interrupted delegated task from the durable transcript. " +
			"Re-check the latest observable state before acting; do not repeat completed work unless verification requires it." +
			"\n\nOriginal delegated goal:\n" + state.Spec.Goal
		if state.Spec.Context != "" {
			content += "\n\nOriginal parent context:\n" + state.Spec.Context
		}
	}
	for _, message := range initialMessages {
		content += "\n\nMessage from parent coordinator [" + message.ID + "]:\n" + message.Text
	}
	result, runErr := child.RunInput(childCtx, RunInput{
		User: provider.Message{Role: "user", Content: content}, History: history,
	})
	if result == nil {
		message := "subtask returned no result"
		if runErr != nil {
			message = runErr.Error()
		}
		if checkpointErr != nil {
			message = checkpointErr.Error()
		}
		state.Status = "failed"
		state.Outcome = tools.SubtaskOutcome{
			ID: spec.ID, SubagentID: spec.AgentID, Status: "failed",
			Error: message, WorktreePath: worktreePath, ResumedFrom: resumedFrom,
			ResumeFromHint: spec.AgentID,
		}
		_ = store.save(state)
		return state.Outcome
	}
	status := "completed"
	if result.Stop != StopCompleted {
		status = "incomplete"
	}
	if runErr != nil {
		status = "failed"
	}
	if checkpointErr != nil {
		status = "failed"
		runErr = errors.Join(checkpointErr, runErr)
	}
	if ctx.Err() != nil || childCtx.Err() != nil {
		status = "cancelled"
		if errors.Is(context.Cause(ctx), tools.ErrBackgroundBrokerShutdown) {
			status = "interrupted"
		}
	}
	summary := strings.TrimSpace(result.FinalText)
	if len(summary) > subtaskSummaryMaxBytes {
		summary = strings.ToValidUTF8(summary[:subtaskSummaryMaxBytes], "") + "\n[summary truncated]"
	}
	outcome = tools.SubtaskOutcome{
		ID: spec.ID, SubagentID: spec.AgentID, Status: status, Summary: summary,
		StopReason: string(result.Stop), Turns: result.Turns, ToolCalls: result.ToolCalls,
		WorktreePath: worktreePath, ResumedFrom: resumedFrom,
		ResumeFromHint: spec.AgentID,
	}
	if runErr != nil {
		outcome.Error = runErr.Error()
	}
	state.Status = status
	state.History = append([]provider.Message(nil), result.Messages...)
	state.Outcome = outcome
	if err := store.save(state); err != nil {
		outcome.Status = "failed"
		outcome.Error = err.Error()
	}
	return outcome
}

func (p SubtaskPolicy) String() string {
	p = normalizeSubtaskPolicy(p)
	return fmt.Sprintf("enabled=%t tasks=%d concurrent=%d turns=%d depth=%d/%d", p.Enabled, p.MaxTasks, p.MaxConcurrent, p.MaxTurns, p.Depth, p.MaxDepth)
}
