package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// MigrationReport describes the intentionally narrow aliases migrated by the
// native runtime. It does not claim the full Python _config_version history.
type MigrationReport struct {
	Changed    bool
	BackupPath string
	Paths      []string
}

// MigrateLegacyAliases persists only the compatibility aliases the Go runtime
// understands. It leaves _config_version untouched so a partial Go migration
// can never masquerade as all historical Python migrations having run.
func (d *Document) MigrateLegacyAliases() (MigrationReport, error) {
	var report MigrationReport
	if d == nil {
		return report, fmt.Errorf("config document is unavailable")
	}
	normalized, paths := normalizeLegacyAliases(d.Raw)
	if reflect.DeepEqual(normalized, d.Raw) {
		return report, nil
	}
	if !d.Exists {
		previous := d.Raw
		d.Raw = normalized
		report.Changed = true
		report.Paths = paths
		if err := d.Save(); err != nil {
			d.Raw = previous
			return MigrationReport{}, err
		}
		return report, nil
	}
	original, err := os.ReadFile(d.Path)
	if err != nil {
		return report, fmt.Errorf("read pre-migration config %s: %w", d.Path, err)
	}
	backup, err := writeMigrationBackup(d.Path, original)
	if err != nil {
		return report, err
	}
	previous := d.Raw
	d.Raw = normalized
	if err := d.Save(); err != nil {
		d.Raw = previous
		return report, fmt.Errorf("save migrated config (backup: %s): %w", backup, err)
	}
	report.Changed = true
	report.BackupPath = backup
	report.Paths = paths
	return report, nil
}

func writeMigrationBackup(path string, data []byte) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	backup := path + ".pre-h-go-v0.1." + stamp + ".bak"
	file, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(backup)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	complete = true
	return backup, nil
}

func normalizeLegacyAliases(source map[string]any) (map[string]any, []string) {
	result := cloneMap(source)
	var changed []string

	if rootTurns, exists := result["max_turns"]; exists {
		agent := mapValue(result["agent"])
		if agent == nil {
			agent = map[string]any{}
		}
		if _, alreadyCanonical := agent["max_turns"]; !alreadyCanonical {
			agent["max_turns"] = rootTurns
		}
		result["agent"] = agent
		delete(result, "max_turns")
		changed = append(changed, "max_turns -> agent.max_turns")
	}

	modelValue, modelPresent := result["model"]
	model := mapValue(modelValue)
	hasRootAlias := false
	for _, key := range []string{"provider", "base_url", "context_length", "api_base"} {
		if _, exists := result[key]; exists {
			hasRootAlias = true
			break
		}
	}
	hasModelAlias := model != nil && (model["api_base"] != nil ||
		model["model"] != nil ||
		model["name"] != nil)
	if hasRootAlias || hasModelAlias {
		if model == nil {
			model = map[string]any{}
			if modelPresent && stringValue(modelValue) != "" {
				model["default"] = modelValue
			}
		} else {
			model = cloneMap(model)
		}
		result["model"] = model
	}

	for _, key := range []string{"provider", "base_url", "context_length"} {
		value, exists := result[key]
		if !exists {
			continue
		}
		if model != nil && isEmptyConfigValue(model[key]) && !isEmptyConfigValue(value) {
			model[key] = value
		}
		delete(result, key)
		changed = append(changed, key+" -> model."+key)
	}

	if model != nil {
		for _, alias := range []any{result["api_base"], model["api_base"]} {
			if isEmptyConfigValue(model["base_url"]) && !isEmptyConfigValue(alias) {
				model["base_url"] = alias
			}
		}
		if _, exists := result["api_base"]; exists {
			delete(result, "api_base")
			changed = append(changed, "api_base -> model.base_url")
		}
		if _, exists := model["api_base"]; exists {
			delete(model, "api_base")
			changed = append(changed, "model.api_base -> model.base_url")
		}

		if isEmptyConfigValue(model["default"]) {
			if !isEmptyConfigValue(model["model"]) {
				model["default"] = model["model"]
			} else if !isEmptyConfigValue(model["name"]) {
				model["default"] = model["name"]
			}
		}
		if !isEmptyConfigValue(model["default"]) {
			for _, alias := range []string{"model", "name"} {
				if _, exists := model[alias]; exists {
					delete(model, alias)
					changed = append(changed, "model."+alias+" -> model.default")
				}
			}
		}
	}

	return result, changed
}

func isEmptyConfigValue(value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) == ""
	}
	return false
}

func migrationBackupPattern(configPath string) string {
	return filepath.Base(configPath) + ".pre-h-go-v0.1.*.bak"
}
