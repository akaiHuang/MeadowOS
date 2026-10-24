//go:build !meadow_protected

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/hey-meadow/meadow-code/go/core/upstreamconfig"
)

// desktopUpstreamProfile describes the Core-owned route catalog. Settings may
// atomically extend that private catalog; inference remains owned by Core.
type desktopUpstreamProfile struct {
	Catalog         *upstreamconfig.Catalog
	ModelID         string
	ModelIDs        []string
	BaseURL         string
	ContextTokens   int
	MaxOutputTokens int
	APIFormat       string
}

func desktopUsesUpstreamBackend() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("MEADOW_CORE_BACKEND")), "openai-compatible")
}

func loadDesktopUpstreamProfile() (desktopUpstreamProfile, error) {
	if !desktopUsesUpstreamBackend() {
		return desktopUpstreamProfile{}, nil
	}
	if catalog, err := upstreamconfig.LoadEnvironment(); err != nil {
		return desktopUpstreamProfile{}, err
	} else if catalog != nil {
		selected, _ := catalog.Lookup(catalog.DefaultModel)
		if override := strings.TrimSpace(os.Getenv("MEADOW_DESKTOP_MODEL_ID")); override != "" && override != selected.Model {
			return desktopUpstreamProfile{}, fmt.Errorf("MEADOW_DESKTOP_MODEL_ID must match the upstream catalog default")
		}
		if override := strings.TrimSpace(os.Getenv("MEADOW_DESKTOP_MODEL_BASE_URL")); override != "" && !isMeadowCoreBaseURL(override) {
			return desktopUpstreamProfile{}, fmt.Errorf("the upstream API catalog must use Meadow Core at port 18081")
		}
		return desktopUpstreamProfile{Catalog: catalog, ModelIDs: catalog.ModelIDs(), ModelID: selected.Model, BaseURL: selected.URL, ContextTokens: selected.Context, MaxOutputTokens: selected.MaxOutput, APIFormat: selected.APIFormat}, nil
	}
	modelID := strings.TrimSpace(os.Getenv("MEADOW_CORE_UPSTREAM_MODEL"))
	if modelID == "" {
		return desktopUpstreamProfile{}, fmt.Errorf("MEADOW_CORE_UPSTREAM_MODEL is required for the upstream API profile")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("MEADOW_CORE_UPSTREAM_BASE_URL")), "/")
	if baseURL == "" {
		return desktopUpstreamProfile{}, fmt.Errorf("MEADOW_CORE_UPSTREAM_BASE_URL is required for the upstream API profile")
	}
	if override := strings.TrimSpace(os.Getenv("MEADOW_DESKTOP_MODEL_ID")); override != "" && override != modelID {
		return desktopUpstreamProfile{}, fmt.Errorf("MEADOW_DESKTOP_MODEL_ID must match MEADOW_CORE_UPSTREAM_MODEL")
	}
	if override := strings.TrimSpace(os.Getenv("MEADOW_DESKTOP_MODEL_BASE_URL")); override != "" && !isMeadowCoreBaseURL(override) {
		return desktopUpstreamProfile{}, fmt.Errorf("the upstream API profile must use Meadow Core at port 18081")
	}
	limits := []struct {
		key      string
		fallback int
	}{
		{"MEADOW_CORE_UPSTREAM_CONTEXT_TOKENS", 32768},
		{"MEADOW_CORE_UPSTREAM_MAX_OUTPUT_TOKENS", 8192},
	}
	values := make([]int, len(limits))
	for i, limit := range limits {
		values[i] = limit.fallback
		if raw, exists := os.LookupEnv(limit.key); exists {
			value, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || value <= 0 {
				return desktopUpstreamProfile{}, fmt.Errorf("%s must be a positive integer", limit.key)
			}
			values[i] = value
		}
	}
	if values[1] >= values[0] {
		return desktopUpstreamProfile{}, fmt.Errorf("upstream output limit must be smaller than context")
	}
	apiFormat := strings.TrimSpace(os.Getenv("MEADOW_CORE_UPSTREAM_API_FORMAT"))
	if apiFormat == "" {
		apiFormat = "openai"
	}
	if apiFormat != "openai" && apiFormat != "mlx-vlm" && apiFormat != "xai" && apiFormat != "deepseek" {
		return desktopUpstreamProfile{}, fmt.Errorf("MEADOW_CORE_UPSTREAM_API_FORMAT must be openai, mlx-vlm, xai or deepseek")
	}
	modelIDs := []string{}
	for _, id := range strings.Split(os.Getenv("MEADOW_CORE_UPSTREAM_MODELS"), ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			modelIDs = append(modelIDs, id)
		}
	}
	if len(modelIDs) > 0 {
		found := false
		seen := map[string]bool{}
		for _, id := range modelIDs {
			if seen[id] || strings.ContainsAny(id, "\r\n\t") {
				return desktopUpstreamProfile{}, fmt.Errorf("invalid upstream model catalog")
			}
			seen[id] = true
			found = found || id == modelID
		}
		if !found || len(modelIDs) > 32 {
			return desktopUpstreamProfile{}, fmt.Errorf("upstream model catalog must include default and at most 32 models")
		}
	}
	return desktopUpstreamProfile{ModelIDs: modelIDs, ModelID: modelID, BaseURL: baseURL, ContextTokens: values[0], MaxOutputTokens: values[1], APIFormat: apiFormat}, nil
}

func (p desktopUpstreamProfile) enabled() bool { return p.ModelID != "" }

// Closures registered at launch must see models added in Settings. A broken
// managed catalog fails closed instead of restoring a stale admission list.
func (p desktopUpstreamProfile) current() desktopUpstreamProfile {
	if p.Catalog == nil {
		return p
	}
	if current, err := loadDesktopUpstreamProfile(); err == nil {
		return current
	}
	p.Catalog = &upstreamconfig.Catalog{}
	p.ModelIDs = nil
	return p
}

// Match Core's sorted compact JSON (including Python's ASCII Unicode escapes).
// No credential value or credential-file path participates in the identity.
func (p desktopUpstreamProfile) configurationFingerprint() string {
	if p.Catalog != nil {
		return p.Catalog.Fingerprint()
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(struct {
		APIFormat string   `json:"api_format"`
		Context   int      `json:"context"`
		MaxOutput int      `json:"max_output"`
		Model     string   `json:"model"`
		Models    []string `json:"models,omitempty"`
		URL       string   `json:"url"`
	}{p.APIFormat, p.ContextTokens, p.MaxOutputTokens, p.ModelID, p.ModelIDs, p.BaseURL})
	var canonical strings.Builder
	for _, char := range strings.TrimSuffix(encoded.String(), "\n") {
		if char < 127 {
			canonical.WriteRune(char)
		} else {
			for _, unit := range utf16.Encode([]rune{char}) {
				fmt.Fprintf(&canonical, `\u%04x`, unit)
			}
		}
	}
	digest := sha256.Sum256([]byte(canonical.String()))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func (p desktopUpstreamProfile) models() []string {
	p = p.current()
	if p.Catalog != nil {
		return p.Catalog.ModelIDs()
	}
	if len(p.ModelIDs) > 0 {
		return p.ModelIDs
	}
	return []string{p.ModelID}
}
func (p desktopUpstreamProfile) allows(modelID string) bool {
	for _, id := range p.models() {
		if id == strings.TrimSpace(modelID) {
			return true
		}
	}
	return false
}
func (p desktopUpstreamProfile) validateModelChoice(modelID string) error {
	if p.enabled() && !p.allows(modelID) {
		return fmt.Errorf("此 API 設定未提供 %s", modelID)
	}
	return nil
}

func (p desktopUpstreamProfile) persistReadyModel(modelID string) {
	if !p.enabled() {
		persistPreferredModel(modelID)
	}
}

func (p desktopUpstreamProfile) modelList(snapshot meadowCoreSnapshot) []modelEntry {
	p = p.current()
	rows := []modelEntry{}
	for _, id := range p.models() {
		live := snapshot.Ready && snapshot.ModelID == id
		name, description := id, "模型 API · 經由 Meadow Core"
		if entry, ok := p.Catalog.Lookup(id); ok {
			name, description = entry.DisplayName, entry.Provider+" · 模型 API · 經由 Meadow Core"
		}
		rows = append(rows, modelEntry{
			Name: name, ModelID: id, Delivery: "api",
			Desc: description, Active: live || (!snapshot.Ready && id == p.ModelID),
			Live: live, Installed: false, Multimodal: live && snapshot.Multimodal,
		})
	}
	return rows
}

// An API profile has no downloadable local-model catalog. In particular it
// must not expose the bundled five-tier inventory or allow a download to
// accidentally overwrite the user's normal local-model configuration.
func (p desktopUpstreamProfile) registerCatalogHandlers(api *http.ServeMux) {
	api.HandleFunc("GET /native/model-catalog", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"models": []catalogRow{}, "delivery": "api", "configuredModel": p.ModelID})
	})
	api.HandleFunc("POST /native/pull-model", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": "目前使用模型 API，不需下載本機模型"})
	})
	api.HandleFunc("GET /native/pull-status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"running": false, "done": true, "disabled": true})
	})
	api.HandleFunc("POST /native/pull-cancel", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"cancelled": false, "disabled": true})
	})
}
