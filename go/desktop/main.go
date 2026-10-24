//go:build !meadow_protected

// Meadow Desktop (M4): a Wails 3 shell around the same Go core the CLI
// uses. One process replaces the Electron triple (main / Node WS server /
// renderer): the daemon serve contract is mounted as asset-server
// middleware, so the embedded frontend talks plain HTTP/SSE on the same
// origin — no Wails bindings in the hot path, the shell stays swappable.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/hey-meadow/meadow-code/go/core/agentclient"
	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	editioncfg "github.com/hey-meadow/meadow-code/go/core/edition"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/licenseclient"
	licenseproductauth "github.com/hey-meadow/meadow-code/go/core/licenseclient/productauth"
	"github.com/hey-meadow/meadow-code/go/core/localipc"
	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

//go:embed all:frontend
var assets embed.FS

// appIcon is the dock / about-box icon (1024² PNG, source in build/icon.svg).
// On macOS Wails feeds these bytes to NSApp setApplicationIconImage:, so the
// dock shows it under `go run` too — the .icns next to it is for .app bundles
// produced by `wails3 package`.
//
//go:embed build/appicon.png
var appIcon []byte

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

type desktopModelUnloadReason string

const (
	desktopModelUnloadAppQuit  desktopModelUnloadReason = "app_quit"
	desktopModelUnloadManual   desktopModelUnloadReason = "manual_unload"
	desktopModelUnloadSwitch   desktopModelUnloadReason = "model_switch"
	desktopConversationEnd     desktopModelUnloadReason = "conversation_end"
	desktopConversationCancel  desktopModelUnloadReason = "conversation_cancel"
	backendEventRunCancel      string                   = "run.cancel.v1"
	backendEventSteerImmediate string                   = "steering.immediate.v1"
	backendEventPromptNextTurn string                   = "prompt.queued.next-turn.v1"
	backendEventHeader         string                   = "X-Meadow-Backend-Event"
)

func desktopModelUnloadAllowed(reason desktopModelUnloadReason) bool {
	switch reason {
	case desktopModelUnloadAppQuit, desktopModelUnloadManual, desktopModelUnloadSwitch:
		return true
	default:
		return false
	}
}

func desktopSessionAction(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "session" || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	switch parts[2] {
	case "end", "cancel", "steer", "queue":
		return parts[2], true
	default:
		return "", false
	}
}

// desktopConversationBackend gives Desktop conversation controls a closed
// lifecycle contract while preserving agentd as the run-state authority.
// There is intentionally no model lifecycle callback here: ending/cancelling
// a conversation can only reach agentd's active-run cancel route.
func desktopConversationBackend(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		action, ok := desktopSessionAction(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		forward := r.Clone(r.Context())
		forward.Header = r.Header.Clone()
		forwardURL := *r.URL
		forward.URL = &forwardURL
		switch action {
		case "end":
			forward.URL.Path = strings.TrimSuffix(r.URL.Path, "/end") + "/cancel"
			forward.URL.RawPath = ""
			forward.Body = io.NopCloser(bytes.NewReader([]byte("{}")))
			forward.ContentLength = 2
			forward.Header.Set("Content-Type", "application/json")
			forward.Header.Set(backendEventHeader, backendEventRunCancel)
		case "cancel":
			forward.Header.Set(backendEventHeader, backendEventRunCancel)
		case "steer":
			// /steer is delivered synchronously to the active run. It must
			// never pass through Desktop's next-turn prompt queue.
			forward.Header.Set(backendEventHeader, backendEventSteerImmediate)
		case "queue":
			// A normal prompt submitted while busy remains a daemon-owned
			// next-turn item; Desktop does not promote it into steering.
			forward.Header.Set(backendEventHeader, backendEventPromptNextTurn)
		}
		next.ServeHTTP(w, forward)
	})
}

func removedNativeNotification(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusGone)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  "native_notification_removed",
		"detail": "conversation guidance is delivered through the immediate backend steering event",
	})
}

func writeLicenseClientError(w http.ResponseWriter, fallback int, err error) {
	w.Header().Set("Content-Type", "application/json")
	var apiErr licenseclient.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.Status
		if status == 0 {
			status = fallback
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code":    apiErr.Code,
			"message": apiErr.Message,
		}})
		return
	}
	w.WriteHeader(fallback)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
}

// loadEditionConfig keeps Desktop, CLI, and Core on one product profile.
// Explicit process environment always wins; the config only supplies defaults.
func loadEditionConfig() {
	explicit := currentProcessEnvironmentKeys()
	if buildinfo.DesktopPackageMode == "portable" {
		// Portable defaults belong to this bundle. Legacy CWD/user editions
		// and persisted model profiles are not part of its state namespace.
		if executable, err := os.Executable(); err == nil {
			loadEditionEnvFile(filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "Resources", "config", "edition.conf")), explicit)
		}
		return
	}
	selection := strings.TrimSpace(os.Getenv("MEADOW_EDITION_CONFIG"))
	if selection == "" {
		candidates := []string{filepath.Join("config", "edition.conf")}
		if executable, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "Resources", "config", "edition.conf")))
		}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, ".config", "meadow", "edition.conf"))
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				selection = candidate
				break
			}
		}
	}
	if selection == "" {
		return
	}
	loadEditionConfigFiles(selection, explicit)
}

// loadEditionConfigFiles applies a base edition file and then the selected
// profile. Values inherited from the parent process remain protected, while
// values supplied by the base file may be replaced by a persisted profile
// override. This distinction matters after /badai: edition.conf selects the
// standard default, but it must not pin every standard value before skull.env
// has a chance to provide the real Huihui model path.
func loadEditionConfigFiles(selection string, explicit map[string]bool) {
	loadEditionEnvFile(selection, explicit)
	if buildinfo.DesktopPackageMode == "portable" {
		return
	}
	profile := strings.TrimSpace(os.Getenv("MEADOW_EDITION_PROFILE"))
	if override := editioncfg.ReadOverride(); override != "" && !explicit["MEADOW_EDITION_PROFILE"] {
		profile = override
		_ = os.Setenv("MEADOW_EDITION_PROFILE", override)
	}
	if profile == "" || strings.ContainsAny(profile, `/\\`) {
		return
	}
	loadEditionEnvFile(filepath.Join(filepath.Dir(selection), "editions", profile+".env"), explicit)
}

func currentProcessEnvironmentKeys() map[string]bool {
	keys := make(map[string]bool)
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && key != "" {
			keys[key] = true
		}
	}
	return keys
}

func loadEditionEnvFile(path string, protected map[string]bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		if buildinfo.DesktopPackageMode == "portable" && portableBootstrapOwnedEnvironment(key) {
			continue
		}
		// Approval is a user/session choice, never an edition property. Keep an
		// explicit process value intact, but ignore approval defaults embedded
		// in either the base edition file or a model profile.
		if key == "MEADOW_APPROVAL_MODE" {
			continue
		}
		if protected[key] {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		_ = os.Setenv(key, os.ExpandEnv(value))
	}
}

func main() {
	if err := prepareDesktopPortableState(buildinfo.DesktopPackageMode); err != nil {
		log.Fatalf("prepare Desktop state: %v", err)
	}
	explicitEnvironment := currentProcessEnvironmentKeys()
	loadEditionConfig()
	if err := productsecurity.VerifyCurrentBundle(); err != nil {
		log.Fatalf("Meadow product integrity check failed: %v", err)
	}
	connectionRuntime, err := resolveDesktopConnectionStartup(buildinfo.DesktopPackageMode)
	if err != nil {
		log.Fatalf("Meadow model connection could not be restored: %v", err)
	}
	if connectionRuntime == nil {
		if err := restoreDesktopModelProviderStartup(explicitEnvironment); err != nil {
			log.Fatalf("Meadow model provider settings could not be restored: %v", err)
		}
	}
	settings, err := session.LoadSettings()
	if err != nil {
		log.Fatal(err)
	}
	// Account authentication is independent of paid/protected entitlements.
	desktopLicenseRequired := !buildinfo.VMLab
	if buildinfo.VMLab && connectionRuntime == nil {
		log.Fatal("VM lab requires portable API connections")
	}
	licenseClient := licenseclient.FromEnv()
	productAuthClient := licenseproductauth.New()
	loginURLs := &desktopLoginURLRegistry{}
	epBase, epKey, epModel := settings.ModelEndpoint()
	var upstreamProfile desktopUpstreamProfile
	if connectionRuntime != nil {
		epBase, epKey, epModel = connectionRuntime.connection.BaseURL, "", connectionRuntime.selectedModel()
	} else {
		upstreamProfile, err = loadDesktopUpstreamProfile()
		if err != nil {
			log.Fatal(err)
		}
		// Explicit diagnostic builds retain their original Core route.
		epBase, epModel = desktopModelEndpoint(epBase, epModel)
		if value := strings.TrimSpace(os.Getenv("MEADOW_DESKTOP_MODEL_BASE_URL")); value != "" {
			epBase = strings.TrimRight(value, "/")
		}
		if value := strings.TrimSpace(os.Getenv("MEADOW_DESKTOP_MODEL_ID")); value != "" {
			epModel = value
		}
		if upstreamProfile.enabled() {
			epBase, epKey, epModel = defaultDesktopCoreBaseURL, "", upstreamProfile.ModelID
		}
		if epModel == "" {
			epModel = modelmgr.DefaultModelID()
		}
	}
	validateModelChoice := upstreamProfile.validateModelChoice
	if connectionRuntime != nil {
		validateModelChoice = connectionRuntime.validateModel
	}
	var preferredModelMu sync.RWMutex
	preferredModel := epModel
	preferredModelID := func() string {
		preferredModelMu.RLock()
		defer preferredModelMu.RUnlock()
		return preferredModel
	}
	rememberPreferredModel := func(modelID string) {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			return
		}
		preferredModelMu.Lock()
		preferredModel = modelID
		preferredModelMu.Unlock()
	}

	defaultWorkspace, _ := os.UserHomeDir()
	if v := os.Getenv("MEADOW_WORKSPACE"); v != "" {
		defaultWorkspace = v
	}
	automationWorkspace, err := prepareAutomationWorkspace(os.Getenv(automationWorkspaceEnv))
	if err != nil {
		log.Printf("desktop automation workspace disabled: %v", err)
		automationWorkspace = ""
	}
	if automationWorkspace != "" {
		defaultWorkspace = automationWorkspace
	}

	mode := desktopApprovalMode(settings.ApprovalMode())
	harnessMode := desktopHarnessModeOverride()
	// The Desktop is a local presentation client. The signed helper owns the
	// complete Harness, tools, memory and session state; its selected connection
	// supplies inference independently of the optional local Core service.
	browserEmbed := newDesktopBrowserEmbed()
	daemonBinary, daemonErr := daemontransport.BundledDaemonPath()
	if daemonErr != nil {
		log.Fatalf("resolve bundled Meadow Agent: %v", daemonErr)
	}
	launchConfig := daemontransport.LaunchConfig{
		PromptGrants: buildinfo.DesktopPackageMode == "portable",
		BinaryPath:   daemonBinary, WorkspaceRoot: defaultWorkspace,
		CoreBaseURL: epBase, ModelID: epModel, ApprovalMode: string(mode),
		HarnessMode: harnessMode, ExpectedTeamID: strings.TrimSpace(os.Getenv("MEADOW_TEAM_ID")),
	}
	// The embedded Browser is a macOS capability. Linux Desktop attaches to
	// its managed Agent without advertising an unsupported native handler.
	if runtime.GOOS == "darwin" {
		launchConfig.BrowserHandler = newDesktopEmbeddedBrowserHandler(browserEmbed)
	}
	if connectionRuntime != nil {
		launchConfig.ConnectionFile = connectionRuntime.file
		launchConfig.ConnectionID = connectionRuntime.connection.ID
		if len(connectionRuntime.catalog.Connections) > 0 {
			launchConfig.ConnectionFingerprint = connectionRuntime.catalog.Fingerprint()
		}
	}
	daemonSupervisor, daemonErr := daemontransport.EnsureDaemon(context.Background(), launchConfig)
	if daemonErr != nil {
		log.Fatalf("start local Meadow Agent: %v", daemonErr)
	}
	contract, daemonErr := daemontransport.NewReverseProxyHandler()
	if daemonErr != nil {
		log.Fatalf("connect local Meadow Agent: %v", daemonErr)
	}
	modelControl, daemonErr := agentclient.New(agentclient.Config{
		WorkspaceRoot: defaultWorkspace,
		ModelID:       epModel,
		ApprovalMode:  mode,
		HarnessMode:   harnessMode,
	})
	if daemonErr != nil {
		log.Fatalf("connect Meadow Agent model control: %v", daemonErr)
	}
	approvalControl, daemonErr := daemontransport.NewClient()
	if daemonErr != nil {
		log.Fatalf("connect Meadow Agent approval control: %v", daemonErr)
	}
	if connectionRuntime != nil {
		connectionRuntime.discover = desktopAgentModelDiscovery(approvalControl, connectionRuntime.connection)
	}

	// Model availability has ONE source of truth: a live probe of the model
	// server. Supervisor narration (start/restart events) is descriptive
	// detail only — it must never gate the chat. A busy server that misses
	// one probe (26B mid-prefill) keeps its ready state for a sticky window
	// instead of flapping the UI into "not loaded".
	var statusMu sync.Mutex
	modelPhase := "starting" // starting | error — what the supervisor is doing
	modelDetail := "啟動中…"    // supervisor narration, display-only
	modelName := preferredModelID()
	var lastReadyAt time.Time
	setDetail := func(s string) { statusMu.Lock(); modelDetail = s; statusMu.Unlock() }
	setPhase := func(p, detail string) {
		statusMu.Lock()
		modelPhase = p
		if detail != "" {
			modelDetail = detail
		}
		statusMu.Unlock()
	}
	setModelName := func(n string) {
		statusMu.Lock()
		modelName = n
		statusMu.Unlock()
	}
	clearReadyStick := func() { statusMu.Lock(); lastReadyAt = time.Time{}; statusMu.Unlock() }
	const readyStick = 45 * time.Second
	var bundledCore *desktopCoreSupervisor
	if connectionRuntime == nil && isMeadowCoreBaseURL(epBase) {
		bundledCore = newDesktopCoreSupervisor(epBase)
	}

	api := http.NewServeMux()
	// The Browser drawer has a narrow, loopback-only bridge to the bundled
	// Meadow Browser daemon. It is intentionally separate from the Agent's
	// protected browser transport and never proxies arbitrary remote hosts.
	api.Handle("/browser/", newDesktopBrowserDrawerFromEnvironment())
	registerDesktopBrowserEmbed(api, browserEmbed)
	startDefaultModel := func() {}
	conversationBackend := desktopConversationBackend(contract)
	api.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The local Agent may outlive a Desktop update, and Core may outlive
		// both.  Before a real model turn, verify that the resident Core belongs
		// to this edition and artifact.  This does not inspect or steer the
		// prompt; it only prevents a skull/Huihui conversation from silently
		// running on a stale standard 35B process.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt") && bundledCore != nil {
			if err := bundledCore.EnsureRunning(r.Context()); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
		}
		conversationBackend.ServeHTTP(w, r)
	}))
	// Model lifecycle uses the typed Agent client instead of the generic
	// reverse proxy. This validates and re-encodes the selected id, preventing
	// WebKit/Wails request-body quirks from turning a valid local model choice
	// into an empty `{}` request at the daemon boundary.
	api.HandleFunc("POST /agent/model", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Model) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "model is required"})
			return
		}
		modelID := strings.TrimSpace(body.Model)
		if err := validateModelChoice(modelID); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		result, err := activateDesktopModel(r.Context(), bundledCore, modelControl, modelID)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		rememberPreferredModel(modelID)
		_ = json.NewEncoder(w).Encode(result)
	})
	api.HandleFunc("GET /agent/model", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status, err := modelControl.Status(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"ready": false, "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	})
	api.HandleFunc("DELETE /agent/model", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		result, err := modelControl.UnloadModel(r.Context(), preferredModelID())
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	})
	api.HandleFunc("POST /session/{id}/approval", func(w http.ResponseWriter, r *http.Request) {
		mode := strings.TrimSpace(r.Header.Get("X-Meadow-Approval-Mode"))
		if mode == "" {
			var body struct {
				Mode string `json:"mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				mode = strings.TrimSpace(body.Mode)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if mode == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "mode required"})
			return
		}
		sessionID := strings.TrimSpace(r.PathValue("id"))
		if sessionID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "session id required"})
			return
		}
		requestContext, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		forward, err := approvalControl.NewRequest(
			requestContext,
			http.MethodPost,
			"/session/"+url.PathEscape(sessionID)+"/approval",
			map[string]string{"mode": mode},
		)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		response, err := approvalControl.HTTP.Do(forward)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		defer response.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(payload)
	})
	api.HandleFunc("POST /session/{id}/model", func(w http.ResponseWriter, r *http.Request) {
		modelID := strings.TrimSpace(r.Header.Get("X-Meadow-Model-ID"))
		if modelID == "" {
			var body struct {
				ModelID string `json:"modelId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				modelID = strings.TrimSpace(body.ModelID)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if modelID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "modelId required"})
			return
		}
		if err := validateModelChoice(modelID); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		sessionID := strings.TrimSpace(r.PathValue("id"))
		if sessionID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "session id required"})
			return
		}
		requestContext, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		forward, err := approvalControl.NewRequest(
			requestContext,
			http.MethodPost,
			"/session/"+url.PathEscape(sessionID)+"/model",
			map[string]string{"modelId": modelID},
		)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		response, err := approvalControl.HTTP.Do(forward)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		defer response.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(payload)
	})
	api.HandleFunc("GET /agent/client/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(desktopClientCapabilities(connectionRuntime != nil))
	})
	api.HandleFunc("GET /workspace/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"automation": automationWorkspace != "",
			"path":       automationWorkspace,
		})
	})
	accountLost := func() {
		if connectionRuntime != nil {
			connectionRuntime.invalidateDiscovery()
		}
		desktopCloseBrowserForAccountLoss(browserEmbed)
	}
	if buildinfo.VMLab {
		registerVMLabAccountRoutes(api, productAuthClient)
	} else {
		registerDesktopProductAuthRoutesWithLifecycle(api, productAuthClient, func() { startDefaultModel() }, accountLost, loginURLs)
	}
	/* legacy membership routes are intentionally disabled; product authorization
	   is available only through the canonical local-daemon routes above.
	api.HandleFunc("GET /license/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(licenseClient.Status(r.Context()))
	})
	api.HandleFunc("POST /license/request-code", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email string `json:"email"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		out, err := licenseClient.RequestCode(r.Context(), body.Email)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		go startDefaultModel()
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("POST /license/verify-code", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email string `json:"email"`
			Code  string `json:"code"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		out, err := licenseClient.VerifyCode(r.Context(), body.Email, body.Code)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		go startDefaultModel()
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("POST /license/activate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out, err := licenseClient.Activate(r.Context(), "")
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		go startDefaultModel()
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("POST /license/activate-key", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LicenseKey string `json:"license_key"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		out, err := licenseClient.ActivateLicenseKey(r.Context(), body.LicenseKey, "")
		if err != nil {
			writeLicenseClientError(w, http.StatusBadGateway, err)
			return
		}
		go startDefaultModel()
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("GET /license/current-key", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out, err := licenseClient.CurrentLicenseKey(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("GET /license/devices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out, err := licenseClient.Devices(r.Context(), "")
		if err != nil {
			writeLicenseClientError(w, http.StatusBadGateway, err)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("POST /license/devices", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LicenseKey string `json:"license_key"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		out, err := licenseClient.Devices(r.Context(), body.LicenseKey)
		if err != nil {
			writeLicenseClientError(w, http.StatusBadGateway, err)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("POST /license/devices/revoke", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LicenseKey string `json:"license_key"`
			DeviceID   string `json:"device_id"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		if err := licenseClient.RevokeDevice(r.Context(), body.DeviceID, body.LicenseKey); err != nil {
			writeLicenseClientError(w, http.StatusBadGateway, err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	api.HandleFunc("POST /license/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out, err := licenseClient.Refresh(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		go startDefaultModel()
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("POST /license/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := licenseclient.Logout(); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	*/
	api.HandleFunc("POST /license/paypal/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Plan string `json:"plan"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		out, err := licenseClient.CreatePayPalCheckout(r.Context(), body.Plan)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		if _, err := productAuthClient.Refresh(r.Context()); err != nil {
			writeDesktopProductAuthResult(w, nil, err)
			return
		}
		go startDefaultModel()
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("POST /license/paypal/capture", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OrderID string `json:"order_id"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		out, err := licenseClient.CapturePayPalOrder(r.Context(), body.OrderID)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		go startDefaultModel()
		_ = json.NewEncoder(w).Encode(out)
	})
	api.HandleFunc("GET /model/status", func(w http.ResponseWriter, r *http.Request) {
		// Live probe decides readiness; phase/detail only describe why when
		// the server is not reachable.
		pctx, pcancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
		ready := false
		liveModel := ""
		var perr error
		if isMeadowCoreBaseURL(epBase) {
			identityProbe := probeMeadowCore(pctx, bundledCore.client, epBase, currentDesiredCoreIdentity())
			if upstreamProfile.enabled() && identityProbe.capability != coreCompatible {
				// Polling is observation only. The explicit prompt/activation
				// path checks ownership and starts the requested Core profile.
				perr = identityProbe.err
				setPhase("starting", "模型 API 尚未連線；送出訊息時會啟動 Meadow Core")
			} else if identityProbe.capability == coreStale {
				perr = identityProbe.err
				setPhase("starting", "正在切換至此版本指定的 Huihui 模型…")
				bundledCore.EnsureRunningBackground(context.Background(), func(err error) {
					if err != nil {
						setPhase("error", "Meadow Core 切換失敗："+err.Error())
						return
					}
					clearReadyStick()
					setPhase("starting", "Huihui Core 已就緒，將在你送出訊息時載入模型")
				})
			} else {
				liveModel, ready, perr = probeMeadowCoreStatus(pctx, epBase)
			}
		} else {
			var served []string
			served, perr = provider.NewClient(epBase, epKey, "").Models(pctx)
			if perr == nil && len(served) > 0 {
				ready = true
				liveModel = served[0]
			}
		}
		pcancel()
		if upstreamProfile.enabled() && !upstreamProfile.allows(liveModel) {
			ready = false
		}
		statusMu.Lock()
		if perr == nil && ready {
			if liveModel != "" {
				modelName = liveModel
			}
			lastReadyAt = time.Now()
		}
		if !upstreamProfile.enabled() && !ready && !lastReadyAt.IsZero() && time.Since(lastReadyAt) < readyStick {
			ready = true // busy server, not a dead one — keep the chat open
		}
		out := map[string]string{"model": modelName, "detail": modelDetail}
		if ready {
			out["status"] = "ready"
		} else {
			out["status"] = modelPhase // starting | error
			// The selected preference is not a resident model. Keep the idle
			// status honest so Desktop does not present cold startup as a model
			// restoration. The first prompt will activate the session's model.
			out["model"] = ""
		}
		statusMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	// Models the user can pick in the composer: live-detect whatever the
	// local server is serving right now (marked active + live) and append
	// the settings catalog after it.
	api.HandleFunc("GET /model/list", func(w http.ResponseWriter, r *http.Request) {
		// Short probe: the picker paints from the catalog instantly and the
		// live badge arrives async — a dead server must not hold the modal
		// hostage for 2s (user report).
		lctx, lcancel := context.WithTimeout(r.Context(), 600*time.Millisecond)
		if upstreamProfile.enabled() {
			defer lcancel()
			snapshot := meadowCoreSnapshot{}
			if identity := probeMeadowCore(lctx, bundledCore.client, epBase, currentDesiredCoreIdentity()); identity.capability == coreCompatible {
				snapshot, _ = probeMeadowCoreSnapshot(lctx, epBase)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(upstreamProfile.modelList(snapshot))
			return
		}
		var served []string
		var installed []string
		if isMeadowCoreBaseURL(epBase) {
			// /v1/models is Core's allowlist, not the resident engine. Only the
			// model reported ready by /v1/status may receive the live badge.
			if snapshot, err := probeMeadowCoreSnapshot(lctx, epBase); err == nil {
				installed = snapshot.Installed
				if snapshot.Ready {
					served = []string{snapshot.ModelID}
				}
			}
		} else {
			served, _ = provider.NewClient(epBase, epKey, "").Models(lctx)
		}
		lcancel()
		if len(installed) == 0 {
			// Cold startup intentionally leaves Core offline. The signed edition
			// inventory remains authoritative in that state, so an old settings
			// cache cannot revive retired rows such as 26B before the first prompt.
			installed = configuredInstalledModels()
		}
		out := mergeModelListWithInstalled(
			served,
			installed,
			settings.Raw,
			epModel,
			// Keep the five-tier picker stable on every supported Mac. Machine
			// policy still guards the actual load/download operation; it must not
			// silently hide a row or renumber Command+1..5.
			modelmgr.KnownModels(),
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	var settingsGuardedAPI http.Handler = api
	if connectionRuntime != nil {
		providerSettings := newDesktopModelConnectionSettings(approvalControl, connectionRuntime.file)
		providerSettings.bindRuntime(connectionRuntime, approvalControl, rememberPreferredModel)
		if !buildinfo.VMLab {
			providerSettings.setManagedAccount(productAuthClient)
		}
		providerSettings.register(api)
		settingsGuardedAPI = connectionRuntime.guard(api, modelControl, rememberPreferredModel)
		settingsGuardedAPI = providerSettings.guard(settingsGuardedAPI)
	} else {
		if upstreamProfile.enabled() {
			upstreamProfile.registerCatalogHandlers(api)
		} else {
			registerPullHandlers(api, buildinfo.DesktopBundledModels()...)
		}
		registerProviderCatalogHandlers(api)
		providerSettings := newDesktopModelProviderSettings(approvalControl)
		providerSettings.register(api)
		settingsGuardedAPI = providerSettings.guard(api)
	}
	if !buildinfo.VMLab {
		settingsGuardedAPI = desktopRequireAccountWithLifecycle(settingsGuardedAPI, productAuthClient, accountLost, loginURLs)
	}

	app := application.New(application.Options{
		Name:        buildinfo.DesktopAboutLabel(),
		Description: "Meadow Desktop · Browser 與 AI Harness\n© 2026 Hey-Meadow Lab",
		Icon:        appIcon, // dock + about box (NSApp setApplicationIconImage:)
		KeyBindings: desktopComposerKeyBindings(),
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(mustSub(assets, "frontend")),
			Middleware: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// WKWebView can retain ES modules across an in-place app upgrade even
					// when the signed bundle changed.  The Desktop frontend shares live
					// session state between its modules, so a cached core/commercial module
					// combined with a new composer can leave Send permanently inert.  Keep
					// the small local shell fresh; models and browser artifacts are served
					// by separate endpoints and are unaffected.
					if strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") || r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html") {
						w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
					}
					// Wails' embedded runtime probes this optional script on every
					// launch, but alpha2.117 deliberately answers with 404 outside
					// server mode.  Serve a valid empty script so Safari's inspector
					// stays free of a misleading missing-application-asset error.
					if serveVMLabBuildProfile(w, r) {
						return
					}
					if r.URL.Path == "/wails/custom.js" {
						w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
						w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
						if r.Method != http.MethodHead {
							_, _ = w.Write([]byte("// Meadow Desktop intentionally has no Wails custom runtime.\n"))
						}
						return
					}
					if isAPIPath(r.URL.Path) {
						settingsGuardedAPI.ServeHTTP(w, r)
						return
					}
					// Desktop assets are embedded in each build.  WebKit otherwise
					// reuses JS from a previously installed app with the same wails://
					// URL, which can resurrect retired model-download and Core startup
					// flows after an upgrade.
					w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
					w.Header().Set("Pragma", "no-cache")
					next.ServeHTTP(w, r)
				})
			},
		},
		Mac: application.MacOptions{
			// Closing the document window is a hide/minimize action. The process,
			// Core and Dock icon stay alive so an in-flight session is not killed.
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})
	// Native folder picker, exposed as an HTTP endpoint so the frontend
	// stays free of Wails bindings (the shell remains swappable). Needs the
	// app handle, so it registers after application.New.
	accountWatchContext, cancelAccountWatch := context.WithCancel(context.Background())
	var accountWatchOnce sync.Once
	api.HandleFunc("POST /native/save-artifact", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			CWD          string `json:"cwd"`
			SessionID    string `json:"sessionId"`
			RelativePath string `json:"relativePath"`
		}
		w.Header().Set("Content-Type", "application/json")
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid artifact request"})
			return
		}
		request, err := approvalControl.NewRequest(r.Context(), http.MethodPost, "/workspace/artifact/download", body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		response, err := approvalControl.HTTP.Do(request)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, response.Body)
			return
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
		if err != nil || len(data) > 64<<20 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		path, err := app.Dialog.SaveFile().SetMessage("儲存完整檔案").SetFilename(filepath.Base(body.RelativePath)).PromptForSingleSelection()
		if err != nil || path == "" {
			_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": true})
			return
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "儲存失敗"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"saved": true})
	})
	api.HandleFunc("POST /native/pick-folder", func(w http.ResponseWriter, r *http.Request) {
		dialog := app.Dialog.OpenFile().
			CanChooseDirectories(true).
			CanChooseFiles(false).
			SetTitle("選擇工作區資料夾")
		path, err := dialog.PromptForSingleSelection()
		w.Header().Set("Content-Type", "application/json")
		if err != nil || path == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": true})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"path": path})
	})

	// Known editor detection order also determines the default application.
	platformActions := newDesktopPlatformActions()
	api.HandleFunc("GET /native/editors", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"editors": platformActions.editors()})
	})
	api.HandleFunc("POST /native/open-editor", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path   string `json:"path"`
			Editor string `json:"editor"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Path) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "path required"})
			return
		}
		editor, err := platformActions.openEditor(body.Path, body.Editor)
		if err != nil {
			w.WriteHeader(desktopActionHTTPStatus(err))
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "editor": editor})
	})
	api.HandleFunc("POST /native/reveal-path", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			CWD       string `json:"cwd"`
			SessionID string `json:"sessionId"`
			Path      string `json:"path"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid reveal request"})
			return
		}
		path, err := revealHostPath(body.CWD, body.SessionID, body.Path)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		revealed, err := platformActions.reveal(path)
		if err != nil {
			w.WriteHeader(desktopActionHTTPStatus(err))
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": path, "revealed": revealed})
	})
	api.HandleFunc("POST /native/open-url", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "url required"})
			return
		}
		if err := platformActions.openURL(body.URL); err != nil {
			w.WriteHeader(desktopActionHTTPStatus(err))
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	// Native notifications were previously an AppleScript side channel. User
	// guidance now has one path only: agentd's authenticated /steer event.
	api.HandleFunc("POST /native/notify", removedNativeNotification)

	// Model lifecycle: bring vllm-mlx up in the background and keep it up.
	// The supervisor is swappable: /native/load-model (below) stops the
	// current server and brings up the picked model through the same
	// starting → ready status flow the boot path uses.
	var (
		mgrMu         sync.Mutex
		mgr           *modelmgr.Manager
		mgrCancel     context.CancelFunc
		loadingTarget string // model id being brought up; "" = idle
	)
	// startManager swaps in a fresh supervisor for id. Call with mgrMu held.
	// explicit marks a user pick from the model picker: a pick that doesn't
	// match the settings.json modelServer recipe drops the recipe, while the
	// boot path always honors it (that is the user's hand-tuned launch).
	startManager := func(id string, explicit bool) {
		if connectionRuntime != nil {
			return
		}
		if isMeadowCoreBaseURL(epBase) {
			target := strings.TrimSpace(id)
			if info, ok := modelmgr.ModelByName(target); ok {
				target = info.ServedName
			}
			if target == "" {
				target = preferredModelID()
			}
			setModelName(target)
			setPhase("starting", "正在啟動 Meadow Core…")
			if bundledCore == nil {
				setPhase("error", "Meadow Core supervisor 未初始化")
				return
			}
			loadingTarget = target
			go func(target string) {
				lifecycleContext := context.Background()
				done := func() {
					mgrMu.Lock()
					if loadingTarget == target {
						loadingTarget = ""
					}
					mgrMu.Unlock()
				}
				if err := bundledCore.EnsureRunning(lifecycleContext); err != nil {
					setPhase("error", "Meadow Core 啟動失敗："+err.Error())
					done()
					return
				}
				pctx, pcancel := context.WithTimeout(lifecycleContext, 3*time.Second)
				liveModel, ready, _ := probeMeadowCoreStatus(pctx, epBase)
				pcancel()
				if ready && strings.EqualFold(strings.TrimSpace(liveModel), strings.TrimSpace(target)) {
					setModelName(liveModel)
					setPhase("starting", "Meadow Core 模型就緒")
					done()
					return
				}
				setPhase("starting", "Meadow Core 已連線，正在恢復模型…")
				ctx, cancel := context.WithTimeout(lifecycleContext, 10*time.Minute)
				defer cancel()
				result, err := provider.ActivateMeadowCoreModel(ctx, epBase, epKey, target)
				if err == nil && strings.TrimSpace(result.Model) != "" &&
					!strings.EqualFold(strings.TrimSpace(result.Model), target) {
					err = fmt.Errorf("Meadow Core 載入了 %q，而不是要求的 %q", result.Model, target)
				}
				if err != nil {
					setPhase("error", "Meadow Core 載入失敗："+err.Error())
					done()
					return
				}
				setModelName(target)
				setPhase("starting", "Meadow Core 模型就緒")
				done()
			}(target)
			return
		}
		launch := modelmgr.ParseLaunchSettings(settings.ModelServer())
		if explicit && !launch.Matches(id) {
			launch = nil
		}
		cfg := modelmgr.ConfigFromSettings(epBase, id, launch)
		if launch == nil && strings.Contains(id, "/") {
			cfg.ModelPath = id // explicit pick beats the MEADOW_MODEL_PATH override
		}
		// picker 選的模型用 catalog 顯示名 serve（避免 manager 預設 26B 名）
		if launch == nil {
			providers, _ := settings.Raw["modelProviders"].(map[string]any)
			if nm, _ := catalogNameDesc(providers, id); nm != "" && nm != id {
				cfg.ServedName = nm
			}
		}
		plan := ""
		if st, err := productAuthClient.Status(context.Background()); err == nil && st.Authenticated {
			plan = string(st.Plan)
		}
		cfg.ApplyPlanRuntime(plan, launch == nil && !strings.Contains(id, "/"))
		m := modelmgr.NewManager(cfg)
		m.OnEvent = setDetail // narration only; readiness comes from the probe
		ctx, cancel := context.WithCancel(app.Context())
		mgr, mgrCancel = m, cancel
		setPhase("starting", "啟動模型伺服器…")
		go func() {
			done := func() {
				mgrMu.Lock()
				if loadingTarget == id {
					loadingTarget = ""
				}
				mgrMu.Unlock()
			}
			if err := m.EnsureRunning(ctx); err != nil {
				setPhase("error", "模型伺服器啟動失敗："+err.Error())
				done()
				return
			}
			// Report the actually-served name (a path id serves under
			// --served-model-name); the picker switches sessions to it.
			name := id
			rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
			if served, err := provider.NewClient(epBase, epKey, "").Models(rctx); err == nil && len(served) > 0 {
				name = served[0]
			}
			rcancel()
			setModelName(name)
			upstreamProfile.persistReadyModel(preferredReadyModel(id, name))
			setPhase("starting", "模型伺服器就緒") // probe flips status to ready
			done()
			m.Watch(ctx)
		}()
	}
	startDefaultModel = func() {
		if connectionRuntime != nil || isMeadowCoreBaseURL(epBase) {
			// Meadow Agent is the sole Core/model lifecycle owner.
			return
		}
		if desktopLicenseRequired && !desktopProductAuthorized(context.Background(), productAuthClient) {
			return
		}
		mgrMu.Lock()
		defer mgrMu.Unlock()
		if mgr != nil || loadingTarget != "" {
			return
		}
		clearReadyStick()
		startManager(preferredModelID(), false)
	}
	if connectionRuntime == nil && !isMeadowCoreBaseURL(epBase) && (!desktopLicenseRequired || desktopProductAuthorized(context.Background(), productAuthClient)) {
		mgrMu.Lock()
		startManager(epModel, false)
		mgrMu.Unlock()
	} else if connectionRuntime == nil && !isMeadowCoreBaseURL(epBase) {
		setPhase("error", "請先登入 Meadow 帳號開始試用或同步授權")
	}

	unloadModel := func(reason desktopModelUnloadReason) bool {
		if connectionRuntime != nil {
			return true
		}
		if !desktopModelUnloadAllowed(reason) {
			// Conversation lifecycle never owns model lifecycle. Fail closed as
			// an idempotent no-op and leave the resident model untouched.
			return true
		}
		killExternal := reason == desktopModelUnloadManual
		mgrMu.Lock()
		if mgrCancel != nil {
			mgrCancel()
			mgrCancel = nil
		}
		old := mgr
		mgr = nil
		loadingTarget = ""
		mgrMu.Unlock()

		if old != nil {
			old.Stop()
		}
		if isMeadowCoreBaseURL(epBase) {
			stopped := false
			if bundledCore != nil && killExternal {
				stopped = bundledCore.StopOwned()
			}
			clearReadyStick()
			setPhase("starting", "模型已卸載")
			return !stopped
		}
		if !killExternal {
			clearReadyStick()
			setPhase("starting", "模型已卸載")
			return old == nil
		}

		port := epPort(epBase)
		out, _ := exec.Command("lsof", "-ti", ":"+port, "-sTCP:LISTEN").Output()
		pids := strings.Fields(string(out))
		if len(pids) == 0 {
			clearReadyStick()
			setPhase("starting", "模型已卸載")
			return old == nil
		}
		for _, pid := range pids {
			_ = exec.Command("kill", pid).Run()
		}
		for i := 0; i < 10; i++ {
			chk, _ := exec.Command("lsof", "-ti", ":"+port, "-sTCP:LISTEN").Output()
			if len(strings.Fields(string(chk))) == 0 {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		clearReadyStick()
		setPhase("starting", "模型已卸載")
		return false
	}
	app.OnShutdown(func() {
		cancelAccountWatch()
		if connectionRuntime != nil {
			connectionRuntime.invalidateDiscovery()
		}
		browserEmbed.revokeAgentOwner()
		// Portable Main must remain alive until its exact owned helper exits.
		// A signal-only shutdown can orphan a helper stuck in a native call.
		// Legacy embedded Core keeps its existing nonblocking shutdown path.
		if err := stopDesktopAgentOnShutdown(buildinfo.DesktopPackageMode, daemonSupervisor); err != nil {
			log.Printf("stop owned Meadow Agent during shutdown: %v", err)
		}
		if bundledCore != nil {
			bundledCore.SignalOwned()
		}
		mgrMu.Lock()
		if mgrCancel != nil {
			mgrCancel()
			mgrCancel = nil
		}
		old := mgr
		mgr = nil
		loadingTarget = ""
		mgrMu.Unlock()
		if old != nil {
			go old.Stop()
		}
	})

	api.Handle("POST /native/edition", desktopEditionSwitch(nil, func() {
		if daemonSupervisor != nil && daemonSupervisor.Owned() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = daemonSupervisor.Stop(ctx)
			cancel()
		}
		executable, err := os.Executable()
		if err == nil {
			// Relaunch with edition-owned environment removed. The persisted
			// /goodai or /badai override becomes the only source of truth.
			_ = editionRestartCommand(executable, os.Environ()).Start()
		}
		app.Quit()
	}))

	// Cold-model load: tear down the supervised server (foreign servers are
	// never touched) and supervise the picked model instead. Returns at
	// once; the frontend follows progress by polling GET /model/status.
	api.HandleFunc("POST /native/load-model", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ModelID string `json:"modelId"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ModelID) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "modelId required"})
			return
		}
		target := strings.TrimSpace(body.ModelID)
		if err := validateModelChoice(target); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		if isMeadowCoreBaseURL(epBase) {
			if !upstreamProfile.enabled() {
				info, ok := modelmgr.ModelByName(target)
				if !ok {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": "Meadow Core 不支援此模型"})
					return
				}
				target = info.ServedName
			}
			result, err := activateDesktopModel(r.Context(), bundledCore, modelControl, target)
			if err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
				return
			}
			rememberPreferredModel(target)
			upstreamProfile.persistReadyModel(target)
			setModelName(target)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "loading": false, "model": result.Model})
			return
		}
		if isMeadowCoreBaseURL(epBase) {
			info, ok := modelmgr.ModelByName(target)
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "Meadow Core 不支援此模型"})
				return
			}
			target = info.ServedName

			// Core's /v1/models is an allowlist, not proof that weights are
			// resident. Only an explicit ready status may short-circuit loading.
			pctx, pcancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
			liveModel, ready, _ := probeMeadowCoreStatus(pctx, epBase)
			pcancel()
			if ready && strings.EqualFold(strings.TrimSpace(liveModel), target) {
				rememberPreferredModel(target)
				upstreamProfile.persistReadyModel(target)
				setModelName(target)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "loading": false})
				return
			}

			mgrMu.Lock()
			if loadingTarget != "" {
				same := loadingTarget == target
				mgrMu.Unlock()
				if same {
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "loading": true})
					return
				}
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "另一個模型正在載入中"})
				return
			}
			loadingTarget = target
			mgrMu.Unlock()

			if bundledCore == nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "Meadow Core supervisor 未初始化"})
				return
			}
			if err := bundledCore.EnsureRunning(r.Context()); err != nil {
				w.WriteHeader(http.StatusBadGateway)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
				return
			}
			clearReadyStick()
			rememberPreferredModel(target)
			upstreamProfile.persistReadyModel(target)
			setModelName(target)
			setPhase("starting", "正在啟動 Meadow Core 並載入模型…")
			go func() {
				ctx, cancel := context.WithTimeout(app.Context(), 10*time.Minute)
				defer cancel()
				result, err := provider.ActivateMeadowCoreModel(ctx, epBase, epKey, target)
				if err == nil && strings.TrimSpace(result.Model) != "" &&
					!strings.EqualFold(strings.TrimSpace(result.Model), target) {
					err = fmt.Errorf("Meadow Core 載入了 %q，而不是要求的 %q", result.Model, target)
				}
				mgrMu.Lock()
				if loadingTarget == target {
					loadingTarget = ""
				}
				mgrMu.Unlock()
				if err != nil {
					setPhase("error", "Meadow Core 載入失敗："+err.Error())
					return
				}
				setModelName(target)
				setPhase("starting", "Meadow Core 模型就緒") // live probe flips ready
			}()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "loading": true})
			return
		}

		mgrMu.Lock()
		if loadingTarget != "" {
			same := loadingTarget == target
			mgrMu.Unlock()
			if same { // popover reopened mid-load: re-attach, don't restart
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "loading": true})
				return
			}
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "另一個模型正在載入中"})
			return
		}
		curPath := ""
		if mgr != nil {
			curPath = mgr.Cfg.ModelPath
		}
		mgrMu.Unlock()

		// Already live? Served-name match, or the exact path the current
		// supervisor is serving while the server answers.
		lctx, lcancel := context.WithTimeout(r.Context(), 2*time.Second)
		served, _ := provider.NewClient(epBase, epKey, "").Models(lctx)
		lcancel()
		for _, id := range served {
			if id == target {
				rememberPreferredModel(target)
				upstreamProfile.persistReadyModel(target)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "loading": false})
				return
			}
		}
		if len(served) > 0 && curPath != "" && curPath == target {
			rememberPreferredModel(target)
			upstreamProfile.persistReadyModel(target)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "loading": false})
			return
		}

		mgrMu.Lock()
		if loadingTarget != "" { // raced with a concurrent load-model call
			same := loadingTarget == target
			mgrMu.Unlock()
			if same {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "loading": true})
				return
			}
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "另一個模型正在載入中"})
			return
		}
		loadingTarget = target
		old, oldCancel := mgr, mgrCancel
		mgr, mgrCancel = nil, nil
		mgrMu.Unlock()

		// Drop the sticky-ready window: a deliberate model swap means the
		// old server is going away and the gate should reflect loading.
		statusMu.Lock()
		lastReadyAt = time.Time{}
		statusMu.Unlock()
		setPhase("starting", "載入模型中…")
		go func() {
			// Cancel the old watch first so it cannot restart the old
			// model, then kill the process group and wait for the port to
			// come free before the new spawn claims it.
			if oldCancel != nil {
				oldCancel()
			}
			if old != nil {
				old.Stop()
				deadline := time.Now().Add(10 * time.Second)
				for old.Healthy(context.Background()) && time.Now().Before(deadline) {
					time.Sleep(500 * time.Millisecond)
				}
			}
			mgrMu.Lock()
			startManager(target, true)
			mgrMu.Unlock()
		}()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "loading": true})
	})

	// 卸載模型釋放記憶體（model picker 的 ✕，用戶回饋）。受管 server 走
	// supervisor Stop；用戶手動開的（mgr 沒管）按 endpoint port 找進程送 TERM。
	api.HandleFunc("POST /native/unload-model", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if isMeadowCoreBaseURL(epBase) {
			result, err := modelControl.UnloadModel(r.Context(), preferredModelID())
			if err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(result)
			return
		}
		already := unloadModel(desktopModelUnloadManual)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "already": already})
	})

	// 記憶體用量（checkoutRow 的 memPill）：總量 / 模型 RSS / 其他已用。
	api.HandleFunc("GET /native/memstats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(memStats(epBase))
	})

	hoverQABuild := strings.Contains(strings.ToLower(filepath.Base(os.Args[0])), "hover qa")
	hoverDebug := os.Getenv("MEADOW_DESKTOP_HOVER_DEBUG") == "1" || hoverQABuild
	if hoverDebug {
		api.HandleFunc("POST /native/hover-debug", func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
			var entry map[string]any
			if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
				http.Error(w, "invalid hover trace", http.StatusBadRequest)
				return
			}
			encoded, _ := json.Marshal(entry)
			log.Printf("[hover-debug] %s", encoded)
			w.WriteHeader(http.StatusNoContent)
		})
	}
	windowURL := "/"
	windowTitle := "Meadow"
	if buildinfo.VMLab {
		windowTitle = "Meadow VM Lab · 免登入測試版"
		if buildinfo.DesktopProductName == "MeadowOS" {
			windowTitle = "MeadowOS · 開發版"
		}
	}
	if hoverDebug {
		windowURL = "/?hoverDebug=1"
		windowTitle = "Meadow — Hover Debug Build"
		if hoverQABuild {
			// QA opens the first level without a pointer action. The native test
			// can then move a buttons=0 cursor into the model row and prove the
			// second level is hover-only, with no click in the event sequence.
			windowURL += "&autoHover=1"
		}
	}
	mainWindow := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:                  windowTitle,
		Width:                  1180,
		Height:                 780,
		MinWidth:               760,
		MinHeight:              520,
		EnableFileDrop:         true,
		DevToolsEnabled:        true,
		OpenInspectorOnStartup: hoverDebug,
		BackgroundColour:       application.NewRGB(12, 18, 16), // meadow theme bg
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 40,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		URL: windowURL,
	})
	mainWindow.OnWindowEvent(events.Common.WindowShow, func(_ *application.WindowEvent) {
		enableDesktopWebInspector()
		accountWatchOnce.Do(func() {
			if !buildinfo.VMLab {
				startDesktopAccountWatcher(accountWatchContext, productAuthClient, 30*time.Second, accountLost)
			}
		})
	})
	registerNativeImageDrop(mainWindow, productAuthClient)
	browserEmbed.Attach(mainWindow)
	if liveBrowserDrawer != nil {
		liveBrowserDrawer.SetEmbed(browserEmbed)
	}
	menu := application.NewMenu()
	menu.AddRole(application.AppMenu)
	menu.AddRole(application.EditMenu)
	addDesktopComposerMenu(menu, mainWindow)
	menu.AddRole(application.WindowMenu)
	app.Menu.SetApplicationMenu(menu)
	mainWindow.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		mainWindow.Hide()
		event.Cancel()
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

func isMeadowCoreBaseURL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return parsed.Port() == "18081" && (host == "127.0.0.1" || host == "localhost" || host == "::1")
}

const (
	defaultDesktopCoreBaseURL = "http://127.0.0.1:18081/v1"
	defaultDesktopCoreModel   = "Meadow Coder 35B"
)

func desktopModelEndpoint(configuredBaseURL, configuredModel string) (string, string) {
	if desktopUsesUpstreamBackend() {
		profile, err := loadDesktopUpstreamProfile()
		if err != nil {
			return defaultDesktopCoreBaseURL, ""
		}
		return defaultDesktopCoreBaseURL, profile.ModelID
	}
	baseURL := desktopModelBaseURL(configuredBaseURL)
	if isMeadowCoreBaseURL(baseURL) {
		// Keep only the current 35B release preference. Retired sessions remain
		// readable, but new work must migrate to the supported default.
		if info, ok := modelmgr.ModelByName(configuredModel); ok {
			switch info.ServedName {
			case "Meadow Coder 35B":
				return baseURL, info.ServedName
			}
		}
		// Retired rows remain resolvable for old sessions, but a fresh public
		// Core no longer serves them. Do not let stale 12B/26B/27B/30B settings create a
		// session whose composer and backend disagree before any model is loaded.
		return baseURL, defaultDesktopCoreModel
	}
	return baseURL, configuredModel
}

func desktopModelBaseURL(configured string) string {
	parsed, err := url.Parse(strings.TrimSpace(configured))
	if err != nil {
		return configured
	}
	host := strings.ToLower(parsed.Hostname())
	loopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if loopback && (parsed.Port() == "8080" || parsed.Port() == "8081") &&
		strings.TrimSuffix(parsed.EscapedPath(), "/") == "/v1" {
		return defaultDesktopCoreBaseURL
	}
	return configured
}

type meadowCoreSnapshot struct {
	ModelID    string
	Ready      bool
	Installed  []string
	Multimodal bool
}

func newMeadowCoreStatusRequest(ctx context.Context, baseURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/status", nil)
	if err != nil {
		return nil, err
	}
	if key := strings.TrimSpace(localipc.CoreAPIKey(baseURL, "")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

func probeMeadowCoreSnapshot(ctx context.Context, baseURL string) (meadowCoreSnapshot, error) {
	req, err := newMeadowCoreStatusRequest(ctx, baseURL)
	if err != nil {
		return meadowCoreSnapshot{}, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return meadowCoreSnapshot{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return meadowCoreSnapshot{}, fmt.Errorf("Meadow Core status: HTTP %d", res.StatusCode)
	}
	var body struct {
		ModelIdentity struct {
			CanonicalID string `json:"canonical_id"`
			Multimodal  bool   `json:"multimodal"`
		} `json:"model_identity"`
		Model struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"model"`
		ModelManager struct {
			Models []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Source string `json:"source"`
			} `json:"models"`
		} `json:"model_manager"`
		Engines []struct {
			Model string `json:"model"`
		} `json:"engines"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return meadowCoreSnapshot{}, err
	}
	modelID := strings.TrimSpace(body.Model.ID)
	state := strings.TrimSpace(body.Model.State)
	if modelID == "" {
		for _, engine := range body.Engines {
			if candidate := strings.TrimSpace(engine.Model); candidate != "" {
				modelID = candidate
				break
			}
		}
	}
	if modelID != "" && state == "" {
		for _, model := range body.ModelManager.Models {
			if strings.EqualFold(strings.TrimSpace(model.ID), modelID) {
				state = strings.TrimSpace(model.Status)
				break
			}
		}
	}
	if modelID == "" {
		for _, model := range body.ModelManager.Models {
			candidateState := strings.ToLower(strings.TrimSpace(model.Status))
			if candidateState == "loaded" || candidateState == "ready" || candidateState == "loading" || candidateState == "warming" {
				modelID = strings.TrimSpace(model.ID)
				state = candidateState
				break
			}
		}
	}
	ready := strings.EqualFold(state, "ready") || strings.EqualFold(state, "loaded")
	installed := make([]string, 0, len(body.ModelManager.Models))
	seen := map[string]bool{}
	for _, model := range body.ModelManager.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		// Meadow Core's model manager is already the installed-model
		// inventory. Newer runtime snapshots intentionally omit Source for
		// unloaded models, so requiring a filesystem path here made every
		// non-resident model look uninstalled after a model switch.
		if source := strings.TrimSpace(model.Source); source != "" && !modelmgr.ModelPresent(source) {
			continue
		}
		if info, ok := modelmgr.ModelByName(id); ok {
			id = info.ServedName
		}
		key := strings.ToLower(id)
		if !seen[key] {
			installed = append(installed, id)
			seen[key] = true
		}
	}
	if ready && modelID != "" && !seen[strings.ToLower(modelID)] {
		installed = append(installed, modelID)
	}
	return meadowCoreSnapshot{ModelID: modelID, Ready: ready, Installed: installed, Multimodal: ready && body.ModelIdentity.CanonicalID == modelID && body.ModelIdentity.Multimodal}, nil
}

func probeMeadowCoreStatus(ctx context.Context, baseURL string) (string, bool, error) {
	snapshot, err := probeMeadowCoreSnapshot(ctx, baseURL)
	return snapshot.ModelID, snapshot.Ready, err
}

// resolveDesktopModelID binds every newly-created session to the model Meadow
// Core is actually serving. The configured model is only a cold-start
// fallback: after the user switches 35B -> 30B (or the reverse), keeping the
// stale setting here would select the wrong family-specific Hermes runtime
// even though the composer and resident Core both show the new model.
func resolveDesktopModelID(
	ctx context.Context,
	baseURL string,
	apiKey string,
	configuredModel string,
	meadowCore bool,
) string {
	canonical := func(id string) string {
		if info, ok := modelmgr.ModelByName(id); ok {
			return info.ServedName
		}
		return id
	}
	if meadowCore {
		// A loading status already carries the selected target id. Use it as
		// well as ready status so a chat created during activation cannot bind
		// itself to the previous model family.
		if selected, _, err := probeMeadowCoreStatus(ctx, baseURL); err == nil && strings.TrimSpace(selected) != "" {
			return canonical(selected)
		}
		return canonical(configuredModel)
	}
	if served, err := provider.NewClient(baseURL, apiKey, "").Models(ctx); err == nil && len(served) > 0 {
		return canonical(served[0])
	}
	return canonical(configuredModel)
}

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		log.Fatal(err)
	}
	return sub
}

func buildEnvMLXRoutePredictor() (*engine.MLXRoutePredictor, error) {
	kind := strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_ROUTE_PREDICTOR")))
	adapterPath := strings.TrimSpace(os.Getenv("MEADOW_ROUTE_PREDICTOR_ADAPTER"))
	switch kind {
	case "":
		// 27B/35B Hermes profiles already use the resident model's semantic
		// intent router. Do not silently load a second 2B model and spend roughly
		// 1-2 GiB of memory for a duplicate preflight. The MLX predictor remains
		// available as an explicit experiment through MEADOW_ROUTE_PREDICTOR.
		return nil, nil
	case "off", "none", "disabled", "0", "false":
		return nil, nil
	case "mlx", "1", "true", "yes":
		adapterPath = firstDesktopNonEmpty(adapterPath, os.Getenv("MEADOW_CALL_ADAPTER_PATH"))
		if adapterPath == "" {
			adapterPath = findDesktopRoutePredictorAdapter()
		}
	case "auto":
		adapterPath = firstDesktopNonEmpty(adapterPath, os.Getenv("MEADOW_CALL_ADAPTER_PATH"))
		if adapterPath == "" {
			adapterPath = findDesktopRoutePredictorAdapter()
		}
		if adapterPath == "" {
			return nil, nil
		}
	default:
		return nil, fmt.Errorf("unsupported MEADOW_ROUTE_PREDICTOR=%q", kind)
	}
	scriptPath := strings.TrimSpace(os.Getenv("MEADOW_ROUTE_PREDICTOR_SCRIPT"))
	if scriptPath == "" {
		scriptPath = findDesktopRoutePredictorScript()
	}
	pythonPath := strings.TrimSpace(os.Getenv("MEADOW_ROUTE_PREDICTOR_PYTHON"))
	if pythonPath == "" {
		pythonPath = findDesktopRoutePredictorPython()
	}
	modelPath := strings.TrimSpace(os.Getenv("MEADOW_ROUTE_PREDICTOR_MODEL"))
	if modelPath == "" {
		modelPath = findDesktopRoutePredictorModel()
	}
	return engine.NewMLXRoutePredictor(engine.MLXRoutePredictorConfig{
		PythonPath:  pythonPath,
		ScriptPath:  scriptPath,
		ModelPath:   modelPath,
		AdapterPath: adapterPath,
		MaxTokens:   desktopEnvInt("MEADOW_ROUTE_PREDICTOR_MAX_TOKENS", 16),
		Timeout:     time.Duration(desktopEnvInt("MEADOW_ROUTE_PREDICTOR_TIMEOUT_MS", 20_000)) * time.Millisecond,
		RouteFormat: "intent-code",
	})
}

func envRoutePredictorMode(predictorEnabled bool) string {
	kind := strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_ROUTE_PREDICTOR")))
	if kind == "auto" || (kind == "" && predictorEnabled) {
		return "auto"
	}
	return ""
}

func envRoutePredictorPromotion(mode string) string {
	promotion := strings.TrimSpace(os.Getenv("MEADOW_ROUTE_PREDICTOR_PROMOTE"))
	if promotion == "" && mode == "auto" {
		return "safe"
	}
	return promotion
}

func findDesktopRoutePredictorScript() string {
	const rel = "scripts/meadow-call-predict-route.py"
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, rel),
			filepath.Join(cwd, "..", rel),
			filepath.Join(cwd, "..", "..", rel),
		)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, rel),
			filepath.Join(dir, "..", rel),
			filepath.Join(dir, "..", "..", rel),
			filepath.Join(dir, "..", "Resources", rel),
		)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			if abs, err := filepath.Abs(candidate); err == nil {
				return abs
			}
			return candidate
		}
	}
	return ""
}

func findDesktopRoutePredictorAdapter() string {
	for _, rel := range []string{
		".meadow-call/adapters/tool-router-lora",
		".meadow-call/adapters/tool-router-intent-code-lora",
	} {
		if adapterPath := firstExistingDesktopDir(desktopRoutePredictorSearchCandidates(rel)); adapterPath != "" {
			return adapterPath
		}
	}
	return ""
}

func findDesktopRoutePredictorModel() string {
	if exe, err := os.Executable(); err == nil {
		if model := firstExistingDesktopDir([]string{
			filepath.Join(filepath.Dir(exe), "..", "Resources", ".meadow-call", "models", "meadow-2b-mlx-4bit"),
		}); model != "" {
			return model
		}
	}
	if !desktopLegacyDiscoveryEnabled() {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		if model := firstExistingDesktopDir([]string{
			filepath.Join(home, "Desktop", "LLM", "meadow-2b-v2", "mlx-4bit"),
			filepath.Join(home, "Desktop", "LLM", "meadow-2b", "mlx-4bit"),
		}); model != "" {
			return model
		}
	}
	return ""
}

func findDesktopRoutePredictorPython() string {
	if exe, err := os.Executable(); err == nil {
		if python := firstExistingDesktopFile([]string{
			filepath.Join(filepath.Dir(exe), "..", "Resources", "runtime", "python", "bin", "python3"),
			filepath.Join(filepath.Dir(exe), "..", "Resources", "runtime", "python", "bin", "python"),
		}); python != "" {
			return python
		}
	}
	if !desktopLegacyDiscoveryEnabled() {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		if python := firstExistingDesktopFile([]string{filepath.Join(home, "Desktop", "LLM", "mlx-env", "bin", "python")}); python != "" {
			return python
		}
	}
	return ""
}

func desktopLegacyDiscoveryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_ALLOW_DESKTOP_DISCOVERY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func desktopRoutePredictorSearchCandidates(rel string) []string {
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, rel),
			filepath.Join(cwd, "..", rel),
			filepath.Join(cwd, "..", "..", rel),
		)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, rel),
			filepath.Join(dir, "..", rel),
			filepath.Join(dir, "..", "..", rel),
			filepath.Join(dir, "..", "Resources", rel),
		)
	}
	return candidates
}

func firstExistingDesktopDir(candidates []string) string {
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			if abs, err := filepath.Abs(candidate); err == nil {
				return abs
			}
			return candidate
		}
	}
	return ""
}

func firstExistingDesktopFile(candidates []string) string {
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			if abs, err := filepath.Abs(candidate); err == nil {
				return abs
			}
			return candidate
		}
	}
	return ""
}

func firstDesktopNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func desktopEnvInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
