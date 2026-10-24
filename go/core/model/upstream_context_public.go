//go:build !meadow_protected

package model

import (
	"os"
	"strconv"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/upstreamconfig"
)

// A comparison session may exist before Core's first authenticated readiness
// probe. Its capacity must come from that explicit API profile, not a fuzzy
// parameter-count match to a differently configured local artifact.
func configuredUpstreamContextWindowTokens(modelID string) int {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("MEADOW_CORE_BACKEND")), "openai-compatible") ||
		strings.TrimSpace(modelID) == "" {
		return 0
	}
	catalog, err := upstreamconfig.LoadEnvironment()
	if err != nil {
		// Agent admission rejects the invalid explicit catalog. Keep cold UI
		// capacity conservative rather than reusing an unrelated model window.
		return 32768
	}
	if catalog != nil {
		if profile, ok := catalog.Lookup(strings.TrimSpace(modelID)); ok {
			return profile.Context
		}
		return 0
	}
	if !upstreamContextModelAllowed(modelID) {
		return 0
	}
	value := strings.TrimSpace(os.Getenv("MEADOW_CORE_UPSTREAM_CONTEXT_TOKENS"))
	if value == "" {
		return 32768
	}
	tokens, err := strconv.Atoi(value)
	if err != nil || tokens <= 0 {
		// Core rejects malformed configuration. Keep the cold UI conservative
		// as well; never fall through to a larger architectural model window.
		return 32768
	}
	return tokens
}

func upstreamContextModelAllowed(modelID string) bool {
	if strings.TrimSpace(modelID) == strings.TrimSpace(os.Getenv("MEADOW_CORE_UPSTREAM_MODEL")) {
		return true
	}
	for _, id := range strings.Split(os.Getenv("MEADOW_CORE_UPSTREAM_MODELS"), ",") {
		if strings.TrimSpace(id) != "" && strings.TrimSpace(id) == strings.TrimSpace(modelID) {
			return true
		}
	}
	return false
}
