package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const writeFileCollisionOriginalPathKey = "__meadow_collision_original_path"
const writeFileSourceReferenceKey = "__meadow_source_reference"

// A wire caller cannot construct this provenance type. The owner binds the
// resolved operand to the same immutable run-local writer, even if another run
// advertises the same source ID. Only the stable reference is serialized.
type writeFileSourceReference struct {
	Ref   string `json:"content_ref"`
	owner *WriteFile
}

// WriteFileOwnedOverwriteKey is injected by a run-scoped agent runtime after a
// grounded read of the requested path or creation by that run. It is absent
// from the public schema; the tool rechecks prior-read provenance before replace.
const WriteFileOwnedOverwriteKey = "__meadow_owned_overwrite"

// WriteFile creates one file, preserving collisions unless the agent runtime
// authorizes a whole-file correction grounded in the workspace's prior read.
type WriteFile struct {
	WS             *Workspace
	contentSources map[string]string
}

// WithContentSources returns a run-local tool copy. Source IDs and bytes are
// host supplied, immutable after binding, and never looked up outside this
// snapshot. Sharing the existing workspace does not alter its authorization.
func (t *WriteFile) WithContentSources(sources map[string]string) *WriteFile {
	copyTool := *t
	copyTool.contentSources = make(map[string]string, len(sources))
	for ref, content := range sources {
		copyTool.contentSources[ref] = content
	}
	return &copyTool
}

func (t *WriteFile) Name() string { return "write_file" }
func (t *WriteFile) Description() string {
	description := "Create a local file or replace the full contents of an existing file after a grounded read in this workspace when the host authorizes that correction. Otherwise Meadow preserves different existing content and saves under the next available numbered name. Use edit for a targeted change to an inspected file."
	if len(t.contentSources) > 0 {
		description += " To save existing conversation text unchanged, use content_ref instead of retyping content."
	}
	return description
}
func (t *WriteFile) Kind() Kind { return KindEdit }

func (t *WriteFile) RunScopedSchema() bool { return t != nil && len(t.contentSources) > 0 }

func (t *WriteFile) Schema() map[string]any {
	schema := obj(map[string]any{
		"file_path": str("Absolute file path. An existing file is replaced only with host-authorized prior-read provenance; other name collisions are saved as a numbered sibling."),
		"content":   str("Full file content to write."),
	}, "file_path", "content")
	if len(t.contentSources) > 0 {
		schema["properties"].(map[string]any)["content"] = str("New or edited file text. For an unchanged copy of a conversation message, use its content_ref instead; do not retype it here.")
		refs := make([]string, 0, len(t.contentSources))
		for ref := range t.contentSources {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		schema["properties"].(map[string]any)["content_ref"] = map[string]any{
			"type": "string", "enum": refs,
			"description": "Explicitly copy the exact unchanged message bytes from this run-local source ID instead of rewriting them. Supply exactly one of content or content_ref; never both. Choose file_path yourself.",
		}
		schema["required"] = []string{"file_path"}
		schema["oneOf"] = []any{
			map[string]any{"required": []string{"content"}},
			map[string]any{"required": []string{"content_ref"}},
		}
	}
	return schema
}

func (t *WriteFile) ResolveArguments(args map[string]any) error {
	for key := range args {
		if strings.HasPrefix(key, "__meadow_") {
			return fmt.Errorf("write_file does not accept caller-supplied internal arguments")
		}
	}
	refValue, hasRef := args["content_ref"]
	_, hasContent := args["content"]
	if hasRef {
		if hasContent {
			return fmt.Errorf("supply exactly one of content or content_ref, never both")
		}
		if len(t.contentSources) == 0 {
			return fmt.Errorf("content_ref is unavailable without run-local content sources")
		}
		ref, ok := refValue.(string)
		if !ok {
			return fmt.Errorf("content_ref must be an exact source ID string")
		}
		content, known := t.contentSources[ref]
		if !known {
			return fmt.Errorf("unknown content_ref; choose an exact currently advertised source ID")
		}
		if containsCompactedToolResult(content) {
			return fmt.Errorf("content_ref source contains a compacted tool-result marker; exact source copying is unavailable for this reference")
		}
		args["content"] = content
		args[writeFileSourceReferenceKey] = &writeFileSourceReference{Ref: ref, owner: t}
		delete(args, "content_ref")
		return nil
	}
	return t.validateLiteralContent(args, hasContent)
}

func (t *WriteFile) validateLiteralContent(args map[string]any, hasContent bool) error {
	if !hasContent && len(t.contentSources) > 0 {
		return fmt.Errorf("supply exactly one of content or content_ref")
	}
	if len(t.contentSources) > 0 {
		// content is optional in the reference-capable schema. Reject malformed
		// scalar/null input before optional-field normalization can erase it;
		// retain the existing lossless object/array-to-JSON content adapter.
		switch args["content"].(type) {
		case string, map[string]any, []any:
		default:
			return fmt.Errorf("content must be a string or structured JSON content")
		}
	}
	return nil
}

// HasContentReference lets the runtime preserve create-only behavior for source
// copies. Presence is sufficient: malformed provenance must not gain overwrite
// authority while execution is still responsible for rejecting it.
func (t *WriteFile) HasContentReference(args map[string]any) bool {
	_, raw := args["content_ref"]
	_, resolved := args[writeFileSourceReferenceKey]
	return raw || resolved
}

func (t *WriteFile) validateExecutionContent(args map[string]any) error {
	if _, raw := args["content_ref"]; raw {
		if err := t.ResolveArguments(args); err != nil {
			return err
		}
	}
	if value, exists := args[writeFileSourceReferenceKey]; exists {
		provenance, ok := value.(*writeFileSourceReference)
		if !ok || provenance == nil || provenance.owner != t {
			return fmt.Errorf("content reference provenance does not belong to this run-local writer")
		}
		for key := range args {
			if strings.HasPrefix(key, "__meadow_") && key != writeFileSourceReferenceKey && key != writeFileCollisionOriginalPathKey {
				return fmt.Errorf("content references do not accept overwrite or extra internal arguments")
			}
		}
		source, known := t.contentSources[provenance.Ref]
		content, isText := args["content"].(string)
		if !known || !isText || content != source || containsCompactedToolResult(source) {
			return fmt.Errorf("canonical content differs from the bound conversation source")
		}
		return nil
	}
	// Literal canonical calls may carry the runtime's existing prior-read and
	// collision hints. Raw wire arguments were checked by ResolveArguments before
	// either hint was injected; do not strip or reinterpret their literal content.
	for key := range args {
		if strings.HasPrefix(key, "__meadow_") && key != WriteFileOwnedOverwriteKey && key != writeFileCollisionOriginalPathKey {
			return fmt.Errorf("write_file does not accept extra internal arguments")
		}
	}
	_, hasContent := args["content"]
	return t.validateLiteralContent(args, hasContent)
}

// NormalizeArguments preserves structured model-authored content and resolves
// a deterministic, non-destructive filename before authorization and mutation
// tracking inspect the call. The model chooses the requested path and explicit
// content or source reference; Meadow protects existing artifacts from replacement.
func (t *WriteFile) NormalizeArguments(args map[string]any) {
	if args == nil {
		return
	}
	content, exists := args["content"]
	if !exists {
		return
	}
	switch content.(type) {
	case map[string]any, []any:
		encoded, err := json.MarshalIndent(content, "", "  ")
		if err != nil {
			return
		}
		args["content"] = string(encoded) + "\n"
	}
	if p := pathArg(args); p != "" {
		args["file_path"] = p
	}
	t.normalizeCollisionPath(args)
}

func (t *WriteFile) Execute(ctx context.Context, args map[string]any) Result {
	if err := t.validateExecutionContent(args); err != nil {
		return errResult(err)
	}
	path, err := t.WS.Resolve(pathArg(args))
	if err != nil {
		return errResult(err)
	}
	if err := recoverProtocolLeakedFile(path); err != nil {
		return errResult(err)
	}
	if err := t.WS.CheckWritable(path); err != nil {
		return errResult(err)
	}
	content, hasContent := args["content"].(string)
	if !hasContent {
		return errResultf("content parameter is required")
	}
	if containsCompactedToolResult(content) {
		restored, ok := t.restoreCompactedWrite(path, content)
		if !ok {
			return errResultf("content contains a compacted tool-result marker; re-read the exact file range before writing")
		}
		content = restored
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errResult(err)
	}
	if info, statErr := os.Stat(path); statErr == nil {
		if info.IsDir() {
			return errResultf("%s is a directory", path)
		}
		prior, readErr := os.ReadFile(path)
		if readErr != nil {
			return errResult(readErr)
		}
		if string(prior) == content {
			t.WS.NoteRead(path)
			msg := fmt.Sprintf("Unchanged %s (%d bytes already match)", t.WS.ModelPath(path), len(content))
			return appendSourceDiagnostics(ctx, Result{LLMContent: confirmedWriteArtifactReceipt(msg, t.WS.ModelPath(path), path, content, "unchanged", false), Display: msg, NoChange: true}, t.WS.ModelPath(path), content)
		}
		if owned, _ := args[WriteFileOwnedOverwriteKey].(bool); owned {
			if err := t.WS.CheckPriorRead(path); err != nil {
				return errResult(err)
			}
			if err := atomicWriteFile(path, []byte(content), info.Mode().Perm()); err != nil {
				return errResult(err)
			}
			t.WS.NoteRead(path)
			msg := fmt.Sprintf("Updated %s (%d bytes)", t.WS.ModelPath(path), len(content))
			return appendSourceDiagnostics(ctx, Result{
				LLMContent: confirmedWriteArtifactReceipt(msg, t.WS.ModelPath(path), path, content, "updated", true), Display: msg,
				Diff: boundedUnifiedDiff(t.WS.ModelPath(path), string(prior), content),
			}, t.WS.ModelPath(path), content)
		}
		return errResult(&RecoveryError{
			Code:           "FILE_EXISTS",
			Message:        fmt.Sprintf("%s already exists and was preserved; retry write_file so Meadow can select another available name", t.WS.ModelPath(path)),
			Path:           t.WS.ModelPath(path),
			RequiredAction: "write_file",
			RequiredArgs: map[string]any{
				"file_path": t.WS.ModelPath(path),
				"content":   content,
			},
			PreserveDraft: true,
		})
	} else if !os.IsNotExist(statErr) {
		return errResult(statErr)
	}
	if err := t.WS.CheckPriorRead(path); err != nil {
		return errResult(err)
	}
	if err := atomicWriteFile(path, []byte(content), 0o644); err != nil {
		return errResult(err)
	}
	t.WS.NoteRead(path)

	lineCount := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		lineCount++
	}
	msg := fmt.Sprintf("Created %s (%d lines, %d bytes)", t.WS.ModelPath(path), lineCount, len(content))
	if requested := strings.TrimSpace(strArg(args, writeFileCollisionOriginalPathKey)); requested != "" {
		if requestedPath, resolveErr := t.WS.Resolve(requested); resolveErr == nil {
			requestedModelPath := t.WS.ModelPath(requestedPath)
			actualModelPath := t.WS.ModelPath(path)
			msg += fmt.Sprintf(
				"; preserved existing %s. Continue using %s as the saved artifact; do not retry %s",
				requestedModelPath, actualModelPath, requestedModelPath,
			)
		}
	}
	return appendSourceDiagnostics(ctx, Result{
		LLMContent: confirmedWriteArtifactReceipt(msg, t.WS.ModelPath(path), path, content, "created", true), Display: msg,
		Diff: boundedUnifiedDiff(t.WS.ModelPath(path), "", content),
	}, t.WS.ModelPath(path), content)
}

func confirmedWriteArtifact(summary, modelPath string) string {
	return fmt.Sprintf(
		"%s\nSAVED_ARTIFACT_CONFIRMED: %s is the authoritative saved path. Do not call ls, find, stat, or read_file solely to confirm persistence or rediscover this path; continue directly to the cheapest task-relevant validation.",
		summary, modelPath,
	)
}

// Append machine-readable persistence facts without replacing the established
// model/display text. Call only after an atomic write commits, or after the
// existing bytes were read and matched. No extra filesystem read is needed;
// these are the exact committed/matched bytes and the resolved actual path,
// including collision renames. A receipt is not semantic validation and does
// not guarantee the file remains unchanged after this observation.
func confirmedWriteArtifactReceipt(summary, modelPath, localPath, content, operation string, mutated bool) string {
	observation := "atomic_write_committed"
	if !mutated {
		observation = "existing_bytes_match"
	}
	receipt := struct {
		Version     int    `json:"version"`
		ModelPath   string `json:"model_path"`
		LocalPath   string `json:"local_path"`
		Bytes       int    `json:"bytes"`
		SHA256      string `json:"sha256"`
		Operation   string `json:"operation"`
		Observation string `json:"observation"`
		Mutated     bool   `json:"mutated"`
		Scope       string `json:"scope"`
	}{
		Version: 1, ModelPath: modelPath, LocalPath: localPath, Bytes: len(content),
		SHA256:    fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
		Operation: operation, Observation: observation, Mutated: mutated,
		Scope: "persistence observation only; not semantic validation or a guarantee of future file contents",
	}
	// The receipt contains only finite integers, booleans and strings.
	encoded, _ := json.Marshal(receipt)
	return confirmedWriteArtifact(summary, modelPath) + "\nWRITE_ARTIFACT_RECEIPT: " + string(encoded)
}

func (t *WriteFile) normalizeCollisionPath(args map[string]any) {
	if t == nil || t.WS == nil || strings.TrimSpace(strArg(args, writeFileCollisionOriginalPathKey)) != "" {
		return
	}
	if owned, _ := args[WriteFileOwnedOverwriteKey].(bool); owned {
		return
	}
	requested := strings.TrimSpace(pathArg(args))
	content, ok := args["content"].(string)
	if requested == "" || !ok {
		return
	}
	path, err := t.WS.Resolve(requested)
	if err != nil {
		return
	}
	info, statErr := os.Stat(path)
	if os.IsNotExist(statErr) || statErr != nil || info.IsDir() {
		return
	}
	prior, readErr := os.ReadFile(path)
	if readErr != nil || string(prior) == content {
		return
	}
	alternate, err := reusableOrNextAvailableWritePath(path, content)
	if err != nil {
		return
	}
	args[writeFileCollisionOriginalPathKey] = requested
	args["file_path"] = t.WS.ModelPath(alternate)
}

// reusableOrNextAvailableWritePath prevents a recovery retry from creating a
// second numbered artifact when the exact payload was already saved by an
// earlier attempt. Otherwise it returns the first unused numbered sibling.
func reusableOrNextAvailableWritePath(path, content string) (string, error) {
	directory := filepath.Dir(path)
	base := filepath.Base(path)
	extension := filepath.Ext(base)
	stem := strings.TrimSuffix(base, extension)
	if stem == "" {
		stem = base
		extension = ""
	}
	for index := 2; ; index++ {
		candidate := filepath.Join(directory, fmt.Sprintf("%s-%d%s", stem, index, extension))
		info, err := os.Stat(candidate)
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			continue
		}
		prior, err := os.ReadFile(candidate)
		if err != nil {
			return "", err
		}
		if string(prior) == content {
			return candidate, nil
		}
	}
}

func nextAvailableWritePath(path string) (string, error) {
	directory := filepath.Dir(path)
	base := filepath.Base(path)
	extension := filepath.Ext(base)
	stem := strings.TrimSuffix(base, extension)
	if stem == "" {
		stem = base
		extension = ""
	}
	for index := 2; ; index++ {
		candidate := filepath.Join(directory, fmt.Sprintf("%s-%d%s", stem, index, extension))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
}

func containsCompactedToolResult(content string) bool {
	return strings.Contains(content, "[Meadow cold-prefill compacted tool result]")
}

// restoreCompactedWrite resolves a content-addressed history marker only when
// the referenced artifact already exists at the requested path and still
// matches the recorded digest. This turns a redundant model retry into the
// normal Unchanged result without ever writing marker text to disk.
func (t *WriteFile) restoreCompactedWrite(path, marker string) (string, bool) {
	const pathPrefix = "Exact write payload is stored once in artifact "
	const digestPrefix = "SHA-256: "
	var modelPath, wantDigest string
	for _, line := range strings.Split(marker, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, pathPrefix):
			modelPath = strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, pathPrefix)), ".")
		case strings.HasPrefix(line, digestPrefix):
			wantDigest = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, digestPrefix)))
		}
	}
	if modelPath == "" || wantDigest == "" {
		return "", false
	}
	markerPath, err := t.WS.Resolve(modelPath)
	if err != nil || filepath.Clean(markerPath) != filepath.Clean(path) {
		return "", false
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	if digest != wantDigest {
		return "", false
	}
	return string(content), true
}
