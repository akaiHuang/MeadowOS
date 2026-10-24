package productauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	coreauth "github.com/hey-meadow/meadow-code/go/core/productauth"
)

const maxResponseBytes = 256 << 10

type requestDoer func(context.Context, string, string, any, any) error

// Client only talks to the authenticated local daemon. The daemon owns the
// fixed Firebase Hosting device-auth transport and every credential.
type Client struct{ do requestDoer }

func New() *Client { return &Client{do: daemonDo} }

func (client *Client) Status(ctx context.Context) (coreauth.Status, error) {
	var status coreauth.Status
	if err := client.call(ctx, http.MethodGet, "/auth/status", nil, &status); err != nil {
		return coreauth.Status{}, err
	}
	if status.Validate() != nil {
		return coreauth.Status{}, invalidResponse()
	}
	return status, nil
}

// AdmissionStatus reads the Agent's verified, bounded in-memory window. Only
// older helpers without the endpoint fall back to a full verification.
func (client *Client) AdmissionStatus(ctx context.Context) (coreauth.Status, error) {
	var status coreauth.Status
	err := client.call(ctx, http.MethodGet, "/auth/admission-status", nil, &status)
	if err != nil {
		if ErrorStatus(err) == http.StatusNotFound {
			return client.Status(ctx)
		}
		return coreauth.Status{}, err
	}
	if status.Validate() != nil {
		return coreauth.Status{}, invalidResponse()
	}
	return status, nil
}

// PreparePromptGrant is background maintenance. The Agent owns reservation,
// identity fencing, durable debit and settlement; this client stores no credit.
func (client *Client) PreparePromptGrant(ctx context.Context) error {
	var result map[string]any
	return client.call(ctx, http.MethodPost, "/commercial/prepare-prompt-grant",
		map[string]string{"schema": "meadow.desktop-prompt-grant.v1"}, &result)
}

func (client *Client) Start(ctx context.Context, mode coreauth.LoginMode) (coreauth.StartResult, error) {
	var result coreauth.StartResult
	err := client.call(ctx, http.MethodPost, "/auth/start", coreauth.StartRequest{Schema: coreauth.SchemaV1, Mode: mode}, &result)
	if err != nil {
		return coreauth.StartResult{}, err
	}
	if result.Schema != coreauth.SchemaV1 || result.State != coreauth.StateLoginPending || result.TransactionID == "" || result.VerificationURI == "" || result.UserCode == "" || result.ExpiresIn <= 0 || result.Interval <= 0 {
		return coreauth.StartResult{}, invalidResponse()
	}
	return result, nil
}

func (client *Client) Poll(ctx context.Context, transactionID string) (coreauth.PollResult, error) {
	var result coreauth.PollResult
	err := client.call(ctx, http.MethodPost, "/auth/poll", coreauth.PollRequest{Schema: coreauth.SchemaV1, TransactionID: strings.TrimSpace(transactionID)}, &result)
	if err != nil {
		return coreauth.PollResult{}, err
	}
	if result.Schema != coreauth.SchemaV1 {
		return coreauth.PollResult{}, invalidResponse()
	}
	switch result.State {
	case coreauth.StateLoginPending:
		if result.Interval <= 0 || (result.ErrorCode != "authorization_pending" && result.ErrorCode != "slow_down") {
			return coreauth.PollResult{}, invalidResponse()
		}
	case coreauth.StateAuthorizedGeneral, coreauth.StateAuthorizedPro:
		if result.Status == nil || result.Status.State != result.State || result.Status.Validate() != nil {
			return coreauth.PollResult{}, invalidResponse()
		}
	case coreauth.StateLoginDenied, coreauth.StateLoginExpired:
		if result.ErrorCode == "" {
			return coreauth.PollResult{}, invalidResponse()
		}
	default:
		return coreauth.PollResult{}, invalidResponse()
	}
	return result, nil
}

func (client *Client) Refresh(ctx context.Context) (coreauth.Status, error) {
	var status coreauth.Status
	if err := client.call(ctx, http.MethodPost, "/auth/refresh", coreauth.RefreshRequest{Schema: coreauth.SchemaV1}, &status); err != nil {
		return coreauth.Status{}, err
	}
	if status.Validate() != nil || !status.Authenticated {
		return coreauth.Status{}, invalidResponse()
	}
	return status, nil
}

func (client *Client) Logout(ctx context.Context) error {
	var response struct {
		Schema string `json:"schema"`
		OK     bool   `json:"ok"`
	}
	if err := client.call(ctx, http.MethodPost, "/auth/logout", coreauth.LogoutRequest{Schema: coreauth.SchemaV1}, &response); err != nil {
		return err
	}
	if response.Schema != coreauth.SchemaV1 || !response.OK {
		return invalidResponse()
	}
	return nil
}

func (client *Client) Redeem(ctx context.Context, serial string) (coreauth.RedeemResult, error) {
	requestID, err := coreauth.NewRequestID()
	if err != nil {
		return coreauth.RedeemResult{}, unavailable()
	}
	idempotencyKey, err := opaqueID()
	if err != nil {
		return coreauth.RedeemResult{}, unavailable()
	}
	request := coreauth.RedeemRequest{Schema: coreauth.SchemaV1, Serial: strings.TrimSpace(serial), RequestID: requestID, IdempotencyKey: idempotencyKey}
	return client.RedeemRequest(ctx, request)
}

func (client *Client) RedeemRequest(ctx context.Context, request coreauth.RedeemRequest) (coreauth.RedeemResult, error) {
	if request.Schema != coreauth.SchemaV1 || strings.TrimSpace(request.Serial) == "" || strings.TrimSpace(request.RequestID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return coreauth.RedeemResult{}, invalidResponse()
	}
	var result coreauth.RedeemResult
	if err := client.call(ctx, http.MethodPost, "/license/redeem", request, &result); err != nil {
		return coreauth.RedeemResult{}, err
	}
	if result.Schema != coreauth.SchemaV1 || result.Availability.Validate() != nil || !result.Availability.Authenticated || result.Receipt.RequestID != request.RequestID || result.Receipt.Status != "accepted" {
		return coreauth.RedeemResult{}, invalidResponse()
	}
	return result, nil
}

func (client *Client) ListDevices(ctx context.Context) (coreauth.DeviceListResult, error) {
	var result coreauth.DeviceListResult
	if err := client.call(ctx, http.MethodPost, "/devices/list", coreauth.DeviceListRequest{Schema: coreauth.SchemaV1}, &result); err != nil {
		return coreauth.DeviceListResult{}, err
	}
	if result.Schema != coreauth.SchemaV1 || len(result.Devices) == 0 || result.CurrentDeviceID == "" {
		return coreauth.DeviceListResult{}, invalidResponse()
	}
	return result, nil
}

func (client *Client) RevokeDevice(ctx context.Context, deviceID, reason string) (coreauth.DeviceRevokeResult, error) {
	requestID, err := coreauth.NewRequestID()
	if err != nil {
		return coreauth.DeviceRevokeResult{}, unavailable()
	}
	idempotencyKey, err := opaqueID()
	if err != nil {
		return coreauth.DeviceRevokeResult{}, unavailable()
	}
	return client.RevokeDeviceRequest(ctx, coreauth.DeviceRevokeRequest{
		Schema: coreauth.SchemaV1, DeviceID: strings.TrimSpace(deviceID), RequestID: requestID,
		IdempotencyKey: idempotencyKey, Reason: strings.TrimSpace(reason), Confirmed: true,
	})
}

func (client *Client) RevokeDeviceRequest(ctx context.Context, request coreauth.DeviceRevokeRequest) (coreauth.DeviceRevokeResult, error) {
	if request.Schema != coreauth.SchemaV1 || request.DeviceID == "" || request.RequestID == "" || request.IdempotencyKey == "" || !request.Confirmed {
		return coreauth.DeviceRevokeResult{}, invalidResponse()
	}
	var result coreauth.DeviceRevokeResult
	if err := client.call(ctx, http.MethodPost, "/devices/revoke", request, &result); err != nil {
		return coreauth.DeviceRevokeResult{}, err
	}
	if result.Schema != coreauth.SchemaV1 || result.Availability.Validate() != nil || !result.Availability.Authenticated ||
		result.Receipt.RequestID != request.RequestID || result.Receipt.DeviceID != request.DeviceID || result.Receipt.Status != "revoked" {
		return coreauth.DeviceRevokeResult{}, invalidResponse()
	}
	return result, nil
}

func (client *Client) call(ctx context.Context, method, path string, input, output any) error {
	if client == nil || client.do == nil || ctx == nil {
		return unavailable()
	}
	return client.do(ctx, method, path, input, output)
}

func daemonDo(ctx context.Context, method, path string, input, output any) error {
	transport, err := daemontransport.NewClient()
	if err != nil {
		return unavailable()
	}
	request, err := transport.NewRequest(ctx, method, path, input)
	if err != nil {
		return unavailable()
	}
	response, err := transport.HTTP.Do(request)
	if err != nil {
		return unavailable()
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return invalidResponse()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeRemoteError(response.StatusCode, body)
	}
	if output == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return invalidResponse()
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return invalidResponse()
	}
	return nil
}

func decodeRemoteError(status int, body []byte) error {
	var envelope struct {
		Schema string `json:"schema"`
		Error  struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Schema != coreauth.SchemaV1 || envelope.Error.Code == "" {
		return &coreauth.Error{Code: "auth_unavailable", Message: "Meadow authorization request was rejected", HTTPStatus: status}
	}
	return &coreauth.Error{Code: envelope.Error.Code, Message: safeMessage(envelope.Error.Message), HTTPStatus: status, Retryable: envelope.Error.Retryable}
}

func safeMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" || len(message) > 240 || strings.Contains(strings.ToLower(message), "token") {
		return "Meadow authorization request was rejected"
	}
	return message
}

func opaqueID() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func invalidResponse() error {
	return &coreauth.Error{Code: "invalid_server_response", Message: "Meadow authorization response is invalid", HTTPStatus: http.StatusBadGateway}
}
func unavailable() error {
	return &coreauth.Error{Code: "auth_unavailable", Message: "Meadow authorization service is unavailable", HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
}

func ErrorStatus(err error) int {
	var authErr *coreauth.Error
	if errors.As(err, &authErr) && authErr.HTTPStatus >= 400 && authErr.HTTPStatus <= 599 {
		return authErr.HTTPStatus
	}
	return http.StatusServiceUnavailable
}

func PublicError(err error) map[string]any {
	code, message, retryable := coreauth.ErrorCode(err), "Meadow authorization service is unavailable", false
	var authErr *coreauth.Error
	if errors.As(err, &authErr) {
		message, retryable = safeMessage(authErr.Message), authErr.Retryable
	}
	return map[string]any{"schema": coreauth.SchemaV1, "error": map[string]any{"code": code, "message": message, "retryable": retryable}}
}

func PrintStatus(output io.Writer, status coreauth.Status) {
	fmt.Fprintf(output, "state: %s\nplan: %s\n", status.State, status.Plan)
	if names := status.Capabilities.EnabledNames(); len(names) != 0 {
		fmt.Fprintf(output, "capabilities: %s\n", strings.Join(names, ","))
	}
}
