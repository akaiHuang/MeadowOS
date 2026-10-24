// Package theme defines the three launch palettes for the Bubble Tea TUI —
// meadow (brand grass greens + blossom accents), ayu (the palette the TS
// CLI ships today), and mist (single green accent, everything else gray).
// The selection persists to settings.json ui.theme, the same key the TS
// CLI reads, so the choice follows the user across both CLIs.
package theme

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Theme is one named palette. Fields are semantic, not per-widget: views
// derive their lipgloss styles from these via Styles().
type Theme struct {
	Name  string
	Label string // human label shown in /theme

	Background lipgloss.Color // terminal bg hint (not forced; views stay transparent)
	Text       lipgloss.Color
	TextDim    lipgloss.Color
	TextBright lipgloss.Color

	Accent    lipgloss.Color // prompt symbol, selection, links
	Secondary lipgloss.Color // tool names, secondary highlights
	Warn      lipgloss.Color // approve-always, cautions
	Error     lipgloss.Color
	Success   lipgloss.Color

	DiffAdd    lipgloss.Color
	DiffRemove lipgloss.Color

	Border    lipgloss.Color // tool/confirmation boxes
	BorderDim lipgloss.Color // separators, status line rule

	// Logo blossoms; equal in mist (monochrome logo).
	BlossomPink lipgloss.Color
	BlossomGold lipgloss.Color
	// LogoRamp colors the wordmark letters left to right.
	LogoRamp []lipgloss.Color
}

// Meadow is the brand theme: Header.tsx grass ramp + blossom hues.
var Meadow = Theme{
	Name:  "meadow",
	Label: "Meadow 草原",

	Background: "#0c1210",
	Text:       "#c9d4c9",
	TextDim:    "#5f6f5f",
	TextBright: "#e8f0e8",

	Accent:    "#4ade80",
	Secondary: "#86efac",
	Warn:      "#fbbf24",
	Error:     "#f87171",
	Success:   "#4ade80",

	DiffAdd:    "#4ade80",
	DiffRemove: "#f87171",

	Border:    "#1e4d2b",
	BorderDim: "#1e2e1e",

	BlossomPink: "#f9a8d4",
	BlossomGold: "#fbbf24",
	LogoRamp:    []lipgloss.Color{"#86efac", "#6ee7a0", "#4ade80", "#3bc873", "#2da55f", "#166534"},
}

// Ayu mirrors the qwen-dark/ayu palette the TS CLI uses today.
var Ayu = Theme{
	Name:  "ayu",
	Label: "Ayu 現行",

	Background: "#0b0e14",
	Text:       "#bfbdb6",
	TextDim:    "#646A71",
	TextBright: "#e6e1d6",

	Accent:    "#39BAE6",
	Secondary: "#D2A6FF",
	Warn:      "#FFD700",
	Error:     "#F26D78",
	Success:   "#AAD94C",

	DiffAdd:    "#AAD94C",
	DiffRemove: "#F26D78",

	Border:    "#2d3640",
	BorderDim: "#1c2128",

	BlossomPink: "#FFD700",
	BlossomGold: "#da7959",
	LogoRamp:    []lipgloss.Color{"#FFD700", "#f4c75e", "#e9b04a", "#e09a3f", "#dd8a4c", "#da7959"},
}

// Mist is the quiet one: a single green accent over grays.
var Mist = Theme{
	Name:  "mist",
	Label: "Mist 晨霧極簡",

	Background: "#101312",
	Text:       "#b6bdb6",
	TextDim:    "#566056",
	TextBright: "#dde3dd",

	Accent:    "#4ade80",
	Secondary: "#4ade80",
	Warn:      "#b6bdb6",
	Error:     "#9b6b6b",
	Success:   "#4ade80",

	DiffAdd:    "#84cc8e",
	DiffRemove: "#9b6b6b",

	Border:    "#2a302a",
	BorderDim: "#232823",

	BlossomPink: "#4ade80",
	BlossomGold: "#4ade80",
	LogoRamp:    []lipgloss.Color{"#4ade80", "#4ade80", "#4ade80", "#4ade80", "#4ade80", "#4ade80"},
}

// All lists the launch themes in /theme display order.
var All = []Theme{Meadow, Ayu, Mist}

// ByName returns the named theme, falling back to Meadow.
func ByName(name string) Theme {
	for _, t := range All {
		if t.Name == name {
			return t
		}
	}
	return Meadow
}

// Styles is the derived lipgloss style set views actually render with.
type Styles struct {
	Text       lipgloss.Style
	Dim        lipgloss.Style
	Bright     lipgloss.Style
	Accent     lipgloss.Style
	Secondary  lipgloss.Style
	Warn       lipgloss.Style
	Error      lipgloss.Style
	Success    lipgloss.Style
	DiffAdd    lipgloss.Style
	DiffRemove lipgloss.Style

	Prompt    lipgloss.Style // the "›" marker
	ToolBox   lipgloss.Style // confirmation / tool-call frame
	Separator lipgloss.Style
	StatusBar lipgloss.Style
	Selected  lipgloss.Style // menu selection row
}

func (t Theme) Styles() Styles {
	return Styles{
		Text:       lipgloss.NewStyle().Foreground(t.Text),
		Dim:        lipgloss.NewStyle().Foreground(t.TextDim),
		Bright:     lipgloss.NewStyle().Foreground(t.TextBright),
		Accent:     lipgloss.NewStyle().Foreground(t.Accent),
		Secondary:  lipgloss.NewStyle().Foreground(t.Secondary),
		Warn:       lipgloss.NewStyle().Foreground(t.Warn),
		Error:      lipgloss.NewStyle().Foreground(t.Error),
		Success:    lipgloss.NewStyle().Foreground(t.Success),
		DiffAdd:    lipgloss.NewStyle().Foreground(t.DiffAdd),
		DiffRemove: lipgloss.NewStyle().Foreground(t.DiffRemove),

		Prompt: lipgloss.NewStyle().Foreground(t.Accent).Bold(true),
		ToolBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(t.Border).
			Padding(0, 1),
		Separator: lipgloss.NewStyle().Foreground(t.BorderDim),
		StatusBar: lipgloss.NewStyle().Foreground(t.TextDim),
		Selected:  lipgloss.NewStyle().Foreground(t.Background).Background(t.Accent).Bold(true),
	}
}

// Logo renders the flowered wordmark with the theme's ramp.
func (t Theme) Logo() string {
	word := "meadow"
	out := lipgloss.NewStyle().Foreground(t.BlossomPink).Render("✿") + " "
	for i, ch := range word {
		c := t.LogoRamp[len(t.LogoRamp)-1]
		if i < len(t.LogoRamp) {
			c = t.LogoRamp[i]
		}
		out += lipgloss.NewStyle().Foreground(c).Render(string(ch))
	}
	out += " " + lipgloss.NewStyle().Foreground(t.BlossomGold).Render("❀")
	return out
}

// bannerArt is the flowered-meadow startup banner from the TS CLI
// (AsciiArt.ts) — grass blades on a horizon with two blossoms. The glyphs
// carry meaning for the colorizer: ✿ pink, ❀ gold, ▔ horizon.
var bannerArt = []string{
	"    ✿   ╿   ╿     ❀    ╿",
	"  ╿ ┃ ╿ ┃ ╿ ┃ ╿ ┃ ╿ ┃ ╿",
	"▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔",
}

// Banner colors the meadow art with this theme's palette: per-line grass
// shades from the logo ramp, blossom spot colors, deep horizon.
func (t Theme) Banner() string {
	grassByLine := []lipgloss.Color{
		t.LogoRamp[0],
		t.LogoRamp[min(2, len(t.LogoRamp)-1)],
		t.LogoRamp[len(t.LogoRamp)-1],
	}
	horizon := t.LogoRamp[len(t.LogoRamp)-1]
	var out []string
	for li, line := range bannerArt {
		var b string
		for _, ch := range line {
			switch ch {
			case '✿':
				b += lipgloss.NewStyle().Foreground(t.BlossomPink).Render(string(ch))
			case '❀':
				b += lipgloss.NewStyle().Foreground(t.BlossomGold).Render(string(ch))
			case '▔':
				b += lipgloss.NewStyle().Foreground(horizon).Render(string(ch))
			case ' ':
				b += " "
			default:
				b += lipgloss.NewStyle().Foreground(grassByLine[li]).Render(string(ch))
			}
		}
		out = append(out, b)
	}
	return strings.Join(out, "\n")
}
