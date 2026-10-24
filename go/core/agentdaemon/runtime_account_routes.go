package agentdaemon

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/productauth"
)

// configureRuntimeAccountReaders is composition-owned. Only the private
// prompt-grant pilot has a separate verified account-continuity authority;
// other compositions retain their existing fresh account admission.
func (bridge *commercialBridge) configureRuntimeAccountReaders(routes *productAuthRoutes, discovery *connectionDiscovery) {
	if !bridge.runtimeAccountRoutesEnabled() {
		return
	}
	if routes != nil {
		routes.accountStatusForRequest = bridge.accountStatusForRequest
	}
	if discovery != nil {
		discovery.accountStatus = bridge.runtimeAccountStatus
	}
}

func (bridge *commercialBridge) runtimeAccountRoutesEnabled() bool {
	return bridge != nil && bridge.accountAdmission && bridge.promptGrants != nil && bridge.promptGrants.runtimeService != nil
}

func (bridge *commercialBridge) accountStatusForRequest(request *http.Request) (productauth.Status, error) {
	if bridge.runtimeAccountRoute(request) {
		return bridge.runtimeAccountStatus(request.Context())
	}
	return bridge.admissionStatus(request.Context())
}

// The allowlist is account-only: observation and responses to a pending tool
// request cannot authorize a new prompt, alter model/approval policy, deliver
// queued input, or reserve/settle quota. Unknown routes keep the billing gate.
func (bridge *commercialBridge) runtimeAccountRoute(request *http.Request) bool {
	if !bridge.runtimeAccountRoutesEnabled() || request == nil {
		return false
	}
	if request.Method == http.MethodGet {
		switch request.URL.Path {
		case "/auth/admission-status", "/agent/connections/models", "/agent/model", "/sessions", "/usage":
			return true
		}
	}
	// This POST is a fixed, bounded Git observation with optional locks and
	// fsmonitor disabled. Other VCS routes can mutate and retain the strict gate.
	if request.Method == http.MethodPost && request.URL.Path == "/vcs/status" {
		return true
	}
	_, suffix, canonical := canonicalSessionPath(request.URL.Path)
	if !canonical {
		return false
	}
	if request.Method == http.MethodGet {
		switch suffix {
		case "/events", "/context", "/queue", "/tasks", "/task-plan", "/sources", "/places", "/location", "/evo", "/inference-controls", "/harness-controls":
			return true
		}
	}
	if request.Method != http.MethodPost {
		return false
	}
	if suffix == "/heartbeat" {
		return true
	}
	parts := strings.Split(suffix, "/")
	if len(parts) != 3 || !validCommercialID(parts[2]) {
		return false
	}
	return parts[1] == "permission" || parts[1] == "user-input" || parts[1] == "location"
}

// Session continuity also needs the signed login-session principal. This path
// deliberately cannot replace principalAccess at prompt/billing boundaries.
func (bridge *commercialBridge) runtimePrincipalAccess(ctx context.Context) (string, time.Time, error) {
	status, err := bridge.runtimeAccountStatus(ctx)
	return bridge.principalFromStatus(status, err)
}
