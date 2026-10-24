package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const meadowKnowledgeRequestLimit = 32768

var meadowKnowledgePrivateID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// MeadowKnowledgeClient is a trusted host capability, not a model-visible tool.
// It uses the existing retrieval service and profile principal. Callers retain
// their own session-owned record registry; this client never enumerates it.
type MeadowKnowledgeClient struct {
	config meadowSearchConfig
	http   *http.Client
}

func NewMeadowKnowledgeClient() (*MeadowKnowledgeClient, error) {
	config, err := loadMeadowSearchConfig()
	if err != nil {
		return nil, err
	}
	return &MeadowKnowledgeClient{config: config}, nil
}

// Identity binds private local receipts to the configured service and principal
// without returning either principal or credentials. It is not authorization.
func (c *MeadowKnowledgeClient) Identity() string {
	if c == nil || c.config.baseURL == "" || c.config.actor == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(c.config.baseURL + "\x00" + c.config.actor))
	return "meadow-knowledge-" + hex.EncodeToString(digest[:])
}

type MeadowKnowledgeCreateResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Scope string `json:"scope"`
}

type MeadowKnowledgePutResult struct {
	ID            string `json:"id"`
	Revision      int    `json:"revision"`
	Collection    string `json:"collection"`
	SourceVersion string `json:"source_version"`
	Published     bool   `json:"published"`
}

// Coverage and read coordinates remain provider JSON. In particular, provider
// character offsets are Unicode code points, not Go UTF-8 byte offsets. The
// source version is an opaque provider receipt, not a local raw-text SHA.
type MeadowKnowledgeRecord struct {
	ID                  string          `json:"id"`
	Collection          string          `json:"collection"`
	Title               string          `json:"title"`
	URL                 string          `json:"url"`
	Revision            int             `json:"revision"`
	SourceVersion       string          `json:"source_version"`
	RecordedAt          string          `json:"recorded_at"`
	Text                string          `json:"text"`
	ContentKind         string          `json:"content_kind"`
	Coverage            json.RawMessage `json:"coverage"`
	ReadRef             json.RawMessage `json:"read_ref"`
	MatchedRange        json.RawMessage `json:"matched_range,omitempty"`
	Score               json.RawMessage `json:"score,omitempty"`
	EvidenceIsUntrusted bool            `json:"evidence_is_untrusted"`
}

type MeadowKnowledgeSearchResult struct {
	Status  string                  `json:"status"`
	Results []MeadowKnowledgeRecord `json:"results"`
	Meta    json.RawMessage         `json:"meta"`
}

type MeadowKnowledgeRemoveResult struct {
	DeletedVersions int  `json:"deleted_versions"`
	CacheUsed       bool `json:"cache_used"`
}

// MeadowKnowledgeHTTPError contains only bounded protocol fields. Unknown
// server errors and HTML are never incorporated into diagnostics.
type MeadowKnowledgeHTTPError struct {
	StatusCode        int
	Code              string
	RetryAfterSeconds int // -1 when no valid Retry-After was observed.
}

func (e *MeadowKnowledgeHTTPError) Error() string {
	message := fmt.Sprintf("Meadow Knowledge HTTP %d: %s", e.StatusCode, e.Code)
	if e.RetryAfterSeconds >= 0 {
		message += fmt.Sprintf("; retry after %d seconds", e.RetryAfterSeconds)
	}
	return message
}

func (c *MeadowKnowledgeClient) Create(ctx context.Context, title string) (MeadowKnowledgeCreateResult, error) {
	var result MeadowKnowledgeCreateResult
	if err := meadowKnowledgeText(title, "title", 200, 0); err != nil {
		return result, err
	}
	if err := c.post(ctx, "/v1/knowledge/create", map[string]any{"title": title}, &result); err != nil {
		return MeadowKnowledgeCreateResult{}, err
	}
	if !meadowKnowledgePrivateID.MatchString(result.ID) || result.Scope != "project" {
		return MeadowKnowledgeCreateResult{}, errors.New("Meadow Knowledge returned an invalid private collection receipt")
	}
	return result, nil
}

func (c *MeadowKnowledgeClient) Put(ctx context.Context, collection, title, text string) (MeadowKnowledgePutResult, error) {
	var result MeadowKnowledgePutResult
	if err := meadowKnowledgeCollection(collection); err != nil {
		return result, err
	}
	if err := meadowKnowledgeText(title, "title", 200, 0); err != nil {
		return result, err
	}
	if err := meadowKnowledgeText(text, "text", 8000, 16000); err != nil {
		return result, err
	}
	if err := c.post(ctx, "/v1/knowledge/put", map[string]any{"collection": collection, "title": title, "text": text}, &result); err != nil {
		return MeadowKnowledgePutResult{}, err
	}
	if !meadowKnowledgePrivateID.MatchString(result.ID) || result.Collection != collection || result.Revision < 1 ||
		!meadowSourceVersion.MatchString(result.SourceVersion) || result.Published {
		return MeadowKnowledgePutResult{}, errors.New("Meadow Knowledge returned an invalid private document receipt")
	}
	return result, nil
}

func (c *MeadowKnowledgeClient) Search(ctx context.Context, collection, query string, limit int) (MeadowKnowledgeSearchResult, error) {
	var result MeadowKnowledgeSearchResult
	if err := meadowKnowledgeCollection(collection); err != nil {
		return result, err
	}
	if err := meadowKnowledgeText(query, "query", 1000, 0); err != nil {
		return result, err
	}
	if limit < 1 || limit > 20 {
		return result, errors.New("Meadow Knowledge search limit must be between 1 and 20")
	}
	if err := c.post(ctx, "/v1/knowledge/search", map[string]any{"collection": collection, "query": query, "limit": limit}, &result); err != nil {
		return MeadowKnowledgeSearchResult{}, err
	}
	if (result.Status != "ok" && result.Status != "no_evidence") || len(result.Results) > limit ||
		(result.Status == "no_evidence" && len(result.Results) != 0) {
		return MeadowKnowledgeSearchResult{}, errors.New("Meadow Knowledge returned an invalid search response")
	}
	for _, record := range result.Results {
		if err := meadowKnowledgeRecord(record, collection); err != nil {
			return MeadowKnowledgeSearchResult{}, err
		}
	}
	return result, nil
}

func (c *MeadowKnowledgeClient) Read(ctx context.Context, collection, documentID string, revision int, sourceVersion string, offsetChars, maxTokens int) (MeadowKnowledgeRecord, error) {
	var result MeadowKnowledgeRecord
	if err := meadowKnowledgeCollection(collection); err != nil {
		return result, err
	}
	if !meadowKnowledgePrivateID.MatchString(documentID) || revision < 1 || !meadowSourceVersion.MatchString(sourceVersion) {
		return result, errors.New("Meadow Knowledge read requires a valid private document receipt")
	}
	if offsetChars < 0 || maxTokens < 64 || maxTokens > 4096 {
		return result, errors.New("Meadow Knowledge read requires a nonnegative offset and a token budget between 64 and 4096")
	}
	if err := c.post(ctx, "/v1/knowledge/read", map[string]any{
		"collection": collection, "document_id": documentID, "revision": revision,
		"source_version": sourceVersion, "offset_chars": offsetChars, "max_tokens": maxTokens,
	}, &result); err != nil {
		return MeadowKnowledgeRecord{}, err
	}
	if err := meadowKnowledgeRecord(result, collection); err != nil {
		return MeadowKnowledgeRecord{}, err
	}
	if result.ID != documentID || result.Revision != revision || result.SourceVersion != sourceVersion {
		return MeadowKnowledgeRecord{}, errors.New("Meadow Knowledge returned a different document version")
	}
	var coverage struct {
		StartChar int `json:"start_char"`
	}
	_ = json.Unmarshal(result.Coverage, &coverage)
	if coverage.StartChar != offsetChars {
		return MeadowKnowledgeRecord{}, errors.New("Meadow Knowledge returned a different original-text offset")
	}
	return result, nil
}

func (c *MeadowKnowledgeClient) Remove(ctx context.Context, collection, documentID string) (MeadowKnowledgeRemoveResult, error) {
	var result MeadowKnowledgeRemoveResult
	if err := meadowKnowledgeCollection(collection); err != nil {
		return result, err
	}
	if !meadowKnowledgePrivateID.MatchString(documentID) {
		return result, errors.New("Meadow Knowledge removal requires a private document ID")
	}
	if err := c.post(ctx, "/v1/knowledge/remove", map[string]any{"collection": collection, "document_id": documentID}, &result); err != nil {
		return MeadowKnowledgeRemoveResult{}, err
	}
	if result.DeletedVersions < 1 {
		return MeadowKnowledgeRemoveResult{}, errors.New("Meadow Knowledge returned an invalid removal receipt")
	}
	return result, nil
}

func meadowKnowledgeCollection(collection string) error {
	if !meadowKnowledgePrivateID.MatchString(collection) {
		return errors.New("Meadow Knowledge requires an exact private collection ID")
	}
	return nil
}

func meadowKnowledgeText(value, field string, maxRunes, maxBytes int) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > maxRunes ||
		(maxBytes > 0 && len(value) > maxBytes) {
		return fmt.Errorf("Meadow Knowledge %s exceeds its nonempty UTF-8 text limits", field)
	}
	return nil
}

func meadowKnowledgeRecord(record MeadowKnowledgeRecord, collection string) error {
	if !meadowKnowledgePrivateID.MatchString(record.ID) || record.Collection != collection || record.Revision < 1 ||
		!meadowSourceVersion.MatchString(record.SourceVersion) || !utf8.ValidString(record.Text) || record.Text == "" ||
		(record.ContentKind != "stored_original" && record.ContentKind != "stored_original_excerpt") ||
		!record.EvidenceIsUntrusted || !json.Valid(record.Coverage) ||
		!bytes.HasPrefix(bytes.TrimSpace(record.Coverage), []byte("{")) {
		return errors.New("Meadow Knowledge returned an invalid private evidence receipt")
	}
	var coverage struct {
		StartChar             *int   `json:"start_char"`
		EndChar               *int   `json:"end_char"`
		DocumentCharacters    *int   `json:"document_characters"`
		CharacterUnit         string `json:"character_unit"`
		WholeSourceInResponse *bool  `json:"whole_source_in_response"`
		TextRewritten         *bool  `json:"text_rewritten"`
		SourceTruncated       *bool  `json:"source_truncated"`
	}
	if json.Unmarshal(record.Coverage, &coverage) != nil || coverage.StartChar == nil || coverage.EndChar == nil ||
		coverage.DocumentCharacters == nil || coverage.CharacterUnit != "unicode_code_points" ||
		coverage.WholeSourceInResponse == nil || coverage.TextRewritten == nil || *coverage.TextRewritten ||
		coverage.SourceTruncated == nil || *coverage.SourceTruncated || *coverage.StartChar < 0 ||
		*coverage.EndChar <= *coverage.StartChar || *coverage.EndChar > *coverage.DocumentCharacters ||
		*coverage.EndChar-*coverage.StartChar != utf8.RuneCountInString(record.Text) ||
		*coverage.WholeSourceInResponse != (*coverage.StartChar == 0 && *coverage.EndChar == *coverage.DocumentCharacters) ||
		(record.ContentKind == "stored_original") != *coverage.WholeSourceInResponse {
		return errors.New("Meadow Knowledge returned invalid original-text coordinates")
	}
	var ref struct {
		Collection    string `json:"collection"`
		DocumentID    string `json:"document_id"`
		Revision      int    `json:"revision"`
		SourceVersion string `json:"source_version"`
		OffsetChars   *int   `json:"offset_chars"`
	}
	if json.Unmarshal(record.ReadRef, &ref) != nil || ref.Collection != collection || ref.DocumentID != record.ID ||
		ref.Revision != record.Revision || ref.SourceVersion != record.SourceVersion || ref.OffsetChars == nil ||
		*ref.OffsetChars < 0 || *ref.OffsetChars >= *coverage.DocumentCharacters {
		return errors.New("Meadow Knowledge returned an invalid private evidence receipt")
	}
	return nil
}

func (c *MeadowKnowledgeClient) post(ctx context.Context, endpoint string, payload, result any) error {
	if c == nil || c.config.baseURL == "" || c.config.token == "" || c.config.actor == "" {
		return errors.New("Meadow Knowledge is not configured")
	}
	if ctx == nil {
		return errors.New("Meadow Knowledge requires a request context")
	}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > meadowKnowledgeRequestLimit {
		return errors.New("Meadow Knowledge request exceeds the safe size limit")
	}
	ctx, cancel := context.WithTimeout(ctx, meadowSearchTimeout)
	defer cancel()
	release, err := acquireMeadowSearch(ctx, c.config.baseURL+"\x00"+c.config.actor)
	if err != nil {
		return fmt.Errorf("Meadow Knowledge admission interrupted: %w", err)
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Meadow Knowledge request interrupted: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Meadow Knowledge endpoint")
	}
	request.Header.Set("Authorization", "Bearer "+c.config.token)
	request.Header.Set("X-Meadow-Actor-ID", c.config.actor)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	client := c.http
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.Timeout = meadowSearchTimeout
	if request.URL.Scheme == "https" {
		copyClient = *guardPublicHTTPClient(&copyClient, false, nil)
	} else {
		transport := copyClient.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		if standard, ok := transport.(*http.Transport); ok {
			direct := standard.Clone()
			direct.Proxy = nil
			copyClient.Transport = direct
		}
	}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer copyClient.CloseIdleConnections()
	response, err := copyClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Meadow Knowledge request interrupted: %w", ctx.Err())
		}
		return errors.New("Meadow Knowledge connection failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, meadowSearchResponseLimit+1))
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Meadow Knowledge response interrupted: %w", ctx.Err())
		}
		return errors.New("Meadow Knowledge response could not be read")
	}
	if len(raw) > meadowSearchResponseLimit {
		return errors.New("Meadow Knowledge response exceeds the safe size limit")
	}
	if response.StatusCode != http.StatusOK {
		return meadowKnowledgeHTTPError(response, raw)
	}
	if !utf8.Valid(raw) || !json.Valid(raw) || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || json.Unmarshal(raw, result) != nil {
		return errors.New("Meadow Knowledge returned invalid JSON")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Meadow Knowledge response interrupted: %w", err)
	}
	return nil
}

func meadowKnowledgeHTTPError(response *http.Response, raw []byte) error {
	failure := &MeadowKnowledgeHTTPError{StatusCode: response.StatusCode, Code: "service_error", RetryAfterSeconds: -1}
	var decoded struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &decoded) == nil {
		switch decoded.Error {
		case "unauthorized", "trusted_principal_required", "not_found", "publication_not_authorized", "project_capacity_reached",
			"collection_capacity_reached", "invalid_text", "document_version_changed", "invalid_read_offset", "invalid_read_budget",
			"read_budget_too_small", "read_budget_exceeded", "busy", "request_timeout", "content_length_required", "request_too_large":
			failure.Code = decoded.Error
		}
	}
	if retry, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && retry >= 0 && retry <= 86400 {
		failure.RetryAfterSeconds = retry
	}
	return failure
}
