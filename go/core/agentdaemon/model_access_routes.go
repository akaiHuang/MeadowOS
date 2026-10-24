package agentdaemon

import (
	"context"
	"net/http"

	"github.com/hey-meadow/meadow-code/go/core/productauth"
)

type ModelAccessService interface {
	ModelAccessStatus(context.Context) (productauth.ModelAccessStatus, error)
	ModelAccessUsage(context.Context) (productauth.ModelAccessUsage, error)
	ModelAccessProvision(context.Context) (productauth.ModelAccessProvision, error)
}

func registerModelAccessRoutes(mux *http.ServeMux, service ModelAccessService) {
	mux.HandleFunc("GET /model-access/status", func(w http.ResponseWriter, r *http.Request) {
		result, err := service.ModelAccessStatus(r.Context())
		if err != nil {
			writeProductAuthError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /model-access/usage", func(w http.ResponseWriter, r *http.Request) {
		result, err := service.ModelAccessUsage(r.Context())
		if err != nil {
			writeProductAuthError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /model-access/provision", func(w http.ResponseWriter, r *http.Request) {
		var input productauth.RefreshRequest
		if decodeAuthJSON(r, &input) != nil || input.Schema != productauth.SchemaV1 {
			writeProductAuthError(w, &productauth.Error{Code: "invalid_request", Message: "model access request is invalid", HTTPStatus: http.StatusBadRequest})
			return
		}
		// The public DTO has no credential field. Only the native provider's
		// ResolveModelAccessForAccountRef method can retrieve an inference key.
		result, err := service.ModelAccessProvision(r.Context())
		if err != nil {
			writeProductAuthError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
