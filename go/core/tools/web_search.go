package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	searchTimeout        = 10 * time.Second
	searchDefaultResults = 5
	searchMaxResults     = 10
)

type searchHit struct {
	Title   string
	URL     string
	Snippet string
}

// WebSearch is a read-only retrieval tool: network access alone does not require
// execution approval. It uses a host-selected member search proxy, configured
// Meadow Search, or the legacy provider
// chain: Brave (if BRAVE_SEARCH_API_KEY) → Google CSE (if GOOGLE_CSE_KEY+GOOGLE_CSE_CX) →
// DuckDuckGo HTML scrape (no key needed).
type WebSearch struct {
	HTTP *http.Client
}

func (t *WebSearch) Name() string { return "web_search" }
func (t *WebSearch) Description() string {
	return "Search the web. Uses the host-configured private search proxy when selected, otherwise the host-configured Meadow Search adapter, which can integrate SearXNG; without that adapter, tries configured Brave, Google CSE, then DuckDuckGo HTML. This description does not verify the active upstream engine, service readiness or successful retrieval; inspect actual tool results. Search titles and snippets alone are not source-body evidence."
}
func (t *WebSearch) Kind() Kind { return KindReadOnly }
func (t *WebSearch) ParallelSafe(map[string]any) bool {
	return !memberWebSearchConfigured() && !meadowSearchConfigured()
}
func (t *WebSearch) SubtaskSafe() bool { return true }

func (t *WebSearch) Schema() map[string]any {
	return obj(map[string]any{
		"query":       str("Search query."),
		"num_results": num("Number of results, 1-10."),
	}, "query")
}

func (t *WebSearch) client() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return &http.Client{Timeout: searchTimeout}
}

func (t *WebSearch) Execute(ctx context.Context, args map[string]any) Result {
	rawQuery := strArg(args, "query")
	query := strings.TrimSpace(rawQuery)
	if query == "" {
		return errResultf("query parameter is required")
	}
	n := searchDefaultResults
	if v, ok := intArg(args, "num_results"); ok {
		n = max(1, min(v, searchMaxResults))
	}
	if memberWebSearchConfigured() {
		return executeMemberWebSearch(ctx, t.HTTP, rawQuery, n)
	}
	if meadowSearchConfigured() {
		return executeMeadowSearch(ctx, t.HTTP, query, n)
	}

	type provider struct {
		name string
		run  func(context.Context, string, int) ([]searchHit, error)
	}
	chain := []provider{}
	if os.Getenv("BRAVE_SEARCH_API_KEY") != "" {
		chain = append(chain, provider{"brave", t.searchBrave})
	}
	if os.Getenv("GOOGLE_CSE_KEY") != "" && os.Getenv("GOOGLE_CSE_CX") != "" {
		chain = append(chain, provider{"google", t.searchGoogleCSE})
	}
	chain = append(chain, provider{"ddg", t.searchDDG})

	var lastErr error
	for _, p := range chain {
		hits, err := p.run(ctx, query, n)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", p.name, err)
			continue
		}
		if len(hits) == 0 {
			lastErr = fmt.Errorf("%s returned no results", p.name)
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Web search results for %q (via %s):\n", query, p.name)
		for i, h := range hits {
			fmt.Fprintf(&b, "\n%d. %s\n   URL: %s\n   %s\n", i+1, h.Title, h.URL, h.Snippet)
		}
		return Result{
			LLMContent: strings.TrimRight(b.String(), "\n"),
			Display:    fmt.Sprintf("%d results via %s", len(hits), p.name),
		}
	}
	return errResultf("all search providers failed: %v", lastErr)
}

func (t *WebSearch) EvidenceSources(_ map[string]any, result Result) []EvidenceSource {
	if sources := memberWebSearchEvidenceSources(result); sources != nil {
		return sources
	}
	return meadowSearchEvidenceSources(result)
}

func (t *WebSearch) searchBrave(ctx context.Context, query string, n int) ([]searchHit, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.search.brave.com/res/v1/web/search?q="+url.QueryEscape(query), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Subscription-Token", os.Getenv("BRAVE_SEARCH_API_KEY"))
	req.Header.Set("Accept", "application/json")
	resp, err := t.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	var hits []searchHit
	for _, r := range parsed.Web.Results {
		hits = append(hits, searchHit{Title: stripTags(r.Title), URL: r.URL, Snippet: stripTags(r.Description)})
		if len(hits) == n {
			break
		}
	}
	return hits, nil
}

func (t *WebSearch) searchGoogleCSE(ctx context.Context, query string, n int) ([]searchHit, error) {
	u := fmt.Sprintf("https://www.googleapis.com/customsearch/v1?key=%s&cx=%s&q=%s&num=%d",
		url.QueryEscape(os.Getenv("GOOGLE_CSE_KEY")), url.QueryEscape(os.Getenv("GOOGLE_CSE_CX")),
		url.QueryEscape(query), n)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	var hits []searchHit
	for _, r := range parsed.Items {
		hits = append(hits, searchHit{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
		if len(hits) == n {
			break
		}
	}
	return hits, nil
}

var (
	ddgResultRe  = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	tagRe        = regexp.MustCompile(`<[^>]+>`)
)

func (t *WebSearch) searchDDG(ctx context.Context, query string, n int) ([]searchHit, error) {
	form := url.Values{"q": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")
	resp, err := t.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body := make([]byte, 0, 256*1024)
	buf := make([]byte, 32*1024)
	for {
		m, rerr := resp.Body.Read(buf)
		body = append(body, buf[:m]...)
		if rerr != nil || len(body) > 2*1024*1024 {
			break
		}
	}
	html := string(body)

	links := ddgResultRe.FindAllStringSubmatch(html, n*2)
	snippets := ddgSnippetRe.FindAllStringSubmatch(html, n*2)
	var hits []searchHit
	for i, l := range links {
		h := searchHit{Title: stripTags(l[2]), URL: unwrapDDGURL(l[1])}
		if i < len(snippets) {
			h.Snippet = stripTags(snippets[i][1])
		}
		hits = append(hits, h)
		if len(hits) == n {
			break
		}
	}
	return hits, nil
}

// unwrapDDGURL turns //duckduckgo.com/l/?uddg=<encoded> redirects into the
// real target URL.
func unwrapDDGURL(raw string) string {
	if !strings.Contains(raw, "duckduckgo.com/l/") {
		return raw
	}
	if u, err := url.Parse(raw); err == nil {
		if target := u.Query().Get("uddg"); target != "" {
			return target
		}
	}
	return raw
}

func stripTags(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	for entity, repl := range map[string]string{
		"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'", "&nbsp;": " ",
	} {
		s = strings.ReplaceAll(s, entity, repl)
	}
	return strings.TrimSpace(s)
}
