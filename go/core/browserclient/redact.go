package browserclient

import (
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var sensitivePatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`), `${1}` + redacted},
	{regexp.MustCompile(`(?i)((?:peer[_-]?secret|api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|authorization|cookie|set-cookie|nonce)\s*[:=]\s*["']?)[^"',;\s]+`), `${1}` + redacted},
	{regexp.MustCompile(`(?i)([?&](?:key|token|secret|password|signature|sig)=)[^&#\s]+`), `${1}` + redacted},
	{regexp.MustCompile(`(?i)(https?://)[^/@\s:]+:[^/@\s]+@`), `${1}` + redacted + `@`},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`), redacted},
	{regexp.MustCompile(`/Users/[^/\s]+`), `/Users/` + redacted},
}

// Redact masks credentials, secret-bearing fields, URL credentials, sensitive
// query values, JWT-shaped values, and macOS home-directory identities.
func Redact(value string) string {
	redactedValue := value
	for _, rule := range sensitivePatterns {
		redactedValue = rule.pattern.ReplaceAllString(redactedValue, rule.replacement)
	}
	return strings.TrimSpace(redactedValue)
}
