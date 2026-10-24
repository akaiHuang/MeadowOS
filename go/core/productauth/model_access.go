package productauth

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const ModelAccessSchema = "meadow.model-access.v1"

const (
	modelAccessProviderKind = "meadow_api"
	modelAccessKeyTTL       = 15 * time.Minute
	modelAccessRefreshSkew  = 30 * time.Second
)

type ModelAccessBudget struct {
	Currency           string `json:"currency"`
	MaxBudgetUSDMicros int64  `json:"maxBudgetUsdMicros"`
	Unit               string `json:"unit"`
}

type ModelAccessBilling struct {
	Currency           string `json:"currency"`
	Meter              string `json:"meter"`
	Unit               string `json:"unit"`
	MaxBudgetUSDMicros int64  `json:"maxBudgetUsdMicros"`
}

// ModelAccessModel contains capabilities explicitly configured by the membership
// server. Missing model entries remain unknown; callers must not infer features.
type ModelAccessModel struct {
	ID              string `json:"id"`
	SupportsImages  bool   `json:"supportsImages"`
	SupportsTools   bool   `json:"supportsTools"`
	ContextTokens   int    `json:"contextTokens"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
}

type ModelAccessStatus struct {
	Schema         string             `json:"schema"`
	AccountRef     string             `json:"accountRef"`
	ProviderKind   string             `json:"providerKind"`
	Allowed        bool               `json:"allowed"`
	Billing        string             `json:"billing"`
	DebitMeadowAPI bool               `json:"debitMeadowApi"`
	Configured     bool               `json:"configured"`
	Provisioning   string             `json:"provisioning"`
	Budget         *ModelAccessBudget `json:"budget"`
}

type ModelAccessUsage struct {
	Schema       string   `json:"schema"`
	AccountRef   string   `json:"accountRef"`
	ProviderKind string   `json:"providerKind"`
	Meter        string   `json:"meter"`
	Currency     string   `json:"currency"`
	Spend        *float64 `json:"spend"`
	MaxBudget    *float64 `json:"maxBudget"`
	Remaining    *float64 `json:"remaining"`
}

// ModelAccessProvision is safe for local IPC and the settings projection. The
// subject identifies this native login session. AccountRef provides the stable
// server-derived account binding across sessions; it is not a Firebase UID.
// Provider credentials deliberately have no field in this public type.
type ModelAccessProvision struct {
	Schema       string             `json:"schema"`
	AccountRef   string             `json:"accountRef"`
	ProviderKind string             `json:"providerKind"`
	Subject      string             `json:"subject"`
	APIBase      string             `json:"apiBase"`
	ExpiresAt    time.Time          `json:"expiresAt"`
	Models       []string           `json:"models"`
	ModelCatalog []ModelAccessModel `json:"modelCatalog,omitempty"`
	Billing      ModelAccessBilling `json:"billing"`
}

type modelAccessKeyGrant struct {
	Schema       string             `json:"schema"`
	AccountRef   string             `json:"accountRef"`
	ProviderKind string             `json:"providerKind"`
	APIBase      string             `json:"apiBase"`
	APIKey       string             `json:"apiKey"`
	ExpiresAt    time.Time          `json:"expiresAt"`
	Models       []string           `json:"models"`
	ModelCatalog []ModelAccessModel `json:"modelCatalog,omitempty"`
	Billing      ModelAccessBilling `json:"billing"`
}

type modelAccessCache struct {
	subject string
	grant   modelAccessKeyGrant
}

type modelAccessIdentity struct{ subject, accountRef string }

type modelAccessClient interface {
	modelAccessCall(context.Context, string, commercialRemoteRequest, any) error
}

func (client *FixedClient) modelAccessCall(ctx context.Context, operation string, request commercialRemoteRequest, output any) error {
	paths := map[string]string{"status": "/modelAccessStatus", "key": "/modelAccessKey", "usage": "/modelAccessUsage"}
	path, ok := paths[operation]
	if !ok || !validCredential(request.AccessToken, 32, 128) || !validOpaque(request.RequestID, 160) ||
		!validInstallationID(request.InstallationID) || !validCanonicalBase64URL(request.DeviceProof, ed25519.SignatureSize) {
		return modelAccessError("invalid_request", http.StatusBadRequest)
	}
	return client.doCommercial(ctx, path, request.AccessToken, map[string]any{
		"schema": SchemaV1, "request_id": request.RequestID, "installation_id": request.InstallationID,
		"device_proof": request.DeviceProof, "provider_kind": modelAccessProviderKind,
	}, map[string]string{"X-Request-ID": request.RequestID}, output)
}

func (service *Service) modelAccessRemote(ctx context.Context, operation string) (modelAccessClient, commercialRemoteRequest, accessSession, uint64, error) {
	client, ok := service.client.(modelAccessClient)
	if !ok {
		return nil, commercialRemoteRequest{}, accessSession{}, 0, modelAccessError("model_access_unavailable", http.StatusServiceUnavailable)
	}
	status, err := service.Status(ctx)
	if err != nil || status.Validate() != nil || !status.Authenticated {
		return nil, commercialRemoteRequest{}, accessSession{}, 0, modelAccessError("login_required", http.StatusUnauthorized)
	}
	service.mu.Lock()
	epoch := service.modelAccessEpoch
	service.mu.Unlock()
	access, err := service.currentAccess(ctx)
	if err != nil {
		return nil, commercialRemoteRequest{}, accessSession{}, 0, err
	}
	if access.lease.Subject == "" || access.lease.Subject != status.Subject {
		return nil, commercialRemoteRequest{}, accessSession{}, 0, modelAccessError("account_changed", http.StatusUnauthorized)
	}
	requestID, err := service.randomRequestID()
	if err != nil {
		return nil, commercialRemoteRequest{}, accessSession{}, 0, modelAccessError("auth_unavailable", http.StatusServiceUnavailable)
	}
	privateKey, _, err := service.deviceProofKey(ctx)
	if err != nil {
		return nil, commercialRemoteRequest{}, accessSession{}, 0, err
	}
	defer clear(privateKey)
	remote := commercialRemoteRequest{
		AccessToken: access.token, RequestID: requestID, InstallationID: service.installationID,
		DeviceProof: signBoundDeviceProof(privateKey, "model_access_"+operation, access.token, service.installationID, requestID, "", modelAccessProviderKind),
	}
	return client, remote, access, epoch, nil
}

func (service *Service) ModelAccessStatus(ctx context.Context) (ModelAccessStatus, error) {
	client, remote, access, epoch, err := service.modelAccessRemote(ctx, "status")
	if err != nil {
		return ModelAccessStatus{}, err
	}
	var result ModelAccessStatus
	if err := client.modelAccessCall(ctx, "status", remote, &result); err != nil {
		service.clearModelAccessKey(access.lease.Subject)
		return ModelAccessStatus{}, err
	}
	if result.Validate() != nil {
		return ModelAccessStatus{}, modelAccessError("invalid_server_response", http.StatusBadGateway)
	}
	if !service.modelAccessSessionCurrent(access.lease.Subject, epoch, result.AccountRef) {
		return ModelAccessStatus{}, modelAccessError("account_changed", http.StatusUnauthorized)
	}
	if !result.Allowed {
		service.clearModelAccessKey(access.lease.Subject)
	}
	return result, nil
}

func (service *Service) ModelAccessUsage(ctx context.Context) (ModelAccessUsage, error) {
	client, remote, access, epoch, err := service.modelAccessRemote(ctx, "usage")
	if err != nil {
		return ModelAccessUsage{}, err
	}
	var result ModelAccessUsage
	if err := client.modelAccessCall(ctx, "usage", remote, &result); err != nil {
		service.clearModelAccessKey(access.lease.Subject)
		return ModelAccessUsage{}, err
	}
	if result.Validate() != nil {
		return ModelAccessUsage{}, modelAccessError("invalid_server_response", http.StatusBadGateway)
	}
	if !service.modelAccessSessionCurrent(access.lease.Subject, epoch, result.AccountRef) {
		return ModelAccessUsage{}, modelAccessError("account_changed", http.StatusUnauthorized)
	}
	return result, nil
}

func (service *Service) ModelAccessProvision(ctx context.Context) (ModelAccessProvision, error) {
	grant, subject, err := service.modelAccessKey(ctx)
	if err != nil {
		return ModelAccessProvision{}, err
	}
	return ModelAccessProvision{
		Schema: grant.Schema, AccountRef: grant.AccountRef, ProviderKind: grant.ProviderKind, Subject: subject,
		APIBase: grant.APIBase, ExpiresAt: grant.ExpiresAt, Models: append([]string(nil), grant.Models...), ModelCatalog: append([]ModelAccessModel(nil), grant.ModelCatalog...), Billing: grant.Billing,
	}, nil
}

// ResolveModelAccessForAccountRef binds persisted connections to a stable
// account reference supplied by the trusted membership server, never lease sub.
// The key stays in memory and is never serialized to local IPC or the browser.
func (service *Service) ResolveModelAccessForAccountRef(ctx context.Context, accountRef, expectedBaseURL, modelID string) (string, error) {
	if !validModelAccountRef(accountRef) {
		return "", modelAccessError("model_access_binding_invalid", http.StatusForbidden)
	}
	base, err := modelAccessBase(expectedBaseURL)
	if err != nil || modelID == "" {
		return "", modelAccessError("model_access_binding_invalid", http.StatusForbidden)
	}
	grant, _, err := service.modelAccessKey(ctx)
	if err != nil {
		return "", err
	}
	if base != grant.APIBase {
		return "", modelAccessError("model_access_origin_mismatch", http.StatusForbidden)
	}
	if grant.AccountRef != accountRef {
		return "", modelAccessError("account_changed", http.StatusUnauthorized)
	}
	for _, allowed := range grant.Models {
		if allowed == modelID {
			return grant.APIKey, nil
		}
	}
	return "", modelAccessError("model_access_model_denied", http.StatusForbidden)
}

func (service *Service) modelAccessKey(ctx context.Context) (modelAccessKeyGrant, string, error) {
	service.modelAccessMu.Lock()
	defer service.modelAccessMu.Unlock()
	status, err := service.Status(ctx)
	if err != nil || status.Validate() != nil || !status.Authenticated || status.Subject == "" {
		return modelAccessKeyGrant{}, "", modelAccessError("login_required", http.StatusUnauthorized)
	}
	service.stateMu.Lock()
	service.mu.Lock()
	cached := service.modelAccessCache
	if service.localDenial == "" && service.access.lease.Subject == status.Subject && cached.subject == status.Subject && cached.grant.ExpiresAt.After(service.now().Add(modelAccessRefreshSkew)) {
		service.mu.Unlock()
		service.stateMu.Unlock()
		return cached.grant, cached.subject, nil
	}
	service.modelAccessCache = modelAccessCache{}
	service.mu.Unlock()
	service.stateMu.Unlock()
	client, remote, access, epoch, err := service.modelAccessRemote(ctx, "key")
	if err != nil {
		return modelAccessKeyGrant{}, "", err
	}
	if access.lease.Subject != status.Subject {
		return modelAccessKeyGrant{}, "", modelAccessError("account_changed", http.StatusUnauthorized)
	}
	var grant modelAccessKeyGrant
	if err := client.modelAccessCall(ctx, "key", remote, &grant); err != nil {
		return modelAccessKeyGrant{}, "", err
	}
	if err := grant.validate(service.now()); err != nil {
		return modelAccessKeyGrant{}, "", modelAccessError("invalid_server_response", http.StatusBadGateway)
	}
	// A managed response cannot restore a key while Status is re-verifying a
	// changed secure document. Wait for its fresh identity decision first.
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return modelAccessKeyGrant{}, "", err
	}
	if err := grant.validate(service.now()); err != nil {
		return modelAccessKeyGrant{}, "", modelAccessError("invalid_server_response", http.StatusBadGateway)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.modelAccessSessionCurrentLocked(access.lease.Subject, epoch, grant.AccountRef) {
		return modelAccessKeyGrant{}, "", modelAccessError("account_changed", http.StatusUnauthorized)
	}
	service.modelAccessCache = modelAccessCache{subject: access.lease.Subject, grant: grant}
	return grant, access.lease.Subject, nil
}

func (service *Service) modelAccessSessionCurrent(subject string, epoch uint64, accountRef string) bool {
	service.stateMu.Lock()
	defer service.stateMu.Unlock()
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.modelAccessSessionCurrentLocked(subject, epoch, accountRef)
}

func (service *Service) modelAccessSessionCurrentLocked(subject string, epoch uint64, accountRef string) bool {
	if service.localDenial != "" || service.modelAccessEpoch != epoch || service.access.lease.Subject != subject {
		return false
	}
	identity := service.modelAccessIdentity
	if identity.subject == subject && identity.accountRef != "" && identity.accountRef != accountRef {
		service.modelAccessCache = modelAccessCache{}
		service.modelAccessEpoch++
		return false
	}
	service.modelAccessIdentity = modelAccessIdentity{subject: subject, accountRef: accountRef}
	return true
}

func (service *Service) clearModelAccessKey(subject string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.access.lease.Subject == subject {
		service.modelAccessCache = modelAccessCache{}
		// A denied status or failed authoritative request also invalidates key
		// requests already in flight, even when no key has been cached yet.
		service.modelAccessEpoch++
	}
}

func (status ModelAccessStatus) Validate() error {
	if status.Schema != ModelAccessSchema || !validModelAccountRef(status.AccountRef) || status.ProviderKind != modelAccessProviderKind || status.Billing != "meadow_usd" || !status.DebitMeadowAPI || (status.Allowed && !status.Configured) {
		return errors.New("invalid managed model status")
	}
	switch status.Provisioning {
	case "not_provisioned", "working", "ready", "retry", "revoking", "blocked", "failed":
	default:
		return errors.New("invalid model provisioning state")
	}
	if status.Budget != nil && (status.Budget.Currency != "USD" || status.Budget.Unit != "money" || status.Budget.MaxBudgetUSDMicros < 0) {
		return errors.New("invalid model budget")
	}
	return nil
}

func (usage ModelAccessUsage) Validate() error {
	if usage.Schema != ModelAccessSchema || !validModelAccountRef(usage.AccountRef) || usage.ProviderKind != modelAccessProviderKind || usage.Meter != "litellm" || usage.Currency != "USD" {
		return errors.New("invalid model usage")
	}
	for _, value := range []*float64{usage.Spend, usage.MaxBudget, usage.Remaining} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return errors.New("invalid model usage amount")
		}
	}
	return nil
}

func (provision ModelAccessProvision) Validate() error {
	if provision.Subject == "" || !validOpaque(provision.Subject, 256) {
		return errors.New("missing native session binding")
	}
	grant := modelAccessKeyGrant{Schema: provision.Schema, AccountRef: provision.AccountRef, ProviderKind: provision.ProviderKind, APIBase: provision.APIBase, ExpiresAt: provision.ExpiresAt, Models: provision.Models, ModelCatalog: provision.ModelCatalog, Billing: provision.Billing}
	return grant.validateMetadata()
}

func (grant modelAccessKeyGrant) validate(now time.Time) error {
	if err := grant.validateMetadata(); err != nil {
		return err
	}
	if !strings.HasPrefix(grant.APIKey, "sk-meadow-") || !validCanonicalBase64URL(strings.TrimPrefix(grant.APIKey, "sk-meadow-"), 32) ||
		!grant.ExpiresAt.After(now) || grant.ExpiresAt.After(now.Add(modelAccessKeyTTL+modelAccessRefreshSkew)) {
		return errors.New("invalid managed model credential")
	}
	return nil
}

func (grant modelAccessKeyGrant) validateMetadata() error {
	base, err := modelAccessBase(grant.APIBase)
	if err != nil || base != grant.APIBase || grant.Schema != ModelAccessSchema || !validModelAccountRef(grant.AccountRef) || grant.ProviderKind != modelAccessProviderKind || grant.ExpiresAt.IsZero() ||
		grant.Billing.Currency != "USD" || grant.Billing.Meter != "litellm" || grant.Billing.Unit != "money" || grant.Billing.MaxBudgetUSDMicros < 0 || len(grant.Models) == 0 || len(grant.Models) > 64 {
		return errors.New("invalid model access metadata")
	}
	seen := map[string]bool{}
	for _, model := range grant.Models {
		if model == "" || model == "*" || len(model) > 80 || strings.TrimSpace(model) != model || strings.ContainsFunc(model, unicode.IsControl) || seen[model] {
			return errors.New("invalid managed model catalog")
		}
		seen[model] = true
	}
	catalogSeen := make(map[string]bool, len(grant.ModelCatalog))
	for _, model := range grant.ModelCatalog {
		if !seen[model.ID] || catalogSeen[model.ID] || model.ContextTokens <= 0 || model.ContextTokens > 16777216 || model.MaxOutputTokens <= 0 || model.MaxOutputTokens > model.ContextTokens {
			return errors.New("invalid managed model capabilities")
		}
		catalogSeen[model.ID] = true
	}
	return nil
}

func modelAccessBase(value string) (string, error) {
	base, err := url.Parse(value)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" || base.RawPath != "" || strings.TrimSpace(value) != value {
		return "", errors.New("managed model origin must be fixed HTTPS")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	return base.String(), nil
}

func modelAccessError(code string, status int) error {
	message := "Meadow API access is unavailable"
	if status == http.StatusUnauthorized {
		message = "Meadow account login is required"
	}
	return authError(code, message, status, status >= 500, nil)
}

func validModelAccountRef(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}
