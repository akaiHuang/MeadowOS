package tools

import (
	"context"
	"fmt"
	"strings"
)

// SaveConversationText copies one explicitly selected, complete message. Bind
// Writer with WithContentSources for this run; no text is retrieved or inferred.
type SaveConversationText struct {
	Writer *WriteFile
}

func (t *SaveConversationText) Name() string { return "save_conversation_text" }
func (t *SaveConversationText) Kind() Kind   { return KindEdit }
func (t *SaveConversationText) RunScopedSchema() bool {
	return t != nil && t.Writer != nil && t.Writer.RunScopedSchema()
}
func (t *SaveConversationText) Description() string {
	return "Save the full existing conversation message unchanged to a local file. Choose its content_ref and a file_path; Meadow copies the exact message bytes without retyping, summarizing or editing. This tool does not accept new text. Existing different files are preserved under numbered sibling names."
}

func (t *SaveConversationText) Schema() map[string]any {
	ref := map[string]any{"type": "string", "enum": []string{}}
	if t != nil && t.Writer != nil {
		if properties, ok := t.Writer.Schema()["properties"].(map[string]any); ok {
			if bound, ok := properties["content_ref"].(map[string]any); ok {
				ref = bound
			}
		}
	}
	ref["description"] = "Exact source ID of the full conversation message to copy unchanged. No content or replacement text is accepted."
	schema := obj(map[string]any{
		"file_path":   str("Absolute destination file path. Existing different files are preserved and a numbered sibling is selected before authorization."),
		"content_ref": ref,
	}, "file_path", "content_ref")
	schema["additionalProperties"] = false
	return schema
}

// ResolveArguments accepts only public wire arguments. Keep the reference in
// canonical arguments so execution can independently check the immutable source,
// while authorization and mutation tracking see the actual output bytes.
func (t *SaveConversationText) ResolveArguments(args map[string]any) error {
	for key := range args {
		if key != "file_path" && key != "content_ref" {
			return fmt.Errorf("save_conversation_text accepts only file_path and content_ref; no content or internal arguments")
		}
	}
	resolved, err := t.resolveSource(args)
	if err != nil {
		return err
	}
	args["content"] = resolved["content"]
	return nil
}

func (t *SaveConversationText) resolveSource(args map[string]any) (map[string]any, error) {
	if t == nil || t.Writer == nil || t.Writer.WS == nil {
		return nil, fmt.Errorf("save_conversation_text requires a bound workspace and run-local sources")
	}
	path, ok := args["file_path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("file_path must be a nonempty string")
	}
	ref, exists := args["content_ref"]
	if !exists {
		return nil, fmt.Errorf("content_ref is required; choose an exact currently advertised source ID")
	}
	resolved := map[string]any{"file_path": path, "content_ref": ref}
	if err := t.Writer.ResolveArguments(resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

func (t *SaveConversationText) NormalizeArguments(args map[string]any) {
	resolved, err := t.resolveSource(args)
	if err != nil || args["content"] != resolved["content"] {
		return
	}
	// Only these source-owned fields reach the writer normalizer. In particular,
	// prior reads or caller-supplied flags cannot grant overwrite authority.
	t.Writer.NormalizeArguments(resolved)
	args["file_path"] = resolved["file_path"]
	if original, exists := resolved[writeFileCollisionOriginalPathKey]; exists {
		args[writeFileCollisionOriginalPathKey] = original
	}
}

func (t *SaveConversationText) Execute(ctx context.Context, args map[string]any) Result {
	for key := range args {
		switch key {
		case "file_path", "content_ref", "content", writeFileCollisionOriginalPathKey:
		default:
			return errResultf("save_conversation_text does not accept extra or overwrite arguments")
		}
	}
	resolved, err := t.resolveSource(args)
	if err != nil {
		return errResult(err)
	}
	if content, exists := args["content"]; exists {
		text, ok := content.(string)
		if !ok || text != resolved["content"] {
			return errResultf("canonical content differs from the bound conversation source")
		}
	}
	// This field is only a display hint; it cannot select content or authorize
	// replacement. The writer independently checks the real destination again.
	if original, ok := args[writeFileCollisionOriginalPathKey].(string); ok {
		resolved[writeFileCollisionOriginalPathKey] = original
	}
	// resolved is a fresh source-derived map, with neither content_ref nor any
	// overwrite flag. Reuse the existing atomic write, conflict checks and receipt.
	return t.Writer.Execute(ctx, resolved)
}
