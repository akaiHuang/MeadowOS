package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/searchconnection"
)

const meadowSearchTimeout = 95 * time.Second // Service has a 90s retrieval deadline.
const meadowSearchResponseLimit = 2 << 20

type meadowSearchGate struct {
	slot  chan struct{}
	users int
}

var meadowSearchAdmission = struct {
	sync.Mutex
	gates map[string]*meadowSearchGate
}{gates: make(map[string]*meadowSearchGate)}

// Sessions and subagents in one daemon share the service's profile principal.
// A tool-batch ParallelSafe flag alone cannot serialize those independent runs.
func acquireMeadowSearch(ctx context.Context, key string) (func(), error) {
	meadowSearchAdmission.Lock()
	gate := meadowSearchAdmission.gates[key]
	if gate == nil {
		gate = &meadowSearchGate{slot: make(chan struct{}, 1)}
		meadowSearchAdmission.gates[key] = gate
	}
	gate.users++
	meadowSearchAdmission.Unlock()
	drop := func() {
		meadowSearchAdmission.Lock()
		defer meadowSearchAdmission.Unlock()
		gate.users--
		if gate.users == 0 {
			delete(meadowSearchAdmission.gates, key)
		}
	}
	select {
	case gate.slot <- struct{}{}:
		return func() { <-gate.slot; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

type meadowSearchConfig struct{ baseURL, token, actor string }

// Only host configuration selects a provider and principal. Neither is a tool
// argument. With no URL configured, existing deployments retain their providers.
func meadowSearchConfigured() bool {
	return strings.TrimSpace(os.Getenv("MEADOW_SEARCH_BASE_URL")) != "" || searchconnection.ConfiguredEnvironment()
}

func loadMeadowSearchConfig() (meadowSearchConfig, error) {
	if strings.TrimSpace(os.Getenv("MEADOW_SEARCH_BASE_URL")) == "" {
		connection, err := searchconnection.LoadEnvironment()
		if err != nil {
			return meadowSearchConfig{}, err
		}
		if connection != nil {
			return meadowSearchConfig{baseURL: connection.BaseURL, token: connection.Token(), actor: connection.ActorID()}, nil
		}
	}
	u, err := url.Parse(strings.TrimSpace(os.Getenv("MEADOW_SEARCH_BASE_URL")))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return meadowSearchConfig{}, errors.New("invalid Meadow Search base URL")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return meadowSearchConfig{}, errors.New("Meadow Search HTTP requires a loopback IP; remote endpoints require HTTPS")
		}
	}
	token := strings.TrimSpace(os.Getenv("MEADOW_SEARCH_SERVICE_KEY"))
	if tokenFile := strings.TrimSpace(os.Getenv("MEADOW_SEARCH_TOKEN_FILE")); tokenFile != "" {
		contents, err := readMeadowPrivateFile(tokenFile, 8192)
		if err != nil {
			return meadowSearchConfig{}, errors.New("Meadow Search token file is unavailable or not private")
		}
		token = strings.TrimSpace(string(contents))
	}
	if token == "" || len(token) > 8192 || strings.ContainsAny(token, "\r\n\t ") {
		return meadowSearchConfig{}, errors.New("Meadow Search requires a valid service key or private token file")
	}
	actor, err := meadowSearchActor()
	if err != nil {
		return meadowSearchConfig{}, errors.New("Meadow Search profile identity is unavailable")
	}
	return meadowSearchConfig{strings.TrimRight(u.String(), "/"), token, actor}, nil
}

func readMeadowPrivateFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("not a private regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("invalid private file")
	}
	return b, nil
}

var meadowSourceVersion = regexp.MustCompile(`^[a-f0-9]{64}$`)

func meadowSearchActor() (string, error) {
	profile := strings.TrimSpace(os.Getenv("MEADOW_HOME"))
	if profile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		profile = filepath.Join(home, ".meadow")
	}
	dir := filepath.Join(profile, "search")
	path := filepath.Join(dir, "actor-id")
	read := func() (string, error) {
		data, err := readMeadowPrivateFile(path, 128)
		id := strings.TrimSpace(string(data))
		if err != nil || !meadowSourceVersion.MatchString(id) {
			return "", errors.New("invalid profile identity")
		}
		return "meadow-" + id, nil
	}
	if _, err := os.Stat(path); err == nil {
		return read()
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".actor-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(hex.EncodeToString(random[:]) + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	// Publishing a complete file without replacement also handles concurrent
	// processes: the losing creator reads the winner's stable principal.
	if err = os.Link(f.Name(), path); err != nil && !os.IsExist(err) {
		return "", err
	}
	return read()
}

type meadowSearchEvidence struct {
	URL               string          `json:"url"`
	Title             string          `json:"title"`
	Text              string          `json:"text"`
	ContentKind       string          `json:"content_kind"`
	ThumbnailURL      string          `json:"thumbnail_url,omitempty"`
	SourceVersion     string          `json:"source_version,omitempty"`
	FetchedAt         string          `json:"fetched_at,omitempty"`
	RetrievedAt       string          `json:"retrieved_at,omitempty"`
	InputTruncated    bool            `json:"input_truncated"`
	EvidenceTruncated bool            `json:"evidence_truncated"`
	CacheHit          bool            `json:"cache_hit"`
	FetchError        string          `json:"fetch_error,omitempty"`
	Section           string          `json:"section,omitempty"`
	ChunkID           json.RawMessage `json:"chunk_id,omitempty"`
}

type meadowSearchResponse struct {
	Provider string                 `json:"provider"`
	Query    string                 `json:"query,omitempty"`
	Status   string                 `json:"status"`
	Results  []meadowSearchEvidence `json:"results"`
	Errors   []struct {
		URL  string `json:"url,omitempty"`
		Code string `json:"code"`
	} `json:"errors"`
	Meta        map[string]any   `json:"meta"`
	SourceLinks []map[string]any `json:"source_links,omitempty"`
}

func (c meadowSearchConfig) post(ctx context.Context, client *http.Client, endpoint string, payload any) (meadowSearchResponse, error) {
	var result meadowSearchResponse
	body, err := json.Marshal(payload)
	if err != nil {
		return result, errors.New("invalid Meadow Search request")
	}
	ctx, cancel := context.WithTimeout(ctx, meadowSearchTimeout)
	defer cancel()
	release, err := acquireMeadowSearch(ctx, c.baseURL+"\x00"+c.actor)
	if err != nil {
		return result, fmt.Errorf("Meadow Search admission interrupted: %w", err)
	}
	defer release()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("invalid Meadow Search endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Meadow-Actor-ID", c.actor)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.Timeout = meadowSearchTimeout
	if req.URL.Scheme == "https" {
		copyClient = *guardPublicHTTPClient(&copyClient, false, nil)
	} else {
		transport := copyClient.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		if standard, ok := transport.(*http.Transport); ok {
			direct := standard.Clone()
			direct.Proxy = nil
			copyClient.Transport = direct
		}
	}
	// Never forward credentials, including to a same-host redirect. A redirect
	// is a configuration/protocol error and must remain visible to the model.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer copyClient.CloseIdleConnections()
	resp, err := copyClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return result, fmt.Errorf("Meadow Search request interrupted: %w", ctx.Err())
		}
		return result, errors.New("Meadow Search connection failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, meadowSearchResponseLimit+1))
	if err != nil {
		return result, errors.New("Meadow Search response could not be read")
	}
	if len(raw) > meadowSearchResponseLimit {
		return result, errors.New("Meadow Search response exceeds the safe size limit")
	}
	if resp.StatusCode != http.StatusOK {
		// Do not echo server bodies: auth/proxy failures may reflect secrets.
		failure := meadowSearchHTTPError(resp.StatusCode, raw)
		if retry, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && retry >= 0 && retry <= 86400 {
			failure = fmt.Errorf("%w; retry after %d seconds", failure, retry)
		}
		return result, failure
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, errors.New("Meadow Search returned invalid JSON")
	}
	if result.Status != "ok" && result.Status != "partial" {
		return result, errors.New("Meadow Search returned an invalid retrieval status")
	}
	if result.Meta == nil || result.Results == nil {
		return result, errors.New("Meadow Search returned an incomplete retrieval response")
	}
	for _, item := range result.Results {
		parsed, err := url.Parse(item.URL)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return result, errors.New("Meadow Search returned an invalid source URL")
		}
	}
	result.Provider = "Meadow Search"
	result.Meta["evidence_is_untrusted"] = true
	result.Meta["source_fidelity"] = map[string]any{"basis": "extracted_text", "original_page_completeness": "not_verified"}
	if coverage, ok := result.Meta["coverage"].(map[string]any); ok {
		if _, exists := coverage["all_extracted_text_in_response"]; !exists {
			coverage["all_extracted_text_in_response"] = coverage["whole_source_in_response"]
		}
		coverage["whole_source_in_response"] = false
		coverage["coverage_scope"] = "extracted_text"
		coverage["original_page_completeness"] = "not_verified"
		coverage["notice"] = "Offsets and exhaustion describe extracted text, not verified original-page completeness or model understanding."
	}
	return result, nil
}

func meadowSearchHTTPError(status int, raw []byte) error {
	var data struct {
		Error  string `json:"error"`
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &data)
	code := data.Error
	if code == "" && len(data.Errors) > 0 {
		code = data.Errors[0].Code
	}
	// Only known public protocol codes may reach a model/tool result.
	known := map[string]bool{"unauthorized": true, "busy": true, "request_timeout": true, "unsafe_url": true, "source_version_required": true, "source_version_changed": true, "invalid_read_offset": true, "read_budget_too_small": true, "document_exceeds_budget_use_coverage": true, "focused_read_requires_query_without_offset": true, "empty_evidence": true, "search_backend_failed": true, "search_engines_unavailable": true,
		"member_busy": true, "member_rate_limited": true, "retrieval_busy": true, "admission_capacity": true, "retrieval_deadline_exceeded": true, "retrieval_response_too_large": true, "retrieval_failed": true}
	if known[code] {
		return fmt.Errorf("Meadow Search HTTP %d: %s", status, code)
	}
	return fmt.Errorf("Meadow Search HTTP %d", status)
}

func executeMeadowSearch(ctx context.Context, client *http.Client, query string, n int) Result {
	if utf8.RuneCountInString(query) > 1000 {
		return errResultf("Meadow Search query must be at most 1000 characters")
	}
	config, err := loadMeadowSearchConfig()
	if err != nil {
		return errResult(err)
	}
	response, err := config.post(ctx, client, "/v1/search", map[string]any{"query": query, "max_urls": n, "max_chunks": n, "max_tokens": 4000, "language": "zh-TW", "fetch_pages": true})
	if err != nil {
		return errResult(err)
	}
	return response.result()
}

func executeMeadowFetch(ctx context.Context, client *http.Client, target string, args map[string]any, maxChars int) Result {
	mode := strings.TrimSpace(strArg(args, "mode"))
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "full" && mode != "focused" && mode != "coverage" {
		return errResultf("mode must be auto, full, focused, or coverage")
	}
	offset, _ := intArg(args, "offset_tokens")
	version := strArg(args, "source_version")
	if offset < 0 || offset > 500000 {
		return errResultf("offset_tokens must be between 0 and 500000")
	}
	if version != "" && !meadowSourceVersion.MatchString(version) {
		return errResultf("source_version must be a 64-character lowercase hex digest")
	}
	if offset > 0 && version == "" {
		return errResultf("source_version is required to continue reading")
	}
	prompt := strings.TrimSpace(strArg(args, "prompt"))
	if utf8.RuneCountInString(prompt) > 1000 || utf8.RuneCountInString(target) > 4096 {
		return errResultf("Meadow Search prompt or URL exceeds the service limit")
	}
	if mode == "focused" && (prompt == "" || offset != 0) {
		return errResultf("focused reading requires a prompt and no offset")
	}
	maxTokens := min(8000, maxChars/4)
	if value, ok := intArg(args, "max_tokens"); ok {
		if value < 128 || value > 8000 {
			return errResultf("max_tokens must be between 128 and 8000")
		}
		maxTokens = value
	}
	payload := map[string]any{"url": target, "max_chunks": 8, "max_tokens": maxTokens, "mode": mode, "offset_tokens": offset}
	if prompt != "" {
		payload["query"] = prompt
	}
	if version != "" {
		payload["source_version"] = version
	}
	config, err := loadMeadowSearchConfig()
	if err != nil {
		return errResult(err)
	}
	response, err := config.post(ctx, client, "/v1/fetch", payload)
	if err != nil {
		return errResult(err)
	}
	remaining, clipped := maxChars, false
	for i := range response.Results {
		text := []rune(response.Results[i].Text)
		if len(text) > remaining {
			text = text[:remaining]
			clipped = true
			response.Results[i].EvidenceTruncated = true
		}
		response.Results[i].Text = string(text)
		remaining -= len(text)
	}
	if clipped {
		response.Meta["adapter_truncated"] = true
		response.Meta["adapter_notice"] = "max_chars clipped this token window. Retry the same offset_tokens with a smaller max_tokens before advancing; no safe next offset is known."
		if coverage, ok := response.Meta["coverage"].(map[string]any); ok {
			coverage["all_extracted_text_in_response"] = false
			coverage["available_text_exhausted"] = false
			coverage["next_offset_tokens"] = nil
			coverage["retry_offset_tokens"] = offset
		}
	}
	return response.result()
}

func (r meadowSearchResponse) result() Result {
	data, err := json.Marshal(r)
	if err != nil {
		return errResultf("Meadow Search response could not be encoded")
	}
	passages := []EvidencePassage{}
	coverage, _ := json.Marshal(r.Meta["coverage"])
	for _, row := range r.Results {
		// Discovery snippets and failed fetches remain in the original response,
		// but cannot become an opened-page index receipt.
		if row.ContentKind != "body" || row.FetchError != "" || strings.TrimSpace(row.Text) == "" {
			continue
		}
		source := EvidenceSource{URL: row.URL, Title: row.Title, ContentKind: row.ContentKind,
			ThumbnailURL: row.ThumbnailURL, FetchedAt: row.FetchedAt, SourceVersion: row.SourceVersion,
			InputTruncated: row.InputTruncated, EvidenceTruncated: row.EvidenceTruncated}
		passage := exactEvidencePassage(source, row.Text, "returned_extracted_fragment")
		passage.RetrievedAt, passage.CacheHit = row.RetrievedAt, row.CacheHit
		passage.Section = row.Section
		passage.Locator = append(json.RawMessage(nil), row.ChunkID...)
		if string(coverage) != "null" {
			passage.Coverage = append(json.RawMessage(nil), coverage...)
		}
		passages = append(passages, passage)
	}
	return Result{LLMContent: string(data), Display: fmt.Sprintf("%d excerpts via Meadow Search (%s)", len(r.Results), r.Status), SourcePassages: passages}
}

func meadowSearchEvidenceSources(result Result) []EvidenceSource {
	if result.Err != nil {
		return nil
	}
	var response meadowSearchResponse
	if json.Unmarshal([]byte(result.LLMContent), &response) != nil || response.Provider != "Meadow Search" {
		return nil
	}
	sources := make([]EvidenceSource, 0, len(response.Results))
	seen := map[string]bool{}
	for _, row := range response.Results {
		if row.Text == "" || seen[row.URL] {
			continue
		}
		if source, ok := evidenceSourceForURL(row.URL, row.Title); ok {
			source.Snippet = row.Text
			source.ContentKind = row.ContentKind
			source.ThumbnailURL = row.ThumbnailURL
			source.FetchedAt = row.FetchedAt
			source.SourceVersion = row.SourceVersion
			source.FetchError = row.FetchError
			source.InputTruncated = row.InputTruncated
			source.EvidenceTruncated = row.EvidenceTruncated
			if source, valid := NormalizeEvidenceSource(source); valid {
				sources = append(sources, source)
				seen[row.URL] = true
			}
		}
	}
	return sources
}
