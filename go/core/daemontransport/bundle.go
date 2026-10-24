package daemontransport

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// BundledDaemonPath resolves the helper beside the currently executing
// Desktop/CLI binary. Public releases cannot redirect this boundary with an
// environment variable; development overrides require an explicit opt-in.
func BundledDaemonPath() (string, error) {
	publicRelease := fixedProductTransport()
	if override := strings.TrimSpace(os.Getenv("MEADOW_AGENTD_BIN")); override != "" {
		if publicRelease {
			return "", fmt.Errorf("MEADOW_AGENTD_BIN is forbidden in a public release")
		}
		if strings.TrimSpace(os.Getenv("MEADOW_ALLOW_EXTERNAL_AGENTD")) != "1" {
			return "", fmt.Errorf("external Meadow Agent requires MEADOW_ALLOW_EXTERNAL_AGENTD=1")
		}
		return filepath.Abs(override)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve Meadow executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve Meadow executable identity: %w", err)
	}
	return bundledDaemonPathForExecutable(executable, runtime.GOOS)
}

func bundledDaemonPathForExecutable(executable, platform string) (string, error) {
	if platform == "linux" {
		return linuxBundledDaemonPath(executable)
	}
	macOSDirectory := filepath.Dir(executable)
	contentsDirectory := filepath.Dir(macOSDirectory)
	if filepath.Base(macOSDirectory) != "MacOS" || filepath.Base(contentsDirectory) != "Contents" {
		return "", fmt.Errorf("Meadow executable is not inside a macOS app bundle")
	}
	return filepath.Join(contentsDirectory, "Helpers", "meadow-agentd"), nil
}

// Linux installations use one relocatable, canonical prefix. A libexec symlink
// must not redirect the trusted helper outside that payload (or to another app).
func linuxBundledDaemonPath(executable string) (string, error) {
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return "", fmt.Errorf("Linux Meadow executable requires a canonical absolute path")
	}
	switch filepath.Base(executable) {
	case "meadow-desktop", "meadow":
	default:
		return "", fmt.Errorf("unrecognized Linux Meadow executable name")
	}
	bin := filepath.Dir(executable)
	prefix := filepath.Dir(bin)
	if filepath.Base(bin) != "bin" || prefix == string(filepath.Separator) {
		return "", fmt.Errorf("Linux Meadow executable requires <prefix>/bin layout")
	}
	candidate := filepath.Join(prefix, "libexec", "meadow-agentd")
	canonical, err := filepath.EvalSymlinks(candidate)
	if err != nil || canonical != candidate {
		return "", fmt.Errorf("Linux Meadow helper requires canonical <prefix>/libexec layout")
	}
	info, err := os.Lstat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("Linux Meadow bundled helper is missing or not executable")
	}
	return candidate, nil
}
