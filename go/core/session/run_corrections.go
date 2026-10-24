package session

// DisplayTestCorrection is a model-declared disposition of a failed check.
// It preserves the failed/replacement evidence identities and public rationale;
// it is not a passing result or independent verification of that rationale.
type DisplayTestCorrection struct {
	OriginalCallID    string   `json:"originalCallId"`
	ReplacementCallID string   `json:"replacementCallId"`
	CorrectionCallID  string   `json:"correctionCallId"`
	Tool              string   `json:"tool"`
	Reason            string   `json:"reason"`
	EvidenceCallIDs   []string `json:"evidenceCallIds"`
	Status            string   `json:"status"`
}

// VerificationCorrections records only structured ledger receipts. The next
// terminal marker binds them to a visible run outcome; model history omits them.
func (w *ChatWriter) VerificationCorrections(corrections []DisplayTestCorrection) error {
	if len(corrections) == 0 {
		return nil
	}
	return w.append(chatRecord{Type: "verification_corrections", VerificationCorrections: corrections})
}
