// Package productupdate implements the fail-closed product-side consumer for
// Meadow's authenticated release-update contract.
package productupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	KeyringSchema      = "meadow.update-keyring.v1"
	FeedSchema         = "meadow.update-feed.v1"
	StateSchema        = "meadow.update-state.v1"
	ArchiveStatementV1 = "meadow.archive-statement.v1"
	SignatureAlgorithm = "Ed25519"
	maxSafeInteger     = int64(9007199254740991)
)

const (
	maxFeedLifetime    = 7 * 24 * time.Hour
	maxKeyringLifetime = 400 * 24 * time.Hour
	maxFutureSkew      = 5 * time.Minute
	timestampLayout    = "2006-01-02T15:04:05.000Z"
)

var (
	idPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	digestPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	semverPattern    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9A-Za-z-][0-9A-Za-z-]*))*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	osVersionPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){1,3}$`)
)

// ContractError carries a stable release-update error classification.
type ContractError struct {
	Code    string
	Message string
	Cause   error
}

func (e *ContractError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *ContractError) Unwrap() error { return e.Cause }

func contractError(code, message string, cause error) error {
	return &ContractError{Code: code, Message: message, Cause: cause}
}

func requireContract(ok bool, code, message string) error {
	if !ok {
		return contractError(code, message, nil)
	}
	return nil
}

// ErrorCode returns the stable contract error code, if present.
func ErrorCode(err error) string {
	var target *ContractError
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

type Signature struct {
	KeyID           string `json:"keyId"`
	Algorithm       string `json:"algorithm"`
	SignatureBase64 string `json:"signatureBase64"`
}

type Key struct {
	KeyID               string   `json:"keyId"`
	Algorithm           string   `json:"algorithm"`
	PublicKeySPKIBase64 string   `json:"publicKeySpkiBase64"`
	PublicKeySPKISHA256 string   `json:"publicKeySpkiSha256"`
	Roles               []string `json:"roles"`
	Status              string   `json:"status"`
	NotBefore           string   `json:"notBefore"`
	NotAfter            string   `json:"notAfter"`
	RevokedAt           *string  `json:"revokedAt"`
}

type Keyring struct {
	Schema     string      `json:"schema"`
	Sequence   int64       `json:"sequence"`
	IssuedAt   string      `json:"issuedAt"`
	ExpiresAt  string      `json:"expiresAt"`
	Threshold  int64       `json:"threshold"`
	Keys       []Key       `json:"keys"`
	Signatures []Signature `json:"signatures"`
}

type Artifact struct {
	ArtifactID   string     `json:"artifactId"`
	FileName     string     `json:"fileName"`
	Platform     string     `json:"platform"`
	Architecture string     `json:"architecture"`
	MinOS        string     `json:"minOS"`
	Version      string     `json:"version"`
	BuildNumber  int64      `json:"buildNumber"`
	Size         int64      `json:"size"`
	SHA256       string     `json:"sha256"`
	Signature    *Signature `json:"signature"`
}

type Release struct {
	ReleaseID   string     `json:"releaseId"`
	Version     string     `json:"version"`
	BuildNumber int64      `json:"buildNumber"`
	PublishedAt string     `json:"publishedAt"`
	Artifacts   []Artifact `json:"artifacts"`
}

type Feed struct {
	Schema          string      `json:"schema"`
	Sequence        int64       `json:"sequence"`
	FeedID          string      `json:"feedId"`
	IssuedAt        string      `json:"issuedAt"`
	ExpiresAt       string      `json:"expiresAt"`
	KeyringSequence int64       `json:"keyringSequence"`
	Product         string      `json:"product"`
	Channel         string      `json:"channel"`
	Release         Release     `json:"release"`
	Signatures      []Signature `json:"signatures"`
}

type KeyringState struct {
	Sequence      int64    `json:"sequence"`
	Digest        string   `json:"digest"`
	RevokedKeyIDs []string `json:"revokedKeyIds"`
}

type FeedState struct {
	Sequence    int64  `json:"sequence"`
	Digest      string `json:"digest"`
	ReleaseID   string `json:"releaseId"`
	Version     string `json:"version"`
	BuildNumber int64  `json:"buildNumber"`
	AcceptedAt  string `json:"acceptedAt"`
}

// ContractState is byte-for-byte schema-compatible with meadow.update-state.v1.
type ContractState struct {
	Schema  string               `json:"schema"`
	Keyring KeyringState         `json:"keyring"`
	Feeds   map[string]FeedState `json:"feeds"`
}

// ParseKeyring performs strict JSON parsing and complete keyring validation.
func ParseKeyring(raw []byte) (Keyring, error) {
	var keyring Keyring
	if err := decodeStrict(raw, "keyring", &keyring); err != nil {
		return Keyring{}, err
	}
	if err := validateKeyring(keyring); err != nil {
		return Keyring{}, err
	}
	return keyring, nil
}

// ParseFeed performs strict JSON parsing and complete feed validation.
func ParseFeed(raw []byte) (Feed, error) {
	var feed Feed
	if err := decodeStrict(raw, "feed", &feed); err != nil {
		return Feed{}, err
	}
	if err := validateFeed(feed); err != nil {
		return Feed{}, err
	}
	return feed, nil
}

func decodeStrict(raw []byte, label string, destination any) error {
	value, err := parseJSONStrict(raw, label)
	if err != nil {
		return err
	}
	canonical, err := canonicalBytes(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return contractError("JSON_SCHEMA", label+" has invalid fields", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return contractError("JSON_TRAILING_DATA", label+" has trailing data", err)
	}
	return nil
}

func parseJSONStrict(raw []byte, label string) (any, error) {
	if !utf8.Valid(raw) {
		return nil, contractError("JSON_UNICODE", label+" is not valid UTF-8", nil)
	}
	if err := validateUnicodeEscapes(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := parseJSONValue(decoder)
	if err != nil {
		if ErrorCode(err) != "" {
			return nil, err
		}
		return nil, contractError("JSON_PARSE", label+" is invalid JSON", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, contractError("JSON_TRAILING_DATA", label+" has trailing data", err)
	}
	return value, nil
}

func parseJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				if _, duplicate := object[key]; duplicate {
					return nil, contractError("JSON_DUPLICATE_KEY", "duplicate object key "+key, nil)
				}
				entry, err := parseJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				object[key] = entry
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for decoder.More() {
				entry, err := parseJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				array = append(array, entry)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return array, nil
		default:
			return nil, errors.New("unexpected JSON delimiter")
		}
	case json.Number:
		text := value.String()
		if text == "-0" || strings.ContainsAny(text, ".eE") {
			return nil, contractError("CANONICAL_NUMBER", "numbers must be safe non-negative-zero integers", nil)
		}
		integer, err := strconv.ParseInt(text, 10, 64)
		if err != nil || integer > maxSafeInteger || integer < -maxSafeInteger {
			return nil, contractError("CANONICAL_NUMBER", "number is outside the safe integer range", err)
		}
		return integer, nil
	case string, bool, nil:
		return value, nil
	default:
		return nil, contractError("CANONICAL_TYPE", "unsupported JSON value", nil)
	}
}

func ensureEOF(decoder *json.Decoder) error {
	_, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected trailing JSON token")
	}
	return err
}

func validateUnicodeEscapes(raw []byte) error {
	inside := false
	for index := 0; index < len(raw); index++ {
		switch raw[index] {
		case '"':
			inside = !inside
		case '\\':
			if !inside || index+1 >= len(raw) {
				continue
			}
			if raw[index+1] != 'u' {
				index++
				continue
			}
			if index+6 > len(raw) {
				return contractError("JSON_UNICODE", "truncated Unicode escape", nil)
			}
			first, err := strconv.ParseUint(string(raw[index+2:index+6]), 16, 16)
			if err != nil {
				return contractError("JSON_UNICODE", "invalid Unicode escape", err)
			}
			if first >= 0xD800 && first <= 0xDBFF {
				if index+12 > len(raw) || raw[index+6] != '\\' || raw[index+7] != 'u' {
					return contractError("JSON_UNICODE", "unpaired high surrogate", nil)
				}
				second, err := strconv.ParseUint(string(raw[index+8:index+12]), 16, 16)
				if err != nil || second < 0xDC00 || second > 0xDFFF {
					return contractError("JSON_UNICODE", "unpaired high surrogate", err)
				}
				index += 11
				continue
			}
			if first >= 0xDC00 && first <= 0xDFFF {
				return contractError("JSON_UNICODE", "unpaired low surrogate", nil)
			}
			index += 5
		}
	}
	return nil
}

func canonicalBytes(value any) ([]byte, error) {
	var output bytes.Buffer
	if err := appendCanonical(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func appendCanonical(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		if typed {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case string:
		appendJSONString(output, typed)
	case int64:
		if typed > maxSafeInteger || typed < -maxSafeInteger {
			return contractError("CANONICAL_NUMBER", "number is outside the safe integer range", nil)
		}
		output.WriteString(strconv.FormatInt(typed, 10))
	case []any:
		output.WriteByte('[')
		for index, entry := range typed {
			if index != 0 {
				output.WriteByte(',')
			}
			if err := appendCanonical(output, entry); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index != 0 {
				output.WriteByte(',')
			}
			appendJSONString(output, key)
			output.WriteByte(':')
			if err := appendCanonical(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return contractError("CANONICAL_TYPE", fmt.Sprintf("unsupported canonical type %T", value), nil)
	}
	return nil
}

func appendJSONString(output *bytes.Buffer, value string) {
	const hexadecimal = "0123456789abcdef"
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			output.WriteString(`\"`)
		case '\\':
			output.WriteString(`\\`)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			if character < 0x20 {
				output.WriteString(`\u00`)
				output.WriteByte(hexadecimal[byte(character)>>4])
				output.WriteByte(hexadecimal[byte(character)&0x0f])
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
}

func typedCanonical(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, contractError("CANONICAL_TYPE", "document cannot be represented as JSON", err)
	}
	parsed, err := parseJSONStrict(raw, "document")
	if err != nil {
		return nil, err
	}
	return canonicalBytes(parsed)
}

func unsignedDocumentBytes(document any) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, contractError("CANONICAL_TYPE", "signed document cannot be represented as JSON", err)
	}
	parsed, err := parseJSONStrict(raw, "signed document")
	if err != nil {
		return nil, err
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, contractError("CANONICAL_TYPE", "signed document must be an object", nil)
	}
	delete(object, "signatures")
	return canonicalBytes(object)
}

// DocumentDigest returns the lowercase SHA-256 digest of canonical JSON.
func DocumentDigest(document any) (string, error) {
	canonical, err := typedCanonical(document)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func archiveStatement(feed Feed, artifact Artifact) map[string]any {
	return map[string]any{
		"schema":       ArchiveStatementV1,
		"feedId":       feed.FeedID,
		"feedSequence": feed.Sequence,
		"product":      feed.Product,
		"channel":      feed.Channel,
		"releaseId":    feed.Release.ReleaseID,
		"version":      feed.Release.Version,
		"buildNumber":  feed.Release.BuildNumber,
		"artifactId":   artifact.ArtifactID,
		"fileName":     artifact.FileName,
		"platform":     artifact.Platform,
		"architecture": artifact.Architecture,
		"minOS":        artifact.MinOS,
		"size":         artifact.Size,
		"sha256":       artifact.SHA256,
	}
}

func parseTimestamp(value, label string) (time.Time, error) {
	parsed, err := time.Parse(timestampLayout, value)
	if err != nil || parsed.UTC().Format(timestampLayout) != value {
		return time.Time{}, contractError("TIMESTAMP", label+" is not a canonical UTC millisecond timestamp", err)
	}
	return parsed, nil
}

func canonicalNow(now time.Time) string { return now.UTC().Format(timestampLayout) }

func validateKeyring(keyring Keyring) error {
	if err := requireContract(keyring.Schema == KeyringSchema, "KEYRING_SCHEMA", "unsupported keyring schema"); err != nil {
		return err
	}
	if err := requirePositiveSafe(keyring.Sequence, "KEYRING_SEQUENCE", "keyring sequence"); err != nil {
		return err
	}
	issued, err := parseTimestamp(keyring.IssuedAt, "keyring.issuedAt")
	if err != nil {
		return err
	}
	expires, err := parseTimestamp(keyring.ExpiresAt, "keyring.expiresAt")
	if err != nil {
		return err
	}
	if err := requireContract(expires.After(issued) && expires.Sub(issued) <= maxKeyringLifetime, "KEYRING_LIFETIME", "keyring lifetime is invalid"); err != nil {
		return err
	}
	if err := requirePositiveSafe(keyring.Threshold, "KEYRING_THRESHOLD", "keyring threshold"); err != nil {
		return err
	}
	if err := requireContract(len(keyring.Keys) > 0, "KEYRING_KEYS", "keyring must contain keys"); err != nil {
		return err
	}
	keyIDs := make([]string, len(keyring.Keys))
	activeRoots := int64(0)
	for index, key := range keyring.Keys {
		keyIDs[index] = key.KeyID
		if err := validateKey(key); err != nil {
			return err
		}
		if key.Status == "active" && contains(key.Roles, "root") {
			activeRoots++
		}
	}
	if err := requireContract(sortedUnique(keyIDs), "KEY_ORDER", "keyring keys must be sorted and unique by keyId"); err != nil {
		return err
	}
	if err := requireContract(keyring.Threshold <= activeRoots, "KEYRING_THRESHOLD", "keyring threshold exceeds active root keys"); err != nil {
		return err
	}
	return validateSignatures(keyring.Signatures, true)
}

func validateKey(key Key) error {
	if err := validateID(key.KeyID, "KEY_ID", "keyId"); err != nil {
		return err
	}
	if err := requireContract(key.Algorithm == SignatureAlgorithm, "KEY_ALGORITHM", "unsupported key algorithm"); err != nil {
		return err
	}
	if err := requireContract(sortedUnique(key.Roles) && len(key.Roles) > 0, "KEY_ROLES", "key roles must be sorted and unique"); err != nil {
		return err
	}
	for _, role := range key.Roles {
		if role != "root" && role != "feed" && role != "archive" {
			return contractError("KEY_ROLE", "unsupported key role "+role, nil)
		}
	}
	if key.Status != "active" && key.Status != "revoked" {
		return contractError("KEY_STATUS", "unsupported key status", nil)
	}
	notBefore, err := parseTimestamp(key.NotBefore, "key.notBefore")
	if err != nil {
		return err
	}
	notAfter, err := parseTimestamp(key.NotAfter, "key.notAfter")
	if err != nil {
		return err
	}
	if err := requireContract(notAfter.After(notBefore), "KEY_LIFETIME", "key validity range is invalid"); err != nil {
		return err
	}
	if key.Status == "active" {
		if err := requireContract(key.RevokedAt == nil, "KEY_REVOCATION", "active key must have null revokedAt"); err != nil {
			return err
		}
	} else {
		if err := requireContract(key.RevokedAt != nil, "KEY_REVOCATION", "revoked key must have revokedAt"); err != nil {
			return err
		}
		revokedAt, err := parseTimestamp(*key.RevokedAt, "key.revokedAt")
		if err != nil {
			return err
		}
		if err := requireContract(!revokedAt.Before(notBefore), "KEY_REVOCATION", "revokedAt precedes notBefore"); err != nil {
			return err
		}
	}
	der, err := decodeCanonicalBase64(key.PublicKeySPKIBase64, "KEY_PUBLIC_KEY")
	if err != nil {
		return err
	}
	digest := sha256.Sum256(der)
	if err := requireContract(hex.EncodeToString(digest[:]) == key.PublicKeySPKISHA256 && digestPattern.MatchString(key.PublicKeySPKISHA256), "KEY_PUBLIC_KEY_DIGEST", "public key SPKI digest mismatch"); err != nil {
		return err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return contractError("KEY_PUBLIC_KEY", "public key SPKI is invalid", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	return requireContract(ok && len(publicKey) == ed25519.PublicKeySize, "KEY_PUBLIC_KEY", "public key is not Ed25519")
}

func validateFeed(feed Feed) error {
	if err := requireContract(feed.Schema == FeedSchema, "FEED_SCHEMA", "unsupported feed schema"); err != nil {
		return err
	}
	if err := requirePositiveSafe(feed.Sequence, "FEED_SEQUENCE", "feed sequence"); err != nil {
		return err
	}
	if err := validateID(feed.FeedID, "FEED_ID", "feedId"); err != nil {
		return err
	}
	issued, err := parseTimestamp(feed.IssuedAt, "feed.issuedAt")
	if err != nil {
		return err
	}
	expires, err := parseTimestamp(feed.ExpiresAt, "feed.expiresAt")
	if err != nil {
		return err
	}
	if err := requireContract(expires.After(issued) && expires.Sub(issued) <= maxFeedLifetime, "FEED_LIFETIME", "feed lifetime is invalid"); err != nil {
		return err
	}
	if err := requirePositiveSafe(feed.KeyringSequence, "FEED_KEYRING_SEQUENCE", "feed keyring sequence"); err != nil {
		return err
	}
	if err := requireContract(feed.Product != "" && feed.Channel != "", "FEED_IDENTITY", "feed product and channel are required"); err != nil {
		return err
	}
	if err := validateRelease(feed.Release); err != nil {
		return err
	}
	return validateSignatures(feed.Signatures, true)
}

func validateRelease(release Release) error {
	if err := validateID(release.ReleaseID, "RELEASE_ID", "releaseId"); err != nil {
		return err
	}
	if err := validateVersion(release.Version); err != nil {
		return err
	}
	if err := requirePositiveSafe(release.BuildNumber, "BUILD_NUMBER", "release buildNumber"); err != nil {
		return err
	}
	if _, err := parseTimestamp(release.PublishedAt, "release.publishedAt"); err != nil {
		return err
	}
	if err := requireContract(len(release.Artifacts) > 0, "RELEASE_ARTIFACTS", "release must contain artifacts"); err != nil {
		return err
	}
	artifactIDs := make([]string, len(release.Artifacts))
	targets := make([]string, len(release.Artifacts))
	for index, artifact := range release.Artifacts {
		artifactIDs[index] = artifact.ArtifactID
		targets[index] = artifact.Platform + ":" + artifact.Architecture
		if err := validateArtifact(artifact, release); err != nil {
			return err
		}
	}
	if err := requireContract(sortedUnique(artifactIDs), "ARTIFACT_ORDER", "artifacts must be sorted and unique by artifactId"); err != nil {
		return err
	}
	sort.Strings(targets)
	return requireContract(sortedUnique(targets), "ARTIFACT_TARGET", "artifact platform and architecture targets must be unique")
}

func validateArtifact(artifact Artifact, release Release) error {
	if err := validateID(artifact.ArtifactID, "ARTIFACT_ID", "artifactId"); err != nil {
		return err
	}
	if err := requireContract(artifact.FileName != "" && artifact.FileName != "." && artifact.FileName != ".." && !strings.ContainsAny(artifact.FileName, `/\\`), "ARTIFACT_FILE_NAME", "artifact fileName must be a basename"); err != nil {
		return err
	}
	if artifact.Platform != "macos" && artifact.Platform != "linux" && artifact.Platform != "windows" {
		return contractError("ARTIFACT_PLATFORM", "unsupported artifact platform", nil)
	}
	if artifact.Architecture != "arm64" && artifact.Architecture != "x64" && artifact.Architecture != "universal2" {
		return contractError("ARTIFACT_ARCHITECTURE", "unsupported artifact architecture", nil)
	}
	if !osVersionPattern.MatchString(artifact.MinOS) {
		return contractError("ARTIFACT_MIN_OS", "artifact minOS is invalid", nil)
	}
	if artifact.Version != release.Version || artifact.BuildNumber != release.BuildNumber {
		return contractError("ARTIFACT_RELEASE", "artifact version/build does not match release", nil)
	}
	if err := requirePositiveSafe(artifact.Size, "ARTIFACT_SIZE", "artifact size"); err != nil {
		return err
	}
	if !digestPattern.MatchString(artifact.SHA256) {
		return contractError("ARTIFACT_DIGEST", "artifact sha256 is invalid", nil)
	}
	if artifact.Signature == nil {
		return contractError("ARTIFACT_SIGNATURE", "artifact signature is required", nil)
	}
	return validateSignature(*artifact.Signature)
}

func validateState(state ContractState) error {
	if state.Schema != StateSchema {
		return contractError("STATE_SCHEMA", "unsupported update state schema", nil)
	}
	if err := requirePositiveSafe(state.Keyring.Sequence, "STATE_KEYRING_SEQUENCE", "state keyring sequence"); err != nil {
		return err
	}
	if !digestPattern.MatchString(state.Keyring.Digest) {
		return contractError("STATE_KEYRING_DIGEST", "state keyring digest is invalid", nil)
	}
	if !sortedUnique(state.Keyring.RevokedKeyIDs) {
		return contractError("STATE_REVOKED_KEYS", "revoked key IDs must be sorted and unique", nil)
	}
	for _, keyID := range state.Keyring.RevokedKeyIDs {
		if err := validateID(keyID, "STATE_REVOKED_KEYS", "revoked keyId"); err != nil {
			return err
		}
	}
	if state.Feeds == nil {
		return contractError("STATE_FEEDS", "state feeds map is required", nil)
	}
	for feedID, entry := range state.Feeds {
		if err := validateID(feedID, "STATE_FEED_ID", "state feedId"); err != nil {
			return err
		}
		if err := requirePositiveSafe(entry.Sequence, "STATE_FEED_SEQUENCE", "state feed sequence"); err != nil {
			return err
		}
		if !digestPattern.MatchString(entry.Digest) {
			return contractError("STATE_FEED_DIGEST", "state feed digest is invalid", nil)
		}
		if err := validateID(entry.ReleaseID, "STATE_RELEASE_ID", "state releaseId"); err != nil {
			return err
		}
		if err := validateVersion(entry.Version); err != nil {
			return err
		}
		if err := requirePositiveSafe(entry.BuildNumber, "STATE_BUILD_NUMBER", "state buildNumber"); err != nil {
			return err
		}
		if _, err := parseTimestamp(entry.AcceptedAt, "state acceptedAt"); err != nil {
			return err
		}
	}
	return nil
}

func validateSignatures(signatures []Signature, requireNonEmpty bool) error {
	if requireNonEmpty && len(signatures) == 0 {
		return contractError("SIGNATURES", "at least one signature is required", nil)
	}
	keyIDs := make([]string, len(signatures))
	for index, signature := range signatures {
		keyIDs[index] = signature.KeyID
		if err := validateSignature(signature); err != nil {
			return err
		}
	}
	return requireContract(sortedUnique(keyIDs), "SIGNATURE_ORDER", "signatures must be sorted and unique by keyId")
}

func validateSignature(signature Signature) error {
	if err := validateID(signature.KeyID, "SIGNATURE_KEY_ID", "signature keyId"); err != nil {
		return err
	}
	if signature.Algorithm != SignatureAlgorithm {
		return contractError("SIGNATURE_ALGORITHM", "unsupported signature algorithm", nil)
	}
	decoded, err := decodeCanonicalBase64(signature.SignatureBase64, "SIGNATURE_ENCODING")
	if err != nil {
		return err
	}
	return requireContract(len(decoded) == ed25519.SignatureSize, "SIGNATURE_ENCODING", "Ed25519 signature has invalid size")
}

func decodeCanonicalBase64(value, code string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != value {
		return nil, contractError(code, "base64 value is not canonical", err)
	}
	return decoded, nil
}

func validateID(value, code, label string) error {
	return requireContract(idPattern.MatchString(value), code, label+" is invalid")
}

func validateVersion(version string) error {
	match := semverPattern.FindStringSubmatch(version)
	if match == nil {
		return contractError("VERSION", "version is not valid SemVer", nil)
	}
	if match[4] != "" {
		for _, identifier := range strings.Split(match[4], ".") {
			if len(identifier) > 1 && identifier[0] == '0' && allDigits(identifier) {
				return contractError("VERSION", "numeric prerelease identifiers cannot have leading zeroes", nil)
			}
		}
	}
	return nil
}

func requirePositiveSafe(value int64, code, label string) error {
	return requireContract(value > 0 && value <= maxSafeInteger, code, label+" must be a positive safe integer")
}

func sortedUnique(values []string) bool {
	for index := range values {
		if index > 0 && values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func allDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return value != ""
}

func publicKeyFor(key Key) (ed25519.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(key.PublicKeySPKIBase64)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("public key is not Ed25519")
	}
	return publicKey, nil
}

func keyUsable(key Key, role string, signedAt, now time.Time) bool {
	if key.Status != "active" || !contains(key.Roles, role) {
		return false
	}
	notBefore, err := parseTimestamp(key.NotBefore, "key.notBefore")
	if err != nil {
		return false
	}
	notAfter, err := parseTimestamp(key.NotAfter, "key.notAfter")
	if err != nil {
		return false
	}
	return !signedAt.Before(notBefore) && signedAt.Before(notAfter) && now.Before(notAfter)
}

func verifySignatureQuorum(document any, signatures []Signature, keyring Keyring, role string, threshold int64, signedAt, now time.Time) error {
	message, err := unsignedDocumentBytes(document)
	if err != nil {
		return err
	}
	keys := make(map[string]Key, len(keyring.Keys))
	for _, key := range keyring.Keys {
		keys[key.KeyID] = key
	}
	valid := int64(0)
	for _, signature := range signatures {
		key, found := keys[signature.KeyID]
		if !found || !keyUsable(key, role, signedAt, now) {
			continue
		}
		publicKey, err := publicKeyFor(key)
		if err != nil {
			continue
		}
		signatureBytes, err := base64.StdEncoding.DecodeString(signature.SignatureBase64)
		if err == nil && ed25519.Verify(publicKey, message, signatureBytes) {
			valid++
		}
	}
	return requireContract(valid >= threshold, "SIGNATURE_QUORUM", role+" signature quorum was not satisfied")
}

func verifyArchiveSignature(feed Feed, artifact Artifact, keyring Keyring, signedAt, now time.Time) error {
	message, err := typedCanonical(archiveStatement(feed, artifact))
	if err != nil {
		return err
	}
	signature := artifact.Signature
	if signature == nil {
		return contractError("ARTIFACT_SIGNATURE", "artifact signature is required", nil)
	}
	for _, key := range keyring.Keys {
		if key.KeyID != signature.KeyID || !keyUsable(key, "archive", signedAt, now) {
			continue
		}
		publicKey, err := publicKeyFor(key)
		if err != nil {
			break
		}
		signatureBytes, err := base64.StdEncoding.DecodeString(signature.SignatureBase64)
		if err == nil && ed25519.Verify(publicKey, message, signatureBytes) {
			return nil
		}
	}
	return contractError("ARCHIVE_SIGNATURE", "archive signature is invalid for key "+signature.KeyID, nil)
}

func verifyFreshDocument(issuedAt, expiresAt, label string, now time.Time) (time.Time, error) {
	issued, err := parseTimestamp(issuedAt, label+".issuedAt")
	if err != nil {
		return time.Time{}, err
	}
	expires, err := parseTimestamp(expiresAt, label+".expiresAt")
	if err != nil {
		return time.Time{}, err
	}
	if issued.After(now.Add(maxFutureSkew)) {
		return time.Time{}, contractError("DOCUMENT_FROM_FUTURE", label+" issuedAt is too far in the future", nil)
	}
	if !expires.After(now) {
		return time.Time{}, contractError("DOCUMENT_EXPIRED", label+" has expired", nil)
	}
	return issued, nil
}

func createInitialState(keyring Keyring) (ContractState, error) {
	digest, err := DocumentDigest(keyring)
	if err != nil {
		return ContractState{}, err
	}
	revoked := make([]string, 0)
	for _, key := range keyring.Keys {
		if key.Status == "revoked" {
			revoked = append(revoked, key.KeyID)
		}
	}
	sort.Strings(revoked)
	return ContractState{
		Schema:  StateSchema,
		Keyring: KeyringState{Sequence: keyring.Sequence, Digest: digest, RevokedKeyIDs: revoked},
		Feeds:   make(map[string]FeedState),
	}, nil
}

func verifyPinnedKeyring(keyring Keyring, state ContractState) error {
	if err := validateState(state); err != nil {
		return err
	}
	if state.Keyring.Sequence != keyring.Sequence {
		return contractError("KEYRING_ROLLBACK", "keyring sequence does not match pinned state", nil)
	}
	digest, err := DocumentDigest(keyring)
	if err != nil {
		return err
	}
	if state.Keyring.Digest != digest {
		return contractError("KEYRING_REPLAY_SUBSTITUTION", "keyring digest does not match pinned state", nil)
	}
	revoked := make(map[string]struct{}, len(state.Keyring.RevokedKeyIDs))
	for _, keyID := range state.Keyring.RevokedKeyIDs {
		revoked[keyID] = struct{}{}
	}
	for _, key := range keyring.Keys {
		if _, wasRevoked := revoked[key.KeyID]; wasRevoked && key.Status != "revoked" {
			return contractError("KEY_REVOCATION_ROLLBACK", "revoked key "+key.KeyID+" was reactivated", nil)
		}
	}
	return nil
}

func verifyBootstrapKeyring(keyring Keyring, now time.Time) error {
	issued, err := verifyFreshDocument(keyring.IssuedAt, keyring.ExpiresAt, "bootstrap keyring", now)
	if err != nil {
		return err
	}
	return verifySignatureQuorum(keyring, keyring.Signatures, keyring, "root", keyring.Threshold, issued, now)
}

func verifyKeyringRotation(current, candidate Keyring, state ContractState, now time.Time) (ContractState, error) {
	if err := validateKeyring(current); err != nil {
		return ContractState{}, err
	}
	if err := validateKeyring(candidate); err != nil {
		return ContractState{}, err
	}
	if err := verifyPinnedKeyring(current, state); err != nil {
		return ContractState{}, err
	}
	if _, err := verifyFreshDocument(current.IssuedAt, current.ExpiresAt, "current keyring", now); err != nil {
		return ContractState{}, err
	}
	issued, err := verifyFreshDocument(candidate.IssuedAt, candidate.ExpiresAt, "candidate keyring", now)
	if err != nil {
		return ContractState{}, err
	}
	if candidate.Sequence <= current.Sequence {
		return ContractState{}, contractError("KEYRING_ROLLBACK", "candidate keyring sequence must advance", nil)
	}
	candidateKeys := make(map[string]Key, len(candidate.Keys))
	for _, key := range candidate.Keys {
		candidateKeys[key.KeyID] = key
	}
	for _, oldKey := range current.Keys {
		nextKey, found := candidateKeys[oldKey.KeyID]
		if !found || !immutableKeyMaterialEqual(oldKey, nextKey) {
			return ContractState{}, contractError("KEY_ID_REPLACED", "candidate changed or removed key "+oldKey.KeyID, nil)
		}
		if oldKey.Status == "revoked" && nextKey.Status != "revoked" {
			return ContractState{}, contractError("KEY_REVOCATION_ROLLBACK", "candidate reactivated key "+oldKey.KeyID, nil)
		}
	}
	if err := verifySignatureQuorum(candidate, candidate.Signatures, current, "root", current.Threshold, issued, now); err != nil {
		return ContractState{}, err
	}
	if err := verifySignatureQuorum(candidate, candidate.Signatures, candidate, "root", candidate.Threshold, issued, now); err != nil {
		return ContractState{}, err
	}
	digest, err := DocumentDigest(candidate)
	if err != nil {
		return ContractState{}, err
	}
	revoked := append([]string(nil), state.Keyring.RevokedKeyIDs...)
	for _, key := range candidate.Keys {
		if key.Status == "revoked" && !contains(revoked, key.KeyID) {
			revoked = append(revoked, key.KeyID)
		}
	}
	sort.Strings(revoked)
	next := cloneContractState(state)
	next.Keyring = KeyringState{Sequence: candidate.Sequence, Digest: digest, RevokedKeyIDs: revoked}
	if err := validateState(next); err != nil {
		return ContractState{}, err
	}
	return next, nil
}

func immutableKeyMaterialEqual(left, right Key) bool {
	return left.KeyID == right.KeyID &&
		left.Algorithm == right.Algorithm &&
		left.PublicKeySPKIBase64 == right.PublicKeySPKIBase64 &&
		left.PublicKeySPKISHA256 == right.PublicKeySPKISHA256
}

func cloneContractState(state ContractState) ContractState {
	next := state
	next.Keyring.RevokedKeyIDs = append([]string(nil), state.Keyring.RevokedKeyIDs...)
	next.Feeds = make(map[string]FeedState, len(state.Feeds))
	for key, value := range state.Feeds {
		next.Feeds[key] = value
	}
	return next
}

func compareVersions(left, right string) (int, error) {
	leftMatch := semverPattern.FindStringSubmatch(left)
	rightMatch := semverPattern.FindStringSubmatch(right)
	if leftMatch == nil || rightMatch == nil {
		return 0, contractError("VERSION", "cannot compare invalid SemVer", nil)
	}
	for index := 1; index <= 3; index++ {
		if order := compareNumericText(leftMatch[index], rightMatch[index]); order != 0 {
			return order, nil
		}
	}
	leftPre := leftMatch[4]
	rightPre := rightMatch[4]
	if leftPre == "" && rightPre == "" {
		return 0, nil
	}
	if leftPre == "" {
		return 1, nil
	}
	if rightPre == "" {
		return -1, nil
	}
	leftParts := strings.Split(leftPre, ".")
	rightParts := strings.Split(rightPre, ".")
	for index := 0; index < len(leftParts) && index < len(rightParts); index++ {
		leftNumeric := allDigits(leftParts[index])
		rightNumeric := allDigits(rightParts[index])
		switch {
		case leftNumeric && rightNumeric:
			if order := compareNumericText(leftParts[index], rightParts[index]); order != 0 {
				return order, nil
			}
		case leftNumeric:
			return -1, nil
		case rightNumeric:
			return 1, nil
		default:
			if leftParts[index] < rightParts[index] {
				return -1, nil
			}
			if leftParts[index] > rightParts[index] {
				return 1, nil
			}
		}
	}
	if len(leftParts) < len(rightParts) {
		return -1, nil
	}
	if len(leftParts) > len(rightParts) {
		return 1, nil
	}
	return 0, nil
}

func compareNumericText(left, right string) int {
	left = strings.TrimLeft(left, "0")
	right = strings.TrimLeft(right, "0")
	if left == "" {
		left = "0"
	}
	if right == "" {
		right = "0"
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func compareOSVersions(current, minimum string) (int, error) {
	if !osVersionPattern.MatchString(current) || !osVersionPattern.MatchString(minimum) {
		return 0, contractError("OS_VERSION", "OS version is invalid", nil)
	}
	left := strings.Split(current, ".")
	right := strings.Split(minimum, ".")
	for len(left) < 4 {
		left = append(left, "0")
	}
	for len(right) < 4 {
		right = append(right, "0")
	}
	for index := 0; index < 4; index++ {
		if order := compareNumericText(left[index], right[index]); order != 0 {
			return order, nil
		}
	}
	return 0, nil
}
