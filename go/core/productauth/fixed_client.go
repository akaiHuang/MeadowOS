package productauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	deviceVerificationPath = "/activate"
	deviceStartPath        = "/api/v1/device-auth/start"
	devicePollPath         = "/api/v1/device-auth/poll"
	refreshPath            = "/api/v1/device-auth/refresh"
	logoutPath             = "/api/v1/device-auth/logout"
	redeemPath             = "/api/v1/device-auth/serial/redeem"
	devicesListPath        = "/api/v1/device-auth/devices/list"
	deviceRevokePath       = "/api/v1/device-auth/devices/revoke"
	defaultMaxBody         = 1 << 20
)

type FixedClientConfig struct {
	Origin                string
	CommercialOrigin      string
	VerificationOrigin    string
	Issuer                string
	Audience              string
	JWKSKeyring           map[string]ed25519.PublicKey
	HTTPClient            *http.Client
	AllowInsecureLoopback bool
	RequireVerifiedEmail  bool
	MaxResponseBytes      int64
	MaxAccessLifetime     time.Duration
	ClockSkew             time.Duration
	Now                   func() time.Time
}

type FixedClient struct {
	origin             *url.URL
	commercialOrigin   *url.URL
	verificationOrigin *url.URL
	issuer             string
	audience           string
	keys               map[string]ed25519.PublicKey
	http               *http.Client
	maxBody            int64
	maxLifetime        time.Duration
	now                func() time.Time
}

func NewFixedClient(config FixedClientConfig) (*FixedClient, error) {
	origin, err := validateFixedOrigin(config.Origin, config.AllowInsecureLoopback)
	if err != nil {
		return nil, fmt.Errorf("productauth: invalid fixed origin: %w", err)
	}
	commercialOrigin := origin
	if strings.TrimSpace(config.CommercialOrigin) != "" {
		commercialOrigin, err = validateFixedOrigin(config.CommercialOrigin, config.AllowInsecureLoopback)
		if err != nil {
			return nil, fmt.Errorf("productauth: invalid fixed commercial origin: %w", err)
		}
	}
	verificationOrigin, err := validateFixedOrigin(config.VerificationOrigin, config.AllowInsecureLoopback)
	if err != nil {
		return nil, fmt.Errorf("productauth: invalid verification origin: %w", err)
	}
	issuer, err := validateFixedOrigin(config.Issuer, config.AllowInsecureLoopback)
	if err != nil || issuer.String() != origin.String() {
		return nil, errors.New("productauth: lease issuer must equal the fixed service origin")
	}
	if config.Audience != LeaseAudience || !config.RequireVerifiedEmail || len(config.JWKSKeyring) == 0 {
		return nil, errors.New("productauth: audience, verified-account policy and lease keyring are required")
	}
	keys := make(map[string]ed25519.PublicKey, len(config.JWKSKeyring))
	for keyID, key := range config.JWKSKeyring {
		if !validOpaque(keyID, 128) || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("productauth: lease keyring is invalid")
		}
		keys[keyID] = append(ed25519.PublicKey(nil), key...)
	}
	base := config.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: 20 * time.Second}
	}
	clientCopy := *base
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("productauth: redirects are forbidden")
	}
	maxBody := config.MaxResponseBytes
	if maxBody == 0 {
		maxBody = defaultMaxBody
	}
	if maxBody < 1024 || maxBody > 4<<20 {
		return nil, errors.New("productauth: response bound is invalid")
	}
	maxLifetime := config.MaxAccessLifetime
	if maxLifetime == 0 {
		maxLifetime = time.Hour
	}
	if maxLifetime < 5*time.Minute || maxLifetime > maximumLeaseLife || config.ClockSkew < 0 {
		return nil, errors.New("productauth: token timing policy is invalid")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &FixedClient{
		origin: origin, commercialOrigin: commercialOrigin, verificationOrigin: verificationOrigin, issuer: issuer.String(),
		audience: config.Audience, keys: keys, http: &clientCopy, maxBody: maxBody,
		maxLifetime: maxLifetime, now: now,
	}, nil
}

func (client *FixedClient) Issuer() string   { return client.issuer }
func (client *FixedClient) Audience() string { return client.audience }
func (client *FixedClient) LeasePublicKeys() map[string]ed25519.PublicKey {
	keys := make(map[string]ed25519.PublicKey, len(client.keys))
	for keyID, key := range client.keys {
		keys[keyID] = append(ed25519.PublicKey(nil), key...)
	}
	return keys
}

func (client *FixedClient) Start(ctx context.Context, input DeviceAuthorizationStart) (RemoteStart, error) {
	if !validPKCE(input.CodeChallenge) || !validCanonicalBase64URL(input.DevicePublicKey, ed25519.PublicKeySize) ||
		!validOpaque(input.RequestID, 160) || !validInstallationID(input.InstallationID) ||
		!validClientLabel(input.Platform) || !validClientLabel(input.ClientName) {
		return RemoteStart{}, authError("invalid_request", "device authorization request is invalid", http.StatusBadRequest, false, nil)
	}
	var response struct {
		Schema                  string             `json:"schema"`
		State                   AuthorizationState `json:"state"`
		TransactionID           string             `json:"transaction_id"`
		UserCode                string             `json:"user_code"`
		VerificationURI         string             `json:"verification_uri"`
		VerificationURIComplete string             `json:"verification_uri_complete"`
		ExpiresIn               int                `json:"expires_in"`
		Interval                int                `json:"interval"`
	}
	request := map[string]any{
		"schema": SchemaV1, "request_id": input.RequestID,
		"code_challenge": input.CodeChallenge, "code_challenge_method": "S256",
		"installation_id": input.InstallationID, "platform": input.Platform,
		"client_name": input.ClientName, "device_public_key": input.DevicePublicKey,
	}
	// The browser host is a fixed native configuration, never a per-request
	// caller preference. Omit the additive field for legacy same-origin builds.
	if client.verificationOrigin.String() != client.origin.String() {
		request["verification_origin"] = client.verificationOrigin.String()
	}
	err := client.do(ctx, deviceStartPath, "", request, nil, &response)
	if err != nil {
		return RemoteStart{}, err
	}
	if response.Schema != SchemaV1 || response.State != StateLoginPending ||
		!validOpaque(response.TransactionID, 160) || !validUserCode(response.UserCode) ||
		response.ExpiresIn < 1 || response.ExpiresIn > 20*60 || response.Interval < 1 || response.Interval > 60 {
		return RemoteStart{}, authError("invalid_server_response", "device authorization response is invalid", http.StatusBadGateway, false, nil)
	}
	if err := client.validateVerificationURI(response.VerificationURI, ""); err != nil {
		return RemoteStart{}, err
	}
	if response.VerificationURIComplete != "" {
		if err := client.validateVerificationURI(response.VerificationURIComplete, response.UserCode); err != nil {
			return RemoteStart{}, err
		}
	}
	return RemoteStart{
		TransactionID: response.TransactionID, UserCode: response.UserCode,
		VerificationURI: response.VerificationURI, VerificationURIComplete: response.VerificationURIComplete,
		ExpiresAt:  client.now().UTC().Add(time.Duration(response.ExpiresIn) * time.Second),
		RetryAfter: time.Duration(response.Interval) * time.Second,
	}, nil
}

func (client *FixedClient) Poll(ctx context.Context, input DeviceAuthorizationPoll) (RemotePoll, error) {
	if !validOpaque(input.TransactionID, 160) || !validPKCEVerifier(input.CodeVerifier) ||
		!validCanonicalBase64URL(input.DeviceProof, ed25519.SignatureSize) ||
		!validInstallationID(input.InstallationID) || !validOpaque(input.RequestID, 160) {
		return RemotePoll{}, authError("invalid_request", "device authorization poll is invalid", http.StatusBadRequest, false, nil)
	}
	var response grantResponse
	if err := client.do(ctx, devicePollPath, "", map[string]any{
		"schema": SchemaV1, "transaction_id": input.TransactionID,
		"code_verifier": input.CodeVerifier, "installation_id": input.InstallationID,
		"request_id": input.RequestID, "device_proof": input.DeviceProof,
	}, nil, &response); err != nil {
		return RemotePoll{}, err
	}
	if response.Schema != SchemaV1 {
		return RemotePoll{}, authError("invalid_server_response", "poll response schema is invalid", http.StatusBadGateway, false, nil)
	}
	switch response.State {
	case StateLoginPending, StateSlowDown:
		if response.Interval < 1 || response.Interval > 60 || response.ErrorCode == "" {
			return RemotePoll{}, authError("invalid_server_response", "poll response is invalid", http.StatusBadGateway, false, nil)
		}
		return RemotePoll{State: response.State, RetryAfter: time.Duration(response.Interval) * time.Second, ErrorCode: response.ErrorCode}, nil
	case StateLoginDenied, StateLoginExpired:
		return RemotePoll{State: response.State, ErrorCode: response.ErrorCode}, nil
	case StateAuthorizedGeneral, StateAuthorizedPro:
		grant, err := client.validateGrant(response)
		if err != nil {
			return RemotePoll{}, err
		}
		return RemotePoll{State: response.State, Grant: &grant}, nil
	default:
		return RemotePoll{}, authError("invalid_server_response", "poll response state is invalid", http.StatusBadGateway, false, nil)
	}
}

func (client *FixedClient) Refresh(ctx context.Context, input RemoteRefreshRequest) (RemoteGrant, error) {
	if !validCredential(input.RefreshToken, 64, 128) || !validInstallationID(input.InstallationID) ||
		!validOpaque(input.RequestID, 160) || !validCanonicalBase64URL(input.DeviceProof, ed25519.SignatureSize) {
		return RemoteGrant{}, authError("invalid_request", "refresh request is invalid", http.StatusBadRequest, false, nil)
	}
	var response grantResponse
	if err := client.do(ctx, refreshPath, "", map[string]any{
		"schema": SchemaV1, "refresh_token": input.RefreshToken,
		"installation_id": input.InstallationID, "request_id": input.RequestID,
		"device_proof": input.DeviceProof,
	}, nil, &response); err != nil {
		return RemoteGrant{}, err
	}
	return client.validateGrant(response)
}

func (client *FixedClient) Logout(ctx context.Context, input RemoteLogoutRequest) error {
	if !validCredential(input.RefreshToken, 64, 128) || !validInstallationID(input.InstallationID) ||
		!validOpaque(input.RequestID, 160) || !validCanonicalBase64URL(input.DeviceProof, ed25519.SignatureSize) {
		return authError("invalid_request", "logout request is invalid", http.StatusBadRequest, false, nil)
	}
	var response struct {
		Schema string             `json:"schema"`
		State  AuthorizationState `json:"state"`
	}
	if err := client.do(ctx, logoutPath, "", map[string]any{
		"schema": SchemaV1, "refresh_token": input.RefreshToken,
		"installation_id": input.InstallationID, "request_id": input.RequestID,
		"device_proof": input.DeviceProof,
	}, nil, &response); err != nil {
		return err
	}
	if response.Schema != SchemaV1 || response.State != StateLoggedOut {
		return authError("invalid_server_response", "logout response is invalid", http.StatusBadGateway, false, nil)
	}
	return nil
}

func (client *FixedClient) Redeem(ctx context.Context, input RemoteRedeemRequest) (RemoteRedeem, error) {
	if !validCredential(input.AccessToken, 32, 128) || !validSerial(input.Serial) ||
		!validOpaque(input.RequestID, 160) || !validIdempotencyKey(input.IdempotencyKey) ||
		!validInstallationID(input.InstallationID) || !validCanonicalBase64URL(input.DeviceProof, ed25519.SignatureSize) {
		return RemoteRedeem{}, authError("invalid_request", "license redemption request is invalid", http.StatusBadRequest, false, nil)
	}
	var response struct {
		Schema       string             `json:"schema"`
		State        AuthorizationState `json:"state"`
		Lease        string             `json:"lease"`
		KeyID        string             `json:"kid"`
		Plan         Plan               `json:"plan"`
		Capabilities Capabilities       `json:"capabilities"`
		Receipt      RedeemReceipt      `json:"receipt"`
	}
	headers := map[string]string{"X-Request-ID": input.RequestID, "Idempotency-Key": input.IdempotencyKey}
	if err := client.do(ctx, redeemPath, input.AccessToken, map[string]any{
		"schema": SchemaV1, "serial": input.Serial, "request_id": input.RequestID,
		"idempotency_key": input.IdempotencyKey, "installation_id": input.InstallationID,
		"device_proof": input.DeviceProof,
	}, headers, &response); err != nil {
		return RemoteRedeem{}, err
	}
	if response.Schema != SchemaV1 || response.State != StateAuthorizedPro || response.Plan != PlanPro ||
		response.Capabilities.Validate(PlanPro) != nil || response.Lease == "" || !validOpaque(response.KeyID, 128) ||
		response.Receipt.RequestID != input.RequestID || !validOpaque(response.Receipt.ReceiptID, 160) ||
		response.Receipt.Status != "accepted" {
		return RemoteRedeem{}, authError("invalid_server_response", "license redemption response is invalid", http.StatusBadGateway, false, nil)
	}
	return RemoteRedeem{
		State: response.State, Lease: response.Lease, KeyID: response.KeyID,
		Plan: response.Plan, Capabilities: response.Capabilities, Receipt: response.Receipt,
	}, nil
}

func (client *FixedClient) ListDevices(ctx context.Context, input RemoteDeviceListRequest) (DeviceListResult, error) {
	if !validCredential(input.AccessToken, 32, 128) || !validOpaque(input.RequestID, 160) ||
		!validInstallationID(input.InstallationID) || !validCanonicalBase64URL(input.DeviceProof, ed25519.SignatureSize) {
		return DeviceListResult{}, authError("invalid_request", "device list request is invalid", http.StatusBadRequest, false, nil)
	}
	var response DeviceListResult
	if err := client.do(ctx, devicesListPath, input.AccessToken, map[string]any{
		"schema": SchemaV1, "request_id": input.RequestID, "installation_id": input.InstallationID,
		"device_proof": input.DeviceProof,
	}, map[string]string{"X-Request-ID": input.RequestID}, &response); err != nil {
		return DeviceListResult{}, err
	}
	if err := validateDeviceList(response); err != nil {
		return DeviceListResult{}, authError("invalid_server_response", "device list response is invalid", http.StatusBadGateway, false, err)
	}
	return response, nil
}

func (client *FixedClient) RevokeDevice(ctx context.Context, input RemoteDeviceRevokeRequest) (RemoteDeviceRevoke, error) {
	if !validCredential(input.AccessToken, 32, 128) || !validOpaque(input.DeviceID, 128) ||
		!validOpaque(input.RequestID, 160) || !validIdempotencyKey(input.IdempotencyKey) ||
		!validReason(input.Reason) || !input.Confirmed || !validInstallationID(input.InstallationID) ||
		!validCanonicalBase64URL(input.DeviceProof, ed25519.SignatureSize) {
		return RemoteDeviceRevoke{}, authError("invalid_request", "device revoke request is invalid", http.StatusBadRequest, false, nil)
	}
	var response struct {
		Schema       string              `json:"schema"`
		State        AuthorizationState  `json:"state"`
		Lease        string              `json:"lease"`
		KeyID        string              `json:"kid"`
		Plan         Plan                `json:"plan"`
		Capabilities Capabilities        `json:"capabilities"`
		Receipt      DeviceRevokeReceipt `json:"receipt"`
	}
	headers := map[string]string{"X-Request-ID": input.RequestID, "Idempotency-Key": input.IdempotencyKey}
	if err := client.do(ctx, deviceRevokePath, input.AccessToken, map[string]any{
		"schema": SchemaV1, "device_id": input.DeviceID, "request_id": input.RequestID,
		"idempotency_key": input.IdempotencyKey, "reason": input.Reason, "confirmed": true,
		"installation_id": input.InstallationID, "device_proof": input.DeviceProof,
	}, headers, &response); err != nil {
		return RemoteDeviceRevoke{}, err
	}
	expectedState := StateAuthorizedGeneral
	if response.Plan == PlanPro {
		expectedState = StateAuthorizedPro
	}
	if response.Schema != SchemaV1 || response.State != expectedState || response.Capabilities.Validate(response.Plan) != nil ||
		response.Lease == "" || !validOpaque(response.KeyID, 128) || response.Receipt.RequestID != input.RequestID ||
		response.Receipt.DeviceID != input.DeviceID || response.Receipt.Status != "revoked" ||
		!validOpaque(response.Receipt.ReceiptID, 160) || !validOpaque(response.Receipt.AuditID, 160) {
		return RemoteDeviceRevoke{}, authError("invalid_server_response", "device revoke response is invalid", http.StatusBadGateway, false, nil)
	}
	return RemoteDeviceRevoke{
		State: response.State, Lease: response.Lease, KeyID: response.KeyID, Plan: response.Plan,
		Capabilities: response.Capabilities, Receipt: response.Receipt,
	}, nil
}

func validateDeviceList(result DeviceListResult) error {
	if result.Schema != SchemaV1 || !validOpaque(result.CurrentDeviceID, 128) || result.ActiveDevices < 1 ||
		result.MaximumDevices < 1 || result.MaximumDevices > 16 || len(result.Devices) < 1 || len(result.Devices) > 50 {
		return errors.New("device list envelope is invalid")
	}
	seen, active, current := make(map[string]struct{}, len(result.Devices)), 0, 0
	for _, device := range result.Devices {
		if !validOpaque(device.ID, 128) || !validClientLabel(device.Name) || !validClientLabel(device.Platform) ||
			(device.Status != "active" && device.Status != "revoked") || device.CreatedAt == nil || device.CreatedAt.IsZero() {
			return errors.New("device list entry is invalid")
		}
		if _, ok := seen[device.ID]; ok {
			return errors.New("device list contains duplicates")
		}
		seen[device.ID] = struct{}{}
		if device.Status == "active" {
			active++
		}
		if device.Current {
			current++
			if device.ID != result.CurrentDeviceID || device.Status != "active" {
				return errors.New("current device is invalid")
			}
		}
	}
	if current != 1 || active != result.ActiveDevices || active > result.MaximumDevices {
		return errors.New("device counters are invalid")
	}
	return nil
}

func validReason(value string) bool {
	trimmed := strings.TrimSpace(value)
	return len(trimmed) >= 3 && len(trimmed) <= 500 && !strings.ContainsAny(trimmed, "\r\n\x00")
}

type grantResponse struct {
	Schema           string             `json:"schema"`
	State            AuthorizationState `json:"state"`
	Interval         int                `json:"interval,omitempty"`
	ErrorCode        string             `json:"error_code,omitempty"`
	TokenType        string             `json:"token_type,omitempty"`
	AccessToken      string             `json:"access_token,omitempty"`
	ExpiresIn        int                `json:"expires_in,omitempty"`
	RefreshToken     string             `json:"refresh_token,omitempty"`
	RefreshExpiresIn int                `json:"refresh_expires_in,omitempty"`
	Lease            string             `json:"lease,omitempty"`
	KeyID            string             `json:"kid,omitempty"`
	Plan             Plan               `json:"plan,omitempty"`
	Capabilities     Capabilities       `json:"capabilities,omitempty"`
}

func (client *FixedClient) validateGrant(response grantResponse) (RemoteGrant, error) {
	if response.Schema != SchemaV1 || response.TokenType != "Bearer" ||
		!validCredential(response.AccessToken, 32, 128) || !validCredential(response.RefreshToken, 64, 128) ||
		response.ExpiresIn < 1 || response.ExpiresIn > int(client.maxLifetime/time.Second) ||
		response.RefreshExpiresIn < response.ExpiresIn || response.Lease == "" || len(response.Lease) > 32<<10 ||
		!validOpaque(response.KeyID, 128) {
		return RemoteGrant{}, authError("invalid_server_response", "authentication grant is incomplete", http.StatusBadGateway, false, nil)
	}
	expectedState := StateAuthorizedGeneral
	if response.Plan == PlanPro {
		expectedState = StateAuthorizedPro
	}
	if response.State != expectedState || response.Capabilities.Validate(response.Plan) != nil {
		return RemoteGrant{}, authError("invalid_server_response", "authentication grant plan is invalid", http.StatusBadGateway, false, nil)
	}
	return RemoteGrant{
		State: response.State, AccessToken: response.AccessToken, RefreshToken: response.RefreshToken,
		AccessExpiresAt: client.now().UTC().Add(time.Duration(response.ExpiresIn) * time.Second),
		Lease:           response.Lease, KeyID: response.KeyID, Plan: response.Plan, Capabilities: response.Capabilities,
	}, nil
}

func (client *FixedClient) do(ctx context.Context, path, accessToken string, input any, headers map[string]string, output any) error {
	return client.doAt(ctx, client.origin, path, accessToken, input, headers, output)
}

func (client *FixedClient) doCommercial(ctx context.Context, path, accessToken string, input any, headers map[string]string, output any) error {
	return client.doAt(ctx, client.commercialOrigin, path, accessToken, input, headers, output)
}

func (client *FixedClient) doAt(ctx context.Context, origin *url.URL, path, accessToken string, input any, headers map[string]string, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return authError("invalid_request", "request could not be encoded", http.StatusBadRequest, false, err)
	}
	endpoint := *origin
	endpoint.Path = path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return authError("auth_unavailable", "authentication request could not be created", http.StatusServiceUnavailable, false, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return authError("network_unavailable", "authentication service is unavailable", http.StatusBadGateway, true, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, client.maxBody+1))
	if err != nil || int64(len(body)) > client.maxBody {
		return authError("invalid_server_response", "authentication response exceeds the fixed bound", http.StatusBadGateway, false, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var remote struct {
			Schema string `json:"schema"`
			Error  struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		if strictJSON(body, &remote) != nil || remote.Schema != SchemaV1 {
			return authError("invalid_server_response", "authentication error response is invalid", http.StatusBadGateway, false, nil)
		}
		code := remote.Error.Code
		if !validOpaque(code, 80) {
			code = "auth_service_rejected"
		}
		message := "authentication service rejected the request"
		if remote.Error.Message != "" && len(remote.Error.Message) <= 240 {
			message = remote.Error.Message
		}
		return authError(code, message, response.StatusCode, remote.Error.Retryable, nil)
	}
	if err := strictJSON(body, output); err != nil {
		return authError("invalid_server_response", "authentication response is invalid", http.StatusBadGateway, false, err)
	}
	return nil
}

func (client *FixedClient) validateVerificationURI(value, userCode string) error {
	candidate, err := url.Parse(value)
	if err != nil || candidate.User != nil || candidate.Fragment != "" || strings.Contains(value, "#") || candidate.Opaque != "" || strings.TrimSpace(value) != value ||
		candidate.Scheme != client.verificationOrigin.Scheme ||
		!strings.EqualFold(candidate.Host, client.verificationOrigin.Host) {
		return authError("cross_origin_verification_uri", "verification URI is not on the fixed origin", http.StatusBadGateway, false, err)
	}
	if candidate.Path != deviceVerificationPath || candidate.RawPath != "" {
		return authError("invalid_server_response", "verification URI path is invalid", http.StatusBadGateway, false, nil)
	}
	if userCode == "" {
		if candidate.RawQuery != "" || candidate.ForceQuery {
			return authError("invalid_server_response", "verification URI must not contain parameters", http.StatusBadGateway, false, nil)
		}
		return nil
	}
	query, err := url.ParseQuery(candidate.RawQuery)
	if err != nil || len(query) != 1 || len(query["code"]) != 1 || query.Get("code") != userCode {
		return authError("invalid_server_response", "verification URI code does not match the login transaction", http.StatusBadGateway, false, err)
	}
	return nil
}

func validateFixedOrigin(value string, allowLoopback bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("origin must contain only scheme and authority")
	}
	if parsed.Scheme != "https" {
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if !allowLoopback || parsed.Scheme != "http" || !(host == "localhost" || ip != nil && ip.IsLoopback()) {
			return nil, errors.New("origin must use HTTPS")
		}
	}
	parsed.Path = ""
	return parsed, nil
}

func ParseJWKS(payload []byte) (map[string]ed25519.PublicKey, error) {
	var document struct {
		Keys []struct {
			KeyType   string `json:"kty"`
			Curve     string `json:"crv"`
			KeyID     string `json:"kid"`
			Use       string `json:"use"`
			Algorithm string `json:"alg"`
			X         string `json:"x"`
		} `json:"keys"`
	}
	if err := strictJSON(payload, &document); err != nil || len(document.Keys) == 0 {
		return nil, errors.New("productauth: JWKS document is invalid")
	}
	keys := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, item := range document.Keys {
		if item.KeyType != "OKP" || item.Curve != "Ed25519" || item.Use != "sig" ||
			item.Algorithm != "EdDSA" || !validOpaque(item.KeyID, 128) ||
			!validCanonicalBase64URL(item.X, ed25519.PublicKeySize) {
			return nil, errors.New("productauth: JWKS key is unsupported")
		}
		decoded, _ := decodeCanonicalBase64URL(item.X, ed25519.PublicKeySize, ed25519.PublicKeySize)
		if _, exists := keys[item.KeyID]; exists {
			return nil, errors.New("productauth: duplicate JWKS key id")
		}
		keys[item.KeyID] = ed25519.PublicKey(decoded)
	}
	return keys, nil
}

func validCanonicalBase64URL(value string, size int) bool {
	_, err := decodeCanonicalBase64URL(value, size, size)
	return err == nil
}

func validUserCode(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= 4 && len(value) <= 32 && !strings.ContainsAny(value, "\r\n\t")
}

func validPKCE(value string) bool {
	return validCanonicalBase64URL(value, 32)
}

func validPKCEVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '.' || r == '_' || r == '~') {
			return false
		}
	}
	return true
}

func validCredential(value string, minimum, maximum int) bool {
	return len(value) >= minimum && len(value) <= maximum && !strings.ContainsAny(value, "\r\n\t ")
}
