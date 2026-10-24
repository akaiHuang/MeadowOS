package daemontransport

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
)

var ErrManagedDaemonUnavailable = errors.New("managed Meadow Agent service is not running; start the Meadow user service before opening Desktop")

// ManagedAgentRequested is intentionally a development-only Linux composition.
// It never changes account admission, socket credentials, or helper identity.
func ManagedAgentRequested() (bool, error) {
	return managedAgentProfile(os.Getenv("MEADOW_AGENTD_MANAGED"), runtime.GOOS, buildinfo.VMLab, buildinfo.DesktopPackageMode, fixedProductTransport())
}

func managedAgentProfile(value, platform string, lab bool, packageMode string, public bool) (bool, error) {
	if value == "" {
		return false, nil
	}
	if value != "1" {
		return false, fmt.Errorf("MEADOW_AGENTD_MANAGED accepts only the explicit value 1")
	}
	if platform != "linux" || !lab || packageMode != "portable" || public {
		return false, fmt.Errorf("managed Meadow Agent requires the Linux portable VM development profile")
	}
	return true, nil
}
