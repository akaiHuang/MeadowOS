package agent

import (
	"sort"
	"strings"
)

// requirementAuditInspectionGate keeps the independent checker behind one
// concrete artifact observation. Reading TASK.md/spec text is authoritative
// contract discovery, but it is not inspection of the artifact being judged.
// The gate compares only exact runtime paths and never interprets file content
// or chooses a checker.
type requirementAuditInspectionGate struct {
	candidates map[string]bool
	observed   map[string]bool
}

func newRequirementAuditInspectionGate(paths []string) *requirementAuditInspectionGate {
	gate := &requirementAuditInspectionGate{
		candidates: make(map[string]bool, len(paths)),
		observed:   make(map[string]bool, len(paths)),
	}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" {
			gate.candidates[path] = true
		}
	}
	return gate
}

func (g *requirementAuditInspectionGate) Complete() bool {
	return g == nil || len(g.candidates) == 0 || len(g.observed) > 0
}

func (g *requirementAuditInspectionGate) Observe(cwd string, execution ToolExecution) bool {
	if g == nil || !execution.Executed || execution.Status != ToolOK || execution.Effect != EffectRead {
		return false
	}
	path := kernelObservationPath(cwd, execution.Args)
	if path == "" || !g.candidates[path] {
		return false
	}
	g.observed[path] = true
	return true
}

func (g *requirementAuditInspectionGate) Missing() []string {
	if g == nil || g.Complete() {
		return nil
	}
	paths := make([]string, 0, len(g.candidates))
	for path := range g.candidates {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func requirementAuditArtifactPaths(changedPaths []string, deliverables []Deliverable) []string {
	const maxPaths = 24
	seen := make(map[string]bool, len(changedPaths)+len(deliverables))
	paths := make([]string, 0, minInt(len(changedPaths)+len(deliverables), maxPaths))
	appendPath := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] || len(paths) >= maxPaths {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}
	for _, path := range changedPaths {
		appendPath(path)
	}
	for _, deliverable := range deliverables {
		if deliverable.Exists {
			appendPath(deliverable.Path)
		}
	}
	sort.Strings(paths)
	return paths
}
