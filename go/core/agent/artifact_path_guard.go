package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

type artifactPathGuardTool struct {
	tools.Tool
	root     string
	pathArgs []string
}

func (t *artifactPathGuardTool) NormalizeArguments(args map[string]any) {
	authorizePriorReadWriteFileOverwrite(t.Tool, args)
	if normalizer, ok := t.Tool.(tools.ArgumentNormalizer); ok {
		normalizer.NormalizeArguments(args)
	}
}

func (t *artifactPathGuardTool) MutationPaths(args map[string]any) []string {
	provider, ok := t.Tool.(tools.MutationPathProvider)
	if !ok {
		return nil
	}
	return provider.MutationPaths(args)
}

type artifactPathReference struct {
	key   string
	owner map[string]any
}

func (r artifactPathReference) value() string {
	value, _ := r.owner[r.key].(string)
	return value
}

func (r artifactPathReference) set(value string) {
	r.owner[r.key] = value
}

func (t *artifactPathGuardTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	required := make(map[string]any, len(t.pathArgs))
	invalid := false
	invalidPath := ""
	for _, name := range t.pathArgs {
		refs := artifactPathReferences(args, name)
		for index, ref := range refs {
			raw := strings.TrimSpace(ref.value())
			if raw == "" {
				continue
			}
			requiredKey := name
			if len(refs) > 1 {
				requiredKey = fmt.Sprintf("%s[%d]", name, index)
			}
			// /workspace/session was the earlier model-visible spelling for the
			// same active session artifact mount. Workspace.Resolve already treats
			// it as /session; normalize that exact legacy alias here as well so a
			// correct write/build/validation call does not consume an extra model
			// repair turn. No arbitrary project path is redirected.
			if isLegacySessionArtifactPath(raw) {
				normalized := sessionArtifactSuggestion(t.root, raw)
				ref.set(normalized)
				required[requiredKey] = normalized
				continue
			}
			if raw == t.root || strings.HasPrefix(raw, strings.TrimSuffix(t.root, "/")+"/") {
				required[requiredKey] = raw
				continue
			}
			invalid = true
			if invalidPath == "" {
				invalidPath = raw
			}
			required[requiredKey] = sessionArtifactSuggestion(t.root, raw)
		}
	}
	if invalid {
		code := "artifact_path_outside_session"
		requiredAction := "retry_same_native_tool_with_session_paths"
		if t.Tool.Name() == "edit" {
			// Redirecting an edit of a project file to /session is not sufficient:
			// a fresh session has no file to edit yet. Require a real session copy
			// first, then later edits may operate on that saved artifact.
			code = "artifact_edit_requires_session_copy"
			requiredAction = "call_write_file_with_full_content_and_session_path"
		}
		recovery := &tools.RecoveryError{
			Code:           code,
			Message:        "standalone artifacts must use the active session artifact mount, not the selected project",
			Path:           invalidPath,
			RequiredAction: requiredAction,
			RequiredArgs:   required,
			PreserveDraft:  true,
		}
		payload, _ := json.Marshal(recovery)
		return tools.Result{
			LLMContent: "TOOL_ERROR " + string(payload),
			Display:    recovery.Message, Err: recovery,
		}
	}
	authorizePriorReadWriteFileOverwrite(t.Tool, args)
	return t.Tool.Execute(ctx, args)
}

// authorizePriorReadWriteFileOverwrite enables write_file's internal replace
// path only when the exact existing file has already been observed through the
// same Workspace. Unknown files retain write_file's collision-preserving
// behavior, while an explicit full-file correction after a grounded read does
// not get silently redirected to a numbered sibling.
func authorizePriorReadWriteFileOverwrite(tool tools.Tool, args map[string]any) {
	if tool == nil || args == nil {
		return
	}
	if guarded, ok := tool.(*artifactPathGuardTool); ok {
		authorizePriorReadWriteFileOverwrite(guarded.Tool, args)
		return
	}
	write, ok := tool.(*tools.WriteFile)
	if !ok || write.WS == nil || write.HasContentReference(args) {
		return
	}
	raw, _ := args["file_path"].(string)
	resolved, err := write.WS.Resolve(strings.TrimSpace(raw))
	if err != nil {
		return
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() || write.WS.CheckPriorRead(resolved) != nil {
		return
	}
	args[tools.WriteFileOwnedOverwriteKey] = true
}

// artifactPathReferences supports ordinary top-level path arguments and one
// bounded array selector such as files[].file_path. Keeping the selector in
// policy data lets a batched mutation receive the same session-root guard as a
// one-file tool without teaching the Kernel a specific file bundle schema.
func artifactPathReferences(args map[string]any, selector string) []artifactPathReference {
	const marker = "[]."
	if !strings.Contains(selector, marker) {
		return []artifactPathReference{{key: selector, owner: args}}
	}
	parts := strings.SplitN(selector, marker, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	items, _ := args[parts[0]].([]any)
	refs := make([]artifactPathReference, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		refs = append(refs, artifactPathReference{key: parts[1], owner: item})
	}
	return refs
}

func isLegacySessionArtifactPath(raw string) bool {
	slash := strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
	return slash == "/workspace/session" || strings.HasPrefix(slash, "/workspace/session/")
}

func sessionArtifactSuggestion(root, raw string) string {
	slash := strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
	if strings.HasPrefix(slash, "/workspace/session/") {
		return strings.TrimSuffix(root, "/") + "/" + strings.TrimPrefix(slash, "/workspace/session/")
	}
	base := filepath.Base(filepath.Clean(slash))
	if base == "." || base == "/" || base == "" {
		return root
	}
	return strings.TrimSuffix(root, "/") + "/" + base
}

func guardArtifactRegistry(registry *tools.Registry, root string, pathArgs map[string][]string) *tools.Registry {
	if registry == nil || strings.TrimSpace(root) == "" || len(pathArgs) == 0 {
		return registry
	}
	guarded := tools.NewRegistry()
	for _, name := range registry.Names() {
		tool, ok := registry.Get(name)
		if !ok {
			continue
		}
		if args := pathArgs[name]; len(args) > 0 {
			guarded.Register(&artifactPathGuardTool{
				Tool: tool, root: strings.TrimSuffix(root, "/"),
				pathArgs: append([]string(nil), args...),
			})
		} else {
			guarded.Register(tool)
		}
	}
	return guarded
}
