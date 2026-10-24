package localipc

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/portablepaths"
	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
	"github.com/hey-meadow/meadow-code/go/core/securitybroker"
)

const (
	AgentAPIKeyEnv    = "MEADOW_AGENT_API_KEY"
	AgentTokenFileEnv = "MEADOW_AGENT_TOKEN_FILE"
)

func AgentTokenPath() (string, error) {
	if buildinfo.DesktopPackageMode == "portable" {
		root, err := portablepaths.StateRoot(os.Getenv("MEADOW_HOME"), os.UserConfigDir)
		if err != nil {
			return "", err
		}
		if override := strings.TrimSpace(os.Getenv(AgentTokenFileEnv)); override != "" {
			if !filepath.IsAbs(override) || strings.ContainsRune(override, '\x00') {
				return "", fmt.Errorf("portable Agent token file must be absolute")
			}
			return filepath.Clean(override), nil
		}
		return filepath.Join(root, "run", "agent-api-token"), nil
	}
	if override := strings.TrimSpace(os.Getenv(AgentTokenFileEnv)); override != "" {
		return filepath.Clean(override), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Meadow home: %w", err)
	}
	return filepath.Join(home, ".meadow", "run", "agent-api-token"), nil
}

// AgentToken is separate from the model Core token. Compromise of a model
// transport credential must not grant workspace, tool or session authority.
func AgentToken() (string, error) {
	return agentToken(true)
}

// ExistingAgentToken checks access to an already running helper without creating
// a credential file or changing any token directory when no token exists.
func ExistingAgentToken() (string, error) {
	return agentToken(false)
}

func agentToken(create bool) (string, error) {
	if buildinfo.DesktopPackageMode == "portable" {
		if _, err := AgentTokenPath(); err != nil {
			return "", err
		}
	}
	if productsecurity.Required() {
		token, err := securitybroker.Secret("agent-bearer")
		if err != nil {
			return "", fmt.Errorf("load protected Agent credential: %w", err)
		}
		if err := validateToken(token); err != nil {
			return "", fmt.Errorf("invalid Security Broker credential: %w", err)
		}
		return token, nil
	}
	if explicit := strings.TrimSpace(os.Getenv(AgentAPIKeyEnv)); explicit != "" {
		if err := validateToken(explicit); err != nil {
			return "", fmt.Errorf("%s: %w", AgentAPIKeyEnv, err)
		}
		return explicit, nil
	}
	path, err := AgentTokenPath()
	if err != nil {
		return "", err
	}
	if token, err := readToken(path); err == nil {
		return token, nil
	} else if !create || !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create Agent IPC directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("protect Agent IPC directory: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate Agent IPC token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readToken(path)
	}
	if err != nil {
		return "", fmt.Errorf("create Agent IPC token: %w", err)
	}
	var writeErr error
	if _, err := file.WriteString(token + "\n"); err != nil {
		writeErr = err
	} else if err := file.Sync(); err != nil {
		writeErr = err
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("write Agent IPC token: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close Agent IPC token: %w", closeErr)
	}
	return token, nil
}
