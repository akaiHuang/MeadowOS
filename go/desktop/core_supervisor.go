//go:build !meadow_protected

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
)

type coreCapability int

const (
	coreAbsent coreCapability = iota
	coreCompatible
	coreStale
	coreIncompatible
)

type desiredCoreIdentity struct {
	editionProfile                   string
	modelPathFingerprint             string
	upstreamConfigurationFingerprint string
	buildIdentity                    string
	configuredBackend                string
}

type meadowCoreProbe struct {
	capability           coreCapability
	err                  error
	corePID              int
	activeRequests       int
	editionProfile       string
	modelPathFingerprint string
}

func meadowCoreHealthURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	if !isMeadowCoreBaseURL(baseURL) {
		return "", fmt.Errorf("not a Meadow Core endpoint: %s", baseURL)
	}
	parsed.Path = "/health"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func resolvedPathFingerprint(rawPath string) string {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	digest := sha256.Sum256([]byte(filepath.Clean(path)))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func currentDesiredCoreIdentity() *desiredCoreIdentity {
	profile := strings.TrimSpace(os.Getenv("MEADOW_EDITION_PROFILE"))
	if profile == "" {
		profile = strings.TrimSpace(os.Getenv("MEADOW_MODEL_EDITION"))
	}
	modelPathFingerprint := resolvedPathFingerprint(os.Getenv("MEADOW_CORE_MODEL_35B_PATH"))
	upstreamConfigurationFingerprint := ""
	if desktopUsesUpstreamBackend() {
		// This profile serves a remote API through Core, not a local 35B
		// artifact. Bind reuse to its non-secret upstream configuration.
		if upstream, err := loadDesktopUpstreamProfile(); err == nil {
			upstreamConfigurationFingerprint = upstream.configurationFingerprint()
		} else {
			upstreamConfigurationFingerprint = "invalid-upstream-profile"
		}
		modelPathFingerprint = ""
	}
	buildIdentity := buildinfo.CoreBuildIdentity()
	backend := strings.TrimSpace(os.Getenv("MEADOW_CORE_BACKEND"))
	if profile == "" && modelPathFingerprint == "" && buildIdentity == "" && backend == "" {
		return nil
	}
	if profile == "" {
		profile = "standard"
	}
	if backend == "" {
		backend = "sglang-mlx"
	}
	return &desiredCoreIdentity{
		editionProfile:                   profile,
		modelPathFingerprint:             modelPathFingerprint,
		upstreamConfigurationFingerprint: upstreamConfigurationFingerprint,
		buildIdentity:                    buildIdentity,
		configuredBackend:                backend,
	}
}

func probeMeadowCore(ctx context.Context, client *http.Client, baseURL string, desired *desiredCoreIdentity) meadowCoreProbe {
	healthURL, err := meadowCoreHealthURL(baseURL)
	if err != nil {
		return meadowCoreProbe{capability: coreIncompatible, err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return meadowCoreProbe{capability: coreIncompatible, err: err}
	}
	res, err := client.Do(req)
	if err != nil {
		connection, dialErr := net.DialTimeout("tcp", "127.0.0.1:18081", 300*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return meadowCoreProbe{
				capability: coreIncompatible,
				err:        fmt.Errorf("port 18081 has a listener that did not answer Meadow Core health: %w", err),
			}
		}
		return meadowCoreProbe{capability: coreAbsent}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return meadowCoreProbe{
			capability: coreIncompatible,
			err:        fmt.Errorf("port 18081 is occupied by an incompatible service (HTTP %d)", res.StatusCode),
		}
	}
	var body struct {
		Service                          string `json:"service"`
		RuntimeBackend                   string `json:"runtime_backend"`
		ConfiguredBackend                string `json:"configured_backend"`
		BuildIdentity                    string `json:"build_identity"`
		CorePID                          int    `json:"core_pid"`
		EditionProfile                   string `json:"edition_profile"`
		ModelPathFingerprint             string `json:"model_path_fingerprint"`
		UpstreamConfigurationFingerprint string `json:"upstream_configuration_fingerprint"`
		Model                            struct {
			Active int `json:"active"`
		} `json:"model"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return meadowCoreProbe{
			capability: coreIncompatible,
			err:        fmt.Errorf("port 18081 did not return Meadow Core status: %w", err),
		}
	}
	if !strings.EqualFold(strings.TrimSpace(body.Service), "Meadow Core") {
		return meadowCoreProbe{
			capability: coreIncompatible,
			err:        fmt.Errorf("port 18081 belongs to %q, not Meadow Core", body.Service),
		}
	}
	probe := meadowCoreProbe{
		capability:           coreCompatible,
		corePID:              body.CorePID,
		activeRequests:       body.Model.Active,
		editionProfile:       strings.TrimSpace(body.EditionProfile),
		modelPathFingerprint: strings.TrimSpace(body.ModelPathFingerprint),
	}
	if desired == nil {
		return probe
	}
	profileMatches := strings.EqualFold(probe.editionProfile, desired.editionProfile)
	pathMatches := desired.modelPathFingerprint == "" || probe.modelPathFingerprint == desired.modelPathFingerprint
	buildMatches := desired.buildIdentity == "" || body.BuildIdentity == desired.buildIdentity
	// Compare the configured router, not the active model's resolved backend:
	// Muse 30B legitimately uses MLX-VLM under the SGLang product router.
	backendMatches := desired.configuredBackend == "" || body.ConfiguredBackend == desired.configuredBackend
	upstreamMatches := desired.upstreamConfigurationFingerprint == "" || body.UpstreamConfigurationFingerprint == desired.upstreamConfigurationFingerprint
	if !profileMatches || !pathMatches || !buildMatches || !backendMatches || !upstreamMatches {
		probe.capability = coreStale
		probe.err = fmt.Errorf(
			"running Meadow Core profile is stale (edition=%q model_path=%q build=%q configured_backend=%q upstream_config=%q)",
			probe.editionProfile,
			probe.modelPathFingerprint,
			body.BuildIdentity,
			body.ConfiguredBackend,
			body.UpstreamConfigurationFingerprint,
		)
	}
	return probe
}

func probeMeadowCoreCapability(ctx context.Context, client *http.Client, baseURL string) (coreCapability, error) {
	probe := probeMeadowCore(ctx, client, baseURL, nil)
	return probe.capability, probe.err
}

func meadowCoreProcessCommand(pid int) (string, error) {
	if pid <= 1 {
		return "", fmt.Errorf("invalid Meadow Core PID %d", pid)
	}
	output, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func isMeadowCoreProcessCommand(command string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(command), " "))
	return strings.Contains(normalized, " -m meadow_core") ||
		strings.Contains(normalized, "/meadow-core")
}

func adjacentBundledCore(executable string) string {
	if override := strings.TrimSpace(os.Getenv("MEADOW_DESKTOP_CORE_BIN")); override != "" {
		return override
	}
	if executable == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(executable), "meadow-core")
}

type desktopCoreSupervisor struct {
	baseURL          string
	binary           string
	logPath          string
	client           *http.Client
	ensureMu         sync.Mutex
	mu               sync.Mutex
	cmd              *exec.Cmd
	owned            bool
	backgroundEnsure bool
	lastError        error
}

func newDesktopCoreSupervisor(baseURL string) *desktopCoreSupervisor {
	executable, _ := os.Executable()
	stateDir, _ := os.UserHomeDir()
	logPath := filepath.Join(stateDir, ".meadow", "logs", "meadow-core.log")
	return &desktopCoreSupervisor{
		baseURL: baseURL,
		binary:  adjacentBundledCore(executable),
		logPath: logPath,
		client:  &http.Client{Timeout: 1500 * time.Millisecond},
	}
}

func (s *desktopCoreSupervisor) EnsureRunning(ctx context.Context) error {
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	desired := currentDesiredCoreIdentity()
	probe := probeMeadowCore(probeCtx, s.client, s.baseURL, desired)
	cancel()
	if probe.capability == coreCompatible {
		return nil
	}
	if probe.capability == coreStale {
		if err := s.replaceStaleCore(ctx, probe, desired); err != nil {
			return err
		}
	} else if probe.capability == coreIncompatible {
		return probe.err
	}

	if info, statErr := os.Stat(s.binary); statErr != nil || info.IsDir() {
		if statErr == nil {
			statErr = errors.New("expected an executable file")
		}
		return fmt.Errorf("bundled meadow-core is unavailable: %s: %w", s.binary, statErr)
	}
	if err := os.MkdirAll(filepath.Dir(s.logPath), 0o700); err != nil {
		return fmt.Errorf("create Core log directory: %w", err)
	}
	logFile, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open Core log: %w", err)
	}
	cmd := exec.Command(s.binary)
	cmd.Env = os.Environ()
	if strings.TrimSpace(os.Getenv("MEADOW_CORE_BACKEND")) == "" {
		cmd.Env = append(cmd.Env, "MEADOW_CORE_BACKEND=sglang-mlx")
	}
	if strings.TrimSpace(os.Getenv("MEADOW_CORE_SGLANG_MODEL")) == "" {
		cmd.Env = append(cmd.Env, "MEADOW_CORE_SGLANG_MODEL=Meadow Coder 35B")
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start bundled meadow-core: %w", err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.owned = true
	s.lastError = nil
	s.mu.Unlock()
	exited := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		_ = logFile.Close()
		s.mu.Lock()
		if s.cmd == cmd {
			s.cmd = nil
			s.owned = false
			s.lastError = waitErr
		}
		s.mu.Unlock()
		exited <- waitErr
	}()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	// A first launch from a freshly replaced signed bundle can spend close to a
	// minute in macOS validation and Python/Metal cold-start work before ASGI is
	// reachable. Keep the UI in its existing "starting" state long enough for
	// that legitimate cold path instead of killing a healthy Core at 45s.
	startupCtx, startupCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer startupCancel()
	for {
		select {
		case waitErr := <-exited:
			if waitErr == nil {
				waitErr = errors.New("process exited before readiness")
			}
			return fmt.Errorf("bundled Meadow Core exited during startup: %w; see %s", waitErr, s.logPath)
		case <-startupCtx.Done():
			s.StopOwned()
			return fmt.Errorf("bundled Meadow Core was not ready: %w; see %s", startupCtx.Err(), s.logPath)
		case <-ticker.C:
			probeCtx, probeCancel := context.WithTimeout(startupCtx, time.Second)
			probe := probeMeadowCore(probeCtx, s.client, s.baseURL, desired)
			probeCancel()
			if probe.capability == coreCompatible {
				return nil
			}
			// A newly launched Core opens its TCP listener before the ASGI
			// health endpoint is ready. Treat every non-ready probe as startup
			// progress here; the initial probe above still rejects a port that
			// was already occupied before this supervisor started Core.
		}
	}
}

// EnsureRunningBackground coalesces the frequent Desktop status polls into a
// single lifecycle repair.  The caller can keep rendering the cold/starting
// state while a stale edition is replaced and the new Core reaches health.
func (s *desktopCoreSupervisor) EnsureRunningBackground(ctx context.Context, done func(error)) bool {
	s.mu.Lock()
	if s.backgroundEnsure {
		s.mu.Unlock()
		return false
	}
	s.backgroundEnsure = true
	s.mu.Unlock()
	go func() {
		err := s.EnsureRunning(ctx)
		s.mu.Lock()
		s.backgroundEnsure = false
		s.mu.Unlock()
		if done != nil {
			done(err)
		}
	}()
	return true
}

func (s *desktopCoreSupervisor) replaceStaleCore(ctx context.Context, probe meadowCoreProbe, desired *desiredCoreIdentity) error {
	if probe.activeRequests > 0 {
		return fmt.Errorf("cannot replace stale Meadow Core while %d request(s) are active", probe.activeRequests)
	}
	if probe.corePID <= 1 {
		return errors.New("stale Meadow Core did not provide a safe process identity")
	}
	command, err := meadowCoreProcessCommand(probe.corePID)
	if err != nil {
		return fmt.Errorf("inspect stale Meadow Core process %d: %w", probe.corePID, err)
	}
	if !isMeadowCoreProcessCommand(command) {
		return fmt.Errorf("refusing to replace PID %d because it is not a Meadow Core process", probe.corePID)
	}
	if err := syscall.Kill(probe.corePID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop stale Meadow Core process %d: %w", probe.corePID, err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		current := probeMeadowCore(probeCtx, s.client, s.baseURL, desired)
		cancel()
		if current.capability == coreAbsent {
			return nil
		}
		if current.corePID > 1 && current.corePID != probe.corePID {
			return fmt.Errorf("another Meadow Core process %d appeared while replacing stale process %d", current.corePID, probe.corePID)
		}
	}
	if err := syscall.Kill(probe.corePID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("force stop stale Meadow Core process %d: %w", probe.corePID, err)
	}
	return nil
}

func (s *desktopCoreSupervisor) takeOwnedCmd() *exec.Cmd {
	s.mu.Lock()
	defer s.mu.Unlock()
	cmd := s.cmd
	if !s.owned || cmd == nil || cmd.Process == nil {
		return nil
	}
	s.owned = false
	return cmd
}

// SignalOwned sends SIGTERM to the owned Core process group and returns
// immediately. Wails runs application shutdown on the AppKit main thread;
// waiting here freezes the cursor as the spinning wait cursor.
func (s *desktopCoreSupervisor) SignalOwned() bool {
	cmd := s.takeOwnedCmd()
	if cmd == nil {
		return false
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	return true
}

func (s *desktopCoreSupervisor) StopOwned() bool {
	cmd := s.takeOwnedCmd()
	if cmd == nil {
		return false
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return true
}

func (s *desktopCoreSupervisor) Owned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owned
}
