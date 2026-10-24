package producthost

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
)

const HarnessPolicyRegistrySchema = "meadow.harness-policy.registry.v1"

// HarnessPolicyProfile configures technical execution capabilities. It has no
// task classifier, task-specific route, tool sequence, or answer-rewriting rule.
// A candidate is inert until an explicit offline promotion changes Active.
type HarnessPolicyProfile struct {
	ID                           string                `json:"id"`
	ModelID                      string                `json:"model_id"`
	ArtifactRetrieval            ArtifactRetrievalMode `json:"artifact_retrieval"`
	RunHandoffReserveEnabled     bool                  `json:"run_handoff_reserve_enabled"`
	HandoffReserveMs             int64                 `json:"handoff_reserve_ms"`
	HandoffMaxOutputTokens       int                   `json:"handoff_max_output_tokens"`
	RevisionBoundEvidenceEnabled bool                  `json:"revision_bound_evidence_enabled"`
}

func (p *HarnessPolicyProfile) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	keys := []string{"id", "model_id", "artifact_retrieval", "run_handoff_reserve_enabled", "handoff_reserve_ms", "handoff_max_output_tokens", "revision_bound_evidence_enabled"}
	if len(fields) != len(keys) {
		return fmt.Errorf("all explicit harness profile fields are required")
	}
	for _, key := range keys {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("explicit non-null harness field %q is required", key)
		}
	}
	type profileAlias HarnessPolicyProfile
	var decoded profileAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*p = HarnessPolicyProfile(decoded)
	return nil
}

type HarnessPolicyRegistry struct {
	Schema    string                 `json:"schema"`
	Profiles  []HarnessPolicyProfile `json:"profiles"`
	Active    map[string]string      `json:"active"`
	Candidate map[string]string      `json:"candidate,omitempty"`
}

func (p HarnessPolicyProfile) Fingerprint() string {
	encoded, _ := json.Marshal(p)
	hash := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func defaultHarnessPolicy(modelID string) HarnessPolicyProfile {
	return HarnessPolicyProfile{
		ID: "builtin-v1", ModelID: strings.TrimSpace(modelID),
		ArtifactRetrieval:        artifactRetrievalForModel(modelID, ArtifactRetrievalAuto),
		RunHandoffReserveEnabled: true, RevisionBoundEvidenceEnabled: true,
	}
}

// Resolve selects only an exact model identity from the frozen active map.
// Unknown models retain the existing metadata-based retrieval default.
func (r *HarnessPolicyRegistry) Resolve(modelID string) HarnessPolicyProfile {
	modelID = strings.TrimSpace(modelID)
	if r != nil {
		if id, ok := r.Active[modelID]; ok {
			for _, profile := range r.Profiles {
				if profile.ID == id && profile.ModelID == modelID {
					return profile
				}
			}
		}
	}
	return defaultHarnessPolicy(modelID)
}

func validHarnessPolicyID(value string) bool {
	if value == "" || len(value) > 96 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func (r *HarnessPolicyRegistry) validate() error {
	if r.Schema != HarnessPolicyRegistrySchema || len(r.Profiles) == 0 || len(r.Profiles) > 128 {
		return fmt.Errorf("invalid harness policy schema or profile count")
	}
	profiles := make(map[string]HarnessPolicyProfile, len(r.Profiles))
	for _, p := range r.Profiles {
		if !validHarnessPolicyID(p.ID) || strings.TrimSpace(p.ModelID) != p.ModelID || p.ModelID == "" || len(p.ModelID) > 200 {
			return fmt.Errorf("invalid harness profile identity")
		}
		if _, duplicate := profiles[p.ID]; duplicate {
			return fmt.Errorf("duplicate harness profile %q", p.ID)
		}
		if p.ArtifactRetrieval == "" || p.ArtifactRetrieval == ArtifactRetrievalAuto {
			return fmt.Errorf("profile %q must declare an explicit retrieval mode", p.ID)
		}
		if _, err := parseArtifactRetrievalMode(string(p.ArtifactRetrieval)); err != nil {
			return err
		}
		if p.HandoffReserveMs < 0 || p.HandoffReserveMs > 120000 || p.HandoffMaxOutputTokens < 0 || p.HandoffMaxOutputTokens > 8192 {
			return fmt.Errorf("profile %q exceeds handoff configuration bounds", p.ID)
		}
		profiles[p.ID] = p
	}
	for _, pointers := range []map[string]string{r.Active, r.Candidate} {
		if len(pointers) > 128 {
			return fmt.Errorf("too many harness policy pointers")
		}
		for modelID, id := range pointers {
			p, ok := profiles[id]
			if !ok || p.ModelID != modelID {
				return fmt.Errorf("harness policy pointer has missing or mismatched model")
			}
		}
	}
	return nil
}

// LoadHarnessPolicyRegistry is fail-closed for explicit files. An empty path
// means built-in defaults. The host loads once, never while a task is running.
func LoadHarnessPolicyRegistry(path string) (*HarnessPolicyRegistry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect harness policy: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 256<<10 {
		return nil, fmt.Errorf("harness policy must be a bounded regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read harness policy: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var registry HarnessPolicyRegistry
	if err := decoder.Decode(&registry); err != nil {
		return nil, fmt.Errorf("decode harness policy: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("harness policy contains trailing data")
	}
	if err := registry.validate(); err != nil {
		return nil, err
	}
	return &registry, nil
}

type harnessPolicyObserver struct {
	observer agenttrace.Observer
	profile  HarnessPolicyProfile
}

// WithHarnessPolicyObservation records the actual runtime selection. Selection
// is separate from proof that a mechanism was exercised during this run.
func WithHarnessPolicyObservation(observer agenttrace.Observer, profile HarnessPolicyProfile) agenttrace.Observer {
	if observer == nil {
		return nil
	}
	return harnessPolicyObserver{observer: observer, profile: profile}
}

func (o harnessPolicyObserver) Observe(event agenttrace.Event) {
	if event.Type == agenttrace.EventRunStarted || event.Type == agenttrace.EventModelRequested {
		fields := make(map[string]any, len(event.Fields)+5)
		for key, value := range event.Fields {
			fields[key] = value
		}
		fields["harness_policy_id"] = o.profile.ID
		fields["harness_policy_fingerprint"] = o.profile.Fingerprint()
		fields["handoff_reserve_enabled"] = o.profile.RunHandoffReserveEnabled
		fields["revision_bound_evidence_enabled"] = o.profile.RevisionBoundEvidenceEnabled
		fields["artifact_retrieval_mode"] = string(o.profile.ArtifactRetrieval)
		event.Fields = fields
	}
	o.observer.Observe(event)
}

func (o harnessPolicyObserver) ObserveReport(report agenttrace.Report) {
	o.observer.ObserveReport(report)
}
