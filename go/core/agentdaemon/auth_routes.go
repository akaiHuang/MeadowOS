package agentdaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/productauth"
)

const authInspectionBytes = 2 << 20

type ProductAuthService interface {
	Status(context.Context) (productauth.Status, error)
	Start(context.Context, productauth.StartRequest) (productauth.StartResult, error)
	Poll(context.Context, productauth.PollRequest) (productauth.PollResult, error)
	Refresh(context.Context, productauth.RefreshRequest) (productauth.Status, error)
	Logout(context.Context, productauth.LogoutRequest) error
	Redeem(context.Context, productauth.RedeemRequest) (productauth.RedeemResult, error)
	ListDevices(context.Context, productauth.DeviceListRequest) (productauth.DeviceListResult, error)
	RevokeDevice(context.Context, productauth.DeviceRevokeRequest) (productauth.DeviceRevokeResult, error)
	Authorize(context.Context, string) error
	IsProModel(string) bool
}

type productAuthRoutes struct {
	service                 ProductAuthService
	onProRevoked            func()
	onStatus                func(productauth.Status)
	accountStatus           func(context.Context) (productauth.Status, error)
	accountStatusForRequest func(*http.Request) (productauth.Status, error)
}

func newProductAuthRoutes(service ProductAuthService, onProRevoked ...func()) *productAuthRoutes {
	routes := &productAuthRoutes{service: service}
	if len(onProRevoked) != 0 {
		routes.onProRevoked = onProRevoked[0]
	}
	return routes
}

func (routes *productAuthRoutes) register(mux *http.ServeMux) {
	if service, ok := routes.service.(ModelAccessService); ok {
		registerModelAccessRoutes(mux, service)
	}
	mux.HandleFunc("GET /auth/status", routes.handleStatus)
	mux.HandleFunc("POST /auth/start", routes.handleStart)
	mux.HandleFunc("POST /auth/poll", routes.handlePoll)
	mux.HandleFunc("POST /auth/refresh", routes.handleRefresh)
	mux.HandleFunc("POST /auth/logout", routes.handleLogout)
	mux.HandleFunc("GET /entitlement/status", routes.handleStatus)
	mux.HandleFunc("POST /license/redeem", routes.handleRedeem)
	mux.HandleFunc("POST /devices/list", routes.handleDeviceList)
	mux.HandleFunc("POST /devices/revoke", routes.handleDeviceRevoke)
}

func (routes *productAuthRoutes) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if authExemptPath(request.URL.Path) {
			next.ServeHTTP(writer, request)
			return
		}
		if err := routes.service.Authorize(request.Context(), routes.requiredCapability(request)); err != nil {
			if !authorizationUpgradeOnly(err) {
				routes.closeProRuntime()
			}
			writeProductAuthError(writer, err)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// AccountAuthenticationError validates the account proof returned by the native
// product-auth service. General is a free verified account; paid capabilities
// are checked separately by the protected-product admission policy.
// The caller must obtain status from its trusted service, never request JSON.
func AccountAuthenticationError(status productauth.Status, statusErr error) error {
	if statusErr != nil {
		var authErr *productauth.Error
		if errors.As(statusErr, &authErr) {
			return statusErr
		}
		return &productauth.Error{Code: "auth_unavailable", Message: "account verification is unavailable", HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
	}
	if err := status.Validate(); err != nil {
		return &productauth.Error{Code: "invalid_auth_state", Message: "account verification is unavailable", HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
	}
	if !status.Authenticated || (status.State != productauth.StateAuthorizedGeneral && status.State != productauth.StateAuthorizedPro) {
		return &productauth.Error{Code: "login_required", Message: "Meadow login is required", HTTPStatus: http.StatusUnauthorized}
	}
	// The trusted service verifies signatures and device binding. Recheck its
	// effective deadline at admission so a response that expires in transit
	// cannot admit work. EffectiveExpiresAt includes the signed offline grace;
	// AccessExpiresAt alone would incorrectly reject that existing policy.
	if !status.Entitlement.EffectiveExpiresAt.After(time.Now()) {
		return &productauth.Error{Code: "login_required", Message: "Meadow login is required", HTTPStatus: http.StatusUnauthorized}
	}
	return nil
}

func (routes *productAuthRoutes) requireAccount(ctx context.Context) error {
	read := routes.service.Status
	if routes.accountStatus != nil {
		read = routes.accountStatus
	}
	status, err := read(ctx)
	return AccountAuthenticationError(status, err)
}

func (routes *productAuthRoutes) requireAccountForRequest(request *http.Request) error {
	if routes.accountStatusForRequest != nil {
		status, err := routes.accountStatusForRequest(request)
		return AccountAuthenticationError(status, err)
	}
	return routes.requireAccount(request.Context())
}

func (routes *productAuthRoutes) protectAccount(next http.Handler) http.Handler {
	return routes.protectAccountDelegating(next, nil)
}

// Delegation is composition-owned and applies only where another native route
// gate performs the same fresh account proof before touching protected state.
// It is not an exemption, cached status, or a client-supplied authorization.
func (routes *productAuthRoutes) protectAccountDelegating(next http.Handler, delegated func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !AccountAuthenticationExempt(request) && (delegated == nil || !delegated(request)) {
			if err := routes.requireAccountForRequest(request); err != nil {
				writeProductAuthError(writer, err)
				return
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func sameNativeMembershipService(auth ProductAuthService, membership CommercialMembershipService) bool {
	left, right := reflect.ValueOf(auth), reflect.ValueOf(membership)
	// Only one concrete service instance can own both proofs. Value wrappers,
	// aliases with another dynamic type, and typed nils retain both guards.
	return left.IsValid() && right.IsValid() && left.Kind() == reflect.Pointer && right.Kind() == reflect.Pointer &&
		!left.IsNil() && !right.IsNil() && left.Type() == right.Type() && left.Pointer() == right.Pointer()
}

// AccountAuthenticationExempt keeps bootstrap and existing work cancellation reachable after a
// sign-out. Exact methods and route shapes prevent new /auth/* or */cancel
// operations from silently inheriting an authentication exemption. Startup
// compositions use the same allowlist before creating any work runtime.
func AccountAuthenticationExempt(request *http.Request) bool {
	path := request.URL.Path
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return path == "/health" || path == "/auth/status" || path == "/entitlement/status" || (request.Method == http.MethodGet && path == "/commercial/prompt-grant-capability")
	}
	if request.Method != http.MethodPost {
		return false
	}
	switch path {
	case "/auth/start", "/auth/poll", "/auth/refresh", "/auth/logout", "/agent/browser/cancel":
		return true
	}
	parts := strings.Split(path, "/")
	return len(parts) == 4 && parts[0] == "" && parts[1] == "session" &&
		parts[2] != "" && parts[2] != "." && parts[2] != ".." && parts[3] == "cancel"
}

func (routes *productAuthRoutes) handleStatus(writer http.ResponseWriter, request *http.Request) {
	status, err := routes.service.Status(request.Context())
	if err != nil {
		writeProductAuthError(writer, err)
		return
	}
	if !statusKeepsCurrentModel(status) {
		routes.closeProRuntime()
	}
	if routes.onStatus != nil {
		routes.onStatus(status)
	}
	writeJSON(writer, http.StatusOK, status)
}

func (routes *productAuthRoutes) handleStart(writer http.ResponseWriter, request *http.Request) {
	var input productauth.StartRequest
	if decodeAuthJSON(request, &input) != nil {
		writeProductAuthError(writer, &productauth.Error{Code: "invalid_request", Message: "authentication request is invalid", HTTPStatus: http.StatusBadRequest})
		return
	}
	result, err := routes.service.Start(request.Context(), input)
	if err != nil {
		writeProductAuthError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (routes *productAuthRoutes) handlePoll(writer http.ResponseWriter, request *http.Request) {
	var input productauth.PollRequest
	if decodeAuthJSON(request, &input) != nil {
		writeProductAuthError(writer, &productauth.Error{Code: "invalid_request", Message: "authentication poll is invalid", HTTPStatus: http.StatusBadRequest})
		return
	}
	result, err := routes.service.Poll(request.Context(), input)
	if err != nil {
		writeProductAuthError(writer, err)
		return
	}
	if result.Status != nil && !statusKeepsCurrentModel(*result.Status) {
		routes.closeProRuntime()
	}
	if result.Status != nil && routes.onStatus != nil {
		routes.onStatus(*result.Status)
	}
	writeJSON(writer, http.StatusOK, result)
}

func (routes *productAuthRoutes) handleRefresh(writer http.ResponseWriter, request *http.Request) {
	var input productauth.RefreshRequest
	if decodeAuthJSON(request, &input) != nil {
		writeProductAuthError(writer, &productauth.Error{Code: "invalid_request", Message: "refresh request is invalid", HTTPStatus: http.StatusBadRequest})
		return
	}
	status, err := routes.service.Refresh(request.Context(), input)
	if err != nil {
		routes.closeProRuntime()
		writeProductAuthError(writer, err)
		return
	}
	if !statusKeepsCurrentModel(status) {
		routes.closeProRuntime()
	}
	if routes.onStatus != nil {
		routes.onStatus(status)
	}
	writeJSON(writer, http.StatusOK, status)
}

func (routes *productAuthRoutes) closeProRuntime() {
	if routes.onProRevoked != nil {
		routes.onProRevoked()
	}
}

func statusKeepsCurrentModel(status productauth.Status) bool {
	if status.State != productauth.StateAuthorizedGeneral && status.State != productauth.StateAuthorizedPro {
		return false
	}
	return status.Authenticated && status.Capabilities.Enabled(productauth.CapabilityModel12B)
}

func authorizationUpgradeOnly(err error) bool {
	var authErr *productauth.Error
	return errors.As(err, &authErr) && authErr.Code == "upgrade_required"
}

func (routes *productAuthRoutes) handleLogout(writer http.ResponseWriter, request *http.Request) {
	var input productauth.LogoutRequest
	if decodeAuthJSON(request, &input) != nil {
		writeProductAuthError(writer, &productauth.Error{Code: "invalid_request", Message: "logout request is invalid", HTTPStatus: http.StatusBadRequest})
		return
	}
	routes.closeProRuntime()
	if err := routes.service.Logout(request.Context(), input); err != nil {
		writeProductAuthError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"schema": productauth.SchemaV1, "ok": true})
}

func (routes *productAuthRoutes) handleRedeem(writer http.ResponseWriter, request *http.Request) {
	var input productauth.RedeemRequest
	if decodeAuthJSON(request, &input) != nil {
		writeProductAuthError(writer, &productauth.Error{Code: "invalid_request", Message: "license redemption request is invalid", HTTPStatus: http.StatusBadRequest})
		return
	}
	result, err := routes.service.Redeem(request.Context(), input)
	if err != nil {
		writeProductAuthError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (routes *productAuthRoutes) handleDeviceList(writer http.ResponseWriter, request *http.Request) {
	var input productauth.DeviceListRequest
	if decodeAuthJSON(request, &input) != nil {
		writeProductAuthError(writer, &productauth.Error{Code: "invalid_request", Message: "device list request is invalid", HTTPStatus: http.StatusBadRequest})
		return
	}
	result, err := routes.service.ListDevices(request.Context(), input)
	if err != nil {
		writeProductAuthError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (routes *productAuthRoutes) handleDeviceRevoke(writer http.ResponseWriter, request *http.Request) {
	var input productauth.DeviceRevokeRequest
	if decodeAuthJSON(request, &input) != nil {
		writeProductAuthError(writer, &productauth.Error{Code: "invalid_request", Message: "device revoke request is invalid", HTTPStatus: http.StatusBadRequest})
		return
	}
	result, err := routes.service.RevokeDevice(request.Context(), input)
	if err != nil {
		writeProductAuthError(writer, err)
		return
	}
	if !statusKeepsCurrentModel(result.Availability) {
		routes.closeProRuntime()
	}
	writeJSON(writer, http.StatusOK, result)
}

func (routes *productAuthRoutes) requiredCapability(request *http.Request) string {
	if strings.HasPrefix(request.URL.Path, "/agent/browser/") {
		return productauth.CapabilityBrowser
	}
	if strings.HasPrefix(request.URL.Path, "/agent/models/install") ||
		strings.HasPrefix(request.URL.Path, "/agent/models/remove") ||
		strings.HasPrefix(request.URL.Path, "/agent/models/import") {
		return productauth.AdmissionPro
	}
	if request.Body == nil || !proSensitivePath(request.URL.Path) {
		return productauth.AdmissionGeneral
	}
	prefix, err := io.ReadAll(io.LimitReader(request.Body, authInspectionBytes+1))
	request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), request.Body))
	if err != nil || len(prefix) > authInspectionBytes {
		return productauth.AdmissionPro
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(prefix))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return productauth.AdmissionPro
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return productauth.AdmissionPro
	}
	if request.URL.Path == "/agent/model" {
		root, ok := document.(map[string]any)
		modelID, modelOK := root["model"].(string)
		if !ok || !modelOK || strings.TrimSpace(modelID) == "" {
			return productauth.AdmissionPro
		}
		if routes.service.IsProModel(modelID) {
			return productauth.AdmissionPro
		}
		return productauth.AdmissionGeneral
	}
	if proRequested(document) || routes.containsProModel(document) {
		return productauth.AdmissionPro
	}
	return productauth.AdmissionGeneral
}

func (routes *productAuthRoutes) containsProModel(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "modelId" {
				if modelID, ok := child.(string); ok && routes.service.IsProModel(modelID) {
					return true
				}
			}
			if routes.containsProModel(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if routes.containsProModel(child) {
				return true
			}
		}
	}
	return false
}

func proRequested(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "twoPass" || key == "two_pass" {
				if enabled, ok := child.(bool); ok && enabled {
					return true
				}
			}
			if key == "mode" {
				if mode, ok := child.(string); ok && (mode == "research" || mode == "pro") {
					return true
				}
			}
			if proRequested(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if proRequested(child) {
				return true
			}
		}
	}
	return false
}

func authExemptPath(path string) bool {
	return path == "/health" || strings.HasPrefix(path, "/auth/") || path == "/entitlement/status" ||
		path == "/license/redeem" || strings.HasPrefix(path, "/devices/") || strings.HasSuffix(path, "/cancel")
}

func proSensitivePath(path string) bool {
	return path == "/agent/model" || strings.Contains(path, "/models/install") || strings.HasSuffix(path, "/model") ||
		strings.HasSuffix(path, "/prompt") || strings.HasSuffix(path, "/inference-controls")
}

func decodeAuthJSON(request *http.Request, output any) error {
	if request.Body == nil {
		return errors.New("request body is required")
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func writeProductAuthError(writer http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	code, message, retryable := productauth.ErrorCode(err), "authorization service is unavailable", false
	var authErr *productauth.Error
	if errors.As(err, &authErr) {
		if authErr.HTTPStatus >= 400 && authErr.HTTPStatus <= 599 {
			status = authErr.HTTPStatus
		}
		message, retryable = authErr.Message, authErr.Retryable
	}
	writeJSON(writer, status, map[string]any{
		"schema": productauth.SchemaV1,
		"error":  map[string]any{"code": code, "message": message, "retryable": retryable},
	})
}
