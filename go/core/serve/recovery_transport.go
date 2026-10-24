package serve

import (
	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/sessionrecovery"
)

const recoveryStatusEvent = "recovery_status"

func recoveryPlanCallback(sess *serveSession) func(sessionrecovery.ResumePlan) {
	return func(plan sessionrecovery.ResumePlan) {
		planCopy := plan
		sess.broadcast(recoveryStatusEvent, agentcore.RecoveryStatus{
			SessionID:  sess.id,
			ResumePlan: &planCopy,
		})
	}
}

func publishRecoveryWarning(sess *serveSession, warning string) {
	if warning == "" {
		return
	}
	sess.broadcast(recoveryStatusEvent, agentcore.RecoveryStatus{
		SessionID:          sess.id,
		RecoveryWarning:    warning,
		Degraded:           true,
		MutationFailClosed: true,
	})
}
