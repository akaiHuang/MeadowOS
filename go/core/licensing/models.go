package licensing

import (
	"encoding/json"
	"time"
)

type Plan string

const (
	PlanTrial    Plan = "trial"
	PlanMonthly  Plan = "monthly"
	PlanLifetime Plan = "lifetime"
)

type EntitlementStatus string

const (
	EntitlementActive  EntitlementStatus = "active"
	EntitlementExpired EntitlementStatus = "expired"
	EntitlementRevoked EntitlementStatus = "revoked"
)

type PurchaseKind string

const (
	PurchaseSubscription PurchaseKind = "subscription"
	PurchaseOneTime      PurchaseKind = "one_time"
)

type User struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	LastSeenAt      time.Time  `json:"last_seen_at"`
}

type Device struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	DeviceHash string     `json:"device_hash"`
	Name       string     `json:"name,omitempty"`
	FirstSeen  time.Time  `json:"first_seen_at"`
	LastSeen   time.Time  `json:"last_seen_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type Entitlement struct {
	ID           string            `json:"id"`
	UserID       string            `json:"user_id"`
	Plan         Plan              `json:"plan"`
	Status       EntitlementStatus `json:"status"`
	LicenseType  string            `json:"license_type"`
	ValidUntil   *time.Time        `json:"valid_until,omitempty"`
	OfflineUntil *time.Time        `json:"offline_until,omitempty"`
	MaxDevices   int               `json:"max_devices"`
	Source       string            `json:"source"`
	Notes        string            `json:"notes,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

type Purchase struct {
	ID                     string       `json:"id"`
	UserID                 string       `json:"user_id"`
	Provider               string       `json:"provider"`
	ProviderCustomerID     string       `json:"provider_customer_id,omitempty"`
	ProviderOrderID        string       `json:"provider_order_id,omitempty"`
	ProviderSubscriptionID string       `json:"provider_subscription_id,omitempty"`
	ProviderCaptureID      string       `json:"provider_capture_id,omitempty"`
	Plan                   Plan         `json:"plan"`
	Kind                   PurchaseKind `json:"kind"`
	Status                 string       `json:"status"`
	LicenseKey             string       `json:"license_key,omitempty"`
	LicenseKeyIssuedAt     *time.Time   `json:"license_key_issued_at,omitempty"`
	CurrentPeriodEnd       *time.Time   `json:"current_period_end,omitempty"`
	CreatedAt              time.Time    `json:"created_at"`
	UpdatedAt              time.Time    `json:"updated_at"`
}

type WebhookEvent struct {
	ID          string          `json:"id"`
	Provider    string          `json:"provider"`
	EventID     string          `json:"event_id"`
	EventType   string          `json:"event_type"`
	ProcessedAt time.Time       `json:"processed_at"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

type AuthCode struct {
	Email      string     `json:"email"`
	CodeHash   string     `json:"code_hash"`
	ExpiresAt  time.Time  `json:"expires_at"`
	ConsumedAt *time.Time `json:"consumed_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

type Session struct {
	Token     string    `json:"token"`
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

type AuditEvent struct {
	ID        string         `json:"id"`
	Actor     string         `json:"actor"`
	Action    string         `json:"action"`
	UserID    string         `json:"user_id,omitempty"`
	DeviceID  string         `json:"device_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

type State struct {
	Users         map[string]User         `json:"users"`
	Devices       map[string]Device       `json:"devices"`
	Entitlements  map[string]Entitlement  `json:"entitlements"`
	Purchases     map[string]Purchase     `json:"purchases"`
	AuthCodes     map[string]AuthCode     `json:"auth_codes"`
	Sessions      map[string]Session      `json:"sessions"`
	WebhookEvents map[string]WebhookEvent `json:"webhook_events"`
	AuditEvents   []AuditEvent            `json:"audit_events"`
}

func (s *State) ensure() {
	if s.Users == nil {
		s.Users = map[string]User{}
	}
	if s.Devices == nil {
		s.Devices = map[string]Device{}
	}
	if s.Entitlements == nil {
		s.Entitlements = map[string]Entitlement{}
	}
	if s.Purchases == nil {
		s.Purchases = map[string]Purchase{}
	}
	if s.AuthCodes == nil {
		s.AuthCodes = map[string]AuthCode{}
	}
	if s.Sessions == nil {
		s.Sessions = map[string]Session{}
	}
	if s.WebhookEvents == nil {
		s.WebhookEvents = map[string]WebhookEvent{}
	}
}

type PublicUser struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

func publicUser(u User) PublicUser {
	return PublicUser{
		ID:              u.ID,
		Email:           u.Email,
		EmailVerifiedAt: u.EmailVerifiedAt,
		CreatedAt:       u.CreatedAt,
	}
}
