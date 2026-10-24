package tools

import (
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// NormalizeEvidenceSource bounds untrusted presentation metadata while retaining
// the tool's actual receipt. It neither generates a summary nor fetches an image.
// Thumbnail URLs remain inert metadata; only the authenticated preview adapter
// may turn an authorized source into a bounded image for the frontend.
func NormalizeEvidenceSource(source EvidenceSource) (EvidenceSource, bool) {
	source.URL = evidenceMetadataURL(source.URL, 4096)
	if source.URL == "" {
		return EvidenceSource{}, false
	}
	source.Title = boundedEvidenceText(source.Title, 512)
	source.SnippetTruncated = source.SnippetTruncated || utf8.RuneCountInString(strings.TrimSpace(source.Snippet)) > 1200
	source.Snippet = boundedEvidenceText(source.Snippet, 1200)
	source.ContentKind = boundedEvidenceText(source.ContentKind, 80)
	source.FetchError = boundedEvidenceText(source.FetchError, 160)
	source.ThumbnailURL = evidenceMetadataURL(source.ThumbnailURL, 2048)
	source.FetchedAt = strings.TrimSpace(source.FetchedAt)
	if _, err := time.Parse(time.RFC3339Nano, source.FetchedAt); err != nil {
		source.FetchedAt = ""
	}
	if !meadowSourceVersion.MatchString(source.SourceVersion) {
		source.SourceVersion = ""
	}
	return source, true
}

func boundedEvidenceText(value string, limit int) string {
	value = strings.TrimSpace(value)
	characters := []rune(value)
	if len(characters) > limit {
		return string(characters[:limit])
	}
	return value
}

func evidenceMetadataURL(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit || strings.ContainsAny(value, "\x00\r\n\t") {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return value
}
