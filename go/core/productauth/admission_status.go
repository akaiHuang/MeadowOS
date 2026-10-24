package productauth

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sync"
	"time"
)

const maximumAdmissionWindowLife = 15 * time.Minute

// AdmissionWindow is an opaque, process-local binding between a verified
// account session and a server-reserved prompt grant. It cannot be restored
// from JSON or transferred to another Service. It grants no prompt quota by
// itself: the caller must also hold and debit the verified server reservation.
type AdmissionWindow struct {
	mu                sync.Mutex
	service           *Service
	epoch             uint64
	stateDigest       [sha256.Size]byte
	subject           string
	deviceID          string
	expiresAt         time.Time
	preparedAt        time.Time
	monotonicDeadline time.Time
	closed            bool
}

// PrepareAdmissionWindow performs the ordinary fresh authorization read before
// preparing a short admission window. It is for native callers that have
// already verified a server reservation bound to the supplied subject/device.
// Ordinary Status and Authorize behavior is deliberately unchanged.
func (service *Service) PrepareAdmissionWindow(ctx context.Context, subject, deviceID string, reservationExpiresAt time.Time) (*AdmissionWindow, error) {
	if service == nil || ctx == nil {
		return nil, admissionWindowUnavailable()
	}
	access, err := service.currentAccess(ctx)
	if err != nil {
		return nil, err
	}
	return service.admissionWindowFromAccess(ctx, access, subject, deviceID, reservationExpiresAt)
}

// admissionWindowFromAccess avoids another secure-store read when the verified
// reservation client has just obtained currentAccess for the same operation.
// Its access value must be the one used to obtain and verify that reservation.
func (service *Service) admissionWindowFromAccess(ctx context.Context, access accessSession, subject, deviceID string, reservationExpiresAt time.Time) (*AdmissionWindow, error) {
	if service == nil || ctx == nil {
		return nil, admissionWindowUnavailable()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	now := service.now().UTC()
	if service.localDenial != "" || access.token == "" || access.token != service.access.token ||
		access.epoch != service.authorizationEpoch.Load() || access.epoch != service.access.epoch ||
		access.stateDigest != service.access.stateDigest || subject == "" || deviceID == "" ||
		access.lease.Subject != subject || access.lease.DeviceID != deviceID {
		return nil, authorizationChanged()
	}
	expires := now.Add(maximumAdmissionWindowLife)
	for _, deadline := range []time.Time{reservationExpiresAt, access.expiresAt, access.lease.ExpiresAt} {
		if !deadline.After(now) {
			return nil, admissionWindowUnavailable()
		}
		if deadline.Before(expires) {
			expires = deadline
		}
	}
	return &AdmissionWindow{
		service: service, epoch: access.epoch, stateDigest: access.stateDigest,
		subject: subject, deviceID: deviceID, expiresAt: expires.UTC(), preparedAt: now,
		// A backward wall-clock adjustment cannot extend the original lifetime.
		monotonicDeadline: time.Now().Add(expires.Sub(now)),
	}, nil
}

// AdmissionStatus checks an explicitly prepared server-reservation window
// without disk, Keychain, network or stateMu acquisition on the send path.
// The window expires no later than the bearer, signed lease or reservation.
// Local logout, refresh, account changes and observed revocation invalidate it
// immediately. Cross-process changes become visible through the ordinary
// background Status/Refresh handshake or the bounded window expiry.
func (service *Service) AdmissionStatus(ctx context.Context, window *AdmissionWindow) (Status, error) {
	if service == nil || ctx == nil || window == nil || window.service != service {
		return Status{}, admissionWindowUnavailable()
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	window.mu.Lock()
	defer window.mu.Unlock()
	if window.closed {
		return Status{}, admissionWindowUnavailable()
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	access := service.access
	if service.localDenial != "" || access.token == "" || access.epoch != window.epoch ||
		access.epoch != service.authorizationEpoch.Load() || access.stateDigest != window.stateDigest ||
		access.lease.Subject != window.subject || access.lease.DeviceID != window.deviceID {
		window.closed = true
		return Status{}, authorizationChanged()
	}
	now := service.now().UTC()
	if !window.monotonicDeadline.After(time.Now()) || !window.expiresAt.After(now) ||
		!access.expiresAt.After(now) || !access.lease.ExpiresAt.After(now) ||
		now.Before(window.preparedAt.Add(-service.clockRollbackTolerance)) {
		window.closed = true
		return Status{}, admissionWindowUnavailable()
	}
	status := service.statusFromLease(access.lease, access)
	expires := window.expiresAt
	status.AccessExpiresAt = &expires
	if err := status.Validate(); err != nil {
		window.closed = true
		return Status{}, admissionWindowUnavailable()
	}
	// Logout advances the epoch before it waits for mu to erase credentials.
	if service.authorizationEpoch.Load() != window.epoch {
		window.closed = true
		return Status{}, authorizationChanged()
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	return status, nil
}

func admissionWindowUnavailable() error {
	return authError("admission_window_unavailable", "a fresh prompt reservation is required", http.StatusUnauthorized, false, nil)
}
