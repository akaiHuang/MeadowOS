package daemonruntime

import agentcore "github.com/hey-meadow/meadow-code/go/core/agent"

const recoveryStatusEvent = "recovery_status"

func mergeRecoveryStatus(dst *agentcore.RecoveryStatus, update agentcore.RecoveryStatus) {
	if update.SessionID != "" {
		dst.SessionID = update.SessionID
	}
	if update.ResumePlan != nil {
		plan := *update.ResumePlan
		dst.ResumePlan = &plan
	}
	if update.RecoveryWarning != "" {
		dst.RecoveryWarning = update.RecoveryWarning
	}
	dst.Degraded = dst.Degraded || update.Degraded
	dst.MutationFailClosed = dst.MutationFailClosed || update.MutationFailClosed
}

func cloneRecoveryStatus(status agentcore.RecoveryStatus) agentcore.RecoveryStatus {
	if status.ResumePlan != nil {
		plan := *status.ResumePlan
		status.ResumePlan = &plan
	}
	return status
}

func (runtime *Runtime) setRecoveryStatus(status agentcore.RecoveryStatus) {
	runtime.mu.Lock()
	mergeRecoveryStatus(&runtime.recovery, status)
	runtime.mu.Unlock()
}

// RecoveryStatus returns the latest deterministic plan and any explicit
// content-only degradation warning observed on the daemon SSE stream.
func (runtime *Runtime) RecoveryStatus() agentcore.RecoveryStatus {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return cloneRecoveryStatus(runtime.recovery)
}
