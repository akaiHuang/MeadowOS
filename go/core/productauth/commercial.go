package productauth

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	CommercialSchemaV2 = "meadow.commercial-access.v2"

	commercialUsagePath             = "/commercialUsageStatus"
	commercialConversationPath      = "/commercialAuthorizeNewConversation"
	commercialPromptPath            = "/commercialAuthorizePrompt"
	commercialConsumePath           = "/commercialConsumePrompt"
	commercialReferralSharePath     = "/commercialReferralShare"
	commercialReferralAcceptPath    = "/commercialReferralAccept"
	commercialReferralStatusPath    = "/commercialReferralStatus"
	commercialSharedPromptAllowance = 5
	commercialAcceptedPromptLimit   = 100
)

type CommercialTrialStatus struct {
	Legacy            bool      `json:"legacy"`
	Active            bool      `json:"active"`
	StartedAt         time.Time `json:"started_at"`
	EndsAt            time.Time `json:"ends_at"`
	ReferralBonusDays int       `json:"referral_bonus_days"`
}

type CommercialPostTrialStatus struct {
	Legacy                 bool `json:"legacy"`
	SharedPromptAllowance  int  `json:"shared_prompt_allowance"`
	SharedPromptRemaining  int  `json:"shared_prompt_remaining"`
	NewConversationAllowed bool `json:"new_conversation_allowed"`
}

type CommercialUsageStatus struct {
	Schema                string                    `json:"schema"`
	PolicyVersion         int                       `json:"policy_version"`
	Usage                 CommercialUsageQuota      `json:"usage"`
	ServerTime            time.Time                 `json:"server_time"`
	Lifetime              bool                      `json:"lifetime"`
	Trial                 CommercialTrialStatus     `json:"trial"`
	PostTrial             CommercialPostTrialStatus `json:"post_trial"`
	SuccessfulPromptCount int                       `json:"successful_prompt_count"`
}

// CommercialUsageQuota counts server-accepted user submissions, independently
// of model/tool requests and the historical successful-prompt audit counter.
type CommercialUsageQuota struct {
	Unit                string `json:"unit"`
	Limit               *int   `json:"limit"`
	AcceptedPromptCount int    `json:"accepted_prompt_count"`
	Remaining           *int   `json:"remaining"`
	Unlimited           bool   `json:"unlimited"`
	CanSubmit           bool   `json:"can_submit"`
}

func (value *CommercialUsageQuota) UnmarshalJSON(data []byte) error {
	type plain CommercialUsageQuota
	var decoded plain
	if err := strictJSON(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"unit", "limit", "accepted_prompt_count", "remaining", "unlimited", "can_submit"} {
		raw, ok := fields[name]
		if !ok || (name != "limit" && name != "remaining" && string(raw) == "null") {
			return errors.New("commercial usage quota field is required")
		}
	}
	*value = CommercialUsageQuota(decoded)
	return nil
}

type CommercialAuthorization struct {
	Schema          string               `json:"schema"`
	PolicyVersion   int                  `json:"policy_version"`
	Usage           CommercialUsageQuota `json:"usage"`
	Allowed         bool                 `json:"allowed"`
	AuthorizationID string               `json:"authorization_id,omitempty"`
	ConversationID  string               `json:"conversation_id"`
	Reason          string               `json:"reason,omitempty"`
	Source          string               `json:"source,omitempty"`
	SharedRemaining int                  `json:"shared_prompt_remaining,omitempty"`
	ServerTime      time.Time            `json:"server_time"`
}

type CommercialConsumeReceipt struct {
	Schema                string               `json:"schema"`
	PolicyVersion         int                  `json:"policy_version"`
	Usage                 CommercialUsageQuota `json:"usage"`
	Consumed              bool                 `json:"consumed"`
	Replay                bool                 `json:"replay"`
	AuthorizationID       string               `json:"authorization_id"`
	SuccessfulPromptCount int                  `json:"successful_prompt_count"`
	ReferralStatus        string               `json:"referral_status"`
	ReferralRewarded      bool                 `json:"referral_rewarded"`
	ServerTime            time.Time            `json:"server_time"`
}

type CommercialReferralShare struct {
	Schema    string `json:"schema"`
	Code      string `json:"code"`
	Link      string `json:"link"`
	QRPayload string `json:"qr_payload"`
}

type CommercialReferralClaim struct {
	Status                string     `json:"status"`
	AcceptedAt            *time.Time `json:"accepted_at"`
	ActivatedAt           *time.Time `json:"activated_at"`
	RewardedAt            *time.Time `json:"rewarded_at"`
	SuccessfulPromptCount int        `json:"successful_prompt_count"`
}

type CommercialReferralCounts struct {
	Pending  int `json:"pending"`
	Rewarded int `json:"rewarded"`
	Rejected int `json:"rejected"`
}

type CommercialReferralStatus struct {
	Schema       string                   `json:"schema"`
	ShareCreated bool                     `json:"share_created"`
	InviteeClaim *CommercialReferralClaim `json:"invitee_claim"`
	Invited      CommercialReferralCounts `json:"invited"`
}

type CommercialReferralAcceptReceipt struct {
	Schema    string `json:"schema"`
	Accepted  bool   `json:"accepted"`
	Status    string `json:"status"`
	RequestID string `json:"request_id"`
}

type commercialRemoteRequest struct {
	AccessToken     string
	RequestID       string
	InstallationID  string
	DeviceProof     string
	ConversationID  string
	AuthorizationID string
	Code            string
}

type CommercialAccessClient interface {
	CommercialUsageStatus(context.Context, commercialRemoteRequest) (CommercialUsageStatus, error)
	CommercialAuthorizeNewConversation(context.Context, commercialRemoteRequest) (CommercialAuthorization, error)
	CommercialAuthorizePrompt(context.Context, commercialRemoteRequest) (CommercialAuthorization, error)
	CommercialConsumePrompt(context.Context, commercialRemoteRequest) (CommercialConsumeReceipt, error)
	CommercialReferralShare(context.Context, commercialRemoteRequest) (CommercialReferralShare, error)
	CommercialReferralAccept(context.Context, commercialRemoteRequest) (CommercialReferralAcceptReceipt, error)
	CommercialReferralStatus(context.Context, commercialRemoteRequest) (CommercialReferralStatus, error)
}

type CommercialConversationRequest struct {
	RequestID      string
	ConversationID string
}

type CommercialPromptRequest struct {
	RequestID      string
	ConversationID string
}

type CommercialConsumeRequest struct {
	RequestID       string
	AuthorizationID string
}

type CommercialReferralRequest struct{ RequestID string }

type CommercialReferralAcceptRequest struct {
	RequestID string
	Code      string
}

func (client *FixedClient) CommercialUsageStatus(ctx context.Context, input commercialRemoteRequest) (CommercialUsageStatus, error) {
	var response CommercialUsageStatus
	err := client.commercialCall(ctx, commercialUsagePath, "usage_status", "", input, nil, &response)
	if err != nil {
		return CommercialUsageStatus{}, err
	}
	if err := validateCommercialUsage(response); err != nil {
		return CommercialUsageStatus{}, invalidCommercialResponse(err)
	}
	return response, nil
}

func (client *FixedClient) CommercialAuthorizeNewConversation(ctx context.Context, input commercialRemoteRequest) (CommercialAuthorization, error) {
	var response CommercialAuthorization
	err := client.commercialCall(ctx, commercialConversationPath, "conversation_authorize", input.ConversationID, input,
		map[string]any{"conversation_id": input.ConversationID}, &response)
	if err != nil {
		return CommercialAuthorization{}, err
	}
	if response.Schema != CommercialSchemaV2 || response.PolicyVersion != 2 || validateCommercialQuota(response.Usage, response.Usage.Unlimited) != nil ||
		response.ConversationID != input.ConversationID || response.ServerTime.IsZero() ||
		!validCommercialReason(response.Reason) || response.Allowed == (response.Reason == "free_prompt_limit_reached") || response.Source != "" || response.AuthorizationID != "" {
		return CommercialAuthorization{}, invalidCommercialResponse(errors.New("conversation authorization is invalid"))
	}
	return response, nil
}

func (client *FixedClient) CommercialAuthorizePrompt(ctx context.Context, input commercialRemoteRequest) (CommercialAuthorization, error) {
	var response CommercialAuthorization
	err := client.commercialCall(ctx, commercialPromptPath, "prompt_authorize", input.ConversationID, input,
		map[string]any{"conversation_id": input.ConversationID}, &response)
	if err != nil {
		return CommercialAuthorization{}, err
	}
	validSource := response.Source == "lifetime" || response.Source == "free_account" || response.Source == "blocked"
	validReason := (response.Allowed && response.Reason == "accepted_user_prompt") || (!response.Allowed && response.Reason == "free_prompt_limit_reached")
	if response.Schema != CommercialSchemaV2 || response.PolicyVersion != 2 || validateCommercialQuota(response.Usage, response.Usage.Unlimited) != nil ||
		response.ConversationID != input.ConversationID || response.ServerTime.IsZero() ||
		!validSource || response.Allowed == (response.Source == "blocked") || !validReason || response.SharedRemaining < 0 || response.SharedRemaining > commercialSharedPromptAllowance ||
		!validOpaque(response.AuthorizationID, 160) {
		return CommercialAuthorization{}, invalidCommercialResponse(errors.New("prompt authorization is invalid"))
	}
	return response, nil
}

func (client *FixedClient) CommercialConsumePrompt(ctx context.Context, input commercialRemoteRequest) (CommercialConsumeReceipt, error) {
	var response CommercialConsumeReceipt
	err := client.commercialCall(ctx, commercialConsumePath, "prompt_consume", input.AuthorizationID, input,
		map[string]any{"authorization_id": input.AuthorizationID}, &response)
	if err != nil {
		return CommercialConsumeReceipt{}, err
	}
	if response.Schema != CommercialSchemaV2 || response.PolicyVersion != 2 || validateCommercialQuota(response.Usage, response.Usage.Unlimited) != nil ||
		!response.Consumed || response.AuthorizationID != input.AuthorizationID ||
		response.SuccessfulPromptCount < 0 || response.ServerTime.IsZero() || !validCommercialReferralSettlement(response.ReferralStatus) {
		return CommercialConsumeReceipt{}, invalidCommercialResponse(errors.New("prompt consumption receipt is invalid"))
	}
	return response, nil
}

func (client *FixedClient) CommercialReferralShare(ctx context.Context, input commercialRemoteRequest) (CommercialReferralShare, error) {
	var response CommercialReferralShare
	if err := client.commercialCall(ctx, commercialReferralSharePath, "referral_share", "", input, nil, &response); err != nil {
		return CommercialReferralShare{}, err
	}
	if response.Schema != CommercialSchemaV2 || !validOpaque(response.Code, 160) || !validHTTPS(response.Link) ||
		response.QRPayload != response.Link {
		return CommercialReferralShare{}, invalidCommercialResponse(errors.New("referral share response is invalid"))
	}
	return response, nil
}

func (client *FixedClient) CommercialReferralAccept(ctx context.Context, input commercialRemoteRequest) (CommercialReferralAcceptReceipt, error) {
	var response CommercialReferralAcceptReceipt
	if err := client.commercialCall(ctx, commercialReferralAcceptPath, "referral_accept", input.Code, input,
		map[string]any{"code": input.Code}, &response); err != nil {
		return CommercialReferralAcceptReceipt{}, err
	}
	if response.Schema != CommercialSchemaV2 || !response.Accepted || response.Status != "pending" || response.RequestID != input.RequestID {
		return CommercialReferralAcceptReceipt{}, invalidCommercialResponse(errors.New("referral accept receipt is invalid"))
	}
	return response, nil
}

func (client *FixedClient) CommercialReferralStatus(ctx context.Context, input commercialRemoteRequest) (CommercialReferralStatus, error) {
	var response CommercialReferralStatus
	if err := client.commercialCall(ctx, commercialReferralStatusPath, "referral_status", "", input, nil, &response); err != nil {
		return CommercialReferralStatus{}, err
	}
	if err := validateCommercialReferralStatus(response); err != nil {
		return CommercialReferralStatus{}, invalidCommercialResponse(err)
	}
	return response, nil
}

func (client *FixedClient) commercialCall(ctx context.Context, path, action, binding string, input commercialRemoteRequest, fields map[string]any, output any) error {
	if !validCommercialAction(path, action) || !validOpaqueOrEmpty(binding, 256) ||
		!validCredential(input.AccessToken, 32, 128) || !validOpaque(input.RequestID, 160) ||
		!validInstallationID(input.InstallationID) || !validCanonicalBase64URL(input.DeviceProof, ed25519.SignatureSize) {
		return authError("invalid_request", "commercial request is invalid", http.StatusBadRequest, false, nil)
	}
	body := map[string]any{
		"schema": SchemaV1, "request_id": input.RequestID, "installation_id": input.InstallationID,
		"device_proof": input.DeviceProof,
	}
	for key, value := range fields {
		body[key] = value
	}
	return client.doCommercial(ctx, path, input.AccessToken, body, map[string]string{"X-Request-ID": input.RequestID}, output)
}

func validCommercialAction(path, action string) bool {
	expected := map[string]string{
		commercialUsagePath:          "usage_status",
		commercialConversationPath:   "conversation_authorize",
		commercialPromptPath:         "prompt_authorize",
		commercialConsumePath:        "prompt_consume",
		commercialReferralSharePath:  "referral_share",
		commercialReferralAcceptPath: "referral_accept",
		commercialReferralStatusPath: "referral_status",
	}
	return expected[path] == action
}

func (service *Service) commercialClient() (CommercialAccessClient, error) {
	client, ok := service.client.(CommercialAccessClient)
	if !ok || client == nil {
		return nil, authError("commercial_unavailable", "commercial authorization is unavailable", http.StatusServiceUnavailable, false, nil)
	}
	return client, nil
}

func (service *Service) commercialRemote(ctx context.Context, requestID, action, binding string) (CommercialAccessClient, commercialRemoteRequest, error) {
	if !validOpaque(requestID, 160) || !validOpaqueOrEmpty(binding, 256) {
		return nil, commercialRemoteRequest{}, authError("invalid_request", "commercial binding is invalid", http.StatusBadRequest, false, nil)
	}
	client, err := service.commercialClient()
	if err != nil {
		return nil, commercialRemoteRequest{}, err
	}
	access, err := service.currentAccess(ctx)
	if err != nil {
		return nil, commercialRemoteRequest{}, err
	}
	privateKey, _, err := service.deviceProofKey(ctx)
	if err != nil {
		return nil, commercialRemoteRequest{}, err
	}
	defer clear(privateKey)
	return client, commercialRemoteRequest{
		AccessToken: access.token, RequestID: requestID, InstallationID: service.installationID,
		DeviceProof: signBoundDeviceProof(privateKey, action, access.token, service.installationID, requestID, "", binding),
	}, nil
}

func (service *Service) CommercialUsageStatus(ctx context.Context) (CommercialUsageStatus, error) {
	requestID, err := service.randomRequestID()
	if err != nil {
		return CommercialUsageStatus{}, authError("random_unavailable", "commercial request identity is unavailable", http.StatusServiceUnavailable, false, err)
	}
	client, remote, err := service.commercialRemote(ctx, requestID, "usage_status", "")
	if err != nil {
		return CommercialUsageStatus{}, err
	}
	return client.CommercialUsageStatus(ctx, remote)
}

func (service *Service) CommercialAuthorizeNewConversation(ctx context.Context, request CommercialConversationRequest) (CommercialAuthorization, error) {
	if !validOpaque(request.ConversationID, 160) {
		return CommercialAuthorization{}, authError("invalid_request", "conversation identity is invalid", http.StatusBadRequest, false, nil)
	}
	client, remote, err := service.commercialRemote(ctx, request.RequestID, "conversation_authorize", request.ConversationID)
	if err != nil {
		return CommercialAuthorization{}, err
	}
	remote.ConversationID = request.ConversationID
	return client.CommercialAuthorizeNewConversation(ctx, remote)
}

func (service *Service) CommercialAuthorizePrompt(ctx context.Context, request CommercialPromptRequest) (CommercialAuthorization, error) {
	if !validOpaque(request.ConversationID, 160) {
		return CommercialAuthorization{}, authError("invalid_request", "conversation identity is invalid", http.StatusBadRequest, false, nil)
	}
	client, remote, err := service.commercialRemote(ctx, request.RequestID, "prompt_authorize", request.ConversationID)
	if err != nil {
		return CommercialAuthorization{}, err
	}
	remote.ConversationID = request.ConversationID
	return client.CommercialAuthorizePrompt(ctx, remote)
}

func (service *Service) CommercialConsumePrompt(ctx context.Context, request CommercialConsumeRequest) (CommercialConsumeReceipt, error) {
	if !validOpaque(request.AuthorizationID, 160) {
		return CommercialConsumeReceipt{}, authError("invalid_request", "authorization identity is invalid", http.StatusBadRequest, false, nil)
	}
	client, remote, err := service.commercialRemote(ctx, request.RequestID, "prompt_consume", request.AuthorizationID)
	if err != nil {
		return CommercialConsumeReceipt{}, err
	}
	remote.AuthorizationID = request.AuthorizationID
	return client.CommercialConsumePrompt(ctx, remote)
}

func (service *Service) CommercialReferralShare(ctx context.Context, request CommercialReferralRequest) (CommercialReferralShare, error) {
	client, remote, err := service.commercialRemote(ctx, request.RequestID, "referral_share", "")
	if err != nil {
		return CommercialReferralShare{}, err
	}
	return client.CommercialReferralShare(ctx, remote)
}

func (service *Service) CommercialReferralAccept(ctx context.Context, request CommercialReferralAcceptRequest) (CommercialReferralAcceptReceipt, error) {
	if !validOpaque(request.Code, 160) {
		return CommercialReferralAcceptReceipt{}, authError("invalid_request", "referral code is invalid", http.StatusBadRequest, false, nil)
	}
	client, remote, err := service.commercialRemote(ctx, request.RequestID, "referral_accept", request.Code)
	if err != nil {
		return CommercialReferralAcceptReceipt{}, err
	}
	remote.Code = request.Code
	return client.CommercialReferralAccept(ctx, remote)
}

func (service *Service) CommercialReferralStatus(ctx context.Context) (CommercialReferralStatus, error) {
	requestID, err := service.randomRequestID()
	if err != nil {
		return CommercialReferralStatus{}, authError("random_unavailable", "commercial request identity is unavailable", http.StatusServiceUnavailable, false, err)
	}
	client, remote, err := service.commercialRemote(ctx, requestID, "referral_status", "")
	if err != nil {
		return CommercialReferralStatus{}, err
	}
	return client.CommercialReferralStatus(ctx, remote)
}

func validateCommercialUsage(value CommercialUsageStatus) error {
	if value.Schema != CommercialSchemaV2 || value.PolicyVersion != 2 || value.ServerTime.IsZero() || !value.Trial.Legacy || !value.PostTrial.Legacy || value.Trial.Active || value.Trial.StartedAt.IsZero() || value.Trial.EndsAt.IsZero() ||
		!value.Trial.EndsAt.After(value.Trial.StartedAt) || value.Trial.ReferralBonusDays < 0 || value.Trial.ReferralBonusDays > 36500 ||
		value.PostTrial.SharedPromptAllowance != commercialSharedPromptAllowance || value.PostTrial.SharedPromptRemaining < 0 ||
		value.PostTrial.SharedPromptRemaining > commercialSharedPromptAllowance || value.SuccessfulPromptCount < 0 {
		return errors.New("usage status is invalid")
	}
	if err := validateCommercialQuota(value.Usage, value.Lifetime); err != nil {
		return err
	}
	if value.PostTrial.NewConversationAllowed != value.Usage.CanSubmit {
		return errors.New("conversation admission disagrees with quota")
	}
	return nil
}

// ValidateCommercialUsage also protects projections built from injected native
// services; the HTTP client's decoding alone is not an authorization boundary.
func ValidateCommercialUsage(value CommercialUsageStatus) error {
	return validateCommercialUsage(value)
}

func ValidateCommercialQuota(value CommercialUsageQuota) error {
	return validateCommercialQuota(value, value.Unlimited)
}

func validateCommercialQuota(value CommercialUsageQuota, lifetime bool) error {
	if value.Unit != "accepted_user_prompt" || value.AcceptedPromptCount < 0 || value.Unlimited != lifetime {
		return errors.New("commercial quota is invalid")
	}
	if lifetime {
		if value.Limit != nil || value.Remaining != nil || !value.CanSubmit {
			return errors.New("lifetime quota is invalid")
		}
		return nil
	}
	remaining := commercialAcceptedPromptLimit - value.AcceptedPromptCount
	if remaining < 0 {
		remaining = 0
	}
	if value.Limit == nil || *value.Limit != commercialAcceptedPromptLimit || value.Remaining == nil || *value.Remaining != remaining || value.CanSubmit != (remaining > 0) {
		return errors.New("free account quota is inconsistent")
	}
	return nil
}

func validateCommercialReferralStatus(value CommercialReferralStatus) error {
	if value.Schema != CommercialSchemaV2 || value.Invited.Pending < 0 || value.Invited.Rewarded < 0 || value.Invited.Rejected < 0 {
		return errors.New("referral status is invalid")
	}
	if value.InviteeClaim != nil {
		claim := value.InviteeClaim
		if claim.Status != "pending" && claim.Status != "rewarded" && !strings.HasPrefix(claim.Status, "rejected_") {
			return errors.New("referral claim status is invalid")
		}
		if claim.AcceptedAt == nil || claim.AcceptedAt.IsZero() || claim.SuccessfulPromptCount < 0 {
			return errors.New("referral claim is incomplete")
		}
	}
	return nil
}

func validCommercialReason(value string) bool {
	return value == "lifetime" || value == "free_account" || value == "free_prompt_limit_reached"
}

func validCommercialReferralSettlement(value string) bool {
	return value == "none" || value == "pending" || value == "rewarded" || strings.HasPrefix(value, "rejected_")
}

func validOpaqueOrEmpty(value string, maximum int) bool {
	return value == "" || validOpaque(value, maximum)
}

func validHTTPS(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && len(value) <= 2048 && parsed.Scheme == "https" && parsed.Hostname() != "" &&
		parsed.User == nil && parsed.Fragment == "" && !strings.ContainsAny(value, "\r\n\x00")
}

func invalidCommercialResponse(err error) error {
	return authError("invalid_server_response", "commercial authorization response is invalid", http.StatusBadGateway, false, err)
}
