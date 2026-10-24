// Package upstreamconfig reads the public metadata of a private Core route
// catalog. Credentials remain Core-owned: this package never opens key files.
package upstreamconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const maxCatalogBytes = 256 << 10

// Profile contains only non-secret route metadata. JSON field order matches
// Python's sort_keys canonicalization used by Core's configuration identity.
type Profile struct {
	APIFormat      string `json:"api_format"`
	Context        int    `json:"context"`
	DisplayName    string `json:"display_name"`
	MaxOutput      int    `json:"max_output"`
	Model          string `json:"model"`
	Provider       string `json:"provider"`
	SupportsImages bool   `json:"supports_images"`
	URL            string `json:"url"`
}

type Catalog struct {
	DefaultModel string    `json:"default_model"`
	Profiles     []Profile `json:"profiles"`
}

type rawProfile struct {
	Model          string          `json:"model"`
	URL            string          `json:"url"`
	APIFormat      string          `json:"api_format"`
	Context        int             `json:"context"`
	MaxOutput      int             `json:"max_output"`
	DisplayName    string          `json:"display_name"`
	Provider       string          `json:"provider"`
	SupportsImages json.RawMessage `json:"supports_images"`
	APIKeyFile     json.RawMessage `json:"api_key_file"`
}

// LoadEnvironment leaves legacy single-route launch settings to the caller.
// An explicitly configured but invalid catalog returns an error, never a
// partially accepted catalog or a fallback to an unrelated local model.
func LoadEnvironment() (*Catalog, error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("MEADOW_CORE_BACKEND")), "openai-compatible") {
		return nil, nil
	}
	path := strings.TrimSpace(os.Getenv("MEADOW_CORE_UPSTREAM_PROFILES_FILE"))
	if path == "" {
		return nil, nil
	}
	return Load(path)
}

func Load(path string) (*Catalog, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("upstream catalog path must be absolute")
	}
	// Check the same opened inode as the non-symlink path. Errors never echo
	// catalog bytes, route URLs or credential-file references.
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("upstream catalog must be a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("upstream catalog is unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 || opened.Size() > maxCatalogBytes {
		return nil, fmt.Errorf("upstream catalog must be a private regular file of at most 256 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCatalogBytes+1))
	if err != nil || len(data) > maxCatalogBytes || !utf8.Valid(data) {
		return nil, fmt.Errorf("upstream catalog exceeds its read limit")
	}
	if !validCatalogFields(data) {
		return nil, fmt.Errorf("upstream catalog has invalid fields or JSON")
	}
	var raw struct {
		DefaultModel string       `json:"default_model"`
		Profiles     []rawProfile `json:"profiles"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("upstream catalog has invalid fields or JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("upstream catalog must contain exactly one JSON object")
	}
	catalog := &Catalog{DefaultModel: strings.TrimSpace(raw.DefaultModel), Profiles: make([]Profile, 0, len(raw.Profiles))}
	if !validText(catalog.DefaultModel) || len(raw.Profiles) == 0 || len(raw.Profiles) > 32 {
		return nil, fmt.Errorf("upstream catalog requires a default and 1 to 32 profiles")
	}
	seen := make(map[string]bool, len(raw.Profiles))
	for _, item := range raw.Profiles {
		profile := Profile{
			Model: strings.TrimSpace(item.Model), URL: strings.TrimRight(strings.TrimSpace(item.URL), "/"),
			APIFormat: strings.ToLower(strings.TrimSpace(item.APIFormat)), Context: item.Context, MaxOutput: item.MaxOutput,
			DisplayName: strings.TrimSpace(item.DisplayName), Provider: strings.TrimSpace(item.Provider),
		}
		if profile.APIFormat == "" {
			profile.APIFormat = "openai"
		}
		if profile.DisplayName == "" {
			profile.DisplayName = profile.Model
		}
		if profile.Provider == "" {
			profile.Provider = profile.Model
		}
		if !validText(profile.Model) || !validText(profile.DisplayName) || !validText(profile.Provider) || seen[profile.Model] {
			return nil, fmt.Errorf("upstream catalog contains invalid or duplicate profile IDs or labels")
		}
		if profile.Context <= 0 || profile.MaxOutput <= 0 || profile.MaxOutput >= profile.Context {
			return nil, fmt.Errorf("upstream catalog output limits must be positive and smaller than context")
		}
		switch profile.APIFormat {
		case "openai", "mlx-vlm", "xai", "deepseek":
		default:
			return nil, fmt.Errorf("upstream catalog contains an unsupported API format")
		}
		if !validURL(profile.URL) {
			return nil, fmt.Errorf("upstream catalog requires HTTPS or loopback HTTP URLs without credentials, queries or fragments")
		}
		if len(item.SupportsImages) > 0 {
			if string(item.SupportsImages) == "null" || json.Unmarshal(item.SupportsImages, &profile.SupportsImages) != nil {
				return nil, fmt.Errorf("upstream catalog image capability must be a boolean")
			}
		}
		if len(item.APIKeyFile) > 0 {
			// Type-check and discard the reference. Only Core validates/opens it.
			var ignored string
			if string(item.APIKeyFile) == "null" || json.Unmarshal(item.APIKeyFile, &ignored) != nil {
				return nil, fmt.Errorf("upstream catalog credential reference must be a string")
			}
			if ignored = strings.TrimSpace(ignored); ignored != "" && !filepath.IsAbs(ignored) {
				return nil, fmt.Errorf("upstream catalog credential reference must be absolute")
			}
		}
		seen[profile.Model] = true
		catalog.Profiles = append(catalog.Profiles, profile)
	}
	if !seen[catalog.DefaultModel] {
		return nil, fmt.Errorf("upstream catalog must include its default model")
	}
	return catalog, nil
}

func validText(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < 32 {
			return false
		}
	}
	return true
}

// encoding/json normally matches struct fields case-insensitively and treats
// null strings as empty. Keep the private JSON contract exact across Go/Core.
func validCatalogFields(data []byte) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return false
	}
	for key := range root {
		if key != "default_model" && key != "profiles" {
			return false
		}
	}
	var profiles []map[string]json.RawMessage
	if json.Unmarshal(root["profiles"], &profiles) != nil {
		return false
	}
	for _, profile := range profiles {
		if profile == nil {
			return false
		}
		for key, raw := range profile {
			switch key {
			case "model", "url", "api_format", "context", "max_output", "display_name", "provider", "supports_images", "api_key_file":
			default:
				return false
			}
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return false
			}
		}
	}
	return true
}

func validURL(value string) bool {
	if strings.ContainsAny(value, "?#") {
		return false
	}
	for _, char := range value {
		if char < 33 {
			return false
		}
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	loopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	port := 0
	if raw := parsed.Port(); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil || port < 0 || port > 65535 {
			return false
		}
	}
	return (scheme == "https" || loopback) && !(loopback && port == 18081)
}

func (c *Catalog) Lookup(model string) (Profile, bool) {
	if c != nil {
		for _, profile := range c.Profiles {
			if profile.Model == model {
				return profile, true
			}
		}
	}
	return Profile{}, false
}

func (c *Catalog) ModelIDs() []string {
	if c == nil {
		return nil
	}
	ids := make([]string, 0, len(c.Profiles))
	for _, profile := range c.Profiles {
		ids = append(ids, profile.Model)
	}
	return ids
}

// Fingerprint matches Python json.dumps(sort_keys=True, ensure_ascii=True).
// API key values and file references are absent from Catalog by construction.
func (c *Catalog) Fingerprint() string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(c)
	var canonical strings.Builder
	for _, char := range strings.TrimSuffix(encoded.String(), "\n") {
		if char < 127 {
			canonical.WriteRune(char)
		} else {
			for _, unit := range utf16.Encode([]rune{char}) {
				fmt.Fprintf(&canonical, `\u%04x`, unit)
			}
		}
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(canonical.String())))
}
