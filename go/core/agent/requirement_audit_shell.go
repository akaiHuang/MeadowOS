package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// requirementAuditShell is the capability-reduced view of the normal shell
// exposed only while an independent requirement audit is active. The model
// supplies Python source; Meadow owns the interpreter, exit contract, working
// copy and read-only isolation. A dedicated public name keeps small models
// from mistaking raw checker source for a shell command while the delegate
// remains the normal shell implementation.
type requirementAuditShell struct {
	delegate tools.Tool
	ledger   *RequirementAuditLedger
}

func newRequirementAuditShell(delegate tools.Tool, ledger ...*RequirementAuditLedger) tools.Tool {
	var activeLedger *RequirementAuditLedger
	if len(ledger) > 0 {
		activeLedger = ledger[0]
	}
	return &requirementAuditShell{delegate: delegate, ledger: activeLedger}
}

func (t *requirementAuditShell) Name() string {
	if t.structured() {
		return RequirementAuditCheckToolName
	}
	return DefaultShellTool
}

func (t *requirementAuditShell) structured() bool {
	return t != nil && t.ledger != nil
}

func (t *requirementAuditShell) Description() string {
	if t.structured() {
		return "Run raw Python assertions for an independent requirement audit in an isolated read-only workspace. This is not a shell: never send python3, unittest commands, redirects, or shell syntax. Select requirement_ids when the schema advertises them."
	}
	return "Run one isolated read-only Python requirement checker. Meadow fixes the interpreter, validation exit contract and temporary workspace."
}

func (t *requirementAuditShell) Schema() map[string]any {
	properties := map[string]any{
		"script": map[string]any{
			"type":        "string",
			"description": "Raw Python source only, for example `from src.task import run\nassert run() == expected`. Never put `python3`, a unittest/pytest command, shell operators, redirects, or AUDIT protocol markers here.",
		},
	}
	required := []string{"script"}
	if t != nil && t.ledger != nil {
		items := t.ledger.SemanticContractItems()
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		if len(ids) > 0 {
			properties["requirement_ids"] = map[string]any{
				"type":        "array",
				"description": "Stable IDs visibly asserted by this checker. Select several only when one script independently asserts every selected clause; after a failure, retry with exactly one localized ID.",
				"items": map[string]any{
					"type": "string",
					"enum": ids,
				},
				"minItems":    1,
				"uniqueItems": true,
			}
			required = append(required, "requirement_ids")
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func (t *requirementAuditShell) Kind() tools.Kind { return tools.KindExecute }

func (t *requirementAuditShell) Execute(ctx context.Context, args map[string]any) tools.Result {
	if t == nil || t.delegate == nil {
		err := errors.New("independent requirement checker is not configured")
		return tools.Result{LLMContent: "ERROR: " + err.Error(), Display: err.Error(), Err: err}
	}
	script, _ := args["script"].(string)
	if strings.TrimSpace(script) == "" {
		err := errors.New("independent requirement checker script is required")
		return tools.Result{LLMContent: "ERROR: " + err.Error(), Display: err.Error(), Err: err}
	}
	return t.delegate.Execute(ctx, map[string]any{
		"command":                       "python3",
		"script":                        script,
		"purpose":                       "validation",
		"expected_exit_codes":           []int{0},
		tools.ReadOnlyWorkspaceArgument: true,
	})
}

func requirementAuditRegistry(
	registry *tools.Registry, ledger ...*RequirementAuditLedger,
) *tools.Registry {
	if registry == nil {
		return tools.NewRegistry()
	}
	// Register directly rather than using Registry.Select: the normal shell has
	// a process companion, but background process control is deliberately absent
	// from the bounded independent-audit capability surface.
	selected := tools.NewRegistry()
	for _, name := range requirementAuditToolNames(registry.Names()) {
		tool, ok := registry.Get(name)
		if !ok {
			continue
		}
		if name == DefaultShellTool {
			selected.Register(newRequirementAuditShell(tool, ledger...))
			continue
		}
		selected.Register(tool)
	}
	return selected
}

func requirementAuditInspectionRegistry(
	registry *tools.Registry, ledger ...*RequirementAuditLedger,
) *tools.Registry {
	full := requirementAuditRegistry(registry, ledger...)
	names := make([]string, 0, len(full.Names()))
	for _, name := range full.Names() {
		if name == DefaultShellTool || name == RequirementAuditCheckToolName ||
			name == RequirementAuditToolName {
			continue
		}
		names = append(names, name)
	}
	return full.Select(names)
}

// requirementAuditCheckRegistry is the final capability reduction after any
// mandatory artifact inspection has completed and the ledger advertises stable
// requirement IDs. At this boundary the model still authors every predicate and
// selects every ID; the Kernel only prevents another read/list loop from
// replacing the protocol action that records executable acceptance evidence.
func requirementAuditCheckRegistry(
	registry *tools.Registry, ledger ...*RequirementAuditLedger,
) *tools.Registry {
	return requirementAuditRegistry(registry, ledger...).Select([]string{
		RequirementAuditCheckToolName,
	})
}

func requirementAuditProtocolRegistry(
	registry *tools.Registry, ledger *RequirementAuditLedger,
	inspection *requirementAuditInspectionGate,
) *tools.Registry {
	if inspection != nil && !inspection.Complete() {
		return requirementAuditInspectionRegistry(registry, ledger)
	}
	if ledger == nil {
		return requirementAuditRegistry(registry)
	}
	// Structured requirement_ids bind checker results directly into the ledger,
	// so the only remaining protocol action is another checker until the audit
	// completes or a grounded repair is unlocked.
	if len(ledger.SemanticContractItems()) > 0 {
		return requirementAuditCheckRegistry(registry, ledger)
	}

	snapshot := ledger.Snapshot()
	// Legacy/no-ID audits need one executable checker followed by a ledger
	// attestation. Invalid drafts do not enter EvidenceChecks; one failing check
	// needs an independently-shaped second check before an uncovered claim is
	// eligible for submission.
	if !snapshot.Submitted {
		if snapshot.EvidenceChecks == 0 ||
			(len(snapshot.FailingCallIDs) > 0 && len(snapshot.FailingCallIDs) < minimumUncoveredEvidenceChecks) {
			return requirementAuditCheckRegistry(registry, ledger)
		}
		return requirementAuditRegistry(registry, ledger).Select([]string{
			RequirementAuditToolName,
		})
	}
	if requirementAuditNeedsLedgerCorrection(snapshot.MissingEvidence) ||
		requirementAuditNeedsTargetedUncovered(snapshot.MissingEvidence) {
		return requirementAuditRegistry(registry, ledger).Select([]string{
			RequirementAuditToolName,
		})
	}
	return requirementAuditCheckRegistry(registry, ledger)
}

var _ tools.Tool = (*requirementAuditShell)(nil)
