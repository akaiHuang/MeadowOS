package agentdaemon

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
)

type configurationLeaseApplier interface {
	ApplyConfigurationLease(string, func() (string, error)) error
}
type externalConfigurationOwner interface {
	ReloadConfiguration(string) (runtimecontrol.ExternalConfiguration, error)
	ConfigurationIdentity() runtimecontrol.ExternalConfiguration
}

// ConnectionReloadResult is an acknowledgement of the applied local snapshot.
// It does not claim provider connectivity or expose credentials/endpoint paths.
type ConnectionReloadResult struct {
	Applied      bool   `json:"applied"`
	ConnectionID string `json:"connectionId"`
	ModelID      string `json:"modelId"`
	Fingerprint  string `json:"fingerprint"`
}

func connectionReloadHandler(control runtimecontrol.RuntimeControl, lease configurationLeaseApplier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, ok := control.(externalConfigurationOwner)
		if !ok || lease == nil {
			writeError(w, http.StatusConflict, "API configuration reload is unavailable")
			return
		}
		var body struct {
			LeaseID             string `json:"leaseId"`
			ExpectedFingerprint string `json:"expectedFingerprint"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || body.LeaseID == "" || len(body.ExpectedFingerprint) != 64 {
			writeError(w, http.StatusBadRequest, "leaseId and expectedFingerprint are required")
			return
		}
		if _, err := hex.DecodeString(body.ExpectedFingerprint); err != nil {
			writeError(w, http.StatusBadRequest, "invalid configuration fingerprint")
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid trailing configuration data")
			return
		}
		var applied runtimecontrol.ExternalConfiguration
		err := lease.ApplyConfigurationLease(body.LeaseID, func() (string, error) {
			if err := r.Context().Err(); err != nil {
				return "", err
			}
			var err error
			applied, err = owner.ReloadConfiguration(body.ExpectedFingerprint)
			return applied.ConnectionID, err
		})
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, ConnectionReloadResult{Applied: true, ConnectionID: applied.ConnectionID, ModelID: applied.ModelID, Fingerprint: applied.Fingerprint})
	})
}

// Preserve the applied configuration snapshot through the HTTP/account/limit
// wrappers for the private transport identity. Process/profile identity stays
// owned by daemontransport and cannot be supplied by a reload request.
type connectionIdentityHandler struct {
	http.Handler
	owner externalConfigurationOwner
}

func (h *connectionIdentityHandler) Close() error {
	if owner, ok := h.Handler.(interface{ Close() error }); ok {
		return owner.Close()
	}
	return nil
}

func (h *connectionIdentityHandler) AppliedConnectionIdentity() (string, string, string) {
	identity := h.owner.ConfigurationIdentity()
	return identity.ConnectionID, identity.ModelID, identity.Fingerprint
}
