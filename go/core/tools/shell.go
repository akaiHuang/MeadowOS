package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/creack/pty"
)

const (
	shellDefaultTimeout = 2 * time.Minute
	shellMaxTimeout     = 10 * time.Minute
	shellMaxOutputBytes = 64 * 1024
	shellHeadBytes      = 48 * 1024
	shellTailBytes      = 12 * 1024

	// ReadOnlyWorkspaceArgument is an internal runtime capability. It is not
	// part of the model-visible schema: the requirement-audit ledger adds it
	// only after accepting a fresh executable check. The shell then runs that
	// check against an isolated workspace clone so a checker cannot mutate the
	// user's post-validation artifact.
	ReadOnlyWorkspaceArgument = "_meadow_read_only_workspace"
	// AllowedWorkspaceMutationPathsArgument is an internal runtime capability
	// derived only from an explicit user-authored "only modify ..." contract.
	// Shell restores every out-of-scope file change before returning.
	AllowedWorkspaceMutationPathsArgument = "_meadow_allowed_workspace_mutation_paths"
)

// Shell is the run_shell_command tool. Foreground commands run through zsh;
// background commands are owned by Manager so their pipes and descendants
// cannot strand the agent turn.
type Shell struct {
	WS      *Workspace
	Manager *ProcessManager
}

func (t *Shell) Name() string { return "run_shell_command" }
func (t *Shell) Description() string {
	return "Run a command or script for computation, diagnostics, validation, or intentional workspace output. Unless directory or temporary is selected, commands start in the selected project root; omit a redundant cd. Use directory to choose another allowed working directory. Quote paths containing spaces in command text; pass directory as a plain JSON path value, not wrapped in additional shell quotes. A script may write a requested derived artifact; printing content alone does not save it. Foreground results report process exit code, timeout, stdout, and stderr. Exit code 0 reports process status, not task correctness; inspect the actual evidence."
}
func (t *Shell) Kind() Kind { return KindExecute }
func (t *Shell) NormalizeArguments(args map[string]any) {
	NormalizeShellArguments(args)
}

// NormalizeShellArguments applies schema-safe protocol repairs before either
// policy inspection or execution. Keeping this public lets the agent verifier
// judge the same canonical call that Shell will execute.
func NormalizeShellArguments(args map[string]any) {
	normalizeExpectedExitCodes(args)
	normalizeQuotedPythonHeredocScript(args)
	normalizeLongPythonInlineScript(args)
}
func (t *Shell) CompanionTools() []string {
	return []string{"process"}
}
func (t *Shell) SubtaskTool() Tool { return &subtaskReadOnlyShell{shell: t} }
func (t *Shell) ParallelSafe(args map[string]any) bool {
	return t.ClassifyCall(args) == KindReadOnly
}

func (t *Shell) Schema() map[string]any {
	command := str(
		"Command to execute. Prefer paths relative to directory. Use the directory field instead of adding a redundant cd. Quote paths containing spaces in command text. For a multi-line Python, Node, bash, or zsh program, set command to exactly its interpreter and put source in script instead of constructing a huge -c argument.",
	)
	command["minLength"] = 1
	return obj(map[string]any{
		"command": command,
		"script": str(
			"Optional multi-line source executed from an automatically removed temporary script file. The source may intentionally write requested artifacts in directory; only the temporary script itself is removed. Requires command to be exactly python3, node, bash, or zsh; foreground only.",
		),
		"directory": str("Optional working directory; defaults to /workspace, the selected project root. Pass a plain JSON path value; do not wrap it in additional shell quotes. Omit when temporary=true."),
		"timeout":   num("Timeout in milliseconds."),
		"background": boolean(
			"Start a tracked background process and return process_id immediately. Do not add &, nohup, disown, or setsid to the command; use process to poll/log/write/stop it.",
		),
		"pty": boolean(
			"Allocate a pseudo-terminal. Use for interactive programs that require a TTY.",
		),
		"notify_on_complete": boolean(
			"When background=true, deliver one observable completion update on a later agent turn.",
		),
		"temporary": boolean(
			"Run inside a private session-owned scratch directory that is automatically removed. Do not also set directory.",
		),
		"expected_exit_codes": map[string]any{
			"type": "array", "items": map[string]any{"type": "integer"},
			"description": "Expected process exit codes (default [0]); a matching code does not establish task correctness. Use a non-zero code for negative assertions, such as proving a stopped endpoint cannot be reached.",
		},
		"purpose": map[string]any{
			"type": "string", "enum": []string{"diagnostic", "validation"},
			"description": "Optional model-declared role. Declare validation for a relevant foreground test, build, lint, or explicit acceptance assertion whose true child exit status supports completion. Keep the original check identifiable across repair; do not hide its failure behind a pipeline or unconditional exit zero. Ordinary diagnostics and artifact generation do not by themselves validate requirements.",
		},
		"validation_paths": map[string]any{
			"type": "array", "items": map[string]any{"type": "string", "minLength": 1},
			"minItems": 1, "maxItems": shellMaxValidationPaths, "uniqueItems": true,
			"description": "Optional existing files this check concerns, such as the tested source, tests or saved outputs; requires purpose=validation and foreground execution. Up to 8 existing regular files, 8 MiB each. Relative paths start at workspace root, independent of directory; /workspace and /session are supported. Records unchanged before/after SHA-256 bytes, not correctness or proof the command read them. After relevant edits, select the affected original check again rather than treating old observations as current.",
		},
	}, "command")
}

func normalizeLongPythonInlineScript(args map[string]any) {
	if strings.TrimSpace(strArg(args, "script")) != "" ||
		boolArg(args, "background") {
		return
	}
	command := strings.TrimSpace(strArg(args, "command"))
	if separator := strings.Index(command, "&&"); separator > 0 {
		changeDir := strings.TrimSpace(command[:separator])
		inline := strings.TrimSpace(command[separator+2:])
		directory := filepath.Clean(strings.TrimSpace(strArg(args, "directory")))
		if strings.HasPrefix(changeDir, "cd ") &&
			strings.HasPrefix(inline, `python3 -c "`) {
			target := strings.Trim(strings.TrimSpace(strings.TrimPrefix(changeDir, "cd ")), `"'`)
			cleanTarget := filepath.Clean(target)
			// /workspace is the shell tool's stable alias for its default root,
			// so an explicit directory argument is unnecessary. Qwen commonly
			// prefixes large audit programs with this exact cd and omits directory.
			// Other directories are stripped only when the call already declared
			// the same working directory, preserving shell cwd semantics.
			if cleanTarget == "/workspace" || cleanTarget == "//workspace" ||
				(directory != "." && cleanTarget == directory) {
				command = inline
				args["command"] = inline
			}
		}
	}
	const prefix = `python3 -c "`
	if !strings.HasPrefix(command, prefix) || !strings.HasSuffix(command, `"`) {
		return
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(command, prefix), `"`)
	if len(raw) < 512 && strings.Count(raw, "\n") < 8 {
		return
	}
	// These tokens deliberately depend on zsh -c evaluation or Python -c's
	// missing __file__. Keep their original semantics.
	if strings.ContainsAny(raw, "$`") || strings.Contains(raw, "__file__") {
		return
	}
	script, ok := unquoteShellDoubleQuotedProgram(raw)
	if !ok {
		return
	}
	args["command"] = "python3"
	args["script"] = script
}

// NormalizeAuditInlinePythonScript moves one exact double-quoted `python3 -c`
// program onto the temporary-script carrier regardless of its size. It is
// deliberately narrower than a shell parser: pipelines, redirects, command
// substitution, environment prefixes, single-quoted programs and additional
// commands are left untouched so an audit policy can reject them instead of
// silently changing their meaning.
func NormalizeAuditInlinePythonScript(args map[string]any) bool {
	if strings.TrimSpace(strArg(args, "script")) != "" ||
		boolArg(args, "background") {
		return false
	}
	command := strings.TrimSpace(strArg(args, "command"))
	const prefix = `python3 -c "`
	if !strings.HasPrefix(command, prefix) || !strings.HasSuffix(command, `"`) {
		return false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(command, prefix), `"`)
	if strings.ContainsAny(raw, "$`") || strings.Contains(raw, "__file__") {
		return false
	}
	script, ok := unquoteShellDoubleQuotedProgram(raw)
	if !ok {
		return false
	}
	args["command"] = "python3"
	args["script"] = script
	return true
}

// NormalizeAuditScriptArgumentPythonCarrier repairs a narrow Qwen native-tool
// mistake at the audit boundary: after the schema says `script` contains
// Python source, the model sometimes puts an entire workspace-prefixed
// `python3 -c "..."` carrier inside that field. The carrier has no semantic
// value because the audit tool already owns cwd and interpreter selection.
// Accept only one exact double-quoted Python program with an optional stderr
// merge; reject shell variables, command substitution, __file__, suffixes and
// other directories instead of trying to parse general shell.
func NormalizeAuditScriptArgumentPythonCarrier(args map[string]any) bool {
	carrier := strings.TrimSpace(strArg(args, "script"))
	if carrier == "" || boolArg(args, "background") {
		return false
	}
	for _, prefix := range []string{
		`cd /workspace && python3 -c "`,
		`cd //workspace && python3 -c "`,
		`python3 -c "`,
	} {
		if !strings.HasPrefix(carrier, prefix) {
			continue
		}
		raw := strings.TrimPrefix(carrier, prefix)
		if strings.HasSuffix(raw, `" 2>&1`) {
			raw = strings.TrimSuffix(raw, `" 2>&1`)
		} else if strings.HasSuffix(raw, `"`) {
			raw = strings.TrimSuffix(raw, `"`)
		} else {
			return false
		}
		if strings.ContainsAny(raw, "$`") || strings.Contains(raw, "__file__") {
			return false
		}
		script, ok := unquoteShellDoubleQuotedProgram(raw)
		if !ok || strings.TrimSpace(script) == "" {
			return false
		}
		args["command"] = "python3"
		args["script"] = script
		return true
	}
	return false
}

var quotedPythonHeredocHeader = regexp.MustCompile(
	`^python3(?:\s+-)?\s+<<(-?)\s*(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)")[ \t]*\r?\n`,
)

var auditNamedPythonHeredocHeader = regexp.MustCompile(
	`^python3[ \t]+script[ \t]+<<(-?)[ \t]*(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)")[ \t]*\r?\n`,
)

// normalizeQuotedPythonHeredocScript preserves the literal semantics of a
// quoted Python heredoc while moving it onto the existing temporary-script
// carrier. Unquoted heredocs are intentionally left alone because shell
// expansion of $, backticks, and backslashes would otherwise change meaning.
func normalizeQuotedPythonHeredocScript(args map[string]any) {
	normalizeQuotedPythonHeredocScriptWithHeader(args, quotedPythonHeredocHeader)
}

// NormalizeAuditNamedPythonHeredocScript repairs one narrow Qwen protocol
// error seen after the audit prompt says "put source in script": the model
// emits the literal schema field name as `python3 script <<'EOF'` inside the
// command instead of assigning the body to the script argument. This is only
// called by the active requirement-audit boundary. It deliberately accepts no
// real filename, unquoted delimiter, shell prefix/suffix, or second heredoc.
func NormalizeAuditNamedPythonHeredocScript(args map[string]any) bool {
	return normalizeQuotedPythonHeredocScriptWithHeader(args, auditNamedPythonHeredocHeader)
}

func normalizeQuotedPythonHeredocScriptWithHeader(
	args map[string]any,
	header *regexp.Regexp,
) bool {
	if strings.TrimSpace(strArg(args, "script")) != "" || boolArg(args, "background") {
		return false
	}
	command := strings.TrimSpace(strArg(args, "command"))
	match := header.FindStringSubmatchIndex(command)
	if match == nil || match[0] != 0 {
		return false
	}
	delimiter := ""
	for _, pair := range [][2]int{{4, 5}, {6, 7}} {
		if match[pair[0]] >= 0 {
			delimiter = command[match[pair[0]]:match[pair[1]]]
			break
		}
	}
	if delimiter == "" {
		return false
	}
	stripTabs := match[2] >= 0 && match[3] > match[2]
	bodyAndEnd := command[match[1]:]
	lines := strings.Split(bodyAndEnd, "\n")
	terminator := -1
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSuffix(lines[index], "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if stripTabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == delimiter {
			terminator = index
		}
		break
	}
	if terminator < 0 {
		return false
	}
	body := lines[:terminator]
	if stripTabs {
		for index := range body {
			body[index] = strings.TrimLeft(body[index], "\t")
		}
	}
	args["command"] = "python3"
	args["script"] = strings.Join(body, "\n") + "\n"
	return true
}

func normalizeExpectedExitCodes(args map[string]any) {
	if raw, ok := args["expected_exit_codes"].(string); ok {
		var values []int
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &values) == nil && len(values) > 0 {
			args["expected_exit_codes"] = values
			return
		}
	}
	value, ok := intArg(args, "expected_exit_codes")
	if !ok {
		return
	}
	args["expected_exit_codes"] = []int{value}
}

func unquoteShellDoubleQuotedProgram(raw string) (string, bool) {
	var result strings.Builder
	result.Grow(len(raw))
	for index := 0; index < len(raw); index++ {
		if raw[index] != '\\' {
			result.WriteByte(raw[index])
			continue
		}
		if index+1 >= len(raw) {
			return "", false
		}
		next := raw[index+1]
		switch next {
		case '"', '\\':
			result.WriteByte(next)
			index++
		case '\n':
			index++
		default:
			// In shell double quotes a backslash before other characters is
			// literal, so preserve it for Python.
			result.WriteByte('\\')
		}
	}
	return result.String(), true
}

// subtaskReadOnlyShell preserves useful repository inspection for delegated
// workers without exposing an execute-capable shell. The parent Shell's
// conservative classifier remains the single source of truth.
type subtaskReadOnlyShell struct {
	shell *Shell
}

func (t *subtaskReadOnlyShell) Name() string { return "run_shell_command" }
func (t *subtaskReadOnlyShell) Description() string {
	return "Run a read-only repository or system inspection command. Mutating, background, interactive, and unclassified commands are rejected."
}
func (t *subtaskReadOnlyShell) Kind() Kind { return KindReadOnly }
func (t *subtaskReadOnlyShell) ClassifyCall(map[string]any) Kind {
	return KindReadOnly
}
func (t *subtaskReadOnlyShell) ParallelSafe(map[string]any) bool { return true }
func (t *subtaskReadOnlyShell) Schema() map[string]any {
	command := str("Read-only command such as git status, git diff, rg, find, ls, cat, or pwd.")
	command["minLength"] = 1
	return obj(map[string]any{
		"command":   command,
		"directory": str("Working directory."),
		"timeout":   num("Timeout in milliseconds."),
	}, "command")
}
func (t *subtaskReadOnlyShell) Execute(ctx context.Context, args map[string]any) Result {
	if t == nil || t.shell == nil {
		return errResultf("read-only shell is not configured")
	}
	if t.shell.ClassifyCall(args) != KindReadOnly {
		return errResultf("delegated workers may run only commands classified as read-only")
	}
	if boolArg(args, "background") || boolArg(args, "pty") {
		return errResultf("delegated workers cannot start background or interactive shell commands")
	}
	return t.shell.Execute(ctx, args)
}

// readOnlyHeads are command heads that never mutate state on their own.
var readOnlyHeads = map[string]bool{
	"cat": true, "head": true, "tail": true, "wc": true, "ls": true,
	"pwd": true, "echo": true, "printf": true, "which": true, "file": true,
	"stat": true, "date": true, "whoami": true, "uname": true, "id": true,
	"grep": true, "rg": true, "tree": true, "du": true, "df": true,
	"basename": true, "dirname": true, "realpath": true, "env": true,
	"diff": true, "cmp": true, "sort": true, "uniq": true, "cut": true,
	"jq": true, "column": true, "nl": true, "find": true,
	"od": true, "strings": true, "md5": true, "shasum": true,
	"true": true, "test": true, "type": true, "uptime": true, "hostname": true,
}

// Git models commonly add safe global flags or `-C /workspace` even when the
// shell already runs at the workspace root. Accept those spellings while
// keeping the actual subcommand allowlist unchanged.
var readOnlyGitRe = regexp.MustCompile(
	`^git\s+` +
		`(?:(?:--no-pager|--no-optional-locks)\s+)*` +
		`(?:-C\s+(?:/workspace|\.)(?:/[\w./-]+)?\s+)?` +
		`(status|log|diff|show|shortlog|describe|rev-parse|ls-files|blame|branch\s*$|branch\s+(-a|-r|--list|--show-current)|remote\s+-v|remote\s*$|tag\s*$|tag\s+(-l|--list)|stash\s+list|config\s+(--get|--list|-l))`,
)

// writeMarkers anywhere in the command disqualify it from auto-approval
// regardless of the head — redirection, substitution, and privilege moves.
var writeMarkersRe = regexp.MustCompile("[>`]|\\$\\(|<\\(|\\bsudo\\b|\\brm\\b|\\bmv\\b|\\bkill\\b|\\bchmod\\b|\\bchown\\b|\\btee\\b|\\bdd\\b|\\bmkfifo\\b|\\bln\\b")

// destructiveMarkers are irreversible deletions/wipes — they escalate to
// KindDestructive (always-confirm, even yolo) so a weak model can never
// delete or wipe the working tree unattended. Beyond file deletion this
// covers the git commands that throw away uncommitted work and truncate.
var destructiveMarkersRe = regexp.MustCompile(`\b(rm|rmdir|shred|mkfs)\b|\bdd\b` +
	`|\bgit\s+reset\s+--hard\b|\bgit\s+clean\s+-[a-z]*f|\bgit\s+checkout\s+(--\s+|\.|-f\b)` +
	`|\bgit\s+restore\s+(--\s+|\.)|\btruncate\b`)

// Interpreter and copy-program delete APIs are just as destructive as a
// literal `rm`. These spellings are intentionally conservative: script mode
// is executable input and yolo must not turn an obvious delete primitive into
// an unattended mutation merely because it is wrapped by Python or Node.
var destructiveRuntimeMarkersRe = regexp.MustCompile(`(?i)` +
	`\bos\.(remove|unlink|rmdir|removedirs)\s*\(` +
	`|\bshutil\.rmtree\s*\(` +
	`|\.unlink\s*\(` +
	`|\b(unlinkSync|rmSync|rmdirSync)\s*\(` +
	`|\bremove-item\b` +
	`|\brsync\b[^\n\r]*(--delete(?:-[a-z-]+)?\b)`)

var shellBackgroundWrapperRe = regexp.MustCompile(`(?i)(^|[\s;|&])(nohup|disown|setsid)([\s;|&]|$)`)

var untrackedProcessSignalRe = regexp.MustCompile(`(?i)(^|[\s;|&()])(?:/usr/bin/|/bin/)?(kill|pkill|killall)([\s;|&()]|$)`)

// Commands that expose the host process environment or read absolute paths
// outside the mounted workspace are observations, but not safe auto-approved
// observations. They can contain API keys, credentials, or other host data.
// Keep ordinary relative repository inspection (cat go.mod, head README) in
// the read-only fast path while routing these calls through execution approval.
func shellReadsHostState(words []topLevelShellWord) bool {
	if len(words) == 0 {
		return false
	}
	if words[0].value == "env" || words[0].value == "printenv" {
		return true
	}
	for _, word := range words {
		if word.expanded {
			return true
		}
	}
	for _, word := range words[1:] {
		value := word.value
		if _, optionValue, ok := strings.Cut(value, "="); ok {
			value = optionValue
		}
		if value == "~" || strings.HasPrefix(value, "~/") {
			return true
		}
		for _, component := range strings.Split(filepath.ToSlash(value), "/") {
			if component == ".." {
				return true
			}
		}
		if strings.HasPrefix(value, "/") &&
			value != "/workspace" && !strings.HasPrefix(value, "/workspace/") &&
			value != "/session" && !strings.HasPrefix(value, "/session/") {
			return true
		}
	}
	return false
}

func (t *Shell) shellClassificationRoots(args map[string]any) (base, namespace string, ok bool) {
	if t == nil || t.WS == nil || strings.TrimSpace(t.WS.Root) == "" {
		return "", "", false
	}
	base = t.WS.Root
	if directory := strings.TrimSpace(strArg(args, "directory")); directory != "" {
		resolved, err := t.WS.Resolve(directory)
		if err != nil {
			return "", "", false
		}
		base = resolved
	}
	switch {
	case containsRoot(t.WS.Root, base):
		return base, t.WS.Root, true
	case t.WS.SessionRoot != "" && containsRoot(t.WS.SessionRoot, base):
		return base, t.WS.SessionRoot, true
	default:
		return "", "", false
	}
}

func (t *Shell) shellReadsThroughEscapingSymlink(words []topLevelShellWord, base, namespace string) bool {
	if t == nil || t.WS == nil || base == "" || namespace == "" {
		return false
	}
	for _, word := range words[1:] {
		token := word.value
		if _, optionValue, ok := strings.Cut(token, "="); ok {
			token = optionValue
		}
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		var candidate string
		switch {
		case token == "/workspace":
			candidate = t.WS.Root
		case strings.HasPrefix(token, "/workspace/"):
			candidate = filepath.Join(t.WS.Root, filepath.FromSlash(strings.TrimPrefix(token, "/workspace/")))
		case token == "/session" && t.WS.SessionRoot != "":
			candidate = t.WS.SessionRoot
		case strings.HasPrefix(token, "/session/") && t.WS.SessionRoot != "":
			candidate = filepath.Join(t.WS.SessionRoot, filepath.FromSlash(strings.TrimPrefix(token, "/session/")))
		case !filepath.IsAbs(token):
			candidate = filepath.Join(base, filepath.FromSlash(token))
		default:
			continue
		}

		candidates := []string{candidate}
		if strings.ContainsAny(candidate, "*?[") {
			matches, err := filepath.Glob(candidate)
			if err != nil {
				return true
			}
			candidates = matches
		}
		for _, path := range candidates {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				continue
			}
			if !containsRoot(namespace, resolved) {
				return true
			}
		}
	}
	return false
}

func shellWritesNamedOutput(head string, words []topLevelShellWord) bool {
	if len(words) < 2 {
		return false
	}
	for _, word := range words[1:] {
		token := word.value
		switch head {
		case "sort", "tree", "md5":
			if token == "-o" || token == "--output" || strings.HasPrefix(token, "--output=") ||
				(strings.HasPrefix(token, "-o") && !strings.HasPrefix(token, "--") && len(token) > 2) {
				return true
			}
		case "rg":
			// ripgrep's preprocessor is an arbitrary executable, not a read.
			if token == "--pre" || strings.HasPrefix(token, "--pre=") || token == "--pre-glob" || strings.HasPrefix(token, "--pre-glob=") {
				return true
			}
		case "git":
			if token == "--output" || strings.HasPrefix(token, "--output=") ||
				token == "--ext-diff" || token == "--textconv" {
				return true
			}
		}
		if head == "sort" && (token == "--compress-program" || strings.HasPrefix(token, "--compress-program=")) {
			return true
		}
	}
	return false
}

func findPredicateConsumesValue(token string) bool {
	switch token {
	case "-name", "-iname", "-path", "-ipath", "-regex", "-iregex", "-lname", "-ilname",
		"-type", "-xtype", "-user", "-group", "-uid", "-gid", "-size", "-inum", "-links",
		"-perm", "-mtime", "-mmin", "-atime", "-amin", "-ctime", "-cmin", "-newer", "-fstype",
		"-context", "-printf":
		return true
	default:
		return false
	}
}

func classifyFindWords(words []topLevelShellWord) Kind {
	skipValue := false
	for _, word := range words[1:] {
		token := word.value
		if skipValue {
			skipValue = false
			continue
		}
		if findPredicateConsumesValue(token) {
			skipValue = true
			continue
		}
		switch token {
		case "-delete":
			return KindDestructive
		case "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls":
			return KindExecute
		}
	}
	return KindReadOnly
}

func shellSourceIsDestructive(source string) bool {
	return shellSourceIsDestructiveAtDepth(source, 0)
}

func shellSourceIsDestructiveAtDepth(source string, depth int) bool {
	if depth >= 8 {
		return true
	}
	if destructiveMarkersRe.MatchString(source) || destructiveRuntimeMarkersRe.MatchString(source) {
		return true
	}
	for _, segment := range splitTopLevelShellSegments(source) {
		words := normalizeShellControlWords(parseTopLevelShellWords(segment))
		if len(words) == 0 {
			continue
		}
		if nested, expanded, ok := nestedShellCommandSource(words); ok {
			if expanded || shellSourceIsDestructiveAtDepth(nested, depth+1) {
				return true
			}
		}
		head := strings.ToLower(filepath.Base(words[0].value))
		switch head {
		case "rm", "rmdir", "shred", "mkfs", "dd", "truncate", "unlink":
			return true
		case "find":
			if classifyFindWords(words) == KindDestructive {
				return true
			}
		case "rsync":
			for _, word := range words[1:] {
				if word.value == "--delete" || strings.HasPrefix(word.value, "--delete-") ||
					word.value == "--remove-source-files" {
					return true
				}
			}
		case "git":
			for index, word := range words[1:] {
				token := word.value
				if (token == "reset" && shellWordsContain(words[index+2:], "--hard")) ||
					(token == "clean" && shellWordsContainShortFlag(words[index+2:], 'f')) ||
					(token == "checkout" && (shellWordsContain(words[index+2:], "--", ".") || shellWordsContainShortFlag(words[index+2:], 'f'))) ||
					token == "restore" ||
					(token == "branch" && shellWordsContain(words[index+2:], "-D")) ||
					(token == "tag" && shellWordsContain(words[index+2:], "-d")) ||
					(token == "stash" && shellWordsContain(words[index+2:], "drop", "clear")) ||
					(token == "worktree" && shellWordsContain(words[index+2:], "remove")) ||
					(token == "push" && (shellWordsContain(words[index+2:], "--delete", "--force") || shellWordsContainShortFlag(words[index+2:], 'f'))) {
					return true
				}
			}
		}
	}
	return false
}

func normalizeShellControlWords(words []topLevelShellWord) []topLevelShellWord {
	if len(words) == 0 {
		return nil
	}
	normalized := append([]topLevelShellWord(nil), words...)
	normalized[0].value = strings.TrimLeft(normalized[0].value, "({!")
	normalized[len(normalized)-1].value = strings.TrimRight(normalized[len(normalized)-1].value, ")}")
	return normalized
}

func shellWordsContain(words []topLevelShellWord, values ...string) bool {
	for _, word := range words {
		for _, value := range values {
			if word.value == value {
				return true
			}
		}
	}
	return false
}

func shellWordsContainShortFlag(words []topLevelShellWord, flag byte) bool {
	for _, word := range words {
		token := word.value
		if strings.HasPrefix(token, "--") {
			continue
		}
		if strings.HasPrefix(token, "-") && strings.ContainsRune(token[1:], rune(flag)) {
			return true
		}
	}
	return false
}

func shellMayFollowRecursiveSymlinks(head string, words []topLevelShellWord) bool {
	for _, word := range words[1:] {
		token := word.value
		switch head {
		case "find":
			if token == "-L" || token == "-H" {
				return true
			}
		case "rg":
			if token == "-L" || token == "--follow" {
				return true
			}
		case "grep":
			if token == "-R" || token == "--dereference-recursive" ||
				(strings.HasPrefix(token, "-") && !strings.HasPrefix(token, "--") && strings.Contains(token[1:], "R")) {
				return true
			}
		case "du":
			if token == "-L" || token == "--dereference" {
				return true
			}
		case "tree":
			if token == "-l" {
				return true
			}
		case "ls":
			if token == "-L" ||
				(strings.HasPrefix(token, "-") && !strings.HasPrefix(token, "--") && strings.Contains(token[1:], "L")) {
				return true
			}
		}
	}
	return false
}

// ClassifyCall reports read-only only when every segment of the command
// chain (split on &&, ||, ;, |) starts with an allowlisted head and the
// whole command carries no write markers. Conservative by construction:
// anything unparsed stays KindExecute.
func (t *Shell) ClassifyCall(args map[string]any) Kind {
	command := strings.TrimSpace(strArg(args, "command"))
	script := strings.TrimSpace(strArg(args, "script"))
	if shellSourceIsDestructive(command) || shellSourceIsDestructive(script) {
		return KindDestructive
	}
	// An interpreter script is executable program input, not a read-only
	// description of the interpreter command. Never auto-approve it merely
	// because command happens to be an allowlisted reader.
	if script != "" {
		return KindExecute
	}
	if boolArg(args, "background") || boolArg(args, "pty") {
		return KindExecute
	}
	if command == "" || strings.ContainsAny(command, "\n\r") {
		return KindExecute
	}
	if writeMarkersRe.MatchString(command) {
		return KindExecute
	}
	var classificationBase, classificationNamespace string
	if t != nil && t.WS != nil {
		var ok bool
		classificationBase, classificationNamespace, ok = t.shellClassificationRoots(args)
		if !ok {
			return KindExecute
		}
	} else if strings.TrimSpace(strArg(args, "directory")) != "" {
		// A relative operand cannot be classified without knowing the actual
		// working directory. Keep callers that provide no workspace in the
		// conservative approval path rather than guessing from process cwd.
		return KindExecute
	}
	for _, seg := range splitCommandChain(command) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return KindExecute
		}
		words := parseTopLevelShellWords(seg)
		if len(words) == 0 {
			return KindExecute
		}
		if shellReadsHostState(words) {
			return KindExecute
		}
		if classificationBase != "" &&
			t.shellReadsThroughEscapingSymlink(words, classificationBase, classificationNamespace) {
			return KindExecute
		}
		if readOnlyGitRe.MatchString(seg) {
			if shellWritesNamedOutput("git", words) {
				return KindExecute
			}
			continue
		}
		head := words[0].value
		// Only the actual allowlisted command name is eligible. Treat `./cat`,
		// `/workspace/rg`, and similar path-qualified executables as arbitrary
		// programs; basename matching would otherwise auto-run workspace code.
		if filepath.Base(head) != head {
			return KindExecute
		}
		if !readOnlyHeads[head] {
			return KindExecute
		}
		if shellMayFollowRecursiveSymlinks(head, words) {
			return KindExecute
		}
		if shellWritesNamedOutput(head, words) {
			return KindExecute
		}
		if head == "find" {
			if kind := classifyFindWords(words); kind != KindReadOnly {
				return kind
			}
		}
	}
	return KindReadOnly
}

func splitCommandChain(command string) []string {
	return splitTopLevelShellSegments(command)
}

func (t *Shell) Execute(ctx context.Context, args map[string]any) Result {
	origin := time.Now()
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	command := strings.TrimSpace(strArg(args, "command"))
	script := strArg(args, "script")
	readOnlyWorkspace := boolArg(args, ReadOnlyWorkspaceArgument)
	allowedMutationPaths := workspaceMutationPathSet(t.WS, args[AllowedWorkspaceMutationPathsArgument])
	if command == "" {
		return errResultf("command parameter is required")
	}
	if hasUnboundedHostRootTraversal(command) || hasUnboundedHostRootTraversal(script) {
		return errResultf("unbounded host-root traversal is not allowed; search inside /workspace or another specific directory instead of using find /")
	}
	if readOnlyWorkspace && (boolArg(args, "background") || boolArg(args, "temporary")) {
		return errResultf("isolated requirement checks must run in the foreground workspace clone")
	}
	if len(allowedMutationPaths) > 0 && boolArg(args, "background") {
		return errResultf("background shell commands are unavailable while an explicit file mutation scope is active")
	}
	if script != "" && boolArg(args, "background") {
		return errResultf("script mode is foreground-only; omit background or save a durable script explicitly")
	}
	if untrackedProcessSignalRe.MatchString(command) || untrackedProcessSignalRe.MatchString(script) {
		return errResultf("untracked process signalling is not supported; stop only a harness-owned background process with process(action=stop, process_id=...)")
	}
	if hasUntrackedBackground(command) || hasNestedUntrackedBackground(command) {
		return errResultf("untracked shell backgrounding is not supported; remove &, nohup, disown, or setsid and call run_shell_command with background=true, then use process(action=poll|logs|stop)")
	}
	if script != "" {
		switch strings.TrimSpace(command) {
		case "bash", "zsh":
			if hasUntrackedBackground(script) || hasNestedUntrackedBackground(script) {
				return errResultf("untracked shell backgrounding is not supported inside script; remove &, nohup, disown, or setsid and call run_shell_command with background=true, then use process(action=poll|logs|stop)")
			}
		}
	}
	validationBefore, validationErr := t.prepareValidationObservations(args)
	if validationErr != nil {
		return errResult(validationErr)
	}

	executionRoot := t.WS.Root
	cleanupExecutionRoot := func() {}
	if readOnlyWorkspace {
		clone, cleanup, cloneErr := cloneReadOnlyWorkspace(t.WS.Root)
		if cloneErr != nil {
			return errResultf("prepare isolated requirement-check workspace: %v", cloneErr)
		}
		executionRoot = clone
		cleanupExecutionRoot = cleanup
		defer cleanupExecutionRoot()
	}
	dir := executionRoot
	temporary := boolArg(args, "temporary")
	if temporary && strings.TrimSpace(strArg(args, "directory")) != "" {
		return errResultf("temporary=true owns its working directory; omit directory and use absolute /workspace paths for inputs")
	}
	if d := strArg(args, "directory"); d != "" {
		resolved, err := t.WS.Resolve(d)
		if err != nil {
			return errResult(err)
		}
		if readOnlyWorkspace {
			relative, relErr := filepath.Rel(t.WS.Root, resolved)
			if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return errResultf("isolated requirement-check directory must stay inside /workspace")
			}
			dir = filepath.Join(executionRoot, relative)
		} else {
			dir = resolved
		}
	}
	protectedBefore, err := t.WS.snapshotProtectedFiles()
	if err != nil {
		return errResult(err)
	}
	var beforeWorkspace map[string]workspaceFileState
	var beforeScopedWorkspace map[string]workspaceFileContentState
	if readOnlyWorkspace {
		beforeWorkspace = snapshotWorkspaceFiles(executionRoot)
	} else if t.ClassifyCall(args) != KindReadOnly {
		beforeWorkspace = snapshotWorkspaceFiles(t.WS.Root)
		if len(allowedMutationPaths) > 0 {
			beforeScopedWorkspace = snapshotWorkspaceFileContents(t.WS.Root)
		}
	}
	// File tools expose /workspace as a stable model-facing alias. Local shells
	// do not have that literal directory, so map the alias at execution time.
	// Preserve the surrounding quote context: blindly inserting a second pair
	// of single quotes corrupts paths inside heredoc scripts such as
	// open('/workspace/input.csv').
	command = rewriteWorkspaceShellAliases(command, executionRoot)

	env, _, err := processEnvironment(t.Manager)
	if err != nil {
		return errResult(err)
	}
	// Durable programs cannot use the model-facing /workspace alias as a real
	// filesystem path once their interpreter starts. Give every foreground and
	// background process one stable, session-scoped way to discover the physical
	// root without creating a global /workspace symlink (which would be unsafe
	// across concurrent sessions).
	env = append(env, "MEADOW_WORKSPACE_ROOT="+executionRoot)
	if boolArg(args, "background") {
		if t.Manager == nil {
			return errResultf("background process manager is not configured")
		}
		if err := ctx.Err(); err != nil {
			return errResult(err)
		}
		snapshot, err := t.Manager.startWithOrigin(
			command, dir, env, boolArg(args, "pty"), temporary, boolArg(args, "notify_on_complete"), origin,
		)
		if err != nil {
			return errResult(err)
		}
		payload, _ := json.Marshal(snapshot)
		return Result{
			LLMContent: string(payload),
			Display:    fmt.Sprintf("background %s pid %d", snapshot.ProcessID, snapshot.PID),
		}
	}

	cleanupDir := ""
	if temporary {
		if t.Manager != nil {
			cleanupDir, err = t.Manager.NewScratchDir()
		} else {
			cleanupDir, err = os.MkdirTemp("", "meadow-command-*")
		}
		if err != nil {
			return errResult(err)
		}
		defer os.RemoveAll(cleanupDir)
		dir = cleanupDir
	}

	timeout := shellDefaultTimeout
	if ms, ok := intArg(args, "timeout"); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
		if timeout > shellMaxTimeout {
			timeout = shellMaxTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	scriptPath := ""
	if script != "" {
		interpreter, ok := temporaryScriptInterpreter(command)
		if !ok {
			return errResultf("script mode requires command to be exactly python3, node, bash, or zsh")
		}
		temporaryFile, createErr := os.CreateTemp(dir, ".meadow-script-*")
		if createErr != nil {
			return errResult(createErr)
		}
		scriptPath = temporaryFile.Name()
		defer os.Remove(scriptPath)
		rewrittenScript := rewriteWorkspaceShellAliases(script, executionRoot)
		if readOnlyWorkspace && interpreter == "python3" {
			// An audit can load a logical /workspace path from JSON or another
			// artifact at runtime. Literal rewriting cannot see that value, so
			// keep Python filesystem APIs on the same model-facing namespace while
			// the checker executes inside its private clone. This is capability
			// plumbing only: file contents stay untouched and mutation detection
			// still observes the physical clone.
			rewrittenScript = pythonWorkspaceAliasPrelude(executionRoot) + rewrittenScript
		}
		if _, writeErr := temporaryFile.WriteString(rewrittenScript); writeErr != nil {
			_ = temporaryFile.Close()
			return errResult(writeErr)
		}
		if closeErr := temporaryFile.Close(); closeErr != nil {
			return errResult(closeErr)
		}
		cmd = exec.Command(interpreter, scriptPath)
	} else {
		cmd = exec.Command(commandShell(), "-c", command)
	}
	cmd.Dir = dir
	cmd.Env = env
	stdout, stderr, runErr, timedOut := runForegroundShell(ctx, cmd, boolArg(args, "pty"))
	if scriptPath != "" {
		_ = os.Remove(scriptPath)
		scriptPath = ""
	}

	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else if !timedOut {
			return errResult(runErr)
		}
	}
	if timedOut {
		exitCode = -1
	}

	var b strings.Builder
	// Keep process facts explicit even for exit 0. Output remains raw evidence:
	// a program may print failed checks and still choose to exit successfully.
	fmt.Fprintf(&b, "process_status: {\"exit_code\":%d,\"timed_out\":%t}\nstdout: ", exitCode, timedOut)
	if out := strings.TrimRight(stdout, "\n"); out != "" {
		b.WriteString(truncateOutput(out))
	} else {
		b.WriteString("(no stdout)")
	}
	if errOut := strings.TrimRight(stderr, "\n"); errOut != "" {
		b.WriteString("\nstderr: " + truncateOutput(errOut))
		if diagnostic := workspaceAliasRuntimeDiagnostic(errOut); diagnostic != "" {
			b.WriteString("\n" + diagnostic)
		}
	} else {
		b.WriteString("\nstderr: (no stderr)")
	}
	if timedOut {
		b.WriteString(fmt.Sprintf("\n(command timed out after %s)", timeout))
	}
	if exitCode != 0 {
		b.WriteString(fmt.Sprintf("\n(exit code %d)", exitCode))
	}
	expectedExitCodes := intSliceArg(args, "expected_exit_codes")
	if len(expectedExitCodes) == 0 {
		expectedExitCodes = []int{0}
	}
	exitExpected := false
	for _, expected := range expectedExitCodes {
		if exitCode == expected {
			exitExpected = true
			break
		}
	}
	if !timedOut && exitExpected && (len(expectedExitCodes) != 1 || expectedExitCodes[0] != 0) {
		b.WriteString(fmt.Sprintf("\n(exit code matched expected_exit_codes %v)", expectedExitCodes))
	}
	protectedChanged, protectedRestoreErr := t.WS.restoreProtectedFiles(protectedBefore)
	if len(protectedChanged) > 0 {
		b.WriteString("\nPROTECTED_FILE_WRITE_BLOCKED: restored immutable host files: " +
			strings.Join(protectedChanged, ", "))
	}
	scopeBlocked, scopeRestoreErr := restoreOutOfScopeWorkspaceChanges(
		t.WS, beforeScopedWorkspace, allowedMutationPaths,
	)
	if len(scopeBlocked) > 0 {
		b.WriteString("\nMUTATION_SCOPE_BLOCKED: restored out-of-scope files: " +
			strings.Join(scopeBlocked, ", "))
	}

	res := Result{
		LLMContent: b.String(),
		Display:    fmt.Sprintf("exit %d", exitCode),
	}
	if readOnlyWorkspace {
		changed := changedWorkspacePaths(
			t.WS,
			beforeWorkspace,
			snapshotWorkspaceFiles(executionRoot),
		)
		if len(changed) > 0 {
			res.LLMContent += "\nREAD_ONLY_AUDIT_MUTATION_REJECTED: isolated check changed " +
				strings.Join(changed, ", ")
			res.Err = errors.Join(res.Err, errors.New("isolated requirement check mutated its workspace clone"))
		}
	} else if beforeWorkspace != nil {
		res.ChangedPaths = changedWorkspacePaths(
			t.WS,
			beforeWorkspace,
			snapshotWorkspaceFiles(t.WS.Root),
		)
	}
	if timedOut {
		res.Err = fmt.Errorf("command timed out after %s", timeout)
	} else if !exitExpected {
		res.Err = fmt.Errorf("command exited with code %d; expected one of %v", exitCode, expectedExitCodes)
	}
	if protectedRestoreErr != nil {
		res.Err = errors.Join(res.Err, protectedRestoreErr)
	}
	if scopeRestoreErr != nil {
		res.Err = errors.Join(res.Err, scopeRestoreErr)
	}
	if len(scopeBlocked) > 0 {
		res.Err = errors.Join(res.Err, fmt.Errorf(
			"shell attempted to modify file(s) outside the explicit mutation scope: %s",
			strings.Join(scopeBlocked, ", "),
		))
	}
	if len(protectedChanged) > 0 {
		res.Err = errors.Join(res.Err, fmt.Errorf(
			"shell attempted to modify protected file(s): %s", strings.Join(protectedChanged, ", "),
		))
	}
	res.ProcessExited = !timedOut && ctx.Err() == nil && exitCode >= 0 &&
		!readOnlyWorkspace && len(scopeBlocked) == 0 && len(protectedChanged) == 0 &&
		scopeRestoreErr == nil && protectedRestoreErr == nil
	t.attachValidationObservations(&res, validationBefore)
	return res
}

func hasUnboundedHostRootTraversal(source string) bool {
	if strings.TrimSpace(source) == "" {
		return false
	}
	for _, segment := range splitTopLevelShellSegments(source) {
		segment = strings.TrimSpace(segment)
		segment = strings.TrimPrefix(segment, "sudo ")
		if matched, _ := regexp.MatchString(`^find\s+/(?:\s|$)`, segment); matched {
			return true
		}
	}
	return false
}

func workspaceAliasRuntimeDiagnostic(stderr string) string {
	if !strings.Contains(stderr, "/workspace/") {
		return ""
	}
	lower := strings.ToLower(stderr)
	if !strings.Contains(lower, "filenotfounderror") &&
		!strings.Contains(lower, "no such file or directory") &&
		!strings.Contains(lower, "enoent") {
		return ""
	}
	return "WORKSPACE_ALIAS_RUNTIME_ERROR: `/workspace` is a tool-call alias, not a literal path available inside a saved program. Edit the saved program before retrying: resolve files relative to its working directory, or read the physical session root from the `MEADOW_WORKSPACE_ROOT` environment variable. Do not rerun the unchanged program."
}

type workspaceFileState struct {
	size    int64
	modTime int64
	mode    os.FileMode
}

type workspaceFileContentState struct {
	data      []byte
	mode      os.FileMode
	symlink   string
	isSymlink bool
}

func workspaceMutationPathSet(workspace *Workspace, raw any) map[string]bool {
	result := map[string]bool{}
	var values []string
	switch typed := raw.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, value := range typed {
			if text, ok := value.(string); ok {
				values = append(values, text)
			}
		}
	}
	for _, value := range values {
		resolved, err := workspace.Resolve(value)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(workspace.Root, resolved)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		result[filepath.ToSlash(relative)] = true
	}
	return result
}

func snapshotWorkspaceFileContents(root string) map[string]workspaceFileContentState {
	snapshot := map[string]workspaceFileContentState{}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." {
			return nil
		}
		if ignoredWorkspaceStatePath(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		state := workspaceFileContentState{mode: entry.Type()}
		if entry.Type()&os.ModeSymlink != 0 {
			state.isSymlink = true
			state.symlink, _ = os.Readlink(path)
		} else {
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				return nil
			}
			state.mode = info.Mode()
			state.data, _ = os.ReadFile(path)
		}
		snapshot[filepath.ToSlash(relative)] = state
		return nil
	})
	return snapshot
}

func restoreOutOfScopeWorkspaceChanges(
	workspace *Workspace,
	before map[string]workspaceFileContentState,
	allowed map[string]bool,
) ([]string, error) {
	if before == nil || len(allowed) == 0 {
		return nil, nil
	}
	after := snapshotWorkspaceFileContents(workspace.Root)
	changed := map[string]bool{}
	for relative, prior := range before {
		current, ok := after[relative]
		if !ok || prior.mode != current.mode || prior.isSymlink != current.isSymlink ||
			prior.symlink != current.symlink || !bytes.Equal(prior.data, current.data) {
			changed[relative] = true
		}
	}
	for relative := range after {
		if _, ok := before[relative]; !ok {
			changed[relative] = true
		}
	}
	var blocked []string
	var restoreErr error
	for relative := range changed {
		if allowed[relative] {
			continue
		}
		path := filepath.Join(workspace.Root, filepath.FromSlash(relative))
		prior, existed := before[relative]
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			restoreErr = errors.Join(restoreErr, removeErr)
			continue
		}
		if existed {
			if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o755); mkdirErr != nil {
				restoreErr = errors.Join(restoreErr, mkdirErr)
				continue
			}
			if prior.isSymlink {
				restoreErr = errors.Join(restoreErr, os.Symlink(prior.symlink, path))
			} else if writeErr := os.WriteFile(path, prior.data, prior.mode.Perm()); writeErr != nil {
				restoreErr = errors.Join(restoreErr, writeErr)
			}
		}
		blocked = append(blocked, workspace.ModelPath(path))
	}
	sort.Strings(blocked)
	return blocked, restoreErr
}

func snapshotWorkspaceFiles(root string) map[string]workspaceFileState {
	snapshot := map[string]workspaceFileState{}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." {
			return nil
		}
		if ignoredWorkspaceStatePath(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		snapshot[filepath.ToSlash(relative)] = workspaceFileState{
			size: info.Size(), modTime: info.ModTime().UnixNano(), mode: info.Mode(),
		}
		return nil
	})
	return snapshot
}

func ignoredWorkspaceStatePath(relative string) bool {
	for _, component := range strings.Split(filepath.ToSlash(relative), "/") {
		switch component {
		case ".git", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache":
			return true
		}
	}
	return false
}

func changedWorkspacePaths(
	workspace *Workspace,
	before, after map[string]workspaceFileState,
) []string {
	changed := make([]string, 0)
	for relative, afterState := range after {
		if beforeState, ok := before[relative]; !ok || beforeState != afterState {
			changed = append(changed, workspace.ModelPath(filepath.Join(workspace.Root, relative)))
		}
	}
	for relative := range before {
		if _, ok := after[relative]; !ok {
			changed = append(changed, workspace.ModelPath(filepath.Join(workspace.Root, relative)))
		}
	}
	sort.Strings(changed)
	return changed
}

func temporaryScriptInterpreter(command string) (string, bool) {
	switch strings.TrimSpace(command) {
	case "python3":
		return "python3", true
	case "node":
		return "node", true
	case "bash":
		return "bash", true
	case "zsh":
		return "zsh", true
	default:
		return "", false
	}
}

func runForegroundShell(ctx context.Context, cmd *exec.Cmd, usePTY bool) (stdout, stderr string, runErr error, timedOut bool) {
	if usePTY {
		var output bytes.Buffer
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
		if err != nil {
			return "", "", err, false
		}
		wait := make(chan error, 1)
		go func() {
			_, _ = io.Copy(&output, ptmx)
			_ = ptmx.Close()
			wait <- cmd.Wait()
		}()
		select {
		case err := <-wait:
			return output.String(), "", err, false
		case <-ctx.Done():
			_ = forceKillProcessTree(cmd.Process)
			_ = ptmx.Close()
			err := <-wait
			return output.String(), "", err, ctx.Err() == context.DeadlineExceeded
		}
	}

	configureProcessGroup(cmd)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		return "", "", err, false
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		return out.String(), errOut.String(), err, false
	case <-ctx.Done():
		_ = forceKillProcessTree(cmd.Process)
		err := <-wait
		return out.String(), errOut.String(), err, ctx.Err() == context.DeadlineExceeded
	}
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func rewriteWorkspaceShellAliases(command, root string) string {
	const alias = "/workspace"
	var rewritten strings.Builder
	rewritten.Grow(len(command) + len(root))
	var quote byte
	escaped := false
	previousEscaped := false

	for index := 0; index < len(command); {
		aliasStart := strings.HasPrefix(command[index:], alias)
		localFileURI := aliasStart && workspaceShellAliasLocalFileURI(command, index, quote)
		if aliasStart &&
			(workspaceShellAliasStartsPath(command, index, quote, escaped, previousEscaped) || localFileURI) {
			end := index + len(alias)
			boundary := end == len(command) || command[end] == '/' ||
				strings.ContainsRune("'\"", rune(command[end])) ||
				quote == 0 && strings.ContainsRune(" \t\r\n;|&()[]{}<>,:", rune(command[end]))
			if boundary {
				replacementRoot := root
				if localFileURI {
					// A local URL uses URL path escaping, including spaces and '#'.
					// Preserve its existing scheme/authority and suffix verbatim.
					replacementRoot = (&url.URL{Path: filepath.ToSlash(root)}).EscapedPath()
				}
				switch quote {
				case '\'':
					rewritten.WriteString(strings.ReplaceAll(replacementRoot, "'", `'"'"'`))
				case '"':
					escapedRoot := strings.NewReplacer(
						`\`, `\\`,
						`"`, `\"`,
						`$`, `\$`,
						"`", "\\`",
					).Replace(replacementRoot)
					rewritten.WriteString(escapedRoot)
				default:
					rewritten.WriteString(shellSingleQuote(replacementRoot))
				}
				index = end
				previousEscaped = false
				continue
			}
		}

		char := command[index]
		rewritten.WriteByte(char)
		previousEscaped = escaped
		if escaped {
			escaped = false
		} else if char == '\\' && quote != '\'' {
			escaped = true
		} else if quote == 0 && (char == '\'' || char == '"') {
			quote = char
		} else if quote != 0 && char == quote {
			quote = 0
		}
		index++
	}
	return rewritten.String()
}

// An explicit local file URI can name the same virtual root as a filesystem
// path. Never rewrite remote authorities or an embedded /workspace component
// farther down an existing physical URL path.
func workspaceShellAliasLocalFileURI(command string, index int, quote byte) bool {
	for _, prefix := range []string{"file:", "file://", "file://localhost"} {
		start := index - len(prefix)
		if start >= 0 && strings.EqualFold(command[start:index], prefix) &&
			workspaceShellAliasStartsPath(command, start, quote, false, false) {
			return true
		}
	}
	return false
}

// Only a path beginning with the alias belongs to the virtual namespace. In
// particular, a physical root may itself end in /workspace, and paths with an
// escaped or quoted space must not be treated as a new shell argument there.
// Quotes also delimit path literals in heredoc and script source.
func workspaceShellAliasStartsPath(command string, index int, quote byte, escaped, previousEscaped bool) bool {
	if escaped {
		return false
	}
	// An unquoted/double-quoted backslash-newline pair is removed by the shell;
	// inspect the preceding logical character, not the continuation newline.
	if previousEscaped && command[index-1] == '\n' {
		for index >= 2 && command[index-2:index] == "\\\n" {
			index -= 2
		}
	} else if previousEscaped {
		return false
	}
	if index == 0 {
		return true
	}
	previous := command[index-1]
	if previous == '\'' || previous == '"' {
		// A quote can start a path literal, but cannot reset an existing path
		// token such as /tmp/"parent"'/workspace/note.txt'.
		return quote != 0 && (index == 1 ||
			strings.ContainsRune(" \t\r\n=([{,;|&<>:", rune(command[index-2])))
	}
	return quote == 0 && strings.ContainsRune(" \t\r\n=([{,;|&<>:", rune(previous))
}

// pythonWorkspaceAliasPrelude maps /workspace values discovered at runtime
// (for example an artifact path read from answer.json) into the isolated audit
// clone. Direct path literals are already handled by
// rewriteWorkspaceShellAliases; this narrow Python shim closes the dynamic
// value gap without rewriting any cloned artifact content.
func pythonWorkspaceAliasPrelude(root string) string {
	quotedRoot, _ := json.Marshal(root)
	return fmt.Sprintf(`import builtins as _meadow_builtins
import io as _meadow_io
import os as _meadow_os

_meadow_workspace_root = %s
_meadow_workspace_alias = "/workspace"

def _meadow_workspace_path(value):
    try:
        raw = _meadow_os.fspath(value)
    except TypeError:
        return value
    if isinstance(raw, bytes):
        alias = _meadow_workspace_alias.encode()
        root = _meadow_workspace_root.encode()
        separator = b"/"
    else:
        alias = _meadow_workspace_alias
        root = _meadow_workspace_root
        separator = "/"
    if raw == alias:
        return root
    if raw.startswith(alias + separator):
        return root + raw[len(alias):]
    return value

class _MeadowWorkspaceUnary:
    def __init__(self, original):
        self.original = original
    def __call__(self, path, *args, **kwargs):
        return self.original(_meadow_workspace_path(path), *args, **kwargs)

class _MeadowWorkspaceOptional:
    def __init__(self, original):
        self.original = original
    def __call__(self, path=None, *args, **kwargs):
        if path is None:
            return self.original(*args, **kwargs)
        return self.original(_meadow_workspace_path(path), *args, **kwargs)

class _MeadowWorkspaceBinary:
    def __init__(self, original):
        self.original = original
    def __call__(self, source, destination, *args, **kwargs):
        return self.original(
            _meadow_workspace_path(source),
            _meadow_workspace_path(destination),
            *args,
            **kwargs,
        )

_meadow_builtins.open = _MeadowWorkspaceUnary(_meadow_builtins.open)
_meadow_io.open = _MeadowWorkspaceUnary(_meadow_io.open)
_meadow_original_os_open = _meadow_os.open
class _MeadowWorkspaceOSOpen:
    def __call__(self, path, flags, mode=0o777, dir_fd=None):
        mapped = _meadow_workspace_path(path)
        if dir_fd is None:
            return _meadow_original_os_open(mapped, flags, mode)
        return _meadow_original_os_open(mapped, flags, mode, dir_fd=dir_fd)
_meadow_os.open = _MeadowWorkspaceOSOpen()
for _meadow_name in (
    "access", "chdir", "chmod", "chown", "lstat", "mkdir",
    "readlink", "remove", "rmdir", "stat", "truncate", "unlink",
):
    if hasattr(_meadow_os, _meadow_name):
        setattr(
            _meadow_os,
            _meadow_name,
            _MeadowWorkspaceUnary(getattr(_meadow_os, _meadow_name)),
        )
for _meadow_name in ("listdir", "scandir"):
    if hasattr(_meadow_os, _meadow_name):
        setattr(
            _meadow_os,
            _meadow_name,
            _MeadowWorkspaceOptional(getattr(_meadow_os, _meadow_name)),
        )
for _meadow_name in ("link", "rename", "replace", "symlink"):
    if hasattr(_meadow_os, _meadow_name):
        setattr(
            _meadow_os,
            _meadow_name,
            _MeadowWorkspaceBinary(getattr(_meadow_os, _meadow_name)),
        )

`, string(quotedRoot))
}

func hasUntrackedBackground(command string) bool {
	visible := unquotedShellText(command)
	if shellBackgroundWrapperRe.MatchString(visible) {
		return true
	}
	for i := 0; i < len(visible); i++ {
		if visible[i] != '&' {
			continue
		}
		previousIsAmp := i > 0 && visible[i-1] == '&'
		nextIsAmp := i+1 < len(visible) && visible[i+1] == '&'
		redirectionAmp := (i > 0 && (visible[i-1] == '>' || visible[i-1] == '<')) ||
			(i+1 < len(visible) && visible[i+1] == '>')
		if !previousIsAmp && !nextIsAmp && !redirectionAmp {
			return true
		}
	}
	return false
}

func hasNestedUntrackedBackground(command string) bool {
	return hasNestedUntrackedBackgroundAtDepth(command, 0)
}

func hasNestedUntrackedBackgroundAtDepth(command string, depth int) bool {
	if depth >= 8 {
		return true
	}
	for _, segment := range splitTopLevelShellSegments(command) {
		words := parseTopLevelShellWords(segment)
		source, expanded, ok := nestedShellCommandSource(words)
		if ok && (expanded || hasUntrackedBackground(source) ||
			hasDynamicNestedShellSource(source) ||
			hasNestedUntrackedBackgroundAtDepth(source, depth+1)) {
			return true
		}
	}
	return false
}

// A variable-expanded -c/eval program cannot be checked before execution:
// the expansion result is parsed again by the nested shell. Command
// substitutions are rejected only when they form the program itself (or an
// eval argument). A substitution used as an ordinary argument inside a static
// program, such as `bash -c 'printf "%s\n" "$(pwd)"'`, does not become shell
// syntax and remains usable.
func hasDynamicNestedShellSource(source string) bool {
	for _, segment := range splitTopLevelShellSegments(source) {
		words := parseTopLevelShellWords(segment)
		for index := 0; index < len(words); index++ {
			word := words[index]
			value := strings.TrimSpace(word.value)
			if value == "" || isShellAssignmentWord(value) {
				continue
			}
			switch value {
			case "if", "then", "elif", "else", "while", "until", "do", "!", "time":
				// These reserved words introduce another command position in
				// the same top-level segment.
				continue
			case "for", "case", "select":
				// The following word is loop/case data, not a command.
				break
			}
			if word.expanded {
				return true
			}
			break
		}
	}
	return false
}

func isShellAssignmentWord(word string) bool {
	equals := strings.IndexByte(word, '=')
	if equals <= 0 {
		return false
	}
	name := word[:equals]
	for index := 0; index < len(name); index++ {
		char := name[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			char == '_' || (index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func splitTopLevelShellSegments(command string) []string {
	var segments []string
	start := 0
	var quote byte
	escaped := false
	for index := 0; index < len(command); index++ {
		char := command[index]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char != ';' && char != '|' && char != '&' && char != '\n' {
			continue
		}
		segments = append(segments, command[start:index])
		if index+1 < len(command) && command[index+1] == char {
			index++
		}
		start = index + 1
	}
	segments = append(segments, command[start:])
	return segments
}

type topLevelShellWord struct {
	value    string
	expanded bool
}

func parseTopLevelShellWords(segment string) []topLevelShellWord {
	var words []topLevelShellWord
	var current strings.Builder
	var quote byte
	escaped := false
	started := false
	expanded := false
	flush := func() {
		if started {
			words = append(words, topLevelShellWord{value: current.String(), expanded: expanded})
			current.Reset()
			started = false
			expanded = false
		}
	}
	for index := 0; index < len(segment); index++ {
		char := segment[index]
		if escaped {
			current.WriteByte(char)
			started = true
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				current.WriteByte(char)
				if quote != '\'' && (char == '$' || char == '`') {
					expanded = true
				}
			}
			started = true
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			started = true
			continue
		}
		if char == ' ' || char == '\t' || char == '\r' {
			flush()
			continue
		}
		current.WriteByte(char)
		if char == '$' || char == '`' {
			expanded = true
		}
		started = true
	}
	if escaped {
		current.WriteByte('\\')
	}
	flush()
	return words
}

func nestedShellCommandSource(words []topLevelShellWord) (source string, expanded bool, ok bool) {
	index := 0
	if index < len(words) && filepath.Base(words[index].value) == "env" {
		index++
		for index < len(words) {
			if words[index].value == "-S" || words[index].value == "--split-string" {
				if index+1 >= len(words) {
					return "", false, false
				}
				source, nestedExpanded, nestedOK := nestedShellCommandSource(
					parseTopLevelShellWords(words[index+1].value),
				)
				return source, words[index+1].expanded || nestedExpanded, nestedOK
			}
			if !strings.HasPrefix(words[index].value, "-") && !strings.Contains(words[index].value, "=") {
				break
			}
			index++
		}
	}
	for index < len(words) {
		head := filepath.Base(words[index].value)
		if head != "command" && head != "exec" {
			break
		}
		index++
		for index < len(words) && strings.HasPrefix(words[index].value, "-") {
			index++
		}
	}
	if index >= len(words) {
		return "", false, false
	}
	if filepath.Base(words[index].value) == "eval" {
		if index+1 >= len(words) {
			return "", false, false
		}
		var parts []string
		for _, word := range words[index+1:] {
			parts = append(parts, word.value)
			expanded = expanded || word.expanded
		}
		return strings.Join(parts, " "), expanded, true
	}
	if filepath.Base(words[index].value) == "busybox" {
		index++
		if index >= len(words) {
			return "", false, false
		}
	}
	switch filepath.Base(words[index].value) {
	case "sh", "bash", "zsh", "dash", "ksh":
	default:
		return "", false, false
	}
	index++
	for index < len(words) {
		option := words[index].value
		if option == "--command" || option == "-c" ||
			(strings.HasPrefix(option, "-") && !strings.HasPrefix(option, "--") &&
				strings.Contains(option[1:], "c")) {
			if index+1 < len(words) {
				return words[index+1].value, words[index+1].expanded, true
			}
			return "", false, false
		}
		if !strings.HasPrefix(option, "-") {
			return "", false, false
		}
		index++
	}
	return "", false, false
}

// unquotedShellText preserves only shell syntax outside quotes. It prevents
// literal text such as printf '%s\n' 'A & B' from being mistaken for process
// control while still detecting any bare background operator or wrapper.
func unquotedShellText(command string) string {
	visible := []byte(stripShellHeredocBodies(command))
	var quote byte
	escaped := false
	for i := 0; i < len(visible); i++ {
		char := visible[i]
		if escaped {
			visible[i] = ' '
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			visible[i] = ' '
			escaped = true
			continue
		}
		if quote != 0 {
			visible[i] = ' '
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '#' && (i == 0 || visible[i-1] == ' ' || visible[i-1] == '\t' ||
			visible[i-1] == '\n' || visible[i-1] == ';') {
			for i < len(visible) && visible[i] != '\n' {
				visible[i] = ' '
				i++
			}
			continue
		}
		if char == '\'' || char == '"' {
			visible[i] = ' '
			quote = char
		}
	}
	return string(visible)
}

func stripShellHeredocBodies(command string) string {
	visible := []byte(command)
	searchFrom := 0
	for searchFrom < len(command) {
		headerEnd, delimiter, stripTabs, ok := findShellHeredocStart(command, searchFrom)
		if !ok {
			break
		}
		newline := strings.IndexByte(command[headerEnd:], '\n')
		if newline < 0 {
			break
		}
		bodyStart := headerEnd + newline + 1
		lineStart := bodyStart
		found := false
		for lineStart <= len(command) {
			lineEnd := strings.IndexByte(command[lineStart:], '\n')
			if lineEnd < 0 {
				lineEnd = len(command)
			} else {
				lineEnd += lineStart
			}
			line := strings.TrimSuffix(command[lineStart:lineEnd], "\r")
			if stripTabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == delimiter {
				for index := bodyStart; index < lineEnd; index++ {
					if visible[index] != '\n' {
						visible[index] = ' '
					}
				}
				searchFrom = lineEnd
				found = true
				break
			}
			if lineEnd >= len(command) {
				break
			}
			lineStart = lineEnd + 1
		}
		if !found {
			break
		}
	}
	return string(visible)
}

// findShellHeredocStart recognizes a heredoc operator only when it appears in
// shell syntax. Raw regexp matching is unsafe here because a fake `<<EOF`
// inside a comment or quoted string could hide a real background operator in
// the following line.
func findShellHeredocStart(command string, start int) (headerEnd int, delimiter string, stripTabs bool, ok bool) {
	var quote byte
	escaped := false
	for index := start; index < len(command); index++ {
		char := command[index]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '#' && (index == 0 || command[index-1] == ' ' || command[index-1] == '\t' ||
			command[index-1] == '\n' || command[index-1] == ';') {
			if newline := strings.IndexByte(command[index:], '\n'); newline >= 0 {
				index += newline
				continue
			}
			return 0, "", false, false
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char != '<' || index+1 >= len(command) || command[index+1] != '<' ||
			(index+2 < len(command) && command[index+2] == '<') {
			continue
		}
		cursor := index + 2
		if cursor < len(command) && command[cursor] == '-' {
			stripTabs = true
			cursor++
		}
		for cursor < len(command) && (command[cursor] == ' ' || command[cursor] == '\t') {
			cursor++
		}
		delimiterStart := cursor
		var delimiterBuilder strings.Builder
		var delimiterQuote byte
		for cursor < len(command) {
			char := command[cursor]
			if delimiterQuote == 0 && strings.ContainsRune(" \t\r\n;|&()<>", rune(char)) {
				break
			}
			if char == '\\' && delimiterQuote != '\'' {
				if cursor+1 >= len(command) || command[cursor+1] == '\n' ||
					command[cursor+1] == '\r' {
					break
				}
				// Shell quote removal strips an unquoted/double-quoted
				// backslash and keeps the escaped delimiter character.
				delimiterBuilder.WriteByte(command[cursor+1])
				cursor += 2
				continue
			}
			if delimiterQuote == 0 && (char == '\'' || char == '"') {
				delimiterQuote = char
				cursor++
				continue
			}
			if delimiterQuote != 0 && char == delimiterQuote {
				delimiterQuote = 0
				cursor++
				continue
			}
			delimiterBuilder.WriteByte(char)
			cursor++
		}
		if cursor == delimiterStart || delimiterBuilder.Len() == 0 || delimiterQuote != 0 {
			continue
		}
		delimiter = delimiterBuilder.String()
		return cursor, delimiter, stripTabs, true
	}
	return 0, "", false, false
}

func truncateOutput(s string) string {
	if len(s) <= shellMaxOutputBytes {
		return s
	}
	head := s[:shellHeadBytes]
	tail := s[len(s)-shellTailBytes:]
	return head + fmt.Sprintf("\n[... %d bytes truncated ...]\n", len(s)-shellHeadBytes-shellTailBytes) + tail
}
