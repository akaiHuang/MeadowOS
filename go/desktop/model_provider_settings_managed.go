//go:build !meadow_protected

package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
	coreauth "github.com/hey-meadow/meadow-code/go/core/productauth"
)

// This transport exposes only native authorization DTOs. The Agent owns the
// scoped Meadow API key and refreshes it during inference; neither Desktop
// settings nor its browser receive, persist, or accept that key.
type desktopManagedModelAccess interface {
	ModelAccessStatus(context.Context) (coreauth.ModelAccessStatus, error)
	ModelAccessUsage(context.Context) (coreauth.ModelAccessUsage, error)
	ModelAccessProvision(context.Context) (coreauth.ModelAccessProvision, error)
}

func (s *desktopModelConnectionSettings) setManagedAccount(client desktopManagedModelAccess) {
	s.managed = client
}

func (s *desktopModelConnectionSettings) managedStatus(w http.ResponseWriter, r *http.Request) {
	disabled := map[string]any{"configured": false, "allowed": false, "provisioning": "not_provisioned", "budget": nil}
	if s.managed == nil {
		writeJSON(w, map[string]any{"available": false, "status": disabled})
		return
	}
	status, err := s.managed.ModelAccessStatus(r.Context())
	if err != nil {
		// Deployment may intentionally leave cloud API disabled. Preserve a
		// useful manual-API settings page and never manufacture a ready state.
		writeJSON(w, map[string]any{"available": false, "status": disabled, "error": map[string]string{"code": "managed_api_unavailable"}})
		return
	}
	result := map[string]any{"available": status.Configured, "status": status}
	if status.Configured {
		if usage, err := s.managed.ModelAccessUsage(r.Context()); err == nil {
			result["usage"] = usage
		} else {
			result["usageUnavailable"] = true
		}
	}
	writeJSON(w, result)
}

func (s *desktopModelConnectionSettings) connectManaged(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if err := decodeProviderSettings(r, &body); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	if s.managed == nil {
		writeProviderSettingsError(w, settingsError(503, "managed_api_unavailable", "Meadow API 目前尚未開放，可先連接自己的 API 或本地模型"))
		return
	}
	catalog, err := s.readConnections()
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	provision, err := s.managed.ModelAccessProvision(r.Context())
	if err != nil {
		var denied *coreauth.Error
		if errors.As(err, &denied) && denied.HTTPStatus == http.StatusForbidden {
			writeProviderSettingsError(w, settingsError(403, "managed_api_not_allowed", "目前帳號尚未開放 Meadow API，請確認訂閱或 API 額度"))
			return
		}
		writeProviderSettingsError(w, settingsError(503, "managed_api_unavailable", "無法連接 Meadow API，請稍後重試或確認帳號方案"))
		return
	}
	if strings.TrimSpace(provision.AccountRef) == "" || !provision.ExpiresAt.After(s.now()) || len(provision.Models) == 0 || len(provision.Models) > 512 {
		writeProviderSettingsError(w, settingsError(503, "managed_api_unavailable", "Meadow API 尚未提供有效的模型連線"))
		return
	}
	connection := modelconnection.Connection{
		ID:   modelconnection.ManagedConnectionID(provision.AccountRef, provision.APIBase),
		Kind: modelconnection.KindManagedAPI, Name: "Meadow API", BaseURL: provision.APIBase, AccountRef: provision.AccountRef,
	}
	// These capabilities are issued by the native account service from server
	// policy. Model aliases and user-editable settings never infer or widen them.
	capabilities := make(map[string]modelconnection.Model, len(provision.ModelCatalog))
	for _, declared := range provision.ModelCatalog {
		if _, duplicate := capabilities[declared.ID]; duplicate {
			writeProviderSettingsError(w, settingsError(503, "managed_api_unavailable", "Meadow API 模型能力設定無效"))
			return
		}
		capabilities[declared.ID] = modelconnection.Model{
			ID: declared.ID, DisplayName: declared.ID,
			SupportsImages: declared.SupportsImages, SupportsTools: declared.SupportsTools,
			ContextTokens: declared.ContextTokens, MaxOutputTokens: declared.MaxOutputTokens,
		}
	}
	for _, id := range provision.Models {
		model, declared := capabilities[id]
		if !declared {
			model = modelconnection.Model{ID: id, DisplayName: id}
		}
		connection.Models = append(connection.Models, model)
		delete(capabilities, id)
	}
	if len(capabilities) != 0 {
		writeProviderSettingsError(w, settingsError(503, "managed_api_unavailable", "Meadow API 模型能力不符合帳號授權"))
		return
	}
	if err := modelconnection.Validate(&modelconnection.Catalog{Version: modelconnection.Version, Connections: []modelconnection.Connection{connection}}); err != nil {
		writeProviderSettingsError(w, settingsError(503, "managed_api_unavailable", "Meadow API 模型設定無效"))
		return
	}
	expires := s.now().Add(10 * time.Minute)
	if provision.ExpiresAt.Before(expires) {
		expires = provision.ExpiresAt
	}
	replaces := ""
	if _, exists := catalog.LookupConnection(connection.ID); exists {
		replaces = connection.ID
	}
	s.rememberConnection(w, r, desktopConnectionVerification{
		connection: connection, replaces: replaces, managedAccountRef: provision.AccountRef,
		fingerprint: connectionCatalogFingerprint(catalog), expires: expires,
	})
}
