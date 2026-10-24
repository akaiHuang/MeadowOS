package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	webCrawlTimeout        = 20 * time.Second
	webCrawlDefaultPages   = 8
	webCrawlMaxPages       = 30
	webCrawlDefaultDepth   = 1
	webCrawlMaxDepth       = 3
	webCrawlMaxReadBytes   = 500_000
	webCrawlMaxOutputBytes = 120_000
)

// WebCrawl is a bounded local crawler for Meadow research tasks. It starts at
// one URL, fetches pages, extracts links, filters them, and returns a compact
// corpus to the model. It is intentionally local/in-process; heavyweight proxy,
// queue, and cache backends belong behind optional adapters.
type WebCrawl struct {
	HTTP *http.Client

	// Test-only seams remain package-private so a model or product config cannot
	// disable the outbound public-network boundary.
	allowPrivateTestTargets bool
	networkPolicy           *publicHTTPPolicy
}

func (t *WebCrawl) Name() string { return "web_crawl" }
func (t *WebCrawl) Description() string {
	return "Crawl a small bounded set of pages from a website and return compact page text, titles, URLs, and discovered links. Use for website research, docs crawling, and collecting several related pages."
}
func (t *WebCrawl) Kind() Kind                       { return KindExecute }
func (t *WebCrawl) ParallelSafe(map[string]any) bool { return true }
func (t *WebCrawl) SubtaskSafe() bool                { return true }

func (t *WebCrawl) EvidenceSources(_ map[string]any, result Result) []EvidenceSource {
	lines := strings.Split(result.LLMContent, "\n")
	sources := make([]EvidenceSource, 0, 8)
	title := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if separator := strings.Index(trimmed, ". "); separator > 0 {
			numbered := true
			for _, char := range trimmed[:separator] {
				if char < '0' || char > '9' {
					numbered = false
					break
				}
			}
			if numbered {
				title = strings.TrimSpace(trimmed[separator+2:])
				continue
			}
		}
		if !strings.HasPrefix(trimmed, "URL: ") {
			continue
		}
		if source, ok := evidenceSourceForURL(strings.TrimPrefix(trimmed, "URL: "), title); ok {
			sources = append(sources, source)
		}
	}
	return sources
}

func (t *WebCrawl) Schema() map[string]any {
	return obj(map[string]any{
		"url":              str("Seed http(s) URL to crawl."),
		"query":            str("Optional research focus for the returned corpus."),
		"max_pages":        num("Maximum pages to fetch, 1-30. Default 8."),
		"max_depth":        num("Maximum link depth from the seed URL, 0-3. Default 1."),
		"include":          arrStr("Optional regex patterns. If set, a page path must match one to be crawled."),
		"exclude":          arrStr("Optional regex patterns. Matching page paths are skipped."),
		"same_domain":      boolean("Keep crawl on the seed host. Default true."),
		"allow_subdomains": boolean("Allow subdomains of the seed host when same_domain is true."),
	}, "url")
}

func (t *WebCrawl) client() *http.Client {
	var base *http.Client
	if t.HTTP != nil {
		base = t.HTTP
	} else {
		base = &http.Client{Timeout: webCrawlTimeout}
	}
	return guardPublicHTTPClient(base, t.allowPrivateTestTargets, t.networkPolicy)
}

type crawlJob struct {
	u      *url.URL
	depth  int
	reason string
}

type crawledPage struct {
	URL       string
	Title     string
	Text      string
	Depth     int
	Reason    string
	LinkCount int
}

type crawlConfig struct {
	seed            *url.URL
	query           string
	maxPages        int
	maxDepth        int
	sameDomain      bool
	allowSubdomains bool
	allowPrivate    bool
	networkPolicy   *publicHTTPPolicy
	include         []*regexp.Regexp
	exclude         []*regexp.Regexp
}

func (t *WebCrawl) Execute(ctx context.Context, args map[string]any) Result {
	cfg, err := parseCrawlConfig(ctx, args, t.allowPrivateTestTargets, t.networkPolicy)
	if err != nil {
		return errResult(err)
	}

	queue := []crawlJob{{u: cfg.seed, depth: 0, reason: "seed"}}
	enqueued := map[string]bool{canonicalCrawlURL(cfg.seed): true}
	visited := map[string]bool{}
	client := t.client()
	var pages []crawledPage
	var skipped []string
	var failed []string

	for len(queue) > 0 && len(pages) < cfg.maxPages {
		select {
		case <-ctx.Done():
			return errResult(ctx.Err())
		default:
		}

		job := queue[0]
		queue = queue[1:]
		key := canonicalCrawlURL(job.u)
		if visited[key] {
			continue
		}
		visited[key] = true

		page, links, err := t.fetchPage(ctx, client, job)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", job.u.String(), err))
			continue
		}
		pages = append(pages, page)

		if job.depth >= cfg.maxDepth {
			continue
		}
		for _, link := range links {
			if len(pages)+len(queue) >= cfg.maxPages {
				break
			}
			if reason := cfg.skipReason(ctx, link); reason != "" {
				if len(skipped) < 20 {
					skipped = append(skipped, fmt.Sprintf("%s (%s)", link.String(), reason))
				}
				continue
			}
			nextKey := canonicalCrawlURL(link)
			if visited[nextKey] || enqueued[nextKey] {
				continue
			}
			enqueued[nextKey] = true
			queue = append(queue, crawlJob{
				u:      link,
				depth:  job.depth + 1,
				reason: "discovered from " + job.u.String(),
			})
		}
	}

	if len(pages) == 0 && len(failed) > 0 {
		return errResultf("crawl fetched no pages; first failure: %s", failed[0])
	}
	out := renderCrawlResult(cfg, pages, skipped, failed)
	return Result{
		LLMContent: out,
		Display:    fmt.Sprintf("crawled %d pages from %s", len(pages), cfg.seed.Host),
	}
}

func parseCrawlConfig(ctx context.Context, args map[string]any, allowPrivateHosts bool, policy *publicHTTPPolicy) (*crawlConfig, error) {
	raw := strings.TrimSpace(strArg(args, "url"))
	if raw == "" {
		return nil, fmt.Errorf("url parameter is required")
	}
	seed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if seed.Scheme != "http" && seed.Scheme != "https" {
		return nil, fmt.Errorf("url must start with http:// or https://")
	}
	if err := validateCrawlURLTarget(ctx, seed, allowPrivateHosts, policy); err != nil {
		return nil, err
	}
	seed.Fragment = ""

	maxPages := webCrawlDefaultPages
	if n, ok := intArg(args, "max_pages"); ok {
		maxPages = max(1, min(n, webCrawlMaxPages))
	}
	maxDepth := webCrawlDefaultDepth
	if n, ok := intArg(args, "max_depth"); ok {
		maxDepth = max(0, min(n, webCrawlMaxDepth))
	}
	sameDomain := true
	if v, ok := args["same_domain"].(bool); ok {
		sameDomain = v
	}

	include, err := compileRegexList(sliceArg(args, "include"))
	if err != nil {
		return nil, fmt.Errorf("include: %w", err)
	}
	exclude, err := compileRegexList(sliceArg(args, "exclude"))
	if err != nil {
		return nil, fmt.Errorf("exclude: %w", err)
	}

	return &crawlConfig{
		seed:            seed,
		query:           strings.TrimSpace(strArg(args, "query")),
		maxPages:        maxPages,
		maxDepth:        maxDepth,
		sameDomain:      sameDomain,
		allowSubdomains: boolArg(args, "allow_subdomains"),
		allowPrivate:    allowPrivateHosts,
		networkPolicy:   policy,
		include:         include,
		exclude:         exclude,
	}, nil
}

func (t *WebCrawl) fetchPage(ctx context.Context, client *http.Client, job crawlJob) (crawledPage, []*url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, job.u.String(), nil)
	if err != nil {
		return crawledPage{}, nil, err
	}
	req.Header.Set("Accept", "text/html, text/markdown, text/plain")
	req.Header.Set("User-Agent", "MeadowCode/1.0 (+https://heymeadow.ai)")
	resp, err := client.Do(req)
	if err != nil {
		return crawledPage{}, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return crawledPage{}, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, webCrawlMaxReadBytes))
	if err != nil {
		return crawledPage{}, nil, err
	}
	finalURL := job.u
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL
	}
	raw := string(body)
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isHTML := strings.Contains(contentType, "text/html") || looksLikeHTML(raw)
	title := strings.TrimSpace(finalURL.String())
	text := raw
	var links []*url.URL
	if isHTML {
		title = pageTitle(raw)
		text = htmlToText(raw)
		links = extractCrawlLinks(raw, finalURL)
	}
	if strings.TrimSpace(title) == "" {
		title = finalURL.String()
	}
	return crawledPage{
		URL:       finalURL.String(),
		Title:     title,
		Text:      strings.TrimSpace(text),
		Depth:     job.depth,
		Reason:    job.reason,
		LinkCount: len(links),
	}, links, nil
}

func (c *crawlConfig) skipReason(ctx context.Context, u *url.URL) string {
	if u == nil {
		return "invalid"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "non-web protocol"
	}
	if err := validateCrawlURLTarget(ctx, u, c.allowPrivate, c.networkPolicy); err != nil {
		return err.Error()
	}
	if isSkippedCrawlFile(u.Path) {
		return "non-text file type"
	}
	if c.sameDomain && !sameCrawlHost(c.seed.Hostname(), u.Hostname(), c.allowSubdomains) {
		return "outside domain"
	}
	target := u.EscapedPath()
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	for _, re := range c.exclude {
		if re.MatchString(target) || re.MatchString(u.String()) {
			return "exclude pattern"
		}
	}
	if len(c.include) > 0 {
		for _, re := range c.include {
			if re.MatchString(target) || re.MatchString(u.String()) {
				return ""
			}
		}
		return "include pattern"
	}
	return ""
}

func validateCrawlURLTarget(ctx context.Context, u *url.URL, allowPrivateHosts bool, policy *publicHTTPPolicy) error {
	if allowPrivateHosts {
		return nil
	}
	if policy == nil {
		policy = &publicHTTPPolicy{}
	}
	_, err := policy.validateURL(ctx, u)
	return err
}

func renderCrawlResult(cfg *crawlConfig, pages []crawledPage, skipped, failed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Crawl result for %s\n", cfg.seed.String())
	fmt.Fprintf(&b, "Pages fetched: %d; skipped links sampled: %d; failed pages: %d; max_pages=%d; max_depth=%d\n", len(pages), len(skipped), len(failed), cfg.maxPages, cfg.maxDepth)
	if cfg.query != "" {
		fmt.Fprintf(&b, "Requested focus: %s\n", cfg.query)
	}
	for i, p := range pages {
		excerpt := compactWhitespace(p.Text)
		if len(excerpt) > 1200 {
			excerpt = excerpt[:1200] + " ... [truncated]"
		}
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, p.Title)
		fmt.Fprintf(&b, "   URL: %s\n", p.URL)
		fmt.Fprintf(&b, "   Depth: %d\n", p.Depth)
		fmt.Fprintf(&b, "   Reason: %s\n", p.Reason)
		fmt.Fprintf(&b, "   Links discovered: %d\n", p.LinkCount)
		fmt.Fprintf(&b, "   Excerpt: %s\n", excerpt)
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		b.WriteString("\nFailed pages:\n")
		for _, f := range failed[:min(len(failed), 10)] {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		b.WriteString("\nSkipped links sample:\n")
		for _, s := range skipped[:min(len(skipped), 10)] {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	out := b.String()
	if len(out) > webCrawlMaxOutputBytes {
		out = out[:webCrawlMaxOutputBytes] + "\n\n[web_crawl output truncated]"
	}
	return strings.TrimRight(out, "\n")
}

var (
	crawlHrefRe  = regexp.MustCompile(`(?is)<a\b[^>]*\bhref\s*=\s*["']([^"']+)["']`)
	crawlTitleRe = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title>`)
)

func extractCrawlLinks(raw string, base *url.URL) []*url.URL {
	matches := crawlHrefRe.FindAllStringSubmatch(raw, -1)
	seen := map[string]bool{}
	var out []*url.URL
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		href := strings.TrimSpace(html.UnescapeString(m[1]))
		if href == "" || strings.HasPrefix(href, "#") {
			continue
		}
		u, err := url.Parse(href)
		if err != nil {
			continue
		}
		u = base.ResolveReference(u)
		u.Fragment = ""
		key := canonicalCrawlURL(u)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, u)
	}
	return out
}

func pageTitle(raw string) string {
	if m := crawlTitleRe.FindStringSubmatch(raw); len(m) > 1 {
		return compactWhitespace(stripTags(html.UnescapeString(m[1])))
	}
	return ""
}

func compileRegexList(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, re)
	}
	return out, nil
}

func sliceArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return append([]string{}, v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	default:
		return nil
	}
}

func arrStr(desc string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": desc,
		"items":       map[string]any{"type": "string"},
	}
}

func canonicalCrawlURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.Scheme = strings.ToLower(c.Scheme)
	c.Host = strings.ToLower(c.Host)
	c.Fragment = ""
	switch c.Scheme {
	case "http":
		c.Host = strings.TrimSuffix(c.Host, ":80")
	case "https":
		c.Host = strings.TrimSuffix(c.Host, ":443")
	}
	path := c.EscapedPath()
	if path == "" {
		path = "/"
	}
	for _, suffix := range []string{"/index.html", "/index.htm", "/index.php"} {
		if strings.HasSuffix(strings.ToLower(path), suffix) {
			path = strings.TrimSuffix(path, suffix)
			if path == "" {
				path = "/"
			}
			break
		}
	}
	if path != "/" {
		path = strings.TrimRight(path, "/")
	}
	c.Path = path
	c.RawPath = ""
	return c.String()
}

func sameCrawlHost(seedHost, candidateHost string, allowSubdomains bool) bool {
	seedHost = strings.ToLower(strings.TrimPrefix(seedHost, "www."))
	candidateHost = strings.ToLower(strings.TrimPrefix(candidateHost, "www."))
	if candidateHost == seedHost {
		return true
	}
	return allowSubdomains && strings.HasSuffix(candidateHost, "."+seedHost)
}

func isSkippedCrawlFile(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range []string{
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico",
		".mp4", ".mov", ".webm", ".mp3", ".wav", ".ogg",
		".css", ".js", ".mjs", ".map", ".woff", ".woff2", ".ttf", ".otf",
		".zip", ".tar", ".gz", ".rar", ".7z", ".dmg", ".exe",
	} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func compactWhitespace(s string) string {
	lines := strings.Fields(strings.TrimSpace(s))
	return strings.Join(lines, " ")
}
