package edition

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	Standard              = "standard"
	Skull                 = "skull"
	SkullModelDisplayName = "Meadow Azazel 🐐💀"
)

var profileEnvironmentKeys = map[string]struct{}{
	"MEADOW_EDITION_CONFIG":         {},
	"MEADOW_EDITION_OVERRIDE":       {},
	"MEADOW_EDITION_PROFILE":        {},
	"MEADOW_MODEL_EDITION":          {},
	"MEADOW_AGENT_HARNESS":          {},
	"MEADOW_INSTALLED_MODELS":       {},
	"MEADOW_CORE_INSTALLED_MODELS":  {},
	"MEADOW_DESKTOP_MODEL_BASE_URL": {},
	"MEADOW_DESKTOP_MODEL_ID":       {},
}

func Normalize(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case Standard, "goodai", "/goodai":
		return Standard, true
	case Skull, "badai", "/badai":
		return Skull, true
	default:
		return "", false
	}
}

func OverridePath() string {
	if configured := strings.TrimSpace(os.Getenv("MEADOW_EDITION_OVERRIDE")); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "meadow", "edition.profile")
}

func ReadOverride() string {
	path := OverridePath()
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	profile, ok := Normalize(string(raw))
	if !ok {
		return ""
	}
	return profile
}

func Persist(profile string) error {
	normalized, ok := Normalize(profile)
	if !ok {
		return os.ErrInvalid
	}
	path := OverridePath()
	if path == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".edition.profile.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(normalized + "\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func CleanEnvironment(environment []string) []string {
	cleaned := make([]string, 0, len(environment))
	for _, item := range environment {
		key, _, _ := strings.Cut(item, "=")
		if _, owned := profileEnvironmentKeys[key]; owned || strings.HasPrefix(key, "MEADOW_CORE_") {
			continue
		}
		cleaned = append(cleaned, item)
	}
	return cleaned
}
