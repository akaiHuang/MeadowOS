package daemontransport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
)

const maxDesktopProxyRequestBytes int64 = 256 << 20

// stabilizeProxyRequestBody repairs the request shape produced by WKWebView's
// custom-scheme fetch bridge. It can supply a readable Body while leaving
// ContentLength at zero; net/http then correctly assumes there is no outbound
// payload and agentd sees an empty JSON document. Buffering here gives the
// reverse proxy an ordinary replayable HTTP request.
func stabilizeProxyRequestBody(request *http.Request) error {
	if request == nil || request.Body == nil || request.Body == http.NoBody {
		return nil
	}
	payload, err := io.ReadAll(request.Body)
	closeErr := request.Body.Close()
	if err != nil {
		return fmt.Errorf("read Desktop proxy request body: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close Desktop proxy request body: %w", closeErr)
	}
	if len(payload) == 0 {
		request.Body = http.NoBody
		request.ContentLength = 0
		request.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		return nil
	}
	request.Body = io.NopCloser(bytes.NewReader(payload))
	request.ContentLength = int64(len(payload))
	request.TransferEncoding = nil
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	return nil
}

// NewReverseProxyHandler preserves the existing HTTP/SSE contract for a
// presentation shell while all execution remains in the private daemon. The
// handler must only be mounted on a native-app-private origin; it injects the
// daemon credential and therefore must never be exposed as a public listener.
func NewReverseProxyHandler() (http.Handler, error) {
	client, err := NewClient()
	if err != nil {
		return nil, err
	}
	proxy := &httputil.ReverseProxy{
		Transport:     client.HTTP.Transport,
		FlushInterval: -1,
		Director: func(request *http.Request) {
			request.URL.Scheme = "http"
			request.URL.Host = "meadow-agent.local"
			request.Host = "meadow-agent.local"
			request.Header.Set("Authorization", "Bearer "+client.token)
		},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, proxyErr error) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"error": map[string]string{
					"type":    "local_agent_unavailable",
					"message": proxyErr.Error(),
				},
			})
		},
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Startup identity belongs to native IPC clients. The presentation proxy
		// must never inject its bearer into private transport-control requests.
		if request.URL.Path == "/transport" || strings.HasPrefix(request.URL.Path, "/transport/") {
			http.NotFound(writer, request)
			return
		}
		if request.ContentLength > maxDesktopProxyRequestBytes {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(writer).Encode(map[string]any{"error": "Desktop proxy request body is too large"})
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, maxDesktopProxyRequestBytes)
		if err := stabilizeProxyRequestBody(request); err != nil {
			writer.Header().Set("Content-Type", "application/json")
			status := http.StatusBadRequest
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				status = http.StatusRequestEntityTooLarge
			}
			writer.WriteHeader(status)
			_ = json.NewEncoder(writer).Encode(map[string]any{"error": err.Error()})
			return
		}
		proxy.ServeHTTP(writer, request)
	}), nil
}
