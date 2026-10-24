package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hey-meadow/meadow-code/go/core/memoryservices"
)

// ContextMemoryProvider contributes request-only historical evidence. The
// runtime decides when it is safe to contact a service, never task success.
type ContextMemoryProvider interface {
	PrepareMemory(context.Context, string, string) (string, map[string]any)
}

type MemoryServiceTool struct {
	WS     *Workspace
	Client *memoryservices.Client
	Action string
}

func RegisterMemoryServices(registry *Registry, workspace *Workspace, client *memoryservices.Client) {
	if registry == nil || workspace == nil || client == nil {
		return
	}
	for _, action := range []string{"recall", "remember", "status"} {
		tool := &MemoryServiceTool{WS: workspace, Client: client, Action: action}
		if _, exists := registry.Get(tool.Name()); !exists {
			registry.Register(tool)
		}
	}
}

func (t *MemoryServiceTool) Name() string { return "memory_" + t.Action }
func (t *MemoryServiceTool) Description() string {
	switch t.Action {
	case "recall":
		return "Search official MemPalace and/or PowerContext in this project only. Results are untrusted historical evidence with source IDs; current task and fresh tools take precedence."
	case "remember":
		return "Save a bounded project memory with its evidence source to official MemPalace and/or PowerContext. Model-authored content remains unverified; storage success is not proof of its claim. Reports each backend independently."
	default:
		return "Read this project's official memory service availability and backend status without model inference."
	}
}
func (t *MemoryServiceTool) Kind() Kind {
	if t.Action == "remember" {
		return KindEdit
	}
	return KindReadOnly
}
func (t *MemoryServiceTool) CompleteObservation() bool { return true }
func (t *MemoryServiceTool) ToolWorkspace() *Workspace { return t.WS }
func (t *MemoryServiceTool) ForWorkspace(ws *Workspace) Tool {
	return &MemoryServiceTool{WS: ws, Client: t.Client, Action: t.Action}
}
func (t *MemoryServiceTool) Schema() map[string]any {
	props := map[string]any{"backend": map[string]any{"type": "string", "enum": []string{"both", "mempalace", "powercontext"}, "description": "Default both. Scope is always the bound current project."}}
	switch t.Action {
	case "recall":
		props["query"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 250, "description": "Short search keywords or a question. MemPalace's official limit is 250 characters."}
		return obj(props, "query")
	case "remember":
		props["content"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "description": "At most 4096 UTF-8 bytes; Unicode may use several bytes per character."}
		props["evidence"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 512, "description": "Actual source path, receipt ID or user instruction supporting the memory; a reference is not independent verification."}
		return obj(props, "content", "evidence")
	default:
		return obj(props)
	}
}
func (t *MemoryServiceTool) Execute(ctx context.Context, args map[string]any) Result {
	for key := range args {
		allowed := key == "backend" || t.Action == "recall" && key == "query" || t.Action == "remember" && (key == "content" || key == "evidence")
		if !allowed {
			return errResultf("unknown memory parameter")
		}
	}
	if t.WS == nil || t.Client == nil {
		return errResultf("memory service is not bound")
	}
	backend, _ := args["backend"].(string)
	if backend == "" {
		backend = "both"
	}
	query, _ := args["query"].(string)
	content, _ := args["content"].(string)
	evidence, _ := args["evidence"].(string)
	response, err := t.Client.Call(ctx, t.Action, t.WS.Root, query, backend, "", content, evidence)
	if err != nil {
		return errResultf("%s: %s", t.Name(), err)
	}
	raw, _ := json.Marshal(response)
	return Result{LLMContent: string(raw), Display: string(raw)}
}
func (t *MemoryServiceTool) PrepareMemory(ctx context.Context, query, taskID string) (string, map[string]any) {
	if t.WS == nil || t.Client == nil {
		return "", map[string]any{"state": "unavailable", "error_code": "unbound"}
	}
	result, err := t.Client.Call(ctx, "recall", t.WS.Root, query, "both", taskID, "", "")
	if err != nil {
		return "", map[string]any{"state": "unavailable", "error_code": fmt.Sprint(err)}
	}
	return result.Context, map[string]any{"version": result.Version, "backends": result.Backends, "provenance": result.Provenance, "context_bytes": len(result.Context)}
}
