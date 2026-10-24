//go:build !meadow_protected

package agentdaemon

import (
	"os"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/upstreamconfig"
)

// This route is an explicit local diagnostic configuration. Inference still
// goes through Core; no provider credential or alternate listener enters Agent.
func configuredUpstreamCatalog() (*upstreamconfig.Catalog, error) {
	return upstreamconfig.LoadEnvironment()
}

// The legacy environment is used only when no explicit profile catalog exists.
func configuredUpstreamModelID() string {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("MEADOW_CORE_BACKEND")), "openai-compatible") {
		return ""
	}
	return strings.TrimSpace(os.Getenv("MEADOW_CORE_UPSTREAM_MODEL"))
}

func configuredUpstreamModelAllowed(modelID string) bool {
	if configuredUpstreamModelID() == "" || strings.TrimSpace(modelID) == "" {
		return false
	}
	if strings.TrimSpace(modelID) == configuredUpstreamModelID() {
		return true
	}
	for _, id := range strings.Split(os.Getenv("MEADOW_CORE_UPSTREAM_MODELS"), ",") {
		if strings.TrimSpace(id) == strings.TrimSpace(modelID) {
			return true
		}
	}
	return false
}
func configuredUpstreamModelIDs() []string {
	if configuredUpstreamModelID() == "" {
		return nil
	}
	ids := []string{}
	for _, id := range strings.Split(os.Getenv("MEADOW_CORE_UPSTREAM_MODELS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		ids = []string{configuredUpstreamModelID()}
	}
	return ids
}
