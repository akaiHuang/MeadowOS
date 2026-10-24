package agentclient

import (
	"context"
	"os"

	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
)

var (
	desktopBundledDaemonPath = daemontransport.BundledDaemonPath
	desktopUserHomeDir       = os.UserHomeDir
	desktopEnsureDaemon      = daemontransport.EnsureDaemon
)

// ensureBundledDesktopDaemon is the protected Desktop cold-start gate. The
// user's home is the stable bootstrap workspace; individual sessions remain
// bound to the workspace selected through the existing server API.
func ensureBundledDesktopDaemon(ctx context.Context) error {
	binaryPath, err := desktopBundledDaemonPath()
	if err != nil {
		return &UpdateError{Unavailable: true}
	}
	workspace, err := desktopUserHomeDir()
	if err != nil || workspace == "" {
		return &UpdateError{Unavailable: true}
	}
	config, err := authDaemonLaunchConfig(binaryPath, workspace)
	if err != nil {
		return &UpdateError{Unavailable: true}
	}
	helper, err := desktopEnsureDaemon(ctx, config)
	if err != nil {
		return &UpdateError{Unavailable: true}
	}
	retainOwnedDaemon(ctx, helper)
	return nil
}
