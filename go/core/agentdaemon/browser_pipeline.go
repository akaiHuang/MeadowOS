package agentdaemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/browserclient"
)

// BrowserOwner is the complete browser-helper seam owned by meadow-agentd.
// Implementations must use the inherited anonymous worker transport; protected
// composition never supplies an HTTP, TCP, executable-path or argv fallback.
type BrowserOwner interface {
	Load(context.Context, browserclient.LoadRequest) (*browserclient.LoadResult, error)
	Inspect(context.Context, browserclient.InspectRequest) (*browserclient.InspectResult, error)
	Cancel(context.Context, string) (*browserclient.CancelResult, error)
	Status(context.Context, string) (*browserclient.StatusResult, error)
	Close() error
}

func (server *serverHandler) handleBrowserLoad(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(request, &body, true); err != nil || strings.TrimSpace(body.Path) == "" {
		writeError(writer, http.StatusBadRequest, "relative browser artifact path is required")
		return
	}
	correlationID, err := browserclient.NewCorrelationID()
	if err != nil {
		writeBrowserError(writer, err)
		return
	}
	stopWatching := server.watchBrowserCancellation(request.Context(), correlationID)
	result, err := server.browser.Load(request.Context(), browserclient.LoadRequest{
		CorrelationID: correlationID,
		Path: strings.TrimSpace(body.Path),
	})
	stopWatching()
	if err != nil {
		writeBrowserError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *serverHandler) handleBrowserInspect(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Kind string `json:"kind"`
		MaxEvents int `json:"max_events,omitempty"`
		MaxNodes int `json:"max_nodes,omitempty"`
		MaxDepth int `json:"max_depth,omitempty"`
	}
	if err := decodeJSON(request, &body, true); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid browser inspection request")
		return
	}
	correlationID, err := browserclient.NewCorrelationID()
	if err != nil {
		writeBrowserError(writer, err)
		return
	}
	stopWatching := server.watchBrowserCancellation(request.Context(), correlationID)
	result, err := server.browser.Inspect(request.Context(), browserclient.InspectRequest{
		CorrelationID: correlationID,
		Kind: browserclient.InspectKind(strings.TrimSpace(body.Kind)),
		MaxEvents: body.MaxEvents,
		MaxNodes: body.MaxNodes,
		MaxDepth: body.MaxDepth,
	})
	stopWatching()
	if err != nil {
		writeBrowserError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *serverHandler) handleBrowserCancel(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		CorrelationID string `json:"correlation_id"`
	}
	if err := decodeJSON(request, &body, true); err != nil || strings.TrimSpace(body.CorrelationID) == "" {
		writeError(writer, http.StatusBadRequest, "browser correlation_id is required")
		return
	}
	result, err := server.browser.Cancel(request.Context(), strings.TrimSpace(body.CorrelationID))
	if err != nil {
		writeBrowserError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *serverHandler) handleBrowserStatus(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		CorrelationID string `json:"correlation_id,omitempty"`
	}
	if err := decodeJSON(request, &body, false); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid browser status request")
		return
	}
	result, err := server.browser.Status(request.Context(), strings.TrimSpace(body.CorrelationID))
	if err != nil {
		writeBrowserError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *serverHandler) watchBrowserCancellation(ctx context.Context, correlationID string) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cancelContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = server.browser.Cancel(cancelContext, correlationID)
			cancel()
		case <-done:
		}
	}()
	return func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}
}

func writeBrowserError(writer http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, browserclient.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, browserclient.ErrBusy), errors.Is(err, browserclient.ErrLimit):
		status = http.StatusTooManyRequests
	case errors.Is(err, browserclient.ErrCancelled):
		status = http.StatusConflict
	case errors.Is(err, browserclient.ErrTimeout):
		status = http.StatusGatewayTimeout
	}
	writeError(writer, status, browserclient.Redact(err.Error()))
}
