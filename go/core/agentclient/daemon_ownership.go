package agentclient

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
)

type ownedDaemon interface {
	Owned() bool
	Stop(context.Context) error
}

type daemonOwnershipKey struct{}

// Keep signal handling active until command cleanup has returned. A signal
// directed only at the CLI must cancel its requests, then run owned cleanup.
func portableCommandContext() (context.Context, context.CancelFunc) {
	if buildinfo.DesktopPackageMode != "portable" {
		return context.Background(), func() {}
	}
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// A command or Desktop bridge owns only helpers that it actually spawned.
// Reusing another client's helper never grants authority to stop it.
type daemonOwnership struct {
	mu      sync.Mutex
	closed  bool
	helpers []ownedDaemon
}

func (owner *daemonOwnership) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, daemonOwnershipKey{}, owner)
}

func retainOwnedDaemon(ctx context.Context, helper ownedDaemon) {
	if buildinfo.DesktopPackageMode != "portable" || helper == nil || !helper.Owned() {
		return
	}
	owner, _ := ctx.Value(daemonOwnershipKey{}).(*daemonOwnership)
	if owner == nil {
		return
	}
	owner.mu.Lock()
	if !owner.closed {
		owner.helpers = append(owner.helpers, helper)
		owner.mu.Unlock()
		return
	}
	owner.mu.Unlock()
	// Recovery can finish concurrently with Desktop shutdown. Do not orphan
	// a helper returned after the owning bridge has already closed.
	_ = stopOwnedDaemon(helper)
}

func stopOwnedDaemon(helper ownedDaemon) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return helper.Stop(ctx)
}

func (owner *daemonOwnership) close() error {
	owner.mu.Lock()
	if owner.closed {
		owner.mu.Unlock()
		return nil
	}
	owner.closed = true
	helpers := owner.helpers
	owner.helpers = nil
	owner.mu.Unlock()
	var result error
	for _, helper := range helpers {
		result = errors.Join(result, stopOwnedDaemon(helper))
	}
	return result
}
