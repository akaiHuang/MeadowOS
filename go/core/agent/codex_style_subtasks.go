package agent

import (
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// subtaskExecutor binds the child runner to one admitted parent turn. Both the
// provider/executor and inference controls are snapshots, so a future Rebind or
// UI effort selection cannot change a running child's identity or authority.
// A trusted hosted broker can retain the snapshot after this parent turn ends.
// Without one delegation remains foreground-only and inherits cancellation.
func (r *CodexStyleRuntime) subtaskExecutor(base *ToolExecutor, thinking provider.ThinkingMode, effort provider.ReasoningEffort) *ToolExecutor {
	config := r.config
	policy := normalizeSubtaskPolicy(config.Subtasks)
	if !policy.Enabled || policy.Depth >= policy.MaxDepth {
		if config.CoordinatorOnly {
			executor := *base
			executor.Registry = tools.CoordinatorRegistry(base.Registry.Excluding([]string{tools.DelegateTaskToolName}))
			return &executor
		}
		return base
	}
	registry := base.Registry.Select(base.Registry.Names())
	executor := *base
	executor.Registry = registry
	workerExecutor := executor
	broker, _ := tools.FindBackgroundTaskBroker(registry)
	runner := subtaskRunner{
		Broker: broker, AllowSharedWorkspace: config.CoordinatorOnly,
		Policy: policy, Executor: &workerExecutor,
		CWD: config.CWD, ModelID: config.ModelID, StateDir: config.SubtaskStateDir,
		ParentSessionID: config.SessionID, SystemPrompt: config.SystemPrompt,
		NewChild: func(childExecutor *ToolExecutor, cwd, id, prompt string, hooks KernelHooks, childPolicy SubtaskPolicy) (Runtime, error) {
			childConfig := config
			childConfig.Executor, childConfig.CWD, childConfig.SystemPrompt = childExecutor, cwd, prompt
			childConfig.CacheSalt = r.cacheSalt + ":subagent:" + id
			childConfig.SessionID = config.SessionID + ":" + id
			childConfig.MaxTurns = min(config.MaxTurns, childPolicy.MaxTurns)
			// A child owns its transcript. Parent recovery and stop hooks must
			// not observe or checkpoint unrelated child tool-call identities.
			childConfig.Recovery, childConfig.ResumePlan, childConfig.RecoveryError = nil, nil, nil
			childConfig.RecoveryRequired = false
			childConfig.StopHooks = nil
			childConfig.CoordinatorOnly = false
			childConfig.Subtasks = SubtaskPolicy{}
			childConfig.SubtaskStateDir = ""
			child, err := NewCodexStyleRuntime(childConfig, hooks)
			if err != nil {
				return nil, err
			}
			child.thinkingMode, child.reasoningEffort = thinking, effort
			return child, nil
		},
	}
	delegate := tools.NewDelegateTask(policy.MaxTasks, runner.run)
	delegate.AllowSharedWorkspace = config.CoordinatorOnly
	// Hosted brokers preserve this immutable provider/executor snapshot and own
	// cancellation independently from the parent foreground turn.
	if broker != nil && broker.Hosted() {
		delegate.Broker = broker
	}
	registry.Register(delegate)
	if config.CoordinatorOnly {
		executor.Registry = tools.CoordinatorRegistry(registry)
	}
	return &executor
}
