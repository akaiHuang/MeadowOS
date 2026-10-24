//go:build !meadow_protected

package main

import (
	"os"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	harnesscore "github.com/hey-meadow/meadow-code/go/core/harness"
)

func desktopCodexStyleHarnessEnabled(servedName string) bool {
	return harnesscore.CodexStyleEnabled(servedName, desktopHarnessModeOverride())
}

// desktopHarnessModeOverride is intentionally empty in the shipped app. An
// empty override lets the shared Harness select the runtime by model family:
// 30B keeps its Nemotron task controller/self-iteration loop while 35B uses
// the Codex-style coding Harness. The environment variable remains a
// developer-only escape hatch for isolated comparisons.
func desktopHarnessModeOverride() string {
	if override := strings.TrimSpace(os.Getenv("MEADOW_AGENT_HARNESS")); override != "" {
		return override
	}
	if buildinfo.DesktopVariant == "evo" {
		return "evo"
	}
	return ""
}

// desktopApprovalMode keeps the Desktop launcher and the daemon health
// identity on the same effective approval mode. Edition loading deliberately
// ignores MEADOW_APPROVAL_MODE, so an environment value here is an explicit
// user/developer override rather than a model-edition capability grant.
func desktopApprovalMode(configured string) engine.ApprovalMode {
	raw := strings.TrimSpace(os.Getenv("MEADOW_APPROVAL_MODE"))
	if raw == "" {
		raw = strings.TrimSpace(configured)
	}
	mode, err := engine.ParseApprovalMode(raw)
	if err != nil {
		return engine.ModeDefault
	}
	return mode
}
