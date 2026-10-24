package productauth

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	PromptGrantSchema          = "meadow.prompt-grant.v1"
	PromptGrantAudience        = "meadow-desktop-prompt-grant"
	PromptGrantType            = "meadow-prompt-grant+jwt"
	MaximumPromptGrantCapacity = 5
	maximumPromptGrantLife     = 15 * time.Minute
	promptGrantPreparePath     = "/commercialPreparePromptGrant"
	promptGrantSettlePath      = "/commercialSettlePromptGrant"
	promptGrantClosePath       = "/commercialClosePromptGrant"
)

// PromptGrantUsage is deliberately separate from the legacy commercial-v2 DTO.
// Remaining includes reserved slots; Available excludes them across all devices.
type PromptGrantUsage struct {
	Unit                string `json:"unit"`
	Limit               *int   `json:"limit"`
	AcceptedPromptCount int    `json:"accepted_prompt_count"`
	Remaining           *int   `json:"remaining"`
	ReservedPromptCount int    `json:"reserved_prompt_count"`
	Available           *int   `json:"available"`
	Unlimited           bool   `json:"unlimited"`
	CanSubmit           bool   `json:"can_submit"`
}

func (value *PromptGrantUsage) UnmarshalJSON(data []byte) error {
	type plain PromptGrantUsage
	var decoded plain
	if err := strictJSON(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"unit", "limit", "accepted_prompt_count", "remaining", "reserved_prompt_count", "available", "unlimited", "can_submit"} {
		raw, ok := fields[name]
		if !ok || (name != "limit" && name != "remaining" && name != "available" && string(raw) == "null") {
			return errors.New("prompt grant quota field is required")
		}
	}
	*value = PromptGrantUsage(decoded)
	return ValidatePromptGrantUsage(*value)
}

func ValidatePromptGrantUsage(value PromptGrantUsage) error {
	if value.Unit != "accepted_user_prompt" || value.AcceptedPromptCount < 0 || value.ReservedPromptCount < 0 {
		return errors.New("prompt grant quota is invalid")
	}
	if value.Unlimited {
		if value.Limit != nil || value.Remaining != nil || value.Available != nil || !value.CanSubmit {
			return errors.New("unlimited prompt grant quota is invalid")
		}
		return nil
	}
	remaining := commercialAcceptedPromptLimit - value.AcceptedPromptCount
	if remaining < 0 {
		remaining = 0
	}
	available := remaining - value.ReservedPromptCount
	if available < 0 {
		return errors.New("prompt grant reservations exceed remaining quota")
	}
	if value.Limit == nil || *value.Limit != commercialAcceptedPromptLimit || value.Remaining == nil || *value.Remaining != remaining || value.Available == nil || *value.Available != available || value.CanSubmit != (available > 0) {
		return errors.New("prompt grant quota is inconsistent")
	}
	return nil
}

// VerifiedPromptGrant stays inside the native process. Subject is the existing
// lease principal/session; AccountID is the server's signed Firebase subject.
// Neither SignedGrant nor Window is exposed to a browser projection.
type VerifiedPromptGrant struct {
	SignedGrant            string `json:"-"`
	GrantID                string
	Subject                string
	AccountID              string
	SessionID              string
	DeviceID               string
	Capacity               int
	Source                 string
	IssuedAt               time.Time
	ExpiresAt              time.Time
	RevocationEpoch        uint64
	SettledSlots           []int
	ReconciliationRequired bool
	Usage                  PromptGrantUsage
	Window                 *AdmissionWindow `json:"-"`
}

type PromptGrantSettlementRequest struct {
	RequestID       string
	GrantID         string
	SlotIndex       int
	ConversationID  string
	PromptRequestID string
	ContentSHA256   string
}

type PromptGrantCloseRequest struct {
	RequestID string
	GrantID   string
}

type PromptGrantSettlementReceipt struct {
	Schema          string           `json:"schema"`
	GrantID         string           `json:"grant_id"`
	SlotIndex       int              `json:"slot_index"`
	AuthorizationID string           `json:"authorization_id"`
	ConversationID  string           `json:"conversation_id"`
	PromptRequestID string           `json:"prompt_request_id"`
	ContentSHA256   string           `json:"content_sha256"`
	Settled         bool             `json:"settled"`
	ServerTime      time.Time        `json:"server_time"`
	Usage           PromptGrantUsage `json:"usage"`
}

func (value *PromptGrantSettlementReceipt) UnmarshalJSON(data []byte) error {
	type plain PromptGrantSettlementReceipt
	var decoded plain
	if err := strictJSON(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["slot_index"]; !ok || string(raw) == "null" {
		return errors.New("prompt grant slot index is required")
	}
	*value = PromptGrantSettlementReceipt(decoded)
	return nil
}

type PromptGrantCloseReceipt struct {
	Schema        string           `json:"schema"`
	GrantID       string           `json:"grant_id"`
	Closed        bool             `json:"closed"`
	ReleasedSlots []int            `json:"released_slots"`
	ServerTime    time.Time        `json:"server_time"`
	Usage         PromptGrantUsage `json:"usage"`
}

type promptGrantResponse struct {
	Schema                 string           `json:"schema"`
	Grant                  string           `json:"grant"`
	KeyID                  string           `json:"kid"`
	ServerTime             time.Time        `json:"server_time"`
	SettledSlots           []int            `json:"settled_slots"`
	ReconciliationRequired *bool            `json:"reconciliation_required"`
	Usage                  PromptGrantUsage `json:"usage"`
}

type promptGrantClaims struct {
	Schema             string `json:"schema"`
	Issuer             string `json:"iss"`
	Audience           string `json:"aud"`
	Subject            string `json:"sub"`
	SessionID          string `json:"session_id"`
	DeviceID           string `json:"device_id"`
	InstallationDigest string `json:"installation_digest"`
	KeyThumbprint      string `json:"key_thumbprint"`
	RevocationEpoch    uint64 `json:"revocation_epoch"`
	GrantID            string `json:"grant_id"`
	Capacity           int    `json:"capacity"`
	Source             string `json:"source"`
	IssuedAt           int64  `json:"iat"`
	ExpiresAt          int64  `json:"exp"`
	KeyID              string `json:"kid"`
}

type promptGrantAccessClient interface {
	preparePromptGrant(context.Context, commercialRemoteRequest) (promptGrantResponse, error)
	settlePromptGrant(context.Context, commercialRemoteRequest, PromptGrantSettlementRequest) (PromptGrantSettlementReceipt, error)
	closePromptGrant(context.Context, commercialRemoteRequest, PromptGrantCloseRequest) (PromptGrantCloseReceipt, error)
}

func (client *FixedClient) promptGrantCall(ctx context.Context, path string, input commercialRemoteRequest, fields map[string]any, output any) error {
	if (path != promptGrantPreparePath && path != promptGrantSettlePath && path != promptGrantClosePath) || !validCredential(input.AccessToken, 32, 128) || !validOpaque(input.RequestID, 160) || !validInstallationID(input.InstallationID) || !validCanonicalBase64URL(input.DeviceProof, ed25519.SignatureSize) {
		return authError("invalid_request", "prompt grant request is invalid", http.StatusBadRequest, false, nil)
	}
	body := map[string]any{"schema": SchemaV1, "request_id": input.RequestID, "installation_id": input.InstallationID, "device_proof": input.DeviceProof}
	for key, value := range fields {
		body[key] = value
	}
	return client.doCommercial(ctx, path, input.AccessToken, body, map[string]string{"X-Request-ID": input.RequestID}, output)
}

func (client *FixedClient) preparePromptGrant(ctx context.Context, input commercialRemoteRequest) (promptGrantResponse, error) {
	var response promptGrantResponse
	if err := client.promptGrantCall(ctx, promptGrantPreparePath, input, nil, &response); err != nil {
		return response, err
	}
	if response.Schema != PromptGrantSchema || !validOpaque(response.KeyID, 128) || response.Grant == "" || response.ServerTime.IsZero() || response.ReconciliationRequired == nil || response.SettledSlots == nil || ValidatePromptGrantUsage(response.Usage) != nil {
		return promptGrantResponse{}, invalidCommercialResponse(errors.New("prompt grant response is invalid"))
	}
	return response, nil
}

func validPromptGrantSettlement(request PromptGrantSettlementRequest) bool {
	return validOpaque(request.RequestID, 160) && validOpaque(request.GrantID, 160) && request.SlotIndex >= 0 && request.SlotIndex < MaximumPromptGrantCapacity && validOpaque(request.ConversationID, 160) && validOpaque(request.PromptRequestID, 160) && validPromptContentDigest(request.ContentSHA256)
}

func validPromptContentDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func promptGrantSettlementBinding(request PromptGrantSettlementRequest) string {
	return strings.Join([]string{request.GrantID, strconv.Itoa(request.SlotIndex), request.ConversationID, request.PromptRequestID, request.ContentSHA256}, "\n")
}

func (client *FixedClient) settlePromptGrant(ctx context.Context, input commercialRemoteRequest, request PromptGrantSettlementRequest) (PromptGrantSettlementReceipt, error) {
	var response PromptGrantSettlementReceipt
	if !validPromptGrantSettlement(request) || input.RequestID != request.RequestID {
		return response, authError("invalid_request", "prompt grant settlement is invalid", http.StatusBadRequest, false, nil)
	}
	fields := map[string]any{"grant_id": request.GrantID, "slot_index": request.SlotIndex, "conversation_id": request.ConversationID, "prompt_request_id": request.PromptRequestID, "content_sha256": request.ContentSHA256}
	if err := client.promptGrantCall(ctx, promptGrantSettlePath, input, fields, &response); err != nil {
		return response, err
	}
	if response.Schema != PromptGrantSchema || response.GrantID != request.GrantID || response.SlotIndex != request.SlotIndex || response.ConversationID != request.ConversationID || response.PromptRequestID != request.PromptRequestID || response.ContentSHA256 != request.ContentSHA256 || !validOpaque(response.AuthorizationID, 160) || !response.Settled || response.ServerTime.IsZero() || ValidatePromptGrantUsage(response.Usage) != nil {
		return PromptGrantSettlementReceipt{}, invalidCommercialResponse(errors.New("prompt grant settlement receipt is invalid"))
	}
	return response, nil
}

func validPromptGrantSlots(slots []int, capacity int) bool {
	if slots == nil || len(slots) > capacity {
		return false
	}
	seen := make(map[int]bool, len(slots))
	for _, slot := range slots {
		if slot < 0 || slot >= capacity || seen[slot] {
			return false
		}
		seen[slot] = true
	}
	return true
}

func (client *FixedClient) closePromptGrant(ctx context.Context, input commercialRemoteRequest, request PromptGrantCloseRequest) (PromptGrantCloseReceipt, error) {
	var response PromptGrantCloseReceipt
	if !validOpaque(request.GrantID, 160) || input.RequestID != request.RequestID {
		return response, authError("invalid_request", "prompt grant close is invalid", http.StatusBadRequest, false, nil)
	}
	if err := client.promptGrantCall(ctx, promptGrantClosePath, input, map[string]any{"grant_id": request.GrantID}, &response); err != nil {
		return response, err
	}
	if response.Schema != PromptGrantSchema || response.GrantID != request.GrantID || !response.Closed || response.ServerTime.IsZero() || !validPromptGrantSlots(response.ReleasedSlots, MaximumPromptGrantCapacity) || ValidatePromptGrantUsage(response.Usage) != nil {
		return PromptGrantCloseReceipt{}, invalidCommercialResponse(errors.New("prompt grant close receipt is invalid"))
	}
	return response, nil
}

func (service *Service) promptGrantRemote(ctx context.Context, requestID, action, binding string) (promptGrantAccessClient, commercialRemoteRequest, accessSession, string, error) {
	if !validOpaque(requestID, 160) {
		return nil, commercialRemoteRequest{}, accessSession{}, "", authError("invalid_request", "prompt grant request identity is invalid", http.StatusBadRequest, false, nil)
	}
	client, ok := service.client.(promptGrantAccessClient)
	if !ok {
		return nil, commercialRemoteRequest{}, accessSession{}, "", authError("commercial_unavailable", "prompt grants are unavailable", http.StatusServiceUnavailable, false, nil)
	}
	access, err := service.currentAccess(ctx)
	if err != nil {
		return nil, commercialRemoteRequest{}, accessSession{}, "", err
	}
	privateKey, publicKey, err := service.deviceProofKey(ctx)
	if err != nil {
		return nil, commercialRemoteRequest{}, accessSession{}, "", err
	}
	defer clear(privateKey)
	remote := commercialRemoteRequest{AccessToken: access.token, RequestID: requestID, InstallationID: service.installationID, DeviceProof: signBoundDeviceProof(privateKey, action, access.token, service.installationID, requestID, "", binding)}
	return client, remote, access, publicKey, nil
}

func isRotatedPromptGrantAccess(err error) bool {
	var rejected *Error
	return errors.As(err, &rejected) && rejected.HTTPStatus == http.StatusUnauthorized && rejected.Code == "invalid_access_token"
}

// Only an explicit stale-bearer rejection permits one exact operation retry.
// Quota, ownership, proof and revocation failures retain their original result.
// The durable logical request identity is never replaced by this refresh.
func (service *Service) retryPromptGrantRemote(ctx context.Context, requestID, action, binding string, previous accessSession) (promptGrantAccessClient, commercialRemoteRequest, accessSession, string, error) {
	current, err := service.currentAccess(ctx)
	if err != nil {
		return nil, commercialRemoteRequest{}, accessSession{}, "", err
	}
	samePrincipal := func(value accessSession) bool {
		return value.lease.Subject == previous.lease.Subject && value.lease.DeviceID == previous.lease.DeviceID
	}
	if !samePrincipal(current) {
		return nil, commercialRemoteRequest{}, accessSession{}, "", authorizationChanged()
	}
	if current.token == previous.token {
		if _, err := service.refreshForCaller(ctx, false); err != nil {
			return nil, commercialRemoteRequest{}, accessSession{}, "", err
		}
	}
	client, remote, current, publicKey, err := service.promptGrantRemote(ctx, requestID, action, binding)
	if err != nil {
		return nil, commercialRemoteRequest{}, accessSession{}, "", err
	}
	if !samePrincipal(current) {
		return nil, commercialRemoteRequest{}, accessSession{}, "", authorizationChanged()
	}
	return client, remote, current, publicKey, nil
}

// PreparePromptGrant does authoritative account I/O before creating a bounded,
// device-bound native admission window. It never makes a browser token.
func (service *Service) PreparePromptGrant(ctx context.Context, requestID string) (VerifiedPromptGrant, error) {
	client, remote, access, publicKey, err := service.promptGrantRemote(ctx, requestID, "prompt_grant_prepare", "")
	if err != nil {
		return VerifiedPromptGrant{}, err
	}
	response, err := client.preparePromptGrant(ctx, remote)
	if isRotatedPromptGrantAccess(err) {
		client, remote, access, publicKey, err = service.retryPromptGrantRemote(ctx, requestID, "prompt_grant_prepare", "", access)
		if err == nil {
			response, err = client.preparePromptGrant(ctx, remote)
		}
	}
	if err != nil {
		return VerifiedPromptGrant{}, err
	}
	if response.ReconciliationRequired == nil || ValidatePromptGrantUsage(response.Usage) != nil {
		return VerifiedPromptGrant{}, invalidCommercialResponse(errors.New("prompt grant response is invalid"))
	}
	grant, err := service.verifySignedPromptGrant(response.Grant, access, publicKey, response.KeyID, *response.ReconciliationRequired)
	if err != nil {
		return VerifiedPromptGrant{}, err
	}
	if !validPromptGrantSlots(response.SettledSlots, grant.Capacity) || (grant.Source == "lifetime" && !response.Usage.Unlimited) {
		return VerifiedPromptGrant{}, invalidCommercialResponse(errors.New("prompt grant reservation disagrees with quota"))
	}
	grant.SettledSlots = append([]int{}, response.SettledSlots...)
	grant.Usage = response.Usage
	grant.ReconciliationRequired = *response.ReconciliationRequired
	deadline := grant.ExpiresAt
	if grant.ReconciliationRequired {
		deadline = service.now().Add(time.Second)
	}
	window, err := service.admissionWindowFromAccess(ctx, access, grant.Subject, grant.DeviceID, deadline)
	if err != nil {
		return VerifiedPromptGrant{}, err
	}
	if !grant.ReconciliationRequired {
		grant.Window = window
	}
	return grant, nil
}

func (service *Service) SettlePromptGrant(ctx context.Context, request PromptGrantSettlementRequest) (PromptGrantSettlementReceipt, error) {
	if !validPromptGrantSettlement(request) {
		return PromptGrantSettlementReceipt{}, authError("invalid_request", "prompt grant settlement is invalid", http.StatusBadRequest, false, nil)
	}
	client, remote, access, _, err := service.promptGrantRemote(ctx, request.RequestID, "prompt_grant_settle", promptGrantSettlementBinding(request))
	if err != nil {
		return PromptGrantSettlementReceipt{}, err
	}
	response, err := client.settlePromptGrant(ctx, remote, request)
	if isRotatedPromptGrantAccess(err) {
		client, remote, access, _, err = service.retryPromptGrantRemote(ctx, request.RequestID, "prompt_grant_settle", promptGrantSettlementBinding(request), access)
		if err == nil {
			response, err = client.settlePromptGrant(ctx, remote, request)
		}
	}
	if err != nil {
		return PromptGrantSettlementReceipt{}, err
	}
	if _, err := service.admissionWindowFromAccess(ctx, access, access.lease.Subject, access.lease.DeviceID, service.now().Add(time.Second)); err != nil {
		return PromptGrantSettlementReceipt{}, err
	}
	return response, nil
}

func (service *Service) ClosePromptGrant(ctx context.Context, request PromptGrantCloseRequest) (PromptGrantCloseReceipt, error) {
	if !validOpaque(request.GrantID, 160) {
		return PromptGrantCloseReceipt{}, authError("invalid_request", "prompt grant identity is invalid", http.StatusBadRequest, false, nil)
	}
	client, remote, access, _, err := service.promptGrantRemote(ctx, request.RequestID, "prompt_grant_close", request.GrantID)
	if err != nil {
		return PromptGrantCloseReceipt{}, err
	}
	response, err := client.closePromptGrant(ctx, remote, request)
	if isRotatedPromptGrantAccess(err) {
		client, remote, access, _, err = service.retryPromptGrantRemote(ctx, request.RequestID, "prompt_grant_close", request.GrantID, access)
		if err == nil {
			response, err = client.closePromptGrant(ctx, remote, request)
		}
	}
	if err != nil {
		return PromptGrantCloseReceipt{}, err
	}
	if _, err := service.admissionWindowFromAccess(ctx, access, access.lease.Subject, access.lease.DeviceID, service.now().Add(time.Second)); err != nil {
		return PromptGrantCloseReceipt{}, err
	}
	return response, nil
}

func (service *Service) verifySignedPromptGrant(compact string, access accessSession, devicePublicKey, responseKeyID string, reconciliation bool) (VerifiedPromptGrant, error) {
	reject := func() (VerifiedPromptGrant, error) {
		return VerifiedPromptGrant{}, authError("prompt_grant_rejected", "signed prompt grant is invalid", http.StatusForbidden, false, nil)
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 || len(compact) > 16<<10 {
		return reject()
	}
	headerBytes, err := decodeCanonicalBase64URL(parts[0], 1, 1024)
	if err != nil {
		return reject()
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
		Type      string `json:"typ"`
	}
	if strictJSON(headerBytes, &header) != nil || header.Algorithm != "EdDSA" || header.Type != PromptGrantType || !validOpaque(header.KeyID, 128) || header.KeyID != responseKeyID {
		return reject()
	}
	key := service.leaseKeys[header.KeyID]
	signature, err := decodeCanonicalBase64URL(parts[2], ed25519.SignatureSize, ed25519.SignatureSize)
	if len(key) != ed25519.PublicKeySize || err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return reject()
	}
	payload, err := decodeCanonicalBase64URL(parts[1], 1, 12<<10)
	if err != nil {
		return reject()
	}
	var claimFields map[string]json.RawMessage
	if json.Unmarshal(payload, &claimFields) != nil {
		return reject()
	}
	if raw, ok := claimFields["revocation_epoch"]; !ok || string(raw) == "null" {
		return reject()
	}
	var claims promptGrantClaims
	if strictJSON(payload, &claims) != nil || claims.Schema != PromptGrantSchema || claims.Issuer != service.client.Issuer() || claims.Audience != PromptGrantAudience || claims.KeyID != header.KeyID || !validOpaque(claims.Subject, 160) || !validOpaque(claims.GrantID, 160) || claims.SessionID != access.lease.Subject || claims.DeviceID != access.lease.DeviceID || claims.InstallationDigest != domainDigestHex("meadow.installation.public.v1", service.installationID) || claims.KeyThumbprint != domainDigestHex("meadow.device-auth-key.v1", devicePublicKey) || claims.RevocationEpoch != access.lease.RevocationEpoch || claims.Capacity < 1 || claims.Capacity > MaximumPromptGrantCapacity || (claims.Source != "free_account" && claims.Source != "lifetime") || claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt {
		return reject()
	}
	issued := time.Unix(claims.IssuedAt, 0).UTC()
	expires := time.Unix(claims.ExpiresAt, 0).UTC()
	now := service.now().UTC()
	if expires.Sub(issued) > maximumPromptGrantLife || issued.After(now.Add(service.futureClockSkew)) || (!reconciliation && !expires.After(now)) || (reconciliation && expires.After(now)) {
		return reject()
	}
	return VerifiedPromptGrant{SignedGrant: compact, GrantID: claims.GrantID, Subject: claims.SessionID, AccountID: claims.Subject, SessionID: claims.SessionID, DeviceID: claims.DeviceID, Capacity: claims.Capacity, Source: claims.Source, IssuedAt: issued, ExpiresAt: expires, RevocationEpoch: claims.RevocationEpoch}, nil
}
