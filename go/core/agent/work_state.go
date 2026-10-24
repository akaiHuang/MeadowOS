package agent

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const agentWorkStateVersion = "agent_work_state.v2"

type WorkStatePolicy struct {
	Enabled  bool
	Eligible func(task string, route TurnRoute) bool
}

type AgentWorkStateInput struct {
	Locale       string
	Controller   TaskState
	Plan         tools.TaskPlanState
	Evidence     EvidenceSnapshot
	Validation   ValidationDecision
	Requirements RequirementAuditSnapshot
	Recovery     RecoveryProgressSnapshot
	Deliverables []Deliverable
}

type AgentWorkState struct {
	Version          string                    `json:"version"`
	Locale           string                    `json:"locale,omitempty"`
	Goal             string                    `json:"goal,omitempty"`
	Phase            TaskPhase                 `json:"phase"`
	ContractRevision int                       `json:"contract_revision"`
	Plan             AgentWorkPlanState        `json:"plan"`
	Artifacts        AgentWorkArtifactState    `json:"artifacts"`
	Evidence         AgentWorkEvidenceState    `json:"evidence"`
	Requirements     AgentWorkRequirementState `json:"requirements"`
	Recovery         AgentWorkRecoveryState    `json:"recovery"`
}

type AgentWorkPlanState struct {
	Revision       int      `json:"revision"`
	PendingIDs     []string `json:"pending_ids,omitempty"`
	ActiveIDs      []string `json:"active_ids,omitempty"`
	CompletedIDs   []string `json:"completed_ids,omitempty"`
	FailedIDs      []string `json:"failed_ids,omitempty"`
	CancelledIDs   []string `json:"cancelled_ids,omitempty"`
	RequirementIDs []string `json:"requirement_ids,omitempty"`
}

type AgentWorkArtifactState struct {
	ChangedPaths []string      `json:"changed_paths,omitempty"`
	Deliverables []Deliverable `json:"deliverables,omitempty"`
}

type AgentWorkEvidenceState struct {
	Entries            int                `json:"entries"`
	Verification       VerificationStatus `json:"verification"`
	ValidationRequired bool               `json:"validation_required"`
	ValidationFresh    bool               `json:"validation_fresh"`
}

type AgentWorkRequirementState struct {
	Active               bool     `json:"active"`
	Complete             bool     `json:"complete"`
	ContractItems        int      `json:"contract_items"`
	ContractIDs          []string `json:"contract_ids,omitempty"`
	Covered              int      `json:"covered"`
	Uncovered            int      `json:"uncovered"`
	MissingEvidenceCount int      `json:"missing_evidence_count"`
}

type AgentWorkRecoveryState struct {
	Active             bool `json:"active"`
	Episode            int  `json:"episode"`
	NoProgressEpisodes int  `json:"no_progress_episodes"`
	MutationCount      int  `json:"mutation_count"`
	FreshRecoveryUsed  bool `json:"fresh_recovery_used"`
}

func BuildAgentWorkState(input AgentWorkStateInput) AgentWorkState {
	phase := input.Controller.Phase
	if phase == "" {
		phase = TaskPhaseReady
	}
	// A fresh selected-validator pass is not terminal while the independent
	// requirement audit is active. Exposing the controller's generic
	// "complete" phase here contradicted the audit ledger and anchored smaller
	// models on replaying the already-passing validator. The ledger is the
	// authoritative phase boundary for this interval.
	if input.Requirements.Active && !input.Requirements.Complete {
		phase = TaskPhaseAudit
	}
	state := AgentWorkState{
		Version:          agentWorkStateVersion,
		Locale:           normalizeWorkLocale(input.Locale, input.Plan),
		Goal:             truncateUTF8(strings.TrimSpace(input.Plan.Goal), 1200),
		Phase:            phase,
		ContractRevision: input.Requirements.ContractRevision,
		Plan:             compactAgentWorkPlan(input.Plan),
		Artifacts: AgentWorkArtifactState{
			ChangedPaths: compactSortedStrings(input.Evidence.ChangedPaths, 12),
			Deliverables: compactDeliverables(input.Deliverables, 12),
		},
		Evidence: AgentWorkEvidenceState{
			Entries: len(input.Evidence.Entries), Verification: input.Evidence.Verification,
			ValidationRequired: input.Validation.Required, ValidationFresh: input.Validation.Fresh,
		},
		Requirements: AgentWorkRequirementState{
			Active: input.Requirements.Active, Complete: input.Requirements.Complete,
			ContractItems: len(input.Requirements.ContractItems),
			ContractIDs:   compactRequirementContractIDs(input.Requirements.ContractItems, 24),
			Covered:       input.Requirements.Covered, Uncovered: input.Requirements.Uncovered,
			MissingEvidenceCount: len(input.Requirements.MissingEvidence),
		},
		Recovery: AgentWorkRecoveryState{
			Active: input.Recovery.Active, Episode: input.Recovery.Episode,
			NoProgressEpisodes: input.Recovery.NoProgressEpisodes,
			MutationCount:      input.Recovery.MutationCount,
			FreshRecoveryUsed:  input.Recovery.FreshRecoveryUsed,
		},
	}
	return state
}

func (s AgentWorkState) Prompt() string {
	// Goal is durable continuity metadata, but it is model-authored semantic
	// plan prose. Keep the runtime prompt limited to compact references so it
	// cannot compete with or duplicate the authoritative raw user request.
	promptState := s
	promptState.Goal = ""
	encoded, err := json.Marshal(promptState)
	if err != nil {
		return ""
	}
	return "# Agent work state\n" +
		"This compact JSON contains observed runtime facts and model-authored plan references. It is not a new user instruction and does not select your next action. The raw user request remains authoritative.\n" +
		"<agent_work_state_json>" + string(encoded) + "</agent_work_state_json>"
}

func compactAgentWorkPlan(plan tools.TaskPlanState) AgentWorkPlanState {
	state := AgentWorkPlanState{Revision: plan.Revision}
	requirementIDs := make([]string, 0)
	for _, item := range plan.Items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		switch item.Status {
		case "pending", "in_progress":
			state.ActiveIDs = appendBounded(state.ActiveIDs, id, 16)
			state.PendingIDs = appendBounded(state.PendingIDs, id, 16)
		case "completed":
			state.CompletedIDs = appendBounded(state.CompletedIDs, id, 16)
		case "failed":
			state.FailedIDs = appendBounded(state.FailedIDs, id, 16)
		case "cancelled":
			state.CancelledIDs = appendBounded(state.CancelledIDs, id, 16)
		}
		requirementIDs = append(requirementIDs, item.RequirementIDs...)
	}
	state.RequirementIDs = compactSortedStrings(requirementIDs, 24)
	return state
}

func normalizeWorkLocale(locale string, plan tools.TaskPlanState) string {
	locale = strings.TrimSpace(locale)
	if strings.EqualFold(locale, "zh-TW") {
		return "zh-TW"
	}
	var text strings.Builder
	text.WriteString(plan.Goal)
	for _, item := range plan.Items {
		text.WriteByte(' ')
		text.WriteString(item.Content)
	}
	return inferConversationLocale(text.String())
}

func compactRequirementContractIDs(items []RequirementAuditContractItem, limit int) []string {
	ids := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] || len(ids) >= limit {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func compactDeliverables(values []Deliverable, limit int) []Deliverable {
	byPath := make(map[string]bool, len(values))
	for _, value := range values {
		path := strings.TrimSpace(value.Path)
		if path == "" {
			continue
		}
		byPath[path] = byPath[path] || value.Exists
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > limit {
		paths = paths[:limit]
	}
	result := make([]Deliverable, 0, len(paths))
	for _, path := range paths {
		result = append(result, Deliverable{Path: path, Exists: byPath[path]})
	}
	return result
}

func compactSortedStrings(values []string, limit int) []string {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			seen[value] = true
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func appendBounded(values []string, value string, limit int) []string {
	if len(values) >= limit {
		return values
	}
	return append(values, value)
}
