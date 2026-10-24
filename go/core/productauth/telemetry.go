package productauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/hey-meadow/meadow-code/go/core/telemetry"
	"net/http"
)

const telemetryIngestPath = "/desktopTelemetryIngest"

type telemetryClient interface {
	TelemetryIngest(context.Context, commercialRemoteRequest, []byte) error
}

// TelemetryPrincipal is a native verified session subject, not a caller-supplied
// Firebase UID. It performs no disk/network I/O on model/event paths.
func (service *Service) TelemetryPrincipal() string {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.localDenial != "" {
		return ""
	}
	if service.access.lease.Subject != "" && service.access.lease.EffectiveExpiresAt.After(service.now().UTC()) {
		return service.access.lease.Subject
	}
	if service.telemetryLease.EffectiveExpiresAt.After(service.now().UTC()) {
		return service.telemetryLease.Subject
	}
	return ""
}
func (service *Service) ExportTelemetry(ctx context.Context, principal string, payload []byte) error {
	client, ok := service.client.(telemetryClient)
	if !ok {
		return errors.New("telemetry receiver unavailable")
	}
	if len(payload) == 0 || len(payload) > telemetry.MaxBatchBytes {
		return errors.New("telemetry payload exceeds bound")
	}
	access, err := service.currentAccess(ctx)
	if err != nil {
		return err
	}
	if principal == "" || access.lease.Subject != principal {
		return errors.New("telemetry account changed")
	}
	requestID, err := service.randomRequestID()
	if err != nil {
		return err
	}
	privateKey, _, err := service.deviceProofKey(ctx)
	if err != nil {
		return err
	}
	defer clear(privateKey)
	hash := sha256.Sum256(payload)
	binding := hex.EncodeToString(hash[:])
	request := commercialRemoteRequest{AccessToken: access.token, RequestID: requestID, InstallationID: service.installationID, DeviceProof: signBoundDeviceProof(privateKey, "telemetry_ingest", access.token, service.installationID, requestID, "", binding)}
	return client.TelemetryIngest(ctx, request, payload)
}
func (client *FixedClient) TelemetryIngest(ctx context.Context, request commercialRemoteRequest, payload []byte) error {
	var batch struct {
		Schema string            `json:"schema"`
		Events []json.RawMessage `json:"events"`
	}
	if len(payload) > telemetry.MaxBatchBytes || json.Unmarshal(payload, &batch) != nil || batch.Schema != telemetry.Schema || len(batch.Events) == 0 || len(batch.Events) > 32 {
		return errors.New("invalid telemetry batch")
	}
	body := map[string]any{"schema": SchemaV1, "request_id": request.RequestID, "installation_id": request.InstallationID, "device_proof": request.DeviceProof, "telemetry_json": string(payload)}
	var response struct {
		Schema   string `json:"schema"`
		Accepted int    `json:"accepted"`
	}
	if err := client.doCommercial(ctx, telemetryIngestPath, request.AccessToken, body, map[string]string{"X-Request-ID": request.RequestID}, &response); err != nil {
		return err
	}
	if response.Schema != telemetry.Schema || response.Accepted != len(batch.Events) {
		return authError("telemetry_rejected", "telemetry acceptance is incomplete", http.StatusBadGateway, true, nil)
	}
	return nil
}
