package modelconnection_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

func TestProtocolDefaultsPreserveLegacyCatalog(t *testing.T) {
	catalog := exampleCatalog()
	before, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), `"protocol"`) {
		t.Fatal("legacy catalog serialization gained a protocol field")
	}
	beforeFingerprint := catalog.Fingerprint()
	path := filepath.Join(t.TempDir(), "connections.json")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := modelconnection.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	connection, _, err := loaded.Resolve()
	if err != nil || connection.Protocol != "" || connection.EffectiveProtocol() != modelconnection.ProtocolOpenAICompatible {
		t.Fatalf("legacy protocol did not resolve to OpenAI-compatible: %v", err)
	}
	after, err := json.Marshal(loaded)
	if err != nil || string(after) != string(before) || loaded.Fingerprint() != beforeFingerprint {
		t.Fatal("legacy catalog bytes or reuse fingerprint changed on load")
	}
	loaded.Connections[0].Protocol = modelconnection.ProtocolAnthropic
	if loaded.Fingerprint() == beforeFingerprint || loaded.Fingerprint() == "" {
		t.Fatal("wire protocol did not change the connection fingerprint")
	}
	if err := modelconnection.Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	reopened, err := modelconnection.Load(path)
	if err != nil || reopened.Connections[0].EffectiveProtocol() != modelconnection.ProtocolAnthropic || reopened.Fingerprint() != loaded.Fingerprint() {
		t.Fatal("Anthropic protocol did not survive save/reopen")
	}
}

func TestProtocolValidationPreservesManagedAndCoreContracts(t *testing.T) {
	for _, kind := range []modelconnection.Kind{modelconnection.KindProviderAPI, modelconnection.KindLocalAPI, modelconnection.KindManagedAPI, modelconnection.KindMeadowCore} {
		for _, protocol := range []modelconnection.Protocol{"", modelconnection.ProtocolOpenAICompatible, modelconnection.ProtocolAnthropic, "private-invalid-protocol", "ANTHROPIC", " anthropic"} {
			t.Run(string(kind)+"/"+string(protocol), func(t *testing.T) {
				catalog := exampleCatalog()
				catalog.Connections[0].Kind = kind
				catalog.Connections[0].Protocol = protocol
				wantOK := protocol == "" || protocol == modelconnection.ProtocolOpenAICompatible || (protocol == modelconnection.ProtocolAnthropic && (kind == modelconnection.KindProviderAPI || kind == modelconnection.KindLocalAPI))
				err := modelconnection.Validate(catalog)
				if (err == nil) != wantOK {
					t.Fatalf("protocol allowed=%v, want %v: %v", err == nil, wantOK, err)
				}
				if err != nil && strings.Contains(err.Error(), "private-invalid-protocol") {
					t.Fatal("protocol error reflected untrusted protocol contents")
				}
				if !wantOK {
					if _, _, err := catalog.Resolve(); err == nil || catalog.Fingerprint() != "" {
						t.Fatal("invalid protocol resolved or obtained a reuse fingerprint")
					}
				}
			})
		}
	}
}

func TestDiscoveryRejectsUnknownProtocolBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	connection := modelconnection.Connection{ID: "fixture", Kind: modelconnection.KindLocalAPI, BaseURL: server.URL, Protocol: "private-invalid-protocol"}
	if _, err := modelconnection.DiscoverModels(context.Background(), connection, "fixture-key"); err == nil || strings.Contains(err.Error(), "private-invalid-protocol") {
		t.Fatalf("unknown protocol did not fail safely: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("unknown protocol reached the endpoint")
	}
}

func TestExplicitOpenAIProtocolRetainsSingleGETAndBearer(t *testing.T) {
	for _, protocol := range []modelconnection.Protocol{"", modelconnection.ProtocolOpenAICompatible} {
		t.Run(string(protocol), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/v1/models" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer fixture-key" || r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != "" {
					t.Error("OpenAI request wire contract changed")
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"fixture"}],"has_more":true,"last_id":"fixture"}`))
			}))
			defer server.Close()
			connection := modelconnection.Connection{ID: "fixture", Kind: modelconnection.KindLocalAPI, BaseURL: server.URL + "/v1", Protocol: protocol}
			models, err := modelconnection.DiscoverModels(context.Background(), connection, "fixture-key")
			if err != nil || len(models) != 1 || requests.Load() != 1 {
				t.Fatalf("OpenAI discovery changed: model count=%d requests=%d error=%v", len(models), requests.Load(), err)
			}
		})
	}
}
