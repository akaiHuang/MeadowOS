// Package searchconnection reads an explicitly selected Desktop profile's
// private local Search connection. It never installs or starts a service and
// does not confer a paid module entitlement.
package searchconnection

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Schema = "meadow.search-connection.v1"
const FileName = "search-connection-v1.json"
const maxDescriptorBytes = 16 << 10
const maxTokenBytes = 8192

// Connection exposes metadata only. Token and actor remain native host state;
// ordinary formatting and JSON serialization cannot disclose either value.
type Connection struct {
	BaseURL        string `json:"base_url"`
	InstallationID string `json:"installation_id"`
	token          string
	actor          string
}

func (c *Connection) Token() string {
	if c == nil {
		return ""
	}
	return c.token
}
func (c *Connection) ActorID() string {
	if c == nil {
		return ""
	}
	return c.actor
}
func (c Connection) String() string   { return "Search connection (private native credentials)" }
func (c Connection) GoString() string { return c.String() }

// ConfiguredEnvironment detects presence, not readiness. A present but invalid
// file remains configured so callers fail closed instead of selecting another
// search service. No HOME, QWEN_HOME, global registry or legacy fallback exists.
func ConfiguredEnvironment() bool {
	profile := strings.TrimSpace(os.Getenv("MEADOW_HOME"))
	if profile == "" {
		return false
	}
	if !filepath.IsAbs(profile) {
		return true
	}
	_, err := os.Lstat(filepath.Join(profile, FileName))
	return !os.IsNotExist(err)
}

func LoadEnvironment() (*Connection, error) {
	return Load(strings.TrimSpace(os.Getenv("MEADOW_HOME")))
}

// Load returns nil when no profile or descriptor was selected. An explicitly
// present but unsafe or malformed descriptor never falls back to another file.
func Load(profile string) (*Connection, error) {
	if profile == "" {
		return nil, nil
	}
	if !filepath.IsAbs(profile) {
		return nil, errors.New("Search profile must be absolute")
	}
	profile = filepath.Clean(profile)
	path := filepath.Join(profile, FileName)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, errors.New("Search connection is unavailable")
	}
	data, err := readPrivate(profile, path, maxDescriptorBytes)
	if err != nil {
		return nil, err
	}
	descriptor, err := decodeDescriptor(data)
	if err != nil {
		return nil, err
	}
	if descriptor["schema"] != Schema || !validID(descriptor["installation_id"]) {
		return nil, errors.New("Search connection schema or installation identity is invalid")
	}
	baseURL, err := validateURL(descriptor["base_url"])
	if err != nil {
		return nil, err
	}
	ref := descriptor["credential_ref"]
	if ref != "search/"+descriptor["installation_id"]+".key" {
		return nil, errors.New("Search credential reference must name its profile installation key")
	}
	keyBytes, err := readPrivate(profile, filepath.Join(profile, filepath.FromSlash(ref)), maxTokenBytes+1)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSuffix(string(keyBytes), "\n")
	if token == "" || len(token) > maxTokenBytes || !utf8.ValidString(token) {
		return nil, errors.New("Search credential is invalid")
	}
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return nil, errors.New("Search credential is invalid")
		}
	}
	canonical, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return nil, errors.New("Search profile is unavailable")
	}
	// Installation identity is part of the principal. Replacing a local service
	// at the same port cannot reuse another installation's cached RAG receipts.
	digest := sha256.Sum256([]byte(Schema + "\x00" + canonical + "\x00" + descriptor["installation_id"]))
	return &Connection{BaseURL: baseURL, InstallationID: descriptor["installation_id"], token: token, actor: "meadow-" + hex.EncodeToString(digest[:])}, nil
}

func decodeDescriptor(data []byte) (map[string]string, error) {
	invalid := errors.New("Search connection descriptor is invalid")
	if !utf8.Valid(data) {
		return nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, invalid
	}
	fields := make(map[string]string, 4)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, invalid
		}
		name, ok := key.(string)
		if !ok {
			return nil, invalid
		}
		switch name {
		case "schema", "installation_id", "base_url", "credential_ref":
		default:
			return nil, invalid
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, invalid
		}
		var value string
		if err := decoder.Decode(&value); err != nil || value == "" {
			return nil, invalid
		}
		fields[name] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(fields) != 4 {
		return nil, invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, invalid
	}
	return fields, nil
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validateURL(value string) (string, error) {
	invalid := errors.New("Search profile requires a loopback HTTP service URL")
	if len(value) > 2048 || strings.TrimSpace(value) != value {
		return "", invalid
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" {
		return "", invalid
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", invalid
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", invalid
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n\t ") {
			return "", invalid
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func privatePath(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("Search connection path escapes its profile")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	current := root
	for i := -1; i < len(parts); i++ {
		if i >= 0 {
			current = filepath.Join(current, parts[i])
		}
		info, err := os.Lstat(current)
		if err != nil {
			return errors.New("Search connection file is unavailable")
		}
		wantDir := i < len(parts)-1
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != wantDir || (!wantDir && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 {
			return errors.New("Search profile and connection files must be private without symlinks")
		}
		if err := validateOwner(info); err != nil {
			return err
		}
	}
	return nil
}

func readPrivate(root, path string, limit int64) ([]byte, error) {
	if err := privatePath(root, path); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil || before.Size() > limit {
		return nil, errors.New("Search connection file exceeds its read limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("Search connection file is unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || opened.Size() > limit {
		return nil, errors.New("Search connection file changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("Search connection file exceeds its read limit")
	}
	if err := privatePath(root, path); err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) {
		return nil, errors.New("Search connection file changed")
	}
	return data, nil
}
