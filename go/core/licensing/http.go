package licensing

import (
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type HTTPConfig struct {
	AdminToken                    string
	DevCodes                      bool
	PayPal                        *PayPalClient
	SkipPayPalWebhookVerification bool
}

type Handler struct {
	service *Service
	cfg     HTTPConfig
	mux     *http.ServeMux
}

func NewHandler(service *Service, cfg HTTPConfig) *Handler {
	h := &Handler{service: service, cfg: cfg, mux: http.NewServeMux()}
	h.routes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /healthz", h.health)
	h.mux.HandleFunc("GET /public-key", h.publicKey)
	h.mux.HandleFunc("POST /auth/request-code", h.requestCode)
	h.mux.HandleFunc("POST /auth/verify-code", h.verifyCode)
	h.mux.HandleFunc("POST /devices/activate", h.activateDevice)
	h.mux.HandleFunc("POST /devices/revoke", h.revokeDevice)
	h.mux.HandleFunc("POST /licenses/activate", h.activateLicenseKey)
	h.mux.HandleFunc("POST /licenses/devices", h.licenseKeyDevices)
	h.mux.HandleFunc("POST /licenses/revoke-device", h.licenseKeyRevokeDevice)
	h.mux.HandleFunc("GET /licenses/current-key", h.currentLicenseKey)
	h.mux.HandleFunc("GET /entitlements/current", h.currentEntitlement)
	h.mux.HandleFunc("POST /checkout/paypal/create", h.createPayPalCheckout)
	h.mux.HandleFunc("POST /checkout/paypal/capture", h.capturePayPalOrder)
	h.mux.HandleFunc("POST /webhooks/paypal", h.paypalWebhook)
	h.mux.HandleFunc("GET /paypal/return", h.paypalReturn)
	h.mux.HandleFunc("GET /paypal/cancel", h.paypalCancel)
	h.mux.HandleFunc("GET /admin/login", h.adminLogin)
	h.mux.HandleFunc("POST /admin/login", h.adminLoginPost)
	h.mux.HandleFunc("POST /admin/logout", h.adminLogout)
	h.mux.HandleFunc("GET /admin", h.adminIndex)
	h.mux.HandleFunc("GET /admin/api/summary", h.adminSummaryAPI)
	h.mux.HandleFunc("POST /admin/entitlements/grant", h.adminGrantEntitlement)
	h.mux.HandleFunc("POST /admin/devices/revoke", h.adminRevokeDevice)
}

func (h *Handler) paypalReturn(w http.ResponseWriter, r *http.Request) {
	h.paypalResultPage(w, "付款已完成", "請回到 Meadow Code，按「啟用授權」產生序號並啟用這台 Mac。", r.URL.Query().Get("token"))
}

func (h *Handler) paypalCancel(w http.ResponseWriter, _ *http.Request) {
	h.paypalResultPage(w, "付款已取消", "你可以關閉這個視窗，回到 Meadow Code 後重新開始付款。", "")
}

func (h *Handler) paypalResultPage(w http.ResponseWriter, title, message, orderID string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		Title   string
		Message string
		OrderID string
	}{
		Title:   title,
		Message: message,
		OrderID: orderID,
	}
	if err := paypalResultTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *Handler) createPayPalCheckout(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	if h.cfg.PayPal == nil || !h.cfg.PayPal.Enabled() {
		writeAppError(w, AppError{Code: "paypal_not_configured", Message: "PayPal is not configured", Status: 503})
		return
	}
	var req struct {
		Plan Plan `json:"plan"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var kind PurchaseKind
	switch req.Plan {
	case PlanLifetime:
		kind = PurchaseOneTime
	case PlanMonthly:
		kind = PurchaseSubscription
	default:
		writeAppError(w, AppError{Code: "unsupported_paypal_plan", Message: "PayPal checkout supports lifetime or monthly plans", Status: 400})
		return
	}
	purchase, err := h.service.BeginPurchase(r.Context(), BeginPurchaseInput{
		UserID:   user.ID,
		Provider: "paypal",
		Plan:     req.Plan,
		Kind:     kind,
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	var checkout PayPalCheckoutResult
	if req.Plan == PlanLifetime {
		checkout, err = h.cfg.PayPal.CreateLifetimeOrder(r.Context(), purchase, user.Email)
		if err == nil {
			_, err = h.service.AttachPayPalOrder(r.Context(), purchase.ID, checkout.OrderID)
		}
	} else {
		checkout, err = h.cfg.PayPal.CreateMonthlySubscription(r.Context(), purchase)
		if err == nil {
			_, err = h.service.AttachPayPalSubscription(r.Context(), purchase.ID, checkout.SubscriptionID)
		}
	}
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, checkout)
}

func (h *Handler) capturePayPalOrder(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	if h.cfg.PayPal == nil || !h.cfg.PayPal.Enabled() {
		writeAppError(w, AppError{Code: "paypal_not_configured", Message: "PayPal is not configured", Status: 503})
		return
	}
	var req struct {
		OrderID string `json:"order_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	orderID, captureID, payerID, err := h.cfg.PayPal.CaptureOrder(r.Context(), req.OrderID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	purchase, entitlement, err := h.service.CompletePayPalOrder(r.Context(), orderID, captureID, payerID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	if purchase.UserID != user.ID {
		writeAppError(w, AppError{Code: "paypal_order_user_mismatch", Message: "PayPal order belongs to another user", Status: 403})
		return
	}
	writeJSON(w, 200, map[string]any{
		"purchase":         purchase,
		"entitlement":      entitlement,
		"license_key":      purchase.LicenseKey,
		"license_delivery": "email",
		"delivery_email":   user.Email,
	})
}

func (h *Handler) paypalWebhook(w http.ResponseWriter, r *http.Request) {
	if h.cfg.PayPal == nil || !h.cfg.PayPal.Enabled() {
		writeAppError(w, AppError{Code: "paypal_not_configured", Message: "PayPal is not configured", Status: 503})
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeAppError(w, err)
		return
	}
	if !h.cfg.SkipPayPalWebhookVerification {
		verified, err := h.cfg.PayPal.VerifyWebhook(r.Context(), r.Header, body)
		if err != nil {
			writeAppError(w, err)
			return
		}
		if !verified {
			writeAppError(w, AppError{Code: "paypal_webhook_verification_failed", Message: "PayPal webhook verification failed", Status: 401})
			return
		}
	}
	event, err := h.cfg.PayPal.ParseWebhookEvent(body)
	if err != nil {
		writeAppError(w, AppError{Code: "paypal_webhook_invalid", Message: err.Error(), Status: 400})
		return
	}
	if err := h.service.ProcessPayPalWebhook(r.Context(), event); err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "service": "meadow-license"})
}

func (h *Handler) publicKey(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, h.service.PublicKey())
}

func (h *Handler) requestCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.service.RequestCode(r.Context(), req.Email)
	if err != nil {
		writeAppError(w, err)
		return
	}
	if !h.cfg.DevCodes {
		result.DevCode = ""
	}
	writeJSON(w, 200, result)
}

func (h *Handler) verifyCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.service.VerifyCode(r.Context(), req.Email, req.Code)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (h *Handler) activateDevice(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var req struct {
		DeviceHash string `json:"device_hash"`
		Name       string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.service.ActivateDevice(r.Context(), user.ID, req.DeviceHash, req.Name)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (h *Handler) activateLicenseKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LicenseKey string `json:"license_key"`
		DeviceHash string `json:"device_hash"`
		Name       string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.service.ActivateLicenseKey(r.Context(), ActivateLicenseKeyInput{
		LicenseKey: req.LicenseKey,
		DeviceHash: req.DeviceHash,
		Name:       req.Name,
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (h *Handler) revokeDevice(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.service.RevokeDeviceForUser(r.Context(), user.ID, req.DeviceID, user.ID); err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *Handler) licenseKeyDevices(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LicenseKey string `json:"license_key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.service.CurrentEntitlementForLicenseKey(r.Context(), req.LicenseKey)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (h *Handler) licenseKeyRevokeDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LicenseKey string `json:"license_key"`
		DeviceID   string `json:"device_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.service.RevokeDeviceForLicenseKey(r.Context(), req.LicenseKey, req.DeviceID); err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *Handler) currentEntitlement(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	result, err := h.service.CurrentEntitlement(r.Context(), user.ID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (h *Handler) currentLicenseKey(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	result, err := h.service.CurrentLicenseKey(r.Context(), user.ID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request) (User, bool) {
	user, err := h.service.AuthenticateSession(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeAppError(w, err)
		return User{}, false
	}
	return user, true
}

func (h *Handler) adminLogin(w http.ResponseWriter, r *http.Request) {
	if h.isAdmin(r) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = adminLoginTemplate.Execute(w, map[string]any{"AdminEnabled": h.cfg.AdminToken != ""})
}

func (h *Handler) adminLoginPost(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AdminToken == "" {
		http.Error(w, "admin token is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !h.tokenEqual(r.FormValue("token"), h.cfg.AdminToken) {
		http.Error(w, "invalid admin token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "meadow_admin",
		Value:    h.cfg.AdminToken,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((8 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) adminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "meadow_admin",
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (h *Handler) adminIndex(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	summary, err := h.service.AdminSummary(r.Context())
	if err != nil {
		writeAppError(w, err)
		return
	}
	page := buildAdminPage(summary)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminIndexTemplate.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *Handler) adminSummaryAPI(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	summary, err := h.service.AdminSummary(r.Context())
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, 200, summary)
}

func (h *Handler) adminGrantEntitlement(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	plan, err := parsePlan(r.FormValue("plan"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	days, _ := strconv.Atoi(r.FormValue("days"))
	maxDevices, _ := strconv.Atoi(r.FormValue("max_devices"))
	_, err = h.service.GrantEntitlement(r.Context(), GrantEntitlementInput{
		Email:      r.FormValue("email"),
		Plan:       plan,
		Days:       days,
		MaxDevices: maxDevices,
		Source:     "admin",
		Notes:      r.FormValue("notes"),
		Actor:      "admin",
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) adminRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := h.service.RevokeDevice(r.Context(), r.FormValue("device_id"), "admin"); err != nil {
		writeAppError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if h.cfg.AdminToken == "" {
		http.Error(w, "admin token is not configured", http.StatusServiceUnavailable)
		return false
	}
	if h.isAdmin(r) {
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/admin/api/") {
		writeAppError(w, AppError{Code: "admin_token_required", Message: "admin token is required", Status: 401})
		return false
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	return false
}

func (h *Handler) isAdmin(r *http.Request) bool {
	if h.cfg.AdminToken == "" {
		return false
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if h.tokenEqual(bearer, h.cfg.AdminToken) {
		return true
	}
	cookie, err := r.Cookie("meadow_admin")
	if err != nil {
		return false
	}
	return h.tokenEqual(cookie.Value, h.cfg.AdminToken)
}

func (h *Handler) tokenEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeAppError(w, AppError{Code: "invalid_json", Message: err.Error(), Status: 400})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAppError(w http.ResponseWriter, err error) {
	ae := appError(err, 500)
	writeJSON(w, ae.Status, map[string]any{
		"error": map[string]any{
			"code":    ae.Code,
			"message": ae.Message,
		},
	})
}

type adminPage struct {
	Users        []adminUserRow
	Devices      []Device
	Entitlements []Entitlement
	AuditEvents  []AuditEvent
	Totals       map[string]int
}

type adminUserRow struct {
	User              User
	BestEntitlement   *Entitlement
	Devices           []Device
	ActiveDeviceCount int
}

func buildAdminPage(summary AdminSummary) adminPage {
	devicesByUser := map[string][]Device{}
	activeDeviceCount := map[string]int{}
	for _, device := range summary.Devices {
		devicesByUser[device.UserID] = append(devicesByUser[device.UserID], device)
		if device.RevokedAt == nil {
			activeDeviceCount[device.UserID]++
		}
	}
	entitlementsByUser := map[string][]Entitlement{}
	for _, ent := range summary.Entitlements {
		entitlementsByUser[ent.UserID] = append(entitlementsByUser[ent.UserID], ent)
	}
	now := time.Now().UTC()
	var rows []adminUserRow
	for _, user := range summary.Users {
		var best *Entitlement
		for _, ent := range entitlementsByUser[user.ID] {
			if ent.Status != EntitlementActive || (ent.ValidUntil != nil && now.After(*ent.ValidUntil)) {
				continue
			}
			e := ent
			if best == nil || planRank(e.Plan) > planRank(best.Plan) {
				best = &e
			}
		}
		rows = append(rows, adminUserRow{
			User:              user,
			BestEntitlement:   best,
			Devices:           devicesByUser[user.ID],
			ActiveDeviceCount: activeDeviceCount[user.ID],
		})
	}
	if len(summary.AuditEvents) > 30 {
		summary.AuditEvents = summary.AuditEvents[:30]
	}
	return adminPage{
		Users:        rows,
		Devices:      summary.Devices,
		Entitlements: summary.Entitlements,
		AuditEvents:  summary.AuditEvents,
		Totals: map[string]int{
			"users":        len(summary.Users),
			"devices":      len(summary.Devices),
			"entitlements": len(summary.Entitlements),
			"purchases":    len(summary.Purchases),
		},
	}
}

var paypalResultTemplate = template.Must(template.New("paypal-result").Parse(`<!doctype html>
<html lang="zh-Hant">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} - Meadow Code</title>
  <style>
    body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#0f1115;color:#f3f5f7;margin:0;display:grid;place-items:center;min-height:100vh}
    main{width:min(520px,calc(100vw - 32px));border:1px solid #2b3038;border-radius:8px;background:#171a20;padding:28px;text-align:center}
    h1{font-size:24px;margin:0 0 10px}
    p{color:#aab2bf;margin:0 0 18px;line-height:1.55}
    code{display:inline-block;background:#0f1115;border:1px solid #343b46;border-radius:6px;padding:6px 8px;color:#d7e1ee}
  </style>
</head>
<body>
<main>
  <h1>{{.Title}}</h1>
  <p>{{.Message}}</p>
  {{if .OrderID}}<p>Order ID<br><code>{{.OrderID}}</code></p>{{end}}
</main>
</body>
</html>`))

var adminLoginTemplate = template.Must(template.New("admin-login").Parse(`<!doctype html>
<html lang="zh-Hant">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Meadow License Admin</title>
  <style>
    body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#0f1115;color:#f3f5f7;margin:0;display:grid;place-items:center;min-height:100vh}
    main{width:min(420px,calc(100vw - 32px));border:1px solid #2b3038;border-radius:8px;background:#171a20;padding:28px}
    h1{font-size:22px;margin:0 0 8px}
    p{color:#aab2bf;margin:0 0 22px}
    input,button{width:100%;box-sizing:border-box;border-radius:6px;border:1px solid #343b46;background:#0f1115;color:#f3f5f7;padding:12px;font-size:15px}
    button{margin-top:12px;background:#26a269;border-color:#26a269;font-weight:700;cursor:pointer}
    .warn{color:#ffcf70}
  </style>
</head>
<body>
<main>
  <h1>Meadow License Admin</h1>
  {{if .AdminEnabled}}
  <p>輸入管理 token 進入後台。</p>
  <form method="post" action="/admin/login">
    <input name="token" type="password" autocomplete="current-password" placeholder="Admin token" autofocus>
    <button type="submit">登入</button>
  </form>
  {{else}}
  <p class="warn">尚未設定 MEADOW_LICENSE_ADMIN_TOKEN。</p>
  {{end}}
</main>
</body>
</html>`))

var adminIndexTemplate = template.Must(template.New("admin-index").Funcs(template.FuncMap{
	"time": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
	"timep": func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
}).Parse(`<!doctype html>
<html lang="zh-Hant">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Meadow License Admin</title>
  <style>
    body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#0f1115;color:#f3f5f7;margin:0}
    header{display:flex;justify-content:space-between;align-items:center;padding:18px 24px;border-bottom:1px solid #272d36;background:#141820;position:sticky;top:0}
    h1{font-size:20px;margin:0} h2{font-size:16px;margin:28px 0 12px}
    main{padding:24px;max-width:1220px;margin:auto}
    .grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px}
    .metric,.panel{border:1px solid #2b3038;border-radius:8px;background:#171a20;padding:16px}
    .metric b{display:block;font-size:26px;margin-top:8px}.muted{color:#9aa4b2}.tag{display:inline-block;padding:3px 8px;border-radius:999px;background:#263241;color:#b7d7ff;font-size:12px}
    table{width:100%;border-collapse:collapse;background:#171a20;border:1px solid #2b3038;border-radius:8px;overflow:hidden}
    th,td{text-align:left;border-bottom:1px solid #272d36;padding:10px 12px;font-size:14px;vertical-align:top}
    th{color:#aab2bf;font-weight:600;background:#151922} tr:last-child td{border-bottom:0}
    input,select,button,textarea{border-radius:6px;border:1px solid #343b46;background:#0f1115;color:#f3f5f7;padding:9px;font-size:14px}
    button{background:#26a269;border-color:#26a269;font-weight:700;cursor:pointer}.danger{background:#b42318;border-color:#b42318}
    form.inline{display:inline}.grant{display:grid;grid-template-columns:2fr 1fr 1fr 1fr 2fr auto;gap:8px}.logout button{background:#222832;border-color:#343b46}
    @media(max-width:900px){.grid{grid-template-columns:repeat(2,minmax(0,1fr))}.grant{grid-template-columns:1fr}}
  </style>
</head>
<body>
<header>
  <h1>Meadow License Admin</h1>
  <form class="logout" method="post" action="/admin/logout"><button type="submit">登出</button></form>
</header>
<main>
  <section class="grid">
    <div class="metric"><span class="muted">Users</span><b>{{index .Totals "users"}}</b></div>
    <div class="metric"><span class="muted">Devices</span><b>{{index .Totals "devices"}}</b></div>
    <div class="metric"><span class="muted">Entitlements</span><b>{{index .Totals "entitlements"}}</b></div>
    <div class="metric"><span class="muted">Purchases</span><b>{{index .Totals "purchases"}}</b></div>
  </section>

  <section class="panel" style="margin-top:16px">
    <h2>新增 / 調整授權</h2>
    <form class="grant" method="post" action="/admin/entitlements/grant">
      <input name="email" type="email" placeholder="user@example.com" required>
      <select name="plan"><option value="monthly">monthly</option><option value="lifetime">lifetime</option><option value="trial">trial</option></select>
      <input name="days" type="number" min="0" placeholder="天數；0=永久/預設">
      <input name="max_devices" type="number" min="0" placeholder="裝置數">
      <input name="notes" placeholder="備註">
      <button type="submit">Grant</button>
    </form>
  </section>

  <h2>Users</h2>
  <table>
    <thead><tr><th>Email</th><th>授權</th><th>裝置</th><th>建立</th><th>最後活動</th></tr></thead>
    <tbody>
    {{range .Users}}
      <tr>
        <td>{{.User.Email}}<br><span class="muted">{{.User.ID}}</span></td>
        <td>{{if .BestEntitlement}}<span class="tag">{{.BestEntitlement.Plan}}</span> {{.BestEntitlement.Status}}<br><span class="muted">到期 {{timep .BestEntitlement.ValidUntil}}</span>{{else}}<span class="muted">無有效授權</span>{{end}}</td>
        <td>{{.ActiveDeviceCount}} active / {{len .Devices}} total</td>
        <td>{{time .User.CreatedAt}}</td>
        <td>{{time .User.LastSeenAt}}</td>
      </tr>
    {{else}}
      <tr><td colspan="5" class="muted">尚無使用者</td></tr>
    {{end}}
    </tbody>
  </table>

  <h2>Devices</h2>
  <table>
    <thead><tr><th>Name</th><th>User</th><th>Device hash</th><th>Last seen</th><th>Status</th><th></th></tr></thead>
    <tbody>
    {{range .Devices}}
      <tr>
        <td>{{.Name}}<br><span class="muted">{{.ID}}</span></td>
        <td>{{.UserID}}</td>
        <td><span class="muted">{{.DeviceHash}}</span></td>
        <td>{{time .LastSeen}}</td>
        <td>{{if .RevokedAt}}revoked {{timep .RevokedAt}}{{else}}active{{end}}</td>
        <td>{{if not .RevokedAt}}<form class="inline" method="post" action="/admin/devices/revoke"><input type="hidden" name="device_id" value="{{.ID}}"><button class="danger" type="submit">撤銷</button></form>{{end}}</td>
      </tr>
    {{else}}
      <tr><td colspan="6" class="muted">尚無裝置</td></tr>
    {{end}}
    </tbody>
  </table>

  <h2>Audit</h2>
  <table>
    <thead><tr><th>時間</th><th>Action</th><th>Actor</th><th>User</th><th>Device</th></tr></thead>
    <tbody>
    {{range .AuditEvents}}
      <tr><td>{{time .CreatedAt}}</td><td>{{.Action}}</td><td>{{.Actor}}</td><td>{{.UserID}}</td><td>{{.DeviceID}}</td></tr>
    {{else}}
      <tr><td colspan="5" class="muted">尚無事件</td></tr>
    {{end}}
    </tbody>
  </table>
</main>
</body>
</html>`))
