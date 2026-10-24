package modelconnection_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

func exampleCatalog() *modelconnection.Catalog {
	return &modelconnection.Catalog{
		Version:              modelconnection.Version,
		SelectedConnectionID: "provider-a",
		SelectedModelID:      "qwen3",
		Connections: []modelconnection.Connection{{
			ID:      "provider-a",
			Name:    "Provider A",
			Kind:    modelconnection.KindProviderAPI,
			BaseURL: "https://provider-a.example/v1",
			Models: []modelconnection.Model{{
				ID:              "qwen3",
				DisplayName:     "Qwen 3",
				ContextTokens:   32768,
				MaxOutputTokens: 8192,
				SupportsImages:  true,
				SupportsTools:   true,
			}},
		}},
	}
}

func TestDefaultPathIsScopedToExplicitDesktopHome(t *testing.T) {
	t.Setenv("MEADOW_MODEL_CONNECTIONS_FILE", "")
	t.Setenv("MEADOW_HOME", "")
	if got := modelconnection.DefaultPath(); got != "" {
		t.Fatalf("unconfigured process inherited a catalog path: %q", got)
	}
	home := t.TempDir()
	t.Setenv("MEADOW_HOME", home)
	if got := modelconnection.DefaultPath(); got != filepath.Join(home, "model-connections-v1.json") {
		t.Fatalf("catalog was not scoped to Desktop home: %q", got)
	}
	explicit := filepath.Join(t.TempDir(), "isolated-connections.json")
	t.Setenv("MEADOW_MODEL_CONNECTIONS_FILE", explicit)
	if got := modelconnection.DefaultPath(); got != explicit {
		t.Fatalf("explicit catalog override was not used: %q", got)
	}
}

func TestCatalogRoundTripSeparatesCredentialsAndUsesPrivateFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	catalog := exampleCatalog()
	if err := modelconnection.Save(path, catalog); err != nil {
		t.Fatal(err)
	}
	loaded, err := modelconnection.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, catalog) {
		t.Fatalf("catalog round trip changed connection capabilities: got %#v, want %#v", loaded, catalog)
	}
	if loaded.Fingerprint() == "" || loaded.Fingerprint() != catalog.Fingerprint() {
		t.Fatal("persisted catalog identity changed")
	}
	if key, err := modelconnection.Credential(path, "provider-a"); err != nil || key != "" {
		t.Fatalf("missing credential = %q, %v; want empty credential without error", key, err)
	}
	const firstSecret = "test-private-provider-key-123456"
	if err := modelconnection.SaveCredential(path, "provider-a", firstSecret); err != nil {
		t.Fatal(err)
	}
	key, err := modelconnection.Credential(path, "provider-a")
	if err != nil || key != firstSecret {
		t.Fatal("saved credential could not be retrieved for its connection")
	}
	if key, err := modelconnection.Credential(path, "provider-b"); err != nil || key != "" {
		t.Fatal("another connection inherited a credential")
	}
	public, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), firstSecret) || strings.Contains(string(encoded), firstSecret) {
		t.Fatal("public connection catalog exposed the credential")
	}
	before := loaded.Fingerprint()
	if err := modelconnection.SaveCredential(path, "provider-a", "test-rotated-private-provider-key"); err != nil {
		t.Fatal(err)
	}
	loaded, err = modelconnection.Load(path)
	if err != nil || loaded.Fingerprint() != before {
		t.Fatal("credential rotation changed non-secret connection identity")
	}
	if runtime.GOOS != "windows" {
		if err := filepath.Walk(filepath.Dir(path), func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if p == filepath.Dir(path) {
				return nil // The caller owns the pre-existing Desktop home.
			}
			if info.Mode().Perm()&0077 != 0 {
				return fmt.Errorf("connection storage %s has non-private mode %o", filepath.Base(p), info.Mode().Perm())
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConnectionScopedModelIdentityAndSelection(t *testing.T) {
	catalog := exampleCatalog()
	second := catalog.Connections[0]
	second.ID, second.Name, second.BaseURL = "provider-b", "Provider B", "https://provider-b.example/v1"
	catalog.Connections = append(catalog.Connections, second)
	if err := modelconnection.Validate(catalog); err != nil {
		t.Fatalf("same served model ID at separate connections must be valid: %v", err)
	}
	catalog.SelectedConnectionID = "provider-b"
	conn, model, err := catalog.Resolve()
	if err != nil || conn.ID != "provider-b" || conn.BaseURL != second.BaseURL || model.ID != "qwen3" {
		t.Fatalf("selection did not resolve the connection/model pair: connection=%+v model=%+v error=%v", conn, model, err)
	}
	if _, ok := catalog.LookupConnection("PROVIDER-B"); ok {
		t.Fatal("connection IDs were matched without exact identity")
	}
	if _, ok := conn.LookupModel("QWEN3"); ok {
		t.Fatal("served model IDs were matched without exact identity")
	}
	if _, ok := catalog.LookupConnection("missing"); ok {
		t.Fatal("unknown connection resolved")
	}
	if _, ok := conn.LookupModel("missing"); ok {
		t.Fatal("unknown served model resolved")
	}
}

func TestCatalogRejectsAmbiguousOrMissingSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*modelconnection.Catalog)
	}{
		{"duplicate connection", func(c *modelconnection.Catalog) { c.Connections = append(c.Connections, c.Connections[0]) }},
		{"duplicate model in connection", func(c *modelconnection.Catalog) {
			c.Connections[0].Models = append(c.Connections[0].Models, c.Connections[0].Models[0])
		}},
		{"unknown selected connection", func(c *modelconnection.Catalog) { c.SelectedConnectionID = "missing" }},
		{"unknown selected model", func(c *modelconnection.Catalog) { c.SelectedModelID = "missing" }},
		{"missing selected connection", func(c *modelconnection.Catalog) { c.SelectedConnectionID = "" }},
		{"missing selected model", func(c *modelconnection.Catalog) { c.SelectedModelID = "" }},
		{"unknown kind", func(c *modelconnection.Catalog) { c.Connections[0].Kind = "unrecognized" }},
		{"future version", func(c *modelconnection.Catalog) { c.Version = modelconnection.Version + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := exampleCatalog()
			tc.mutate(catalog)
			if err := modelconnection.Validate(catalog); err == nil {
				t.Fatal("invalid catalog accepted")
			}
			if _, _, err := catalog.Resolve(); err == nil {
				t.Fatal("invalid catalog silently fell back to another connection or model")
			}
			path := filepath.Join(t.TempDir(), "invalid.json")
			data, err := json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := modelconnection.Load(path); err == nil {
				t.Fatal("invalid persisted catalog accepted")
			}
		})
	}
}

func TestCatalogFingerprintTracksConnectionSelectionAndCapabilities(t *testing.T) {
	original := exampleCatalog().Fingerprint()
	for _, tc := range []struct {
		name   string
		mutate func(*modelconnection.Catalog)
	}{
		{"origin", func(c *modelconnection.Catalog) { c.Connections[0].BaseURL = "https://replacement.example/v1" }},
		{"image capability", func(c *modelconnection.Catalog) { c.Connections[0].Models[0].SupportsImages = false }},
		{"tool capability", func(c *modelconnection.Catalog) { c.Connections[0].Models[0].SupportsTools = false }},
		{"output limit", func(c *modelconnection.Catalog) { c.Connections[0].Models[0].MaxOutputTokens = 4096 }},
		{"selected pair", func(c *modelconnection.Catalog) {
			c.Connections[0].Models = append(c.Connections[0].Models, modelconnection.Model{ID: "other", ContextTokens: 32768, MaxOutputTokens: 8192})
			c.SelectedModelID = "other"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := exampleCatalog()
			tc.mutate(changed)
			if changed.Fingerprint() == original {
				t.Fatal("daemon reuse fingerprint did not change with connection semantics")
			}
		})
	}
}

func TestCatalogRejectsSymlinkAndPubliclyReadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	if err := modelconnection.Save(path, exampleCatalog()); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := modelconnection.Load(link); err == nil {
		t.Fatal("symlink catalog accepted for reading")
	}
	if err := modelconnection.Save(link, exampleCatalog()); err == nil {
		t.Fatal("symlink catalog accepted for writing")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := modelconnection.Load(path); err == nil {
			t.Fatal("publicly readable catalog accepted")
		}
	}
}

func TestCredentialStoreRejectsSymlinkDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	if err := modelconnection.Save(path, exampleCatalog()); err != nil {
		t.Fatal(err)
	}
	if err := modelconnection.SaveCredential(path, "provider-a", "original-private-key"); err != nil {
		t.Fatal(err)
	}
	credentialDir := path + ".credentials"
	relocated := filepath.Join(t.TempDir(), "relocated-credentials")
	if err := os.Rename(credentialDir, relocated); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relocated, credentialDir); err != nil {
		t.Fatal(err)
	}
	if _, err := modelconnection.Credential(path, "provider-a"); err == nil {
		t.Fatal("credential read followed a symlink directory")
	}
	if err := modelconnection.SaveCredential(path, "provider-a", "replacement-private-key"); err == nil {
		t.Fatal("credential write followed a symlink directory")
	}
	if err := os.Remove(credentialDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(relocated, credentialDir); err != nil {
		t.Fatal(err)
	}
	if key, err := modelconnection.Credential(path, "provider-a"); err != nil || key != "original-private-key" {
		t.Fatal("refused symlink operation modified the original credential")
	}
}

func TestCanceledStorageUpdatesPreservePreviouslyCommittedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	if err := modelconnection.Save(path, exampleCatalog()); err != nil {
		t.Fatal(err)
	}
	if err := modelconnection.SaveCredential(path, "provider-a", "original-private-key"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	changed := exampleCatalog()
	changed.Connections[0].BaseURL = "https://replacement.example/v1"
	if err := modelconnection.SaveContext(ctx, path, changed); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled catalog update = %v, want context cancellation", err)
	}
	if err := modelconnection.SaveCredentialContext(ctx, path, "provider-a", "replacement-private-key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled credential update = %v, want context cancellation", err)
	}
	if err := modelconnection.SaveCredentialContext(ctx, path, "provider-a", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled credential removal = %v, want context cancellation", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("canceled catalog update changed committed bytes")
	}
	if key, err := modelconnection.Credential(path, "provider-a"); err != nil || key != "original-private-key" {
		t.Fatal("canceled credential update changed committed key")
	}
}

func TestCredentialRemovalIsScopedAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	if err := modelconnection.SaveCredential(path, "provider-a", "provider-a-private-key"); err != nil {
		t.Fatal(err)
	}
	if err := modelconnection.SaveCredential(path, "provider-b", "provider-b-private-key"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := modelconnection.DeleteCredential(path, "provider-a"); err != nil {
			t.Fatal(err)
		}
	}
	if key, err := modelconnection.Credential(path, "provider-a"); err != nil || key != "" {
		t.Fatal("removed connection credential remains available")
	}
	if key, err := modelconnection.Credential(path, "provider-b"); err != nil || key != "provider-b-private-key" {
		t.Fatal("removing one connection's key affected another connection")
	}
}

func TestCatalogRejectsAmbiguousJSONWithoutExposingPrivateFields(t *testing.T) {
	canonical, err := json.Marshal(exampleCatalog())
	if err != nil {
		t.Fatal(err)
	}
	valid := string(canonical)
	for _, tc := range []struct{ name, contents string }{
		{"duplicate selection", strings.Replace(valid, `"selected_model_id":"qwen3"`, `"selected_model_id":"qwen3","selected_model_id":"other"`, 1)},
		{"uppercase field alias", strings.Replace(valid, `"version":`, `"Version":`, 1)},
		{"unknown credential field", strings.Replace(valid, `"version":1`, `"version":1,"api_key":"secret-json-fixture"`, 1)},
		{"trailing object", valid + `{}`},
		{"null catalog", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "connections.json")
			if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := modelconnection.Load(path); err == nil {
				t.Fatal("ambiguous connection catalog accepted")
			} else if strings.Contains(err.Error(), "secret-json-fixture") {
				t.Fatal("catalog error exposed supplied private field")
			}
		})
	}
}

func TestBaseURLPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind modelconnection.Kind
		url  string
		ok   bool
	}{
		{"provider HTTPS", modelconnection.KindProviderAPI, "https://provider.example/v1", true},
		{"managed HTTPS", modelconnection.KindManagedAPI, "https://gateway.example/v1", true},
		{"provider plaintext external", modelconnection.KindProviderAPI, "http://provider.example/v1", false},
		{"managed plaintext external", modelconnection.KindManagedAPI, "http://gateway.example/v1", false},
		{"local loopback", modelconnection.KindLocalAPI, "http://127.0.0.1:8000/v1", true},
		{"local localhost", modelconnection.KindLocalAPI, "http://localhost:8000/v1", true},
		{"local IPv6 loopback", modelconnection.KindLocalAPI, "http://[::1]:8000/v1", true},
		{"local private LAN", modelconnection.KindLocalAPI, "http://192.168.50.2:8000/v1", true},
		{"core private LAN", modelconnection.KindMeadowCore, "http://10.1.2.3:18081/v1", true},
		{"core loopback", modelconnection.KindMeadowCore, "http://127.0.0.1:18081/v1", true},
		{"local public IP plaintext", modelconnection.KindLocalAPI, "http://8.8.8.8:8000/v1", false},
		{"core public IP plaintext", modelconnection.KindMeadowCore, "http://1.1.1.1:18081/v1", false},
		{"embedded password", modelconnection.KindProviderAPI, "https://member:credential-secret-fixture@example.com/v1", false},
		{"query credential", modelconnection.KindProviderAPI, "https://example.com/v1?api_key=credential-secret-fixture", false},
		{"fragment", modelconnection.KindProviderAPI, "https://example.com/v1#fragment", false},
		{"non HTTP transport", modelconnection.KindLocalAPI, "file:///private/connection", false},
		{"missing host", modelconnection.KindProviderAPI, "https:///v1", false},
		{"invalid port", modelconnection.KindProviderAPI, "https://example.com:65536/v1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := modelconnection.ValidateBaseURL(tc.kind, tc.url)
			if (err == nil) != tc.ok {
				t.Fatalf("URL policy allowed=%v, want %v; error=%v", err == nil, tc.ok, err)
			}
			if err != nil && strings.Contains(err.Error(), "credential-secret-fixture") {
				t.Fatal("URL validation error exposed supplied credentials")
			}
		})
	}
}

func TestDiscoverModelsRetainsExplicitCapabilitiesForExistingModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credential-free local connection sent an Authorization header")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3"},{"id":"new-model"}]}`))
	}))
	defer server.Close()
	conn := exampleCatalog().Connections[0]
	conn.Kind, conn.BaseURL = modelconnection.KindLocalAPI, server.URL
	models, err := modelconnection.DiscoverModels(context.Background(), conn, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || !reflect.DeepEqual(models[0], conn.Models[0]) {
		t.Fatalf("model discovery discarded explicit capability configuration: %+v", models)
	}
	if models[1].ContextTokens != 0 || models[1].MaxOutputTokens != 0 || models[1].SupportsImages || models[1].SupportsTools {
		t.Fatal("new model inherited the existing model's capabilities")
	}
}

func TestDiscoverModelsUsesConfiguredBasePathAndScopedCredential(t *testing.T) {
	for _, basePath := range []string{"", "/v1"} {
		t.Run("base"+basePath, func(t *testing.T) {
			var observedPath, observedMethod, observedAuthorization string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observedPath, observedMethod, observedAuthorization = r.URL.Path, r.Method, r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"provider/model-one","object":"model","owned_by":"fixture"},{"id":"model-two","object":"model"}]}`))
			}))
			defer server.Close()
			conn := modelconnection.Connection{ID: "local-fixture", Name: "Local fixture", Kind: modelconnection.KindLocalAPI, BaseURL: server.URL + basePath}
			models, err := modelconnection.DiscoverModels(context.Background(), conn, "fixture-key")
			if err != nil {
				t.Fatal(err)
			}
			if observedPath != basePath+"/models" || observedMethod != http.MethodGet || observedAuthorization != "Bearer fixture-key" {
				t.Fatalf("model discovery request path=%q method=%q authorization present=%v", observedPath, observedMethod, observedAuthorization != "")
			}
			if len(models) != 2 || models[0].ID != "provider/model-one" || models[1].ID != "model-two" {
				t.Fatalf("served model identity was not preserved: %+v", models)
			}
			for _, model := range models {
				if model.SupportsImages || model.SupportsTools || model.ContextTokens != 0 || model.MaxOutputTokens != 0 {
					t.Fatal("model-list metadata invented unreported model capabilities")
				}
			}
		})
	}
}

func TestDiscoverModelsDoesNotFollowRedirectOrForwardCredential(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"wrong-origin-model"}]}`))
	}))
	defer destination.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/models", http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	conn := modelconnection.Connection{ID: "local-fixture", Kind: modelconnection.KindLocalAPI, BaseURL: first.URL}
	if _, err := modelconnection.DiscoverModels(context.Background(), conn, "private-redirect-key"); err == nil {
		t.Fatal("redirected model discovery accepted")
	}
	if redirected.Load() != 0 {
		t.Fatal("model discovery followed a redirect to a second origin")
	}
}

func TestDiscoverModelsRejectsErrorsAndOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"upstream auth failure", http.StatusUnauthorized, `{"error":"server-private-echo"}`},
		{"invalid JSON", http.StatusOK, `{"data":`},
		{"over one MiB", http.StatusOK, `{"data":[{"id":"` + strings.Repeat("x", 1<<20) + `"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			conn := modelconnection.Connection{ID: "local-fixture", Kind: modelconnection.KindLocalAPI, BaseURL: server.URL}
			if _, err := modelconnection.DiscoverModels(context.Background(), conn, "test-key"); err == nil {
				t.Fatal("invalid model discovery response accepted")
			} else if strings.Contains(err.Error(), "server-private-echo") {
				t.Fatal("model discovery reflected the upstream private error body")
			}
		})
	}
}

func TestDiscoverModelsHonorsCallerCancellation(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := modelconnection.Connection{ID: "local-fixture", Kind: modelconnection.KindLocalAPI, BaseURL: server.URL}
	result := make(chan error, 1)
	go func() {
		_, err := modelconnection.DiscoverModels(ctx, conn, "")
		result <- err
	}()
	select {
	case <-entered:
		cancel()
	case err := <-result:
		t.Fatalf("discovery returned before the fixture received the request: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("discovery never reached the local fixture")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation did not propagate: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discovery ignored caller cancellation")
	}
}

func TestServiceKeyPreservesEscapedProviderRoutes(t *testing.T) {
	core := modelconnection.Connection{Kind: modelconnection.KindMeadowCore, BaseURL: "http://127.0.0.1:18081/v1/"}
	alias := core
	alias.BaseURL = "http://localhost:18081/v1"
	if core.ServiceKey() != alias.ServiceKey() {
		t.Fatal("loopback alias creates another Core service")
	}
	provider := modelconnection.Connection{Kind: modelconnection.KindProviderAPI, BaseURL: "https://provider.invalid/v1%2F"}
	literal := provider
	literal.BaseURL = "https://provider.invalid/v1/"
	if provider.ServiceKey() == literal.ServiceKey() {
		t.Fatal("escaped provider route collapsed into a distinct service")
	}
	trailing := provider
	trailing.BaseURL += "/"
	if provider.ServiceKey() != trailing.ServiceKey() {
		t.Fatal("literal trailing slash creates another provider service")
	}
}
