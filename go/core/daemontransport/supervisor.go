package daemontransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/localipc"
	"github.com/hey-meadow/meadow-code/go/core/portablepaths"
)

type LaunchConfig struct {
	// AttachOnly reuses the current authenticated profile without changing its
	// model or run settings. When no helper exists, normal startup still applies.
	AttachOnly            bool
	StateRoot             string
	BinaryPath            string
	WorkspaceRoot         string
	CoreBaseURL           string
	ConnectionFile        string
	ConnectionID          string
	ConnectionFingerprint string
	ModelID               string
	ApprovalMode          string
	HarnessMode           string
	ExpectedTeamID        string
	ReadyTimeout          time.Duration
	LogPath               string
	BrowserHandler        http.Handler
	PromptGrants          bool
}

type Supervisor struct {
	mu            sync.Mutex
	command       *exec.Cmd
	done          chan struct{}
	waitErr       error
	reaped        bool
	readySocket   *ownedReadySocket
	socketCleanup sync.Once
	socketError   error
	owned         bool
	browser       *desktopBrowserLaunch
	interruptOnce sync.Once
	interruptErr  error
	killOnce      sync.Once
	killErr       error
	forced        atomic.Bool
}

const ownedDaemonGraceLimit = 5 * time.Second
const ownedDaemonReapLimit = time.Second

type daemonHealth struct {
	VMLab                 bool   `json:"vm_lab,omitempty"`
	PackageMode           string `json:"package_mode,omitempty"`
	StateRootFingerprint  string `json:"state_root_fingerprint,omitempty"`
	ModelID               string `json:"model_id,omitempty"`
	ConnectionMode        bool   `json:"connection_mode"`
	OK                    bool   `json:"ok"`
	PID                   int    `json:"pid"`
	BinaryFingerprint     string `json:"binary_fingerprint"`
	EditionProfile        string `json:"edition_profile"`
	HarnessMode           string `json:"harness_mode"`
	ApprovalMode          string `json:"approval_mode"`
	ConnectionID          string `json:"connection_id"`
	ConnectionFingerprint string `json:"connection_fingerprint"`
	PromptGrants          bool   `json:"prompt_grants,omitempty"`
}

var ErrPortableDaemonConflict = errors.New("existing Meadow Agent cannot be safely reused; close Meadow Desktop before opening this profile again")

// EnsureDaemon reuses an already healthy local daemon or starts the supplied
// signed bundle executable. It never falls back to a TCP or cloud endpoint.
func EnsureDaemon(ctx context.Context, config LaunchConfig) (*Supervisor, error) {
	constrained, err := constrainLaunchConfig(config)
	if err != nil {
		return nil, err
	}
	config = constrained
	managed, err := ManagedAgentRequested()
	if err != nil {
		return nil, err
	}
	if managed {
		// A service owns the effective model/run settings. Only the exact
		// authenticated profile/build may be attached; never restart it here.
		config.AttachOnly = true
	}
	if buildinfo.DesktopPackageMode == "portable" {
		wantedProfile, err := stateRootFingerprint(config.StateRoot)
		if err != nil {
			return nil, err
		}
		transportProfile, err := stateRootFingerprint("")
		if err != nil {
			return nil, err
		}
		if wantedProfile != transportProfile {
			return nil, fmt.Errorf("portable Agent state root differs from the selected transport profile")
		}
	}
	binaryPath, err := validateDaemonBinary(config.BinaryPath, config.ExpectedTeamID)
	if err != nil {
		return nil, err
	}
	expectedFingerprint, err := executableFingerprint(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("fingerprint bundled Meadow Agent: %w", err)
	}
	if buildinfo.DesktopPackageMode == "portable" {
		exists, err := existingPortableDaemon(ctx, config, expectedFingerprint)
		if err != nil {
			return nil, err
		}
		if exists {
			return &Supervisor{}, nil
		}
		if managed {
			return nil, ErrManagedDaemonUnavailable
		}
	} else if health, healthErr := probeDaemonHealth(ctx); healthErr == nil {
		if err := rejectDesktopBrowserReuse(config); err != nil {
			return nil, err
		}
		if daemonHealthMatchesLaunch(health, expectedFingerprint, config) {
			return &Supervisor{}, nil
		}
		if err := stopStaleDaemon(ctx); err != nil {
			return nil, fmt.Errorf("replace stale Meadow Agent: %w", err)
		}
	}
	workspaceRoot, err := filepath.Abs(strings.TrimSpace(config.WorkspaceRoot))
	if err != nil {
		return nil, fmt.Errorf("resolve Agent workspace: %w", err)
	}
	logFile, err := openDaemonLog(config.LogPath)
	if err != nil {
		return nil, err
	}

	args := []string{"-workspace", workspaceRoot}
	if config.ConnectionFile != "" {
		args = append(args, "-connection-file", config.ConnectionFile)
	}
	if config.ConnectionID != "" {
		args = append(args, "-connection-id", config.ConnectionID)
	}
	if value := strings.TrimSpace(config.CoreBaseURL); value != "" {
		args = append(args, "-core-base-url", value)
	}
	if value := strings.TrimSpace(config.ModelID); value != "" {
		args = append(args, "-model", value)
	}
	if value := strings.TrimSpace(config.ApprovalMode); value != "" {
		args = append(args, "-approval-mode", value)
	}
	if value := strings.TrimSpace(config.HarnessMode); value != "" {
		args = append(args, "-harness-mode", value)
	}

	environment, err := daemonEnvironment()
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}
	command := exec.Command(binaryPath, args...)
	command.Env = environment
	if config.PromptGrants {
		command.Env = append(command.Env, "MEADOW_ENABLE_PROMPT_GRANTS=1")
	}
	command.Stdout = logFile
	command.Stderr = logFile
	var browser *desktopBrowserLaunch
	if config.BrowserHandler != nil {
		browser, err = prepareDesktopBrowserLaunch(ctx, config.BrowserHandler)
		if err != nil {
			_ = logFile.Close()
			return nil, err
		}
		command.ExtraFiles = browser.inheritedFiles()
		command.Args = append(command.Args, "-desktop-browser-fd", "3", "-desktop-browser-secret-fd", "4")
	}
	if err := command.Start(); err != nil {
		browser.close()
		_ = logFile.Close()
		return nil, fmt.Errorf("start bundled Meadow Agent: %w", err)
	}
	browser.closeInherited()
	supervisor := superviseOwnedCommand(command, browser, func() { _ = logFile.Close() })

	timeout := config.ReadyTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	readyContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := supervisor.waitReady(readyContext, expectedFingerprint, config); err != nil {
		stopContext, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = supervisor.Stop(stopContext)
		stopCancel()
		return nil, err
	}
	if buildinfo.DesktopPackageMode == "portable" {
		if err := supervisor.captureReadySocket(readyContext, expectedFingerprint, config); err != nil {
			stopContext, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = supervisor.Stop(stopContext)
			stopCancel()
			return nil, fmt.Errorf("capture owned Meadow Agent socket: %w", err)
		}
	}
	return supervisor, nil
}

// Called only after this process successfully starts command. The one Wait
// goroutine reaps that exact child and publishes a repeatable completion result.
func superviseOwnedCommand(command *exec.Cmd, browser *desktopBrowserLaunch, afterExit func()) *Supervisor {
	supervisor := &Supervisor{
		command: command,
		done:    make(chan struct{}),
		owned:   true,
		browser: browser,
	}
	go func() {
		err := command.Wait()
		browser.close()
		if afterExit != nil {
			afterExit()
		}
		supervisor.mu.Lock()
		supervisor.waitErr = err
		supervisor.reaped = true
		supervisor.mu.Unlock()
		_ = supervisor.cleanupReapedSocket()
		close(supervisor.done)
	}()
	return supervisor
}

func (supervisor *Supervisor) Owned() bool {
	return supervisor != nil && supervisor.owned
}

func (supervisor *Supervisor) waitReady(ctx context.Context, expectedFingerprint string, config LaunchConfig) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		probe := probeDaemonHealth
		if buildinfo.DesktopPackageMode == "portable" {
			probe = probeExistingDaemonIdentity
		}
		if health, err := probe(ctx); err == nil {
			if supervisor.command == nil || supervisor.command.Process == nil || !daemonReadinessMatchesLaunch(health, expectedFingerprint, config, supervisor.command.Process.Pid) {
				return fmt.Errorf("Meadow Agent readiness identity does not match the spawned process")
			}
			if supervisor.browser.authenticated() {
				return nil
			}
		}
		select {
		case <-supervisor.done:
			err := supervisor.waitResult()
			if err == nil {
				return fmt.Errorf("Meadow Agent exited before becoming ready")
			}
			return fmt.Errorf("Meadow Agent exited before becoming ready: %w", err)
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("wait for Meadow Agent readiness: %w", ctx.Err())
		}
	}
}

// Auth/control clients attach to the profile's effective configuration even on
// a cold start. The helper resolves the selected catalog itself; readiness must
// still prove this is the exact child and the intended binary and state root.
func daemonReadinessMatchesLaunch(health daemonHealth, expectedFingerprint string, config LaunchConfig, spawnedPID int) bool {
	if config.PromptGrants && !health.PromptGrants {
		return false
	}
	if !health.OK || spawnedPID <= 1 || health.PID != spawnedPID {
		return false
	}
	if buildinfo.DesktopPackageMode == "portable" && config.AttachOnly {
		profile, err := stateRootFingerprint(config.StateRoot)
		return err == nil && health.PackageMode == "portable" && expectedFingerprint != "" && health.BinaryFingerprint == expectedFingerprint && health.StateRootFingerprint == profile
	}
	return daemonHealthMatchesLaunch(health, expectedFingerprint, config)
}

// SignalStop interrupts an owned daemon and returns without waiting. Desktop
// quit runs on the AppKit main thread; blocking here freezes the UI cursor.
func (supervisor *Supervisor) SignalStop() {
	if supervisor == nil || !supervisor.owned || supervisor.command == nil || supervisor.command.Process == nil {
		return
	}
	supervisor.browser.close()
	_ = supervisor.interruptOwned()
}

// Stop terminates only a daemon started by this supervisor. Reused daemons are
// left running for other local clients and resumable sessions. The context
// bounds graceful shutdown; expiry forces this exact child to exit, followed
// by an independent bounded wait for its sole command.Wait goroutine to reap.
func (supervisor *Supervisor) Stop(ctx context.Context) error {
	if supervisor == nil || !supervisor.owned || supervisor.command == nil || supervisor.command.Process == nil {
		return nil
	}
	supervisor.browser.close()
	select {
	case <-supervisor.done:
		return supervisor.stoppedResult()
	default:
	}
	grace, cancel := context.WithTimeout(ctx, ownedDaemonGraceLimit)
	defer cancel()
	if err := supervisor.interruptOwned(); err != nil {
		return supervisor.killAndReapOwned()
	}
	select {
	case <-supervisor.done:
		return supervisor.stoppedResult()
	case <-grace.Done():
		return supervisor.killAndReapOwned()
	}
}

func (supervisor *Supervisor) interruptOwned() error {
	supervisor.interruptOnce.Do(func() {
		supervisor.interruptErr = supervisor.command.Process.Signal(os.Interrupt)
	})
	return supervisor.interruptErr
}

func (supervisor *Supervisor) killAndReapOwned() error {
	select {
	case <-supervisor.done:
		return supervisor.stoppedResult()
	default:
	}
	supervisor.killOnce.Do(func() {
		supervisor.forced.Store(true)
		supervisor.killErr = supervisor.command.Process.Kill()
	})
	if supervisor.killErr != nil && !errors.Is(supervisor.killErr, os.ErrProcessDone) {
		return fmt.Errorf("terminate owned Meadow Agent: %w", supervisor.killErr)
	}
	timer := time.NewTimer(ownedDaemonReapLimit)
	defer timer.Stop()
	select {
	case <-supervisor.done:
		return supervisor.stoppedResult()
	case <-timer.C:
		return fmt.Errorf("owned Meadow Agent did not finish reaping after forced termination")
	}
}

func (supervisor *Supervisor) waitResult() error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.waitErr
}

func (supervisor *Supervisor) stoppedResult() error {
	err := supervisor.waitResult()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() &&
			(status.Signal() == syscall.SIGINT || (supervisor.forced.Load() && status.Signal() == syscall.SIGKILL)) {
			err = nil
		}
	}
	return errors.Join(err, supervisor.cleanupReapedSocket())
}

func waitForDaemon(ctx context.Context, duration time.Duration) error {
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := probeDaemon(ctx); err == nil {
			return nil
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return fmt.Errorf("Meadow Agent is not ready")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func probeDaemon(ctx context.Context) error {
	_, err := probeDaemonHealth(ctx)
	return err
}

func probeDaemonHealth(ctx context.Context) (daemonHealth, error) {
	return probeDaemonEndpoint(ctx, "/health", false)
}

func probeExistingDaemonIdentity(ctx context.Context) (daemonHealth, error) {
	return probeDaemonEndpoint(ctx, "/transport/identity", true)
}

// Probes never provision or rotate a credential. In particular public health
// must not gain side effects merely because another profile occupies a socket.
func probeDaemonEndpoint(ctx context.Context, path string, authenticated bool) (daemonHealth, error) {
	probeContext, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	socketPath, err := DefaultSocketPath()
	if err != nil {
		return daemonHealth{}, err
	}
	token := ""
	if authenticated {
		token, err = localipc.ExistingAgentToken()
		if err != nil {
			return daemonHealth{}, err
		}
	}
	client := newClientAtSocket(socketPath, token)
	return probeDaemonWithClient(probeContext, client, path, authenticated, peerPIDFromConn)
}

type authenticatedProbeConn struct {
	net.Conn
	peerPID int
}

// Each probe owns its transport. Check the socket peer before sending a bearer
// and retain that identity on the connection which actually carries the reply.
// The injected resolver is only for in-memory tests; production uses the OS's
// same-user Unix peer credentials through peerPIDFromConn.
func probeDaemonWithClient(ctx context.Context, client *Client, path string, authenticated bool, resolvePeerPID func(net.Conn) (int, error)) (daemonHealth, error) {
	defer client.HTTP.CloseIdleConnections()
	transport, ok := client.HTTP.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil {
		return daemonHealth{}, fmt.Errorf("Meadow Agent probe requires its private socket transport")
	}
	transport.DisableKeepAlives = true
	transport.MaxConnsPerHost = 1
	var peerMu sync.Mutex
	responsePeerPID := 0
	if authenticated {
		if resolvePeerPID == nil {
			return daemonHealth{}, fmt.Errorf("Meadow Agent probe requires peer verification")
		}
		dial := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			connection, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			pid, err := resolvePeerPID(connection)
			if err != nil || pid <= 1 {
				_ = connection.Close()
				return nil, fmt.Errorf("Meadow Agent socket peer could not be verified")
			}
			return &authenticatedProbeConn{Conn: connection, peerPID: pid}, nil
		}
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			peerMu.Lock()
			defer peerMu.Unlock()
			responsePeerPID = 0
			if connection, ok := info.Conn.(*authenticatedProbeConn); ok {
				responsePeerPID = connection.peerPID
			}
		}})
	}
	request, err := client.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return daemonHealth{}, err
	}
	response, err := client.HTTP.Do(request)
	if err != nil {
		return daemonHealth{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return daemonHealth{}, fmt.Errorf("Meadow Agent health returned %s", response.Status)
	}
	var health daemonHealth
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&health); err != nil {
		return daemonHealth{}, fmt.Errorf("decode Meadow Agent health: %w", err)
	}
	if !health.OK {
		return daemonHealth{}, fmt.Errorf("Meadow Agent health is not ready")
	}
	if authenticated {
		peerMu.Lock()
		pid := responsePeerPID
		peerMu.Unlock()
		if pid <= 1 || health.PID != pid {
			return daemonHealth{}, fmt.Errorf("Meadow Agent identity does not match the private socket peer")
		}
	}
	return health, nil
}

// An occupied portable socket is never an invitation to replace its process.
// Authentication, identity and configuration failures all preserve the owner.
func existingPortableDaemon(ctx context.Context, config LaunchConfig, expectedFingerprint string) (bool, error) {
	socketPath, err := DefaultSocketPath()
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(socketPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return true, fmt.Errorf("%w: socket ownership is unavailable", ErrPortableDaemonConflict)
	}
	if _, err := probeDaemonHealth(ctx); err != nil {
		return true, fmt.Errorf("%w: existing helper is not ready", ErrPortableDaemonConflict)
	}
	identity, err := probeExistingDaemonIdentity(ctx)
	if err != nil {
		return true, fmt.Errorf("%w: transport identity could not be authenticated", ErrPortableDaemonConflict)
	}
	profile, err := stateRootFingerprint(config.StateRoot)
	if err != nil {
		return true, err
	}
	if identity.PackageMode != "portable" || identity.StateRootFingerprint != profile || identity.BinaryFingerprint != expectedFingerprint || expectedFingerprint == "" {
		return true, fmt.Errorf("%w: helper belongs to another profile or build", ErrPortableDaemonConflict)
	}
	if err := rejectDesktopBrowserReuse(config); err != nil {
		return true, err
	}
	if !config.AttachOnly && !daemonHealthMatchesLaunch(identity, expectedFingerprint, config) {
		return true, fmt.Errorf("%w: helper configuration differs", ErrPortableDaemonConflict)
	}
	return true, nil
}

func daemonHealthMatchesLaunch(health daemonHealth, expectedFingerprint string, config LaunchConfig) bool {
	if config.PromptGrants && !health.PromptGrants {
		return false
	}
	if buildinfo.DesktopPackageMode == "portable" {
		profile, err := stateRootFingerprint(config.StateRoot)
		if err != nil || health.PackageMode != "portable" || health.StateRootFingerprint != profile {
			return false
		}
		if modelID := strings.TrimSpace(config.ModelID); modelID != "" && health.ModelID != modelID {
			return false
		}
	}
	if health.ConnectionMode != (config.ConnectionFile != "") {
		return false
	}
	if health.ConnectionID != config.ConnectionID || health.ConnectionFingerprint != config.ConnectionFingerprint {
		return false
	}
	if strings.TrimSpace(expectedFingerprint) == "" || health.BinaryFingerprint != expectedFingerprint {
		return false
	}
	desiredEdition := firstNonEmptyEnvironment("MEADOW_EDITION_PROFILE", "MEADOW_MODEL_EDITION")
	if desiredEdition != "" && !strings.EqualFold(strings.TrimSpace(health.EditionProfile), desiredEdition) {
		return false
	}
	if desired := strings.TrimSpace(config.HarnessMode); desired != "" && !strings.EqualFold(strings.TrimSpace(health.HarnessMode), desired) {
		return false
	}
	if desired := strings.TrimSpace(config.ApprovalMode); desired != "" && !strings.EqualFold(strings.TrimSpace(health.ApprovalMode), desired) {
		return false
	}
	return true
}

func stopStaleDaemon(ctx context.Context) error {
	pid, err := daemonPeerPID()
	if err != nil {
		return fmt.Errorf("resolve private socket peer: %w", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := probeDaemon(ctx); err != nil {
			return finishStoppingProcess(ctx, process)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// finishStoppingProcess closes the gap between the private socket disappearing
// and the exact previous daemon process exiting. Without this bounded reap, an
// old generation can remain orphaned after its listener is already gone.
func finishStoppingProcess(ctx context.Context, process *os.Process) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processStillRunning(process) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func processStillRunning(process *os.Process) bool {
	if process == nil {
		return false
	}
	err := process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func validateDaemonBinary(value string, expectedTeamID string) (string, error) {
	binaryPath := strings.TrimSpace(value)
	if binaryPath == "" {
		return "", fmt.Errorf("bundled Meadow Agent binary path is required")
	}
	absolutePath, err := filepath.Abs(binaryPath)
	if err != nil {
		return "", fmt.Errorf("resolve Meadow Agent binary: %w", err)
	}
	info, err := os.Lstat(absolutePath)
	if err != nil {
		return "", fmt.Errorf("inspect Meadow Agent binary: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("unsafe Meadow Agent binary: %s", absolutePath)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("Meadow Agent binary is not executable: %s", absolutePath)
	}
	if err := verifyDaemonIdentity(absolutePath, expectedTeamID); err != nil {
		return "", err
	}
	return absolutePath, nil
}

func openDaemonLog(override string) (*os.File, error) {
	logPath := strings.TrimSpace(override)
	if logPath == "" {
		if buildinfo.DesktopPackageMode == "portable" {
			root, err := portablepaths.StateRoot(os.Getenv("MEADOW_HOME"), os.UserConfigDir)
			if err != nil {
				return nil, err
			}
			logPath = filepath.Join(root, "logs", "meadow-agentd.log")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve Meadow log directory: %w", err)
			}
			logPath = filepath.Join(home, ".meadow", "logs", "meadow-agentd.log")
		}
	}
	absoluteLogPath, err := filepath.Abs(logPath)
	if err != nil {
		return nil, fmt.Errorf("resolve Meadow Agent log path: %w", err)
	}
	logPath = absoluteLogPath
	directory := filepath.Dir(logPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create Meadow Agent log directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("protect Meadow Agent log directory: %w", err)
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Meadow Agent log: %w", err)
	}
	return file, nil
}
