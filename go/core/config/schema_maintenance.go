package config

import (
	"fmt"
	"sort"
	"strings"

	coretools "github.com/hey-meadow/meadow-code/go/core/tools"
)

func applyPostMigrationMaintenance(
	config map[string]any,
	paths *[]string,
) ([]string, []string) {
	normalizePlatformToolsetIdentity(config, paths)
	disabled, warnings := disableSuspiciousMCPServers(config, paths)
	warnings = append(
		warnings,
		coretools.ValidatePlatformToolsets(
			config["platform_toolsets"],
			coretools.IsNativeToolset,
		)...,
	)
	return disabled, warnings
}

func normalizePlatformToolsetIdentity(
	config map[string]any,
	paths *[]string,
) {
	platforms := mapValue(config["platform_toolsets"])
	if len(platforms) == 0 {
		return
	}
	normalized := cloneMap(platforms)
	changed := false
	for platform, raw := range platforms {
		value, valueChanged := normalizeToolsetValue(platform, raw)
		if !valueChanged {
			continue
		}
		normalized[platform] = value
		changed = true
	}
	if !changed {
		return
	}
	config["platform_toolsets"] = normalized
	*paths = append(
		*paths,
		"platform_toolsets legacy product prefixes -> Meadow",
	)
}

func normalizeToolsetValue(platform string, value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		return normalizeToolsetName(platform, typed)
	case []any:
		result := make([]any, len(typed))
		changed := false
		for index, item := range typed {
			name, ok := item.(string)
			if !ok {
				result[index] = item
				continue
			}
			normalized, itemChanged := normalizeToolsetName(platform, name)
			result[index] = normalized
			changed = changed || itemChanged
		}
		return result, changed
	default:
		return value, false
	}
}

func normalizeToolsetName(platform, name string) (string, bool) {
	const legacyPrefix = "hermes-"
	if strings.EqualFold(name, "hermes") {
		suggestion := "meadow-" + platform
		if coretools.IsNativeToolset(suggestion) {
			return suggestion, true
		}
	}
	if !strings.HasPrefix(strings.ToLower(name), legacyPrefix) {
		return name, false
	}
	return "meadow-" + name[len(legacyPrefix):], true
}

func disableSuspiciousMCPServers(
	config map[string]any,
	paths *[]string,
) ([]string, []string) {
	servers := mapValue(config["mcp_servers"])
	if len(servers) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	normalized := cloneMap(servers)
	var disabled []string
	var warnings []string
	changed := false
	for _, name := range names {
		entry := mapValue(servers[name])
		if entry == nil {
			continue
		}
		issues := coretools.ValidateMCPServerEntry(name, entry)
		if len(issues) == 0 {
			continue
		}
		warnings = append(warnings, issues...)
		displayName := safeMaintenanceName(name)
		if enabled, explicit := entry["enabled"].(bool); explicit && !enabled {
			warnings = append(warnings, fmt.Sprintf(
				"Suspicious MCP server %q remains disabled pending review",
				displayName,
			))
			continue
		}
		entry = cloneMap(entry)
		entry["enabled"] = false
		normalized[name] = entry
		disabled = append(disabled, displayName)
		warnings = append(warnings, fmt.Sprintf(
			"Disabled suspicious MCP server %q pending review",
			displayName,
		))
		*paths = append(
			*paths,
			"mcp_servers."+displayName+".enabled -> false",
		)
		changed = true
	}
	if changed {
		config["mcp_servers"] = normalized
	}
	return disabled, warnings
}

func safeMaintenanceName(name string) string {
	if strings.Contains(strings.ToLower(name), "hermes") {
		return "legacy-entry"
	}
	return name
}
