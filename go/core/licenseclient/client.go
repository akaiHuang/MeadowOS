package licenseclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
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
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/licensing"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

const (
	defaultServerURL = "http://127.0.0.1:18088"
	sessionFileName  = "session.json"
	tokenFileName    = "license.token"
	publicKeyFile    = "public-key.json"
	deviceFileName   = "device.id"
)

var (
	buildServerURL    = ""
	buildRequired     = ""
	buildPublicKeyB64 = ""
)

type Config struct {
	ServerURL    string
	Required     bool
	PublicKeyB64 string
	HTTPClient   *http.Client
}

type Client struct {
	cfg        Config
	httpClient *http.Client
}

type LocalSession struct {
	Email         string    `json:"email"`
	AccessToken   string    `json:"access_token"`
	LicenseKey    string    `json:"license_key,omitempty"`
	LicenseServer string    `json:"license_server"`
	DeviceHash    string    `json:"device_hash"`
	LastRefreshAt time.Time `json:"last_refresh_at,omitempty"`
}

type StoredPublicKey struct {
	KeyID   string `json:"key_id"`
	Ed25519 string `json:"ed25519_base64"`
}

type PublicConfig struct {
	ServerURL    string `json:"server_url"`
	Required     string `json:"required"`
	PublicKeyB64 string `json:"public_key_base64"`
}

type Status struct {
	Required       bool       `json:"required"`
	Configured     bool       `json:"configured"`
	Authorized     bool       `json:"authorized"`
	NeedsLogin     bool       `json:"needs_login"`
	NeedsPayment   bool       `json:"needs_payment"`
	CanRefresh     bool       `json:"can_refresh"`
	Email          string     `json:"email,omitempty"`
	Plan           string     `json:"plan,omitempty"`
	LicenseType    string     `json:"license_type,omitempty"`
	TokenExpiresAt *time.Time `json:"token_expires_at,omitempty"`
	ValidUntil     *time.Time `json:"valid_until,omitempty"`
	Message        string     `json:"message,omitempty"`
	ServerURL      string     `json:"server_url,omitempty"`
	DeviceHash     string     `json:"device_hash,omitempty"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func (e APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return fmt.Sprintf("license server returned HTTP %d", e.Status)
}

type RequestCodeResult struct {
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expires_at"`
	DevCode   string    `json:"dev_code,omitempty"`
}

type VerifyCodeResult struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	User        struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

type ActivateResult struct {
	Device struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"device"`
	Entitlement  licensing.Entitlement `json:"entitlement"`
	LicenseToken string                `json:"license_token"`
	PublicKey    StoredPublicKey       `json:"public_key"`
}

type PayPalCheckoutResult struct {
	Provider       string `json:"provider"`
	Plan           string `json:"plan"`
	PurchaseID     string `json:"purchase_id"`
	OrderID        string `json:"order_id,omitempty"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	ApprovalURL    string `json:"approval_url"`
}

type PayPalCaptureResult struct {
	Purchase        licensing.Purchase    `json:"purchase"`
	Entitlement     licensing.Entitlement `json:"entitlement"`
	LicenseKey      string                `json:"license_key,omitempty"`
	LicenseDelivery string                `json:"license_delivery,omitempty"`
	DeliveryEmail   string                `json:"delivery_email,omitempty"`
}

type CurrentLicenseKeyResult struct {
	LicenseKey string `json:"license_key,omitempty"`
	Plan       string `json:"plan,omitempty"`
	PurchaseID string `json:"purchase_id,omitempty"`
}

type DevicesResult struct {
	User              licensing.PublicUser   `json:"user"`
	Entitlement       *licensing.Entitlement `json:"entitlement,omitempty"`
	Devices           []licensing.Device     `json:"devices"`
	CurrentDeviceHash string                 `json:"current_device_hash,omitempty"`
	CurrentDeviceID   string                 `json:"current_device_id,omitempty"`
	MaxDevices        int                    `json:"max_devices"`
	ActiveDeviceCount int                    `json:"active_device_count"`
}

func FromEnv() *Client {
	publicCfg := loadPublicConfig()
	serverURL := strings.TrimRight(strings.TrimSpace(firstNonEmpty(
		os.Getenv("MEADOW_LICENSE_SERVER_URL"),
		publicCfg.ServerURL,
		buildServerURL,
	)), "/")
	required := envBool("MEADOW_LICENSE_REQUIRED", false) ||
		envBoolString(publicCfg.Required, false) ||
		envBoolString(buildRequired, false) ||
		serverURL != ""
	publicKeyB64 := firstNonEmpty(
		os.Getenv("MEADOW_LICENSE_PUBLIC_KEY_BASE64"),
		publicCfg.PublicKeyB64,
		buildPublicKeyB64,
	)
	if serverURL == "" {
		serverURL = defaultServerURL
	}
	return New(Config{
		ServerURL:    serverURL,
		Required:     required,
		PublicKeyB64: strings.TrimSpace(publicKeyB64),
	})
}

func New(cfg Config) *Client {
	cfg.ServerURL = strings.TrimRight(strings.TrimSpace(cfg.ServerURL), "/")
	if cfg.ServerURL == "" {
		cfg.ServerURL = defaultServerURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 12 * time.Second}
	}
	return &Client{cfg: cfg, httpClient: httpClient}
}

func loadPublicConfig() PublicConfig {
	cfg := PublicConfig{}
	for _, path := range publicConfigPaths() {
		next := readPublicConfig(path)
		if next.ServerURL != "" {
			cfg.ServerURL = next.ServerURL
		}
		if next.Required != "" {
			cfg.Required = next.Required
		}
		if next.PublicKeyB64 != "" {
			cfg.PublicKeyB64 = next.PublicKeyB64
		}
	}
	return cfg
}

func publicConfigPaths() []string {
	seen := map[string]bool{}
	add := func(paths *[]string, candidate string) {
		if candidate == "" {
			return
		}
		abs, err := filepath.Abs(candidate)
		if err != nil {
			return
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		*paths = append(*paths, abs)
	}
	var paths []string
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		add(&paths, filepath.Join(exeDir, "..", "Resources", "license-config.json"))
		add(&paths, filepath.Join(exeDir, "license-config.json"))
	}
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for i := 0; i < 6; i++ {
			add(&paths, filepath.Join(dir, "license-config.json"))
			add(&paths, filepath.Join(dir, ".env"))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return paths
}

func readPublicConfig(path string) PublicConfig {
	b, err := os.ReadFile(path)
	if err != nil {
		return PublicConfig{}
	}
	switch filepath.Base(path) {
	case ".env":
		return parsePublicEnv(string(b))
	default:
		var cfg PublicConfig
		if err := json.Unmarshal(b, &cfg); err != nil {
			return PublicConfig{}
		}
		return PublicConfig{
			ServerURL:    strings.TrimSpace(cfg.ServerURL),
			Required:     strings.TrimSpace(cfg.Required),
			PublicKeyB64: strings.TrimSpace(cfg.PublicKeyB64),
		}
	}
}

func parsePublicEnv(src string) PublicConfig {
	cfg := PublicConfig{}
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		switch key {
		case "MEADOW_LICENSE_SERVER_URL":
			cfg.ServerURL = value
		case "MEADOW_LICENSE_REQUIRED":
			cfg.Required = value
		case "MEADOW_LICENSE_PUBLIC_KEY_BASE64":
			cfg.PublicKeyB64 = value
		}
	}
	return cfg
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func envBoolString(value string, fallback bool) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func (c *Client) Enabled() bool {
	return c.cfg.Required
}

func (c *Client) ServerURL() string {
	return c.cfg.ServerURL
}

func (c *Client) Status(ctx context.Context) Status {
	status := Status{
		Required:   c.cfg.Required,
		Configured: c.cfg.ServerURL != "",
		ServerURL:  c.cfg.ServerURL,
	}
	if !c.cfg.Required {
		status.Authorized = true
		status.Message = "license gate disabled"
		return status
	}
	local, _ := LoadSession()
	status.Email = local.Email
	status.DeviceHash = local.DeviceHash
	status.CanRefresh = local.AccessToken != "" || local.LicenseKey != ""

	token, tokenErr := LoadToken()
	var keyErr error
	if token != "" {
		publicKey, err := c.loadPublicKey(ctx)
		keyErr = err
		if keyErr != nil {
			tokenErr = keyErr
		} else {
			claims, err := licensing.VerifyToken(token, publicKey)
			if err == nil {
				status.Authorized = true
				status.Plan = string(claims.Plan)
				status.LicenseType = claims.LicenseType
				status.TokenExpiresAt = &claims.ExpiresAt
				status.ValidUntil = claims.ValidUntil
				status.Email = claims.Email
				status.Message = "authorized"
				return status
			}
			tokenErr = err
		}
	}

	if local.AccessToken != "" {
		if refreshed, err := c.Refresh(ctx); err == nil && refreshed.Authorized {
			return refreshed
		}
	}

	if local.LicenseKey != "" {
		if refreshed, err := c.Refresh(ctx); err == nil && refreshed.Authorized {
			return refreshed
		}
	}

	if local.AccessToken == "" && local.LicenseKey == "" {
		status.NeedsLogin = true
		status.Message = "請登入 Meadow 帳號開始 7 天試用或同步授權"
		return status
	}
	status.NeedsPayment = true
	if tokenErr != nil {
		if strings.Contains(strings.ToLower(tokenErr.Error()), "expired") {
			status.Message = "授權或試用已到期，請選擇買斷或月訂閱後重新同步授權"
		} else {
			status.Message = tokenErr.Error()
		}
	} else if keyErr != nil {
		status.Message = keyErr.Error()
	} else {
		status.Message = "授權已到期，請付款或重新同步授權"
	}
	return status
}

func (c *Client) Require(ctx context.Context) error {
	status := c.Status(ctx)
	if status.Authorized {
		return nil
	}
	return errors.New(status.Message)
}

func (c *Client) RequestCode(ctx context.Context, email string) (RequestCodeResult, error) {
	var out RequestCodeResult
	if err := c.doJSON(ctx, http.MethodPost, "/auth/request-code", "", map[string]string{"email": email}, &out); err != nil {
		return RequestCodeResult{}, err
	}
	return out, nil
}

func (c *Client) VerifyCode(ctx context.Context, email, code string) (VerifyCodeResult, error) {
	var out VerifyCodeResult
	if err := c.doJSON(ctx, http.MethodPost, "/auth/verify-code", "", map[string]string{"email": email, "code": code}, &out); err != nil {
		return VerifyCodeResult{}, err
	}
	deviceHash, err := DeviceHash()
	if err != nil {
		return VerifyCodeResult{}, err
	}
	local := LocalSession{
		Email:         out.User.Email,
		AccessToken:   out.AccessToken,
		LicenseServer: c.cfg.ServerURL,
		DeviceHash:    deviceHash,
	}
	if err := SaveSession(local); err != nil {
		return VerifyCodeResult{}, err
	}
	if _, err := c.Activate(ctx, ""); err != nil {
		return VerifyCodeResult{}, err
	}
	return out, nil
}

func (c *Client) Activate(ctx context.Context, deviceName string) (ActivateResult, error) {
	local, err := LoadSession()
	if err != nil {
		return ActivateResult{}, err
	}
	if local.AccessToken == "" {
		return ActivateResult{}, errors.New("not logged in")
	}
	if local.DeviceHash == "" {
		local.DeviceHash, err = DeviceHash()
		if err != nil {
			return ActivateResult{}, err
		}
	}
	if strings.TrimSpace(deviceName) == "" {
		deviceName = defaultDeviceName()
	}
	var out ActivateResult
	err = c.doJSON(ctx, http.MethodPost, "/devices/activate", local.AccessToken, map[string]string{
		"device_hash": local.DeviceHash,
		"name":        deviceName,
	}, &out)
	if err != nil {
		return ActivateResult{}, err
	}
	if out.LicenseToken == "" {
		return ActivateResult{}, errors.New("license server returned empty token")
	}
	if err := SaveToken(out.LicenseToken); err != nil {
		return ActivateResult{}, err
	}
	if out.PublicKey.Ed25519 != "" {
		if err := SavePublicKey(out.PublicKey); err != nil {
			return ActivateResult{}, err
		}
	}
	local.LastRefreshAt = time.Now().UTC()
	if err := SaveSession(local); err != nil {
		return ActivateResult{}, err
	}
	return out, nil
}

func (c *Client) Refresh(ctx context.Context) (Status, error) {
	local, _ := LoadSession()
	var err error
	if local.AccessToken != "" {
		_, err = c.Activate(ctx, "")
		if err == nil {
			return c.Status(ctx), nil
		}
	}
	if local.LicenseKey != "" {
		_, err = c.ActivateLicenseKey(ctx, local.LicenseKey, "")
	} else {
		err = errors.New("not logged in and no license key saved")
	}
	if err != nil {
		return Status{}, err
	}
	return c.Status(ctx), nil
}

func (c *Client) ActivateLicenseKey(ctx context.Context, licenseKey, deviceName string) (ActivateResult, error) {
	licenseKey = strings.TrimSpace(licenseKey)
	if licenseKey == "" {
		return ActivateResult{}, errors.New("license key is required")
	}
	deviceHash, err := DeviceHash()
	if err != nil {
		return ActivateResult{}, err
	}
	if strings.TrimSpace(deviceName) == "" {
		deviceName = defaultDeviceName()
	}
	var out ActivateResult
	err = c.doJSON(ctx, http.MethodPost, "/licenses/activate", "", map[string]string{
		"license_key": licenseKey,
		"device_hash": deviceHash,
		"name":        deviceName,
	}, &out)
	if err != nil {
		return ActivateResult{}, err
	}
	if out.LicenseToken == "" {
		return ActivateResult{}, errors.New("license server returned empty token")
	}
	if err := SaveToken(out.LicenseToken); err != nil {
		return ActivateResult{}, err
	}
	if out.PublicKey.Ed25519 != "" {
		if err := SavePublicKey(out.PublicKey); err != nil {
			return ActivateResult{}, err
		}
	}
	local, _ := LoadSession()
	local.LicenseKey = licenseKey
	local.LicenseServer = c.cfg.ServerURL
	local.DeviceHash = deviceHash
	local.LastRefreshAt = time.Now().UTC()
	if err := SaveSession(local); err != nil {
		return ActivateResult{}, err
	}
	return out, nil
}

func (c *Client) CurrentLicenseKey(ctx context.Context) (CurrentLicenseKeyResult, error) {
	local, err := LoadSession()
	if err != nil {
		return CurrentLicenseKeyResult{}, err
	}
	if local.LicenseKey != "" {
		return CurrentLicenseKeyResult{LicenseKey: local.LicenseKey}, nil
	}
	if local.AccessToken == "" {
		return CurrentLicenseKeyResult{}, errors.New("not logged in")
	}
	var out CurrentLicenseKeyResult
	if err := c.doJSON(ctx, http.MethodGet, "/licenses/current-key", local.AccessToken, nil, &out); err != nil {
		return CurrentLicenseKeyResult{}, err
	}
	if out.LicenseKey != "" {
		if err := c.rememberLicenseKey(out.LicenseKey); err != nil {
			return CurrentLicenseKeyResult{}, err
		}
	}
	return out, nil
}

func (c *Client) Devices(ctx context.Context, licenseKey string) (DevicesResult, error) {
	local, _ := LoadSession()
	if strings.TrimSpace(licenseKey) == "" {
		licenseKey = local.LicenseKey
	}
	var out licensing.CurrentEntitlementResult
	if strings.TrimSpace(licenseKey) != "" {
		if err := c.doJSON(ctx, http.MethodPost, "/licenses/devices", "", map[string]string{"license_key": licenseKey}, &out); err != nil {
			return DevicesResult{}, err
		}
	} else {
		if local.AccessToken == "" {
			return DevicesResult{}, errors.New("not logged in and no license key saved")
		}
		if err := c.doJSON(ctx, http.MethodGet, "/entitlements/current", local.AccessToken, nil, &out); err != nil {
			return DevicesResult{}, err
		}
	}
	return c.enrichDevices(out, local), nil
}

func (c *Client) RevokeDevice(ctx context.Context, deviceID, licenseKey string) error {
	local, _ := LoadSession()
	if strings.TrimSpace(licenseKey) == "" {
		licenseKey = local.LicenseKey
	}
	if strings.TrimSpace(licenseKey) != "" {
		return c.doJSON(ctx, http.MethodPost, "/licenses/revoke-device", "", map[string]string{
			"license_key": licenseKey,
			"device_id":   deviceID,
		}, nil)
	}
	if local.AccessToken == "" {
		return errors.New("not logged in and no license key saved")
	}
	return c.doJSON(ctx, http.MethodPost, "/devices/revoke", local.AccessToken, map[string]string{"device_id": deviceID}, nil)
}

func (c *Client) enrichDevices(in licensing.CurrentEntitlementResult, local LocalSession) DevicesResult {
	out := DevicesResult{
		User:              in.User,
		Entitlement:       in.Entitlement,
		Devices:           in.Devices,
		CurrentDeviceHash: local.DeviceHash,
	}
	if out.CurrentDeviceHash == "" {
		if hash, err := DeviceHash(); err == nil {
			out.CurrentDeviceHash = hash
		}
	}
	if in.Entitlement != nil {
		out.MaxDevices = in.Entitlement.MaxDevices
	}
	for _, device := range in.Devices {
		if device.RevokedAt == nil {
			out.ActiveDeviceCount++
		}
		if out.CurrentDeviceHash != "" && device.DeviceHash == out.CurrentDeviceHash {
			out.CurrentDeviceID = device.ID
		}
	}
	return out
}

func (c *Client) CreatePayPalCheckout(ctx context.Context, plan string) (PayPalCheckoutResult, error) {
	local, err := LoadSession()
	if err != nil {
		return PayPalCheckoutResult{}, err
	}
	if local.AccessToken == "" {
		return PayPalCheckoutResult{}, errors.New("not logged in")
	}
	var out PayPalCheckoutResult
	if err := c.doJSON(ctx, http.MethodPost, "/checkout/paypal/create", local.AccessToken, map[string]string{"plan": plan}, &out); err != nil {
		return PayPalCheckoutResult{}, err
	}
	if out.ApprovalURL == "" {
		return PayPalCheckoutResult{}, errors.New("PayPal checkout did not return an approval URL")
	}
	return out, nil
}

func (c *Client) CapturePayPalOrder(ctx context.Context, orderID string) (PayPalCaptureResult, error) {
	local, err := LoadSession()
	if err != nil {
		return PayPalCaptureResult{}, err
	}
	if local.AccessToken == "" {
		return PayPalCaptureResult{}, errors.New("not logged in")
	}
	var out PayPalCaptureResult
	if err := c.doJSON(ctx, http.MethodPost, "/checkout/paypal/capture", local.AccessToken, map[string]string{"order_id": orderID}, &out); err != nil {
		return PayPalCaptureResult{}, err
	}
	if out.LicenseKey == "" {
		out.LicenseKey = out.Purchase.LicenseKey
	}
	if out.LicenseDelivery == "" {
		out.LicenseDelivery = "email"
	}
	if out.DeliveryEmail == "" {
		out.DeliveryEmail = local.Email
	}
	return out, nil
}

func (c *Client) rememberLicenseKey(licenseKey string) error {
	licenseKey = strings.TrimSpace(licenseKey)
	if licenseKey == "" {
		return nil
	}
	local, _ := LoadSession()
	if local.DeviceHash == "" {
		if deviceHash, err := DeviceHash(); err == nil {
			local.DeviceHash = deviceHash
		}
	}
	local.LicenseKey = licenseKey
	local.LicenseServer = c.cfg.ServerURL
	local.LastRefreshAt = time.Now().UTC()
	return SaveSession(local)
}

func Logout() error {
	dir := Dir()
	for _, name := range []string{sessionFileName, tokenFileName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func Dir() string {
	return filepath.Join(session.PrimaryDir(), "license")
}

func LoadSession() (LocalSession, error) {
	path := filepath.Join(Dir(), sessionFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LocalSession{}, nil
		}
		return LocalSession{}, err
	}
	var local LocalSession
	if err := json.Unmarshal(data, &local); err != nil {
		return LocalSession{}, err
	}
	return local, nil
}

func SaveSession(local LocalSession) error {
	return writeJSON(filepath.Join(Dir(), sessionFileName), local, 0o600)
}

func LoadToken() (string, error) {
	data, err := os.ReadFile(filepath.Join(Dir(), tokenFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func SaveToken(token string) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(Dir(), tokenFileName), []byte(strings.TrimSpace(token)+"\n"), 0o600)
}

func LoadPublicKey() (StoredPublicKey, error) {
	data, err := os.ReadFile(filepath.Join(Dir(), publicKeyFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StoredPublicKey{}, nil
		}
		return StoredPublicKey{}, err
	}
	var key StoredPublicKey
	if err := json.Unmarshal(data, &key); err != nil {
		return StoredPublicKey{}, err
	}
	return key, nil
}

func SavePublicKey(key StoredPublicKey) error {
	return writeJSON(filepath.Join(Dir(), publicKeyFile), key, 0o600)
}

func (c *Client) loadPublicKey(ctx context.Context) (ed25519.PublicKey, error) {
	if c.cfg.PublicKeyB64 != "" {
		return parsePublicKey(c.cfg.PublicKeyB64)
	}
	if key, err := LoadPublicKey(); err == nil && key.Ed25519 != "" {
		return parsePublicKey(key.Ed25519)
	}
	var remote StoredPublicKey
	if err := c.doJSON(ctx, http.MethodGet, "/public-key", "", nil, &remote); err != nil {
		return nil, err
	}
	if remote.Ed25519 == "" {
		return nil, errors.New("license public key is missing")
	}
	_ = SavePublicKey(remote)
	return parsePublicKey(remote.Ed25519)
}

func parsePublicKey(raw string) (ed25519.PublicKey, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if len(data) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("license public key must be %d bytes, got %d", ed25519.PublicKeySize, len(data))
	}
	return ed25519.PublicKey(data), nil
}

func (c *Client) doJSON(ctx context.Context, method, path, bearer string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.ServerURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		code := ""
		var parsed struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &parsed) == nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
			code = parsed.Error.Code
		}
		if msg == "" {
			msg = resp.Status
		}
		return APIError{Code: code, Message: msg, Status: resp.StatusCode}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func DeviceHash() (string, error) {
	if override := strings.TrimSpace(os.Getenv("MEADOW_DEVICE_HASH")); override != "" {
		return override, nil
	}
	parts := []string{"meadow-license-device-v1", runtime.GOOS, runtime.GOARCH}
	if id := platformMachineID(); id != "" {
		parts = append(parts, id)
	} else if id, err := installID(); err == nil && id != "" {
		parts = append(parts, id)
	}
	if host, err := os.Hostname(); err == nil {
		parts = append(parts, host)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "dev_" + hex.EncodeToString(sum[:]), nil
}

func platformMachineID() string {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if !strings.Contains(line, "IOPlatformUUID") {
					continue
				}
				parts := strings.Split(line, "=")
				if len(parts) == 2 {
					return strings.Trim(strings.TrimSpace(parts[1]), `"`)
				}
			}
		}
	}
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		data, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

func installID() (string, error) {
	path := filepath.Join(Dir(), deviceFileName)
	data, err := os.ReadFile(path)
	if err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data)), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func defaultDeviceName() string {
	host, _ := os.Hostname()
	if host == "" {
		host = runtime.GOOS + "-" + runtime.GOARCH
	}
	return "Meadow on " + host
}

func writeJSON(path string, value any, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), perm)
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
