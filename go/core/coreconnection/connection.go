// Package coreconnection imports connections to independently owned Meadow Core
// services. It never starts, stops, upgrades, or takes ownership of a service.
package coreconnection

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const ProtocolMajor = 1
const InvitationSchema = "meadow.core.connection-invitation.v1"
const LocalSchema = "meadow.core.local-connection.v1"
const maxDocument = 32 << 10

var coreIDPattern = regexp.MustCompile(`^core_[a-f0-9]{32}$`)
var deviceIDPattern = regexp.MustCompile(`^device_[a-f0-9]{32}$`)

type Invitation struct {
	Schema        string    `json:"schema"`
	CoreID        string    `json:"core_id"`
	ProtocolMajor int       `json:"protocol_major"`
	BaseURL       string    `json:"base_url"`
	ExpiresAt     time.Time `json:"expires_at"`
	PairingToken  string    `json:"pairing_token"`
}

// APIKey is deliberately excluded from JSON marshaling and formatted output.
// Callers persist it in their credential store, never a browser response.
type PairedConnection struct {
	CoreID        string   `json:"core_id"`
	DeviceID      string   `json:"device_id"`
	BaseURL       string   `json:"base_url"`
	ProtocolMajor int      `json:"protocol_major"`
	Scopes        []string `json:"scopes"`
	APIKey        string   `json:"-"`
}

func (p PairedConnection) String() string {
	return "Meadow Core paired connection (credential redacted)"
}
func (p PairedConnection) GoString() string { return p.String() }
func (i Invitation) String() string         { return "Meadow Core pairing invitation (token redacted)" }
func (i Invitation) GoString() string       { return i.String() }

func strictJSON(data []byte, target any) error {
	if len(data) == 0 || len(data) > maxDocument {
		return errors.New("invalid Core connection document size")
	}
	// encoding/json otherwise accepts duplicate keys and case-insensitive aliases.
	// Connection imports use one exact, closed top-level schema.
	typeOf := reflect.TypeOf(target).Elem()
	allowed := map[string]bool{}
	for i := 0; i < typeOf.NumField(); i++ {
		name := strings.Split(typeOf.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			allowed[name] = true
		}
	}
	check := json.NewDecoder(bytes.NewReader(data))
	token, err := check.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid Core connection object")
	}
	seen := map[string]bool{}
	for check.More() {
		token, err := check.Token()
		name, ok := token.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return errors.New("invalid or duplicate Core connection field")
		}
		seen[name] = true
		var raw json.RawMessage
		if check.Decode(&raw) != nil {
			return errors.New("invalid Core connection field")
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid Core connection document")
	}
	return nil
}

func validSecret(secret string) bool {
	if len(secret) < 43 || len(secret) > 128 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(secret)
	return err == nil && len(b) >= 32
}

func validateBaseURL(raw string, localOnly bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n\t ") {
		return "", errors.New("invalid Core API URL")
	}
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if (u.Scheme != "https" && !(u.Scheme == "http" && loopback)) || (localOnly && !loopback) {
		return "", errors.New("Core API requires HTTPS or loopback HTTP")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/v1") || strings.Contains(u.Path, "//") {
		return "", errors.New("Core API URL must end in /v1")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return "", errors.New("invalid Core API path")
		}
	}
	return u.String(), nil
}

func ParseInvitation(data []byte, now time.Time) (Invitation, error) {
	var i Invitation
	if err := strictJSON(data, &i); err != nil {
		return i, err
	}
	if i.Schema != InvitationSchema || !coreIDPattern.MatchString(i.CoreID) || i.ProtocolMajor != ProtocolMajor || !validSecret(i.PairingToken) {
		return Invitation{}, errors.New("unsupported or invalid Core pairing invitation")
	}
	if !i.ExpiresAt.After(now) || i.ExpiresAt.After(now.Add(10*time.Minute+time.Second)) {
		return Invitation{}, errors.New("Core pairing invitation is expired or exceeds its lifetime")
	}
	u, err := validateBaseURL(i.BaseURL, false)
	if err != nil {
		return Invitation{}, err
	}
	i.BaseURL = u
	return i, nil
}

// Exchange consumes a one-time invitation and obtains an inference-only device
// key. Redirects are refused so the invitation cannot leak to another origin.
func Exchange(ctx context.Context, client *http.Client, invitation Invitation, clientName string) (PairedConnection, error) {
	data, err := json.Marshal(invitation)
	if err != nil {
		return PairedConnection{}, errors.New("invalid Core invitation")
	}
	i, err := ParseInvitation(data, time.Now())
	if err != nil {
		return PairedConnection{}, err
	}
	clientName = strings.TrimSpace(clientName)
	if clientName == "" || len(clientName) > 128 || strings.ContainsAny(clientName, "\r\n\x00") {
		return PairedConnection{}, errors.New("invalid Desktop device name")
	}
	requestBody, _ := json.Marshal(map[string]string{"pairing_token": i.PairingToken, "client_name": clientName})
	var wire struct {
		Schema        string   `json:"schema"`
		CoreID        string   `json:"core_id"`
		DeviceID      string   `json:"device_id"`
		BaseURL       string   `json:"base_url"`
		ProtocolMajor int      `json:"protocol_major"`
		Scopes        []string `json:"scopes"`
		APIKey        string   `json:"api_key"`
	}
	if err := requestJSON(ctx, client, http.MethodPost, i.BaseURL+"/pairing/redeem", requestBody, "", &wire); err != nil {
		return PairedConnection{}, err
	}
	url, err := validateBaseURL(wire.BaseURL, false)
	if err != nil || wire.Schema != "meadow.core.paired-connection.v1" || wire.CoreID != i.CoreID || url != i.BaseURL || wire.ProtocolMajor != ProtocolMajor || !deviceIDPattern.MatchString(wire.DeviceID) || !validSecret(wire.APIKey) || len(wire.Scopes) != 1 || wire.Scopes[0] != "inference" {
		return PairedConnection{}, errors.New("Core pairing response identity or scope mismatch")
	}
	return PairedConnection{wire.CoreID, wire.DeviceID, url, wire.ProtocolMajor, wire.Scopes, wire.APIKey}, nil
}

func requestJSON(ctx context.Context, client *http.Client, method, target string, body []byte, key string, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Core connection request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	cl := http.Client{Timeout: 15 * time.Second}
	if client != nil {
		cl = *client
	}
	if cl.Timeout == 0 {
		cl.Timeout = 15 * time.Second
	}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cl.Do(req)
	if err != nil {
		return errors.New("Core connection is unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Core connection rejected (HTTP %d)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil {
		return errors.New("Core connection response unavailable")
	}
	return strictJSON(data, result)
}

// Probe verifies persistent service identity using the scoped device credential.
// It neither loads a model nor sends a generation request.
func Probe(ctx context.Context, client *http.Client, p PairedConnection) error {
	base, err := validateBaseURL(p.BaseURL, false)
	if err != nil {
		return err
	}
	if !coreIDPattern.MatchString(p.CoreID) || !deviceIDPattern.MatchString(p.DeviceID) || p.ProtocolMajor != ProtocolMajor || !validSecret(p.APIKey) {
		return errors.New("invalid Core connection identity")
	}
	var identity struct {
		Schema        string   `json:"schema"`
		CoreID        string   `json:"core_id"`
		ProtocolMajor int      `json:"protocol_major"`
		InstanceID    string   `json:"instance_id"`
		Capabilities  []string `json:"capabilities"`
	}
	if err := requestJSON(ctx, client, http.MethodGet, base+"/connection", nil, p.APIKey, &identity); err != nil {
		return err
	}
	if identity.Schema != "meadow.core.service-identity.v1" || identity.CoreID != p.CoreID || identity.ProtocolMajor != ProtocolMajor {
		return errors.New("Core service identity or protocol mismatch")
	}
	return nil
}

func DefaultRegistryRoot() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Meadow", "core-connections"), nil
}

// DiscoverLocal reads only the explicit private registry; it never scans ports.
func DiscoverLocal(registryRoot string) ([]PairedConnection, error) {
	if _, err := os.Lstat(registryRoot); os.IsNotExist(err) {
		return []PairedConnection{}, nil
	}
	if err := privatePath(registryRoot, registryRoot, true); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(registryRoot)
	if err != nil {
		return nil, errors.New("Core registry is unavailable")
	}
	out := []PairedConnection{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".meadow-core.json") {
			p, err := LoadLocalDescriptor(registryRoot, entry.Name())
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	return out, nil
}

func LoadLocalDescriptor(registryRoot, path string) (PairedConnection, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(registryRoot, path)
	}
	data, err := readPrivate(registryRoot, path, maxDocument)
	if err != nil {
		return PairedConnection{}, err
	}
	var d struct {
		Schema        string `json:"schema"`
		CoreID        string `json:"core_id"`
		ProtocolMajor int    `json:"protocol_major"`
		BaseURL       string `json:"base_url"`
		DeviceID      string `json:"device_id"`
		CredentialRef string `json:"credential_ref"`
	}
	if err := strictJSON(data, &d); err != nil {
		return PairedConnection{}, err
	}
	u, err := validateBaseURL(d.BaseURL, true)
	if err != nil || d.Schema != LocalSchema || !coreIDPattern.MatchString(d.CoreID) || !deviceIDPattern.MatchString(d.DeviceID) || d.ProtocolMajor != ProtocolMajor || d.CredentialRef == "" || filepath.IsAbs(d.CredentialRef) {
		return PairedConnection{}, errors.New("invalid local Core descriptor")
	}
	key, err := readPrivate(registryRoot, filepath.Join(registryRoot, d.CredentialRef), 256)
	if err != nil {
		return PairedConnection{}, err
	}
	secret := strings.TrimSpace(string(key))
	if !validSecret(secret) {
		return PairedConnection{}, errors.New("invalid local Core credential")
	}
	return PairedConnection{d.CoreID, d.DeviceID, u, d.ProtocolMajor, []string{"inference"}, secret}, nil
}

func privatePath(root, path string, directory bool) error {
	if !filepath.IsAbs(root) {
		return errors.New("Core registry root must be absolute")
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("Core connection path escapes the registry")
	}
	current := root
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for i := -1; i < len(parts); i++ {
		if i >= 0 {
			current = filepath.Join(current, parts[i])
		}
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Core connection path must not contain symlinks")
		}
		wantDir := i < len(parts)-1 || directory
		if info.IsDir() != wantDir || (!wantDir && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 {
			return errors.New("Core registry and credentials must be private")
		}
		if err := validateOwner(info); err != nil {
			return err
		}
	}
	return nil
}

func readPrivate(root, path string, limit int64) ([]byte, error) {
	if err := privatePath(root, path, false); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("Core connection file unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("Core connection file unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("Core connection file changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("Core connection file exceeds limit")
	}
	// Verify the path after reading as well; parents may not be swapped to symlinks.
	if err := privatePath(root, path, false); err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) {
		return nil, errors.New("Core connection file changed")
	}
	return data, nil
}
