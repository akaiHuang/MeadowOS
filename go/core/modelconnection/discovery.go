package modelconnection

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DiscoverModels performs read-only model discovery in the selected protocol.
// OpenAI-compatible discovery uses one request; Anthropic uses bounded pages.
// Neither probes inference nor follows a redirect with a credential.
func DiscoverModels(ctx context.Context, connection Connection, credential string) ([]Model, error) {
	if err := ValidateBaseURL(connection.Kind, connection.BaseURL); err != nil {
		return nil, err
	}
	if err := ValidateProtocol(connection.Kind, connection.Protocol); err != nil {
		return nil, err
	}
	if credential != "" && !validCredential(credential) {
		return nil, fmt.Errorf("model credential is invalid")
	}
	if connection.EffectiveProtocol() == ProtocolAnthropic {
		return discoverAnthropicModels(ctx, connection, credential)
	}
	endpoint := strings.TrimRight(connection.BaseURL, "/") + "/models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("model discovery request is invalid")
	}
	request.Header.Set("Accept", "application/json")
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
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
	data, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogBytes+1))
	if err != nil || len(data) > maxCatalogBytes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("model discovery response exceeds read limit")
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &list); err != nil || list.Data == nil || len(list.Data) > 512 {
		return nil, fmt.Errorf("model discovery response is invalid")
	}
	models := make([]Model, 0, len(list.Data))
	seen := map[string]bool{}
	for _, item := range list.Data {
		if !validText(item.ID, 512) || strings.TrimSpace(item.ID) != item.ID || seen[item.ID] {
			return nil, fmt.Errorf("model discovery contains invalid or duplicate model IDs")
		}
		seen[item.ID] = true
		// Preserve explicit configuration for an already-known model, while only
		// asserting the ID for models newly discovered from an untyped endpoint.
		model, ok := connection.LookupModel(item.ID)
		if !ok {
			model = Model{ID: item.ID, DisplayName: item.ID}
		}
		models = append(models, model)
	}
	return models, nil
}
