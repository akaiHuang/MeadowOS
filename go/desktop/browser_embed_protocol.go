package main

// This handler is reachable only through the private parent/child Browser RPC.
// Its session and workspace headers are supplied by the executor, never by page
// JavaScript or model tool arguments. It controls the same visible WKWebView.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type desktopEmbeddedProtocolNative interface {
	documentState() (uint64, bool, string)
}

type desktopEmbeddedProtocolPinnedNative interface {
	evalPinnedContext(context.Context, string, bool, uint64) (string, error)
	navigatePinnedContext(context.Context, string, string, uint64) error
}

type desktopEmbeddedProtocolDiagnostics interface {
	diagnosticsContext(context.Context, int, string) (string, error)
}

type desktopEmbeddedProtocolCapture interface {
	screenshotPinnedContext(context.Context, string, bool, uint64) ([]byte, error)
}

type desktopEmbeddedBrowserHandler struct {
	embed *desktopBrowserEmbed
	gate  chan struct{}
}

func newDesktopEmbeddedBrowserHandler(embed *desktopBrowserEmbed) http.Handler {
	return &desktopEmbeddedBrowserHandler{embed: embed, gate: make(chan struct{}, 1)}
}

type desktopEmbeddedProtocolInput struct {
	URL              string   `json:"url"`
	Selector         string   `json:"selector"`
	Code             string   `json:"code"`
	Text             string   `json:"text"`
	Key              string   `json:"key"`
	Modifiers        []string `json:"modifiers"`
	WaitUntil        string   `json:"wait_until"`
	WaitStrategy     string   `json:"wait_strategy"`
	SettleMS         int      `json:"settle_ms"`
	MaxDepth         int      `json:"max_depth"`
	FullPage         *bool    `json:"full_page"`
	PageID           string   `json:"page_id"`
	DocumentID       string   `json:"document_id"`
	ExpectedURL      string   `json:"expected_url"`
	ExpectedOrigin   string   `json:"expected_origin"`
	ExpectedRevision *int     `json:"expected_revision"`
	ToolName         string   `json:"tool_name"`
	StateToolName    string   `json:"state_tool_name"`
	InputJSON        string   `json:"input_json"`
	AllowStateChange bool     `json:"allow_state_change"`
}

type desktopEmbeddedProtocolState struct {
	generation uint64
	loading    bool
	err        string
	url        string
	title      string
}

func (state desktopEmbeddedProtocolState) pageID() string {
	return "embedded-" + strconv.FormatUint(state.generation, 10)
}

type desktopEmbeddedProtocolError struct {
	status  int
	code    string
	message string
}

func (err *desktopEmbeddedProtocolError) Error() string { return err.message }

func embeddedProtocolFailure(status int, code, message string) error {
	return &desktopEmbeddedProtocolError{status: status, code: code, message: message}
}

func (handler *desktopEmbeddedBrowserHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if handler.embed == nil || !handler.embed.Live() {
		handler.writeError(writer, embeddedProtocolFailure(503, "browser_embedded_unavailable", "the embedded Browser is unavailable"))
		return
	}
	session := strings.TrimSpace(request.Header.Get("X-Meadow-Browser-Session"))
	workspace, err := embeddedProtocolWorkspace(request.Header.Get("X-Meadow-Browser-Workspace"))
	if !validDesktopBrowserSessionID(session) || err != nil {
		handler.writeError(writer, embeddedProtocolFailure(403, "browser_context_missing", "trusted Browser session and workspace are required"))
		return
	}
	ctx, cancel, err := handler.embed.agentContext(request.Context(), session, workspace)
	if err != nil {
		handler.writeError(writer, embeddedProtocolFailure(409, "browser_context_lost", err.Error()))
		return
	}
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	request = request.WithContext(ctx)
	input := desktopEmbeddedProtocolInput{PageID: request.URL.Query().Get("page_id")}
	if request.Method == http.MethodPost {
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 256<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			handler.writeError(writer, embeddedProtocolFailure(400, "browser_request_invalid", "invalid Browser request JSON"))
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			handler.writeError(writer, embeddedProtocolFailure(400, "browser_request_invalid", "Browser request must contain one JSON object"))
			return
		}
	}
	// The private RPC transport remembers the /goto receipt even when a Core
	// tool does not copy page_id into its next request. Model arguments cannot
	// replace this trusted document capability.
	if request.URL.Path != "/goto" && request.URL.Path != "/health" {
		if page := request.Header.Get("X-Meadow-Browser-Page"); page != "" {
			if input.PageID != "" && input.PageID != page {
				handler.writeError(writer, embeddedProtocolFailure(409, "browser_document_stale", "request page conflicts with its trusted document receipt"))
				return
			}
			input.PageID = page
		}
	}
	// One native pane has one execution stream. UI owner changes can still revoke
	// its context while an asynchronous page action is running.
	select {
	case handler.gate <- struct{}{}:
		defer func() { <-handler.gate }()
	case <-ctx.Done():
		handler.writeError(writer, embeddedProtocolFailure(409, "browser_context_lost", "Browser operation was cancelled while waiting for the pane"))
		return
	}
	if err := handler.authorize(ctx, session, workspace); err != nil {
		handler.writeError(writer, err)
		return
	}
	result, png, err := handler.route(ctx, session, workspace, request, input)
	// Report owner revocation even when native cancellation returned an error.
	if ownerErr := handler.authorize(ctx, session, workspace); ownerErr != nil {
		err = ownerErr
	}
	if err != nil {
		handler.writeError(writer, err)
		return
	}
	if err := handler.authorize(ctx, session, workspace); err != nil {
		handler.writeError(writer, err)
		return
	}
	if png != nil {
		writer.Header().Set("Content-Type", "image/png")
		writer.Header().Set("Content-Length", strconv.Itoa(len(png)))
		_, _ = writer.Write(png)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(result)
}

func (handler *desktopEmbeddedBrowserHandler) authorize(ctx context.Context, session, workspace string) error {
	if err := ctx.Err(); err != nil {
		return embeddedProtocolFailure(409, "browser_context_lost", "Browser context was revoked or cancelled; do not replay an action automatically")
	}
	if err := handler.embed.authorizeAgent(session, workspace); err != nil {
		return embeddedProtocolFailure(409, "browser_context_lost", err.Error())
	}
	return nil
}

func (handler *desktopEmbeddedBrowserHandler) state() (desktopEmbeddedProtocolState, error) {
	native, ok := handler.embed.impl.(desktopEmbeddedProtocolNative)
	if !ok {
		return desktopEmbeddedProtocolState{}, embeddedProtocolFailure(501, "browser_embedded_protocol_unavailable", "native document lifecycle support is unavailable")
	}
	generation, loading, nativeError := native.documentState()
	pageURL, title, _ := handler.embed.session()
	return desktopEmbeddedProtocolState{generation, loading, nativeError, pageURL, title}, nil
}

func (handler *desktopEmbeddedBrowserHandler) pin(input desktopEmbeddedProtocolInput) (desktopEmbeddedProtocolState, error) {
	state, err := handler.state()
	if err != nil {
		return state, err
	}
	if input.PageID != "" && input.PageID != state.pageID() {
		return state, embeddedProtocolFailure(409, "browser_document_stale", "the requested Browser page is no longer the visible document")
	}
	if state.generation == 0 || state.url == "" {
		return state, embeddedProtocolFailure(409, "browser_document_missing", "the embedded Browser has no loaded document")
	}
	if state.loading {
		return state, embeddedProtocolFailure(409, "browser_document_loading", "the embedded document is still loading")
	}
	if state.err != "" {
		return state, embeddedProtocolFailure(503, "browser_navigation_failed", state.err)
	}
	return state, nil
}

func (handler *desktopEmbeddedBrowserHandler) unchanged(before desktopEmbeddedProtocolState) error {
	after, err := handler.state()
	if err != nil {
		return err
	}
	if after.generation != before.generation || after.loading || after.url != before.url {
		return embeddedProtocolFailure(409, "browser_document_stale", "the visible document changed during this operation; an action may already have occurred")
	}
	if after.err != "" {
		return embeddedProtocolFailure(503, "browser_navigation_failed", after.err)
	}
	return nil
}

func (handler *desktopEmbeddedBrowserHandler) route(ctx context.Context, session, workspace string, request *http.Request, input desktopEmbeddedProtocolInput) (any, []byte, error) {
	path := request.URL.Path
	if request.Method == http.MethodGet && (path == "/health" || path == "/state") {
		state, err := handler.state()
		if err != nil {
			return nil, nil, err
		}
		if input.PageID != "" && input.PageID != state.pageID() {
			return nil, nil, embeddedProtocolFailure(409, "browser_document_stale", "the requested page is not the visible document")
		}
		if path == "/health" && state.generation == 0 {
			return nil, nil, embeddedProtocolFailure(503, "browser_embedded_unavailable", "the native Browser view has not been created")
		}
		return map[string]any{"ok": true, "mode": "desktop-embedded", "embedded": true, "clean_visual_surface": true,
			"page_id": state.pageID(), "url": state.url, "title": state.title,
			"loading": state.loading, "ready": state.generation != 0 && state.url != "" && !state.loading && state.err == "", "error": state.err}, nil, nil
	}
	if request.Method == http.MethodPost && path == "/goto" {
		return handler.navigate(ctx, session, workspace, input)
	}
	if request.Method == http.MethodGet && (path == "/diagnostics" || path == "/console-logs" || path == "/console") {
		return handler.diagnostics(ctx, request, input)
	}
	if request.Method == http.MethodGet && path == "/webmcp/tools" {
		state, err := handler.pin(input)
		if err != nil {
			return nil, nil, err
		}
		catalog, err := handler.catalog(ctx, state)
		return catalog, nil, err
	}
	if request.Method != http.MethodPost {
		return nil, nil, embeddedProtocolFailure(404, "browser_route_not_found", "unsupported embedded Browser route")
	}
	state, err := handler.pin(input)
	if err != nil {
		return nil, nil, err
	}
	if path == "/webmcp/execute" {
		result, err := handler.executeWebMCP(ctx, workspace, state, input)
		return result, nil, err
	}
	if path == "/screenshot" {
		if input.Selector != "" {
			return nil, nil, embeddedProtocolFailure(501, "browser_capture_unsupported", "native selector screenshots are unavailable")
		}
		fullPage := input.FullPage == nil || *input.FullPage
		var png []byte
		if capture, ok := handler.embed.impl.(desktopEmbeddedProtocolCapture); ok {
			png, err = capture.screenshotPinnedContext(ctx, input.Selector, fullPage, state.generation)
		} else {
			err = embeddedProtocolFailure(501, "browser_capture_unsupported", "this native Browser cannot capture the requested selector or viewport")
		}
		if stateErr := handler.unchanged(state); stateErr != nil {
			err = stateErr
		}
		if err == nil && len(png) == 0 {
			err = embeddedProtocolFailure(503, "browser_capture_failed", "native Browser produced no screenshot")
		}
		return nil, png, err
	}
	code := input.Code
	var envelope string
	switch path {
	case "/eval":
		envelope = "value"
	case "/js":
		envelope = "result"
	case "/dom":
		if input.MaxDepth < 0 || input.MaxDepth > 20 {
			return nil, nil, embeddedProtocolFailure(400, "browser_request_invalid", "max_depth must be between 0 and 20")
		}
		code = embeddedProtocolDOMCode(input.Selector, input.MaxDepth)
		envelope = "tree"
	case "/extract":
		code = `(()=>{const el=document.querySelector(` + embeddedProtocolQuote(input.Selector) + `);if(!el)throw new Error('selector not found: '+` + embeddedProtocolQuote(input.Selector) + `);return el.innerText||''})()`
		envelope = "text"
	case "/click", "/fill", "/press":
		code, err = embeddedProtocolInputCode(path, input)
		if err != nil {
			return nil, nil, err
		}
		envelope = "action"
	default:
		return nil, nil, embeddedProtocolFailure(404, "browser_route_not_found", "unsupported embedded Browser route")
	}
	if len(code) == 0 || len(code) > 128<<10 {
		return nil, nil, embeddedProtocolFailure(400, "browser_request_invalid", "bounded JavaScript source is required")
	}
	value, err := handler.evaluate(ctx, state, code)
	if stateErr := handler.unchanged(state); stateErr != nil {
		err = stateErr
	}
	if err != nil {
		return nil, nil, err
	}
	switch envelope {
	case "result":
		return map[string]any{"ok": true, "result": map[string]any{"v": value}}, nil, nil
	case "action":
		return map[string]any{"ok": true, "result": value, "page_id": state.pageID()}, nil, nil
	default:
		return map[string]any{"ok": true, envelope: value, "page_id": state.pageID()}, nil, nil
	}
}

func (handler *desktopEmbeddedBrowserHandler) evaluate(ctx context.Context, state desktopEmbeddedProtocolState, code string) (json.RawMessage, error) {
	native, ok := handler.embed.impl.(desktopEmbeddedProtocolPinnedNative)
	if !ok {
		return nil, embeddedProtocolFailure(501, "browser_embedded_protocol_unavailable", "native document-pinned asynchronous evaluation is unavailable")
	}
	if ctx.Err() != nil {
		return nil, embeddedProtocolFailure(409, "browser_context_lost", "Browser operation was cancelled before dispatch")
	}
	raw, err := native.evalPinnedContext(ctx, "return await (0,eval)("+embeddedProtocolQuote(code)+");", true, state.generation)
	if err != nil {
		return nil, embeddedProtocolFailure(400, "browser_javascript_failed", err.Error())
	}
	if len(raw) > 2<<20 || !json.Valid([]byte(raw)) {
		return nil, embeddedProtocolFailure(502, "browser_response_invalid", "native JavaScript result is not bounded JSON")
	}
	return json.RawMessage(raw), nil
}

func (handler *desktopEmbeddedBrowserHandler) navigate(ctx context.Context, session, workspace string, input desktopEmbeddedProtocolInput) (any, []byte, error) {
	if input.SettleMS < 0 || input.SettleMS > 10000 || (input.WaitUntil != "" && input.WaitUntil != "load" && input.WaitUntil != "network_idle") || (input.WaitStrategy != "" && input.WaitStrategy != "paint") {
		return nil, nil, embeddedProtocolFailure(400, "browser_request_invalid", "unsupported or unbounded navigation wait")
	}
	target, readRoot, err := embeddedProtocolTarget(input.URL, workspace)
	if err != nil {
		return nil, nil, err
	}
	native, ok := handler.embed.impl.(desktopEmbeddedProtocolPinnedNative)
	if !ok {
		return nil, nil, embeddedProtocolFailure(501, "browser_embedded_protocol_unavailable", "native navigation lifecycle is unavailable")
	}
	handler.embed.agentNavigation(session, target)
	if err := handler.authorize(ctx, session, workspace); err != nil {
		return nil, nil, err
	}
	before, err := handler.state()
	if err != nil {
		return nil, nil, err
	}
	if input.PageID != "" && input.PageID != before.pageID() {
		return nil, nil, embeddedProtocolFailure(409, "browser_document_stale", "navigation page no longer matches the visible document")
	}
	if err := native.navigatePinnedContext(ctx, target, readRoot, before.generation); err != nil {
		return nil, nil, embeddedProtocolFailure(503, "browser_navigation_failed", err.Error())
	}
	state, err := handler.pin(desktopEmbeddedProtocolInput{})
	if err != nil {
		return nil, nil, err
	}
	// Both load and paint are observed in the actual document. network_idle
	// additionally requires the installed document-start activity tracker.
	wait := `new Promise((resolve,reject)=>{const deadline=performance.now()+15000;const network=` + strconv.FormatBool(input.WaitUntil == "network_idle") + `;const settle=` + strconv.Itoa(input.SettleMS) + `;let readyAt=null;function poll(){if(performance.now()>deadline){reject(new Error('navigation readiness deadline'));return;}const n=window.__meadowEmbedNetwork;if(network&&(!n||n.installed!==true)){reject(new Error('network idle observation unavailable'));return;}const loaded=document.readyState==='complete';const quiet=!network||(n.pending===0&&performance.now()-n.lastActivity>=500);if(loaded&&quiet){if(readyAt===null)readyAt=performance.now();if(performance.now()-readyAt>=settle){requestAnimationFrame(()=>requestAnimationFrame(()=>resolve({ready_state:document.readyState,paint_frames:2,wait_until:network?'network_idle':'load'})));return;}}else readyAt=null;setTimeout(poll,25);}poll();})`
	proof, err := handler.evaluate(ctx, state, wait)
	if err == nil {
		err = handler.unchanged(state)
	}
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"ok": true, "page_id": state.pageID(), "url": state.url, "ready": true, "load_evidence": proof}, nil, nil
}

func (handler *desktopEmbeddedBrowserHandler) diagnostics(ctx context.Context, request *http.Request, input desktopEmbeddedProtocolInput) (any, []byte, error) {
	state, err := handler.state()
	if err != nil {
		return nil, nil, err
	}
	if input.PageID != "" && input.PageID != state.pageID() {
		return nil, nil, embeddedProtocolFailure(409, "browser_document_stale", "diagnostics page no longer matches the visible document")
	}
	native, ok := handler.embed.impl.(desktopEmbeddedProtocolDiagnostics)
	if !ok {
		return nil, nil, embeddedProtocolFailure(501, "browser_diagnostics_unavailable", "native structured diagnostics are unavailable")
	}
	limit := 200
	if requested := request.URL.Query().Get("limit"); requested != "" {
		limit, err = strconv.Atoi(requested)
		if err != nil || limit < 1 || limit > 200 {
			return nil, nil, embeddedProtocolFailure(400, "browser_request_invalid", "diagnostics limit must be between 1 and 200")
		}
	}
	raw, err := native.diagnosticsContext(ctx, limit, request.URL.Query().Get("level"))
	if err != nil {
		return nil, nil, embeddedProtocolFailure(503, "browser_diagnostics_unavailable", err.Error())
	}
	var response struct {
		OK              bool              `json:"ok"`
		ProtocolVersion string            `json:"protocol_version"`
		Entries         []json.RawMessage `json:"entries"`
		LastID          int               `json:"last_id"`
	}
	if len(raw) > 2<<20 || json.Unmarshal([]byte(raw), &response) != nil || !response.OK || response.ProtocolVersion != "meadow.browser.diagnostics.v1" || response.Entries == nil || response.LastID < 0 {
		return nil, nil, embeddedProtocolFailure(502, "browser_diagnostics_unavailable", "native structured diagnostics did not establish its protocol")
	}
	after, err := handler.state()
	if err != nil {
		return nil, nil, err
	}
	// Failure diagnostics remain useful when this same navigation failed. Only
	// changing documents or lifecycle state invalidates the ledger read.
	if after.generation != state.generation || after.url != state.url || after.loading != state.loading || after.err != state.err {
		return nil, nil, embeddedProtocolFailure(409, "browser_document_stale", "diagnostics document changed during collection")
	}
	if ctx.Err() != nil {
		return nil, nil, embeddedProtocolFailure(409, "browser_context_lost", "diagnostics context was cancelled")
	}
	return json.RawMessage(raw), nil, nil
}

type desktopEmbeddedWebMCPDescriptor struct {
	Name                string          `json:"name"`
	Title               string          `json:"title"`
	Description         string          `json:"description"`
	InputSchema         json.RawMessage `json:"inputSchema"`
	Annotations         map[string]bool `json:"annotations"`
	StateChangeDeclared bool            `json:"state_change_declared"`
}

type desktopEmbeddedWebMCPCatalog struct {
	ProtocolVersion string                            `json:"protocol_version"`
	Implementation  string                            `json:"implementation"`
	Revision        int                               `json:"revision"`
	DocumentID      string                            `json:"document_id"`
	URL             string                            `json:"url"`
	Title           string                            `json:"title"`
	Origin          string                            `json:"origin"`
	Tools           []desktopEmbeddedWebMCPDescriptor `json:"tools"`
}

func (handler *desktopEmbeddedBrowserHandler) bridgeCatalog(ctx context.Context, state desktopEmbeddedProtocolState) (desktopEmbeddedWebMCPCatalog, error) {
	var catalog desktopEmbeddedWebMCPCatalog
	value, err := handler.evaluate(ctx, state, `(()=>{const b=window.__meadowWebMCPBridge;if(!b||typeof b.describe!=='function')throw new Error('WebMCP bridge unavailable');return b.describe()})()`)
	if err != nil {
		return catalog, err
	}
	data := embeddedProtocolDecodeString(value)
	if len(data) > 256<<10 || json.Unmarshal(data, &catalog) != nil || catalog.ProtocolVersion != "meadow.webmcp.v1" || catalog.DocumentID == "" || catalog.Revision < 0 || len(catalog.Tools) > 64 || catalog.URL != state.url {
		return catalog, embeddedProtocolFailure(502, "browser_webmcp_unavailable", "WebMCP catalog did not establish current document identity")
	}
	return catalog, handler.unchanged(state)
}

func (handler *desktopEmbeddedBrowserHandler) catalog(ctx context.Context, state desktopEmbeddedProtocolState) (any, error) {
	catalog, err := handler.bridgeCatalog(ctx, state)
	if err != nil {
		return nil, err
	}
	tools := make([]map[string]any, 0, len(catalog.Tools))
	for _, tool := range catalog.Tools {
		var schema map[string]any
		if tool.Name == "" || json.Unmarshal(tool.InputSchema, &schema) != nil || schema == nil {
			return nil, embeddedProtocolFailure(502, "browser_webmcp_unavailable", "WebMCP tool has no bounded object input schema")
		}
		tools = append(tools, map[string]any{"name": tool.Name, "title": tool.Title, "description": tool.Description,
			"input_schema": string(tool.InputSchema), "annotations": tool.Annotations, "state_change_declared": tool.StateChangeDeclared})
	}
	return map[string]any{"ok": true, "protocol_version": catalog.ProtocolVersion, "page_id": state.pageID(), "document_id": catalog.DocumentID,
		"url": catalog.URL, "title": catalog.Title, "origin": catalog.Origin, "implementation": catalog.Implementation, "revision": catalog.Revision, "tools": tools}, nil
}

func (handler *desktopEmbeddedBrowserHandler) executeWebMCP(ctx context.Context, workspace string, state desktopEmbeddedProtocolState, input desktopEmbeddedProtocolInput) (any, error) {
	catalog, err := handler.bridgeCatalog(ctx, state)
	if err != nil {
		return nil, err
	}
	if input.PageID != state.pageID() || input.DocumentID != catalog.DocumentID || input.ExpectedURL != catalog.URL || input.ExpectedOrigin != catalog.Origin || input.ExpectedRevision == nil || *input.ExpectedRevision != catalog.Revision {
		return nil, embeddedProtocolFailure(409, "browser_document_stale", "WebMCP document, URL, origin, page and catalog revision pins must match discovery")
	}
	var descriptor *desktopEmbeddedWebMCPDescriptor
	for index := range catalog.Tools {
		if catalog.Tools[index].Name == input.ToolName {
			descriptor = &catalog.Tools[index]
			break
		}
	}
	if descriptor == nil {
		return nil, embeddedProtocolFailure(400, "browser_webmcp_tool_missing", "tool name does not match the discovered catalog")
	}
	if descriptor.Annotations["destructiveHint"] || descriptor.Annotations["openWorldHint"] || descriptor.Annotations["consequentialHint"] {
		return nil, embeddedProtocolFailure(403, "browser_webmcp_action_denied", "destructive, open-world or consequential WebMCP tools are unavailable")
	}
	if input.StateToolName != "" {
		validStateTool := false
		for _, tool := range catalog.Tools {
			if tool.Name == input.StateToolName && tool.Annotations["readOnlyHint"] && !tool.Annotations["destructiveHint"] && !tool.Annotations["openWorldHint"] && !tool.Annotations["consequentialHint"] {
				validStateTool = true
			}
		}
		if !validStateTool {
			return nil, embeddedProtocolFailure(403, "browser_webmcp_action_denied", "state_tool_name must name a discovered read-only closed-world tool")
		}
	}
	if !descriptor.Annotations["readOnlyHint"] {
		parsed, _ := url.Parse(catalog.URL)
		if !input.AllowStateChange || !descriptor.StateChangeDeclared || parsed == nil || parsed.Scheme != "file" || catalog.Origin != "file://" {
			return nil, embeddedProtocolFailure(403, "browser_webmcp_action_denied", "state-changing tools require explicit declaration, authorization and a pinned local workspace document")
		}
		if _, _, err := embeddedProtocolTarget(catalog.URL, workspace); err != nil {
			return nil, err
		}
	}
	inputJSON := input.InputJSON
	if inputJSON == "" {
		inputJSON = "{}"
	}
	var decoded any
	if len(inputJSON) > 32<<10 || json.Unmarshal([]byte(inputJSON), &decoded) != nil {
		return nil, embeddedProtocolFailure(400, "browser_request_invalid", "WebMCP input_json must be bounded JSON")
	}
	if _, object := decoded.(map[string]any); !object {
		if _, array := decoded.([]any); !array {
			return nil, embeddedProtocolFailure(400, "browser_request_invalid", "WebMCP input_json must be an object or array")
		}
	}
	if !embeddedProtocolSafeJSON(decoded, 0) {
		return nil, embeddedProtocolFailure(400, "browser_request_invalid", "WebMCP input_json contains reserved keys or excessive nesting")
	}
	options, _ := json.Marshal(map[string]any{"expected_revision": *input.ExpectedRevision, "document_id": input.DocumentID,
		"expected_url": input.ExpectedURL, "expected_origin": input.ExpectedOrigin, "allow_state_change": input.AllowStateChange, "state_tool_name": input.StateToolName})
	native, ok := handler.embed.impl.(desktopEmbeddedProtocolPinnedNative)
	if !ok {
		return nil, embeddedProtocolFailure(501, "browser_webmcp_unavailable", "asynchronous native WebMCP execution is unavailable")
	}
	raw, err := native.evalPinnedContext(ctx, `return await window.__meadowWebMCPBridge.execute(`+embeddedProtocolQuote(input.ToolName)+`,`+embeddedProtocolQuote(inputJSON)+`,`+string(options)+`);`, true, state.generation)
	if err != nil {
		return nil, embeddedProtocolFailure(400, "browser_webmcp_execution_failed", err.Error()+"; inspect state before retrying an action")
	}
	if err := handler.unchanged(state); err != nil {
		return nil, err
	}
	var response map[string]json.RawMessage
	if len(raw) > 256<<10 || json.Unmarshal(embeddedProtocolDecodeString(json.RawMessage(raw)), &response) != nil {
		return nil, embeddedProtocolFailure(502, "browser_response_invalid", "WebMCP returned invalid execution evidence; do not replay an action automatically")
	}
	var executed bool
	_ = json.Unmarshal(response["ok"], &executed)
	if !executed {
		return nil, embeddedProtocolFailure(400, "browser_webmcp_execution_failed", "WebMCP did not complete execution; inspect state before retrying")
	}
	var execution struct {
		Revision        int    `json:"revision"`
		DocumentID      string `json:"document_id"`
		ToolName        string `json:"tool_name"`
		ResultJSON      string `json:"result_json"`
		StateToolName   string `json:"state_tool_name"`
		BeforeStateJSON string `json:"before_state_json"`
		AfterStateJSON  string `json:"after_state_json"`
	}
	if json.Unmarshal(embeddedProtocolDecodeString(json.RawMessage(raw)), &execution) != nil || execution.Revision != catalog.Revision || execution.DocumentID != catalog.DocumentID || execution.ToolName != input.ToolName || len(execution.ResultJSON) > 64<<10 || !json.Valid([]byte(execution.ResultJSON)) || execution.StateToolName != input.StateToolName || !embeddedProtocolStateJSON(execution.BeforeStateJSON) || !embeddedProtocolStateJSON(execution.AfterStateJSON) || (input.StateToolName == "" && (execution.BeforeStateJSON != "" || execution.AfterStateJSON != "")) || (input.StateToolName != "" && (execution.BeforeStateJSON == "" || execution.AfterStateJSON == "")) {
		return nil, embeddedProtocolFailure(502, "browser_response_invalid", "WebMCP execution evidence did not match the pinned request; do not replay an action automatically")
	}
	afterCatalog, err := handler.bridgeCatalog(ctx, state)
	if err != nil {
		return nil, err
	}
	if afterCatalog.DocumentID != catalog.DocumentID || afterCatalog.URL != catalog.URL || afterCatalog.Origin != catalog.Origin || afterCatalog.Revision != catalog.Revision {
		return nil, embeddedProtocolFailure(409, "browser_document_stale", "WebMCP document changed during execution; an action may already have occurred")
	}
	response["protocol_version"], _ = json.Marshal("meadow.webmcp.v1")
	response["page_id"], _ = json.Marshal(state.pageID())
	response["url"], _ = json.Marshal(catalog.URL)
	response["origin"], _ = json.Marshal(catalog.Origin)
	return response, nil
}

func embeddedProtocolWorkspace(raw string) (string, error) {
	root := strings.TrimSpace(raw)
	if root == "" || !filepath.IsAbs(root) || strings.ContainsRune(root, '\x00') {
		return "", fmt.Errorf("absolute trusted workspace is required")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("trusted workspace must be a directory")
	}
	return canonical, nil
}

func embeddedProtocolTarget(raw, workspace string) (string, string, error) {
	if len(raw) > 8192 || strings.ContainsRune(raw, '\x00') {
		return "", "", embeddedProtocolFailure(400, "browser_request_invalid", "invalid Browser target URL")
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.User != nil || parsed.Opaque != "" {
		return "", "", embeddedProtocolFailure(400, "browser_request_invalid", "invalid Browser target URL")
	}
	if (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
		return parsed.String(), "", nil
	}
	if parsed.Scheme != "file" || (parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost")) || !filepath.IsAbs(filepath.FromSlash(parsed.Path)) {
		return "", "", embeddedProtocolFailure(400, "browser_request_invalid", "Browser target must be HTTP(S) or a trusted workspace HTML file")
	}
	path, err := filepath.EvalSymlinks(filepath.FromSlash(parsed.Path))
	if err != nil {
		return "", "", embeddedProtocolFailure(403, "browser_target_denied", "local Browser target could not be resolved")
	}
	relative, err := filepath.Rel(workspace, path)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", embeddedProtocolFailure(403, "browser_target_denied", "local Browser target is outside the trusted workspace")
	}
	info, err := os.Stat(path)
	extension := strings.ToLower(filepath.Ext(path))
	if err != nil || !info.Mode().IsRegular() || (extension != ".html" && extension != ".htm") {
		return "", "", embeddedProtocolFailure(403, "browser_target_denied", "local Browser target must be one regular workspace HTML file")
	}
	parsed.Host = ""
	parsed.Path = filepath.ToSlash(path)
	parsed.RawPath = ""
	return parsed.String(), workspace, nil
}

func embeddedProtocolQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func embeddedProtocolDecodeString(value json.RawMessage) []byte {
	var encoded string
	if json.Unmarshal(value, &encoded) == nil {
		return []byte(encoded)
	}
	return value
}

func embeddedProtocolStateJSON(value string) bool {
	return value == "" || (len(value) <= 64<<10 && json.Valid([]byte(value)))
}

func embeddedProtocolSafeJSON(value any, depth int) bool {
	if depth > 32 {
		return false
	}
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "__proto__" || key == "constructor" || key == "prototype" || !embeddedProtocolSafeJSON(child, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range value {
			if !embeddedProtocolSafeJSON(child, depth+1) {
				return false
			}
		}
	}
	return true
}

func embeddedProtocolDOMCode(selector string, maxDepth int) string {
	if selector == "" {
		selector = "body"
	}
	if maxDepth == 0 {
		maxDepth = 8
	}
	return `(()=>{const selector=` + embeddedProtocolQuote(selector) + `;const root=document.querySelector(selector);if(!root)throw new Error('selector not found: '+selector);let bytes=0;const lines=[];function walk(el,depth){if(depth>` + strconv.Itoa(maxDepth) + `||bytes>65536||['SCRIPT','STYLE','NOSCRIPT'].includes(el.tagName))return;const css=getComputedStyle(el);if(css.display==='none'||css.visibility==='hidden')return;const attrs=['id','role','aria-label','type','name','href','disabled','checked'].filter(k=>el.hasAttribute(k)).map(k=>k+'='+JSON.stringify(el.getAttribute(k))).join(' ');const text=Array.from(el.childNodes).filter(n=>n.nodeType===Node.TEXT_NODE).map(n=>n.textContent).join(' ').trim();const line='  '.repeat(depth)+'<'+el.tagName.toLowerCase()+(attrs?' '+attrs:'')+'>'+(text?' '+text:'');bytes+=line.length;lines.push(line);for(const child of el.children)walk(child,depth+1);}walk(root,0);return lines.join('\n')})()`
}

func embeddedProtocolInputCode(route string, input desktopEmbeddedProtocolInput) (string, error) {
	if len(input.Selector) > 4096 || len(input.Text) > 128<<10 || len(input.Key) > 64 || len(input.Modifiers) > 4 {
		return "", embeddedProtocolFailure(400, "browser_request_invalid", "unbounded Browser input request")
	}
	selector := embeddedProtocolQuote(input.Selector)
	prefix := `(()=>{const selector=` + selector + `;const el=selector?document.querySelector(selector):(document.activeElement||document.body);if(!el)throw new Error('selector not found: '+selector);`
	switch route {
	case "/click":
		if input.Selector == "" {
			return "", embeddedProtocolFailure(400, "browser_request_invalid", "click requires selector")
		}
		return prefix + `const r=el.getBoundingClientRect();const css=getComputedStyle(el);if(el.disabled||r.width<=0||r.height<=0||css.display==='none'||css.visibility==='hidden')throw new Error('selector is not actionable: '+selector);el.scrollIntoView({block:'nearest'});el.click();return {dispatched:true}})()`, nil
	case "/fill":
		if input.Selector == "" {
			return "", embeddedProtocolFailure(400, "browser_request_invalid", "fill requires selector")
		}
		return prefix + `if(el.disabled||el.readOnly)throw new Error('selector is not editable: '+selector);el.focus();const text=` + embeddedProtocolQuote(input.Text) + `;if(el instanceof HTMLInputElement||el instanceof HTMLTextAreaElement){const proto=el instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;Object.getOwnPropertyDescriptor(proto,'value').set.call(el,text);}else if(el.isContentEditable)el.textContent=text;else throw new Error('selector is not editable: '+selector);el.dispatchEvent(new Event('input',{bubbles:true}));el.dispatchEvent(new Event('change',{bubbles:true}));return {dispatched:true}})()`, nil
	case "/press":
		if input.Key == "" {
			return "", embeddedProtocolFailure(400, "browser_request_invalid", "press requires key")
		}
		flags := map[string]bool{}
		for _, modifier := range input.Modifiers {
			switch modifier {
			case "shift":
				flags["shiftKey"] = true
			case "cmd", "meta":
				flags["metaKey"] = true
			case "opt", "alt":
				flags["altKey"] = true
			case "ctrl":
				flags["ctrlKey"] = true
			default:
				return "", embeddedProtocolFailure(400, "browser_request_invalid", "unsupported key modifier")
			}
		}
		options := map[string]any{"key": input.Key, "code": input.Code, "bubbles": true, "cancelable": true}
		for key, enabled := range flags {
			options[key] = enabled
		}
		encoded, _ := json.Marshal(options)
		return prefix + `if(selector)el.focus();const options=` + string(encoded) + `;el.dispatchEvent(new KeyboardEvent('keydown',options));el.dispatchEvent(new KeyboardEvent('keyup',options));return {dispatched:true}})()`, nil
	}
	return "", embeddedProtocolFailure(404, "browser_route_not_found", "unsupported Browser input route")
}

func (handler *desktopEmbeddedBrowserHandler) writeError(writer http.ResponseWriter, err error) {
	status, code := 503, "browser_embedded_unavailable"
	var failure *desktopEmbeddedProtocolError
	if errors.As(err, &failure) {
		status, code = failure.status, failure.code
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"ok": false, "error": err.Error(), "code": code})
}
