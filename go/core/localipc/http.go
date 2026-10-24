package localipc

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// RequireBearer protects one local HTTP surface. PublicPaths must contain only
// non-sensitive readiness endpoints; every other route fails closed.
func RequireBearer(
	next http.Handler,
	token string,
	publicPaths map[string]struct{},
) http.Handler {
	expected := []byte(strings.TrimSpace(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, public := publicPaths[r.URL.Path]; public {
			next.ServeHTTP(w, r)
			return
		}
		scheme, candidate, found := strings.Cut(
			strings.TrimSpace(r.Header.Get("Authorization")),
			" ",
		)
		provided := []byte(strings.TrimSpace(candidate))
		allowed := found && strings.EqualFold(scheme, "Bearer") &&
			len(expected) > 0 && len(provided) == len(expected) &&
			subtle.ConstantTimeCompare(provided, expected) == 1
		if !allowed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"message": "Meadow local client authentication failed",
					"type":    "authentication_error",
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
