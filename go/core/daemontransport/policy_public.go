//go:build !meadow_protected

package daemontransport

import "os"

func protectedTransportBuild() bool { return false }

func constrainLaunchConfig(config LaunchConfig) (LaunchConfig, error) {
	return config, nil
}

func daemonEnvironment() ([]string, error) {
	return os.Environ(), nil
}
