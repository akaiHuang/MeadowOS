package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	coreconfig "github.com/hey-meadow/meadow-code/go/core/config"
)

// Settings is a loose view over the existing settings.json (schema v4).
// Only the keys the Go core actually consumes get typed accessors; the rest
// round-trips untouched in Raw.
type Settings struct {
	Raw    map[string]any
	Path   string // legacy settings.json; "" when defaults
	Config *coreconfig.Document
}

// LoadSettings walks the home-dir chain for settings.json. A missing file
// is not an error — defaults apply.
func LoadSettings() (*Settings, error) {
	path := findInChain("settings.json")
	raw := map[string]any{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	document, err := coreconfig.LoadFromDirs(HomeDirs())
	if err != nil {
		return nil, err
	}
	return &Settings{Raw: raw, Path: path, Config: document}, nil
}

func (s *Settings) get(path ...string) any {
	var cur any = s.Raw
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

func (s *Settings) getString(path ...string) string {
	v, _ := s.get(path...).(string)
	return v
}

// ModelEndpoint resolves the provider base URL, API key, and served model
// id for the selected model, walking modelProviders the way the TS CLI
// does: defaultProvider/defaultModel name → provider entry → models list.
func (s *Settings) ModelEndpoint() (baseURL, apiKey, modelID string) {
	baseURL, apiKey, modelID = s.legacyModelEndpoint()
	if s.Config == nil || !s.Config.ModelRouteConfigured() {
		return baseURL, apiKey, modelID
	}
	// A current config route is one atomic ownership boundary. Never carry a
	// legacy cloud credential into a new local/custom base URL merely because
	// the current route intentionally has no API key.
	current := s.Config.ModelConfig()
	baseURL = "http://localhost:8080/v1"
	apiKey = ""
	modelID = current.Model
	if current.BaseURL != "" {
		baseURL = current.BaseURL
	}
	if current.APIKey != "" {
		apiKey = current.APIKey
	}
	return baseURL, apiKey, modelID
}

func (s *Settings) legacyModelEndpoint() (baseURL, apiKey, modelID string) {
	baseURL = "http://localhost:8080/v1"
	apiKey = "dummy"

	selected := s.getString("model", "name")
	if selected == "" {
		selected = s.getString("defaultModel")
	}
	// Product model names are valid Core model ids even when this compact
	// settings file has no legacy modelProviders catalog. A provider row may
	// still translate a display name to a custom/path id below.
	modelID = selected

	providers, _ := s.get("modelProviders").(map[string]any)
	pickProvider := func(name string) (map[string]any, bool) {
		p, ok := providers[name].(map[string]any)
		return p, ok
	}
	var provider map[string]any
	if p, ok := pickProvider(s.getString("defaultProvider")); ok {
		provider = p
	} else {
		for _, v := range providers {
			if p, ok := v.(map[string]any); ok {
				provider = p
				break
			}
		}
	}
	if provider == nil {
		return baseURL, apiKey, modelID
	}
	if v, ok := provider["baseUrl"].(string); ok && v != "" {
		baseURL = v
	}
	if v, ok := provider["apiKey"].(string); ok && v != "" {
		apiKey = v
	}
	models, _ := provider["models"].([]any)
	for _, mv := range models {
		m, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		id, _ := m["modelId"].(string)
		if modelID == "" || name == selected {
			modelID = id
		}
		if name == selected {
			break
		}
	}
	return baseURL, apiKey, modelID
}

// ModelServer returns the raw modelServer object — the optional hand-tuned
// vllm-mlx launch recipe (nil when absent). core/model owns the schema and
// parses it; settings only carries it through the home-dir chain.
func (s *Settings) ModelServer() map[string]any {
	if s.Config != nil {
		if value := s.Config.Object("model_server"); value != nil {
			return value
		}
		if value := s.Config.Object("modelServer"); value != nil {
			return value
		}
	}
	v, _ := s.get("modelServer").(map[string]any)
	return v
}

// ApprovalMode returns tools.approvalMode, defaulting to "default".
func (s *Settings) ApprovalMode() string {
	if s.Config != nil {
		if value := s.Config.ApprovalMode(); value != "" {
			return value
		}
	}
	if v := s.getString("tools", "approvalMode"); v != "" {
		return v
	}
	return "default"
}

// SessionTokenLimit returns model.sessionTokenLimit, 0 = unlimited.
func (s *Settings) SessionTokenLimit() int {
	if s.Config != nil {
		if value := s.Config.ModelConfig().SessionTokenLimit; value > 0 {
			return value
		}
	}
	if v, ok := s.get("model", "sessionTokenLimit").(float64); ok {
		return int(v)
	}
	return 0
}

// SetDefaultModel persists the last model that reached the ready state. The
// current config route owns model selection when present; otherwise preserve
// the legacy settings.json schema used by existing Desktop installations.
func (s *Settings) SetDefaultModel(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("default model is required")
	}
	if s.Config != nil && s.Config.ModelRouteConfigured() {
		if err := s.Config.Set(name, "model", "default"); err != nil {
			return err
		}
		return s.Config.Save()
	}
	model, ok := s.Raw["model"].(map[string]any)
	if !ok {
		model = map[string]any{}
		s.Raw["model"] = model
	}
	model["name"] = name
	return s.save()
}

// SetApprovalMode writes tools.approvalMode back, preserving every other
// key. Creates the file under PrimaryDir when none exists yet.
func (s *Settings) SetApprovalMode(mode string) error {
	if s.Config != nil {
		if err := s.Config.Set(mode, "meadow", "approval_mode"); err != nil {
			return err
		}
		return s.Config.Save()
	}
	tools, ok := s.Raw["tools"].(map[string]any)
	if !ok {
		tools = map[string]any{}
		s.Raw["tools"] = tools
	}
	tools["approvalMode"] = mode
	return s.save()
}

// Theme returns ui.theme ("" when unset).
func (s *Settings) Theme() string {
	if s.Config != nil {
		if value := s.Config.Theme(); value != "" {
			return value
		}
	}
	return s.getString("ui", "theme")
}

// SetTheme writes ui.theme back — the same key the TS CLI persists, so the
// choice follows the user across both CLIs.
func (s *Settings) SetTheme(name string) error {
	if s.Config != nil {
		if err := s.Config.Set(name, "display", "skin"); err != nil {
			return err
		}
		return s.Config.Save()
	}
	ui, ok := s.Raw["ui"].(map[string]any)
	if !ok {
		ui = map[string]any{}
		s.Raw["ui"] = ui
	}
	ui["theme"] = name
	return s.save()
}

func (s *Settings) save() error {
	path := s.Path
	if path == "" {
		path = filepath.Join(PrimaryDir(), "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		s.Path = path
	}
	b, err := json.MarshalIndent(s.Raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
