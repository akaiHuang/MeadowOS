package evolve

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

type activityLog struct {
	mu     sync.Mutex
	events []map[string]any
}

func (a *activityLog) add(event map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	event["sequence"] = len(a.events) + 1
	a.events = append(a.events, event)
}

func (a *activityLog) snapshot() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any{}, a.events...)
}

type observedCandidateTool struct {
	tools.Tool
	surface *surface
	scope   string
}

func (t *observedCandidateTool) ObserveWorkspaceFileState(p string) (tools.WorkspaceFileState, error) {
	if observer, ok := t.Tool.(tools.WorkspaceFileStateObserver); ok {
		return observer.ObserveWorkspaceFileState(p)
	}
	return tools.WorkspaceFileState{}, errors.New("file state observation unavailable")
}

func (t *observedCandidateTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	result := t.Tool.Execute(ctx, args)
	component, name := "", ""
	switch t.Name() {
	case "skill_read":
		component, name = "skill", text(args, "name")
	case "delegate_task":
		component, name = "subagent", text(args, "role")
	case "run_harness_script":
		component, name = "script", text(args, "file_path")
	case "run_python":
		if strings.HasPrefix(text(args, "file_path"), "/harness/") {
			component, name = "script", text(args, "file_path")
		}
	case "read_file":
		p := text(args, "file_path")
		if strings.HasPrefix(p, "/harness/.agents/skills/") && strings.HasSuffix(p, "/SKILL.md") {
			component, name = "skill", p
		}
	}
	if component != "" {
		t.surface.activity.add(map[string]any{"component": component, "tool": t.Name(), "name": name, "scope": t.scope, "success": result.Err == nil})
	}
	return result
}

func observeCandidates(reg *tools.Registry, s *surface, scope string) *tools.Registry {
	out := tools.NewRegistry()
	for _, name := range reg.Names() {
		tool, _ := reg.Get(name)
		switch name {
		case "skill_read", "delegate_task", "run_harness_script", "run_python", "read_file":
			tool = &observedCandidateTool{Tool: tool, surface: s, scope: scope}
		}
		out.Register(tool)
	}
	return out
}
