package runtimecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

// ExternalConnection describes an API binding, not an owned model process.
// A paired Core with an inference-only key also uses this capability.
type ExternalConnection interface {
	Connection() (modelconnection.Connection, error)
	ConfiguredModel() (string, int, int)
	AllowsModel(string) bool
}

// ExternalConfiguration is applied native metadata, never credential material.
// Generation also changes for credential-only reloads whose catalog is equal.
type ExternalConfiguration struct {
	ConnectionID string
	ModelID      string
	Fingerprint  string
	Generation   uint64
}

// ExternalAdapter never launches, loads, unloads, probes or stops a server.
// ReloadConfiguration is called only by the Agent's whole-runtime idle lease.
// A changed on-disk catalog remains unusable until that transaction commits.
type ExternalAdapter struct {
	mu                                        sync.RWMutex
	path, connectionID, fingerprint, selected string
	connection                                modelconnection.Connection
	identities                                map[string]string
	generation                                uint64
	closed                                    bool
}

func NewExternalAdapter(path, connectionID, modelID string) (*ExternalAdapter, error) {
	a := &ExternalAdapter{path: path, selected: "unconfigured", identities: make(map[string]string)}
	catalog, err := modelconnection.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	connection, selected, err := externalCatalogSelection(catalog)
	if err != nil {
		return nil, err
	}
	if connectionID != "" && connectionID != connection.ID {
		return nil, errors.New("selected API connection changed")
	}
	if modelID != "" && modelID != "unconfigured" && connection.ID != "" {
		model, ok := connection.LookupModel(modelID)
		if !ok {
			return nil, errors.New("selected API model is not configured")
		}
		selected = model.ID
	}
	a.publish(catalog, connection, selected)
	return a, nil
}

func externalCatalogSelection(catalog *modelconnection.Catalog) (modelconnection.Connection, string, error) {
	if err := modelconnection.Validate(catalog); err != nil {
		return modelconnection.Connection{}, "", err
	}
	if catalog.SelectedConnectionID == "" {
		return modelconnection.Connection{}, "unconfigured", nil
	}
	connection, model, err := catalog.Resolve()
	return connection, model.ID, err
}

// Endpoint identity cannot be edited in-place under a session's durable ID.
// Names/capabilities may change; endpoint/kind/protocol/account need a new ID.
func externalEndpointIdentity(connection modelconnection.Connection) string {
	connection.Name = ""
	connection.Models = nil
	connection.Protocol = connection.EffectiveProtocol()
	data, _ := json.Marshal(connection)
	return string(data)
}

// Caller holds mu, or has not published the newly constructed adapter.
func (a *ExternalAdapter) publish(catalog *modelconnection.Catalog, connection modelconnection.Connection, selected string) {
	for _, item := range catalog.Connections {
		a.identities[item.ID] = externalEndpointIdentity(item)
	}
	a.connectionID, a.connection, a.selected, a.fingerprint = connection.ID, connection, selected, catalog.Fingerprint()
	a.generation++
}

// ReloadConfiguration validates the exact saved revision before a single
// in-memory publication. It performs no provider request or process lifecycle.
// The caller must hold the existing whole-Agent configuration lease.
func (a *ExternalAdapter) ReloadConfiguration(expectedFingerprint string) (ExternalConfiguration, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ExternalConfiguration{}, errors.New("API connection is closed")
	}
	catalog, err := modelconnection.Load(a.path)
	if err != nil {
		return ExternalConfiguration{}, errors.New("saved API configuration is unavailable")
	}
	if expectedFingerprint == "" || catalog.Fingerprint() != expectedFingerprint {
		return ExternalConfiguration{}, errors.New("API configuration revision changed")
	}
	connection, selected, err := externalCatalogSelection(catalog)
	if err != nil {
		return ExternalConfiguration{}, errors.New("saved API configuration is invalid")
	}
	for _, item := range catalog.Connections {
		if previous, ok := a.identities[item.ID]; ok && previous != externalEndpointIdentity(item) {
			return ExternalConfiguration{}, errors.New("API endpoint identity changed; save it as a new connection")
		}
	}
	a.publish(catalog, connection, selected)
	return a.configurationLocked(), nil
}

func (a *ExternalAdapter) configurationLocked() ExternalConfiguration {
	return ExternalConfiguration{ConnectionID: a.connectionID, ModelID: a.selected, Fingerprint: a.fingerprint, Generation: a.generation}
}

func (a *ExternalAdapter) ConfigurationIdentity() ExternalConfiguration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.configurationLocked()
}

func (a *ExternalAdapter) Connection() (modelconnection.Connection, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.connectionLocked()
}

// Caller holds mu so connection, selection and capabilities share a revision.
func (a *ExternalAdapter) connectionLocked() (modelconnection.Connection, error) {
	if a.closed {
		return modelconnection.Connection{}, errors.New("API connection is closed")
	}
	if a.connectionID == "" {
		return modelconnection.Connection{}, errors.New("configure an API connection in Meadow Desktop settings")
	}
	catalog, err := modelconnection.Load(a.path)
	if err != nil || catalog.Fingerprint() != a.fingerprint {
		return modelconnection.Connection{}, errors.New("API configuration changed; apply the saved settings before continuing")
	}
	connection := a.connection
	connection.Models = append([]modelconnection.Model(nil), connection.Models...)
	return connection, nil
}

func (a *ExternalAdapter) AllowsModel(id string) bool {
	connection, err := a.Connection()
	if err != nil {
		return false
	}
	_, ok := connection.LookupModel(strings.TrimSpace(id))
	return ok
}
func (a *ExternalAdapter) ConfiguredModel() (string, int, int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	connection, err := a.connectionLocked()
	if err != nil {
		return "unconfigured", 0, 0
	}
	model, ok := connection.LookupModel(a.selected)
	if !ok {
		return "unconfigured", 0, 0
	}
	return model.ID, model.ContextTokens, model.MaxOutputTokens
}
func (a *ExternalAdapter) Ensure(context.Context) error { return nil }
func (a *ExternalAdapter) Activate(_ context.Context, id string) (LifecycleResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	connection, err := a.connectionLocked()
	if err != nil {
		return LifecycleResult{}, err
	}
	if _, ok := connection.LookupModel(strings.TrimSpace(id)); !ok {
		return LifecycleResult{}, errors.New("API model is not configured")
	}
	a.selected = strings.TrimSpace(id)
	return LifecycleResult{Accepted: true, Model: id, State: "configured"}, nil
}
func (a *ExternalAdapter) Unload(context.Context, string) (LifecycleResult, error) {
	return LifecycleResult{}, errors.New("this API connection is managed independently; model unload is unavailable")
}
func (a *ExternalAdapter) Status(context.Context) (Status, bool, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	connection, err := a.connectionLocked()
	if err != nil {
		return Status{Status: "unconfigured"}, false, nil
	}
	model, ok := connection.LookupModel(a.selected)
	if !ok {
		return Status{Status: "unconfigured"}, false, nil
	}
	return Status{Status: "configured", RuntimeBackend: string(connection.EffectiveProtocol()), ContextWindowTokens: model.ContextTokens, MaxOutputTokens: model.MaxOutputTokens, Model: ResidentModelStatus{ID: model.ID, State: "configured"}}, true, nil
}
func (a *ExternalAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return nil
}

var _ RuntimeControl = (*ExternalAdapter)(nil)
