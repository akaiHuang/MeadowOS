// Package mempalace is the Go client for the MemPalace long-term memory shim
// (a local Python/MLX service on :8891 backed by ~/.mempalace). It is the
// device-side counterpart to RAG: RAG hands the model proven STRUCTURE up front,
// MemPalace hands it LESSONS accumulated across runs — a repeated typo, the
// right CDN URL, a task's max_tok ceiling — that no static template captures.
//
// The embedding + vector search stay in the Python shim (BGE-M3 4-bit MLX, which
// Go cannot run); this client only speaks its small HTTP API. Everything is
// fail-open: a disabled, down, or slow shim yields zero values / no-ops and
// never blocks or breaks the generation hot path.
package mempalace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client is the surface the engine depends on (mockable). Reads fail-open to the
// zero value ("" / 0 / nil); writes are best-effort and never error.
type Client interface {
	GetPref(ctx context.Context, key string) string
	WritePref(ctx context.Context, key, value string)
	GetMaxTokOverride(ctx context.Context, taskType string) int
	GetLessons(ctx context.Context, taskType, query string) []string
	WriteLesson(ctx context.Context, taskType, content string)
}

const (
	defaultURL       = "http://127.0.0.1:8891"
	defaultTimeoutMS = 800
)

// Null is the no-op client used when MemPalace is disabled or unreachable.
type Null struct{}

func (Null) GetPref(context.Context, string) string        { return "" }
func (Null) WritePref(context.Context, string, string)     {}
func (Null) GetMaxTokOverride(context.Context, string) int { return 0 }
func (Null) GetLessons(context.Context, string, string) []string { return nil }
func (Null) WriteLesson(context.Context, string, string)   {}

// httpClient talks to the shim over HTTP with a short timeout; every error is
// swallowed so the caller sees only zero values.
type httpClient struct {
	baseURL string
	http    *http.Client
}

// New builds an HTTP-backed client for baseURL with the given per-call timeout.
func New(baseURL string, timeout time.Duration) Client {
	return &httpClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *httpClient) getJSON(ctx context.Context, path string, out any) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(resp.Body).Decode(out) == nil
}

func (c *httpClient) postJSON(ctx context.Context, path string, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := c.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

func (c *httpClient) GetPref(ctx context.Context, key string) string {
	if key == "" {
		return ""
	}
	var data struct {
		Value *string `json:"value"`
	}
	if !c.getJSON(ctx, "/prefs?key="+url.QueryEscape(key), &data) || data.Value == nil {
		return ""
	}
	return *data.Value
}

func (c *httpClient) WritePref(ctx context.Context, key, value string) {
	if key == "" {
		return
	}
	c.postJSON(ctx, "/pref", map[string]string{"key": key, "value": value})
}

func (c *httpClient) GetMaxTokOverride(ctx context.Context, taskType string) int {
	if taskType == "" {
		return 0
	}
	var data struct {
		Value *float64 `json:"value"`
	}
	if !c.getJSON(ctx, "/max_tok?task_type="+url.QueryEscape(taskType), &data) || data.Value == nil {
		return 0
	}
	if v := *data.Value; v > 0 {
		return int(v)
	}
	return 0
}

// GetLessons recalls lessons for a task. A non-empty query is sent as the shim's
// `q`, which triggers cross-task_type SEMANTIC recall (bge-m3 vector + lexical
// hybrid); with no query the shim does a plain exact task_type lookup. The shim
// returns rich entries ({"content":...}); we surface the content strings.
func (c *httpClient) GetLessons(ctx context.Context, taskType, query string) []string {
	if taskType == "" && query == "" {
		return nil
	}
	path := "/lessons?task_type=" + url.QueryEscape(taskType)
	if query != "" {
		path += "&q=" + url.QueryEscape(query)
	}
	var data struct {
		Lessons []json.RawMessage `json:"lessons"`
	}
	if !c.getJSON(ctx, path, &data) {
		return nil
	}
	out := make([]string, 0, len(data.Lessons))
	for _, raw := range data.Lessons {
		if s := lessonContent(raw); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// lessonContent pulls the text out of one shim lesson entry, accepting both the
// rich {"content":...} object and a bare JSON string (forward/backward compat).
func lessonContent(raw json.RawMessage) string {
	var obj struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Content != "" {
		return obj.Content
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func (c *httpClient) WriteLesson(ctx context.Context, taskType, content string) {
	if taskType == "" || content == "" {
		return
	}
	c.postJSON(ctx, "/lesson", map[string]string{"task_type": taskType, "content": content})
}

// Make returns an HTTP client when MemPalace is enabled (the default), else a
// Null stub. MEADOW_MEMPALACE=0 disables it; MEADOW_MEMPALACE_URL and
// MEADOW_MEMPALACE_TIMEOUT_MS override the endpoint and per-call timeout.
func Make() Client {
	if !enabled() {
		return Null{}
	}
	base := defaultURL
	if v := os.Getenv("MEADOW_MEMPALACE_URL"); v != "" {
		base = v
	}
	timeout := time.Duration(defaultTimeoutMS) * time.Millisecond
	if v := os.Getenv("MEADOW_MEMPALACE_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Millisecond
		}
	}
	return New(base, timeout)
}

// enabled reports whether MemPalace is on. Default OFF (opt-in): on a weak
// local model the heavy memory block hurts more than it helps, and a release
// build must not auto-spawn a Python sidecar unprompted. Only an explicit
// 1/true/yes/on turns it on — the infra stays intact for a stronger model.
func enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_MEMPALACE"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// FormatForSystemPrompt renders lessons + prefs into a system-prompt chunk, or
// "" when there is nothing to add (caller skips injection). Pref keys are sorted
// for a stable prefix so vllm's prefix cache is not invalidated run to run.
func FormatForSystemPrompt(lessons []string, prefs map[string]string) string {
	var b strings.Builder
	if len(lessons) > 0 {
		b.WriteString("## MemPalace 記憶 (lessons)\n")
		for _, l := range lessons {
			if s := strings.TrimSpace(l); s != "" {
				b.WriteString("- ")
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
	}
	keys := make([]string, 0, len(prefs))
	for k, v := range prefs {
		if strings.TrimSpace(v) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		b.WriteString("## Pref (CDN / framework)\n")
		for _, k := range keys {
			b.WriteString("- ")
			b.WriteString(k)
			b.WriteString(" = ")
			b.WriteString(prefs[k])
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
