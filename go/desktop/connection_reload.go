//go:build !meadow_protected

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

// Both processes acknowledge the exact persisted catalog while the whole-Agent
// lease blocks admissions. A lost acknowledgement leaves the file intact and
// reports restart-required; rolling it back could undo an already applied save.
func (s *desktopModelConnectionSettings) bindRuntime(runtime *desktopConnectionRuntime, transport *daemontransport.Client, selected func(string)) {
	s.reload = func(ctx context.Context, leaseID string, catalog *modelconnection.Catalog) error {
		if transport == nil || leaseID == "" {
			return errors.New("connection reload unavailable")
		}
		fingerprint := catalog.Fingerprint()
		expectedModel := catalog.SelectedModelID
		if catalog.SelectedConnectionID == "" {
			expectedModel = "unconfigured"
		}
		request, err := transport.NewRequest(ctx, http.MethodPost, "/agent/connections/reload", map[string]string{"leaseId": leaseID, "expectedFingerprint": fingerprint})
		if err != nil {
			return errors.New("connection reload unavailable")
		}
		response, err := transport.HTTP.Do(request)
		if err != nil {
			return errors.New("connection reload unavailable")
		}
		defer response.Body.Close()
		var result struct {
			Applied      bool   `json:"applied"`
			ConnectionID string `json:"connectionId"`
			ModelID      string `json:"modelId"`
			Fingerprint  string `json:"fingerprint"`
		}
		decoder := json.NewDecoder(io.LimitReader(response.Body, 8193))
		if response.StatusCode != http.StatusOK || decoder.Decode(&result) != nil || !result.Applied || result.Fingerprint != fingerprint || result.ConnectionID != catalog.SelectedConnectionID || result.ModelID != expectedModel {
			return errors.New("connection reload acknowledgement is invalid")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return errors.New("connection reload acknowledgement is invalid")
		}
		if err := runtime.applyCatalog(catalog, transport); err != nil {
			return err
		}
		if selected != nil {
			selected(result.ModelID)
		}
		return nil
	}
}

func (runtime *desktopConnectionRuntime) applyCatalog(catalog *modelconnection.Catalog, transport *daemontransport.Client) error {
	// Copy through the validated metadata representation, so later edits to a
	// settings request cannot mutate the snapshot used by concurrent status reads.
	data, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	var snapshot modelconnection.Catalog
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	if err := modelconnection.Validate(&snapshot); err != nil {
		return err
	}
	var connection modelconnection.Connection
	modelID := "unconfigured"
	if snapshot.SelectedConnectionID != "" {
		resolved, model, err := snapshot.Resolve()
		if err != nil {
			return err
		}
		connection, modelID = resolved, model.ID
	}
	// Publish catalog and cache invalidation as one boundary; old flights cannot
	// report a previous provider after the new snapshot becomes visible.
	runtime.cache.mu.Lock()
	defer runtime.cache.mu.Unlock()
	runtime.mu.Lock()
	runtime.catalog, runtime.connection, runtime.modelID = &snapshot, connection, modelID
	runtime.discover = desktopAgentModelDiscovery(transport, connection)
	runtime.mu.Unlock()
	runtime.cache.reset("")
	return nil
}
