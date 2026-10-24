package checkpoint

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const sensitiveContentMarker = "[REDACTED SENSITIVE CONTENT]"

type Redactor interface {
	Redact(string) (redacted string, changed bool)
}

type RedactorFunc func(string) (string, bool)

func (f RedactorFunc) Redact(value string) (string, bool) {
	return f(value)
}

type credentialRedactor struct{}

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bbearer[ \t]+[A-Za-z0-9._~+/=-]{6,}`),
	regexp.MustCompile(`(?i)\b(?:sk|rk|pk)-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|authorization)[ \t]*[\"']?[ \t]*[:=][ \t]*[\"']?[A-Za-z0-9._~+/=-]{6,}[\"']?`),
	regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
}

func (credentialRedactor) Redact(value string) (string, bool) {
	redacted := value
	for _, pattern := range credentialPatterns {
		redacted = pattern.ReplaceAllString(redacted, "[REDACTED CREDENTIAL]")
	}
	return redacted, redacted != value
}

// DefaultRedactor returns the mandatory bearer/key/token redactor used for all
// persisted strings.
func DefaultRedactor() Redactor {
	return credentialRedactor{}
}

// ReferenceContent hashes content in memory and returns only a persistable
// reference. It never copies content into Summary. A sensitive reference uses
// a fixed marker even if a summary was supplied.
func ReferenceContent(content, summary string, sensitive bool, additional Redactor) ContentReference {
	digest := sha256.Sum256([]byte(content))
	reference := ContentReference{
		SHA256: fmt.Sprintf("%x", digest[:]),
		Bytes: int64(len(content)),
		Summary: summary,
		Sensitive: sensitive,
	}
	if sensitive {
		reference.Summary = sensitiveContentMarker
		reference.Redacted = true
		return reference
	}
	reference.Summary, reference.Redacted = applyRedactors(reference.Summary, additional)
	return reference
}

func normalizeOptions(options Options) Options {
	defaults := DefaultOptions()
	if options.Limits.MaxTailBytes <= 0 {
		options.Limits.MaxTailBytes = defaults.Limits.MaxTailBytes
	}
	if options.Limits.MaxRecords <= 0 {
		options.Limits.MaxRecords = defaults.Limits.MaxRecords
	}
	if options.Limits.MaxRecordBytes <= 0 {
		options.Limits.MaxRecordBytes = defaults.Limits.MaxRecordBytes
	}
	if options.Limits.MaxSnapshotBytes <= 0 {
		options.Limits.MaxSnapshotBytes = defaults.Limits.MaxSnapshotBytes
	}
	if options.Limits.MaxSummaryBytes <= 0 {
		options.Limits.MaxSummaryBytes = defaults.Limits.MaxSummaryBytes
	}
	if options.Clock == nil {
		options.Clock = defaults.Clock
	}
	return options
}

func applyRedactors(value string, additional Redactor) (string, bool) {
	redacted, changed := DefaultRedactor().Redact(value)
	if additional != nil {
		var extraChanged bool
		redacted, extraChanged = additional.Redact(redacted)
		changed = changed || extraChanged
	}
	return redacted, changed
}

func redactAndBound(value string, options Options) (string, bool) {
	value, changed := applyRedactors(value, options.Redactor)
	bounded, truncated := truncateUTF8(value, options.Limits.MaxSummaryBytes)
	return bounded, changed || truncated
}

func truncateUTF8(value string, limit int) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	const suffix = " [TRUNCATED]"
	keep := limit - len(suffix)
	if keep <= 0 {
		return suffix[:limit], true
	}
	for keep > 0 && !utf8.ValidString(value[:keep]) {
		keep--
	}
	return value[:keep] + suffix, true
}

func sanitizeIdentifier(name, value string, additional Redactor) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if len(value) > 256 {
		return "", fmt.Errorf("%s exceeds 256 bytes", name)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%s contains a control character", name)
		}
	}
	if _, changed := applyRedactors(value, additional); changed {
		return "", fmt.Errorf("%w: %s", ErrSensitiveIdentifier, name)
	}
	return value, nil
}

func sanitizeLabel(value string, options Options) (string, error) {
	value, changed := applyRedactors(strings.TrimSpace(value), options.Redactor)
	if len(value) > options.Limits.MaxSummaryBytes {
		return "", fmt.Errorf("checkpoint label exceeds %d bytes", options.Limits.MaxSummaryBytes)
	}
	if changed {
		return value, nil
	}
	return value, nil
}

func sanitizeReference(reference *ContentReference, options Options) {
	if reference == nil {
		return
	}
	if reference.Sensitive {
		reference.Summary = sensitiveContentMarker
		reference.Redacted = true
		return
	}
	reference.Summary, reference.Redacted = redactAndBound(reference.Summary, options)
}

func sanitizeEvent(event Event, options Options) (Event, error) {
	var err error
	if event.ID, err = sanitizeIdentifier("event id", event.ID, options.Redactor); err != nil {
		return Event{}, err
	}
	if event.At.IsZero() {
		event.At = options.Clock().UTC()
	} else {
		event.At = event.At.UTC()
	}
	if event.Turn != nil {
		if event.Turn.TurnID, err = sanitizeIdentifier("turn id", event.Turn.TurnID, options.Redactor); err != nil {
			return Event{}, err
		}
		sanitizeReference(event.Turn.Prompt, options)
		event.Turn.Summary, _ = redactAndBound(event.Turn.Summary, options)
	}
	if event.ToolCall != nil {
		if event.ToolCall.CallID, err = sanitizeIdentifier("tool call id", event.ToolCall.CallID, options.Redactor); err != nil {
			return Event{}, err
		}
		if event.ToolCall.TurnID != "" {
			if event.ToolCall.TurnID, err = sanitizeIdentifier("tool call turn id", event.ToolCall.TurnID, options.Redactor); err != nil {
				return Event{}, err
			}
		}
		if event.ToolCall.Name, err = sanitizeLabel(event.ToolCall.Name, options); err != nil {
			return Event{}, err
		}
		sanitizeReference(event.ToolCall.Arguments, options)
	}
	if event.ToolResult != nil {
		if event.ToolResult.CallID, err = sanitizeIdentifier("tool result call id", event.ToolResult.CallID, options.Redactor); err != nil {
			return Event{}, err
		}
		sanitizeReference(event.ToolResult.Output, options)
		event.ToolResult.Summary, _ = redactAndBound(event.ToolResult.Summary, options)
	}
	if event.Cancellation != nil {
		event.Cancellation.Source, _ = redactAndBound(event.Cancellation.Source, options)
		event.Cancellation.Reason, _ = redactAndBound(event.Cancellation.Reason, options)
	}
	if event.Generation != nil {
		if event.Generation.GenerationID, err = sanitizeIdentifier("generation id", event.Generation.GenerationID, options.Redactor); err != nil {
			return Event{}, err
		}
		if event.Generation.TurnID != "" {
			if event.Generation.TurnID, err = sanitizeIdentifier("generation turn id", event.Generation.TurnID, options.Redactor); err != nil {
				return Event{}, err
			}
		}
		if event.Generation.Model, err = sanitizeLabel(event.Generation.Model, options); err != nil {
			return Event{}, err
		}
		sanitizeReference(event.Generation.Prompt, options)
		sanitizeReference(event.Generation.Output, options)
	}
	if event.Permission != nil {
		if event.Permission.RequestID, err = sanitizeIdentifier("permission request id", event.Permission.RequestID, options.Redactor); err != nil {
			return Event{}, err
		}
		if event.Permission.TurnID != "" {
			if event.Permission.TurnID, err = sanitizeIdentifier("permission turn id", event.Permission.TurnID, options.Redactor); err != nil {
				return Event{}, err
			}
		}
		if event.Permission.Scope, err = sanitizeLabel(event.Permission.Scope, options); err != nil {
			return Event{}, err
		}
		event.Permission.Reason, _ = redactAndBound(event.Permission.Reason, options)
	}
	if event.Terminal != nil {
		event.Terminal.Summary, _ = redactAndBound(event.Terminal.Summary, options)
		event.Terminal.At = event.At
	}
	if event.TaskProgress != nil {
		if err := sanitizeTaskProgressUpdate(event.TaskProgress, options); err != nil {
			return Event{}, err
		}
	}
	if err := validateEvent(event); err != nil {
		return Event{}, err
	}
	return event, nil
}
