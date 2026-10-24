//go:build !meadow_protected

package main

import (
	"context"
	"time"
)

type desktopOwnedAgent interface {
	Owned() bool
	SignalStop()
	Stop(context.Context) error
}

func stopDesktopAgentOnShutdown(packageMode string, helper desktopOwnedAgent) error {
	if helper == nil || !helper.Owned() {
		return nil
	}
	if packageMode != "portable" {
		helper.SignalStop()
		return nil
	}
	// The GUI process cannot exit while the cleanup goroutine is still needed.
	// Stop allows two seconds of grace, then kills and reaps only its own child.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return helper.Stop(ctx)
}
