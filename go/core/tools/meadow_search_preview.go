package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	meadowPreviewTimeout       = 10 * time.Second
	meadowPreviewResponseLimit = 40 << 10
	meadowPreviewImageLimit    = 24000
	meadowPreviewDimension     = 128
	meadowPreviewURLLimit      = 2048
	meadowPreviewImagePrefix   = "data:image/jpeg;base64,"
)

// FetchMeadowSourcePreview requests an optional, bounded thumbnail for observed
// evidence. The host configuration supplies the provider and profile identity;
// neither can be selected by a source URL. Previews never become model evidence.
// An unconfigured provider or a provider response with no image returns "", nil.
func FetchMeadowSourcePreview(ctx context.Context, source EvidenceSource) (string, error) {
	if !meadowSearchConfigured() {
		return "", nil
	}
	if !meadowPreviewPublicURL(source.URL) ||
		(source.ThumbnailURL != "" && !meadowPreviewPublicURL(source.ThumbnailURL)) {
		return "", errors.New("invalid Meadow Search preview source")
	}
	config, err := loadMeadowSearchConfig()
	if err != nil {
		return "", errors.New("Meadow Search preview configuration is unavailable")
	}
	payload := struct {
		URL      string `json:"url"`
		ImageURL string `json:"image_url,omitempty"`
	}{source.URL, source.ThumbnailURL}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", errors.New("invalid Meadow Search preview request")
	}
	ctx, cancel := context.WithTimeout(ctx, meadowPreviewTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.baseURL+"/v1/preview", bytes.NewReader(body))
	if err != nil {
		return "", errors.New("invalid Meadow Search preview endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+config.token)
	req.Header.Set("X-Meadow-Actor-ID", config.actor)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: meadowPreviewTimeout}
	if req.URL.Scheme == "https" {
		client = guardPublicHTTPClient(client, false, nil)
	} else {
		// HTTP is allowed only for a configured loopback IP. Bypass environment
		// proxies, just as the retrieval transport does, to keep credentials local.
		if transport, ok := http.DefaultTransport.(*http.Transport); ok {
			direct := transport.Clone()
			direct.Proxy = nil
			client.Transport = direct
		} else {
			client.Transport = &http.Transport{Proxy: nil}
		}
	}
	// Even same-host redirects must not receive the service credential.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer closeMeadowPreviewConnections(client)
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("Meadow Search preview connection failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, meadowPreviewResponseLimit+1))
	if err != nil || len(raw) > meadowPreviewResponseLimit {
		return "", errors.New("invalid Meadow Search preview response size")
	}
	if resp.StatusCode != http.StatusOK {
		// Error responses may contain credentials or private upstream diagnostics.
		return "", errors.New("Meadow Search preview is unavailable")
	}
	var result struct {
		Image json.RawMessage `json:"image"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Image) == 0 {
		return "", errors.New("invalid Meadow Search preview response")
	}
	if bytes.Equal(result.Image, []byte("null")) {
		return "", nil
	}
	var image string
	if err := json.Unmarshal(result.Image, &image); err != nil || !meadowPreviewJPEG(image) {
		return "", errors.New("invalid Meadow Search preview image")
	}
	return image, nil
}

func closeMeadowPreviewConnections(client *http.Client) {
	client.CloseIdleConnections()
	// The public-address guard wraps the transport, so Client cannot reach its
	// idle pool. Each optional request owns a fresh pool and must close it here.
	if guard, ok := client.Transport.(*publicHTTPGuardRoundTripper); ok {
		if closer, ok := guard.base.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

// The service resolves and pins the final public address for page/image URLs.
// Reject syntactically non-public targets here before forwarding any request.
func meadowPreviewPublicURL(value string) bool {
	if value == "" || len(value) > meadowPreviewURLLimit || value != strings.TrimSpace(value) {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" ||
		u.User != nil || u.Hostname() == "" || u.Fragment != "" || u.RawFragment != "" ||
		strings.Contains(value, "#") || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if publicHTTPHostnameIsBlocked(host) || strings.HasSuffix(host, ".internal") || strings.Contains(host, "%") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		// Match the service's rejection of IPv4-mapped IPv6 spellings, too.
		return !publicHTTPIPIsBlocked(ip) && !(strings.Contains(host, ":") && ip.To4() != nil)
	}
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

func meadowPreviewJPEG(value string) bool {
	if !strings.HasPrefix(value, meadowPreviewImagePrefix) {
		return false
	}
	encoded := strings.TrimPrefix(value, meadowPreviewImagePrefix)
	if len(encoded) > base64.StdEncoding.EncodedLen(meadowPreviewImageLimit) {
		return false
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > meadowPreviewImageLimit ||
		base64.StdEncoding.EncodeToString(data) != encoded {
		return false
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 ||
		config.Width > meadowPreviewDimension || config.Height > meadowPreviewDimension {
		return false
	}
	// DecodeConfig alone accepts truncated images whose header happens to survive.
	_, err = jpeg.Decode(bytes.NewReader(data))
	return err == nil
}
