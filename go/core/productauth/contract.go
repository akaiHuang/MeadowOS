// Package productauth owns the only native product authorization contract used
// by Firebase, the local Agent, CLI, and Desktop proxy.
package productauth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sort"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/licensing"
)

const (
	SchemaV1       = "meadow.product-auth.v1"
	LeaseTypeV1    = "meadow-license-lease+jwt"
	LeaseAudience  = "meadow-cli"
	LeaseProduct   = "meadow"
	LeaseStatusOK  = "authorized"
	LeaseVersionV1 = uint64(1)

	CapabilityModel12B       = "model.12b"
	CapabilityModel27B       = "model.27b"
	CapabilityModel30B       = "model.30b"
	CapabilityModel35B       = "model.35b"
	CapabilityVision         = "vision"
	CapabilityAgentAuto      = "agent.auto"
	CapabilityBrowser        = "browser.interact"
	CapabilityDelegation     = "delegation"
	CapabilityCommercialUse  = "commercial_use"
	CapabilityDevicesMaximum = "devices.max"

	AdmissionGeneral = "admission.general"
	AdmissionPro     = "admission.pro"
)

type Plan string

const (
	PlanGuest   Plan = "guest"
	PlanGeneral Plan = "general"
	PlanPro     Plan = "pro"
)

type AuthorizationState string

const (
	StateGuest             AuthorizationState = "guest"
	StateLoginRequired     AuthorizationState = "login_required"
	StateLoginPending      AuthorizationState = "login_pending"
	StateAuthorizedGeneral AuthorizationState = "authorized_general"
	StateAuthorizedPro     AuthorizationState = "authorized_pro"
	StateLoginDenied       AuthorizationState = "login_denied"
	StateLoginExpired      AuthorizationState = "login_expired"
	StateSlowDown          AuthorizationState = "slow_down"
	StateLoggedOut         AuthorizationState = "logged_out"
)

type LoginMode string

const (
	LoginModeBrowser    LoginMode = "browser"
	LoginModeDeviceCode LoginMode = "device_code"
)

// Capabilities is deliberately closed. A new capability requires a new
// product-auth schema instead of being silently accepted by an old binary.
type Capabilities struct {
	Model12B      bool `json:"model.12b"`
	Model27B      bool `json:"model.27b"`
	Model30B      bool `json:"model.30b"`
	Model35B      bool `json:"model.35b"`
	Vision        bool `json:"vision"`
	AgentAuto     bool `json:"agent.auto"`
	Browser       bool `json:"browser.interact"`
	Delegation    bool `json:"delegation"`
	CommercialUse bool `json:"commercial_use"`
	DevicesMax    int  `json:"devices.max"`
}

func GuestCapabilities() Capabilities { return Capabilities{} }

func GeneralCapabilities() Capabilities {
	return Capabilities{Model12B: true, DevicesMax: 3}
}

func ProCapabilities() Capabilities {
	return Capabilities{
		Model12B: true, Model27B: true, Model30B: true, Model35B: true,
		Vision: true, AgentAuto: true, Browser: true, Delegation: true,
		CommercialUse: true, DevicesMax: 3,
	}
}

func (capabilities Capabilities) Validate(plan Plan) error {
	var expected Capabilities
	switch plan {
	case PlanGuest:
		expected = GuestCapabilities()
	case PlanGeneral:
		expected = GeneralCapabilities()
		// Existing signed v1 leases retain their original one-device quota
		// until a verified refresh replaces them. Accept only that exact
		// historical capability set; never promote an unsigned quota.
		if capabilities.DevicesMax == 1 {
			expected.DevicesMax = 1
		}
	case PlanPro:
		expected = ProCapabilities()
	default:
		return errors.New("productauth: unsupported authorization plan")
	}
	if capabilities != expected {
		return errors.New("productauth: capability set does not match plan")
	}
	return nil
}

func (capabilities Capabilities) Enabled(name string) bool {
	switch name {
	case CapabilityModel12B:
		return capabilities.Model12B
	case CapabilityModel27B:
		return capabilities.Model27B
	case CapabilityModel30B:
		return capabilities.Model30B
	case CapabilityModel35B:
		return capabilities.Model35B
	case CapabilityVision:
		return capabilities.Vision
	case CapabilityAgentAuto:
		return capabilities.AgentAuto
	case CapabilityBrowser:
		return capabilities.Browser
	case CapabilityDelegation:
		return capabilities.Delegation
	case CapabilityCommercialUse:
		return capabilities.CommercialUse
	default:
		return false
	}
}

func (capabilities Capabilities) EnabledNames() []string {
	result := make([]string, 0, 9)
	for _, name := range []string{
		CapabilityModel12B, CapabilityModel27B, CapabilityModel30B, CapabilityModel35B,
		CapabilityVision, CapabilityAgentAuto, CapabilityBrowser, CapabilityDelegation,
		CapabilityCommercialUse,
	} {
		if capabilities.Enabled(name) {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}

// Status is the stable Desktop and CLI view. Credentials, device proof keys,
// and the compact signed lease are never exposed.
type Status struct {
	Schema          string             `json:"schema"`
	State           AuthorizationState `json:"state"`
	Plan            Plan               `json:"plan"`
	Authenticated   bool               `json:"authenticated"`
	Capabilities    Capabilities       `json:"capabilities"`
	Subject         string             `json:"subject,omitempty"`
	AccessExpiresAt *time.Time         `json:"access_expires_at,omitempty"`
	Entitlement     *EntitlementStatus `json:"lease,omitempty"`
	Reason          string             `json:"reason,omitempty"`
}

type EntitlementStatus struct {
	LeaseID            string    `json:"lease_id"`
	DeviceID           string    `json:"device_id"`
	EffectiveExpiresAt time.Time `json:"effective_expires_at"`
	RevocationEpoch    uint64    `json:"revocation_epoch"`
}

func (status Status) Validate() error {
	if status.Schema != SchemaV1 {
		return errors.New("productauth: unsupported status schema")
	}
	switch status.State {
	case StateGuest, StateLoginRequired:
		if status.Plan != PlanGuest || status.Authenticated || status.Entitlement != nil || status.Capabilities.Validate(PlanGuest) != nil {
			return errors.New("productauth: invalid unauthenticated status")
		}
	case StateAuthorizedGeneral:
		if status.Plan != PlanGeneral || !status.Authenticated || status.Entitlement == nil || status.Capabilities.Validate(PlanGeneral) != nil {
			return errors.New("productauth: invalid General status")
		}
	case StateAuthorizedPro:
		if status.Plan != PlanPro || !status.Authenticated || status.Entitlement == nil || status.Capabilities.Validate(PlanPro) != nil {
			return errors.New("productauth: invalid Pro status")
		}
	default:
		return errors.New("productauth: unsupported status state")
	}
	if status.Entitlement != nil && (status.Entitlement.LeaseID == "" || status.Entitlement.DeviceID == "" || status.Entitlement.EffectiveExpiresAt.IsZero()) {
		return errors.New("productauth: invalid lease status")
	}
	return nil
}

type StartRequest struct {
	Schema string    `json:"schema"`
	Mode   LoginMode `json:"mode"`
}

type StartResult struct {
	Schema                  string             `json:"schema"`
	State                   AuthorizationState `json:"state"`
	TransactionID           string             `json:"transaction_id"`
	VerificationURI         string             `json:"verification_uri"`
	VerificationURIComplete string             `json:"verification_uri_complete,omitempty"`
	UserCode                string             `json:"user_code"`
	ExpiresIn               int                `json:"expires_in"`
	Interval                int                `json:"interval"`
}

type PollRequest struct {
	Schema        string `json:"schema"`
	TransactionID string `json:"transaction_id"`
}

type PollResult struct {
	Schema    string             `json:"schema"`
	State     AuthorizationState `json:"state"`
	Interval  int                `json:"interval,omitempty"`
	ErrorCode string             `json:"error_code,omitempty"`
	Status    *Status            `json:"status,omitempty"`
}

type RefreshRequest struct {
	Schema string `json:"schema"`
}

type LogoutRequest struct {
	Schema string `json:"schema"`
}

type RedeemRequest struct {
	Schema         string `json:"schema"`
	Serial         string `json:"serial"`
	RequestID      string `json:"request_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type RedeemReceipt struct {
	RequestID string `json:"request_id"`
	ReceiptID string `json:"receipt_id"`
	Status    string `json:"status"`
}

type RedeemResult struct {
	Schema       string        `json:"schema"`
	Availability Status        `json:"availability"`
	Receipt      RedeemReceipt `json:"receipt"`
}

type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	Status     string     `json:"status"`
	Current    bool       `json:"current"`
	CreatedAt  *time.Time `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

type DeviceListRequest struct {
	Schema string `json:"schema"`
}

type DeviceListResult struct {
	Schema          string   `json:"schema"`
	CurrentDeviceID string   `json:"current_device_id"`
	ActiveDevices   int      `json:"active_devices"`
	MaximumDevices  int      `json:"maximum_devices"`
	Devices         []Device `json:"devices"`
}

type DeviceRevokeRequest struct {
	Schema         string `json:"schema"`
	DeviceID       string `json:"device_id"`
	RequestID      string `json:"request_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Reason         string `json:"reason"`
	Confirmed      bool   `json:"confirmed"`
}

type DeviceRevokeReceipt struct {
	RequestID string `json:"request_id"`
	ReceiptID string `json:"receipt_id"`
	AuditID   string `json:"audit_id"`
	DeviceID  string `json:"device_id"`
	Status    string `json:"status"`
	Changed   bool   `json:"changed"`
}

type DeviceRevokeResult struct {
	Schema       string              `json:"schema"`
	Availability Status              `json:"availability"`
	Receipt      DeviceRevokeReceipt `json:"receipt"`
}

type Error struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
	cause      error
}

func (err *Error) Error() string {
	if err == nil || err.Message == "" {
		return "product authorization failed"
	}
	return err.Message
}

func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func ErrorCode(err error) string {
	var authErr *Error
	if errors.As(err, &authErr) && authErr.Code != "" {
		return authErr.Code
	}
	return "auth_unavailable"
}

var ErrRefreshNotFound = errors.New("productauth: refresh credential not found")
var ErrLeaseNotFound = errors.New("productauth: signed lease not found")

// SecureStateStore atomically persists the refresh credential and its matching
// signed lease. ClearAuthorization retains monotonic anti-rollback state.
type SecureStateStore interface {
	licensing.SecureStore
	licensing.AntiRollbackStore
	LoadRefreshToken(context.Context) (string, error)
	LoadLease(context.Context) (string, error)
	StoreAuthorization(context.Context, string, string) error
	LoadDenial(context.Context) (string, error)
	StoreDenial(context.Context, string) error
	ClearDenial(context.Context) error
	ClearAuthorization(context.Context) error
}

type DeviceBindingProvider interface {
	DeviceBinding(context.Context) (licensing.DeviceBinding, error)
}

type DeviceAuthorizationStart struct {
	CodeChallenge   string
	DevicePublicKey string
	InstallationID  string
	Platform        string
	ClientName      string
	RequestID       string
}

type RemoteStart struct {
	TransactionID           string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresAt               time.Time
	RetryAfter              time.Duration
}

type DeviceAuthorizationPoll struct {
	TransactionID  string
	CodeVerifier   string
	DeviceProof    string
	InstallationID string
	RequestID      string
}

type RemotePoll struct {
	State      AuthorizationState
	RetryAfter time.Duration
	ErrorCode  string
	Grant      *RemoteGrant
}

type RemoteGrant struct {
	State       AuthorizationState
	AccessToken string
	// AccessExpiresAt is the bearer deadline from the validated server response;
	// the independently signed lease may authorize local use for longer.
	AccessExpiresAt time.Time
	RefreshToken    string
	Lease           string
	KeyID           string
	Plan            Plan
	Capabilities    Capabilities
}

type RemoteRefreshRequest struct {
	RefreshToken   string
	InstallationID string
	RequestID      string
	DeviceProof    string
}

type RemoteLogoutRequest struct {
	RefreshToken   string
	InstallationID string
	RequestID      string
	DeviceProof    string
}

type RemoteRedeem struct {
	State        AuthorizationState
	Lease        string
	KeyID        string
	Plan         Plan
	Capabilities Capabilities
	Receipt      RedeemReceipt
}

type RemoteRedeemRequest struct {
	AccessToken    string
	Serial         string
	RequestID      string
	IdempotencyKey string
	InstallationID string
	DeviceProof    string
}

type RemoteDeviceListRequest struct {
	AccessToken    string
	RequestID      string
	InstallationID string
	DeviceProof    string
}

type RemoteDeviceRevokeRequest struct {
	AccessToken    string
	DeviceID       string
	RequestID      string
	IdempotencyKey string
	Reason         string
	Confirmed      bool
	InstallationID string
	DeviceProof    string
}

type RemoteDeviceRevoke struct {
	State        AuthorizationState
	Lease        string
	KeyID        string
	Plan         Plan
	Capabilities Capabilities
	Receipt      DeviceRevokeReceipt
}

// AuthClient accepts no URL, audience override, keyring, path, or caller
// supplied device identity.
type AuthClient interface {
	Issuer() string
	Audience() string
	LeasePublicKeys() map[string]ed25519.PublicKey
	Start(context.Context, DeviceAuthorizationStart) (RemoteStart, error)
	Poll(context.Context, DeviceAuthorizationPoll) (RemotePoll, error)
	Refresh(context.Context, RemoteRefreshRequest) (RemoteGrant, error)
	Logout(context.Context, RemoteLogoutRequest) error
	Redeem(context.Context, RemoteRedeemRequest) (RemoteRedeem, error)
	ListDevices(context.Context, RemoteDeviceListRequest) (DeviceListResult, error)
	RevokeDevice(context.Context, RemoteDeviceRevokeRequest) (RemoteDeviceRevoke, error)
}

type BuildConfig struct {
	Product                string
	LeaseVersion           uint64
	LeasePublicKeys        map[string]ed25519.PublicKey
	MaxOfflineGrace        time.Duration
	FutureClockSkew        time.Duration
	ClockRollbackTolerance time.Duration
	ProModelIDs            []string
	InstallationID         string
	Platform               string
	ClientName             string
	Now                    func() time.Time
}

type Dependencies struct {
	Client        AuthClient
	Store         SecureStateStore
	DeviceBinding DeviceBindingProvider
	Random        func([]byte) error
}
