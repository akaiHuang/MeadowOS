package telemetry

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

var metadataKeys = map[string]bool{"source": true, "runtime": true, "turn": true, "sequence": true, "message_count": true, "tool_count": true, "http_status": true, "streaming": true, "response_bytes": true, "finish_reason": true, "error_code": true, "tool_name": true, "call_id": true, "ttft_ms": true, "elapsed_ms": true, "prompt_tokens": true, "completion_tokens": true, "tool_calls": true, "stop_reason": true, "dropped_events": true, "payload_truncated": true, "evidence_support": true, "claim_supported": true, "completed_truth": true}
var credentials = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+|(?:sk-|pk-lf-|sk-lf-)[A-Za-z0-9_-]{8,}|((?:api[_-]?key|access[_-]?token|password|secret)\s*[:=]\s*["']?)[^\s,"'}]+`)

func RedactText(value string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return credentials.ReplaceAllString(value, "[redacted]")
}
func clip(value string, n int) string {
	if len(value) <= n {
		return value
	}
	value = value[:n]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
func Project(event Event, diagnostic bool) Event {
	event.Model = clip(RedactText(event.Model), 256)
	metadata := map[string]any{}
	for key, value := range event.Metadata {
		if !metadataKeys[key] {
			continue
		}
		switch v := value.(type) {
		case string:
			metadata[key] = clip(RedactText(v), 256)
		case bool, float64, int, int64, uint64:
			metadata[key] = v
		}
	}
	event.Metadata = metadata
	if !diagnostic || !event.Diagnostic {
		event.Payload = nil
		event.Diagnostic = false
		return event
	}
	payload := map[string]any{}
	for _, key := range []string{"input", "output"} {
		if value, ok := event.Payload[key]; ok {
			clean := sanitize(value, 0)
			body, err := json.Marshal(clean)
			limit := 256 << 10
			if key == "output" {
				limit = 128 << 10
			}
			if err != nil || len(body) > limit {
				payload[key] = map[string]any{"omitted": "diagnostic_payload_limit"}
				metadata["payload_truncated"] = true
			} else {
				payload[key] = clean
				if hasOmission(clean) {
					metadata["payload_truncated"] = true
				}
			}
		}
	}
	event.Payload = payload
	return event
}

// DiagnosticPayload never captures headers, binary image/audio bytes or private
// provider reasoning. Free-form content is opt-in and still may contain personal
// material; common credentials and explicit current API keys are removed.
func DiagnosticPayload(raw []byte, secrets ...string) any {
	if len(raw) > 32<<20 {
		return map[string]any{"omitted": "diagnostic_source_limit"}
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return redactValues(sanitize(value, 0), secrets)
}
func redactValues(value any, secrets []string) any {
	switch v := value.(type) {
	case string:
		return RedactText(v, secrets...)
	case map[string]any:
		for key, item := range v {
			v[key] = redactValues(item, secrets)
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = redactValues(item, secrets)
		}
		return v
	default:
		return value
	}
}

func sanitize(value any, depth int) any {
	if depth > 24 {
		return "[depth limit]"
	}
	switch v := value.(type) {
	case map[string]any:
		if receipt, ok := mediaFromMap(v); ok {
			return receipt
		}
		clean := map[string]any{}
		for key, item := range v {
			lower := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if strings.Contains(lower, "reasoning") || strings.Contains(lower, "secret") || strings.Contains(lower, "credential") || strings.Contains(lower, "token") && lower != "max_tokens" && lower != "max_completion_tokens" || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "api_key") || lower == "headers" || lower == "image_url" || lower == "input_audio" || lower == "audio" || lower == "cache_salt" {
				continue
			}
			clean[key] = sanitize(item, depth+1)
		}
		return clean
	case []any:
		clean := make([]any, 0, min(len(v), 1024))
		for i, item := range v {
			if i >= 1024 {
				break
			}
			clean = append(clean, sanitize(item, depth+1))
		}
		if len(v) > 1024 {
			clean = append(clean, map[string]any{"omitted": "diagnostic_array_limit"})
		}
		return clean
	case string:
		if strings.HasPrefix(v, "data:") {
			return mediaReceipt(v)
		}
		return RedactText(v)
	default:
		return value
	}
}

// Media hashes cover decoded data bytes; remote references are labelled as URL
// reference bytes. Collection never fetches a remote image or exports its URL.
func mediaFromMap(value map[string]any) (any, bool) {
	if raw, ok := value["image_url"]; ok && (value["type"] == "image_url" || value["type"] == "input_image" || len(value) == 1) {
		if image, ok := raw.(map[string]any); ok {
			if url, ok := image["url"].(string); ok {
				return mediaReceipt(url), true
			}
		}
		if url, ok := raw.(string); ok {
			return mediaReceipt(url), true
		}
		return mediaReceipt(""), true
	}
	if raw, ok := value["input_audio"].(map[string]any); ok && (value["type"] == "input_audio" || len(value) == 1) {
		data, _ := raw["data"].(string)
		format, _ := raw["format"].(string)
		return mediaReceipt("data:audio/" + format + ";base64," + data), true
	}
	return nil, false
}
func mediaReceipt(value string) any {
	mime := "application/x-url-reference"
	var reader io.Reader = strings.NewReader(value)
	if strings.HasPrefix(value, "data:") {
		if comma := strings.IndexByte(value, ','); comma >= 0 {
			header := value[5:comma]
			mime = strings.Split(header, ";")[0]
			if mime == "" {
				mime = "application/octet-stream"
			}
			if strings.HasSuffix(header, ";base64") {
				reader = base64.NewDecoder(base64.StdEncoding, strings.NewReader(value[comma+1:]))
			} else {
				reader = strings.NewReader(value[comma+1:])
			}
		}
	}
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	if err != nil {
		hash.Reset()
		size, _ = io.Copy(hash, strings.NewReader(value))
		mime = "application/x-invalid-media-reference"
	}
	return map[string]any{"type": "media_omitted", "sha256": hex.EncodeToString(hash.Sum(nil)), "bytes": size, "mime": clip(mime, 128)}
}

func hasOmission(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if marker, ok := v["omitted"].(string); ok && strings.HasPrefix(marker, "diagnostic_") {
			return true
		}
		for _, item := range v {
			if hasOmission(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if hasOmission(item) {
				return true
			}
		}
	case string:
		return v == "[depth limit]"
	}
	return false
}
