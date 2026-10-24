// Package agentclient is the protected presentation-client boundary for the
// signed local Meadow Agent. It uses only daemontransport's authenticated Unix
// socket and never accepts a Core, provider, runtime or public TCP endpoint.
//go:build !meadow_protected

package agentclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/cli"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
)

const maxResponseBytes = 16 << 20

// Config contains logical product choices only. In particular, it has no
// provider URL, Core URL, runtime executable or model artifact path.
type Config struct {
	WorkspaceRoot       string
	ModelID             string
	ApprovalMode        engine.ApprovalMode
	HarnessMode         string
	ContextWindowTokens int
}

// ModelStatus is the protected lifecycle status returned by meadow-agentd.
type ModelStatus struct {
	Ready  bool                  `json:"ready"`
	Status runtimecontrol.Status `json:"status"`
	Error  string                `json:"error,omitempty"`
}

// Client owns no inference process. Ensure may start only the signed bundled
// meadow-agentd helper; model and runtime ownership stay behind that boundary.
type Client struct {
	config Config

	ensureOnce    sync.Once
	ensureErr     error
	binaryPath    string
	transportOnce sync.Once
	transport     *daemontransport.Client
	transportErr  error
}

func New(config Config) (*Client, error) {
	workspace := strings.TrimSpace(config.WorkspaceRoot)
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve Meadow workspace: %w", err)
		}
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve Meadow workspace: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect Meadow workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("Meadow workspace is not a directory: %s", absolute)
	}
	config.WorkspaceRoot = absolute
	config.ModelID = strings.TrimSpace(config.ModelID)
	config.HarnessMode = strings.TrimSpace(config.HarnessMode)
	if config.ApprovalMode == "" {
		config.ApprovalMode = engine.ModeDefault
	}
	return &Client{config: config}, nil
}

// Ensure reuses a healthy daemon or starts only the signed helper embedded in
// the current app bundle. Protected clients deliberately reject development
// binary redirection even when daemontransport would permit it.
func (client *Client) Ensure(ctx context.Context) error {
	if client == nil {
		return fmt.Errorf("protected Meadow Agent client is nil")
	}
	client.ensureOnce.Do(func() {
		if strings.TrimSpace(os.Getenv("MEADOW_AGENTD_BIN")) != "" {
			client.ensureErr = fmt.Errorf("protected Meadow Agent forbids daemon binary overrides")
			return
		}
		client.binaryPath, client.ensureErr = daemontransport.BundledDaemonPath()
		if client.ensureErr != nil {
			return
		}
		launchConfig, err := portableControlLaunchConfig(daemontransport.LaunchConfig{
			BinaryPath:    client.binaryPath,
			WorkspaceRoot: client.config.WorkspaceRoot,
			ModelID:       client.config.ModelID,
			ApprovalMode:  string(client.config.ApprovalMode),
			HarnessMode:   client.config.HarnessMode,
		})
		if err != nil {
			client.ensureErr = err
			return
		}
		_, client.ensureErr = daemontransport.EnsureDaemon(ctx, launchConfig)
	})
	return client.ensureErr
}

// RuntimeBinding exposes chat streaming, thought/progress callbacks, tool
// draft/call/result events, permission requests, guidance and context-driven
// cancellation through the existing CLI contract. daemonruntime translates a
// cancelled RunInput context into the daemon's private /cancel route.
func (client *Client) RuntimeBinding(ctx context.Context) (cli.RuntimeBinding, error) {
	if err := client.Ensure(ctx); err != nil {
		return cli.RuntimeBinding{}, err
	}
	launchConfig, err := portableRuntimeLaunchConfig(daemontransport.LaunchConfig{
		BinaryPath: client.binaryPath, WorkspaceRoot: client.config.WorkspaceRoot,
		ModelID: client.config.ModelID, ApprovalMode: string(client.config.ApprovalMode), HarnessMode: client.config.HarnessMode,
	})
	if err != nil {
		return cli.RuntimeBinding{}, err
	}
	return cli.NewDaemonRuntimeBinding(cli.DaemonBindingConfig{
		WorkspaceRoot:         client.config.WorkspaceRoot,
		ModelID:               launchConfig.ModelID,
		ApprovalMode:          client.config.ApprovalMode,
		HarnessMode:           client.config.HarnessMode,
		ContextWindowTokens:   client.config.ContextWindowTokens,
		DaemonBinaryPath:      client.binaryPath,
		StateRoot:             launchConfig.StateRoot,
		ConnectionFile:        launchConfig.ConnectionFile,
		ConnectionID:          launchConfig.ConnectionID,
		ConnectionFingerprint: launchConfig.ConnectionFingerprint,
	}), nil
}

func (client *Client) ActivateModel(ctx context.Context, modelID string) (runtimecontrol.LifecycleResult, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return runtimecontrol.LifecycleResult{}, fmt.Errorf("model is required")
	}
	var result runtimecontrol.LifecycleResult
	err := client.doJSON(ctx, http.MethodPost, "/agent/model", map[string]string{"model": modelID}, &result)
	return result, err
}

func (client *Client) UnloadModel(ctx context.Context, modelID string) (runtimecontrol.LifecycleResult, error) {
	var result runtimecontrol.LifecycleResult
	err := client.doJSON(ctx, http.MethodDelete, "/agent/model", map[string]string{"model": strings.TrimSpace(modelID)}, &result)
	return result, err
}

func (client *Client) Status(ctx context.Context) (ModelStatus, error) {
	response, err := client.request(ctx, http.MethodGet, "/agent/model", nil)
	if err != nil {
		return ModelStatus{}, err
	}
	defer response.Body.Close()
	var status ModelStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&status); err != nil {
		return ModelStatus{}, fmt.Errorf("decode Meadow Agent model status: %w", err)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return status, nil
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		return status, nil
	}
	return ModelStatus{}, fmt.Errorf("Meadow Agent model status returned %s: %s", response.Status, status.Error)
}

func (client *Client) Health(ctx context.Context) error {
	response, err := client.request(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError(response)
	}
	return nil
}

// CancelSession is available to non-TUI callers that retain a daemon session
// ID. Interactive and headless CLI runs normally cancel by cancelling their
// RunInput context, which reaches this same authenticated endpoint.
func (client *Client) CancelSession(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("session id is required")
	}
	return client.doJSON(ctx, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/cancel", map[string]any{}, nil)
}

func (client *Client) doJSON(ctx context.Context, method, path string, body, output any) error {
	response, err := client.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError(response)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(output); err != nil {
		return fmt.Errorf("decode Meadow Agent response: %w", err)
	}
	return nil
}

func (client *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	if err := client.Ensure(ctx); err != nil {
		return nil, err
	}
	// Reuse one authenticated Unix-socket transport. Creating a fresh
	// http.Transport for every two-second model poll leaves one idle file
	// descriptor per request and eventually destabilizes a long-running
	// Desktop session.
	client.transportOnce.Do(func() {
		client.transport, client.transportErr = daemontransport.NewClient()
	})
	if client.transportErr != nil {
		return nil, client.transportErr
	}
	request, err := client.transport.NewRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	response, err := client.transport.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call protected Meadow Agent: %w", err)
	}
	return response, nil
}

func responseError(response *http.Response) error {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	return fmt.Errorf("Meadow Agent returned %s: %s", response.Status, strings.TrimSpace(string(payload)))
}
