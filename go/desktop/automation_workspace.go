//go:build !meadow_protected

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const automationWorkspaceEnv = "MEADOW_AUTOMATION_WORKSPACE"

// prepareAutomationWorkspace returns an opt-in workspace used by Desktop
// automation. Keeping it outside Desktop/Documents avoids macOS TCC prompts,
// while normal launches continue to use the native folder picker.
func prepareAutomationWorkspace(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("%s must be an absolute path", automationWorkspaceEnv)
	}
	clean := filepath.Clean(raw)
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return "", fmt.Errorf("prepare automation workspace: %w", err)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return "", fmt.Errorf("inspect automation workspace: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("automation workspace must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("resolve automation workspace: %w", err)
	}
	return resolved, nil
}
