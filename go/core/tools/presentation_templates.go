package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const presentonCatalogMaxBytes = 256 << 10
const presentonContentMaxBytes = 128 << 10

// PresentationTemplates exposes the installed renderer's catalog, rather than
// an independently maintained list or a harness-selected design workflow.
type PresentationTemplates struct {
	PresentonURL   string
	PresentonToken string
	HTTP           *http.Client
}

func (t *PresentationTemplates) Name() string { return "presentation_templates" }
func (t *PresentationTemplates) Description() string {
	return `Create PPTX/HTML from supplied prose, notes, research or discussion. Initial compact discovery (operation=markdown) is exactly {"operation":"markdown"}: omit template_id and layout_id, never invent "auto" or a default ID. It returns available compact templates/layouts, complete examples and capacities. Author Markdown yourself, choose a fitting template/layout, then call presentation_from_markdown without separate schema lookups. list/layouts/schema exposes the broader native catalog for other designs. Discovery never creates a deck.`
}
func (t *PresentationTemplates) Kind() Kind                { return KindReadOnly }
func (t *PresentationTemplates) CompleteObservation() bool { return true }
func (t *PresentationTemplates) Schema() map[string]any {
	return obj(map[string]any{
		"operation":   map[string]any{"type": "string", "enum": []string{"markdown", "list", "layouts", "schema"}, "description": `Initial compact call: {"operation":"markdown"}. Broader template list: {"operation":"list"}. Both omit selectors. layouts requires template_id and forbids layout_id. schema requires both exact IDs previously returned by list/layouts; it does not search for a topic or alias.`},
		"template_id": str(`For initial markdown discovery and every list call, omit this key entirely. No "auto" or guessed default ID. layouts/schema require an observed installed ID. A selected markdown template must advertise compatible markdown_adapters; ordinary native templates may not support Markdown.`),
		"layout_id":   str("Use only for schema, with an exact layout ID returned by layouts for the selected template. Omit for markdown/list/layouts; do not guess topic names such as metrics."),
	}, "operation")
}

type presentonCatalogTemplate struct {
	ID                            string                         `json:"id"`
	Name                          string                         `json:"name,omitempty"`
	Description                   string                         `json:"description,omitempty"`
	LayoutCount                   int                            `json:"layout_count"`
	Thumbnail                     string                         `json:"thumbnail,omitempty"`
	PreviewURL                    string                         `json:"preview_url,omitempty"`
	IsDefault                     bool                           `json:"is_default,omitempty"`
	Fonts                         json.RawMessage                `json:"fonts,omitempty"`
	Theme                         json.RawMessage                `json:"theme,omitempty"`
	MarkdownAdapters              []presentonMarkdownAdapterInfo `json:"markdown_adapters,omitempty"`
	TemplateLayoutsSHA256         string                         `json:"template_layouts_sha256,omitempty"`
	MarkdownAdapterManifestSHA256 string                         `json:"markdown_adapter_manifest_sha256,omitempty"`
}

type presentonCatalogLayout struct {
	ID                     string                        `json:"id"`
	Description            string                        `json:"description,omitempty"`
	RequiredComponents     json.RawMessage               `json:"required_components,omitempty"`
	RequiredComponentCount *int                          `json:"required_component_count,omitempty"`
	FieldCount             *int                          `json:"field_count,omitempty"`
	ElementTypes           []string                      `json:"element_types,omitempty"`
	ContentTypes           []string                      `json:"content_types,omitempty"`
	TextLengthRange        json.RawMessage               `json:"text_length_range,omitempty"`
	RepeatBounds           json.RawMessage               `json:"repeat_bounds,omitempty"`
	Editable               *bool                         `json:"editable,omitempty"`
	EditableAssetCount     *int                          `json:"editable_asset_count,omitempty"`
	SupportsDirectRender   *bool                         `json:"supports_direct_render,omitempty"`
	SupportsMarkdownRender *bool                         `json:"supports_markdown_render,omitempty"`
	MarkdownAdapter        *presentonMarkdownAdapterInfo `json:"markdown_adapter,omitempty"`
}

type presentonCatalogAsset struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

type presentonCatalogTextFrame struct {
	Path       string   `json:"path"`
	Name       string   `json:"name,omitempty"`
	Width      *float64 `json:"width"`
	Height     *float64 `json:"height"`
	FontSize   *float64 `json:"font_size"`
	FontFamily string   `json:"font_family,omitempty"`
	LineHeight *float64 `json:"line_height"`
}

type presentonCatalogResponse struct {
	Operation                     string                         `json:"operation"`
	Total                         int                            `json:"total,omitempty"`
	Templates                     []presentonCatalogTemplate     `json:"templates,omitempty"`
	Template                      *presentonCatalogTemplate      `json:"template,omitempty"`
	Layouts                       []presentonCatalogLayout       `json:"layouts,omitempty"`
	Layout                        *presentonCatalogLayout        `json:"layout,omitempty"`
	Schema                        map[string]any                 `json:"schema,omitempty"`
	TextFrames                    []presentonCatalogTextFrame    `json:"text_frames,omitempty"`
	EditableAssets                []presentonCatalogAsset        `json:"editable_assets,omitempty"`
	SupportsDirectRender          *bool                          `json:"supports_direct_render,omitempty"`
	Usage                         []string                       `json:"usage"`
	RendererSHA256                string                         `json:"renderer_sha256,omitempty"`
	TemplateSHA256                string                         `json:"template_sha256,omitempty"`
	TemplateLayoutsSHA256         string                         `json:"template_layouts_sha256,omitempty"`
	MarkdownAdapterManifestSHA256 string                         `json:"markdown_adapter_manifest_sha256,omitempty"`
	MarkdownAdapters              []presentonMarkdownAdapterInfo `json:"markdown_adapters,omitempty"`
	MarkdownAdapter               *presentonMarkdownAdapterInfo  `json:"markdown_adapter,omitempty"`
}

var presentonToolUsage = []string{
	"For supplied prose, research or discussion, the compact path needs one presentation_templates operation=markdown discovery, then presentation_from_markdown. The input need not already be Markdown: author it using the returned complete examples and capacities. You choose among the compact available templates/layouts; no layouts/schema calls or separate preflight are needed for that path.",
	"The broader native catalog remains available for designs outside the compact layouts. For that path, choose a template from list, inspect its layouts and request schemas only for selected layouts. Reuse already returned contracts rather than repeating discovery without a missing requirement. Choose using the audience, material and constraints, not only a topic-matching name.",
	"For presentation_artifact use engine=presenton and one explicit template for the whole deck. Every slide requires its own title and presenton_layout_id belonging to that same template. Supply presenton_content_json as a STRING encoding a strict JSON object matching the selected schema, not a raw object. Rendering preserves exactly that content without a second model rewrite.",
	"Keep literal facts and supported quantities; never invent a chart, slogan, source or placeholder to fill a layout. Choose another suitable layout when the material does not support a required field. Omit sources or use [] for local discussion; external HTTP(S) content needs an observed source ledger.",
	"Advanced JSON requires supports_direct_render=true; unresolved editable assets are rejected. Markdown photo adapters accept only explicit local supplied PNG/JPEG or labelled placeholder:photo blocks; image briefs remain pending, with no generation or network fetch.",
	"Read the returned presenton.json to inspect actual rendered content, then inspect ALL returned slide PNG paths with inspect_image detail=high or original. Check text, CJK wrapping, hierarchy, fit and factual fidelity. Technical export success does not prove content or visual quality; revise selected content/layout if the evidence requires it.",
	"HTML and real PPTX are downloadable artifacts; use /session for standalone delivery. Builtin typed HTML remains available with engine=builtin and no native Presenton fields. Tool availability does not authorize creating a presentation unless the user's task calls for it.",
}

var presentonMarkdownToolUsage = []string{
	"This compact contract can turn supplied prose, research or discussion into PPTX/HTML; input need not already be Markdown. You choose a returned template and compatible aliases, author the Markdown, then call presentation_from_markdown directly; no layouts/schema lookup or separate preflight is needed. markdown_example is a complete valid one-slide deck: replace its sample words with your content. H1 is metadata, not a slide; every H2 starts a complete slide. Put at most one layout comment immediately below H2, before content. All text is literal; unsupported blocks or overfull slots fail without truncation.",
	"grammar.items=ordered_list: list-only H2 page; sections: a lead then H3 heading/body sections; none: title/lead without item sections. Counts and text_limits are exact. Effective Markdown string minimum is one; maxima still apply.",
	"grammar.images=required needs one standalone ![literal image description](/session/path.png) or workspace path. Explicit ![literal description](placeholder:photo) renders a labelled replaceable placeholder. PNG/JPEG only, 2 MiB/image, 8 MiB total, 8 unique images, 16 MP decoded. Never use HTTP(S), file/data URLs or guessed sample imagery. image-brief fences remain pending plans.",
	"External claims need their observed source ledger and HTTP(S) citations; local discussion may omit sources. Read actual presenton.json and inspect every returned PNG before claiming content/design acceptance; HTML and editable PPTX are deliverables.",
}

func (t *PresentationTemplates) Execute(ctx context.Context, args map[string]any) Result {
	operation := strings.TrimSpace(strArg(args, "operation"))
	templateID := strings.TrimSpace(strArg(args, "template_id"))
	layoutID := strings.TrimSpace(strArg(args, "layout_id"))
	if len(templateID) > 200 || len(layoutID) > 200 {
		return errResultf("template_id and layout_id must be bounded catalog IDs")
	}
	switch operation {
	case "markdown":
		if layoutID != "" {
			return errResultf(`operation=markdown does not accept layout_id. Initial compact discovery is exactly {"operation":"markdown"}; omit both selectors. A selected template_id is allowed only for a template with advertised Markdown support; no selection was changed`)
		}
	case "list":
		if templateID != "" || layoutID != "" {
			return errResultf(`operation=list does not accept template_id or layout_id. To list installed templates, call exactly {"operation":"list"}. To list layouts in one selected template, use operation=layouts with its observed template_id`)
		}
	case "layouts":
		if templateID == "" || layoutID != "" {
			return errResultf(`operation=layouts requires template_id and no layout_id. Obtain an exact template ID with {"operation":"list"}; then request its layouts. Initial compact discovery instead uses exactly {"operation":"markdown"}`)
		}
	case "schema":
		if templateID == "" || layoutID == "" {
			if call := presentationLayoutsRecoveryCall(templateID, t.PresentonToken); call != "" {
				return errResultf("operation=schema requires template_id and layout_id. To obtain exact layout IDs for the selected template, call %s, then request schema with a returned layout_id; do not guess aliases or topic names", call)
			}
			return errResultf(`operation=schema requires template_id and layout_id. Discover exact IDs via {"operation":"list"}, then operation=layouts with a returned template_id. Initial compact discovery instead uses exactly {"operation":"markdown"}`)
		}
	default:
		return errResultf("operation must be markdown, list, layouts or schema")
	}
	config, err := loadPresentonEndpoint("presenton", t.PresentonURL, t.PresentonToken)
	if err != nil {
		return errResult(err)
	}
	request := map[string]any{"operation": operation}
	if templateID != "" {
		request["template_id"] = templateID
	}
	if layoutID != "" {
		request["layout_id"] = layoutID
	}
	payload, _ := json.Marshal(request)
	responseLimit := int64(presentonCatalogMaxBytes)
	if operation == "markdown" {
		responseLimit = 16 << 10
	}
	raw, err := presentonPost(ctx, config, t.HTTP, "/catalog", payload, responseLimit, 30*time.Second)
	if err != nil {
		// Match the sanitized structured HTTP field prefix, not text inside
		// an unrelated error body. Transport/auth failures retain their own
		// meaning; no retry or replacement selection happens here.
		if operation == "markdown" && templateID != "" && presentationCompactSelectorDiagnostic(err) {
			return errResultf(`Compact discovery rejected the explicit template; it was not substituted. To discover supported compact templates, omit template_id and layout_id and call exactly {"operation":"markdown"}. The original diagnostic's list advice applies to the broader native catalog. Original diagnostic: %v`, err)
		}
		return errResult(err)
	}
	var catalog presentonCatalogResponse
	if err := json.Unmarshal(raw, &catalog); err != nil || catalog.Operation != operation {
		return errResultf("invalid Presenton catalog response")
	}
	if len(catalog.Templates) > 128 || len(catalog.Layouts) > 128 || len(catalog.Usage) > 12 {
		return errResultf("Presenton catalog is not bounded; request one selected layout schema")
	}
	switch operation {
	case "markdown":
		if len(catalog.MarkdownAdapters) > 8 || catalog.Template == nil || catalog.Template.ID == "" || catalog.Template.LayoutCount < 1 || (templateID != "" && catalog.Template.ID != templateID) {
			return errResultf("Presenton returned no bounded usable Markdown template")
		}
		if len(catalog.MarkdownAdapters) == 0 {
			manifest, manifestErr := loadMarkdownAdapters()
			if manifestErr != nil {
				return errResult(manifestErr)
			}
			if catalog.Template.ID == manifest.TemplateID {
				return errResultf("Previously supported compact template returned no verified Markdown adapters; its installed identity may have changed. No compatible contract was advertised and no selection was substituted")
			}
			if call := presentationLayoutsRecoveryCall(templateID, config.Token); call != "" {
				return errResultf(`Selected installed template returned no verified Markdown adapters compatible with this importer; it was not substituted. To discover supported compact templates, call exactly {"operation":"markdown"}, omitting both selectors. To retain your selected native style, call %s and use its actual layouts/schemas with presentation_artifact`, call)
			}
			return errResultf("Presenton returned no bounded usable Markdown template")
		}
		for _, adapter := range catalog.MarkdownAdapters {
			if !adapter.SupportsMarkdownRender || adapter.Alias == "" || len(adapter.TextLimits) == 0 || len(adapter.TextLimits) > 8 {
				return errResultf("Presenton returned an incomplete Markdown authoring contract")
			}
			for _, limit := range adapter.TextLimits {
				if limit.Min != 1 || limit.Max < 1 || (limit.Unit != "characters" && limit.Unit != "bytes") {
					return errResultf("Presenton returned invalid literal Markdown text capacity")
				}
			}
		}
		// Keep only authoring metadata: native schemas, sample assets, frames
		// and the large general layout tree cannot inflate the MD handoff.
		manifest, err := loadMarkdownAdapters()
		if err != nil {
			return errResult(err)
		}
		if err := validateMarkdownCatalog(catalog, manifest); err != nil {
			return errResult(err)
		}
		for i := range catalog.MarkdownAdapters {
			// Derive examples locally after matching the live contract; never
			// trust an upstream example to describe the installed compiler.
			catalog.MarkdownAdapters[i].MarkdownExample = markdownAdapterExample(catalog.MarkdownAdapters[i].Alias)
		}
		catalog = presentonCatalogResponse{Operation: operation, Template: &presentonCatalogTemplate{ID: catalog.Template.ID, Name: catalog.Template.Name, Description: catalog.Template.Description, LayoutCount: len(catalog.MarkdownAdapters)}, MarkdownAdapters: catalog.MarkdownAdapters, TemplateLayoutsSHA256: catalog.TemplateLayoutsSHA256, MarkdownAdapterManifestSHA256: catalog.MarkdownAdapterManifestSHA256}
	case "list":
		if len(catalog.Templates) == 0 {
			return errResultf("Presenton returned no installed templates")
		}
		for _, template := range catalog.Templates {
			if template.ID == "" || template.LayoutCount < 1 {
				return errResultf("invalid installed Presenton template metadata")
			}
		}
	case "layouts":
		if catalog.Template == nil || catalog.Template.ID != templateID || len(catalog.Layouts) == 0 {
			return errResultf("Presenton template/layout catalog mismatch")
		}
		for _, layout := range catalog.Layouts {
			if layout.ID == "" {
				return errResultf("Presenton returned an invalid native layout ID")
			}
		}
	case "schema":
		if catalog.Template == nil || catalog.Template.ID != templateID || catalog.Layout == nil || catalog.Layout.ID != layoutID || catalog.Schema == nil || catalog.Schema["type"] != "object" {
			return errResultf("Presenton selected layout schema mismatch")
		}
	}
	for _, guidance := range catalog.Usage {
		if len(guidance) > 2048 {
			return errResultf("Presenton catalog usage guidance exceeds its bound")
		}
	}
	if operation == "markdown" {
		catalog.Usage = append([]string(nil), presentonMarkdownToolUsage...)
	} else {
		// State both capability paths before the renderer's native-only
		// instructions. A prose input does not require advanced schema
		// discovery, and choosing a compact route remains the model's decision.
		upstreamUsage := catalog.Usage
		catalog.Usage = append([]string(nil), presentonToolUsage...)
		for _, guidance := range upstreamUsage {
			catalog.Usage = append(catalog.Usage, "Advanced native renderer guidance (presentation_artifact): "+guidance)
		}
	}
	// Project only the public catalog contract. A renderer accidentally adding
	// top-level provider/configuration fields must not make them model-visible.
	encoded, err := json.Marshal(catalog)
	if err != nil || len(encoded) > presentonCatalogMaxBytes || (operation == "markdown" && len(encoded) > 8<<10) {
		return errResultf("Presenton catalog exceeds its model response bound")
	}
	if len(config.Token) >= 8 && strings.Contains(string(encoded), config.Token) {
		return errResultf("Presenton catalog contained private configuration; discovery was rejected")
	}
	return Result{LLMContent: string(encoded), Display: fmt.Sprintf("Presenton catalog: %s (%d templates, %d layouts)", operation, len(catalog.Templates), len(catalog.Layouts))}
}

// Recovery examples preserve a supplied native selection only when it is a
// bounded catalog identifier. They never copy arbitrary input or credentials
// into tool guidance, and are examples only: no HTTP request is made here.
func presentationLayoutsRecoveryCall(templateID, privateToken string) string {
	if templateID == "" || len(templateID) > 120 || (privateToken != "" && strings.Contains(templateID, privateToken)) {
		return ""
	}
	for i, r := range templateID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || (i > 0 && (r == '.' || r == '-')) {
			continue
		}
		return ""
	}
	call, _ := json.Marshal(map[string]string{"operation": "layouts", "template_id": templateID})
	return string(call)
}

func presentationCompactSelectorDiagnostic(err error) bool {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity} {
		if strings.HasPrefix(err.Error(), fmt.Sprintf("Presenton catalog failed (HTTP %d) at template_id:", status)) {
			return true
		}
	}
	return false
}

func validatePresentationEngineSlides(config presentonEndpointConfig, slides []presentationSlideInput, template string) error {
	if config.URL != "" && strings.TrimSpace(template) == "" {
		return fmt.Errorf("$.template: choose an explicit installed template ID using presentation_templates; Presenton has no default template")
	}
	if config.URL != "" && len(template) > 200 {
		return fmt.Errorf("$.template: installed template ID exceeds 200 bytes")
	}
	for index, slide := range slides {
		layout := strings.TrimSpace(slide.PresentonLayoutID)
		content := strings.TrimSpace(slide.PresentonContentJSON)
		if config.URL == "" {
			if layout != "" || content != "" {
				return fmt.Errorf("$.slides[%d]: native Presenton fields require engine=presenton with an installed renderer; builtin HTML cannot render them", index)
			}
			continue
		}
		if layout == "" || content == "" {
			return fmt.Errorf("$.slides[%d]: every Presenton slide requires presenton_layout_id and presenton_content_json from the selected live schema; mixed typed/native slides are unsupported", index)
		}
		if len(layout) > 200 {
			return fmt.Errorf("$.slides[%d].presenton_layout_id: catalog ID exceeds 200 bytes", index)
		}
		if slide.Layout != "" || slide.Eyebrow != "" || slide.Summary != "" || len(slide.Bullets) > 0 || len(slide.Metrics) > 0 || slide.Table != nil || slide.Quote != "" {
			return fmt.Errorf("$.slides[%d]: builtin visual fields cannot be mixed with native Presenton content; place rendered text/data inside presenton_content_json using the selected schema", index)
		}
		if _, err := parsePresentonContent(content); err != nil {
			return fmt.Errorf("$.slides[%d].presenton_content_json: %v", index, err)
		}
	}
	return nil
}

// Content JSON has no transport repair: duplicate keys, malformed JSON and
// non-object values are ambiguous authoring, so the model receives an error.
func parsePresentonContent(raw string) (map[string]any, error) {
	if len(raw) > presentonContentMaxBytes {
		return nil, fmt.Errorf("JSON content exceeds 128 KiB")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	value, err := strictPresentonJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("must contain one strict JSON object with no trailing data")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must contain a JSON object, not an array, scalar or null")
	}
	return object, nil
}

func strictPresentonJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("JSON content nesting exceeds 64 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("must contain valid strict JSON")
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("must contain valid strict JSON")
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("JSON object keys must be strings")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("JSON content contains duplicate object keys")
			}
			value, err := strictPresentonJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if closer, err := decoder.Token(); err != nil || closer != json.Delim('}') {
			return nil, fmt.Errorf("must contain valid strict JSON")
		}
		return object, nil
	case '[':
		values := []any{}
		for decoder.More() {
			value, err := strictPresentonJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if closer, err := decoder.Token(); err != nil || closer != json.Delim(']') {
			return nil, fmt.Errorf("must contain valid strict JSON")
		}
		return values, nil
	default:
		return nil, fmt.Errorf("must contain valid strict JSON")
	}
}

func presentonContentCitesURL(value any) bool {
	switch typed := value.(type) {
	case string:
		lower := strings.ToLower(typed)
		return strings.Contains(lower, "https://") || strings.Contains(lower, "http://")
	case map[string]any:
		for key, child := range typed {
			if presentonContentCitesURL(key) || presentonContentCitesURL(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if presentonContentCitesURL(child) {
				return true
			}
		}
	}
	return false
}
