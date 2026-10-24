//go:build !meadow_protected

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// warmMeadowCoreModel pays one bounded decode after Core reports ready so the
// first user turn does not also absorb lazy chat-template/kernel setup. Model
// loading remains owned by the existing supervisor and /native/load-model flow.
func warmMeadowCoreModel(ctx context.Context, baseURL, apiKey, model string) error {
	payload := struct {
		Model     string              `json:"model"`
		Messages  []map[string]string `json:"messages"`
		MaxTokens int                 `json:"max_tokens"`
		Stream    bool                `json:"stream"`
	}{
		Model:     strings.TrimSpace(model),
		Messages:  []map[string]string{{"role": "user", "content": "OK"}},
		MaxTokens: 1,
		Stream:    false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal model warm-up: %w", err)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/chat/completions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create model warm-up request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(apiKey); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("model warm-up request: %w", err)
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if readErr != nil {
		return fmt.Errorf("read model warm-up response: %w", readErr)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message := strings.TrimSpace(string(responseBody))
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("model warm-up failed (%s): %s", response.Status, message)
	}
	return nil
}
