//go:build !meadow_protected

package main

import (
	"context"

	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
)

type desktopModelActivator interface {
	ActivateModel(context.Context, string) (runtimecontrol.LifecycleResult, error)
}

func activateDesktopModel(ctx context.Context, core *desktopCoreSupervisor, models desktopModelActivator, modelID string) (runtimecontrol.LifecycleResult, error) {
	// Activation must verify the same package/profile identity as a prompt.
	// The Agent's service-health check alone cannot identify a stale Core.
	// EnsureRunning refuses replacement while the stale Core has active work.
	if core != nil {
		if err := core.EnsureRunning(ctx); err != nil {
			return runtimecontrol.LifecycleResult{}, err
		}
	}
	return models.ActivateModel(ctx, modelID)
}
