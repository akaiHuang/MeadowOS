// Package config owns Meadow's public config.yaml contract.
//
// The native runtime keeps a loose raw tree so keys implemented by the
// compatibility runtime survive Go reads and writes, while typed accessors
// expose only the settings the Go core currently consumes.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const FileName = "config.yaml"

// Document is one config.yaml file. Raw always remains the source of truth so
// unknown and not-yet-ported keys round-trip unchanged.
type Document struct {
	Raw    map[string]any
	Path   string
	Exists bool

	userEnv      map[string]string
	bootstrapEnv map[string]string
	managedRaw   map[string]any
	managedEnv   map[string]string
	secretEnv    map[string]string
	secretSource map[string]string
	managedDir   string
	managedKeys  map[string]struct{}
	warnings     []string
}

// ModelConfig is the typed model route shared by CLI, daemon, and desktop.
type ModelConfig struct {
	Provider          string
	Model             string
	BaseURL           string
	APIKey            string
	KeyEnv            string
	SessionTokenLimit int
}

// LoadFromDirs loads the first config.yaml in the preferred home chain.
// Missing config is valid and returns an empty document targeting dirs[0].
func LoadFromDirs(dirs []string) (*Document, error) {
	return loadFromDirs(dirs, true)
}

// LoadFromDirsForEdit loads the same public config and managed boundaries as
// LoadFromDirs but does not contact enabled external secret sources. It is the
// safe entrypoint for status and configuration-only CLI commands.
func LoadFromDirsForEdit(dirs []string) (*Document, error) {
	return loadFromDirs(dirs, false)
}

func loadFromDirs(dirs []string, resolveSecrets bool) (*Document, error) {
	var target string
	var document *Document
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		path := filepath.Join(dir, FileName)
		if target == "" {
			target = path
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			var loadErr error
			document, loadErr = loadPath(path)
			if loadErr != nil {
				return nil, loadErr
			}
			break
		}
	}
	if document == nil {
		document = &Document{Raw: map[string]any{}, Path: target}
	}
	if err := document.loadEnvironmentAndManagedScope(); err != nil {
		return nil, err
	}
	if resolveSecrets {
		document.resolveExternalSecrets()
	}
	return document, nil
}

func loadPath(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			backup, backupErr := backupCorrupt(path, data)
			if backupErr != nil {
				return nil, fmt.Errorf(
					"parse %s: %w (corrupt backup also failed: %v)",
					path, err, backupErr,
				)
			}
			return nil, fmt.Errorf("parse %s: %w (backup: %s)", path, err, backup)
		}
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return &Document{Raw: raw, Path: path, Exists: true}, nil
}

func backupCorrupt(path string, data []byte) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	backup := path + ".corrupt." + stamp
	file, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(backup)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	ok = true
	return backup, nil
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return ""
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		value, _ := strconv.Atoi(strings.TrimSpace(typed))
		return value
	default:
		return 0
	}
}

func mapValue(value any) map[string]any {
	typed, _ := value.(map[string]any)
	return typed
}

func lookup(root map[string]any, path ...string) any {
	var current any = root
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

// Object returns an object from the effective config tree. Administrator
// managed values win at the leaf while unrelated user-owned siblings survive.
func (d *Document) Object(path ...string) map[string]any {
	if d == nil {
		return nil
	}
	return mapValue(lookup(d.effectiveRaw(), path...))
}

// ModelRouteConfigured reports whether the current Meadow config, including a
// managed overlay, owns the model route. Callers use this to avoid blending a
// legacy cloud credential into a new local route.
func (d *Document) ModelRouteConfigured() bool {
	if d == nil {
		return false
	}
	switch model := d.effectiveRaw()["model"].(type) {
	case string:
		return strings.TrimSpace(model) != ""
	case map[string]any:
		for _, key := range []string{
			"provider",
			"default",
			"model",
			"name",
			"base_url",
			"baseUrl",
			"api_base",
			"api_key",
			"apiKey",
			"key_env",
			"api_key_env",
		} {
			if !isEmptyConfigValue(model[key]) {
				return true
			}
		}
	}
	return false
}

// EnvValue resolves one explicit env reference without mutating os.Environ.
// Administrator-pinned values win, then the process environment, then the
// user's ~/.meadow/.env compatibility file.
func (d *Document) EnvValue(key string) string {
	value, _ := d.lookupEnvValue(key)
	return value
}

func (d *Document) lookupEnvValue(key string) (string, bool) {
	if d == nil {
		return "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	if value, exists := d.managedEnv[key]; exists {
		return strings.TrimSpace(value), true
	}
	if value, exists := d.secretEnv[key]; exists {
		return strings.TrimSpace(value), true
	}
	if value, exists := os.LookupEnv(key); exists {
		return strings.TrimSpace(value), true
	}
	if value, exists := d.userEnv[key]; exists {
		return strings.TrimSpace(value), true
	}
	value, exists := d.bootstrapEnv[key]
	return strings.TrimSpace(value), exists
}

// ManagedDir returns the active administrator scope, or an empty string.
func (d *Document) ManagedDir() string {
	if d == nil {
		return ""
	}
	return d.managedDir
}

// ManagedConfigKeys returns the sorted effective leaf paths owned by the
// administrator scope.
func (d *Document) ManagedConfigKeys() []string {
	if d == nil {
		return nil
	}
	return sortedKeys(d.managedKeys)
}

// ManagedEnvKeys returns the sorted environment names owned by the
// administrator scope.
func (d *Document) ManagedEnvKeys() []string {
	if d == nil {
		return nil
	}
	keys := make(map[string]struct{}, len(d.managedEnv))
	for key := range d.managedEnv {
		keys[key] = struct{}{}
	}
	return sortedKeys(keys)
}

// SecretSource returns the external source that supplied one effective
// variable. It never returns the secret value.
func (d *Document) SecretSource(key string) (string, bool) {
	if d == nil {
		return "", false
	}
	source, exists := d.secretSource[strings.TrimSpace(key)]
	return source, exists
}

// ResolvedSecretKeys returns names resolved by external sources without
// exposing their values.
func (d *Document) ResolvedSecretKeys() []string {
	if d == nil {
		return nil
	}
	keys := make(map[string]struct{}, len(d.secretEnv))
	for key := range d.secretEnv {
		keys[key] = struct{}{}
	}
	return sortedKeys(keys)
}

// Warnings returns non-fatal managed-scope and .env parse warnings.
func (d *Document) Warnings() []string {
	if d == nil {
		return nil
	}
	return append([]string(nil), d.warnings...)
}

// Model resolves both the historical scalar model key and the current typed
// model/providers shape used by Meadow CLI.
func (d *Document) ModelConfig() ModelConfig {
	var result ModelConfig
	if d == nil {
		return result
	}
	effective := d.effectiveRaw()
	switch model := effective["model"].(type) {
	case string:
		result.Model = strings.TrimSpace(model)
	case map[string]any:
		result.Provider = stringValue(model["provider"])
		result.Model = firstString(model, "default", "model", "name")
		result.BaseURL = firstString(model, "base_url", "baseUrl", "api_base")
		result.APIKey = firstString(model, "api_key", "apiKey")
		result.KeyEnv = firstString(model, "key_env", "api_key_env")
		result.SessionTokenLimit = firstInt(
			model,
			"session_token_limit",
			"sessionTokenLimit",
		)
	}

	if result.Provider != "" {
		if providers := mapValue(effective["providers"]); providers != nil {
			if entry := mapValue(providers[result.Provider]); entry != nil {
				fillModelFromProvider(&result, entry)
			}
		}
	}
	if result.APIKey == "" && result.KeyEnv != "" {
		result.APIKey = d.EnvValue(result.KeyEnv)
	}
	return result
}

func fillModelFromProvider(model *ModelConfig, provider map[string]any) {
	if model.BaseURL == "" {
		model.BaseURL = firstString(provider, "base_url", "baseUrl", "api", "url")
	}
	if model.APIKey == "" {
		model.APIKey = firstString(provider, "api_key", "apiKey")
	}
	if model.KeyEnv == "" {
		model.KeyEnv = firstString(provider, "key_env", "api_key_env")
	}
	if model.Model == "" {
		model.Model = firstString(provider, "default_model", "model", "default")
	}
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstInt(values map[string]any, keys ...string) int {
	for _, key := range keys {
		if value := intValue(values[key]); value != 0 {
			return value
		}
	}
	return 0
}

// ApprovalMode translates the public config into the native engine's current
// approval vocabulary. A Meadow-specific exact value wins over compatibility
// approximations of the Python modes.
func (d *Document) ApprovalMode() string {
	if d == nil {
		return ""
	}
	effective := d.effectiveRaw()
	if value := stringValue(lookup(effective, "meadow", "approval_mode")); value != "" {
		return value
	}
	switch strings.ToLower(stringValue(lookup(effective, "approvals", "mode"))) {
	case "off":
		return "yolo"
	case "manual", "smart":
		return "default"
	default:
		return ""
	}
}

func (d *Document) Theme() string {
	if d == nil {
		return ""
	}
	effective := d.effectiveRaw()
	if value := stringValue(lookup(effective, "display", "skin")); value != "" {
		return value
	}
	return stringValue(lookup(effective, "ui", "theme"))
}

func (d *Document) TerminalCWD() string {
	if d == nil {
		return ""
	}
	return stringValue(lookup(d.effectiveRaw(), "terminal", "cwd"))
}

func (d *Document) MaxTurns() int {
	if d == nil {
		return 0
	}
	return intValue(lookup(d.effectiveRaw(), "agent", "max_turns"))
}

// Set stores one value without replacing sibling or unknown keys.
func (d *Document) Set(value any, path ...string) error {
	if d == nil || len(path) == 0 {
		return errors.New("config path is required")
	}
	if d.isManagedPath(path) {
		return fmt.Errorf(
			"config key %s is managed by the administrator scope %s",
			strings.Join(path, "."),
			d.managedDir,
		)
	}
	if d.Raw == nil {
		d.Raw = map[string]any{}
	}
	current := d.Raw
	for _, key := range path[:len(path)-1] {
		next := mapValue(current[key])
		if next == nil {
			next = map[string]any{}
			current[key] = next
		}
		current = next
	}
	current[path[len(path)-1]] = value
	return nil
}

// Delete removes one user-owned key while preserving unknown siblings. Empty
// parent objects are retained because they may be meaningful to older
// runtimes. Administrator-managed paths cannot be deleted.
func (d *Document) Delete(path ...string) error {
	if d == nil || len(path) == 0 {
		return errors.New("config path is required")
	}
	if d.isManagedPath(path) {
		return fmt.Errorf(
			"config key %s is managed by the administrator scope %s",
			strings.Join(path, "."),
			d.managedDir,
		)
	}
	current := d.Raw
	for _, key := range path[:len(path)-1] {
		current = mapValue(current[key])
		if current == nil {
			return nil
		}
	}
	delete(current, path[len(path)-1])
	return nil
}

// Save atomically replaces config.yaml with owner-only permissions.
func (d *Document) Save() error {
	if d == nil || strings.TrimSpace(d.Path) == "" {
		return errors.New("config path is unavailable")
	}
	if err := requireReadableConfigBeforeWrite(d.Path); err != nil {
		return err
	}
	dir := filepath.Dir(d.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	rawForSave := cloneMap(d.Raw)
	stripManagedLeaves(rawForSave, d.managedKeys)
	data, err := yaml.Marshal(rawForSave)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(d.Path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, d.Path); err != nil {
		return err
	}
	if err := os.Chmod(d.Path, 0o600); err != nil {
		return err
	}
	committed = true
	d.Raw = rawForSave
	d.Exists = true
	return nil
}

func requireReadableConfigBeforeWrite(path string) error {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf(
			"refusing to overwrite %s: existing config cannot be accessed: %w",
			path,
			err,
		)
	case info.IsDir():
		return fmt.Errorf(
			"refusing to overwrite %s: existing config path is a directory",
			path,
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf(
			"refusing to overwrite %s: existing config cannot be read: %w",
			path,
			err,
		)
	}
	defer file.Close()
	var probe [1]byte
	if _, err := file.Read(probe[:]); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf(
			"refusing to overwrite %s: existing config cannot be read: %w",
			path,
			err,
		)
	}
	return nil
}
