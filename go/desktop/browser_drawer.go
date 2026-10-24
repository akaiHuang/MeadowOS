package main

// This file owns the narrow Desktop-to-Meadow-Browser bridge used by the
// right-hand Browser drawer.  It deliberately does not proxy arbitrary URLs:
// the Desktop talks only to a loopback Meadow Browser daemon and it chooses
// the daemon route itself.  A chat owns one tab/page binding, so changing the
// selected Desktop conversation cannot accidentally make the agent inspect a
// different conversation's active browser page.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/browserruntime"
)

const (
	desktopBrowserDefaultBase      = "http://127.0.0.1:8085"
	desktopBrowserRequestBodyLimit = 16 << 10
	desktopBrowserJSONLimit        = 2 << 20
	desktopBrowserScreenshotLimit  = 16 << 20
	desktopBrowserTimeout          = 20 * time.Second
)

var desktopBrowserSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type desktopBrowserDrawer struct {
	base             *url.URL
	client           *http.Client
	launch           func() error
	identityCheck    func(context.Context, string) error
	identityRequired bool
	embed            *desktopBrowserEmbed

	mu       sync.Mutex
	sessions map[string]desktopBrowserDrawerSession
}

func (drawer *desktopBrowserDrawer) SetEmbed(embed *desktopBrowserEmbed) {
	if drawer == nil {
		return
	}
	drawer.embed = embed
}

func (drawer *desktopBrowserDrawer) embedLive() bool {
	return drawer != nil && drawer.embed != nil && drawer.embed.Live()
}

type desktopBrowserDrawerSession struct {
	SessionID string    `json:"session_id"`
	TabID     string    `json:"tab_id"`
	PageID    string    `json:"page_id"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Ready     bool      `json:"ready"`
	UpdatedAt time.Time `json:"updated_at"`
}

type desktopBrowserTab struct {
	TabID  string `json:"tab_id"`
	PageID string `json:"page_id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
}

type desktopBrowserTabResponse struct {
	OK  bool              `json:"ok"`
	Tab desktopBrowserTab `json:"tab"`
}

type desktopBrowserState struct {
	URL    string `json:"url"`
	Title  string `json:"title"`
	Ready  bool   `json:"ready"`
	PageID string `json:"page_id"`
}

type desktopBrowserRouteError struct {
	Status      int
	Code        string
	RuntimeGate bool
}

func (err *desktopBrowserRouteError) Error() string {
	if err == nil {
		return ""
	}
	return err.Code
}

func newDesktopBrowserDrawerFromEnvironment() http.Handler {
	base := strings.TrimSpace(os.Getenv("MEADOW_BROWSER_BASE"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("MEADOW_BROWSER_URL"))
	}
	if base == "" {
		base = desktopBrowserDefaultBase
	}
	drawer, err := newDesktopBrowserDrawer(base, nil)
	if err != nil {
		// The Agent's protected browser transport is intentionally not a
		// loopback HTTP service. The visual Desktop drawer owns its separate
		// bundled loopback daemon.
		drawer, err = newDesktopBrowserDrawer(desktopBrowserDefaultBase, nil)
		if err != nil {
			return unavailableDesktopBrowserDrawer{}
		}
	}
	return drawer
}

func newDesktopBrowserDrawer(rawBase string, client *http.Client) (*desktopBrowserDrawer, error) {
	base, err := desktopBrowserBaseURL(rawBase)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: desktopBrowserTimeout}
	}
	identityRequired := browserruntime.RequiresBundledCheck(base.String())
	sidecarClient := *client
	sidecarClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if identityRequired {
		transport := sidecarClient.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		if httpTransport, ok := transport.(*http.Transport); ok {
			// A reused connection could still target the old process after its
			// listener was replaced. Dial the listener verified for this request.
			isolated := httpTransport.Clone()
			isolated.DisableKeepAlives = true
			sidecarClient.Transport = isolated
		}
	}
	drawer := &desktopBrowserDrawer{
		base: base, client: &sidecarClient, sessions: make(map[string]desktopBrowserDrawerSession),
		identityCheck:    browserruntime.CheckBundled,
		identityRequired: identityRequired,
	}
	drawer.launch = desktopBrowserLauncher(base)
	liveBrowserDrawer = drawer
	return drawer, nil
}

var liveBrowserDrawer *desktopBrowserDrawer

func desktopBrowserBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("invalid Meadow Browser base URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("Meadow Browser base URL must not include a path")
	}
	host := strings.TrimSpace(parsed.Hostname())
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("Meadow Browser must be a loopback service")
	}
	if port := parsed.Port(); port != "" {
		value, convertErr := strconv.Atoi(port)
		if convertErr != nil || value < 1 || value > 65535 {
			return nil, errors.New("invalid Meadow Browser port")
		}
	}
	parsed.Path = ""
	return parsed, nil
}

type unavailableDesktopBrowserDrawer struct{}

func (unavailableDesktopBrowserDrawer) ServeHTTP(writer http.ResponseWriter, _ *http.Request) {
	writeDesktopBrowserError(writer, http.StatusServiceUnavailable, "browser_unavailable")
}

func (drawer *desktopBrowserDrawer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if drawer == nil {
		writeDesktopBrowserError(writer, http.StatusServiceUnavailable, "browser_unavailable")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/browser/health":
		drawer.handleHealth(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/browser/start":
		drawer.handleStart(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/browser/open":
		drawer.handleOpen(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/browser/session":
		drawer.handleSession(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/browser/goto":
		drawer.handleGoto(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/browser/dom":
		drawer.handleDOM(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/browser/console":
		drawer.handleConsole(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/browser/screenshot":
		drawer.handleScreenshot(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/browser/eval":
		drawer.handleEval(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/browser/nav":
		drawer.handleNav(writer, request)
	default:
		writeDesktopBrowserError(writer, http.StatusNotFound, "browser_route_not_found")
	}
}

func (drawer *desktopBrowserDrawer) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if drawer.embedLive() {
		url, title, ready := drawer.embed.session()
		writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{
			"ok": true, "embedded": true, "url": url, "title": title, "ready": ready,
		})
		return
	}
	raw, err := drawer.health(request.Context())
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	writeDesktopBrowserRawJSON(writer, raw)
}

func (drawer *desktopBrowserDrawer) handleStart(writer http.ResponseWriter, request *http.Request) {
	if drawer.embedLive() {
		drawer.handleHealth(writer, request)
		return
	}
	if err := drawer.ensureAvailable(request.Context()); err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	drawer.handleHealth(writer, request)
}

type desktopBrowserOpenRequest struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url,omitempty"`
}

func (drawer *desktopBrowserDrawer) handleOpen(writer http.ResponseWriter, request *http.Request) {
	var input desktopBrowserOpenRequest
	if err := decodeDesktopBrowserJSON(writer, request, &input); err != nil {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if !validDesktopBrowserSessionID(input.SessionID) || !validDesktopBrowserURL(input.URL, true) {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if drawer.embedLive() {
		session, err := drawer.openEmbedded(strings.TrimSpace(input.SessionID), strings.TrimSpace(input.URL))
		if err != nil {
			writeDesktopBrowserError(writer, http.StatusBadGateway, "browser_unavailable")
			return
		}
		writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{"ok": true, "session": session})
		return
	}
	if err := drawer.ensureAvailable(request.Context()); err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	session, err := drawer.open(request.Context(), strings.TrimSpace(input.SessionID), strings.TrimSpace(input.URL))
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{"ok": true, "session": session})
}

func (drawer *desktopBrowserDrawer) handleSession(writer http.ResponseWriter, request *http.Request) {
	sessionID := strings.TrimSpace(request.URL.Query().Get("session_id"))
	if !validDesktopBrowserSessionID(sessionID) {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if session, ok := drawer.embeddedSession(sessionID); ok {
		writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{"ok": true, "session": session})
		return
	}
	session, err := drawer.session(request.Context(), sessionID)
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{"ok": true, "session": session})
}

func (drawer *desktopBrowserDrawer) handleGoto(writer http.ResponseWriter, request *http.Request) {
	var input desktopBrowserOpenRequest
	if err := decodeDesktopBrowserJSON(writer, request, &input); err != nil {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if !validDesktopBrowserSessionID(input.SessionID) || !validDesktopBrowserURL(input.URL, false) {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if drawer.embedLive() {
		session, err := drawer.openEmbedded(strings.TrimSpace(input.SessionID), strings.TrimSpace(input.URL))
		if err != nil {
			writeDesktopBrowserError(writer, http.StatusBadGateway, "browser_unavailable")
			return
		}
		writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{"ok": true, "session": session})
		return
	}
	if err := drawer.ensureAvailable(request.Context()); err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	session, err := drawer.open(request.Context(), strings.TrimSpace(input.SessionID), strings.TrimSpace(input.URL))
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{"ok": true, "session": session})
}

type desktopBrowserDOMRequest struct {
	SessionID string `json:"session_id"`
	Selector  string `json:"selector,omitempty"`
	MaxDepth  int    `json:"max_depth,omitempty"`
}

func (drawer *desktopBrowserDrawer) handleDOM(writer http.ResponseWriter, request *http.Request) {
	var input desktopBrowserDOMRequest
	if err := decodeDesktopBrowserJSON(writer, request, &input); err != nil {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if !validDesktopBrowserSessionID(input.SessionID) || len(input.Selector) > 4096 || input.MaxDepth < 0 || input.MaxDepth > 12 {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if drawer.embedLive() {
		html, err := drawer.embed.dom()
		if err != nil {
			writeDesktopBrowserError(writer, http.StatusBadGateway, "browser_response_invalid")
			return
		}
		writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{"ok": true, "tree": html})
		return
	}
	session, err := drawer.session(request.Context(), strings.TrimSpace(input.SessionID))
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	payload := map[string]any{"page_id": session.PageID}
	if selector := strings.TrimSpace(input.Selector); selector != "" {
		payload["selector"] = selector
	}
	if input.MaxDepth > 0 {
		payload["max_depth"] = input.MaxDepth
	}
	raw, err := drawer.call(request.Context(), http.MethodPost, "/dom", payload, desktopBrowserJSONLimit)
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	writeDesktopBrowserRawJSON(writer, raw)
}

func (drawer *desktopBrowserDrawer) handleConsole(writer http.ResponseWriter, request *http.Request) {
	sessionID := strings.TrimSpace(request.URL.Query().Get("session_id"))
	if !validDesktopBrowserSessionID(sessionID) {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	limit := 100
	if raw := request.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
			return
		}
		limit = parsed
	}
	if drawer.embedLive() {
		writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{"ok": true, "logs": drawer.embed.console(limit)})
		return
	}
	session, err := drawer.session(request.Context(), sessionID)
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	query := url.Values{"page_id": {session.PageID}, "limit": {strconv.Itoa(limit)}}
	raw, err := drawer.callPath(request.Context(), http.MethodGet, "/console-logs?"+query.Encode(), nil, desktopBrowserJSONLimit)
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	writeDesktopBrowserRawJSON(writer, raw)
}

func (drawer *desktopBrowserDrawer) handleScreenshot(writer http.ResponseWriter, request *http.Request) {
	sessionID := strings.TrimSpace(request.URL.Query().Get("session_id"))
	if !validDesktopBrowserSessionID(sessionID) {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if drawer.embedLive() {
		raw, err := drawer.embed.screenshot()
		if err != nil {
			writeDesktopBrowserError(writer, http.StatusBadGateway, "browser_response_invalid")
			return
		}
		writer.Header().Set("Content-Type", "image/png")
		writer.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = writer.Write(raw)
		return
	}
	session, err := drawer.session(request.Context(), sessionID)
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	raw, headers, err := drawer.callWithHeaders(request.Context(), http.MethodPost, "/screenshot", map[string]any{
		"page_id": session.PageID, "full_page": true,
	}, desktopBrowserScreenshotLimit)
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(headers.Get("Content-Type")))
	if !strings.HasPrefix(contentType, "image/png") {
		writeDesktopBrowserError(writer, http.StatusBadGateway, "browser_response_invalid")
		return
	}
	writer.Header().Set("Content-Type", "image/png")
	writer.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	_, _ = writer.Write(raw)
}

func (drawer *desktopBrowserDrawer) handleEval(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
	}
	if err := decodeDesktopBrowserJSON(writer, request, &input); err != nil ||
		!validDesktopBrowserSessionID(input.SessionID) ||
		strings.TrimSpace(input.Code) == "" || len(input.Code) > 12<<10 {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if drawer.embedLive() {
		value, err := drawer.embed.eval(input.Code)
		if err != nil {
			writeDesktopBrowserError(writer, http.StatusBadGateway, "browser_response_invalid")
			return
		}
		writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{"ok": true, "value": value})
		return
	}
	session, err := drawer.session(request.Context(), strings.TrimSpace(input.SessionID))
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	raw, err := drawer.call(request.Context(), http.MethodPost, "/eval", map[string]any{
		"page_id": session.PageID,
		"code":    input.Code,
	}, desktopBrowserJSONLimit)
	if err != nil {
		writeDesktopBrowserRouteError(writer, err)
		return
	}
	writeDesktopBrowserRawJSON(writer, raw)
}

func (drawer *desktopBrowserDrawer) handleNav(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Action string `json:"action"`
	}
	if err := decodeDesktopBrowserJSON(writer, request, &input); err != nil {
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	if !drawer.embedLive() {
		writeDesktopBrowserError(writer, http.StatusServiceUnavailable, "browser_unavailable")
		return
	}
	switch strings.TrimSpace(strings.ToLower(input.Action)) {
	case "back":
		drawer.embed.impl.back()
	case "forward":
		drawer.embed.impl.forward()
	case "reload":
		drawer.embed.impl.reload()
	default:
		writeDesktopBrowserError(writer, http.StatusBadRequest, "browser_request_invalid")
		return
	}
	url, title, ready := drawer.embed.session()
	writeDesktopBrowserJSON(writer, http.StatusOK, map[string]any{
		"ok":      true,
		"session": desktopBrowserDrawerSession{URL: url, Title: title, Ready: ready, UpdatedAt: time.Now().UTC()},
	})
}

func (drawer *desktopBrowserDrawer) openEmbedded(sessionID, rawURL string) (desktopBrowserDrawerSession, error) {
	if rawURL != "" {
		if err := drawer.embed.navigate(rawURL); err != nil {
			return desktopBrowserDrawerSession{}, err
		}
	}
	session, _ := drawer.embeddedSession(sessionID)
	return session, nil
}

func (drawer *desktopBrowserDrawer) embeddedSession(sessionID string) (desktopBrowserDrawerSession, bool) {
	if !drawer.embedLive() {
		return desktopBrowserDrawerSession{}, false
	}
	url, title, ready := drawer.embed.session()
	return desktopBrowserDrawerSession{
		SessionID: sessionID,
		URL:       url,
		Title:     title,
		Ready:     ready,
		UpdatedAt: time.Now().UTC(),
	}, true
}

func (drawer *desktopBrowserDrawer) open(ctx context.Context, sessionID, rawURL string) (desktopBrowserDrawerSession, error) {
	drawer.mu.Lock()
	defer drawer.mu.Unlock()
	session, err := drawer.ensureLocked(ctx, sessionID, true)
	if err != nil {
		return desktopBrowserDrawerSession{}, err
	}
	if rawURL != "" {
		if _, err := drawer.call(ctx, http.MethodPost, "/goto", map[string]any{
			"url": rawURL, "page_id": session.PageID, "wait_until": "network_idle", "settle_ms": 800,
		}, desktopBrowserJSONLimit); err != nil {
			return desktopBrowserDrawerSession{}, err
		}
	}
	return drawer.refreshLocked(ctx, session)
}

func (drawer *desktopBrowserDrawer) session(ctx context.Context, sessionID string) (desktopBrowserDrawerSession, error) {
	drawer.mu.Lock()
	defer drawer.mu.Unlock()
	session, err := drawer.ensureLocked(ctx, sessionID, false)
	if err != nil {
		return desktopBrowserDrawerSession{}, err
	}
	return drawer.refreshLocked(ctx, session)
}

func (drawer *desktopBrowserDrawer) ensureLocked(ctx context.Context, sessionID string, create bool) (desktopBrowserDrawerSession, error) {
	session, found := drawer.sessions[sessionID]
	if !found {
		if !create {
			return desktopBrowserDrawerSession{}, &desktopBrowserRouteError{Status: http.StatusNotFound, Code: "browser_context_missing"}
		}
		raw, err := drawer.call(ctx, http.MethodPost, "/tabs/new", map[string]any{"activate": true}, desktopBrowserJSONLimit)
		if err != nil {
			return desktopBrowserDrawerSession{}, err
		}
		var created desktopBrowserTabResponse
		if err := json.Unmarshal(raw, &created); err != nil || !created.OK || !validDesktopBrowserTab(created.Tab) {
			return desktopBrowserDrawerSession{}, &desktopBrowserRouteError{Status: http.StatusBadGateway, Code: "browser_response_invalid"}
		}
		session = desktopBrowserDrawerSession{SessionID: sessionID, TabID: created.Tab.TabID, PageID: created.Tab.PageID}
		drawer.sessions[sessionID] = session
		return session, nil
	}
	raw, err := drawer.call(ctx, http.MethodPost, "/tabs/activate", map[string]any{"tab_id": session.TabID}, desktopBrowserJSONLimit)
	if err != nil {
		var routeError *desktopBrowserRouteError
		if errors.As(err, &routeError) && (routeError.RuntimeGate || routeError.Code == "browser_runtime_mismatch" || routeError.Code == "browser_runtime_identity_unknown") {
			return desktopBrowserDrawerSession{}, err
		}
		if !create {
			return desktopBrowserDrawerSession{}, &desktopBrowserRouteError{Status: http.StatusConflict, Code: "browser_context_lost"}
		}
		delete(drawer.sessions, sessionID)
		return drawer.ensureLocked(ctx, sessionID, true)
	}
	var activated desktopBrowserTabResponse
	if err := json.Unmarshal(raw, &activated); err != nil || !activated.OK || !validDesktopBrowserTab(activated.Tab) {
		return desktopBrowserDrawerSession{}, &desktopBrowserRouteError{Status: http.StatusBadGateway, Code: "browser_response_invalid"}
	}
	session.TabID = activated.Tab.TabID
	session.PageID = activated.Tab.PageID
	session.URL = activated.Tab.URL
	session.Title = activated.Tab.Title
	session.UpdatedAt = time.Now().UTC()
	drawer.sessions[sessionID] = session
	return session, nil
}

func (drawer *desktopBrowserDrawer) refreshLocked(ctx context.Context, session desktopBrowserDrawerSession) (desktopBrowserDrawerSession, error) {
	// Meadow Browser's state endpoint accepts the optional page_id as JSON on
	// its GET request. Keeping it in the body is important: query parameters
	// are deliberately stripped by the native API dispatcher.
	raw, err := drawer.call(ctx, http.MethodGet, "/state", map[string]any{"page_id": session.PageID}, desktopBrowserJSONLimit)
	if err != nil {
		return desktopBrowserDrawerSession{}, err
	}
	var state desktopBrowserState
	if err := json.Unmarshal(raw, &state); err != nil || strings.TrimSpace(state.PageID) == "" || state.PageID != session.PageID {
		return desktopBrowserDrawerSession{}, &desktopBrowserRouteError{Status: http.StatusBadGateway, Code: "browser_response_invalid"}
	}
	session.URL = state.URL
	session.Title = state.Title
	session.Ready = state.Ready
	session.UpdatedAt = time.Now().UTC()
	drawer.sessions[session.SessionID] = session
	return session, nil
}

func (drawer *desktopBrowserDrawer) ensureAvailable(ctx context.Context) error {
	if drawer == nil {
		return &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_unavailable"}
	}
	if _, err := drawer.health(ctx); err == nil {
		return nil
	} else if !isDesktopBrowserUnavailable(err) {
		return err
	}
	if drawer.launch == nil {
		return &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_unavailable"}
	}
	if err := drawer.launch(); err != nil {
		return &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_unavailable"}
	}
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(160 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return &desktopBrowserRouteError{Status: http.StatusGatewayTimeout, Code: "browser_unavailable"}
		case <-deadline.C:
			return &desktopBrowserRouteError{Status: http.StatusGatewayTimeout, Code: "browser_unavailable"}
		case <-tick.C:
			if _, err := drawer.health(ctx); err == nil {
				return nil
			} else if !isDesktopBrowserUnavailable(err) {
				return err
			}
		}
	}
}

func (drawer *desktopBrowserDrawer) health(ctx context.Context) ([]byte, error) {
	if drawer.identityCheck != nil {
		if err := drawer.identityCheck(ctx, drawer.base.String()); err != nil {
			code := browserruntime.Code(err)
			if browserruntime.IsUnavailable(err) {
				code = "browser_unavailable"
			}
			if code != "browser_unavailable" && code != "browser_runtime_mismatch" {
				code = "browser_runtime_identity_unknown"
			}
			return nil, &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: code, RuntimeGate: true}
		}
	}
	raw, err := drawer.call(ctx, http.MethodGet, "/health", nil, desktopBrowserJSONLimit)
	if err != nil {
		if drawer.identityRequired {
			// A verified listener that fails health is still present. Preserve it
			// rather than treating its unhealthy service as permission to launch.
			return nil, &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_runtime_identity_unknown", RuntimeGate: true}
		}
		var routeError *desktopBrowserRouteError
		if errors.As(err, &routeError) {
			return nil, &desktopBrowserRouteError{Status: routeError.Status, Code: routeError.Code, RuntimeGate: true}
		}
		return nil, err
	}
	var health struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &health); err != nil || !health.OK {
		return nil, &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_runtime_identity_unknown", RuntimeGate: true}
	}
	return raw, nil
}

func isDesktopBrowserUnavailable(err error) bool {
	var routeError *desktopBrowserRouteError
	return errors.As(err, &routeError) && routeError.Code == "browser_unavailable"
}

func desktopBrowserLauncher(base *url.URL) func() error {
	return func() error {
		binary, err := desktopBrowserBinary()
		if err != nil {
			return err
		}
		host, port := base.Hostname(), base.Port()
		if host == "" || port == "" {
			return errors.New("Meadow Browser requires an explicit loopback port")
		}
		command := exec.Command(binary, "--scrape", "--host", host, "--port", port)
		if err := command.Start(); err != nil {
			return err
		}
		return command.Process.Release()
	}
}

func desktopBrowserBinary() (string, error) {
	candidates := []string{strings.TrimSpace(os.Getenv("MEADOW_BROWSER_BIN"))}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "..", "Resources", "meadow-browser"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".meadow", "bin", "meadow-browser"))
	}
	if installed, err := exec.LookPath("meadow-browser"); err == nil {
		candidates = append(candidates, installed)
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("bundled Meadow Browser executable was not found")
}

func validDesktopBrowserSessionID(value string) bool {
	return desktopBrowserSessionIDPattern.MatchString(strings.TrimSpace(value))
}

func validDesktopBrowserTab(tab desktopBrowserTab) bool {
	return strings.TrimSpace(tab.TabID) != "" && strings.TrimSpace(tab.PageID) != "" && len(tab.TabID) <= 256 && len(tab.PageID) <= 256
}

func validDesktopBrowserURL(value string, allowEmpty bool) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return allowEmpty
	}
	if len(value) > 8192 || strings.ContainsRune(value, '\x00') {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil
}

func (drawer *desktopBrowserDrawer) call(ctx context.Context, method, route string, payload any, limit int64) ([]byte, error) {
	raw, _, err := drawer.callWithHeaders(ctx, method, route, payload, limit)
	return raw, err
}

func (drawer *desktopBrowserDrawer) callPath(ctx context.Context, method, route string, payload any, limit int64) ([]byte, error) {
	raw, _, err := drawer.callWithHeaders(ctx, method, route, payload, limit)
	return raw, err
}

func (drawer *desktopBrowserDrawer) callWithHeaders(ctx context.Context, method, route string, payload any, limit int64) ([]byte, http.Header, error) {
	if drawer == nil || drawer.base == nil || drawer.client == nil || limit <= 0 {
		return nil, nil, &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_unavailable"}
	}
	path, query, _ := strings.Cut(route, "?")
	if path != "/health" {
		// Recheck before every sidecar operation, including activation for an
		// existing conversation. A replaced listener must not receive its pages.
		if _, err := drawer.health(ctx); err != nil {
			return nil, nil, err
		}
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil || len(encoded) > desktopBrowserRequestBodyLimit {
			return nil, nil, &desktopBrowserRouteError{Status: http.StatusBadRequest, Code: "browser_request_invalid"}
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := *drawer.base
	endpoint.Path = path
	endpoint.RawQuery = query
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, nil, &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_unavailable"}
	}
	request.Header.Set("Accept", "application/json, image/png")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := drawer.client.Do(request)
	if err != nil {
		return nil, nil, &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_unavailable"}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, nil, &desktopBrowserRouteError{Status: http.StatusBadGateway, Code: "browser_response_invalid"}
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		return nil, nil, &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_runtime_identity_unknown"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, nil, &desktopBrowserRouteError{Status: http.StatusServiceUnavailable, Code: "browser_unavailable"}
	}
	return raw, response.Header.Clone(), nil
}

func decodeDesktopBrowserJSON(writer http.ResponseWriter, request *http.Request, output any) error {
	if request == nil || request.Body == nil {
		return errors.New("request body is required")
	}
	request.Body = http.MaxBytesReader(writer, request.Body, desktopBrowserRequestBodyLimit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func writeDesktopBrowserRouteError(writer http.ResponseWriter, err error) {
	var routeError *desktopBrowserRouteError
	if errors.As(err, &routeError) {
		writeDesktopBrowserError(writer, routeError.Status, routeError.Code)
		return
	}
	writeDesktopBrowserError(writer, http.StatusServiceUnavailable, "browser_unavailable")
}

func writeDesktopBrowserError(writer http.ResponseWriter, status int, code string) {
	writeDesktopBrowserJSON(writer, status, map[string]any{
		"error": map[string]string{"code": code, "message": desktopBrowserErrorMessage(code)},
	})
}

func desktopBrowserErrorMessage(code string) string {
	switch code {
	case "browser_context_missing":
		return "This conversation has no Meadow Browser page yet"
	case "browser_context_lost":
		return "The Meadow Browser page was closed or restarted"
	case "browser_response_invalid":
		return "Meadow Browser returned an invalid response"
	case "browser_request_invalid":
		return "Invalid Meadow Browser request"
	case "browser_runtime_mismatch":
		return "The running Meadow Browser does not match this app's Browser executable"
	case "browser_runtime_identity_unknown":
		return "The running Meadow Browser's identity could not be verified"
	default:
		return "Meadow Browser is unavailable"
	}
}

func writeDesktopBrowserJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeDesktopBrowserRawJSON(writer http.ResponseWriter, raw []byte) {
	if !json.Valid(raw) {
		writeDesktopBrowserError(writer, http.StatusBadGateway, "browser_response_invalid")
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(raw)
}

func (drawer *desktopBrowserDrawer) String() string {
	if drawer == nil || drawer.base == nil {
		return "Meadow Browser unavailable"
	}
	return fmt.Sprintf("Meadow Browser %s", drawer.base.String())
}
