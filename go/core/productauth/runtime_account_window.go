package productauth

import (
	"context"
	"time"
)

// RuntimeAccountWindow proves only account continuity for already admitted
// work. Its distinct type cannot authorize a new prompt or reserve quota.
type RuntimeAccountWindow struct {
	proof      *AdmissionWindow
	generation uint64
}

// Deadline is the earliest verified bearer, lease or process-window deadline.
// It is scheduling metadata, never authority to extend a window.
func (window *RuntimeAccountWindow) Deadline() time.Time {
	if window == nil || window.proof == nil {
		return time.Time{}
	}
	window.proof.mu.Lock()
	defer window.proof.mu.Unlock()
	return window.proof.expiresAt
}

// PrepareRuntimeAccountWindow reads current secure authorization before
// preparing a bounded account-only proof. Billing reservations are separate.
func (service *Service) PrepareRuntimeAccountWindow(ctx context.Context) (*RuntimeAccountWindow, error) {
	if service == nil || ctx == nil {
		return nil, admissionWindowUnavailable()
	}
	access, err := service.currentAccess(ctx)
	if err != nil {
		return nil, err
	}
	return service.runtimeAccountWindowFromAccess(ctx, access)
}

func (service *Service) runtimeAccountWindowFromAccess(ctx context.Context, access accessSession) (*RuntimeAccountWindow, error) {
	generation := service.runtimeGeneration.Load()
	proof, err := service.admissionWindowFromAccess(ctx, access, access.lease.Subject, access.lease.DeviceID, access.expiresAt)
	if err != nil {
		return nil, err
	}
	if generation != service.runtimeGeneration.Load() {
		return nil, authorizationChanged()
	}
	return &RuntimeAccountWindow{proof: proof, generation: generation}, nil
}

// RuntimeAccountStatus retains the same epoch, identity, monotonic deadline
// and observed-revocation fences as ordinary prepared admission. It grants no
// billing slot and must not be used as a prompt/delivery authorization.
func (service *Service) RuntimeAccountStatus(ctx context.Context, window *RuntimeAccountWindow) (Status, error) {
	if service == nil || window == nil || window.generation != service.runtimeGeneration.Load() {
		return Status{}, admissionWindowUnavailable()
	}
	status, err := service.AdmissionStatus(ctx, window.proof)
	if err == nil && window.generation != service.runtimeGeneration.Load() {
		return Status{}, authorizationChanged()
	}
	return status, err
}

// RenewRuntimeAccountWindow obtains a newly signed server authorization using
// the normal refresh protocol. It never edits or extends the predecessor.
// A changed account/device cannot inherit an earlier run's continuity proof.
func (service *Service) RenewRuntimeAccountWindow(ctx context.Context, previous *RuntimeAccountWindow) (*RuntimeAccountWindow, error) {
	if service == nil || ctx == nil || previous == nil || previous.proof == nil || previous.proof.service != service || previous.generation != service.runtimeGeneration.Load() {
		return nil, admissionWindowUnavailable()
	}
	previous.proof.mu.Lock()
	subject, device := previous.proof.subject, previous.proof.deviceID
	epoch, digest := previous.proof.epoch, previous.proof.stateDigest
	previous.proof.mu.Unlock()
	access, _, err := service.cachedAccess(ctx)
	if err != nil {
		return nil, err
	}
	if previous.generation != service.runtimeGeneration.Load() || access.lease.Subject != subject || access.lease.DeviceID != device {
		return nil, authorizationChanged()
	}
	if access.epoch != epoch || access.stateDigest != digest {
		// A normal, already verified refresh can win concurrently. The fresh
		// secure snapshot and unchanged login/loss generation prove its lineage.
		// Mint a new bounded proof; never revive the predecessor or rotate twice.
		window, err := service.runtimeAccountWindowFromAccess(ctx, access)
		if err == nil && previous.generation != service.runtimeGeneration.Load() {
			return nil, authorizationChanged()
		}
		return window, err
	}
	if _, err := service.refreshForExpectedEpoch(ctx, &epoch); err != nil {
		return nil, err
	}
	access, err = service.currentAccess(ctx)
	if err != nil {
		return nil, err
	}
	if previous.generation != service.runtimeGeneration.Load() || access.lease.Subject != subject || access.lease.DeviceID != device {
		return nil, authorizationChanged()
	}
	window, err := service.runtimeAccountWindowFromAccess(ctx, access)
	if err == nil && previous.generation != service.runtimeGeneration.Load() {
		return nil, authorizationChanged()
	}
	return window, err
}
