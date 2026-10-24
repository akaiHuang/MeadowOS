package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var ansiCSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

type commandResult struct {
	stdout string
	stderr string
	err    error
}

func runCommand(
	ctx context.Context,
	binary string,
	args []string,
	environment Environment,
) commandResult {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = environmentList(environment)
	command.Stdin = nil
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return commandResult{
		stdout: stdout.String(),
		stderr: stderr.String(),
		err:    err,
	}
}

func environmentList(environment Environment) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		if validEnvName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}

func scrubDiagnostic(value string) string {
	value = ansiCSI.ReplaceAllString(value, "")
	value = strings.ReplaceAll(value, "\x1b", "")
	value = strings.TrimSpace(value)
	characters := []rune(value)
	if len(characters) > 200 {
		value = string(characters[:200])
	}
	return value
}

func redactSensitiveEnvironment(
	value string,
	environment Environment,
) string {
	for name, secret := range environment {
		upper := strings.ToUpper(name)
		if !containsAny(
			upper,
			"TOKEN",
			"SECRET",
			"PASSWORD",
			"SESSION",
			"CREDENTIAL",
		) {
			continue
		}
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func findExecutable(
	pinned string,
	name string,
	environment Environment,
) (string, error) {
	pinned = strings.TrimSpace(pinned)
	if pinned != "" {
		if executableFile(pinned) {
			return pinned, nil
		}
		return "", fmt.Errorf("%s is not an executable file", pinned)
	}
	pathValue := environment["PATH"]
	if pathValue == "" {
		pathValue = os.Getenv("PATH")
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, name)
		if executableFile(candidate) {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

func commandError(
	helper string,
	result commandResult,
) (string, ErrorKind) {
	if errors.Is(result.err, context.DeadlineExceeded) ||
		errors.Is(result.err, context.Canceled) {
		return helper + " command timed out", ErrorTimeout
	}
	if errors.Is(result.err, exec.ErrNotFound) {
		return helper + " command is missing", ErrorBinaryMissing
	}
	message := scrubDiagnostic(result.stderr)
	if message == "" {
		message = scrubDiagnostic(result.stdout)
	}
	if message == "" {
		message = result.err.Error()
	}
	lowered := strings.ToLower(message)
	kind := ErrorInternal
	switch {
	case strings.Contains(lowered, "expired"):
		kind = ErrorAuthExpired
	case containsAny(
		lowered,
		"unauthorized",
		"not signed in",
		"authentication",
		"invalid token",
		"access token",
		"401",
		"403",
	):
		kind = ErrorAuthFailed
	case containsAny(
		lowered,
		"network",
		"connection",
		"resolve host",
		"dns",
		"transport",
	):
		kind = ErrorNetwork
	}
	return fmt.Sprintf("%s command failed: %s", helper, message), kind
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func minimalEnvironment(
	base Environment,
	allowlist []string,
) Environment {
	result := Environment{"NO_COLOR": "1"}
	for _, name := range allowlist {
		if value, exists := base[name]; exists {
			result[name] = value
		}
	}
	return result
}
