package tools

const (
	qwen35OfficeReadMaxBytes        = 48 * 1024
	qwen35ResearchFetchDefaultChars = 6_000
	museGlimmerReadMaxBytes         = 32 * 1024
	ornithCodingReadMaxBytes        = 64 * 1024
	qwen38CodingReadMaxBytes        = 64 * 1024
	gemma12AgentReadMaxBytes        = 48 * 1024
	museGlimmerFetchDefaultChars    = 4_000
	lfm25ResearchFetchDefaultChars  = 4_000
)

// DefaultRegistry assembles the core tools over one workspace. asker may be nil
// (headless); the registry order is
// the order schemas are presented to the model.
func DefaultRegistry(ws *Workspace, asker Asker) *Registry {
	processManager := NewProcessManager(ws)
	registry := NewRegistry(
		&ReadFile{WS: ws},
		&CodeOutline{WS: ws},
		&InspectImage{WS: ws},
		&WriteFile{WS: ws},
		&Edit{WS: ws},
		&Shell{WS: ws, Manager: processManager},
		&ProcessTool{Manager: processManager},
		&Glob{WS: ws},
		&Grep{WS: ws},
		&Ls{WS: ws},
		&WebFetch{},
		&WebSearch{},
		&GetCurrentLocation{},
		&SearchPlaces{},
		&WebCrawl{},
		&DownloadImage{WS: ws},
		&BrowsePage{WS: ws},
		&MeadowBrowserVisualCheck{WS: ws},
		&MeadowBrowserWebMCP{WS: ws},
		&ResearchArtifact{WS: ws},
		&TodoWrite{},
		&AskUser{Ask: asker},
	)
	registerBundledSkills(registry)
	return registry
}

// Gemma12AgentRegistry is the compact general-agent capability surface for
// Meadow 12B. It deliberately does not reuse the Qwen or Muse registries:
// Gemma receives the same real capabilities, but keeps its own schema order,
// context budget, and serialized tool-call behavior.
func Gemma12AgentRegistry(base *Registry, ws *Workspace) *Registry {
	if base == nil || ws == nil {
		return nil
	}
	result := NewRegistry()
	copyBundledSkills(result, base)
	result.Register(&ReadFile{WS: ws, MaxBytes: gemma12AgentReadMaxBytes})
	registerCodeOutline(result, base, ws)
	result.Register(&WriteFile{WS: ws})
	result.Register(&Edit{WS: ws})
	result.Register(&Glob{WS: ws})
	result.Register(&Grep{WS: ws})
	result.Register(&Ls{WS: ws})
	for _, name := range []string{
		"run_shell_command", "process", TaskPlanToolName, "web_search", "web_fetch",
	} {
		if tool, ok := base.Get(name); ok {
			result.Register(tool)
		}
	}
	if _, ok := result.Get(TaskPlanToolName); !ok {
		result.Register(NewTaskPlan())
	}
	result.Register(&PDFExtract{WS: ws})
	dom := &MeadowBrowserDOM{WS: ws, DefaultMaxChars: 5_000}
	if existing, ok := base.Get("meadow_browser_dom"); ok {
		if configured, ok := existing.(*MeadowBrowserDOM); ok {
			dom.BaseURL, dom.HTTP, dom.StartLocal = configured.BaseURL, configured.HTTP, configured.StartLocal
		}
	}
	result.Register(dom)
	visual := &MeadowBrowserVisualCheck{WS: ws}
	if existing, ok := base.Get("meadow_browser_visual_check"); ok {
		if configured, ok := existing.(*MeadowBrowserVisualCheck); ok {
			visual.BaseURL, visual.HTTP, visual.StartLocal = configured.BaseURL, configured.HTTP, configured.StartLocal
		}
	}
	result.Register(visual)
	interaction := &MeadowBrowserInteractionCheck{WS: ws}
	if existing, ok := base.Get("meadow_browser_interaction_check"); ok {
		if configured, ok := existing.(*MeadowBrowserInteractionCheck); ok {
			interaction.BaseURL, interaction.HTTP, interaction.StartLocal = configured.BaseURL, configured.HTTP, configured.StartLocal
		}
	}
	result.Register(interaction)
	result.Register(configuredMeadowBrowserWebMCP(base, ws))
	registerPresentationTools(result, base, ws)
	result.Register(&TypeScriptWebBuild{WS: ws})
	return result
}

// Qwen38CodingRegistry is the deliberately separate Qwen3.8 repository
// engineering surface.  Keep this function independent from the Ornith
// profile even while both families need the same capabilities: model-specific
// prompts and scheduling may evolve without silently changing the other
// product line.
func Qwen38CodingRegistry(base *Registry, ws *Workspace) *Registry {
	if base == nil || ws == nil {
		return nil
	}
	result := NewRegistry()
	copyBundledSkills(result, base)
	result.Register(&ReadFile{WS: ws, MaxBytes: qwen38CodingReadMaxBytes})
	registerCodeOutline(result, base, ws)
	result.Register(&WriteFile{WS: ws})
	result.Register(&Edit{WS: ws})
	result.Register(&Glob{WS: ws})
	result.Register(&Grep{WS: ws})
	result.Register(&Ls{WS: ws})
	for _, name := range []string{
		"run_shell_command", "process", TaskPlanToolName, "web_search", "web_fetch",
	} {
		if tool, ok := base.Get(name); ok {
			result.Register(tool)
		}
	}
	if _, ok := result.Get(TaskPlanToolName); !ok {
		result.Register(NewTaskPlan())
	}
	result.Register(&PDFExtract{WS: ws})
	dom := &MeadowBrowserDOM{WS: ws, DefaultMaxChars: 12_000}
	if existing, ok := base.Get("meadow_browser_dom"); ok {
		if configured, ok := existing.(*MeadowBrowserDOM); ok {
			dom.BaseURL, dom.HTTP, dom.StartLocal = configured.BaseURL, configured.HTTP, configured.StartLocal
		}
	}
	result.Register(dom)
	visual := &MeadowBrowserVisualCheck{WS: ws}
	if existing, ok := base.Get("meadow_browser_visual_check"); ok {
		if configured, ok := existing.(*MeadowBrowserVisualCheck); ok {
			visual.BaseURL, visual.HTTP, visual.StartLocal = configured.BaseURL, configured.HTTP, configured.StartLocal
		}
	}
	result.Register(visual)
	interaction := &MeadowBrowserInteractionCheck{WS: ws}
	if existing, ok := base.Get("meadow_browser_interaction_check"); ok {
		if configured, ok := existing.(*MeadowBrowserInteractionCheck); ok {
			interaction.BaseURL, interaction.HTTP, interaction.StartLocal = configured.BaseURL, configured.HTTP, configured.StartLocal
		}
	}
	result.Register(interaction)
	result.Register(configuredMeadowBrowserWebMCP(base, ws))
	registerPresentationTools(result, base, ws)
	result.Register(&TypeScriptWebBuild{WS: ws})
	return result
}

// OrnithCodingRegistry exposes one cohesive repository engineering surface.
// It intentionally starts from the caller's configured instances so shell and
// process ownership, permissions, browser connection and session hooks remain
// shared, then rebinds file/browser tools to the active session workspace.
// Presentation publishing and evidence tools are preserved
// so the resident 35B model can answer ordinary questions, inspect local work,
// or gather external sources without a separate intent router choosing a
// workflow on its behalf. Existing configured instances keep their permission,
// browser and network policy boundaries.
func OrnithCodingRegistry(base *Registry, ws *Workspace) *Registry {
	if base == nil || ws == nil {
		return nil
	}
	result := NewRegistry()
	copyBundledSkills(result, base)
	result.Register(&ReadFile{WS: ws, MaxBytes: ornithCodingReadMaxBytes})
	registerCodeOutline(result, base, ws)
	result.Register(&InspectImage{WS: ws})
	result.Register(&WriteFile{WS: ws})
	result.Register(&Edit{WS: ws})
	result.Register(&Glob{WS: ws})
	result.Register(&Grep{WS: ws})
	result.Register(&Ls{WS: ws})
	for _, name := range []string{
		"run_shell_command", "process", TaskPlanToolName,
		"web_search", "web_fetch", "get_current_location", "search_places", "meadow_browser_dom", "pdf_extract",
	} {
		if tool, ok := base.Get(name); ok {
			result.Register(tool)
		}
	}
	if _, ok := result.Get(TaskPlanToolName); !ok {
		result.Register(NewTaskPlan())
	}
	visual := &MeadowBrowserVisualCheck{WS: ws}
	if existing, ok := base.Get("meadow_browser_visual_check"); ok {
		if configured, ok := existing.(*MeadowBrowserVisualCheck); ok {
			visual.BaseURL, visual.HTTP, visual.StartLocal = configured.BaseURL, configured.HTTP, configured.StartLocal
		}
	}
	result.Register(visual)
	interaction := &MeadowBrowserInteractionCheck{WS: ws}
	if existing, ok := base.Get("meadow_browser_interaction_check"); ok {
		if configured, ok := existing.(*MeadowBrowserInteractionCheck); ok {
			interaction.BaseURL, interaction.HTTP, interaction.StartLocal = configured.BaseURL, configured.HTTP, configured.StartLocal
		}
	}
	result.Register(interaction)
	result.Register(&MeadowBrowserSuite{Checker: interaction})
	result.Register(configuredMeadowBrowserWebMCP(base, ws))
	registerPresentationTools(result, base, ws)
	result.Register(&TypeScriptWebBuild{WS: ws})
	return result
}

// registerCodeOutline rebinds an existing capability without introducing it
// into reduced registries or replacing a caller's constrained wrapper.
func registerCodeOutline(result, base *Registry, ws *Workspace) {
	if base == nil || result == nil {
		return
	}
	if existing, ok := base.Get("code_outline"); ok {
		if configured, ok := existing.(*CodeOutline); ok {
			result.Register(configured.ForWorkspace(ws))
		}
	}
}

// registerPresentationTools keeps discovery and delivery on the same configured
// private renderer while rebinding only artifact storage to the active chat.
func registerPresentationTools(result, base *Registry, ws *Workspace) {
	presentation := &PresentationArtifact{WS: ws}
	if existing, ok := base.Get("presentation_artifact"); ok {
		if configured, ok := existing.(*PresentationArtifact); ok {
			*presentation = *configured
			presentation.WS = ws
		}
	}
	catalog := &PresentationTemplates{
		PresentonURL: presentation.PresentonURL, PresentonToken: presentation.PresentonToken, HTTP: presentation.HTTP,
	}
	if existing, ok := base.Get("presentation_templates"); ok {
		if configured, ok := existing.(*PresentationTemplates); ok {
			*catalog = *configured
		}
	}
	result.Register(catalog)
	result.Register(presentation)
	result.Register(&PresentationFromMarkdown{Artifact: presentation})
}

// LFM25ResearchRegistry is the deliberately small evidence surface for the
// 2.6B research worker. Search discovers candidates; bounded fetch/DOM tools
// open evidence. Raw crawl, shell, mutation, and browser-page dumps are not
// presented to this model.
func LFM25ResearchRegistry(base *Registry, ws *Workspace) *Registry {
	if base == nil {
		return nil
	}
	result := NewRegistry()
	copyBundledSkills(result, base)
	if search, ok := base.Get("web_search"); ok {
		result.Register(search)
	} else {
		result.Register(&WebSearch{})
	}
	fetch := &WebFetch{DefaultMaxChars: lfm25ResearchFetchDefaultChars}
	if existing, ok := base.Get("web_fetch"); ok {
		if configured, ok := existing.(*WebFetch); ok {
			fetch.HTTP = configured.HTTP
		}
	}
	result.Register(fetch)
	result.Register(&PDFExtract{WS: ws})
	dom := &MeadowBrowserDOM{WS: ws, DefaultMaxChars: lfm25ResearchFetchDefaultChars}
	if existing, ok := base.Get("meadow_browser_dom"); ok {
		if configured, ok := existing.(*MeadowBrowserDOM); ok {
			dom.BaseURL = configured.BaseURL
			dom.HTTP = configured.HTTP
			dom.StartLocal = configured.StartLocal
		}
	}
	result.Register(dom)
	return result
}

// MuseGlimmerAgentRegistry is a separate capability surface for the Muse
// experiment. It deliberately does not call Qwen35OfficeRegistry so the two
// profiles can evolve, be measured, and be removed independently.
func MuseGlimmerAgentRegistry(base *Registry, ws *Workspace) *Registry {
	result := ModelLedRegistry(base)
	if result == nil {
		return nil
	}
	result.Register(&ReadFile{WS: ws, MaxBytes: museGlimmerReadMaxBytes})
	registerCodeOutline(result, base, ws)
	result.Register(&WriteFile{WS: ws})
	// JavaScript matching a TypeScript sibling is reproducible compiler output.
	// Accept Muse's full native transaction, but store only source artifacts so
	// a redundant JS entry cannot cost another dense-model retry or drift from TS.
	result.Register(&WriteFiles{
		WS: ws, DropCompiledJSSiblings: true, FlatPairArguments: true,
	})
	result.Register(&Edit{WS: ws})
	result.Register(&Glob{WS: ws})
	result.Register(&Grep{WS: ws})
	result.Register(&Ls{WS: ws})
	result.Register(&MeadowBrowserVisualCheck{WS: ws})
	result.Register(&MeadowBrowserInteractionCheck{WS: ws})
	museFetch := &WebFetch{DefaultMaxChars: museGlimmerFetchDefaultChars}
	if existing, ok := result.Get("web_fetch"); ok {
		if configured, ok := existing.(*WebFetch); ok {
			museFetch.HTTP = configured.HTTP
		}
	}
	result.Register(museFetch)
	result.Register(&PDFExtract{WS: ws})
	dom := &MeadowBrowserDOM{WS: ws, DefaultMaxChars: museGlimmerFetchDefaultChars}
	if existing, ok := result.Get("meadow_browser_dom"); ok {
		if configured, ok := existing.(*MeadowBrowserDOM); ok {
			dom.BaseURL = configured.BaseURL
			dom.HTTP = configured.HTTP
			dom.StartLocal = configured.StartLocal
		}
	}
	result.Register(dom)
	registerPresentationTools(result, base, ws)
	result.Register(&TypeScriptWebBuild{WS: ws})
	inheritDesktopBrowserTransport(result, base)
	return result
}

// ModelLedRegistry preserves every existing tool instance while replacing the
// legacy todo_write schema with task_plan. The two names are never exposed
// together, avoiding duplicate planning tools and keeping 12B/26B unchanged.
func ModelLedRegistry(base *Registry) *Registry {
	if base == nil {
		return nil
	}
	result := base.EmptyView()
	insertedPlan := false
	for _, name := range base.Names() {
		if name == "todo_write" || name == TaskPlanToolName {
			if !insertedPlan {
				result.Register(NewTaskPlan())
				insertedPlan = true
			}
			continue
		}
		if tool, ok := base.Get(name); ok {
			result.Register(tool)
		}
	}
	if !insertedPlan {
		result.Register(NewTaskPlan())
	}
	return result
}

// Qwen35OfficeRegistry is the independently versioned office capability
// surface for Meadow 35B. DefaultRegistry remains unchanged so the Gemma
// 12B/26B harnesses cannot accidentally inherit Qwen-specific prompt budget,
// browser state, or structured presentation behavior.
func Qwen35OfficeRegistry(base *Registry, ws *Workspace) *Registry {
	result := ModelLedRegistry(base)
	if result == nil {
		return nil
	}
	// The Desktop constructs its generic registry before a chat session exists.
	// Rebind every workspace-aware tool that the 35B artifact pipeline can expose
	// to the per-session workspace supplied by Office35. Otherwise write/edit/ls
	// and visual verification keep the project-only workspace from `base`, while
	// build uses `ws`, producing a split-brain `/session` mount.
	result.Register(&ReadFile{WS: ws, MaxBytes: qwen35OfficeReadMaxBytes})
	registerCodeOutline(result, base, ws)
	result.Register(&WriteFile{WS: ws})
	result.Register(&Edit{WS: ws})
	result.Register(&Glob{WS: ws})
	result.Register(&Grep{WS: ws})
	result.Register(&Ls{WS: ws})
	result.Register(&MeadowBrowserVisualCheck{WS: ws})
	result.Register(&MeadowBrowserInteractionCheck{WS: ws})
	qwenFetch := &WebFetch{DefaultMaxChars: qwen35ResearchFetchDefaultChars}
	if existing, ok := result.Get("web_fetch"); ok {
		if configured, ok := existing.(*WebFetch); ok {
			qwenFetch.HTTP = configured.HTTP
		}
	}
	result.Register(qwenFetch)
	result.Register(&PDFExtract{WS: ws})
	dom := &MeadowBrowserDOM{WS: ws, DefaultMaxChars: qwen35ResearchFetchDefaultChars}
	if existing, ok := result.Get("meadow_browser_dom"); ok {
		if configured, ok := existing.(*MeadowBrowserDOM); ok {
			dom.BaseURL = configured.BaseURL
			dom.HTTP = configured.HTTP
			dom.StartLocal = configured.StartLocal
		}
	}
	result.Register(dom)
	registerPresentationTools(result, base, ws)
	result.Register(&TypeScriptWebBuild{WS: ws})
	inheritDesktopBrowserTransport(result, base)
	return result
}

// SubtaskRegistry creates a fresh, read-only worker surface from tool-declared
// capabilities. Tool instances may be shared only when they explicitly opt in;
// task_plan is always new so child planning state cannot leak into the parent.
func SubtaskRegistry(base *Registry) *Registry {
	result := base.EmptyView()
	if base != nil {
		for _, name := range base.Names() {
			if name == TaskPlanToolName || name == "todo_write" || name == DelegateTaskToolName {
				continue
			}
			tool, ok := base.Get(name)
			if !ok {
				continue
			}
			if provider, ok := tool.(SubtaskToolProvider); ok {
				if reduced := provider.SubtaskTool(); reduced != nil {
					result.Register(reduced)
				}
				continue
			}
			access, ok := tool.(SubtaskAccessible)
			if ok && access.SubtaskSafe() {
				result.Register(tool)
			}
		}
	}
	result.Register(NewTaskPlan())
	return result
}

// WorktreeSubtaskRegistry gives an isolated child normal repository editing
// and execution tools rooted at its private git worktree. Session control,
// delegation, scheduling, and user-interaction tools stay parent-owned.
func WorktreeSubtaskRegistry(ws *Workspace) *Registry {
	base := ModelLedRegistry(DefaultRegistry(ws, nil))
	result := NewRegistry()
	copyBundledSkills(result, base)
	for _, name := range base.Names() {
		switch name {
		case "ask_user_question", "get_current_location", DelegateTaskToolName, BackgroundTaskToolName,
			SchedulerCreateToolName, SchedulerListToolName,
			SchedulerControlToolName, SchedulerDeleteToolName, MonitorToolName:
			continue
		}
		tool, ok := base.Get(name)
		if ok {
			result.Register(tool)
		}
	}
	return result
}

// TaskPlanStateTool is the narrow runtime contract for restoring and
// re-presenting model-authored plan state. It provides no method for the
// runtime to invent or modify individual plan items.
type TaskPlanStateTool interface {
	Tool
	ResetPlan()
	RestoreArguments(map[string]any) error
	HistoryToolNames() []string
	Reminder() string
	PlanState() TaskPlanState
}

// RecordedTaskPlanStateTool preserves the exact revision in a committed plan
// snapshot. Implementations without this optional contract use argument replay.
type RecordedTaskPlanStateTool interface {
	TaskPlanStateTool
	RestorePlanState(TaskPlanState) error
}

func FindTaskPlanStateTool(registry *Registry) (TaskPlanStateTool, bool) {
	if registry == nil {
		return nil, false
	}
	for _, name := range []string{TaskPlanToolName, "todo_write"} {
		tool, ok := registry.Get(name)
		if !ok {
			continue
		}
		stateTool, ok := tool.(TaskPlanStateTool)
		if ok {
			return stateTool, true
		}
	}
	return nil, false
}
