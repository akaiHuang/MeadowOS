package sessionrecovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,159}$`)
var labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,159}$`)
var credentialPattern = regexp.MustCompile(`(?i)(authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|secret|password)\s*[:=]\s*[^\s,;]+`)
var bearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{8,}={0,2}`)
var providerKeyPattern = regexp.MustCompile(`\b(sk|rk|pk)-[A-Za-z0-9_-]{12,}\b`)

// Reference computes metadata without retaining content. Sensitive content is
// represented only by a fixed redaction marker, with no digest or byte count.
func Reference(content []byte, summary string, sensitive bool) ContentReference {
	if sensitive {
		return RedactedReference()
	}
	digest := sha256.Sum256(content)
	return ContentReference{
		SHA256:  hex.EncodeToString(digest[:]),
		Bytes:   int64(len(content)),
		Summary: summary,
		Content: string(content),
	}
}

// ReferenceAuto applies the mandatory credential detector before deciding
// whether a digest is safe to retain.
func ReferenceAuto(content []byte, summary string) ContentReference {
	if containsSensitive(string(content)) {
		return RedactedReference()
	}
	return Reference(content, summary, false)
}

func RedactedReference() ContentReference {
	return ContentReference{Summary: "[redacted]", Sensitive: true, Redacted: true}
}

func DigestAction(toolName string, canonicalArguments []byte) string {
	return digestActionBytes(toolName, canonicalActionArguments(canonicalArguments))
}

// DigestActionLegacy reproduces the pre-canonicalisation identity. It exists
// only so an upgraded process can continue to fail closed against an exact
// action already guarded in an older journal.
func DigestActionLegacy(toolName string, arguments []byte) string {
	return digestActionBytes(toolName, arguments)
}

func digestActionBytes(toolName string, arguments []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strings.TrimSpace(toolName)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(arguments)
	return hex.EncodeToString(hash.Sum(nil))
}

// canonicalActionArguments gives semantically equivalent JSON objects one
// stable identity. This intentionally mirrors the executor's json.Unmarshal
// into any followed by a deterministic json.Marshal: numeric spellings such
// as 1, 1.0 and 1e0 therefore identify the same action the tool will receive.
//
// Tool integrations may also pass opaque, non-JSON argument bytes. Those are
// deliberately hashed byte-for-byte: malformed input must never be "repaired"
// into the identity of a different action.
func canonicalActionArguments(arguments []byte) []byte {
	if strings.TrimSpace(string(arguments)) == "" {
		return []byte("{}")
	}
	var value any
	if err := json.Unmarshal(arguments, &value); err == nil {
		if encoded, marshalErr := json.Marshal(value); marshalErr == nil {
			return encoded
		}
	}
	return append([]byte(nil), arguments...)
}

func sanitizeReference(reference *ContentReference, options Options) (*ContentReference, error) {
	if reference == nil {
		return nil, nil
	}
	if reference.Sensitive || reference.Redacted {
		redacted := RedactedReference()
		return &redacted, nil
	}
	if !validHash(reference.SHA256) || reference.Bytes < 0 {
		return nil, errors.New("content reference requires a valid SHA-256 digest and byte count")
	}
	clean := *reference
	clean.Summary = sanitizeText(clean.Summary, options)
	if clean.Content != "" {
		if int64(len(clean.Content)) != clean.Bytes {
			return nil, errors.New("content reference byte count does not match hydrated evidence")
		}
		digest := sha256.Sum256([]byte(clean.Content))
		if hex.EncodeToString(digest[:]) != clean.SHA256 || containsSensitive(clean.Content) {
			return nil, errors.New("content reference evidence is sensitive or does not match its digest")
		}
	}
	if clean.StorageRef != "" && clean.StorageRef != evidenceStorageRef(clean.SHA256) {
		return nil, errors.New("content reference storage locator is invalid")
	}
	return &clean, nil
}

func sanitizeText(value string, options Options) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.Contains(strings.ToUpper(value), "PRIVATE KEY") {
		return "[redacted]"
	}
	value = credentialPattern.ReplaceAllString(value, "$1=[redacted]")
	value = bearerPattern.ReplaceAllString(value, "Bearer [redacted]")
	value = providerKeyPattern.ReplaceAllString(value, "[redacted-key]")
	if options.Redactor != nil {
		value = options.Redactor(value)
	}
	value = strings.TrimSpace(value)
	if len(value) > options.Limits.MaxSummaryBytes {
		value = value[:options.Limits.MaxSummaryBytes]
		for !utf8.ValidString(value) && len(value) > 0 {
			value = value[:len(value)-1]
		}
	}
	return value
}

func containsSensitive(value string) bool {
	upper := strings.ToUpper(value)
	return strings.Contains(upper, "PRIVATE KEY") || credentialPattern.MatchString(value) ||
		bearerPattern.MatchString(value) || providerKeyPattern.MatchString(value)
}

func sanitizeAttachment(metadata AttachmentMetadata, options Options) (AttachmentMetadata, error) {
	if !validIdentifier(metadata.AttachmentID) {
		return AttachmentMetadata{}, errors.New("attachment id is invalid")
	}
	switch metadata.Class {
	case AttachmentUserUpload, AttachmentGenerated, AttachmentWorkspaceArtifact:
	default:
		return AttachmentMetadata{}, errors.New("attachment class is invalid or prohibited")
	}
	metadata.OriginalName = sanitizeText(filepath.Base(metadata.OriginalName), options)
	metadata.MediaType = sanitizeText(metadata.MediaType, options)
	if !metadata.Authorized {
		metadata.StorageRef = ""
		metadata.SHA256 = ""
		metadata.Bytes = 0
		metadata.Available = false
		return metadata, nil
	}
	if metadata.Available {
		if metadata.StorageRef == "" || filepath.IsAbs(metadata.StorageRef) {
			return AttachmentMetadata{}, errors.New("authorized available attachment requires a relative storage reference")
		}
		clean := filepath.Clean(metadata.StorageRef)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean != metadata.StorageRef {
			return AttachmentMetadata{}, errors.New("attachment storage reference escapes or is not clean")
		}
		if !validHash(metadata.SHA256) || metadata.Bytes < 0 {
			return AttachmentMetadata{}, errors.New("available attachment metadata requires digest and byte count")
		}
	}
	return metadata, nil
}

func validIdentifier(value string) bool {
	return identifierPattern.MatchString(value)
}

func validLabel(value string) bool {
	return labelPattern.MatchString(value)
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func requireIdentifier(field, value string) error {
	if !validIdentifier(value) {
		return fmt.Errorf("%s is invalid", field)
	}
	return nil
}

func sameReference(left, right *ContentReference) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.SHA256 == right.SHA256 && left.Bytes == right.Bytes &&
		left.Summary == right.Summary && left.StorageRef == right.StorageRef &&
		left.Sensitive == right.Sensitive && left.Redacted == right.Redacted
}

func metadataOnlyReference(reference *ContentReference) *ContentReference {
	if reference == nil {
		return nil
	}
	clean := *reference
	clean.Content = ""
	clean.StorageRef = ""
	return &clean
}

func evidenceStorageRef(digest string) string {
	return EvidenceDirectoryName + "/" + digest + ".blob"
}
