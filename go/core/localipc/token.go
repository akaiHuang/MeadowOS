package localipc

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
	"github.com/hey-meadow/meadow-code/go/core/securitybroker"
)

const (
	CoreAPIKeyEnv      = "MEADOW_CORE_API_KEY"
	CoreTokenFileEnv   = "MEADOW_CORE_TOKEN_FILE"
	CoreRequireAuthEnv = "MEADOW_CORE_REQUIRE_AUTH"
	corePort           = "18081"
)

// CoreTokenPath is deliberately outside the application bundle. Product
// updates can replace the signed bundle without rotating the local IPC secret.
func CoreTokenPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv(CoreTokenFileEnv)); override != "" {
		return filepath.Clean(override), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Meadow home: %w", err)
	}
	return filepath.Join(home, ".meadow", "run", "core-api-token"), nil
}

// CoreToken loads or creates the per-user 256-bit local IPC secret. Existing
// files with unsafe type or permissions are rejected rather than repaired so
// a planted symlink or world-readable token cannot silently become trusted.
func CoreToken() (string, error) {
	if productsecurity.Required() {
		token, err := securitybroker.Secret("core-bearer")
		if err != nil {
			return "", fmt.Errorf("load protected Core IPC token: %w", err)
		}
		if err := validateToken(token); err != nil {
			return "", fmt.Errorf("invalid protected Core IPC token: %w", err)
		}
		return token, nil
	}
	if explicit := strings.TrimSpace(os.Getenv(CoreAPIKeyEnv)); explicit != "" {
		if err := validateToken(explicit); err != nil {
			return "", fmt.Errorf("%s: %w", CoreAPIKeyEnv, err)
		}
		return explicit, nil
	}
	path, err := CoreTokenPath()
	if err != nil {
		return "", err
	}
	if token, err := readToken(path); err == nil {
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create Core IPC directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("protect Core IPC directory: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate Core IPC token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readToken(path)
	}
	if err != nil {
		return "", fmt.Errorf("create Core IPC token: %w", err)
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
		return "", fmt.Errorf("write Core IPC token: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close Core IPC token: %w", closeErr)
	}
	return token, nil
}

func readToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("Core IPC token is not a regular file: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("Core IPC token permissions must be 0600: %s", path)
	}
	if info.Size() <= 0 || info.Size() > 4096 {
		return "", fmt.Errorf("Core IPC token has an invalid size: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Core IPC token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if err := validateToken(token); err != nil {
		return "", fmt.Errorf("invalid Core IPC token: %w", err)
	}
	return token, nil
}

func validateToken(token string) error {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil || len(raw) < 32 {
		return errors.New("token must contain at least 256 bits of URL-safe entropy")
	}
	return nil
}

// EnsureCoreTokenEnvironment is called by the signed meadow-core launcher.
// Development builds pass only the private token path. Protected builds fetch
// the device-bound secret from the Security Broker and pass it only to the
// immediate runtime child; no persistent Core token file is created.
func EnsureCoreTokenEnvironment() error {
	token, err := CoreToken()
	if err != nil {
		return err
	}
	if productsecurity.Required() {
		if err := os.Unsetenv(CoreTokenFileEnv); err != nil {
			return fmt.Errorf("clear Core IPC token path: %w", err)
		}
		if err := os.Setenv(CoreAPIKeyEnv, token); err != nil {
			return fmt.Errorf("set protected Core IPC token: %w", err)
		}
		if err := os.Setenv(CoreRequireAuthEnv, "1"); err != nil {
			return fmt.Errorf("require Core IPC authentication: %w", err)
		}
		return nil
	}
	path, err := CoreTokenPath()
	if err != nil {
		return err
	}
	if err := os.Setenv(CoreTokenFileEnv, path); err != nil {
		return fmt.Errorf("set Core IPC token path: %w", err)
	}
	if err := os.Setenv(CoreRequireAuthEnv, "1"); err != nil {
		return fmt.Errorf("require Core IPC authentication: %w", err)
	}
	return nil
}

// CoreAPIKey transparently upgrades Meadow's fixed local Core endpoint from
// the historical dummy OpenAI key to the per-user IPC token. Other local or
// remote OpenAI-compatible providers retain their configured credentials.
func CoreAPIKey(baseURL, fallback string) string {
	if !IsMeadowCoreURL(baseURL) {
		return fallback
	}
	token, err := CoreToken()
	if err != nil {
		return fallback
	}
	return token
}

func IsMeadowCoreURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" {
		return false
	}
	host := strings.Trim(strings.ToLower(parsed.Hostname()), "[]")
	if host != "localhost" {
		address := net.ParseIP(host)
		if address == nil || !address.IsLoopback() {
			return false
		}
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "http" {
			port = "80"
		} else if parsed.Scheme == "https" {
			port = "443"
		}
	}
	return port == corePort
}
