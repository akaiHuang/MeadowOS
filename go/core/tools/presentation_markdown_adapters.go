package tools

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// This is reviewed template binding data, shared byte-for-byte with the local
// renderer. It is not a prompt, a topic classifier, or generated slide content.
//
//go:embed presentation_assets/markdown_adapters.json
var presentationMarkdownAdapters []byte

type markdownAdapterManifest struct {
	Version               int               `json:"version"`
	TemplateID            string            `json:"template_id"`
	TemplateLayoutsSHA256 string            `json:"template_layouts_sha256"`
	Adapters              []markdownAdapter `json:"adapters"`
}

type markdownAdapterGrammar struct {
	Lead   string `json:"lead"`
	Items  string `json:"items"`
	Images string `json:"images,omitempty"`
}

type markdownAdapter struct {
	Alias          string                 `json:"alias"`
	LayoutID       string                 `json:"layout_id"`
	LayoutSHA256   string                 `json:"layout_sha256"`
	SchemaSHA256   string                 `json:"schema_sha256"`
	MinItems       int                    `json:"min_items"`
	MaxItems       int                    `json:"max_items"`
	ItemOrder      []int                  `json:"item_order,omitempty"`
	ImageUIPointer string                 `json:"image_ui_pointer,omitempty"`
	Grammar        markdownAdapterGrammar `json:"grammar"`
	Bindings       map[string]string      `json:"bindings,omitempty"`
	Schema         map[string]any         `json:"schema,omitempty"`
}

func markdownAdapterManifestSHA256() string {
	hash := sha256.Sum256(presentationMarkdownAdapters)
	return hex.EncodeToString(hash[:])
}

func canonicalMarkdownJSONSHA256(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	hash := sha256.Sum256(bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}))
	return hex.EncodeToString(hash[:]), nil
}

func loadMarkdownAdapters() (*markdownAdapterManifest, error) {
	var manifest markdownAdapterManifest
	if err := json.Unmarshal(presentationMarkdownAdapters, &manifest); err != nil || manifest.Version != 1 || manifest.TemplateID != "editorial" || len(manifest.Adapters) < 3 || len(manifest.Adapters) > 8 {
		return nil, fmt.Errorf("installed Markdown adapter manifest is invalid")
	}
	seen := map[string]bool{}
	for _, adapter := range manifest.Adapters {
		hash, err := canonicalMarkdownJSONSHA256(adapter.Schema)
		if err != nil || hash != adapter.SchemaSHA256 || seen[adapter.Alias] || adapter.Alias == "" || adapter.LayoutID == "" || adapter.MinItems < 0 || adapter.MaxItems < adapter.MinItems || len(adapter.LayoutSHA256) != 64 {
			return nil, fmt.Errorf("installed Markdown adapter schema fingerprint is invalid")
		}
		if err := validateMarkdownItemOrder(adapter); err != nil {
			return nil, err
		}
		if adapter.Grammar.Images == "required" {
			if adapter.Bindings["image"] == "" || adapter.ImageUIPointer == "" || adapter.Grammar.Items != "none" || adapter.MinItems != 0 || adapter.MaxItems != 0 {
				return nil, fmt.Errorf("installed Markdown photo adapter requires one fully declared image slot")
			}
		} else if adapter.Grammar.Images != "" || adapter.ImageUIPointer != "" || adapter.Bindings["image"] != "" || adapter.MinItems < 1 {
			return nil, fmt.Errorf("installed Markdown text adapter has inconsistent image bindings")
		}
		seen[adapter.Alias] = true
	}
	return &manifest, nil
}

func validateMarkdownItemOrder(adapter markdownAdapter) error {
	if len(adapter.ItemOrder) == 0 {
		return nil
	}
	if adapter.MinItems != adapter.MaxItems || len(adapter.ItemOrder) != adapter.MaxItems {
		return fmt.Errorf("installed Markdown adapter item order requires an exact fixed item count")
	}
	seen := make([]bool, len(adapter.ItemOrder))
	for _, index := range adapter.ItemOrder {
		if index < 0 || index >= len(seen) || seen[index] {
			return fmt.Errorf("installed Markdown adapter item order is not a complete unique permutation")
		}
		seen[index] = true
	}
	return nil
}

func markdownAdapterForAlias(alias string) (markdownAdapter, bool) {
	manifest, err := loadMarkdownAdapters()
	if err == nil {
		for _, adapter := range manifest.Adapters {
			if adapter.Alias == alias {
				return adapter, true
			}
		}
	}
	return markdownAdapter{}, false
}

func markdownAdapterFits(slide *markdownSlide, adapter markdownAdapter) bool {
	if (adapter.Grammar.Images == "required" && len(slide.Images) != 1) || (adapter.Grammar.Images != "required" && len(slide.Images) != 0) {
		return false
	}
	if len(slide.Items) < adapter.MinItems || len(slide.Items) > adapter.MaxItems {
		return false
	}
	if adapter.Grammar.Lead == "required" && len(slide.Lead) == 0 {
		return false
	}
	if adapter.Grammar.Lead == "forbidden" && len(slide.Lead) != 0 {
		return false
	}
	return (adapter.Grammar.Items == "none" && len(slide.Items) == 0 && slide.ItemKind == "") || (adapter.Grammar.Items == "ordered_list" && slide.ItemKind == "list") ||
		(adapter.Grammar.Items == "sections" && slide.ItemKind == "sections")
}

func markdownAdapterTextLimits(adapter markdownAdapter) (map[string]markdownTextLimit, error) {
	limits := map[string]markdownTextLimit{}
	for key, path := range adapter.Bindings {
		if key == "items" {
			continue
		}
		schema := adapter.Schema
		if strings.HasPrefix(key, "item_") {
			arraySchema := markdownSchemaAt(schema, adapter.Bindings["items"])
			schema, _ = arraySchema["items"].(map[string]any)
		}
		field := markdownSchemaAt(schema, path)
		if field == nil || field["type"] != "string" {
			return nil, fmt.Errorf("installed Markdown adapter text capacity binding is invalid")
		}
		if key == "image" {
			limits[key] = markdownTextLimit{Min: 1, Max: presentationMarkdownMaxLabelBytes, Unit: "bytes"}
		} else {
			maximum, _ := field["maxLength"].(float64)
			if maximum < 1 {
				return nil, fmt.Errorf("installed Markdown adapter text capacity is unbounded")
			}
			limits[key] = markdownTextLimit{Min: 1, Max: int(maximum), Unit: "characters"}
		}
	}
	return limits, nil
}

func bindMarkdownSlide(slide *markdownSlide) error {
	manifest, err := loadMarkdownAdapters()
	if err != nil {
		return err
	}
	var selected *markdownAdapter
	for index := range manifest.Adapters {
		adapter := &manifest.Adapters[index]
		if slide.LayoutAlias == "" {
			// Choosing the first physically compatible declared adapter depends
			// only on AST shape and slot capacity, never prose or task intent.
			if selected == nil && markdownAdapterFits(slide, *adapter) {
				selected = adapter
			}
		} else if adapter.Alias == slide.LayoutAlias {
			selected = adapter
		}
	}
	if selected == nil || !markdownAdapterFits(slide, *selected) {
		return markdownLayoutCompatibilityError(slide, selected, manifest)
	}
	slide.LayoutAlias, slide.LayoutID, slide.SchemaSHA256 = selected.Alias, selected.LayoutID, selected.SchemaSHA256
	_, err = markdownNativeContent(slide, *selected)
	return err
}

func markdownSchemaAt(schema map[string]any, path string) map[string]any {
	current := schema
	for _, part := range strings.Split(path, ".") {
		properties, _ := current["properties"].(map[string]any)
		current, _ = properties[part].(map[string]any)
		if current == nil {
			return nil
		}
	}
	return current
}

func setMarkdownBoundValue(content map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	current := content
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
}

func markdownBoundText(content, schema map[string]any, path, value string, line int) error {
	field := markdownSchemaAt(schema, path)
	if path == "" || field == nil || field["type"] != "string" || strings.TrimSpace(value) == "" {
		return fmt.Errorf("markdown line %d: selected adapter requires nonblank text for %s", line, path)
	}
	limit, _ := field["maxLength"].(float64)
	if limit < 1 || utf8.RuneCountInString(value) > int(limit) {
		return fmt.Errorf("markdown line %d: %s exceeds the selected slot's %d-character maximum; revise the Markdown or select a compatible layout; text was not shortened", line, path, int(limit))
	}
	setMarkdownBoundValue(content, path, value)
	return nil
}

func markdownNativeContent(slide *markdownSlide, adapter markdownAdapter) (map[string]any, error) {
	if err := validateMarkdownItemOrder(adapter); err != nil {
		return nil, err
	}
	if len(adapter.ItemOrder) != 0 && len(slide.Items) != len(adapter.ItemOrder) {
		return nil, fmt.Errorf("markdown line %d: selected adapter geometry requires exactly %d items", slide.Title.Line, len(adapter.ItemOrder))
	}
	content := map[string]any{}
	if err := markdownBoundText(content, adapter.Schema, adapter.Bindings["title"], slide.Title.Text, slide.Title.Line); err != nil {
		return nil, err
	}
	if path := adapter.Bindings["lead"]; path != "" {
		line := slide.Title.Line
		if len(slide.Lead) > 0 {
			line = slide.Lead[0].Line
		}
		if err := markdownBoundText(content, adapter.Schema, path, joinMarkdownText(slide.Lead), line); err != nil {
			return nil, err
		}
	}
	if path := adapter.Bindings["image"]; path != "" {
		field := markdownSchemaAt(adapter.Schema, path)
		if len(slide.Images) != 1 || field == nil || field["type"] != "string" || strings.TrimSpace(slide.Images[0].Alt) == "" || len(slide.Images[0].Alt) > presentationMarkdownMaxLabelBytes {
			return nil, fmt.Errorf("markdown line %d: selected photo adapter requires one literal image description of at most 512 bytes", slide.Title.Line)
		}
		setMarkdownBoundValue(content, path, slide.Images[0].Alt)
	}
	arrayPath := adapter.Bindings["items"]
	if arrayPath == "" {
		if len(slide.Items) != 0 {
			return nil, fmt.Errorf("markdown line %d: selected adapter has no item slots; no authored item was dropped", slide.Title.Line)
		}
		return content, nil
	}
	arraySchema := markdownSchemaAt(adapter.Schema, arrayPath)
	itemSchema, _ := arraySchema["items"].(map[string]any)
	if itemSchema == nil {
		return nil, fmt.Errorf("installed Markdown adapter item binding is invalid")
	}
	items := make([]any, 0, len(slide.Items))
	for nativeIndex := range slide.Items {
		index := nativeIndex
		if len(adapter.ItemOrder) != 0 {
			index = adapter.ItemOrder[nativeIndex]
		}
		item := slide.Items[index]
		entry := map[string]any{}
		line := slide.Title.Line
		if len(item.Body) > 0 {
			line = item.Body[0].Line
		}
		if path := adapter.Bindings["item_index"]; path != "" {
			ordinal := item.Ordinal
			if ordinal == "" {
				ordinal = fmt.Sprint(index + 1)
			}
			if err := markdownBoundText(entry, itemSchema, path, ordinal, line); err != nil {
				return nil, err
			}
		}
		if path := adapter.Bindings["item_text"]; path != "" {
			if err := markdownBoundText(entry, itemSchema, path, joinMarkdownText(item.Body), line); err != nil {
				return nil, err
			}
		}
		if path := adapter.Bindings["item_title"]; path != "" {
			if err := markdownBoundText(entry, itemSchema, path, item.Heading.Text, item.Heading.Line); err != nil {
				return nil, err
			}
		}
		if path := adapter.Bindings["item_body"]; path != "" {
			if err := markdownBoundText(entry, itemSchema, path, joinMarkdownText(item.Body), line); err != nil {
				return nil, err
			}
		}
		items = append(items, entry)
	}
	setMarkdownBoundValue(content, arrayPath, items)
	return content, nil
}
