package secrets

import (
	"context"
	"fmt"
	"runtime"
	"strings"
)

var keychainEnvironmentAllowlist = []string{
	"PATH",
	"HOME",
	"USER",
	"TMPDIR",
	"TMP",
	"TEMP",
}

type keychainSource struct{}

func NewKeychainSource() Source {
	return keychainSource{}
}

func (keychainSource) Descriptor() Descriptor {
	return Descriptor{
		Name:       "keychain",
		Label:      "macOS Keychain",
		Shape:      ShapeMapped,
		Scheme:     "keychain",
		APIVersion: APIVersion,
	}
}

func (keychainSource) Enabled(config map[string]any) bool {
	return boolConfig(config, "enabled", false)
}

func (keychainSource) OverrideExisting(config map[string]any) bool {
	return boolConfig(config, "override_existing", true)
}

func (keychainSource) ProtectedEnvVars(map[string]any) []string {
	return nil
}

func (keychainSource) Fetch(
	ctx context.Context,
	request FetchRequest,
) FetchResult {
	result := FetchResult{Secrets: map[string]string{}}
	entries := mapConfig(request.Config, "env")
	if len(entries) == 0 {
		result.Error = "secrets.keychain.env is empty"
		result.ErrorKind = ErrorNotConfigured
		return result
	}

	pinned := stringConfig(request.Config, "binary_path", "")
	if runtime.GOOS != "darwin" && pinned == "" {
		result.Error = "macOS Keychain is unavailable on this platform"
		result.ErrorKind = ErrorBinaryMissing
		return result
	}
	binary, err := findExecutable(pinned, "security", request.Env)
	if err != nil {
		result.Error = "macOS security CLI was not found or is not executable"
		result.ErrorKind = ErrorBinaryMissing
		return result
	}
	result.BinaryPath = binary
	childEnvironment := minimalEnvironment(
		request.Env,
		keychainEnvironmentAllowlist,
	)

	for _, name := range sortedAnyMapKeys(entries) {
		if !validEnvName(name) {
			result.Warnings = append(
				result.Warnings,
				fmt.Sprintf(
					"Keychain skipped %q: invalid environment name",
					name,
				),
			)
			continue
		}
		service, account, ok := keychainBinding(entries[name])
		if !ok {
			result.Warnings = append(
				result.Warnings,
				fmt.Sprintf(
					"Keychain skipped %q: service is required",
					name,
				),
			)
			continue
		}
		args := []string{"find-generic-password", "-s", service}
		if account != "" {
			args = append(args, "-a", account)
		}
		args = append(args, "-w")
		command := runCommand(ctx, binary, args, childEnvironment)
		if command.err != nil {
			message, kind := commandError("security", command)
			message = redactSensitiveEnvironment(message, childEnvironment)
			result.Warnings = append(
				result.Warnings,
				fmt.Sprintf(
					"Keychain could not resolve %q (%s): %s",
					name,
					kind,
					message,
				),
			)
			continue
		}
		value := strings.TrimRight(command.stdout, "\r\n")
		if strings.TrimSpace(value) == "" {
			result.Warnings = append(
				result.Warnings,
				fmt.Sprintf(
					"Keychain could not resolve %q (%s): empty value",
					name,
					ErrorEmptyValue,
				),
			)
			continue
		}
		result.Secrets[name] = value
	}
	return result
}

func keychainBinding(value any) (service string, account string, ok bool) {
	switch typed := value.(type) {
	case string:
		service = strings.TrimSpace(typed)
	case map[string]any:
		service = stringConfig(typed, "service", "")
		account = stringConfig(typed, "account", "")
	}
	return service, account, service != ""
}
