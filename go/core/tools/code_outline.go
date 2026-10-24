package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const codeOutlineMaxBytes = 1024 * 1024

//go:embed code_outline.cjs
var codeOutlineRunner string

// CodeOutline reads one current source snapshot. It deliberately does not mark
// the whole file as read: declarations are not the source text needed to edit.
type CodeOutline struct{ WS *Workspace }

func (t *CodeOutline) Name() string { return "code_outline" }
func (t *CodeOutline) Description() string {
	return "Read current JS/TS/JSX/TSX, inline HTML JavaScript, or Go declarations and lexical member locations without reading whole source into context. Returns source SHA256, parser errors and bounded pagination. No resolved references, call graph, project types or behavior verification. Use read_file on the relevant lines before editing."
}
func (t *CodeOutline) Kind() Kind                       { return KindReadOnly }
func (t *CodeOutline) ParallelSafe(map[string]any) bool { return true }
func (t *CodeOutline) SubtaskSafe() bool                { return true }
func (t *CodeOutline) CompleteObservation() bool        { return true }
func (t *CodeOutline) ToolWorkspace() *Workspace        { return t.WS }
func (t *CodeOutline) ForWorkspace(ws *Workspace) Tool  { return &CodeOutline{WS: ws} }

func (t *CodeOutline) Schema() map[string]any {
	integer := func(description string, minimum int) map[string]any {
		return map[string]any{"type": "integer", "description": description, "minimum": minimum}
	}
	limit := integer("Maximum declarations; default 60, maximum 120.", 1)
	limit["maximum"] = 120
	return obj(map[string]any{
		"file_path":  str("Path to one current source file; /workspace and /session aliases are supported."),
		"start_line": integer("Optional inclusive one-based line-window start; default 1.", 1),
		"end_line":   integer("Optional inclusive line-window end; default end of file. Declarations overlapping the window are included.", 1),
		"offset":     integer("Optional zero-based declaration offset within the line window; use next_offset to continue.", 0),
		"limit":      limit,
	}, "file_path")
}

type codeOutlineWindow struct {
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
	Offset    int `json:"offset"`
	Limit     int `json:"limit"`
}

type codeOutlineSymbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Parent    string `json:"parent,omitempty"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	EndLine   int    `json:"end_line"`
	EndColumn int    `json:"end_column"`
}

type codeOutlineReport struct {
	Status         string              `json:"status"`
	Path           string              `json:"path"`
	SHA256         string              `json:"sha256"`
	Bytes          int                 `json:"bytes"`
	Language       string              `json:"language"`
	ColumnEncoding string              `json:"column_encoding"`
	Scope          string              `json:"scope"`
	Window         codeOutlineWindow   `json:"window"`
	Symbols        []codeOutlineSymbol `json:"symbols"`
	Diagnostics    []sourceDiagnostic  `json:"diagnostics"`
	Truncated      bool                `json:"truncated,omitempty"`
	NextOffset     *int                `json:"next_offset,omitempty"`
	Reason         string              `json:"reason,omitempty"`
}

func codeOutlineInteger(args map[string]any, key string, fallback, minimum, maximum int) (int, error) {
	value, present := args[key]
	if !present {
		return fallback, nil
	}
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case int:
		number = float64(v)
	case json.Number:
		parsed, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		number = parsed
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || number < float64(minimum) || number > float64(maximum) {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", key, minimum, maximum)
	}
	return int(number), nil
}

func (t *CodeOutline) Execute(ctx context.Context, args map[string]any) Result {
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	if t == nil || t.WS == nil {
		return errResultf("workspace is not configured")
	}
	path, err := (&ReadFile{WS: t.WS}).resolvePath(pathArg(args))
	if err != nil {
		return errResult(err)
	}
	ext := strings.ToLower(filepath.Ext(path))
	language := "javascript"
	switch ext {
	case ".js", ".mjs", ".cjs", ".jsx":
	case ".ts", ".tsx":
		language = "typescript"
	case ".html", ".htm":
		language = "html_inline_javascript"
	case ".go":
		language = "go"
	default:
		return errResultf("unsupported source extension %q; supported: JS/TS/JSX/TSX, inline HTML JavaScript, Go", ext)
	}
	root, _, declared := t.WS.narrowestNamespace(path, true)
	if !declared {
		return errResultf("source path is outside authorized roots")
	}
	rootDir, err := os.OpenRoot(root)
	if err != nil {
		return errResult(err)
	}
	defer rootDir.Close()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return errResult(err)
	}
	before, err := rootDir.Stat(rel)
	if err != nil {
		return errResult(err)
	}
	if !before.Mode().IsRegular() {
		return errResultf("code_outline requires one regular source file")
	}
	// Root-relative open preserves confinement during symlink replacement;
	// nonblocking mode prevents regular-file-to-FIFO swaps from stalling.
	file, err := rootDir.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errResult(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errResultf("code_outline requires one regular source file")
	}
	if info.Size() > codeOutlineMaxBytes {
		return errResultf("source exceeds the 1 MiB outline limit; use a smaller source file or read_file/search for specific regions")
	}
	source, err := io.ReadAll(io.LimitReader(file, codeOutlineMaxBytes+1))
	if err != nil {
		return errResult(err)
	}
	if len(source) > codeOutlineMaxBytes {
		return errResultf("source exceeds the 1 MiB outline limit")
	}
	if !utf8.Valid(source) {
		return errResultf("code_outline requires UTF-8 source")
	}
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	lines := len(sourceDiagnosticLines(string(source)))
	if language == "go" {
		lines = bytes.Count(source, []byte{'\n'}) + 1
	}
	window := codeOutlineWindow{}
	if window.StartLine, err = codeOutlineInteger(args, "start_line", 1, 1, lines); err != nil {
		return errResult(err)
	}
	if window.EndLine, err = codeOutlineInteger(args, "end_line", lines, window.StartLine, lines); err != nil {
		return errResult(err)
	}
	if window.Offset, err = codeOutlineInteger(args, "offset", 0, 0, codeOutlineMaxBytes); err != nil {
		return errResult(err)
	}
	if window.Limit, err = codeOutlineInteger(args, "limit", 60, 1, 120); err != nil {
		return errResult(err)
	}
	report := codeOutlineReport{Status: "unavailable", Path: t.WS.ModelPath(path), SHA256: fmt.Sprintf("%x", sha256.Sum256(source)),
		Bytes: len(source), Language: language, ColumnEncoding: "utf16", Window: window,
		Scope:   "Current-file lexical declarations and members, exclusive end positions; imports and function-local data variables omitted; no resolved references/call graph/project types/execution; read exact source lines before editing",
		Symbols: []codeOutlineSymbol{}, Diagnostics: []sourceDiagnostic{}}
	if language == "go" {
		report.ColumnEncoding = "bytes"
		report.Scope = "Current-file Go top-level declarations and struct/interface members, exclusive end positions; no resolved references/call graph/project types/execution; read exact source lines before editing"
		outlineGo(ctx, path, source, &report)
	} else {
		units, err := sourceDiagnosticUnits(ext, string(source))
		if err != nil {
			report.Reason = err.Error()
		} else if len(units) == 0 {
			report.Status, report.Reason = "not_applicable", "no supported inline JavaScript; external scripts and non-JavaScript script types are excluded"
		} else {
			outlineJavaScript(ctx, units, &report)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return errResult(err)
	}
	result := Result{LLMContent: string(encoded), Display: fmt.Sprintf("%s: %d declarations in %s", report.Status, len(report.Symbols), report.Path)}
	if report.Status == "unavailable" {
		result.Err = fmt.Errorf("code_outline unavailable: %s", report.Reason)
	}
	return result
}

type codeOutlineOutput struct{ bytes.Buffer }

func (b *codeOutlineOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 80*1024 {
		return 0, fmt.Errorf("outline output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func outlineJavaScript(ctx context.Context, units []sourceDiagnosticUnit, report *codeOutlineReport) {
	node, compiler, err := sourceDiagnosticRuntime()
	if err != nil {
		report.Reason = err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"units": units, "window": report.Window})
	cmd := exec.CommandContext(ctx, node, "--max-old-space-size=128", "-e", codeOutlineRunner, compiler)
	cmd.Dir, cmd.Env, cmd.Stdin = filepath.Dir(compiler), []string{"LANG=C.UTF-8"}, bytes.NewReader(payload)
	var output, stderr codeOutlineOutput
	cmd.Stdout, cmd.Stderr = &output, &stderr
	if err := cmd.Run(); err != nil {
		report.Reason = "outline runtime failed or was cancelled; no parser result"
		return
	}
	var observed codeOutlineReport
	if err := json.Unmarshal(output.Bytes(), &observed); err != nil ||
		(observed.Status != "parsed" && observed.Status != "parsed_with_errors") || len(observed.Symbols) > report.Window.Limit || len(observed.Diagnostics) > 20 {
		report.Reason = "invalid outline runtime response"
		return
	}
	report.Status, report.Symbols, report.Diagnostics, report.Truncated, report.NextOffset = observed.Status, observed.Symbols, observed.Diagnostics, observed.Truncated, observed.NextOffset
}

func codeOutlineText(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	return validUTF8Prefix(text, maxBytes-3) + "…"
}

func outlineGo(ctx context.Context, path string, source []byte, report *codeOutlineReport) {
	fset := token.NewFileSet()
	file, parseErr := parser.ParseFile(fset, path, source, parser.AllErrors|parser.SkipObjectResolution)
	var physicalFile *token.File
	fset.Iterate(func(file *token.File) bool { physicalFile = file; return false })
	if err := ctx.Err(); err != nil {
		report.Reason = "outline cancelled; no parser result"
		return
	}
	report.Status = "parsed"
	if parseErr != nil {
		report.Status = "parsed_with_errors"
		if errors, ok := parseErr.(scanner.ErrorList); ok {
			for _, e := range errors {
				if len(report.Diagnostics) >= 20 {
					report.Truncated = true
					break
				}
				position := e.Pos
				if physicalFile != nil && position.Offset >= 0 && position.Offset <= physicalFile.Size() {
					position = fset.PositionFor(physicalFile.Pos(position.Offset), false)
				}
				report.Diagnostics = append(report.Diagnostics, sourceDiagnostic{Code: "GO_PARSE", Line: position.Line, Column: position.Column, Message: codeOutlineText(e.Msg, 300)})
			}
		} else {
			report.Diagnostics = append(report.Diagnostics, sourceDiagnostic{Code: "GO_PARSE", Message: codeOutlineText(parseErr.Error(), 300)})
		}
	}
	if file == nil {
		return
	}
	matched := 0
	emit := func(name, kind, parent string, node ast.Node) {
		start, end := fset.PositionFor(node.Pos(), false), fset.PositionFor(node.End(), false)
		if start.Line > report.Window.EndLine || end.Line < report.Window.StartLine {
			return
		}
		matched++
		if matched <= report.Window.Offset {
			return
		}
		if len(report.Symbols) == report.Window.Limit {
			report.Truncated = true
			next := report.Window.Offset + len(report.Symbols)
			report.NextOffset = &next
			return
		}
		report.Symbols = append(report.Symbols, codeOutlineSymbol{Name: codeOutlineText(name, 100), Kind: kind, Parent: codeOutlineText(parent, 160), Line: start.Line, Column: start.Column, EndLine: end.Line, EndColumn: end.Column})
	}
	for _, declaration := range file.Decls {
		switch d := declaration.(type) {
		case *ast.FuncDecl:
			kind, parent := "function", ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				var buf bytes.Buffer
				_ = format.Node(&buf, fset, d.Recv.List[0].Type)
				parent = buf.String()
			}
			emit(d.Name.Name, kind, parent, d)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					emit(s.Name.Name, "type", "", s)
					var fields *ast.FieldList
					if st, ok := s.Type.(*ast.StructType); ok {
						fields = st.Fields
					} else if it, ok := s.Type.(*ast.InterfaceType); ok {
						fields = it.Methods
					}
					if fields != nil {
						for _, field := range fields.List {
							kind := "field"
							if _, ok := field.Type.(*ast.FuncType); ok {
								kind = "method_signature"
							}
							for _, name := range field.Names {
								emit(name.Name, kind, s.Name.Name, field)
							}
							if len(field.Names) == 0 {
								var buf bytes.Buffer
								_ = format.Node(&buf, fset, field.Type)
								name := buf.String()
								if len(name) > 100 {
									name = codeOutlineText(name, 100)
								}
								emit(name, "embedded_type", s.Name.Name, field)
							}
						}
					}
				case *ast.ValueSpec:
					for _, name := range s.Names {
						emit(name.Name, d.Tok.String(), "", s)
					}
				}
			}
		}
	}
}
