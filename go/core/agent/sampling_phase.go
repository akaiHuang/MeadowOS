package agent

import "github.com/hey-meadow/meadow-code/go/core/provider"

// kernelSamplingPhase maps only already-observed lifecycle state. Semantic
// task interpretation remains model-owned; the phase merely lets the provider
// choose a precision-appropriate sampling envelope.
func kernelSamplingPhase(auditPending, repairPending, auditSatisfied, recovery, auditRecovery bool) provider.SamplingPhase {
	switch {
	case auditSatisfied:
		return provider.SamplingPhaseFinal
	case repairPending:
		return provider.SamplingPhaseRepair
	case auditPending && auditRecovery:
		return provider.SamplingPhaseAuditRecovery
	case auditPending:
		return provider.SamplingPhaseAudit
	case recovery:
		return provider.SamplingPhaseRecovery
	default:
		return provider.SamplingPhaseSolve
	}
}

func routeSamplingPhase(lifecycle, selected provider.SamplingPhase) provider.SamplingPhase {
	if lifecycle == provider.SamplingPhaseSolve && selected != "" {
		return selected
	}
	return lifecycle
}
