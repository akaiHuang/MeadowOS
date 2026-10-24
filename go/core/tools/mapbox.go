package tools

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const DefaultMapboxStyle = "mapbox/dark-v11"

// The private host file is separate from settings sent to clients or models.
// Only the map proxy reads it. A configured but invalid provider must not
// silently fall back to a differently branded basemap.
type mapboxHostConfig struct {
	AccessToken string `json:"accessToken"`
	Style       string `json:"style,omitempty"`
}

func MapTileConfigFromEnv(cacheDirectory string) (MapTileConfig, error) {
	if os.Getenv("MEADOW_WEB_MAP_PROXY_TOKEN_FILE") != "" {
		return memberMapTileConfig(cacheDirectory)
	}
	config := MapTileConfig{Provider: strings.TrimSpace(os.Getenv("MEADOW_MAP_PROVIDER")), Template: os.Getenv("MEADOW_MAP_TILE_URL"), Attribution: os.Getenv("MEADOW_MAP_ATTRIBUTION"), AttributionURL: os.Getenv("MEADOW_MAP_ATTRIBUTION_URL"), ContactURL: os.Getenv("MEADOW_MAP_CONTACT_URL"), CacheDirectory: cacheDirectory}
	if config.Provider == "openstreetmap" {
		return config, nil // Explicit host rollback ignores the optional Mapbox file.
	}
	config.MapboxToken = strings.TrimSpace(os.Getenv("MEADOW_MAPBOX_ACCESS_TOKEN"))
	config.MapboxStyle = strings.TrimSpace(os.Getenv("MEADOW_MAPBOX_STYLE"))
	path := strings.TrimSpace(os.Getenv("MEADOW_MAPBOX_CONFIG_FILE"))
	explicitPath := path != ""
	if path == "" {
		path = filepath.Join(filepath.Dir(cacheDirectory), "mapbox.json")
	}
	// An existing custom provider remains explicit; don't silently supersede it
	// with an unrelated default file. Mixed explicit providers are rejected below.
	if config.Template == "" || explicitPath || config.Provider == "mapbox" {
		stored, found, err := readMapboxHostConfig(path)
		if err != nil || (explicitPath && !found) {
			return MapTileConfig{}, errors.New("Mapbox configuration unavailable")
		}
		if found {
			if config.MapboxToken == "" {
				config.MapboxToken = strings.TrimSpace(stored.AccessToken)
			}
			if config.MapboxStyle == "" {
				config.MapboxStyle = strings.TrimSpace(stored.Style)
			}
			if config.Provider == "" {
				config.Provider = "mapbox"
			}
		}
	}
	return config, nil
}

func readMapboxHostConfig(path string) (mapboxHostConfig, bool, error) {
	var config mapboxHostConfig
	if !filepath.IsAbs(path) {
		return config, false, errors.New("Mapbox configuration path must be absolute")
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return config, false, nil
	}
	if err != nil {
		return config, false, errors.New("Mapbox configuration unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return config, true, errors.New("invalid Mapbox configuration")
	}
	decoder := json.NewDecoder(io.LimitReader(f, (16<<10)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || decoder.Decode(new(any)) != io.EOF {
		return mapboxHostConfig{}, true, errors.New("invalid Mapbox configuration")
	}
	return config, true, nil
}

func configureMapProvider(config MapTileConfig) (MapTileConfig, error) {
	config.Provider = strings.TrimSpace(config.Provider)
	if config.Provider == "" && (config.MapboxToken != "" || config.MapboxStyle != "") {
		config.Provider = "mapbox"
	}
	if config.Provider == "mapbox" {
		if strings.TrimSpace(config.Template) != "" {
			return MapTileConfig{}, errors.New("conflicting map providers")
		}
		token := strings.TrimSpace(config.MapboxToken)
		if len(token) < 10 || len(token) > 4096 || !strings.HasPrefix(token, "pk.") || !mapboxTokenCharacters(token) {
			return MapTileConfig{}, errors.New("Mapbox requires a public access token")
		}
		style := strings.TrimSpace(config.MapboxStyle)
		if style == "" {
			style = DefaultMapboxStyle
		}
		// These classic street styles share the same attribution contract. Standard is
		// not supported by Static Tiles; satellite/custom sources need separate
		// provider attribution, so they are not accepted by this integration.
		switch style {
		case "mapbox/dark-v11", "mapbox/light-v11", "mapbox/streets-v12", "mapbox/outdoors-v12", "mapbox/navigation-night-v1":
		default:
			return MapTileConfig{}, errors.New("unsupported Mapbox raster style")
		}
		config.Template = "https://api.mapbox.com/styles/v1/" + style + "/tiles/512/{z}/{x}/{y}?access_token=" + url.QueryEscape(token)
		config.Attribution = "© Mapbox © OpenStreetMap"
		config.AttributionURL = "https://www.mapbox.com/about/maps/"
		config.MapboxToken = "" // The fixed upstream URL is the only consumer.
		return config, nil
	}
	if config.Provider != "" && config.Provider != "openstreetmap" {
		return MapTileConfig{}, errors.New("unknown map provider")
	}
	if config.Provider == "openstreetmap" && config.Template != "" && config.Template != DefaultMapTileURL {
		return MapTileConfig{}, errors.New("conflicting map providers")
	}
	if config.Template == "" || strings.TrimSpace(config.Template) == DefaultMapTileURL {
		config.Provider = "openstreetmap"
	} else {
		config.Provider = "custom"
	}
	return config, nil
}

func mapboxTokenCharacters(value string) bool {
	for _, ch := range value {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}
