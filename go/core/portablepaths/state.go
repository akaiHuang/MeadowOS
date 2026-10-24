// Package portablepaths resolves the portable product's state namespace without
// depending on session, provider, or transport packages.
package portablepaths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StateRoot validates an explicit absolute directory, or the dedicated portable
// default. Missing directories are valid and never trigger legacy discovery.
func StateRoot(explicitRoot string, userConfigDir func() (string, error)) (string, error) {
	root := strings.TrimSpace(explicitRoot)
	if root == "" {
		configDir, err := userConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve portable state directory: %w", err)
		}
		if !filepath.IsAbs(configDir) || strings.ContainsRune(configDir, '\x00') {
			return "", fmt.Errorf("portable configuration directory must be absolute")
		}
		root = filepath.Join(configDir, "Meadow Desktop Portable")
	}
	if !filepath.IsAbs(root) || strings.ContainsRune(root, '\x00') {
		return "", fmt.Errorf("portable MEADOW_HOME must be an absolute directory")
	}
	root = filepath.Clean(root)
	if info, err := os.Stat(root); err == nil && !info.IsDir() {
		return "", fmt.Errorf("portable state path is not a directory")
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect portable state directory: %w", err)
	}
	return root, nil
}
