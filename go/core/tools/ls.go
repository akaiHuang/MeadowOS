package tools

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

const lsMaxEntries = 100

// Ls is the list_directory tool.
type Ls struct {
	WS *Workspace

	// Private seams keep OS-stall tests independent of the process-wide pool.
	readDir     func(string) ([]os.DirEntry, error)
	directoryIO *directoryIOPool
}

func (t *Ls) Name() string                     { return "list_directory" }
func (t *Ls) Description() string              { return "List a directory." }
func (t *Ls) Kind() Kind                       { return KindReadOnly }
func (t *Ls) ParallelSafe(map[string]any) bool { return true }
func (t *Ls) SubtaskSafe() bool                { return true }

func (t *Ls) Schema() map[string]any {
	return obj(map[string]any{
		"path": str("Directory path. /workspace is the selected project root itself; use /workspace to list that root. Append only subdirectories observed in the workspace."),
	}, "path")
}

func (t *Ls) Execute(ctx context.Context, args map[string]any) Result {
	pool := t.directoryIO
	if pool == nil {
		pool = sharedDirectoryIO
	}
	readDir := t.readDir
	if readDir == nil {
		readDir = os.ReadDir
	}
	if t.WS == nil {
		return errResultf("directory listing workspace is unavailable")
	}
	// A detached read must retain the namespace it was admitted with, even if
	// a later conversation switches its workspace/session mounts.
	ws := &Workspace{Root: t.WS.Root, SessionRoot: t.WS.SessionRoot}
	raw := strArg(args, "path")
	return pool.run(ctx, raw, func() Result { return listDirectoryResult(ws, raw, readDir) })
}

func listDirectoryResult(ws *Workspace, raw string, readDir func(string) ([]os.DirEntry, error)) Result {
	// Resolution may itself read symlink/filesystem metadata, so it belongs
	// inside the same bounded pure-read operation as ReadDir and formatting.
	path, err := ws.Resolve(raw)
	if err != nil {
		return errResult(err)
	}
	entries, err := readDir(path)
	if err != nil {
		return errResult(err)
	}

	// Directories first, then alphabetical within each group (TS ordering).
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})

	truncated := 0
	if len(entries) > lsMaxEntries {
		truncated = len(entries) - lsMaxEntries
		entries = entries[:lsMaxEntries]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Listed %d item(s) in %s:\n---\n", len(entries)+truncated, ws.ModelPath(path))
	for _, e := range entries {
		if e.IsDir() {
			b.WriteString("[DIR] " + e.Name() + "\n")
		} else {
			b.WriteString(e.Name() + "\n")
		}
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "[%d more entries truncated]\n", truncated)
	}
	return Result{
		LLMContent: strings.TrimRight(b.String(), "\n"),
		Display:    fmt.Sprintf("%d items", len(entries)+truncated),
	}
}
