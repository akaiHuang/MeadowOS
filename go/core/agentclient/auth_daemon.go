package agentclient

import (
	"context"
	"fmt"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
)

func authDaemonLaunchConfig(binaryPath, workspace string) (daemontransport.LaunchConfig, error) {
	return portableControlLaunchConfig(daemontransport.LaunchConfig{
		BinaryPath: binaryPath, WorkspaceRoot: workspace, ReadyTimeout: 20 * time.Second,
	})
}

func ensureAuthDaemon(ctx context.Context, workspace string) error {
	binaryPath, err := daemontransport.BundledDaemonPath()
	if err != nil {
		return fmt.Errorf("protected authentication daemon is unavailable")
	}
	config, err := authDaemonLaunchConfig(binaryPath, workspace)
	if err != nil {
		return fmt.Errorf("protected authentication daemon is unavailable")
	}
	helper, err := daemontransport.EnsureDaemon(ctx, config)
	if err != nil {
		return fmt.Errorf("protected authentication daemon is unavailable")
	}
	retainOwnedDaemon(ctx, helper)
	return nil
}
