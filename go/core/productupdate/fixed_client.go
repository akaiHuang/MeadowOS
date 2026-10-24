package productupdate

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	keyringSequencePlaceholder = "{sequence}"
	artifactIDPlaceholder      = "{artifactId}"
)

// FixedHTTPSFeedClientConfig is immutable composition data. Templates are
// same-origin absolute paths with exactly one typed placeholder.
type FixedHTTPSFeedClientConfig struct {
	Origin              string
	FeedPath            string
	KeyringPathTemplate string
	ArchivePathTemplate string
}

type fixedHTTPSFeedClient struct {
	origin              *url.URL
	feedURL             string
	keyringPathTemplate string
	archivePathTemplate string
	client              *http.Client
}

// NewFixedHTTPSFeedClient constructs a redirect-free, proxy-free client. A
// transport may be injected only by direct tests; production passes nil.
func NewFixedHTTPSFeedClient(config FixedHTTPSFeedClientConfig, transport http.RoundTripper) (FixedFeedClient, error) {
	origin, err := parseFixedOrigin(config.Origin)
	if err != nil {
		return nil, err
	}
	if err := validateFixedEndpointPath(config.FeedPath, ""); err != nil {
		return nil, err
	}
	if err := validateKeyringEndpointPath(config.KeyringPathTemplate); err != nil {
		return nil, err
	}
	if err := validateFixedEndpointPath(config.ArchivePathTemplate, artifactIDPlaceholder); err != nil {
		return nil, err
	}
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.Proxy = nil
		base.DisableCompression = true
		if base.TLSClientConfig == nil {
			base.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		} else {
			base.TLSClientConfig = base.TLSClientConfig.Clone()
			base.TLSClientConfig.MinVersion = tls.VersionTLS13
		}
		transport = base
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &fixedHTTPSFeedClient{
		origin: origin, feedURL: fixedURL(origin, config.FeedPath),
		keyringPathTemplate: config.KeyringPathTemplate,
		archivePathTemplate: config.ArchivePathTemplate,
		client:              client,
	}, nil
}

// validateKeyringEndpointPath accepts either an immutable sequence-addressed
// keyring path or one fixed same-origin path whose signed document carries the
// authoritative sequence.  Both forms retain the signature, rotation and
// anti-rollback checks in Service.resolveCandidate; the fixed form matches the
// production publishing contract without weakening origin or redirect policy.
func validateKeyringEndpointPath(value string) error {
	if strings.Contains(value, keyringSequencePlaceholder) {
		return validateFixedEndpointPath(value, keyringSequencePlaceholder)
	}
	return validateFixedEndpointPath(value, "")
}

func (client *fixedHTTPSFeedClient) FetchFeed(ctx context.Context) (Download, error) {
	return client.fetch(ctx, client.feedURL, "application/json")
}

func (client *fixedHTTPSFeedClient) FetchKeyring(ctx context.Context, sequence int64) (Download, error) {
	if sequence <= 0 || sequence > maxSafeInteger {
		return Download{}, contractError("KEYRING_FETCH", "requested keyring sequence is invalid", nil)
	}
	path := strings.Replace(client.keyringPathTemplate, keyringSequencePlaceholder, strconv.FormatInt(sequence, 10), 1)
	return client.fetch(ctx, fixedURL(client.origin, path), "application/json")
}

func (client *fixedHTTPSFeedClient) FetchArchive(ctx context.Context, artifactID string) (Download, error) {
	if !idPattern.MatchString(artifactID) {
		return Download{}, contractError("ARCHIVE_FETCH", "requested artifactId is invalid", nil)
	}
	path := strings.Replace(client.archivePathTemplate, artifactIDPlaceholder, url.PathEscape(artifactID), 1)
	return client.fetch(ctx, fixedURL(client.origin, path), "application/octet-stream")
}

func (client *fixedHTTPSFeedClient) fetch(ctx context.Context, target, accept string) (Download, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Download{}, contractError("DOWNLOAD_REQUEST", "cannot create fixed update request", err)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.client.Do(request)
	if err != nil {
		return Download{}, contractError("DOWNLOAD_TRANSPORT", "fixed update request failed", err)
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		response.Body.Close()
		return Download{}, contractError("DOWNLOAD_REDIRECT", "update redirects are forbidden", nil)
	}
	if response.Request == nil || response.Request.URL == nil || response.Request.URL.String() != target {
		response.Body.Close()
		return Download{}, contractError("DOWNLOAD_REDIRECT", "update response did not retain its fixed URL", nil)
	}
	finalOrigin := response.Request.URL.Scheme + "://" + response.Request.URL.Host
	if finalOrigin != client.origin.String() {
		response.Body.Close()
		return Download{}, contractError("DOWNLOAD_ORIGIN", "update response escaped the fixed origin", nil)
	}
	return Download{
		Body: response.Body, StatusCode: response.StatusCode,
		ContentLength: response.ContentLength,
		Origin:        client.origin.String(), FinalOrigin: finalOrigin,
		Redirected: false,
	}, nil
}

func parseFixedOrigin(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, contractError("CONFIG_FEED_ORIGIN", "fixed update origin must be a canonical HTTPS origin", err)
	}
	if parsed.String() != value {
		return nil, contractError("CONFIG_FEED_ORIGIN", "fixed update origin is not canonical", nil)
	}
	return parsed, nil
}

func validateFixedEndpointPath(value, placeholder string) error {
	if value == "" || value[0] != '/' || strings.ContainsAny(value, "?#\\") || strings.Contains(value, "//") {
		return contractError("CONFIG_FEED_PATH", "fixed update endpoint must be an absolute URL path", nil)
	}
	if placeholder == "" {
		if strings.ContainsAny(value, "{}") {
			return contractError("CONFIG_FEED_PATH", "feed path cannot contain placeholders", nil)
		}
	} else if strings.Count(value, placeholder) != 1 || strings.Count(value, "{") != 1 || strings.Count(value, "}") != 1 {
		return contractError("CONFIG_FEED_PATH", "fixed update endpoint has an invalid placeholder", nil)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return contractError("CONFIG_FEED_PATH", "fixed update endpoint contains path traversal", nil)
		}
	}
	if _, err := url.ParseRequestURI(value); err != nil {
		return contractError("CONFIG_FEED_PATH", "fixed update endpoint is invalid", err)
	}
	return nil
}

func fixedURL(origin *url.URL, path string) string {
	return fmt.Sprintf("%s://%s%s", origin.Scheme, origin.Host, path)
}
