package agentdaemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
)

type inceptionConfigurationOwner interface {
	ConfigureInception(context.Context, string) (runtimecontrol.ExternalConfiguration, error)
}

// The outer Unix transport authenticates the current user and signed peer.
// This bounded route accepts only an in-memory key and the native idle lease;
// callers cannot supply endpoint URLs, model capabilities, or storage paths.
func inceptionConfigurationHandler(control runtimecontrol.RuntimeControl, lease configurationLeaseApplier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, ok := control.(inceptionConfigurationOwner)
		if !ok || lease == nil {
			writeError(w, http.StatusConflict, "Inception configuration is unavailable")
			return
		}
		var body struct {
			LeaseID string `json:"leaseId"`
			APIKey  string `json:"apiKey"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 20<<10))
		decoder.DisallowUnknownFields()
		valid := decoder.Decode(&body) == nil && len(body.LeaseID) == 64 && body.APIKey != "" && len(body.APIKey) <= 16<<10 && strings.TrimSpace(body.APIKey) == body.APIKey
		for _, char := range body.APIKey {
			valid = valid && char >= 33 && char <= 126
		}
		if !valid || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
			writeError(w, http.StatusBadRequest, "valid leaseId and apiKey are required")
			return
		}
		var applied runtimecontrol.ExternalConfiguration
		err := lease.ApplyConfigurationLease(body.LeaseID, func() (string, error) {
			var err error
			applied, err = owner.ConfigureInception(r.Context(), body.APIKey)
			return applied.ConnectionID, err
		})
		body.APIKey = ""
		if err != nil {
			// Storage and transport errors must never echo credential material.
			writeError(w, http.StatusConflict, "Inception configuration was not applied; Agent must be idle with a valid lease")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"applied": true, "connectionId": applied.ConnectionID, "modelId": applied.ModelID,
			"fingerprint": applied.Fingerprint, "supportsTools": true, "supportsImages": false,
			"reasoningControl": "inception-effort",
		})
	})
}
