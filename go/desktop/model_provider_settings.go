//go:build !meadow_protected

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/upstreamconfig"
)

// This surface performs credential validation/model discovery only. Completions
// and tools continue through Core, with its existing model activation contract.
type desktopModelProvider struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	URL           string `json:"-"`
	DiscoveryPath string `json:"-"`
}

var desktopModelProviders = []desktopModelProvider{
	{"deepseek", "DeepSeek", "https://api.deepseek.com", "/models"},
	{"xai", "xAI / Grok", "https://api.x.ai/v1", "/language-models"},
}

type providerSettingsError struct {
	status        int
	code, message string
}

func (e *providerSettingsError) Error() string { return e.message }
func settingsError(status int, code, message string) *providerSettingsError {
	return &providerSettingsError{status, code, message}
}

type configuredProviderProfile struct {
	upstreamconfig.Profile
	APIKeyFile string `json:"api_key_file,omitempty"`
}
type configuredProviderCatalog struct {
	DefaultModel string                      `json:"default_model"`
	Profiles     []configuredProviderProfile `json:"profiles"`
}
type discoveredProviderModel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Context    int    `json:"-"`
	Images     bool   `json:"-"`
}
type modelProviderVerification struct {
	provider  desktopModelProvider
	key       string
	keyPath   string
	useStored bool
	models    []discoveredProviderModel
	expires   time.Time
}
type desktopModelProviderSettings struct {
	mu        sync.Mutex
	lifecycle sync.RWMutex
	path      string
	client    *http.Client
	lease     func(context.Context) (func(), error)
	verified  map[string]modelProviderVerification
	now       func() time.Time
}

func newDesktopModelProviderSettings(transport *daemontransport.Client) *desktopModelProviderSettings {
	path := ""
	if desktopUsesUpstreamBackend() {
		path = strings.TrimSpace(os.Getenv("MEADOW_CORE_UPSTREAM_PROFILES_FILE"))
	}
	return &desktopModelProviderSettings{
		path: path, verified: map[string]modelProviderVerification{}, now: time.Now,
		client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		lease:  func(ctx context.Context) (func(), error) { return acquireProviderConfigurationLease(ctx, transport) },
	}
}

func (s *desktopModelProviderSettings) register(api *http.ServeMux) {
	api.Handle("GET /native/model-providers", s.native(http.HandlerFunc(s.list)))
	api.Handle("POST /native/model-providers/verify", s.native(http.HandlerFunc(s.verify)))
	api.Handle("POST /native/model-providers/save", s.native(http.HandlerFunc(s.save)))
}

// Wails on macOS supplies this reserved in-process address; this is not a TCP
// credential endpoint. The custom header and JSON content type exclude forms.
// Remote/opaque document origins cannot use this administrative native surface.
func (s *desktopModelProviderSettings) native(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		origin := r.Header.Get("Origin")
		if r.RemoteAddr != "192.0.2.1:1234" || r.Host != "localhost" || r.Header.Get("X-Meadow-Settings") != "1" || (origin != "" && origin != "wails://localhost") || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeProviderSettingsError(w, settingsError(403, "native_settings_only", "請從 Meadow Desktop 設定開啟模型管理"))
			return
		}
		if r.Method == http.MethodPost {
			media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || media != "application/json" {
				writeProviderSettingsError(w, settingsError(400, "invalid_request", "請重新開啟模型設定後再試"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Serialize local Core preflight/activation with the brief catalog transaction.
// The Agent lease separately gates actual running turns and queued admissions.
func (s *desktopModelProviderSettings) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		guarded := path == "/model/status" || path == "/model/list" || path == "/native/load-model" || path == "/agent/model" || (r.Method == http.MethodPost && strings.HasPrefix(path, "/session/") && strings.HasSuffix(path, "/prompt"))
		if guarded {
			s.lifecycle.RLock()
			defer s.lifecycle.RUnlock()
		}
		next.ServeHTTP(w, r)
	})
}

func (s *desktopModelProviderSettings) readCatalog() (configuredProviderCatalog, error) {
	var catalog configuredProviderCatalog
	if s.path == "" {
		return catalog, settingsError(409, "model_provider_disabled", "目前啟動方式未使用多模型設定")
	}
	validated, err := upstreamconfig.Load(s.path)
	if err != nil {
		return catalog, settingsError(503, "catalog_unavailable", "無法讀取模型設定，請檢查設定檔權限")
	}
	data, err := readPrivateProviderFile(s.path, 256<<10)
	if err != nil || json.Unmarshal(data, &catalog) != nil {
		return catalog, settingsError(503, "catalog_unavailable", "無法讀取模型設定")
	}
	metadata := &upstreamconfig.Catalog{DefaultModel: catalog.DefaultModel}
	for _, p := range catalog.Profiles {
		metadata.Profiles = append(metadata.Profiles, p.Profile)
	}
	// The existing loader normalizes optional labels; compare normalized rows.
	for i, p := range metadata.Profiles {
		if p.DisplayName == "" {
			metadata.Profiles[i].DisplayName = p.Model
		}
		if p.Provider == "" {
			metadata.Profiles[i].Provider = p.Model
		}
		if p.APIFormat == "" {
			metadata.Profiles[i].APIFormat = "openai"
		}
		metadata.Profiles[i].URL = strings.TrimRight(p.URL, "/")
	}
	if metadata.Fingerprint() != validated.Fingerprint() {
		return catalog, settingsError(409, "catalog_changed", "模型設定已變更，請重新載入")
	}
	return catalog, nil
}
func providerMatches(p configuredProviderProfile, provider desktopModelProvider) bool {
	base := strings.TrimRight(p.URL, "/")
	// DeepSeek documents both its root and the OpenAI-compatible /v1 base.
	if provider.ID == "deepseek" {
		base = strings.TrimSuffix(base, "/v1")
	}
	return p.APIFormat == provider.ID && base == provider.URL
}
func providerKeyPath(c configuredProviderCatalog, p desktopModelProvider) (string, error) {
	path := ""
	for _, row := range c.Profiles {
		if providerMatches(row, p) && row.APIKeyFile != "" {
			if path != "" && path != row.APIKeyFile {
				return "", settingsError(409, "multiple_credentials", "此供應商有多組獨立金鑰，請先統一模型設定")
			}
			path = row.APIKeyFile
		}
	}
	return path, nil
}
func (s *desktopModelProviderSettings) list(w http.ResponseWriter, r *http.Request) {
	if s.path == "" {
		writeJSON(w, map[string]any{"enabled": false, "providers": []any{}, "models": []any{}})
		return
	}
	catalog, err := s.readCatalog()
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	providers := []map[string]any{}
	models := []map[string]any{}
	for _, p := range desktopModelProviders {
		path, pathErr := providerKeyPath(catalog, p)
		providers = append(providers, map[string]any{"id": p.ID, "name": p.Name, "configured": pathErr == nil && path != ""})
	}
	for _, p := range catalog.Profiles {
		providerID := ""
		for _, candidate := range desktopModelProviders {
			if providerMatches(p, candidate) {
				providerID = candidate.ID
				break
			}
		}
		models = append(models, map[string]any{"id": p.Model, "name": p.DisplayName, "provider": p.Provider, "providerId": providerID})
	}
	writeJSON(w, map[string]any{"enabled": true, "providers": providers, "models": models})
}
func decodeProviderSettings(r *http.Request, value any) error {
	return decodeProviderSettingsBounded(r, value, 16<<10)
}

func decodeProviderSettingsBounded(r *http.Request, value any, limit int64) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(data)) > limit || !utf8.Valid(data) {
		return settingsError(400, "invalid_request", "輸入內容無效")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		return settingsError(400, "invalid_request", "輸入內容無效")
	}
	return nil
}
func validProviderSecret(key string) bool {
	if key == "" || len(key) > 8192 {
		return false
	}
	for _, c := range key {
		if c < 33 || c == 127 {
			return false
		}
	}
	return true
}
func (s *desktopModelProviderSettings) verify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider     string `json:"provider"`
		APIKey       string `json:"apiKey"`
		UseStoredKey bool   `json:"useStoredKey"`
	}
	if err := decodeProviderSettings(r, &body); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	catalog, err := s.readCatalog()
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	var p desktopModelProvider
	for _, item := range desktopModelProviders {
		if item.ID == body.Provider {
			p = item
			break
		}
	}
	if p.ID == "" {
		writeProviderSettingsError(w, settingsError(400, "unsupported_provider", "請選擇支援的模型供應商"))
		return
	}
	key := strings.TrimSpace(body.APIKey)
	keyPath := ""
	if body.UseStoredKey {
		if key != "" {
			writeProviderSettingsError(w, settingsError(400, "invalid_request", "請選擇使用已儲存金鑰或輸入新金鑰"))
			return
		}
		keyPath, err = providerKeyPath(catalog, p)
		if err == nil {
			var data []byte
			data, err = readPrivateProviderFile(keyPath, 8192)
			key = strings.TrimSpace(string(data))
		}
	}
	if err != nil || !validProviderSecret(key) {
		writeProviderSettingsError(w, settingsError(400, "invalid_api_key", "請輸入有效 API key，或重新設定已儲存金鑰"))
		return
	}
	models, err := s.discover(r.Context(), p, key, catalog)
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	token, err := randomProviderToken()
	if err != nil {
		writeProviderSettingsError(w, settingsError(503, "verification_unavailable", "目前無法建立驗證，請稍後再試"))
		return
	}
	s.mu.Lock()
	now := s.now()
	for id, v := range s.verified {
		if !v.expires.After(now) {
			delete(s.verified, id)
		}
	}
	if len(s.verified) >= 8 {
		s.mu.Unlock()
		writeProviderSettingsError(w, settingsError(429, "too_many_verifications", "請稍後再驗證金鑰"))
		return
	}
	s.verified[token] = modelProviderVerification{p, key, keyPath, body.UseStoredKey, models, now.Add(10 * time.Minute)}
	s.mu.Unlock()
	// Drop this verification (including its key) even if the UI never sends again.
	time.AfterFunc(10*time.Minute, func() { s.mu.Lock(); delete(s.verified, token); s.mu.Unlock() })
	writeJSON(w, map[string]any{"verificationId": token, "models": models, "expiresInSeconds": 600})
}
func (s *desktopModelProviderSettings) discover(ctx context.Context, p desktopModelProvider, key string, catalog configuredProviderCatalog) ([]discoveredProviderModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+p.DiscoveryPath, nil)
	if err != nil {
		return nil, settingsError(502, "provider_unavailable", "供應商連線失敗")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return nil, settingsError(502, "provider_unavailable", "無法連線至供應商，請稍後再試")
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return nil, settingsError(401, "invalid_api_key", "供應商拒絕此 API key，請檢查金鑰與權限")
	}
	if response.StatusCode == 429 {
		return nil, settingsError(429, "provider_rate_limited", "供應商暫時限制請求，請稍後再試")
	}
	if response.StatusCode != 200 {
		return nil, settingsError(502, "provider_unavailable", "供應商未能提供模型清單，請稍後再試")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, settingsError(502, "invalid_provider_response", "供應商模型清單格式不支援")
	}
	type row struct {
		ID      string   `json:"id"`
		Context int      `json:"context_length"`
		Inputs  []string `json:"input_modalities"`
		Outputs []string `json:"output_modalities"`
	}
	var payload struct {
		Data   []row `json:"data"`
		Models []row `json:"models"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, settingsError(502, "invalid_provider_response", "供應商模型清單格式不支援")
	}
	rows := payload.Data
	if p.ID == "xai" {
		rows = payload.Models
	}
	if len(rows) > 512 {
		return nil, settingsError(502, "invalid_provider_response", "供應商模型清單超過上限")
	}
	out := []discoveredProviderModel{}
	seen := map[string]bool{}
	for _, row := range rows {
		if !validProviderModelID(row.ID) || seen[row.ID] {
			continue
		}
		contract, supported := provider.ProfileForModel(row.ID)
		if !supported || contract.Family != p.ID {
			continue
		}
		if p.ID == "xai" && (!stringListContains(row.Inputs, "text") || !stringListContains(row.Outputs, "text")) {
			continue
		}
		// The profile registry owns wire/reasoning-history compatibility. A
		// provider listing alone must not opt an unsupported alias into Agent.
		entry := discoveredProviderModel{ID: row.ID, Name: row.ID, Context: 32768, Images: contract.Multimodal && (p.ID != "xai" || stringListContains(row.Inputs, "image"))}
		if row.Context >= 16384 && row.Context <= 2097152 {
			entry.Context = row.Context
		}
		for _, existing := range catalog.Profiles {
			if existing.Model == row.ID {
				if !providerMatches(existing, p) {
					continue
				}
				entry.Configured = true
				entry.Name = existing.DisplayName
				entry.Context = existing.Context
				entry.Images = existing.SupportsImages
			}
		}
		seen[row.ID] = true
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) == 0 {
		return nil, settingsError(422, "no_supported_models", "此金鑰沒有可用的對話模型")
	}
	return out, nil
}
func stringListContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func validProviderModelID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/:", r)) {
			return false
		}
	}
	return true
}
func randomProviderToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *desktopModelProviderSettings) save(w http.ResponseWriter, r *http.Request) {
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
	verification, ok := s.verified[body.VerificationID]
	if !ok || !verification.expires.After(s.now()) {
		delete(s.verified, body.VerificationID)
		writeProviderSettingsError(w, settingsError(410, "verification_expired", "驗證已過期，請重新驗證金鑰"))
		return
	}
	if len(body.ModelIDs) == 0 || len(body.ModelIDs) > 32 {
		writeProviderSettingsError(w, settingsError(400, "invalid_models", "請選擇 1 至 32 個模型"))
		return
	}
	if !s.lifecycle.TryLock() {
		writeProviderSettingsError(w, settingsError(409, "model_runtime_busy", "模型正在處理工作，請完成後再儲存"))
		return
	}
	defer s.lifecycle.Unlock()
	// The Agent lease lasts fifteen seconds. Bound the entire acquisition and
	// local transaction to five seconds, and check that deadline immediately
	// before each atomic replacement (including after a slow file sync).
	commitCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	release, err := s.lease(commitCtx)
	if err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	defer release()
	if err := s.saveVerified(commitCtx, verification, body.ModelIDs); err != nil {
		writeProviderSettingsError(w, err)
		return
	}
	delete(s.verified, body.VerificationID)
	writeJSON(w, map[string]any{"saved": true, "modelIds": body.ModelIDs, "activation": "next-use"})
}
func (s *desktopModelProviderSettings) saveVerified(ctx context.Context, v modelProviderVerification, ids []string) error {
	catalog, err := s.readCatalog()
	if err != nil {
		return err
	}
	keyPath, err := providerKeyPath(catalog, v.provider)
	if err != nil {
		return err
	}
	if keyPath == "" {
		keyPath = filepath.Join(filepath.Dir(s.path), "credentials", v.provider.ID+".key")
	}
	if v.useStored {
		key, err := readPrivateProviderFile(v.keyPath, 8192)
		if err != nil || keyPath != v.keyPath || strings.TrimSpace(string(key)) != v.key {
			return settingsError(410, "verification_expired", "已儲存金鑰已變更，請重新驗證")
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return settingsError(400, "invalid_models", "模型選擇不可重複")
		}
		seen[id] = true
		var selected *discoveredProviderModel
		for i := range v.models {
			if v.models[i].ID == id {
				selected = &v.models[i]
				break
			}
		}
		if selected == nil {
			return settingsError(400, "invalid_models", "所選模型不在已驗證清單中")
		}
		found := false
		for i, existing := range catalog.Profiles {
			if existing.Model == id {
				if !providerMatches(existing, v.provider) {
					return settingsError(409, "model_id_conflict", "已有其他供應商使用相同模型 ID")
				}
				if existing.APIKeyFile == "" {
					// Core fingerprints deliberately omit credential references.
					// Attaching a new path to an already resident credentialless
					// route would leave its immutable profile pointing at no key.
					return settingsError(409, "credential_reference_missing", "既有模型缺少受管理金鑰設定，請先修復原有模型設定")
				}
				catalog.Profiles[i].APIKeyFile = keyPath
				found = true
				break
			}
		}
		if !found {
			catalog.Profiles = append(catalog.Profiles, configuredProviderProfile{Profile: upstreamconfig.Profile{Model: id, URL: v.provider.URL, APIFormat: v.provider.ID, Context: selected.Context, MaxOutput: 8192, DisplayName: id, Provider: v.provider.Name, SupportsImages: selected.Images}, APIKeyFile: keyPath})
		}
	}
	if len(catalog.Profiles) > 32 {
		return settingsError(409, "model_limit", "最多可設定 32 個模型")
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return settingsError(503, "save_failed", "無法儲存模型設定")
	}
	data = append(data, '\n')
	// Stage and validate exact bytes before rotating any credential. Only the
	// catalog pointer is replaced; local35B and unselected metadata are retained.
	stage, err := stagePrivateProviderFile(s.path, data)
	if err != nil {
		return settingsError(503, "save_failed", "無法儲存模型設定")
	}
	defer os.Remove(stage)
	if _, err := upstreamconfig.Load(stage); err != nil {
		return settingsError(503, "save_failed", "模型設定未通過驗證")
	}
	var oldKey []byte
	keyExisted := false
	if !v.useStored {
		oldKey, err = readPrivateProviderFile(keyPath, 8192)
		if err == nil {
			keyExisted = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return settingsError(503, "credential_unavailable", "金鑰檔案必須是僅限目前帳號的私人檔案")
		}
		if err := ctx.Err(); err != nil {
			return settingsError(409, "save_cancelled", "儲存已取消")
		}
		if err := atomicPrivateProviderFileContext(ctx, keyPath, []byte(v.key)); err != nil {
			return settingsError(503, "save_failed", "無法儲存金鑰")
		}
	}
	commitErr := ctx.Err()
	if commitErr == nil {
		commitErr = os.Rename(stage, s.path)
	}
	if commitErr != nil {
		if !v.useStored {
			if keyExisted {
				if err := atomicPrivateProviderFile(keyPath, oldKey); err != nil {
					return settingsError(503, "credential_recovery_required", "設定未儲存，且無法還原金鑰；請重新驗證並儲存")
				}
			} else {
				_ = os.Remove(keyPath)
			}
		}
		return settingsError(503, "save_failed", "無法儲存模型設定")
	}
	return nil
}

func readPrivateProviderFile(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("invalid private file")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, errors.New("invalid private file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0077 != 0 || after.Size() > limit {
		return nil, errors.New("invalid private file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("invalid private file")
	}
	return data, nil
}
func stagePrivateProviderFile(target string, data []byte) (string, error) {
	if !filepath.IsAbs(target) {
		return "", errors.New("invalid private path")
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("private directory required")
	}
	if info, err := os.Lstat(target); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return "", errors.New("private target required")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	file, err := os.CreateTemp(dir, ".model-settings-*")
	if err != nil {
		return "", err
	}
	name := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Close()
	}
	if err != nil {
		return "", err
	}
	ok = true
	return name, nil
}
func atomicPrivateProviderFile(target string, data []byte) error {
	return atomicPrivateProviderFileContext(context.Background(), target, data)
}

func atomicPrivateProviderFileContext(ctx context.Context, target string, data []byte) error {
	path, err := stagePrivateProviderFile(target, data)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(path, target)
}
func writeProviderSettingsError(w http.ResponseWriter, err error) {
	var public *providerSettingsError
	if !errors.As(err, &public) {
		public = settingsError(503, "settings_unavailable", "模型設定暫時無法使用")
	}
	w.WriteHeader(public.status)
	writeJSON(w, map[string]any{"error": map[string]string{"code": public.code, "message": public.message}})
}

func acquireProviderConfigurationLease(ctx context.Context, client *daemontransport.Client) (func(), error) {
	_, release, err := acquireProviderConfigurationLeaseID(ctx, client)
	return release, err
}

func acquireProviderConfigurationLeaseID(ctx context.Context, client *daemontransport.Client) (string, func(), error) {
	if client == nil {
		return "", nil, settingsError(503, "agent_unavailable", "無法確認工作狀態，請稍後再試")
	}
	call := func(ctx context.Context, method string, body any, target any) error {
		req, err := client.NewRequest(ctx, method, "/agent/configuration/lease", body)
		if err != nil {
			return settingsError(503, "agent_unavailable", "無法確認工作狀態，請稍後再試")
		}
		response, err := client.HTTP.Do(req)
		if err != nil {
			return settingsError(503, "agent_unavailable", "無法確認工作狀態，請稍後再試")
		}
		defer response.Body.Close()
		if response.StatusCode == 409 {
			return settingsError(409, "model_runtime_busy", "仍有進行中或排隊工作，請完成後再儲存")
		}
		if response.StatusCode != 200 {
			return settingsError(503, "agent_unavailable", "請重新啟動 Meadow Desktop 後再儲存")
		}
		if target != nil && json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(target) != nil {
			return settingsError(503, "agent_unavailable", "無法確認工作狀態")
		}
		return nil
	}
	var receipt struct {
		LeaseID string `json:"leaseId"`
		Expires int    `json:"expiresInSeconds"`
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := call(bounded, http.MethodPost, map[string]any{}, &receipt); err != nil {
		return "", nil, err
	}
	if len(receipt.LeaseID) < 32 || receipt.Expires < 10 {
		return "", nil, settingsError(503, "agent_unavailable", "無法取得模型設定鎖定")
	}
	return receipt.LeaseID, func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = call(releaseCtx, http.MethodDelete, map[string]string{"leaseId": receipt.LeaseID}, nil)
	}, nil
}
