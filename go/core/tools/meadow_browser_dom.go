package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/browserruntime"
)

const (
	meadowBrowserDOMDefaultMaxChars = 24_000
	meadowBrowserDOMHardMaxChars    = 120_000
)

var meadowBrowserContentScopeRetryDelays = [...]time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	750 * time.Millisecond,
	1_000 * time.Millisecond,
}

var meadowBrowserPageIdentityPattern = regexp.MustCompile(`(?s)<page\s+url="([^"]*)"\s+title="([^"]*)"`)
var meadowBrowserVisibleTextPattern = regexp.MustCompile(`(?m)<(?:heading|text|link|button|cell|option|summary|label)\b[^>\n]*\btext="([^"]*)"`)
var meadowBrowserLinkNodePattern = regexp.MustCompile(`(?m)<link\s+\[([^\]]+)\]([^>]*)>`)
var meadowBrowserTextAttributePattern = regexp.MustCompile(`\btext="([^"]*)"`)

const meadowBrowserDOMMaxDiscoveryLinks = 24

type meadowBrowserDiscoveryLink struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// MeadowBrowserDOM navigates the native Meadow Browser and returns its
// simplified, rendered DOM contract. Unlike BrowsePage it can reuse the local
// browser's authenticated WebKit session. Unlike Computer Use it gives a
// text-only model structured evidence instead of asking it to guess pixels.
type MeadowBrowserDOM struct {
	WS              *Workspace
	BaseURL         string
	HTTP            *http.Client
	StartLocal      func(context.Context, string) error
	RuntimeIdentity func(context.Context, string) error
	DefaultMaxChars int
}

func (t *MeadowBrowserDOM) Name() string { return "meadow_browser_dom" }
func (t *MeadowBrowserDOM) Description() string {
	return "Open a live page in the Desktop right-hand browser drawer and return its simplified DOM with the final URL, title, visible text, links, tables, and interactive elements. Call this whenever the user should see a website. Prefer it for JavaScript-rendered or authenticated pages. Do not ask the user to open the drawer or switch to Chrome."
}
func (t *MeadowBrowserDOM) Kind() Kind                       { return KindReadOnly }
func (t *MeadowBrowserDOM) ParallelSafe(map[string]any) bool { return false }

func (t *MeadowBrowserDOM) EvidenceSources(args map[string]any, result Result) []EvidenceSource {
	requestedURL := strings.TrimSpace(strArg(args, "url"))
	finalURL := evidenceLine(result.LLMContent, "final_url:")
	if finalURL == "" {
		finalURL = requestedURL
	}
	title := evidenceLine(result.LLMContent, "title:")
	if title == "" {
		if parsed, err := url.Parse(finalURL); err == nil {
			title = parsed.Host
		}
	}
	if finalURL == "" {
		return nil
	}
	return []EvidenceSource{{Title: title, URL: finalURL}}
}

func evidenceLine(content, prefix string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func (t *MeadowBrowserDOM) Schema() map[string]any {
	defaultMaxChars := t.defaultMaxChars()
	return obj(map[string]any{
		"url":        str("Full http(s) source URL to open."),
		"selector":   str("Optional CSS selector that scopes extraction to one page region."),
		"max_depth":  num("Optional DOM tree depth from 1 to 12. Omit for the full simplified page."),
		"max_chars":  num(fmt.Sprintf("Maximum returned DOM characters from 1000 to 120000. Default %d; increase only when the first evidence excerpt is insufficient.", defaultMaxChars)),
		"settle_ms":  num("Optional render settle time in milliseconds. Default 1800, maximum 10000."),
		"wait_until": str("Navigation readiness: network_idle or load. Default network_idle."),
	}, "url")
}

func (t *MeadowBrowserDOM) defaultMaxChars() int {
	if t != nil && t.DefaultMaxChars >= 1_000 && t.DefaultMaxChars <= meadowBrowserDOMHardMaxChars {
		return t.DefaultMaxChars
	}
	return meadowBrowserDOMDefaultMaxChars
}

func (t *MeadowBrowserDOM) Execute(ctx context.Context, args map[string]any) Result {
	ctx = desktopBrowserPageContext(ctx, t.BaseURL)
	rawURL := strings.TrimSpace(strArg(args, "url"))
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errResultf("url must be a full http(s) URL")
	}

	settle := 1800
	if value, ok := intArg(args, "settle_ms"); ok {
		if value < 0 || value > 10_000 {
			return errResultf("settle_ms must be between 0 and 10000")
		}
		settle = value
	}
	waitUntil := strings.TrimSpace(strArg(args, "wait_until"))
	if waitUntil == "" {
		waitUntil = "network_idle"
	}
	if waitUntil != "network_idle" && waitUntil != "load" {
		return errResultf("wait_until must be network_idle or load")
	}
	depth := 0
	if value, ok := intArg(args, "max_depth"); ok {
		if value < 1 || value > 12 {
			return errResultf("max_depth must be between 1 and 12")
		}
		depth = value
	}
	maxChars := t.defaultMaxChars()
	if value, ok := intArg(args, "max_chars"); ok {
		if value < 1000 || value > meadowBrowserDOMHardMaxChars {
			return errResultf("max_chars must be between 1000 and %d", meadowBrowserDOMHardMaxChars)
		}
		maxChars = value
	}

	helper := &MeadowBrowserVisualCheck{
		WS: t.WS, BaseURL: t.BaseURL, HTTP: t.HTTP, StartLocal: t.StartLocal,
		RuntimeIdentity: t.RuntimeIdentity,
	}
	base := helper.base()
	if _, _, err := helper.ensureAvailable(ctx, base); err != nil {
		return browserRuntimeGuardResult(fmt.Errorf("Meadow Browser is unavailable at %s: %w", base, err))
	}
	if err := helper.postJSON(ctx, base+"/goto", map[string]any{
		"url": rawURL, "wait_until": waitUntil, "settle_ms": settle,
	}, nil); err != nil {
		return errResult(fmt.Errorf("Meadow Browser goto %s: %w", rawURL, err))
	}

	domRequest := map[string]any{}
	if selector := strings.TrimSpace(strArg(args, "selector")); selector != "" {
		domRequest["selector"] = selector
	}
	if depth > 0 {
		domRequest["max_depth"] = depth
	}
	var dom json.RawMessage
	if err := helper.postJSON(ctx, base+"/dom", domRequest, &dom); err != nil {
		return errResultf("Meadow Browser DOM extraction: %w", err)
	}
	if len(dom) == 0 || string(dom) == "null" {
		return errResultf("Meadow Browser returned an empty DOM")
	}

	// Preserve the full page envelope for identity and link discovery, but feed
	// the model the primary article body when Meadow Browser exposes only a
	// large simplified tree. Documentation sites commonly put thousands of
	// navigation nodes before the article; truncating that full tree can remove
	// every factual sentence even though the rendered page is valid.
	contentDOM := dom
	contentSelector := strings.TrimSpace(strArg(args, "selector"))
	if contentSelector == "" && domNeedsContentScope(dom) {
		var runtimeErr error
		selectPrimaryContent := func() bool {
			for _, candidate := range []string{"article", "main"} {
				scopeRequest := map[string]any{"selector": candidate}
				if depth > 0 {
					scopeRequest["max_depth"] = depth
				}
				var scoped json.RawMessage
				if scopeErr := helper.postJSON(ctx, base+"/dom", scopeRequest, &scoped); scopeErr != nil {
					if browserruntime.Code(scopeErr) != "" {
						runtimeErr = scopeErr
						return true
					}
					continue
				}
				if !domContentScopeMeaningful(scoped) {
					continue
				}
				contentDOM = scoped
				contentSelector = candidate
				return true
			}
			return false
		}
		// Documentation shells often report navigation ready before WebKit has
		// hydrated their paragraph and code nodes. A single 500 ms retry proved
		// too short on real MLX documentation: the correct official page was
		// reduced to its navigation tree, rejected as non-article content, and
		// the Research Kernel opened a long tail of irrelevant sources. Poll the
		// primary content region for a small bounded window instead. The first
		// meaningful article/main body wins, so already-rendered pages still pay
		// no delay and cancellation remains immediate.
		_ = waitForDOMContentScope(ctx, selectPrimaryContent)
		if runtimeErr != nil {
			return browserRuntimeGuardResult(runtimeErr)
		}
	}

	payload := string(contentDOM)
	truncated := len([]rune(payload)) > maxChars
	if truncated {
		payload = string([]rune(payload)[:maxChars])
	}

	finalURL, title := domIdentity(dom)
	visibleText := domVisibleText(contentDOM)
	discoveryLinks := domDiscoveryLinks(dom, finalURL)
	var out strings.Builder
	out.WriteString("Meadow Browser rendered DOM evidence\n")
	fmt.Fprintf(&out, "requested_url: %s\n", rawURL)
	if finalURL != "" {
		fmt.Fprintf(&out, "final_url: %s\n", finalURL)
	}
	if title != "" {
		fmt.Fprintf(&out, "title: %s\n", title)
	}
	if contentSelector != "" {
		fmt.Fprintf(&out, "content_selector: %s\n", contentSelector)
	}
	fmt.Fprintf(&out, "captured_at: %s\n", time.Now().UTC().Format(time.RFC3339))
	if len(discoveryLinks) > 0 {
		// Links are discovery candidates only, never evidence. Keep their full
		// resolved URLs ahead of the potentially truncated raw DOM so a section
		// landing page can lead the Research Kernel to an actual article without
		// asking a small model to reconstruct hrefs from selector syntax.
		if encoded, encodeErr := json.Marshal(discoveryLinks); encodeErr == nil {
			fmt.Fprintf(&out, "discovery_links_json: %s\n", encoded)
		}
	}
	if visibleText != "" {
		// Keep rendered prose ahead of selector metadata. The RAG store chunks
		// this response verbatim, so a text-first copy gives a small embedding
		// model clean semantic material while the raw DOM below still preserves
		// the browser contract for diagnostics and source identity.
		out.WriteString("visible_text:\n")
		out.WriteString(visibleText)
		out.WriteByte('\n')
	}
	if truncated {
		out.WriteString("dom_json_excerpt:\n")
	} else {
		out.WriteString("dom_json:\n")
	}
	out.WriteString(payload)
	if truncated {
		out.WriteString("\n[DOM excerpt truncated; narrow selector or increase max_chars]")
	}
	displayTitle := title
	if displayTitle == "" {
		displayTitle = parsed.Host
	}
	passages := []EvidencePassage{}
	if strings.TrimSpace(visibleText) != "" && finalURL != "" {
		passage := exactEvidencePassage(EvidenceSource{URL: finalURL, Title: title,
			ContentKind: "body", FetchedAt: time.Now().UTC().Format(time.RFC3339)}, visibleText, "returned_rendered_fragment")
		passage.Locator, _ = json.Marshal(map[string]string{"content_selector": contentSelector})
		passages = append(passages, passage)
	}
	return Result{
		LLMContent:     out.String(),
		Display:        "DOM extracted: " + compactOneLine(displayTitle, 72),
		SourcePassages: passages,
	}
}

func waitForDOMContentScope(ctx context.Context, selectPrimaryContent func() bool) bool {
	if selectPrimaryContent == nil {
		return false
	}
	if selectPrimaryContent() {
		return true
	}
	for _, delay := range meadowBrowserContentScopeRetryDelays {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return false
		case <-timer.C:
		}
		if selectPrimaryContent() {
			return true
		}
	}
	return false
}

// domNeedsContentScope distinguishes the production tree envelope from the
// older compact /dom response whose top-level text is already clean prose.
// Avoiding an extra selector request for the latter keeps simple pages fast.
func domNeedsContentScope(raw json.RawMessage) bool {
	var value struct {
		Text string `json:"text"`
		Tree string `json:"tree"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return strings.TrimSpace(value.Text) == "" && strings.TrimSpace(value.Tree) != ""
}

func domContentScopeMeaningful(raw json.RawMessage) bool {
	text := domVisibleText(raw)
	if len([]rune(text)) < 160 {
		return false
	}
	// A documentation shell can expose a long sequence of section headings
	// before its paragraph/code nodes finish rendering. Treating that heading
	// outline as the article body produces a clean-looking but fact-free RAG
	// document. Require several actual text nodes so the next selector (or a
	// later retry) can recover the rendered body instead of accepting a table of
	// contents as evidence.
	var value struct {
		Tree string `json:"tree"`
	}
	if json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value.Tree) != "" {
		if strings.Count(value.Tree, "<text ") < 3 {
			return false
		}
	}
	lines := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	return lines >= 2
}

// domDiscoveryLinks extracts a bounded set of full links from Meadow
// Browser's production tree + selectorMap contract. DOMSimplifier truncates
// displayed href attributes for readability, while selectorMap deliberately
// preserves the complete selector. Prefer that complete value and use the
// tree only to associate a human-readable label with the stable selector ID.
func domDiscoveryLinks(raw json.RawMessage, pageURL string) []meadowBrowserDiscoveryLink {
	var value struct {
		Tree        string            `json:"tree"`
		SelectorMap map[string]string `json:"selectorMap"`
		Links       []struct {
			Text string `json:"text"`
			Href string `json:"href"`
		} `json:"links"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	base, _ := url.Parse(strings.TrimSpace(pageURL))
	titles := make(map[string]string)
	for _, match := range meadowBrowserLinkNodePattern.FindAllStringSubmatch(value.Tree, -1) {
		if len(match) < 2 {
			continue
		}
		label := ""
		if len(match) > 2 {
			if textMatch := meadowBrowserTextAttributePattern.FindStringSubmatch(match[2]); len(textMatch) == 2 {
				label = strings.Join(strings.Fields(html.UnescapeString(textMatch[1])), " ")
			}
		}
		titles[match[1]] = label
	}

	links := make([]meadowBrowserDiscoveryLink, 0, len(value.SelectorMap)+len(value.Links))
	appendLink := func(rawURL, title string) {
		rawURL = strings.TrimSpace(html.UnescapeString(rawURL))
		if rawURL == "" {
			return
		}
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return
		}
		if base != nil {
			parsed = base.ResolveReference(parsed)
		}
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return
		}
		parsed.Fragment = ""
		resolved := parsed.String()
		if pageURL != "" && resolved == pageURL {
			return
		}
		links = append(links, meadowBrowserDiscoveryLink{
			URL: resolved, Title: strings.Join(strings.Fields(html.UnescapeString(title)), " "),
		})
	}
	for selectorID, selector := range value.SelectorMap {
		const prefix = `a[href="`
		if !strings.HasPrefix(selector, prefix) || !strings.HasSuffix(selector, `"]`) {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(selector, prefix), `"]`)
		// DOMSimplifier escapes only backslashes and quotes. strconv.Unquote
		// exactly reverses that bounded representation without interpreting CSS.
		decoded, err := strconv.Unquote(`"` + inner + `"`)
		if err != nil {
			continue
		}
		appendLink(decoded, titles[selectorID])
	}
	// Older /dom fixtures expose a top-level links array. Preserve compatibility
	// while production uses tree + selectorMap.
	for _, link := range value.Links {
		appendLink(link.Href, link.Text)
	}

	sort.SliceStable(links, func(i, j int) bool {
		if links[i].URL != links[j].URL {
			return links[i].URL < links[j].URL
		}
		return links[i].Title < links[j].Title
	})
	result := links[:0]
	seen := make(map[string]bool, len(links))
	for _, link := range links {
		if seen[link.URL] {
			continue
		}
		seen[link.URL] = true
		result = append(result, link)
		if len(result) == meadowBrowserDOMMaxDiscoveryLinks {
			break
		}
	}
	return result
}

func domVisibleText(raw json.RawMessage) string {
	var value struct {
		Text string `json:"text"`
		Tree string `json:"tree"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	if text := compactDOMVisibleLines(value.Text); text != "" {
		return text
	}
	if strings.TrimSpace(value.Tree) == "" {
		return ""
	}
	matches := meadowBrowserVisibleTextPattern.FindAllStringSubmatch(value.Tree, -1)
	if len(matches) == 0 {
		return ""
	}
	lines := make([]string, 0, len(matches))
	previous := ""
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		line := strings.Join(strings.Fields(html.UnescapeString(match[1])), " ")
		if line == "" || line == previous {
			continue
		}
		lines = append(lines, line)
		previous = line
	}
	return strings.Join(lines, "\n")
}

func compactDOMVisibleLines(value string) string {
	lines := make([]string, 0)
	previous := ""
	for _, raw := range strings.Split(value, "\n") {
		line := strings.Join(strings.Fields(html.UnescapeString(raw)), " ")
		if line == "" || line == previous {
			continue
		}
		lines = append(lines, line)
		previous = line
	}
	return strings.Join(lines, "\n")
}

func domIdentity(raw json.RawMessage) (string, string) {
	var value struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Tree  string `json:"tree"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return "", ""
	}
	observedURL := strings.TrimSpace(value.URL)
	title := strings.TrimSpace(value.Title)
	// Meadow Browser's production /dom contract wraps the identity inside the
	// first <page> node of `tree`; older fixtures returned top-level url/title.
	// Accept both so redirects and rendered error pages are never mislabeled as
	// the requested URL merely because the adapter read the wrong JSON shape.
	if (observedURL == "" || title == "") && value.Tree != "" {
		if match := meadowBrowserPageIdentityPattern.FindStringSubmatch(value.Tree); len(match) == 3 {
			if observedURL == "" {
				observedURL = html.UnescapeString(strings.TrimSpace(match[1]))
			}
			if title == "" {
				title = html.UnescapeString(strings.TrimSpace(match[2]))
			}
		}
	}
	return observedURL, title
}

func compactOneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}
