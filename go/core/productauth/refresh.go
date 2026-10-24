package productauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/licensing"
)

const (
	pendingRefreshKey       = "product-auth-refresh-pending-v1"
	pendingRefreshSchema    = "meadow.product-auth.refresh-pending.v1"
	refreshOperationTimeout = 30 * time.Second
	refreshCommitTimeout    = 5 * time.Second
)

type refreshFlight struct {
	done   chan struct{}
	status Status
	err    error
	epoch  uint64
}

type pendingRefresh struct {
	Schema        string `json:"schema"`
	RequestID     string `json:"request_id"`
	RefreshDigest string `json:"refresh_digest"`
	CreatedAt     int64  `json:"created_at"`
}

// A caller may stop waiting, but it must not cancel a credential rotation after
// the server can have committed it. All concurrent callers share one bounded
// operation; ambiguous outcomes retain a durable request ID for exact replay.
func (service *Service) refreshForCaller(ctx context.Context, onlyIfNeeded bool) (Status, error) {
	if ctx == nil {
		return Status{}, authError("auth_unavailable", "authorization context is unavailable", http.StatusServiceUnavailable, false, nil)
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	if onlyIfNeeded {
		access, ready, err := service.cachedAccess(ctx)
		if err != nil {
			return Status{}, err
		}
		if ready {
			return service.statusFromLease(access.lease, access), nil
		}
	}
	return service.refreshForExpectedEpoch(ctx, nil)
}

// A runtime continuity refresh must dispatch against the exact predecessor
// epoch it verified. Normal callers do not supply this extra restriction.
func (service *Service) refreshForExpectedEpoch(ctx context.Context, expected *uint64) (Status, error) {
	if ctx == nil {
		return Status{}, authError("auth_unavailable", "authorization context is unavailable", http.StatusServiceUnavailable, false, nil)
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	service.refreshMu.Lock()
	epoch := service.authorizationEpoch.Load()
	if expected != nil && epoch != *expected {
		service.refreshMu.Unlock()
		return Status{}, authorizationChanged()
	}
	flight := service.refreshFlight
	if flight != nil && expected != nil && flight.epoch != *expected {
		service.refreshMu.Unlock()
		return Status{}, authorizationChanged()
	}
	if flight == nil {
		flight = &refreshFlight{done: make(chan struct{}), epoch: epoch}
		service.refreshFlight = flight
		go service.runRefresh(flight, epoch)
	}
	service.refreshMu.Unlock()
	select {
	case <-ctx.Done():
		return Status{}, ctx.Err()
	case <-flight.done:
		return flight.status, flight.err
	}
}

func (service *Service) runRefresh(flight *refreshFlight, epoch uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), refreshOperationTimeout)
	status, err := service.performRefresh(ctx, epoch)
	cancel()
	service.refreshMu.Lock()
	flight.status, flight.err = status, err
	if service.refreshFlight == flight {
		service.refreshFlight = nil
	}
	close(flight.done)
	service.refreshMu.Unlock()
}

func (service *Service) performRefresh(ctx context.Context, epoch uint64) (Status, error) {
	service.stateMu.Lock()
	if epoch != service.authorizationEpoch.Load() {
		service.stateMu.Unlock()
		return Status{}, authorizationChanged()
	}
	service.mu.Lock()
	denial := service.localDenial
	originalSubject := service.access.lease.Subject
	if originalSubject == "" {
		originalSubject = service.telemetryLease.Subject
	}
	service.mu.Unlock()
	if denial == "" {
		var err error
		denial, err = service.store.LoadDenial(ctx)
		if err != nil && !errors.Is(err, ErrRefreshNotFound) {
			service.stateMu.Unlock()
			return Status{}, authError("keychain_unavailable", "secure authorization state is unavailable", http.StatusServiceUnavailable, false, err)
		}
	}
	if denial != "" {
		service.stateMu.Unlock()
		return Status{}, authError("login_required", "login is required", http.StatusUnauthorized, false, nil)
	}
	original, err := service.loadAuthorization(ctx)
	if err != nil {
		service.stateMu.Unlock()
		return Status{}, err
	}
	if original.denial != "" || original.refresh == "" || original.lease == "" {
		service.stateMu.Unlock()
		return Status{}, authError("login_required", "login is required", http.StatusUnauthorized, false, nil)
	}
	refreshToken := original.refresh
	requestID, err := service.pendingRefreshRequestID(ctx, original)
	service.stateMu.Unlock()
	if err != nil {
		return Status{}, err
	}
	if _, err := service.currentBinding(ctx); err != nil {
		return Status{}, err
	}
	privateKey, _, err := service.deviceProofKey(ctx)
	if err != nil {
		return Status{}, err
	}
	defer clear(privateKey)
	grant, remoteErr := service.client.Refresh(ctx, RemoteRefreshRequest{
		RefreshToken: refreshToken, InstallationID: service.installationID, RequestID: requestID,
		DeviceProof: signDeviceProof(privateKey, "refresh", refreshToken, service.installationID, requestID, ""),
	})
	// A response received at the network deadline still deserves one bounded
	// secure commit attempt. Never use the already-canceled export/request ctx.
	commitCtx, cancel := context.WithTimeout(context.Background(), refreshCommitTimeout)
	defer cancel()
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	current, loadErr := service.loadAuthorization(commitCtx)
	if loadErr != nil {
		service.clearCachedAccessLocked()
		return Status{}, loadErr
	}
	if current != original {
		service.reconcileCachedAccessLocked(current)
		// Entitlement responses can replace a signed lease without rotating its
		// refresh credential. A completed remote rotation may still have
		// superseded that same-account bearer; retain its replay journal and
		// force recovery, even when the new local cache matches the new lease.
		service.mu.Lock()
		unsafeSameAccount := current.denial == "" && current.refresh == refreshToken &&
			originalSubject != "" && service.localDenial == "" && service.access.lease.Subject == originalSubject
		service.mu.Unlock()
		if unsafeSameAccount {
			service.clearCachedAccessLocked()
		}
		return Status{}, authorizationChanged()
	}
	if epoch != service.authorizationEpoch.Load() {
		// A same-account entitlement update may have accepted another signed
		// lease while retaining the pre-rotation refresh credential. Preserve
		// its durable replay ID and force recovery instead of reusing an access
		// token the completed server rotation may already have superseded.
		if current, err := service.store.LoadRefreshToken(commitCtx); err == nil && current == refreshToken {
			service.mu.Lock()
			if originalSubject != "" && service.localDenial == "" && service.access.lease.Subject == originalSubject {
				service.access = accessSession{}
			}
			service.mu.Unlock()
		}
		return Status{}, authorizationChanged()
	}
	if remoteErr != nil {
		// A transport failure can arrive after the issuer has rotated this
		// credential. Keeping the predecessor bearer usable would defer exact-ID
		// recovery until its expiry, beyond the issuer's short replay window.
		service.clearCachedAccessLocked()
		if shouldInvalidateRefresh(remoteErr) {
			if store, ok := service.store.(authorizationSnapshotStore); ok {
				if err := store.denyAuthorizationIfCurrent(commitCtx, original, ErrorCode(remoteErr)); err != nil {
					service.clearCachedAccessLocked()
					if errors.Is(err, errAuthorizationStoreChanged) {
						return Status{}, authorizationChanged()
					}
					return Status{}, authError("keychain_unavailable", "authorization denial could not be stored atomically", http.StatusServiceUnavailable, false, err)
				}
				service.clearCachedAccessLocked()
				service.mu.Lock()
				service.localDenial = ErrorCode(remoteErr)
				service.pending = make(map[string]pendingAuthorization)
				service.mu.Unlock()
			} else {
				service.invalidateAuthorization(commitCtx, ErrorCode(remoteErr))
			}
		}
		return Status{}, remoteErr
	}
	// StoreAuthorization clears the pending request in the same atomic write as
	// the changed successor refresh token and signed lease. Entitlement-only
	// grants retaining the old token preserve this recovery record.
	status, err := service.acceptGrantForStateLocked(commitCtx, grant, &original)
	if err == nil {
		return status, nil
	}
	service.clearCachedAccessLocked()
	if ErrorCode(err) != "keychain_unavailable" {
		return Status{}, err
	}
	// Retry a transient secure commit using the already received signed grant.
	// This does not rotate remotely again or replace the durable request ID.
	// An ambiguous write may already have stored this exact successor; any
	// different authorization must still win over the late response.
	retryCtx, retryCancel := context.WithTimeout(context.Background(), refreshCommitTimeout)
	defer retryCancel()
	current, loadErr = service.loadAuthorization(retryCtx)
	if loadErr != nil {
		return Status{}, loadErr
	}
	successor := authorizationSnapshot{refresh: grant.RefreshToken, lease: grant.Lease}
	if current != original && current != successor {
		service.reconcileCachedAccessLocked(current)
		return Status{}, authorizationChanged()
	}
	status, err = service.acceptGrantForStateLocked(retryCtx, grant, &current)
	if err != nil {
		service.clearCachedAccessLocked()
	}
	return status, err
}

// pendingRefreshRequestID requires stateMu. A stored request is never replaced
// for the same token after a timeout: the server might already have rotated it.
func (service *Service) pendingRefreshRequestID(ctx context.Context, original authorizationSnapshot) (string, error) {
	digest := sha256.Sum256([]byte(original.refresh))
	digestHex := hex.EncodeToString(digest[:])
	payload, err := service.store.LoadSecureValue(ctx, pendingRefreshKey)
	if err == nil {
		return service.checkedPendingRefresh(payload, digestHex)
	}
	if !errors.Is(err, licensing.ErrSecureStoreNotFound) {
		return "", authError("keychain_unavailable", "pending refresh state is unavailable", http.StatusServiceUnavailable, false, err)
	}
	requestID, err := service.randomRequestID()
	if err != nil {
		return "", authError("auth_unavailable", "could not create refresh request", http.StatusServiceUnavailable, false, err)
	}
	payload, err = json.Marshal(pendingRefresh{Schema: pendingRefreshSchema, RequestID: requestID, RefreshDigest: digestHex, CreatedAt: service.now().Unix()})
	if err != nil {
		return "", err
	}
	if store, ok := service.store.(refreshReservationStore); ok {
		reserved, err := store.reservePendingRefresh(ctx, original, payload)
		if errors.Is(err, errAuthorizationStoreChanged) {
			return "", authorizationChanged()
		}
		if err != nil {
			return "", authError("keychain_unavailable", "pending refresh request could not be stored atomically", http.StatusServiceUnavailable, false, err)
		}
		return service.checkedPendingRefresh(reserved, digestHex)
	}
	if err := service.store.StoreSecureValues(ctx, map[string][]byte{pendingRefreshKey: payload}); err != nil {
		return "", authError("keychain_unavailable", "pending refresh request could not be stored", http.StatusServiceUnavailable, false, err)
	}
	return requestID, nil
}

func (service *Service) checkedPendingRefresh(payload []byte, digestHex string) (string, error) {
	var pending pendingRefresh
	if len(payload) > 1024 || strictJSON(payload, &pending) != nil || pending.Schema != pendingRefreshSchema || pending.RefreshDigest != digestHex || pending.CreatedAt <= 0 || pending.CreatedAt > service.now().Add(service.futureClockSkew).Unix() || len(pending.RequestID) != 36 || !strings.HasPrefix(pending.RequestID, "req_") {
		return "", authError("refresh_recovery_unavailable", "pending refresh state is invalid; sign in again", http.StatusServiceUnavailable, false, nil)
	}
	if _, err := decodeCanonicalBase64URL(strings.TrimPrefix(pending.RequestID, "req_"), transactionBytes, transactionBytes); err != nil {
		return "", authError("refresh_recovery_unavailable", "pending refresh request is invalid; sign in again", http.StatusServiceUnavailable, false, nil)
	}
	return pending.RequestID, nil
}
