package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type schemaMigrationEffects struct {
	clearEnv   []string
	createDirs []string
}

var providerTemplatePattern = regexp.MustCompile(`\{[^}]+\}`)

var localSTTModels = map[string]struct{}{
	"tiny.en":           {},
	"tiny":              {},
	"base.en":           {},
	"base":              {},
	"small.en":          {},
	"small":             {},
	"medium.en":         {},
	"medium":            {},
	"large-v1":          {},
	"large-v2":          {},
	"large-v3":          {},
	"large":             {},
	"distil-large-v2":   {},
	"distil-medium.en":  {},
	"distil-small.en":   {},
	"distil-large-v3":   {},
	"distil-large-v3.5": {},
	"large-v3-turbo":    {},
	"turbo":             {},
}

func applyLegacySchemaMigrations(
	document *Document,
	config map[string]any,
	current int,
	paths *[]string,
	effects *schemaMigrationEffects,
) {
	if current < 4 {
		migrateLegacyToolProgress(document, config, paths)
	}
	if current < 5 {
		migrateLegacyTimezone(config, paths)
	}
	if current < 9 && document.EnvValue("ANTHROPIC_TOKEN") != "" {
		effects.clearEnv = appendUnique(effects.clearEnv, "ANTHROPIC_TOKEN")
	}
	if current < 12 {
		migrateLegacyCustomProviders(config, paths)
	}
	if current < 13 {
		for _, name := range []string{"LLM_MODEL", "OPENAI_MODEL"} {
			if document.EnvValue(name) != "" {
				effects.clearEnv = appendUnique(effects.clearEnv, name)
			}
		}
	}
	if current < 14 {
		migrateLegacySTT(config, paths)
	}
	if current < 16 {
		migrateLegacyToolProgressOverrides(config, paths)
	}
	if current < 17 {
		migrateLegacyCompression(config, paths)
	}
	if current < 21 {
		migrateLegacyPlugins(document, config, paths)
	}
	if current < 23 {
		effects.createDirs = appendUnique(
			effects.createDirs,
			filepath.Join(filepath.Dir(document.Path), "logs", "curator"),
		)
	}
	if current < 25 {
		migrateLegacyModelCatalog(config, paths)
	}
}

func migrateLegacyToolProgress(
	document *Document,
	config map[string]any,
	paths *[]string,
) {
	display := mapValue(config["display"])
	if display == nil {
		display = map[string]any{}
	} else if _, exists := display["tool_progress"]; exists {
		return
	} else {
		display = cloneMap(display)
	}
	mode := "all"
	enabled := strings.ToLower(strings.TrimSpace(
		document.EnvValue("HERMES_TOOL_PROGRESS"),
	))
	legacyMode := strings.ToLower(strings.TrimSpace(
		document.EnvValue("HERMES_TOOL_PROGRESS_MODE"),
	))
	switch {
	case enabled == "false" || enabled == "0" || enabled == "no":
		mode = "off"
	case legacyMode == "new" || legacyMode == "all" || legacyMode == "verbose":
		mode = legacyMode
	}
	display["tool_progress"] = mode
	config["display"] = display
	*paths = append(
		*paths,
		"display.tool_progress <- legacy environment ("+mode+")",
	)
}

func migrateLegacyTimezone(config map[string]any, paths *[]string) {
	if _, exists := config["timezone"]; exists {
		return
	}
	timezone := strings.TrimSpace(os.Getenv("HERMES_TIMEZONE"))
	if timezone == "" {
		return
	}
	config["timezone"] = timezone
	*paths = append(*paths, "timezone <- legacy environment")
}

func migrateLegacyCustomProviders(config map[string]any, paths *[]string) {
	custom, ok := config["custom_providers"].([]any)
	if !ok || len(custom) == 0 {
		return
	}
	providers := mapValue(config["providers"])
	if providers == nil {
		providers = map[string]any{}
	} else {
		providers = cloneMap(providers)
	}
	migrated := 0
	for _, raw := range custom {
		entry := mapValue(raw)
		if entry == nil {
			continue
		}
		oldName := stringValue(entry["name"])
		oldURL := firstString(entry, "base_url", "baseUrl", "url", "api")
		if oldURL == "" {
			continue
		}
		key := providerMigrationKey(oldName, oldURL, migrated)
		baseKey := key
		suffix := migrated
		for {
			if _, exists := providers[key]; !exists {
				break
			}
			key = fmt.Sprintf("%s-%d", baseKey, suffix)
			suffix++
		}
		normalized := normalizeLegacyProvider(entry, key)
		if normalized == nil {
			continue
		}
		if oldName == "" {
			delete(normalized, "name")
		}
		switch normalized["api_key"] {
		case "no-key", "no-key-required", "":
			delete(normalized, "api_key")
		}
		providers[key] = normalized
		migrated++
	}
	if migrated == 0 {
		return
	}
	config["providers"] = providers
	delete(config, "custom_providers")
	*paths = append(
		*paths,
		fmt.Sprintf("custom_providers -> providers (%d)", migrated),
	)
}

func providerMigrationKey(name, baseURL string, migrated int) string {
	key := strings.ToLower(strings.TrimSpace(name))
	key = strings.ReplaceAll(key, " ", "-")
	key = strings.ReplaceAll(key, "(", "")
	key = strings.ReplaceAll(key, ")", "")
	for strings.Contains(key, "--") {
		key = strings.ReplaceAll(key, "--", "-")
	}
	key = strings.Trim(key, "-")
	if key != "" {
		return key
	}
	parsed, err := url.Parse(baseURL)
	if err == nil && parsed.Hostname() != "" {
		return strings.ReplaceAll(parsed.Hostname(), ".", "-")
	}
	return fmt.Sprintf("endpoint-%d", migrated)
}

func normalizeLegacyProvider(
	source map[string]any,
	providerKey string,
) map[string]any {
	entry := cloneMap(source)
	aliases := map[string]string{
		"apiKey":         "api_key",
		"baseUrl":        "base_url",
		"apiMode":        "api_mode",
		"keyEnv":         "key_env",
		"apiKeyEnv":      "key_env",
		"api_key_env":    "key_env",
		"defaultModel":   "default_model",
		"contextLength":  "context_length",
		"rateLimitDelay": "rate_limit_delay",
	}
	for alias, canonical := range aliases {
		if _, exists := entry[canonical]; exists {
			continue
		}
		if value, exists := entry[alias]; exists {
			entry[canonical] = value
		}
	}

	baseURL := ""
	for _, key := range []string{"base_url", "url", "api"} {
		candidate, ok := entry[key].(string)
		candidate = strings.TrimSpace(candidate)
		if !ok || candidate == "" {
			continue
		}
		if providerTemplatePattern.MatchString(candidate) {
			baseURL = candidate
			break
		}
		parsed, err := url.Parse(candidate)
		if err == nil && parsed.Scheme != "" && parsed.Host != "" {
			baseURL = candidate
			break
		}
	}
	if baseURL == "" {
		return nil
	}
	name := stringValue(entry["name"])
	if name == "" {
		name = strings.TrimSpace(providerKey)
	}
	if name == "" {
		return nil
	}

	result := map[string]any{
		"api":  baseURL,
		"name": name,
	}
	copyTrimmedString(result, entry, "api_key", "api_key")
	copyTrimmedString(result, entry, "key_env", "key_env")
	if value := firstString(entry, "model", "default_model"); value != "" {
		result["default_model"] = value
	}
	if value := firstString(entry, "api_mode", "transport"); value != "" {
		result["transport"] = value
	}
	if models := normalizeProviderModels(entry["models"]); len(models) > 0 {
		result["models"] = models
	}
	if value, valid := positiveMigrationInt(entry["context_length"]); valid {
		result["context_length"] = value
	}
	if value, valid := nonnegativeMigrationNumber(entry["rate_limit_delay"]); valid {
		result["rate_limit_delay"] = value
	}
	if value, ok := entry["discover_models"].(bool); ok {
		result["discover_models"] = value
	}
	if value := mapValue(entry["extra_body"]); value != nil {
		result["extra_body"] = cloneMap(value)
	}
	if value := normalizeProviderHeaders(entry["extra_headers"]); len(value) > 0 {
		result["extra_headers"] = value
	}
	copyTrimmedString(result, entry, "ssl_ca_cert", "ssl_ca_cert")
	switch value := entry["ssl_verify"].(type) {
	case bool:
		result["ssl_verify"] = value
	case string:
		if value = strings.TrimSpace(value); value != "" {
			result["ssl_verify"] = value
		}
	}
	return result
}

func copyTrimmedString(
	target map[string]any,
	source map[string]any,
	sourceKey string,
	targetKey string,
) {
	if value := stringValue(source[sourceKey]); value != "" {
		target[targetKey] = value
	}
}

func normalizeProviderModels(value any) map[string]any {
	if values := mapValue(value); len(values) > 0 {
		return cloneMap(values)
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := map[string]any{}
	for _, item := range items {
		if name, ok := item.(string); ok {
			if name = strings.TrimSpace(name); name != "" {
				result[name] = map[string]any{}
			}
			continue
		}
		model := mapValue(item)
		if model == nil {
			continue
		}
		name := firstString(model, "id", "name")
		if name == "" {
			continue
		}
		metadata := cloneMap(model)
		delete(metadata, "id")
		delete(metadata, "name")
		result[name] = metadata
	}
	return result
}

func normalizeProviderHeaders(value any) map[string]any {
	headers := mapValue(value)
	if len(headers) == 0 {
		return nil
	}
	result := map[string]any{}
	for key, raw := range headers {
		if raw == nil {
			continue
		}
		result[fmt.Sprint(key)] = pythonString(raw)
	}
	return result
}

func pythonString(value any) string {
	switch typed := value.(type) {
	case bool:
		if typed {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprint(value)
	}
}

func positiveMigrationInt(value any) (int, bool) {
	number, valid := migrationInt(value)
	return number, valid && number > 0
}

func nonnegativeMigrationNumber(value any) (any, bool) {
	switch number := value.(type) {
	case int:
		return number, number >= 0
	case int8:
		return number, number >= 0
	case int16:
		return number, number >= 0
	case int32:
		return number, number >= 0
	case int64:
		return number, number >= 0
	case uint:
		return number, true
	case uint8:
		return number, true
	case uint16:
		return number, true
	case uint32:
		return number, true
	case uint64:
		return number, true
	case float32:
		return number, number >= 0
	case float64:
		return number, number >= 0
	default:
		return nil, false
	}
}

func migrateLegacySTT(config map[string]any, paths *[]string) {
	stt := mapValue(config["stt"])
	if stt == nil {
		return
	}
	legacyModel, exists := stt["model"]
	if !exists {
		return
	}
	stt = cloneMap(stt)
	delete(stt, "model")
	provider := firstString(stt, "provider")
	if provider == "" {
		provider = "local"
	}
	switch provider {
	case "local", "local_command":
		model, ok := legacyModel.(string)
		if ok {
			if _, valid := localSTTModels[model]; valid {
				nested := mapValue(stt["local"])
				if nested == nil {
					nested = map[string]any{}
				} else {
					nested = cloneMap(nested)
				}
				if _, alreadySet := nested["model"]; !alreadySet {
					nested["model"] = legacyModel
					stt["local"] = nested
				}
			}
		}
	default:
		nested := mapValue(stt[provider])
		if nested == nil {
			nested = map[string]any{}
		} else {
			nested = cloneMap(nested)
		}
		if _, alreadySet := nested["model"]; !alreadySet {
			nested["model"] = legacyModel
			stt[provider] = nested
		}
	}
	config["stt"] = stt
	*paths = append(*paths, "stt.model -> provider-specific stt model")
}

func migrateLegacyToolProgressOverrides(
	config map[string]any,
	paths *[]string,
) {
	display := mapValue(config["display"])
	if display == nil {
		return
	}
	overrides := mapValue(display["tool_progress_overrides"])
	if len(overrides) == 0 {
		return
	}
	display = cloneMap(display)
	platforms := mapValue(display["platforms"])
	if platforms == nil {
		platforms = map[string]any{}
	} else {
		platforms = cloneMap(platforms)
	}
	keys := make([]string, 0, len(overrides))
	for platform := range overrides {
		keys = append(keys, platform)
	}
	sort.Strings(keys)
	for _, platform := range keys {
		settings := mapValue(platforms[platform])
		if settings == nil {
			settings = map[string]any{}
		} else {
			settings = cloneMap(settings)
		}
		if _, exists := settings["tool_progress"]; !exists {
			settings["tool_progress"] = overrides[platform]
		}
		platforms[platform] = settings
	}
	display["platforms"] = platforms
	config["display"] = display
	*paths = append(
		*paths,
		"display.tool_progress_overrides -> display.platforms",
	)
}

func migrateLegacyCompression(config map[string]any, paths *[]string) {
	compression := mapValue(config["compression"])
	if compression == nil {
		return
	}
	_, hasModel := compression["summary_model"]
	_, hasProvider := compression["summary_provider"]
	_, hasBaseURL := compression["summary_base_url"]
	if !hasModel && !hasProvider && !hasBaseURL {
		return
	}
	compression = cloneMap(compression)
	summaryModel := compression["summary_model"]
	summaryProvider := compression["summary_provider"]
	summaryBaseURL := compression["summary_base_url"]
	delete(compression, "summary_model")
	delete(compression, "summary_provider")
	delete(compression, "summary_base_url")
	config["compression"] = compression

	auxiliary := mapValue(config["auxiliary"])
	if auxiliary == nil {
		auxiliary = map[string]any{}
	} else {
		auxiliary = cloneMap(auxiliary)
	}
	auxCompression := mapValue(auxiliary["compression"])
	if auxCompression == nil {
		auxCompression = map[string]any{}
	} else {
		auxCompression = cloneMap(auxCompression)
	}
	if value := migrationText(summaryModel); pythonTruthy(summaryModel) &&
		value != "" && pythonFalsy(auxCompression["model"]) {
		auxCompression["model"] = value
	}
	if value := migrationText(summaryProvider); pythonTruthy(summaryProvider) &&
		value != "" &&
		!strings.EqualFold(value, "auto") &&
		(pythonFalsy(auxCompression["provider"]) ||
			strings.EqualFold(stringValue(auxCompression["provider"]), "auto")) {
		auxCompression["provider"] = value
	}
	if value := migrationText(summaryBaseURL); pythonTruthy(summaryBaseURL) &&
		value != "" && pythonFalsy(auxCompression["base_url"]) {
		auxCompression["base_url"] = value
	}
	if len(auxCompression) > 0 {
		auxiliary["compression"] = auxCompression
		config["auxiliary"] = auxiliary
	}
	*paths = append(
		*paths,
		"compression.summary_* -> auxiliary.compression",
	)
}

func migrationText(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(pythonString(value))
}

func pythonTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case int:
		return typed != 0
	case int8:
		return typed != 0
	case int16:
		return typed != 0
	case int32:
		return typed != 0
	case int64:
		return typed != 0
	case uint:
		return typed != 0
	case uint8:
		return typed != 0
	case uint16:
		return typed != 0
	case uint32:
		return typed != 0
	case uint64:
		return typed != 0
	case float32:
		return typed != 0
	case float64:
		return typed != 0
	case []any:
		return len(typed) != 0
	case map[string]any:
		return len(typed) != 0
	default:
		return true
	}
}

func pythonFalsy(value any) bool {
	return !pythonTruthy(value)
}

func migrateLegacyPlugins(
	document *Document,
	config map[string]any,
	paths *[]string,
) {
	plugins := mapValue(config["plugins"])
	if plugins == nil {
		plugins = map[string]any{}
	} else {
		if _, exists := plugins["enabled"]; exists {
			return
		}
		plugins = cloneMap(plugins)
	}
	disabled := map[string]struct{}{}
	if entries, ok := plugins["disabled"].([]any); ok {
		for _, entry := range entries {
			if name, ok := entry.(string); ok {
				disabled[name] = struct{}{}
			}
		}
	}
	enabled := discoverLegacyPlugins(
		filepath.Join(filepath.Dir(document.Path), "plugins"),
		disabled,
	)
	plugins["enabled"] = enabled
	config["plugins"] = plugins
	*paths = append(
		*paths,
		fmt.Sprintf("plugins.enabled <- installed user plugins (%d)", len(enabled)),
	)
}

func discoverLegacyPlugins(
	root string,
	disabled map[string]struct{},
) []any {
	children, err := os.ReadDir(root)
	if err != nil {
		return []any{}
	}
	result := make([]any, 0, len(children))
	for _, child := range children {
		if !child.IsDir() {
			continue
		}
		manifestPath := filepath.Join(root, child.Name(), "plugin.yaml")
		if _, err := os.Stat(manifestPath); err != nil {
			manifestPath = filepath.Join(root, child.Name(), "plugin.yml")
			if _, err := os.Stat(manifestPath); err != nil {
				continue
			}
		}
		name := child.Name()
		if data, err := os.ReadFile(manifestPath); err == nil {
			manifest := map[string]any{}
			if yaml.Unmarshal(data, &manifest) == nil {
				if declared := stringValue(manifest["name"]); declared != "" {
					name = declared
				}
			}
		}
		if _, blocked := disabled[name]; blocked {
			continue
		}
		result = append(result, name)
	}
	return result
}

func migrateLegacyModelCatalog(config map[string]any, paths *[]string) {
	catalog := mapValue(config["model_catalog"])
	if catalog == nil {
		return
	}
	ttl, exactInt := catalog["ttl_hours"].(int)
	if !exactInt || ttl != 24 {
		return
	}
	catalog = cloneMap(catalog)
	catalog["ttl_hours"] = 1
	config["model_catalog"] = catalog
	*paths = append(*paths, "model_catalog.ttl_hours 24 -> 1")
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
