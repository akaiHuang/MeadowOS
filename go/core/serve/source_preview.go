package serve

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// handleSourcePreview is display-only. The caller may select an observed URL
// from this session, but cannot choose a provider, service actor, or image URL.
// Native Agent authentication also protects this route when forwarded to phone.
func (s *Server) handleSourcePreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	var body struct {
		URL   string  `json:"url"`
		RunID *string `json:"runId,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6144))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.URL == "" {
		writeErr(w, http.StatusBadRequest, "source URL required")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "invalid preview request")
		return
	}
	var authorized *tools.EvidenceSource
	sources := sess.sourceSnapshot()
	for i := len(sources) - 1; i >= 0; i-- {
		source := sources[i]
		if source.URL == body.URL && (body.RunID == nil || source.RunID == *body.RunID) {
			metadata := source.evidenceSource()
			authorized = &metadata
			break
		}
	}
	if authorized == nil {
		writeErr(w, http.StatusNotFound, "unknown source")
		return
	}
	image, err := tools.FetchMeadowSourcePreview(r.Context(), *authorized)
	if err != nil {
		// Optional pictures cannot change a source's truth or tool status.
		// Do not expose provider configuration, private tokens, or raw errors.
		writeJSON(w, map[string]any{"image": nil, "reason": "unavailable"})
		return
	}
	if image == "" {
		writeJSON(w, map[string]any{"image": nil})
		return
	}
	writeJSON(w, map[string]any{"image": image})
}
