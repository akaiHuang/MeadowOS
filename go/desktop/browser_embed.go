package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

var errDesktopBrowserEmbedUnavailable = errors.New("embedded browser is unavailable")

// desktopBrowserEmbed is the in-window browser that fills the right drawer.
// Debug evidence (DOM / console / eval) is taken from this same view so the
// agent inspects the page the user is looking at, not a second hidden browser.
type desktopBrowserEmbed struct {
	mu             sync.Mutex
	layoutMu       sync.Mutex
	window         *application.WebviewWindow
	impl           desktopBrowserEmbedImpl
	ownerSession   string
	ownerWorkspace string
	ownerContext   context.Context
	ownerCancel    context.CancelFunc
	ownerRevision  uint64
}

type desktopBrowserEmbedImpl interface {
	setFrame(visible bool, x, y, width, height float64)
	navigate(rawURL string) error
	session() (url, title string, ready bool)
	eval(code string) (string, error)
	dom() (string, error)
	console(limit int) []map[string]string
	screenshot() ([]byte, error)
	back()
	forward()
	reload()
	close()
}

type desktopBrowserEmbedLayout struct {
	SessionID     string  `json:"session_id,omitempty"`
	WorkspaceRoot string  `json:"workspace_root,omitempty"`
	OwnerRevision uint64  `json:"owner_revision,omitempty"`
	Visible       bool    `json:"visible"`
	X             float64 `json:"x"`
	Y             float64 `json:"y"`
	Width         float64 `json:"width"`
	Height        float64 `json:"height"`
	URL           string  `json:"url,omitempty"`
	Action        string  `json:"action,omitempty"`
}

func newDesktopBrowserEmbed() *desktopBrowserEmbed {
	embed := &desktopBrowserEmbed{}
	embed.impl = newDesktopBrowserEmbedImpl(embed)
	return embed
}

func (embed *desktopBrowserEmbed) Attach(window *application.WebviewWindow) {
	if embed == nil {
		return
	}
	embed.mu.Lock()
	embed.window = window
	embed.mu.Unlock()
}

func (embed *desktopBrowserEmbed) Live() bool {
	return embed != nil && embed.impl != nil && desktopBrowserEmbedSupported
}

func (embed *desktopBrowserEmbed) nativeWindow() unsafe.Pointer {
	if embed == nil {
		return nil
	}
	embed.mu.Lock()
	window := embed.window
	embed.mu.Unlock()
	if window == nil {
		return nil
	}
	return window.NativeWindow()
}

func registerDesktopBrowserEmbed(api *http.ServeMux, embed *desktopBrowserEmbed) {
	if api == nil || embed == nil {
		return
	}
	api.HandleFunc("GET /native/browser-embed", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		revision, ownerSession, ownerWorkspace := embed.agentOwnerReceipt()
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"ok":                   true,
			"embedded":             embed.Live(),
			"supported":            desktopBrowserEmbedSupported,
			"agent_control":        desktopBrowserEmbedSupported,
			"owner_revision":       revision,
			"owner_session_id":     ownerSession,
			"owner_workspace_root": ownerWorkspace,
		})
	})
	api.HandleFunc("POST /native/browser-embed", func(writer http.ResponseWriter, request *http.Request) {
		var layout desktopBrowserEmbedLayout
		if err := json.NewDecoder(request.Body).Decode(&layout); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(map[string]any{"error": "invalid browser embed layout"})
			return
		}
		if !embed.apply(layout) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(writer).Encode(map[string]any{"error": "embedded Browser ownership/layout request is stale"})
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		url, title, ready := embed.session()
		revision, ownerSession, ownerWorkspace := embed.agentOwnerReceipt()
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"ok":                       true,
			"url":                      url,
			"title":                    title,
			"ready":                    ready,
			"owner_revision":           revision,
			"owner_session_id":         ownerSession,
			"owner_workspace_root":     ownerWorkspace,
			"requested_workspace_root": layout.WorkspaceRoot,
		})
	})
}

func (embed *desktopBrowserEmbed) apply(layout desktopBrowserEmbedLayout) bool {
	if embed == nil || embed.impl == nil {
		return false
	}
	embed.layoutMu.Lock()
	defer embed.layoutMu.Unlock()
	if !embed.setAgentOwner(layout.SessionID, layout.WorkspaceRoot, layout.OwnerRevision) {
		return false
	}
	embed.impl.setFrame(layout.Visible, layout.X, layout.Y, layout.Width, layout.Height)
	switch strings.TrimSpace(strings.ToLower(layout.Action)) {
	case "back":
		embed.impl.back()
	case "forward":
		embed.impl.forward()
	case "reload":
		embed.impl.reload()
	}
	if url := strings.TrimSpace(layout.URL); url != "" {
		_ = embed.impl.navigate(url)
	}
	return true
}

func (embed *desktopBrowserEmbed) agentOwnerReceipt() (uint64, string, string) {
	if embed == nil {
		return 0, "", ""
	}
	embed.mu.Lock()
	defer embed.mu.Unlock()
	return embed.ownerRevision, embed.ownerSession, embed.ownerWorkspace
}

// Ownership follows the selected Desktop conversation, never page metadata or
// model arguments. Selecting another chat revokes outstanding Browser work.
func (embed *desktopBrowserEmbed) setAgentOwner(sessionID, workspaceRoot string, revision uint64) bool {
	if embed == nil {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if sessionID != "" && workspaceRoot != "" {
		resolved, err := filepath.EvalSymlinks(workspaceRoot)
		if err != nil || !filepath.IsAbs(resolved) {
			sessionID, workspaceRoot = "", ""
		} else {
			workspaceRoot = filepath.Clean(resolved)
		}
	} else {
		sessionID, workspaceRoot = "", ""
	}
	embed.mu.Lock()
	defer embed.mu.Unlock()
	if revision < embed.ownerRevision {
		return false
	}
	if revision == embed.ownerRevision && embed.ownerContext != nil && (embed.ownerSession != sessionID || embed.ownerWorkspace != workspaceRoot) {
		return false
	}
	embed.ownerRevision = revision
	if embed.ownerSession == sessionID && embed.ownerWorkspace == workspaceRoot {
		return true
	}
	if embed.ownerCancel != nil {
		embed.ownerCancel()
	}
	embed.ownerSession, embed.ownerWorkspace = sessionID, workspaceRoot
	embed.ownerContext, embed.ownerCancel = context.WithCancel(context.Background())
	return true
}

func (embed *desktopBrowserEmbed) authorizeAgent(sessionID, workspaceRoot string) error {
	if embed == nil || !embed.Live() {
		return errDesktopBrowserEmbedUnavailable
	}
	resolved, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil || !filepath.IsAbs(resolved) {
		return errors.New("embedded Browser workspace is unavailable")
	}
	embed.mu.Lock()
	defer embed.mu.Unlock()
	if sessionID == "" || embed.ownerSession != sessionID || embed.ownerWorkspace != filepath.Clean(resolved) {
		return errors.New("embedded Browser belongs to another Desktop conversation; select the requesting conversation before retrying")
	}
	return nil
}

func (embed *desktopBrowserEmbed) agentContext(ctx context.Context, sessionID, workspaceRoot string) (context.Context, context.CancelFunc, error) {
	if err := embed.authorizeAgent(sessionID, workspaceRoot); err != nil {
		return nil, nil, err
	}
	resolved, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return nil, nil, err
	}
	embed.mu.Lock()
	owner := embed.ownerContext
	// Recheck under the same lock that captures the revocable owner capability.
	if owner == nil || embed.ownerSession != sessionID || embed.ownerWorkspace != filepath.Clean(resolved) {
		embed.mu.Unlock()
		return nil, nil, errors.New("embedded Browser conversation changed")
	}
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(owner, cancel)
	embed.mu.Unlock()
	return child, func() { stop(); cancel() }, nil
}

func (embed *desktopBrowserEmbed) agentNavigation(sessionID, rawURL string) {
	if embed == nil {
		return
	}
	embed.mu.Lock()
	window, owner := embed.window, embed.ownerSession
	embed.mu.Unlock()
	if window == nil || owner != sessionID {
		return
	}
	detail, _ := json.Marshal(map[string]string{"session_id": sessionID, "url": rawURL})
	window.ExecJS("window.dispatchEvent(new CustomEvent('meadow:embedded-browser-navigation',{detail:" + string(detail) + "}));")
}

func (embed *desktopBrowserEmbed) revokeAgentOwner() {
	if embed == nil {
		return
	}
	embed.mu.Lock()
	if embed.ownerCancel != nil {
		embed.ownerCancel()
	}
	embed.ownerSession, embed.ownerWorkspace = "", ""
	embed.mu.Unlock()
}

func (embed *desktopBrowserEmbed) navigate(rawURL string) error {
	if embed == nil || embed.impl == nil {
		return errDesktopBrowserEmbedUnavailable
	}
	return embed.impl.navigate(rawURL)
}

func (embed *desktopBrowserEmbed) session() (string, string, bool) {
	if embed == nil || embed.impl == nil {
		return "", "", false
	}
	return embed.impl.session()
}

func (embed *desktopBrowserEmbed) eval(code string) (string, error) {
	if embed == nil || embed.impl == nil {
		return "", errDesktopBrowserEmbedUnavailable
	}
	return embed.impl.eval(code)
}

func (embed *desktopBrowserEmbed) dom() (string, error) {
	if embed == nil || embed.impl == nil {
		return "", errDesktopBrowserEmbedUnavailable
	}
	return embed.impl.dom()
}

func (embed *desktopBrowserEmbed) console(limit int) []map[string]string {
	if embed == nil || embed.impl == nil {
		return nil
	}
	return embed.impl.console(limit)
}

func (embed *desktopBrowserEmbed) screenshot() ([]byte, error) {
	if embed == nil || embed.impl == nil {
		return nil, errDesktopBrowserEmbedUnavailable
	}
	return embed.impl.screenshot()
}
