package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/localipc"
)

// MeadowCoreStatus is the capability handshake shared by CLI and Desktop.
// Generic OpenAI-compatible servers do not expose model_manager/runtime_backend
// and therefore cannot be mistaken for Meadow Core.
type MeadowCoreStatus struct {
	Status              string `json:"status"`
	RuntimeBackend      string `json:"runtime_backend"`
	CorePID             int    `json:"core_pid"`
	ContextWindowTokens int    `json:"context_window_tokens"`
	MaxOutputTokens     int    `json:"max_output_tokens"`
	ModelIdentity       struct {
		CanonicalID      string `json:"canonical_id"`
		ArtifactPath     string `json:"artifact_path"`
		ArtifactName     string `json:"artifact_name"`
		Architecture     string `json:"architecture"`
		ModelType        string `json:"model_type"`
		RuntimeBackend   string `json:"runtime_backend"`
		RuntimePID       int    `json:"runtime_pid"`
		IdentityVerified bool   `json:"identity_verified"`
		Multimodal       bool   `json:"multimodal"`
		Quantization     struct {
			Method    string `json:"method"`
			Bits      int    `json:"bits"`
			GroupSize int    `json:"group_size"`
		} `json:"quantization"`
	} `json:"model_identity"`
	Model struct {
		ID             string `json:"id"`
		State          string `json:"state"`
		ActiveRequests int    `json:"active_requests"`
		QueuedRequests int    `json:"queued_requests"`
	} `json:"model"`
	ModelManager struct {
		Models []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"models"`
	} `json:"model_manager"`
}

func meadowCoreEndpoint(baseURL, path string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base + path
}

func meadowCoreRequest(ctx context.Context, method, baseURL, apiKey, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, meadowCoreEndpoint(baseURL, path), reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key := strings.TrimSpace(localipc.CoreAPIKey(baseURL, apiKey)); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Meadow Core %s returned %s: %s", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode Meadow Core %s: %w", path, err)
		}
	}
	return nil
}

// ProbeMeadowCoreStatus returns ok=false for an ordinary OpenAI-compatible
// endpoint even if it happens to implement /v1/status.
func ProbeMeadowCoreStatus(ctx context.Context, baseURL, apiKey string) (status MeadowCoreStatus, ok bool, err error) {
	var raw map[string]json.RawMessage
	if err := meadowCoreRequest(ctx, http.MethodGet, baseURL, apiKey, "/status", nil, &raw); err != nil {
		return MeadowCoreStatus{}, false, err
	}
	// Inspect Meadow-only capability fields before decoding the entire status.
	// Generic OpenAI/vLLM endpoints commonly expose `model` as a string rather
	// than Meadow Core's structured model object; that is a clean capability
	// miss, not a malformed Meadow Core response.
	var runtimeBackend string
	_ = json.Unmarshal(raw["runtime_backend"], &runtimeBackend)
	var manager struct {
		Models []json.RawMessage `json:"models"`
	}
	_ = json.Unmarshal(raw["model_manager"], &manager)
	if strings.TrimSpace(runtimeBackend) == "" || len(manager.Models) == 0 {
		return MeadowCoreStatus{}, false, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return MeadowCoreStatus{}, false, err
	}
	if err := json.Unmarshal(encoded, &status); err != nil {
		return MeadowCoreStatus{}, false, fmt.Errorf("decode Meadow Core status: %w", err)
	}
	return status, true, nil
}

func (s MeadowCoreStatus) ResidentModel() (string, bool) {
	state := strings.ToLower(strings.TrimSpace(s.Model.State))
	ready := state == "ready" || state == "loaded"
	return strings.TrimSpace(s.Model.ID), ready
}

// VerifyRuntimeIdentity is the precondition for a performance report. It
// proves that the public Core, private SGLang child, artifact configuration,
// and requested served id all refer to the same loaded model.
func (s MeadowCoreStatus) VerifyRuntimeIdentity(expectedModel string) error {
	expected := strings.TrimSpace(expectedModel)
	resident, ready := s.ResidentModel()
	if !ready {
		return fmt.Errorf("Meadow Core model is not ready")
	}
	if expected != "" && !strings.EqualFold(resident, expected) {
		return fmt.Errorf("resident model %q does not match requested model %q", resident, expected)
	}
	identity := s.ModelIdentity
	if !identity.IdentityVerified {
		return fmt.Errorf("Meadow Core did not verify the local model artifact identity")
	}
	if !strings.EqualFold(strings.TrimSpace(identity.CanonicalID), resident) {
		return fmt.Errorf("artifact identity %q does not match resident model %q", identity.CanonicalID, resident)
	}
	if s.CorePID <= 0 || identity.RuntimePID <= 0 {
		return fmt.Errorf("Meadow Core/runtime PID evidence is incomplete")
	}
	if strings.TrimSpace(s.RuntimeBackend) == "" || !strings.EqualFold(s.RuntimeBackend, identity.RuntimeBackend) {
		return fmt.Errorf("runtime backend evidence is inconsistent")
	}
	if strings.TrimSpace(identity.Architecture) == "" || strings.TrimSpace(identity.ArtifactPath) == "" {
		return fmt.Errorf("model architecture or artifact path is missing")
	}
	if identity.Quantization.Bits <= 0 || strings.TrimSpace(identity.Quantization.Method) == "" {
		return fmt.Errorf("model quantization evidence is incomplete")
	}
	return nil
}

type MeadowCoreLifecycleResult struct {
	Accepted bool   `json:"accepted"`
	Model    string `json:"model"`
	State    string `json:"state"`
	Unloaded bool   `json:"unloaded"`
	PID      int    `json:"pid"`
}

func ActivateMeadowCoreModel(ctx context.Context, baseURL, apiKey, modelID string) (MeadowCoreLifecycleResult, error) {
	var result MeadowCoreLifecycleResult
	err := meadowCoreRequest(ctx, http.MethodPost, baseURL, apiKey, "/models/load", map[string]any{
		"model": strings.TrimSpace(modelID),
	}, &result)
	return result, err
}

func UnloadMeadowCoreModel(ctx context.Context, baseURL, apiKey, modelID string) (MeadowCoreLifecycleResult, error) {
	body := map[string]any{}
	if model := strings.TrimSpace(modelID); model != "" {
		body["model"] = model
	}
	var result MeadowCoreLifecycleResult
	err := meadowCoreRequest(ctx, http.MethodPost, baseURL, apiKey, "/models/unload", body, &result)
	return result, err
}
