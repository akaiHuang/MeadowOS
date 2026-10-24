//go:build !meadow_protected

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/upstreamconfig"
)

// restoreDesktopModelProviderStartup restores the managed provider catalog
// after edition defaults, before Core identity or status polling is composed.
// explicit contains only keys present before loading those defaults. A process
// selecting a backend or upstream route keeps its existing launch contract.
func restoreDesktopModelProviderStartup(explicit map[string]bool) error {
	if explicit["MEADOW_CORE_BACKEND"] {
		return nil
	}
	for key, present := range explicit {
		if present && strings.HasPrefix(key, "MEADOW_CORE_UPSTREAM_") {
			return nil
		}
	}

	// A bundle may keep provider configuration outside the session state root.
	// Otherwise use the same resolved state root as Settings, without searching
	// other homes for an unrelated provider selection.
	path := strings.TrimSpace(os.Getenv("MEADOW_CORE_UPSTREAM_PROFILES_FILE"))
	if path == "" {
		path = filepath.Join(session.PrimaryDir(), "providers", "model-profiles.json")
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("managed upstream catalog is unavailable")
		}
	}
	catalog, err := upstreamconfig.Load(path)
	if err != nil {
		return err
	}

	// Validation is metadata-only and precedes every mutation. Credentials and
	// provider settings remain on disk unchanged; Core owns credential loading.
	defaults := []struct{ key, value string }{
		{"MEADOW_CORE_BACKEND", "openai-compatible"},
		{"MEADOW_CORE_UPSTREAM_PROFILES_FILE", path},
	}
	if !explicit["MEADOW_DESKTOP_MODEL_ID"] {
		defaults = append(defaults, struct{ key, value string }{"MEADOW_DESKTOP_MODEL_ID", catalog.DefaultModel})
	}
	if !explicit["MEADOW_DESKTOP_MODEL_BASE_URL"] {
		defaults = append(defaults, struct{ key, value string }{"MEADOW_DESKTOP_MODEL_BASE_URL", defaultDesktopCoreBaseURL})
	}
	for _, setting := range defaults {
		if err := os.Setenv(setting.key, setting.value); err != nil {
			return fmt.Errorf("managed upstream catalog could not be applied")
		}
	}
	return nil
}
