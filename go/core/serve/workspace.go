// Workspace + VCS endpoints (M4 step 2: comms-layer migration). These let
// the desktop's Node server delegate its hot-path workspace RPCs to the Go
// daemon — filesystem browse/search/read/write and git refs/status — while
// keeping the frontend WS protocol untouched (Node becomes a thin proxy).
//
// Shapes mirror the @meadow/contracts schemas the Node handlers return, so
// the proxy layer is a field-for-field passthrough.
package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	browseMaxEntries   = 200
	searchMaxEntries   = 200       // ProjectSearchEntriesInput limit cap
	searchIndexMax     = 50_000    // scan backstop, mirrors the Node index cap's role
	readFileDefaultMax = 128 << 10 // ProjectReadFileInput maxBytes default
	readFileMaxBytes   = 256 << 10 // ProjectReadFileInput maxBytes cap
	refsDefaultLimit   = 50
	statusMaxFiles     = 500
	vcsStatusTimeout   = 5 * time.Second
)

var (
	errNotGitRepository = errors.New("not a git repository")
	errGitNoUpstream    = errors.New("Git upstream is not configured")
)

// searchSkipDirs mirrors the Node index's IGNORED_DIRECTORY_NAMES.
var searchSkipDirs = map[string]bool{
	".git": true, ".convex": true, "node_modules": true,
	".next": true, ".turbo": true, "dist": true, "build": true,
	"out": true, ".cache": true,
}

func (s *Server) registerWorkspaceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /workspace/browse", s.handleBrowse)
	mux.HandleFunc("POST /workspace/search", s.handleSearch)
	mux.HandleFunc("POST /workspace/read", s.handleReadFile)
	mux.HandleFunc("POST /workspace/artifact/read", s.handleArtifactPreview)
	mux.HandleFunc("POST /workspace/artifact/download", s.handleArtifactDownload)
	mux.HandleFunc("POST /workspace/write", s.handleWriteFile)
	mux.HandleFunc("POST /vcs/refs", s.handleVcsRefs)
	mux.HandleFunc("POST /vcs/status", s.handleVcsStatus)
}

func decodeBody[T any](w http.ResponseWriter, r *http.Request, into *T) bool {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// handleBrowse ports filesystem.browse: directory completion for the
// folder picker. partialPath may be absolute, ~-prefixed, or relative to
// cwd; entries are directories under the resolved parent.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PartialPath string `json:"partialPath"`
		CWD         string `json:"cwd"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	p := strings.TrimSpace(body.PartialPath)
	if home, err := os.UserHomeDir(); err == nil {
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
	}
	if p == "" {
		p = body.CWD
	}
	// Explicitly-relative paths resolve against the project cwd and
	// require one, matching the Node resolveBrowseTarget semantics.
	explicitRel := p == "." || p == ".." ||
		strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../")
	if explicitRel && body.CWD == "" {
		writeErr(w, http.StatusBadRequest, "Relative filesystem browse paths require a current project.")
		return
	}
	if !filepath.IsAbs(p) {
		if body.CWD != "" {
			p = filepath.Join(body.CWD, p)
		} else {
			p, _ = filepath.Abs(p)
		}
	}

	// Complete against the containing directory unless p is itself a dir.
	parent := p
	prefix := ""
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		parent = filepath.Dir(p)
		prefix = strings.ToLower(filepath.Base(p))
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Hidden directories show in directory mode and when the typed
	// component itself starts with a dot (Node showHidden semantics).
	showHidden := prefix == "" || strings.HasPrefix(prefix, ".")
	type entry struct {
		Name     string `json:"name"`
		FullPath string `json:"fullPath"`
	}
	out := []entry{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !showHidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if prefix != "" && !strings.HasPrefix(strings.ToLower(e.Name()), prefix) {
			continue
		}
		out = append(out, entry{Name: e.Name(), FullPath: filepath.Join(parent, e.Name())})
		if len(out) >= browseMaxEntries {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, map[string]any{"parentPath": parent, "entries": out})
}

// projectEntry mirrors @meadow/contracts ProjectEntry: posix-relative path,
// kind file|directory, parentPath omitted at the root level.
type projectEntry struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	ParentPath string `json:"parentPath,omitempty"`
}

// normalizeSearchQuery mirrors the Node handler: lowercase, strip the
// leading @/./ trigger characters typed in the composer.
func normalizeSearchQuery(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	return strings.TrimLeft(q, "@./")
}

// scoreSearchEntry mirrors the main tiers of the Node scoreEntry ranking
// (lower is better): exact name 0, exact path 1, name prefix 2, path
// prefix 3, path boundary 4, name substring 5, path substring 6. The Node
// fuzzy-subsequence tier is intentionally not ported. Returns ok=false
// when the entry doesn't match.
func scoreSearchEntry(e projectEntry, query string) (int, bool) {
	if query == "" {
		if e.Kind == "directory" {
			return 0, true
		}
		return 1, true
	}
	name := strings.ToLower(path.Base(e.Path))
	full := strings.ToLower(e.Path)
	best := -1
	consider := func(score int) {
		if best == -1 || score < best {
			best = score
		}
	}
	switch {
	case name == query:
		consider(0)
	case strings.HasPrefix(name, query):
		consider(2)
	case strings.Contains(name, query):
		consider(5)
	}
	switch {
	case full == query:
		consider(1)
	case strings.HasPrefix(full, query):
		consider(3)
	case strings.Contains(full, "/"+query):
		consider(4)
	case strings.Contains(full, query):
		consider(6)
	}
	// Fuzzy subsequence tier, after all exact/substring tiers miss.
	if best == -1 {
		if isSubsequence(query, name) {
			consider(100)
		} else if isSubsequence(query, full) {
			consider(200)
		}
	}
	return best, best != -1
}

// isSubsequence reports whether the characters of query appear in order
// (not necessarily contiguously) in s. Both are already lowercased.
func isSubsequence(query, s string) bool {
	qi := 0
	for i := 0; i < len(s) && qi < len(query); i++ {
		if s[i] == query[qi] {
			qi++
		}
	}
	return qi == len(query)
}

// checkIgnoreFilter drops paths matching ignore rules even when tracked,
// mirroring the Node index's check-ignore pass over candidates.
func checkIgnoreFilter(root string, files []string) []string {
	// --no-index reports tracked files too, same flags as the Node driver.
	cmd := exec.Command("git", "check-ignore", "--no-index", "--stdin", "-z")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(files, "\x00"))
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		// Exit 1 means nothing is ignored; on real errors keep everything.
		return files
	}
	ignored := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			ignored[p] = true
		}
	}
	kept := files[:0]
	for _, f := range files {
		if !ignored[f] {
			kept = append(kept, f)
		}
	}
	return kept
}

// listWorkspaceFiles returns posix-relative file paths under root. Inside a
// git work tree it uses `git ls-files` (tracked + untracked, ignores
// applied) — the same source the Node workspace index prefers; otherwise it
// falls back to a directory walk with junk dirs pruned.
func listWorkspaceFiles(root string) []string {
	if _, err := runGit(root, "rev-parse", "--is-inside-work-tree"); err == nil {
		cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
		cmd.Dir = root
		raw, err := cmd.Output()
		if err == nil {
			files := []string{}
			for _, line := range strings.Split(string(raw), "\x00") {
				if line != "" {
					files = append(files, line)
					if len(files) >= searchIndexMax {
						break
					}
				}
			}
			return checkIgnoreFilter(root, files)
		}
	}
	files := []string{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if searchSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= searchIndexMax {
			return filepath.SkipAll
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files
}

// handleSearch ports projects.searchEntries. Response mirrors
// ProjectSearchEntriesResult: {entries: ProjectEntry[], truncated}.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD   string `json:"cwd"`
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CWD == "" {
		writeErr(w, http.StatusBadRequest, "cwd required")
		return
	}
	limit := body.Limit
	if limit <= 0 || limit > searchMaxEntries {
		limit = searchMaxEntries
	}
	query := normalizeSearchQuery(body.Query)

	files := listWorkspaceFiles(body.CWD)

	// Directories are derived from the file list (the Node index walks
	// them directly; deriving keeps git-ignore semantics for free).
	dirSet := map[string]bool{}
	for _, f := range files {
		for dir := path.Dir(f); dir != "." && dir != "/" && !dirSet[dir]; dir = path.Dir(dir) {
			dirSet[dir] = true
		}
	}
	entries := make([]projectEntry, 0, len(files)+len(dirSet))
	for _, f := range files {
		e := projectEntry{Path: f, Kind: "file"}
		if dir := path.Dir(f); dir != "." {
			e.ParentPath = dir
		}
		entries = append(entries, e)
	}
	for dir := range dirSet {
		e := projectEntry{Path: dir, Kind: "directory"}
		if parent := path.Dir(dir); parent != "." {
			e.ParentPath = parent
		}
		entries = append(entries, e)
	}

	type ranked struct {
		entry projectEntry
		score int
	}
	matches := []ranked{}
	for _, e := range entries {
		if score, ok := scoreSearchEntry(e, query); ok {
			matches = append(matches, ranked{entry: e, score: score})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score < matches[j].score
		}
		return matches[i].entry.Path < matches[j].entry.Path
	})
	truncated := len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}
	out := make([]projectEntry, len(matches))
	for i, m := range matches {
		out[i] = m.entry
	}
	writeJSON(w, map[string]any{"entries": out, "truncated": truncated})
}

func resolveUnder(root, rel string) (string, bool) {
	p := filepath.Clean(filepath.Join(root, rel))
	rootClean := filepath.Clean(root)
	return p, p == rootClean || strings.HasPrefix(p, rootClean+string(filepath.Separator))
}

// handleReadFile ports project.readFile. Response mirrors
// ProjectReadFileResult: {relativePath, contents, binary, truncated,
// sizeBytes}; binary files report binary=true with empty contents.
func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD          string `json:"cwd"`
		RelativePath string `json:"relativePath"`
		SessionID    string `json:"sessionId"`
		MaxBytes     int    `json:"maxBytes"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	chatID, err := s.artifactChatID(body.CWD, body.SessionID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	maxBytes := body.MaxBytes
	if maxBytes <= 0 || maxBytes > readFileMaxBytes {
		maxBytes = readFileDefaultMax
	}
	// Use the same rooted handle as artifact downloads. A realpath check
	// followed by os.ReadFile would still permit a symlink swap between them.
	file, info, err := openArtifactFile(artifactFileRequest{
		CWD: body.CWD, SessionID: chatID, RelativePath: body.RelativePath,
	}, math.MaxInt64)
	if err != nil {
		status := http.StatusBadRequest
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	defer file.Close()
	view, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	sizeBytes := info.Size()
	truncated := len(view) > maxBytes
	if truncated {
		view = view[:maxBytes]
	}
	rel := filepath.ToSlash(filepath.Clean(body.RelativePath))
	// Same binary test as the Node handler: NUL byte or invalid UTF-8.
	binary := bytes.IndexByte(view, 0) >= 0 || !utf8.Valid(view)
	contents := ""
	if !binary {
		contents = string(view)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"relativePath": rel,
		"contents":     contents,
		"binary":       binary,
		"truncated":    truncated,
		"sizeBytes":    sizeBytes,
	})
}

// handleWriteFile ports project.writeFile. Response mirrors
// ProjectWriteFileResult: {relativePath}.
func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD          string `json:"cwd"`
		RelativePath string `json:"relativePath"`
		Contents     string `json:"contents"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	p, ok := resolveUnder(body.CWD, body.RelativePath)
	if !ok {
		writeErr(w, http.StatusBadRequest,
			"Workspace file path must be relative to the project root: "+body.RelativePath)
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(p, []byte(body.Contents), 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rel, _ := filepath.Rel(body.CWD, p)
	writeJSON(w, map[string]any{"relativePath": filepath.ToSlash(rel)})
}

func runGit(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err
}

// Status reads share the HTTP request's bounded context. This helper is not
// used by mutation endpoints, whose execution and cancellation remain owned
// by their existing contracts. Disable index refreshes and fsmonitor hooks for
// this passive observation; cancellation terminates the invoked git process.
func runGitStatus(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	// Bound waiting on inherited pipes after cancellation or process exit. This
	// does not claim to cancel an arbitrary unrelated descendant process.
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr := string(exit.Stderr)
			if strings.Contains(stderr, "not a git repository") {
				return "", errNotGitRepository
			}
			if len(args) > 0 && args[0] == "rev-list" &&
				(strings.Contains(stderr, "no upstream configured for branch") || strings.Contains(stderr, "HEAD does not point to a branch")) {
				return "", errGitNoUpstream
			}
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// handleVcsRefs ports vcs.listRefs: branches (local + remote) with the
// current one flagged, optional substring filter.
func (s *Server) handleVcsRefs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD   string `json:"cwd"`
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	limit := body.Limit
	if limit <= 0 || limit > 200 {
		limit = refsDefaultLimit
	}
	if _, err := runGit(body.CWD, "rev-parse", "--git-dir"); err != nil {
		writeJSON(w, map[string]any{"isRepo": false, "refs": []any{}})
		return
	}
	current, _ := runGit(body.CWD, "branch", "--show-current")
	raw, err := runGit(body.CWD, "for-each-ref",
		"--format=%(refname:short)\t%(refname)", "refs/heads", "refs/remotes")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type ref struct {
		Name    string `json:"name"`
		Kind    string `json:"kind"` // local | remote
		Current bool   `json:"current"`
	}
	query := strings.ToLower(strings.TrimSpace(body.Query))
	refs := []ref{}
	for _, line := range strings.Split(raw, "\n") {
		name, full, ok := strings.Cut(line, "\t")
		if !ok || name == "" {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(name), query) {
			continue
		}
		kind := "local"
		if strings.HasPrefix(full, "refs/remotes/") {
			kind = "remote"
			if strings.HasSuffix(name, "/HEAD") {
				continue
			}
		}
		refs = append(refs, ref{Name: name, Kind: kind, Current: name == current})
		if len(refs) >= limit {
			break
		}
	}
	writeJSON(w, map[string]any{"isRepo": true, "refs": refs})
}

// handleVcsStatus ports the local half of vcs.refreshStatus: branch,
// dirty files (porcelain), ahead/behind upstream.
func (s *Server) handleVcsStatus(w http.ResponseWriter, r *http.Request) {
	s.handleVcsStatusWithGit(w, r, runGitStatus)
}

// A private runner seam lets cancellation be tested without changing a global
// command factory or starting model/provider work.
func (s *Server) handleVcsStatusWithGit(w http.ResponseWriter, r *http.Request, run func(context.Context, string, ...string) (string, error)) {
	var body struct {
		CWD string `json:"cwd"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), vcsStatusTimeout)
	defer cancel()
	read := func(args ...string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		out, err := run(ctx, body.CWD, args...)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return out, err
	}
	failed := func(err error) {
		status := http.StatusInternalServerError
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		} else if errors.Is(err, context.Canceled) {
			status = http.StatusRequestTimeout
		}
		writeErr(w, status, "Git status unavailable: "+err.Error())
	}
	if _, err := read("rev-parse", "--git-dir"); err != nil {
		if errors.Is(err, errNotGitRepository) {
			writeJSON(w, map[string]any{"isRepo": false})
		} else {
			failed(err)
		}
		return
	}
	branch, err := read("branch", "--show-current")
	if err != nil {
		failed(err)
		return
	}
	porcelain, err := read("status", "--porcelain")
	if err != nil {
		failed(err)
		return
	}
	type fileStatus struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	files := []fileStatus{}
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 4 {
			continue
		}
		files = append(files, fileStatus{Path: strings.TrimSpace(line[3:]), Status: strings.TrimSpace(line[:2])})
		if len(files) >= statusMaxFiles {
			break
		}
	}
	ahead, behind := 0, 0
	if counts, err := read("rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
		parts := strings.Fields(counts)
		if len(parts) == 2 {
			behind = atoiSafe(parts[0])
			ahead = atoiSafe(parts[1])
		}
	} else if !errors.Is(err, errGitNoUpstream) {
		failed(err)
		return
	}
	if err := ctx.Err(); err != nil {
		failed(err)
		return
	}
	writeJSON(w, map[string]any{
		"isRepo": true,
		"branch": branch,
		"dirty":  len(files) > 0,
		"files":  files,
		"ahead":  ahead,
		"behind": behind,
	})
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
