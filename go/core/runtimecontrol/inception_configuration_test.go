package runtimecontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

const fixtureInceptionKey = "synthetic-test-key-NOT-A-REAL-CREDENTIAL"

func TestConfigureInceptionPrivatePersistenceAndActivation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile", "model-connections-v1.json")
	adapter, err := NewExternalAdapter(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := adapter.ConfigureInception(context.Background(), fixtureInceptionKey)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := modelconnection.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	connection, model, err := catalog.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if model.ID != "mercury-3" || !model.SupportsTools || model.SupportsImages || model.ReasoningControl != "inception-effort" || model.Disabled {
		t.Fatalf("incomplete native capabilities: %+v", model)
	}
	if connection.Kind != modelconnection.KindProviderAPI || connection.Protocol != modelconnection.ProtocolOpenAICompatible || connection.BaseURL != "https://api.inceptionlabs.ai/v1" {
		t.Fatalf("unexpected fixed route: %+v", connection)
	}
	if applied.ConnectionID != connection.ID || applied.ModelID != model.ID || applied.Fingerprint != catalog.Fingerprint() || !adapter.AllowsModel(model.ID) {
		t.Fatal("persisted and applied revisions differ")
	}
	status, ready, err := adapter.Status(context.Background())
	if err != nil || !ready || status.Model.ID != model.ID {
		t.Fatal("native selection was not applied")
	}
	digest := sha256.Sum256([]byte(connection.ID))
	credentialPath := filepath.Join(path+".credentials", hex.EncodeToString(digest[:])+".key")
	for _, name := range []string{path, credentialPath} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file permissions: %s", name)
		}
	}
	info, err := os.Stat(path + ".credentials")
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("credential directory permissions")
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), fixtureInceptionKey) {
		t.Fatal("catalog contains a credential")
	}
	key, err := modelconnection.Credential(path, connection.ID)
	if err != nil || key != fixtureInceptionKey {
		t.Fatal("native credential store mismatch")
	}
	// Reopening the adapter proves a container restart reads the same private state.
	reopened, err := NewExternalAdapter(path, "", "")
	if err != nil || reopened.ConfigurationIdentity().ConnectionID != connection.ID {
		t.Fatal("persistent selection did not reopen")
	}
}

func TestConfigureInceptionStableIdentityAndCredentialRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	adapter, _ := NewExternalAdapter(path, "", "")
	first, err := adapter.ConfigureInception(context.Background(), fixtureInceptionKey)
	if err != nil {
		t.Fatal(err)
	}
	again, err := adapter.ConfigureInception(context.Background(), fixtureInceptionKey)
	if err != nil || again.ConnectionID != first.ConnectionID {
		t.Fatal("same key created a duplicate service")
	}
	rotated, err := adapter.ConfigureInception(context.Background(), fixtureInceptionKey+"-rotated")
	if err != nil || rotated.ConnectionID == first.ConnectionID {
		t.Fatal("changed key reused a chat identity")
	}
	oldKey, err := modelconnection.Credential(path, first.ConnectionID)
	if err != nil || oldKey != fixtureInceptionKey {
		t.Fatal("rotation changed an existing chat's saved key")
	}
	catalog, _ := modelconnection.Load(path)
	if len(catalog.Connections) != 1 || catalog.SelectedConnectionID != rotated.ConnectionID {
		t.Fatal("rotation left duplicate visible services")
	}
}

func TestConfigureInceptionRejectsUnsafeOrStaleStateWithoutChangingIt(t *testing.T) {
	for _, key := range []string{"", " fake", "fake\nkey", "fake key"} {
		path := filepath.Join(t.TempDir(), "connections.json")
		adapter, _ := NewExternalAdapter(path, "", "")
		if _, err := adapter.ConfigureInception(context.Background(), key); err == nil {
			t.Fatal("invalid credential accepted")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("invalid key created catalog")
		}
	}
	path := filepath.Join(t.TempDir(), "connections.json")
	adapter, _ := NewExternalAdapter(path, "", "")
	if _, err := adapter.ConfigureInception(context.Background(), fixtureInceptionKey); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	catalog, _ := modelconnection.Load(path)
	catalog.Connections[0].Name = "changed outside the running Agent"
	if err := modelconnection.Save(path, catalog); err != nil {
		t.Fatal(err)
	}
	changed, _ := os.ReadFile(path)
	if _, err := adapter.ConfigureInception(context.Background(), fixtureInceptionKey); err == nil {
		t.Fatal("stale runtime overwrote a catalog")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(changed) || string(after) == string(before) {
		t.Fatal("stale revision was modified")
	}
}

func TestConfigureInceptionCancellationDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	adapter, _ := NewExternalAdapter(path, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.ConfigureInception(ctx, fixtureInceptionKey); err == nil {
		t.Fatal("cancelled setup accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cancelled setup wrote state")
	}
}

func TestConfigureInceptionCanonicalizesEquivalentEndpointWithNewIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	catalog := &modelconnection.Catalog{
		Version: 1, SelectedConnectionID: "existing-inception", SelectedModelID: "mercury-3",
		Connections: []modelconnection.Connection{{ID: "existing-inception", Kind: modelconnection.KindProviderAPI,
			Name: "Existing Inception", BaseURL: "https://api.inceptionlabs.ai/v1/",
			Models: []modelconnection.Model{{ID: "mercury-3"}}}},
	}
	if err := modelconnection.Save(path, catalog); err != nil {
		t.Fatal(err)
	}
	if err := modelconnection.SaveCredential(path, "existing-inception", fixtureInceptionKey); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewExternalAdapter(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := adapter.ConfigureInception(context.Background(), fixtureInceptionKey)
	if err != nil || applied.ConnectionID == "existing-inception" {
		t.Fatal("canonical endpoint reused immutable old identity")
	}
	catalog, err = modelconnection.Load(path)
	if err != nil || len(catalog.Connections) != 1 || catalog.Connections[0].BaseURL != "https://api.inceptionlabs.ai/v1" {
		t.Fatal("canonical service was not saved")
	}
	key, err := modelconnection.Credential(path, "existing-inception")
	if err != nil || key != fixtureInceptionKey {
		t.Fatal("canonicalization modified the old credential binding")
	}
}
