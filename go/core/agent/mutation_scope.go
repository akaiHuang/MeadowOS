package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// applyExplicitMutationScope turns the user's exact exclusive file list into
// an execution boundary. It does not infer scope from ordinary task wording.
// Direct file tools are rejected before execution; shell receives a hidden
// capability that restores any out-of-scope changes before returning.
func applyExplicitMutationScope(
	registry *tools.Registry,
	call provider.ToolCall,
	workspace *tools.Workspace,
	allowed []string,
) (provider.ToolCall, error) {
	if registry == nil || len(allowed) == 0 {
		return call, nil
	}
	tool, ok := registry.Get(call.Function.Name)
	if !ok {
		return call, nil
	}
	args := map[string]any{}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return call, nil
	}
	if call.Function.Name == DefaultShellTool {
		args[tools.AllowedWorkspaceMutationPathsArgument] = append([]string(nil), allowed...)
		encoded, err := json.Marshal(args)
		if err != nil {
			return call, fmt.Errorf("MUTATION_SCOPE_BLOCKED: could not bind shell to the explicit mutation scope")
		}
		call.Function.Arguments = string(encoded)
		return call, nil
	}
	kind := tools.CallKind(tool, args)
	if kind != tools.KindEdit && kind != tools.KindDestructive {
		return call, nil
	}
	rawPath := kernelArgumentPath(args)
	if strings.TrimSpace(rawPath) == "" {
		return call, fmt.Errorf("MUTATION_SCOPE_BLOCKED: tool %s does not expose a verifiable target path", call.Function.Name)
	}
	if workspace == nil {
		return call, fmt.Errorf("MUTATION_SCOPE_BLOCKED: workspace is unavailable")
	}
	target, err := workspace.Resolve(rawPath)
	if err != nil {
		return call, fmt.Errorf("MUTATION_SCOPE_BLOCKED: invalid mutation target %q", rawPath)
	}
	for _, rawAllowed := range allowed {
		candidate, resolveErr := workspace.Resolve(rawAllowed)
		if resolveErr == nil && filepath.Clean(candidate) == filepath.Clean(target) {
			return call, nil
		}
	}
	return call, fmt.Errorf(
		"MUTATION_SCOPE_BLOCKED: %s is outside the user's explicit file mutation scope (%s)",
		workspace.ModelPath(target), strings.Join(allowed, ", "),
	)
}

// bindExplicitMutationScopeToHistory keeps an exclusive user scope narrow
// while resolving a shorthand path against the artifact the agent was just
// working on. A follow-up such as "只編輯 src/app.ts" must not be interpreted
// as the repository-root /workspace/src/app.ts when the latest tool episode
// already established exactly one /workspace/artifacts/.../src/app.ts target.
//
// This is deliberately evidence-bound rather than basename-based: only exact
// path-segment suffixes from native tool arguments in the latest assistant
// episode are eligible, and ambiguous matches are left untouched so the
// existing mutation guard fails closed.
func bindExplicitMutationScopeToHistory(
	allowed []string,
	history []provider.Message,
	workspace *tools.Workspace,
) []string {
	if len(allowed) == 0 || len(history) == 0 || workspace == nil {
		return append([]string(nil), allowed...)
	}
	start := 0
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == "user" {
			start = index + 1
			break
		}
	}
	candidates := mutationScopeHistoryPaths(history[start:], workspace)
	bound := append([]string(nil), allowed...)
	for index, rawAllowed := range bound {
		suffix, ok := relativeMutationScopeSuffix(rawAllowed)
		if !ok {
			continue
		}
		matches := make(map[string]bool)
		for _, candidate := range candidates {
			candidateSlash := filepath.ToSlash(filepath.Clean(candidate))
			if candidateSlash == suffix || strings.HasSuffix(candidateSlash, "/"+suffix) {
				matches[candidateSlash] = true
			}
		}
		if len(matches) != 1 {
			continue
		}
		for match := range matches {
			bound[index] = match
		}
	}
	return bound
}

func mutationScopeHistoryPaths(history []provider.Message, workspace *tools.Workspace) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, message := range history {
		for _, call := range message.ToolCalls {
			// Only inspection and validator calls can ground a shorthand scope.
			// A model-authored write/edit target is not evidence that the user
			// authorized that target and must never widen this safety boundary.
			switch call.Function.Name {
			case "read_file", "typescript_web_build", "meadow_browser_visual_check", "meadow_browser_interaction_check":
			default:
				continue
			}
			var args any
			if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
				continue
			}
			var rawPaths []string
			collectMutationScopeArgumentPaths(args, "", &rawPaths)
			for _, rawPath := range rawPaths {
				resolved, err := workspace.Resolve(rawPath)
				if err != nil {
					continue
				}
				modelPath := filepath.ToSlash(filepath.Clean(workspace.ModelPath(resolved)))
				if !seen[modelPath] {
					seen[modelPath] = true
					paths = append(paths, modelPath)
				}
			}
		}
	}
	return paths
}

func collectMutationScopeArgumentPaths(value any, key string, paths *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, child := range typed {
			collectMutationScopeArgumentPaths(child, strings.ToLower(strings.TrimSpace(childKey)), paths)
		}
	case []any:
		for _, child := range typed {
			collectMutationScopeArgumentPaths(child, key, paths)
		}
	case string:
		switch key {
		case "file_path", "source_path", "html_path", "path", "filename":
			if strings.TrimSpace(typed) != "" {
				*paths = append(*paths, typed)
			}
		}
	}
}

func relativeMutationScopeSuffix(raw string) (string, bool) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" || strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) {
		return "", false
	}
	cleaned := filepath.ToSlash(filepath.Clean(raw))
	cleaned = strings.TrimPrefix(cleaned, "./")
	if cleaned == "." || cleaned == "" || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}
