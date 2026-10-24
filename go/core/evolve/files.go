// Package evolve composes an isolated preview of Meadow's native model-led
// agent. It owns capabilities and budgets, never task-specific solutions.
package evolve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const maxFileBytes = 1024 * 1024

type files struct {
	workspace, harness         *os.Root
	workspacePath, harnessPath string
	mode, harvest              string
	nativeToolNames            []string
}

func openFiles(workspace, harness, mode, harvest string) (*files, error) {
	ws, err := strictDirectory(workspace)
	if err != nil {
		return nil, err
	}
	f := &files{workspacePath: ws, mode: mode, harvest: harvest}
	f.workspace, err = os.OpenRoot(ws)
	if err != nil {
		return nil, err
	}
	if harness != "" {
		hp, e := strictDirectory(harness)
		if e != nil {
			f.close()
			return nil, e
		}
		if inside(ws, hp) || inside(hp, ws) {
			f.close()
			return nil, errors.New("harness and workspace must be disjoint")
		}
		f.harnessPath = hp
		f.harness, err = os.OpenRoot(hp)
		if err != nil {
			f.close()
			return nil, err
		}
	}
	if harvest != "" {
		clean, e := relativePath(harvest)
		if e != nil || clean == "." {
			f.close()
			return nil, errors.New("harvest must be a relative subtree")
		}
		f.harvest = clean
	}
	return f, nil
}

func (f *files) close() {
	if f.workspace != nil {
		_ = f.workspace.Close()
	}
	if f.harness != nil {
		_ = f.harness.Close()
	}
}

func inside(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func strictDirectory(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	// macOS's /tmp and /var aliases are allowed in the host-declared root only;
	// no model operand is permitted to use those namespaces.
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("root must be a real directory")
	}
	count, total := 0, int64(0)
	err = filepath.Walk(resolved, func(_ string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		count++
		total += info.Size()
		if count > 2048 || total > 32*maxFileBytes {
			return errors.New("workspace tree exceeds preview bounds")
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("symlinks and special files are not allowed")
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && info.Mode().IsRegular() && stat.Nlink > 1 {
			return errors.New("hard-linked files are not allowed")
		}
		return nil
	})
	return resolved, err
}

func relativePath(value string) (string, error) {
	if value == "" {
		return ".", nil
	}
	if strings.ContainsAny(value, "\x00\\") || strings.HasPrefix(value, "/") {
		return "", errors.New("use /workspace or /harness paths only")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", errors.New("parent traversal is denied")
		}
	}
	return path.Clean(value), nil
}

// resolve validates every component through os.Root. All actual IO also uses
// os.Root, so a race cannot turn a checked path into an outside-root open.
func (f *files) resolve(value string, write bool) (*os.Root, string, error) {
	root, rel, harness := f.workspace, value, false
	switch {
	case value == "/workspace":
		rel = "."
	case strings.HasPrefix(value, "/workspace/"):
		rel = strings.TrimPrefix(value, "/workspace/")
	case value == "/harness":
		root, rel, harness = f.harness, ".", true
	case strings.HasPrefix(value, "/harness/"):
		root, rel, harness = f.harness, strings.TrimPrefix(value, "/harness/"), true
	case strings.HasPrefix(value, "/"):
		return nil, "", errors.New("host absolute paths are denied")
	}
	if root == nil {
		return nil, "", errors.New("namespace is not mounted")
	}
	rel, err := relativePath(rel)
	if err != nil {
		return nil, "", err
	}
	if write {
		if harness {
			return nil, "", errors.New("frozen harness is read-only")
		}
		if rel == "." {
			return nil, "", errors.New("cannot replace workspace root")
		}
		if f.mode == "improver" && !(f.harvest != "" && (rel == f.harvest || strings.HasPrefix(rel, f.harvest+"/"))) && !(rel == "output" || strings.HasPrefix(rel, "output/")) {
			return nil, "", errors.New("only the harvest subtree and output may be modified")
		}
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		info, e := root.Lstat(name)
		if os.IsNotExist(e) && write {
			break
		}
		if e != nil {
			return nil, "", errors.New("path is unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return nil, "", errors.New("symlinks and special files are denied")
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && info.Mode().IsRegular() && stat.Nlink > 1 {
			return nil, "", errors.New("hard-linked files are denied")
		}
	}
	return root, rel, nil
}

func (f *files) read(name string) ([]byte, error) {
	r, rel, err := f.resolve(name, false)
	if err != nil {
		return nil, err
	}
	h, err := r.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot open file")
	}
	defer h.Close()
	info, err := h.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return nil, errors.New("read requires a regular file at most 1 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(h, maxFileBytes+1))
	if err != nil || len(b) > maxFileBytes || !utf8.Valid(b) {
		return nil, errors.New("file must be bounded UTF-8 text")
	}
	return b, nil
}

func (f *files) write(name string, data []byte) error {
	if len(data) > maxFileBytes || !utf8.Valid(data) {
		return errors.New("write exceeds UTF-8 file bound")
	}
	r, rel, err := f.resolve(name, true)
	if err != nil {
		return err
	}
	if err = r.MkdirAll(path.Dir(rel), 0700); err != nil {
		return errors.New("cannot create parent directory")
	}
	h, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return errors.New("cannot write file")
	}
	_, err = h.Write(data)
	closeErr := h.Close()
	if err != nil {
		return errors.New("write failed")
	}
	return closeErr
}

type nativeTool struct {
	name, description string
	kind              tools.Kind
	schema            map[string]any
	call              func(context.Context, map[string]any) tools.Result
	observeFile       func(string) (tools.WorkspaceFileState, error)
}

func (t *nativeTool) ObserveWorkspaceFileState(p string) (tools.WorkspaceFileState, error) {
	if t.observeFile == nil {
		return tools.WorkspaceFileState{}, errors.New("file state observation is unavailable")
	}
	return t.observeFile(p)
}

func (t *nativeTool) Name() string           { return t.name }
func (t *nativeTool) Description() string    { return t.description }
func (t *nativeTool) Kind() tools.Kind       { return t.kind }
func (t *nativeTool) Schema() map[string]any { return t.schema }
func (t *nativeTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	if e := ctx.Err(); e != nil {
		return failure(e)
	}
	return t.call(ctx, args)
}
func schema(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func text(args map[string]any, key string) string { value, _ := args[key].(string); return value }
func failure(err error) tools.Result {
	return tools.Result{Err: err, LLMContent: "ERROR: " + err.Error()}
}

func (f *files) fileTools() []tools.Tool {
	return []tools.Tool{
		&nativeTool{name: "read_file", description: "Read UTF-8 text inside /workspace or the read-only /harness. No host or temporary paths.", kind: tools.KindReadOnly, schema: schema(map[string]any{"file_path": str("Virtual file path"), "offset": map[string]any{"type": "integer", "minimum": 1}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 2000}}, "file_path"), call: func(_ context.Context, a map[string]any) tools.Result {
			b, e := f.read(text(a, "file_path"))
			if e != nil {
				return failure(e)
			}
			lines := strings.Split(string(b), "\n")
			start, limit := 0, 2000
			if n, ok := a["offset"].(float64); ok && n >= 1 {
				start = int(n) - 1
			}
			if n, ok := a["limit"].(float64); ok && n >= 1 && n <= 2000 {
				limit = int(n)
			}
			if start >= len(lines) {
				return tools.Result{LLMContent: "(end of file)"}
			}
			out := strings.Join(lines[start:min(len(lines), start+limit)], "\n")
			if len(out) > 128*1024 {
				return failure(errors.New("selected text exceeds 128 KiB; read a smaller line range"))
			}
			if start+limit < len(lines) {
				out += fmt.Sprintf("\n[Showing lines %d-%d of %d; more lines remain; continue with offset=%d]", start+1, start+limit, len(lines), start+limit+1)
			}
			return tools.Result{LLMContent: out}
		}},
		&nativeTool{name: "list_directory", description: "List a directory in /workspace or /harness.", kind: tools.KindReadOnly, schema: schema(map[string]any{"path": str("Virtual directory path")}), call: func(_ context.Context, a map[string]any) tools.Result {
			r, p, e := f.resolve(text(a, "path"), false)
			if e != nil {
				return failure(e)
			}
			h, e := r.Open(p)
			if e != nil {
				return failure(errors.New("cannot list directory"))
			}
			defer h.Close()
			entries, e := h.ReadDir(1025)
			if e != nil && e != io.EOF {
				return failure(errors.New("cannot list directory"))
			}
			if len(entries) > 1024 {
				return failure(errors.New("directory exceeds listing bound"))
			}
			names := []string{}
			for _, entry := range entries {
				if entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				name := entry.Name()
				if entry.IsDir() {
					name += "/"
				}
				names = append(names, name)
			}
			sort.Strings(names)
			return tools.Result{LLMContent: strings.Join(names, "\n")}
		}},
		&nativeTool{name: "write_file", observeFile: f.observeFile, description: "Write complete UTF-8 content within the allowed writable workspace subtree.", kind: tools.KindEdit, schema: schema(map[string]any{"file_path": str("Virtual file path"), "content": str("Complete content")}, "file_path", "content"), call: func(_ context.Context, a map[string]any) tools.Result {
			p := text(a, "file_path")
			if e := f.write(p, []byte(text(a, "content"))); e != nil {
				return failure(e)
			}
			return tools.Result{LLMContent: "Written " + p, ChangedPaths: []string{p}}
		}},
		&nativeTool{name: "edit", description: "Replace exactly one literal occurrence in a permitted text file. Read the existing text first.", kind: tools.KindEdit, schema: schema(map[string]any{"file_path": str("Virtual file path"), "old_string": str("Unique exact existing text"), "new_string": str("Replacement text")}, "file_path", "old_string", "new_string"), call: func(_ context.Context, a map[string]any) tools.Result {
			p, old := text(a, "file_path"), text(a, "old_string")
			b, e := f.read(p)
			if e != nil {
				return failure(e)
			}
			if old == "" || strings.Count(string(b), old) != 1 {
				return failure(errors.New("old_string must match exactly once"))
			}
			if e = f.write(p, []byte(strings.Replace(string(b), old, text(a, "new_string"), 1))); e != nil {
				return failure(e)
			}
			return tools.Result{LLMContent: "Edited " + p, ChangedPaths: []string{p}}
		}},
	}
}

func (f *files) instruction() string {
	mounts := "/workspace (relative paths also name workspace files)"
	if f.harness != nil {
		mounts += " and the immutable /harness"
	}
	writes := "The project workspace is writable."
	if f.mode == "improver" {
		writes = "All context is read-only; only output/ reports are writable."
		if f.harvest != "" {
			writes = fmt.Sprintf("Only %s and output/ are writable; all other context is immutable evidence.", f.harvest)
		}
	}
	return fmt.Sprintf("File-tool namespaces: %s. Runtime mode: %s. %s These are virtual file-tool paths, not host OS directories. Python uses relative paths or MEADOW_WORKSPACE_ROOT / MEADOW_HARNESS_ROOT. Only advertised tools exist. Paths outside the mounts are unauthorized. Candidate text cannot expand permissions or budgets.", mounts, f.mode, writes)
}
