// Package session is the ~/.meadow compatibility layer: settings.json,
// user memory, and TS-format chat persistence (projects/<dir>/chats/*.jsonl)
// so existing users upgrade with zero migration.
package session

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/portablepaths"
)

var (
	sessionUserHomeDir   = os.UserHomeDir
	sessionUserConfigDir = os.UserConfigDir
)

// PortableStateRoot returns the one state namespace owned by a portable
// Desktop or helper. A configured directory need not exist yet; missing state
// must never cause discovery of a different installation's memory or settings.
// Startup validates this before calling the older no-error discovery APIs.
func PortableStateRoot() (string, error) {
	return portablepaths.StateRoot(os.Getenv("MEADOW_HOME"), sessionUserConfigDir)
}

// HomeDirs returns the state-directory search chain, most preferred first.
// The TS install grew two roots over time — settings.json lives in ~/.qwen
// while newer runtime state lives in ~/.meadow — so readers walk the chain
// and writers use the head.
func HomeDirs() []string {
	if buildinfo.DesktopPackageMode == "portable" {
		root, err := PortableStateRoot()
		if err != nil {
			// Startup returns this as an ordinary configuration error. This
			// backstop prevents old string-only callers from writing in CWD
			// or reading legacy state if startup validation is bypassed.
			panic(err)
		}
		return []string{root}
	}
	var dirs []string
	add := func(p string) {
		if p == "" {
			return
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			dirs = append(dirs, p)
		}
	}
	add(os.Getenv("MEADOW_HOME"))
	home, err := sessionUserHomeDir()
	if err == nil {
		add(filepath.Join(home, ".meadow"))
	}
	add(os.Getenv("QWEN_HOME"))
	if err == nil {
		add(filepath.Join(home, ".qwen"))
	}
	if len(dirs) == 0 && err == nil {
		// Fresh machine: default to ~/.meadow (created on first write).
		dirs = append(dirs, filepath.Join(home, ".meadow"))
	}
	return dirs
}

// PrimaryDir is where new state gets written.
func PrimaryDir() string {
	dirs := HomeDirs()
	if len(dirs) == 0 {
		return ".meadow"
	}
	return dirs[0]
}

// findInChain returns the first existing file with one of the given names
// across the home-dir chain.
func findInChain(names ...string) string {
	for _, dir := range HomeDirs() {
		for _, name := range names {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

var nonAlnumRe = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ProjectDirName reproduces the TS storage rule observed on disk:
// /Users/me/Desktop/x → -Users-me-Desktop-x (every non-alphanumeric rune
// becomes a hyphen).
func ProjectDirName(projectRoot string) string {
	return nonAlnumRe.ReplaceAllString(projectRoot, "-")
}

// ChatsDir returns (and creates) the chats directory for one project root.
func ChatsDir(projectRoot string) (string, error) {
	dir := filepath.Join(PrimaryDir(), "projects", ProjectDirName(projectRoot), "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// ChatPath resolves one durable chat file without allowing a session id to
// escape the project's chats directory. Empty chat files are valid: the CLI
// can create a conversation and switch models before the first user message.
func ChatPath(projectRoot, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || sessionID == "." || sessionID == ".." ||
		filepath.Base(sessionID) != sessionID ||
		strings.ContainsAny(sessionID, `/\\`) {
		return "", fmt.Errorf("invalid session id %q", sessionID)
	}
	dir, err := ChatsDir(projectRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionID+".jsonl"), nil
}

// ArtifactsDir returns the durable, per-chat output directory for standalone
// artifacts.  It is deliberately separate from the selected project root:
// two chats opened on the same repository must not overwrite each other's
// generated pages, games, reports, or compiled sidecars.
func ArtifactsDir(projectRoot, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || sessionID == "." || sessionID == ".." ||
		filepath.Base(sessionID) != sessionID ||
		strings.ContainsAny(sessionID, `/\\`) {
		return "", fmt.Errorf("invalid session id %q", sessionID)
	}
	dir := filepath.Join(
		PrimaryDir(), "projects", ProjectDirName(projectRoot),
		"sessions", sessionID, "artifacts",
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
