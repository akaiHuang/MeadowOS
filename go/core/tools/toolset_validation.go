package tools

import (
	"fmt"
	"sort"
	"strings"
)

var nativeToolsets = map[string]struct{}{
	"*":                     {},
	"all":                   {},
	"browser":               {},
	"clarify":               {},
	"code_execution":        {},
	"coding":                {},
	"computer_use":          {},
	"context_engine":        {},
	"cronjob":               {},
	"debugging":             {},
	"delegation":            {},
	"discord":               {},
	"discord_admin":         {},
	"feishu_doc":            {},
	"feishu_drive":          {},
	"file":                  {},
	"homeassistant":         {},
	"image_gen":             {},
	"kanban":                {},
	"meadow-acp":            {},
	"meadow-api-server":     {},
	"meadow-bluebubbles":    {},
	"meadow-cli":            {},
	"meadow-cron":           {},
	"meadow-dingtalk":       {},
	"meadow-discord":        {},
	"meadow-email":          {},
	"meadow-feishu":         {},
	"meadow-gateway":        {},
	"meadow-homeassistant":  {},
	"meadow-matrix":         {},
	"meadow-mattermost":     {},
	"meadow-qqbot":          {},
	"meadow-signal":         {},
	"meadow-slack":          {},
	"meadow-sms":            {},
	"meadow-telegram":       {},
	"meadow-webhook":        {},
	"meadow-wecom":          {},
	"meadow-wecom-callback": {},
	"meadow-weixin":         {},
	"meadow-whatsapp":       {},
	"meadow-yuanbao":        {},
	"memory":                {},
	"project":               {},
	"safe":                  {},
	"search":                {},
	"session_search":        {},
	"skills":                {},
	"spotify":               {},
	"terminal":              {},
	"todo":                  {},
	"tts":                   {},
	"video":                 {},
	"video_gen":             {},
	"vision":                {},
	"web":                   {},
	"x_search":              {},
	"yuanbao":               {},
}

// IsNativeToolset reports whether a public Meadow built-in toolset exists.
func IsNativeToolset(name string) bool {
	_, exists := nativeToolsets[name]
	return exists
}

// NativeToolsetNames returns a sorted defensive snapshot.
func NativeToolsetNames() []string {
	names := make([]string, 0, len(nativeToolsets))
	for name := range nativeToolsets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ValidatePlatformToolsets warns when a platform references an unknown
// toolset, or when a non-empty mapping resolves to zero valid toolsets.
func ValidatePlatformToolsets(
	raw any,
	isValid func(string) bool,
) []string {
	platforms, ok := raw.(map[string]any)
	if !ok || len(platforms) == 0 || isValid == nil {
		return nil
	}
	keys := make([]string, 0, len(platforms))
	for platform := range platforms {
		keys = append(keys, platform)
	}
	sort.Strings(keys)

	validCount := 0
	var warnings []string
	for _, platform := range keys {
		names := toolsetNamesFromValue(platforms[platform])
		for _, name := range names {
			if name == "" {
				continue
			}
			if isValid(name) {
				validCount++
				continue
			}
			suggestion := "meadow-" + platform
			hint := ""
			if isValid(suggestion) {
				hint = fmt.Sprintf("; did you mean %q?", suggestion)
			}
			warnings = append(warnings, fmt.Sprintf(
				"platform %q references unknown toolset %q%s",
				safeToolsetDisplayName(platform),
				safeToolsetDisplayName(name),
				hint,
			))
		}
	}
	if validCount == 0 {
		warnings = append(
			warnings,
			"platform_toolsets resolves to zero valid toolsets; the agent "+
				"will have no tools. Run `meadow tools` to reconfigure.",
		)
	}
	return warnings
}

func toolsetNamesFromValue(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		names := make([]string, 0, len(typed))
		for _, item := range typed {
			if name, ok := item.(string); ok {
				names = append(names, name)
			}
		}
		return names
	case []string:
		return append([]string(nil), typed...)
	default:
		return nil
	}
}

func safeToolsetDisplayName(name string) string {
	if strings.Contains(strings.ToLower(name), "hermes") {
		return "legacy-toolset"
	}
	return name
}
