package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultManagedDir = "/etc/meadow"

var envReferencePattern = regexp.MustCompile(`\$\{([^}]+)\}`)

func (d *Document) loadEnvironmentAndManagedScope() error {
	d.userEnv = map[string]string{}
	d.bootstrapEnv = map[string]string{}
	d.secretEnv = map[string]string{}
	d.secretSource = map[string]string{}
	if strings.TrimSpace(d.Path) != "" {
		userDir := filepath.Dir(d.Path)
		userEnv, warnings, err := loadEnvFile(filepath.Join(userDir, EnvFileName))
		if err != nil {
			return fmt.Errorf("load %s: %w", filepath.Join(userDir, EnvFileName), err)
		}
		d.userEnv = userEnv
		d.warnings = append(d.warnings, warnings...)

		bootstrapPath := filepath.Join(userDir, ".op.env")
		bootstrapEnv, bootstrapWarnings, bootstrapErr := loadEnvFile(bootstrapPath)
		if bootstrapErr != nil {
			d.warnings = append(
				d.warnings,
				fmt.Sprintf(
					"%s: bootstrap environment could not be read: %v",
					bootstrapPath,
					bootstrapErr,
				),
			)
		} else {
			d.bootstrapEnv = bootstrapEnv
			d.warnings = append(d.warnings, bootstrapWarnings...)
		}
	}
	d.managedRaw = map[string]any{}
	d.managedEnv = map[string]string{}
	d.managedKeys = map[string]struct{}{}

	managedDir := resolveManagedDir()
	if managedDir == "" {
		return nil
	}
	d.managedDir = managedDir

	managedEnv, managedWarnings, envErr := loadEnvFile(filepath.Join(managedDir, EnvFileName))
	if envErr != nil {
		d.warnings = append(d.warnings, fmt.Sprintf(
			"managed scope cannot read %s: %v; managed environment was ignored",
			filepath.Join(managedDir, EnvFileName),
			envErr,
		))
	} else {
		d.managedEnv = managedEnv
		d.warnings = append(d.warnings, managedWarnings...)
	}

	managedPath := filepath.Join(managedDir, FileName)
	data, readErr := os.ReadFile(managedPath)
	if os.IsNotExist(readErr) {
		return nil
	}
	if readErr != nil {
		d.warnings = append(d.warnings, fmt.Sprintf(
			"managed scope cannot read %s: %v; managed config was ignored",
			managedPath,
			readErr,
		))
		return nil
	}
	raw := map[string]any{}
	if unmarshalErr := yaml.Unmarshal(data, &raw); unmarshalErr != nil {
		d.warnings = append(d.warnings, fmt.Sprintf(
			"managed scope cannot parse %s: %v; managed config was ignored",
			managedPath,
			unmarshalErr,
		))
		return nil
	}
	if raw == nil {
		raw = map[string]any{}
	}
	normalized, _ := normalizeLegacyAliases(raw)
	if model, ok := normalized["model"].(string); ok && strings.TrimSpace(model) != "" {
		normalized["model"] = map[string]any{"default": model}
	}
	d.managedRaw = normalized
	flattenLeafKeys(d.managedRaw, "", d.managedKeys)
	return nil
}

func resolveManagedDir() string {
	if configured, present := os.LookupEnv("MEADOW_MANAGED_DIR"); present {
		configured = strings.TrimSpace(configured)
		if configured == "" {
			return ""
		}
		if info, err := os.Stat(configured); err == nil && info.IsDir() {
			return configured
		}
		return ""
	}
	if info, err := os.Stat(DefaultManagedDir); err == nil && info.IsDir() {
		return DefaultManagedDir
	}
	return ""
}

func (d *Document) effectiveRaw() map[string]any {
	if d == nil {
		return nil
	}
	user, _ := normalizeLegacyAliases(cloneMap(d.Raw))
	user = mapValue(expandEnvReferences(user, d.lookupEnvValue))
	managed := mapValue(expandEnvReferences(cloneMap(d.managedRaw), d.lookupEnvValue))
	return deepMerge(user, managed)
}

func expandEnvReferences(value any, resolve func(string) (string, bool)) any {
	switch typed := value.(type) {
	case string:
		return envReferencePattern.ReplaceAllStringFunc(typed, func(match string) string {
			groups := envReferencePattern.FindStringSubmatch(match)
			if len(groups) != 2 {
				return match
			}
			if replacement, exists := resolve(groups[1]); exists {
				return replacement
			}
			return match
		})
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[key] = expandEnvReferences(child, resolve)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = expandEnvReferences(child, resolve)
		}
		return result
	default:
		return value
	}
}

func stripManagedLeaves(root map[string]any, managed map[string]struct{}) {
	for dotted := range managed {
		parts := strings.Split(dotted, ".")
		current := root
		for _, part := range parts[:len(parts)-1] {
			next := mapValue(current[part])
			if next == nil {
				current = nil
				break
			}
			current = next
		}
		if current != nil {
			delete(current, parts[len(parts)-1])
		}
	}
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = cloneValue(value)
	}
	return result
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = cloneValue(child)
		}
		return result
	default:
		return value
	}
}

func deepMerge(base, overlay map[string]any) map[string]any {
	result := cloneMap(base)
	for key, value := range overlay {
		baseMap := mapValue(result[key])
		overlayMap := mapValue(value)
		switch {
		case baseMap != nil && overlayMap != nil:
			result[key] = deepMerge(baseMap, overlayMap)
		case baseMap != nil && value == nil:
			continue
		default:
			result[key] = cloneValue(value)
		}
	}
	return result
}

func flattenLeafKeys(value map[string]any, prefix string, output map[string]struct{}) {
	for key, child := range value {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if nested := mapValue(child); nested != nil && len(nested) > 0 {
			flattenLeafKeys(nested, path, output)
			continue
		}
		output[path] = struct{}{}
	}
}

func (d *Document) isManagedPath(path []string) bool {
	if d == nil || len(d.managedKeys) == 0 {
		return false
	}
	dotted := strings.Join(path, ".")
	if _, ok := d.managedKeys[dotted]; ok {
		return true
	}
	prefix := dotted + "."
	for managed := range d.managedKeys {
		if strings.HasPrefix(managed, prefix) {
			return true
		}
	}
	return false
}
