// Package memoryservices connects Native Meadow to its opt-in local adapter for
// the official MemPalace and PowerContext services. It never calls a model.
package memoryservices

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxResponseBytes = 32 * 1024
const MaxMemoryBytes = 4096

type Config struct {
	Enabled        bool   `json:"enabled"`
	URL            string `json:"url"`
	TokenFile      string `json:"token_file"`
	TimeoutMS      int    `json:"timeout_ms"`
	CapturePrompts bool   `json:"capture_prompts"`
}

type Client struct {
	base    string
	token   string
	capture bool
	http    *http.Client
}

// Load is silent only for an absent/disabled opt-in. Configuration errors are
// returned without echoing secret values and cannot make an outbound request.
func Load(stateRoot string) (*Client, error) {
	path := filepath.Join(stateRoot, "memory-services.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("memory services config must be a private regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 8192 {
		return nil, errors.New("memory services config unreadable or oversized")
	}
	var config Config
	if json.Unmarshal(raw, &config) != nil {
		return nil, errors.New("memory services config is invalid JSON")
	}
	if !config.Enabled {
		return nil, nil
	}
	u, err := url.Parse(config.URL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("memory services URL must be an explicit IPv4 loopback HTTP origin")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("memory services URL port is invalid")
	}
	if !filepath.IsAbs(config.TokenFile) {
		return nil, errors.New("memory services token path must be absolute")
	}
	secretInfo, err := os.Lstat(config.TokenFile)
	if err != nil || !secretInfo.Mode().IsRegular() || secretInfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("memory services token must be a private regular file")
	}
	secret, err := os.ReadFile(config.TokenFile)
	token := strings.TrimSpace(string(secret))
	if err != nil || len(token) < 32 || len(token) > 256 || strings.ContainsAny(token, "\r\n\x00") {
		return nil, errors.New("memory services token is invalid")
	}
	timeout := config.TimeoutMS
	if timeout == 0 {
		timeout = 3000
	}
	if timeout < 100 || timeout > 10000 {
		return nil, errors.New("memory services timeout outside100..10000ms")
	}
	return &Client{base: strings.TrimRight(config.URL, "/"), token: token, capture: config.CapturePrompts, http: &http.Client{Timeout: time.Duration(timeout) * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type BackendReceipt struct {
	Service            string   `json:"service"`
	State              string   `json:"state"`
	ErrorCode          string   `json:"error_code,omitempty"`
	Scope              string   `json:"scope,omitempty"`
	IDs                []string `json:"ids,omitempty"`
	CaptureState       string   `json:"capture_state,omitempty"`
	CaptureIDs         []string `json:"capture_ids,omitempty"`
	QueryOriginalBytes int      `json:"query_original_bytes,omitempty"`
	QuerySentBytes     int      `json:"query_sent_bytes,omitempty"`
	QueryShortened     bool     `json:"query_shortened,omitempty"`
}

type Response struct {
	Version    string           `json:"version"`
	Context    string           `json:"context,omitempty"`
	Backends   []BackendReceipt `json:"backends"`
	Provenance string           `json:"provenance,omitempty"`
}

func (c *Client) Call(ctx context.Context, action, workspace, query, backend, taskID, content, evidence string) (Response, error) {
	if c == nil {
		return Response{}, errors.New("memory services disabled")
	}
	if action != "recall" && action != "remember" && action != "status" {
		return Response{}, errors.New("invalid memory action")
	}
	if backend != "both" && backend != "mempalace" && backend != "powercontext" {
		return Response{}, errors.New("invalid memory backend")
	}
	if !filepath.IsAbs(workspace) || strings.TrimSpace(workspace) == "" {
		return Response{}, errors.New("memory workspace must be absolute")
	}
	if !utf8.ValidString(query) || !utf8.ValidString(content) || !utf8.ValidString(evidence) || !utf8.ValidString(taskID) || len(taskID) > 256 || len(query) > 16*1024 || len(content) > MaxMemoryBytes || len(evidence) > 512 {
		return Response{}, errors.New("memory input exceeds UTF-8 limits")
	}
	if action == "recall" && strings.TrimSpace(query) == "" {
		return Response{}, errors.New("memory query is empty")
	}
	if action == "remember" && (strings.TrimSpace(content) == "" || strings.TrimSpace(evidence) == "") {
		return Response{}, errors.New("memory requires content and evidence source")
	}
	payload, err := json.Marshal(map[string]any{"action": action, "workspace": workspace, "query": query, "backend": backend, "task_id": taskID, "capture_prompt": c.capture && taskID != "" && action == "recall", "content": content, "evidence": evidence})
	if err != nil {
		return Response{}, errors.New("memory request encoding failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/memory", bytes.NewReader(payload))
	if err != nil {
		return Response{}, errors.New("memory request creation failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	afterSendError := func(err error) (Response, error) {
		if action == "remember" {
			return Response{}, fmt.Errorf("memory write delivery unknown; do not automatically retry: %w", err)
		}
		return Response{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return afterSendError(ctx.Err())
		}
		return afterSendError(errors.New("memory adapter unavailable or timed out"))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return afterSendError(fmt.Errorf("memory adapter HTTP%d", resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil || len(raw) > MaxResponseBytes || !utf8.Valid(raw) {
		return afterSendError(errors.New("memory response unreadable or oversized"))
	}
	var result Response
	if json.Unmarshal(raw, &result) != nil || result.Version != "meadow.memory-services.v1" || len(result.Context) > 8192 || !utf8.ValidString(result.Context) || len(result.Backends) == 0 || len(result.Backends) > 2 {
		return afterSendError(errors.New("memory response contract invalid"))
	}
	seen := map[string]bool{}
	if backend == "both" && len(result.Backends) != 2 {
		return afterSendError(errors.New("memory response missing backend receipt"))
	}
	for _, receipt := range result.Backends {
		if (receipt.Service != "mempalace" && receipt.Service != "powercontext") || seen[receipt.Service] || (backend != "both" && receipt.Service != backend) || len(receipt.State) == 0 || len(receipt.State) > 64 || len(receipt.IDs) > 10 || len(receipt.CaptureIDs) > 10 || len(receipt.Scope) > 256 || len(receipt.ErrorCode) > 128 || len(receipt.CaptureState) > 64 {
			return afterSendError(errors.New("memory response backend contract invalid"))
		}
		for _, id := range append(append([]string{}, receipt.IDs...), receipt.CaptureIDs...) {
			if len(id) == 0 || len(id) > 256 || !utf8.ValidString(id) {
				return afterSendError(errors.New("memory response source ID invalid"))
			}
		}
		seen[receipt.Service] = true
	}
	return result, nil
}
