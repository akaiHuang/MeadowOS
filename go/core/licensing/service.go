package licensing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"sort"
	"strings"
	"time"
)

type AppError struct {
	Code    string
	Message string
	Status  int
}

func (e AppError) Error() string {
	return e.Message
}

type ServiceConfig struct {
	Issuer          string
	TrialDays       int
	TrialMaxDevices int
	PaidMaxDevices  int
	OfflineDays     int
	CodeTTL         time.Duration
	SessionTTL      time.Duration
	Now             func() time.Time
}

type Service struct {
	store  Store
	signer *Signer
	cfg    ServiceConfig
}

type RequestCodeResult struct {
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expires_at"`
	DevCode   string    `json:"dev_code,omitempty"`
}

type VerifyCodeResult struct {
	AccessToken string       `json:"access_token"`
	ExpiresAt   time.Time    `json:"expires_at"`
	User        PublicUser   `json:"user"`
	Entitlement *Entitlement `json:"entitlement,omitempty"`
}

type ActivateDeviceResult struct {
	Device       Device       `json:"device"`
	Entitlement  Entitlement  `json:"entitlement"`
	LicenseToken string       `json:"license_token"`
	PublicKey    PublicKeyDTO `json:"public_key"`
}

type PublicKeyDTO struct {
	KeyID   string `json:"key_id"`
	Ed25519 string `json:"ed25519_base64"`
}

type CurrentEntitlementResult struct {
	User        PublicUser   `json:"user"`
	Entitlement *Entitlement `json:"entitlement,omitempty"`
	Devices     []Device     `json:"devices"`
}

type CurrentLicenseKeyResult struct {
	LicenseKey string `json:"license_key,omitempty"`
	Plan       Plan   `json:"plan,omitempty"`
	PurchaseID string `json:"purchase_id,omitempty"`
}

type AdminSummary struct {
	Users        []User        `json:"users"`
	Devices      []Device      `json:"devices"`
	Entitlements []Entitlement `json:"entitlements"`
	Purchases    []Purchase    `json:"purchases"`
	AuditEvents  []AuditEvent  `json:"audit_events"`
}

type GrantEntitlementInput struct {
	Email      string
	Plan       Plan
	Days       int
	MaxDevices int
	Source     string
	Notes      string
	Actor      string
}

type BeginPurchaseInput struct {
	UserID   string
	Provider string
	Plan     Plan
	Kind     PurchaseKind
}

type ActivateLicenseKeyInput struct {
	LicenseKey string
	DeviceHash string
	Name       string
}

type PayPalWebhookEvent struct {
	ID              string
	EventType       string
	OrderID         string
	SubscriptionID  string
	CaptureID       string
	ProviderPayerID string
	NextBillingTime *time.Time
	Payload         json.RawMessage
}

func NewService(store Store, signer *Signer, cfg ServiceConfig) *Service {
	if cfg.Issuer == "" {
		cfg.Issuer = "meadow-license"
	}
	if cfg.TrialDays <= 0 {
		cfg.TrialDays = 7
	}
	if cfg.TrialMaxDevices <= 0 {
		cfg.TrialMaxDevices = 1
	}
	if cfg.PaidMaxDevices <= 0 {
		cfg.PaidMaxDevices = 3
	}
	if cfg.OfflineDays <= 0 {
		cfg.OfflineDays = 30
	}
	if cfg.CodeTTL <= 0 {
		cfg.CodeTTL = 10 * time.Minute
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{store: store, signer: signer, cfg: cfg}
}

func (s *Service) RequestCode(ctx context.Context, email string) (RequestCodeResult, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return RequestCodeResult{}, err
	}
	code, err := randomDigits(6)
	if err != nil {
		return RequestCodeResult{}, err
	}
	now := s.now()
	expiresAt := now.Add(s.cfg.CodeTTL)
	err = s.store.Update(ctx, func(st *State) error {
		st.AuthCodes[email] = AuthCode{
			Email:     email,
			CodeHash:  hashCode(email, code),
			ExpiresAt: expiresAt,
			CreatedAt: now,
		}
		st.AuditEvents = append(st.AuditEvents, AuditEvent{
			ID:        randomID("aud"),
			Actor:     "system",
			Action:    "auth_code_requested",
			Details:   map[string]any{"email": email},
			CreatedAt: now,
		})
		return nil
	})
	if err != nil {
		return RequestCodeResult{}, err
	}
	return RequestCodeResult{Email: email, ExpiresAt: expiresAt, DevCode: code}, nil
}

func (s *Service) VerifyCode(ctx context.Context, email, code string) (VerifyCodeResult, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return VerifyCodeResult{}, err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return VerifyCodeResult{}, AppError{Code: "code_required", Message: "verification code is required", Status: 400}
	}
	now := s.now()
	var result VerifyCodeResult
	err = s.store.Update(ctx, func(st *State) error {
		authCode, ok := st.AuthCodes[email]
		if !ok || authCode.ConsumedAt != nil || now.After(authCode.ExpiresAt) || authCode.CodeHash != hashCode(email, code) {
			return AppError{Code: "invalid_code", Message: "verification code is invalid or expired", Status: 401}
		}
		authCode.ConsumedAt = &now
		st.AuthCodes[email] = authCode

		user := findUserByEmail(st, email)
		if user == nil {
			created := User{
				ID:              randomID("usr"),
				Email:           email,
				EmailVerifiedAt: &now,
				CreatedAt:       now,
				LastSeenAt:      now,
			}
			st.Users[created.ID] = created
			user = &created
		} else {
			u := *user
			u.EmailVerifiedAt = &now
			u.LastSeenAt = now
			st.Users[u.ID] = u
			user = &u
		}

		trial := ensureTrialEntitlement(st, user.ID, now, s.cfg.TrialDays, s.cfg.TrialMaxDevices)
		token := randomID("ses")
		expiresAt := now.Add(s.cfg.SessionTTL)
		st.Sessions[token] = Session{
			Token:     token,
			UserID:    user.ID,
			ExpiresAt: expiresAt,
			CreatedAt: now,
		}
		st.AuditEvents = append(st.AuditEvents, AuditEvent{
			ID:        randomID("aud"),
			Actor:     user.ID,
			Action:    "auth_code_verified",
			UserID:    user.ID,
			CreatedAt: now,
		})
		result = VerifyCodeResult{
			AccessToken: token,
			ExpiresAt:   expiresAt,
			User:        publicUser(*user),
			Entitlement: trial,
		}
		return nil
	})
	if err != nil {
		return VerifyCodeResult{}, err
	}
	return result, nil
}

func (s *Service) AuthenticateSession(ctx context.Context, token string) (User, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		return User{}, AppError{Code: "token_required", Message: "authorization token is required", Status: 401}
	}
	now := s.now()
	var user User
	err := s.store.View(ctx, func(st State) error {
		session, ok := st.Sessions[token]
		if !ok || now.After(session.ExpiresAt) {
			return AppError{Code: "invalid_token", Message: "authorization token is invalid or expired", Status: 401}
		}
		u, ok := st.Users[session.UserID]
		if !ok {
			return AppError{Code: "invalid_token_user", Message: "authorization token user no longer exists", Status: 401}
		}
		user = u
		return nil
	})
	return user, err
}

func (s *Service) ActivateDevice(ctx context.Context, userID, deviceHash, name string) (ActivateDeviceResult, error) {
	deviceHash = strings.TrimSpace(deviceHash)
	name = strings.TrimSpace(name)
	if len(deviceHash) < 16 {
		return ActivateDeviceResult{}, AppError{Code: "device_hash_invalid", Message: "device_hash must be at least 16 characters", Status: 400}
	}
	now := s.now()
	var result ActivateDeviceResult
	err := s.store.Update(ctx, func(st *State) error {
		user, ok := st.Users[userID]
		if !ok {
			return AppError{Code: "user_not_found", Message: "user not found", Status: 404}
		}
		ent := bestEntitlement(st, userID, now)
		if ent == nil {
			return AppError{Code: "no_entitlement", Message: "no active entitlement", Status: 402}
		}

		device := findDeviceByHash(st, userID, deviceHash)
		if device == nil {
			if activeDeviceCount(st, userID) >= ent.MaxDevices {
				return AppError{Code: "device_limit_reached", Message: "device limit reached", Status: 409}
			}
			created := Device{
				ID:         randomID("dev"),
				UserID:     userID,
				DeviceHash: deviceHash,
				Name:       name,
				FirstSeen:  now,
				LastSeen:   now,
			}
			st.Devices[created.ID] = created
			device = &created
		} else {
			d := *device
			if d.RevokedAt != nil && activeDeviceCount(st, userID) >= ent.MaxDevices {
				return AppError{Code: "device_limit_reached", Message: "device limit reached", Status: 409}
			}
			d.LastSeen = now
			if name != "" {
				d.Name = name
			}
			if d.RevokedAt != nil {
				d.RevokedAt = nil
			}
			st.Devices[d.ID] = d
			device = &d
		}

		tokenID := randomID("tok")
		expiresAt := tokenExpiry(*ent, now, s.cfg.OfflineDays)
		token, err := s.signer.Sign(TokenClaims{
			Issuer:        s.cfg.Issuer,
			Subject:       user.ID,
			UserID:        user.ID,
			Email:         user.Email,
			DeviceID:      device.ID,
			EntitlementID: ent.ID,
			Plan:          ent.Plan,
			LicenseType:   ent.LicenseType,
			ValidUntil:    ent.ValidUntil,
			MaxDevices:    ent.MaxDevices,
			IssuedAt:      now,
			ExpiresAt:     expiresAt,
			TokenID:       tokenID,
		})
		if err != nil {
			return err
		}
		st.AuditEvents = append(st.AuditEvents, AuditEvent{
			ID:        randomID("aud"),
			Actor:     userID,
			Action:    "device_activated",
			UserID:    userID,
			DeviceID:  device.ID,
			CreatedAt: now,
		})
		result = ActivateDeviceResult{
			Device:       *device,
			Entitlement:  *ent,
			LicenseToken: token,
			PublicKey:    s.PublicKey(),
		}
		return nil
	})
	if err != nil {
		return ActivateDeviceResult{}, err
	}
	return result, nil
}

func (s *Service) ActivateLicenseKey(ctx context.Context, input ActivateLicenseKeyInput) (ActivateDeviceResult, error) {
	key := normalizeLicenseKey(input.LicenseKey)
	if key == "" {
		return ActivateDeviceResult{}, AppError{Code: "license_key_required", Message: "license key is required", Status: 400}
	}
	now := s.now()
	var userID string
	err := s.store.View(ctx, func(st State) error {
		purchase, ok := findPurchaseByLicenseKey(&st, key)
		if !ok {
			return AppError{Code: "license_key_not_found", Message: "license key was not found", Status: 404}
		}
		if purchase.Status != "completed" && purchase.Status != "active" {
			return AppError{Code: "license_key_inactive", Message: "license key is not active yet", Status: 402}
		}
		ent := bestEntitlement(&st, purchase.UserID, now)
		if ent == nil {
			return AppError{Code: "license_key_no_entitlement", Message: "license key has no active entitlement", Status: 402}
		}
		userID = purchase.UserID
		return nil
	})
	if err != nil {
		return ActivateDeviceResult{}, err
	}
	result, err := s.ActivateDevice(ctx, userID, input.DeviceHash, input.Name)
	if err != nil {
		return ActivateDeviceResult{}, err
	}
	return result, nil
}

func (s *Service) CurrentEntitlement(ctx context.Context, userID string) (CurrentEntitlementResult, error) {
	now := s.now()
	var result CurrentEntitlementResult
	err := s.store.View(ctx, func(st State) error {
		user, ok := st.Users[userID]
		if !ok {
			return AppError{Code: "user_not_found", Message: "user not found", Status: 404}
		}
		ent := bestEntitlement(&st, userID, now)
		devices := devicesForUser(&st, userID)
		result = CurrentEntitlementResult{
			User:        publicUser(user),
			Entitlement: ent,
			Devices:     devices,
		}
		return nil
	})
	return result, err
}

func (s *Service) CurrentEntitlementForLicenseKey(ctx context.Context, licenseKey string) (CurrentEntitlementResult, error) {
	key := normalizeLicenseKey(licenseKey)
	if key == "" {
		return CurrentEntitlementResult{}, AppError{Code: "license_key_required", Message: "license key is required", Status: 400}
	}
	now := s.now()
	var result CurrentEntitlementResult
	err := s.store.View(ctx, func(st State) error {
		purchase, ok := findPurchaseByLicenseKey(&st, key)
		if !ok {
			return AppError{Code: "license_key_not_found", Message: "license key was not found", Status: 404}
		}
		if purchase.Status != "completed" && purchase.Status != "active" {
			return AppError{Code: "license_key_inactive", Message: "license key is not active yet", Status: 402}
		}
		user, ok := st.Users[purchase.UserID]
		if !ok {
			return AppError{Code: "user_not_found", Message: "user not found", Status: 404}
		}
		ent := bestEntitlement(&st, purchase.UserID, now)
		if ent == nil {
			return AppError{Code: "license_key_no_entitlement", Message: "license key has no active entitlement", Status: 402}
		}
		result = CurrentEntitlementResult{
			User:        publicUser(user),
			Entitlement: ent,
			Devices:     devicesForUser(&st, purchase.UserID),
		}
		return nil
	})
	return result, err
}

func (s *Service) CurrentLicenseKey(ctx context.Context, userID string) (CurrentLicenseKeyResult, error) {
	now := s.now()
	var result CurrentLicenseKeyResult
	err := s.store.View(ctx, func(st State) error {
		if _, ok := st.Users[userID]; !ok {
			return AppError{Code: "user_not_found", Message: "user not found", Status: 404}
		}
		ent := bestEntitlement(&st, userID, now)
		if ent == nil {
			return AppError{Code: "no_entitlement", Message: "no active entitlement", Status: 402}
		}
		var best *Purchase
		for _, purchase := range st.Purchases {
			if purchase.UserID != userID || purchase.Plan != ent.Plan || strings.TrimSpace(purchase.LicenseKey) == "" {
				continue
			}
			if purchase.Status != "completed" && purchase.Status != "active" {
				continue
			}
			candidate := purchase
			if best == nil || candidate.UpdatedAt.After(best.UpdatedAt) {
				best = &candidate
			}
		}
		if best == nil {
			return AppError{Code: "license_key_not_found", Message: "license key was not found for current entitlement", Status: 404}
		}
		result = CurrentLicenseKeyResult{
			LicenseKey: best.LicenseKey,
			Plan:       best.Plan,
			PurchaseID: best.ID,
		}
		return nil
	})
	return result, err
}

func (s *Service) GrantEntitlement(ctx context.Context, input GrantEntitlementInput) (Entitlement, error) {
	email, err := normalizeEmail(input.Email)
	if err != nil {
		return Entitlement{}, err
	}
	if input.Plan == "" {
		return Entitlement{}, AppError{Code: "plan_required", Message: "plan is required", Status: 400}
	}
	if input.Source == "" {
		input.Source = "admin"
	}
	if input.Actor == "" {
		input.Actor = "admin"
	}
	now := s.now()
	var granted Entitlement
	err = s.store.Update(ctx, func(st *State) error {
		user := findUserByEmail(st, email)
		if user == nil {
			created := User{
				ID:         randomID("usr"),
				Email:      email,
				CreatedAt:  now,
				LastSeenAt: now,
			}
			st.Users[created.ID] = created
			user = &created
		}
		maxDevices := input.MaxDevices
		if maxDevices <= 0 {
			if input.Plan == PlanTrial {
				maxDevices = s.cfg.TrialMaxDevices
			} else {
				maxDevices = s.cfg.PaidMaxDevices
			}
		}
		var validUntil *time.Time
		if input.Days > 0 {
			v := now.Add(time.Duration(input.Days) * 24 * time.Hour)
			validUntil = &v
		} else if input.Plan == PlanTrial {
			v := now.Add(time.Duration(s.cfg.TrialDays) * 24 * time.Hour)
			validUntil = &v
		}
		granted = Entitlement{
			ID:          randomID("ent"),
			UserID:      user.ID,
			Plan:        input.Plan,
			Status:      EntitlementActive,
			LicenseType: licenseType(input.Plan),
			ValidUntil:  validUntil,
			MaxDevices:  maxDevices,
			Source:      input.Source,
			Notes:       input.Notes,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		st.Entitlements[granted.ID] = granted
		st.AuditEvents = append(st.AuditEvents, AuditEvent{
			ID:        randomID("aud"),
			Actor:     input.Actor,
			Action:    "entitlement_granted",
			UserID:    user.ID,
			Details:   map[string]any{"plan": input.Plan, "source": input.Source},
			CreatedAt: now,
		})
		return nil
	})
	if err != nil {
		return Entitlement{}, err
	}
	return granted, nil
}

func (s *Service) BeginPurchase(ctx context.Context, input BeginPurchaseInput) (Purchase, error) {
	if strings.TrimSpace(input.Provider) == "" {
		return Purchase{}, AppError{Code: "provider_required", Message: "payment provider is required", Status: 400}
	}
	if input.Plan == "" {
		return Purchase{}, AppError{Code: "plan_required", Message: "plan is required", Status: 400}
	}
	if input.Kind == "" {
		return Purchase{}, AppError{Code: "purchase_kind_required", Message: "purchase kind is required", Status: 400}
	}
	now := s.now()
	var purchase Purchase
	err := s.store.Update(ctx, func(st *State) error {
		if _, ok := st.Users[input.UserID]; !ok {
			return AppError{Code: "user_not_found", Message: "user not found", Status: 404}
		}
		purchase = Purchase{
			ID:        randomID("pur"),
			UserID:    input.UserID,
			Provider:  strings.TrimSpace(input.Provider),
			Plan:      input.Plan,
			Kind:      input.Kind,
			Status:    "created",
			CreatedAt: now,
			UpdatedAt: now,
		}
		st.Purchases[purchase.ID] = purchase
		st.AuditEvents = append(st.AuditEvents, AuditEvent{
			ID:        randomID("aud"),
			Actor:     input.UserID,
			Action:    "purchase_created",
			UserID:    input.UserID,
			Details:   map[string]any{"provider": input.Provider, "plan": input.Plan, "kind": input.Kind},
			CreatedAt: now,
		})
		return nil
	})
	return purchase, err
}

func (s *Service) AttachPayPalOrder(ctx context.Context, purchaseID, orderID string) (Purchase, error) {
	return s.updatePurchase(ctx, purchaseID, func(p *Purchase, now time.Time) error {
		p.Provider = "paypal"
		p.ProviderOrderID = strings.TrimSpace(orderID)
		p.Status = "pending_approval"
		p.UpdatedAt = now
		return nil
	})
}

func (s *Service) AttachPayPalSubscription(ctx context.Context, purchaseID, subscriptionID string) (Purchase, error) {
	return s.updatePurchase(ctx, purchaseID, func(p *Purchase, now time.Time) error {
		p.Provider = "paypal"
		p.ProviderSubscriptionID = strings.TrimSpace(subscriptionID)
		p.Status = "pending_approval"
		p.UpdatedAt = now
		return nil
	})
}

func (s *Service) CompletePayPalOrder(ctx context.Context, orderID, captureID, providerPayerID string) (Purchase, Entitlement, error) {
	now := s.now()
	var purchase Purchase
	var entitlement Entitlement
	err := s.store.Update(ctx, func(st *State) error {
		p, ok := findPurchaseByPayPalOrderID(st, orderID)
		if !ok {
			return AppError{Code: "purchase_not_found", Message: "paypal order purchase not found", Status: 404}
		}
		if p.Status == "completed" {
			if err := issueLicenseKey(&p, now); err != nil {
				return err
			}
			st.Purchases[p.ID] = p
			purchase = p
			ent := bestEntitlement(st, p.UserID, now)
			if ent != nil {
				entitlement = *ent
			}
			return nil
		}
		if err := issueLicenseKey(&p, now); err != nil {
			return err
		}
		p.Status = "completed"
		p.ProviderCaptureID = strings.TrimSpace(captureID)
		p.ProviderCustomerID = strings.TrimSpace(providerPayerID)
		p.UpdatedAt = now
		st.Purchases[p.ID] = p
		entitlement = Entitlement{
			ID:          randomID("ent"),
			UserID:      p.UserID,
			Plan:        p.Plan,
			Status:      EntitlementActive,
			LicenseType: licenseType(p.Plan),
			MaxDevices:  s.cfg.PaidMaxDevices,
			Source:      "paypal:" + orderID,
			Notes:       "PayPal order captured",
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		st.Entitlements[entitlement.ID] = entitlement
		st.AuditEvents = append(st.AuditEvents, AuditEvent{
			ID:        randomID("aud"),
			Actor:     "paypal",
			Action:    "paypal_order_completed",
			UserID:    p.UserID,
			Details:   map[string]any{"order_id": orderID, "capture_id": captureID},
			CreatedAt: now,
		})
		purchase = p
		return nil
	})
	return purchase, entitlement, err
}

func (s *Service) ActivatePayPalSubscription(ctx context.Context, subscriptionID, providerPayerID string, nextBillingTime *time.Time) (Purchase, Entitlement, error) {
	now := s.now()
	var purchase Purchase
	var entitlement Entitlement
	err := s.store.Update(ctx, func(st *State) error {
		p, ok := findPurchaseByPayPalSubscriptionID(st, subscriptionID)
		if !ok {
			return AppError{Code: "purchase_not_found", Message: "paypal subscription purchase not found", Status: 404}
		}
		if err := issueLicenseKey(&p, now); err != nil {
			return err
		}
		p.Status = "active"
		p.ProviderCustomerID = strings.TrimSpace(providerPayerID)
		p.CurrentPeriodEnd = nextBillingTime
		p.UpdatedAt = now
		st.Purchases[p.ID] = p

		entitlement = upsertProviderEntitlement(st, p.UserID, "paypal:"+subscriptionID, Entitlement{
			UserID:      p.UserID,
			Plan:        PlanMonthly,
			Status:      EntitlementActive,
			LicenseType: "subscription",
			ValidUntil:  nextBillingTime,
			MaxDevices:  s.cfg.PaidMaxDevices,
			Source:      "paypal:" + subscriptionID,
			Notes:       "PayPal subscription active",
		}, now)
		st.AuditEvents = append(st.AuditEvents, AuditEvent{
			ID:        randomID("aud"),
			Actor:     "paypal",
			Action:    "paypal_subscription_active",
			UserID:    p.UserID,
			Details:   map[string]any{"subscription_id": subscriptionID},
			CreatedAt: now,
		})
		purchase = p
		return nil
	})
	return purchase, entitlement, err
}

func (s *Service) MarkPayPalSubscriptionInactive(ctx context.Context, subscriptionID, status string) error {
	now := s.now()
	return s.store.Update(ctx, func(st *State) error {
		p, ok := findPurchaseByPayPalSubscriptionID(st, subscriptionID)
		if !ok {
			return AppError{Code: "purchase_not_found", Message: "paypal subscription purchase not found", Status: 404}
		}
		p.Status = strings.TrimSpace(status)
		p.UpdatedAt = now
		st.Purchases[p.ID] = p
		for id, ent := range st.Entitlements {
			if ent.UserID == p.UserID && ent.Source == "paypal:"+subscriptionID {
				ent.Status = EntitlementExpired
				ent.UpdatedAt = now
				st.Entitlements[id] = ent
			}
		}
		st.AuditEvents = append(st.AuditEvents, AuditEvent{
			ID:        randomID("aud"),
			Actor:     "paypal",
			Action:    "paypal_subscription_" + status,
			UserID:    p.UserID,
			Details:   map[string]any{"subscription_id": subscriptionID},
			CreatedAt: now,
		})
		return nil
	})
}

func (s *Service) ProcessPayPalWebhook(ctx context.Context, event PayPalWebhookEvent) error {
	if strings.TrimSpace(event.ID) == "" {
		return AppError{Code: "webhook_event_id_required", Message: "paypal webhook event id is required", Status: 400}
	}
	key := "paypal:" + event.ID
	now := s.now()
	alreadyProcessed := false
	err := s.store.Update(ctx, func(st *State) error {
		if _, ok := st.WebhookEvents[key]; ok {
			alreadyProcessed = true
			return nil
		}
		st.WebhookEvents[key] = WebhookEvent{
			ID:          key,
			Provider:    "paypal",
			EventID:     event.ID,
			EventType:   event.EventType,
			ProcessedAt: now,
			Payload:     append(json.RawMessage(nil), event.Payload...),
		}
		return nil
	})
	if err != nil || alreadyProcessed {
		return err
	}

	switch event.EventType {
	case "PAYMENT.CAPTURE.COMPLETED":
		if event.OrderID == "" {
			return s.auditPaymentWebhook(ctx, "paypal_capture_completed_missing_order", map[string]any{"event_id": event.ID, "capture_id": event.CaptureID})
		}
		_, _, err := s.CompletePayPalOrder(ctx, event.OrderID, event.CaptureID, event.ProviderPayerID)
		return err
	case "BILLING.SUBSCRIPTION.ACTIVATED", "BILLING.SUBSCRIPTION.PAYMENT.COMPLETED":
		if event.SubscriptionID == "" {
			return s.auditPaymentWebhook(ctx, "paypal_subscription_missing_id", map[string]any{"event_id": event.ID, "event_type": event.EventType})
		}
		_, _, err := s.ActivatePayPalSubscription(ctx, event.SubscriptionID, event.ProviderPayerID, event.NextBillingTime)
		return err
	case "BILLING.SUBSCRIPTION.CANCELLED", "BILLING.SUBSCRIPTION.SUSPENDED", "BILLING.SUBSCRIPTION.EXPIRED":
		if event.SubscriptionID == "" {
			return nil
		}
		return s.MarkPayPalSubscriptionInactive(ctx, event.SubscriptionID, strings.ToLower(strings.TrimPrefix(event.EventType, "BILLING.SUBSCRIPTION.")))
	default:
		return s.auditPaymentWebhook(ctx, "paypal_webhook_ignored", map[string]any{"event_id": event.ID, "event_type": event.EventType})
	}
}

func (s *Service) RevokeDevice(ctx context.Context, deviceID, actor string) error {
	if actor == "" {
		actor = "admin"
	}
	now := s.now()
	return s.store.Update(ctx, func(st *State) error {
		return revokeDeviceInState(st, "", deviceID, actor, now)
	})
}

func (s *Service) RevokeDeviceForUser(ctx context.Context, userID, deviceID, actor string) error {
	if strings.TrimSpace(userID) == "" {
		return AppError{Code: "user_required", Message: "user is required", Status: 400}
	}
	if actor == "" {
		actor = userID
	}
	now := s.now()
	return s.store.Update(ctx, func(st *State) error {
		return revokeDeviceInState(st, userID, deviceID, actor, now)
	})
}

func (s *Service) RevokeDeviceForLicenseKey(ctx context.Context, licenseKey, deviceID string) error {
	key := normalizeLicenseKey(licenseKey)
	if key == "" {
		return AppError{Code: "license_key_required", Message: "license key is required", Status: 400}
	}
	now := s.now()
	return s.store.Update(ctx, func(st *State) error {
		purchase, ok := findPurchaseByLicenseKey(st, key)
		if !ok {
			return AppError{Code: "license_key_not_found", Message: "license key was not found", Status: 404}
		}
		if purchase.Status != "completed" && purchase.Status != "active" {
			return AppError{Code: "license_key_inactive", Message: "license key is not active yet", Status: 402}
		}
		return revokeDeviceInState(st, purchase.UserID, deviceID, purchase.UserID, now)
	})
}

func (s *Service) AdminSummary(ctx context.Context) (AdminSummary, error) {
	var summary AdminSummary
	err := s.store.View(ctx, func(st State) error {
		for _, u := range st.Users {
			summary.Users = append(summary.Users, u)
		}
		for _, d := range st.Devices {
			summary.Devices = append(summary.Devices, d)
		}
		for _, e := range st.Entitlements {
			summary.Entitlements = append(summary.Entitlements, e)
		}
		for _, p := range st.Purchases {
			summary.Purchases = append(summary.Purchases, p)
		}
		summary.AuditEvents = append(summary.AuditEvents, st.AuditEvents...)
		sort.Slice(summary.Users, func(i, j int) bool { return summary.Users[i].CreatedAt.After(summary.Users[j].CreatedAt) })
		sort.Slice(summary.Devices, func(i, j int) bool { return summary.Devices[i].LastSeen.After(summary.Devices[j].LastSeen) })
		sort.Slice(summary.Entitlements, func(i, j int) bool { return summary.Entitlements[i].CreatedAt.After(summary.Entitlements[j].CreatedAt) })
		sort.Slice(summary.Purchases, func(i, j int) bool { return summary.Purchases[i].CreatedAt.After(summary.Purchases[j].CreatedAt) })
		sort.Slice(summary.AuditEvents, func(i, j int) bool { return summary.AuditEvents[i].CreatedAt.After(summary.AuditEvents[j].CreatedAt) })
		return nil
	})
	return summary, err
}

func (s *Service) PublicKey() PublicKeyDTO {
	return PublicKeyDTO{KeyID: s.signer.KeyID(), Ed25519: s.signer.PublicKeyBase64()}
}

func (s *Service) now() time.Time {
	return s.cfg.Now().UTC().Truncate(time.Second)
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", AppError{Code: "email_required", Message: "email is required", Status: 400}
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || strings.ToLower(addr.Address) != email {
		return "", AppError{Code: "email_invalid", Message: "email is invalid", Status: 400}
	}
	return email, nil
}

func hashCode(email, code string) string {
	sum := sha256.Sum256([]byte(email + "\x00" + code))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomDigits(n int) (string, error) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteByte(byte('0' + v.Int64()))
	}
	return b.String(), nil
}

func issueLicenseKey(p *Purchase, now time.Time) error {
	if strings.TrimSpace(p.LicenseKey) != "" {
		return nil
	}
	key, err := randomLicenseKey()
	if err != nil {
		return err
	}
	p.LicenseKey = key
	p.LicenseKeyIssuedAt = &now
	return nil
}

func randomLicenseKey() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	parts := make([]string, 4)
	for part := range parts {
		var b strings.Builder
		for i := 0; i < 4; i++ {
			v, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return "", err
			}
			b.WriteByte(alphabet[v.Int64()])
		}
		parts[part] = b.String()
	}
	return "MEADOW-" + strings.Join(parts, "-"), nil
}

func normalizeLicenseKey(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func randomID(prefix string) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buf)
}

func findUserByEmail(st *State, email string) *User {
	for _, user := range st.Users {
		if user.Email == email {
			u := user
			return &u
		}
	}
	return nil
}

func ensureTrialEntitlement(st *State, userID string, now time.Time, trialDays int, maxDevices int) *Entitlement {
	for _, ent := range st.Entitlements {
		if ent.UserID == userID && ent.Plan == PlanTrial {
			e := ent
			return &e
		}
	}
	validUntil := now.Add(time.Duration(trialDays) * 24 * time.Hour)
	ent := Entitlement{
		ID:          randomID("ent"),
		UserID:      userID,
		Plan:        PlanTrial,
		Status:      EntitlementActive,
		LicenseType: "trial",
		ValidUntil:  &validUntil,
		MaxDevices:  maxDevices,
		Source:      "trial",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	st.Entitlements[ent.ID] = ent
	return &ent
}

func bestEntitlement(st *State, userID string, now time.Time) *Entitlement {
	var best *Entitlement
	for _, ent := range st.Entitlements {
		if ent.UserID != userID || ent.Status != EntitlementActive {
			continue
		}
		if ent.ValidUntil != nil && now.After(*ent.ValidUntil) {
			continue
		}
		candidate := ent
		if best == nil || planRank(candidate.Plan) > planRank(best.Plan) {
			best = &candidate
			continue
		}
		if best != nil && planRank(candidate.Plan) == planRank(best.Plan) {
			if best.ValidUntil == nil {
				continue
			}
			if candidate.ValidUntil == nil || candidate.ValidUntil.After(*best.ValidUntil) {
				best = &candidate
			}
		}
	}
	return best
}

func planRank(plan Plan) int {
	switch plan {
	case PlanLifetime:
		return 3
	case PlanMonthly:
		return 2
	case PlanTrial:
		return 1
	default:
		return 0
	}
}

func licenseType(plan Plan) string {
	switch plan {
	case PlanLifetime:
		return "perpetual"
	case PlanMonthly:
		return "subscription"
	case PlanTrial:
		return "trial"
	default:
		return string(plan)
	}
}

func tokenExpiry(ent Entitlement, now time.Time, offlineDays int) time.Time {
	expiry := now.Add(time.Duration(offlineDays) * 24 * time.Hour)
	if ent.Plan == PlanTrial {
		expiry = now.Add(24 * time.Hour)
	}
	if ent.ValidUntil != nil && ent.ValidUntil.Before(expiry) {
		expiry = *ent.ValidUntil
	}
	return expiry
}

func findDeviceByHash(st *State, userID, deviceHash string) *Device {
	for _, device := range st.Devices {
		if device.UserID == userID && device.DeviceHash == deviceHash {
			d := device
			return &d
		}
	}
	return nil
}

func activeDeviceCount(st *State, userID string) int {
	count := 0
	for _, device := range st.Devices {
		if device.UserID == userID && device.RevokedAt == nil {
			count++
		}
	}
	return count
}

func devicesForUser(st *State, userID string) []Device {
	var devices []Device
	for _, device := range st.Devices {
		if device.UserID == userID {
			devices = append(devices, device)
		}
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].LastSeen.After(devices[j].LastSeen) })
	return devices
}

func revokeDeviceInState(st *State, userID, deviceID, actor string, now time.Time) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return AppError{Code: "device_id_required", Message: "device_id is required", Status: 400}
	}
	device, ok := st.Devices[deviceID]
	if !ok {
		return AppError{Code: "device_not_found", Message: "device not found", Status: 404}
	}
	if userID != "" && device.UserID != userID {
		return AppError{Code: "device_not_found", Message: "device not found", Status: 404}
	}
	device.RevokedAt = &now
	st.Devices[device.ID] = device
	st.AuditEvents = append(st.AuditEvents, AuditEvent{
		ID:        randomID("aud"),
		Actor:     actor,
		Action:    "device_revoked",
		UserID:    device.UserID,
		DeviceID:  device.ID,
		CreatedAt: now,
	})
	return nil
}

func (s *Service) updatePurchase(ctx context.Context, purchaseID string, fn func(*Purchase, time.Time) error) (Purchase, error) {
	now := s.now()
	var purchase Purchase
	err := s.store.Update(ctx, func(st *State) error {
		p, ok := st.Purchases[purchaseID]
		if !ok {
			return AppError{Code: "purchase_not_found", Message: "purchase not found", Status: 404}
		}
		if err := fn(&p, now); err != nil {
			return err
		}
		st.Purchases[p.ID] = p
		purchase = p
		return nil
	})
	return purchase, err
}

func findPurchaseByPayPalOrderID(st *State, orderID string) (Purchase, bool) {
	orderID = strings.TrimSpace(orderID)
	for _, purchase := range st.Purchases {
		if purchase.Provider == "paypal" && purchase.ProviderOrderID == orderID {
			return purchase, true
		}
	}
	return Purchase{}, false
}

func findPurchaseByPayPalSubscriptionID(st *State, subscriptionID string) (Purchase, bool) {
	subscriptionID = strings.TrimSpace(subscriptionID)
	for _, purchase := range st.Purchases {
		if purchase.Provider == "paypal" && purchase.ProviderSubscriptionID == subscriptionID {
			return purchase, true
		}
	}
	return Purchase{}, false
}

func findPurchaseByLicenseKey(st *State, licenseKey string) (Purchase, bool) {
	licenseKey = normalizeLicenseKey(licenseKey)
	if licenseKey == "" {
		return Purchase{}, false
	}
	for _, purchase := range st.Purchases {
		if normalizeLicenseKey(purchase.LicenseKey) == licenseKey {
			return purchase, true
		}
	}
	return Purchase{}, false
}

func upsertProviderEntitlement(st *State, userID string, source string, ent Entitlement, now time.Time) Entitlement {
	for id, existing := range st.Entitlements {
		if existing.UserID == userID && existing.Source == source {
			ent.ID = existing.ID
			ent.CreatedAt = existing.CreatedAt
			ent.UpdatedAt = now
			st.Entitlements[id] = ent
			return ent
		}
	}
	ent.ID = randomID("ent")
	ent.CreatedAt = now
	ent.UpdatedAt = now
	st.Entitlements[ent.ID] = ent
	return ent
}

func (s *Service) auditPaymentWebhook(ctx context.Context, action string, details map[string]any) error {
	now := s.now()
	return s.store.Update(ctx, func(st *State) error {
		st.AuditEvents = append(st.AuditEvents, AuditEvent{
			ID:        randomID("aud"),
			Actor:     "paypal",
			Action:    action,
			Details:   details,
			CreatedAt: now,
		})
		return nil
	})
}

func appError(err error, fallbackStatus int) AppError {
	var ae AppError
	if errors.As(err, &ae) {
		return ae
	}
	if fallbackStatus == 0 {
		fallbackStatus = 500
	}
	if errors.Is(err, ErrNotFound) {
		return AppError{Code: "not_found", Message: "not found", Status: 404}
	}
	return AppError{Code: "internal_error", Message: err.Error(), Status: fallbackStatus}
}

func parsePlan(raw string) (Plan, error) {
	switch Plan(strings.TrimSpace(raw)) {
	case PlanTrial:
		return PlanTrial, nil
	case PlanMonthly:
		return PlanMonthly, nil
	case PlanLifetime:
		return PlanLifetime, nil
	default:
		return "", fmt.Errorf("unknown plan %q", raw)
	}
}
