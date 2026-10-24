package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
)

const presentationMarkdownDiagnosticMaxBytes = 2 << 10

// Diagnostics describe the supplied structure and reviewed slot contracts.
// They never move authored blocks, choose a narrative, or modify the source.
func markdownSlideStructure(slide *markdownSlide) string {
	if slide == nil {
		return "no H2 slide exists; H1 is deck metadata, not a slide"
	}
	kind := slide.ItemKind
	if kind == "" {
		kind = "none"
	}
	title := []rune(slide.Title.Text)
	if len(title) > 60 {
		title = append(title[:60], '…')
	}
	return fmt.Sprintf("slide %d %q (H2 line %d): lead paragraphs=%d, item kind=%s, items=%d, images=%d", slide.Number, string(title), slide.Title.Line, len(slide.Lead), kind, len(slide.Items), len(slide.Images))
}

func markdownAdapterRequirement(adapter markdownAdapter) string {
	items := fmt.Sprintf("%d–%d", adapter.MinItems, adapter.MaxItems)
	if adapter.MinItems == adapter.MaxItems {
		items = fmt.Sprint(adapter.MinItems)
	}
	images := "0"
	if adapter.Grammar.Images == "required" {
		images = "1 (local PNG/JPEG or explicit placeholder:photo)"
	}
	return fmt.Sprintf("%s: lead=%s; item kind=%s; items=%s; images=%s", adapter.Alias, adapter.Grammar.Lead, adapter.Grammar.Items, items, images)
}

func markdownAdapterMismatches(slide *markdownSlide, adapter markdownAdapter) []string {
	problems := []string{}
	if adapter.Grammar.Lead == "required" && len(slide.Lead) == 0 {
		problems = append(problems, "missing lead paragraph directly below the layout comment and before items")
	} else if adapter.Grammar.Lead == "forbidden" && len(slide.Lead) != 0 {
		problems = append(problems, "lead paragraphs are forbidden in this layout")
	}
	wantedKind := map[string]string{"ordered_list": "list", "sections": "sections", "none": ""}[adapter.Grammar.Items]
	if slide.ItemKind != wantedKind {
		if wantedKind == "sections" {
			problems = append(problems, "items must be H3 headings, each followed by a body paragraph; list items are not H3 sections")
		} else if wantedKind == "list" {
			problems = append(problems, "items must be a plain ordered or unordered list; H3 sections are forbidden")
		} else {
			problems = append(problems, "item sections and lists are forbidden in this layout")
		}
	}
	if len(slide.Items) < adapter.MinItems || len(slide.Items) > adapter.MaxItems {
		problems = append(problems, fmt.Sprintf("item count %d is outside %d–%d", len(slide.Items), adapter.MinItems, adapter.MaxItems))
	}
	if adapter.Grammar.Images == "required" && len(slide.Images) != 1 {
		problems = append(problems, fmt.Sprintf("requires exactly one standalone image; received %d", len(slide.Images)))
	} else if adapter.Grammar.Images != "required" && len(slide.Images) != 0 {
		problems = append(problems, "images are forbidden in this layout")
	}
	return problems
}

func markdownDiagnostic(problem string, line int, slide *markdownSlide, repair, exampleAlias string) error {
	message := fmt.Sprintf("markdown line %d: %s\nActual: %s.", line, problem, markdownSlideStructure(slide))
	if repair != "" {
		message += "\nRepair: " + repair
	}
	message += "\nNo content was moved, omitted or rendered."
	// Keep the whole example or omit it; do not return a truncated skeleton.
	if example := markdownAdapterExample(exampleAlias); example != "" {
		addition := "\nValid structure example (replace its sample text with your own content):\n```markdown\n" + strings.TrimSpace(example) + "\n```"
		if len(message)+len(addition) <= presentationMarkdownDiagnosticMaxBytes {
			message += addition
		}
	}
	if len(message) > presentationMarkdownDiagnosticMaxBytes {
		const suffix = "\nSee presentation_templates operation=markdown for complete structure examples."
		message = message[:presentationMarkdownDiagnosticMaxBytes-len(suffix)]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += suffix
	}
	return fmt.Errorf("%s", message)
}

func markdownLayoutPlacementError(source []byte, node ast.Node, slide *markdownSlide, content string) error {
	alias := ""
	if strings.HasPrefix(content, "<!-- layout:") && strings.HasSuffix(content, "-->") {
		candidate := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(content, "<!-- layout:"), "-->"))
		if _, known := markdownAdapterForAlias(candidate); known {
			alias = candidate
		}
	}
	repair := "Raw HTML is unsupported. Use only one advertised layout comment as the first block after the intended H2 slide heading."
	if alias != "" {
		switch {
		case slide == nil:
			repair = "Add an H2 heading for this slide after the H1 deck title, then put its layout comment immediately below that H2. A subtitle written as H2 creates another slide."
		case slide.LayoutAlias != "":
			repair = "This H2 already has a layout comment. Keep exactly one per slide; put a different slide's comment immediately below that slide's own H2 heading."
		default:
			repair = "Move this layout comment directly below the intended H2 heading, before any paragraph, list, H3 section or image. A comment before the next H2 still belongs to the preceding slide."
		}
	}
	return markdownDiagnostic("only one <!-- layout: alias --> comment directly below H2 is supported; raw HTML is never executed", markdownLine(source, node), slide, repair, alias)
}

func markdownLayoutCompatibilityError(slide *markdownSlide, selected *markdownAdapter, manifest *markdownAdapterManifest) error {
	repair := "Choose a structure compatible with your intended layout; do not add unsupported facts to fill slots. H1 is metadata; every H2 creates a slide, so a subtitle must not accidentally be another H2."
	alias := ""
	if selected != nil {
		alias = selected.Alias
		repair += "\nRequired: " + markdownAdapterRequirement(*selected) + "."
		repair += "\nProblems: " + strings.Join(markdownAdapterMismatches(slide, *selected), "; ") + "."
	} else {
		repair += "\nAvailable structures:"
		for _, adapter := range manifest.Adapters {
			repair += "\n- " + markdownAdapterRequirement(adapter)
		}
	}
	return markdownDiagnostic("no compatible declared layout", slide.Title.Line, slide, repair, alias)
}
