package productauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/licensing"
)

const (
	pkceVerifierBytes   = 32
	transactionBytes    = 24
	maximumOfflineGrace = 7 * 24 * time.Hour
)

type pendingAuthorization struct {
	remoteTransactionID string
	codeVerifier        string
	expiresAt           time.Time
	retryAfter          time.Duration
	requestID           string
}

type accessSession struct {
	token       string
	lease       verifiedLease
	epoch       uint64
	expiresAt   time.Time
	stateDigest [sha256.Size]byte
}

type Service struct {
	client                 AuthClient
	store                  SecureStateStore
	deviceBinding          DeviceBindingProvider
	now                    func() time.Time
	random                 func([]byte) error
	leaseKeys              map[string]ed25519.PublicKey
	leaseVersion           uint64
	product                string
	futureClockSkew        time.Duration
	clockRollbackTolerance time.Duration
	maxOfflineGrace        time.Duration
	proModels              map[string]struct{}
	installationID         string
	platform               string
	clientName             string

	// stateMu serializes local authorization reads/commits, never remote calls.
	stateMu             sync.Mutex
	refreshMu           sync.Mutex
	refreshFlight       *refreshFlight
	authorizationEpoch  atomic.Uint64 // commits hold stateMu; flight admission reads without blocking caller cancellation
	runtimeGeneration   atomic.Uint64 // new login or authority loss; ordinary verified refresh retains continuity
	mu                  sync.Mutex
	access              accessSession
	telemetryLease      verifiedLease
	pending             map[string]pendingAuthorization
	localDenial         string
	modelAccessMu       sync.Mutex
	modelAccessCache    modelAccessCache
	modelAccessIdentity modelAccessIdentity
	modelAccessEpoch    uint64
}

func NewService(config BuildConfig, dependencies Dependencies) (*Service, error) {
	if config.Product != LeaseProduct || config.LeaseVersion != LeaseVersionV1 ||
		!validInstallationID(config.InstallationID) || !validClientLabel(config.Platform) ||
		!validClientLabel(config.ClientName) || len(config.LeasePublicKeys) == 0 {
		return nil, errors.New("productauth: fixed product lease configuration is invalid")
	}
	if dependencies.Client == nil || dependencies.Store == nil || dependencies.DeviceBinding == nil {
		return nil, errors.New("productauth: client, secure store and device binding are required")
	}
	if dependencies.Client.Issuer() == "" || dependencies.Client.Audience() != LeaseAudience ||
		!sameLeaseKeys(config.LeasePublicKeys, dependencies.Client.LeasePublicKeys()) {
		return nil, errors.New("productauth: fixed issuer, audience and lease keyrings do not match")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	randomSource := dependencies.Random
	if randomSource == nil {
		randomSource = func(buffer []byte) error {
			_, err := rand.Read(buffer)
			return err
		}
	}
	futureClockSkew := config.FutureClockSkew
	if futureClockSkew == 0 {
		futureClockSkew = 2 * time.Minute
	}
	rollbackTolerance := config.ClockRollbackTolerance
	if rollbackTolerance == 0 {
		rollbackTolerance = 2 * time.Minute
	}
	if futureClockSkew < 0 || futureClockSkew > 10*time.Minute || rollbackTolerance < 0 || rollbackTolerance > time.Hour {
		return nil, errors.New("productauth: lease timing policy is invalid")
	}
	if config.MaxOfflineGrace < 0 || config.MaxOfflineGrace > maximumOfflineGrace {
		return nil, errors.New("productauth: offline grace policy is invalid")
	}
	leaseKeys := make(map[string]ed25519.PublicKey, len(config.LeasePublicKeys))
	for keyID, key := range config.LeasePublicKeys {
		if !validOpaque(keyID, 128) || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("productauth: lease keyring is invalid")
		}
		leaseKeys[keyID] = append(ed25519.PublicKey(nil), key...)
	}
	proModels := make(map[string]struct{}, len(config.ProModelIDs))
	for _, modelID := range config.ProModelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			return nil, errors.New("productauth: pro model id is empty")
		}
		proModels[modelID] = struct{}{}
	}
	return &Service{
		client: dependencies.Client, store: dependencies.Store, deviceBinding: dependencies.DeviceBinding,
		now: now, random: randomSource, leaseKeys: leaseKeys, leaseVersion: config.LeaseVersion,
		product: config.Product, futureClockSkew: futureClockSkew, clockRollbackTolerance: rollbackTolerance,
		maxOfflineGrace: config.MaxOfflineGrace,
		proModels:       proModels, installationID: config.InstallationID, platform: config.Platform,
		clientName: config.ClientName, pending: make(map[string]pendingAuthorization),
	}, nil
}

func (service *Service) Status(ctx context.Context) (Status, error) {
	if ctx == nil {
		return Status{}, authError("auth_unavailable", "authorization service is unavailable", http.StatusServiceUnavailable, false, nil)
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	// A caller cancelled while waiting for another authorization operation is
	// not evidence that the verified account changed or was revoked.
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	service.mu.Lock()
	localDenial := service.localDenial
	service.mu.Unlock()
	if localDenial != "" {
		service.clearCachedAccessLocked()
		return service.unauthorizedStatus(StateLoginRequired, localDenial), nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		attemptCtx := ctx
		if store, ok := service.store.(statusSnapshotStore); ok {
			var err error
			attemptCtx, err = store.beginStatusSnapshot(ctx)
			if err != nil {
				if canceled := authorizationCallerCancellation(ctx, err); canceled != nil {
					return Status{}, canceled
				}
				service.clearCachedAccessLocked()
				return Status{}, authError("keychain_unavailable", "secure authorization state is unavailable", http.StatusServiceUnavailable, false, err)
			}
		}
		status, err := service.statusLocked(attemptCtx)
		if store, ok := service.store.(statusSnapshotStore); ok {
			store.endStatusSnapshot(attemptCtx)
		}
		if errors.Is(err, errAuthorizationStoreChanged) {
			continue
		}
		return status, err
	}
	service.clearCachedAccessLocked()
	return Status{}, authError("keychain_unavailable", "secure authorization state changed concurrently", http.StatusServiceUnavailable, false, errAuthorizationStoreChanged)
}

func (service *Service) statusLocked(ctx context.Context) (Status, error) {
	service.mu.Lock()
	localDenial := service.localDenial
	service.mu.Unlock()
	if localDenial != "" {
		service.clearCachedAccessLocked()
		return service.unauthorizedStatus(StateLoginRequired, localDenial), nil
	}
	stored, err := service.loadAuthorization(ctx)
	if err != nil {
		if canceled := authorizationCallerCancellation(ctx, err); canceled != nil {
			return Status{}, canceled
		}
		service.clearCachedAccessLocked()
		return Status{}, err
	}
	if stored.denial != "" {
		service.clearCachedAccessLocked()
		return service.unauthorizedStatus(StateLoginRequired, stored.denial), nil
	}
	if stored.refresh == "" && stored.lease == "" {
		service.clearCachedAccessLocked()
		return service.unauthorizedStatus(StateGuest, ""), nil
	}
	if stored.refresh == "" || stored.lease == "" {
		service.clearCachedAccessLocked()
		return service.unauthorizedStatus(StateLoginRequired, "authorization_incomplete"), nil
	}
	// Reconcile an observed credential replacement before subsequent slow I/O.
	// Cancellation may preserve an unchanged prior proof, never a superseded one.
	service.reconcileCachedAccessLocked(stored)
	binding, err := service.currentBinding(ctx)
	if err != nil {
		if canceled := authorizationCallerCancellation(ctx, err); canceled != nil {
			return Status{}, canceled
		}
		service.clearCachedAccessLocked()
		return Status{}, err
	}
	_, publicKey, err := service.deviceProofKey(ctx)
	if err != nil {
		if canceled := authorizationCallerCancellation(ctx, err); canceled != nil {
			return Status{}, canceled
		}
		service.clearCachedAccessLocked()
		return Status{}, err
	}
	verified, err := service.verifySignedLease(ctx, stored.lease, binding, publicKey)
	if err != nil {
		if errors.Is(err, errAuthorizationStoreChanged) {
			return Status{}, err
		}
		if canceled := authorizationCallerCancellation(ctx, err); canceled != nil {
			return Status{}, canceled
		}
		service.clearCachedAccessLocked()
		return service.unauthorizedStatus(StateLoginRequired, ErrorCode(err)), nil
	}
	if store, ok := service.store.(statusSnapshotStore); ok {
		if err := store.finishStatusSnapshot(ctx); err != nil {
			if errors.Is(err, errAuthorizationStoreChanged) {
				return Status{}, err
			}
			if canceled := authorizationCallerCancellation(ctx, err); canceled != nil {
				return Status{}, canceled
			}
			service.clearCachedAccessLocked()
			return Status{}, authError("keychain_unavailable", "secure authorization state changed during verification", http.StatusServiceUnavailable, false, err)
		}
	}
	service.reconcileCachedAccessLocked(stored)
	service.mu.Lock()
	access := service.access
	service.telemetryLease = verified
	service.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	return service.statusFromLease(verified, access), nil
}

func (service *Service) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if request.Schema != SchemaV1 {
		return StartResult{}, authError("unsupported_schema", "authentication schema is unsupported", http.StatusBadRequest, false, nil)
	}
	if request.Mode != LoginModeBrowser && request.Mode != LoginModeDeviceCode {
		return StartResult{}, authError("invalid_login_mode", "login mode is invalid", http.StatusBadRequest, false, nil)
	}
	if _, err := service.currentBinding(ctx); err != nil {
		return StartResult{}, err
	}
	_, publicKey, err := service.deviceProofKey(ctx)
	if err != nil {
		return StartResult{}, err
	}
	verifier, err := service.randomToken(pkceVerifierBytes)
	if err != nil {
		return StartResult{}, authError("auth_unavailable", "could not create login transaction", http.StatusServiceUnavailable, false, err)
	}
	digest := sha256.Sum256([]byte(verifier))
	requestID, err := service.randomRequestID()
	if err != nil {
		return StartResult{}, authError("auth_unavailable", "could not create login request", http.StatusServiceUnavailable, false, err)
	}
	service.stateMu.Lock()
	epoch := service.authorizationEpoch.Load()
	service.stateMu.Unlock()
	remote, err := service.client.Start(ctx, DeviceAuthorizationStart{
		CodeChallenge: base64.RawURLEncoding.EncodeToString(digest[:]), DevicePublicKey: publicKey,
		InstallationID: service.installationID, Platform: service.platform,
		ClientName: service.clientName, RequestID: requestID,
	})
	if err != nil {
		return StartResult{}, err
	}
	transactionID, err := service.randomToken(transactionBytes)
	if err != nil {
		return StartResult{}, authError("auth_unavailable", "could not create local login transaction", http.StatusServiceUnavailable, false, err)
	}
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	if epoch != service.authorizationEpoch.Load() {
		return StartResult{}, authorizationChanged()
	}
	service.mu.Lock()
	service.pending[transactionID] = pendingAuthorization{
		remoteTransactionID: remote.TransactionID, codeVerifier: verifier,
		expiresAt: remote.ExpiresAt, retryAfter: remote.RetryAfter, requestID: requestID,
	}
	service.mu.Unlock()
	return StartResult{
		Schema: SchemaV1, State: StateLoginPending, TransactionID: transactionID,
		VerificationURI: remote.VerificationURI, VerificationURIComplete: remote.VerificationURIComplete,
		UserCode: remote.UserCode, ExpiresIn: durationSeconds(remote.ExpiresAt.Sub(service.now().UTC())),
		Interval: durationSeconds(remote.RetryAfter),
	}, nil
}

func (service *Service) Poll(ctx context.Context, request PollRequest) (PollResult, error) {
	if request.Schema != SchemaV1 || !validOpaque(request.TransactionID, 160) {
		return PollResult{}, authError("invalid_transaction", "login transaction is invalid", http.StatusBadRequest, false, nil)
	}
	service.mu.Lock()
	pending, ok := service.pending[request.TransactionID]
	service.mu.Unlock()
	if !ok {
		return PollResult{}, authError("login_expired", "login transaction is expired", http.StatusGone, false, nil)
	}
	if !service.now().UTC().Before(pending.expiresAt) {
		service.removePending(request.TransactionID)
		return PollResult{Schema: SchemaV1, State: StateLoginExpired, ErrorCode: "expired_token"}, nil
	}
	if _, err := service.currentBinding(ctx); err != nil {
		return PollResult{}, err
	}
	privateKey, _, err := service.deviceProofKey(ctx)
	if err != nil {
		return PollResult{}, err
	}
	proof := signDeviceProof(privateKey, "poll", pending.remoteTransactionID, service.installationID, pending.requestID, "")
	remote, err := service.client.Poll(ctx, DeviceAuthorizationPoll{
		TransactionID: pending.remoteTransactionID, CodeVerifier: pending.codeVerifier,
		DeviceProof: proof, InstallationID: service.installationID, RequestID: pending.requestID,
	})
	if err != nil {
		return PollResult{}, err
	}
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	service.mu.Lock()
	current, exists := service.pending[request.TransactionID]
	service.mu.Unlock()
	if !exists || current.remoteTransactionID != pending.remoteTransactionID || !service.now().UTC().Before(current.expiresAt) {
		return PollResult{}, authError("login_expired", "login transaction is expired", http.StatusGone, false, nil)
	}
	switch remote.State {
	case StateLoginPending, StateSlowDown:
		retry := remote.RetryAfter
		if retry <= 0 {
			retry = pending.retryAfter
		}
		code := "authorization_pending"
		if remote.State == StateSlowDown {
			pending.retryAfter = retry
			service.mu.Lock()
			service.pending[request.TransactionID] = pending
			service.mu.Unlock()
			code = "slow_down"
		}
		return PollResult{Schema: SchemaV1, State: StateLoginPending, Interval: durationSeconds(retry), ErrorCode: code}, nil
	case StateLoginDenied, StateLoginExpired:
		service.removePending(request.TransactionID)
		code := remote.ErrorCode
		if code == "" {
			code = "access_denied"
			if remote.State == StateLoginExpired {
				code = "expired_token"
			}
		}
		return PollResult{Schema: SchemaV1, State: remote.State, ErrorCode: code}, nil
	case StateAuthorizedGeneral, StateAuthorizedPro:
		if remote.Grant == nil {
			return PollResult{}, authError("invalid_server_response", "authentication response is incomplete", http.StatusBadGateway, false, nil)
		}
		status, acceptErr := service.acceptGrantLocked(ctx, *remote.Grant)
		if acceptErr != nil {
			return PollResult{}, acceptErr
		}
		service.mu.Lock()
		service.pending = make(map[string]pendingAuthorization)
		service.mu.Unlock()
		return PollResult{Schema: SchemaV1, State: status.State, Status: &status}, nil
	default:
		return PollResult{}, authError("invalid_server_response", "authentication response state is invalid", http.StatusBadGateway, false, nil)
	}
}

func (service *Service) Refresh(ctx context.Context, request RefreshRequest) (Status, error) {
	if request.Schema != SchemaV1 {
		return Status{}, authError("unsupported_schema", "authentication schema is unsupported", http.StatusBadRequest, false, nil)
	}
	return service.refreshForCaller(ctx, false)
}

func (service *Service) Logout(ctx context.Context, request LogoutRequest) error {
	if request.Schema != SchemaV1 {
		return authError("unsupported_schema", "authentication schema is unsupported", http.StatusBadRequest, false, nil)
	}
	service.stateMu.Lock()
	stateLocked := true
	defer func() {
		if stateLocked {
			service.stateMu.Unlock()
		}
	}()
	service.authorizationEpoch.Add(1)
	service.runtimeGeneration.Add(1)
	epoch := service.authorizationEpoch.Load()
	service.mu.Lock()
	service.access = accessSession{}
	service.telemetryLease = verifiedLease{}
	service.modelAccessCache = modelAccessCache{}
	service.modelAccessIdentity = modelAccessIdentity{}
	service.modelAccessEpoch++
	service.pending = make(map[string]pendingAuthorization)
	service.localDenial = "logout_pending"
	service.mu.Unlock()
	if err := service.store.StoreDenial(ctx, "logout_pending"); err != nil {
		return authError("keychain_unavailable", "logout denial could not be stored", http.StatusServiceUnavailable, false, err)
	}
	refreshToken, loadErr := service.store.LoadRefreshToken(ctx)
	if errors.Is(loadErr, ErrRefreshNotFound) {
		if err := service.store.ClearAuthorization(ctx); err != nil {
			return authError("keychain_unavailable", "local authorization could not be cleared", http.StatusServiceUnavailable, false, err)
		}
		service.setDenial(ctx, "logged_out")
		return nil
	}
	if loadErr != nil {
		return authError("keychain_unavailable", "secure authorization state is unavailable", http.StatusServiceUnavailable, false, loadErr)
	}
	privateKey, _, err := service.deviceProofKey(ctx)
	if err != nil {
		return err
	}
	requestID, err := service.randomRequestID()
	if err != nil {
		return authError("auth_unavailable", "could not create logout request", http.StatusServiceUnavailable, false, err)
	}
	service.stateMu.Unlock()
	stateLocked = false
	if err := service.client.Logout(ctx, RemoteLogoutRequest{
		RefreshToken: refreshToken, InstallationID: service.installationID, RequestID: requestID,
		DeviceProof: signDeviceProof(privateKey, "logout", refreshToken, service.installationID, requestID, ""),
	}); err != nil {
		return authError("logout_remote_failed", "remote session revocation is pending and can be retried", http.StatusBadGateway, true, err)
	}
	service.stateMu.Lock()
	stateLocked = true
	if epoch != service.authorizationEpoch.Load() {
		return nil
	}
	if err := service.store.ClearAuthorization(ctx); err != nil {
		return authError("keychain_unavailable", "remote logout completed but local authorization could not be cleared", http.StatusServiceUnavailable, false, err)
	}
	service.setDenial(ctx, "logged_out")
	return nil
}

func (service *Service) Redeem(ctx context.Context, request RedeemRequest) (RedeemResult, error) {
	if request.Schema != SchemaV1 || !validSerial(request.Serial) || !validOpaque(request.RequestID, 160) ||
		!validIdempotencyKey(request.IdempotencyKey) {
		return RedeemResult{}, authError("invalid_serial", "license redemption request is invalid", http.StatusBadRequest, false, nil)
	}
	access, err := service.currentAccess(ctx)
	if err != nil {
		return RedeemResult{}, err
	}
	privateKey, _, err := service.deviceProofKey(ctx)
	if err != nil {
		return RedeemResult{}, err
	}
	remote, err := service.client.Redeem(ctx, RemoteRedeemRequest{
		AccessToken: access.token, Serial: strings.TrimSpace(request.Serial), RequestID: request.RequestID,
		IdempotencyKey: request.IdempotencyKey, InstallationID: service.installationID,
		DeviceProof: signDeviceProof(privateKey, "serial", access.token, service.installationID, request.RequestID, request.IdempotencyKey),
	})
	if err != nil {
		return RedeemResult{}, err
	}
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	if access.epoch != service.authorizationEpoch.Load() {
		return RedeemResult{}, authorizationChanged()
	}
	stored, loadErr := service.loadAuthorization(ctx)
	if loadErr != nil {
		return RedeemResult{}, authError("keychain_unavailable", "secure authorization state is unavailable", http.StatusServiceUnavailable, false, loadErr)
	}
	if stored.denial != "" || stored.digest() != access.stateDigest {
		service.reconcileCachedAccessLocked(stored)
		return RedeemResult{}, authorizationChanged()
	}
	status, err := service.acceptGrantForStateLocked(ctx, RemoteGrant{
		State: remote.State, AccessToken: access.token, RefreshToken: stored.refresh,
		AccessExpiresAt: access.expiresAt,
		Lease:           remote.Lease, KeyID: remote.KeyID, Plan: remote.Plan, Capabilities: remote.Capabilities,
	}, &stored)
	if err != nil {
		return RedeemResult{}, err
	}
	if remote.Receipt.RequestID != request.RequestID || remote.Receipt.Status != "accepted" ||
		!validOpaque(remote.Receipt.ReceiptID, 160) {
		return RedeemResult{}, authError("invalid_server_response", "license receipt is invalid", http.StatusBadGateway, false, nil)
	}
	return RedeemResult{Schema: SchemaV1, Availability: status, Receipt: remote.Receipt}, nil
}

func (service *Service) ListDevices(ctx context.Context, request DeviceListRequest) (DeviceListResult, error) {
	if request.Schema != SchemaV1 {
		return DeviceListResult{}, authError("unsupported_schema", "device list schema is unsupported", http.StatusBadRequest, false, nil)
	}
	access, err := service.currentAccess(ctx)
	if err != nil {
		return DeviceListResult{}, err
	}
	privateKey, _, err := service.deviceProofKey(ctx)
	if err != nil {
		return DeviceListResult{}, err
	}
	requestID, err := service.randomRequestID()
	if err != nil {
		return DeviceListResult{}, authError("auth_unavailable", "could not create device list request", http.StatusServiceUnavailable, false, err)
	}
	result, err := service.client.ListDevices(ctx, RemoteDeviceListRequest{
		AccessToken: access.token, RequestID: requestID, InstallationID: service.installationID,
		DeviceProof: signDeviceProof(privateKey, "devices_list", access.token, service.installationID, requestID, ""),
	})
	if err != nil {
		return DeviceListResult{}, err
	}
	if validateDeviceList(result) != nil || result.CurrentDeviceID != access.lease.DeviceID ||
		result.MaximumDevices != access.lease.Capabilities.DevicesMax {
		return DeviceListResult{}, authError("invalid_server_response", "device list disagrees with the signed lease", http.StatusBadGateway, false, nil)
	}
	return result, nil
}

func (service *Service) RevokeDevice(ctx context.Context, request DeviceRevokeRequest) (DeviceRevokeResult, error) {
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Schema != SchemaV1 || !validOpaque(request.DeviceID, 128) || !validOpaque(request.RequestID, 160) ||
		!validIdempotencyKey(request.IdempotencyKey) || !validReason(request.Reason) || !request.Confirmed {
		return DeviceRevokeResult{}, authError("invalid_request", "device revoke request is invalid", http.StatusBadRequest, false, nil)
	}
	access, err := service.currentAccess(ctx)
	if err != nil {
		return DeviceRevokeResult{}, err
	}
	if request.DeviceID == access.lease.DeviceID {
		return DeviceRevokeResult{}, authError("current_device_revoke_requires_logout", "use logout to revoke the current device", http.StatusConflict, false, nil)
	}
	privateKey, _, err := service.deviceProofKey(ctx)
	if err != nil {
		return DeviceRevokeResult{}, err
	}
	binding := request.DeviceID + "\x00" + request.Reason + "\x00true"
	remote, err := service.client.RevokeDevice(ctx, RemoteDeviceRevokeRequest{
		AccessToken: access.token, DeviceID: request.DeviceID, RequestID: request.RequestID,
		IdempotencyKey: request.IdempotencyKey, Reason: request.Reason, Confirmed: true,
		InstallationID: service.installationID,
		DeviceProof:    signBoundDeviceProof(privateKey, "device_revoke", access.token, service.installationID, request.RequestID, request.IdempotencyKey, binding),
	})
	if err != nil {
		return DeviceRevokeResult{}, err
	}
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	if access.epoch != service.authorizationEpoch.Load() {
		return DeviceRevokeResult{}, authorizationChanged()
	}
	stored, err := service.loadAuthorization(ctx)
	if err != nil {
		return DeviceRevokeResult{}, authError("keychain_unavailable", "secure authorization state is unavailable", http.StatusServiceUnavailable, false, err)
	}
	if stored.denial != "" || stored.digest() != access.stateDigest {
		service.reconcileCachedAccessLocked(stored)
		return DeviceRevokeResult{}, authorizationChanged()
	}
	status, err := service.acceptGrantForStateLocked(ctx, RemoteGrant{
		State: remote.State, AccessToken: access.token, RefreshToken: stored.refresh, Lease: remote.Lease,
		AccessExpiresAt: access.expiresAt,
		KeyID:           remote.KeyID, Plan: remote.Plan, Capabilities: remote.Capabilities,
	}, &stored)
	if err != nil {
		return DeviceRevokeResult{}, err
	}
	if remote.Receipt.RequestID != request.RequestID || remote.Receipt.DeviceID != request.DeviceID || remote.Receipt.Status != "revoked" ||
		!validOpaque(remote.Receipt.ReceiptID, 160) || !validOpaque(remote.Receipt.AuditID, 160) {
		return DeviceRevokeResult{}, authError("invalid_server_response", "device revoke receipt is invalid", http.StatusBadGateway, false, nil)
	}
	return DeviceRevokeResult{Schema: SchemaV1, Availability: status, Receipt: remote.Receipt}, nil
}

func (service *Service) Authorize(ctx context.Context, requirement string) error {
	status, err := service.Status(ctx)
	if err != nil {
		return err
	}
	switch requirement {
	case AdmissionGeneral:
		if status.State == StateAuthorizedGeneral || status.State == StateAuthorizedPro {
			return nil
		}
	case AdmissionPro:
		if status.State == StateAuthorizedPro && status.Plan == PlanPro {
			return nil
		}
		if status.State == StateAuthorizedGeneral {
			return authError("upgrade_required", "this operation requires Meadow Pro", http.StatusForbidden, false, nil)
		}
	case CapabilityBrowser:
		if status.Capabilities.Enabled(CapabilityBrowser) {
			return nil
		}
		if status.State == StateAuthorizedGeneral {
			return authError("upgrade_required", "browser interaction requires Meadow Pro", http.StatusForbidden, false, nil)
		}
	default:
		return authError("unknown_capability", "authorization requirement is unsupported", http.StatusForbidden, false, nil)
	}
	if status.Reason != "" && status.Reason != "logged_out" {
		return authError(status.Reason, "authorization was revoked or is unavailable", http.StatusUnauthorized, false, nil)
	}
	return authError("login_required", "login is required", http.StatusUnauthorized, false, nil)
}

func (service *Service) IsProModel(modelID string) bool {
	_, ok := service.proModels[strings.TrimSpace(modelID)]
	return ok
}

func (service *Service) acceptGrant(ctx context.Context, grant RemoteGrant) (Status, error) {
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	return service.acceptGrantLocked(ctx, grant)
}

// acceptGrantLocked commits secure storage and its in-memory projection together.
func (service *Service) acceptGrantLocked(ctx context.Context, grant RemoteGrant) (Status, error) {
	return service.acceptGrantForStateLocked(ctx, grant, nil)
}

func (service *Service) acceptGrantForStateLocked(ctx context.Context, grant RemoteGrant, expected *authorizationSnapshot) (Status, error) {
	if grant.AccessToken == "" || grant.RefreshToken == "" || grant.Lease == "" ||
		(grant.State != StateAuthorizedGeneral && grant.State != StateAuthorizedPro) {
		return Status{}, authError("invalid_server_response", "authentication grant is incomplete", http.StatusBadGateway, false, nil)
	}
	binding, err := service.currentBinding(ctx)
	if err != nil {
		return Status{}, err
	}
	_, publicKey, err := service.deviceProofKey(ctx)
	if err != nil {
		return Status{}, err
	}
	verified, err := service.verifySignedLease(ctx, grant.Lease, binding, publicKey)
	if err != nil {
		return Status{}, err
	}
	expectedState := StateAuthorizedGeneral
	if verified.Plan == PlanPro {
		expectedState = StateAuthorizedPro
	}
	if grant.KeyID != verified.KeyID || grant.State != expectedState || grant.Plan != verified.Plan ||
		grant.Capabilities != verified.Capabilities {
		return Status{}, authError("invalid_server_response", "grant and signed lease disagree", http.StatusBadGateway, false, nil)
	}
	expiresAt := grant.AccessExpiresAt.UTC()
	if expiresAt.IsZero() || !expiresAt.After(service.now().UTC()) || expiresAt.After(service.now().UTC().Add(maximumLeaseLife)) {
		return Status{}, authError("invalid_server_response", "authentication bearer deadline is invalid", http.StatusBadGateway, false, nil)
	}
	if expiresAt.After(verified.ExpiresAt) {
		expiresAt = verified.ExpiresAt
	}
	if !expiresAt.After(service.now().UTC()) {
		return Status{}, authError("invalid_server_response", "authentication bearer deadline is expired", http.StatusBadGateway, false, nil)
	}
	if err := service.storeAuthorization(ctx, expected, grant.RefreshToken, grant.Lease); err != nil {
		if errors.Is(err, errAuthorizationStoreChanged) {
			return Status{}, authorizationChanged()
		}
		return Status{}, authError("keychain_unavailable", "authorization state could not be stored atomically", http.StatusServiceUnavailable, false, err)
	}
	service.mu.Lock()
	if previous := service.access.lease.Subject; previous != "" && previous != verified.Subject {
		service.modelAccessCache = modelAccessCache{}
		service.modelAccessIdentity = modelAccessIdentity{}
		service.modelAccessEpoch++
	}
	service.authorizationEpoch.Add(1)
	if expected == nil {
		service.runtimeGeneration.Add(1)
	}
	service.access = accessSession{token: grant.AccessToken, lease: verified, epoch: service.authorizationEpoch.Load(), expiresAt: expiresAt, stateDigest: authorizationSnapshot{refresh: grant.RefreshToken, lease: grant.Lease}.digest()}
	service.localDenial = ""
	access := service.access
	service.mu.Unlock()
	return service.statusFromLease(verified, access), nil
}

func (service *Service) currentAccess(ctx context.Context) (accessSession, error) {
	access, ready, err := service.cachedAccess(ctx)
	if err != nil {
		return accessSession{}, err
	}
	if ready {
		return access, nil
	}
	if _, err := service.refreshForCaller(ctx, true); err != nil {
		return accessSession{}, err
	}

	access, ready, err = service.cachedAccess(ctx)
	if err != nil {
		return accessSession{}, err
	}
	if !ready {
		return accessSession{}, authorizationChanged()
	}
	return access, nil
}

func (service *Service) statusFromLease(lease verifiedLease, access accessSession) Status {
	state := StateAuthorizedGeneral
	if lease.Plan == PlanPro {
		state = StateAuthorizedPro
	}
	expires := lease.ExpiresAt.UTC()
	effectiveExpires := lease.EffectiveExpiresAt.UTC()
	reason := ""
	if lease.OfflineGrace {
		reason = "offline_grace"
	}
	return Status{
		Schema: SchemaV1, State: state, Plan: lease.Plan, Authenticated: true,
		Capabilities: lease.Capabilities, Subject: lease.Subject, AccessExpiresAt: &expires, Reason: reason,
		Entitlement: &EntitlementStatus{
			LeaseID: lease.LeaseID, DeviceID: lease.DeviceID, EffectiveExpiresAt: effectiveExpires,
			RevocationEpoch: lease.RevocationEpoch,
		},
	}
}

func (service *Service) unauthorizedStatus(state AuthorizationState, reason string) Status {
	return Status{Schema: SchemaV1, State: state, Plan: PlanGuest, Capabilities: GuestCapabilities(), Reason: reason}
}

func (service *Service) currentBinding(ctx context.Context) (licensing.DeviceBinding, error) {
	binding, err := service.deviceBinding.DeviceBinding(ctx)
	if err != nil || binding.Algorithm != licensing.DeviceBindingSHA256 || binding.Digest != service.installationID {
		return licensing.DeviceBinding{}, authError("device_binding_unavailable", "fixed device binding is unavailable", http.StatusServiceUnavailable, false, err)
	}
	return binding, nil
}

// invalidateAuthorization requires stateMu.
func (service *Service) invalidateAuthorization(ctx context.Context, reason string) {
	service.authorizationEpoch.Add(1)
	service.runtimeGeneration.Add(1)
	if !validOpaque(reason, 128) {
		reason = "authorization_revoked"
	}
	service.mu.Lock()
	service.access = accessSession{}
	service.telemetryLease = verifiedLease{}
	service.modelAccessCache = modelAccessCache{}
	service.modelAccessIdentity = modelAccessIdentity{}
	service.modelAccessEpoch++
	service.localDenial = reason
	service.pending = make(map[string]pendingAuthorization)
	service.mu.Unlock()
	_ = service.store.StoreDenial(ctx, reason)
	_ = service.store.ClearAuthorization(ctx)
}

func (service *Service) setDenial(ctx context.Context, reason string) {
	service.mu.Lock()
	service.localDenial = reason
	service.mu.Unlock()
	_ = service.store.StoreDenial(ctx, reason)
}

func (service *Service) randomToken(size int) (string, error) {
	buffer := make([]byte, size)
	if err := service.random(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (service *Service) removePending(transactionID string) {
	service.mu.Lock()
	delete(service.pending, transactionID)
	service.mu.Unlock()
}

func authorizationChanged() error {
	return authError("authorization_changed", "authorization changed during the request", http.StatusConflict, false, nil)
}

func authError(code, message string, status int, retryable bool, cause error) error {
	return &Error{Code: code, Message: message, HTTPStatus: status, Retryable: retryable, cause: cause}
}

func shouldInvalidateRefresh(err error) bool {
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Retryable {
		return false
	}
	return authErr.HTTPStatus == http.StatusUnauthorized || authErr.HTTPStatus == http.StatusForbidden ||
		authErr.Code == "refresh_reuse_detected" || authErr.Code == "device_revoked" ||
		authErr.Code == "activation_revoked" || authErr.Code == "invalid_refresh_token"
}

func durationSeconds(duration time.Duration) int {
	seconds := int(duration / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func validOpaque(value string, max int) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validSerial(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 32 || len(value) > 256 || !strings.HasPrefix(value, "MEADOW-") {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validIdempotencyKey(value string) bool {
	if len(value) < 8 || len(value) > 200 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func validInstallationID(value string) bool {
	return len(value) >= 16 && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\t ")
}

func validClientLabel(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 100 && !strings.ContainsAny(value, "\r\n\t")
}

func sameLeaseKeys(left, right map[string]ed25519.PublicKey) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	for keyID, key := range left {
		if !bytes.Equal(key, right[keyID]) {
			return false
		}
	}
	return true
}
