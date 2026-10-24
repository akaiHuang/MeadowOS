//go:build !meadow_protected

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

// Package composition is a linker fact, so an inherited edition or provider
// environment cannot make a portable build supervise a local inference server.
func prepareDesktopPortableState(packageMode string) error {
	if packageMode != "portable" {
		return nil
	}
	home := strings.TrimSpace(os.Getenv("MEADOW_DESKTOP_STATE_HOME"))
	if home != "" && (productsecurity.Required() || strings.TrimSpace(os.Getenv("MEADOW_PUBLIC_RELEASE")) == "1") {
		return fmt.Errorf("public portable Desktop forbids state directory overrides")
	}
	if home == "" {
		root, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		home = filepath.Join(root, "Meadow Desktop Portable")
		if buildinfo.VMLab {
			home = filepath.Join(root, "Meadow VM Lab")
		} else if buildinfo.DesktopVariant == "evo" {
			home = filepath.Join(root, "Meadow Code Evo")
		}
	}
	if !filepath.IsAbs(home) || strings.ContainsRune(home, '\x00') {
		return fmt.Errorf("portable Desktop state directory must be absolute")
	}
	home = filepath.Clean(home)
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	// Portable state is independent of legacy Qwen/Meadow discovery. Children
	// inherit this environment, so do not pass a second state root to helpers.
	if err := os.Unsetenv("QWEN_HOME"); err != nil {
		return err
	}
	if err := os.Setenv("MEADOW_HOME", home); err != nil {
		return err
	}
	// A new portable profile must not inherit another profile's IPC authority.
	if err := os.Unsetenv("MEADOW_AGENT_API_KEY"); err != nil {
		return err
	}
	if err := os.Setenv("MEADOW_AGENT_TOKEN_FILE", filepath.Join(home, "run", "agent-api-token")); err != nil {
		return err
	}
	// Pin the socket passed to children to the same canonical profile root.
	// Never replace another installed Desktop's helper during portable startup.
	if err := os.Setenv("MEADOW_AGENT_SOCKET", filepath.Join(home, "run", "agent.sock")); err != nil {
		return err
	}
	return os.Setenv("MEADOW_MODEL_CONNECTIONS_FILE", filepath.Join(home, "model-connections-v1.json"))
}

func portableBootstrapOwnedEnvironment(key string) bool {
	switch key {
	case "MEADOW_HOME", "QWEN_HOME", "MEADOW_DESKTOP_STATE_HOME", "MEADOW_DESKTOP_PACKAGE_MODE",
		"MEADOW_AGENT_API_KEY", "MEADOW_AGENT_TOKEN_FILE", "MEADOW_AGENT_SOCKET", "MEADOW_MODEL_CONNECTIONS_FILE":
		return true
	default:
		return false
	}
}

type desktopConnectionRuntime struct {
	file       string
	catalog    *modelconnection.Catalog
	connection modelconnection.Connection
	mu         sync.RWMutex
	modelID    string
	discover   func(context.Context) ([]modelconnection.Model, error)
	cache      desktopDiscoveryCache
}

// A nil runtime is the explicit legacy diagnostic composition. A non-nil but
// unconfigured runtime is a valid fresh portable application with working login.
func resolveDesktopConnectionStartup(packageMode string) (*desktopConnectionRuntime, error) {
	mode := strings.TrimSpace(os.Getenv("MEADOW_DESKTOP_PACKAGE_MODE"))
	path := modelconnection.DefaultPath()
	explicit := strings.TrimSpace(os.Getenv("MEADOW_MODEL_CONNECTIONS_FILE")) != ""
	connected := packageMode == "portable" || mode == "portable" || mode == "api" || explicit
	if packageMode != "portable" && mode == "diagnostic" && !explicit {
		return nil, nil
	}
	if path == "" {
		path = filepath.Join(session.PrimaryDir(), "model-connections-v1.json")
	}
	if !connected {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
	}
	catalog, err := modelconnection.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		catalog, err = &modelconnection.Catalog{Version: modelconnection.Version, Connections: []modelconnection.Connection{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read model connections: %w", err)
	}
	runtime := &desktopConnectionRuntime{file: path, catalog: catalog, modelID: "unconfigured"}
	if catalog.SelectedConnectionID == "" {
		return runtime, nil
	}
	connection, model, err := catalog.Resolve()
	if err != nil {
		return nil, err
	}
	runtime.connection, runtime.modelID = connection, model.ID
	return runtime, nil
}

func (runtime *desktopConnectionRuntime) selectedModel() string {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.modelID
}

func (runtime *desktopConnectionRuntime) validateModel(modelID string) error {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	if _, ok := runtime.connection.LookupModel(modelID); !ok {
		return fmt.Errorf("此連線未設定模型 %s", modelID)
	}
	return nil
}

func (runtime *desktopConnectionRuntime) restartRequired() bool {
	if runtime.file == "" {
		return false
	}
	catalog, err := modelconnection.Load(runtime.file)
	if errors.Is(err, os.ErrNotExist) {
		runtime.mu.RLock()
		defer runtime.mu.RUnlock()
		return runtime.catalog != nil && len(runtime.catalog.Connections) > 0
	}
	if err != nil {
		return true
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.catalog == nil || runtime.catalog.Fingerprint() != catalog.Fingerprint()
}

func (runtime *desktopConnectionRuntime) connectionSnapshot() modelconnection.Connection {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.connection
}

func (runtime *desktopConnectionRuntime) list(ctx context.Context) ([]modelEntry, bool, error) {
	rows := []modelEntry{}
	connection := runtime.connectionSnapshot()
	if connection.ID == "" {
		return rows, false, nil
	}
	models, err := runtime.discoveredModels(ctx)
	if runtime.connectionSnapshot().ID != connection.ID {
		return rows, false, errors.New("model connection changed during discovery")
	}
	live := map[string]bool{}
	for _, model := range models {
		live[model.ID] = true
	}
	selected := runtime.selectedModel()
	for _, model := range connection.Models {
		if model.Disabled {
			continue
		}
		name := model.DisplayName
		if name == "" {
			name = model.ID
		}
		rows = append(rows, modelEntry{Name: name, ModelID: model.ID, Delivery: "api", Desc: connection.Name + " · 模型 API", Active: model.ID == selected, Live: live[model.ID], Installed: false, Multimodal: model.SupportsImages})
	}
	return rows, live[selected], err
}

// This wraps the legacy mux and owns every route that could control a model
// process. External Core on :18081 is still just an inference connection.
func (runtime *desktopConnectionRuntime) guard(next http.Handler, control desktopModelActivator, selected func(string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkConfiguration := r.URL.Path == "/model/status" || r.URL.Path == "/model/list" || r.URL.Path == "/native/load-model" || r.URL.Path == "/agent/model"
		if checkConfiguration {
			w.Header().Set("Cache-Control", "no-store")
		}
		if checkConfiguration && runtime.restartRequired() {
			switch r.Method + " " + r.URL.Path {
			case "GET /model/status", "HEAD /model/status":
				writeJSON(w, map[string]any{"status": "restart_required", "ready": false, "model": "", "detail": "模型連線設定已更新，請重新開啟 Meadow Desktop", "restartRequired": true, "lifecycleManaged": false})
				return
			case "GET /model/list", "HEAD /model/list":
				writeJSON(w, []modelEntry{})
				return
			case "POST /native/load-model", "POST /agent/model":
				writeEditionError(w, http.StatusConflict, "模型連線設定已更新，請重新開啟 Meadow Desktop")
				return
			}
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /model/status", "HEAD /model/status":
			connection := runtime.connectionSnapshot()
			_, ready, err := runtime.list(r.Context())
			status, detail := "unconfigured", "請到設定新增模型 API 連線"
			if connection.ID != "" {
				status, detail = "error", "模型 API 尚未就緒"
			}
			if ready {
				status, detail = "ready", "模型 API 已連線"
			} else if err != nil {
				detail = "模型 API 無法連線，請檢查設定"
			}
			model := ""
			if ready {
				model = runtime.selectedModel()
			}
			writeJSON(w, map[string]any{"status": status, "ready": ready, "model": model, "detail": detail, "delivery": "api", "connectionId": connection.ID, "lifecycleManaged": false})
			return
		case "GET /model/list", "HEAD /model/list":
			rows, _, _ := runtime.list(r.Context())
			writeJSON(w, rows)
			return
		case "POST /native/load-model", "POST /agent/model":
			var body struct {
				ModelID string `json:"modelId"`
				Model   string `json:"model"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
				writeEditionError(w, 400, "model is required")
				return
			}
			target := strings.TrimSpace(body.ModelID)
			if r.URL.Path == "/agent/model" {
				target = strings.TrimSpace(body.Model)
			}
			if err := runtime.validateModel(target); err != nil {
				writeEditionError(w, 400, err.Error())
				return
			}
			result, err := control.ActivateModel(r.Context(), target)
			if err != nil {
				writeEditionError(w, 503, err.Error())
				return
			}
			runtime.mu.Lock()
			runtime.modelID = target
			runtime.mu.Unlock()
			if selected != nil {
				selected(target)
			}
			writeJSON(w, result)
			return
		case "POST /native/unload-model", "DELETE /agent/model", "POST /native/pull-model", "POST /native/edition":
			writeEditionError(w, http.StatusConflict, "外部模型服務由服務端管理；請在設定選擇連線")
			return
		case "GET /native/model-catalog", "HEAD /native/model-catalog":
			writeJSON(w, map[string]any{"models": []catalogRow{}, "delivery": "api", "downloadEnabled": false})
			return
		case "GET /native/pull-status", "HEAD /native/pull-status":
			writeJSON(w, map[string]any{"running": false, "done": true, "disabled": true})
			return
		case "POST /native/pull-cancel":
			writeJSON(w, map[string]any{"cancelled": false, "disabled": true})
			return
		}
		next.ServeHTTP(w, r)
	})
}
