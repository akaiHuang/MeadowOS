//go:build !meadow_protected

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
)

// pullState holds the single in-flight model download the desktop is driving.
// The frontend starts a pull, then polls /native/pull-status for the bar.
type pullState struct {
	mu      sync.Mutex
	model   string
	prog    modelmgr.PullProgress
	running bool
	done    bool
	errMsg  string
	cancel  context.CancelFunc
}

var pull pullState

// catalogRow is one downloadable-model card for the frontend.
type catalogRow struct {
	ServedName      string `json:"servedName"`
	ApproxGB        int    `json:"approxGB"`
	MinRAMGiB       int64  `json:"minRamGiB"`
	ProductRole     string `json:"productRole,omitempty"`
	RoleLabel       string `json:"roleLabel,omitempty"`
	RoleDescription string `json:"roleDescription,omitempty"`
	ProductionReady bool   `json:"productionReady"`
	Present         bool   `json:"present"`
	Recommended     bool   `json:"recommended"`
	Delivery        string `json:"delivery,omitempty"`
	DownloadEnabled bool   `json:"downloadEnabled"`
}

// registerPullHandlers wires the model-catalog + download endpoints onto the
// desktop's api mux.
func registerPullHandlers(api *http.ServeMux, bundledModelIDs ...string) {
	bundled := make(map[string]bool, len(bundledModelIDs))
	for _, id := range bundledModelIDs {
		if info, ok := modelmgr.ModelByName(id); ok {
			bundled[info.ServedName] = true
		}
	}
	selfContained := len(bundled) > 0
	// Catalog: which models exist, which are downloaded, which we recommend.
	api.HandleFunc("GET /native/model-catalog", func(w http.ResponseWriter, r *http.Request) {
		recommended := modelmgr.DefaultModelTier()
		rows := make([]catalogRow, 0)
		for _, m := range modelmgr.DownloadableModelsForCurrentMachine() {
			if selfContained && !bundled[m.ServedName] {
				continue
			}
			delivery := "download"
			present := modelmgr.ResolveModelPath(m.ServedName) != ""
			if bundled[m.ServedName] {
				delivery = "embedded"
				present = true
			}
			row := catalogRow{
				ServedName:      m.ServedName,
				ApproxGB:        m.ApproxGB,
				MinRAMGiB:       m.MinRAMGiB,
				Present:         present,
				Recommended:     m.ServedName == recommended || bundled[m.ServedName],
				Delivery:        delivery,
				DownloadEnabled: !selfContained,
			}
			if position, ok := modelmgr.ProductPositionFor(m.ServedName); ok {
				row.ProductRole = string(position.Role)
				row.RoleLabel = position.Label
				row.RoleDescription = position.CoreTasks
				row.ProductionReady = position.Ready
			}
			rows = append(rows, row)
		}
		machine := modelmgr.CurrentMachineProfile()
		writeJSON(w, map[string]any{"models": rows, "totalRamGiB": machine.TotalRAMGiB, "machine": machine})
	})

	// Start a download (idempotent: a second call while one runs is a no-op).
	api.HandleFunc("POST /native/pull-model", func(w http.ResponseWriter, r *http.Request) {
		if selfContained {
			http.Error(w, "此版本已內建完整產品模型，不需要也不允許下載模型", http.StatusConflict)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		info, ok := modelmgr.ModelByName(body.Model)
		if !ok {
			http.Error(w, "unknown model", http.StatusBadRequest)
			return
		}
		if ok, reason := modelmgr.ModelDownloadAllowed(info.ServedName); !ok {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
		pull.mu.Lock()
		if pull.running {
			busy := pull.model
			pull.mu.Unlock()
			writeJSON(w, map[string]any{"started": false, "busyWith": busy})
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		pull.model = info.ServedName
		pull.running = true
		pull.done = false
		pull.errMsg = ""
		pull.prog = modelmgr.PullProgress{Model: info.ServedName}
		pull.cancel = cancel
		pull.mu.Unlock()

		go func() {
			_, err := modelmgr.Pull(ctx, info.ServedName, "", func(p modelmgr.PullProgress) {
				pull.mu.Lock()
				pull.prog = p
				pull.mu.Unlock()
			})
			pull.mu.Lock()
			pull.running = false
			pull.done = err == nil
			if err != nil && ctx.Err() == nil {
				pull.errMsg = err.Error()
			}
			pull.cancel = nil
			pull.mu.Unlock()
		}()
		writeJSON(w, map[string]any{"started": true, "model": info.ServedName})
	})

	// Poll the in-flight download.
	api.HandleFunc("GET /native/pull-status", func(w http.ResponseWriter, r *http.Request) {
		if selfContained {
			writeJSON(w, map[string]any{"running": false, "done": true, "disabled": true})
			return
		}
		pull.mu.Lock()
		defer pull.mu.Unlock()
		writeJSON(w, map[string]any{
			"model":       pull.model,
			"running":     pull.running,
			"done":        pull.done,
			"error":       pull.errMsg,
			"totalDone":   pull.prog.TotalDone,
			"totalSize":   pull.prog.TotalSize,
			"bytesPerSec": pull.prog.BytesPerSec,
			"fileIndex":   pull.prog.FileIndex,
			"fileCount":   pull.prog.FileCount,
		})
	})

	// Cancel the in-flight download (the .partial is kept for resume).
	api.HandleFunc("POST /native/pull-cancel", func(w http.ResponseWriter, r *http.Request) {
		if selfContained {
			writeJSON(w, map[string]any{"cancelled": false, "disabled": true})
			return
		}
		pull.mu.Lock()
		if pull.cancel != nil {
			pull.cancel()
		}
		pull.mu.Unlock()
		writeJSON(w, map[string]any{"cancelled": true})
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
