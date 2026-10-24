package secrets

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CachedFetch is one complete source result and its wall-clock fetch time.
type CachedFetch struct {
	Secrets   map[string]string
	FetchedAt float64
}

func (entry CachedFetch) Fresh(ttlSeconds float64, now time.Time) bool {
	if ttlSeconds <= 0 {
		return false
	}
	return float64(now.UnixNano())/1e9-entry.FetchedAt < ttlSeconds
}

// MemoryCache is the process-local L1. Values are copied at its boundary so a
// caller cannot mutate a cached result shared by another config document.
type MemoryCache struct {
	mu      sync.RWMutex
	entries map[string]CachedFetch
}

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: map[string]CachedFetch{}}
}

func (cache *MemoryCache) Get(
	key string,
	ttlSeconds float64,
	now time.Time,
) (CachedFetch, bool) {
	if cache == nil || ttlSeconds <= 0 {
		return CachedFetch{}, false
	}
	cache.mu.RLock()
	entry, exists := cache.entries[key]
	cache.mu.RUnlock()
	if !exists || !entry.Fresh(ttlSeconds, now) {
		return CachedFetch{}, false
	}
	entry.Secrets = cloneSecrets(entry.Secrets)
	return entry, true
}

func (cache *MemoryCache) Put(key string, entry CachedFetch) {
	if cache == nil {
		return
	}
	entry.Secrets = cloneSecrets(entry.Secrets)
	cache.mu.Lock()
	cache.entries[key] = entry
	cache.mu.Unlock()
}

func (cache *MemoryCache) Clear() {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	cache.entries = map[string]CachedFetch{}
	cache.mu.Unlock()
}

// DiskCache is the best-effort, profile-scoped L2. The serialized key must
// contain only fingerprints, never raw authentication material.
type DiskCache struct {
	BaseName string
}

func (cache DiskCache) Path(home string) string {
	return filepath.Join(resolveCacheHome(home), "cache", cache.BaseName)
}

func resolveCacheHome(home string) string {
	if home != "" {
		return home
	}
	if configured := os.Getenv("MEADOW_HOME"); configured != "" {
		return configured
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ".meadow"
	}
	return filepath.Join(userHome, ".meadow")
}

func (cache DiskCache) Read(
	key string,
	ttlSeconds float64,
	home string,
	now time.Time,
) (CachedFetch, bool) {
	if ttlSeconds <= 0 || cache.BaseName == "" {
		return CachedFetch{}, false
	}
	data, err := os.ReadFile(cache.Path(home))
	if err != nil {
		return CachedFetch{}, false
	}
	var payload struct {
		Key       string         `json:"key"`
		Secrets   map[string]any `json:"secrets"`
		FetchedAt any            `json:"fetched_at"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Key != key {
		return CachedFetch{}, false
	}
	if payload.Secrets == nil {
		return CachedFetch{}, false
	}
	fetchedAt, ok := numericTimestamp(payload.FetchedAt)
	if !ok {
		return CachedFetch{}, false
	}
	secrets := map[string]string{}
	for name, value := range payload.Secrets {
		typed, ok := value.(string)
		if ok {
			secrets[name] = typed
		}
	}
	entry := CachedFetch{Secrets: secrets, FetchedAt: fetchedAt}
	if !entry.Fresh(ttlSeconds, now) {
		return CachedFetch{}, false
	}
	return entry, true
}

func numericTimestamp(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

func (cache DiskCache) Write(
	key string,
	entry CachedFetch,
	ttlSeconds float64,
	home string,
) error {
	if ttlSeconds <= 0 || cache.BaseName == "" {
		return nil
	}
	path := cache.Path(home)
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(directory, 0o700)
	payload := struct {
		Key       string            `json:"key"`
		Secrets   map[string]string `json:"secrets"`
		FetchedAt float64           `json:"fetched_at"`
	}{
		Key:       key,
		Secrets:   cloneSecrets(entry.Secrets),
		FetchedAt: entry.FetchedAt,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(
		directory,
		"."+cacheStem(cache.BaseName)+"-*.tmp",
	)
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
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
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	keep = true
	if err := syncDirectory(directory); err != nil {
		return err
	}
	return nil
}

func cacheStem(baseName string) string {
	extension := filepath.Ext(baseName)
	if extension == "" {
		return baseName
	}
	return baseName[:len(baseName)-len(extension)]
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync cache directory: %w", err)
	}
	return nil
}

func (cache DiskCache) Clear(home string) error {
	err := os.Remove(cache.Path(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func cloneSecrets(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}

func fingerprint(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}
