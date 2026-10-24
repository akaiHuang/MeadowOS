package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

const globMaxResults = 100

// skipDirs are pruned from glob and the pure-Go grep fallback — the TS side
// gets this via .gitignore; M1 hard-codes the universal noise instead.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "__pycache__": true,
	".venv": true, "venv": true, "dist": true, "build": true,
	".next": true, ".cache": true, "target": true,
}

// Glob is the glob tool: find files by pattern, newest first.
type Glob struct {
	WS *Workspace
}

func (t *Glob) Name() string                     { return "glob" }
func (t *Glob) Description() string              { return "Find files by glob pattern." }
func (t *Glob) Kind() Kind                       { return KindReadOnly }
func (t *Glob) ParallelSafe(map[string]any) bool { return true }
func (t *Glob) SubtaskSafe() bool                { return true }

func (t *Glob) Schema() map[string]any {
	return obj(map[string]any{
		"pattern": str("Glob pattern, e.g. **/*.go."),
		"path":    str("Directory to search."),
	}, "pattern")
}

func (t *Glob) Execute(ctx context.Context, args map[string]any) Result {
	pattern := strings.TrimSpace(strArg(args, "pattern"))
	if pattern == "" {
		return errResultf("pattern parameter is required")
	}
	root := t.WS.Root
	if p := strArg(args, "path"); p != "" {
		resolved, err := t.WS.Resolve(p)
		if err != nil {
			return errResult(err)
		}
		root = resolved
	}

	type hit struct {
		path  string
		mtime time.Time
	}
	collect := func(globPattern string) ([]hit, error) {
		var found []hit
		err := doublestar.GlobWalk(os.DirFS(root), globPattern, func(rel string, d fs.DirEntry) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return fs.SkipDir
				}
				return nil
			}
			for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
				if skipDirs[part] {
					return nil
				}
			}
			info, ierr := d.Info()
			var mt time.Time
			if ierr == nil {
				mt = info.ModTime()
			}
			found = append(found, hit{path: filepath.Join(root, rel), mtime: mt})
			return nil
		})
		return found, err
	}

	patterns := []string{pattern}
	if alternatives := strings.Split(pattern, " or "); len(alternatives) > 1 {
		patterns = patterns[:0]
		for _, alternative := range alternatives {
			if trimmed := strings.TrimSpace(alternative); trimmed != "" {
				patterns = append(patterns, trimmed)
			}
		}
	}
	var hits []hit
	seen := map[string]bool{}
	appendUnique := func(found []hit) {
		for _, item := range found {
			if !seen[item.path] {
				seen[item.path] = true
				hits = append(hits, item)
			}
		}
	}
	for _, candidatePattern := range patterns {
		normalizedPattern, err := t.relativeGlobPattern(root, candidatePattern)
		if err != nil {
			return errResult(err)
		}
		found, err := collect(normalizedPattern)
		if err != nil && ctx.Err() == nil {
			return errResultf("glob %q: %v", candidatePattern, err)
		}
		appendUnique(found)
	}
	// A small model commonly emits "*" or "*.py" while trying to discover
	// project files. If the workspace root has only directories, a strict
	// root-only glob returns no evidence and the model starts guessing paths.
	// Retry the same pattern recursively; keep the original pattern in the
	// report so this remains a transparent zero-result fallback.
	if len(hits) == 0 && ctx.Err() == nil {
		for _, candidatePattern := range patterns {
			normalizedPattern, err := t.relativeGlobPattern(root, candidatePattern)
			if err != nil {
				return errResult(err)
			}
			if strings.Contains(normalizedPattern, "**") {
				continue
			}
			found, err := collect(filepath.ToSlash(filepath.Join("**", normalizedPattern)))
			if err != nil && ctx.Err() == nil {
				return errResultf("recursive glob fallback for %q: %v", candidatePattern, err)
			}
			appendUnique(found)
		}
	}

	// TS ordering: files touched in the last 24h sort newest-first, the
	// rest alphabetically after them.
	cutoff := time.Now().Add(-24 * time.Hour)
	sort.SliceStable(hits, func(i, j int) bool {
		ri, rj := hits[i].mtime.After(cutoff), hits[j].mtime.After(cutoff)
		if ri != rj {
			return ri
		}
		if ri {
			return hits[i].mtime.After(hits[j].mtime)
		}
		return hits[i].path < hits[j].path
	})

	if len(hits) == 0 {
		return Result{
			LLMContent: fmt.Sprintf("No files matched %q under %s", pattern, t.WS.ModelPath(root)),
			Display:    "0 matches",
		}
	}
	truncated := 0
	if len(hits) > globMaxResults {
		truncated = len(hits) - globMaxResults
		hits = hits[:globMaxResults]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d file(s) matching %q under %s, newest first:\n---\n", len(hits)+truncated, pattern, t.WS.ModelPath(root))
	for _, h := range hits {
		b.WriteString(t.WS.ModelPath(h.path) + "\n")
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "[%d more files truncated]\n", truncated)
	}
	return Result{
		LLMContent: strings.TrimRight(b.String(), "\n"),
		Display:    fmt.Sprintf("%d matches", len(hits)+truncated),
	}
}

func (t *Glob) relativeGlobPattern(root, pattern string) (string, error) {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	rootModelPath := strings.TrimRight(filepath.ToSlash(t.WS.ModelPath(root)), "/")
	workspaceModelPath := strings.TrimRight(filepath.ToSlash(t.WS.ModelPath(t.WS.Root)), "/")

	var absolutePattern string
	switch {
	case pattern == rootModelPath:
		return ".", nil
	case strings.HasPrefix(pattern, rootModelPath+"/"):
		return strings.TrimPrefix(pattern, rootModelPath+"/"), nil
	case pattern == workspaceModelPath:
		absolutePattern = t.WS.Root
	case strings.HasPrefix(pattern, workspaceModelPath+"/"):
		absolutePattern = filepath.Join(t.WS.Root, filepath.FromSlash(strings.TrimPrefix(pattern, workspaceModelPath+"/")))
	case filepath.IsAbs(filepath.FromSlash(pattern)):
		absolutePattern = filepath.FromSlash(pattern)
	default:
		return pattern, nil
	}

	relative, err := filepath.Rel(root, absolutePattern)
	if err != nil {
		return "", fmt.Errorf("resolve glob pattern %q: %w", pattern, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("glob pattern %q escapes search root %s", pattern, t.WS.ModelPath(root))
	}
	return filepath.ToSlash(relative), nil
}
