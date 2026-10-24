package engine

import (
	"context"
	"fmt"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// ApprovalMode is an explicit tool confirmation policy. It never changes tool
// validation, account admission, or workspace boundaries.
type ApprovalMode string

const (
	ModePlan     ApprovalMode = "plan"
	ModeDefault  ApprovalMode = "default"
	ModeAutoEdit ApprovalMode = "auto-edit"
	ModeYolo     ApprovalMode = "yolo"
	// ModeFullAccess is an explicit deployment/user grant, including destructive
	// tool calls. It is not the default for ordinary Meadow products.
	ModeFullAccess ApprovalMode = "full-access"
)

func ParseApprovalMode(s string) (ApprovalMode, error) {
	switch ApprovalMode(s) {
	case ModePlan, ModeDefault, ModeAutoEdit, ModeYolo, ModeFullAccess:
		return ApprovalMode(s), nil
	case "":
		return ModeDefault, nil
	}
	return "", fmt.Errorf("unknown approval mode %q (plan|default|auto-edit|yolo|full-access)", s)
}

// Decision is the user's answer to a confirmation request.
type Decision int

const (
	DecisionDeny Decision = iota
	DecisionApprove
	// DecisionApproveAlways approves and stops asking for this tool for the
	// rest of the session.
	DecisionApproveAlways
)

// ConfirmRequest is what the frontend shows for one pending call.
type ConfirmRequest struct {
	ToolName string
	Args     map[string]any
	Kind     tools.Kind
}

// Confirmer asks the user. nil means non-interactive: calls that need
// confirmation are denied (TS parity for non-interactive runs).
type Confirmer func(ctx context.Context, req ConfirmRequest) (Decision, error)

// approvalVerdict is the engine-internal outcome of the policy gate.
type approvalVerdict int

const (
	verdictAuto approvalVerdict = iota
	verdictNeedsConfirmation
	verdictPlanBlocked
)

// classify retains the existing four confirmation modes and adds an explicit
// full-access grant:
//   - read-only ("info") tools pass every mode, including plan;
//   - ask_user_question is the interaction channel itself and never gets a
//     second confirmation layer;
//   - plan mode hard-blocks anything mutating;
//   - auto-edit auto-approves edit-kind calls;
//   - yolo auto-approves non-destructive work; destructive calls still ask.
//   - full-access auto-approves every kind, including destructive calls.
func classify(mode ApprovalMode, kind tools.Kind) approvalVerdict {
	if mode == ModeFullAccess {
		return verdictAuto
	}
	if kind == tools.KindReadOnly || kind == tools.KindInteract {
		return verdictAuto
	}
	// Without an explicit full-access grant, destructive calls confirm even in
	// yolo. Plan mode still blocks first.
	if kind == tools.KindDestructive {
		if mode == ModePlan {
			return verdictPlanBlocked
		}
		return verdictNeedsConfirmation
	}
	switch mode {
	case ModeYolo:
		return verdictAuto
	case ModePlan:
		return verdictPlanBlocked
	case ModeAutoEdit:
		if kind == tools.KindEdit {
			return verdictAuto
		}
		return verdictNeedsConfirmation
	default:
		return verdictNeedsConfirmation
	}
}
