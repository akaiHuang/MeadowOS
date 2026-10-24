package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
)

const PresentationFromMarkdownToolName = "presentation_from_markdown"

const (
	presentationMarkdownMaxIRBytes        = 512 << 10
	presentationMarkdownMaxImagePlanBytes = 256 << 10
	presentationMarkdownMaxSidecarBytes   = 1 << 20
)

var presentationMarkdownSidecars = []string{"source.md", "markdown-ir.json", "image-briefs.json"}

type presentonMarkdownAdapterInfo struct {
	Alias                  string                       `json:"alias"`
	LayoutID               string                       `json:"layout_id"`
	LayoutSHA256           string                       `json:"layout_sha256"`
	SchemaSHA256           string                       `json:"schema_sha256"`
	MinItems               int                          `json:"min_items"`
	MaxItems               int                          `json:"max_items"`
	ItemOrder              []int                        `json:"item_order,omitempty"`
	ImageUIPointer         string                       `json:"image_ui_pointer,omitempty"`
	Grammar                markdownAdapterGrammar       `json:"grammar"`
	TemplateLayoutsSHA256  string                       `json:"template_layouts_sha256"`
	AdapterManifestSHA256  string                       `json:"adapter_manifest_sha256"`
	SupportsMarkdownRender bool                         `json:"supports_markdown_render,omitempty"`
	TextLimits             map[string]markdownTextLimit `json:"text_limits,omitempty"`
	MarkdownExample        string                       `json:"markdown_example,omitempty"`
}

type markdownTextLimit struct {
	Min  int    `json:"min"`
	Max  int    `json:"max"`
	Unit string `json:"unit"`
}

type presentationMarkdownSource struct {
	Markdown              string `json:"markdown"`
	SHA256                string `json:"sha256"`
	ByteLength            int    `json:"byte_length"`
	SlideCount            int    `json:"slide_count"`
	AdapterManifestSHA256 string `json:"adapter_manifest_sha256"`
}

type presentationMarkdownImport struct {
	Source          presentationMarkdownSource
	Bindings        []markdownAdapter
	Files           map[string][]byte
	PendingImages   int
	Assets          map[string]presentonMarkdownAsset
	ImagePlacements [][]presentonMarkdownImagePlacement
}

// PresentationFromMarkdown keeps the model's authoring interface small. The
// model chooses the installed template and authors Markdown; this tool only
// parses its declared hierarchy, binds reviewed slots and invokes the same
// native renderer and edit/storage boundaries as the advanced tool.
type PresentationFromMarkdown struct {
	Artifact *PresentationArtifact
}

func (*PresentationFromMarkdown) Name() string { return PresentationFromMarkdownToolName }
func (*PresentationFromMarkdown) Kind() Kind   { return KindEdit }
func (*PresentationFromMarkdown) Description() string {
	return "Turn supplied prose, research or discussion into editable PPTX/HTML by authoring Markdown; input need not arrive as Markdown. presentation_templates operation=markdown provides the compact available templates/layouts, complete examples and capacities in one call. Choose a fitting template/layout and render directly without native schema lookups or a second model rewrite. Inspect returned PNGs. image-brief fences remain pending."
}
func (*PresentationFromMarkdown) Schema() map[string]any {
	schema := obj(map[string]any{
		"markdown":    map[string]any{"type": "string", "minLength": 1, "maxLength": presentationMarkdownMaxBytes, "description": "H1 is deck metadata, not a slide. Every H2 starts a complete slide; place its optional layout comment immediately below H2. Follow markdown_example and text_limits from presentation_templates operation=markdown, replacing example text with authored content. This call validates then renders; no separate preflight is required."},
		"template":    map[string]any{"type": "string"},
		"artifact_id": map[string]any{"type": "string"},
		"sources":     map[string]any{"type": "array", "maxItems": 40, "items": obj(map[string]any{"title": map[string]any{"type": "string"}, "url": map[string]any{"type": "string"}, "excerpt": map[string]any{"type": "string"}, "accessed_at": map[string]any{"type": "string"}}, "title", "url")},
		"output_dir":  map[string]any{"type": "string"},
	}, "markdown", "template")
	schema["additionalProperties"] = false
	return schema
}
func (*PresentationFromMarkdown) NormalizeArguments(args map[string]any) {
	normalizePresentationArrayArgument(args, "sources", "sources_json")
	omitLocalPresentationSources(args)
}

func markdownArtifactID(args map[string]any, markdown, template string) string {
	if id := sanitizeArtifactID(strArg(args, "artifact_id")); id != "" {
		return id
	}
	hash := sha256.Sum256([]byte(template + "\x00" + markdown))
	return "md-" + hex.EncodeToString(hash[:8])
}

func (t *PresentationFromMarkdown) outputDirectory(args map[string]any, markdown, template string) (string, error) {
	if t == nil || t.Artifact == nil || t.Artifact.WS == nil {
		return "", fmt.Errorf("workspace is required")
	}
	directory := strings.TrimSpace(strArg(args, "output_dir"))
	if directory == "" {
		directory = filepath.Join(".meadow", "presentations", markdownArtifactID(args, markdown, template))
	}
	return t.Artifact.WS.ResolveArtifact(directory)
}

// Declare exact outputs before execution so the runtime can snapshot and roll
// back the complete owned multi-file edit, including durable compiler inputs.
func (t *PresentationFromMarkdown) MutationPaths(args map[string]any) []string {
	markdown, template := strArg(args, "markdown"), strings.TrimSpace(strArg(args, "template"))
	deck, err := compilePresentationMarkdown(markdown, template)
	if err != nil {
		return nil
	}
	directory, err := t.outputDirectory(args, markdown, template)
	if err != nil {
		return nil
	}
	names := []string{"index.html", "presentation.pptx", "presentation.json", "sources.json", "deck.ts", "presenton.json"}
	for index := range deck.Slides {
		names = append(names, fmt.Sprintf("slide-%02d.png", index+1))
	}
	names = append(names, presentationMarkdownSidecars...)
	for _, slide := range deck.Slides {
		if len(slide.Images) != 0 {
			names = append(names, "image-assets.json")
			break
		}
	}
	paths := make([]string, len(names))
	for index, name := range names {
		paths[index] = filepath.Join(directory, name)
	}
	return paths
}

func (t *PresentationFromMarkdown) Execute(ctx context.Context, args map[string]any) Result {
	if t == nil || t.Artifact == nil || t.Artifact.WS == nil {
		return errResultf("workspace is required")
	}
	for key := range args {
		switch key {
		case "markdown", "template", "artifact_id", "sources", "output_dir":
		default:
			return errResultf("presentation_from_markdown does not accept %q; compiler modes and native bindings are internal", key)
		}
	}
	markdown, ok := args["markdown"].(string)
	if !ok {
		return errResultf("markdown must be a string")
	}
	template := strings.TrimSpace(strArg(args, "template"))
	manifest, err := loadMarkdownAdapters()
	if err != nil {
		return errResult(err)
	}
	if template != manifest.TemplateID {
		return errResultf("selected installed template has no matching verified Markdown adapters; read presentation_templates layouts and choose a supported template; no fallback deck was created")
	}
	deck, err := compilePresentationMarkdown(markdown, template)
	if err != nil {
		return errResult(err)
	}
	assets, err := resolveMarkdownAssets(ctx, t.Artifact.WS, deck)
	if err != nil {
		return errResult(err)
	}
	compiled := &presentationMarkdownImport{Files: map[string][]byte{}, Bindings: make([]markdownAdapter, len(deck.Slides)), Assets: assets, ImagePlacements: make([][]presentonMarkdownImagePlacement, len(deck.Slides))}
	sourceHash := sha256.Sum256([]byte(markdown))
	compiled.Source = presentationMarkdownSource{Markdown: markdown, SHA256: hex.EncodeToString(sourceHash[:]), ByteLength: len(markdown), SlideCount: len(deck.Slides), AdapterManifestSHA256: markdownAdapterManifestSHA256()}
	slides := make([]any, 0, len(deck.Slides))
	imageBriefs := []markdownImageBrief{}
	for index := range deck.Slides {
		slide := &deck.Slides[index]
		adapter, _ := markdownAdapterForAlias(slide.LayoutAlias)
		content, err := markdownNativeContent(slide, adapter)
		if err != nil {
			return errResult(err)
		}
		nativeJSON, err := json.Marshal(content)
		if err != nil {
			return errResult(err)
		}
		urls := []string{}
		seen := map[string]bool{}
		for _, reference := range slide.References {
			if !seen[reference.URL] {
				seen[reference.URL] = true
				urls = append(urls, reference.URL)
			}
		}
		if len(urls) > 8 {
			return errResultf("markdown line %d: one slide may reference at most eight source URLs", slide.Title.Line)
		}
		slides = append(slides, map[string]any{"id": fmt.Sprintf("slide-%02d", index+1), "title": slide.Title.Text, "presenton_layout_id": adapter.LayoutID, "presenton_content_json": string(nativeJSON), "source_urls": urls})
		compiled.Bindings[index] = adapter
		for _, image := range slide.Images {
			compiled.ImagePlacements[index] = append(compiled.ImagePlacements[index], presentonMarkdownImagePlacement{AssetID: image.AssetID, Slot: adapter.Bindings["image"], Alt: image.Alt, Placeholder: image.Placeholder})
		}
		imageBriefs = append(imageBriefs, slide.ImageBriefs...)
	}
	compiled.Files, err = encodeMarkdownSidecars(markdown, deck, imageBriefs)
	if err != nil {
		return errResult(err)
	}
	compiled.PendingImages = len(imageBriefs)
	if len(assets) != 0 {
		compiled.Files["image-assets.json"], err = encodeMarkdownAssetSidecar(deck, assets)
		if err != nil {
			return errResult(err)
		}
		if err := validateMarkdownSidecarFiles(compiled.Files); err != nil {
			return errResult(err)
		}
	}
	if err := t.verifyLiveAdapters(ctx, manifest); err != nil {
		return errResult(err)
	}
	arguments := map[string]any{"title": deck.Title, "slides": slides, "engine": "presenton", "template": template, "artifact_id": markdownArtifactID(args, markdown, template)}
	for _, key := range []string{"sources", "output_dir"} {
		if value, exists := args[key]; exists {
			arguments[key] = value
		}
	}
	artifact := *t.Artifact
	artifact.markdownImport = compiled
	return artifact.Execute(ctx, arguments)
}

func encodeMarkdownSidecars(markdown string, deck *markdownPresentation, imageBriefs []markdownImageBrief) (map[string][]byte, error) {
	ir, err := json.MarshalIndent(deck, "", "  ")
	if err != nil {
		return nil, err
	}
	images, err := json.MarshalIndent(imageBriefs, "", "  ")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"source.md": []byte(markdown), "markdown-ir.json": append(ir, '\n'), "image-briefs.json": append(images, '\n')}
	if err := validateMarkdownSidecarFiles(files); err != nil {
		return nil, err
	}
	return files, nil
}

func validateMarkdownSidecarFiles(files map[string][]byte) error {
	if len(files["markdown-ir.json"]) > presentationMarkdownMaxIRBytes || len(files["image-briefs.json"]) > presentationMarkdownMaxImagePlanBytes || len(files["image-assets.json"]) > presentationMarkdownMaxImagePlanBytes {
		return fmt.Errorf("compiled Markdown exceeds the 512 KiB IR or 256 KiB image-plan/provenance limit; no content was truncated or rendered")
	}
	total := 0
	for _, file := range files {
		total += len(file)
	}
	if total > presentationMarkdownMaxSidecarBytes {
		return fmt.Errorf("compiled Markdown sidecars exceed 1 MiB; no content was truncated or rendered")
	}
	return nil
}

func (t *PresentationFromMarkdown) verifyLiveAdapters(ctx context.Context, manifest *markdownAdapterManifest) error {
	discovery := &PresentationTemplates{PresentonURL: t.Artifact.PresentonURL, PresentonToken: t.Artifact.PresentonToken, HTTP: t.Artifact.HTTP}
	result := discovery.Execute(ctx, map[string]any{"operation": "markdown", "template_id": manifest.TemplateID})
	if result.Err != nil {
		return result.Err
	}
	var catalog presentonCatalogResponse
	if json.Unmarshal([]byte(result.LLMContent), &catalog) != nil {
		return fmt.Errorf("invalid installed Markdown catalog")
	}
	return validateMarkdownCatalog(catalog, manifest)
}

func validateMarkdownCatalog(catalog presentonCatalogResponse, manifest *markdownAdapterManifest) error {
	if catalog.Template == nil || catalog.Template.ID != manifest.TemplateID || len(catalog.MarkdownAdapters) != len(manifest.Adapters) || catalog.TemplateLayoutsSHA256 != manifest.TemplateLayoutsSHA256 || catalog.MarkdownAdapterManifestSHA256 != markdownAdapterManifestSHA256() {
		return fmt.Errorf("installed template/Markdown adapter fingerprint changed or import is unsupported; no deck was rendered")
	}
	for _, expected := range manifest.Adapters {
		limits, err := markdownAdapterTextLimits(expected)
		if err != nil {
			return err
		}
		found := false
		for _, actual := range catalog.MarkdownAdapters {
			if actual.LayoutID != expected.LayoutID {
				continue
			}
			found = actual.SupportsMarkdownRender && actual.Alias == expected.Alias && actual.LayoutID == expected.LayoutID && actual.SchemaSHA256 == expected.SchemaSHA256 && actual.LayoutSHA256 == expected.LayoutSHA256 && actual.MinItems == expected.MinItems && actual.MaxItems == expected.MaxItems && actual.TemplateLayoutsSHA256 == manifest.TemplateLayoutsSHA256 && actual.AdapterManifestSHA256 == markdownAdapterManifestSHA256() && actual.ImageUIPointer == expected.ImageUIPointer && reflect.DeepEqual(actual.Grammar, expected.Grammar) && reflect.DeepEqual(actual.ItemOrder, expected.ItemOrder) && reflect.DeepEqual(actual.TextLimits, limits)
		}
		if !found {
			return fmt.Errorf("installed native layout no longer matches Markdown adapter %s; no fallback or content rewrite was performed", expected.Alias)
		}
	}
	return nil
}
