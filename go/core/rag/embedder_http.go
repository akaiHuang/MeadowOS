package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPEmbedder talks only to a loopback BGE-M3 MLX service. The short timeout
// keeps semantic retrieval optional and fail-open; DocumentStore automatically
// falls back to lexical ranking on any error.
type HTTPEmbedder struct {
	BaseURL string
	HTTP    *http.Client
}

func NewHTTPEmbedder(baseURL string, timeout time.Duration) *HTTPEmbedder {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &HTTPEmbedder{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		HTTP:    &http.Client{Timeout: timeout},
	}
}

func (e *HTTPEmbedder) EmbeddingIdentity() string {
	if e == nil {
		return ""
	}
	return "bge-m3-mlx:" + strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
}

func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e == nil || e.BaseURL == "" {
		return nil, fmt.Errorf("embedding service is not configured")
	}
	payload, err := json.Marshal(map[string]any{"texts": texts})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := e.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding service returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	if len(decoded.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embedding count %d does not match input count %d", len(decoded.Embeddings), len(texts))
	}
	return decoded.Embeddings, nil
}
