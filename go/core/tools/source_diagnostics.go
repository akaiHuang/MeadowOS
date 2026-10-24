package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/net/html"
)

//go:embed source_diagnostics.cjs
var sourceDiagnosticRunner string

//go:embed source_diagnostics.py
var sourceDiagnosticPythonRunner string

type sourceDiagnosticUnit struct {
	Text      string `json:"text"`
	Extension string `json:"extension"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
}

type sourceDiagnostic struct {
	Code       string `json:"code"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Message    string `json:"message"`
	SourceLine string `json:"source_line,omitempty"`
}

type sourceDiagnosticReport struct {
	Status      string             `json:"status"`
	Path        string             `json:"path"`
	SHA256      string             `json:"sha256"`
	Scope       string             `json:"scope"`
	Parser      string             `json:"parser,omitempty"`
	Diagnostics []sourceDiagnostic `json:"diagnostics"`
	Truncated   bool               `json:"truncated,omitempty"`
	Reason      string             `json:"reason,omitempty"`
}

// This is an advisory attached to a successful mutation, never behavior evidence.
func appendSourceDiagnostics(ctx context.Context, result Result, modelPath, source string) Result {
	ext := strings.ToLower(filepath.Ext(modelPath))
	switch ext {
	case ".html", ".htm", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".py", ".json", ".go":
	default:
		return result
	}
	report := sourceDiagnosticReport{Status: "unavailable", Path: modelPath,
		SHA256:      fmt.Sprintf("%x", sha256.Sum256([]byte(source))),
		Scope:       "TypeScript parser diagnostics and TS2588 only (not complete JS syntax/type checking); compiler suppression directives honored; inline scripts independently; no project/dependency/behavior verification",
		Diagnostics: []sourceDiagnostic{}}
	switch ext {
	case ".html", ".htm":
		report.Scope = "HTML executable script closing tags outside SVG/MathML subtrees only (not complete HTML validation); " + report.Scope
	case ".py":
		report.Scope = "Python ast.parse syntax only for the reported interpreter grammar and source encoding; no project/dependency/name resolution or behavior verification"
	case ".json":
		report.Scope = "JSON syntax and UTF-8 only, exactly one value; no schema, duplicate-key, project/dependency or behavior verification"
	case ".go":
		report.Scope = "Go parser syntax only; no imports loaded, build, type checking, project/dependency or behavior verification"
	}
	if len(source) > 1024*1024 {
		report.Reason = "source exceeds 1 MiB diagnostic limit"
	} else if ctx.Err() != nil {
		report.Reason = "diagnostic request was cancelled; no validation result"
	} else if ext == ".json" || ext == ".go" {
		runNativeSourceDiagnostics(ctx, ext, modelPath, source, &report)
	} else if ext == ".py" {
		runPythonSourceDiagnostics(ctx, source, &report)
	} else if units, err := sourceDiagnosticUnits(ext, source); err != nil {
		report.Reason = err.Error()
	} else if len(units) == 0 {
		report.Status, report.Reason = "not_applicable", "no supported inline script"
	} else {
		runSourceDiagnostics(ctx, units, &report)
	}
	if (ext == ".html" || ext == ".htm") && len(source) <= 1024*1024 && ctx.Err() == nil {
		// Keep this native observation even if the optional JS parser is absent.
		// It does not turn an unavailable JS check into a successful one.
		if diagnostic := sourceDiagnosticUnclosedScript(source); diagnostic != nil {
			if report.Status == "not_applicable" {
				report.Status, report.Reason = "checked", ""
			}
			report.Diagnostics = append([]sourceDiagnostic{*diagnostic}, report.Diagnostics...)
			if len(report.Diagnostics) > 20 {
				report.Diagnostics, report.Truncated = report.Diagnostics[:20], true
			}
			if report.Parser != "" {
				report.Parser = "HTML tokenizer; " + report.Parser
			} else {
				report.Parser = "HTML tokenizer"
			}
		}
	}
	lines := sourceDiagnosticReportLines(ext, source)
	for i := range report.Diagnostics {
		d := &report.Diagnostics[i]
		if d.SourceLine == "" && d.Line > 0 && d.Line <= len(lines) {
			line := []rune(strings.TrimSuffix(lines[d.Line-1], "\r"))
			if len(line) > 300 {
				line = append(line[:300], '…')
			}
			d.SourceLine = string(line)
		}
	}
	encoded, _ := json.Marshal(report)
	result.LLMContent += "\n儲存後來源診斷（檔案已保留；僅檢查所列範圍，沒有診斷不代表功能完成）：\nsource_diagnostics: " + string(encoded)
	if len(report.Diagnostics) > 0 {
		result.Display += fmt.Sprintf("；來源診斷 %d 項", len(report.Diagnostics))
	}
	return result
}

// A JavaScript-valid suffix can still be an unfinished HTML script element and
// therefore never execute. Use HTML raw-text tokenization, not a tag regex or a
// JavaScript scanner: even a literal </script> inside a JS string ends the HTML
// element, while script-looking text in comments/textarea/style is not a tag.
// Foreign-content subtrees are intentionally outside this bounded check; their
// namespace/integration-point rules need a full HTML tree-building context.
func sourceDiagnosticUnclosedScript(source string) *sourceDiagnostic {
	z := html.NewTokenizer(strings.NewReader(source))
	offset, openScript := 0, -1
	var foreign []string
	for {
		kind := z.Next()
		start := offset
		offset += len(z.Raw())
		if kind == html.ErrorToken {
			if z.Err() != io.EOF || openScript < 0 {
				return nil
			}
			prefixLines := sourceDiagnosticLines(source[:openScript])
			return &sourceDiagnostic{Code: "HTML_UNCLOSED_SCRIPT", Line: len(prefixLines),
				Column:  len(utf16.Encode([]rune(prefixLines[len(prefixLines)-1]))) + 1,
				Message: "Executable <script> reaches the end of the HTML file without a closing </script> tag; close the element before validating the delivered page."}
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken && kind != html.EndTagToken {
			continue
		}
		token := z.Token()
		if token.Data == "svg" || token.Data == "math" {
			if kind == html.StartTagToken {
				foreign = append(foreign, token.Data)
			} else if kind == html.EndTagToken && len(foreign) > 0 && foreign[len(foreign)-1] == token.Data {
				foreign = foreign[:len(foreign)-1]
			}
			continue
		}
		if len(foreign) > 0 {
			if kind == html.SelfClosingTagToken {
				z.NextIsNotRawText()
			}
			continue
		}
		if token.Data != "script" {
			continue
		}
		if kind == html.EndTagToken {
			openScript = -1
			continue
		}
		if sourceDiagnosticExecutableHTMLScript(token.Attr) {
			// HTML script elements are not void; a trailing slash does not close
			// them. src scripts also require an end tag even with no inline JS.
			openScript = start
		}
	}
}

func sourceDiagnosticExecutableHTMLScript(attrs []html.Attribute) bool {
	typeValue, language := "", ""
	hasType, hasLanguage := false, false
	for _, attr := range attrs {
		// HTML keeps the first occurrence of a duplicate attribute.
		if attr.Key == "type" && !hasType {
			typeValue, hasType = attr.Val, true
		} else if attr.Key == "language" && !hasLanguage {
			language, hasLanguage = attr.Val, true
		}
	}
	if (hasType && typeValue == "") || (!hasType && language == "") {
		return true
	}
	if hasType {
		typeValue = strings.Trim(typeValue, " \t\r\n\f")
	} else {
		typeValue = "text/" + language
	}
	// The HTML type attribute uses an exact string comparison, not MIME
	// parsing: "text/javascript;example" is a data block, not JavaScript.
	switch strings.ToLower(typeValue) {
	case "module", "application/ecmascript", "application/javascript", "application/x-ecmascript", "application/x-javascript",
		"text/ecmascript", "text/javascript", "text/javascript1.0", "text/javascript1.1", "text/javascript1.2", "text/javascript1.3",
		"text/javascript1.4", "text/javascript1.5", "text/jscript", "text/livescript", "text/x-ecmascript", "text/x-javascript":
		return true
	default:
		return false
	}
}

func sourceDiagnosticUnits(ext, source string) ([]sourceDiagnosticUnit, error) {
	if ext != ".html" && ext != ".htm" {
		return []sourceDiagnosticUnit{{Text: source, Extension: ext, Line: 1, Column: 1}}, nil
	}
	z := html.NewTokenizer(strings.NewReader(source))
	var units []sourceDiagnosticUnit
	offset, bodyStart := 0, -1
	for {
		kind := z.Next()
		raw := z.Raw()
		start := offset
		offset += len(raw)
		if kind == html.ErrorToken {
			if z.Err() != io.EOF {
				return nil, z.Err()
			}
			break
		}
		if kind == html.StartTagToken {
			token := z.Token()
			if token.Data == "script" {
				allowed := true
				for _, attr := range token.Attr {
					if attr.Key == "src" {
						allowed = false
					}
					if attr.Key == "type" {
						mime := strings.TrimSpace(strings.ToLower(strings.Split(attr.Val, ";")[0]))
						if mime != "" && mime != "module" && mime != "text/javascript" && mime != "application/javascript" {
							allowed = false
						}
					}
				}
				if allowed {
					bodyStart = offset
				} else {
					bodyStart = -1
				}
			}
		}
		if kind == html.TextToken && bodyStart == start {
			if len(units) >= 32 {
				return nil, fmt.Errorf("inline script count exceeds 32")
			}
			prefix := source[:start]
			prefixLines := sourceDiagnosticLines(prefix)
			units = append(units, sourceDiagnosticUnit{Text: string(raw), Extension: ".js",
				Line:   len(prefixLines),
				Column: len(utf16.Encode([]rune(prefixLines[len(prefixLines)-1]))) + 1})
			bodyStart = -1
		}
	}
	return units, nil
}

// Match the parser's line separators without modifying the saved source bytes.
func sourceDiagnosticLines(source string) []string {
	return strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n").Replace(source), "\n")
}

func sourceDiagnosticReportLines(ext, source string) []string {
	switch ext {
	case ".go", ".json":
		return strings.Split(source, "\n")
	case ".py":
		return strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(source), "\n")
	default:
		return sourceDiagnosticLines(source)
	}
}

func sourceDiagnosticRuntime() (string, string, error) {
	node, compiler := os.Getenv("MEADOW_SOURCE_DIAGNOSTICS_NODE"), os.Getenv("MEADOW_SOURCE_DIAGNOSTICS_TYPESCRIPT")
	if node == "" && compiler == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", "", err
		}
		resources := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "Resources"))
		node = filepath.Join(resources, "source-diagnostics", "node")
		compiler = filepath.Join(resources, "source-diagnostics", "typescript.cjs")
	}
	for _, path := range []string{node, compiler} {
		if !filepath.IsAbs(path) {
			return "", "", fmt.Errorf("trusted diagnostic runtime is not configured")
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("trusted diagnostic runtime is unavailable")
		}
	}
	return node, compiler, nil
}

type sourceDiagnosticOutput struct{ bytes.Buffer }

func (b *sourceDiagnosticOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 48*1024 {
		return 0, fmt.Errorf("diagnostic output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func runSourceDiagnostics(ctx context.Context, units []sourceDiagnosticUnit, report *sourceDiagnosticReport) {
	node, compiler, err := sourceDiagnosticRuntime()
	if err != nil {
		report.Reason = err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"units": units})
	cmd := exec.CommandContext(ctx, node, "--max-old-space-size=128", "-e", sourceDiagnosticRunner, compiler)
	cmd.Dir = filepath.Dir(compiler)
	// Do not inherit NODE_OPTIONS/NODE_PATH/DYLD injection or project settings.
	cmd.Env = []string{"LANG=C.UTF-8"}
	cmd.Stdin = bytes.NewReader(payload)
	var output, stderr sourceDiagnosticOutput
	cmd.Stdout, cmd.Stderr = &output, &stderr
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		report.Reason = "diagnostic runtime failed or was cancelled; no validation result"
		return
	}
	var observed sourceDiagnosticReport
	if err := json.Unmarshal(output.Bytes(), &observed); err != nil || (observed.Status != "checked" && observed.Status != "checked_with_suppressions") || len(observed.Diagnostics) > 20 {
		report.Reason = "invalid diagnostic runtime response"
		return
	}
	report.Status, report.Diagnostics, report.Truncated, report.Parser = observed.Status, observed.Diagnostics, observed.Truncated, observed.Parser
}
