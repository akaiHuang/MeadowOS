---
name: presentation-delivery
description: Deliver editable PPTX and HTML from supplied material using Meadow presentation tools, preserving facts and checking rendered pages.
---

# Presentation delivery

For prose, research or discussion that should become a deck, choose structure and wording from the user's goal. Preserve supplied quantities, dates, source attributions and uncertainty. Do not add an invented timeline or metric to fill a layout.

`presentation_templates` with `operation="markdown"` returns the current compact templates, full Markdown examples, layout capacities and text limits together. If a compact layout fits, author directly with `presentation_from_markdown`; no native-schema lookup or separate preflight is required. H1 is deck metadata; each H2 starts a slide. Follow the returned example's hierarchy and place a layout comment immediately below its H2. Template names and capacities come from the live catalog, not this skill.

If the requested design needs the advanced native interface, inspect the relevant template/layout schema and use `presentation_artifact`. Every slide needs its own title; `presenton_content_json` is a JSON-encoded string when that schema requires it, not a nested object. Choose the interface that fits rather than mixing both contracts.

A rejected call did not render anything. Use the exact structural or readable-fit error to correct the affected field/layout; preserve required content instead of dropping facts to make it fit. Do not repeatedly re-list the catalog when the answer is already available and unchanged.

After rendering, inspect the returned page images with `inspect_image` for clipping, legibility and layout; compare authored text to the source for content fidelity. Renderer success proves file creation, not correct content or visual quality. Pending image briefs are still pending assets. Link actual delivered files and disclose remaining omissions. For edits, preserve the existing artifact identity when supported by the current tool schema.
