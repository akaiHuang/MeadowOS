package coreconnection

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalDetection is inventory/connectivity, not authorization. In particular,
// public health never supplies a trusted CoreID or an inference credential.
type LocalDetection struct {
	Installed          bool   `json:"installed"`
	Running            bool   `json:"running"`
	Paired             bool   `json:"paired"`
	LegacyRuntimeFound bool   `json:"legacyRuntimeFound"`
	State              string `json:"state"`
	Detail             string `json:"detail"`
	BaseURL            string `json:"baseUrl,omitempty"`
}

func DetectLocal(ctx context.Context, pairs []PairedConnection) LocalDetection {
	config, err := os.UserConfigDir()
	if err != nil {
		return LocalDetection{State: "unavailable", Detail: "無法讀取本機安裝位置；仍可使用 Core 連線邀請"}
	}
	home, err := os.UserHomeDir()
	legacy := ""
	if err == nil {
		legacy = filepath.Join(home, ".meadow", "runtime")
	}
	// The diagnostic runtime is inventory only. Never import its owner token,
	// model configuration, memory, or lifecycle authority into portable Desktop.
	return detectLocal(ctx, nil, "http://127.0.0.1:18081/health", filepath.Join(config, "Meadow Core"), legacy, pairs)
}

func detectLocal(ctx context.Context, client *http.Client, healthURL, installation, legacy string, pairs []PairedConnection) LocalDetection {
	result := LocalDetection{Paired: len(pairs) > 0, State: "not-detected", Detail: "未找到可配對的本機 Core；可安裝 Core，或連接既有模型 API"}
	if data, err := readPrivate(installation, filepath.Join(installation, "installation.json"), maxDocument); err == nil {
		var receipt struct {
			Schema      string `json:"schema"`
			Destination string `json:"destination"`
			Status      string `json:"status"`
		}
		if json.Unmarshal(data, &receipt) == nil && receipt.Schema == "meadow.core.installation.v1" && filepath.Clean(receipt.Destination) == filepath.Clean(installation) && receipt.Status == "installed" {
			result.Installed = true
			result.State, result.Detail = "installed-offline", "已找到 Core 安裝記錄，目前尚未連線"
		}
	}
	// An older diagnostic runtime does not imply the standalone installer ran.
	// Report its presence separately; do not claim model readiness or delete it.
	if info, err := os.Lstat(legacy); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && validateOwner(info) == nil {
		if file, err := os.Lstat(filepath.Join(legacy, "run-current-sglang-core.sh")); err == nil && file.Mode().IsRegular() && validateOwner(file) == nil {
			result.LegacyRuntimeFound = true
			if !result.Installed {
				result.State, result.Detail = "legacy-runtime", "找到舊版 Core 啟動設定，目前尚未確認服務；可啟動原服務後使用既有本地 API"
			}
		}
	}
	if result.Paired {
		result.State, result.Detail = "paired-offline", "已找到 Core 配對記錄；驗證連線後即可選擇模型"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	probe := http.Client{Transport: transport, Timeout: 2 * time.Second}
	if client != nil {
		probe = *client
	}
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return result
	}
	response, err := probe.Do(req)
	if err != nil {
		return result
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDocument+1))
	if err != nil || len(data) > maxDocument {
		return result
	}
	var health struct {
		Service string `json:"service"`
	}
	if json.Unmarshal(data, &health) != nil || health.Service != "Meadow Core" {
		return result
	}
	result.Running = true
	result.BaseURL = strings.TrimSuffix(healthURL, "/health") + "/v1"
	result.State, result.Detail = "running-unverified", "偵測到運作中的 Core 服務；請驗證配對，或使用既有本地 API 與金鑰連接"
	// Only an authenticated identity probe of the exact local endpoint upgrades
	// service-reported health to a verified paired connection.
	for _, pair := range pairs {
		if pair.BaseURL == result.BaseURL && Probe(probeCtx, &probe, pair) == nil {
			result.State, result.Detail = "paired-ready", "已確認本機 Core 身分；可選擇並驗證可用模型"
			break
		}
	}
	return result
}
