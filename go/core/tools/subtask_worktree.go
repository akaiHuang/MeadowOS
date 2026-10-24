package tools

import (
	"fmt"
	"path/filepath"
)

// InheritedWorktreeSubtaskRegistry re-roots recognized tools without widening
// the parent's configured capability surface. Unknown wrappers are omitted,
// never replaced by a more powerful default tool with the same name.
func InheritedWorktreeSubtaskRegistry(base *Registry, ws *Workspace) (*Registry, error) {
	result := base.EmptyView()
	if base == nil || ws == nil {
		return result, nil
	}
	if raw, ok := base.Get("run_shell_command"); ok {
		if shell, ok := raw.(*Shell); ok && shell.WS != nil {
			ws.SetDesktopSession(shell.WS.DesktopSession())
		}
	}
	var manager *ProcessManager
	if raw, ok := base.Get("run_shell_command"); ok {
		if shell, ok := raw.(*Shell); ok && shell.Manager != nil {
			if rawProcess, exists := base.Get("process"); exists {
				if process, ok := rawProcess.(*ProcessTool); ok && process.Manager == shell.Manager {
					manager = NewProcessManager(ws)
				}
			}
		}
	}
	fail := func(err error) (*Registry, error) {
		if manager != nil {
			_ = manager.Close()
		}
		return nil, err
	}
	for _, name := range base.Names() {
		tool, _ := base.Get(name)
		var child Tool
		var parentWS *Workspace
		switch t := tool.(type) {
		case *ReadFile:
			copy := *t
			parentWS, copy.WS = t.WS, ws
			child = &copy
		case *InspectImage:
			parentWS = t.WS
			child = &InspectImage{WS: ws}
		case *ListAttachmentImages, *InspectAttachmentImages:
			// Collection access belongs to the original chat, not the worker's
			// repository root. Retain the exact source-scoped read-only instance.
			child = tool
		case *WriteFile:
			// Parent conversation content references must not enter child context.
			parentWS = t.WS
			child = &WriteFile{WS: ws}
		case *Edit:
			parentWS = t.WS
			child = &Edit{WS: ws}
		case *Glob:
			parentWS = t.WS
			child = &Glob{WS: ws}
		case *Grep:
			parentWS = t.WS
			child = &Grep{WS: ws}
		case *Ls:
			parentWS = t.WS
			child = &Ls{WS: ws}
		case *Shell:
			parentWS = t.WS
			child = &Shell{WS: ws, Manager: manager}
		case *ProcessTool:
			if manager != nil {
				child = &ProcessTool{Manager: manager}
			}
		case *WebSearch, *WebFetch, *WebCrawl:
			// These tools have no workspace-owned mutable state. Preserve exact
			// configured clients, auth boundaries and public-network policies.
			child = tool
		case *BrowsePage:
			copy := *t
			parentWS, copy.WS = t.WS, ws
			child = &copy
		case *MeadowBrowserDOM:
			copy := *t
			parentWS, copy.WS = t.WS, ws
			child = &copy
			if t.BaseURL == desktopBrowserPrivateBase {
				child = &parentOnlyDesktopBrowser{original: &copy}
			}
		case *MeadowBrowserVisualCheck:
			copy := *t
			parentWS, copy.WS = t.WS, ws
			child = &copy
			if t.BaseURL == desktopBrowserPrivateBase {
				child = &parentOnlyDesktopBrowser{original: &copy}
			}
		case *MeadowBrowserWebMCP:
			copy := *t
			parentWS, copy.WS = t.WS, ws
			child = &copy
			if t.BaseURL == desktopBrowserPrivateBase {
				child = &parentOnlyDesktopBrowser{original: &copy}
			}
		case *PDFExtract:
			copy := *t
			parentWS, copy.WS = t.WS, ws
			child = &copy
		case *DownloadImage:
			copy := *t
			parentWS, copy.WS = t.WS, ws
			child = &copy
		case *ResearchArtifact:
			copy := *t
			parentWS, copy.WS = t.WS, ws
			child = &copy
		case WorkspaceToolProvider:
			parentWS = t.ToolWorkspace()
			child = t.ForWorkspace(ws)
		case *TypeScriptWebBuild:
			copy := *t
			parentWS, copy.WS = t.WS, ws
			child = &copy
		}
		if parentWS != nil {
			if err := inheritSubtaskProtectedFiles(parentWS, ws); err != nil {
				return fail(err)
			}
		}
		if child != nil {
			result.Register(child)
		}
	}
	// Plans are private model-authored state, never a shared mutable tool.
	if _, ok := FindTaskPlanStateTool(base); ok {
		result.Register(NewTaskPlan())
	}
	return result, nil
}

func inheritSubtaskProtectedFiles(parent, child *Workspace) error {
	parent.mu.Lock()
	paths := make([]string, 0, len(parent.protectedFiles))
	for protected := range parent.protectedFiles {
		rel, err := filepath.Rel(parent.Root, protected)
		if err != nil {
			parent.mu.Unlock()
			return err
		}
		paths = append(paths, filepath.Join(child.Root, rel))
	}
	parent.mu.Unlock()
	if err := child.ProtectFiles(paths); err != nil {
		return fmt.Errorf("inherit protected child files: %w", err)
	}
	return nil
}
