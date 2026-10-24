// Package coretransport owns the private Meadow Core process boundary used by
// meadow-agentd. Presentation clients never receive runtime paths or launch
// arguments; they can only request a product model by its public id.
package coretransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/localipc"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

type Supervisor struct {
	mu      sync.Mutex
	baseURL string
	apiKey  string
	command *exec.Cmd
	logFile io.Closer
}

func Ensure(ctx context.Context, baseURL, modelID string) (*Supervisor, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:18081/v1"
	}
	if err := localipc.EnsureCoreTokenEnvironment(); err != nil {
		return nil, err
	}
	supervisor := &Supervisor{
		baseURL: baseURL,
		apiKey:  localipc.CoreAPIKey(baseURL, ""),
	}
	if !supervisor.healthy(ctx) {
		if err := supervisor.startBundledCore(); err != nil {
			return nil, err
		}
		if err := supervisor.waitReady(ctx, 45*time.Second); err != nil {
			_ = supervisor.Stop()
			return nil, err
		}
	}
	if strings.TrimSpace(modelID) != "" {
		activationCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if _, err := supervisor.Activate(activationCtx, modelID); err != nil {
			return nil, err
		}
	}
	return supervisor, nil
}

func (supervisor *Supervisor) Activate(ctx context.Context, modelID string) (provider.MeadowCoreLifecycleResult, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return provider.MeadowCoreLifecycleResult{}, errors.New("Meadow model id is required")
	}
	result, err := provider.ActivateMeadowCoreModel(ctx, supervisor.baseURL, supervisor.apiKey, modelID)
	if err != nil {
		return result, fmt.Errorf("activate local Meadow model %q: %w", modelID, err)
	}
	return result, nil
}

func (supervisor *Supervisor) Status(ctx context.Context) (provider.MeadowCoreStatus, bool, error) {
	return provider.ProbeMeadowCoreStatus(ctx, supervisor.baseURL, supervisor.apiKey)
}

func (supervisor *Supervisor) Stop() error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.logFile != nil {
		defer supervisor.logFile.Close()
		supervisor.logFile = nil
	}
	if supervisor.command == nil || supervisor.command.Process == nil {
		return nil
	}
	err := supervisor.command.Process.Signal(os.Interrupt)
	supervisor.command = nil
	return err
}

func (supervisor *Supervisor) healthy(ctx context.Context) bool {
	// RuntimeControl.Ensure owns two ordered phases: first make the Core HTTP
	// service reachable, then Activate the requested model. During early startup
	// /v1/status can have an empty model_manager, so the capability probe is not
	// yet conclusive even though Core is ready to accept /models/load. Probe the
	// deliberately light service-identity endpoint first so startup cannot be
	// blocked by status collection or model metadata. The capability probe is a
	// compatibility fallback for older Core builds. Never accept a generic
	// process that merely happens to occupy the port.
	healthCtx, cancelHealth := context.WithTimeout(ctx, 1200*time.Millisecond)
	serviceReady := supervisor.coreServiceHealthy(healthCtx)
	cancelHealth()
	if serviceReady {
		return true
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancelProbe()
	_, compatible, err := supervisor.Status(probeCtx)
	return err == nil && compatible
}

func (supervisor *Supervisor) coreServiceHealthy(ctx context.Context) bool {
	parsed, err := url.Parse(supervisor.baseURL)
	if err != nil {
		return false
	}
	parsed.Path = "/health"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return false
	}
	if supervisor.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+supervisor.apiKey)
	}
	response, err := (&http.Client{Timeout: 1200 * time.Millisecond}).Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false
	}
	var health struct {
		Service string `json:"service"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&health); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(health.Service), "Meadow Core")
}

func (supervisor *Supervisor) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if supervisor.healthy(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("bundled Meadow Core did not become ready")
		case <-ticker.C:
		}
	}
}

func (supervisor *Supervisor) startBundledCore() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Meadow Agent executable: %w", err)
	}
	corePath := filepath.Join(filepath.Dir(filepath.Dir(executable)), "MacOS", "meadow-core")
	if strings.TrimSpace(os.Getenv("MEADOW_RELEASE_CHANNEL")) != "public" {
		if override := strings.TrimSpace(os.Getenv("MEADOW_CORE_LAUNCHER")); override != "" {
			if strings.TrimSpace(os.Getenv("MEADOW_ALLOW_EXTERNAL_CORE")) != "1" {
				return errors.New("external Meadow Core requires MEADOW_ALLOW_EXTERNAL_CORE=1")
			}
			corePath = override
		}
	}
	info, err := os.Stat(corePath)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("bundled Meadow Core launcher is unavailable: %s", corePath)
	}
	logPath, err := privateCoreLogPath()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open Meadow Core log: %w", err)
	}
	command := exec.Command(corePath)
	command.Stdout = logFile
	command.Stderr = logFile
	command.Env = os.Environ()
	if err := command.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start bundled Meadow Core: %w", err)
	}
	supervisor.mu.Lock()
	supervisor.command = command
	supervisor.logFile = logFile
	supervisor.mu.Unlock()
	go func() {
		_ = command.Wait()
	}()
	return nil
}

func privateCoreLogPath() (string, error) {
	root := strings.TrimSpace(os.Getenv("MEADOW_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".meadow")
	}
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(logDir, "meadow-core.log"), nil
}
