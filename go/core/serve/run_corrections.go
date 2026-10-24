package serve

import (
	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

// Persist the public correction ledger beside the run's durable messages.
// Its terminal marker is written by the caller only after this succeeds.
func (sess *serveSession) persistVerificationCorrections(recap *agentcore.RunRecap) error {
	if sess.chat == nil || recap == nil || len(recap.Corrections) == 0 {
		return nil
	}
	receipts := make([]session.DisplayTestCorrection, 0, len(recap.Corrections))
	for _, correction := range recap.Corrections {
		receipts = append(receipts, session.DisplayTestCorrection{
			OriginalCallID: correction.OriginalCallID, ReplacementCallID: correction.ReplacementCallID,
			CorrectionCallID: correction.CorrectionCallID, Tool: correction.Tool,
			Reason: correction.Reason, Status: correction.Status,
			EvidenceCallIDs: append([]string(nil), correction.EvidenceCallIDs...),
		})
	}
	return sess.chat.VerificationCorrections(receipts)
}
