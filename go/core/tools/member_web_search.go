package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	memberWebSearchTimeout       = 60 * time.Second
	memberWebSearchResponseLimit = 1 << 20
	memberWebSearchProvider      = "Private SearXNG"
)

type memberWebSearchConfig struct{ endpoint, token string }

// The member host selects this search-only route. Tool arguments cannot choose
// its endpoint or credentials. A partial/invalid selection must not fall back.
func memberWebSearchConfigured() bool {
	return os.Getenv("MEADOW_WEB_SEARCH_PROXY_URL") != "" || os.Getenv("MEADOW_WEB_SEARCH_PROXY_TOKEN_FILE") != ""
}

func loadMemberWebSearchConfig() (memberWebSearchConfig, error) {
	rawURL := strings.TrimSpace(os.Getenv("MEADOW_WEB_SEARCH_PROXY_URL"))
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil ||
		u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Path == "" || u.RawPath != "" || strings.ContainsAny(rawURL, "\r\n\t#") {
		return memberWebSearchConfig{}, errors.New("private search requires a valid loopback proxy URL")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return memberWebSearchConfig{}, errors.New("private search requires a loopback IP and explicit port")
	}
	tokenFile := strings.TrimSpace(os.Getenv("MEADOW_WEB_SEARCH_PROXY_TOKEN_FILE"))
	if !filepath.IsAbs(tokenFile) {
		return memberWebSearchConfig{}, errors.New("private search requires an absolute private token file")
	}
	data, err := readMeadowPrivateFile(tokenFile, 8192)
	if err != nil {
		return memberWebSearchConfig{}, errors.New("private search proxy token is unavailable or not private")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || len(token) > 8192 {
		return memberWebSearchConfig{}, errors.New("private search proxy token is invalid")
	}
	for _, c := range token {
		if c < 0x21 || c > 0x7e {
			return memberWebSearchConfig{}, errors.New("private search proxy token is invalid")
		}
	}
	return memberWebSearchConfig{endpoint: rawURL, token: token}, nil
}

type memberWebSearchHit struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Content     string `json:"content"`
	Engine      string `json:"engine"`
	ContentKind string `json:"content_kind,omitempty"`
}

type memberWebSearchResponse struct {
	Query               string               `json:"query"`
	Results             []memberWebSearchHit `json:"results"`
	UnresponsiveEngines []json.RawMessage    `json:"unresponsive_engines"`
	Provider            string               `json:"provider,omitempty"`
	Status              string               `json:"status,omitempty"`
	Meta                map[string]any       `json:"meta,omitempty"`
}

func executeMemberWebSearch(ctx context.Context, client *http.Client, rawQuery string, n int) Result {
	query := strings.TrimSpace(rawQuery)
	if !utf8.ValidString(rawQuery) || len(rawQuery) > 2000 || query == "" || utf8.RuneCountInString(query) > 500 {
		return errResultf("private search query requires 1-500 characters and at most 2000 UTF-8 bytes before trimming")
	}
	config, err := loadMemberWebSearchConfig()
	if err != nil {
		return errResult(err)
	}
	response, err := config.search(ctx, client, query, n)
	if err != nil {
		return errResult(err)
	}
	data, err := json.Marshal(response)
	if err != nil {
		return errResultf("private search response could not be encoded")
	}
	// Search snippets are discovery evidence. They deliberately produce no
	// SourcePassages or receipt claiming that the destination page was fetched.
	return Result{LLMContent: string(data), Display: fmt.Sprintf("%d snippets via Private SearXNG (%s)", len(response.Results), response.Status)}
}

func (c memberWebSearchConfig) search(ctx context.Context, client *http.Client, query string, n int) (memberWebSearchResponse, error) {
	var response memberWebSearchResponse
	ctx, cancel := context.WithTimeout(ctx, memberWebSearchTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{"query": query, "num_results": n})
	if err != nil {
		return response, errors.New("invalid private search request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return response, errors.New("invalid private search proxy URL")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.Timeout = memberWebSearchTimeout
	transport := copyClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if standard, ok := transport.(*http.Transport); ok {
		direct := standard.Clone()
		direct.Proxy = nil
		copyClient.Transport = direct
	}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer copyClient.CloseIdleConnections()
	resp, err := copyClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return response, fmt.Errorf("private search interrupted: %w", ctx.Err())
		}
		return response, errors.New("private search proxy connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Never include server bodies or arbitrary headers in errors: a failure
		// may contain reflected authorization or upstream diagnostics.
		failure := fmt.Errorf("private search HTTP %d", resp.StatusCode)
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 0 && seconds <= 86400 {
			failure = fmt.Errorf("%w; retry after %d seconds", failure, seconds)
		}
		return response, failure
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, memberWebSearchResponseLimit+1))
	if err != nil {
		if ctx.Err() != nil {
			return response, fmt.Errorf("private search interrupted: %w", ctx.Err())
		}
		return response, errors.New("private search response could not be read")
	}
	if len(raw) > memberWebSearchResponseLimit {
		return response, errors.New("private search response exceeds the safe size limit")
	}
	if bytes.Contains(raw, []byte(c.token)) || !utf8.Valid(raw) || json.Unmarshal(raw, &response) != nil {
		return response, errors.New("private search returned an invalid response")
	}
	// Decode escapes before checking fields that could enter tool output, too.
	// A reflected bearer key must not evade redaction with JSON unicode escapes.
	decoded, err := json.Marshal(response)
	if err != nil || bytes.Contains(decoded, []byte(c.token)) {
		return response, errors.New("private search returned an invalid response")
	}
	if response.Query != query || response.Results == nil || response.UnresponsiveEngines == nil || len(response.Results) > searchMaxResults {
		return response, errors.New("private search returned an incomplete response")
	}
	for i := range response.Results {
		row := &response.Results[i]
		if !memberWebSearchSourceURL(row.URL) {
			return response, errors.New("private search returned an unsafe source URL")
		}
		if strings.TrimSpace(row.Engine) == "" {
			return response, errors.New("private search returned incomplete engine metadata")
		}
		row.ContentKind = "snippet"
	}
	if len(response.Results) > n {
		response.Results = response.Results[:n]
	}
	response.Provider, response.Status = memberWebSearchProvider, "ok"
	if len(response.UnresponsiveEngines) > 0 {
		response.Status = "partial"
	}
	response.Meta = map[string]any{
		"evidence_is_untrusted": true,
		"source_fidelity":       map[string]any{"basis": "search_snippet", "original_page_completeness": "not_fetched"},
	}
	return response, nil
}

func memberWebSearchSourceURL(raw string) bool {
	if len(raw) > 4096 || strings.ContainsAny(raw, "\x00\r\n\t") || raw != strings.TrimSpace(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	// Fragments are useful source citations but do not participate in HTTP
	// addressing. Apply the existing syntactic public-target guard without one.
	u.Fragment, u.RawFragment = "", ""
	return meadowPreviewPublicURL(u.String())
}

func memberWebSearchEvidenceSources(result Result) []EvidenceSource {
	if result.Err != nil {
		return nil
	}
	var response memberWebSearchResponse
	if json.Unmarshal([]byte(result.LLMContent), &response) != nil || response.Provider != memberWebSearchProvider {
		return nil
	}
	sources := make([]EvidenceSource, 0, len(response.Results))
	seen := map[string]bool{}
	for _, row := range response.Results {
		if seen[row.URL] || !memberWebSearchSourceURL(row.URL) {
			continue
		}
		source := EvidenceSource{URL: row.URL, Title: row.Title, Snippet: row.Content, ContentKind: "snippet"}
		if source, valid := NormalizeEvidenceSource(source); valid {
			sources = append(sources, source)
			seen[row.URL] = true
		}
	}
	return sources
}
