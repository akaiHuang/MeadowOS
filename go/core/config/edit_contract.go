package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

var explicitEnvironmentConfigKeys = map[string]struct{}{
	"OPENROUTER_API_KEY":      {},
	"OPENAI_API_KEY":          {},
	"ANTHROPIC_API_KEY":       {},
	"VOICE_TOOLS_OPENAI_KEY":  {},
	"EXA_API_KEY":             {},
	"PARALLEL_API_KEY":        {},
	"FIRECRAWL_API_KEY":       {},
	"FIRECRAWL_API_URL":       {},
	"FIRECRAWL_GATEWAY_URL":   {},
	"TOOL_GATEWAY_DOMAIN":     {},
	"TOOL_GATEWAY_SCHEME":     {},
	"TOOL_GATEWAY_USER_TOKEN": {},
	"TAVILY_API_KEY":          {},
	"BROWSERBASE_API_KEY":     {},
	"BROWSERBASE_PROJECT_ID":  {},
	"BROWSER_USE_API_KEY":     {},
	"FAL_KEY":                 {},
	"TELEGRAM_BOT_TOKEN":      {},
	"DISCORD_BOT_TOKEN":       {},
	"TERMINAL_SSH_HOST":       {},
	"TERMINAL_SSH_USER":       {},
	"TERMINAL_SSH_KEY":        {},
	"SUDO_PASSWORD":           {},
	"SLACK_BOT_TOKEN":         {},
	"SLACK_APP_TOKEN":         {},
	"GITHUB_TOKEN":            {},
	"HONCHO_API_KEY":          {},
}

var terminalConfigEnvironment = map[string]string{
	"backend":                         "TERMINAL_ENV",
	"modal_mode":                      "TERMINAL_MODAL_MODE",
	"cwd":                             "TERMINAL_CWD",
	"timeout":                         "TERMINAL_TIMEOUT",
	"lifetime_seconds":                "TERMINAL_LIFETIME_SECONDS",
	"docker_image":                    "TERMINAL_DOCKER_IMAGE",
	"docker_forward_env":              "TERMINAL_DOCKER_FORWARD_ENV",
	"singularity_image":               "TERMINAL_SINGULARITY_IMAGE",
	"modal_image":                     "TERMINAL_MODAL_IMAGE",
	"daytona_image":                   "TERMINAL_DAYTONA_IMAGE",
	"ssh_host":                        "TERMINAL_SSH_HOST",
	"ssh_user":                        "TERMINAL_SSH_USER",
	"ssh_port":                        "TERMINAL_SSH_PORT",
	"ssh_key":                         "TERMINAL_SSH_KEY",
	"container_cpu":                   "TERMINAL_CONTAINER_CPU",
	"container_memory":                "TERMINAL_CONTAINER_MEMORY",
	"container_disk":                  "TERMINAL_CONTAINER_DISK",
	"container_persistent":            "TERMINAL_CONTAINER_PERSISTENT",
	"docker_volumes":                  "TERMINAL_DOCKER_VOLUMES",
	"docker_env":                      "TERMINAL_DOCKER_ENV",
	"docker_mount_cwd_to_workspace":   "TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE",
	"docker_network":                  "TERMINAL_DOCKER_NETWORK",
	"docker_extra_args":               "TERMINAL_DOCKER_EXTRA_ARGS",
	"docker_run_as_host_user":         "TERMINAL_DOCKER_RUN_AS_HOST_USER",
	"docker_persist_across_processes": "TERMINAL_DOCKER_PERSIST_ACROSS_PROCESSES",
	"docker_orphan_reaper":            "TERMINAL_DOCKER_ORPHAN_REAPER",
	"sandbox_dir":                     "TERMINAL_SANDBOX_DIR",
	"persistent_shell":                "TERMINAL_PERSISTENT_SHELL",
}

var secretConfigKeys = map[string]struct{}{
	"api_key":       {},
	"apikey":        {},
	"key":           {},
	"token":         {},
	"access_token":  {},
	"refresh_token": {},
	"id_token":      {},
	"secret":        {},
	"client_secret": {},
	"password":      {},
	"passwd":        {},
	"auth":          {},
	"authorization": {},
	"private_key":   {},
	"bearer":        {},
	"jwt":           {},
}

// IsEnvironmentConfigKey matches the Python config set routing contract.
func IsEnvironmentConfigKey(key string) bool {
	if strings.Contains(key, ".") {
		return false
	}
	upper := strings.ToUpper(strings.TrimSpace(key))
	if _, found := explicitEnvironmentConfigKeys[upper]; found {
		return true
	}
	return strings.HasSuffix(upper, "_API_KEY") ||
		strings.HasSuffix(upper, "_TOKEN") ||
		strings.HasPrefix(upper, "TERMINAL_SSH")
}

// CoerceConfigValue preserves schema-declared strings and applies the Python
// CLI's legacy bool/int/float coercion to every other value.
func CoerceConfigValue(key, raw string) (any, error) {
	defaultValue, found, err := DefaultValue(key)
	if err != nil {
		return nil, err
	}
	if found {
		if _, stringDefault := defaultValue.(string); stringDefault {
			return raw, nil
		}
	}
	lower := strings.ToLower(raw)
	switch lower {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off":
		return false, nil
	}
	if raw != "" && allDigits(raw) {
		if value, parseErr := strconv.Atoi(raw); parseErr == nil {
			return value, nil
		}
	}
	if strings.Count(raw, ".") == 1 &&
		allDigits(strings.Replace(raw, ".", "", 1)) {
		if value, parseErr := strconv.ParseFloat(raw, 64); parseErr == nil {
			return value, nil
		}
	}
	return raw, nil
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !unicode.IsDigit(character) {
			return false
		}
	}
	return true
}

// TerminalEnvironmentForKey returns the compatibility variable mirrored by a
// terminal.* config setting.
func TerminalEnvironmentForKey(key string) string {
	const prefix = "terminal."
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	return terminalConfigEnvironment[strings.TrimPrefix(key, prefix)]
}

// TerminalEnvironmentValue formats one compatibility value as Python str/json.
func TerminalEnvironmentValue(value any) string {
	switch typed := value.(type) {
	case map[string]any, []any:
		encoded, err := json.Marshal(typed)
		if err == nil {
			return string(encoded)
		}
	case bool:
		if typed {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(value)
}

// IsSecretConfigLeaf identifies exact credential-shaped field names.
func IsSecretConfigLeaf(key string) bool {
	parts := strings.Split(key, ".")
	_, found := secretConfigKeys[strings.ToLower(parts[len(parts)-1])]
	return found
}

// RedactConfigValue recursively masks credential-shaped fields for show/set
// output while leaving benign names such as token_count untouched.
func RedactConfigValue(value any) any {
	return redactConfigValue(value, 0)
}

func redactConfigValue(value any, depth int) any {
	if depth > 20 {
		return value
	}
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if IsSecretConfigLeaf(key) {
				if text, ok := child.(string); ok && text != "" {
					result[key] = "[redacted]"
					continue
				}
			}
			result[key] = redactConfigValue(child, depth+1)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = redactConfigValue(child, depth+1)
		}
		return result
	default:
		return value
	}
}

// EnvPath returns the user .env adjacent to config.yaml.
func (d *Document) EnvPath() string {
	if d == nil || strings.TrimSpace(d.Path) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.Path), EnvFileName)
}

// LookupEnvValue resolves one value while preserving missing-vs-empty state.
func (d *Document) LookupEnvValue(key string) (string, bool) {
	return d.lookupEnvValue(key)
}
