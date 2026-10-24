package harness

import (
	"fmt"
	"strings"

	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
)

type Mode string

const (
	ModeKernel     Mode = "kernel"
	ModeEvo        Mode = "evo"
	ModeCodexStyle Mode = "codex-style"
	ModeRavenCode  Mode = "raven-code"
)

// ParseMode validates explicit product preferences. Empty preserves the
// deployment/model-family default; developer aliases are not persisted modes.
func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case "", ModeKernel, ModeCodexStyle, ModeRavenCode, ModeEvo:
		return Mode(value), nil
	default:
		return "", fmt.Errorf("invalid harness mode %q", value)
	}
}

func ResolveMode(modelID, override string) Mode {
	switch normalizeMode(override) {
	case ModeEvo:
		return ModeEvo
	case ModeRavenCode:
		return ModeRavenCode
	case ModeCodexStyle:
		return ModeCodexStyle
	case ModeKernel:
		return ModeKernel
	}

	if info, ok := modelmgr.ModelByName(strings.TrimSpace(modelID)); ok {
		modelID = info.ServedName
	}
	switch strings.TrimSpace(modelID) {
	case "Meadow 27B", "Meadow Coder 35B":
		return ModeCodexStyle
	default:
		return ModeKernel
	}
}

func CodexStyleEnabled(modelID, override string) bool {
	return ResolveMode(modelID, override) == ModeCodexStyle
}

func CodexStyleRequested(override string) bool {
	return normalizeMode(override) == ModeCodexStyle
}

func normalizeMode(value string) Mode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "evo":
		return ModeEvo
	case "raven-code", "raven_code":
		return ModeRavenCode
	case "codex-style", "codex_style", "thin":
		return ModeCodexStyle
	case "legacy", "kernel", "off":
		return ModeKernel
	default:
		return ""
	}
}
