package runtimecontrol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

const InceptionModelID = "mercury-3"

// ConfigureInception persists one fixed, documented provider route and applies
// the resulting native catalog revision. The caller must hold the whole-Agent
// idle configuration lease. It performs no network requests inside that lease.
// A changed credential gets a new connection ID, preserving existing chats'
// immutable credential binding. Credentials never enter catalog metadata.
func (a *ExternalAdapter) ConfigureInception(ctx context.Context, key string) (ExternalConfiguration, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || ctx.Err() != nil {
		return ExternalConfiguration{}, errors.New("Inception configuration is unavailable")
	}
	validKey := key != "" && len(key) <= 16<<10 && strings.TrimSpace(key) == key
	for _, char := range key {
		validKey = validKey && char >= 33 && char <= 126
	}
	if !validKey {
		return ExternalConfiguration{}, errors.New("Inception credential is invalid")
	}
	catalog, err := modelconnection.Load(a.path)
	if errors.Is(err, os.ErrNotExist) && a.fingerprint == "" {
		catalog = &modelconnection.Catalog{Version: modelconnection.Version, Connections: []modelconnection.Connection{}}
	} else if err != nil || catalog.Fingerprint() != a.fingerprint {
		return ExternalConfiguration{}, errors.New("API configuration revision changed")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return ExternalConfiguration{}, errors.New("Inception connection identity is unavailable")
	}
	connection := modelconnection.Connection{
		ID: "inception-" + hex.EncodeToString(token[:]), Kind: modelconnection.KindProviderAPI,
		Protocol: modelconnection.ProtocolOpenAICompatible, Name: "Inception",
		BaseURL: "https://api.inceptionlabs.ai/v1",
		Models:  []modelconnection.Model{{ID: InceptionModelID, DisplayName: "Mercury 3", SupportsTools: true, SupportsImages: false, ReasoningControl: "inception-effort"}},
	}
	stored := false
	for _, row := range catalog.Connections {
		if row.ServiceKey() != connection.ServiceKey() {
			continue
		}
		previousKey, keyErr := modelconnection.Credential(a.path, row.ID)
		if keyErr != nil {
			return ExternalConfiguration{}, errors.New("saved API credential is unavailable")
		}
		candidate := connection
		candidate.ID = row.ID
		if previousKey == key && externalEndpointIdentity(candidate) == externalEndpointIdentity(row) {
			connection.ID, stored = row.ID, true
			// Retain other explicitly configured models on an unchanged route.
			for _, model := range row.Models {
				if model.ID != InceptionModelID {
					connection.Models = append(connection.Models, model)
				}
			}
		}
		break
	}
	rows := make([]modelconnection.Connection, 0, len(catalog.Connections)+1)
	for _, row := range catalog.Connections {
		if row.ServiceKey() != connection.ServiceKey() {
			rows = append(rows, row)
		}
	}
	catalog.Connections = append(rows, connection)
	catalog.SelectedConnectionID, catalog.SelectedModelID = connection.ID, InceptionModelID
	if err := modelconnection.Validate(catalog); err != nil {
		return ExternalConfiguration{}, errors.New("Inception configuration is invalid")
	}
	for _, row := range catalog.Connections {
		if previous, exists := a.identities[row.ID]; exists && previous != externalEndpointIdentity(row) {
			return ExternalConfiguration{}, errors.New("API endpoint identity changed")
		}
	}
	if !stored {
		if err := modelconnection.SaveCredentialContext(ctx, a.path, connection.ID, key); err != nil {
			return ExternalConfiguration{}, errors.New("Inception credential could not be saved")
		}
	}
	if err := modelconnection.SaveContext(ctx, a.path, catalog); err != nil {
		if !stored {
			_ = modelconnection.DeleteCredential(a.path, connection.ID)
		}
		return ExternalConfiguration{}, errors.New("Inception configuration could not be saved")
	}
	// SaveContext's atomic rename is the commit point. Publish the already
	// validated exact revision through the same adapter operation used by reload;
	// no fallible operation follows that commit, so an error cannot report an
	// unapplied transaction after its catalog was persisted.
	a.publish(catalog, connection, InceptionModelID)
	return a.configurationLocked(), nil
}
