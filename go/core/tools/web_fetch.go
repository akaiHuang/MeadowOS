package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	webFetchTimeout         = 10 * time.Second
	webFetchDefaultMaxChars = 16_000
	webFetchHardMaxChars    = 40_000
	webFetchReadMaxBytes    = 400_000
)

// WebFetch returns public source text directly to the main model without execution
// approval. Public-network validation still applies to every request. Meadow Search
// retrieval and legacy direct fetching never invoke a separate language model.
type WebFetch struct {
	HTTP            *http.Client
	DefaultMaxChars int
	// allowPrivateTestTargets is deliberately package-private. Unit fixtures
	// may opt a loopback httptest.Server in; production registries cannot expose
	// a model-controlled switch that disables the SSRF boundary.
	allowPrivateTestTargets bool
	networkPolicy           *publicHTTPPolicy
}

func (t *WebFetch) Name() string                     { return "web_fetch" }
func (t *WebFetch) Description() string              { return "Fetch a URL and return its text content." }
func (t *WebFetch) Kind() Kind                       { return KindReadOnly }
func (t *WebFetch) ParallelSafe(map[string]any) bool { return !meadowSearchConfigured() }
func (t *WebFetch) SubtaskSafe() bool                { return true }

// ReadIdentity prevents a research loop from downloading the same canonical
// URL with the same extraction arguments more than once in one agent run. The
// original full result remains in conversation context; repeats receive a
// lightweight unchanged-resource observation instead.
func (t *WebFetch) ReadIdentity(args map[string]any) (ReadIdentity, error) {
	raw := strings.TrimSpace(strArg(args, "url"))
	canonical, err := canonicalWebReadURL(raw)
	if err != nil {
		return ReadIdentity{}, err
	}
	maxChars := t.defaultMaxChars()
	if value, ok := intArg(args, "max_chars"); ok {
		maxChars = value
	}
	prompt := strings.Join(strings.Fields(strArg(args, "prompt")), " ")
	mode := strings.TrimSpace(strArg(args, "mode"))
	if mode == "" {
		mode = "auto"
	}
	offset, _ := intArg(args, "offset_tokens")
	maxTokens, _ := intArg(args, "max_tokens")
	key := fmt.Sprintf("web_fetch\x00%s\x00%d\x00%s\x00%s\x00%d\x00%s\x00%d", canonical, maxChars, prompt, mode, offset, strArg(args, "source_version"), maxTokens)
	return ReadIdentity{Key: key, Path: canonical, Version: "one-run"}, nil
}

func canonicalWebReadURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("invalid http(s) URL %q", raw)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	if parsed.Path != "/" {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	parsed.RawQuery = parsed.Query().Encode()
	return parsed.String(), nil
}

func parseWebFetchURL(raw string) (*url.URL, error) { return url.Parse(raw) }

func (t *WebFetch) EvidenceSources(args map[string]any, result Result) []EvidenceSource {
	if passages := successfulSourcePassages(result); len(passages) > 0 {
		sources := make([]EvidenceSource, 0, len(passages))
		for _, passage := range passages {
			source := passage.Source
			// Legacy fetch prose keeps the requested URL for compatibility; this
			// typed receipt records the actual final response URL after redirects.
			source.Snippet = passage.Text
			if source, ok := NormalizeEvidenceSource(source); ok {
				sources = append(sources, source)
			}
		}
		return sources
	}
	if sources := meadowSearchEvidenceSources(result); sources != nil {
		return sources
	}
	rawURL := strings.TrimSpace(strArg(args, "url"))
	for _, line := range strings.Split(result.LLMContent, "\n") {
		if strings.HasPrefix(line, "Content from ") {
			rawURL = strings.TrimSuffix(strings.TrimPrefix(line, "Content from "), ":")
			break
		}
	}
	if source, ok := evidenceSourceForURL(rawURL, ""); ok {
		return []EvidenceSource{source}
	}
	return nil
}

func (t *WebFetch) Schema() map[string]any {
	defaultMaxChars := t.defaultMaxChars()
	properties := map[string]any{
		"url":       str("Full http(s) URL to fetch."),
		"prompt":    str("What to look for in the content."),
		"max_chars": num(fmt.Sprintf("Maximum returned text characters from 2000 to 40000. Default %d; increase only when the first excerpt is insufficient.", defaultMaxChars)),
	}
	if meadowSearchConfigured() {
		properties["mode"] = str("Meadow Search reading mode: auto (default), full, focused, or coverage. Coverage supports sequential reading of extracted text.")
		properties["offset_tokens"] = num("Meadow Search continuation offset from the previous response; requires its source_version.")
		properties["source_version"] = str("Meadow Search source_version from a previous response; required for continuation.")
		properties["max_tokens"] = num("Meadow Search token window, 128-8000. Reduce it if max_chars clipped a read window.")
	}
	return obj(properties, "url")
}

func (t *WebFetch) defaultMaxChars() int {
	if t != nil && t.DefaultMaxChars >= 2_000 && t.DefaultMaxChars <= webFetchHardMaxChars {
		return t.DefaultMaxChars
	}
	return webFetchDefaultMaxChars
}

func (t *WebFetch) client() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return &http.Client{Timeout: webFetchTimeout}
}

func (t *WebFetch) guardedClient() *http.Client {
	return guardPublicHTTPClient(t.client(), t.allowPrivateTestTargets, t.networkPolicy)
}

var githubBlobRe = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/blob/(.+)$`)

func (t *WebFetch) Execute(ctx context.Context, args map[string]any) Result {
	url := strings.TrimSpace(strArg(args, "url"))
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return errResultf("url must start with http:// or https://")
	}
	maxChars := t.defaultMaxChars()
	if value, ok := intArg(args, "max_chars"); ok {
		if value < 2_000 || value > webFetchHardMaxChars {
			return errResultf("max_chars must be between 2000 and %d", webFetchHardMaxChars)
		}
		maxChars = value
	}
	// GitHub blob pages carry the file inside a JS app; raw serves the text.
	if m := githubBlobRe.FindStringSubmatch(url); m != nil {
		url = fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", m[1], m[2], m[3])
	}
	parsedURL, err := parseWebFetchURL(url)
	if err != nil {
		return errResult(err)
	}
	if (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return errResultf("url must be a full http(s) URL")
	}
	if meadowSearchConfigured() {
		if parsedURL.User != nil {
			return errResultf("URL credentials are not allowed")
		}
		if !t.allowPrivateTestTargets {
			if _, err := t.networkPolicy.validateURL(ctx, parsedURL); err != nil {
				return errResultf("fetch target is unavailable under the public-network policy")
			}
		}
		return executeMeadowFetch(ctx, t.HTTP, url, args, maxChars)
	}
	if strArg(args, "mode") != "" || strArg(args, "source_version") != "" || args["offset_tokens"] != nil || args["max_tokens"] != nil {
		return errResultf("advanced reading parameters require configured Meadow Search")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errResult(err)
	}
	req.Header.Set("Accept", "text/markdown, text/html, text/plain")
	req.Header.Set("User-Agent", "MeadowCode/1.0")

	resp, err := t.guardedClient().Do(req)
	if err != nil {
		return errResultf("fetch %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errResultf("fetch %s: HTTP %d", url, resp.StatusCode)
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/pdf") {
		return errResultf("fetch %s: PDF binary is not text evidence; use pdf_extract with this URL or open an authoritative HTML source", url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchReadMaxBytes))
	if err != nil {
		return errResult(err)
	}
	if strings.HasPrefix(string(body), "%PDF-") {
		return errResultf("fetch %s: PDF binary is not text evidence; use pdf_extract with this URL or open an authoritative HTML source", url)
	}
	if contentType != "" &&
		!strings.Contains(contentType, "text/") &&
		!strings.Contains(contentType, "json") &&
		!strings.Contains(contentType, "xml") &&
		!strings.Contains(contentType, "xhtml") &&
		!strings.Contains(contentType, "javascript") {
		return errResultf("fetch %s: unsupported non-text content type %s", url, contentType)
	}

	text := string(body)
	if strings.Contains(contentType, "text/html") || looksLikeHTML(text) {
		text = htmlToText(text)
	}
	runes := []rune(text)
	truncated := len(runes) > maxChars
	if truncated {
		text = string(runes[:maxChars])
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Content from %s:\n\n%s", url, strings.TrimSpace(text))
	if truncated {
		fmt.Fprintf(&b, "\n\n[content excerpt truncated at %d characters]", maxChars)
	}
	if p := strArg(args, "prompt"); p != "" {
		fmt.Fprintf(&b, "\n\n(Requested focus: %s)", p)
	}
	observedURL := url
	if resp.Request != nil && resp.Request.URL != nil {
		observedURL = resp.Request.URL.String()
	}
	passage := exactEvidencePassage(EvidenceSource{URL: observedURL, ContentKind: "body",
		FetchedAt: time.Now().UTC().Format(time.RFC3339), InputTruncated: len(body) >= webFetchReadMaxBytes,
		EvidenceTruncated: truncated}, strings.TrimSpace(text), "returned_extracted_fragment")
	return Result{LLMContent: b.String(), Display: "fetched " + url, SourcePassages: []EvidencePassage{passage}}
}

func looksLikeHTML(s string) bool {
	head := strings.ToLower(s[:min(512, len(s))])
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

var (
	htmlDropRe2  = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)\b[^>]*>.*?</(script|style|noscript|svg|head)>`)
	htmlBlockRe  = regexp.MustCompile(`(?i)</?(p|div|br|li|tr|h[1-6]|section|article|ul|ol|table|blockquote|pre)[^>]*>`)
	htmlTagRe    = regexp.MustCompile(`<[^>]+>`)
	htmlBlankRe  = regexp.MustCompile(`\n{3,}`)
	htmlSpacesRe = regexp.MustCompile(`[ \t]{2,}`)
)

// htmlToText is a deliberately small HTML→text pass: drop non-content
// elements, turn block boundaries into newlines, strip the rest of the
// tags, decode the common entities.
func htmlToText(s string) string {
	s = htmlDropRe2.ReplaceAllString(s, " ")
	s = htmlBlockRe.ReplaceAllString(s, "\n")
	s = htmlTagRe.ReplaceAllString(s, " ")
	for entity, repl := range map[string]string{
		"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`,
		"&#39;": "'", "&apos;": "'", "&nbsp;": " ", "&#x27;": "'",
	} {
		s = strings.ReplaceAll(s, entity, repl)
	}
	s = htmlSpacesRe.ReplaceAllString(s, " ")
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		lines = append(lines, strings.TrimSpace(line))
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(htmlBlankRe.ReplaceAllString(s, "\n\n"))
}
