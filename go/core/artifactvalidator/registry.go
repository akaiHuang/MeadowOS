// Package artifactvalidator provides capability-driven validation and optional
// mechanical repair for files produced by an agent. It does not select tasks,
// call models, or decide run completion.
package artifactvalidator

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

type Status string

const (
	StatusNotApplicable Status = "not_applicable"
	StatusPassed        Status = "passed"
	StatusFailed        Status = "failed"
	StatusRepaired      Status = "repaired"
)

type Capability string

const (
	CapabilityMechanicalRepair Capability = "mechanical_repair"
	CapabilityWebRuntimeRepair Capability = "web_runtime_repair"
)

type Request struct {
	Path         string
	Capabilities []Capability
}

type Result struct {
	Status              Status       `json:"status"`
	Path                string       `json:"path"`
	ValidatorID         string       `json:"validator_id,omitempty"`
	Error               string       `json:"error,omitempty"`
	RepairID            string       `json:"repair_id,omitempty"`
	Detail              string       `json:"detail,omitempty"`
	Capability          Capability   `json:"capability,omitempty"`
	EnabledCapabilities []Capability `json:"enabled_capabilities,omitempty"`
}

func (r Result) Acceptable() bool {
	return r.Status == StatusPassed || r.Status == StatusRepaired || r.Status == StatusNotApplicable
}

type ValidateFunc func(path string) string
type RepairFunc func(path, validationError string) (detail string, repaired bool)

type Repairer struct {
	ID         string
	Capability Capability
	Repair     RepairFunc
}

type Registry struct {
	validate   ValidateFunc
	repairers  []Repairer
	extensions map[string]bool
}

func NewRegistry(validate ValidateFunc, extensions []string, repairers ...Repairer) *Registry {
	registry := &Registry{validate: validate, extensions: map[string]bool{}}
	for _, extension := range extensions {
		extension = strings.ToLower(strings.TrimSpace(extension))
		if extension != "" && !strings.HasPrefix(extension, ".") {
			extension = "." + extension
		}
		if extension != "" {
			registry.extensions[extension] = true
		}
	}
	for _, repairer := range repairers {
		if strings.TrimSpace(repairer.ID) != "" && repairer.Repair != nil {
			registry.repairers = append(registry.repairers, repairer)
		}
	}
	return registry
}

func DefaultRegistry() *Registry {
	return NewRegistry(tools.ValidateSyntax,
		[]string{".js", ".mjs", ".cjs", ".html", ".htm", ".py", ".json"},
		Repairer{ID: "html_runtime_batch", Capability: CapabilityWebRuntimeRepair, Repair: tools.RepairHTMLRuntimeIssues},
		Repairer{ID: "threejs_cdn", Capability: CapabilityWebRuntimeRepair, Repair: tools.RepairBadThreeJSCDN},
		Repairer{ID: "runtime_import", Capability: CapabilityWebRuntimeRepair, Repair: tools.RepairMissingRuntimeImport},
		Repairer{ID: "threejs_fog_density", Capability: CapabilityWebRuntimeRepair, Repair: tools.RepairThreeJSFogDensity},
		Repairer{ID: "threejs_camera_aim", Capability: CapabilityWebRuntimeRepair, Repair: tools.RepairThreeJSCameraAim},
		Repairer{ID: "foreign_token", Capability: CapabilityMechanicalRepair, Repair: tools.RepairForeignToken},
	)
}

func (r *Registry) Validate(request Request) Result {
	path := filepath.Clean(strings.TrimSpace(request.Path))
	result := Result{Path: path, EnabledCapabilities: normalizedCapabilities(request.Capabilities)}
	if r == nil || r.validate == nil || path == "." || strings.TrimSpace(request.Path) == "" {
		result.Status = StatusNotApplicable
		return result
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		result.Status = StatusFailed
		result.Error = "artifact does not exist or is not a regular file"
		return result
	}
	extension := strings.ToLower(filepath.Ext(path))
	if !r.extensions[extension] {
		result.Status = StatusNotApplicable
		return result
	}
	result.ValidatorID = "syntax" + extension
	validationError := r.validate(path)
	if validationError == "" {
		result.Status = StatusPassed
		return result
	}
	result.Status = StatusFailed
	result.Error = validationError
	capabilities := capabilitySet(result.EnabledCapabilities)
	for _, repairer := range r.repairers {
		if !capabilities[repairer.Capability] {
			continue
		}
		if detail, repaired := repairer.Repair(path, validationError); repaired {
			result.Status = StatusRepaired
			result.RepairID = repairer.ID
			result.Detail = detail
			result.Capability = repairer.Capability
			result.Error = ""
			return result
		}
	}
	return result
}

func normalizedCapabilities(values []Capability) []Capability {
	seen := make(map[Capability]bool, len(values))
	result := make([]Capability, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func capabilitySet(values []Capability) map[Capability]bool {
	result := make(map[Capability]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}
