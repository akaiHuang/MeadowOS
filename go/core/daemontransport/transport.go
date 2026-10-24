package daemontransport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/localipc"
	"github.com/hey-meadow/meadow-code/go/core/portablepaths"
	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
)

const virtualBaseURL = "http://meadow-agent.local"

func DefaultSocketPath() (string, error) {
	if buildinfo.DesktopPackageMode == "portable" {
		root, err := portablepaths.StateRoot(os.Getenv("MEADOW_HOME"), os.UserConfigDir)
		if err != nil {
			return "", fmt.Errorf("resolve portable Meadow state: %w", err)
		}
		expected := filepath.Join(root, "run", "agent.sock")
		if override := strings.TrimSpace(os.Getenv("MEADOW_AGENT_SOCKET")); override != "" {
			if !filepath.IsAbs(override) || strings.ContainsRune(override, '\x00') {
				return "", fmt.Errorf("portable Agent socket must be absolute")
			}
			if filepath.Clean(override) != expected && fixedProductTransport() {
				return "", fmt.Errorf("portable Meadow Agent forbids unrelated socket overrides")
			}
			return filepath.Clean(override), nil
		}
		return expected, nil
	}
	if override := strings.TrimSpace(os.Getenv("MEADOW_AGENT_SOCKET")); override != "" {
		if fixedProductTransport() {
			return "", fmt.Errorf("protected Meadow Agent forbids socket path overrides")
		}
		return filepath.Clean(override), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Meadow home: %w", err)
	}
	return filepath.Join(home, ".meadow", "run", "agent.sock"), nil
}

// Listen creates a user-private Unix socket and rejects planted files,
// symlinks and already-running daemons instead of silently replacing them.
func Listen() (net.Listener, error) {
	expectedTeamID := strings.TrimSpace(os.Getenv("MEADOW_EXPECTED_TEAM_ID"))
	if fixedProductTransport() && expectedTeamID != "" {
		return nil, fmt.Errorf("protected Meadow Agent derives peer identity from the current signed product")
	}
	policy := peerPolicy{
		ExpectedTeamID: expectedTeamID,
		RequireSigned:  productsecurity.Required(),
	}
	if err := verifyPeerSupport(policy); err != nil {
		return nil, err
	}
	socketPath, err := DefaultSocketPath()
	if err != nil {
		return nil, err
	}
	if buildinfo.DesktopPackageMode == "portable" {
		if _, err := os.Lstat(socketPath); err == nil {
			return nil, ErrPortableDaemonConflict
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect portable Agent socket: %w", err)
		}
	}
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create Agent socket directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect Agent socket directory: %w", err)
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if buildinfo.DesktopPackageMode == "portable" {
			return nil, ErrPortableDaemonConflict
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("unsafe object occupies Agent socket path: %s", socketPath)
		}
		connection, dialErr := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, fmt.Errorf("Meadow Agent daemon is already running: %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("remove stale Agent socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect Agent socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on Agent socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("protect Agent socket: %w", err)
	}
	return &peerListener{Listener: listener, policy: policy}, nil
}

func fixedProductTransport() bool {
	return protectedTransportBuild() || productsecurity.Required() || strings.TrimSpace(os.Getenv("MEADOW_PUBLIC_RELEASE")) == "1"
}

type peerPolicy struct {
	ExpectedTeamID string
	RequireSigned  bool
}

type peerListener struct {
	net.Listener
	policy peerPolicy
}

func (listener *peerListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := verifyPeerIdentity(connection, listener.policy); err != nil {
			_ = connection.Close()
			continue
		}
		return connection, nil
	}
}

// ProtectedHandler applies the independent Agent credential to every
// workspace, tool, session and terminal route. Health is intentionally the
// only unauthenticated readiness endpoint.
func ProtectedHandler(next http.Handler) (http.Handler, error) {
	return ProtectedHandlerWithConfig(next, HealthConfig{
		HarnessMode:  firstNonEmptyEnvironment("MEADOW_HARNESS_MODE", "MEADOW_AGENT_HARNESS"),
		ApprovalMode: strings.TrimSpace(os.Getenv("MEADOW_APPROVAL_MODE")),
	})
}

// HealthConfig carries the effective parsed Agent startup settings. Empty
// values remain empty; they do not fall back to a conflicting environment.
type HealthConfig struct {
	StateRoot             string
	ModelID               string
	ConnectionMode        bool
	ConnectionID          string
	ConnectionFingerprint string
	HarnessMode           string
	ApprovalMode          string
	// AppliedConnectionIdentity reads the committed native API configuration.
	// Only configuration fields vary; process, binary and profile stay fixed.
	AppliedConnectionIdentity func() (connectionID, modelID, fingerprint string)
}

// ProtectedHandlerWithConfig publishes the settings the Agent actually uses,
// together with its process and executable identity, for strict readiness.
func ProtectedHandlerWithConfig(next http.Handler, config HealthConfig) (http.Handler, error) {
	profileFingerprint := ""
	if buildinfo.DesktopPackageMode == "portable" {
		var err error
		profileFingerprint, err = stateRootFingerprint(config.StateRoot)
		if err != nil {
			return nil, err
		}
	}
	token, err := localipc.AgentToken()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Meadow Agent executable: %w", err)
	}
	fingerprint, err := executableFingerprint(executable)
	if err != nil {
		return nil, fmt.Errorf("fingerprint Meadow Agent executable: %w", err)
	}
	editionProfile := firstNonEmptyEnvironment("MEADOW_EDITION_PROFILE", "MEADOW_MODEL_EDITION")
	harnessMode := strings.TrimSpace(config.HarnessMode)
	approvalMode := strings.TrimSpace(config.ApprovalMode)
	identity := daemonHealth{
		VMLab:        buildinfo.VMLab,
		PromptGrants: os.Getenv("MEADOW_ENABLE_PROMPT_GRANTS") == "1",
		OK:           true, PID: os.Getpid(), BinaryFingerprint: fingerprint,
		PackageMode:          buildinfo.DesktopPackageMode,
		ModelID:              config.ModelID,
		StateRootFingerprint: profileFingerprint, EditionProfile: editionProfile,
		HarnessMode: harnessMode, ApprovalMode: approvalMode,
		ConnectionMode: config.ConnectionMode, ConnectionID: config.ConnectionID,
		ConnectionFingerprint: config.ConnectionFingerprint,
	}
	identified := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/transport/identity") {
			w.Header().Set("Content-Type", "application/json")
			view := identity
			if config.ConnectionMode && config.AppliedConnectionIdentity != nil {
				view.ConnectionID, view.ModelID, view.ConnectionFingerprint = config.AppliedConnectionIdentity()
			}
			if r.URL.Path == "/health" {
				view.StateRootFingerprint = ""
				view.PackageMode = ""
			}
			_ = json.NewEncoder(w).Encode(view)
			return
		}
		next.ServeHTTP(w, r)
	})
	return localipc.RequireBearer(identified, token, map[string]struct{}{
		"/health": {},
	}), nil
}

// State identity is independent of model configuration and credentials. Only
// the authenticated transport endpoint publishes it, never the user's path.
func stateRootFingerprint(explicitRoot string) (string, error) {
	if strings.TrimSpace(explicitRoot) == "" {
		explicitRoot = os.Getenv("MEADOW_HOME")
	}
	root, err := portablepaths.StateRoot(explicitRoot, os.UserConfigDir)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(root))
	return fmt.Sprintf("sha256:%x", digest), nil
}

func executableFingerprint(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}

func firstNonEmptyEnvironment(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

type Client struct {
	HTTP    *http.Client
	token   string
	baseURL string
}

func NewClient() (*Client, error) {
	socketPath, err := DefaultSocketPath()
	if err != nil {
		return nil, err
	}
	token, err := localipc.AgentToken()
	if err != nil {
		return nil, err
	}
	return newClientAtSocket(socketPath, token), nil
}

func newClientAtSocket(socketPath, token string) *Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}
	return &Client{
		HTTP:    &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		token:   token,
		baseURL: virtualBaseURL,
	}
}

func (client *Client) NewRequest(
	ctx context.Context,
	method string,
	path string,
	body any,
) (*http.Request, error) {
	if client == nil || client.HTTP == nil {
		return nil, fmt.Errorf("Meadow Agent client is not initialized")
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode Meadow Agent request: %w", err)
		}
	}
	request, err := http.NewRequestWithContext(
		ctx,
		method,
		strings.TrimRight(client.baseURL, "/")+"/"+strings.TrimLeft(path, "/"),
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, err
	}
	if client.token != "" {
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}
