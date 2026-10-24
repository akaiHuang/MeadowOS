//go:build !meadow_protected

package runtimecontrol

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/coretransport"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const legacyDefaultCoreBaseURL = "http://127.0.0.1:18081/v1"

type LegacyCoreOptions struct {
	BaseURL      string
	InitialModel string
}

// LegacyCoreAdapter preserves the development Core process and HTTP behavior.
// It is not compiled into meadow_protected builds.
type LegacyCoreAdapter struct {
	ensureMu     sync.Mutex
	mu           sync.Mutex
	baseURL      string
	initialModel string
	supervisor   *coretransport.Supervisor
	closed       bool
}

// NormalizeLegacyCoreBaseURL keeps lifecycle and inference on the same Core
// endpoint when a presentation client starts the helper without a URL flag.
// A generic provider's default is not a Meadow Core endpoint.
func NormalizeLegacyCoreBaseURL(value string) string {
	baseURL := strings.TrimRight(strings.TrimSpace(value), "/")
	if baseURL == "" {
		baseURL = legacyDefaultCoreBaseURL
	}
	return baseURL
}

func NewLegacyCoreAdapter(options LegacyCoreOptions) *LegacyCoreAdapter {
	baseURL := NormalizeLegacyCoreBaseURL(options.BaseURL)
	return &LegacyCoreAdapter{baseURL: baseURL, initialModel: strings.TrimSpace(options.InitialModel)}
}

func (adapter *LegacyCoreAdapter) Ensure(ctx context.Context) error {
	// Model activation and the background warm-up are allowed to converge on
	// Ensure concurrently. Serialize the slow launch boundary so two callers
	// cannot both observe a nil supervisor and race two Core processes onto the
	// same loopback port.
	adapter.ensureMu.Lock()
	defer adapter.ensureMu.Unlock()

	adapter.mu.Lock()
	if adapter.closed {
		adapter.mu.Unlock()
		return errors.New("runtime control is closed")
	}
	if adapter.supervisor != nil {
		adapter.mu.Unlock()
		return nil
	}
	baseURL := adapter.baseURL
	adapter.mu.Unlock()
	// Desktop startup owns only the lightweight Core service. The selected
	// model is activated by an explicit model switch or lazily by the first
	// prompt; merely opening the app must not allocate model weights.
	supervisor, err := coretransport.Ensure(ctx, baseURL, "")
	if err != nil {
		return err
	}
	adapter.mu.Lock()
	if adapter.closed {
		adapter.mu.Unlock()
		_ = supervisor.Stop()
		return errors.New("runtime control closed while Meadow Core was starting")
	}
	if adapter.supervisor != nil {
		adapter.mu.Unlock()
		_ = supervisor.Stop()
		return nil
	}
	adapter.supervisor = supervisor
	adapter.mu.Unlock()
	return nil
}

func (adapter *LegacyCoreAdapter) Activate(ctx context.Context, modelID string) (LifecycleResult, error) {
	if err := adapter.Ensure(ctx); err != nil {
		return LifecycleResult{}, err
	}
	supervisor, err := adapter.currentSupervisor()
	if err != nil {
		return LifecycleResult{}, err
	}
	result, err := supervisor.Activate(ctx, modelID)
	return lifecycleResultFromLegacy(result), err
}

func (adapter *LegacyCoreAdapter) Unload(ctx context.Context, modelID string) (LifecycleResult, error) {
	if _, err := adapter.currentSupervisor(); err != nil {
		return LifecycleResult{}, err
	}
	result, err := provider.UnloadMeadowCoreModel(ctx, adapter.baseURL, "", modelID)
	return lifecycleResultFromLegacy(result), err
}

func (adapter *LegacyCoreAdapter) Status(ctx context.Context) (Status, bool, error) {
	supervisor, err := adapter.currentSupervisor()
	if err != nil {
		return Status{}, false, err
	}
	status, ok, err := supervisor.Status(ctx)
	return statusFromLegacy(status), ok, err
}

func (adapter *LegacyCoreAdapter) Close() error {
	adapter.mu.Lock()
	if adapter.closed {
		adapter.mu.Unlock()
		return nil
	}
	adapter.closed = true
	supervisor := adapter.supervisor
	adapter.supervisor = nil
	adapter.mu.Unlock()
	if supervisor == nil {
		return nil
	}
	return supervisor.Stop()
}

func (adapter *LegacyCoreAdapter) currentSupervisor() (*coretransport.Supervisor, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.closed {
		return nil, errors.New("runtime control is closed")
	}
	if adapter.supervisor == nil {
		return nil, errors.New("runtime control has not been ensured")
	}
	return adapter.supervisor, nil
}

func lifecycleResultFromLegacy(source provider.MeadowCoreLifecycleResult) LifecycleResult {
	return LifecycleResult{Accepted: source.Accepted, Model: source.Model, State: source.State, Unloaded: source.Unloaded, PID: source.PID}
}

func statusFromLegacy(source provider.MeadowCoreStatus) Status {
	status := Status{
		Status: source.Status, RuntimeBackend: source.RuntimeBackend, CorePID: source.CorePID,
		ContextWindowTokens: source.ContextWindowTokens,
		MaxOutputTokens:     source.MaxOutputTokens,
		ModelIdentity: ModelIdentityStatus{
			CanonicalID: source.ModelIdentity.CanonicalID, ArtifactPath: source.ModelIdentity.ArtifactPath,
			ArtifactName: source.ModelIdentity.ArtifactName, Architecture: source.ModelIdentity.Architecture,
			ModelType: source.ModelIdentity.ModelType, RuntimeBackend: source.ModelIdentity.RuntimeBackend,
			RuntimePID: source.ModelIdentity.RuntimePID, IdentityVerified: source.ModelIdentity.IdentityVerified,
			Multimodal:   source.ModelIdentity.Multimodal,
			Quantization: QuantizationStatus{Method: source.ModelIdentity.Quantization.Method, Bits: source.ModelIdentity.Quantization.Bits, GroupSize: source.ModelIdentity.Quantization.GroupSize},
		},
		Model:        ResidentModelStatus{ID: source.Model.ID, State: source.Model.State, ActiveRequests: source.Model.ActiveRequests, QueuedRequests: source.Model.QueuedRequests},
		ModelManager: ModelManagerStatus{Models: make([]ManagedModelStatus, 0, len(source.ModelManager.Models))},
	}
	for _, model := range source.ModelManager.Models {
		status.ModelManager.Models = append(status.ModelManager.Models, ManagedModelStatus{ID: model.ID, Status: model.Status})
	}
	return status
}

var _ RuntimeControl = (*LegacyCoreAdapter)(nil)
