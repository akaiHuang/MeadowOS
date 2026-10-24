package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/agentdaemon"
	coreauth "github.com/hey-meadow/meadow-code/go/core/productauth"
)

type desktopAccountScopeKey struct{}

// The scope only comes from the native account verifier. It is never accepted
// from request headers and contains no readable account or lease identifiers.
func desktopAccountScope(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	scope, _ := ctx.Value(desktopAccountScopeKey{}).(string)
	return scope
}

func desktopAccountStatusScope(status coreauth.Status) string {
	if agentdaemon.AccountAuthenticationError(status, nil) != nil {
		return ""
	}
	accessExpiry := ""
	if status.AccessExpiresAt != nil {
		accessExpiry = status.AccessExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	identity := struct {
		Schema, State, Plan, Subject, AccessExpiry string
		LeaseID, DeviceID, LeaseExpiry             string
		RevocationEpoch                            uint64
	}{
		Schema: status.Schema, State: string(status.State), Plan: string(status.Plan),
		Subject: status.Subject, AccessExpiry: accessExpiry,
		LeaseID: status.Entitlement.LeaseID, DeviceID: status.Entitlement.DeviceID,
		LeaseExpiry:     status.Entitlement.EffectiveExpiresAt.UTC().Format(time.RFC3339Nano),
		RevocationEpoch: status.Entitlement.RevocationEpoch,
	}
	encoded, _ := json.Marshal(identity)
	digest := sha256.Sum256(encoded)
	return "account-v1:" + hex.EncodeToString(digest[:])
}

func desktopAccountStatus(ctx context.Context, client desktopProductAuth) (coreauth.Status, error) {
	if client == nil {
		return coreauth.Status{}, &coreauth.Error{Code: "auth_unavailable", Message: "account verification is unavailable", HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
	}
	var status coreauth.Status
	var err error
	if fast, ok := client.(interface {
		AdmissionStatus(context.Context) (coreauth.Status, error)
	}); ok {
		status, err = fast.AdmissionStatus(ctx)
	} else {
		status, err = client.Status(ctx)
	}
	return status, agentdaemon.AccountAuthenticationError(status, err)
}

// The native shell observes account loss independently of frontend polling.
// Ownership stays with the caller: cancellation stops checks and an in-flight
// check receives the same cancellation. Failures notify once until a later
// successful verification establishes another authenticated period.
func startDesktopAccountWatcher(ctx context.Context, client desktopProductAuth, interval time.Duration, unauthorized func()) context.CancelFunc {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	watchContext, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		denied := false
		for {
			if watchContext.Err() != nil {
				return
			}
			checkContext, stopCheck := context.WithTimeout(watchContext, 5*time.Second)
			// Keep this a fresh verification: it observes cross-process logout
			// and revocation even while interactive requests use admission cache.
			var status coreauth.Status
			var err error
			if client == nil {
				err = &coreauth.Error{Code: "auth_unavailable", HTTPStatus: http.StatusServiceUnavailable}
			} else {
				status, err = client.Status(checkContext)
			}
			err = agentdaemon.AccountAuthenticationError(status, err)
			stopCheck()
			if watchContext.Err() != nil {
				return
			}
			if err != nil {
				if !denied && unauthorized != nil {
					unauthorized()
				}
				denied = true
			} else {
				denied = false
				if warmer, ok := client.(interface{ PreparePromptGrant(context.Context) error }); ok {
					warmContext, stopWarm := context.WithTimeout(watchContext, 30*time.Second)
					_ = warmer.PreparePromptGrant(warmContext)
					stopWarm()
				}
			}
			select {
			case <-watchContext.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return cancel
}

// Authentication exceptions are exact routes and methods. Prefix exceptions
// would expose Browser, workspace and provider credentials before login.
func desktopAccountOpenRequest(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		switch r.URL.Path {
		case "/health", "/auth/status", "/auth/admission-status", "/license/status", "/entitlement/status", "/agent/client/capabilities":
			return true
		}
	}
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/auth/start", "/auth/poll", "/auth/refresh", "/auth/logout", "/license/login/start", "/license/login/poll", "/agent/browser/cancel":
		return true
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return len(parts) == 3 && parts[0] == "session" && parts[1] != "" && parts[1] != "." && parts[1] != ".." && (parts[2] == "cancel" || parts[2] == "end")
}

func desktopRequireAccount(next http.Handler, client desktopProductAuth) http.Handler {
	return desktopRequireAccountWithLifecycle(next, client, nil)
}

func desktopRequireAccountWithLifecycle(next http.Handler, client desktopProductAuth, unauthorized func(), loginURLs ...*desktopLoginURLRegistry) http.Handler {
	var mu sync.Mutex
	cleaned := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if desktopAccountOpenRequest(r) || desktopLoginURLRequest(r, loginURLs...) {
			next.ServeHTTP(w, r)
			return
		}
		status, authErr := desktopAccountStatus(r.Context(), client)
		if authErr != nil {
			mu.Lock()
			notify := !cleaned
			cleaned = true
			mu.Unlock()
			if notify && unauthorized != nil {
				unauthorized()
			}
			writeDesktopProductAuthResult(w, nil, authErr)
			return
		}
		mu.Lock()
		cleaned = false
		mu.Unlock()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), desktopAccountScopeKey{}, desktopAccountStatusScope(status))))
	})
}

func desktopCloseBrowserForAccountLoss(embed *desktopBrowserEmbed) {
	if embed == nil {
		return
	}
	embed.layoutMu.Lock()
	defer embed.layoutMu.Unlock()
	embed.revokeAgentOwner()
	if embed.impl != nil {
		embed.impl.close()
	}
}

// Only the exact active native auth handoff is available before login. A
// renderer cannot turn the login exception into an unrestricted URL launcher.
// URLs and short-lived verification codes are held in memory, never logged.
type desktopLoginURLRegistry struct {
	mu          sync.Mutex
	urls        map[string]bool
	expires     time.Time
	generation  uint64
	transaction string
}

func (registry *desktopLoginURLRegistry) reset() uint64 {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.generation++
	registry.urls = nil
	registry.transaction = ""
	registry.expires = time.Time{}
	return registry.generation
}

func (registry *desktopLoginURLRegistry) remember(generation uint64, result coreauth.StartResult) {
	if result.Schema != coreauth.SchemaV1 || result.State != coreauth.StateLoginPending || result.ExpiresIn <= 0 || result.ExpiresIn > 3600 {
		return
	}
	urls := make(map[string]bool)
	for _, target := range []string{result.VerificationURI, result.VerificationURIComplete} {
		parsed, err := url.Parse(target)
		if err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" {
			urls[target] = true
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.generation == generation {
		registry.urls = urls
		registry.transaction = result.TransactionID
		registry.expires = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
}

func (registry *desktopLoginURLRegistry) complete(transaction string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if transaction != "" && registry.transaction == transaction {
		registry.generation++
		registry.urls = nil
		registry.transaction = ""
		registry.expires = time.Time{}
	}
}

func desktopLoginURLRequest(r *http.Request, registries ...*desktopLoginURLRegistry) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/native/open-url" || len(registries) != 1 || registries[0] == nil || r.Body == nil {
		return false
	}
	// Restore exactly the bounded body for the existing native handler. Never
	// consume a large input merely to decide whether account auth is required.
	body, err := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) > 16<<10 {
		return false
	}
	var input struct {
		URL string `json:"url"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return false
	}
	// A guest blocked by device quota must be able to manage their account.
	// This fixed destination grants no Browser, workspace, or provider access.
	if input.URL == "https://meadow-license-2026.web.app/account/devices" {
		return true
	}
	registry := registries[0]
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return time.Now().Before(registry.expires) && registry.urls[strings.TrimSpace(input.URL)]
}
