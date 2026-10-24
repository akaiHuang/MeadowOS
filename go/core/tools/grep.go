package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const grepMaxMatches = 200

var grepIdentifierRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)

// Grep is the grep_search tool. It shells out to ripgrep (--json) when
// available and falls back to a pure-Go walk otherwise, so the binary has
// no hard external dependency.
type Grep struct {
	WS *Workspace
	// rgPath caches the LookPath result; "" means not probed yet, "-" means
	// probed and absent.
	rgOnce sync.Once
	rgPath string
}

func (t *Grep) Name() string                     { return "grep_search" }
func (t *Grep) Description() string              { return "Search text in files." }
func (t *Grep) Kind() Kind                       { return KindReadOnly }
func (t *Grep) ParallelSafe(map[string]any) bool { return true }
func (t *Grep) SubtaskSafe() bool                { return true }

func (t *Grep) Schema() map[string]any {
	return obj(map[string]any{
		"pattern": str("Regex pattern to search for."),
		"path":    str("Directory or file path."),
		"glob":    str("Optional file filter, e.g. *.go."),
	}, "pattern")
}

func (t *Grep) Execute(ctx context.Context, args map[string]any) Result {
	originalPattern := strArg(args, "pattern")
	if originalPattern == "" {
		return errResultf("pattern parameter is required")
	}
	pattern := originalPattern
	fallbackPattern := grepFallbackIdentifier(originalPattern)
	fallbackUsed := false
	if _, err := regexp.Compile(pattern); err != nil {
		if fallbackPattern == "" {
			return errResultf("invalid regex %q: %v", pattern, err)
		}
		pattern = regexp.QuoteMeta(fallbackPattern)
		fallbackUsed = true
	}
	searchPath := t.WS.Root
	if p := strArg(args, "path"); p != "" {
		resolved, err := t.WS.Resolve(p)
		if err != nil {
			return errResult(err)
		}
		searchPath = resolved
	}
	fileGlob := strArg(args, "glob")

	matches, err := t.search(ctx, pattern, searchPath, fileGlob)
	if err != nil {
		return errResult(err)
	}
	if len(matches) == 0 && !fallbackUsed && shouldFallbackGrep(originalPattern) && fallbackPattern != "" && fallbackPattern != originalPattern {
		pattern = regexp.QuoteMeta(fallbackPattern)
		fallbackUsed = true
		matches, err = t.search(ctx, pattern, searchPath, fileGlob)
		if err != nil {
			return errResult(err)
		}
	}

	if len(matches) == 0 {
		if fallbackUsed {
			return Result{
				LLMContent: fmt.Sprintf("No matches for %q or fallback identifier %q in %s", originalPattern, fallbackPattern, t.WS.ModelPath(searchPath)),
				Display:    "0 matches",
			}
		}
		return Result{
			LLMContent: fmt.Sprintf("No matches for %q in %s", originalPattern, t.WS.ModelPath(searchPath)),
			Display:    "0 matches",
		}
	}
	// Keep implementation code visible before a broad search is truncated.
	// Repository walkers often emit tests and docs first; a tiny model then
	// anchors on a test fixture and never reaches the source implementation.
	sort.SliceStable(matches, func(i, j int) bool {
		return grepMatchPathPriority(matches[i]) < grepMatchPathPriority(matches[j])
	})
	truncated := 0
	if len(matches) > grepMaxMatches {
		truncated = len(matches) - grepMaxMatches
		matches = matches[:grepMaxMatches]
	}
	var b strings.Builder
	if fallbackUsed {
		fmt.Fprintf(&b, "The original search %q was invalid or too specific; retried with identifier %q.\n", originalPattern, fallbackPattern)
	}
	fmt.Fprintf(&b, "Found %d match(es) for %q in %s:\n---\n", len(matches)+truncated, pattern, t.WS.ModelPath(searchPath))
	for _, m := range matches {
		b.WriteString(strings.ReplaceAll(m, searchPath, t.WS.ModelPath(searchPath)) + "\n")
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "[%d more matches truncated]\n", truncated)
	}
	return Result{
		LLMContent: strings.TrimRight(b.String(), "\n"),
		Display:    fmt.Sprintf("%d matches", len(matches)+truncated),
	}
}

func (t *Grep) search(ctx context.Context, pattern, searchPath, fileGlob string) ([]string, error) {
	matches, err := t.runRipgrep(ctx, pattern, searchPath, fileGlob)
	if err == nil {
		return matches, nil
	}
	return t.walkGrep(ctx, pattern, searchPath, fileGlob)
}

func grepFallbackIdentifier(pattern string) string {
	var ordinary []string
	for _, token := range grepIdentifierRe.FindAllString(pattern, -1) {
		switch strings.ToLower(token) {
		case "from", "import", "return", "true", "false", "none", "null", "undefined", "error", "with", "argument", "default":
			continue
		}
		// Class/function symbols embedded in a call expression carry more
		// information than generic verbs around them (Sum over aggregate).
		if len(token) > 1 && token[0] >= 'A' && token[0] <= 'Z' {
			return token
		}
		ordinary = append(ordinary, token)
	}
	if len(ordinary) > 0 {
		return ordinary[0]
	}
	return ""
}

func grepMatchPathPriority(match string) int {
	lower := strings.ToLower(match)
	switch {
	case strings.Contains(lower, "/node_modules/"), strings.Contains(lower, "/vendor/"):
		return 3
	case strings.Contains(lower, "/tests/"), strings.Contains(lower, "/test/"),
		strings.Contains(lower, "/docs/"), strings.Contains(lower, "/examples/"),
		strings.Contains(lower, "/fixtures/"):
		return 2
	case strings.Contains(lower, "/migrations/"), strings.Contains(lower, "/generated/"):
		return 1
	default:
		return 0
	}
}

func shouldFallbackGrep(pattern string) bool {
	if len(pattern) >= 24 {
		return true
	}
	punctuation := 0
	for _, r := range pattern {
		if strings.ContainsRune("()[]{}'\"`,= ", r) {
			punctuation++
		}
	}
	return punctuation >= 3
}

func (t *Grep) runRipgrep(ctx context.Context, pattern, path, fileGlob string) ([]string, error) {
	t.rgOnce.Do(func() {
		if p, err := exec.LookPath("rg"); err == nil {
			t.rgPath = p
		} else {
			t.rgPath = "-"
		}
	})
	if t.rgPath == "-" {
		return nil, fmt.Errorf("ripgrep not installed")
	}
	rgArgs := []string{"--json", "--no-messages", "--ignore-case", "--regexp", pattern}
	if fileGlob != "" {
		rgArgs = append(rgArgs, "--glob", fileGlob)
	}
	rgArgs = append(rgArgs, path)

	cmd := exec.CommandContext(ctx, t.rgPath, rgArgs...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err != nil {
		// Exit code 1 = no matches, which is a valid empty result.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("rg: %w", err)
	}

	var matches []string
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				LineNumber int `json:"line_number"`
				Lines      struct {
					Text string `json:"text"`
				} `json:"lines"`
			} `json:"data"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil || ev.Type != "match" {
			continue
		}
		matches = append(matches, fmt.Sprintf("%s:%d:%s",
			ev.Data.Path.Text, ev.Data.LineNumber, strings.TrimRight(ev.Data.Lines.Text, "\n")))
	}
	return matches, nil
}

func (t *Grep) walkGrep(ctx context.Context, pattern, root, fileGlob string) ([]string, error) {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, err
	}
	var matches []string
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || ctx.Err() != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if fileGlob != "" {
			if ok, _ := filepath.Match(fileGlob, d.Name()); !ok {
				return nil
			}
		}
		if len(matches) > grepMaxMatches*2 {
			return filepath.SkipAll
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil || bytes.IndexByte(b, 0) >= 0 {
			return nil // unreadable or binary
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", p, i+1, line))
			}
		}
		return nil
	})
	return matches, err
}
