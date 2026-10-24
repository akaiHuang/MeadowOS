package agentdaemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/productauth"
	"github.com/hey-meadow/meadow-code/go/core/serve"
)

const (
	desktopMembershipSchema    = "meadow.desktop-membership.v2"
	commercialStoreSchema      = "meadow.agentd-commercial-bindings.v1"
	commercialStoreFile        = "commercial-membership-bindings-v1.json"
	maxCommercialStoreBytes    = 4 << 20
	maxCommercialReplyBytes    = 1 << 20
	maxCommercialDeliveryBytes = 256 << 20
	maxCommercialReceiptBytes  = 8 << 20
	maxCommercialReceipts      = 256

	membershipSchemaHeader        = "X-Meadow-Membership-Schema"
	membershipRequestHeader       = "X-Meadow-Membership-Request-ID"
	membershipAuthorizationHeader = "X-Meadow-Membership-Authorization-ID"
	membershipConversationHeader  = "X-Meadow-Membership-Conversation-ID"
	promptRequestIDHeader         = "X-Meadow-Prompt-Request-ID"

	controlsProofSchema             = "meadow.native-controls-proof.v1"
	controlsProofSchemaHeader       = "X-Meadow-Controls-Schema"
	controlsProofConversationHeader = "X-Meadow-Controls-Conversation"
	controlsProofPrincipalHeader    = "X-Meadow-Controls-Principal"
	controlsProofExpiresHeader      = "X-Meadow-Controls-Expires"
)

type CommercialMembershipService interface {
	Status(context.Context) (productauth.Status, error)
	ListDevices(context.Context, productauth.DeviceListRequest) (productauth.DeviceListResult, error)
	CommercialUsageStatus(context.Context) (productauth.CommercialUsageStatus, error)
	CommercialAuthorizeNewConversation(context.Context, productauth.CommercialConversationRequest) (productauth.CommercialAuthorization, error)
	CommercialAuthorizePrompt(context.Context, productauth.CommercialPromptRequest) (productauth.CommercialAuthorization, error)
	CommercialConsumePrompt(context.Context, productauth.CommercialConsumeRequest) (productauth.CommercialConsumeReceipt, error)
	CommercialReferralShare(context.Context, productauth.CommercialReferralRequest) (productauth.CommercialReferralShare, error)
	CommercialReferralAccept(context.Context, productauth.CommercialReferralAcceptRequest) (productauth.CommercialReferralAcceptReceipt, error)
	CommercialReferralStatus(context.Context) (productauth.CommercialReferralStatus, error)
}

type commercialBridge struct {
	service          CommercialMembershipService
	accountAdmission bool
	store            *commercialBindingStore
	now              func() time.Time
	deliveryInstance string
	receiptMu        sync.Mutex
	receipts         map[string]*commercialDeliveryReceipt
	receiptBytes     int
	syncMu           sync.Mutex
	syncing          map[string]bool
	syncAgain        map[string]bool
	promptGrants     *promptGrantManager
	resumeMu         sync.Mutex
	resumedSessions  map[string]commercialResumedSession
	resumeLocks      map[string]*sync.Mutex
}

type commercialDeliveryReceipt struct {
	fingerprint string
	done        chan struct{}
	response    *commercialResponse
	bytes       int
}

type commercialBinding struct {
	PrincipalKey        string    `json:"principal_key,omitempty"`
	Scope               string    `json:"scope"`
	RequestID           string    `json:"request_id"`
	AuthorizationID     string    `json:"authorization_id"`
	ConversationID      string    `json:"conversation_id"`
	ExpiresAt           time.Time `json:"expires_at"`
	InternalSessionID   string    `json:"internal_session_id,omitempty"`
	WorkspaceCWD        string    `json:"workspace_cwd,omitempty"`
	ModelID             string    `json:"model_id,omitempty"`
	ConnectionID        string    `json:"connection_id,omitempty"`
	ApprovalChannel     string    `json:"approval_channel,omitempty"`
	DeliveryStarted     bool      `json:"delivery_started,omitempty"`
	Accepted            bool      `json:"accepted,omitempty"`
	Consumed            bool      `json:"consumed,omitempty"`
	PromptRequestID     string    `json:"prompt_request_id,omitempty"`
	DeliveryInstance    string    `json:"delivery_instance,omitempty"`
	DeliveryPath        string    `json:"delivery_path,omitempty"`
	DeliveryFingerprint string    `json:"delivery_fingerprint,omitempty"`
	QueueItemID         string    `json:"queue_item_id,omitempty"`
	PromptGrantID       string    `json:"prompt_grant_id,omitempty"`
	PromptGrantSlot     int       `json:"prompt_grant_slot,omitempty"`
	ContentSHA256       string    `json:"content_sha256,omitempty"`
	Released            bool      `json:"released,omitempty"`
}

type commercialBindingDocument struct {
	Schema       string                         `json:"schema"`
	Bindings     map[string]*commercialBinding  `json:"bindings"`
	PromptGrants map[string]*durablePromptGrant `json:"prompt_grants,omitempty"`
}

type commercialBindingStore struct {
	path     string
	mu       sync.Mutex
	bindings map[string]*commercialBinding
	grants   map[string]*durablePromptGrant
}

func newCommercialBridge(stateRoot string, service CommercialMembershipService) (*commercialBridge, error) {
	if service == nil {
		return nil, nil
	}
	store, err := newCommercialBindingStore(stateRoot)
	if err != nil {
		return nil, err
	}
	deliveryInstance, err := randomCommercialID("delivery")
	if err != nil {
		return nil, err
	}
	return &commercialBridge{service: service, store: store, now: time.Now, deliveryInstance: deliveryInstance, receipts: make(map[string]*commercialDeliveryReceipt)}, nil
}

func newCommercialBindingStore(stateRoot string) (*commercialBindingStore, error) {
	root := strings.TrimSpace(stateRoot)
	if root == "" {
		return nil, errors.New("commercial membership persistence root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve commercial membership persistence root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create commercial membership persistence root: %w", err)
	}
	store := &commercialBindingStore{path: filepath.Join(absolute, commercialStoreFile), bindings: make(map[string]*commercialBinding), grants: make(map[string]*durablePromptGrant)}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (bridge *commercialBridge) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /commercial/usage", bridge.handleUsage)
	if bridge.promptGrants != nil {
		mux.HandleFunc("POST /commercial/prepare-prompt-grant", bridge.promptGrants.handlePrepare)
		mux.HandleFunc("GET /auth/admission-status", bridge.handleAdmissionStatus)
		mux.HandleFunc("GET /commercial/prompt-grant-capability", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"schema": "meadow.desktop-prompt-grant.v1", "enabled": true})
		})
	}
	mux.HandleFunc("POST /commercial/authorize-new-conversation", bridge.handleAuthorizeNewConversation)
	mux.HandleFunc("POST /commercial/authorize-prompt", bridge.handleAuthorizePrompt)
	mux.HandleFunc("POST /commercial/consume-prompt", bridge.handleConsumePrompt)
	mux.HandleFunc("POST /commercial/referral/share", bridge.handleReferralShare)
	mux.HandleFunc("POST /commercial/referral/accept", bridge.handleReferralAccept)
	mux.HandleFunc("GET /commercial/referral/status", bridge.handleReferralStatus)
}

// Exact synchronous routes already pass through principalAccess before any
// protected mutation or observation. New routes keep the outer account guard
// until explicitly reviewed here; cancellation keeps its existing exemption.
func (bridge *commercialBridge) ownsAccountAdmission(request *http.Request) bool {
	if bridge == nil || !bridge.accountAdmission {
		return false
	}
	if request.Method == http.MethodPost {
		switch request.URL.Path {
		case "/commercial/authorize-new-conversation", "/commercial/authorize-prompt", "/session", "/session/resume":
			return true
		}
	}
	_, suffix, canonical := canonicalSessionPath(request.URL.Path)
	if !canonical {
		return false
	}
	switch suffix {
	case "/model", "/prompt":
		return request.Method == http.MethodPost
	case "/harness-controls", "/inference-controls":
		return request.Method == http.MethodGet || request.Method == http.MethodPost
	default:
		return false
	}
}

func (bridge *commercialBridge) handleUsage(writer http.ResponseWriter, request *http.Request) {
	if principal, _, err := bridge.principalAccess(request.Context()); err == nil {
		defer bridge.kickConsumptionSync(principal)
	}
	projection, err := bridge.usageProjection(request.Context())
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, projection)
}

func (bridge *commercialBridge) handleAuthorizeNewConversation(writer http.ResponseWriter, request *http.Request) {
	if bridge.promptGrants != nil {
		bridge.promptGrants.handleAuthorize(writer, request, "new_conversation")
		return
	}
	principal, _, err := bridge.principalAccess(request.Context())
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	var body struct {
		Schema         string `json:"schema"`
		RequestID      string `json:"request_id"`
		ConversationID string `json:"conversation_id"`
	}
	if decodeJSON(request, &body, true) != nil || body.Schema != desktopMembershipSchema ||
		!validCommercialID(body.RequestID) || !validCommercialID(body.ConversationID) {
		writeCommercialRequestError(writer)
		return
	}
	existing, replay := bridge.store.byRequest(body.RequestID)
	if replay {
		binding := existing
		if binding.PrincipalKey != principal || binding.Scope != "new_conversation" || binding.ConversationID != body.ConversationID {
			writeCommercialConflict(writer, "authorization_binding_mismatch")
			return
		}
	}
	remote, err := bridge.service.CommercialAuthorizeNewConversation(request.Context(), productauth.CommercialConversationRequest{
		RequestID: body.RequestID, ConversationID: body.ConversationID,
	})
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	if remote.Schema != productauth.CommercialSchemaV2 || remote.PolicyVersion != 2 || productauth.ValidateCommercialQuota(remote.Usage) != nil || remote.ConversationID != body.ConversationID {
		writeCommercialGatewayError(writer)
		return
	}
	if !remote.Allowed {
		writeJSON(writer, http.StatusOK, map[string]any{
			"schema": desktopMembershipSchema, "scope": "new_conversation", "request_id": body.RequestID,
			"conversation_id": body.ConversationID, "decision": "denied", "reason": remote.Reason,
			"policy_version": 2, "usage": remote.Usage,
		})
		return
	}
	expiresAt, err := bridge.authoritativeAccessExpiry(request.Context(), remote.ServerTime, principal)
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	authorizationID := ""
	if replay {
		authorizationID = existing.AuthorizationID
	} else {
		authorizationID, err = randomCommercialID("conversation")
		if err != nil {
			writeCommercialError(writer, err)
			return
		}
	}
	binding := &commercialBinding{
		PrincipalKey: principal, Scope: "new_conversation", RequestID: body.RequestID, AuthorizationID: authorizationID,
		ConversationID: body.ConversationID, ExpiresAt: expiresAt,
	}
	if err := bridge.store.putGrant(binding); err != nil {
		writeCommercialError(writer, err)
		return
	}
	bridge.writeGrant(writer, binding, "authorized", remote.Reason, remote.Usage)
}

func (bridge *commercialBridge) handleAuthorizePrompt(writer http.ResponseWriter, request *http.Request) {
	if bridge.promptGrants != nil {
		bridge.promptGrants.handleAuthorize(writer, request, "prompt")
		return
	}
	principal, _, err := bridge.principalAccess(request.Context())
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	var body struct {
		Schema         string `json:"schema"`
		RequestID      string `json:"request_id"`
		ConversationID string `json:"conversation_id"`
	}
	if decodeJSON(request, &body, true) != nil || body.Schema != desktopMembershipSchema ||
		!validCommercialID(body.RequestID) || !validCommercialID(body.ConversationID) {
		writeCommercialRequestError(writer)
		return
	}
	if _, ok := bridge.store.internalSession(body.ConversationID, principal); !ok {
		writeCommercialConflict(writer, "unknown_conversation")
		return
	}
	existing, replay := bridge.store.byRequest(body.RequestID)
	if replay {
		binding := existing
		if binding.PrincipalKey != principal || binding.Scope != "prompt" || binding.ConversationID != body.ConversationID {
			writeCommercialConflict(writer, "authorization_binding_mismatch")
			return
		}
	}
	remote, err := bridge.service.CommercialAuthorizePrompt(request.Context(), productauth.CommercialPromptRequest{
		RequestID: body.RequestID, ConversationID: body.ConversationID,
	})
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	if remote.Schema != productauth.CommercialSchemaV2 || remote.PolicyVersion != 2 || productauth.ValidateCommercialQuota(remote.Usage) != nil || remote.ConversationID != body.ConversationID {
		writeCommercialGatewayError(writer)
		return
	}
	if !remote.Allowed {
		writeJSON(writer, http.StatusOK, map[string]any{
			"schema": desktopMembershipSchema, "scope": "prompt", "request_id": body.RequestID,
			"conversation_id": body.ConversationID, "decision": "denied", "reason": remote.Source,
			"policy_version": 2, "usage": remote.Usage,
		})
		return
	}
	expiresAt, err := bridge.authoritativeAccessExpiry(request.Context(), remote.ServerTime, principal)
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	binding := &commercialBinding{
		PrincipalKey: principal, Scope: "prompt", RequestID: body.RequestID, AuthorizationID: remote.AuthorizationID,
		ConversationID: body.ConversationID, ExpiresAt: expiresAt,
	}
	if err := bridge.store.putGrant(binding); err != nil {
		writeCommercialError(writer, err)
		return
	}
	bridge.writeGrant(writer, binding, "authorized", remote.Source, remote.Usage)
}

func (bridge *commercialBridge) handleConsumePrompt(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Schema          string `json:"schema"`
		RequestID       string `json:"request_id"`
		AuthorizationID string `json:"authorization_id"`
	}
	if decodeJSON(request, &body, true) != nil || body.Schema != desktopMembershipSchema ||
		!validCommercialID(body.RequestID) || !validCommercialID(body.AuthorizationID) {
		writeCommercialRequestError(writer)
		return
	}
	principal, _, err := bridge.principalAccess(request.Context())
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	binding, ok := bridge.store.byAuthorization(body.AuthorizationID)
	if !ok || binding.PrincipalKey != principal || binding.Scope != "prompt" || binding.RequestID != body.RequestID || !binding.Accepted {
		writeCommercialConflict(writer, "consumption_binding_mismatch")
		return
	}
	if binding.PromptGrantID != "" && bridge.promptGrants != nil {
		bridge.kickAcceptedConsumptionSync(principal)
		status := "sync_pending"
		if binding.Consumed {
			status = "consumed"
		}
		projection := bridge.promptGrants.membershipProjectionForPrincipal(principal)
		if projection == nil {
			writeCommercialError(writer, promptGrantError("prompt_grant_usage_pending"))
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"schema": desktopMembershipSchema, "request_id": body.RequestID, "authorization_id": body.AuthorizationID, "status": status, "usage": projection})
		return
	}
	defer bridge.kickConsumptionSync(principal)
	remote, err := bridge.service.CommercialConsumePrompt(request.Context(), productauth.CommercialConsumeRequest{
		RequestID: body.RequestID, AuthorizationID: body.AuthorizationID,
	})
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	if !remote.Consumed || remote.AuthorizationID != body.AuthorizationID {
		writeCommercialGatewayError(writer)
		return
	}
	if err := bridge.store.markConsumed(body.AuthorizationID); err != nil {
		writeCommercialError(writer, err)
		return
	}
	usage, err := bridge.usageProjection(request.Context())
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	status := "consumed"
	if remote.Replay || binding.Consumed {
		status = "already_consumed"
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"schema": desktopMembershipSchema, "request_id": body.RequestID,
		"authorization_id": body.AuthorizationID, "status": status, "usage": usage,
	})
}

func (bridge *commercialBridge) handleReferralShare(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Schema    string `json:"schema"`
		RequestID string `json:"request_id"`
	}
	if decodeJSON(request, &body, true) != nil || body.Schema != desktopMembershipSchema || !validCommercialID(body.RequestID) {
		writeCommercialRequestError(writer)
		return
	}
	remote, err := bridge.service.CommercialReferralShare(request.Context(), productauth.CommercialReferralRequest{RequestID: body.RequestID})
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"schema": desktopMembershipSchema, "request_id": body.RequestID, "url": remote.Link, "qr_payload": remote.QRPayload})
}

func (bridge *commercialBridge) handleReferralAccept(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Schema    string `json:"schema"`
		RequestID string `json:"request_id"`
		Code      string `json:"code"`
	}
	if decodeJSON(request, &body, true) != nil || body.Schema != desktopMembershipSchema || !validCommercialID(body.RequestID) || !validCommercialID(body.Code) {
		writeCommercialRequestError(writer)
		return
	}
	remote, err := bridge.service.CommercialReferralAccept(request.Context(), productauth.CommercialReferralAcceptRequest{RequestID: body.RequestID, Code: body.Code})
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	if remote.RequestID != body.RequestID || !remote.Accepted {
		writeCommercialGatewayError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"schema": desktopMembershipSchema, "request_id": body.RequestID, "accepted": true, "status": remote.Status})
}

func (bridge *commercialBridge) handleReferralStatus(writer http.ResponseWriter, request *http.Request) {
	remote, err := bridge.service.CommercialReferralStatus(request.Context())
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"schema": desktopMembershipSchema, "share_created": remote.ShareCreated,
		"invitee_claim": remote.InviteeClaim, "invited": remote.Invited,
	})
}

func (bridge *commercialBridge) WrapSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/session" {
			bridge.handleSessionCreate(writer, request, next)
			return
		}
		if request.Method == http.MethodPost && request.URL.Path == "/session/resume" {
			bridge.handleSessionResume(writer, request, next)
			return
		}
		canonical, suffix, ok := canonicalSessionPath(request.URL.Path)
		if !ok {
			next.ServeHTTP(writer, request)
			return
		}
		// Cancellation cannot create work and remains available after logout or
		// credential expiry, including for canonical IDs returned by this bridge.
		if request.Method == http.MethodPost && suffix == "/cancel" {
			if internal, ok := bridge.store.cancellationSession(canonical); ok {
				internal = bridge.resumedInternal(canonical, "", internal)
				request.URL.Path = "/session/" + internal + suffix
			}
			next.ServeHTTP(writer, request)
			return
		}
		runtimeContinuation := bridge.runtimeAccountRoute(request)
		readPrincipal := bridge.principalAccess
		if runtimeContinuation {
			readPrincipal = bridge.runtimePrincipalAccess
		}
		principal, accessExpiresAt, err := readPrincipal(request.Context())
		if err != nil {
			writeCommercialError(writer, err)
			return
		}
		internal, mapped := bridge.internalSession(canonical, principal)
		if !mapped {
			// Account-only continuation authority never grants access to a raw
			// private session ID or another login session's conversation.
			if runtimeContinuation {
				writeCommercialConflict(writer, "unknown_conversation")
				return
			}
			if isCommercialDelivery(request.Method, suffix) || (request.Method == http.MethodPost && strings.HasPrefix(suffix, "/queue/") && strings.HasSuffix(suffix, "/guide")) {
				writeCommercialConflict(writer, "unknown_conversation")
				return
			}
			if suffix == "/harness-controls" || suffix == "/inference-controls" {
				// Direct daemon IDs and unknown mappings cannot inherit a proof
				// from downstream headers, even when their native read succeeds.
				buffer := newCommercialResponse()
				next.ServeHTTP(buffer, request)
				clearControlsProof(buffer.header)
				if buffer.err != nil {
					writeCommercialError(writer, buffer.err)
					return
				}
				buffer.commit(writer)
				return
			}
			next.ServeHTTP(writer, request)
			return
		}
		if isCommercialDelivery(request.Method, suffix) {
			grant, err := commercialGrantFromHeaders(request.Header)
			if err != nil || grant.ConversationID != canonical {
				writeCommercialConflict(writer, "authorization_binding_mismatch")
				return
			}
			promptRequestID := request.Header.Get(promptRequestIDHeader)
			if !validCommercialID(promptRequestID) {
				writeCommercialConflict(writer, "invalid_prompt_request_id")
				return
			}
			if bridge.promptGrants != nil {
				if err := bridge.promptGrants.bindDelivery(request, grant, promptRequestID); err != nil {
					writeCommercialError(writer, err)
					return
				}
			}
			if suffix != "/prompt" {
				bridge.handleImmediateDelivery(writer, request, next, grant, principal, internal, suffix, promptRequestID)
				return
			}
			binding, replay, err := bridge.store.beginDelivery(grant, promptRequestID, bridge.deliveryInstance, principal, suffix, "", bridge.grantDeliveryAdmission(request.Context(), grant))
			if err != nil {
				writeCommercialConflict(writer, "authorization_not_deliverable")
				return
			}
			request.URL.Path = "/session/" + internal + suffix
			if replay {
				// Only the first delivery may create a Core turn. Same-process
				// retries must retrieve an active/retained result, never recreate
				// one that Core has already evicted or pruned.
				request = request.WithContext(serve.WithPromptReplayOnly(request.Context(), promptRequestID))
			}
			buffer := newCommercialResponse()
			next.ServeHTTP(buffer, request)
			if buffer.err != nil {
				writeCommercialError(writer, buffer.err)
				return
			}
			if buffer.status >= 200 && buffer.status < 300 {
				if err := bridge.store.markAccepted(binding.AuthorizationID); err != nil {
					writeCommercialError(writer, err)
					return
				}
				bridge.kickAcceptedConsumptionSync(principal)
			} else if !replay {
				// A detached retry cannot release the original delivery's
				// ownership when its Core receipt is unavailable or mismatched.
				if err := bridge.store.resetDelivery(binding.AuthorizationID); err != nil {
					writeCommercialError(writer, err)
					return
				}
			}
			buffer.commit(writer)
			return
		}
		if request.Method == http.MethodPost && strings.HasPrefix(suffix, "/queue/") && strings.HasSuffix(suffix, "/guide") {
			parts := strings.Split(suffix, "/")
			if len(parts) != 4 || !bridge.store.hasAcceptedQueue(canonical, principal, parts[2]) {
				writeCommercialConflict(writer, "queue_authorization_required")
				return
			}
		}
		request.URL.Path = "/session/" + internal + suffix
		if request.Method == http.MethodGet && suffix == "/map-config" {
			buffer := newCommercialResponse()
			next.ServeHTTP(buffer, request)
			if buffer.err != nil {
				writeCommercialError(writer, buffer.err)
				return
			}
			if buffer.status >= 200 && buffer.status < 300 {
				currentInternal, stillMapped := bridge.internalSession(canonical, principal)
				if !stillMapped || currentInternal != internal {
					writeCommercialConflict(writer, "unknown_conversation")
					return
				}
				if !projectMapTileIdentity(buffer, internal, canonical) {
					writeCommercialGatewayError(writer)
					return
				}
				buffer.header.Del("Content-Length")
				buffer.header.Set("Cache-Control", "no-store")
			}
			buffer.commit(writer)
			return
		}
		if request.Method == http.MethodGet && suffix == "/location" {
			// Location preflight and hydration use the same public conversation
			// identity as the owning SSE stream. Only project records belonging
			// to this exact private session, including its current resumed ID.
			buffer := newCommercialResponse()
			next.ServeHTTP(buffer, request)
			if buffer.err != nil {
				writeCommercialError(writer, buffer.err)
				return
			}
			if buffer.status >= 200 && buffer.status < 300 {
				currentInternal, stillMapped := bridge.internalSession(canonical, principal)
				if !stillMapped || currentInternal != internal {
					writeCommercialConflict(writer, "unknown_conversation")
					return
				}
				if !projectLocationRequestIdentities(buffer, internal, canonical) {
					writeCommercialGatewayError(writer)
					return
				}
				buffer.header.Del("Content-Length")
				buffer.header.Set("Cache-Control", "no-store")
			}
			buffer.commit(writer)
			return
		}
		if request.Method == http.MethodGet && suffix == "/evo" {
			// Desktop owns the public conversation. A resumed native session
			// must not make its retained Evo state look like another chat's.
			buffer := newCommercialResponse()
			next.ServeHTTP(buffer, request)
			clearControlsProof(buffer.header)
			if buffer.err != nil {
				writeCommercialError(writer, buffer.err)
				return
			}
			if buffer.status >= 200 && buffer.status < 300 {
				currentInternal, stillMapped := bridge.internalSession(canonical, principal)
				if !stillMapped || currentInternal != internal {
					writeCommercialConflict(writer, "unknown_conversation")
					return
				}
				var value map[string]json.RawMessage
				if json.Unmarshal(buffer.body.Bytes(), &value) != nil || value == nil {
					writeCommercialGatewayError(writer)
					return
				}
				// A session without an Evo task legitimately returns {}.
				if len(value) != 0 {
					var sessionID string
					if json.Unmarshal(value["sessionId"], &sessionID) != nil || sessionID != internal {
						writeCommercialGatewayError(writer)
						return
					}
					value["sessionId"], _ = json.Marshal(canonical)
				}
				buffer.body.Reset()
				_ = json.NewEncoder(&buffer.body).Encode(value)
				buffer.header.Del("Content-Length")
				buffer.header.Set("Cache-Control", "no-store")
			}
			buffer.commit(writer)
			return
		}
		if suffix == "/harness-controls" || suffix == "/inference-controls" {
			// The public conversation is the client's ownership boundary. Do not
			// expose the private session ID in a controls read or update receipt.
			buffer := newCommercialResponse()
			next.ServeHTTP(buffer, request)
			clearControlsProof(buffer.header)
			if buffer.err != nil {
				writeCommercialError(writer, buffer.err)
				return
			}
			if buffer.status >= 200 && buffer.status < 300 {
				var value map[string]json.RawMessage
				var sessionID string
				if json.Unmarshal(buffer.body.Bytes(), &value) != nil || json.Unmarshal(value["sessionId"], &sessionID) != nil || sessionID != internal {
					writeCommercialGatewayError(writer)
					return
				}
				value["sessionId"], _ = json.Marshal(canonical)
				buffer.body.Reset()
				_ = json.NewEncoder(&buffer.body).Encode(value)
				buffer.header.Del("Content-Length")
				if bridge.accountAdmission && (request.Method == http.MethodGet || request.Method == http.MethodPost) {
					// This receipt represents this route's already-completed fresh
					// native admission, not a new post-completion account check.
					// Never reuse it for another request: prompt dispatch retains
					// its own fresh admission. This is not a provider readiness claim.
					// The bridge must match the exact canonical/principal binding
					// and future deadline before using it instead of another read.
					if !accessExpiresAt.After(bridge.now().UTC()) {
						writeCommercialError(writer, &productauth.Error{Code: "login_required", HTTPStatus: http.StatusUnauthorized})
						return
					}
					currentInternal, stillMapped := bridge.internalSession(canonical, principal)
					if !stillMapped || currentInternal != internal {
						writeCommercialConflict(writer, "unknown_conversation")
						return
					}
					buffer.header.Set(controlsProofSchemaHeader, controlsProofSchema)
					buffer.header.Set(controlsProofConversationHeader, canonical)
					buffer.header.Set(controlsProofPrincipalHeader, principal)
					buffer.header.Set(controlsProofExpiresHeader, accessExpiresAt.UTC().Format(time.RFC3339Nano))
					buffer.header.Set("Cache-Control", "no-store")
				}
			}
			buffer.commit(writer)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// Only rewrite the fixed tile route issued by this currently owned session.
// Never pass a foreign session, arbitrary host or extra query to the browser.
func projectMapTileIdentity(response *commercialResponse, internal, canonical string) bool {
	var value map[string]json.RawMessage
	var tileURL, revision string
	if json.Unmarshal(response.body.Bytes(), &value) != nil || value == nil ||
		json.Unmarshal(value["tileURL"], &tileURL) != nil {
		return false
	}
	if raw, ok := value["tileRevision"]; ok {
		if json.Unmarshal(raw, &revision) != nil {
			return false
		}
	}
	suffix := "/map-tiles/{z}/{x}/{y}"
	if revision != "" {
		if _, err := hex.DecodeString(revision); err != nil || len(revision) != 16 || strings.ToLower(revision) != revision {
			return false
		}
		suffix += "?v=" + revision
	}
	if tileURL != "/session/"+internal+suffix {
		return false
	}
	value["tileURL"], _ = json.Marshal("/session/" + canonical + suffix)
	response.body.Reset()
	return json.NewEncoder(&response.body).Encode(value) == nil
}

func projectLocationRequestIdentities(response *commercialResponse, internal, canonical string) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(response.body.Bytes(), &envelope) != nil || envelope == nil {
		return false
	}
	list := bytes.TrimSpace(envelope["requests"])
	var requests []map[string]json.RawMessage
	if len(list) == 0 || list[0] != '[' || json.Unmarshal(list, &requests) != nil {
		return false
	}
	for _, item := range requests {
		var sessionID, runID, requestID string
		if json.Unmarshal(item["sessionId"], &sessionID) != nil || sessionID != internal ||
			json.Unmarshal(item["runId"], &runID) != nil || strings.TrimSpace(runID) == "" ||
			json.Unmarshal(item["requestId"], &requestID) != nil || strings.TrimSpace(requestID) == "" {
			return false
		}
		item["sessionId"], _ = json.Marshal(canonical)
	}
	envelope["requests"], _ = json.Marshal(requests)
	response.body.Reset()
	return json.NewEncoder(&response.body).Encode(envelope) == nil
}

func clearControlsProof(header http.Header) {
	for name := range header {
		for _, owned := range []string{controlsProofSchemaHeader, controlsProofConversationHeader, controlsProofPrincipalHeader, controlsProofExpiresHeader} {
			if strings.EqualFold(name, owned) {
				delete(header, name)
				break
			}
		}
	}
}

// Queue and immediate guidance have no downstream transport replay cache. Keep
// a bounded receipt here and never forward an accepted operation twice. A
// receipt eviction or process restart fails closed via the persisted binding.
func (bridge *commercialBridge) handleImmediateDelivery(writer http.ResponseWriter, request *http.Request, next http.Handler, grant commercialGrant, principal, internal, suffix, promptRequestID string) {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxCommercialDeliveryBytes+1))
	if err != nil || len(body) > maxCommercialDeliveryBytes {
		writeCommercialRequestError(writer)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	digest := sha256.Sum256(body)
	fingerprint := fmt.Sprintf("%s:%s:%s:%s:%s:%x", principal, grant.RequestID, grant.ConversationID, suffix, promptRequestID, digest)
	binding, ok := bridge.store.byAuthorization(grant.AuthorizationID)
	if !ok || binding.PrincipalKey != principal || !sameCommercialGrant(binding, grant) || bridge.now().After(binding.ExpiresAt) {
		writeCommercialConflict(writer, "authorization_not_deliverable")
		return
	}
	bridge.receiptMu.Lock()
	if receipt := bridge.receipts[grant.AuthorizationID]; receipt != nil {
		bridge.receiptMu.Unlock()
		if receipt.fingerprint != fingerprint {
			writeCommercialConflict(writer, "authorization_binding_mismatch")
			return
		}
		select {
		case <-receipt.done:
			current, _, err := bridge.principalAccess(request.Context())
			if err != nil || current != principal {
				writeCommercialConflict(writer, "authorization_binding_mismatch")
				return
			}
			receipt.response.commit(writer)
		case <-request.Context().Done():
			writeCommercialConflict(writer, "delivery_pending")
		}
		return
	}
	// Reserve the maximum response size before execution. Completed receipts can
	// be evicted safely: the persisted Accepted/DeliveryStarted bit prevents a
	// cache miss from executing that grant again.
	for len(bridge.receipts) >= maxCommercialReceipts || bridge.receiptBytes+maxCommercialReplyBytes > maxCommercialReceiptBytes {
		evicted := false
		for id, receipt := range bridge.receipts {
			if receipt.response != nil {
				delete(bridge.receipts, id)
				bridge.receiptBytes -= receipt.bytes
				evicted = true
				break
			}
		}
		if !evicted {
			bridge.receiptMu.Unlock()
			writeCommercialConflict(writer, "delivery_capacity")
			return
		}
	}
	receipt := &commercialDeliveryReceipt{fingerprint: fingerprint, done: make(chan struct{}), bytes: maxCommercialReplyBytes}
	bridge.receipts[grant.AuthorizationID] = receipt
	bridge.receiptBytes += receipt.bytes
	bridge.receiptMu.Unlock()
	result := newCommercialResponse()
	defer func() {
		// Preserve an unknown execution outcome as failure while still letting
		// the outer server recover the panic. Never cache a fabricated 200.
		panicValue := recover()
		if panicValue != nil {
			result = newCommercialResponse()
			writeCommercialEnvelopeError(result, http.StatusInternalServerError, "delivery_outcome_unknown")
		}
		bridge.receiptMu.Lock()
		bridge.receiptBytes -= receipt.bytes
		receipt.response, receipt.bytes = result, result.body.Len()
		bridge.receiptBytes += receipt.bytes
		close(receipt.done)
		bridge.receiptMu.Unlock()
		result.commit(writer)
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	if _, _, err := bridge.store.beginDelivery(grant, promptRequestID, bridge.deliveryInstance, principal, suffix, fingerprint, bridge.grantDeliveryAdmission(request.Context(), grant)); err != nil {
		writeCommercialConflict(result, "authorization_not_deliverable")
		return
	}
	request.URL.Path = "/session/" + internal + suffix
	buffer := newCommercialResponse()
	next.ServeHTTP(buffer, request)
	if buffer.err != nil {
		writeCommercialError(result, buffer.err)
		return
	}
	if buffer.status >= 200 && buffer.status < 300 {
		queueID := ""
		if suffix == "/queue" {
			var reply struct {
				Item struct {
					ID string `json:"id"`
				} `json:"item"`
			}
			if json.Unmarshal(buffer.body.Bytes(), &reply) != nil || !validCommercialID(reply.Item.ID) {
				writeCommercialGatewayError(result)
				return
			}
			queueID = reply.Item.ID
		}
		if err := bridge.store.markAccepted(grant.AuthorizationID, queueID); err != nil {
			writeCommercialError(result, err)
			return
		}
		bridge.kickAcceptedConsumptionSync(principal)
	} else if err := bridge.store.resetDelivery(grant.AuthorizationID); err != nil {
		writeCommercialError(result, err)
		return
	}
	buffer.commit(result)
}

func (store *commercialBindingStore) hasAcceptedQueue(conversation, principal, queueID string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, binding := range store.bindings {
		if binding.PrincipalKey == principal && binding.ConversationID == conversation && binding.Scope == "prompt" &&
			binding.Accepted && binding.DeliveryPath == "/queue" && binding.QueueItemID == queueID && validCommercialID(queueID) {
			return true
		}
	}
	return false
}

func (bridge *commercialBridge) handleSessionCreate(writer http.ResponseWriter, request *http.Request, next http.Handler) {
	principal, _, err := bridge.principalAccess(request.Context())
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	grant, err := commercialGrantFromHeaders(request.Header)
	if err != nil {
		writeCommercialConflict(writer, "new_conversation_authorization_required")
		return
	}
	binding, ok := bridge.store.byAuthorization(grant.AuthorizationID)
	if !ok || binding.PrincipalKey != principal || binding.Scope != "new_conversation" || !sameCommercialGrant(binding, grant) || bridge.now().UTC().After(binding.ExpiresAt) {
		writeCommercialConflict(writer, "authorization_binding_mismatch")
		return
	}
	if binding.InternalSessionID != "" {
		writeJSON(writer, http.StatusOK, map[string]any{
			"sessionId": binding.ConversationID, "workspaceCwd": binding.WorkspaceCWD,
			"modelId": binding.ModelID, "connectionId": binding.ConnectionID, "approvalChannel": binding.ApprovalChannel, "attached": true,
		})
		return
	}
	buffer := newCommercialResponse()
	next.ServeHTTP(buffer, request)
	if buffer.err != nil {
		writeCommercialError(writer, buffer.err)
		return
	}
	if buffer.status < 200 || buffer.status >= 300 {
		buffer.commit(writer)
		return
	}
	var response struct {
		SessionID       string `json:"sessionId"`
		WorkspaceCWD    string `json:"workspaceCwd"`
		ModelID         string `json:"modelId"`
		ConnectionID    string `json:"connectionId"`
		ApprovalChannel string `json:"approvalChannel"`
		Attached        bool   `json:"attached"`
		CreatedAt       string `json:"createdAt,omitempty"`
	}
	if strictCommercialJSON(buffer.body.Bytes(), &response) != nil || !validCommercialID(response.SessionID) ||
		!validCommercialConnectionID(response.ConnectionID) ||
		(response.ApprovalChannel != "" && response.ApprovalChannel != "interactive" && response.ApprovalChannel != "none") {
		writeCommercialGatewayError(writer)
		return
	}
	if err := bridge.store.bindSession(binding.AuthorizationID, response.SessionID, response.WorkspaceCWD, response.ModelID, response.ConnectionID, response.ApprovalChannel); err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"sessionId": binding.ConversationID, "workspaceCwd": response.WorkspaceCWD,
		"modelId": response.ModelID, "connectionId": response.ConnectionID, "approvalChannel": response.ApprovalChannel, "attached": response.Attached, "createdAt": response.CreatedAt,
	})
}

func (bridge *commercialBridge) usageProjection(ctx context.Context) (map[string]any, error) {
	if bridge.promptGrants != nil {
		if local := bridge.promptGrants.membershipProjection(ctx); local != nil {
			return local, nil
		}
	}
	return bridge.remoteUsageProjection(ctx)
}

func (bridge *commercialBridge) remoteUsageProjection(ctx context.Context) (map[string]any, error) {
	// Independent status reads share the same bounded context; device inventory
	// must not add a second sequential network wait to usage reconciliation.
	var usage productauth.CommercialUsageStatus
	var devices productauth.DeviceListResult
	var usageErr, deviceErr error
	var reads sync.WaitGroup
	reads.Add(2)
	go func() { defer reads.Done(); usage, usageErr = bridge.service.CommercialUsageStatus(ctx) }()
	go func() {
		defer reads.Done()
		devices, deviceErr = bridge.service.ListDevices(ctx, productauth.DeviceListRequest{Schema: productauth.SchemaV1})
	}()
	reads.Wait()
	if usageErr != nil {
		return nil, usageErr
	}
	if deviceErr != nil {
		return nil, deviceErr
	}
	if productauth.ValidateCommercialUsage(usage) != nil || devices.Schema != productauth.SchemaV1 {
		return nil, errors.New("commercial projection source schema mismatch")
	}
	kind := "free_account"
	if usage.Lifetime {
		kind = "lifetime"
	}
	access := map[string]any{"kind": kind, "active": usage.Usage.CanSubmit, "trial_total_days": nil, "remaining_days": nil, "usage_fraction": nil, "started_at": nil, "expires_at": nil}
	return map[string]any{
		"schema": desktopMembershipSchema, "policy_version": 2, "usage": usage.Usage, "access": access,
		"legacy_prompt_pool": map[string]any{
			"remaining": usage.PostTrial.SharedPromptRemaining, "total": usage.PostTrial.SharedPromptAllowance,
			"new_conversation_allowed": usage.PostTrial.NewConversationAllowed,
		},
		"devices":                 map[string]any{"active": devices.ActiveDevices, "maximum": devices.MaximumDevices},
		"referral":                map[string]any{"url": nil, "earned_days": usage.Trial.ReferralBonusDays},
		"successful_prompt_count": usage.SuccessfulPromptCount,
		"observed_at":             usage.ServerTime.UTC().Format(time.RFC3339),
	}, nil
}

func (bridge *commercialBridge) principalAccess(ctx context.Context) (string, time.Time, error) {
	status, err := bridge.admissionStatus(ctx)
	return bridge.principalFromStatus(status, err)
}

func (bridge *commercialBridge) principalFromStatus(status productauth.Status, err error) (string, time.Time, error) {
	if bridge.accountAdmission {
		if accountErr := AccountAuthenticationError(status, err); accountErr != nil {
			return "", time.Time{}, accountErr
		}
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if status.Schema != productauth.SchemaV1 || !status.Authenticated || strings.TrimSpace(status.Subject) == "" ||
		status.AccessExpiresAt == nil || !status.AccessExpiresAt.After(bridge.now().UTC()) {
		return "", time.Time{}, errors.New("commercial authorization principal is unavailable")
	}
	// Subject is the signed authorization-session identity, not a claimed UID.
	// A fresh login may rotate it; old grants then deliberately fail closed.
	digest := sha256.Sum256([]byte(status.Subject))
	return fmt.Sprintf("%x", digest), status.AccessExpiresAt.UTC(), nil
}

func (bridge *commercialBridge) authoritativeAccessExpiry(ctx context.Context, serverTime time.Time, principal string) (time.Time, error) {
	current, expiry, err := bridge.principalAccess(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if current != principal || !expiry.After(serverTime) {
		return time.Time{}, errors.New("commercial authorization principal changed")
	}
	return expiry, nil
}

func (bridge *commercialBridge) writeGrant(writer http.ResponseWriter, binding *commercialBinding, decision, reason string, usage productauth.CommercialUsageQuota) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"schema": desktopMembershipSchema, "policy_version": 2, "usage": usage, "scope": binding.Scope, "request_id": binding.RequestID,
		"conversation_id": binding.ConversationID, "decision": decision, "reason": reason,
		"authorization_id": binding.AuthorizationID, "expires_at": binding.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (store *commercialBindingStore) load() error {
	if info, err := os.Lstat(store.path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("commercial membership bindings must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect commercial membership bindings: %w", err)
	}
	payload, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read commercial membership bindings: %w", err)
	}
	if len(payload) == 0 || len(payload) > maxCommercialStoreBytes {
		return errors.New("commercial membership bindings exceed fixed bounds")
	}
	var document commercialBindingDocument
	if strictCommercialJSON(payload, &document) != nil || document.Schema != commercialStoreSchema || document.Bindings == nil {
		return errors.New("commercial membership bindings are invalid")
	}
	for key, binding := range document.Bindings {
		if binding == nil || key != binding.AuthorizationID || !validBinding(binding) {
			return errors.New("commercial membership binding is invalid")
		}
	}
	store.bindings = document.Bindings
	store.grants = document.PromptGrants
	if store.grants == nil {
		store.grants = make(map[string]*durablePromptGrant)
	}
	for id, grant := range store.grants {
		if grant == nil || id != grant.GrantID || !validCommercialID(id) || grant.Capacity < 1 || grant.Capacity > 5 || grant.ExpiresAt.IsZero() || grant.PrincipalKey == "" {
			return errors.New("commercial prompt reservation is invalid")
		}
	}
	if err := store.validatePromptGrantBindings(); err != nil {
		return err
	}
	return nil
}

func (store *commercialBindingStore) saveLocked() error {
	payload, err := json.Marshal(commercialBindingDocument{Schema: commercialStoreSchema, Bindings: store.bindings, PromptGrants: store.grants})
	if err != nil || len(payload) > maxCommercialStoreBytes {
		return errors.New("commercial membership bindings cannot be encoded")
	}
	temporary, err := os.CreateTemp(filepath.Dir(store.path), ".commercial-bindings-*")
	if err != nil {
		return fmt.Errorf("create commercial membership binding transaction: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	err = temporary.Chmod(0o600)
	if err == nil {
		_, err = temporary.Write(payload)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporaryPath, store.path)
	}
	if err != nil {
		return fmt.Errorf("commit commercial membership bindings: %w", err)
	}
	return nil
}

func (store *commercialBindingStore) put(binding *commercialBinding) error {
	if !validBinding(binding) {
		return errors.New("commercial membership binding is invalid")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if existing := store.bindings[binding.AuthorizationID]; existing != nil {
		if sameBinding(existing, binding) {
			return nil
		}
		return errors.New("commercial authorization identity collision")
	}
	for _, existing := range store.bindings {
		if existing.RequestID == binding.RequestID {
			if sameBinding(existing, binding) {
				return nil
			}
			return errors.New("commercial request identity collision")
		}
	}
	copy := *binding
	store.bindings[copy.AuthorizationID] = &copy
	if err := store.saveLocked(); err != nil {
		delete(store.bindings, copy.AuthorizationID)
		return err
	}
	return nil
}

// A replay is revalidated by the remote authority before this call. Refresh
// only its access expiry; delivery/acceptance receipts must survive unchanged.
func (store *commercialBindingStore) putGrant(binding *commercialBinding) error {
	if !validBinding(binding) || binding.PrincipalKey == "" {
		return errors.New("commercial grant principal is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, existing := range store.bindings {
		if existing.RequestID != binding.RequestID && existing.AuthorizationID != binding.AuthorizationID {
			continue
		}
		if existing.PrincipalKey != binding.PrincipalKey || existing.Scope != binding.Scope ||
			existing.RequestID != binding.RequestID || existing.AuthorizationID != binding.AuthorizationID || existing.ConversationID != binding.ConversationID {
			return errors.New("commercial authorization identity collision")
		}
		previous := existing.ExpiresAt
		existing.ExpiresAt = binding.ExpiresAt
		if err := store.saveLocked(); err != nil {
			existing.ExpiresAt = previous
			return err
		}
		return nil
	}
	copy := *binding
	store.bindings[copy.AuthorizationID] = &copy
	if err := store.saveLocked(); err != nil {
		delete(store.bindings, copy.AuthorizationID)
		return err
	}
	return nil
}

func (store *commercialBindingStore) byRequest(requestID string) (*commercialBinding, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, binding := range store.bindings {
		if binding.RequestID == requestID {
			copy := *binding
			return &copy, true
		}
	}
	return nil, false
}

func (store *commercialBindingStore) byAuthorization(authorizationID string) (*commercialBinding, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	binding := store.bindings[authorizationID]
	if binding == nil {
		return nil, false
	}
	copy := *binding
	return &copy, true
}

func (store *commercialBindingStore) internalSession(conversationID, principal string) (string, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, binding := range store.bindings {
		if binding.PrincipalKey == principal && principal != "" && binding.Scope == "new_conversation" && binding.ConversationID == conversationID && binding.InternalSessionID != "" {
			return binding.InternalSessionID, true
		}
	}
	return "", false
}

func (store *commercialBindingStore) bindSession(authorizationID, internalID, cwd, modelID, connectionID, approvalChannel string) error {
	if !validCommercialID(internalID) {
		return errors.New("internal session identity is invalid")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	binding := store.bindings[authorizationID]
	if binding == nil || binding.Scope != "new_conversation" {
		return errors.New("new conversation authorization is unavailable")
	}
	if binding.InternalSessionID != "" {
		if binding.InternalSessionID == internalID {
			return nil
		}
		return errors.New("conversation is already bound to another session")
	}
	for _, existing := range store.bindings {
		if existing.InternalSessionID == internalID {
			return errors.New("internal session is already bound")
		}
	}
	previous := *binding
	binding.InternalSessionID, binding.WorkspaceCWD, binding.ModelID = internalID, cwd, modelID
	binding.ConnectionID, binding.ApprovalChannel = connectionID, approvalChannel
	if err := store.saveLocked(); err != nil {
		*binding = previous
		return err
	}
	return nil
}

func (store *commercialBindingStore) beginDelivery(grant commercialGrant, promptRequestID, deliveryInstance, principal, path, fingerprint string, admission ...func() error) (*commercialBinding, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	binding := store.bindings[grant.AuthorizationID]
	if binding == nil || binding.Released || principal == "" || binding.PrincipalKey != principal || binding.Scope != "prompt" || !sameCommercialGrant(binding, grant) || time.Now().UTC().After(binding.ExpiresAt) {
		return nil, false, errors.New("prompt authorization is not deliverable")
	}

	if binding.PromptGrantID != "" {
		reservation := store.grants[binding.PromptGrantID]
		if reservation == nil || reservation.Sealed || reservation.Closed {
			return nil, false, errors.New("prompt reservation is closed")
		}
		if len(admission) != 1 || admission[0] == nil {
			return nil, false, errors.New("prompt reservation admission is unavailable")
		}
		if err := admission[0](); err != nil {
			return nil, false, err
		}
	}
	if binding.DeliveryPath != "" && (binding.DeliveryPath != path || binding.DeliveryFingerprint != fingerprint) {
		return nil, false, errors.New("prompt delivery payload identity changed")
	}
	if binding.DeliveryStarted || binding.Accepted {
		// Only the exact same Desktop transport request may re-enter an active
		// or accepted delivery. The process instance binding deliberately makes
		// a daemon restart fail closed: Core's in-memory prompt result cannot be
		// assumed to survive that restart, so forwarding could execute twice.
		if path != "/prompt" || binding.DeliveryPath != path || promptRequestID == "" || binding.PromptRequestID != promptRequestID ||
			binding.DeliveryInstance == "" || binding.DeliveryInstance != deliveryInstance {
			return nil, false, errors.New("prompt authorization is not deliverable")
		}
		copy := *binding
		return &copy, true, nil
	}
	// Settlement can finish before the response reaches the browser. The exact
	// same-process replay above is constrained to Core's retained result; a
	// missing result fails closed. A consumed grant never begins a new delivery.
	if binding.Consumed {
		return nil, false, errors.New("prompt authorization is not deliverable")
	}
	if binding.PromptRequestID != "" &&
		(binding.PromptRequestID != promptRequestID || binding.DeliveryInstance != deliveryInstance) {
		return nil, false, errors.New("prompt authorization is not deliverable")
	}
	previous := *binding
	binding.DeliveryStarted = true
	binding.DeliveryPath, binding.DeliveryFingerprint = path, fingerprint
	if promptRequestID != "" {
		binding.PromptRequestID = promptRequestID
		binding.DeliveryInstance = deliveryInstance
	}
	if err := store.saveLocked(); err != nil {
		*binding = previous
		return nil, false, err
	}
	copy := *binding
	return &copy, false, nil
}

func (store *commercialBindingStore) markAccepted(authorizationID string, queueIDs ...string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	binding := store.bindings[authorizationID]
	if binding != nil && binding.Accepted {
		return nil
	}
	if binding == nil || !binding.DeliveryStarted {
		return errors.New("prompt acceptance state is invalid")
	}
	previous := *binding
	binding.Accepted = true
	if len(queueIDs) != 0 {
		binding.QueueItemID = queueIDs[0]
	}
	if err := store.saveLocked(); err != nil {
		*binding = previous
		return err
	}
	return nil
}

func (store *commercialBindingStore) resetDelivery(authorizationID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	binding := store.bindings[authorizationID]
	if binding != nil && binding.Accepted {
		return nil
	}
	if binding == nil {
		return errors.New("prompt delivery state is invalid")
	}
	previous := *binding
	binding.DeliveryStarted = false
	if binding.PromptGrantID != "" {
		binding.Released = true
	}
	if err := store.saveLocked(); err != nil {
		*binding = previous
		return err
	}
	return nil
}

func (store *commercialBindingStore) markConsumed(authorizationID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	binding := store.bindings[authorizationID]
	if binding == nil || !binding.Accepted {
		return errors.New("prompt consumption state is invalid")
	}
	if binding.Consumed {
		return nil
	}
	binding.Consumed = true
	if err := store.saveLocked(); err != nil {
		binding.Consumed = false
		return err
	}
	return nil
}

type commercialGrant struct{ RequestID, AuthorizationID, ConversationID string }

func commercialGrantFromHeaders(header http.Header) (commercialGrant, error) {
	grant := commercialGrant{
		RequestID:       strings.TrimSpace(header.Get(membershipRequestHeader)),
		AuthorizationID: strings.TrimSpace(header.Get(membershipAuthorizationHeader)),
		ConversationID:  strings.TrimSpace(header.Get(membershipConversationHeader)),
	}
	if header.Get(membershipSchemaHeader) != desktopMembershipSchema || !validCommercialID(grant.RequestID) ||
		!validCommercialID(grant.AuthorizationID) || !validCommercialID(grant.ConversationID) {
		return commercialGrant{}, errors.New("membership grant headers are invalid")
	}
	return grant, nil
}

func canonicalSessionPath(path string) (string, string, bool) {
	if !strings.HasPrefix(path, "/session/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, "/session/")
	index := strings.IndexByte(rest, '/')
	if index < 1 {
		return "", "", false
	}
	identifier := rest[:index]
	if identifier == "resume" || !validCommercialID(identifier) {
		return "", "", false
	}
	return identifier, rest[index:], true
}

func isCommercialDelivery(method, suffix string) bool {
	return method == http.MethodPost && (suffix == "/prompt" || suffix == "/queue" || suffix == "/steer")
}

func validBinding(binding *commercialBinding) bool {
	if binding == nil || (binding.Scope != "new_conversation" && binding.Scope != "prompt") ||
		!validCommercialID(binding.RequestID) || !validCommercialID(binding.AuthorizationID) ||
		!validCommercialID(binding.ConversationID) || binding.ExpiresAt.IsZero() {
		return false
	}
	if (binding.PromptRequestID == "") != (binding.DeliveryInstance == "") {
		return false
	}
	return binding.PromptRequestID == "" ||
		(validCommercialID(binding.PromptRequestID) && validCommercialID(binding.DeliveryInstance))
}

func sameBinding(left, right *commercialBinding) bool {
	return left.PrincipalKey == right.PrincipalKey && left.Scope == right.Scope && left.RequestID == right.RequestID && left.AuthorizationID == right.AuthorizationID &&
		left.ConversationID == right.ConversationID && left.ExpiresAt.Equal(right.ExpiresAt)
}

func sameCommercialGrant(binding *commercialBinding, grant commercialGrant) bool {
	return binding.RequestID == grant.RequestID && binding.AuthorizationID == grant.AuthorizationID && binding.ConversationID == grant.ConversationID
}

func validCommercialID(value string) bool {
	if value == "" || len(value) > 160 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func randomCommercialID(prefix string) (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate commercial authorization identity: %w", err)
	}
	return prefix + "-" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

type commercialResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	err    error
}

func newCommercialResponse() *commercialResponse {
	return &commercialResponse{header: make(http.Header), status: http.StatusOK}
}
func (response *commercialResponse) Header() http.Header { return response.header }
func (response *commercialResponse) WriteHeader(status int) {
	if response.status == http.StatusOK {
		response.status = status
	}
}
func (response *commercialResponse) Write(payload []byte) (int, error) {
	if response.body.Len()+len(payload) > maxCommercialReplyBytes {
		response.err = errors.New("commercially gated daemon response exceeds fixed bound")
		return 0, response.err
	}
	return response.body.Write(payload)
}
func (response *commercialResponse) commit(writer http.ResponseWriter) {
	for name, values := range response.header {
		writer.Header()[name] = append([]string(nil), values...)
	}
	writer.WriteHeader(response.status)
	_, _ = writer.Write(response.body.Bytes())
}

func strictCommercialJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func writeCommercialRequestError(writer http.ResponseWriter) {
	writeCommercialEnvelopeError(writer, http.StatusBadRequest, "invalid_request")
}
func writeCommercialConflict(writer http.ResponseWriter, code string) {
	writeCommercialEnvelopeError(writer, http.StatusConflict, code)
}
func writeCommercialGatewayError(writer http.ResponseWriter) {
	writeCommercialEnvelopeError(writer, http.StatusBadGateway, "invalid_server_response")
}
func writeCommercialError(writer http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, productauth.ErrorCode(err)
	var authErr *productauth.Error
	if errors.As(err, &authErr) && authErr.HTTPStatus >= 400 && authErr.HTTPStatus <= 599 {
		status = authErr.HTTPStatus
	}
	writeCommercialEnvelopeError(writer, status, code)
}
func writeCommercialEnvelopeError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, map[string]any{"schema": desktopMembershipSchema, "error": map[string]any{"code": code, "message": "membership authorization failed"}})
}

func (store *commercialBindingStore) cancellationSession(conversationID string) (string, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, binding := range store.bindings {
		if binding.Scope == "new_conversation" && binding.ConversationID == conversationID && binding.InternalSessionID != "" {
			return binding.InternalSessionID, true
		}
	}
	return "", false
}

func validCommercialConnectionID(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, ch := range value {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-' {
			continue
		}
		return false
	}
	return true
}
