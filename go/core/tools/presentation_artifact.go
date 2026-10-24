package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed presentation_assets/presentation.js.txt
var presentationJavaScript string

//go:embed presentation_assets/presentation.css
var presentationCSS string

// PresentationArtifact turns compact, typed slide data into an editable
// TypeScript source and a self-contained browser presentation. The model owns
// the content; the harness owns only schema validation and reliable rendering.
type PresentationArtifact struct {
	WS             *Workspace
	Now            func() time.Time
	PresentonURL   string
	PresentonToken string
	HTTP           *http.Client
	// Only PresentationFromMarkdown can bind this per-call compiler receipt.
	// It is never populated from arguments or exposed by the advanced schema.
	markdownImport *presentationMarkdownImport
	// Test seam for a late publication failure; nil in registered tools.
	markdownBeforePublish func(int, string) error
}

func (t *PresentationArtifact) Name() string { return "presentation_artifact" }
func (t *PresentationArtifact) Description() string {
	return "Advanced native-layout PPTX/HTML renderer. For the compact content-to-deck path, presentation_templates operation=markdown and presentation_from_markdown avoid native schema discovery. Use this tool for a chosen design from the broader native catalog: select one template for the whole deck. Every slide requires title, presenton_layout_id and presenton_content_json as a STRING encoding the selected schema's JSON object, not a raw object. All layouts must belong to that same template. Content is preserved without a second model rewrite. Keep external sources in the ledger; omit for local discussion. Read presenton.json and inspect every slide PNG. engine=builtin keeps compact typed HTML."
}
func (t *PresentationArtifact) Kind() Kind { return KindEdit }
func (t *PresentationArtifact) NormalizeArguments(args map[string]any) {
	normalizePresentationArrayArgument(args, "slides", "slides_json")
	normalizePresentationArrayArgument(args, "sources", "sources_json")
	omitLocalPresentationSources(args)
}
func (t *PresentationArtifact) Schema() map[string]any {
	stringArray := func(description string, maxItems int) map[string]any {
		return map[string]any{
			"type": "array", "description": description,
			"items": map[string]any{"type": "string"}, "maxItems": maxItems,
		}
	}
	metric := obj(map[string]any{
		"label": str("Metric label."), "value": str("Displayed value."),
		"note": str("Optional short context."),
	}, "label", "value")
	table := obj(map[string]any{
		"columns": stringArray("One to eight column labels.", 8),
		"rows": map[string]any{
			"type": "array", "maxItems": 20,
			"items": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"maxItems": 8,
			},
		},
	}, "columns", "rows")
	slide := obj(map[string]any{
		"id": str("Stable slide id."), "title": str("Slide title."),
		"presenton_layout_id":    str("Exact native layout ID from presentation_templates operation=layouts/schema, belonging to this deck's single selected template. Required on every Presenton slide."),
		"presenton_content_json": map[string]any{"type": "string", "maxLength": 131072, "description": "A STRING encoding one strict JSON object matching the selected live layout schema; serialize the object as a JSON string, do not pass a raw object. Required on every Presenton slide, alongside its own title and presenton_layout_id. Preserve source facts and literal text; do not invent chart values or fill unsupported claims."},
		"eyebrow":                str("Optional section label."), "summary": str("Optional concise takeaway."),
		"bullets": stringArray("Up to eight concise bullets.", 8),
		"metrics": map[string]any{"type": "array", "items": metric, "maxItems": 6},
		"table":   table, "quote": str("Optional quote."),
		"source_urls": stringArray("Source URLs supporting this slide.", 8),
		"layout": map[string]any{
			"type": "string", "enum": []string{"cover", "statement", "metrics", "comparison", "table", "quote"},
		},
	}, "title")
	source := obj(map[string]any{
		"title": str("Source title."), "url": str("Full http(s) URL."),
		"excerpt":     str("Optional short evidence excerpt."),
		"accessed_at": str("Optional access date."),
	}, "title", "url")
	return obj(map[string]any{
		"engine":       map[string]any{"type": "string", "enum": []string{"auto", "builtin", "presenton"}, "description": "Use presenton for native layout/content and real PPTX; auto uses installed Presenton and requires the same fields. Unconfigured auto or omission uses builtin HTML, which rejects native fields. Failures never silently fall back."},
		"template":     str("One exact installed template ID for the entire deck, chosen from presentation_templates. Every slide's native layout must belong to it; do not mix templates. Explicitly required for Presenton; no automatic template selection."),
		"language":     str("Slide language. Default Traditional Chinese."),
		"instructions": str("Optional authoring context. Exact native rendering does not interpret this text or change the selected layout/content."),
		"artifact_id":  str("Optional stable id, such as market-review. Reusing an id updates the same presentation."),
		"title":        str("Short presentation title."),
		"subtitle":     str("Optional subtitle."),
		"author":       str("Optional author or team."),
		"slides": map[string]any{
			"type": "array", "description": "One to twenty slides. For Presenton each slide requires its native layout ID and schema-shaped JSON content; typed layout/bullets fields belong to builtin HTML.",
			"items": slide, "minItems": 1, "maxItems": 20,
		},
		"sources": map[string]any{
			"type": "array", "description": "Optional external web source ledger. Omit for local-only material; every URL must be http(s).",
			"items": source, "maxItems": 40,
		},
		"output_dir": str("Optional output directory. Default: /session/.meadow/presentations/<artifact_id> when a chat mount is available; otherwise /workspace/.meadow/presentations/<artifact_id>."),
	}, "title", "slides")
}

func normalizePresentationArrayArgument(
	args map[string]any, canonical, legacy string,
) {
	value, exists := args[canonical]
	if !exists {
		value, exists = args[legacy]
	}
	raw, ok := value.(string)
	if !exists || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var decoded []any
	if json.Unmarshal([]byte(raw), &decoded) == nil {
		args[canonical] = decoded
		return
	}
	// Qwen occasionally stringifies the typed slide array and omits the
	// bullets-array closing bracket immediately before the source_urls sibling:
	// {"bullets":["fact","source_urls":[...]}.
	// The exact token sequence is otherwise invalid JSON and has only one
	// schema-preserving interpretation, so repair it at this tool's wire
	// boundary rather than spending two inference turns asking for the same
	// payload again.
	if canonical == "slides" {
		repaired := strings.ReplaceAll(raw, `,"source_urls":[`, `],"source_urls":[`)
		if repaired != raw && json.Unmarshal([]byte(repaired), &decoded) == nil {
			args[canonical] = decoded
			return
		}
		// Some Qwen native-tool responses preserve every value but omit an
		// enclosing array closer before the following object closer. Repair only
		// delimiter mismatches whose later closer makes the missing delimiter
		// unambiguous. The result must still be strict JSON and pass the ordinary
		// presentation schema; commas, strings, values and fields are never
		// invented here.
		if repaired, ok := repairMissingJSONClosures(raw, 16); ok &&
			json.Unmarshal([]byte(repaired), &decoded) == nil {
			args[canonical] = decoded
		}
	}
}

func repairMissingJSONClosures(raw string, maxInsertions int) (string, bool) {
	if maxInsertions <= 0 {
		return "", false
	}
	stack := make([]byte, 0, 8)
	var out strings.Builder
	out.Grow(len(raw) + maxInsertions)
	inString := false
	escaped := false
	insertions := 0
	matchingOpen := func(close byte) byte {
		if close == '}' {
			return '{'
		}
		return '['
	}
	matchingClose := func(open byte) byte {
		if open == '{' {
			return '}'
		}
		return ']'
	}
	for index := 0; index < len(raw); index++ {
		char := raw[index]
		out.WriteByte(char)
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch char {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, char)
		case '}', ']':
			want := matchingOpen(char)
			for len(stack) > 0 && stack[len(stack)-1] != want {
				if insertions >= maxInsertions {
					return "", false
				}
				// The current closer was already written; place the inferred
				// inner closer immediately before it.
				text := out.String()
				out.Reset()
				out.Grow(len(text) + maxInsertions - insertions)
				out.WriteString(text[:len(text)-1])
				out.WriteByte(matchingClose(stack[len(stack)-1]))
				out.WriteByte(char)
				stack = stack[:len(stack)-1]
				insertions++
			}
			if len(stack) == 0 {
				return "", false
			}
			stack = stack[:len(stack)-1]
		}
	}
	if inString || len(stack) != 0 || insertions == 0 {
		return "", false
	}
	return out.String(), true
}

func omitLocalPresentationSources(args map[string]any) {
	if rawSources, ok := args["sources"].([]any); ok {
		// An explicit empty ledger means "clear sources". Preserve the field so
		// Execute can distinguish that request from an omitted ledger on an
		// update, where existing external evidence must remain intact.
		if len(rawSources) > 0 {
			filtered := make([]any, 0, len(rawSources))
			for _, raw := range rawSources {
				source, ok := raw.(map[string]any)
				if ok && isLocalPresentationSource(strArg(source, "url")) {
					continue
				}
				filtered = append(filtered, raw)
			}
			if len(filtered) == 0 {
				delete(args, "sources")
			} else {
				args["sources"] = filtered
			}
		}
	}
	rawSlides, ok := args["slides"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawSlides {
		slide, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		rawURLs, ok := slide["source_urls"].([]any)
		if !ok {
			continue
		}
		filtered := make([]any, 0, len(rawURLs))
		for _, rawURL := range rawURLs {
			value, isString := rawURL.(string)
			if isString && isLocalPresentationSource(value) {
				continue
			}
			filtered = append(filtered, rawURL)
		}
		if len(filtered) == 0 {
			delete(slide, "source_urls")
		} else {
			slide["source_urls"] = filtered
		}
	}
}

func isLocalPresentationSource(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "/") ||
		strings.HasPrefix(value, "./") ||
		strings.HasPrefix(value, "../") ||
		strings.HasPrefix(value, "input/") ||
		strings.HasPrefix(value, "file://")
}

type presentationMetricInput struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Note  string `json:"note,omitempty"`
}

type presentationTableInput struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

type presentationSlideInput struct {
	ID                   string                    `json:"id"`
	Title                string                    `json:"title"`
	Eyebrow              string                    `json:"eyebrow,omitempty"`
	Summary              string                    `json:"summary,omitempty"`
	Bullets              []string                  `json:"bullets,omitempty"`
	Metrics              []presentationMetricInput `json:"metrics,omitempty"`
	Table                *presentationTableInput   `json:"table,omitempty"`
	Quote                string                    `json:"quote,omitempty"`
	SourceURLs           []string                  `json:"source_urls,omitempty"`
	Layout               string                    `json:"layout,omitempty"`
	PresentonLayoutID    string                    `json:"presenton_layout_id,omitempty"`
	PresentonContentJSON string                    `json:"presenton_content_json,omitempty"`
}

type presentationSourceInput struct {
	Title             string `json:"title"`
	URL               string `json:"url"`
	Excerpt           string `json:"excerpt,omitempty"`
	AccessedAt        string `json:"accessed_at,omitempty"`
	storedObservation *presentationSourceObservation
}

type presentationMetric struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Note  string `json:"note,omitempty"`
}

type presentationTable struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

type presentationSlide struct {
	ID                string               `json:"id"`
	Title             string               `json:"title"`
	Eyebrow           string               `json:"eyebrow,omitempty"`
	Summary           string               `json:"summary,omitempty"`
	Bullets           []string             `json:"bullets,omitempty"`
	Metrics           []presentationMetric `json:"metrics,omitempty"`
	Table             *presentationTable   `json:"table,omitempty"`
	Quote             string               `json:"quote,omitempty"`
	SourceURLs        []string             `json:"sourceUrls,omitempty"`
	Layout            string               `json:"layout,omitempty"`
	PresentonLayoutID string               `json:"presentonLayoutId,omitempty"`
	PresentonContent  json.RawMessage      `json:"presentonContent,omitempty"`
}

type presentationSource struct {
	Title       string                         `json:"title"`
	URL         string                         `json:"url"`
	Excerpt     string                         `json:"excerpt,omitempty"`
	AccessedAt  string                         `json:"accessedAt,omitempty"`
	Observation *presentationSourceObservation `json:"observation,omitempty"`
}

type presentationData struct {
	ArtifactID  string               `json:"artifact_id"`
	Title       string               `json:"title"`
	Subtitle    string               `json:"subtitle,omitempty"`
	Author      string               `json:"author,omitempty"`
	GeneratedAt string               `json:"generatedAt"`
	Slides      []presentationSlide  `json:"slides"`
	Sources     []presentationSource `json:"sources,omitempty"`
}

var presentationLayouts = map[string]bool{
	"": true, "cover": true, "statement": true, "metrics": true,
	"comparison": true, "table": true, "quote": true,
}

func (t *PresentationArtifact) Execute(ctx context.Context, args map[string]any) Result {
	if t.WS == nil {
		return errResultf("workspace is required")
	}
	title := strings.TrimSpace(strArg(args, "title"))
	if title == "" {
		return errResultf("title is required")
	}
	slides, err := parsePresentationSlidesArg(args)
	if err != nil {
		return errResultf("slides: %v", err)
	}
	endpoint, err := t.presentonEndpoint(strArg(args, "engine"))
	if err != nil {
		return errResult(err)
	}
	if err := validatePresentationEngineSlides(endpoint, slides, strArg(args, "template")); err != nil {
		return errResult(err)
	}
	sources, err := parsePresentationSourcesArg(args)
	if err != nil {
		return errResultf("sources: %v", err)
	}
	_, typedSourcesProvided := args["sources"]
	_, legacySourcesProvided := args["sources_json"]
	sourcesProvided := typedSourcesProvided || legacySourcesProvided

	now := time.Now()
	if t.Now != nil {
		now = t.Now()
	}
	artifactID := sanitizeArtifactID(strArg(args, "artifact_id"))
	if artifactID == "" {
		artifactID = sanitizeArtifactID(title)
	}
	if artifactID == "" {
		artifactID = "presentation"
	}
	if strings.TrimSpace(strArg(args, "artifact_id")) == "" {
		artifactID = fmt.Sprintf("%s-%s", artifactID, now.Format("20060102-150405"))
	}
	outputDir := strings.TrimSpace(strArg(args, "output_dir"))
	if outputDir == "" {
		outputDir = filepath.Join(".meadow", "presentations", artifactID)
	}
	storeDir, err := t.WS.ResolveArtifact(outputDir)
	if err != nil {
		return errResult(err)
	}
	paths := []string{
		filepath.Join(storeDir, "index.html"),
		filepath.Join(storeDir, "presentation.json"),
		filepath.Join(storeDir, "sources.json"),
		filepath.Join(storeDir, "deck.ts"),
	}
	if endpoint.URL != "" {
		paths = append(paths, filepath.Join(storeDir, "presentation.pptx"), filepath.Join(storeDir, "presenton.json"))
		for index := range slides {
			paths = append(paths, filepath.Join(storeDir, fmt.Sprintf("slide-%02d.png", index+1)))
		}
	}
	previewEnd := len(paths)
	if t.markdownImport != nil {
		for _, name := range t.markdownImport.sidecars() {
			if _, ok := t.markdownImport.Files[name]; !ok {
				return errResultf("compiled Markdown receipt is incomplete")
			}
			paths = append(paths, filepath.Join(storeDir, name))
		}
	}
	for index, path := range paths {
		if t.markdownImport != nil {
			if info, statErr := os.Lstat(path); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || info.IsDir()) {
				return errResultf("compiled presentation output must be a regular owned file, not a directory or symlink: %s", t.WS.ModelPath(path))
			} else if statErr != nil && !os.IsNotExist(statErr) {
				return errResult(statErr)
			}
			resolved, resolveErr := resolveCanonicalWithinRoot(path, path, storeDir, "presentation output")
			if resolveErr != nil {
				return errResult(resolveErr)
			}
			path, paths[index] = resolved, resolved
		}
		if err := t.WS.CheckWritable(path); err != nil {
			return errResult(err)
		}
		if err := t.WS.CheckPriorRead(path); err != nil {
			return errResult(err)
		}
	}
	if !sourcesProvided {
		existingSources, loadErr := loadExistingPresentationSources(paths[2])
		if loadErr != nil {
			return errResultf("preserve existing sources: %v", loadErr)
		}
		sources = append(existingSources, sources...)
	}
	// Omission means preserve the owned artifact's stored ledger, including its
	// prior receipt metadata. Enforce the requirement after that read; explicit
	// empty sources still bypass preservation and cannot clear cited evidence.
	if len(sources) == 0 {
		observedSources := EvidenceSourcesFromContext(ctx)
		if len(observedSources) > 0 {
			return errResult(&RecoveryError{
				Code:           "source_ledger_required",
				Message:        "source ledger is required because this run opened external evidence",
				RequiredAction: "retry_presentation_with_sources",
				RequiredArgs:   map[string]any{"sources": observedSources},
				PreserveDraft:  true,
			})
		}
		if presentationSlidesCiteExternalURL(slides) {
			return errResult(&RecoveryError{
				Code:           "source_ledger_required",
				Message:        "source ledger is required when slide content cites http(s)",
				RequiredAction: "retry_presentation_with_sources",
				PreserveDraft:  true,
			})
		}
	}
	if err := validatePresentationSourceLineage(ctx, slides, sources); err != nil {
		return errResult(err)
	}
	normalizedSlides, normalizedSources, err := normalizePresentationData(slides, sources)
	if err != nil {
		return errResult(err)
	}
	annotatePresentationSourceObservations(ctx, normalizedSources)

	deck := presentationData{
		ArtifactID:  artifactID,
		Title:       title,
		Subtitle:    strings.TrimSpace(strArg(args, "subtitle")),
		Author:      strings.TrimSpace(strArg(args, "author")),
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Slides:      normalizedSlides,
		Sources:     normalizedSources,
	}
	var rendered *presentonRenderResult
	if endpoint.URL != "" {
		rendered, err = t.renderPresenton(ctx, endpoint, deck, args)
		if err != nil {
			return errResult(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	dataJSON, err := json.MarshalIndent(deck, "", "  ")
	if err != nil {
		return errResult(err)
	}
	sourcesJSON, err := json.MarshalIndent(normalizedSources, "", "  ")
	if err != nil {
		return errResult(err)
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return errResult(err)
	}
	files := map[string][]byte{
		paths[0]: []byte(renderPresentationHTML(deck.Title, dataJSON)),
		paths[1]: append(dataJSON, '\n'),
		paths[2]: append(sourcesJSON, '\n'),
		paths[3]: []byte(renderPresentationTypeScript(dataJSON)),
	}
	if rendered != nil {
		files[paths[0]] = []byte(rendered.HTML)
		files[paths[4]] = rendered.PPTX
		files[paths[5]] = rendered.Presentation
		for index, preview := range rendered.SlidePreviews {
			files[paths[6+index]] = preview
		}
	}
	if t.markdownImport != nil {
		for index, name := range t.markdownImport.sidecars() {
			files[paths[previewEnd+index]] = t.markdownImport.Files[name]
		}
	}
	if t.markdownImport != nil {
		if err := publishMarkdownPresentationBundle(ctx, t.WS, storeDir, paths, files, t.markdownBeforePublish); err != nil {
			result := errResult(err)
			if publication, ok := err.(*markdownPublicationError); ok {
				for _, path := range publication.ChangedPaths {
					result.ChangedPaths = append(result.ChangedPaths, t.WS.ModelPath(path))
				}
			}
			return result
		}
	} else {
		for _, path := range paths {
			if err := os.WriteFile(path, files[path], 0o644); err != nil {
				return errResult(err)
			}
		}
	}
	for _, path := range paths {
		t.WS.NoteRead(path)
	}
	message := fmt.Sprintf(
		"Presentation created: %s (slides=%d, sources=%d, editable=%s)",
		t.WS.ModelPath(paths[0]), len(normalizedSlides), len(normalizedSources), t.WS.ModelPath(paths[3]),
	)
	if rendered != nil {
		message += fmt.Sprintf("; engine=presenton template=%s pptx=%s. Read %s for actual rendered content; presentation.json/deck.ts retain your input. Inspect every returned slide PNG with inspect_image detail=high/original before claiming visual quality. Technical render success is not content/design acceptance. Slide previews:", rendered.Template, t.WS.ModelPath(paths[4]), t.WS.ModelPath(paths[5]))
		for _, path := range paths[6:previewEnd] {
			message += "\n" + t.WS.ModelPath(path)
		}
	}
	if t.markdownImport != nil {
		message += "\nMarkdown and compiler receipt:"
		for _, path := range paths[previewEnd:] {
			message += "\n" + t.WS.ModelPath(path)
		}
		if t.markdownImport.PendingImages > 0 {
			message += fmt.Sprintf("\n%d image brief(s) remain pending in the saved plan; no images were generated or placed from those descriptions.", t.markdownImport.PendingImages)
		}
	}
	changedPaths := make([]string, 0, len(paths))
	outputs := paths
	if rendered != nil {
		outputs = []string{paths[0], paths[4], paths[1], paths[2], paths[3], paths[5]}
		outputs = append(outputs, paths[6:]...)
	}
	for _, path := range outputs {
		changedPaths = append(changedPaths, t.WS.ModelPath(path))
	}
	return Result{
		LLMContent:   message,
		Display:      message,
		ChangedPaths: changedPaths,
	}
}

func presentationSlidesCiteExternalURL(slides []presentationSlideInput) bool {
	cites := func(value string) bool {
		value = strings.ToLower(value)
		return strings.Contains(value, "https://") || strings.Contains(value, "http://")
	}
	for _, slide := range slides {
		if slide.PresentonContentJSON != "" {
			content, err := parsePresentonContent(slide.PresentonContentJSON)
			if err == nil && presentonContentCitesURL(content) {
				return true
			}
		}
		for _, value := range []string{slide.Title, slide.Eyebrow, slide.Summary, slide.Quote} {
			if cites(value) {
				return true
			}
		}
		for _, value := range append(append([]string(nil), slide.Bullets...), slide.SourceURLs...) {
			if cites(value) {
				return true
			}
		}
		for _, metric := range slide.Metrics {
			if cites(metric.Label) || cites(metric.Value) || cites(metric.Note) {
				return true
			}
		}
		if slide.Table != nil {
			for _, value := range slide.Table.Columns {
				if cites(value) {
					return true
				}
			}
			for _, row := range slide.Table.Rows {
				for _, value := range row {
					if cites(value) {
						return true
					}
				}
			}
		}
	}
	return false
}

func loadExistingPresentationSources(path string) ([]presentationSourceInput, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stored []presentationSource
	if err := json.Unmarshal(body, &stored); err != nil {
		return nil, err
	}
	result := make([]presentationSourceInput, 0, len(stored))
	for _, source := range stored {
		result = append(result, presentationSourceInput{
			Title: source.Title, URL: source.URL,
			Excerpt: source.Excerpt, AccessedAt: source.AccessedAt,
			storedObservation: source.Observation,
		})
	}
	return result, nil
}

func parsePresentationSlides(raw string) ([]presentationSlideInput, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("must contain a JSON array")
	}
	var slides []presentationSlideInput
	if err := json.Unmarshal([]byte(raw), &slides); err != nil {
		return nil, err
	}
	if len(slides) < 1 || len(slides) > 20 {
		return nil, fmt.Errorf("must contain 1-20 slides")
	}
	return slides, nil
}

func parsePresentationSlidesArg(args map[string]any) ([]presentationSlideInput, error) {
	if value, exists := args["slides"]; exists {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return parsePresentationSlides(string(raw))
	}
	// Compatibility for saved/replayed calls created before the typed schema.
	return parsePresentationSlides(strArg(args, "slides_json"))
}

func parsePresentationSources(raw string) ([]presentationSourceInput, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var sources []presentationSourceInput
	if err := json.Unmarshal([]byte(raw), &sources); err != nil {
		return nil, err
	}
	if len(sources) > 40 {
		return nil, fmt.Errorf("must contain at most 40 sources")
	}
	return sources, nil
}

func parsePresentationSourcesArg(args map[string]any) ([]presentationSourceInput, error) {
	if value, exists := args["sources"]; exists {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return parsePresentationSources(string(raw))
	}
	return parsePresentationSources(strArg(args, "sources_json"))
}

func normalizePresentationData(
	inputSlides []presentationSlideInput,
	inputSources []presentationSourceInput,
) ([]presentationSlide, []presentationSource, error) {
	sourceByURL := map[string]presentationSource{}
	sources := make([]presentationSource, 0, len(inputSources))
	for index, source := range inputSources {
		source.URL = strings.TrimSpace(source.URL)
		if !validPresentationURL(source.URL) {
			return nil, nil, fmt.Errorf(
				"source %d has invalid http(s) URL; omit local-only sources and never use a workspace path as a URL",
				index+1,
			)
		}
		if _, exists := sourceByURL[source.URL]; exists {
			continue
		}
		normalized := presentationSource{
			Title: strings.TrimSpace(source.Title), URL: source.URL,
			Excerpt: strings.TrimSpace(source.Excerpt), AccessedAt: strings.TrimSpace(source.AccessedAt),
			Observation: source.storedObservation,
		}
		if normalized.Title == "" {
			normalized.Title = normalized.URL
		}
		sourceByURL[normalized.URL] = normalized
		sources = append(sources, normalized)
	}

	ids := map[string]bool{}
	slides := make([]presentationSlide, 0, len(inputSlides))
	for index, slide := range inputSlides {
		slide.Title = strings.TrimSpace(slide.Title)
		if slide.Title == "" {
			return nil, nil, fmt.Errorf("slide %d title is required", index+1)
		}
		id := sanitizeArtifactID(slide.ID)
		if id == "" {
			id = fmt.Sprintf("slide-%02d", index+1)
		}
		if ids[id] {
			return nil, nil, fmt.Errorf("duplicate slide id %q", id)
		}
		ids[id] = true
		if !presentationLayouts[slide.Layout] {
			return nil, nil, fmt.Errorf("slide %q has unsupported layout %q", id, slide.Layout)
		}
		if len(slide.Bullets) > 8 {
			return nil, nil, fmt.Errorf("slide %q has more than 8 bullets", id)
		}
		if len(slide.Metrics) > 6 {
			return nil, nil, fmt.Errorf("slide %q has more than 6 metrics", id)
		}
		var table *presentationTable
		if slide.Table != nil {
			if len(slide.Table.Columns) < 1 || len(slide.Table.Columns) > 8 {
				return nil, nil, fmt.Errorf("slide %q table must have 1-8 columns", id)
			}
			if len(slide.Table.Rows) > 20 {
				return nil, nil, fmt.Errorf("slide %q table has more than 20 rows", id)
			}
			for rowIndex, row := range slide.Table.Rows {
				if len(row) != len(slide.Table.Columns) {
					return nil, nil, fmt.Errorf("slide %q table row %d has %d cells, want %d", id, rowIndex+1, len(row), len(slide.Table.Columns))
				}
			}
			table = &presentationTable{Columns: trimStrings(slide.Table.Columns), Rows: trimRows(slide.Table.Rows)}
		}
		sourceURLs := make([]string, 0, len(slide.SourceURLs))
		for _, rawURL := range slide.SourceURLs {
			sourceURL := strings.TrimSpace(rawURL)
			if !validPresentationURL(sourceURL) {
				return nil, nil, fmt.Errorf("slide %q has invalid source URL", id)
			}
			if _, exists := sourceByURL[sourceURL]; !exists {
				derived := presentationSource{Title: sourceURL, URL: sourceURL}
				sourceByURL[sourceURL] = derived
				sources = append(sources, derived)
			}
			sourceURLs = append(sourceURLs, sourceURL)
		}
		metrics := make([]presentationMetric, 0, len(slide.Metrics))
		for _, metric := range slide.Metrics {
			metrics = append(metrics, presentationMetric{
				Label: strings.TrimSpace(metric.Label), Value: strings.TrimSpace(metric.Value), Note: strings.TrimSpace(metric.Note),
			})
		}
		var nativeContent json.RawMessage
		if slide.PresentonContentJSON != "" {
			if _, err := parsePresentonContent(slide.PresentonContentJSON); err != nil {
				return nil, nil, fmt.Errorf("$.slides[%d].presenton_content_json: %v", index, err)
			}
			nativeContent = json.RawMessage(slide.PresentonContentJSON)
		}
		slides = append(slides, presentationSlide{
			ID: id, Title: slide.Title, Eyebrow: strings.TrimSpace(slide.Eyebrow),
			Summary: strings.TrimSpace(slide.Summary), Bullets: trimStrings(slide.Bullets),
			Metrics: metrics, Table: table, Quote: strings.TrimSpace(slide.Quote),
			SourceURLs: sourceURLs, Layout: slide.Layout,
			PresentonLayoutID: strings.TrimSpace(slide.PresentonLayoutID), PresentonContent: nativeContent,
		})
	}
	return slides, sources, nil
}

func validPresentationURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return strings.Contains(host, ".") &&
		!strings.HasSuffix(host, ".local") &&
		!strings.HasSuffix(host, ".internal") &&
		host != "localhost"
}

func trimStrings(values []string) []string {
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = strings.TrimSpace(value)
	}
	return out
}

func trimRows(rows [][]string) [][]string {
	out := make([][]string, len(rows))
	for index, row := range rows {
		out[index] = trimStrings(row)
	}
	return out
}

func renderPresentationHTML(title string, deckJSON []byte) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="zh-Hant">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <title>%s</title>
  <style>%s</style>
</head>
<body>
  <div data-presentation-root></div>
  <script>window.MEADOW_PRESENTATION = %s;</script>
  <script>%s</script>
</body>
</html>
`, html.EscapeString(title), presentationCSS, deckJSON, presentationJavaScript)
}

func renderPresentationTypeScript(deckJSON []byte) string {
	return fmt.Sprintf(`// Editable source for the Meadow web presentation.
// Run presentation_artifact again after changing structured content.
export interface MeadowPresentation {
  artifact_id: string;
  title: string;
  subtitle?: string;
  author?: string;
  generatedAt: string;
  slides: Array<{
    id: string;
    title: string;
    eyebrow?: string;
    summary?: string;
    bullets?: string[];
    metrics?: Array<{ label: string; value: string; note?: string }>;
    table?: { columns: string[]; rows: string[][] };
    quote?: string;
    sourceUrls?: string[];
    layout?: "cover" | "statement" | "metrics" | "comparison" | "table" | "quote";
    presentonLayoutId?: string;
    presentonContent?: Record<string, unknown>;
  }>;
  sources?: Array<{ title: string; url: string; excerpt?: string; accessedAt?: string }>;
}

export const presentation = %s satisfies MeadowPresentation;
`, deckJSON)
}
