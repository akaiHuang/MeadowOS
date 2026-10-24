package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/tools"
	"gopkg.in/yaml.v3"
)

type descriptor struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	File        string
}
type surface struct {
	policy          string
	skills, roles   []descriptor
	hooks           map[string][]hookGroup
	census          map[string]int
	activity        activityLog
	nativeToolNames []string
}
type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}
type hookGroup struct {
	Matcher string        `json:"matcher"`
	Hooks   []hookCommand `json:"hooks"`
}

var surfacePath = regexp.MustCompile(`^(AGENTS\.md|\.agents/skills/[a-z0-9_-]+/SKILL\.md|\.agents/skills/[a-z0-9_-]+/scripts/[a-zA-Z0-9_.-]+|sub_agents/[a-z0-9_-]+/AGENT\.md|scripts/[a-z0-9_]+\.py|\.codex/hooks\.json)$`)

func loadSurface(f *files) (*surface, error) {
	s := &surface{hooks: map[string][]hookGroup{}, census: map[string]int{"skills": 0, "subagents": 0, "scripts": 0, "hook_events": 0}}
	s.nativeToolNames = f.nativeToolNames
	if f.harness == nil {
		return s, nil
	}
	rootFS := f.harness.FS()
	e := fs.WalkDir(rootFS, ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, "/README.md") {
			return nil
		}
		if !surfacePath.MatchString(p) {
			return candidateValidationf("unsupported harness path: %s", p)
		}
		b, e := f.read("/harness/" + p)
		if e != nil {
			return e
		}
		switch {
		case p == "AGENTS.md":
			if len([]rune(string(b))) > 8000 || strings.TrimSpace(string(b)) == "" {
				return candidateValidationf("AGENTS.md must contain 1..8000 characters")
			}
			s.policy = string(b)
		case strings.HasSuffix(p, "/SKILL.md"), strings.HasSuffix(p, "/AGENT.md"):
			if len(b) > 16*1024 {
				return candidateValidationf("skill/role exceeds 16 KiB")
			}
			body := strings.ReplaceAll(string(b), "\r\n", "\n")
			if !strings.HasPrefix(body, "---\n") {
				return candidateValidationf("frontmatter missing: %s", p)
			}
			meta, _, ok := strings.Cut(body[4:], "\n---\n")
			if !ok {
				return candidateValidationf("frontmatter incomplete: %s", p)
			}
			var desc descriptor
			if e = yaml.Unmarshal([]byte(meta), &desc); e != nil {
				return candidateValidationf("invalid frontmatter in %s: %v", p, e)
			}
			name := path.Base(path.Dir(p))
			if desc.Name != "" && desc.Name != name {
				return candidateValidationf("skill/role name must match directory")
			}
			desc.Name = name
			desc.File = "/harness/" + p
			if strings.TrimSpace(desc.Description) == "" || len(desc.Description) > 500 {
				return candidateValidationf("skill/role needs nonempty description: %s has %d UTF-8 bytes; maximum 500 UTF-8 bytes", p, len(desc.Description))
			}
			if strings.HasSuffix(p, "/SKILL.md") {
				s.skills = append(s.skills, desc)
				s.census["skills"]++
			} else {
				s.roles = append(s.roles, desc)
				s.census["subagents"]++
			}
		case p == ".codex/hooks.json":
			if len(b) > 64*1024 {
				return candidateValidationf("hooks exceed 64 KiB")
			}
			var cfg struct {
				Hooks map[string][]hookGroup `json:"hooks"`
			}
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.DisallowUnknownFields()
			if e = dec.Decode(&cfg); e != nil {
				return candidateValidationf("invalid hooks JSON: %v", e)
			}
			if dec.Decode(new(any)) != io.EOF {
				return candidateValidationf("trailing hooks content")
			}
			for event, groups := range cfg.Hooks {
				if event != "PreToolUse" && event != "PostToolUse" {
					return candidateValidationf("unsupported hook event: %s", event)
				}
				if len(groups) > 8 {
					return candidateValidationf("too many hook groups")
				}
				for _, g := range groups {
					if len(g.Hooks) > 4 {
						return candidateValidationf("too many commands in hook group")
					}
					matcher := g.Matcher
					if matcher == "*" || matcher == "" {
						matcher = ".*"
					}
					if _, e = regexp.Compile("^(?:" + matcher + ")$"); e != nil {
						return candidateValidationf("invalid hook matcher")
					}
					for _, h := range g.Hooks {
						if h.Type != "command" || h.Command == "" || len(h.Command) > 4096 || h.Timeout < 0 || h.Timeout > 60 {
							return candidateValidationf("only bounded command hooks are supported")
						}
					}
				}
				s.hooks[event] = groups
				s.census["hook_events"]++
			}
		case strings.HasSuffix(p, ".py"):
			s.census["scripts"]++
		default:
			return candidateValidationf("unsupported executable type: %s", p)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	if s.policy == "" {
		return nil, candidateValidationf("frozen harness requires AGENTS.md")
	}
	for _, groups := range s.hooks {
		for _, g := range groups {
			matcher := g.Matcher
			if matcher == "" || matcher == "*" {
				matcher = ".*"
			}
			matches := false
			for _, name := range s.toolNames() {
				if ok, _ := regexp.MatchString("^(?:"+matcher+")$", name); ok {
					matches = true
					break
				}
			}
			if !matches {
				return nil, candidateValidationf("hook matcher %q matches no available Meadow tool", g.Matcher)
			}
		}
	}
	return s, nil
}

func (s *surface) instruction() string {
	return s.instructionFor(false)
}

func (s *surface) instructionFor(readOnlyChild bool) string {
	var b strings.Builder
	b.WriteString(s.policy)
	if len(s.skills) > 0 {
		b.WriteString("\n\nAvailable candidate skills: use skill_read with an exact name when relevant. Full instructions are loaded only on demand.\n")
		for _, d := range s.skills {
			fmt.Fprintf(&b, "- %s: %s\n", d.Name, d.Description)
		}
	}
	if len(s.roles) > 0 && !readOnlyChild {
		b.WriteString("\n\nAvailable candidate subagent roles: delegate_task runs one independent read-only child context; the parent remains responsible for integration.\n")
		for _, d := range s.roles {
			fmt.Fprintf(&b, "- %s: %s\n", d.Name, d.Description)
		}
	}
	return b.String()
}

func (s *surface) skillTool(f *files) tools.Tool {
	names := []string{}
	for _, d := range s.skills {
		names = append(names, d.Name)
	}
	return &nativeTool{name: "skill_read", description: "Load the full text of one frozen candidate skill by exact name.", kind: tools.KindReadOnly, schema: schema(map[string]any{"name": map[string]any{"type": "string", "enum": names}}, "name"), call: func(_ context.Context, a map[string]any) tools.Result {
		for _, d := range s.skills {
			if d.Name == text(a, "name") {
				b, e := f.read(d.File)
				if e != nil {
					return failure(e)
				}
				return tools.Result{LLMContent: string(b)}
			}
		}
		return failure(errors.New("unknown candidate skill"))
	}}
}

type hookedTool struct {
	tools.Tool
	hooks   *surface
	sandbox *sandbox
}

func (t *hookedTool) ObserveWorkspaceFileState(p string) (tools.WorkspaceFileState, error) {
	if observer, ok := t.Tool.(tools.WorkspaceFileStateObserver); ok {
		return observer.ObserveWorkspaceFileState(p)
	}
	return tools.WorkspaceFileState{}, errors.New("file state observation unavailable")
}

func wrapHooks(reg *tools.Registry, s *surface, sb *sandbox) *tools.Registry {
	if len(s.hooks) == 0 {
		return reg
	}
	wrapped := tools.NewRegistry()
	for _, name := range reg.Names() {
		tool, _ := reg.Get(name)
		wrapped.Register(&hookedTool{Tool: tool, hooks: s, sandbox: sb})
	}
	return wrapped
}

func (t *hookedTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	if t.hooks == nil || len(t.hooks.hooks) == 0 {
		return t.Tool.Execute(ctx, args)
	}
	if t.sandbox == nil {
		return failure(errors.New("hooks require a working OS sandbox"))
	}
	return t.sandbox.files.trackMutations(t.sandbox.scratchName(), func() tools.Result { return t.executeHooks(ctx, args) })
}

func (t *hookedTool) executeHooks(ctx context.Context, args map[string]any) tools.Result {
	if note, e := t.runHooks(ctx, "PreToolUse", args, nil); e != nil {
		return failure(e)
	} else if note != "" {
		return failure(errors.New("hook blocked: " + note))
	}
	result := t.Tool.Execute(ctx, args)
	if note, e := t.runHooks(ctx, "PostToolUse", args, &result); e != nil {
		result.Err = e
		result.LLMContent += "\nPOST_HOOK_ERROR: " + e.Error()
	} else if note != "" {
		result.LLMContent += "\nPOST_HOOK_FEEDBACK: " + note
	}
	return result
}

func (t *hookedTool) runHooks(ctx context.Context, event string, args map[string]any, result *tools.Result) (string, error) {
	payload := map[string]any{"hook_event_name": event, "tool_name": t.Name(), "tool_input": args,
		"cwd": t.sandbox.files.workspacePath, "virtual_cwd": "/workspace",
		"workspace_root": t.sandbox.files.workspacePath, "harness_root": t.sandbox.files.harnessPath}
	if result != nil {
		payload["tool_response"] = map[string]any{"output": result.LLMContent, "success": result.Err == nil}
	}
	input, _ := json.Marshal(payload)
	for _, g := range t.hooks.hooks[event] {
		matcher := g.Matcher
		if matcher == "" || matcher == "*" {
			matcher = ".*"
		}
		matched, _ := regexp.MatchString("^(?:"+matcher+")$", t.Name())
		if !matched {
			continue
		}
		for _, h := range g.Hooks {
			hookCtx := ctx
			cancel := func() {}
			if h.Timeout > 0 {
				hookCtx, cancel = context.WithTimeout(ctx, time.Duration(h.Timeout)*time.Second)
			}
			// Hook commands are untrusted programs, not host actions. Their stdin
			// has only model-visible call data; credentials never enter the process.
			out, code, e := t.sandbox.run(hookCtx, []string{"/bin/sh", "-c", h.Command}, input)
			cancel()
			processOK := e == nil && code == 0
			record := func(success bool) {
				t.hooks.activity.add(map[string]any{"component": "hook", "event": event, "tool": t.Name(), "matcher": g.Matcher, "success": success, "process_success": processOK, "exit_code": code})
			}
			if e != nil {
				record(false)
				return "", e
			}
			if code != 0 {
				record(false)
				return "", fmt.Errorf("%s hook exited %d", event, code)
			}
			if strings.TrimSpace(out) == "" {
				record(true)
				continue
			}
			var decision struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}
			if e = json.Unmarshal([]byte(out), &decision); e != nil {
				record(false)
				return "", errors.New("hook output must be empty or a JSON decision")
			}
			if decision.Decision == "block" {
				if len(decision.Reason) > 4096 {
					record(false)
					return "", errors.New("hook feedback exceeds bound")
				}
				if strings.TrimSpace(decision.Reason) == "" {
					decision.Reason = "candidate hook requested a block"
				}
				record(true)
				return decision.Reason, nil
			}
			if decision.Decision != "" && decision.Decision != "allow" {
				record(false)
				return "", errors.New("unsupported hook decision")
			}
			record(true)
		}
	}
	return "", nil
}
