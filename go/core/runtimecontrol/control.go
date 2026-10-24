// Package runtimecontrol defines the model/runtime lifecycle boundary owned by
// meadow-agentd. Presentation clients and producthost depend on this boundary,
// not on Meadow Core endpoints or backend launch details.
package runtimecontrol

import (
	"context"
	"fmt"
	"strings"
)

// LifecycleResult is the transport-neutral result returned after one model
// lifecycle operation reaches a terminal state.
type LifecycleResult struct {
	Accepted bool   `json:"accepted"`
	Model    string `json:"model"`
	State    string `json:"state"`
	Unloaded bool   `json:"unloaded"`
	PID      int    `json:"pid"`
}

type QuantizationStatus struct {
	Method    string `json:"method"`
	Bits      int    `json:"bits"`
	GroupSize int    `json:"group_size"`
}

type ModelIdentityStatus struct {
	CanonicalID      string             `json:"canonical_id"`
	ArtifactIdentity string             `json:"artifact_identity,omitempty"`
	ArtifactPath     string             `json:"artifact_path"`
	ArtifactName     string             `json:"artifact_name"`
	Architecture     string             `json:"architecture"`
	ModelType        string             `json:"model_type"`
	RuntimeBackend   string             `json:"runtime_backend"`
	RuntimePID       int                `json:"runtime_pid"`
	IdentityVerified bool               `json:"identity_verified"`
	Multimodal       bool               `json:"multimodal"`
	Quantization     QuantizationStatus `json:"quantization"`
}

type ResidentModelStatus struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	ActiveRequests int    `json:"active_requests"`
	QueuedRequests int    `json:"queued_requests"`
}

type ManagedModelStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type ModelManagerStatus struct {
	Models []ManagedModelStatus `json:"models"`
}

// Status is the shared lifecycle status contract. Native implementations must
// leave evidence fields empty rather than inventing values absent from the ABI.
type Status struct {
	Status              string              `json:"status"`
	RuntimeBackend      string              `json:"runtime_backend"`
	CorePID             int                 `json:"core_pid"`
	ContextWindowTokens int                 `json:"context_window_tokens"`
	MaxOutputTokens     int                 `json:"max_output_tokens,omitempty"`
	ModelIdentity       ModelIdentityStatus `json:"model_identity"`
	Model               ResidentModelStatus `json:"model"`
	ModelManager        ModelManagerStatus  `json:"model_manager"`
}

func (status Status) ResidentModel() (string, bool) {
	state := strings.ToLower(strings.TrimSpace(status.Model.State))
	ready := state == "ready" || state == "loaded"
	return strings.TrimSpace(status.Model.ID), ready
}

func (status Status) VerifyRuntimeIdentity(expectedModel string) error {
	expected := strings.TrimSpace(expectedModel)
	resident, ready := status.ResidentModel()
	if !ready {
		return fmt.Errorf("Meadow Runtime model is not ready")
	}
	if expected != "" && !strings.EqualFold(resident, expected) {
		return fmt.Errorf("resident model %q does not match requested model %q", resident, expected)
	}
	identity := status.ModelIdentity
	if !identity.IdentityVerified {
		return fmt.Errorf("Meadow Runtime did not verify the local model artifact identity")
	}
	if !strings.EqualFold(strings.TrimSpace(identity.CanonicalID), resident) {
		return fmt.Errorf("artifact identity %q does not match resident model %q", identity.CanonicalID, resident)
	}
	if status.CorePID <= 0 || identity.RuntimePID <= 0 {
		return fmt.Errorf("Meadow Runtime PID evidence is incomplete")
	}
	if strings.TrimSpace(status.RuntimeBackend) == "" || !strings.EqualFold(status.RuntimeBackend, identity.RuntimeBackend) {
		return fmt.Errorf("runtime backend evidence is inconsistent")
	}
	artifactEvidence := strings.TrimSpace(identity.ArtifactIdentity)
	if artifactEvidence == "" {
		// Legacy/public Core reports a local artifact path. Protected builds use
		// the signed opaque artifact identity and never expose a physical path.
		artifactEvidence = strings.TrimSpace(identity.ArtifactPath)
	}
	if strings.TrimSpace(identity.Architecture) == "" || artifactEvidence == "" {
		return fmt.Errorf("model architecture or artifact identity is missing")
	}
	if identity.Quantization.Bits <= 0 || strings.TrimSpace(identity.Quantization.Method) == "" {
		return fmt.Errorf("model quantization evidence is incomplete")
	}
	return nil
}

// RuntimeControl is the lifecycle seam required by meadow-agentd. It excludes
// inference, backend launch arguments, transport addresses and artifact paths.
type RuntimeControl interface {
	Ensure(context.Context) error
	Activate(context.Context, string) (LifecycleResult, error)
	Unload(context.Context, string) (LifecycleResult, error)
	Status(context.Context) (Status, bool, error)
	Close() error
}
