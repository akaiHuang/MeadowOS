package tools

import "context"

// CoordinatorRegistry is a capability topology, independent of user wording.
// Discovery and execution see bounded session controls and chat-owned image
// evidence. The full registry remains with the immutable worker runner.
func CoordinatorRegistry(base *Registry) *Registry {
	result := base.EmptyView()
	if base == nil {
		return result
	}
	hostedBroker, hasHostedBroker := FindBackgroundTaskBroker(base)
	hasHostedBroker = hasHostedBroker && hostedBroker.Hosted()
	for _, name := range []string{DelegateTaskToolName, BackgroundTaskToolName,
		SchedulerCreateToolName, SchedulerListToolName, SchedulerControlToolName, SchedulerDeleteToolName,
		"ask_user_question", TaskPlanToolName, "todo_write", "memory_recall", "memory_remember", "memory_status"} {
		tool, ok := base.Get(name)
		if !ok {
			continue
		}
		if name == DelegateTaskToolName {
			delegate, ok := tool.(*DelegateTask)
			if !ok || !hasHostedBroker || delegate.Broker != hostedBroker {
				continue
			}
		}
		if name == BackgroundTaskToolName {
			if !hasHostedBroker {
				continue
			}
			tool = &coordinatorTaskOutput{broker: hostedBroker}
		}
		result.Register(tool)
	}
	// The conversation partner can observe its own computer without occupying
	// a worker. Native input remains unavailable in this readonly view.
	if tool, ok := base.Get("desktop"); ok {
		if desktop, ok := tool.(*DesktopControl); ok {
			result.Register(desktop.ReadOnlyView())
		}
	}
	if tool, ok := base.Get("system_info"); ok && tool.Kind() == KindReadOnly {
		result.Register(tool)
	}
	// These read-only tools resolve only the original conversation's opaque
	// collection IDs. Preserve their source binding; never synthesize a new
	// source or substitute an unrecognized implementation with the same name.
	for _, name := range []string{ListAttachmentImagesToolName, InspectAttachmentImagesToolName} {
		if tool, ok := base.Get(name); ok {
			switch tool.(type) {
			case *ListAttachmentImages, *InspectAttachmentImages:
				result.Register(tool)
			}
		}
	}
	return result
}

// Do not embed the broker: its lifecycle and notification ownership remain on
// the session's full registry, not on this run-local presentation view.
type coordinatorTaskOutput struct{ broker *BackgroundTaskBroker }

func (t *coordinatorTaskOutput) Name() string { return BackgroundTaskToolName }
func (t *coordinatorTaskOutput) Description() string {
	return t.broker.Description() + " This coordinator control is nonblocking: wait is unavailable. Use a single poll when evidence is needed, then let completion wake the conversation."
}
func (t *coordinatorTaskOutput) Kind() Kind { return t.broker.Kind() }
func (t *coordinatorTaskOutput) ClassifyCall(args map[string]any) Kind {
	return t.broker.ClassifyCall(args)
}
func (t *coordinatorTaskOutput) Schema() map[string]any {
	schema := t.broker.Schema()
	props := schema["properties"].(map[string]any)
	props["action"].(map[string]any)["enum"] = []string{"list", "poll", "cancel", "steer_worker", "cancel_worker"}
	delete(props, "timeout")
	return schema
}
func (t *coordinatorTaskOutput) Execute(ctx context.Context, args map[string]any) Result {
	action := strArg(args, "action")
	switch action {
	case "list", "poll", "cancel", "steer_worker", "cancel_worker":
		return t.broker.Execute(ctx, args)
	default:
		return errResultf("coordinator task_output is nonblocking; use list, poll, steer_worker, cancel_worker or cancel")
	}
}

// SharedWorkspaceSubtaskRegistry preserves exact parent tool implementations
// and revocations. It does not synthesize a broader default registry or change
// CWD. Session controls remain primary-owned and model plan state is private.
func SharedWorkspaceSubtaskRegistry(base *Registry) *Registry {
	result := base.EmptyView()
	if base == nil {
		return result
	}
	for _, name := range base.Names() {
		switch name {
		case DelegateTaskToolName, BackgroundTaskToolName, SchedulerCreateToolName, SchedulerListToolName, SchedulerControlToolName, SchedulerDeleteToolName,
			"ask_user_question", TaskPlanToolName, "todo_write", MonitorToolName, "save_conversation_text":
			continue
		}
		tool, ok := base.Get(name)
		if ok {
			result.Register(tool)
		}
	}
	if _, ok := FindTaskPlanStateTool(base); ok {
		result.Register(NewTaskPlan())
	}
	return result
}
