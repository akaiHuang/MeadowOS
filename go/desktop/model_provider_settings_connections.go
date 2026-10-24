//go:build !meadow_protected

package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/coreconnection"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

// API-only Desktop has its own explicit connection catalog. It never rewrites
// the diagnostic Core catalog or starts a local model as a settings side effect.
type desktopModelConnectionSettings struct {
	*desktopModelProviderSettings
	connections     map[string]desktopConnectionVerification
	discover        func(context.Context, modelconnection.Connection, string) ([]modelconnection.Model, error)
	localCores      func() ([]coreconnection.PairedConnection, error)
	localDetection  func(context.Context, []coreconnection.PairedConnection) coreconnection.LocalDetection
	managed         desktopManagedModelAccess
	connectionLease func(context.Context) (string, func(), error)
	reload          func(context.Context, string, *modelconnection.Catalog) error
}

type desktopConnectionVerification struct {
	connection        modelconnection.Connection
	replaces          string
	key               string
	useStored         bool
	managedAccountRef string
	fingerprint       string
	accountScope      string
	expires           time.Time
}

type desktopConnectionCapabilities struct {
	SupportsImages  *bool `json:"supportsImages"`
	ContextTokens   *int  `json:"contextTokens"`
	MaxOutputTokens *int  `json:"maxOutputTokens"`
}

func (c desktopConnectionCapabilities) apply(models []modelconnection.Model) error {
	for _, limit := range []*int{c.ContextTokens, c.MaxOutputTokens} {
		if limit != nil && (*limit < 1 || *limit > 8<<20) {
			return settingsError(400, "invalid_connection", "模型長度限制必須是有效的正整數")
		}
	}
	if c.ContextTokens != nil && c.MaxOutputTokens != nil && *c.MaxOutputTokens >= *c.ContextTokens {
		return settingsError(400, "invalid_connection", "輸出長度必須小於模型上下文長度")
	}
	for i := range models {
		if c.SupportsImages != nil {
			models[i].SupportsImages = *c.SupportsImages
		}
		if c.ContextTokens != nil {
			models[i].ContextTokens = *c.ContextTokens
		}
		if c.MaxOutputTokens != nil {
			models[i].MaxOutputTokens = *c.MaxOutputTokens
		}
		if models[i].ContextTokens > 0 && models[i].MaxOutputTokens >= models[i].ContextTokens {
			return settingsError(400, "invalid_connection", "輸出長度必須小於模型上下文長度")
		}
	}
	return nil
}

func newDesktopModelConnectionSettings(transport *daemontransport.Client, path string) *desktopModelConnectionSettings {
	base := newDesktopModelProviderSettings(transport)
	base.path = path
	settings := &desktopModelConnectionSettings{
		desktopModelProviderSettings: base,
		connections:                  make(map[string]desktopConnectionVerification),
		discover:                     modelconnection.DiscoverModels,
		localDetection:               coreconnection.DetectLocal,
		localCores: func() ([]coreconnection.PairedConnection, error) {
			root, err := coreconnection.DefaultRegistryRoot()
			if err != nil {
				return nil, err
			}
			return coreconnection.DiscoverLocal(root)
		},
	}
	if transport != nil {
		settings.connectionLease = func(ctx context.Context) (string, func(), error) {
			return acquireProviderConfigurationLeaseID(ctx, transport)
		}
	}
	return settings
}

func (s *desktopModelConnectionSettings) register(api *http.ServeMux) {
	api.Handle("GET /native/model-providers", s.native(http.HandlerFunc(s.listConnections)))
	api.Handle("POST /native/model-providers/verify", s.native(http.HandlerFunc(s.verifyConnection)))
	api.Handle("POST /native/model-providers/save", s.native(http.HandlerFunc(s.saveConnection)))
	api.Handle("POST /native/model-providers/select", s.native(http.HandlerFunc(s.selectConnection)))
	api.Handle("POST /native/model-providers/models", s.native(http.HandlerFunc(s.updateConnectionModels)))
	if !buildinfo.VMLab {
		api.Handle("POST /native/model-providers/pair", s.native(http.HandlerFunc(s.pairCore)))
		api.Handle("GET /native/model-providers/local-core", s.native(http.HandlerFunc(s.listLocalCores)))
		api.Handle("POST /native/model-providers/local-core/verify", s.native(http.HandlerFunc(s.verifyLocalCore)))
		api.Handle("GET /native/model-providers/managed-status", s.native(http.HandlerFunc(s.managedStatus)))
		api.Handle("POST /native/model-providers/managed-connect", s.native(http.HandlerFunc(s.connectManaged)))
	}
}

func (s *desktopModelConnectionSettings) readConnections() (*modelconnection.Catalog, error) {
	if s.path == "" {
		return nil, settingsError(409, "model_provider_disabled", "尚未設定模型連線儲存位置")
	}
	catalog, err := modelconnection.Load(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return &modelconnection.Catalog{Version: modelconnection.Version, Connections: []modelconnection.Connection{}}, nil
	}
	if err != nil {
		return nil, settingsError(503, "catalog_unavailable", "無法安全讀取模型連線設定")
	}
	return catalog, nil
}

func connectionCatalogFingerprint(catalog *modelconnection.Catalog) string {
	if len(catalog.Connections) == 0 {
		return "unconfigured"
	}
	return catalog.Fingerprint()
}

func (s *desktopModelConnectionSettings) listConnections(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.readConnections()
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	connections := []map[string]any{}
	models := []map[string]any{}
	for _, connection := range catalog.Connections {
		status := "none"
		if connection.Kind == modelconnection.KindManagedAPI {
			status = "managed-account"
		} else {
			key, keyErr := modelconnection.Credential(s.path, connection.ID)
			if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
				status = "unavailable"
			} else if key != "" {
				status = "stored"
			}
		}
		rows := []map[string]any{}
		for _, model := range connection.Models {
			name := model.DisplayName
			if name == "" {
				name = model.ID
			}
			row := map[string]any{"id": model.ID, "name": name, "enabled": !model.Disabled, "supportsImages": model.SupportsImages, "contextTokens": model.ContextTokens, "maxOutputTokens": model.MaxOutputTokens}
			rows = append(rows, row)
			if !model.Disabled {
				models = append(models, map[string]any{"id": model.ID, "name": name, "connectionId": connection.ID, "provider": connection.Name})
			}
		}
		connections = append(connections, map[string]any{
			"id": connection.ID, "kind": connection.Kind, "name": connection.Name,
			"protocol": connection.EffectiveProtocol(),
			"baseUrl":  connection.BaseURL, "configured": status == "stored", "credentialStatus": status,
			"models": rows, "selected": catalog.SelectedConnectionID == connection.ID,
		})
	}
	writeJSON(w, map[string]any{
		"enabled": true, "mode": "connections", "connections": connections, "models": models,
		"selectedConnectionId": catalog.SelectedConnectionID, "selectedModelId": catalog.SelectedModelID,
		"kinds": desktopConnectionKinds(),
	})
}

func validConnectionLabel(value string) bool {
	if value == "" || utf8.RuneCountInString(value) > 120 || !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool { return r < 32 || r == 127 })
}

func (s *desktopModelConnectionSettings) verifyConnection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ConnectionID string                   `json:"connectionId"`
		Kind         modelconnection.Kind     `json:"kind"`
		Name         string                   `json:"name"`
		BaseURL      string                   `json:"baseUrl"`
		APIKey       string                   `json:"apiKey"`
		Protocol     modelconnection.Protocol `json:"protocol"`
		UseStoredKey bool                     `json:"useStoredKey"`
		desktopConnectionCapabilities
	}
	if err := decodeProviderSettings(r, &body); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	if body.Kind == modelconnection.KindManagedAPI {
		writeProviderSettingsError(w, settingsError(400, "managed_connection_required", "請使用 Meadow 帳號連接 Meadow API"))
		return
	}
	catalog, err := s.readConnections()
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	name := strings.TrimSpace(body.Name)
	if !validConnectionLabel(name) || modelconnection.ValidateBaseURL(body.Kind, baseURL) != nil {
		writeProviderSettingsError(w, settingsError(400, "invalid_connection", "請確認連線名稱、類型與 API 網址"))
		return
	}
	if err := body.desktopConnectionCapabilities.apply(nil); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key != "" && !validProviderSecret(key) {
		writeProviderSettingsError(w, settingsError(400, "invalid_api_key", "API key 格式無效"))
		return
	}
	existing, exists := catalog.LookupConnection(body.ConnectionID)
	if body.ConnectionID != "" && !exists {
		writeProviderSettingsError(w, settingsError(409, "catalog_changed", "連線設定已變更，請重新載入"))
		return
	}
	if body.UseStoredKey {
		// A previously saved secret is bound to one exact origin and path. Even
		// a user-edited endpoint requires entering a fresh credential explicitly.
		if key != "" || !exists || existing.Kind != body.Kind || strings.TrimRight(existing.BaseURL, "/") != baseURL || existing.EffectiveProtocol() != (modelconnection.Connection{Protocol: body.Protocol}).EffectiveProtocol() {
			writeProviderSettingsError(w, settingsError(400, "credential_endpoint_changed", "變更 API 網址後，請重新輸入金鑰"))
			return
		}
		key, err = modelconnection.Credential(s.path, existing.ID)
		if err != nil || !validProviderSecret(key) {
			writeProviderSettingsError(w, settingsError(400, "credential_unavailable", "無法讀取已儲存金鑰，請重新輸入"))
			return
		}
	}
	id, err := randomProviderToken()
	if err != nil {
		writeProviderSettingsError(w, settingsError(503, "verification_unavailable", "無法建立模型驗證"))
		return
	}
	connection := modelconnection.Connection{ID: "connection-" + id, Kind: body.Kind, Name: name, BaseURL: baseURL, Protocol: body.Protocol}
	if err := modelconnection.Validate(&modelconnection.Catalog{Version: modelconnection.Version, Connections: []modelconnection.Connection{connection}}); err != nil {
		writeProviderSettingsError(w, settingsError(400, "invalid_connection", "此連線類型不支援選擇的 API 協定"))
		return
	}
	if body.UseStoredKey {
		connection.ID = existing.ID
	}
	if exists && existing.Kind == body.Kind && strings.TrimRight(existing.BaseURL, "/") == baseURL && existing.EffectiveProtocol() == connection.EffectiveProtocol() {
		connection.Models = append([]modelconnection.Model(nil), existing.Models...)
	}
	models, err := s.discover(r.Context(), connection, key)
	if err != nil {
		// The upstream may echo a secret or a private URL. Only our fixed public
		// error leaves this native surface, never the upstream response string.
		writeProviderSettingsError(w, settingsError(502, "provider_unavailable", "無法取得模型清單，請確認 API 網址、金鑰與服務狀態"))
		return
	}
	connection.Models = append([]modelconnection.Model(nil), models...)
	for i := range connection.Models {
		connection.Models[i].Disabled = false
	}
	if err := body.desktopConnectionCapabilities.apply(connection.Models); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	if len(models) == 0 {
		writeProviderSettingsError(w, settingsError(422, "no_supported_models", "此服務尚未提供可用模型"))
		return
	}
	verification := desktopConnectionVerification{
		connection: connection, replaces: body.ConnectionID, key: key, useStored: body.UseStoredKey,
		fingerprint: connectionCatalogFingerprint(catalog), expires: s.now().Add(10 * time.Minute),
	}
	s.rememberConnection(w, r, verification)
}

func (s *desktopModelConnectionSettings) rememberConnection(w http.ResponseWriter, r *http.Request, verification desktopConnectionVerification) {
	verification.accountScope = desktopAccountScope(r.Context())
	remaining := verification.expires.Sub(s.now())
	if remaining <= 0 {
		writeProviderSettingsError(w, settingsError(410, "verification_expired", "驗證已過期，請重新驗證連線"))
		return
	}
	seconds := min(600, int((remaining+time.Second-1)/time.Second))
	token, err := randomProviderToken()
	if err != nil {
		writeProviderSettingsError(w, settingsError(503, "verification_unavailable", "無法建立模型驗證"))
		return
	}
	s.mu.Lock()
	for id, old := range s.connections {
		if !old.expires.After(s.now()) {
			delete(s.connections, id)
		}
	}
	if len(s.connections) >= 8 {
		s.mu.Unlock()
		writeProviderSettingsError(w, settingsError(429, "too_many_verifications", "請稍後再驗證模型連線"))
		return
	}
	s.connections[token] = verification
	s.mu.Unlock()
	time.AfterFunc(10*time.Minute, func() { s.mu.Lock(); delete(s.connections, token); s.mu.Unlock() })
	rows := []map[string]any{}
	for _, model := range verification.connection.Models {
		name := model.DisplayName
		if name == "" {
			name = model.ID
		}
		rows = append(rows, map[string]any{"id": model.ID, "name": name, "configured": false, "supportsImages": model.SupportsImages, "contextTokens": model.ContextTokens, "maxOutputTokens": model.MaxOutputTokens})
	}
	writeJSON(w, map[string]any{"verificationId": token, "models": rows, "expiresInSeconds": seconds,
		"connection": map[string]any{"kind": verification.connection.Kind, "name": verification.connection.Name, "baseUrl": verification.connection.BaseURL, "protocol": verification.connection.EffectiveProtocol()}})
}

type desktopConnectionLeaseKey struct{}

func (s *desktopModelConnectionSettings) withConnectionLease(ctx context.Context, commit func(context.Context) error) error {
	if !s.lifecycle.TryLock() {
		return settingsError(409, "model_runtime_busy", "仍有工作正在進行，請完成後再儲存")
	}
	defer s.lifecycle.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var release func()
	var err error
	if s.connectionLease != nil {
		var leaseID string
		leaseID, release, err = s.connectionLease(ctx)
		ctx = context.WithValue(ctx, desktopConnectionLeaseKey{}, leaseID)
	} else {
		release, err = s.lease(ctx)
	}
	if err != nil {
		return err
	}
	defer release()
	return commit(ctx)
}

func (s *desktopModelConnectionSettings) saveConnection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VerificationID string   `json:"verificationId"`
		ModelIDs       []string `json:"modelIds"`
	}
	if err := decodeProviderSettings(r, &body); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	verification, exists := s.connections[body.VerificationID]
	if !exists || !verification.expires.After(s.now()) || verification.accountScope != desktopAccountScope(r.Context()) {
		delete(s.connections, body.VerificationID)
		writeProviderSettingsError(w, settingsError(410, "verification_expired", "驗證已過期，請重新驗證連線"))
		return
	}
	if len(body.ModelIDs) == 0 || len(body.ModelIDs) > 32 {
		writeProviderSettingsError(w, settingsError(400, "invalid_models", "請選擇 1 至 32 個模型"))
		return
	}
	selected := []modelconnection.Model{}
	seen := map[string]bool{}
	for _, id := range body.ModelIDs {
		model, found := verification.connection.LookupModel(id)
		if !found || seen[id] {
			writeProviderSettingsError(w, settingsError(400, "invalid_models", "請從已驗證清單選擇模型"))
			return
		}
		seen[id] = true
		selected = append(selected, model)
	}
	for i := range verification.connection.Models {
		verification.connection.Models[i].Disabled = !seen[verification.connection.Models[i].ID]
	}
	activation := "restart-required"
	err := s.withConnectionLease(r.Context(), func(ctx context.Context) error {
		catalog, err := s.readConnections()
		if err != nil {
			return err
		}
		if connectionCatalogFingerprint(catalog) != verification.fingerprint {
			return settingsError(409, "catalog_changed", "模型設定已變更，請重新驗證")
		}
		if verification.useStored {
			key, err := modelconnection.Credential(s.path, verification.replaces)
			if err != nil || key != verification.key {
				return settingsError(410, "verification_expired", "已儲存金鑰已變更，請重新驗證")
			}
		}
		if verification.managedAccountRef != "" {
			if s.managed == nil {
				return settingsError(503, "managed_api_unavailable", "Meadow API 目前尚未開放")
			}
			status, err := s.managed.ModelAccessStatus(ctx)
			if err != nil {
				return settingsError(503, "managed_api_unavailable", "無法確認 Meadow API 權限，請稍後再試")
			}
			if !status.Configured || !status.Allowed || status.AccountRef != verification.managedAccountRef {
				return settingsError(410, "verification_expired", "Meadow 帳號已變更，請重新連接")
			}
		}
		// Upsert a verified route instead of generating another service row.
		credentialAlreadyStored := verification.useStored
		for _, row := range catalog.Connections {
			if row.ServiceKey() == verification.connection.ServiceKey() {
				previousKey, keyErr := modelconnection.Credential(s.path, row.ID)
				if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
					return settingsError(503, "credential_unavailable", "無法安全讀取已儲存連線")
				}
				if previousKey == verification.key {
					verification.connection.ID = row.ID
					credentialAlreadyStored = true
				}
				// A changed secret gets a new immutable ID. Remove the old
				// visible row, but never replace a pinned chat's key in place.
				break
			}
		}
		rows := make([]modelconnection.Connection, 0, len(catalog.Connections)+1)
		for _, row := range catalog.Connections {
			if row.ID != verification.replaces && row.ID != verification.connection.ID && row.ServiceKey() != verification.connection.ServiceKey() {
				rows = append(rows, row)
			}
		}
		catalog.Connections = append(rows, verification.connection)
		catalog.SelectedConnectionID = verification.connection.ID
		catalog.SelectedModelID = selected[0].ID
		if err := modelconnection.Validate(catalog); err != nil {
			return settingsError(400, "invalid_connection", "模型連線設定無效或已達上限")
		}
		if !credentialAlreadyStored && verification.managedAccountRef == "" {
			if err := modelconnection.SaveCredentialContext(ctx, s.path, verification.connection.ID, verification.key); err != nil {
				return settingsError(503, "save_failed", "無法安全儲存連線金鑰")
			}
		}
		if err := modelconnection.SaveContext(ctx, s.path, catalog); err != nil {
			if !credentialAlreadyStored && verification.managedAccountRef == "" {
				_ = modelconnection.DeleteCredential(s.path, verification.connection.ID)
			}
			return settingsError(503, "save_failed", "無法儲存模型連線設定")
		}
		if s.reload != nil {
			leaseID, _ := ctx.Value(desktopConnectionLeaseKey{}).(string)
			if s.reload(ctx, leaseID, catalog) == nil {
				activation = "applied"
			}
		}
		return nil
	})
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	delete(s.connections, body.VerificationID)
	writeJSON(w, map[string]any{"saved": true, "connectionId": verification.connection.ID, "modelIds": body.ModelIDs, "activation": activation})
}

// Model visibility changes use only this connection's verified catalog. A
// checkbox never discovers models, asks for a key again, or starts Core.
func (s *desktopModelConnectionSettings) updateConnectionModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ConnectionID     string   `json:"connectionId"`
		ModelIDs         []string `json:"modelIds"`
		PreferredModelID string   `json:"preferredModelId"`
	}
	if err := decodeProviderSettings(r, &body); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	activation := "restart-required"
	err := s.withConnectionLease(r.Context(), func(ctx context.Context) error {
		catalog, err := s.readConnections()
		if err != nil {
			return err
		}
		connection, exists := catalog.LookupConnection(body.ConnectionID)
		if !exists || len(body.ModelIDs) > 512 {
			return settingsError(400, "invalid_models", "請選擇已驗證的模型")
		}
		known, enabled := map[string]bool{}, map[string]bool{}
		for _, model := range connection.Models {
			known[model.ID] = true
		}
		for _, id := range body.ModelIDs {
			if !known[id] || enabled[id] {
				return settingsError(400, "invalid_models", "請選擇已驗證的模型")
			}
			enabled[id] = true
		}
		if body.PreferredModelID != "" && !enabled[body.PreferredModelID] {
			return settingsError(400, "invalid_models", "預設模型必須已勾選")
		}
		if connection.Kind == modelconnection.KindManagedAPI && len(enabled) > 0 {
			if s.managed == nil {
				return settingsError(503, "managed_api_unavailable", "Meadow API 目前尚未開放")
			}
			status, err := s.managed.ModelAccessStatus(ctx)
			if err != nil || !status.Configured {
				return settingsError(503, "managed_api_unavailable", "無法確認 Meadow API 權限，請稍後再試")
			}
			if !status.Allowed || status.AccountRef != connection.AccountRef {
				return settingsError(403, "managed_api_not_allowed", "請使用目前的 Meadow 帳號重新連接 API")
			}
		}
		for i := range catalog.Connections {
			if catalog.Connections[i].ID != body.ConnectionID {
				continue
			}
			for j := range catalog.Connections[i].Models {
				model := &catalog.Connections[i].Models[j]
				model.Disabled = !enabled[model.ID]
			}
		}
		if body.PreferredModelID != "" {
			catalog.SelectedConnectionID, catalog.SelectedModelID = body.ConnectionID, body.PreferredModelID
		}
		if _, _, err := catalog.Resolve(); err != nil {
			catalog.SelectedConnectionID, catalog.SelectedModelID = "", ""
			for _, row := range catalog.Connections {
				// Only an explicitly checked managed connection has passed the
				// current account's access check above. Do not select another
				// account's stale managed route as an automatic fallback.
				if row.Kind == modelconnection.KindManagedAPI && row.ID != body.ConnectionID {
					continue
				}
				for _, model := range row.Models {
					if !model.Disabled {
						catalog.SelectedConnectionID, catalog.SelectedModelID = row.ID, model.ID
						break
					}
				}
				if catalog.SelectedConnectionID != "" {
					break
				}
			}
		}
		if err := modelconnection.SaveContext(ctx, s.path, catalog); err != nil {
			return settingsError(503, "save_failed", "無法儲存模型選擇")
		}
		if s.reload != nil {
			leaseID, _ := ctx.Value(desktopConnectionLeaseKey{}).(string)
			if s.reload(ctx, leaseID, catalog) == nil {
				activation = "applied"
			}
		}
		return nil
	})
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	writeJSON(w, map[string]any{"saved": true, "connectionId": body.ConnectionID, "activation": activation})
}

func (s *desktopModelConnectionSettings) selectConnection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ConnectionID string `json:"connectionId"`
		ModelID      string `json:"modelId"`
	}
	if err := decodeProviderSettings(r, &body); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	activation := "restart-required"
	err := s.withConnectionLease(r.Context(), func(ctx context.Context) error {
		catalog, err := s.readConnections()
		if err != nil {
			return err
		}
		connection, exists := catalog.LookupConnection(body.ConnectionID)
		if _, found := connection.LookupModel(body.ModelID); !exists || !found {
			return settingsError(400, "invalid_models", "請選擇已儲存的連線與模型")
		}
		if connection.Kind == modelconnection.KindManagedAPI {
			if s.managed == nil {
				return settingsError(503, "managed_api_unavailable", "Meadow API 目前尚未開放")
			}
			status, err := s.managed.ModelAccessStatus(ctx)
			if err != nil || !status.Configured {
				return settingsError(503, "managed_api_unavailable", "無法確認 Meadow API 權限，請稍後再試")
			}
			if !status.Allowed || status.AccountRef != connection.AccountRef {
				return settingsError(403, "managed_api_not_allowed", "請使用目前的 Meadow 帳號重新連接 API")
			}
		}
		catalog.SelectedConnectionID, catalog.SelectedModelID = body.ConnectionID, body.ModelID
		if err := modelconnection.SaveContext(ctx, s.path, catalog); err != nil {
			return settingsError(503, "save_failed", "無法儲存預設模型")
		}
		if s.reload != nil {
			leaseID, _ := ctx.Value(desktopConnectionLeaseKey{}).(string)
			if s.reload(ctx, leaseID, catalog) == nil {
				activation = "applied"
			}
		}
		return nil
	})
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	writeJSON(w, map[string]any{"saved": true, "connectionId": body.ConnectionID, "modelId": body.ModelID, "activation": activation})
}
