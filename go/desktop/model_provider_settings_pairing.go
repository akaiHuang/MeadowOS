//go:build !meadow_protected

package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/coreconnection"
	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

func (s *desktopModelConnectionSettings) pairCore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Invitation string `json:"invitation"`
		desktopConnectionCapabilities
	}
	// The invitation document is bounded to 32 KiB by coreconnection; allow
	// its JSON string representation in this outer native request as well.
	if err := decodeProviderSettingsBounded(r, &body, 64<<10); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	// A failed read must not consume a single-use remote invitation.
	if _, err := s.readConnections(); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	if err := body.desktopConnectionCapabilities.apply(nil); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	invitation, err := coreconnection.ParseInvitation([]byte(body.Invitation), s.now())
	if err != nil {
		writeProviderSettingsError(w, settingsError(400, "invalid_core_invitation", "Meadow Core 連線邀請無效或已過期，請重新產生邀請"))
		return
	}
	paired, err := coreconnection.Exchange(r.Context(), s.client, invitation, "Meadow Desktop")
	if err != nil {
		writeProviderSettingsError(w, settingsError(502, "core_pairing_failed", "無法完成 Meadow Core 配對，請確認網址與邀請期限"))
		return
	}
	s.verifyPairedCore(w, r, paired, body.desktopConnectionCapabilities)
}

func (s *desktopModelConnectionSettings) listLocalCores(w http.ResponseWriter, r *http.Request) {
	connections, err := s.localCores()
	if err != nil {
		writeProviderSettingsError(w, settingsError(503, "local_core_unavailable", "無法安全讀取本機 Meadow Core 連線，請改用連線邀請"))
		return
	}
	rows := []map[string]string{}
	for _, connection := range connections {
		rows = append(rows, map[string]string{"coreId": connection.CoreID, "name": "Meadow Core", "baseUrl": connection.BaseURL})
	}
	var detection coreconnection.LocalDetection
	if s.localDetection != nil {
		detection = s.localDetection(r.Context(), connections)
	}
	writeJSON(w, map[string]any{"cores": rows, "available": true, "detection": detection})
}

func (s *desktopModelConnectionSettings) verifyLocalCore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CoreID string `json:"coreId"`
		desktopConnectionCapabilities
	}
	if err := decodeProviderSettings(r, &body); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	if body.CoreID == "" || len(body.CoreID) > 128 || strings.TrimSpace(body.CoreID) != body.CoreID {
		writeProviderSettingsError(w, settingsError(400, "invalid_connection", "請選擇偵測到的 Meadow Core"))
		return
	}
	connections, err := s.localCores()
	if err != nil {
		writeProviderSettingsError(w, settingsError(503, "local_core_unavailable", "無法安全讀取本機 Meadow Core 連線"))
		return
	}
	var selected coreconnection.PairedConnection
	count := 0
	for _, connection := range connections {
		if connection.CoreID == body.CoreID {
			selected = connection
			count++
		}
	}
	if count != 1 {
		writeProviderSettingsError(w, settingsError(409, "local_core_changed", "本機 Meadow Core 連線已變更，請重新偵測"))
		return
	}
	s.verifyPairedCore(w, r, selected, body.desktopConnectionCapabilities)
}

func (s *desktopModelConnectionSettings) verifyPairedCore(w http.ResponseWriter, r *http.Request, paired coreconnection.PairedConnection, capabilities desktopConnectionCapabilities) {
	if err := capabilities.apply(nil); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	catalog, err := s.readConnections()
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	if err := coreconnection.Probe(r.Context(), s.client, paired); err != nil {
		writeProviderSettingsError(w, settingsError(502, "core_identity_mismatch", "無法確認 Meadow Core 身分與連線權限，請重新配對"))
		return
	}
	id, err := randomProviderToken()
	if err != nil {
		writeProviderSettingsError(w, settingsError(503, "verification_unavailable", "無法建立模型驗證"))
		return
	}
	connection := modelconnection.Connection{ID: "connection-" + id, Kind: modelconnection.KindMeadowCore, Name: "Meadow Core", BaseURL: paired.BaseURL}
	models, err := s.discover(r.Context(), connection, paired.APIKey)
	if err != nil || len(models) == 0 {
		writeProviderSettingsError(w, settingsError(502, "provider_unavailable", "已配對 Meadow Core，但無法取得可用模型，請確認 Core 服務狀態"))
		return
	}
	connection.Models = models
	if err := capabilities.apply(connection.Models); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	s.rememberConnection(w, r, desktopConnectionVerification{
		connection: connection, key: paired.APIKey,
		fingerprint: connectionCatalogFingerprint(catalog), expires: s.now().Add(10 * time.Minute),
	})
}
