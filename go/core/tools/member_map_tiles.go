package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// This fixed loopback capability contains no upstream Mapbox access token.
// The member gateway alone reads that host credential; tools cannot select
// another destination, style or upstream query string.
const memberMapTileURL = "http://127.0.0.1:18156/_maps/v1/mapbox/navigation-night-v1/tiles/512/{z}/{x}/{y}"

type memberMapTileProxy struct{ token string }

func memberMapTileConfig(cacheDirectory string) (MapTileConfig, error) {
	filename := os.Getenv("MEADOW_WEB_MAP_PROXY_TOKEN_FILE")
	if !filepath.IsAbs(filename) {
		return MapTileConfig{}, errors.New("member map proxy requires a private token file")
	}
	data, err := readMeadowPrivateFile(filename, 256)
	token := strings.TrimSpace(string(data))
	if err != nil || len(token) != 43 {
		return MapTileConfig{}, errors.New("member map proxy token unavailable")
	}
	for _, ch := range token {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
			return MapTileConfig{}, errors.New("invalid member map proxy token")
		}
	}
	return MapTileConfig{Provider: "mapbox", Template: memberMapTileURL, Attribution: "© Mapbox © OpenStreetMap", AttributionURL: "https://www.mapbox.com/about/maps/", CacheDirectory: cacheDirectory, memberProxy: &memberMapTileProxy{token: token}}, nil
}
