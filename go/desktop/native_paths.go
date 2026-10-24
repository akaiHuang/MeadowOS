//go:build !meadow_protected

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/session"
)

// revealHostPath resolves a frontend/model path to an existing host path that
// Finder may reveal. It accepts the selected project and the legacy per-chat
// artifact mount, but never lets a renderer-supplied path escape either root.
func revealHostPath(cwd, sessionID, raw string) (string, error) {
	projectRoot, err := filepath.Abs(strings.TrimSpace(cwd))
	if err != nil || strings.TrimSpace(cwd) == "" {
		return "", fmt.Errorf("cwd required")
	}
	projectRoot = filepath.Clean(projectRoot)
	value := strings.TrimSpace(strings.Trim(raw, `"'`))
	if value == "" {
		return "", fmt.Errorf("path required")
	}
	slash := strings.ReplaceAll(value, `\`, "/")

	root := projectRoot
	rel := slash
	if slash == "/workspace/session" || strings.HasPrefix(slash, "/workspace/session/") {
		slash = "/session" + strings.TrimPrefix(slash, "/workspace/session")
	}
	if slash == "/session" || strings.HasPrefix(slash, "/session/") {
		root, err = session.ArtifactsDir(projectRoot, strings.TrimSpace(sessionID))
		if err != nil {
			return "", err
		}
		rel = strings.TrimPrefix(strings.TrimPrefix(slash, "/session"), "/")
	} else if slash == "/workspace" || strings.HasPrefix(slash, "/workspace/") {
		rel = strings.TrimPrefix(strings.TrimPrefix(slash, "/workspace"), "/")
	} else if filepath.IsAbs(value) {
		candidate := filepath.Clean(value)
		if !containsHostRoot(projectRoot, candidate) {
			return "", fmt.Errorf("path is outside the selected project")
		}
		if _, err := os.Stat(candidate); err != nil {
			return "", err
		}
		return candidate, nil
	}

	candidate := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if !containsHostRoot(root, candidate) {
		return "", fmt.Errorf("path is outside the selected project")
	}
	if _, err := os.Stat(candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

func containsHostRoot(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	return candidate == root || strings.HasPrefix(candidate, root+string(filepath.Separator))
}
