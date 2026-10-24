package tools

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

const (
	presentationMarkdownMaxBytes           = 256 << 10
	presentationMarkdownMaxNodes           = 4096
	presentationMarkdownMaxDepth           = 16
	presentationMarkdownMaxURLBytes        = 2048
	presentationMarkdownMaxLabelBytes      = 512
	presentationMarkdownMaxSlideReferences = 64
	presentationMarkdownMaxReferences      = 256
	presentationMarkdownMaxDefinitions     = 40
)

// Markdown is an authored document, not a task instruction. These structures
// retain all supported prose and link/image-plan metadata before a declared
// adapter binds them to the selected real template.
type markdownPresentation struct {
	Version         int                 `json:"version"`
	Title           string              `json:"title"`
	TitleLine       int                 `json:"title_line"`
	Template        string              `json:"template"`
	Slides          []markdownSlide     `json:"slides"`
	LinkDefinitions []markdownReference `json:"link_definitions,omitempty"`
}

type markdownText struct {
	Text string `json:"text"`
	Line int    `json:"line"`
}

type markdownItem struct {
	Heading markdownText   `json:"heading,omitempty"`
	Body    []markdownText `json:"body,omitempty"`
	Ordinal string         `json:"ordinal,omitempty"`
}

type markdownReference struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Line  int    `json:"line,omitempty"`
}

type markdownImageBrief struct {
	Slide  int    `json:"slide"`
	Line   int    `json:"line"`
	Brief  string `json:"brief"`
	Status string `json:"status"`
}

type markdownImage struct {
	Alt         string `json:"alt"`
	Source      string `json:"source"`
	Line        int    `json:"line"`
	AssetID     string `json:"asset_id,omitempty"`
	Placeholder bool   `json:"placeholder"`
}

type markdownSlide struct {
	Number       int                  `json:"number"`
	Title        markdownText         `json:"title"`
	Lead         []markdownText       `json:"lead,omitempty"`
	Items        []markdownItem       `json:"items"`
	ItemKind     string               `json:"item_kind"`
	LayoutAlias  string               `json:"layout_alias"`
	LayoutID     string               `json:"layout_id"`
	SchemaSHA256 string               `json:"schema_sha256"`
	References   []markdownReference  `json:"references,omitempty"`
	ImageBriefs  []markdownImageBrief `json:"image_briefs,omitempty"`
	Images       []markdownImage      `json:"images,omitempty"`
}

func markdownLine(source []byte, node ast.Node) int {
	start := len(source)
	_ = ast.Walk(node, func(child ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if child.Type() == ast.TypeBlock && child.Lines().Len() > 0 {
			if candidate := child.Lines().At(0).Start; candidate < start {
				start = candidate
			}
		}
		if value, ok := child.(*ast.Text); ok && value.Segment.Start < start {
			start = value.Segment.Start
		}
		if value, ok := child.(*ast.FencedCodeBlock); ok && value.Info != nil && value.Info.Segment.Start < start {
			start = value.Info.Segment.Start
		}
		return ast.WalkContinue, nil
	})
	if start == len(source) {
		return 1
	}
	return bytes.Count(source[:start], []byte{'\n'}) + 1
}

func markdownError(source []byte, node ast.Node, problem string) error {
	return fmt.Errorf("markdown line %d: %s", markdownLine(source, node), problem)
}

func markdownPlainText(source []byte, node ast.Node, references *[]markdownReference) (string, error) {
	var result strings.Builder
	var visit func(ast.Node) error
	visit = func(current ast.Node) error {
		switch value := current.(type) {
		case *ast.Text:
			content := value.Value(source)
			if !value.IsRaw() {
				content = util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(content)))
			}
			result.Write(content)
			if value.SoftLineBreak() || value.HardLineBreak() {
				result.WriteByte('\n')
			}
		case *ast.String:
			result.Write(value.Value)
		case *ast.CodeSpan:
			for child := current.FirstChild(); child != nil; child = child.NextSibling() {
				if err := visit(child); err != nil {
					return err
				}
			}
		case *ast.Emphasis:
			for child := current.FirstChild(); child != nil; child = child.NextSibling() {
				if err := visit(child); err != nil {
					return err
				}
			}
		case *ast.Link:
			if len(value.Destination) > presentationMarkdownMaxURLBytes {
				return markdownError(source, current, "source URLs must be at most 2048 bytes; no link was shortened")
			}
			destination := string(value.Destination)
			if !validMarkdownSourceURL(destination) {
				return markdownError(source, current, "links must use a public HTTP(S) source URL; images belong in an image-brief fence")
			}
			label, err := markdownPlainText(source, current, references)
			if err != nil {
				return err
			}
			result.WriteString(label)
			if err := appendMarkdownReference(source, current, references, label, destination); err != nil {
				return err
			}
		case *ast.AutoLink:
			if len(value.URL(source)) > presentationMarkdownMaxURLBytes {
				return markdownError(source, current, "source URLs must be at most 2048 bytes; no link was shortened")
			}
			destination := string(value.URL(source))
			if !validMarkdownSourceURL(destination) {
				return markdownError(source, current, "autolinks must use a public HTTP(S) source URL")
			}
			label := string(value.Label(source))
			result.WriteString(label)
			if err := appendMarkdownReference(source, current, references, label, destination); err != nil {
				return err
			}
		default:
			return markdownError(source, current, "unsupported inline "+current.Kind().String()+"; use plain text, emphasis, inline code or source links")
		}
		return nil
	}
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if err := visit(child); err != nil {
			return "", err
		}
	}
	value := strings.TrimSpace(result.String())
	if value == "" {
		return "", markdownError(source, node, "content must not be blank")
	}
	return value, nil
}

func appendMarkdownReference(source []byte, node ast.Node, references *[]markdownReference, label, destination string) error {
	if len(label) > presentationMarkdownMaxLabelBytes {
		return markdownError(source, node, "source link labels must be at most 512 bytes")
	}
	if len(*references) >= presentationMarkdownMaxSlideReferences {
		return markdownError(source, node, "one slide may contain at most 64 source references; no reference was discarded")
	}
	*references = append(*references, markdownReference{Label: label, URL: destination, Line: markdownLine(source, node)})
	return nil
}

func validMarkdownSourceURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.User == nil && validPresentationURL(raw)
}

func compilePresentationMarkdown(markdown, template string) (*markdownPresentation, error) {
	if len(markdown) == 0 || len(markdown) > presentationMarkdownMaxBytes || !utf8.ValidString(markdown) || strings.IndexByte(markdown, 0) >= 0 {
		return nil, fmt.Errorf("markdown must be nonempty UTF-8 without NUL, at most 256 KiB")
	}
	source := []byte(markdown)
	parseContext := parser.NewContext()
	document := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(text.NewReader(source), parser.WithContext(parseContext))
	nodes, depth := 0, 0
	if err := ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			nodes++
			depth++
			if nodes > presentationMarkdownMaxNodes || depth > presentationMarkdownMaxDepth {
				return ast.WalkStop, markdownError(source, node, "document exceeds the 4096-node or 16-level structural limit")
			}
		} else {
			depth--
		}
		return ast.WalkContinue, nil
	}); err != nil {
		return nil, err
	}
	deck := &markdownPresentation{Version: 1, Template: template, Slides: []markdownSlide{}}
	var globalReferences []markdownReference
	var slide *markdownSlide
	var section *markdownItem
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		switch value := node.(type) {
		case *ast.Heading:
			var references []markdownReference
			content, err := markdownPlainText(source, node, &references)
			if err != nil {
				return nil, err
			}
			line := markdownLine(source, node)
			switch value.Level {
			case 1:
				if deck.Title != "" || len(deck.Slides) != 0 || utf8.RuneCountInString(content) > 200 {
					return nil, markdownError(source, node, "use one opening H1 deck title of at most 200 characters")
				}
				deck.Title, deck.TitleLine = content, line
				globalReferences = append(globalReferences, references...)
			case 2:
				if deck.Title == "" || len(deck.Slides) == 20 {
					return nil, markdownError(source, node, "an H1 title must precede one to twenty H2 slides")
				}
				deck.Slides = append(deck.Slides, markdownSlide{Number: len(deck.Slides) + 1, Title: markdownText{Text: content, Line: line}, Items: []markdownItem{}, References: references})
				slide = &deck.Slides[len(deck.Slides)-1]
				section = nil
			case 3:
				if slide == nil || slide.ItemKind == "list" {
					alias := ""
					if slide != nil {
						alias = slide.LayoutAlias
					}
					return nil, markdownDiagnostic("H3 sections need an H2 slide and cannot be mixed with list items", line, slide, "Use one item structure per H2: a list-only page, or a lead paragraph followed by H3 heading/body sections. Put separate slides under separate H2 headings.", alias)
				}
				slide.ItemKind = "sections"
				slide.Items = append(slide.Items, markdownItem{Heading: markdownText{Text: content, Line: line}, Body: []markdownText{}})
				section = &slide.Items[len(slide.Items)-1]
				slide.References = append(slide.References, references...)
			default:
				return nil, markdownError(source, node, "headings deeper than H3 are unsupported; their content would not fit a declared slot")
			}
		case *ast.Paragraph:
			if image, ok := value.FirstChild().(*ast.Image); ok && image.NextSibling() == nil {
				if slide == nil || len(slide.Images) != 0 || len(image.Title) != 0 {
					return nil, markdownError(source, node, "one standalone image block is allowed per image-capable slide; image title attributes are unsupported")
				}
				var references []markdownReference
				alt, err := markdownPlainText(source, image, &references)
				if err != nil || len(alt) > presentationMarkdownMaxLabelBytes || len(references) != 0 {
					return nil, markdownError(source, node, "image alt text must be nonblank plain text of at most 512 bytes")
				}
				destination := string(image.Destination)
				if err := validateMarkdownImageSource(destination); err != nil {
					return nil, markdownError(source, node, err.Error())
				}
				slide.Images = append(slide.Images, markdownImage{Alt: alt, Source: destination, Line: markdownLine(source, node), Placeholder: destination == "placeholder:photo"})
				continue
			}
			if slide == nil || (slide.ItemKind == "list" && section == nil) {
				alias := ""
				if slide != nil {
					alias = slide.LayoutAlias
				}
				return nil, markdownDiagnostic("put prose inside an H2 slide, before H3 sections; list-only slides cannot include prose", markdownLine(source, node), slide, "H1 is only the deck title. Put slide content after an H2. For a list-only layout, keep prose in authored list items; for heading/body sections, choose a compatible layout and provide its lead before H3 headings.", alias)
			}
			content, err := markdownPlainText(source, node, &slide.References)
			if err != nil {
				return nil, err
			}
			block := markdownText{Text: content, Line: markdownLine(source, node)}
			if section != nil {
				section.Body = append(section.Body, block)
			} else {
				slide.Lead = append(slide.Lead, block)
			}
		case *ast.TextBlock:
			// Goldmark replaces a reference-definition-only paragraph with an
			// empty TextBlock. Definitions are retained below from parseContext.
			if value.Lines().Len() != 0 || value.FirstChild() != nil {
				return nil, markdownError(source, node, "unsupported nonempty top-level text block")
			}
		case *ast.List:
			if slide == nil || len(slide.Lead) != 0 || slide.ItemKind == "sections" {
				alias := ""
				if slide != nil {
					alias = slide.LayoutAlias
				}
				return nil, markdownDiagnostic("use a list-only H2 slide, without a lead paragraph or H3 sections", markdownLine(source, node), slide, "Encountered a list after prose/H3 or before any H2. For two_column_list, place only list items under H2. For intro_cards/timeline, author a lead then H3 heading/body sections instead of a list. Preserve your content when restructuring or choose a different layout.", alias)
			}
			slide.ItemKind = "list"
			index := 0
			for item := value.FirstChild(); item != nil; item = item.NextSibling() {
				if _, ok := item.(*ast.ListItem); !ok {
					return nil, markdownError(source, item, "unsupported list child")
				}
				entry := markdownItem{Body: []markdownText{}}
				if value.IsOrdered() {
					entry.Ordinal = strconv.Itoa(value.Start + index)
				} else {
					entry.Ordinal = strconv.Itoa(len(slide.Items) + 1)
				}
				for block := item.FirstChild(); block != nil; block = block.NextSibling() {
					if block.Kind() != ast.KindParagraph && block.Kind() != ast.KindTextBlock {
						return nil, markdownError(source, block, "nested lists and non-prose list blocks are unsupported")
					}
					content, err := markdownPlainText(source, block, &slide.References)
					if err != nil {
						return nil, err
					}
					entry.Body = append(entry.Body, markdownText{Text: content, Line: markdownLine(source, block)})
				}
				if len(entry.Body) == 0 {
					return nil, markdownError(source, item, "list items must contain text")
				}
				slide.Items = append(slide.Items, entry)
				index++
			}
		case *ast.HTMLBlock:
			content := strings.TrimSpace(string(value.Text(source)))
			if slide == nil || slide.LayoutAlias != "" || len(slide.Lead) != 0 || len(slide.Items) != 0 || len(slide.Images) != 0 || !strings.HasPrefix(content, "<!-- layout:") || !strings.HasSuffix(content, "-->") {
				return nil, markdownLayoutPlacementError(source, node, slide, content)
			}
			alias := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(content, "<!-- layout:"), "-->"))
			if _, ok := markdownAdapterForAlias(alias); !ok {
				return nil, markdownError(source, node, "unknown layout alias; use an advertised Markdown adapter")
			}
			slide.LayoutAlias = alias
		case *ast.FencedCodeBlock:
			if slide == nil || value.Info == nil || string(value.Info.Value(source)) != "image-brief" {
				return nil, markdownError(source, node, "only image-brief fenced blocks inside a slide are supported; code is not evaluated")
			}
			brief := strings.TrimSpace(string(value.Text(source)))
			if brief == "" || len(brief) > 8<<10 || len(slide.ImageBriefs) >= 4 || !markdownFenceClosed(source, value) {
				return nil, markdownError(source, node, "image-brief needs a closing fence and nonempty text of at most 8 KiB; at most four per slide")
			}
			slide.ImageBriefs = append(slide.ImageBriefs, markdownImageBrief{Slide: slide.Number, Line: markdownLine(source, node), Brief: brief, Status: "pending"})
		default:
			return nil, markdownError(source, node, "unsupported block "+node.Kind().String()+"; no content was exported")
		}
	}
	if deck.Title == "" || len(deck.Slides) == 0 {
		return nil, fmt.Errorf("markdown requires one H1 deck title and one to twenty H2 slides")
	}
	definitions := parseContext.References()
	if len(definitions) > presentationMarkdownMaxDefinitions {
		return nil, fmt.Errorf("markdown may contain at most 40 source link definitions")
	}
	for _, reference := range definitions {
		if len(reference.Destination()) > presentationMarkdownMaxURLBytes || len(reference.Label()) > presentationMarkdownMaxLabelBytes {
			return nil, fmt.Errorf("markdown source link definitions require URLs of at most 2048 bytes and labels of at most 512 bytes; no definition was shortened")
		}
		url := string(reference.Destination())
		if !validMarkdownSourceURL(url) {
			return nil, fmt.Errorf("markdown link definitions must use public HTTP(S) source URLs")
		}
		deck.LinkDefinitions = append(deck.LinkDefinitions, markdownReference{Label: string(reference.Label()), URL: url})
	}
	sort.Slice(deck.LinkDefinitions, func(i, j int) bool {
		left, right := deck.LinkDefinitions[i], deck.LinkDefinitions[j]
		if left.URL == right.URL {
			return left.Label < right.Label
		}
		return left.URL < right.URL
	})
	deck.Slides[0].References = append(deck.Slides[0].References, globalReferences...)
	deck.Slides[0].References = append(deck.Slides[0].References, deck.LinkDefinitions...)
	references := 0
	for index := range deck.Slides {
		if len(deck.Slides[index].References) > presentationMarkdownMaxSlideReferences {
			return nil, fmt.Errorf("markdown line %d: one slide may contain at most 64 source references; no reference was discarded", deck.Slides[index].Title.Line)
		}
		references += len(deck.Slides[index].References)
		if references > presentationMarkdownMaxReferences {
			return nil, fmt.Errorf("markdown line %d: a deck may contain at most 256 source references; no reference was discarded", deck.Slides[index].Title.Line)
		}
		if err := bindMarkdownSlide(&deck.Slides[index]); err != nil {
			return nil, err
		}
	}
	return deck, nil
}

func markdownFenceClosed(source []byte, block *ast.FencedCodeBlock) bool {
	start := block.Info.Segment.Start
	lineStart := bytes.LastIndexByte(source[:start], '\n') + 1
	opening := strings.TrimLeft(string(source[lineStart:start]), " \t")
	if len(opening) < 3 || (opening[0] != '`' && opening[0] != '~') {
		return false
	}
	marker, length := opening[0], 0
	for length < len(opening) && opening[length] == marker {
		length++
	}
	end := start
	if block.Lines().Len() > 0 {
		end = block.Lines().At(block.Lines().Len() - 1).Stop
	}
	for _, line := range bytes.Split(source[end:], []byte{'\n'}) {
		trimmed := strings.TrimSpace(string(line))
		count := 0
		for count < len(trimmed) && trimmed[count] == marker {
			count++
		}
		if count >= length && strings.TrimSpace(trimmed[count:]) == "" {
			return true
		}
		if trimmed != "" {
			return false
		}
	}
	return false
}

func joinMarkdownText(blocks []markdownText) string {
	values := make([]string, len(blocks))
	for index, block := range blocks {
		values[index] = block.Text
	}
	return strings.Join(values, "\n\n")
}
