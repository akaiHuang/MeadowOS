// Session history + git write-action endpoints (M5-1 / M5-4: sidebar
// thread restore and source-control buttons for the Wails desktop).
//
// Additive only — nothing here is reachable until Server.Handler (or an
// embedding mux) calls registerExtraRoutes. Git actions shell out like the
// /vcs/refs and /vcs/status handlers in workspace.go and reuse its runGit.
package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

// registerExtraRoutes wires the M5 history and git-action routes.
func (s *Server) registerExtraRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /sessions/search", s.handleSessionSearch)
	mux.HandleFunc("GET /sessions/{chatId}/history", s.handleSessionHistory)
	mux.HandleFunc("GET /sessions/{chatId}/attachments/{name}", s.handleSessionAttachment)
	mux.HandleFunc("POST /vcs/commit", s.handleGitCommit)
	mux.HandleFunc("POST /vcs/push", s.handleGitPush)
	mux.HandleFunc("POST /vcs/pull", s.handleGitPull)
	mux.HandleFunc("POST /vcs/switch", s.handleGitSwitch)
	mux.HandleFunc("POST /vcs/diff", s.handleGitDiff)
	mux.HandleFunc("POST /vcs/init", s.handleGitInit)
	mux.HandleFunc("GET /vcs/remote", s.handleGitRemoteGet)
	mux.HandleFunc("POST /vcs/set-remote", s.handleGitSetRemote)
	mux.HandleFunc("POST /vcs/scan-secrets", s.handleGitScanSecrets)
}

func (s *Server) handleSessionSearch(w http.ResponseWriter, r *http.Request) {
	cwd := strings.TrimSpace(r.URL.Query().Get("cwd"))
	if cwd == "" {
		cwd = s.WorkspaceCwd
	}
	if !filepath.IsAbs(cwd) {
		writeErr(w, http.StatusBadRequest, "cwd must be absolute")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, map[string]any{"results": []session.SessionSearchResult{}})
		return
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = parsed
	}
	results, err := session.SearchSessions(cwd, query, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if results == nil {
		results = []session.SessionSearchResult{}
	}
	writeJSON(w, map[string]any{"results": results})
}

// handleSessionHistory returns one stored chat's dialogue so the desktop
// can restore a sidebar thread. Only user/assistant turns with text come
// back; assistant tool-call-only turns and tool results fold away — the
// frontend re-renders conversation, not transcript plumbing.
func (s *Server) handleSessionHistory(w http.ResponseWriter, r *http.Request) {
	cwd := r.URL.Query().Get("cwd")
	if cwd == "" {
		cwd = s.WorkspaceCwd
	}
	chatID := r.PathValue("chatId")
	infos, err := session.ListSessions(cwd)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := ""
	for _, si := range infos {
		if si.SessionID == chatID {
			path = si.Path
			break
		}
	}
	if path == "" {
		writeErr(w, http.StatusNotFound, "unknown chat: "+chatID)
		return
	}
	// Output/progress is a durable session ledger, not model context. Use the
	// un-compacted transcript so artifacts created before semantic compaction
	// remain attached to this chat without being reintroduced to the model.
	ledgerMsgs, err := session.LoadDisplayHistory(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	display, err := session.LoadDisplayMessages(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	decorateDisplayAttachments(cwd, chatID, display)
	writeJSON(w, map[string]any{
		"messages": display,
		"files":    historyFiles(ledgerMsgs),
		"tools":    historyTools(ledgerMsgs),
	})
}

// handleSessionAttachment serves an image saved by the shared Core prompt
// path. The filename and chat id are both path-confined; cwd remains an
// explicit query parameter so the same daemon can host multiple projects.
func (s *Server) handleSessionAttachment(w http.ResponseWriter, r *http.Request) {
	cwd := r.URL.Query().Get("cwd")
	if cwd == "" {
		cwd = s.WorkspaceCwd
	}
	if !strings.HasPrefix(cwd, "/") {
		writeErr(w, http.StatusBadRequest, "cwd must be absolute")
		return
	}
	chatID, name := r.PathValue("chatId"), r.PathValue("name")
	if strings.TrimSpace(chatID) == "" || name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		writeErr(w, http.StatusBadRequest, "invalid attachment path")
		return
	}
	chatPath, err := session.ChatPath(cwd, chatID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(chatPath); err != nil {
		writeErr(w, http.StatusNotFound, "unknown chat")
		return
	}
	file, info, ok := session.OpenPersistedImageAttachment(chatPath, name)
	if !ok {
		writeErr(w, http.StatusNotFound, "attachment not found")
		return
	}
	defer file.Close()
	contentType := ""
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		contentType = "image/jpeg"
	case ".png":
		contentType = "image/png"
	case ".webp":
		contentType = "image/webp"
	case ".gif":
		contentType = "image/gif"
	default:
		writeErr(w, http.StatusNotFound, "attachment not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func decorateDisplayAttachments(cwd, chatID string, messages []session.DisplayMessage) {
	for i := range messages {
		for j, name := range messages[i].Attachments {
			messages[i].Attachments[j] = fmt.Sprintf(
				"/sessions/%s/attachments/%s?cwd=%s",
				url.PathEscape(chatID), url.PathEscape(filepath.Base(name)), url.QueryEscape(cwd),
			)
		}
	}
}

// historyTools preserves the observable work record needed by Desktop's
// Progress and Expert Lens panels when a stored conversation is reopened.
// It intentionally exposes tool facts only (name, arguments, recorded result),
// never hidden model reasoning or a synthesized success verdict.
func historyTools(msgs []provider.Message) []map[string]any {
	results := map[string]string{}
	for _, m := range msgs {
		if m.Role == "tool" && strings.TrimSpace(m.ToolCallID) != "" {
			results[m.ToolCallID] = m.Content
		}
	}
	out := []map[string]any{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			input := map[string]any{}
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			item := map[string]any{
				"toolCallId": tc.ID,
				"name":       tc.Function.Name,
				"input":      input,
				"status":     "recorded",
			}
			if result, ok := results[tc.ID]; ok {
				item["output"] = result
			}
			out = append(out, item)
		}
	}
	return out
}

// historyFiles reconstructs the durable artifact ledger from completed tool
// results, never from model-authored tool arguments. Arguments are only intent:
// write_file may resolve a collision to a numbered sibling, and a rejected or
// failed call still retains its original arguments in the transcript. The
// result is execution evidence and carries either ChangedPaths appended by the
// runtime or the file tool's authoritative saved path.
//
// Files are returned oldest touch first, deduped to the most recent successful
// touch. Calls without a linked successful mutation result fail closed and do
// not seed Desktop's output drawer on history load or resume.
func historyFiles(msgs []provider.Message) []string {
	calls := map[string]string{}
	files := []string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if id := strings.TrimSpace(tc.ID); id != "" {
				// Resolve in transcript order. If a malformed model reuses a call
				// id later, it cannot retroactively relabel an earlier result.
				calls[id] = strings.TrimSpace(tc.Function.Name)
			}
		}
		if m.Role != "tool" || strings.TrimSpace(m.ToolCallID) == "" {
			continue
		}
		name, knownCall := calls[m.ToolCallID]
		if !knownCall {
			continue
		}
		for _, p := range successfulMutationPaths(name, m.Content) {
			for i, f := range files {
				if f == p { // re-touched → move to the recent end
					files = append(files[:i], files[i+1:]...)
					break
				}
			}
			files = append(files, p)
		}
	}
	return files
}

var (
	historySavedArtifactPattern = regexp.MustCompile(`(?m)^SAVED_ARTIFACT_CONFIRMED:\s+(.+?)\s+is the authoritative saved path\.`)
	historyLegacyWritePattern   = regexp.MustCompile(`(?m)^(?:Created|Updated|Overwrote)\s+(.+?)\s+\((?:\d+ lines,\s+)?\d+ bytes(?: already match)?\)`)
	historyEditPattern          = regexp.MustCompile(`(?m)^Replaced\s+\d+\s+occurrence\(s\)\s+in\s+(.+?)\s*$`)
	historyWriteFilesPattern    = regexp.MustCompile(`(?m)^Created or updated\s+\d+\s+files:\s+(.+?)\s*$`)
	historyNonZeroExitPattern   = regexp.MustCompile(`(?m)^\(exit code\s+(-?[1-9]\d*)\)\s*$`)
)

// successfulMutationPaths extracts only harness-observed mutation paths from
// a linked tool result. It deliberately has no argument fallback: omitting an
// unverifiable legacy output is safer than reopening an unrelated or stale
// artifact from the model's requested path.
func successfulMutationPaths(toolName, output string) []string {
	toolName = strings.TrimSpace(toolName)
	trimmed := strings.TrimSpace(output)
	if trimmed == "" || strings.HasPrefix(trimmed, "ERROR:") ||
		strings.HasPrefix(trimmed, "TOOL_ERROR ") ||
		strings.HasPrefix(trimmed, "Unchanged ") ||
		strings.HasPrefix(trimmed, "SKIPPED_") ||
		strings.Contains(trimmed, "INVALID_MUTATION_ROLLED_BACK:") ||
		strings.Contains(trimmed, "AUTOMATIC_MUTATION_ROLLBACK_BLOCKED:") {
		return nil
	}
	// A shell result with a non-zero exit is failed unless that exact exit was
	// explicitly accepted by expected_exit_codes.
	if historyNonZeroExitPattern.MatchString(trimmed) &&
		!strings.Contains(trimmed, "exit code matched expected_exit_codes") {
		return nil
	}

	paths := []string{}
	add := func(raw string) {
		path := strings.TrimSpace(raw)
		if path == "" {
			return
		}
		for _, existing := range paths {
			if existing == path {
				return
			}
		}
		paths = append(paths, path)
	}
	addCSV := func(raw string) {
		for _, path := range strings.Split(raw, ",") {
			add(path)
		}
	}

	// Kernel runtime appends this line from ToolExecution.ChangedPaths. Prefer
	// it over every human-readable summary because it also covers shell/build
	// tools that mutate files indirectly.
	if historyToolMayMutate(toolName) {
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if rest, ok := strings.CutPrefix(line, "Workspace changes detected:"); ok {
				addCSV(rest)
			}
		}
	}
	if len(paths) > 0 {
		return paths
	}

	switch toolName {
	case "write_file", "save_conversation_text", "edit", "artifact_copy":
		if match := historySavedArtifactPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			add(match[1])
		} else if match := historyLegacyWritePattern.FindStringSubmatch(trimmed); len(match) == 2 {
			// Legacy chats predate SAVED_ARTIFACT_CONFIRMED but their result
			// summary still records the post-collision path actually written.
			// edit also delegates an empty-old-string create to WriteFile.
			add(match[1])
		} else if toolName == "edit" {
			if match := historyEditPattern.FindStringSubmatch(trimmed); len(match) == 2 {
				add(match[1])
			}
		}
	case "write_files":
		if match := historyWriteFilesPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			addCSV(match[1])
		}
	}
	return paths
}

func historyToolMayMutate(name string) bool {
	switch strings.TrimSpace(name) {
	case "write_file", "save_conversation_text", "edit", "write_files", "artifact_copy", "run_shell_command",
		"typescript_web_build", "research_artifact", "research_publish",
		"presentation_artifact", "download_image":
		return true
	default:
		return false
	}
}

// gitFailMsg picks the most useful failure text from a runGit call:
// stderr (cmd.Output stashes it on the ExitError), then captured stdout
// (git commit reports "nothing to commit" there), then the raw error.
func gitFailMsg(out string, err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return msg
		}
	}
	if msg := strings.TrimSpace(out); msg != "" {
		return msg
	}
	return err.Error()
}

// handleGitCommit commits staged work: optional `git add -A` first, then
// `git commit -m`, answering {ok, sha} with the new HEAD.
func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD     string `json:"cwd"`
		Message string `json:"message"`
		AddAll  bool   `json:"addAll"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CWD == "" || strings.TrimSpace(body.Message) == "" {
		writeErr(w, http.StatusBadRequest, "cwd and message required")
		return
	}
	if body.AddAll {
		if out, err := runGit(body.CWD, "add", "-A"); err != nil {
			writeErr(w, http.StatusBadRequest, gitFailMsg(out, err))
			return
		}
	}
	if out, err := runGit(body.CWD, "commit", "-m", body.Message); err != nil {
		writeErr(w, http.StatusBadRequest, gitFailMsg(out, err))
		return
	}
	sha, err := runGit(body.CWD, "rev-parse", "HEAD")
	if err != nil {
		writeErr(w, http.StatusBadRequest, gitFailMsg(sha, err))
		return
	}
	writeJSON(w, map[string]any{"ok": true, "sha": sha})
}

// handleGitPush pushes the current branch. A branch with no upstream gets
// one — `git push -u origin <branch>` — matching the first-push flow.
func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD string `json:"cwd"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CWD == "" {
		writeErr(w, http.StatusBadRequest, "cwd required")
		return
	}
	args := []string{"push"}
	if _, err := runGit(body.CWD, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err != nil {
		branch, berr := runGit(body.CWD, "branch", "--show-current")
		if berr != nil || branch == "" {
			writeErr(w, http.StatusBadRequest, "no upstream and no current branch to push")
			return
		}
		args = []string{"push", "-u", "origin", branch}
	}
	if out, err := runGit(body.CWD, args...); err != nil {
		writeErr(w, http.StatusBadRequest, gitFailMsg(out, err))
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleGitPull runs `git pull` in the workspace.
func (s *Server) handleGitPull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD string `json:"cwd"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CWD == "" {
		writeErr(w, http.StatusBadRequest, "cwd required")
		return
	}
	if out, err := runGit(body.CWD, "pull"); err != nil {
		writeErr(w, http.StatusBadRequest, gitFailMsg(out, err))
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleGitDiff answers one file's unified diff (M6 contract: POST
// /vcs/diff {cwd, path} → {diff, binary}). Tracked files diff against
// HEAD (staged + unstaged in one view); untracked files diff against
// /dev/null via --no-index so new work shows as all-plus lines. Binary
// files answer {binary:true, diff:""} — the "Binary files … differ"
// stanza is detection, not a payload worth shipping.
func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD  string `json:"cwd"`
		Path string `json:"path"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CWD == "" || strings.TrimSpace(body.Path) == "" {
		writeErr(w, http.StatusBadRequest, "cwd and path required")
		return
	}
	abs, ok := resolveUnder(body.CWD, body.Path)
	if !ok {
		writeErr(w, http.StatusBadRequest, "path must stay inside cwd: "+body.Path)
		return
	}
	rel, err := filepath.Rel(body.CWD, abs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	var out string
	if _, trackedErr := runGit(body.CWD, "ls-files", "--error-unmatch", "--", rel); trackedErr == nil {
		out, err = runGit(body.CWD, "diff", "HEAD", "--", rel)
		if err != nil {
			// Repo without a HEAD yet (no commits): fall back to /dev/null
			// so a freshly-staged file still gets a diff.
			out, err = noIndexDiff(body.CWD, rel)
		}
	} else {
		// Untracked (or not a repo at all — --no-index works either way).
		out, err = noIndexDiff(body.CWD, rel)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, gitFailMsg(out, err))
		return
	}
	if isBinaryDiff(out) {
		writeJSON(w, map[string]any{"diff": "", "binary": true})
		return
	}
	writeJSON(w, map[string]any{"diff": out, "binary": false})
}

// noIndexDiff diffs a file against /dev/null. git diff --no-index exits 1
// when the contents differ — there that code carries the diff, not an
// error — so only other exit codes propagate.
func noIndexDiff(cwd, rel string) (string, error) {
	out, err := runGit(cwd, "diff", "--no-index", "--", os.DevNull, rel)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return out, nil
		}
		return out, err
	}
	return out, nil
}

// isBinaryDiff reports whether the diff is git's binary-file stanza.
func isBinaryDiff(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ") {
			return true
		}
	}
	return false
}

// handleGitSwitch checks out refName (`git switch`, `-c` when create) and
// answers {ok, branch} with the branch git actually landed on.
func (s *Server) handleGitSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD     string `json:"cwd"`
		RefName string `json:"refName"`
		Create  bool   `json:"create"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CWD == "" || strings.TrimSpace(body.RefName) == "" {
		writeErr(w, http.StatusBadRequest, "cwd and refName required")
		return
	}
	// Branch names can't start with '-' (git refname rules); reject early
	// so a crafted refName can't smuggle extra git-switch flags.
	if strings.HasPrefix(body.RefName, "-") {
		writeErr(w, http.StatusBadRequest, "invalid refName")
		return
	}
	args := []string{"switch"}
	if body.Create {
		args = append(args, "-c")
	}
	args = append(args, body.RefName)
	if out, err := runGit(body.CWD, args...); err != nil {
		writeErr(w, http.StatusBadRequest, gitFailMsg(out, err))
		return
	}
	branch, _ := runGit(body.CWD, "branch", "--show-current")
	writeJSON(w, map[string]any{"ok": true, "branch": branch})
}

// handleGitInit runs `git init` in cwd. An existing work tree is left
// untouched and reported as {ok, already:true} so the caller can fall
// straight through to the normal repo flow; a fresh init answers {ok}.
func (s *Server) handleGitInit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD string `json:"cwd"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CWD == "" {
		writeErr(w, http.StatusBadRequest, "cwd required")
		return
	}
	if _, err := runGit(body.CWD, "rev-parse", "--is-inside-work-tree"); err == nil {
		writeJSON(w, map[string]any{"ok": true, "already": true})
		return
	}
	if out, err := runGit(body.CWD, "init"); err != nil {
		writeErr(w, http.StatusBadRequest, gitFailMsg(out, err))
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleGitRemoteGet reports whether origin has a URL (GET /vcs/remote?cwd=…).
// No origin → {hasRemote:false, url:""}; the missing-remote case is normal
// state for a fresh repo, not an error.
func (s *Server) handleGitRemoteGet(w http.ResponseWriter, r *http.Request) {
	cwd := r.URL.Query().Get("cwd")
	if cwd == "" {
		writeErr(w, http.StatusBadRequest, "cwd required")
		return
	}
	url, err := runGit(cwd, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(url) == "" {
		writeJSON(w, map[string]any{"hasRemote": false, "url": ""})
		return
	}
	writeJSON(w, map[string]any{"hasRemote": true, "url": strings.TrimSpace(url)})
}

// handleGitSetRemote points origin at url: `git remote add origin` when
// origin is new, `git remote set-url origin` when it already exists.
func (s *Server) handleGitSetRemote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD string `json:"cwd"`
		URL string `json:"url"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CWD == "" || strings.TrimSpace(body.URL) == "" {
		writeErr(w, http.StatusBadRequest, "cwd and url required")
		return
	}
	url := strings.TrimSpace(body.URL)
	// A URL can't begin with '-' (that would smuggle a flag into git remote);
	// reject early, mirroring the refName guard in handleGitSwitch.
	if strings.HasPrefix(url, "-") {
		writeErr(w, http.StatusBadRequest, "invalid remote url")
		return
	}
	sub := "add"
	if _, err := runGit(body.CWD, "remote", "get-url", "origin"); err == nil {
		sub = "set-url"
	}
	if out, err := runGit(body.CWD, "remote", sub, "origin", url); err != nil {
		writeErr(w, http.StatusBadRequest, gitFailMsg(out, err))
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// secretRule is one named credential pattern run over added diff lines.
type secretRule struct {
	kind string
	re   *regexp.Regexp
}

// secretRules are the credential shapes scanned before a push. Patterns are
// deliberately conservative — they target the literal token formats that leak
// most often, plus an assignment heuristic for password/secret/api_key values.
var secretRules = []secretRule{
	{"AWS Access Key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"OpenAI API Key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)},
	{"GitHub Token", regexp.MustCompile(`gh[posru]_[A-Za-z0-9]{20,}`)},
	{"Private Key", regexp.MustCompile(`-----BEGIN(?:[A-Z ]+)? PRIVATE KEY-----`)},
	{"Hardcoded Secret", regexp.MustCompile(`(?i)(?:password|secret|api[_-]?key|access[_-]?token)\s*[:=]\s*["']?[A-Za-z0-9_\-/+.=]{8,}`)},
}

// maskSecret keeps a finding readable in the UI without echoing the full
// credential: head and tail survive, the middle becomes a fixed mask. Short
// values are masked whole.
func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 12 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "…****…" + s[len(s)-4:]
}

// diffHunkLine tracks the new-side line number while walking a unified diff.
// `@@ -a,b +c,d @@` resets the counter to c; added lines advance it, removed
// lines do not — findings report the line in the file being pushed.
var hunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// scanDiffForSecrets walks a `git diff` body and returns one finding per
// added line that trips a rule. Only added lines (`+`, not `+++`) are
// inspected — pre-existing or removed content is not a new leak. file/line
// locate the hit; preview is the masked match.
func scanDiffForSecrets(diff string) []map[string]any {
	findings := []map[string]any{}
	file := ""
	newLine := 0
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			file = strings.TrimPrefix(line, "+++ b/")
			continue
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(line, "+++ ")
			continue
		case strings.HasPrefix(line, "diff --git "):
			continue
		case strings.HasPrefix(line, "---"):
			continue
		}
		if m := hunkHeaderRe.FindStringSubmatch(line); m != nil {
			newLine = atoiSafe(m[1])
			continue
		}
		if !strings.HasPrefix(line, "+") {
			// Context or removed line: advance the new-side counter only for
			// context (leading space); removed lines ('-') don't exist on the
			// new side. Skip headers entirely (handled above).
			if strings.HasPrefix(line, " ") {
				newLine++
			}
			continue
		}
		content := line[1:]
		for _, rule := range secretRules {
			if hit := rule.re.FindString(content); hit != "" {
				findings = append(findings, map[string]any{
					"file":    file,
					"line":    newLine,
					"kind":    rule.kind,
					"preview": maskSecret(hit),
				})
				break // one finding per line keeps the warning card readable
			}
		}
		newLine++
	}
	return findings
}

// untrackedFiles lists paths git sees as new and not ignored — the files a
// `git diff HEAD` cannot show but a push would still publish once committed.
func untrackedFiles(cwd string) []string {
	out, err := runGit(cwd, "ls-files", "--others", "--exclude-standard")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	files := []string{}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// handleGitScanSecrets runs the credential scan over everything a push would
// publish: tracked changes against HEAD (`git diff HEAD`, i.e. staged +
// unstaged) plus every untracked-but-not-ignored file (each diffed against
// /dev/null so a brand-new file with a secret is still caught — git diff
// HEAD alone never shows untracked files). On a repo with no commits yet HEAD
// is missing, so tracked content falls back to staged + unstaged diffs. Empty
// findings = clean.
func (s *Server) handleGitScanSecrets(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD string `json:"cwd"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CWD == "" {
		writeErr(w, http.StatusBadRequest, "cwd required")
		return
	}
	if _, err := runGit(body.CWD, "rev-parse", "--is-inside-work-tree"); err != nil {
		writeErr(w, http.StatusBadRequest, "not a git repository")
		return
	}
	var parts []string
	if _, err := runGit(body.CWD, "rev-parse", "HEAD"); err == nil {
		if d, derr := runGit(body.CWD, "diff", "HEAD"); derr == nil {
			parts = append(parts, d)
		}
	} else {
		// No HEAD yet: combine staged (--cached) with unstaged so a first
		// commit's content is still scanned before its first push.
		staged, _ := runGit(body.CWD, "diff", "--cached")
		unstaged, _ := runGit(body.CWD, "diff")
		parts = append(parts, staged, unstaged)
	}
	// Untracked files: diff each against /dev/null so its added lines (the
	// whole file) flow through the same scanner.
	for _, rel := range untrackedFiles(body.CWD) {
		if d, derr := noIndexDiff(body.CWD, rel); derr == nil {
			parts = append(parts, d)
		}
	}
	writeJSON(w, map[string]any{"findings": scanDiffForSecrets(strings.Join(parts, "\n"))})
}
