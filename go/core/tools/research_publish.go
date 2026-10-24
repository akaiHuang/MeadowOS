package tools

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

const ResearchPublishToolName = "research_publish"

var markdownHTTPLink = regexp.MustCompile(`\[[^\]]*\]\((https?://[^\s)]+)\)`)

// ResearchPublish renders one model-authored, citation-bearing research answer
// into several product views. The model owns all prose and claims; Meadow owns
// deterministic formatting, workspace storage, and PDF validation.
type ResearchPublish struct {
	WS        *Workspace
	Now       func() time.Time
	ExportPDF func(context.Context, string, string) error
}

func (t *ResearchPublish) Name() string { return ResearchPublishToolName }
func (t *ResearchPublish) Description() string {
	return "Publish one source-cited Markdown research answer as a readable HTML document, optional slider, and optional PDF. Use only URLs already opened in this run."
}
func (t *ResearchPublish) Kind() Kind { return KindEdit }
func (t *ResearchPublish) Schema() map[string]any {
	return obj(map[string]any{
		"title":    str("Short document title."),
		"markdown": str("Complete answer in the user's requested language. Use ## headings for slider sections and normal Markdown links for citations."),
		"formats": map[string]any{
			"type": "array", "minItems": 1, "maxItems": 3,
			"items":       map[string]any{"type": "string", "enum": []string{"html", "slides", "pdf"}},
			"description": "Requested views. answer.md is always retained.",
		},
		"output_dir": str("Optional workspace-relative directory. Default: .meadow/research-deliveries/<title>-<timestamp>."),
	}, "title", "markdown", "formats")
}

func (t *ResearchPublish) Execute(ctx context.Context, args map[string]any) Result {
	if t == nil || t.WS == nil {
		return errResultf("workspace is required")
	}
	title := strings.TrimSpace(strArg(args, "title"))
	markdown := strings.TrimSpace(strArg(args, "markdown"))
	if title == "" || markdown == "" {
		return errResultf("title and markdown are required")
	}
	formats, err := researchPublishFormats(args["formats"])
	if err != nil {
		return errResult(err)
	}
	observed := EvidenceSourcesFromContext(ctx)
	markdown, err = normalizeResearchPublishCitations(markdown, observed)
	if err != nil {
		return errResult(err)
	}
	now := time.Now()
	if t.Now != nil {
		now = t.Now()
	}
	outputDir := strings.TrimSpace(strArg(args, "output_dir"))
	if outputDir == "" {
		id := sanitizeArtifactID(title)
		if id == "" {
			id = "research"
		}
		outputDir = filepath.Join(".meadow", "research-deliveries", id+"-"+now.Format("20060102-150405"))
	}
	dir, err := t.WS.Resolve(outputDir)
	if err != nil {
		return errResult(err)
	}
	markdownPath := filepath.Join(dir, "answer.md")
	htmlPath := filepath.Join(dir, "index.html")
	slidesPath := filepath.Join(dir, "slides.html")
	pdfPath := filepath.Join(dir, "answer.pdf")
	pathRefs := []*string{&markdownPath}
	if formats["html"] || formats["pdf"] {
		pathRefs = append(pathRefs, &htmlPath)
	}
	if formats["slides"] {
		pathRefs = append(pathRefs, &slidesPath)
	}
	if formats["pdf"] {
		pathRefs = append(pathRefs, &pdfPath)
	}
	// Validate every requested leaf before creating the directory or writing the
	// first file. A pre-existing symlinked leaf must remain inside this exact
	// delivery directory; merely landing elsewhere in workspace/session/temp is
	// not sufficient authorization for a deterministic multi-file exporter.
	for _, pathRef := range pathRefs {
		resolved, resolveErr := resolveCanonicalWithinRoot(*pathRef, *pathRef, dir, "research output")
		if resolveErr != nil {
			return errResult(resolveErr)
		}
		*pathRef = resolved
	}
	paths := make([]string, 0, len(pathRefs))
	for _, pathRef := range pathRefs {
		if err := t.WS.CheckWritable(*pathRef); err != nil {
			return errResult(err)
		}
		paths = append(paths, *pathRef)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errResult(err)
	}
	sources := normalizedPublishSources(observed)
	displayTitle, presentationMarkdown := researchPresentationContent(title, markdown)
	if err := os.WriteFile(markdownPath, []byte(markdown+"\n"), 0o644); err != nil {
		return errResult(err)
	}
	if formats["html"] || formats["pdf"] {
		htmlDocument, renderErr := renderResearchDocument(displayTitle, presentationMarkdown, sources)
		if renderErr != nil {
			return errResult(renderErr)
		}
		if err := os.WriteFile(htmlPath, []byte(htmlDocument), 0o644); err != nil {
			return errResult(err)
		}
	}
	if formats["slides"] {
		if err := os.WriteFile(slidesPath, []byte(renderResearchSlides(displayTitle, presentationMarkdown, sources)), 0o644); err != nil {
			return errResult(err)
		}
	}
	if formats["pdf"] {
		exporter := t.ExportPDF
		if exporter == nil {
			exporter = exportResearchPDF
		}
		if err := exporter(ctx, htmlPath, pdfPath); err != nil {
			return errResult(&RecoveryError{
				Code: "pdf_export_failed", Message: err.Error(),
				RequiredAction: "retry_research_publish_pdf", PreserveDraft: true,
			})
		}
		payload, err := os.ReadFile(pdfPath)
		if err != nil || len(payload) < 4 || string(payload[:4]) != "%PDF" {
			return errResultf("PDF validation failed for %s", t.WS.ModelPath(pdfPath))
		}
	}
	for _, path := range paths {
		t.WS.NoteRead(path)
	}
	modelPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		modelPaths = append(modelPaths, t.WS.ModelPath(path))
	}
	message := "Research delivery created: " + strings.Join(modelPaths, ", ")
	return Result{LLMContent: message, Display: message, ChangedPaths: modelPaths}
}

// researchPresentationContent keeps the original Markdown as the durable
// answer while deriving a concise display title for rendered views. Small
// models sometimes pass the entire user request as the tool's title even when
// their Markdown already contains the intended document heading. Prefer that
// authored H1 and remove only its first occurrence from the rendered body so
// HTML/PDF do not show two competing H1s.
func researchPresentationContent(fallbackTitle, markdown string) (string, string) {
	lines := strings.Split(markdown, "\n")
	displayTitle := ""
	removeAt := -1
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "# ") {
			continue
		}
		displayTitle = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		displayTitle = strings.Trim(displayTitle, "*_` ")
		if displayTitle != "" {
			removeAt = index
			break
		}
	}
	if displayTitle == "" {
		displayTitle = strings.TrimSpace(fallbackTitle)
	}
	runes := []rune(displayTitle)
	if len(runes) > 120 {
		displayTitle = strings.TrimSpace(string(runes[:117])) + "…"
	}
	if removeAt >= 0 {
		lines = append(lines[:removeAt], lines[removeAt+1:]...)
	}
	return displayTitle, strings.TrimSpace(strings.Join(lines, "\n"))
}

func researchPublishFormats(value any) (map[string]bool, error) {
	values := make([]string, 0, 3)
	switch typed := value.(type) {
	case []string:
		values = typed
	case []any:
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("formats must contain strings")
			}
			values = append(values, text)
		}
	default:
		return nil, fmt.Errorf("formats is required")
	}
	result := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "html" && value != "slides" && value != "pdf" {
			return nil, fmt.Errorf("unsupported research format %q", value)
		}
		result[value] = true
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("formats must not be empty")
	}
	return result, nil
}

func normalizeResearchPublishCitations(markdown string, observed []EvidenceSource) (string, error) {
	allowed := make(map[string]bool, len(observed))
	for _, source := range observed {
		allowed[strings.TrimSpace(source.URL)] = true
	}
	links := markdownHTTPLink.FindAllStringSubmatch(markdown, -1)
	cited := make(map[string]bool, len(links))
	for _, match := range links {
		if len(match) > 1 && !allowed[strings.TrimSpace(match[1])] {
			return "", fmt.Errorf("citation URL was not opened in this run: %s", match[1])
		}
		if len(match) > 1 {
			cited[strings.TrimSpace(match[1])] = true
		}
	}
	// Citation-list rendering is mechanical provenance, not model-authored
	// semantics. Append every missing opened URL so a small model cannot spend
	// several expensive turns failing only Markdown punctuation after it has
	// already produced a grounded answer. Unopened links remain a hard error.
	missing := make([]EvidenceSource, 0, len(observed))
	for _, source := range normalizedPublishSources(observed) {
		if !cited[source.URL] {
			missing = append(missing, source)
		}
	}
	if len(missing) > 0 {
		markdown = strings.TrimSpace(markdown) + "\n\n## 來源\n"
		for index, source := range missing {
			label := strings.TrimSpace(source.Title)
			if label == "" {
				label = fmt.Sprintf("來源 %d", index+1)
			}
			markdown += fmt.Sprintf("- [%s](%s)\n", escapeMarkdownLinkLabel(label), source.URL)
		}
	}
	return strings.TrimSpace(markdown), nil
}

func escapeMarkdownLinkLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "[", "\\[")
	return strings.ReplaceAll(value, "]", "\\]")
}

func normalizedPublishSources(values []EvidenceSource) []EvidenceSource {
	seen := make(map[string]bool)
	result := make([]EvidenceSource, 0, len(values))
	for _, source := range values {
		source.URL = strings.TrimSpace(source.URL)
		source.Title = strings.TrimSpace(source.Title)
		if source.URL == "" || seen[source.URL] {
			continue
		}
		seen[source.URL] = true
		result = append(result, source)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].URL < result[j].URL })
	return result
}

func renderResearchDocument(title, markdown string, sources []EvidenceSource) (string, error) {
	var content bytes.Buffer
	engine := goldmark.New(goldmark.WithExtensions(extension.GFM))
	if err := engine.Convert([]byte(markdown), &content); err != nil {
		return "", err
	}
	return researchDocumentShell(title, template.HTML(content.String()), sources), nil
}

func researchDocumentShell(title string, body template.HTML, sources []EvidenceSource) string {
	var sourceHTML strings.Builder
	for _, source := range sources {
		fmt.Fprintf(&sourceHTML, `<li><a href="%s">%s</a></li>`, template.HTMLEscapeString(source.URL), template.HTMLEscapeString(source.Title))
	}
	return `<!doctype html><html lang="zh-Hant"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + template.HTMLEscapeString(title) + `</title><style>
:root{color-scheme:light;--ink:#18231d;--muted:#667269;--line:#dce5de;--accent:#167a4b}*{box-sizing:border-box}body{margin:0;background:#f4f7f4;color:var(--ink);font:17px/1.75 -apple-system,BlinkMacSystemFont,"Noto Sans TC",sans-serif}.page{max-width:920px;margin:48px auto;padding:64px 72px;background:#fff;border:1px solid var(--line);border-radius:24px;box-shadow:0 18px 60px #203a2814}h1{font-size:2.3rem;line-height:1.18;margin:0 0 32px}h2{margin-top:44px;border-top:1px solid var(--line);padding-top:28px;color:var(--accent)}table{width:100%;border-collapse:collapse;margin:24px 0}th,td{border:1px solid var(--line);padding:10px 12px;text-align:left;vertical-align:top}a{color:var(--accent)}code{background:#edf3ee;padding:.12em .35em;border-radius:5px}.sources{margin-top:54px;padding-top:28px;border-top:1px solid var(--line);color:var(--muted)}@media(max-width:720px){.page{margin:0;padding:32px 22px;border:0;border-radius:0}}@media print{body{background:#fff}.page{max-width:none;margin:0;padding:18mm;border:0;box-shadow:none}}
</style></head><body><main class="page"><h1>` + template.HTMLEscapeString(title) + `</h1>` + string(body) + `<section class="sources"><h2>來源</h2><ol>` + sourceHTML.String() + `</ol></section></main></body></html>`
}

func renderResearchSlides(title, markdown string, sources []EvidenceSource) string {
	sections := append([]researchSection{{title: title}}, splitResearchSections(markdown)...)
	var slides strings.Builder
	for index, section := range sections {
		body := ""
		if strings.TrimSpace(section.body) != "" {
			document, _ := renderResearchDocument("", section.body, nil)
			body = extractResearchMainBody(document)
		}
		fmt.Fprintf(&slides, `<section class="slide%s"><div class="count">%d / %d</div><h2>%s</h2><div class="body">%s</div></section>`,
			map[bool]string{true: " active"}[index == 0], index+1, len(sections), template.HTMLEscapeString(section.title), body)
	}
	return `<!doctype html><html lang="zh-Hant"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + template.HTMLEscapeString(title) + `</title><style>body{margin:0;background:#101713;color:#edf8f0;font:20px/1.55 -apple-system,BlinkMacSystemFont,"Noto Sans TC",sans-serif}.slide{display:none;min-height:100vh;padding:8vh 9vw}.slide.active{display:block}.slide h2{font-size:clamp(36px,6vw,76px);line-height:1.05;max-width:1100px;color:#79efad}.body{max-width:1100px;font-size:clamp(18px,2.3vw,30px)}a{color:#79efad}.count{position:fixed;right:4vw;top:4vh;color:#8ea096;font-size:14px}table{border-collapse:collapse;width:100%}td,th{border:1px solid #405248;padding:10px}</style></head><body>` + slides.String() + `<script>const s=[...document.querySelectorAll('.slide')];let i=0;function show(n){i=(n+s.length)%s.length;s.forEach((x,j)=>x.classList.toggle('active',j===i))}addEventListener('keydown',e=>{if(e.key==='ArrowRight'||e.key===' '){show(i+1)}if(e.key==='ArrowLeft'){show(i-1)}});addEventListener('click',()=>show(i+1));</script></body></html>`
}

type researchSection struct{ title, body string }

func splitResearchSections(markdown string) []researchSection {
	lines := strings.Split(markdown, "\n")
	sections := []researchSection{{title: "重點摘要"}}
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "## ") {
			sections = append(sections, researchSection{title: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "## "))})
			continue
		}
		sections[len(sections)-1].body += line + "\n"
	}
	filtered := sections[:0]
	for _, section := range sections {
		if strings.TrimSpace(section.body) != "" {
			filtered = append(filtered, section)
		}
	}
	if len(filtered) == 0 {
		return []researchSection{{title: "重點摘要", body: markdown}}
	}
	return filtered
}

func extractResearchMainBody(document string) string {
	start := strings.Index(document, `<h1></h1>`)
	end := strings.Index(document, `<section class="sources">`)
	if start >= 0 && end > start {
		return document[start+len(`<h1></h1>`) : end]
	}
	return document
}

func exportResearchPDF(ctx context.Context, htmlPath, pdfPath string) error {
	binary := strings.TrimSpace(os.Getenv("MEADOW_CHROME_BIN"))
	if binary == "" {
		for _, candidate := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				binary = candidate
				break
			}
		}
	}
	if binary == "" {
		return fmt.Errorf("no bundled or installed Chrome-compatible PDF renderer is available")
	}
	fileURL := (&url.URL{Scheme: "file", Path: htmlPath}).String()
	command := exec.CommandContext(ctx, binary,
		"--headless=new", "--disable-gpu", "--no-pdf-header-footer",
		"--print-to-pdf="+pdfPath, fileURL,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("Chrome PDF export failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
