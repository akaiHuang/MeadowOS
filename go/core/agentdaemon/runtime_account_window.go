package agentdaemon

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/productauth"
)

const (
	runtimeAccountRenewalLead = 4 * time.Minute
	runtimeAccountFlightLimit = 3 * time.Minute
	runtimeAccountRetryLimit  = 3
)

var errRuntimeWindowChanged = errors.New("runtime account predecessor changed")

type runtimeAccountWindowService interface {
	PrepareRuntimeAccountWindow(context.Context) (*productauth.RuntimeAccountWindow, error)
	RenewRuntimeAccountWindow(context.Context, *productauth.RuntimeAccountWindow) (*productauth.RuntimeAccountWindow, error)
	RuntimeAccountStatus(context.Context, *productauth.RuntimeAccountWindow) (productauth.Status, error)
}

type runtimeAccountFlight struct {
	done chan struct{}
	err  error
}

// runtimeAccountStatus is intentionally separate from admissionStatus and
// principalAccess. It proves account continuity for tool/model execution and
// account displays, never a new prompt's slot, delivery or settlement proof.
func (bridge *commercialBridge) runtimeAccountStatus(ctx context.Context) (productauth.Status, error) {
	if ctx == nil {
		return productauth.Status{}, promptGrantError("auth_unavailable")
	}
	if err := ctx.Err(); err != nil {
		return productauth.Status{}, err
	}
	manager := bridge.promptGrants
	if manager == nil || manager.runtimeService == nil {
		return bridge.admissionStatus(ctx)
	}
	manager.mu.Lock()
	window := manager.runtimeWindow
	manager.mu.Unlock()
	if window != nil {
		if status, err := manager.runtimeService.RuntimeAccountStatus(ctx, window); err == nil {
			return status, nil
		}
	}
	// During a verified refresh transition all callers join one bounded flight.
	// They must not fan out into six slow full Status calls for each tiny tool.
	flight := manager.startRuntimeAccountFlight(window)
	select {
	case <-ctx.Done():
		return productauth.Status{}, ctx.Err()
	case <-flight.done:
		if flight.err != nil {
			if errors.Is(flight.err, errRuntimeWindowChanged) {
				manager.mu.Lock()
				current := manager.runtimeWindow
				manager.mu.Unlock()
				return manager.runtimeService.RuntimeAccountStatus(ctx, current)
			}
			return productauth.Status{}, flight.err
		}
	}
	manager.mu.Lock()
	window = manager.runtimeWindow
	manager.mu.Unlock()
	return manager.runtimeService.RuntimeAccountStatus(ctx, window)
}

func (manager *promptGrantManager) startRuntimeAccountFlight(previous *productauth.RuntimeAccountWindow) *runtimeAccountFlight {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.runtimeWindow != previous {
		flight := &runtimeAccountFlight{done: make(chan struct{}), err: errRuntimeWindowChanged}
		close(flight.done)
		return flight
	}
	if manager.runtimeFlight != nil {
		return manager.runtimeFlight
	}
	flight := &runtimeAccountFlight{done: make(chan struct{})}
	manager.runtimeFlight = flight
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runtimeAccountFlightLimit)
		defer cancel()
		var window *productauth.RuntimeAccountWindow
		var err error
		if previous == nil {
			window, err = manager.runtimeService.PrepareRuntimeAccountWindow(ctx)
		} else {
			window, err = manager.runtimeService.RenewRuntimeAccountWindow(ctx, previous)
		}
		var status productauth.Status
		if err == nil {
			status, err = manager.runtimeService.RuntimeAccountStatus(ctx, window)
		}
		manager.mu.Lock()
		defer manager.mu.Unlock()
		if err == nil && manager.runtimeWindow == previous {
			manager.runtimeWindow = window
			manager.runtimeSubject = status.Subject
			if status.Entitlement != nil {
				manager.runtimeDevice = status.Entitlement.DeviceID
			}
			manager.runtimeRetries = 0
			manager.scheduleRuntimeAccountRenewalLocked(window)
		} else if err == nil {
			err = errRuntimeWindowChanged
		} else if manager.runtimeWindow == previous && previous != nil && runtimeAccountRetryable(err) && manager.runtimeRetries < runtimeAccountRetryLimit {
			// Retry only a failure that left the existing authority valid (for
			// example a cancelled queued store read). Ambiguous/rejected remote
			// rotations invalidate it and must never revive this old lineage.
			if _, proofErr := manager.runtimeService.RuntimeAccountStatus(context.Background(), previous); proofErr == nil {
				manager.runtimeRetries++
				manager.scheduleRuntimeRetryLocked(previous)
			}
		}
		flight.err = err
		manager.runtimeFlight = nil
		close(flight.done)
	}()
	return flight
}

func (manager *promptGrantManager) scheduleRuntimeAccountRenewalLocked(window *productauth.RuntimeAccountWindow) {
	if manager.runtimeTimer != nil {
		manager.runtimeTimer.Stop()
	}
	remaining := time.Until(window.Deadline())
	if remaining <= 0 {
		return
	}
	lead := runtimeAccountRenewalLead
	if remaining/4 < lead {
		lead = remaining / 4
	}
	delay := remaining - lead
	if delay < time.Second {
		// Extremely short issuer lifetimes fail closed at their real deadline;
		// never turn them into a millisecond credential-rotation loop.
		return
	}
	manager.runtimeTimer = time.AfterFunc(delay, func() {
		manager.mu.Lock()
		current := manager.runtimeWindow == window
		manager.mu.Unlock()
		if current {
			manager.startRuntimeAccountFlight(window)
		}
	})
}

func runtimeAccountRetryable(err error) bool {
	var authErr *productauth.Error
	if errors.As(err, &authErr) {
		if authErr.HTTPStatus == http.StatusUnauthorized || authErr.HTTPStatus == http.StatusForbidden {
			return false
		}
		return authErr.Retryable || authErr.HTTPStatus >= 500
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func (manager *promptGrantManager) scheduleRuntimeRetryLocked(window *productauth.RuntimeAccountWindow) {
	if manager.runtimeTimer != nil {
		manager.runtimeTimer.Stop()
	}
	delay := time.Duration(manager.runtimeRetries*10) * time.Second
	manager.runtimeTimer = time.AfterFunc(delay, func() {
		manager.startRuntimeAccountFlight(window)
	})
}

// Only a separately verified new billing grant can retire a prior principal's
// continuity window. An old task's failed renewal must never switch accounts.
func (manager *promptGrantManager) observeRuntimePrincipalLocked(subject, device string) {
	if manager.runtimeWindow == nil || manager.runtimeService == nil {
		return
	}
	if manager.runtimeSubject == subject && manager.runtimeDevice == device {
		if _, err := manager.runtimeService.RuntimeAccountStatus(context.Background(), manager.runtimeWindow); err == nil {
			return
		}
	}
	if manager.runtimeTimer != nil {
		manager.runtimeTimer.Stop()
	}
	manager.runtimeWindow = nil
	manager.runtimeSubject, manager.runtimeDevice = "", ""
	manager.runtimeRetries = 0
}
