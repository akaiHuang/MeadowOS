package serve

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

type mapTileSource interface {
	Config() tools.MapTilePublicConfig
	Fetch(context.Context, int, int, int) (tools.MapTile, error)
}

func (s *Server) registerMapRoutes(mux *http.ServeMux) {
	config, err := tools.MapTileConfigFromEnv(filepath.Join(session.PrimaryDir(), "map-tiles"))
	var source *tools.MapTileService
	if err == nil {
		source, err = tools.NewMapTileService(config)
	}
	s.registerMapRoutesWithSource(mux, source, err)
}

// These routes inherit the native Agent authentication boundary, including
// phone forwarding. Session existence and observed places are checked before
// any upstream request; a tile request cannot supply an upstream URL.
func (s *Server) registerMapRoutesWithSource(mux *http.ServeMux, source mapTileSource, setupErr error) {
	mux.HandleFunc("GET /session/{id}/map-config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.session(r.PathValue("id")) == nil {
			writeErr(w, http.StatusNotFound, "unknown session")
			return
		}
		if setupErr != nil {
			writeErr(w, http.StatusServiceUnavailable, "map provider unavailable")
			return
		}
		config := source.Config()
		tileURL := "/session/" + r.PathValue("id") + "/map-tiles/{z}/{x}/{y}"
		if config.TileRevision != "" {
			tileURL += "?v=" + config.TileRevision
		}
		writeJSON(w, struct {
			tools.MapTilePublicConfig
			TileURL string `json:"tileURL"`
		}{config, tileURL})
	})
	mux.HandleFunc("GET /session/{id}/map-tiles/{z}/{x}/{y}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		sess := s.session(r.PathValue("id"))
		if sess == nil {
			writeErr(w, http.StatusNotFound, "unknown session")
			return
		}
		if !sess.hasPlaceResults() {
			writeErr(w, http.StatusNotFound, "no places in session")
			return
		}
		z, ez := parseMapTileCoordinate(r.PathValue("z"))
		x, ex := parseMapTileCoordinate(r.PathValue("x"))
		y, ey := parseMapTileCoordinate(r.PathValue("y"))
		if ez != nil || ex != nil || ey != nil || !tools.ValidMapTileCoordinates(z, x, y) {
			writeErr(w, http.StatusBadRequest, "invalid map tile coordinates")
			return
		}
		if setupErr != nil {
			writeErr(w, http.StatusServiceUnavailable, "map provider unavailable")
			return
		}
		expectedQuery := ""
		if revision := source.Config().TileRevision; revision != "" {
			expectedQuery = "v=" + revision
		}
		if r.URL.RawQuery != expectedQuery {
			writeErr(w, http.StatusBadRequest, "invalid map tile revision")
			return
		}
		tile, err := source.Fetch(r.Context(), z, x, y)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "map tile unavailable")
			return
		}
		maxAge := int64(time.Until(tile.ExpiresAt) / time.Second)
		if maxAge < 0 {
			maxAge = 0
		}
		w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", maxAge))
		if tile.NoStore {
			w.Header().Set("Cache-Control", "no-store")
		}
		w.Header().Set("Content-Type", tile.ContentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		etag := fmt.Sprintf(`"%x"`, sha256.Sum256(tile.Data))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(tile.Data)))
		_, _ = w.Write(tile.Data)
	})
}

func parseMapTileCoordinate(value string) (int, error) {
	if len(value) == 0 || len(value) > 6 {
		return 0, fmt.Errorf("invalid coordinate")
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid coordinate")
		}
	}
	return strconv.Atoi(value)
}
