package secrets

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const defaultOnePasswordTokenEnv = "OP_SERVICE_ACCOUNT_TOKEN"

var onePasswordEnvironmentAllowlist = []string{
	"PATH",
	"HOME",
	"USERPROFILE",
	"APPDATA",
	"LOCALAPPDATA",
	"SystemRoot",
	"TMPDIR",
	"TMP",
	"TEMP",
	"XDG_CONFIG_HOME",
	"XDG_RUNTIME_DIR",
	"OP_ACCOUNT",
	"OP_CONNECT_HOST",
	"OP_CONNECT_TOKEN",
}

type onePasswordSource struct{}

var (
	onePasswordMemoryCache = NewMemoryCache()
	onePasswordDiskCache   = DiskCache{BaseName: "op_cache.json"}
)

func NewOnePasswordSource() Source {
	return onePasswordSource{}
}

// LocateOnePassword resolves the configured op helper without executing it.
func LocateOnePassword(
	config map[string]any,
	environment Environment,
) (string, error) {
	return findExecutable(
		stringConfig(config, "binary_path", ""),
		"op",
		environment,
	)
}

func (onePasswordSource) Descriptor() Descriptor {
	return Descriptor{
		Name:       "onepassword",
		Label:      "1Password",
		Shape:      ShapeMapped,
		Scheme:     "op",
		APIVersion: APIVersion,
	}
}

func (onePasswordSource) Enabled(config map[string]any) bool {
	return boolConfig(config, "enabled", false)
}

func (onePasswordSource) OverrideExisting(config map[string]any) bool {
	return boolConfig(config, "override_existing", true)
}

func (onePasswordSource) ProtectedEnvVars(config map[string]any) []string {
	return []string{
		stringConfig(
			config,
			"service_account_token_env",
			defaultOnePasswordTokenEnv,
		),
	}
}

func (onePasswordSource) Fetch(
	ctx context.Context,
	request FetchRequest,
) FetchResult {
	result := FetchResult{Secrets: map[string]string{}}
	references := mapConfig(request.Config, "env")
	if len(references) == 0 {
		result.Error = "secrets.onepassword.env is empty"
		result.ErrorKind = ErrorNotConfigured
		return result
	}

	validReferences := map[string]string{}
	for _, name := range sortedAnyMapKeys(references) {
		if !validEnvName(name) {
			result.Warnings = append(
				result.Warnings,
				fmt.Sprintf(
					"1Password skipped %q: invalid environment name",
					name,
				),
			)
			continue
		}
		reference, ok := references[name].(string)
		reference = strings.TrimSpace(reference)
		if !ok || !strings.HasPrefix(reference, "op://") {
			result.Warnings = append(
				result.Warnings,
				fmt.Sprintf(
					"1Password skipped %q: invalid op:// reference",
					name,
				),
			)
			continue
		}
		validReferences[name] = reference
	}
	if len(validReferences) == 0 {
		return result
	}

	binary, err := LocateOnePassword(request.Config, request.Env)
	if err != nil {
		result.Error = "1Password op CLI was not found or is not executable"
		result.ErrorKind = ErrorBinaryMissing
		return result
	}
	result.BinaryPath = binary

	ttl := floatConfig(request.Config, "cache_ttl_seconds", 300)
	useCache := boolConfig(request.Config, "use_cache", true) && ttl > 0
	memoryKey, diskKey := onePasswordCacheKeys(request, validReferences)
	now := time.Now()
	if useCache {
		if cached, found := onePasswordMemoryCache.Get(memoryKey, ttl, now); found {
			result.Secrets = cached.Secrets
			return result
		}
		if cached, found := onePasswordDiskCache.Read(
			diskKey,
			ttl,
			request.Home,
			now,
		); found {
			onePasswordMemoryCache.Put(memoryKey, cached)
			result.Secrets = cached.Secrets
			return result
		}
	}

	childEnvironment := minimalEnvironment(
		request.Env,
		onePasswordEnvironmentAllowlist,
	)
	for name, value := range request.Env {
		if strings.HasPrefix(name, "OP_SESSION_") {
			childEnvironment[name] = value
		}
	}
	tokenEnv := stringConfig(
		request.Config,
		"service_account_token_env",
		defaultOnePasswordTokenEnv,
	)
	if token, exists := request.Env.Lookup(tokenEnv); exists &&
		strings.TrimSpace(token) != "" {
		childEnvironment[defaultOnePasswordTokenEnv] = token
	}

	account := stringConfig(request.Config, "account", "")
	readErrors := 0
	for _, name := range sortedStringMapKeys(validReferences) {
		reference := validReferences[name]
		args := []string{"read"}
		if account != "" {
			args = append(args, "--account", account)
		}
		args = append(args, "--", reference)
		command := runCommand(ctx, binary, args, childEnvironment)
		if command.err != nil {
			message, kind := commandError("op read", command)
			message = redactSensitiveEnvironment(message, childEnvironment)
			result.Warnings = append(
				result.Warnings,
				fmt.Sprintf(
					"1Password could not resolve %q (%s): %s",
					name,
					kind,
					message,
				),
			)
			readErrors++
			continue
		}
		value := strings.TrimRight(command.stdout, "\r\n")
		if strings.TrimSpace(value) == "" {
			result.Warnings = append(
				result.Warnings,
				fmt.Sprintf(
					"1Password could not resolve %q (%s): empty value",
					name,
					ErrorEmptyValue,
				),
			)
			readErrors++
			continue
		}
		result.Secrets[name] = value
	}
	if useCache && readErrors == 0 && len(result.Secrets) > 0 {
		entry := CachedFetch{
			Secrets:   result.Secrets,
			FetchedAt: float64(time.Now().UnixNano()) / 1e9,
		}
		onePasswordMemoryCache.Put(memoryKey, entry)
		_ = onePasswordDiskCache.Write(
			diskKey,
			entry,
			ttl,
			request.Home,
		)
	}
	return result
}

func onePasswordCacheKeys(
	request FetchRequest,
	references map[string]string,
) (memory string, disk string) {
	tokenEnv := stringConfig(
		request.Config,
		"service_account_token_env",
		defaultOnePasswordTokenEnv,
	)
	parts := []string{
		"token=" + request.Env[tokenEnv],
		"account=" + request.Env["OP_ACCOUNT"],
	}
	var sessionNames []string
	for name := range request.Env {
		if strings.HasPrefix(name, "OP_SESSION_") {
			sessionNames = append(sessionNames, name)
		}
	}
	sort.Strings(sessionNames)
	for _, name := range sessionNames {
		parts = append(parts, name+"="+request.Env[name])
	}
	authFingerprint := fingerprint(strings.Join(parts, "\n"))

	referenceNames := sortedStringMapKeys(references)
	referenceParts := make([]string, 0, len(referenceNames))
	for _, name := range referenceNames {
		referenceParts = append(
			referenceParts,
			name+"="+references[name],
		)
	}
	referencesFingerprint := fingerprint(strings.Join(referenceParts, "\n"))
	account := stringConfig(request.Config, "account", "")
	disk = strings.Join(
		[]string{authFingerprint, account, referencesFingerprint},
		"|",
	)
	memory = strings.Join(
		[]string{
			authFingerprint,
			account,
			request.Home,
			referencesFingerprint,
		},
		"|",
	)
	return memory, disk
}

func sortedStringMapKeys(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func resetOnePasswordCache(home string) {
	onePasswordMemoryCache.Clear()
	_ = onePasswordDiskCache.Clear(home)
}

func sortedAnyMapKeys(values map[string]any) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
