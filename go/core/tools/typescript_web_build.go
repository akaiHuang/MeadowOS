package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const typeScriptWebBuildTimeout = 2 * time.Minute

// TypeScriptWebBuild is the static-web code gate. The model saves an HTML
// shell and a TypeScript source in the same app tree; this tool owns strict
// compilation next to that source before Meadow Browser checks the visuals.
// It is intentionally narrower than a shell so a weak model cannot replace a
// failing build with an unrelated command that merely exits zero.
type TypeScriptWebBuild struct {
	WS           *Workspace
	CompilerPath string
}

func (t *TypeScriptWebBuild) Name() string { return "typescript_web_build" }
func (t *TypeScriptWebBuild) Description() string {
	return "Compile a saved TypeScript entry for a directly-openable local HTML app. The .ts source may be beside the HTML or in a descendant such as src/app.ts; the HTML must load the corresponding relative compiled .js path as a classic script (no type=module), the TypeScript must not emit ES-module import/export syntax, and compiler diagnostics must be repaired before visual verification."
}
func (t *TypeScriptWebBuild) Kind() Kind { return KindExecute }
func (t *TypeScriptWebBuild) Schema() map[string]any {
	return obj(map[string]any{
		"html_path":   str("Saved HTML shell, for example /session/snake_game.html for a standalone session artifact or /workspace/existing_app.html when editing the selected project."),
		"source_path": str("Saved TypeScript entry in the same app tree, for example /session/snake_game.ts or /workspace/app/src/snake.ts. The HTML must load its compiled relative JavaScript path, such as snake_game.js or src/snake.js."),
	}, "html_path", "source_path")
}

func (t *TypeScriptWebBuild) Execute(ctx context.Context, args map[string]any) Result {
	if t == nil || t.WS == nil {
		return errResultf("TypeScript web build requires a workspace")
	}
	htmlPath, err := t.WS.Resolve(strArg(args, "html_path"))
	if err != nil {
		return t.recovery("invalid_html_path", err.Error(), strArg(args, "html_path"), args)
	}
	sourcePath, err := t.WS.Resolve(strArg(args, "source_path"))
	if err != nil {
		return t.recovery("invalid_typescript_path", err.Error(), strArg(args, "source_path"), args)
	}
	if ext := strings.ToLower(filepath.Ext(htmlPath)); ext != ".html" && ext != ".htm" {
		return t.recovery("html_required", "html_path must name a .html or .htm file", t.WS.ModelPath(htmlPath), args)
	}
	if strings.ToLower(filepath.Ext(sourcePath)) != ".ts" || strings.HasSuffix(strings.ToLower(sourcePath), ".d.ts") {
		return t.recovery("typescript_source_required", "source_path must name an executable .ts source file", t.WS.ModelPath(sourcePath), args)
	}
	sourceDirRelative, err := filepath.Rel(filepath.Dir(htmlPath), filepath.Dir(sourcePath))
	if err != nil || sourceDirRelative == ".." || strings.HasPrefix(sourceDirRelative, ".."+string(filepath.Separator)) {
		return t.recovery("web_source_outside_app_tree", "TypeScript source must be beside the HTML or in one of its descendant directories so the compiled page stays directly openable", t.WS.ModelPath(sourcePath), args)
	}
	html, err := os.ReadFile(htmlPath)
	if err != nil {
		return t.recovery("html_not_saved", fmt.Sprintf("read HTML shell: %v", err), t.WS.ModelPath(htmlPath), args)
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		return t.recovery("typescript_not_saved", fmt.Sprintf("read TypeScript source: %v", err), t.WS.ModelPath(sourcePath), args)
	}
	if len(bytes.TrimSpace(source)) < 80 {
		return t.recovery("typescript_source_incomplete", "TypeScript source is too small to contain the interactive app logic", t.WS.ModelPath(sourcePath), args)
	}
	jsName := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath)) + ".js"
	jsReference := filepath.ToSlash(filepath.Join(sourceDirRelative, jsName))
	if !htmlLoadsScript(html, jsReference) {
		return t.recovery("compiled_script_not_loaded", fmt.Sprintf("HTML must load the compiled script %q with a relative script src attribute", jsReference), t.WS.ModelPath(htmlPath), args)
	}
	if hasSubstantialInlineScript(html) {
		return t.recovery("inline_javascript_not_typechecked", "Move interactive application logic out of inline <script> content and into the saved TypeScript source", t.WS.ModelPath(htmlPath), args)
	}
	if htmlLoadsScriptAsModule(html, jsReference) {
		return t.recovery(
			"local_module_script_not_supported",
			fmt.Sprintf("The saved page is validated directly from file:// and cannot load %s as an ES module. Remove type=\"module\" from the existing script tag and keep the TypeScript entry free of import/export syntax; do not duplicate the script tag", jsName),
			t.WS.ModelPath(htmlPath), args,
		)
	}
	if missing := missingStaticDOMIDReferences(html, source); len(missing) > 0 {
		return t.recovery(
			"typescript_dom_binding_missing",
			"TypeScript references element IDs that do not exist in the saved sibling HTML. Repair only the saved .ts selectors so they match the existing HTML, then rebuild:\n"+strings.Join(missing, "\n"),
			t.WS.ModelPath(sourcePath), args,
		)
	}

	compiler, err := t.compiler(sourcePath)
	if err != nil {
		return t.recovery("typescript_compiler_unavailable", err.Error(), t.WS.ModelPath(sourcePath), args)
	}
	tempDir, err := os.MkdirTemp(filepath.Dir(sourcePath), ".meadow-ts-build-")
	if err != nil {
		return errResultf("create TypeScript build directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	buildCtx, cancel := context.WithTimeout(ctx, typeScriptWebBuildTimeout)
	defer cancel()
	command, commandArgs, err := typeScriptCompilerCommand(compiler, sourcePath, tempDir)
	if err != nil {
		return t.recovery("typescript_compiler_unavailable", err.Error(), t.WS.ModelPath(sourcePath), args)
	}
	cmd := exec.CommandContext(buildCtx, command, commandArgs...)
	cmd.Dir = filepath.Dir(sourcePath)
	output, runErr := cmd.CombinedOutput()
	if buildCtx.Err() == context.DeadlineExceeded {
		return t.recovery("typescript_build_timeout", "TypeScript build exceeded two minutes", t.WS.ModelPath(sourcePath), args)
	}
	if runErr != nil {
		diagnostic := strings.TrimSpace(string(output))
		diagnostic = appendTypeScriptSourceExcerpts(diagnostic, source)
		if len(diagnostic) > 12*1024 {
			diagnostic = diagnostic[:12*1024] + "\n... diagnostics truncated"
		}
		if diagnostic == "" {
			diagnostic = runErr.Error()
		}
		return t.recovery("typescript_build_failed", "TypeScript build failed. Repair the saved .ts source and retry:\n"+diagnostic, t.WS.ModelPath(sourcePath), args)
	}

	builtPath := filepath.Join(tempDir, jsName)
	built, err := os.ReadFile(builtPath)
	if err != nil {
		return t.recovery("typescript_output_missing", fmt.Sprintf("compiler succeeded but did not emit %s: %v", jsName, err), t.WS.ModelPath(sourcePath), args)
	}
	if javascriptRequiresModule(built) {
		return t.recovery(
			"compiled_module_not_supported_for_local_artifact",
			fmt.Sprintf("The compiled %s contains ES module import/export syntax, which does not run in this directly-opened local artifact contract. Remove unnecessary TypeScript imports/exports and keep the existing HTML script classic; do not add type=\"module\"", jsName),
			t.WS.ModelPath(sourcePath), args,
		)
	}
	outputPath := filepath.Join(filepath.Dir(sourcePath), jsName)
	if err := t.WS.CheckWritable(outputPath); err != nil {
		return errResult(err)
	}
	previous, previousErr := os.ReadFile(outputPath)
	changed := previousErr != nil || !bytes.Equal(previous, built)
	if changed {
		staged := outputPath + ".meadow-build-tmp"
		if err := os.WriteFile(staged, built, 0o644); err != nil {
			return errResultf("stage compiled JavaScript: %v", err)
		}
		if err := os.Rename(staged, outputPath); err != nil {
			_ = os.Remove(staged)
			return errResultf("install compiled JavaScript: %v", err)
		}
		t.WS.NoteRead(outputPath)
	}

	result := Result{
		LLMContent: fmt.Sprintf("TypeScript build passed. %s compiles to %s and %s loads that compiled script. Next call meadow_browser_visual_check on the HTML file.", t.WS.ModelPath(sourcePath), t.WS.ModelPath(outputPath), t.WS.ModelPath(htmlPath)),
		Display:    "TypeScript build passed · " + t.WS.ModelPath(outputPath),
		NoChange:   !changed,
	}
	if changed {
		result.ChangedPaths = []string{t.WS.ModelPath(outputPath)}
	}
	return result
}

var typeScriptDiagnosticLocation = regexp.MustCompile(`(?m)^.*?\.tsx?\((\d+),(\d+)\):\s+error\s+TS\d+:`)

// appendTypeScriptSourceExcerpts makes compiler repair self-contained. During
// the exact-edit repair stage the kernel intentionally exposes only edit, so a
// small model must not need a separate read_file call just to recover the
// current bytes around a compiler-reported line. The excerpts also survive
// history compaction as part of the durable build diagnostic.
func appendTypeScriptSourceExcerpts(diagnostic string, source []byte) string {
	if strings.TrimSpace(diagnostic) == "" || len(source) == 0 {
		return diagnostic
	}
	lines := strings.Split(string(source), "\n")
	seen := make(map[int]struct{})
	locations := typeScriptDiagnosticLocation.FindAllStringSubmatch(diagnostic, -1)
	var excerpts []string
	for _, location := range locations {
		if len(location) < 2 {
			continue
		}
		lineNumber := 0
		for _, ch := range location[1] {
			lineNumber = lineNumber*10 + int(ch-'0')
		}
		if lineNumber < 1 || lineNumber > len(lines) {
			continue
		}
		if _, exists := seen[lineNumber]; exists {
			continue
		}
		seen[lineNumber] = struct{}{}
		start := lineNumber - 1
		if start > 0 {
			start--
		}
		end := lineNumber - 1
		if end+1 < len(lines) {
			end++
		}
		for index := start; index <= end; index++ {
			marker := " "
			if index+1 == lineNumber {
				marker = ">"
			}
			excerpts = append(excerpts, fmt.Sprintf("%s%4d | %s", marker, index+1, lines[index]))
		}
	}
	hints := typeScriptRepairHints(diagnostic, lines)
	if len(excerpts) == 0 {
		if hints == "" {
			return diagnostic
		}
		return diagnostic + "\n\n" + hints
	}
	result := diagnostic + "\n\nExact saved source excerpts (copy the target line exactly into edit.old_string):\n" + strings.Join(excerpts, "\n")
	if hints != "" {
		result += "\n\n" + hints
	}
	return result
}

func typeScriptRepairHints(diagnostic string, lines []string) string {
	var hints []string
	if value := duplicateTypeScriptFunctionHints(diagnostic, lines); value != "" {
		hints = append(hints, value)
	}
	if value := missingTypeScriptPropertyHints(diagnostic, lines); value != "" {
		hints = append(hints, value)
	}
	if value := nullableTypeScriptValueHints(diagnostic); value != "" {
		hints = append(hints, value)
	}
	if value := implicitSelfReferenceHints(diagnostic); value != "" {
		hints = append(hints, value)
	}
	if value := namespaceExportHints(diagnostic, lines); value != "" {
		hints = append(hints, value)
	}
	return strings.Join(hints, "\n\n")
}

var (
	typeScriptNullableValue = regexp.MustCompile(`error TS18047: '([^']+)' is possibly 'null'`)
	typeScriptImplicitAny   = regexp.MustCompile(`error TS7022: '([^']+)' implicitly has type 'any'`)
)

func nullableTypeScriptValueHints(diagnostic string) string {
	seen := make(map[string]bool)
	var names []string
	for _, match := range typeScriptNullableValue.FindAllStringSubmatch(diagnostic, -1) {
		if len(match) < 2 || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		names = append(names, match[1])
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return "TS18047 nullable control-flow repair for `" + strings.Join(names, "`, `") + "`: after the existing null guard, bind the narrowed value to a local non-null snapshot and use that snapshot inside callbacks or loops. Example shape: `if (!value) return; const stableValue = value;` then reference only `stableValue` in the nested callback/loop. Do not add fallback objects, duplicate state variables, or keep changing unrelated DOM casts."
}

func implicitSelfReferenceHints(diagnostic string) string {
	seen := make(map[string]bool)
	var names []string
	for _, match := range typeScriptImplicitAny.FindAllStringSubmatch(diagnostic, -1) {
		if len(match) < 2 || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		names = append(names, match[1])
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return "TS7022 inference repair for `" + strings.Join(names, "`, `") + "`: give the declaration an explicit existing domain type (for example `const next: Piece = ...`) and build it from the non-null snapshot created after the guard. Do not introduce another mutable global or an `any` cast."
}

func namespaceExportHints(diagnostic string, lines []string) string {
	if !strings.Contains(diagnostic, "error TS1194: Export declarations are not permitted in a namespace") {
		return ""
	}
	var declarations []string
	for _, location := range typeScriptDiagnosticLocation.FindAllStringSubmatch(diagnostic, -1) {
		if len(location) < 2 || !strings.Contains(location[0], "error TS1194:") {
			continue
		}
		lineNumber := 0
		for _, char := range location[1] {
			lineNumber = lineNumber*10 + int(char-'0')
		}
		if lineNumber < 1 || lineNumber > len(lines) {
			continue
		}
		declarations = append(declarations, fmt.Sprintf("line %d `%s`", lineNumber, strings.TrimSpace(lines[lineNumber-1])))
	}
	detail := ""
	if len(declarations) > 0 {
		detail = " Offending saved declaration: " + strings.Join(declarations, "; ") + "."
	}
	return "TS1194 namespace export repair:" + detail + " An `export { ... }` list is illegal inside a TypeScript namespace. If the page already reaches these functions through DOM listeners, remove only that exact export-declaration line (an empty edit.new_string is valid). If namespace members truly must be public, remove the list and add `export` to each original function declaration instead. Never replace the export list with an identical export list."
}

var (
	typeScriptMissingProperty = regexp.MustCompile(`(?m)^.*?\.tsx?\((\d+),\d+\):\s+error\s+TS2339:\s+Property\s+'([A-Za-z_$][A-Za-z0-9_$]*)'\s+does not exist on type\s+'([^']+)'`)
	typeScriptAssertedType    = regexp.MustCompile(`\bas\s+([A-Za-z_$][A-Za-z0-9_$]*(?:<[^;]+>)?)`)
)

func missingTypeScriptPropertyHints(diagnostic string, lines []string) string {
	var hints []string
	seen := make(map[string]bool)
	for _, match := range typeScriptMissingProperty.FindAllStringSubmatch(diagnostic, -1) {
		if len(match) < 4 || seen[match[2]] {
			continue
		}
		lineNumber := 0
		for _, char := range match[1] {
			lineNumber = lineNumber*10 + int(char-'0')
		}
		if lineNumber < 1 || lineNumber > len(lines) {
			continue
		}
		property := match[2]
		savedLine := strings.TrimSpace(lines[lineNumber-1])
		if !strings.Contains(savedLine, "this."+property) {
			continue
		}
		propertyType := "unknown"
		if asserted := typeScriptAssertedType.FindStringSubmatch(savedLine); len(asserted) >= 2 {
			propertyType = strings.TrimSpace(asserted[1])
		}
		seen[property] = true
		hints = append(hints, fmt.Sprintf(
			"TS2339 class property repair at line %d: `%s` assigns `this.%s`, but class `%s` has no declaration. If this assignment is intended, add exactly one class field near its existing private fields: `private %s: %s;`. Do not keep editing the assignment line or duplicate nearby assignments.",
			lineNumber, savedLine, property, match[3], property, propertyType,
		))
	}
	return strings.Join(hints, "\n")
}

var typeScriptFunctionDeclaration = regexp.MustCompile(`\bfunction\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)

func duplicateTypeScriptFunctionHints(diagnostic string, lines []string) string {
	if !strings.Contains(diagnostic, "error TS2393:") || len(lines) == 0 {
		return ""
	}
	type declaration struct {
		line     int
		text     string
		exported bool
		params   string
	}
	byName := make(map[string][]declaration)
	usedNames := make(map[string]bool)
	for _, line := range lines {
		if match := typeScriptFunctionDeclaration.FindStringSubmatch(line); len(match) >= 2 {
			usedNames[match[1]] = true
		}
	}
	for _, location := range typeScriptDiagnosticLocation.FindAllStringSubmatch(diagnostic, -1) {
		if len(location) < 2 || !strings.Contains(location[0], "error TS2393:") {
			continue
		}
		lineNumber := 0
		for _, ch := range location[1] {
			lineNumber = lineNumber*10 + int(ch-'0')
		}
		if lineNumber < 1 || lineNumber > len(lines) {
			continue
		}
		text := strings.TrimSpace(lines[lineNumber-1])
		match := typeScriptFunctionDeclaration.FindStringSubmatch(text)
		if len(match) < 2 {
			continue
		}
		params := ""
		if open := strings.Index(text, "("); open >= 0 {
			if close := strings.Index(text[open+1:], ")"); close >= 0 {
				params = strings.TrimSpace(text[open+1 : open+1+close])
			}
		}
		byName[match[1]] = append(byName[match[1]], declaration{
			line: lineNumber, text: text, params: params,
			exported: strings.Contains(text[:strings.Index(text, "function")], "export"),
		})
	}
	var names []string
	for name, declarations := range byName {
		if len(declarations) > 1 {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var hints []string
	hints = append(hints, "TS2393 duplicate implementation repair map (do not replace a whole function block):")
	for _, name := range names {
		declarations := byName[name]
		parts := make([]string, 0, len(declarations))
		for _, item := range declarations {
			parts = append(parts, fmt.Sprintf("line %d `%s`", item.line, item.text))
		}
		hints = append(hints, fmt.Sprintf("- `%s`: %s", name, strings.Join(parts, "; ")))

		candidateIndex := -1
		for index, item := range declarations {
			if item.exported {
				continue
			}
			if candidateIndex < 0 || (item.params != "" && declarations[candidateIndex].params == "") {
				candidateIndex = index
			}
		}
		if candidateIndex >= 0 {
			candidate := name + "Internal"
			for suffix := 2; usedNames[candidate]; suffix++ {
				candidate = fmt.Sprintf("%sInternal%d", name, suffix)
			}
			usedNames[candidate] = true
			item := declarations[candidateIndex]
			suggestedLine := strings.Replace(item.text, "function "+name+"(", "function "+candidate+"(", 1)
			hints = append(hints, fmt.Sprintf(
				"  Suggested first one-line edit at line %d: old `%s` -> new `%s`.",
				item.line, item.text, suggestedLine,
			))
			if callers := typeScriptFunctionCallSites(lines, name, item.line); len(callers) > 0 {
				hints = append(hints, "  Then update only calls intended for that helper: "+strings.Join(callers, "; ")+".")
			}
		} else {
			hints = append(hints, "  All conflicting declarations are exported wrappers. Remove only one duplicate wrapper using a 2-3 line surrounding-context edit; never replace_all both copies.")
		}
	}
	hints = append(hints, "Apply one suggested collision-free declaration edit, update its matching callers, then rebuild. Keep the public button/API wrapper name stable when possible; do not submit an identical replacement or rename a helper to another already-listed function name.")
	return strings.Join(hints, "\n")
}

func typeScriptFunctionCallSites(lines []string, name string, declarationLine int) []string {
	callPattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\(`)
	var result []string
	for index, line := range lines {
		if index+1 == declarationLine || !callPattern.MatchString(line) {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		result = append(result, fmt.Sprintf("line %d `%s`", index+1, trimmed))
		if len(result) >= 6 {
			break
		}
	}
	return result
}

func (t *TypeScriptWebBuild) recovery(code, message, path string, args map[string]any) Result {
	return errResult(&RecoveryError{
		Code: code, Message: message, Path: path,
		RequiredAction: "preserve_saved_draft_edit_typescript_and_retry_typescript_web_build",
		RequiredArgs: map[string]any{
			"html_path": strArg(args, "html_path"), "source_path": strArg(args, "source_path"),
		},
		PreserveDraft: true,
	})
}

func (t *TypeScriptWebBuild) compiler(sourcePath string) (string, error) {
	if explicit := strings.TrimSpace(t.CompilerPath); explicit != "" {
		if info, err := os.Stat(explicit); err == nil && !info.IsDir() {
			return explicit, nil
		}
		return "", fmt.Errorf("configured TypeScript compiler is unavailable: %s", explicit)
	}
	if explicit := strings.TrimSpace(os.Getenv("MEADOW_TYPESCRIPT_COMPILER")); explicit != "" {
		if info, err := os.Stat(explicit); err == nil && !info.IsDir() {
			return explicit, nil
		}
	}
	// Session artifacts live outside the selected project. Check the project
	// workspace explicitly before walking upward from the session source path,
	// otherwise CLI HG cannot see a project-local TypeScript installation.
	if t.WS != nil && strings.TrimSpace(t.WS.Root) != "" {
		candidate := filepath.Join(t.WS.Root, "node_modules", "typescript", "lib", "tsc.js")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	for dir := filepath.Dir(sourcePath); ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "node_modules", "typescript", "lib", "tsc.js")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	if executable, err := os.Executable(); err == nil {
		candidate := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "Resources", "typescript", "lib", "tsc.js"))
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	if candidate, err := exec.LookPath("tsc"); err == nil {
		return candidate, nil
	}
	return "", fmt.Errorf("TypeScript compiler not found; install the Meadow TypeScript runtime or set MEADOW_TYPESCRIPT_COMPILER")
}

func typeScriptCompilerCommand(compiler, sourcePath, outputDir string) (string, []string, error) {
	executable, _ := os.Executable()
	return typeScriptCompilerCommandForExecutable(executable, compiler, sourcePath, outputDir)
}

func typeScriptCompilerCommandForExecutable(executable, compiler, sourcePath, outputDir string) (string, []string, error) {
	args := []string{
		"--pretty", "false", "--strict", "--skipLibCheck", "--noEmitOnError",
		"--target", "ES2022", "--module", "ES2022", "--moduleResolution", "bundler",
		"--lib", "DOM,ES2022", "--outDir", outputDir, sourcePath,
	}
	if strings.EqualFold(filepath.Ext(compiler), ".js") {
		node, err := typeScriptNodeExecutable(executable)
		if err != nil {
			return "", nil, fmt.Errorf("node is required to run the bundled TypeScript compiler")
		}
		return node, append([]string{compiler}, args...), nil
	}
	return compiler, args, nil
}

// Resolve the runtime from the running application's resources, never from a
// project-controlled compiler path. Portable Desktop must compile without a
// separately installed Node or a shell-specific PATH.
func typeScriptNodeExecutable(executable string) (string, error) {
	if filepath.IsAbs(executable) {
		bundled := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "Resources", "source-diagnostics", "node"))
		info, err := os.Lstat(bundled)
		if err == nil {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				return "", fmt.Errorf("bundled Node runtime is not executable")
			}
			return bundled, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("bundled Node runtime is unavailable")
		}
	}
	return exec.LookPath("node")
}

func htmlLoadsScript(html []byte, jsReference string) bool {
	for _, match := range regexp.MustCompile(`(?is)<script\b[^>]*\bsrc\s*=\s*["']([^"']+)["'][^>]*>`).FindAllSubmatch(html, -1) {
		if len(match) < 2 {
			continue
		}
		src := strings.TrimSpace(string(match[1]))
		if query := strings.IndexByte(src, '?'); query >= 0 {
			src = src[:query]
		}
		// /session and /workspace are model-facing filesystem aliases, not URL
		// roots inside a file:// page. Requiring a sibling-relative reference
		// catches the common false pass where compilation succeeds but the
		// browser silently 404s the application script.
		if strings.HasPrefix(src, "/") || strings.Contains(src, "://") {
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(src))
		if clean == filepath.Clean(filepath.FromSlash(jsReference)) {
			return true
		}
	}
	return false
}

func htmlLoadsScriptAsModule(html []byte, jsReference string) bool {
	for _, match := range regexp.MustCompile(`(?is)<script\b([^>]*)\bsrc\s*=\s*["']([^"']+)["']([^>]*)>`).FindAllSubmatch(html, -1) {
		if len(match) < 4 {
			continue
		}
		src := strings.TrimSpace(string(match[2]))
		if query := strings.IndexByte(src, '?'); query >= 0 {
			src = src[:query]
		}
		if strings.HasPrefix(src, "/") || strings.Contains(src, "://") {
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(src))
		if clean != filepath.Clean(filepath.FromSlash(jsReference)) {
			continue
		}
		attributes := strings.ToLower(string(match[1]) + " " + string(match[3]))
		if regexp.MustCompile(`\btype\s*=\s*["']module["']`).MatchString(attributes) {
			return true
		}
	}
	return false
}

var javaScriptModuleSyntax = regexp.MustCompile(`(?m)^\s*(?:export(?:\s|\{|\*)|import(?:\s|\{|\*|["']))`)

func javascriptRequiresModule(compiled []byte) bool {
	return javaScriptModuleSyntax.Match(compiled)
}

func hasSubstantialInlineScript(html []byte) bool {
	for _, match := range regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllSubmatch(html, -1) {
		if len(match) < 3 || bytes.Contains(bytes.ToLower(match[1]), []byte("src=")) {
			continue
		}
		body := strings.TrimSpace(string(match[2]))
		if len(body) > 120 && !strings.HasPrefix(body, "{") {
			return true
		}
	}
	return false
}

var (
	htmlIDAttribute  = regexp.MustCompile(`(?is)\bid\s*=\s*["']([^"']+)["']`)
	tsGetElementByID = regexp.MustCompile(`(?m)document\.getElementById\(\s*["']([^"']+)["']\s*\)`)
	tsPropertyLookup = regexp.MustCompile(`this\.([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*document\.getElementById`)
)

// missingStaticDOMIDReferences catches a common runtime-only failure that the
// TypeScript compiler cannot see: a full-file repair renames literal DOM IDs in
// the .ts source while leaving the already-rendered HTML unchanged. Static web
// artifacts use a saved sibling HTML shell, so every literal getElementById
// reference must be backed by that shell before build/visual evidence can pass.
func missingStaticDOMIDReferences(html, source []byte) []string {
	available := make(map[string]bool)
	var availableList []string
	for _, match := range htmlIDAttribute.FindAllSubmatch(html, -1) {
		if len(match) < 2 {
			continue
		}
		id := strings.TrimSpace(string(match[1]))
		if id != "" && !available[id] {
			available[id] = true
			availableList = append(availableList, id)
		}
	}
	seen := make(map[string]bool)
	var missing []string
	for _, match := range tsGetElementByID.FindAllSubmatchIndex(source, -1) {
		if len(match) < 4 {
			continue
		}
		id := strings.TrimSpace(string(source[match[2]:match[3]]))
		if id == "" || available[id] || seen[id] {
			continue
		}
		lineStart := bytes.LastIndex(source[:match[0]], []byte{'\n'}) + 1
		lineEnd := len(source)
		if offset := bytes.IndexByte(source[match[1]:], '\n'); offset >= 0 {
			lineEnd = match[1] + offset
		}
		savedLine := strings.TrimSpace(string(source[lineStart:lineEnd]))
		// A stale constructor lookup that is never read cannot cause the page's
		// behavior to fail. Do not let that dead field block more consequential
		// listener/element mismatches from reaching the compiler and browser.
		if property := tsPropertyLookup.FindStringSubmatch(savedLine); len(property) >= 2 &&
			bytes.Count(source, []byte("this."+property[1])) == 1 {
			continue
		}
		seen[id] = true
		line := 1 + bytes.Count(source[:match[0]], []byte{'\n'})
		diagnostic := fmt.Sprintf("- line %d `%s`: id %q has no matching HTML id", line, savedLine, id)
		if suggestion := closestStaticHTMLID(id, availableList); suggestion != "" {
			diagnostic += fmt.Sprintf("; replace only %q with %q in this saved line", id, suggestion)
		} else {
			diagnostic += "; choose an intended id from the available list or remove this lookup only if it is truly unused"
		}
		missing = append(missing, diagnostic)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(availableList)
	if len(availableList) > 24 {
		availableList = append(availableList[:24], "…")
	}
	missing = append(missing, "Available HTML ids: "+strings.Join(availableList, ", "))
	return missing
}

func closestStaticHTMLID(missing string, available []string) string {
	normalize := func(value string) string {
		value = strings.ToLower(value)
		value = strings.TrimPrefix(value, "tetris-")
		value = strings.TrimPrefix(value, "tetris_")
		var result strings.Builder
		for _, char := range value {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
				result.WriteRune(char)
			}
		}
		return result.String()
	}
	want := normalize(missing)
	if want == "" {
		return ""
	}
	for _, candidate := range available {
		if normalize(candidate) == want {
			return candidate
		}
	}
	var suffixMatch string
	for _, candidate := range available {
		got := normalize(candidate)
		if got != "" && (strings.HasSuffix(got, want) || strings.HasSuffix(want, got)) {
			if suffixMatch != "" {
				return ""
			}
			suffixMatch = candidate
		}
	}
	return suffixMatch
}
