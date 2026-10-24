package tools

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var shellInterpreters = map[string]struct{}{
	"bash":           {},
	"sh":             {},
	"zsh":            {},
	"dash":           {},
	"fish":           {},
	"cmd":            {},
	"cmd.exe":        {},
	"powershell":     {},
	"powershell.exe": {},
	"pwsh":           {},
	"pwsh.exe":       {},
}

var mcpEgressPattern = regexp.MustCompile(
	`(?i)(^|[^\w.-])(?:curl|wget|nc|ncat|socat)(?:[^\w.-]|$)` +
		`|/dev/tcp/` +
		`|\bInvoke-WebRequest\b` +
		`|\bInvoke-RestMethod\b` +
		`|\bSystem\.Net\.WebClient\b`,
)

var mcpExfilHintPattern = regexp.MustCompile(
	`(?i)\.env\b|--data-binary|--data-raw|\b-X\s+POST\b|\bPOST\b|<\s*[^\s]+`,
)

var mcpPersistencePattern = regexp.MustCompile(
	`(?i)authorized_keys` +
		`|\.ssh/` +
		`|/etc/ssh\b` +
		`|/etc/pam\.d\b|pam_[\w-]+\.so` +
		`|/etc/sudoers` +
		`|/etc/cron|crontab\b` +
		`|/etc/rc\.local|/etc/systemd` +
		`|\.bashrc\b|\.bash_profile\b|\.profile\b|\.zshrc\b`,
)

var mcpCampaignIndicators = []string{
	"AAAAC3NzaC1lZDI1NTE5AAAAICBoh1oDC4DnsO1m5mJ4yfEKrQebaFh",
	"hermes-0day",
	"60.165.167.",
	"118.182.244.156",
	"61.178.123.196",
}

// ValidateMCPServerEntry returns warnings for narrow, high-signal malicious
// stdio server shapes. It intentionally remains a blocklist rather than
// treating arbitrary local MCP commands as invalid.
func ValidateMCPServerEntry(
	name string,
	entry map[string]any,
) []string {
	if entry == nil {
		return nil
	}
	displayName := safeMCPDisplayName(name)
	flat := flattenMCPEntry(entry)
	for _, indicator := range mcpCampaignIndicators {
		if strings.Contains(flat, indicator) {
			return []string{fmt.Sprintf(
				"MCP server %q contains a known campaign indicator-of-compromise",
				displayName,
			)}
		}
	}

	command := stringFromAny(entry["command"])
	basename := commandBasename(command)
	if _, shell := shellInterpreters[basename]; !shell {
		return nil
	}
	script := inlineMCPScript(entry["args"])
	if script == "" {
		return nil
	}

	var warnings []string
	if mcpEgressPattern.MatchString(script) {
		warning := fmt.Sprintf(
			"MCP server %q uses shell interpreter %q with network egress in args",
			displayName,
			basename,
		)
		if mcpExfilHintPattern.MatchString(script) {
			warning += " and exfiltration-shaped arguments"
		}
		warnings = append(warnings, warning)
	}
	if mcpPersistencePattern.MatchString(script) {
		warnings = append(warnings, fmt.Sprintf(
			"MCP server %q uses shell interpreter %q to write to an OS "+
				"persistence surface (SSH keys, PAM, sudoers, cron, or shell rc); "+
				"this is backdoor-shaped, not a normal MCP server",
			displayName,
			basename,
		))
	}
	return warnings
}

// IsMCPServerEntrySuspicious reports whether the entry must be refused or
// disabled before a helper process can be spawned.
func IsMCPServerEntrySuspicious(name string, entry map[string]any) bool {
	return len(ValidateMCPServerEntry(name, entry)) != 0
}

func flattenMCPEntry(entry map[string]any) string {
	parts := []string{stringFromAny(entry["command"])}
	parts = append(parts, inlineMCPScript(entry["args"]))
	if environment, ok := entry["env"].(map[string]any); ok {
		for _, value := range environment {
			parts = append(parts, fmt.Sprint(value))
		}
	}
	return strings.Join(parts, " ")
}

func inlineMCPScript(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, " ")
	case []string:
		return strings.Join(typed, " ")
	default:
		return fmt.Sprint(value)
	}
}

func commandBasename(command string) string {
	command = strings.ReplaceAll(strings.TrimSpace(command), `\`, "/")
	first := firstCommandWord(command)
	return strings.ToLower(path.Base(first))
}

func firstCommandWord(command string) string {
	if command == "" {
		return ""
	}
	var word strings.Builder
	var quote rune
	escaped := false
	for _, character := range command {
		switch {
		case escaped:
			word.WriteRune(character)
			escaped = false
		case character == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if character == quote {
				quote = 0
			} else {
				word.WriteRune(character)
			}
		case character == '\'' || character == '"':
			quote = character
		case character == ' ' || character == '\t' || character == '\n':
			return word.String()
		default:
			word.WriteRune(character)
		}
	}
	if escaped {
		word.WriteRune('\\')
	}
	return word.String()
}

func safeMCPDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unnamed"
	}
	if strings.Contains(strings.ToLower(name), "hermes") {
		return "legacy-entry"
	}
	return name
}

func stringFromAny(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
