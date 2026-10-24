package modelconnection_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

func anthropicFixtureConnection(baseURL string) modelconnection.Connection {
	return modelconnection.Connection{ID: "anthropic-fixture", Kind: modelconnection.KindLocalAPI, BaseURL: baseURL, Protocol: modelconnection.ProtocolAnthropic}
}

func TestAnthropicDiscoveryHeadersPaginationAndDeclaredCapabilities(t *testing.T) {
	const cursor = "model/with?version=1&part=2"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.URL.Query().Get("limit") != "100" || r.Header.Get("x-api-key") != "fixture-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" || r.Header.Get("Accept") != "application/json" {
			t.Error("Anthropic discovery headers, method or path differ from the contract")
		}
		if page == 1 {
			if len(r.URL.Query()) != 1 {
				t.Error("first page supplied an unexpected cursor")
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"known","display_name":"Known model","max_input_tokens":200000,"max_tokens":10000,"capabilities":{"image_input":{"supported":false}}},{"id":"model/with?version=1&part=2","display_name":"Vision model","max_input_tokens":1000000,"max_tokens":128000,"capabilities":{"image_input":{"supported":true},"server_tools":{"supported":true}}}],"has_more":true,"last_id":"model/with?version=1&part=2"}`))
			return
		}
		if page != 2 || len(r.URL.Query()) != 2 || r.URL.Query().Get("after_id") != cursor {
			t.Error("next page did not preserve the opaque cursor")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-name-without-metadata","capabilities":null,"max_input_tokens":null,"max_tokens":null},{"id":"known-no-metadata"}],"has_more":false,"last_id":"known-no-metadata"}`))
	}))
	defer server.Close()
	connection := anthropicFixtureConnection(server.URL + "/v1/")
	connection.Models = []modelconnection.Model{
		{ID: "known", DisplayName: "Old label", ContextTokens: 65536, MaxOutputTokens: 8192, SupportsImages: true, SupportsTools: true},
		{ID: "known-no-metadata", DisplayName: "Explicit label", ContextTokens: 32768, MaxOutputTokens: 8192, SupportsImages: true, SupportsTools: true},
	}
	models, err := modelconnection.DiscoverModels(context.Background(), connection, "fixture-key")
	if err != nil || len(models) != 4 || requests.Load() != 2 {
		t.Fatalf("discovery returned %d models in %d requests: %v", len(models), requests.Load(), err)
	}
	if models[0].DisplayName != "Known model" || models[0].ContextTokens != 200000 || models[0].MaxOutputTokens != 10000 || models[0].SupportsImages || !models[0].SupportsTools {
		t.Fatalf("explicit API capabilities not applied correctly: %+v", models[0])
	}
	if models[1].ID != cursor || models[1].ContextTokens != 1000000 || models[1].MaxOutputTokens != 128000 || !models[1].SupportsImages || models[1].SupportsTools {
		t.Fatalf("image declaration lost or server tools confused with client tools: %+v", models[1])
	}
	if models[2] != (modelconnection.Model{ID: "claude-name-without-metadata", DisplayName: "claude-name-without-metadata"}) {
		t.Fatalf("model name invented capabilities: %+v", models[2])
	}
	if models[3] != connection.Models[1] {
		t.Fatalf("absent metadata discarded explicit configuration: %+v", models[3])
	}
}

func TestAnthropicDiscoveryRejectsMalformedMetadataAndPagination(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"invalid JSON", `{"data":`},
		{"missing data", `{"has_more":false}`},
		{"missing continuation signal", `{"data":[]}`},
		{"invalid continuation type", `{"data":[],"has_more":"false"}`},
		{"empty continued page", `{"data":[],"has_more":true,"last_id":"private-response"}`},
		{"missing cursor", `{"data":[{"id":"model"}],"has_more":true}`},
		{"unrelated cursor", `{"data":[{"id":"model"}],"has_more":true,"last_id":"private-response"}`},
		{"duplicate model", `{"data":[{"id":"model"},{"id":"model"}],"has_more":false}`},
		{"blank model", `{"data":[{"id":" "}],"has_more":false}`},
		{"negative context", `{"data":[{"id":"model","max_input_tokens":-1}],"has_more":false}`},
		{"negative output", `{"data":[{"id":"model","max_tokens":-1}],"has_more":false}`},
		{"output above context", `{"data":[{"id":"model","max_input_tokens":10,"max_tokens":11}],"has_more":false}`},
		{"fractional limit", `{"data":[{"id":"model","max_tokens":1.1}],"has_more":false}`},
		{"malformed capability", `{"data":[{"id":"model","capabilities":{"image_input":{"supported":"true"}}}],"has_more":false}`},
		{"control display name", `{"data":[{"id":"model","display_name":"private-response\n"}],"has_more":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			models, err := modelconnection.DiscoverModels(context.Background(), anthropicFixtureConnection(server.URL), "private-key")
			if err == nil || models != nil || strings.Contains(err.Error(), "private-response") || strings.Contains(err.Error(), "private-key") {
				t.Fatalf("malformed response was accepted, exposed partial models, or leaked source text: %v", err)
			}
		})
	}
}

func TestAnthropicDiscoveryBoundsModelsPagesAndTotalBody(t *testing.T) {
	for _, tc := range []struct {
		name         string
		maxRequests  int32
		pageContents func(int32) string
	}{
		{"page over requested limit", 1, func(page int32) string { return anthropicPage(0, 101, false, "") }},
		{"catalog over 512 models", 6, func(page int32) string { return anthropicPage(int(page-1)*100, 100, true, "") }},
		{"too many small pages", 8, func(page int32) string { return anthropicPage(int(page), 1, true, "") }},
		{"duplicate across pages", 2, func(page int32) string { return anthropicPage(0, 1, true, "") }},
		{"single body over one MiB", 1, func(page int32) string { return anthropicPage(0, 1, false, strings.Repeat("p", 1<<20)) }},
		{"combined bodies over one MiB", 2, func(page int32) string { return anthropicPage(int(page), 1, true, strings.Repeat("p", 600000)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.pageContents(requests.Add(1)))) }))
			defer server.Close()
			models, err := modelconnection.DiscoverModels(context.Background(), anthropicFixtureConnection(server.URL), "fixture-key")
			if err == nil || models != nil || requests.Load() != tc.maxRequests {
				t.Fatalf("discovery bounds failed: requests=%d want=%d models=%d err=%v", requests.Load(), tc.maxRequests, len(models), err)
			}
		})
	}
}

func anthropicPage(start, count int, hasMore bool, padding string) string {
	data := make([]map[string]string, 0, count)
	lastID := ""
	for i := 0; i < count; i++ {
		lastID = fmt.Sprintf("model-%d", start+i)
		data = append(data, map[string]string{"id": lastID})
	}
	result, _ := json.Marshal(map[string]any{"data": data, "has_more": hasMore, "last_id": lastID, "padding": padding})
	return string(result)
}

func TestAnthropicDiscoveryAllows512ModelsAndEmptyCatalog(t *testing.T) {
	for _, total := range []int{0, 512} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start := int(requests.Add(1)-1) * 100
				count := min(total-start, 100)
				if r.Header.Get("x-api-key") != "" || r.Header.Get("Authorization") != "" {
					t.Error("credential-free local discovery sent credentials")
				}
				_, _ = w.Write([]byte(anthropicPage(start, count, start+count < total, "")))
			}))
			defer server.Close()
			models, err := modelconnection.DiscoverModels(context.Background(), anthropicFixtureConnection(server.URL), "")
			if err != nil || len(models) != total {
				t.Fatalf("valid model count %d rejected: got=%d err=%v", total, len(models), err)
			}
		})
	}
}

func TestAnthropicDiscoveryRefusesRedirectAndRedactsErrors(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", destination.URL+"/models")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"private-upstream-body private-key"}`))
			}))
			defer server.Close()
			models, err := modelconnection.DiscoverModels(context.Background(), anthropicFixtureConnection(server.URL), "private-key")
			if err == nil || models != nil || strings.Contains(err.Error(), "private-") || !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Fatalf("unsafe HTTP error handling: %v", err)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("Anthropic discovery forwarded a credential across a redirect")
	}
}

func TestAnthropicDiscoveryHonorsCancellationDuringPagination(t *testing.T) {
	var requests atomic.Int32
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(anthropicPage(0, 1, true, "")))
			return
		}
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		models, err := modelconnection.DiscoverModels(ctx, anthropicFixtureConnection(server.URL), "fixture-key")
		if models != nil {
			t.Error("cancellation returned a partial model catalog")
		}
		result <- err
	}()
	select {
	case <-entered:
		cancel()
	case err := <-result:
		t.Fatalf("discovery returned before reaching the second page: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("discovery never requested the second page")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || requests.Load() != 2 {
			t.Fatalf("pagination cancellation failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discovery ignored cancellation")
	}
}
