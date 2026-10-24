package modelconnection

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	anthropicDiscoveryPageSize = 100
	anthropicDiscoveryMaxPages = 8
	anthropicDiscoveryVersion  = "2023-06-01"
)

// These optional fields are declarations from the Models API, not deductions
// from a model ID. server_tools does not establish client tool-use support.
type anthropicModelInfo struct {
	ID             string  `json:"id"`
	DisplayName    *string `json:"display_name"`
	MaxInputTokens *int    `json:"max_input_tokens"`
	MaxTokens      *int    `json:"max_tokens"`
	Capabilities   *struct {
		ImageInput *struct {
			Supported *bool `json:"supported"`
		} `json:"image_input"`
	} `json:"capabilities"`
}

// Anthropic's Models API uses after_id/last_id pagination and API-key/version
// headers: https://platform.claude.com/docs/en/api/models/list . The complete
// operation shares one time and byte budget, including subsequent pages.
func discoverAnthropicModels(ctx context.Context, connection Connection, credential string) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	endpoint := strings.TrimRight(connection.BaseURL, "/") + "/models"
	models := make([]Model, 0)
	seen := map[string]bool{}
	seenCursors := map[string]bool{}
	afterID, remainingBytes := "", maxCatalogBytes
	for page := 0; page < anthropicDiscoveryMaxPages; page++ {
		query := url.Values{"limit": {"100"}}
		if afterID != "" {
			query.Set("after_id", afterID)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("model discovery request is invalid")
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("anthropic-version", anthropicDiscoveryVersion)
		if credential != "" {
			request.Header.Set("x-api-key", credential)
		}
		data, err := readAnthropicModelsPage(ctx, client, request, remainingBytes)
		if err != nil {
			return nil, err
		}
		remainingBytes -= len(data)
		var list struct {
			Data    []anthropicModelInfo `json:"data"`
			HasMore *bool                `json:"has_more"`
			LastID  string               `json:"last_id"`
		}
		if !utf8.Valid(data) || json.Unmarshal(data, &list) != nil || list.Data == nil || list.HasMore == nil || len(list.Data) > anthropicDiscoveryPageSize || len(models)+len(list.Data) > 512 {
			return nil, fmt.Errorf("model discovery response is invalid")
		}
		for _, item := range list.Data {
			if !validText(item.ID, 512) || strings.TrimSpace(item.ID) != item.ID || seen[item.ID] {
				return nil, fmt.Errorf("model discovery contains invalid or duplicate model IDs")
			}
			seen[item.ID] = true
			model, ok := connection.LookupModel(item.ID)
			if !ok {
				model = Model{ID: item.ID, DisplayName: item.ID}
			}
			if item.DisplayName != nil {
				if !validText(*item.DisplayName, 512) {
					return nil, fmt.Errorf("model discovery display name is invalid")
				}
				model.DisplayName = *item.DisplayName
			}
			if item.MaxInputTokens != nil {
				model.ContextTokens = *item.MaxInputTokens
			}
			if item.MaxTokens != nil {
				model.MaxOutputTokens = *item.MaxTokens
			}
			if model.ContextTokens < 0 || model.MaxOutputTokens < 0 || (model.ContextTokens > 0 && model.MaxOutputTokens > model.ContextTokens) {
				return nil, fmt.Errorf("model discovery limits are invalid")
			}
			if item.Capabilities != nil && item.Capabilities.ImageInput != nil && item.Capabilities.ImageInput.Supported != nil {
				model.SupportsImages = *item.Capabilities.ImageInput.Supported
			}
			// Existing explicitly configured client tool support is retained.
			// The current API does not declare it, so new models remain unknown.
			models = append(models, model)
		}
		if !*list.HasMore {
			return models, nil
		}
		if len(models) >= 512 || len(list.Data) == 0 || list.LastID != list.Data[len(list.Data)-1].ID || seenCursors[list.LastID] {
			return nil, fmt.Errorf("model discovery pagination is invalid")
		}
		seenCursors[list.LastID] = true
		afterID = list.LastID
	}
	return nil, fmt.Errorf("model discovery exceeds page limit")
}

func readAnthropicModelsPage(ctx context.Context, client *http.Client, request *http.Request, remainingBytes int) ([]byte, error) {
	if remainingBytes <= 0 {
		return nil, fmt.Errorf("model discovery response exceeds read limit")
	}
	response, err := client.Do(request)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("model discovery connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model discovery returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(remainingBytes)+1))
	if err != nil || len(data) > remainingBytes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("model discovery response exceeds read limit")
	}
	return data, nil
}
