// Package modelconnection stores Desktop-owned model connections independently
// of optional Meadow Core lifecycle configuration. Catalogs contain no secrets.
package modelconnection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
)

const Version = 1

type Kind string

const (
	KindManagedAPI  Kind = "managed-api"
	KindProviderAPI Kind = "provider-api"
	KindLocalAPI    Kind = "local-api"
	KindMeadowCore  Kind = "meadow-core"
)

// Protocol selects the wire format independently of connection ownership.
// An omitted protocol retains the OpenAI-compatible contract of v1 catalogs.
type Protocol string

const (
	ProtocolOpenAICompatible Protocol = "openai-compatible"
	ProtocolAnthropic        Protocol = "anthropic"
)

// Zero limits and false capabilities mean that support has not been declared.
// Discovery does not guess capabilities from a model's name.
type Model struct {
	// Disabled retains a verified model in Settings while excluding it from
	// model selection and prompt admission. Old catalogs remain enabled.
	Disabled         bool   `json:"disabled,omitempty"`
	ID               string `json:"id"`
	DisplayName      string `json:"display_name,omitempty"`
	ContextTokens    int    `json:"context_tokens,omitempty"`
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
	SupportsImages   bool   `json:"supports_images,omitempty"`
	SupportsTools    bool   `json:"supports_tools,omitempty"`
	ReasoningControl string `json:"reasoning_control,omitempty"`
}

// Connection is safe to expose as metadata. Credentials are held separately,
// keyed by connection ID; neither a secret nor a secret file path is serialized.
type Connection struct {
	AccountRef string   `json:"account_ref,omitempty"`
	ID         string   `json:"id"`
	Kind       Kind     `json:"kind"`
	Protocol   Protocol `json:"protocol,omitempty"`
	Name       string   `json:"name"`
	BaseURL    string   `json:"base_url"`
	Models     []Model  `json:"models"`
}

func (c Connection) EffectiveProtocol() Protocol {
	if c.Protocol == "" {
		return ProtocolOpenAICompatible
	}
	return c.Protocol
}

// ValidateProtocol keeps managed authorization and Core lifecycle connections
// on their existing wire contract. Only user-configured APIs select Anthropic.
func ValidateProtocol(kind Kind, protocol Protocol) error {
	switch protocol {
	case "", ProtocolOpenAICompatible:
		return nil
	case ProtocolAnthropic:
		if kind == KindProviderAPI || kind == KindLocalAPI {
			return nil
		}
		return fmt.Errorf("Anthropic protocol requires a provider or local API connection")
	default:
		return fmt.Errorf("unsupported model connection protocol")
	}
}

type Catalog struct {
	Version              int          `json:"version"`
	SelectedConnectionID string       `json:"selected_connection_id"`
	SelectedModelID      string       `json:"selected_model_id"`
	Connections          []Connection `json:"connections"`
}

// DefaultPath is opt-in unless the caller has selected an application state
// home. A generic helper must never silently open another Desktop's profile.
func DefaultPath() string {
	if path := strings.TrimSpace(os.Getenv("MEADOW_MODEL_CONNECTIONS_FILE")); path != "" {
		return path
	}
	if home := strings.TrimSpace(os.Getenv("MEADOW_HOME")); home != "" {
		return filepath.Join(home, "model-connections-v1.json")
	}
	return ""
}

func LoadEnvironment() (*Catalog, error) {
	path := DefaultPath()
	if path == "" {
		return nil, nil
	}
	catalog, err := Load(path)
	// An explicit path is a contract; only an absent implicit catalog means that
	// the caller should continue its existing connection selection behavior.
	if os.IsNotExist(err) && strings.TrimSpace(os.Getenv("MEADOW_MODEL_CONNECTIONS_FILE")) == "" {
		return nil, nil
	}
	return catalog, err
}

func (c *Catalog) LookupConnection(id string) (Connection, bool) {
	if c != nil {
		for _, connection := range c.Connections {
			if connection.ID == id {
				return connection, true
			}
		}
	}
	return Connection{}, false
}

func (c Connection) LookupModel(id string) (Model, bool) {
	for _, model := range c.Models {
		if model.ID == id && !model.Disabled {
			return model, true
		}
	}
	return Model{}, false
}

// ServiceKey identifies a route, never a model name or a credential. Local
// loopback spelling and a trailing slash do not create a second service.
func (c Connection) ServiceKey() string {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if c.Kind == KindMeadowCore && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		host = "localhost"
	}
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	// Only literal trailing separators are equivalent. An escaped slash can
	// belong to a provider's route and must not collapse into another service.
	escapedPath := strings.TrimRight(u.EscapedPath(), "/")
	u.Path, _ = url.PathUnescape(escapedPath)
	u.RawPath = escapedPath
	return string(c.Kind) + "\n" + string(c.EffectiveProtocol()) + "\n" + c.AccountRef + "\n" + u.String()
}

// CoalesceServices repairs historical duplicate metadata. The selected row
// wins so its credential ID and session ownership stay stable. It never reads
// or copies credentials; persistence remains the caller's idle mutation.
func (c *Catalog) CoalesceServices() {
	byService := map[string]int{}
	rows := make([]Connection, 0, len(c.Connections))
	for _, connection := range c.Connections {
		key := connection.ServiceKey()
		if index, ok := byService[key]; ok {
			if connection.ID == c.SelectedConnectionID {
				rows[index] = connection
			}
			continue
		}
		byService[key] = len(rows)
		rows = append(rows, connection)
	}
	c.Connections = rows
}

func (c *Catalog) Resolve() (Connection, Model, error) {
	if err := Validate(c); err != nil {
		return Connection{}, Model{}, err
	}
	connection, ok := c.LookupConnection(c.SelectedConnectionID)
	if !ok {
		return Connection{}, Model{}, fmt.Errorf("no model connection is selected")
	}
	model, ok := connection.LookupModel(c.SelectedModelID)
	if !ok {
		return Connection{}, Model{}, fmt.Errorf("selected model is unavailable on its connection")
	}
	return connection, model, nil
}

// Fingerprint identifies public configuration, including selected connection
// and declared capabilities. It intentionally excludes credential material.
func (c *Catalog) Fingerprint() string {
	if Validate(c) != nil {
		return ""
	}
	data, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func Validate(c *Catalog) error {
	if c == nil || c.Version != Version {
		return fmt.Errorf("unsupported model connection catalog version")
	}
	if len(c.Connections) > 64 {
		return fmt.Errorf("too many model connections")
	}
	seen := make(map[string]bool, len(c.Connections))
	for _, connection := range c.Connections {
		if !validID(connection.ID) || seen[connection.ID] {
			return fmt.Errorf("invalid or duplicate connection ID")
		}
		seen[connection.ID] = true
		if connection.AccountRef != "" && (connection.Kind != KindManagedAPI || !validText(connection.AccountRef, 256) || strings.TrimSpace(connection.AccountRef) != connection.AccountRef) {
			return fmt.Errorf("managed account reference is invalid")
		}
		if !validText(connection.Name, 256) {
			return fmt.Errorf("connection name is invalid")
		}
		if err := ValidateBaseURL(connection.Kind, connection.BaseURL); err != nil {
			return err
		}
		if err := ValidateProtocol(connection.Kind, connection.Protocol); err != nil {
			return err
		}
		if len(connection.Models) > 512 {
			return fmt.Errorf("too many models on a connection")
		}
		models := make(map[string]bool, len(connection.Models))
		for _, model := range connection.Models {
			if !validText(model.ID, 512) || strings.TrimSpace(model.ID) != model.ID || models[model.ID] {
				return fmt.Errorf("invalid or duplicate model ID within connection")
			}
			models[model.ID] = true
			if model.ReasoningControl != "" && model.ReasoningControl != "openai-effort" && model.ReasoningControl != "inception-effort" {
				return fmt.Errorf("unsupported explicit reasoning control")
			}
			if model.DisplayName != "" && !validText(model.DisplayName, 512) {
				return fmt.Errorf("model display name is invalid")
			}
			if model.ContextTokens < 0 || model.MaxOutputTokens < 0 || (model.ContextTokens > 0 && model.MaxOutputTokens > model.ContextTokens) {
				return fmt.Errorf("model limits are invalid")
			}
		}
	}
	if (c.SelectedConnectionID == "") != (c.SelectedModelID == "") {
		return fmt.Errorf("selected connection and model must be set together")
	}
	if c.SelectedConnectionID != "" {
		connection, ok := c.LookupConnection(c.SelectedConnectionID)
		if !ok {
			return fmt.Errorf("selected connection does not exist")
		}
		if _, ok := connection.LookupModel(c.SelectedModelID); !ok {
			return fmt.Errorf("selected model is not on the selected connection")
		}
	}
	return nil
}

// ValidateBaseURL permits cleartext HTTP only for explicitly local connection
// kinds and literal private/loopback addresses or localhost. A hostname is not
// treated as LAN merely because DNS could resolve it to a private address.
func ValidateBaseURL(kind Kind, value string) error {
	if buildinfo.VMLab && kind != KindProviderAPI && kind != KindLocalAPI {
		return fmt.Errorf("VM lab only accepts provider-api or local-api connections")
	}
	switch kind {
	case KindManagedAPI, KindProviderAPI, KindLocalAPI, KindMeadowCore:
	default:
		return fmt.Errorf("unsupported model connection kind")
	}
	if !validText(value, 2048) || strings.TrimSpace(value) != value {
		return fmt.Errorf("invalid model connection URL")
	}
	u, err := url.Parse(value)
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || strings.Contains(u.Host, "%") || strings.ContainsAny(u.Path, "\\\r\n") {
		return fmt.Errorf("model connection URL must have no credentials, query or fragment")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("model connection requires HTTPS or explicit local HTTP")
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, " \t\r\n\\") {
		return fmt.Errorf("invalid model connection host")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("invalid model connection port")
		}
	}
	if u.Scheme == "http" {
		if kind != KindLocalAPI && kind != KindMeadowCore {
			return fmt.Errorf("managed and provider API connections require HTTPS")
		}
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || (!ip.IsLoopback() && !ip.IsPrivate())) {
			return fmt.Errorf("local HTTP requires a loopback or private LAN address")
		}
	}
	return nil
}

func validText(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return id != "." && id != ".."
}

// ManagedConnectionID binds a cloud route to the server-issued stable opaque account reference.
// It is safe metadata; neither the raw user identity nor provider credentials enter model IDs.
func ManagedConnectionID(accountRef, baseURL string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(accountRef) + "\n" + strings.TrimRight(strings.TrimSpace(baseURL), "/")))
	return "meadow-managed-" + hex.EncodeToString(digest[:])
}
