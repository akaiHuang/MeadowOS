package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 自我修復循環的語法閘門：寫檔/編輯後對程式檔做一次便宜的語法檢查，
// 把錯誤回饋給模型讓它 edit 修。補本地模型「單次寫不對」的不足——
// 用戶最早踩的 snake_game.html SyntaxError（didGameOver 被寫成含中文
// 字元的亂碼）就是這層攔截的目標。
//
// best-effort：缺工具鏈（node/python 未裝）回 ""（不誤報）；檔案不存在
// （寫入失敗）也回 ""，交給工具本身的錯誤處理。

var (
	inlineScriptRe = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	srcAttrRe      = regexp.MustCompile(`(?i)\bsrc\s*=`)
	threeRefRe     = regexp.MustCompile(`\bTHREE\s*\.`)
	threeImportRe  = regexp.MustCompile(`(?is)<script\b[^>]*\bsrc\s*=\s*["'][^"']*three[^"']*["'][^>]*>|import\s+\*\s+as\s+THREE\s+from\s+["'][^"']*three[^"']*["']|from\s+["'][^"']*three[^"']*["']|\b(?:const|let|var)\s+THREE\b|window\.THREE\b`)
	badThreeCdnRe  = regexp.MustCompile(`(?i)(?:https?:)?//cdnjs\.cloudflare\.com/ajax/libs/three\.js/r([0-9]+)/build/(three(?:\.min)?\.js)`)
	threeCameraRe  = regexp.MustCompile(`\bnew\s+THREE\.PerspectiveCamera\s*\(`)
	cameraPosSetRe = regexp.MustCompile(`\bcamera\.position\.set\s*\([^;]*\);?`)
	fogExp2Re      = regexp.MustCompile(`new\s+THREE\.FogExp2\s*\(\s*([^,]+?)\s*,\s*([0-9]+(?:\.[0-9]+)?)\s*\)`)
)

// ValidateSyntax runs a parse-only syntax check on a freshly written code
// file and returns a short error message, or "" when it parses (or has no
// validator for that type).
func ValidateSyntax(path string) string {
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return ""
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs", ".cjs":
		return nodeCheck(path)
	case ".html", ".htm":
		if err := htmlInlineJSCheck(path); err != "" {
			return err
		}
		return htmlRuntimeDependencyCheck(path)
	case ".py":
		return pyCheck(path)
	case ".json":
		return jsonCheck(path)
	}
	return ""
}

func htmlRuntimeDependencyCheck(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	html := string(b)
	if badThreeCdnRe.MatchString(html) {
		return "HTML runtime dependency broken: known bad Three.js CDN URL uses /build/ under cdnjs; replace it with a jsDelivr three@0.x.0 build URL"
	}
	if threeRefRe.MatchString(html) && !threeImportRe.MatchString(html) {
		return "HTML runtime dependency missing: THREE is referenced but no Three.js import/script is loaded"
	}
	if hasBadFogExp2Density(html) {
		return "HTML runtime visibility issue: THREE.FogExp2 density is too high and can hide the entire scene; use a small density such as 0.006"
	}
	if threeCameraRe.MatchString(html) && cameraPosSetRe.MatchString(html) &&
		!strings.Contains(html, "camera.lookAt(") &&
		!strings.Contains(html, "OrbitControls") {
		return "HTML runtime camera issue: Three.js camera is positioned but never aimed at the scene; add camera.lookAt(0, 0, 0) or OrbitControls before rendering"
	}
	return ""
}

func hasBadFogExp2Density(html string) bool {
	for _, m := range fogExp2Re.FindAllStringSubmatch(html, -1) {
		if len(m) < 3 {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err == nil && v >= 1 {
			return true
		}
	}
	return false
}

// nodeCheck runs `node --check` (parse only — undefined browser globals
// like document/window are runtime, not syntax, so they never false-alarm).
func nodeCheck(path string) string {
	if _, err := exec.LookPath("node"); err != nil {
		return ""
	}
	out, err := exec.Command("node", "--check", path).CombinedOutput()
	if err == nil {
		return ""
	}
	return cleanSyntaxError(string(out))
}

// htmlInlineJSCheck extracts inline <script> bodies (skipping <script src=>),
// concatenates them, and node --checks them as one script.
func htmlInlineJSCheck(path string) string {
	if _, err := exec.LookPath("node"); err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var js strings.Builder
	for _, m := range inlineScriptRe.FindAllStringSubmatch(string(b), -1) {
		if srcAttrRe.MatchString(m[1]) { // external script — skip
			continue
		}
		js.WriteString(m[2])
		js.WriteString("\n")
	}
	code := strings.TrimSpace(js.String())
	if code == "" {
		return ""
	}
	tmp, err := os.CreateTemp("", "meadow-inline-*.js")
	if err != nil {
		return ""
	}
	defer os.Remove(tmp.Name())
	_, _ = tmp.WriteString(code)
	_ = tmp.Close()
	out, err := exec.Command("node", "--check", tmp.Name()).CombinedOutput()
	if err == nil {
		return ""
	}
	return "inline <script> 語法錯誤：" + cleanSyntaxError(string(out))
}

func pyCheck(path string) string {
	bin := "python3"
	if _, err := exec.LookPath(bin); err != nil {
		if _, err := exec.LookPath("python"); err != nil {
			return ""
		}
		bin = "python"
	}
	out, err := exec.Command(bin, "-m", "py_compile", path).CombinedOutput()
	if err == nil {
		return ""
	}
	return cleanSyntaxError(string(out))
}

func jsonCheck(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "JSON 解析失敗：" + err.Error()
	}
	return ""
}

// foreignTokenRe pulls a quoted token out of a syntax error ("Unexpected
// identifier 'X'", "Unexpected token 'X'"), tolerating straight and curly quotes.
var foreignTokenRe = regexp.MustCompile(`['"` + "`" + `\x{2018}\x{2019}]([^'"` + "`" + `\x{2018}\x{2019}]{1,40})['"` + "`" + `\x{2018}\x{2019}]`)

// RepairForeignToken handles a weak-model hallucination the model cannot fix on
// its own: a stray NON-ASCII token injected into code (e.g. a 12B/26B writing
// `didGame ওভার()` for `didGameOver()` — the Bengali word for "over" slipped in
// as its own token). When the syntax error names a non-ASCII token, every
// occurrence is removed and the edit kept ONLY if the file then parses. ASCII
// tokens are never stripped (they may be valid identifiers), and CJK inside
// strings/comments is untouched because it never appears as a syntax-error token.
// Returns the removed token + true on success.
func RepairForeignToken(path, syntaxErr string) (string, bool) {
	var tok string
	for _, m := range foreignTokenRe.FindAllStringSubmatch(syntaxErr, -1) {
		if cand := m[1]; !isASCII(cand) {
			tok = cand
			break
		}
	}
	if tok == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	src := string(data)
	var fixed string
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".html" || ext == ".htm" {
		// HTML re-validation only checks inline <script> JS (htmlInlineJSCheck),
		// so a strip in the HTML body (e.g. a Chinese <h1>) would pass the gate
		// undetected and silently delete legitimate content. Scope the strip to
		// inline <script> regions only — exactly where a hallucinated JS token
		// lives and where re-validation can see it. External scripts untouched.
		fixed = inlineScriptRe.ReplaceAllStringFunc(src, func(block string) string {
			if m := inlineScriptRe.FindStringSubmatch(block); len(m) == 3 && srcAttrRe.MatchString(m[1]) {
				return block
			}
			return strings.ReplaceAll(block, tok, "")
		})
	} else {
		fixed = strings.ReplaceAll(src, tok, "")
	}
	if fixed == src {
		return "", false
	}
	if err := os.WriteFile(path, []byte(fixed), 0o644); err != nil {
		return "", false
	}
	if ValidateSyntax(path) == "" {
		return tok, true // it parses now — keep the strip
	}
	_ = os.WriteFile(path, data, 0o644) // revert: the strip did not make it parse
	return "", false
}

// RepairHTMLRuntimeIssues applies deterministic HTML/Three.js fixes as a batch.
// Some weak VLM outputs contain multiple mechanical runtime defects at once
// (for example a dead CDN URL plus an over-dense FogExp2). The single-issue
// repair helpers intentionally keep changes only when the whole file validates;
// this batch helper lets those cheap repairs compose before the final gate.
func RepairHTMLRuntimeIssues(path, syntaxErr string) (string, bool) {
	if syntaxErr == "" || !strings.HasSuffix(strings.ToLower(path), ".html") {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	original := string(b)
	html := original
	var notes []string

	if badThreeCdnRe.MatchString(html) {
		html = badThreeCdnRe.ReplaceAllStringFunc(html, func(m string) string {
			parts := badThreeCdnRe.FindStringSubmatch(m)
			if len(parts) < 3 {
				return m
			}
			return "https://cdn.jsdelivr.net/npm/three@0." + parts[1] + ".0/build/" + parts[2]
		})
		notes = append(notes, "Three.js jsDelivr CDN")
	}
	if threeRefRe.MatchString(html) && !threeImportRe.MatchString(html) {
		const cdn = `<script src="https://unpkg.com/three@0.160.0/build/three.min.js"></script>`
		lower := strings.ToLower(html)
		insertAt := strings.Index(lower, "<script")
		if insertAt < 0 {
			insertAt = strings.Index(lower, "</head>")
		}
		if insertAt >= 0 {
			html = html[:insertAt] + cdn + "\n" + html[insertAt:]
			notes = append(notes, "Three.js CDN")
		}
	}
	if hasBadFogExp2Density(html) {
		html = fogExp2Re.ReplaceAllStringFunc(html, func(m string) string {
			parts := fogExp2Re.FindStringSubmatch(m)
			if len(parts) < 3 {
				return m
			}
			v, err := strconv.ParseFloat(parts[2], 64)
			if err != nil || v < 1 {
				return m
			}
			return "new THREE.FogExp2(" + strings.TrimSpace(parts[1]) + ", 0.006)"
		})
		notes = append(notes, "FogExp2 density 0.006")
	}
	if threeCameraRe.MatchString(html) && cameraPosSetRe.MatchString(html) &&
		!strings.Contains(html, "camera.lookAt(") &&
		!strings.Contains(html, "OrbitControls") {
		html = cameraPosSetRe.ReplaceAllStringFunc(html, func(m string) string {
			if strings.Contains(m, "\n") {
				return m + "\ncamera.lookAt(0, 0, 0);"
			}
			return m + "\n    camera.lookAt(0, 0, 0);"
		})
		notes = append(notes, "camera.lookAt(0, 0, 0)")
	}
	if html == original || len(notes) == 0 {
		return "", false
	}
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		return "", false
	}
	if ValidateSyntax(path) == "" {
		return strings.Join(notes, ", "), true
	}
	_ = os.WriteFile(path, b, 0o644)
	return "", false
}

// RepairBadThreeJSCDN fixes a common Qwythos/vision web-artifact error:
// cdnjs hosts r128/three.min.js, but not r128/build/three.min.js. The latter
// leaves THREE undefined and the generated page stuck before the first render.
func RepairBadThreeJSCDN(path, syntaxErr string) (string, bool) {
	if !strings.Contains(syntaxErr, "bad Three.js CDN") && !strings.Contains(syntaxErr, "runtime dependency broken") {
		return "", false
	}
	if !strings.HasSuffix(strings.ToLower(path), ".html") {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	html := string(b)
	if !badThreeCdnRe.MatchString(html) {
		return "", false
	}
	repaired := badThreeCdnRe.ReplaceAllStringFunc(html, func(m string) string {
		parts := badThreeCdnRe.FindStringSubmatch(m)
		if len(parts) < 3 {
			return m
		}
		return "https://cdn.jsdelivr.net/npm/three@0." + parts[1] + ".0/build/" + parts[2]
	})
	if repaired == html {
		return "", false
	}
	if err := os.WriteFile(path, []byte(repaired), 0o644); err != nil {
		return "", false
	}
	if ValidateSyntax(path) == "" {
		return "Three.js jsDelivr CDN", true
	}
	_ = os.WriteFile(path, b, 0o644)
	return "", false
}

// RepairMissingRuntimeImport handles a common VLM/LLM web-artifact failure:
// the model writes code that references THREE.* but forgets to load Three.js.
// The repair is deterministic, tiny, and only kept when ValidateSyntax passes.
func RepairMissingRuntimeImport(path, syntaxErr string) (string, bool) {
	if !strings.Contains(syntaxErr, "THREE") || !strings.HasSuffix(strings.ToLower(path), ".html") {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	html := string(b)
	if !threeRefRe.MatchString(html) || threeImportRe.MatchString(html) {
		return "", false
	}
	const cdn = `<script src="https://unpkg.com/three@0.160.0/build/three.min.js"></script>`
	lower := strings.ToLower(html)
	insertAt := strings.Index(lower, "<script")
	if insertAt < 0 {
		insertAt = strings.Index(lower, "</head>")
	}
	if insertAt < 0 {
		return "", false
	}
	repaired := html[:insertAt] + cdn + "\n" + html[insertAt:]
	if err := os.WriteFile(path, []byte(repaired), 0o644); err != nil {
		return "", false
	}
	if ValidateSyntax(path) == "" {
		return "Three.js CDN", true
	}
	_ = os.WriteFile(path, b, 0o644)
	return "", false
}

// RepairThreeJSCameraAim handles the common "all sky / blank-looking canvas"
// failure where Three.js renders correctly but the camera never points at the
// generated scene. It inserts a conservative lookAt after camera.position.set.
func RepairThreeJSCameraAim(path, syntaxErr string) (string, bool) {
	if !strings.Contains(syntaxErr, "camera") || !strings.HasSuffix(strings.ToLower(path), ".html") {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	html := string(b)
	if !threeCameraRe.MatchString(html) || !cameraPosSetRe.MatchString(html) ||
		strings.Contains(html, "camera.lookAt(") ||
		strings.Contains(html, "OrbitControls") {
		return "", false
	}
	repaired := cameraPosSetRe.ReplaceAllStringFunc(html, func(m string) string {
		if strings.Contains(m, "\n") {
			return m + "\ncamera.lookAt(0, 0, 0);"
		}
		return m + "\n    camera.lookAt(0, 0, 0);"
	})
	if repaired == html {
		return "", false
	}
	if err := os.WriteFile(path, []byte(repaired), 0o644); err != nil {
		return "", false
	}
	if ValidateSyntax(path) == "" {
		return "camera.lookAt(0, 0, 0)", true
	}
	_ = os.WriteFile(path, b, 0o644)
	return "", false
}

// RepairThreeJSFogDensity fixes a common visual blanking bug: models confuse
// FogExp2 density with distance and write values like 100, which paints the
// whole scene the fog color.
func RepairThreeJSFogDensity(path, syntaxErr string) (string, bool) {
	if !strings.Contains(syntaxErr, "FogExp2") || !strings.HasSuffix(strings.ToLower(path), ".html") {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	html := string(b)
	if !hasBadFogExp2Density(html) {
		return "", false
	}
	repaired := fogExp2Re.ReplaceAllStringFunc(html, func(m string) string {
		parts := fogExp2Re.FindStringSubmatch(m)
		if len(parts) < 3 {
			return m
		}
		v, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || v < 1 {
			return m
		}
		return "new THREE.FogExp2(" + strings.TrimSpace(parts[1]) + ", 0.006)"
	})
	if repaired == html {
		return "", false
	}
	if err := os.WriteFile(path, []byte(repaired), 0o644); err != nil {
		return "", false
	}
	if ValidateSyntax(path) == "" {
		return "FogExp2 density 0.006", true
	}
	_ = os.WriteFile(path, b, 0o644)
	return "", false
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// cleanSyntaxError pulls the most useful line(s) out of a toolchain's
// stderr — prefer the SyntaxError/Error line, keep it short.
func cleanSyntaxError(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var picked []string
	for _, ln := range lines {
		s := strings.TrimSpace(ln)
		if strings.Contains(s, "SyntaxError") || strings.Contains(s, "Error:") {
			picked = append(picked, s)
		}
	}
	if len(picked) == 0 {
		// fall back to the last non-empty line
		for i := len(lines) - 1; i >= 0; i-- {
			if s := strings.TrimSpace(lines[i]); s != "" {
				picked = []string{s}
				break
			}
		}
	}
	msg := strings.Join(picked, " | ")
	if len(msg) > 240 {
		msg = msg[:240] + "…"
	}
	return msg
}
