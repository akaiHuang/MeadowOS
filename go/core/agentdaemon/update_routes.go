package agentdaemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/productupdate"
)

// ProductUpdateService deliberately excludes CommitInstalled. Product
// acceptance is a protected startup responsibility, never a client route.
type ProductUpdateService interface {
	Check(context.Context) (productupdate.CheckResult, error)
	Stage(context.Context) (productupdate.StageResult, error)
	Install(context.Context, string) (productupdate.InstallResult, error)
}

type updateCheckResponse struct {
	FeedID       string `json:"feedId"`
	Sequence     int64  `json:"sequence"`
	ReleaseID    string `json:"releaseId"`
	Version      string `json:"version"`
	BuildNumber  int64  `json:"buildNumber"`
	FeedDigest   string `json:"feedDigest"`
	ArtifactID   string `json:"artifactId"`
	Architecture string `json:"architecture"`
	MinOS        string `json:"minOS"`
	StagedID     string `json:"stagedId,omitempty"`
}

type updateStageResponse struct {
	StagedID    string `json:"stagedId"`
	ReleaseID   string `json:"releaseId"`
	Version     string `json:"version"`
	BuildNumber int64  `json:"buildNumber"`
}

type updateInstallResponse struct {
	StagedID         string `json:"stagedId"`
	HandoffReference string `json:"handoffReference"`
	Accepted         bool   `json:"accepted"`
}

type updateRouteStatus struct {
	State     string                 `json:"state"`
	Operation string                 `json:"operation,omitempty"`
	Busy      bool                   `json:"busy"`
	Check     *updateCheckResponse   `json:"check,omitempty"`
	Stage     *updateStageResponse   `json:"stage,omitempty"`
	Install   *updateInstallResponse `json:"install,omitempty"`
	ErrorCode string                 `json:"errorCode,omitempty"`
}

type updateRoutes struct {
	service   ProductUpdateService
	operation chan struct{}
	statusMu  sync.RWMutex
	status    updateRouteStatus
}

func newUpdateRoutes(service ProductUpdateService) *updateRoutes {
	return &updateRoutes{
		service: service, operation: make(chan struct{}, 1),
		status: updateRouteStatus{State: "idle"},
	}
}

func (routes *updateRoutes) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /agent/update/check", routes.handleCheck)
	mux.HandleFunc("GET /agent/update/status", routes.handleStatus)
	mux.HandleFunc("POST /agent/update/stage", routes.handleStage)
	mux.HandleFunc("POST /agent/update/install", routes.handleInstall)
}

func (routes *updateRoutes) handleCheck(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyUpdateRequest(request); err != nil {
		writeError(writer, http.StatusBadRequest, "update check request body must be empty")
		return
	}
	if !routes.begin("check", "checking") {
		writeError(writer, http.StatusConflict, "another product update operation is active")
		return
	}
	defer routes.finishOperation()
	result, err := routes.service.Check(request.Context())
	if err != nil {
		routes.fail("check", err)
		writeUpdateError(writer, err)
		return
	}
	response := updateCheckResponse{
		FeedID: result.FeedID, Sequence: result.Sequence, ReleaseID: result.ReleaseID,
		Version: result.Version, BuildNumber: result.BuildNumber, FeedDigest: result.FeedDigest,
		ArtifactID: result.ArtifactID, Architecture: result.Architecture, MinOS: result.MinOS,
		StagedID: result.StagedID,
	}
	state := "available"
	if response.StagedID != "" {
		state = "staged"
	}
	routes.set(updateRouteStatus{State: state, Operation: "check", Check: &response})
	writeJSON(writer, http.StatusOK, response)
}

func (routes *updateRoutes) handleStatus(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyUpdateRequest(request); err != nil {
		writeError(writer, http.StatusBadRequest, "update status request body must be empty")
		return
	}
	routes.statusMu.RLock()
	status := routes.status
	routes.statusMu.RUnlock()
	writeJSON(writer, http.StatusOK, status)
}

func (routes *updateRoutes) handleStage(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyUpdateRequest(request); err != nil {
		writeError(writer, http.StatusBadRequest, "update stage request body must be empty")
		return
	}
	if !routes.begin("stage", "staging") {
		writeError(writer, http.StatusConflict, "another product update operation is active")
		return
	}
	defer routes.finishOperation()
	result, err := routes.service.Stage(request.Context())
	if err != nil {
		routes.fail("stage", err)
		writeUpdateError(writer, err)
		return
	}
	response := updateStageResponse{
		StagedID: result.StagedID, ReleaseID: result.ReleaseID,
		Version: result.Version, BuildNumber: result.BuildNumber,
	}
	routes.set(updateRouteStatus{State: "staged", Operation: "stage", Stage: &response})
	writeJSON(writer, http.StatusOK, response)
}

func (routes *updateRoutes) handleInstall(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		StagedID string `json:"stagedId"`
	}
	if err := decodeJSON(request, &body, true); err != nil || strings.TrimSpace(body.StagedID) == "" {
		writeError(writer, http.StatusBadRequest, "stagedId is required")
		return
	}
	if !routes.begin("install", "installing") {
		writeError(writer, http.StatusConflict, "another product update operation is active")
		return
	}
	defer routes.finishOperation()
	result, err := routes.service.Install(request.Context(), strings.TrimSpace(body.StagedID))
	if err != nil {
		routes.fail("install", err)
		writeUpdateError(writer, err)
		return
	}
	response := updateInstallResponse{
		StagedID: result.StagedID, HandoffReference: result.HandoffReference,
		Accepted: false,
	}
	routes.set(updateRouteStatus{State: "handoff", Operation: "install", Install: &response})
	writeJSON(writer, http.StatusAccepted, response)
}

func (routes *updateRoutes) begin(operation, state string) bool {
	select {
	case routes.operation <- struct{}{}:
		routes.set(updateRouteStatus{State: state, Operation: operation, Busy: true})
		return true
	default:
		return false
	}
}

func (routes *updateRoutes) finishOperation() { <-routes.operation }

func (routes *updateRoutes) fail(operation string, err error) {
	code := productupdate.ErrorCode(err)
	if code == "" {
		code = "UPDATE_UNAVAILABLE"
	}
	routes.set(updateRouteStatus{State: "failed", Operation: operation, ErrorCode: code})
}

func (routes *updateRoutes) set(status updateRouteStatus) {
	routes.statusMu.Lock()
	routes.status = status
	routes.statusMu.Unlock()
}

func requireEmptyUpdateRequest(request *http.Request) error {
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

func writeUpdateError(writer http.ResponseWriter, err error) {
	code := productupdate.ErrorCode(err)
	if code == "" {
		code = "UPDATE_UNAVAILABLE"
	}
	status := http.StatusServiceUnavailable
	switch code {
	case "STAGED_ID":
		status = http.StatusBadRequest
	case "STATE_CONFLICT", "FEED_REPLAY", "FEED_ROLLBACK", "VERSION_ROLLBACK", "BUILD_REPLAY", "INSTALL_IN_PROGRESS":
		status = http.StatusConflict
	}
	writeJSON(writer, status, map[string]any{
		"error": "product update operation failed", "code": code, "status": status,
	})
}
