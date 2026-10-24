package productauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
)

// The native broker returns these fields from one versioned secure document.
// Custom stores can retain the existing interface; they must externally
// serialize operations if multiple processes share their backing state.
type authorizationSnapshot struct {
	refresh string
	lease   string
	denial  string
}

type authorizationSnapshotStore interface {
	loadAuthorization(context.Context) (authorizationSnapshot, error)
	storeAuthorizationIfCurrent(context.Context, authorizationSnapshot, string, string) error
	denyAuthorizationIfCurrent(context.Context, authorizationSnapshot, string) error
}

type refreshReservationStore interface {
	reservePendingRefresh(context.Context, authorizationSnapshot, []byte) ([]byte, error)
}

var errAuthorizationStoreChanged = errors.New("productauth: secure authorization changed")

// A caller timing out during slow secure I/O is not a revocation observation.
// Require the operation's actual error chain to identify that cancellation;
// a concurrent cancellation must never hide an independent verification error.
func authorizationCallerCancellation(ctx context.Context, operationErr error) error {
	if ctx != nil && operationErr != nil {
		if err := ctx.Err(); err != nil && errors.Is(operationErr, err) {
			return err
		}
	}
	return nil
}

func (value authorizationSnapshot) digest() [sha256.Size]byte {
	return sha256.Sum256([]byte(value.refresh + "\x00" + value.lease))
}

func (service *Service) loadAuthorization(ctx context.Context) (authorizationSnapshot, error) {
	if store, ok := service.store.(authorizationSnapshotStore); ok {
		value, err := store.loadAuthorization(ctx)
		if err != nil {
			return authorizationSnapshot{}, authError("keychain_unavailable", "secure authorization state is unavailable", http.StatusServiceUnavailable, false, err)
		}
		return value, nil
	}
	var value authorizationSnapshot
	var err error
	value.denial, err = service.store.LoadDenial(ctx)
	if err != nil && !errors.Is(err, ErrRefreshNotFound) {
		return authorizationSnapshot{}, authError("keychain_unavailable", "secure authorization state is unavailable", http.StatusServiceUnavailable, false, err)
	}
	value.refresh, err = service.store.LoadRefreshToken(ctx)
	if err != nil && !errors.Is(err, ErrRefreshNotFound) {
		return authorizationSnapshot{}, authError("keychain_unavailable", "refresh credential is unavailable", http.StatusServiceUnavailable, false, err)
	}
	value.lease, err = service.store.LoadLease(ctx)
	if err != nil && !errors.Is(err, ErrLeaseNotFound) {
		return authorizationSnapshot{}, authError("keychain_unavailable", "signed lease is unavailable", http.StatusServiceUnavailable, false, err)
	}
	return value, nil
}

func (service *Service) storeAuthorization(ctx context.Context, expected *authorizationSnapshot, refresh, lease string) error {
	if expected == nil {
		return service.store.StoreAuthorization(ctx, refresh, lease)
	}
	if store, ok := service.store.(authorizationSnapshotStore); ok {
		return store.storeAuthorizationIfCurrent(ctx, *expected, refresh, lease)
	}
	actual, err := service.loadAuthorization(ctx)
	if err != nil {
		return err
	}
	if actual != *expected {
		return errAuthorizationStoreChanged
	}
	return service.store.StoreAuthorization(ctx, refresh, lease)
}

// clearCachedAccessLocked requires stateMu. It does not revoke, overwrite or
// otherwise mutate a grant accepted by another native process.
func (service *Service) clearCachedAccessLocked() {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.access.token != "" {
		service.authorizationEpoch.Add(1)
		service.runtimeGeneration.Add(1)
	}
	service.access = accessSession{}
	service.telemetryLease = verifiedLease{}
	service.modelAccessCache = modelAccessCache{}
	service.modelAccessIdentity = modelAccessIdentity{}
	service.modelAccessEpoch++
}

func (service *Service) reconcileCachedAccessLocked(stored authorizationSnapshot) {
	service.mu.Lock()
	changed := service.access.token != "" && service.access.stateDigest != stored.digest()
	service.mu.Unlock()
	if changed {
		service.clearCachedAccessLocked()
	}
}

func (service *Service) cachedAccess(ctx context.Context) (accessSession, bool, error) {
	if ctx == nil {
		return accessSession{}, false, authError("auth_unavailable", "authorization context is unavailable", http.StatusServiceUnavailable, false, nil)
	}
	if err := ctx.Err(); err != nil {
		return accessSession{}, false, err
	}
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	// Do not turn a queued caller's cancellation into a secure-store failure
	// that clears another caller's still-valid authorization session.
	if err := ctx.Err(); err != nil {
		return accessSession{}, false, err
	}
	stored, err := service.loadAuthorization(ctx)
	if err != nil {
		if canceled := authorizationCallerCancellation(ctx, err); canceled != nil {
			return accessSession{}, false, canceled
		}
		service.clearCachedAccessLocked()
		return accessSession{}, false, err
	}
	service.mu.Lock()
	denial := service.localDenial
	service.mu.Unlock()
	if denial != "" || stored.denial != "" || stored.refresh == "" || stored.lease == "" {
		service.clearCachedAccessLocked()
		return accessSession{}, false, authError("login_required", "login is required", http.StatusUnauthorized, false, nil)
	}
	// A changed credential snapshot is already known, even if a later device
	// read times out. Never retain its predecessor's prepared admission window.
	service.reconcileCachedAccessLocked(stored)
	if _, err := service.currentBinding(ctx); err != nil {
		if canceled := authorizationCallerCancellation(ctx, err); canceled != nil {
			return accessSession{}, false, canceled
		}
		service.clearCachedAccessLocked()
		return accessSession{}, false, err
	}
	service.reconcileCachedAccessLocked(stored)
	service.mu.Lock()
	access := service.access
	service.mu.Unlock()
	now := service.now().UTC()
	if err := ctx.Err(); err != nil {
		return accessSession{}, false, err
	}
	return access, access.token != "" && access.epoch == service.authorizationEpoch.Load() &&
		access.expiresAt.After(now) && access.lease.ExpiresAt.After(now), nil
}
