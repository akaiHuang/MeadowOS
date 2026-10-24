package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ResearchArtifact renders a browsable local research page from structured
// findings. Small local models are better at extracting short JSON records than
// hand-writing a long HTML artifact, so the runtime owns the template, storage,
// and follow-up selection hooks.
type ResearchArtifact struct {
	WS  *Workspace
	Now func() time.Time
}

func (t *ResearchArtifact) Name() string { return "research_artifact" }
func (t *ResearchArtifact) Description() string {
	return "Create or update a local interactive research artifact page from structured JSON items and sources."
}
func (t *ResearchArtifact) Kind() Kind { return KindEdit }

func (t *ResearchArtifact) Schema() map[string]any {
	return obj(map[string]any{
		"artifact_id":  str("Optional stable artifact id for follow-up updates, e.g. tokyo-trip or outfit-rainy-day."),
		"title":        str("Short page title."),
		"summary":      str("Short summary shown at the top of the page."),
		"items_json":   str(`JSON array string of cards. Each item may include id, title, description, source_url, image_path, image_url, price, reason, tags.`),
		"sources_json": str(`Optional JSON array string of sources. Each source may include title, url, excerpt.`),
		"output_path":  str("Optional HTML output path. Default is .meadow/research/<artifact_id>/artifact.html."),
	}, "title", "items_json")
}

type researchArtifactItem struct {
	ID          string   `json:"id,omitempty"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	SourceURL   string   `json:"source_url,omitempty"`
	ImagePath   string   `json:"image_path,omitempty"`
	ImageURL    string   `json:"image_url,omitempty"`
	Price       string   `json:"price,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

type researchArtifactSource struct {
	Title   string `json:"title,omitempty"`
	URL     string `json:"url,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

type renderedResearchItem struct {
	researchArtifactItem
	ImageSrc string
	TagsText string
}

type researchArtifactPage struct {
	ArtifactID string
	Title      string
	Summary    string
	Items      []renderedResearchItem
	Sources    []researchArtifactSource
	DataJSON   template.JS
}

func (t *ResearchArtifact) Execute(ctx context.Context, args map[string]any) Result {
	if t.WS == nil {
		return errResultf("workspace is required")
	}
	title := strings.TrimSpace(strArg(args, "title"))
	if title == "" {
		return errResultf("title is required")
	}
	items, err := parseResearchItems(strArg(args, "items_json"))
	if err != nil {
		return errResultf("items_json: %v", err)
	}
	if len(items) == 0 {
		return errResultf("items_json must contain at least one item")
	}
	artifactID := sanitizeArtifactID(strArg(args, "artifact_id"))
	if artifactID == "" {
		artifactID = sanitizeArtifactID(title)
	}
	if artifactID == "" {
		artifactID = "research"
	}
	now := time.Now()
	if t.Now != nil {
		now = t.Now()
	}
	if strArg(args, "artifact_id") == "" {
		artifactID = fmt.Sprintf("%s-%s", artifactID, now.Format("20060102-150405"))
	}

	sources, err := parseResearchSources(strArg(args, "sources_json"))
	if err != nil {
		return errResultf("sources_json: %v", err)
	}
	items = normalizeResearchItems(items)
	if len(sources) == 0 {
		sources = deriveResearchSources(items)
	}

	storeDir, err := t.WS.Resolve(filepath.Join(".meadow", "research", artifactID))
	if err != nil {
		return errResult(err)
	}
	outputPath := strArg(args, "output_path")
	htmlPath := filepath.Join(storeDir, "artifact.html")
	if strings.TrimSpace(outputPath) != "" {
		htmlPath, err = t.WS.Resolve(outputPath)
		if err != nil {
			return errResult(err)
		}
	}
	for _, path := range []string{
		filepath.Join(storeDir, "items.json"),
		filepath.Join(storeDir, "sources.json"),
		filepath.Join(storeDir, "brief.json"),
		htmlPath,
	} {
		if err := t.WS.CheckWritable(path); err != nil {
			return errResult(err)
		}
	}
	if !isUnderDir(htmlPath, filepath.Join(t.WS.Root, ".meadow", "research")) {
		if err := t.WS.CheckPriorRead(htmlPath); err != nil {
			return errResult(err)
		}
	}

	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return errResult(err)
	}
	if err := os.MkdirAll(filepath.Dir(htmlPath), 0o755); err != nil {
		return errResult(err)
	}

	itemsJSON, _ := json.MarshalIndent(items, "", "  ")
	sourcesJSON, _ := json.MarshalIndent(sources, "", "  ")
	if err := os.WriteFile(filepath.Join(storeDir, "items.json"), itemsJSON, 0o644); err != nil {
		return errResult(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "sources.json"), sourcesJSON, 0o644); err != nil {
		return errResult(err)
	}
	meta := map[string]any{
		"artifact_id":  artifactID,
		"title":        title,
		"summary":      strings.TrimSpace(strArg(args, "summary")),
		"created_at":   now.UTC().Format(time.RFC3339),
		"html_path":    htmlPath,
		"item_count":   len(items),
		"source_count": len(sources),
	}
	metaJSON, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(storeDir, "brief.json"), metaJSON, 0o644); err != nil {
		return errResult(err)
	}

	htmlContent, err := t.renderHTML(artifactID, title, strArg(args, "summary"), items, sources, filepath.Dir(htmlPath))
	if err != nil {
		return errResult(err)
	}
	if err := os.WriteFile(htmlPath, []byte(htmlContent), 0o644); err != nil {
		return errResult(err)
	}
	for _, p := range []string{
		filepath.Join(storeDir, "items.json"),
		filepath.Join(storeDir, "sources.json"),
		filepath.Join(storeDir, "brief.json"),
		htmlPath,
	} {
		t.WS.NoteRead(p)
	}

	msg := fmt.Sprintf("Research artifact created: %s (artifact_id=%s, items=%d, sources=%d)", htmlPath, artifactID, len(items), len(sources))
	return Result{LLMContent: msg, Display: msg}
}

func parseResearchItems(raw string) ([]researchArtifactItem, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty JSON array")
	}
	var items []researchArtifactItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	return items, nil
}

func parseResearchSources(raw string) ([]researchArtifactSource, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var sources []researchArtifactSource
	if err := json.Unmarshal([]byte(raw), &sources); err != nil {
		return nil, err
	}
	return sources, nil
}

func normalizeResearchItems(items []researchArtifactItem) []researchArtifactItem {
	for i := range items {
		items[i].Title = strings.TrimSpace(items[i].Title)
		items[i].Description = strings.TrimSpace(items[i].Description)
		items[i].SourceURL = strings.TrimSpace(items[i].SourceURL)
		items[i].ImagePath = strings.TrimSpace(items[i].ImagePath)
		items[i].ImageURL = strings.TrimSpace(items[i].ImageURL)
		items[i].Price = strings.TrimSpace(items[i].Price)
		items[i].Reason = strings.TrimSpace(items[i].Reason)
		if strings.TrimSpace(items[i].ID) == "" {
			items[i].ID = fmt.Sprintf("item-%02d", i+1)
			if s := sanitizeArtifactID(items[i].Title); s != "" {
				items[i].ID = s
			}
		} else {
			items[i].ID = sanitizeArtifactID(items[i].ID)
		}
		if items[i].Title == "" {
			items[i].Title = fmt.Sprintf("Item %d", i+1)
		}
	}
	return items
}

func deriveResearchSources(items []researchArtifactItem) []researchArtifactSource {
	seen := map[string]bool{}
	var out []researchArtifactSource
	for _, item := range items {
		if item.SourceURL == "" || seen[item.SourceURL] {
			continue
		}
		seen[item.SourceURL] = true
		title := item.Title
		if title == "" {
			title = item.SourceURL
		}
		out = append(out, researchArtifactSource{Title: title, URL: item.SourceURL})
	}
	return out
}

func (t *ResearchArtifact) renderHTML(artifactID, title, summary string, items []researchArtifactItem, sources []researchArtifactSource, htmlDir string) (string, error) {
	rendered := make([]renderedResearchItem, 0, len(items))
	for _, item := range items {
		rendered = append(rendered, renderedResearchItem{
			researchArtifactItem: item,
			ImageSrc:             t.imageSrc(item, htmlDir),
			TagsText:             strings.Join(item.Tags, ", "),
		})
	}
	data, _ := json.Marshal(map[string]any{
		"artifact_id": artifactID,
		"title":       title,
		"items":       items,
		"sources":     sources,
	})
	page := researchArtifactPage{
		ArtifactID: artifactID,
		Title:      strings.TrimSpace(title),
		Summary:    strings.TrimSpace(summary),
		Items:      rendered,
		Sources:    sources,
		DataJSON:   template.JS(data),
	}
	var b bytes.Buffer
	if err := researchArtifactTemplate.Execute(&b, page); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (t *ResearchArtifact) imageSrc(item researchArtifactItem, htmlDir string) string {
	if isHTTPURL(item.ImageURL) {
		return item.ImageURL
	}
	raw := item.ImagePath
	if raw == "" {
		raw = item.ImageURL
	}
	if raw == "" {
		return ""
	}
	if isHTTPURL(raw) {
		return raw
	}
	if t.WS == nil {
		return raw
	}
	resolved, err := t.WS.Resolve(raw)
	if err != nil {
		return raw
	}
	rel, err := filepath.Rel(htmlDir, resolved)
	if err != nil {
		return resolved
	}
	return filepath.ToSlash(rel)
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "http://") || strings.HasPrefix(strings.TrimSpace(s), "https://")
}

var artifactIDRe = regexp.MustCompile(`[^a-z0-9._-]+`)

func sanitizeArtifactID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = artifactIDRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-._")
	if len(s) > 72 {
		s = strings.Trim(s[:72], "-._")
	}
	return s
}

func isUnderDir(path, dir string) bool {
	path = filepath.Clean(path)
	dir = filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

var researchArtifactTemplate = template.Must(template.New("research_artifact").Funcs(template.FuncMap{
	"safeURL": func(s string) string {
		s = strings.TrimSpace(s)
		if isHTTPURL(s) || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "/") {
			return s
		}
		return ""
	},
	"sortedSources": func(sources []researchArtifactSource) []researchArtifactSource {
		out := append([]researchArtifactSource{}, sources...)
		sort.SliceStable(out, func(i, j int) bool {
			return strings.ToLower(out[i].URL) < strings.ToLower(out[j].URL)
		})
		return out
	},
}).Parse(`<!DOCTYPE html>
<html lang="zh-TW">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>{{.Title}}</title>
  <style>
    :root { color-scheme: light dark; --bg: #f7f5ef; --ink: #171716; --muted: #68645d; --line: #d8d1c2; --panel: #fffdf7; --accent: #1d7f56; }
    @media (prefers-color-scheme: dark) { :root { --bg: #10110f; --ink: #f2efe7; --muted: #a7a195; --line: #31342f; --panel: #191b18; --accent: #5bd28f; } }
    * { box-sizing: border-box; }
    body { margin: 0; background: var(--bg); color: var(--ink); font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    main { width: min(1120px, calc(100vw - 32px)); margin: 0 auto; padding: 40px 0 56px; }
    header { display: grid; gap: 12px; border-bottom: 1px solid var(--line); padding-bottom: 22px; margin-bottom: 24px; }
    h1 { margin: 0; font-size: clamp(30px, 5vw, 56px); line-height: 1.02; letter-spacing: 0; }
    .summary { margin: 0; max-width: 820px; color: var(--muted); font-size: 17px; }
    .meta { display: flex; flex-wrap: wrap; gap: 10px; color: var(--muted); font-size: 13px; }
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 14px; }
    .card { display: grid; gap: 12px; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 14px; cursor: pointer; min-height: 100%; }
    .card:hover, .card[data-selected="true"] { border-color: var(--accent); box-shadow: 0 0 0 1px color-mix(in srgb, var(--accent) 45%, transparent); }
    .image { width: 100%; aspect-ratio: 4 / 3; object-fit: cover; border-radius: 6px; border: 1px solid var(--line); background: color-mix(in srgb, var(--panel) 80%, var(--ink)); }
    h2 { margin: 0; font-size: 18px; line-height: 1.25; letter-spacing: 0; }
    p { margin: 0; }
    .desc, .reason, .tags, .source, .price { color: var(--muted); }
    .price { font-weight: 700; color: var(--accent); }
    a { color: var(--accent); overflow-wrap: anywhere; }
    .sources { margin-top: 30px; padding-top: 20px; border-top: 1px solid var(--line); }
    .sources h2 { margin-bottom: 12px; }
    .sources ol { padding-left: 20px; }
    .sources li { margin: 0 0 10px; }
    .selection-note { position: sticky; bottom: 12px; margin-top: 22px; padding: 10px 12px; border: 1px solid var(--line); border-radius: 8px; background: color-mix(in srgb, var(--panel) 92%, transparent); color: var(--muted); font-size: 13px; backdrop-filter: blur(12px); }
  </style>
</head>
<body>
  <main data-meadow-artifact-id="{{.ArtifactID}}">
    <header>
      <div class="meta"><span>Meadow Research</span><span>{{.ArtifactID}}</span><span>{{len .Items}} items</span><span>{{len .Sources}} sources</span></div>
      <h1>{{.Title}}</h1>
      {{if .Summary}}<p class="summary">{{.Summary}}</p>{{end}}
    </header>
    <section class="grid" aria-label="Research items">
      {{range .Items}}
      <article class="card" tabindex="0" data-meadow-card="true" data-meadow-artifact-id="{{$.ArtifactID}}" data-meadow-item-id="{{.ID}}" data-source-url="{{.SourceURL}}" data-title="{{.Title}}" data-image-path="{{.ImagePath}}">
        {{if .ImageSrc}}<img class="image" src="{{safeURL .ImageSrc}}" alt="{{.Title}}">{{end}}
        <h2>{{.Title}}</h2>
        {{if .Price}}<p class="price">{{.Price}}</p>{{end}}
        {{if .Description}}<p class="desc">{{.Description}}</p>{{end}}
        {{if .Reason}}<p class="reason">{{.Reason}}</p>{{end}}
        {{if .TagsText}}<p class="tags">{{.TagsText}}</p>{{end}}
        {{if .SourceURL}}<p class="source"><a href="{{safeURL .SourceURL}}" target="_blank" rel="noopener noreferrer">Source</a></p>{{end}}
      </article>
      {{end}}
    </section>
    {{if .Sources}}
    <section class="sources">
      <h2>Sources</h2>
      <ol>
        {{range sortedSources .Sources}}
        <li>{{if .URL}}<a href="{{safeURL .URL}}" target="_blank" rel="noopener noreferrer">{{if .Title}}{{.Title}}{{else}}{{.URL}}{{end}}</a>{{else}}{{.Title}}{{end}}{{if .Excerpt}}<br><span class="desc">{{.Excerpt}}</span>{{end}}</li>
        {{end}}
      </ol>
    </section>
    {{end}}
    <div class="selection-note" id="meadow-selection-note">Click a card, then ask Meadow to refine it.</div>
  </main>
  <script>
    window.__MEADOW_RESEARCH_ARTIFACT = {{.DataJSON}};
    function setMeadowSelection(card) {
      document.querySelectorAll('[data-meadow-card]').forEach(function(el) { el.dataset.selected = 'false'; });
      card.dataset.selected = 'true';
      window.__MEADOW_SELECTED_RESEARCH_ITEM = {
        artifact_id: card.dataset.meadowArtifactId || '',
        item_id: card.dataset.meadowItemId || '',
        title: card.dataset.title || '',
        source_url: card.dataset.sourceUrl || '',
        image_path: card.dataset.imagePath || ''
      };
      var note = document.getElementById('meadow-selection-note');
      if (note) note.textContent = 'Selected: ' + (window.__MEADOW_SELECTED_RESEARCH_ITEM.title || window.__MEADOW_SELECTED_RESEARCH_ITEM.item_id);
      window.dispatchEvent(new CustomEvent('meadow:research-selection', { detail: window.__MEADOW_SELECTED_RESEARCH_ITEM }));
    }
    document.querySelectorAll('[data-meadow-card]').forEach(function(card) {
      card.addEventListener('click', function() { setMeadowSelection(card); });
      card.addEventListener('keydown', function(event) {
        if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); setMeadowSelection(card); }
      });
    });
  </script>
</body>
</html>
`))
