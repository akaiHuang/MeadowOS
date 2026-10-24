package licensing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type PayPalConfig struct {
	ClientID       string
	Secret         string
	WebhookID      string
	Sandbox        bool
	Currency       string
	LifetimeAmount string
	MonthlyPlanID  string
	ReturnURL      string
	CancelURL      string
	BrandName      string
	HTTPClient     *http.Client
}

type PayPalClient struct {
	cfg        PayPalConfig
	httpClient *http.Client

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

type PayPalCheckoutResult struct {
	Provider       string   `json:"provider"`
	Plan           Plan     `json:"plan"`
	PurchaseID     string   `json:"purchase_id"`
	OrderID        string   `json:"order_id,omitempty"`
	SubscriptionID string   `json:"subscription_id,omitempty"`
	ApprovalURL    string   `json:"approval_url"`
	Links          []PPLink `json:"links,omitempty"`
}

type PPLink struct {
	Href   string `json:"href"`
	Rel    string `json:"rel"`
	Method string `json:"method,omitempty"`
}

type paypalAccessTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type paypalOrderResponse struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Links  []PPLink `json:"links"`
	Payer  struct {
		PayerID string `json:"payer_id"`
	} `json:"payer"`
	PurchaseUnits []struct {
		Payments struct {
			Captures []struct {
				ID string `json:"id"`
			} `json:"captures"`
		} `json:"payments"`
	} `json:"purchase_units"`
}

type paypalSubscriptionResponse struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Links  []PPLink `json:"links"`
}

func NewPayPalClient(cfg PayPalConfig) *PayPalClient {
	if cfg.Currency == "" {
		cfg.Currency = "TWD"
	}
	if cfg.LifetimeAmount == "" {
		cfg.LifetimeAmount = "2990"
	}
	if cfg.BrandName == "" {
		cfg.BrandName = "Meadow Code"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &PayPalClient{cfg: cfg, httpClient: client}
}

func (c *PayPalClient) Enabled() bool {
	return c != nil && c.cfg.ClientID != "" && c.cfg.Secret != ""
}

func (c *PayPalClient) HasWebhookVerification() bool {
	return c.Enabled() && c.cfg.WebhookID != ""
}

func (c *PayPalClient) HasMonthlyPlan() bool {
	return c.Enabled() && c.cfg.MonthlyPlanID != ""
}

func (c *PayPalClient) LifetimeAmount() string {
	if c.cfg.LifetimeAmount == "" {
		return "2990"
	}
	return c.cfg.LifetimeAmount
}

func (c *PayPalClient) Currency() string {
	if c.cfg.Currency == "" {
		return "TWD"
	}
	return c.cfg.Currency
}

func (c *PayPalClient) baseURL() string {
	if c.cfg.Sandbox {
		return "https://api-m.sandbox.paypal.com"
	}
	return "https://api-m.paypal.com"
}

func (c *PayPalClient) CreateLifetimeOrder(ctx context.Context, purchase Purchase, email string) (PayPalCheckoutResult, error) {
	if !c.Enabled() {
		return PayPalCheckoutResult{}, AppError{Code: "paypal_not_configured", Message: "PayPal is not configured", Status: 503}
	}
	body := map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{
			{
				"reference_id": purchase.ID,
				"custom_id":    purchase.ID,
				"description":  "Meadow Code lifetime license",
				"amount": map[string]string{
					"currency_code": c.Currency(),
					"value":         c.LifetimeAmount(),
				},
			},
		},
		"payment_source": map[string]any{
			"paypal": map[string]any{
				"experience_context": c.experienceContext("PAY_NOW"),
			},
		},
	}
	if email != "" {
		body["payer"] = map[string]string{"email_address": email}
	}
	var response paypalOrderResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v2/checkout/orders", purchase.ID, body, &response); err != nil {
		return PayPalCheckoutResult{}, err
	}
	return PayPalCheckoutResult{
		Provider:    "paypal",
		Plan:        purchase.Plan,
		PurchaseID:  purchase.ID,
		OrderID:     response.ID,
		ApprovalURL: approvalURL(response.Links),
		Links:       response.Links,
	}, nil
}

func (c *PayPalClient) CaptureOrder(ctx context.Context, orderID string) (orderIDOut string, captureID string, payerID string, err error) {
	if !c.Enabled() {
		return "", "", "", AppError{Code: "paypal_not_configured", Message: "PayPal is not configured", Status: 503}
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return "", "", "", AppError{Code: "paypal_order_id_required", Message: "PayPal order id is required", Status: 400}
	}
	var response paypalOrderResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v2/checkout/orders/"+url.PathEscape(orderID)+"/capture", orderID, nil, &response); err != nil {
		return "", "", "", err
	}
	if len(response.PurchaseUnits) > 0 && len(response.PurchaseUnits[0].Payments.Captures) > 0 {
		captureID = response.PurchaseUnits[0].Payments.Captures[0].ID
	}
	return response.ID, captureID, response.Payer.PayerID, nil
}

func (c *PayPalClient) CreateMonthlySubscription(ctx context.Context, purchase Purchase) (PayPalCheckoutResult, error) {
	if !c.Enabled() {
		return PayPalCheckoutResult{}, AppError{Code: "paypal_not_configured", Message: "PayPal is not configured", Status: 503}
	}
	if c.cfg.MonthlyPlanID == "" {
		return PayPalCheckoutResult{}, AppError{Code: "paypal_monthly_plan_missing", Message: "MEADOW_PAYPAL_MONTHLY_PLAN_ID is required for monthly subscriptions", Status: 503}
	}
	body := map[string]any{
		"plan_id":             c.cfg.MonthlyPlanID,
		"custom_id":           purchase.ID,
		"application_context": c.experienceContext("SUBSCRIBE_NOW"),
	}
	var response paypalSubscriptionResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/billing/subscriptions", purchase.ID, body, &response); err != nil {
		return PayPalCheckoutResult{}, err
	}
	return PayPalCheckoutResult{
		Provider:       "paypal",
		Plan:           purchase.Plan,
		PurchaseID:     purchase.ID,
		SubscriptionID: response.ID,
		ApprovalURL:    approvalURL(response.Links),
		Links:          response.Links,
	}, nil
}

func (c *PayPalClient) VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (bool, error) {
	if !c.HasWebhookVerification() {
		return false, AppError{Code: "paypal_webhook_not_configured", Message: "PayPal webhook verification is not configured", Status: 503}
	}
	var event json.RawMessage
	if err := json.Unmarshal(body, &event); err != nil {
		return false, err
	}
	payload := map[string]any{
		"auth_algo":         headers.Get("Paypal-Auth-Algo"),
		"cert_url":          headers.Get("Paypal-Cert-Url"),
		"transmission_id":   headers.Get("Paypal-Transmission-Id"),
		"transmission_sig":  headers.Get("Paypal-Transmission-Sig"),
		"transmission_time": headers.Get("Paypal-Transmission-Time"),
		"webhook_id":        c.cfg.WebhookID,
		"webhook_event":     event,
	}
	var response struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/notifications/verify-webhook-signature", "", payload, &response); err != nil {
		return false, err
	}
	return response.VerificationStatus == "SUCCESS", nil
}

func (c *PayPalClient) ParseWebhookEvent(body []byte) (PayPalWebhookEvent, error) {
	var raw struct {
		ID        string          `json:"id"`
		EventType string          `json:"event_type"`
		Resource  json.RawMessage `json:"resource"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return PayPalWebhookEvent{}, err
	}
	event := PayPalWebhookEvent{
		ID:        raw.ID,
		EventType: raw.EventType,
		Payload:   append(json.RawMessage(nil), body...),
	}
	var resource struct {
		ID                string `json:"id"`
		CustomID          string `json:"custom_id"`
		SupplementaryData struct {
			RelatedIDs struct {
				OrderID string `json:"order_id"`
			} `json:"related_ids"`
		} `json:"supplementary_data"`
		Payer struct {
			PayerID string `json:"payer_id"`
		} `json:"payer"`
		Subscriber struct {
			PayerID string `json:"payer_id"`
		} `json:"subscriber"`
		BillingInfo struct {
			NextBillingTime string `json:"next_billing_time"`
		} `json:"billing_info"`
	}
	if len(raw.Resource) > 0 {
		if err := json.Unmarshal(raw.Resource, &resource); err != nil {
			return PayPalWebhookEvent{}, err
		}
	}
	switch raw.EventType {
	case "PAYMENT.CAPTURE.COMPLETED":
		event.CaptureID = resource.ID
		event.OrderID = resource.SupplementaryData.RelatedIDs.OrderID
		event.ProviderPayerID = resource.Payer.PayerID
	case "BILLING.SUBSCRIPTION.ACTIVATED", "BILLING.SUBSCRIPTION.PAYMENT.COMPLETED", "BILLING.SUBSCRIPTION.CANCELLED", "BILLING.SUBSCRIPTION.SUSPENDED", "BILLING.SUBSCRIPTION.EXPIRED":
		event.SubscriptionID = resource.ID
		event.ProviderPayerID = resource.Subscriber.PayerID
		if resource.BillingInfo.NextBillingTime != "" {
			if next, err := time.Parse(time.RFC3339, resource.BillingInfo.NextBillingTime); err == nil {
				event.NextBillingTime = &next
			}
		}
	}
	return event, nil
}

func (c *PayPalClient) experienceContext(userAction string) map[string]string {
	ctx := map[string]string{
		"brand_name":          c.cfg.BrandName,
		"shipping_preference": "NO_SHIPPING",
		"user_action":         userAction,
	}
	if c.cfg.ReturnURL != "" {
		ctx["return_url"] = c.cfg.ReturnURL
	}
	if c.cfg.CancelURL != "" {
		ctx["cancel_url"] = c.cfg.CancelURL
	}
	return ctx
}

func (c *PayPalClient) doJSON(ctx context.Context, method, path, requestID string, body any, out any) error {
	token, err := c.accessTokenFor(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		req.Header.Set("PayPal-Request-Id", requestID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AppError{Code: "paypal_api_error", Message: fmt.Sprintf("PayPal API %s %s failed with %d: %s", method, path, resp.StatusCode, string(data)), Status: 502}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *PayPalClient) accessTokenFor(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry.Add(-60*time.Second)) {
		token := c.accessToken
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/v1/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.cfg.ClientID+":"+c.cfg.Secret)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", AppError{Code: "paypal_auth_error", Message: fmt.Sprintf("PayPal OAuth failed with %d: %s", resp.StatusCode, string(data)), Status: 502}
	}
	var parsed paypalAccessTokenResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", err
	}
	if parsed.AccessToken == "" {
		return "", AppError{Code: "paypal_auth_empty_token", Message: "PayPal OAuth returned an empty access token", Status: 502}
	}
	expiry := time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	c.mu.Lock()
	c.accessToken = parsed.AccessToken
	c.tokenExpiry = expiry
	c.mu.Unlock()
	return parsed.AccessToken, nil
}

func approvalURL(links []PPLink) string {
	for _, link := range links {
		if link.Rel == "approve" || link.Rel == "payer-action" {
			return link.Href
		}
	}
	return ""
}
