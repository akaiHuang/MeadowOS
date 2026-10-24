package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMapTileURL   = "https://tile.openstreetmap.org/{z}/{x}/{y}.png"
	MapTileMaxZoom      = 18
	mapTileMaxBytes     = 512 << 10
	mapTileCacheBytes   = 128 << 20
	mapTileCacheEntries = 4096
	mapTileFallbackTTL  = 7 * 24 * time.Hour
	mapTileUserAgent    = "MeadowDesktop/1.0"
)

// MapTileConfig is host configuration, never a model tool argument. A custom
// template can contain a provider key; it is never returned to a client.
type MapTileConfig struct {
	memberProxy    *memberMapTileProxy
	Provider       string
	MapboxToken    string
	MapboxStyle    string
	Template       string
	Attribution    string
	AttributionURL string
	ContactURL     string
	CacheDirectory string
}

type MapTilePublicConfig struct {
	Provider       string `json:"provider"`
	TileRevision   string `json:"tileRevision"`
	TileSize       int    `json:"tileSize"`
	ZoomOffset     int    `json:"zoomOffset"`
	Attribution    string `json:"attribution"`
	AttributionURL string `json:"attributionURL,omitempty"`
	BrowserTileURL string `json:"browserTileURL,omitempty"`
	MaxZoom        int    `json:"maxZoom"`
}

type MapTile struct {
	Data        []byte
	ContentType string
	ExpiresAt   time.Time
	NoStore     bool
}

type mapTileMetadata struct {
	ContentType  string    `json:"contentType"`
	ExpiresAt    time.Time `json:"expiresAt"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"lastModified,omitempty"`
	CacheControl string    `json:"cacheControl,omitempty"`
	Expires      string    `json:"expires,omitempty"`
}

// MapTileService serves only on-demand tiles for the host's fixed provider. Its
// disk cache contains public tile bytes, never sessions, prompts or locations.
// It deliberately has no preload, bulk-download or arbitrary-URL operation.
type MapTileService struct {
	config   MapTileConfig
	client   *http.Client
	mu       sync.Mutex
	inflight map[string]chan struct{}
	capacity chan struct{}
	diskMu   sync.Mutex
	now      func() time.Time
}

func NewMapTileService(config MapTileConfig) (*MapTileService, error) {
	var err error
	if config.memberProxy == nil {
		config, err = configureMapProvider(config)
	} else if config.Template != memberMapTileURL || config.Provider != "mapbox" {
		err = errors.New("invalid member map proxy")
	}
	if err != nil {
		return nil, err
	}
	config.Template = strings.TrimSpace(config.Template)
	if config.Template == "" {
		config.Template = DefaultMapTileURL
	}
	if config.Template == DefaultMapTileURL {
		if config.Attribution == "" {
			config.Attribution = "© OpenStreetMap contributors"
		}
		if config.AttributionURL == "" {
			config.AttributionURL = "https://www.openstreetmap.org/copyright"
		}
	} else if strings.TrimSpace(config.Attribution) == "" {
		return nil, errors.New("custom map provider requires attribution")
	}
	if len(config.Template) > 4096 || len(config.Attribution) > 512 {
		return nil, errors.New("invalid map provider configuration")
	}
	for _, token := range []string{"{z}", "{x}", "{y}"} {
		if strings.Count(config.Template, token) != 1 {
			return nil, errors.New("invalid map tile template")
		}
	}
	sample := strings.NewReplacer("{z}", "0", "{x}", "0", "{y}", "0").Replace(config.Template)
	u, err := url.Parse(sample)
	if err != nil || (u.Scheme != "https" && config.memberProxy == nil) || u.Host == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(sample, "{}") {
		return nil, errors.New("map tile template requires HTTPS")
	}
	// Placeholders must be in the path/query, never in the destination host.
	raw, err := url.Parse(config.Template)
	if err != nil || raw.Host != u.Host {
		return nil, errors.New("invalid map tile host")
	}
	if config.AttributionURL != "" {
		link, err := url.Parse(config.AttributionURL)
		if err != nil || link.Scheme != "https" || link.Host == "" || link.User != nil || link.RawQuery != "" || link.Fragment != "" || len(config.AttributionURL) > 1024 {
			return nil, errors.New("invalid map attribution link")
		}
	}
	if config.ContactURL != "" {
		contact, err := url.Parse(config.ContactURL)
		if err != nil || contact.Scheme != "https" || contact.Host == "" || contact.User != nil || contact.RawQuery != "" || contact.Fragment != "" || len(config.ContactURL) > 1024 || strings.ContainsAny(config.ContactURL, "\r\n() ") {
			return nil, errors.New("invalid map contact link")
		}
	}
	if config.CacheDirectory == "" || !filepath.IsAbs(config.CacheDirectory) {
		return nil, errors.New("map cache requires an absolute state directory")
	}
	if err := os.MkdirAll(config.CacheDirectory, 0700); err != nil {
		return nil, errors.New("map cache unavailable")
	}
	client := guardPublicHTTPClient(&http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("map tile redirects unavailable") }}, false, nil)
	if config.memberProxy != nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		client = &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &MapTileService{config: config, client: client, inflight: map[string]chan struct{}{}, capacity: make(chan struct{}, 4), now: time.Now}, nil
}

func (s *MapTileService) Config() MapTilePublicConfig {
	revision := sha256.Sum256([]byte("map-tiles-v2\n" + s.config.Template))
	config := MapTilePublicConfig{Provider: s.config.Provider, TileRevision: hex.EncodeToString(revision[:8]), TileSize: 256, Attribution: s.config.Attribution, AttributionURL: s.config.AttributionURL, MaxZoom: MapTileMaxZoom}
	if s.config.Provider == "mapbox" {
		config.TileSize = 512
		config.ZoomOffset = -1
	}
	// Web pages use direct OSM requests so their origin-only Referer and normal
	// browser cache meet the provider policy. Native custom-scheme clients use
	// our identified, authenticated proxy. Never expose custom provider secrets.
	if s.config.Template == DefaultMapTileURL {
		config.BrowserTileURL = DefaultMapTileURL
	}
	return config
}

func ValidMapTileCoordinates(z, x, y int) bool {
	return z >= 0 && z <= MapTileMaxZoom && x >= 0 && y >= 0 && x < 1<<z && y < 1<<z
}

func (s *MapTileService) Fetch(ctx context.Context, z, x, y int) (MapTile, error) {
	if !ValidMapTileCoordinates(z, x, y) {
		return MapTile{}, errors.New("invalid map tile coordinates")
	}
	if err := ctx.Err(); err != nil {
		return MapTile{}, err
	}
	keyHash := sha256.Sum256([]byte(fmt.Sprintf("%s\n%d/%d/%d", s.config.Template, z, x, y)))
	key := hex.EncodeToString(keyHash[:]) + ".tile"
	for {
		s.mu.Lock()
		if done, ok := s.inflight[key]; ok {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return MapTile{}, ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		s.inflight[key] = done
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.inflight, key); close(done); s.mu.Unlock() }()
		break
	}
	metadata, data, ok := s.readCached(key)
	if ok && s.now().Before(metadata.ExpiresAt) {
		return MapTile{Data: data, ContentType: metadata.ContentType, ExpiresAt: metadata.ExpiresAt}, nil
	}
	select {
	case s.capacity <- struct{}{}:
		defer func() { <-s.capacity }()
	case <-ctx.Done():
		return MapTile{}, ctx.Err()
	}
	target := strings.NewReplacer("{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x), "{y}", strconv.Itoa(y)).Replace(s.config.Template)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return MapTile{}, errors.New("map provider unavailable")
	}
	if s.config.memberProxy != nil {
		req.Header.Set("Authorization", "Bearer "+s.config.memberProxy.token)
	}
	req.Header.Set("User-Agent", mapTileUserAgent)
	if s.config.ContactURL != "" {
		req.Header.Set("User-Agent", mapTileUserAgent+" (+"+s.config.ContactURL+")")
	}
	req.Header.Set("Accept", "image/png,image/jpeg")
	if ok {
		if metadata.ETag != "" {
			req.Header.Set("If-None-Match", metadata.ETag)
		}
		if metadata.LastModified != "" {
			req.Header.Set("If-Modified-Since", metadata.LastModified)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return MapTile{}, ctx.Err()
		}
		return MapTile{}, errors.New("map provider unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && !(resp.StatusCode == http.StatusNotModified && ok) {
		return MapTile{}, errors.New("map provider unavailable")
	}
	if resp.StatusCode == http.StatusOK {
		contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil || (contentType != "image/png" && contentType != "image/jpeg") {
			return MapTile{}, errors.New("invalid map tile image")
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, mapTileMaxBytes+1))
		if ctx.Err() != nil {
			return MapTile{}, ctx.Err()
		}
		if err != nil || !validMapTileImage(data, contentType) {
			return MapTile{}, errors.New("invalid map tile image")
		}
		metadata = mapTileMetadata{ContentType: contentType}
	}
	if value := resp.Header.Get("ETag"); value != "" && len(value) < 512 {
		metadata.ETag = value
	}
	if value := resp.Header.Get("Last-Modified"); value != "" && len(value) < 128 {
		metadata.LastModified = value
	}
	// 304 updates only supplied policy fields. In particular, it must never
	// replace a cached no-cache/max-age policy with the seven-day fallback.
	if values, present := resp.Header["Cache-Control"]; present {
		metadata.CacheControl = strings.Join(values, ",")
	}
	if values, present := resp.Header["Expires"]; present {
		metadata.Expires = strings.Join(values, ",")
	}
	cacheHeaders := resp.Header.Clone()
	cacheHeaders.Set("Cache-Control", metadata.CacheControl)
	cacheHeaders.Set("Expires", metadata.Expires)
	var canStore bool
	metadata.ExpiresAt, canStore = mapTileExpiration(cacheHeaders, s.now())
	if canStore {
		if err := s.writeCached(key, metadata, data); err != nil {
			return MapTile{}, errors.New("map cache unavailable")
		}
	} else {
		s.removeCached(key)
	}
	return MapTile{Data: data, ContentType: metadata.ContentType, ExpiresAt: metadata.ExpiresAt, NoStore: !canStore}, nil
}

func validMapTileImage(data []byte, contentType string) bool {
	if len(data) == 0 || len(data) > mapTileMaxBytes {
		return false
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != contentType || config.Width < 1 || config.Height < 1 || config.Width > 1024 || config.Height > 1024 {
		return false
	}
	_, _, err = image.Decode(bytes.NewReader(data))
	return err == nil
}

func mapTileExpiration(header http.Header, now time.Time) (time.Time, bool) {
	lifetime := mapTileFallbackTTL
	explicit := false
	noCache := false
	for _, part := range strings.Split(header.Get("Cache-Control"), ",") {
		pair := strings.SplitN(strings.ToLower(strings.TrimSpace(part)), "=", 2)
		switch pair[0] {
		case "no-store", "private":
			return now, false
		case "no-cache":
			noCache = true
		case "max-age":
			if len(pair) == 2 {
				if seconds, err := strconv.ParseInt(strings.Trim(pair[1], `"`), 10, 64); err == nil && seconds >= 0 && seconds <= int64((10*365*24*time.Hour)/time.Second) {
					lifetime = time.Duration(seconds) * time.Second
					explicit = true
				}
			}
		}
	}
	if !explicit {
		if expires, err := http.ParseTime(header.Get("Expires")); err == nil {
			date, err := http.ParseTime(header.Get("Date"))
			if err != nil {
				date = now
			}
			lifetime = expires.Sub(date)
		}
	}
	var currentAge time.Duration
	if date, err := http.ParseTime(header.Get("Date")); err == nil && date.Before(now) {
		currentAge = now.Sub(date)
	}
	if age, err := strconv.ParseInt(header.Get("Age"), 10, 64); err == nil && age >= 0 && age <= int64((10*365*24*time.Hour)/time.Second) {
		if ageDuration := time.Duration(age) * time.Second; ageDuration > currentAge {
			currentAge = ageDuration
		}
	}
	lifetime -= currentAge
	if lifetime < 0 || noCache {
		lifetime = 0
	}
	return now.Add(lifetime), true
}

func (s *MapTileService) readCached(key string) (mapTileMetadata, []byte, bool) {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	root, err := os.OpenRoot(s.config.CacheDirectory)
	if err != nil {
		return mapTileMetadata{}, nil, false
	}
	defer root.Close()
	file, err := root.Open(key)
	if err != nil {
		return mapTileMetadata{}, nil, false
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > mapTileMaxBytes+4096 {
		return mapTileMetadata{}, nil, false
	}
	data, err := io.ReadAll(io.LimitReader(file, mapTileMaxBytes+4097))
	if err != nil {
		return mapTileMetadata{}, nil, false
	}
	separator := bytes.IndexByte(data, '\n')
	var metadata mapTileMetadata
	if separator < 0 || separator > 4096 || json.Unmarshal(data[:separator], &metadata) != nil || !validMapTileImage(data[separator+1:], metadata.ContentType) {
		return mapTileMetadata{}, nil, false
	}
	return metadata, data[separator+1:], true
}

func (s *MapTileService) writeCached(key string, metadata mapTileMetadata, data []byte) error {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	root, err := os.OpenRoot(s.config.CacheDirectory)
	if err != nil {
		return err
	}
	defer root.Close()
	header, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	// A unique temporary file keeps concurrent helper instances from sharing a
	// partially written entry. Root confines both rename and reads to the cache.
	file, err := os.CreateTemp(s.config.CacheDirectory, ".tile-*")
	if err != nil {
		return err
	}
	temp := filepath.Base(file.Name())
	defer root.Remove(temp)
	_, err = file.Write(append(append(header, '\n'), data...))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := root.Rename(temp, key); err != nil {
		return err
	}
	s.pruneCache(root, key)
	return nil
}

func (s *MapTileService) removeCached(key string) {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	root, err := os.OpenRoot(s.config.CacheDirectory)
	if err == nil {
		defer root.Close()
		_ = root.Remove(key)
	}
}

func (s *MapTileService) pruneCache(root *os.Root, keep string) {
	dir, err := root.Open(".")
	if err != nil {
		return
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return
	}
	type cacheEntry struct {
		name     string
		size     int64
		modified time.Time
	}
	files := make([]cacheEntry, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".tile") || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, cacheEntry{entry.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	count := len(files)
	for _, file := range files {
		if total <= mapTileCacheBytes && count <= mapTileCacheEntries {
			break
		}
		if file.name == keep {
			continue
		}
		if root.Remove(file.name) == nil {
			total -= file.size
			count--
		}
	}
}
