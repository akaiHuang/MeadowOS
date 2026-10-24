package agentdaemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

var (
	ErrOfficialModelUnavailable = errors.New("official model service unavailable")
	ErrOfficialModelNotFound    = errors.New("official model not found")
	ErrOfficialModelRejected    = errors.New("official model operation rejected")
	ErrModelRemovalDenied       = errors.New("official model removal denied")
	ErrUserImportUnsupported    = errors.New("user model import requires an opaque file capability")
)

type ModelCatalogStatus struct {
	State            string `json:"state"`
	Sequence         uint64 `json:"sequence,omitempty"`
	Digest           string `json:"digest,omitempty"`
	InstallAvailable bool   `json:"installAvailable"`
}

type OfficialModel struct {
	ModelID      string `json:"modelId"`
	Version      string `json:"version"`
	ProfileID    string `json:"profileId"`
	LicenseID    string `json:"licenseId"`
	Installed    bool   `json:"installed"`
	RuntimeReady bool   `json:"runtimeReady"`
}

type OfficialModelRef struct {
	ModelID string
	Version string
}

type InstallOfficialModelRequest struct {
	ModelID         string
	Version         string
	LicenseAccepted bool
	LicenseSubject  string
}

type RemoveOfficialModelRequest struct {
	ModelID   string
	Version   string
	Confirmed bool
}

type OfficialModelService interface {
	CatalogStatus(context.Context) (ModelCatalogStatus, error)
	Catalog(context.Context) ([]OfficialModel, error)
	List(context.Context) ([]OfficialModel, error)
	ModelStatus(context.Context, OfficialModelRef) (OfficialModel, error)
	InstallOfficial(context.Context, InstallOfficialModelRequest) (OfficialModel, error)
	Remove(context.Context, RemoveOfficialModelRequest) error
}

type modelRoutes struct {
	service   OfficialModelService
	operation chan struct{}
}

func newModelRoutes(service OfficialModelService) *modelRoutes {
	return &modelRoutes{service: service, operation: make(chan struct{}, 1)}
}

func (routes *modelRoutes) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /agent/models/catalog/status", routes.handleCatalogStatus)
	mux.HandleFunc("GET /agent/models/catalog", routes.handleCatalog)
	mux.HandleFunc("GET /agent/models", routes.handleList)
	mux.HandleFunc("POST /agent/models/status", routes.handleStatus)
	mux.HandleFunc("POST /agent/models/install", routes.handleInstall)
	mux.HandleFunc("POST /agent/models/remove", routes.handleRemove)
	mux.HandleFunc("POST /agent/models/import", routes.handleImport)
}

func (routes *modelRoutes) handleCatalogStatus(writer http.ResponseWriter, request *http.Request) {
	if requireEmptyModelRequest(request) != nil {
		writeError(writer, http.StatusBadRequest, "model catalog status request body must be empty")
		return
	}
	status, err := routes.service.CatalogStatus(request.Context())
	if err != nil {
		writeModelError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (routes *modelRoutes) handleCatalog(writer http.ResponseWriter, request *http.Request) {
	if requireEmptyModelRequest(request) != nil {
		writeError(writer, http.StatusBadRequest, "model catalog request body must be empty")
		return
	}
	models, err := routes.service.Catalog(request.Context())
	if err != nil {
		writeModelError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"models": models})
}

func (routes *modelRoutes) handleList(writer http.ResponseWriter, request *http.Request) {
	if requireEmptyModelRequest(request) != nil {
		writeError(writer, http.StatusBadRequest, "model list request body must be empty")
		return
	}
	models, err := routes.service.List(request.Context())
	if err != nil {
		writeModelError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"models": models})
}

func (routes *modelRoutes) handleStatus(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ModelID string `json:"modelId"`
		Version string `json:"version"`
	}
	if decodeJSON(request, &body, true) != nil || !validModelRouteID(body.ModelID) || !validModelRouteID(body.Version) {
		writeError(writer, http.StatusBadRequest, "modelId and version are required")
		return
	}
	model, err := routes.service.ModelStatus(request.Context(), OfficialModelRef{
		ModelID: strings.TrimSpace(body.ModelID), Version: strings.TrimSpace(body.Version),
	})
	if err != nil {
		writeModelError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, model)
}

func (routes *modelRoutes) handleInstall(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ModelID         string `json:"modelId"`
		Version         string `json:"version"`
		LicenseAccepted bool   `json:"licenseAccepted"`
		LicenseSubject  string `json:"licenseSubject"`
	}
	if decodeJSON(request, &body, true) != nil || !validModelRouteID(body.ModelID) ||
		!validModelRouteID(body.Version) || !body.LicenseAccepted || !validLicenseSubject(body.LicenseSubject) {
		writeError(writer, http.StatusBadRequest, "modelId, version, and explicit license acceptance are required")
		return
	}
	if !routes.begin() {
		writeError(writer, http.StatusConflict, "another model operation is active")
		return
	}
	defer routes.finish()
	model, err := routes.service.InstallOfficial(request.Context(), InstallOfficialModelRequest{
		ModelID: strings.TrimSpace(body.ModelID), Version: strings.TrimSpace(body.Version),
		LicenseAccepted: true, LicenseSubject: strings.TrimSpace(body.LicenseSubject),
	})
	if err != nil {
		writeModelError(writer, err)
		return
	}
	if !model.Installed || !model.RuntimeReady {
		writeModelError(writer, ErrOfficialModelRejected)
		return
	}
	writeJSON(writer, http.StatusCreated, model)
}

func (routes *modelRoutes) handleRemove(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ModelID   string `json:"modelId"`
		Version   string `json:"version"`
		Confirmed bool   `json:"confirmed"`
	}
	if decodeJSON(request, &body, true) != nil || !validModelRouteID(body.ModelID) ||
		!validModelRouteID(body.Version) || !body.Confirmed {
		writeError(writer, http.StatusBadRequest, "modelId, version, and explicit confirmation are required")
		return
	}
	if !routes.begin() {
		writeError(writer, http.StatusConflict, "another model operation is active")
		return
	}
	defer routes.finish()
	if err := routes.service.Remove(request.Context(), RemoveOfficialModelRequest{
		ModelID: strings.TrimSpace(body.ModelID), Version: strings.TrimSpace(body.Version), Confirmed: true,
	}); err != nil {
		writeModelError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"modelId": strings.TrimSpace(body.ModelID), "version": strings.TrimSpace(body.Version), "state": "removed",
	})
}

func (routes *modelRoutes) handleImport(writer http.ResponseWriter, request *http.Request) {
	if requireEmptyModelRequest(request) != nil {
		writeError(writer, http.StatusBadRequest, "user model import does not accept a path or request payload")
		return
	}
	writeJSON(writer, http.StatusNotImplemented, map[string]any{
		"error": ErrUserImportUnsupported.Error(), "code": "MODEL_IMPORT_UNSUPPORTED", "status": http.StatusNotImplemented,
	})
}

func (routes *modelRoutes) begin() bool {
	select {
	case routes.operation <- struct{}{}:
		return true
	default:
		return false
	}
}

func (routes *modelRoutes) finish() { <-routes.operation }

func requireEmptyModelRequest(request *http.Request) error {
	if request.Body == nil || request.Body == http.NoBody {
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(request.Body, 1))
	if err != nil {
		return err
	}
	if len(payload) != 0 {
		return errors.New("request body is not empty")
	}
	return nil
}

func validModelRouteID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 192 && !strings.ContainsRune(value, 0)
}

func validLicenseSubject(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsRune(value, 0)
}

func writeModelError(writer http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	code := "MODEL_UNAVAILABLE"
	switch {
	case errors.Is(err, ErrOfficialModelNotFound):
		status, code = http.StatusNotFound, "MODEL_NOT_FOUND"
	case errors.Is(err, ErrModelRemovalDenied):
		status, code = http.StatusForbidden, "MODEL_REMOVAL_DENIED"
	case errors.Is(err, ErrOfficialModelRejected):
		status, code = http.StatusUnprocessableEntity, "MODEL_REJECTED"
	}
	writeJSON(writer, status, map[string]any{
		"error": "official model operation failed", "code": code, "status": status,
	})
}
