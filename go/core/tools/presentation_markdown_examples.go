package tools

import (
	"fmt"
	"strings"
)

// Examples teach the reviewed syntax only. They are never inserted into an
// authored deck or chosen using its topic, audience or narrative.
func markdownAdapterExample(alias string) string {
	adapter, ok := markdownAdapterForAlias(alias)
	if !ok {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# Deck\n\n## Slide\n<!-- layout: %s -->\n\n", alias)
	if adapter.Grammar.Lead == "required" {
		out.WriteString("Summary.\n\n")
	}
	for i := 1; i <= adapter.MinItems; i++ {
		switch adapter.Grammar.Items {
		case "ordered_list":
			fmt.Fprintf(&out, "%d. Point %d\n", i, i)
		case "sections":
			fmt.Fprintf(&out, "### Point %d\nDetails.\n\n", i)
		}
	}
	if adapter.Grammar.Images == "required" {
		out.WriteString("![Replace image](placeholder:photo)\n")
	}
	return out.String()
}
