// Package secrets resolves startup credentials from explicitly configured,
// read-only secret managers without mutating the process environment.
package secrets

import (
	"context"
	"regexp"
	"strings"
)

const (
	// APIVersion is the native secret-source contract version.
	APIVersion = 1

	DefaultFetchTimeoutSeconds = 120.0
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Shape describes whether a source returns explicitly mapped values or a
// project-wide bulk payload. Explicit mappings have stronger precedence.
type Shape string

const (
	ShapeMapped Shape = "mapped"
	ShapeBulk   Shape = "bulk"
)

// ErrorKind is a stable machine-readable failure taxonomy shared by all
// adapters.
type ErrorKind string

const (
	ErrorNotConfigured ErrorKind = "not_configured"
	ErrorBinaryMissing ErrorKind = "binary_missing"
	ErrorAuthFailed    ErrorKind = "auth_failed"
	ErrorAuthExpired   ErrorKind = "auth_expired"
	ErrorRefInvalid    ErrorKind = "ref_invalid"
	ErrorNetwork       ErrorKind = "network"
	ErrorEmptyValue    ErrorKind = "empty_value"
	ErrorTimeout       ErrorKind = "timeout"
	ErrorInternal      ErrorKind = "internal"
)

// Descriptor is immutable source metadata used for validation and ordering.
type Descriptor struct {
	Name       string
	Label      string
	Shape      Shape
	Scheme     string
	APIVersion int
}

// Environment is the startup-time snapshot visible to a source. It contains
// bootstrap credentials but is never mutated by Fetch.
type Environment map[string]string

func (e Environment) Lookup(name string) (string, bool) {
	value, ok := e[strings.TrimSpace(name)]
	return value, ok
}

// FetchRequest is the complete, bounded input to one source fetch.
type FetchRequest struct {
	Config map[string]any
	Home   string
	Env    Environment
}

// FetchResult contains values a source would contribute. The orchestrator,
// not the source, decides which values actually become effective.
type FetchResult struct {
	Secrets    map[string]string
	Warnings   []string
	Error      string
	ErrorKind  ErrorKind
	BinaryPath string
}

func (r FetchResult) OK() bool {
	return strings.TrimSpace(r.Error) == ""
}

// Source is the native read-only startup secret-source contract.
type Source interface {
	Descriptor() Descriptor
	Enabled(config map[string]any) bool
	OverrideExisting(config map[string]any) bool
	ProtectedEnvVars(config map[string]any) []string
	Fetch(context.Context, FetchRequest) FetchResult
}

func validEnvName(name string) bool {
	return envNamePattern.MatchString(name)
}

// ValidEnvironmentName reports whether a mapping key can safely become an
// environment variable.
func ValidEnvironmentName(name string) bool {
	return validEnvName(name)
}

func boolConfig(config map[string]any, key string, fallback bool) bool {
	value, exists := config[key]
	if !exists {
		return fallback
	}
	typed, ok := value.(bool)
	if !ok {
		return fallback
	}
	return typed
}

func stringConfig(config map[string]any, key, fallback string) string {
	value, ok := config[key].(string)
	if !ok {
		return fallback
	}
	return strings.TrimSpace(value)
}

func floatConfig(config map[string]any, key string, fallback float64) float64 {
	switch value := config[key].(type) {
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case float64:
		return value
	case float32:
		return float64(value)
	default:
		return fallback
	}
}

func mapConfig(config map[string]any, key string) map[string]any {
	value, _ := config[key].(map[string]any)
	return value
}
