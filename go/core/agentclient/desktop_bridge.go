package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
)

const (
	desktopAgentProbeInterval  = 2 * time.Second
	desktopAgentRestartDelay   = 250 * time.Millisecond
	desktopAgentRestartMaxWait = 5 * time.Second
)

var errDesktopBridgeClosed = errors.New("protected Desktop Agent bridge is closed")

const desktopModelStatusMaxBytes = 1 << 20

// DesktopModelStatus is the presentation-safe lifecycle shape consumed by the
// shared frontend. It deliberately excludes runtime paths, process identifiers
// and the complete protected Agent status document.
type DesktopModelStatus struct {
	Status string `json:"status"`
	Model  string `json:"model"`
	Detail string `json:"detail"`
}

// DesktopBridge is the complete protected Desktop backend seam. Its exported
// contract contains only standard-library types; the Desktop does not receive
// CLI, provider, Core, runtime, transport-address or process types.
//
// Session and terminal event streams remain ordinary agentd SSE requests. The
// middleware forwards them byte-for-byte through daemontransport's private
// authenticated Unix socket, including Last-Event-ID replay and flushing.
type DesktopBridge struct {
	proxy     http.Handler
	transport *daemontransport.Client
	ctx       context.Context
	cancel    context.CancelFunc
	ownership *daemonOwnership

	mu          sync.RWMutex
	runningCall bool
	running     bool
	closed      bool

	// These seams are instance-local so recovery can be exercised without a
	// real bundle or Unix socket. Production instances use the defaults set by
	// NewDesktopBridge.
	ensureDaemon  func(context.Context) error
	healthProbe   func(context.Context) error
	probeInterval time.Duration
	restartDelay  time.Duration

	closeOnce sync.Once
}

var _ interface {
	EventMiddleware(http.Handler) http.Handler
	FrontendModelStatus(context.Context) (DesktopModelStatus, error)
	RunAgentEvents(context.Context) error
	Close() error
} = (*DesktopBridge)(nil)

// NewDesktopBridge starts or reuses the signed bundled daemon and waits for its
// authenticated readiness before exposing the bridge. It never falls back to
// a caller-supplied path or endpoint.
func NewDesktopBridge(parent context.Context) (*DesktopBridge, error) {
	if parent == nil {
		return nil, fmt.Errorf("protected Desktop Agent bridge requires a parent context")
	}
	owner := &daemonOwnership{}
	retained := false
	defer func() {
		if !retained {
			_ = owner.close()
		}
	}()
	parent = owner.context(parent)
	if err := ensureBundledDesktopDaemon(parent); err != nil {
		return nil, err
	}
	proxy, err := daemontransport.NewReverseProxyHandler()
	if err != nil {
		return nil, fmt.Errorf("create protected Desktop Agent proxy: %w", err)
	}
	transport, err := daemontransport.NewClient()
	if err != nil {
		return nil, fmt.Errorf("create protected Desktop Agent transport: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	retained = true
	return &DesktopBridge{
		proxy: proxy, transport: transport, ctx: ctx, cancel: cancel, ownership: owner,
		ensureDaemon:  ensureBundledDesktopDaemon,
		probeInterval: desktopAgentProbeInterval,
		restartDelay:  desktopAgentRestartDelay,
	}, nil
}

// EventMiddleware serves static Desktop assets through next and forwards every
// other request to agentd. It never falls back after selecting the Agent path:
// an unavailable, unstarted or closed bridge returns a fixed local 503.
func (bridge *DesktopBridge) EventMiddleware(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/agent/client/capabilities" {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "no-store")
			writer.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(writer, `{"protected":true,"modelVaultRequired":false}`)
			return
		}
		if isDesktopAssetRequest(request) {
			next.ServeHTTP(writer, request)
			return
		}
		if bridge == nil || !bridge.available() {
			writeDesktopBridgeUnavailable(writer)
			return
		}
		bridge.proxy.ServeHTTP(writer, request)
	})
}

// FrontendModelStatus adapts agentd's authoritative GET /agent/model response
// to the stable /model/status contract used by the shared Desktop frontend.
// The request stays on the authenticated private transport; Desktop receives
// only the resident model identifier and a coarse lifecycle state.
func (bridge *DesktopBridge) FrontendModelStatus(ctx context.Context) (DesktopModelStatus, error) {
	if bridge == nil || bridge.transport == nil {
		return DesktopModelStatus{}, errDesktopBridgeClosed
	}
	request, err := bridge.transport.NewRequest(ctx, http.MethodGet, "/agent/model", nil)
	if err != nil {
		return DesktopModelStatus{}, err
	}
	response, err := bridge.transport.HTTP.Do(request)
	if err != nil {
		return DesktopModelStatus{}, fmt.Errorf("read protected model status: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, desktopModelStatusMaxBytes+1))
	if err != nil || len(payload) > desktopModelStatusMaxBytes {
		return DesktopModelStatus{}, fmt.Errorf("read protected model status response")
	}
	status, decodeErr := decodeDesktopModelStatus(response.StatusCode, payload)
	if decodeErr != nil {
		return DesktopModelStatus{}, decodeErr
	}
	return status, nil
}

func decodeDesktopModelStatus(statusCode int, payload []byte) (DesktopModelStatus, error) {
	var envelope struct {
		Ready  bool   `json:"ready"`
		Error  string `json:"error"`
		Status struct {
			Model struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"model"`
		} `json:"status"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return DesktopModelStatus{}, fmt.Errorf("decode protected model status: %w", err)
	}
	if statusCode == http.StatusServiceUnavailable {
		return DesktopModelStatus{Status: "starting", Detail: "模型尚未載入"}, nil
	}
	if statusCode < 200 || statusCode >= 300 {
		return DesktopModelStatus{}, fmt.Errorf("protected model status returned HTTP %d", statusCode)
	}
	if !envelope.Ready {
		return DesktopModelStatus{Status: "starting", Detail: "模型尚未載入"}, nil
	}
	modelID := strings.TrimSpace(envelope.Status.Model.ID)
	state := strings.ToLower(strings.TrimSpace(envelope.Status.Model.State))
	if modelID == "" || (state != "ready" && state != "loaded") {
		return DesktopModelStatus{Status: "starting", Model: modelID, Detail: "模型正在準備中"}, nil
	}
	return DesktopModelStatus{Status: "ready", Model: modelID}, nil
}

// RunAgentEvents supervises the private transport used by the per-session SSE
// streams. The streams themselves stay attached to the frontend HTTP requests,
// preserving agentd event IDs, event types, replay cursors and cancellation.
// A protected authentication-only daemon deliberately exits after a terminal
// login result. A full daemon can also crash independently of the Desktop.
// Both are recoverable transport transitions: keep the presentation process
// alive, restart only the signed bundled helper, and make the bridge available
// again after authenticated health succeeds.
func (bridge *DesktopBridge) RunAgentEvents(ctx context.Context) error {
	if bridge == nil {
		return errDesktopBridgeClosed
	}
	if ctx == nil {
		return fmt.Errorf("protected Desktop Agent event context is nil")
	}
	bridge.mu.Lock()
	if bridge.closed {
		bridge.mu.Unlock()
		return errDesktopBridgeClosed
	}
	if bridge.runningCall {
		bridge.mu.Unlock()
		return fmt.Errorf("protected Desktop Agent event pump already started")
	}
	bridge.runningCall = true
	bridge.mu.Unlock()
	defer func() {
		bridge.mu.Lock()
		bridge.runningCall = false
		bridge.running = false
		bridge.mu.Unlock()
	}()

	bridgeContext := bridge.ctx
	if bridgeContext == nil {
		bridgeContext = context.Background()
	}
	runContext, cancelRun := context.WithCancel(ctx)
	stopBridgeCancel := context.AfterFunc(bridgeContext, cancelRun)
	defer func() {
		stopBridgeCancel()
		cancelRun()
	}()
	for {
		if err := bridge.recoverTransport(runContext, bridgeContext); err != nil {
			return err
		}
		bridge.mu.Lock()
		if bridge.closed {
			bridge.mu.Unlock()
			return errDesktopBridgeClosed
		}
		bridge.running = true
		bridge.mu.Unlock()

		probeInterval := bridge.probeEvery()
		timer := time.NewTimer(probeInterval)
		select {
		case <-runContext.Done():
			stopDesktopTimer(timer)
			return runContext.Err()
		case <-bridgeContext.Done():
			stopDesktopTimer(timer)
			return bridgeContext.Err()
		case <-timer.C:
		}
		err := bridge.probeBounded(runContext)
		if err == nil {
			continue
		}
		bridge.mu.Lock()
		bridge.running = false
		bridge.mu.Unlock()
		bridge.closeIdleConnections()
	}
}

func (bridge *DesktopBridge) recoverTransport(ctx, bridgeContext context.Context) error {
	if err := bridge.probeBounded(ctx); err == nil {
		return nil
	}
	bridge.closeIdleConnections()

	// One short second probe avoids replacing a healthy daemon because of a
	// single scheduling hiccup while still recovering a real exit quickly.
	retryDelay := bridge.retryAfter()
	if err := waitDesktopRetry(ctx, bridgeContext, retryDelay); err != nil {
		return err
	}
	if err := bridge.probeBounded(ctx); err == nil {
		return nil
	}
	for {
		ensure := bridge.ensureDaemon
		if ensure == nil {
			ensure = ensureBundledDesktopDaemon
		}
		if err := ensure(ctx); err == nil {
			bridge.closeIdleConnections()
			if err := bridge.probeBounded(ctx); err == nil {
				return nil
			}
		}
		if err := waitDesktopRetry(ctx, bridgeContext, retryDelay); err != nil {
			return err
		}
		if retryDelay < desktopAgentRestartMaxWait {
			retryDelay *= 2
			if retryDelay > desktopAgentRestartMaxWait {
				retryDelay = desktopAgentRestartMaxWait
			}
		}
	}
}

func (bridge *DesktopBridge) probeBounded(ctx context.Context) error {
	probeContext, cancel := context.WithTimeout(ctx, bridge.probeEvery())
	defer cancel()
	return bridge.probe(probeContext)
}

func (bridge *DesktopBridge) probeEvery() time.Duration {
	if bridge.probeInterval > 0 {
		return bridge.probeInterval
	}
	return desktopAgentProbeInterval
}

func (bridge *DesktopBridge) retryAfter() time.Duration {
	if bridge.restartDelay > 0 {
		return bridge.restartDelay
	}
	return desktopAgentRestartDelay
}

func waitDesktopRetry(ctx, bridgeContext context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer stopDesktopTimer(timer)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bridgeContext.Done():
		return bridgeContext.Err()
	case <-timer.C:
		return nil
	}
}

func stopDesktopTimer(timer *time.Timer) {
	if timer != nil && !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (bridge *DesktopBridge) closeIdleConnections() {
	if bridge != nil && bridge.transport != nil && bridge.transport.HTTP != nil {
		bridge.transport.HTTP.CloseIdleConnections()
	}
}

// Close is idempotent. Portable bridges also stop helpers spawned during their
// own lifetime; a reused Desktop helper is never stopped by this bridge.
func (bridge *DesktopBridge) Close() error {
	if bridge == nil {
		return nil
	}
	bridge.closeOnce.Do(func() {
		bridge.mu.Lock()
		bridge.closed = true
		bridge.running = false
		bridge.mu.Unlock()
		if bridge.cancel != nil {
			bridge.cancel()
		}
		bridge.closeIdleConnections()
		if bridge.ownership != nil {
			_ = bridge.ownership.close()
		}
	})
	return nil
}

func (bridge *DesktopBridge) available() bool {
	bridge.mu.RLock()
	defer bridge.mu.RUnlock()
	return bridge.running && !bridge.closed
}

func (bridge *DesktopBridge) probe(ctx context.Context) error {
	if bridge.healthProbe != nil {
		return bridge.healthProbe(ctx)
	}
	if bridge.transport == nil || bridge.transport.HTTP == nil {
		return errDesktopBridgeClosed
	}
	request, err := bridge.transport.NewRequest(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return err
	}
	response, err := bridge.transport.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Meadow Agent health returned %s", response.Status)
	}
	return nil
}

func isDesktopAssetRequest(request *http.Request) bool {
	if request == nil || (request.Method != http.MethodGet && request.Method != http.MethodHead) {
		return false
	}
	path := request.URL.Path
	if path == "" || path == "/" || path == "/index.html" {
		return true
	}
	for _, prefix := range []string{"/assets/", "/build/", "/css/", "/fonts/", "/icons/", "/images/", "/js/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return filepath.Ext(path) != ""
}

func writeDesktopBridgeUnavailable(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]string{
			"type":    "local_agent_unavailable",
			"message": "Protected Meadow Agent is unavailable",
		},
	})
}
