package agentdaemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/hey-meadow/meadow-code/go/core/productauth"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type promptGrantService interface {
	PreparePromptGrant(context.Context, string) (productauth.VerifiedPromptGrant, error)
	SettlePromptGrant(context.Context, productauth.PromptGrantSettlementRequest) (productauth.PromptGrantSettlementReceipt, error)
	ClosePromptGrant(context.Context, productauth.PromptGrantCloseRequest) (productauth.PromptGrantCloseReceipt, error)
	AdmissionStatus(context.Context, *productauth.AdmissionWindow) (productauth.Status, error)
}
type durablePromptGrant struct {
	GrantID      string    `json:"grant_id"`
	PrincipalKey string    `json:"principal_key"`
	AccountID    string    `json:"account_id"`
	DeviceID     string    `json:"device_id"`
	Capacity     int       `json:"capacity"`
	ExpiresAt    time.Time `json:"expires_at"`
	Sealed       bool      `json:"sealed,omitempty"`
	Closed       bool      `json:"closed,omitempty"`
}
type promptGrantManager struct {
	bridge         *commercialBridge
	service        promptGrantService
	mu             sync.Mutex
	current        *productauth.VerifiedPromptGrant
	flight         chan struct{}
	flightErr      error
	usageBase      map[string]any
	usagePrincipal string
	historyRunning bool
	runtimeService runtimeAccountWindowService
	runtimeWindow  *productauth.RuntimeAccountWindow
	runtimeFlight  *runtimeAccountFlight
	runtimeTimer   *time.Timer
	runtimeSubject string
	runtimeDevice  string
	runtimeRetries int
}

// Explicit private-pilot opt-in. Ordinary clients retain their current policy.
func (bridge *commercialBridge) enablePromptGrants() {
	if os.Getenv("MEADOW_ENABLE_PROMPT_GRANTS") != "1" || !bridge.accountAdmission {
		return
	}
	service, ok := bridge.service.(*productauth.Service)
	if !ok || service == nil {
		return
	}
	bridge.promptGrants = &promptGrantManager{bridge: bridge, service: service, runtimeService: service}
}
func (bridge *commercialBridge) admissionStatus(ctx context.Context) (productauth.Status, error) {
	if manager := bridge.promptGrants; manager != nil {
		manager.mu.Lock()
		grant := manager.current
		manager.mu.Unlock()
		if grant != nil && grant.Window != nil {
			status, err := manager.service.AdmissionStatus(ctx, grant.Window)
			if err == nil {
				return status, nil
			}
			manager.kickPrepare()
		}
	}
	return bridge.service.Status(ctx)
}
func (bridge *commercialBridge) handleAdmissionStatus(writer http.ResponseWriter, request *http.Request) {
	status, err := bridge.runtimeAccountStatus(request.Context())
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, status)
}
func promptGrantError(code string) error {
	return &productauth.Error{Code: code, Message: "prompt allowance is being synchronized", HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
}
func (manager *promptGrantManager) kickPrepare() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = manager.prepare(ctx)
	}()
}
func (manager *promptGrantManager) prepare(ctx context.Context) error {
	if grant := manager.snapshot(); grant != nil && grant.Window != nil && grant.ExpiresAt.After(manager.bridge.now()) {
		if _, err := manager.service.AdmissionStatus(ctx, grant.Window); err == nil {
			projection := manager.projection()
			if projection["ready"] == true {
				return nil
			}
		}
	}
	manager.mu.Lock()
	if flight := manager.flight; flight != nil {
		manager.mu.Unlock()
		select {
		case <-flight:
			manager.mu.Lock()
			err := manager.flightErr
			manager.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	manager.flight = make(chan struct{})
	flight := manager.flight
	manager.mu.Unlock()
	err := manager.prepareOnce(ctx)
	manager.mu.Lock()
	manager.flightErr = err
	manager.flight = nil
	close(flight)
	manager.mu.Unlock()
	return err
}
func (manager *promptGrantManager) prepareOnce(ctx context.Context) error {
	// A restart has no current in-memory grant. Reconcile expired reservations
	// owned by this exact verified login/device before requesting new capacity;
	// otherwise remote reserved capacity can prevent historical cleanup forever.
	if err := manager.reconcileExpiredPrincipal(ctx); err != nil {
		return err
	}
	for round := 0; round < 2; round++ {
		requestID, err := randomCommercialID("grant_prepare")
		if err != nil {
			return err
		}
		grant, err := manager.service.PreparePromptGrant(ctx, requestID)
		if err != nil {
			return err
		}
		principalDigest := sha256.Sum256([]byte(grant.Subject))
		principal := fmt.Sprintf("%x", principalDigest)
		if grant.Capacity < 1 || grant.Capacity > 5 || !validCommercialID(grant.GrantID) || grant.Subject == "" {
			return promptGrantError("invalid_prompt_grant")
		}
		store := manager.bridge.store
		store.mu.Lock()
		persisted := store.grants[grant.GrantID]
		if persisted != nil && (persisted.PrincipalKey != principal || persisted.Capacity != grant.Capacity || !persisted.ExpiresAt.Equal(grant.ExpiresAt)) {
			store.mu.Unlock()
			return promptGrantError("prompt_grant_binding_mismatch")
		}
		if persisted == nil {
			persisted = &durablePromptGrant{GrantID: grant.GrantID, PrincipalKey: principal, Capacity: grant.Capacity, ExpiresAt: grant.ExpiresAt, AccountID: grant.AccountID, DeviceID: grant.DeviceID}
			store.grants[grant.GrantID] = persisted
			if err := store.saveLocked(); err != nil {
				delete(store.grants, grant.GrantID)
				store.mu.Unlock()
				return err
			}
		}
		closed := persisted.Closed
		if grant.ReconciliationRequired || !grant.ExpiresAt.After(manager.bridge.now()) || persisted.Sealed {
			previous := persisted.Sealed
			persisted.Sealed = true
			if err := store.saveLocked(); err != nil {
				persisted.Sealed = previous
				store.mu.Unlock()
				return err
			}
			pending := []commercialBinding{}
			for _, binding := range store.bindings {
				if binding.PromptGrantID != grant.GrantID || binding.Consumed {
					continue
				}
				if binding.DeliveryStarted && !binding.Accepted {
					store.mu.Unlock()
					return promptGrantError("prompt_grant_delivery_unresolved")
				}
				if binding.Accepted {
					pending = append(pending, *binding)
				}
			}
			store.mu.Unlock()
			for _, binding := range pending {
				if err := manager.settle(ctx, binding); err != nil {
					return err
				}
			}
			if !closed {
				id, err := randomCommercialID("grant_close")
				if err != nil {
					return err
				}
				receipt, err := manager.service.ClosePromptGrant(ctx, productauth.PromptGrantCloseRequest{RequestID: id, GrantID: grant.GrantID})
				if err != nil {
					return err
				}
				if !receipt.Closed || receipt.GrantID != grant.GrantID {
					return promptGrantError("invalid_prompt_grant_close")
				}
				store.mu.Lock()
				persisted.Closed = true
				err = store.saveLocked()
				store.mu.Unlock()
				if err != nil {
					return err
				}
			}
			continue
		}
		store.mu.Unlock()
		if closed || grant.Window == nil {
			return promptGrantError("prompt_grant_pending")
		}
		if _, err := manager.service.AdmissionStatus(ctx, grant.Window); err != nil {
			return err
		}
		manager.mu.Lock()
		needsUsage := manager.usageBase == nil || manager.usagePrincipal != principal
		manager.mu.Unlock()
		if needsUsage {
			usage, err := manager.bridge.remoteUsageProjection(ctx)
			if err != nil {
				return err
			}
			if _, err := manager.service.AdmissionStatus(ctx, grant.Window); err != nil {
				return err
			}
			manager.mu.Lock()
			manager.usageBase = usage
			manager.usagePrincipal = principal
			manager.mu.Unlock()
		}
		store.mu.Lock()
		for id, prior := range store.grants {
			if id != grant.GrantID {
				prior.Sealed = true
			}
		}
		sealErr := store.saveLocked()
		store.mu.Unlock()
		if sealErr != nil {
			return sealErr
		}
		if _, err := manager.service.AdmissionStatus(ctx, grant.Window); err != nil {
			return err
		}
		manager.mu.Lock()
		manager.current = &grant
		manager.observeRuntimePrincipalLocked(grant.Subject, grant.DeviceID)
		manager.mu.Unlock()
		manager.bridge.kickConsumptionSync(principal)
		manager.kickHistorical()
		return nil
	}
	return promptGrantError("prompt_grant_pending")
}

func (manager *promptGrantManager) reconcileExpiredPrincipal(ctx context.Context) error {
	store := manager.bridge.store
	store.mu.Lock()
	hasExpired := false
	for _, grant := range store.grants {
		if !grant.Closed && !grant.ExpiresAt.After(manager.bridge.now()) {
			hasExpired = true
			break
		}
	}
	store.mu.Unlock()
	if !hasExpired {
		return nil
	}
	status, err := manager.bridge.service.Status(ctx)
	principal, _, err := manager.bridge.principalFromStatus(status, err)
	if err != nil {
		return err
	}
	if status.Entitlement == nil || status.Entitlement.DeviceID == "" {
		// No recovery ownership proof: leave every old reservation untouched.
		// The ordinary authority-owned preparation still performs its own proof.
		return nil
	}
	store.mu.Lock()
	var expired []durablePromptGrant
	for _, grant := range store.grants {
		if !grant.Closed && !grant.ExpiresAt.After(manager.bridge.now()) && grant.PrincipalKey == principal && grant.DeviceID == status.Entitlement.DeviceID {
			expired = append(expired, *grant)
			if len(expired) >= 16 {
				break
			}
		}
	}
	store.mu.Unlock()
	for _, old := range expired {
		store.mu.Lock()
		grant := store.grants[old.GrantID]
		if grant.Closed {
			store.mu.Unlock()
			continue
		}
		previous := grant.Sealed
		grant.Sealed = true
		if err := store.saveLocked(); err != nil {
			grant.Sealed = previous
			store.mu.Unlock()
			return err
		}
		var pending []commercialBinding
		unresolved := false
		for _, binding := range store.bindings {
			if binding.PromptGrantID != old.GrantID || binding.Consumed {
				continue
			}
			if binding.DeliveryStarted && !binding.Accepted {
				unresolved = true
			}
			if binding.Accepted {
				pending = append(pending, *binding)
			}
		}
		store.mu.Unlock()
		if unresolved {
			continue
		} // Uncertain delivery must never be refunded.
		for _, binding := range pending {
			if err := manager.settle(ctx, binding); err != nil {
				return err
			}
		}
		// Recheck the same signed principal after settlement and before close.
		fresh, e := manager.bridge.service.Status(ctx)
		freshPrincipal, _, e := manager.bridge.principalFromStatus(fresh, e)
		if e != nil || freshPrincipal != principal || fresh.Entitlement == nil || fresh.Entitlement.DeviceID != old.DeviceID {
			return promptGrantError("prompt_grant_principal_changed")
		}
		id, e := randomCommercialID("grant_close")
		if e != nil {
			return e
		}
		receipt, e := manager.service.ClosePromptGrant(ctx, productauth.PromptGrantCloseRequest{RequestID: id, GrantID: old.GrantID})
		if e != nil {
			return e
		}
		if !receipt.Closed || receipt.GrantID != old.GrantID {
			return promptGrantError("invalid_prompt_grant_close")
		}
		store.mu.Lock()
		store.grants[old.GrantID].Closed = true
		e = store.saveLocked()
		store.mu.Unlock()
		if e != nil {
			return e
		}
	}
	return nil
}
func (manager *promptGrantManager) handlePrepare(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Schema string `json:"schema"`
	}
	if decodeJSON(request, &body, true) != nil || body.Schema != "meadow.desktop-prompt-grant.v1" {
		writeCommercialRequestError(writer)
		return
	}
	if err := manager.prepare(request.Context()); err != nil {
		var authErr *productauth.Error
		if errors.As(err, &authErr) && authErr.Code == "resource_exhausted" && authErr.HTTPStatus == http.StatusTooManyRequests {
			observed, observeErr := manager.bridge.remoteUsageProjection(request.Context())
			if observeErr != nil {
				writeCommercialError(writer, observeErr)
				return
			}
			quota := observed["usage"].(productauth.CommercialUsageQuota)
			reason := "free_prompt_limit_reached"
			if quota.Remaining != nil && *quota.Remaining > 0 {
				reason = "prompt_allowance_reserved"
			}
			writeJSON(writer, http.StatusOK, map[string]any{"schema": "meadow.desktop-prompt-grant.v1", "ready": false, "available": 0, "reason": reason, "usage": quota, "pending_sync": 0})
			return
		}
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, manager.projection())
}
func (manager *promptGrantManager) snapshot() *productauth.VerifiedPromptGrant {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current == nil {
		return nil
	}
	copy := *manager.current
	copy.SettledSlots = append([]int(nil), copy.SettledSlots...)
	return &copy
}
func localPromptQuota(grant *productauth.VerifiedPromptGrant, bindings map[string]*commercialBinding, grants map[string]*durablePromptGrant) productauth.CommercialUsageQuota {
	accepted := grant.Usage.AcceptedPromptCount
	settled := map[int]bool{}
	for _, slot := range grant.SettledSlots {
		settled[slot] = true
	}
	for _, binding := range bindings {
		if binding.Released {
			continue
		}
		if binding.PromptGrantID == grant.GrantID && !settled[binding.PromptGrantSlot] {
			accepted++
		} else if binding.PromptGrantID != grant.GrantID && !binding.Consumed {
			old := grants[binding.PromptGrantID]
			if old != nil && old.AccountID != "" && old.AccountID == grant.AccountID && old.DeviceID == grant.DeviceID {
				accepted++
			}
		}
	}
	quota := productauth.CommercialUsageQuota{Unit: "accepted_user_prompt", AcceptedPromptCount: accepted, Unlimited: grant.Usage.Unlimited, CanSubmit: true}
	if !quota.Unlimited {
		limit := 100
		remaining := limit - accepted
		if remaining < 0 {
			remaining = 0
		}
		quota.Limit = &limit
		quota.Remaining = &remaining
		quota.CanSubmit = remaining > 0
	}
	return quota
}
func (manager *promptGrantManager) projection() map[string]any {
	grant := manager.snapshot()
	if grant == nil {
		return map[string]any{"schema": "meadow.desktop-prompt-grant.v1", "ready": false, "available": 0, "pending_sync": 0}
	}
	store := manager.bridge.store
	store.mu.Lock()
	defer store.mu.Unlock()
	quota := localPromptQuota(grant, store.bindings, store.grants)
	used := map[int]bool{}
	for _, slot := range grant.SettledSlots {
		used[slot] = true
	}
	pending := 0
	for _, binding := range store.bindings {
		if binding.PromptGrantID == grant.GrantID && !binding.Released {
			used[binding.PromptGrantSlot] = true
		}
		if binding.Accepted && !binding.Consumed {
			pending++
		}
	}
	available := grant.Capacity - len(used)
	reservation := store.grants[grant.GrantID]
	ready := grant.ExpiresAt.After(manager.bridge.now()) && available > 0 && reservation != nil && !reservation.Sealed && !reservation.Closed
	return map[string]any{"schema": "meadow.desktop-prompt-grant.v1", "ready": ready, "available": available, "expires_at": grant.ExpiresAt.UTC().Format(time.RFC3339), "usage": quota, "pending_sync": pending}
}
func (manager *promptGrantManager) handleAuthorize(writer http.ResponseWriter, request *http.Request, scope string) {
	var body struct {
		Schema         string `json:"schema"`
		RequestID      string `json:"request_id"`
		ConversationID string `json:"conversation_id"`
	}
	if decodeJSON(request, &body, true) != nil || body.Schema != desktopMembershipSchema || !validCommercialID(body.RequestID) || !validCommercialID(body.ConversationID) {
		writeCommercialRequestError(writer)
		return
	}
	grant := manager.snapshot()
	if grant == nil || grant.Window == nil {
		manager.kickPrepare()
		writeCommercialError(writer, promptGrantError("prompt_grant_pending"))
		return
	}
	status, err := manager.service.AdmissionStatus(request.Context(), grant.Window)
	principal, expiry, err := manager.bridge.principalFromStatus(status, err)
	if err != nil {
		// Refreshing the bearer invalidates its old process-local window. Only a
		// fresh, still-authorized identity may wait for a replacement reservation;
		// never admit against the expired window or soften account loss into retry.
		fresh, freshErr := manager.bridge.service.Status(request.Context())
		if freshErr != nil {
			var authErr *productauth.Error
			if !errors.As(freshErr, &authErr) || authErr.HTTPStatus >= 500 {
				writeCommercialError(writer, promptGrantError("prompt_grant_verification_unavailable"))
				return
			}
			writeCommercialError(writer, freshErr)
			return
		}
		if freshErr == nil && fresh.Subject == grant.Subject && AccountAuthenticationError(fresh, nil) == nil {
			manager.kickPrepare()
			writeCommercialError(writer, promptGrantError("prompt_grant_pending"))
			return
		}
		writeCommercialError(writer, err)
		return
	}
	if scope == "prompt" {
		if _, ok := manager.bridge.store.internalSession(body.ConversationID, principal); !ok {
			writeCommercialConflict(writer, "unknown_conversation")
			return
		}
	}
	binding, quota, err := manager.allocate(grant, principal, scope, body.RequestID, body.ConversationID, expiry)
	if err != nil {
		manager.kickPrepare()
		writeCommercialError(writer, err)
		return
	}
	manager.bridge.writeGrant(writer, binding, "authorized", grant.Source, quota)
}

// Reservation debit and request binding share one fsynced atomic document.
func (manager *promptGrantManager) allocate(grant *productauth.VerifiedPromptGrant, principal, scope, requestID, conversationID string, expiry time.Time) (*commercialBinding, productauth.CommercialUsageQuota, error) {
	store := manager.bridge.store
	store.mu.Lock()
	defer store.mu.Unlock()
	quota := localPromptQuota(grant, store.bindings, store.grants)
	persisted := store.grants[grant.GrantID]
	if persisted == nil || persisted.Sealed || persisted.Closed || persisted.PrincipalKey != principal || !grant.ExpiresAt.After(manager.bridge.now()) {
		return nil, quota, promptGrantError("prompt_grant_pending")
	}
	for _, existing := range store.bindings {
		if existing.RequestID == requestID {
			if existing.PrincipalKey != principal || existing.Scope != scope || existing.ConversationID != conversationID || existing.Released {
				return nil, quota, promptGrantError("authorization_binding_mismatch")
			}
			copy := *existing
			return &copy, quota, nil
		}
	}
	used := map[int]bool{}
	for _, slot := range grant.SettledSlots {
		used[slot] = true
	}
	for _, existing := range store.bindings {
		if existing.PromptGrantID == grant.GrantID && !existing.Released {
			used[existing.PromptGrantSlot] = true
		}
	}
	slot := -1
	for candidate := 0; candidate < grant.Capacity; candidate++ {
		if !used[candidate] {
			slot = candidate
			break
		}
	}
	if slot < 0 {
		return nil, quota, promptGrantError("prompt_grant_pending")
	}
	id, err := randomCommercialID("local_prompt")
	if err != nil {
		return nil, quota, err
	}
	if grant.ExpiresAt.Before(expiry) {
		expiry = grant.ExpiresAt
	}
	binding := &commercialBinding{PrincipalKey: principal, Scope: scope, RequestID: requestID, AuthorizationID: id, ConversationID: conversationID, ExpiresAt: expiry}
	if scope == "prompt" {
		binding.PromptGrantID = grant.GrantID
		binding.PromptGrantSlot = slot
	}
	store.bindings[id] = binding
	if err := store.saveLocked(); err != nil {
		delete(store.bindings, id)
		return nil, quota, err
	}
	copy := *binding
	return &copy, localPromptQuota(grant, store.bindings, store.grants), nil
}
func (manager *promptGrantManager) bindDelivery(request *http.Request, grant commercialGrant, promptRequestID string) error {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxCommercialDeliveryBytes+1))
	if err != nil || len(body) > maxCommercialDeliveryBytes {
		return promptGrantError("invalid_prompt_body")
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	digest := sha256.Sum256(body)
	content := fmt.Sprintf("%x", digest)
	store := manager.bridge.store
	store.mu.Lock()
	defer store.mu.Unlock()
	binding := store.bindings[grant.AuthorizationID]
	if binding == nil || binding.Released || !sameCommercialGrant(binding, grant) {
		return promptGrantError("authorization_binding_mismatch")
	}
	if binding.PromptGrantID == "" {
		return nil
	}
	reservation := store.grants[binding.PromptGrantID]
	if reservation == nil || reservation.Sealed || reservation.Closed {
		return promptGrantError("prompt_grant_expired")
	}
	if binding.ContentSHA256 != "" && (binding.ContentSHA256 != content || (binding.PromptRequestID != "" && binding.PromptRequestID != promptRequestID)) {
		return promptGrantError("authorization_binding_mismatch")
	}
	if binding.ContentSHA256 != "" {
		return nil
	}
	previous := *binding
	binding.ContentSHA256 = content
	if err := store.saveLocked(); err != nil {
		*binding = previous
		return err
	}
	return nil
}
func (manager *promptGrantManager) settle(ctx context.Context, binding commercialBinding) error {
	if binding.PromptGrantID == "" || !binding.Accepted || binding.ContentSHA256 == "" || binding.PromptRequestID == "" {
		return promptGrantError("prompt_grant_receipt_incomplete")
	}
	status, err := manager.bridge.service.Status(ctx)
	principal, _, err := manager.bridge.principalFromStatus(status, err)
	if err != nil || !manager.settlementPrincipalAllowed(ctx, binding, status, principal) {
		return promptGrantError("prompt_grant_principal_changed")
	}
	receipt, err := manager.service.SettlePromptGrant(ctx, productauth.PromptGrantSettlementRequest{RequestID: binding.RequestID, GrantID: binding.PromptGrantID, SlotIndex: binding.PromptGrantSlot, ConversationID: binding.ConversationID, PromptRequestID: binding.PromptRequestID, ContentSHA256: binding.ContentSHA256})
	if err != nil {
		return err
	}
	status, err = manager.bridge.service.Status(ctx)
	principal, _, err = manager.bridge.principalFromStatus(status, err)
	if err != nil || !manager.settlementPrincipalAllowed(ctx, binding, status, principal) {
		return promptGrantError("prompt_grant_principal_changed")
	}
	if !receipt.Settled || receipt.GrantID != binding.PromptGrantID || receipt.SlotIndex != binding.PromptGrantSlot || receipt.ConversationID != binding.ConversationID || receipt.PromptRequestID != binding.PromptRequestID || receipt.ContentSHA256 != binding.ContentSHA256 {
		return promptGrantError("invalid_prompt_grant_receipt")
	}
	if err := manager.bridge.store.markConsumed(binding.AuthorizationID); err != nil {
		return err
	}
	manager.mu.Lock()
	if manager.current != nil && manager.current.Subject == status.Subject && manager.current.GrantID == binding.PromptGrantID {
		found := false
		for _, slot := range manager.current.SettledSlots {
			if slot == binding.PromptGrantSlot {
				found = true
			}
		}
		if !found {
			manager.current.SettledSlots = append(manager.current.SettledSlots, binding.PromptGrantSlot)
		}
	}
	// Parallel acknowledgements can arrive out of order. Keep a newer account
	// snapshot, and never publish a previous principal's receipt after a switch.
	if manager.current != nil && manager.current.Subject == status.Subject && receipt.Usage.AcceptedPromptCount >= manager.current.Usage.AcceptedPromptCount {
		manager.current.Usage = receipt.Usage
	}
	manager.mu.Unlock()
	manager.kickPrepare()
	return nil
}

// Non-quota fields retain the last real account/device observation. Prompt
// debits are projected locally; slow inventory refresh never gates a send.
func (manager *promptGrantManager) membershipProjection(ctx context.Context) map[string]any {
	grant := manager.snapshot()
	if grant == nil || grant.Window == nil {
		return nil
	}
	if _, err := manager.service.AdmissionStatus(ctx, grant.Window); err != nil {
		return nil
	}
	principal := fmt.Sprintf("%x", sha256.Sum256([]byte(grant.Subject)))
	return manager.membershipProjectionForPrincipal(principal)
}

func (manager *promptGrantManager) membershipProjectionForPrincipal(principal string) map[string]any {
	grant := manager.snapshot()
	if grant == nil || principal != fmt.Sprintf("%x", sha256.Sum256([]byte(grant.Subject))) {
		return nil
	}
	manager.mu.Lock()
	base, observedPrincipal := manager.usageBase, manager.usagePrincipal
	manager.mu.Unlock()
	if base == nil || observedPrincipal != principal {
		return nil
	}
	out := make(map[string]any, len(base))
	for key, value := range base {
		if object, ok := value.(map[string]any); ok {
			copy := make(map[string]any, len(object))
			for k, v := range object {
				copy[k] = v
			}
			out[key] = copy
		} else {
			out[key] = value
		}
	}
	store := manager.bridge.store
	store.mu.Lock()
	quota := localPromptQuota(grant, store.bindings, store.grants)
	store.mu.Unlock()
	out["usage"] = quota
	if access, ok := out["access"].(map[string]any); ok {
		access["active"] = quota.CanSubmit
		if quota.Unlimited {
			access["kind"] = "lifetime"
		} else {
			access["kind"] = "free_account"
		}
	}
	if pool, ok := out["legacy_prompt_pool"].(map[string]any); ok {
		pool["new_conversation_allowed"] = quota.CanSubmit
	}
	return out
}

// A fresh login can settle an earlier login's receipt only after a newly signed
// grant proves the same account and device. It never revives old admission.
func (manager *promptGrantManager) settlementPrincipalAllowed(ctx context.Context, binding commercialBinding, status productauth.Status, principal string) bool {
	if principal == binding.PrincipalKey {
		return true
	}
	current := manager.snapshot()
	if current == nil || current.Window == nil || current.AccountID == "" || current.Subject != status.Subject {
		return false
	}
	if _, err := manager.service.AdmissionStatus(ctx, current.Window); err != nil {
		return false
	}
	store := manager.bridge.store
	store.mu.Lock()
	defer store.mu.Unlock()
	old := store.grants[binding.PromptGrantID]
	return old != nil && old.PrincipalKey == binding.PrincipalKey && old.AccountID == current.AccountID && old.DeviceID != "" && old.DeviceID == current.DeviceID
}

func (manager *promptGrantManager) kickHistorical() {
	manager.mu.Lock()
	if manager.historyRunning {
		manager.mu.Unlock()
		return
	}
	manager.historyRunning = true
	manager.mu.Unlock()
	go func() {
		defer func() { manager.mu.Lock(); manager.historyRunning = false; manager.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		current := manager.snapshot()
		if current == nil || current.AccountID == "" || current.DeviceID == "" {
			return
		}
		store := manager.bridge.store
		store.mu.Lock()
		var grants []durablePromptGrant
		for _, grant := range store.grants {
			if grant.GrantID != current.GrantID && !grant.Closed && grant.AccountID == current.AccountID && grant.DeviceID == current.DeviceID {
				grants = append(grants, *grant)
				if len(grants) >= 16 {
					break
				}
			}
		}
		store.mu.Unlock()
		for _, old := range grants {
			manager.bridge.kickConsumptionSync(old.PrincipalKey)
			if old.ExpiresAt.After(manager.bridge.now()) {
				continue
			}
			store.mu.Lock()
			grant := store.grants[old.GrantID]
			previous := grant.Sealed
			grant.Sealed = true
			if err := store.saveLocked(); err != nil {
				grant.Sealed = previous
				store.mu.Unlock()
				return
			}
			var pending []commercialBinding
			unresolved := false
			for _, binding := range store.bindings {
				if binding.PromptGrantID == old.GrantID && !binding.Consumed {
					if binding.DeliveryStarted && !binding.Accepted {
						unresolved = true
					}
					if binding.Accepted {
						pending = append(pending, *binding)
					}
				}
			}
			store.mu.Unlock()
			if unresolved {
				continue
			}
			settled := true
			for _, binding := range pending {
				if manager.settle(ctx, binding) != nil {
					settled = false
					break
				}
			}
			if !settled {
				continue
			}
			status, err := manager.bridge.service.Status(ctx)
			principal, _, err := manager.bridge.principalFromStatus(status, err)
			if err != nil || !manager.settlementPrincipalAllowed(ctx, commercialBinding{PrincipalKey: old.PrincipalKey, PromptGrantID: old.GrantID}, status, principal) {
				return
			}
			id, err := randomCommercialID("grant_close")
			if err != nil {
				return
			}
			receipt, err := manager.service.ClosePromptGrant(ctx, productauth.PromptGrantCloseRequest{RequestID: id, GrantID: old.GrantID})
			if err != nil || !receipt.Closed || receipt.GrantID != old.GrantID {
				continue
			}
			store.mu.Lock()
			store.grants[old.GrantID].Closed = true
			err = store.saveLocked()
			store.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
}

func (bridge *commercialBridge) grantDeliveryAdmission(ctx context.Context, request commercialGrant) func() error {
	if bridge.promptGrants == nil {
		return nil
	}
	manager := bridge.promptGrants
	return func() error {
		// beginDelivery holds the durable store lock, closing seal/commit races.
		binding := bridge.store.bindings[request.AuthorizationID]
		if binding == nil || binding.PromptGrantID == "" {
			return nil
		}
		current := manager.snapshot()
		if current == nil || current.GrantID != binding.PromptGrantID || current.Window == nil {
			return promptGrantError("prompt_grant_changed")
		}
		status, err := manager.service.AdmissionStatus(ctx, current.Window)
		principal, _, err := bridge.principalFromStatus(status, err)
		if err != nil {
			return err
		}
		if principal != binding.PrincipalKey {
			return promptGrantError("prompt_grant_principal_changed")
		}
		return nil
	}
}

func (store *commercialBindingStore) validatePromptGrantBindings() error {
	used := map[string]map[int]bool{}
	for _, grant := range store.grants {
		if grant.AccountID == "" || len(grant.AccountID) > 160 || grant.DeviceID == "" || len(grant.DeviceID) > 160 || (grant.Closed && !grant.Sealed) {
			return fmt.Errorf("prompt grant owner metadata is invalid")
		}
	}
	for _, binding := range store.bindings {
		if binding.PromptGrantID == "" {
			if binding.ContentSHA256 != "" || binding.PromptGrantSlot != 0 || binding.Released {
				return fmt.Errorf("prompt grant fields lack reservation")
			}
			continue
		}
		grant := store.grants[binding.PromptGrantID]
		if grant == nil || binding.Scope != "prompt" || binding.PrincipalKey != grant.PrincipalKey || binding.PromptGrantSlot < 0 || binding.PromptGrantSlot >= grant.Capacity || binding.ExpiresAt.After(grant.ExpiresAt) {
			return fmt.Errorf("prompt binding reservation is invalid")
		}
		if (binding.Consumed && !binding.Accepted) || (binding.Accepted && !binding.DeliveryStarted) {
			return fmt.Errorf("prompt receipt progression is invalid")
		}
		if binding.Released && (binding.Accepted || binding.Consumed || binding.DeliveryStarted) {
			return fmt.Errorf("released prompt remains accepted")
		}
		if binding.ContentSHA256 != "" {
			digest, err := hex.DecodeString(binding.ContentSHA256)
			if err != nil || len(digest) != sha256.Size || strings.ToLower(binding.ContentSHA256) != binding.ContentSHA256 {
				return fmt.Errorf("prompt content digest is invalid")
			}
		}
		if (binding.Accepted || binding.DeliveryStarted || binding.Consumed) && (binding.ContentSHA256 == "" || binding.PromptRequestID == "") {
			return fmt.Errorf("prompt delivery receipt is incomplete")
		}
		if !binding.Released {
			if used[grant.GrantID] == nil {
				used[grant.GrantID] = map[int]bool{}
			}
			if used[grant.GrantID][binding.PromptGrantSlot] {
				return fmt.Errorf("prompt reservation slot is duplicated")
			}
			used[grant.GrantID][binding.PromptGrantSlot] = true
		}
	}
	return nil
}
