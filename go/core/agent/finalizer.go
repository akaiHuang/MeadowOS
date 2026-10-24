package agent

import "strings"

// EvidenceSupport describes only whether the runtime evidence supports a
// model's completion claim. It is deliberately narrower than judging the
// quality or semantic correctness of the model's answer.
type EvidenceSupport string

const (
	EvidenceSupported   EvidenceSupport = "supported"
	EvidenceUnsupported EvidenceSupport = "unsupported"
	EvidenceUnverified  EvidenceSupport = "unverified"
	EvidenceNotClaimed  EvidenceSupport = "not_claimed"
)

type Deliverable struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

type FinalizeInput struct {
	LegacyStop   StopReason
	RunError     error
	Evidence     EvidenceSnapshot
	Deliverables []Deliverable
	// Validation is the selected verifier's decision. When validation is
	// required it is the authoritative interpretation of the raw evidence;
	// otherwise workspace verification still protects unverified code changes.
	Validation *ValidationDecision
	// RequirementAuditRequired is set only after a changed artifact has a
	// selected passing validator and enters the independent coverage phase.
	// Completion then fails closed until the fresh per-requirement ledger is
	// complete.
	RequirementAuditRequired bool
	RequirementAuditComplete bool
	// DeterministicDeliveryVerified is set only by a scoped product publisher
	// that validates its own complete output contract (for example, cited
	// research HTML/slides plus a non-empty PDF signature). It must never be set
	// from model prose or a generic file mutation.
	DeterministicDeliveryVerified bool
}

type FinalDecision struct {
	StopReason      StopReason      `json:"stop_reason"`
	EvidenceSupport EvidenceSupport `json:"evidence_support"`
	ClaimSupported  bool            `json:"claim_supported"`
	// CompletedTruth is retained for trace/API compatibility. New code should
	// use ClaimSupported: the evidence gate does not own semantic truth.
	CompletedTruth      bool               `json:"completed_truth"`
	Verification        VerificationStatus `json:"verification"`
	Reason              string             `json:"reason"`
	MissingDeliverables []string           `json:"missing_deliverables,omitempty"`
	LegacyAgreement     bool               `json:"legacy_agreement"`
}

// FinalizerIsAuthoritative is retained for trace/API compatibility. It means
// only that the kernel may enforce an evidence verdict; it does not give the
// runtime authority over the model's semantic answer.
func FinalizerIsAuthoritative(runtime string) bool {
	return strings.EqualFold(strings.TrimSpace(runtime), "kernel")
}

// EvidenceGateIsAuthoritative is the preferred name for the kernel-only
// evidence boundary. FinalizerIsAuthoritative remains as a compatibility API.
func EvidenceGateIsAuthoritative(runtime string) bool {
	return FinalizerIsAuthoritative(runtime)
}

// EvaluateEvidenceClaim is policy-only. It never runs a command, writes a
// file, or examines model prose. It decides only whether existing evidence
// supports the model/runtime completion claim.
func EvaluateEvidenceClaim(input FinalizeInput) FinalDecision {
	decision := FinalDecision{
		StopReason:      input.LegacyStop,
		Verification:    effectiveVerificationStatus(input),
		EvidenceSupport: EvidenceNotClaimed,
		Reason:          "legacy_non_completed",
	}
	if input.LegacyStop == StopCompleted {
		decision.EvidenceSupport = EvidenceSupported
		decision.ClaimSupported = true
		decision.CompletedTruth = true
	}

	if input.RunError != nil {
		decision.StopReason = "error"
		markUnsupported(&decision)
		decision.Reason = "run_error"
		return withAgreement(decision, input.LegacyStop)
	}

	// A clarification stop intentionally precedes deliverable creation. Missing
	// outputs are therefore expected state, not evidence that the run failed.
	// Preserve the product-facing reason so an interactive client can resume
	// after the user answers instead of presenting a false incomplete error.
	if input.LegacyStop == StopNeedsInput {
		decision.Reason = "needs_input"
		decision.EvidenceSupport = EvidenceNotClaimed
		decision.ClaimSupported = false
		decision.CompletedTruth = false
		return withAgreement(decision, input.LegacyStop)
	}

	for _, deliverable := range input.Deliverables {
		if !deliverable.Exists {
			decision.MissingDeliverables = append(decision.MissingDeliverables, deliverable.Path)
		}
	}
	if len(decision.MissingDeliverables) > 0 {
		decision.StopReason = StopIncomplete
		markUnsupported(&decision)
		decision.Reason = "missing_deliverable"
		return withAgreement(decision, input.LegacyStop)
	}

	if input.LegacyStop != StopCompleted {
		decision.ClaimSupported = false
		decision.CompletedTruth = false
		return withAgreement(decision, input.LegacyStop)
	}

	if input.RequirementAuditRequired && !input.RequirementAuditComplete {
		decision.StopReason = StopIncomplete
		markUnverified(&decision)
		decision.Reason = "requirement_audit_incomplete"
		return withAgreement(decision, input.LegacyStop)
	}

	// A post-pass mutation proposal can be policy-blocked without changing the
	// retained artifact. A later complete independent audit proves the retained
	// bytes; in that narrow case the rejected proposal must not poison an
	// otherwise fresh completion candidate.
	retainedCandidateAudited := input.RequirementAuditRequired && input.RequirementAuditComplete &&
		decision.Verification == VerificationPassed
	if LastMutationFailed(input.Evidence) && !retainedCandidateAudited {
		decision.StopReason = StopIncomplete
		markUnsupported(&decision)
		decision.Reason = "last_mutation_failed"
		return withAgreement(decision, input.LegacyStop)
	}

	switch decision.Verification {
	case VerificationFailed:
		decision.StopReason = StopIncomplete
		markUnsupported(&decision)
		decision.Reason = "verification_failed"
	case VerificationStale:
		decision.StopReason = StopIncomplete
		markUnverified(&decision)
		decision.Reason = "verification_stale_after_edit"
	case VerificationUnverified:
		decision.StopReason = StopIncomplete
		markUnverified(&decision)
		decision.Reason = "code_changed_without_fresh_verification"
	case VerificationPassed:
		decision.Reason = "fresh_verification_passed"
	case VerificationNotRequired:
		decision.Reason = "verification_not_required"
	default:
		decision.StopReason = StopIncomplete
		markUnverified(&decision)
		decision.Reason = "unknown_verification_state"
	}
	return withAgreement(decision, input.LegacyStop)
}

func effectiveVerificationStatus(input FinalizeInput) VerificationStatus {
	if input.DeterministicDeliveryVerified {
		return VerificationPassed
	}
	if input.Validation != nil && input.Validation.Required {
		if input.Validation.Status == VerificationPassed && !input.Validation.Fresh {
			return VerificationUnverified
		}
		return input.Validation.Status
	}
	return input.Evidence.Verification
}

// LastMutationFailed reports a structural run fact. A later read-only call
// cannot turn an unsuccessful mutation into a successful one; a later
// successful mutation or passing validator does supersede it.
func LastMutationFailed(snapshot EvidenceSnapshot) bool {
	mutationIndex := -1
	for index := len(snapshot.Entries) - 1; index >= 0; index-- {
		if snapshot.Entries[index].MutatesFile {
			mutationIndex = index
			break
		}
	}
	if mutationIndex < 0 || snapshot.Entries[mutationIndex].Status == ToolOK {
		return false
	}
	// A rejected or unsuccessful mutation attempt does not change the
	// workspace by definition. A later validator that passes against the
	// retained workspace state therefore supersedes that failed attempt.
	// Keep the failure authoritative when no later passing validator exists.
	for index := mutationIndex + 1; index < len(snapshot.Entries); index++ {
		entry := snapshot.Entries[index]
		if entry.Validation && entry.Status == ToolOK &&
			entry.Verification == VerificationPassed {
			return false
		}
	}
	return true
}

// Finalize remains as a compatibility wrapper while callers migrate to the
// narrower evidence-gate terminology.
func Finalize(input FinalizeInput) FinalDecision {
	return EvaluateEvidenceClaim(input)
}

func markUnsupported(decision *FinalDecision) {
	decision.EvidenceSupport = EvidenceUnsupported
	decision.ClaimSupported = false
	decision.CompletedTruth = false
}

func markUnverified(decision *FinalDecision) {
	decision.EvidenceSupport = EvidenceUnverified
	decision.ClaimSupported = false
	decision.CompletedTruth = false
}

func withAgreement(decision FinalDecision, legacyStop StopReason) FinalDecision {
	legacyCompleted := legacyStop == StopCompleted
	decision.LegacyAgreement = decision.StopReason == legacyStop && decision.CompletedTruth == legacyCompleted
	return decision
}
