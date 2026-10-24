package agent

import (
	"regexp"
	"sort"
	"strings"
)

const (
	auditRuntimeMutationScope = "runtime:mutation_scope"
	auditRuntimeValidator     = "runtime:selected_validator"
	auditRuntimeInitialFail   = "runtime:initial_failed_validation"
	auditRuntimeInitialPass   = "runtime:initial_passing_validation"
	auditRuntimeRecovery      = "runtime:fail_mutate_revalidate"
	auditRuntimeAuditGuard    = "runtime:audit_before_mutation"
	auditRuntimeAuditProtocol = "runtime:independent_audit_protocol"
	auditRuntimeRepairPolicy  = "runtime:grounded_repair_lifecycle"
)

// BindRuntimeEvidence keeps deterministic workflow and safety facts out of the
// model-authored semantic checker. The model still owns every business-output
// predicate; Meadow only attests facts already enforced by the mutation scope,
// evidence ledger, and selected validator lifecycle.
func (l *RequirementAuditLedger) BindRuntimeEvidence(
	snapshot EvidenceSnapshot,
	mutationScope []string,
	selectedValidator string,
) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active {
		return
	}
	l.runtimeCoverage = make(map[string]string)
	trace := summarizeAuditRuntimeTrace(snapshot)
	scopeEnforced := auditChangedPathsWithinScope(snapshot.ChangedPaths, mutationScope)
	for _, item := range l.contractItems {
		switch requirementAuditRuntimeClauseKind(item.Quote) {
		case "mutation_scope":
			if scopeEnforced || auditMutationClauseCoveredByTrace(item.Quote, snapshot.ChangedPaths) {
				l.runtimeCoverage[item.ID] = auditRuntimeMutationScope
			}
		case "selected_validator":
			if trace.finalPass && strings.TrimSpace(selectedValidator) != "" {
				l.runtimeCoverage[item.ID] = auditRuntimeValidator
			}
		case "initial_failure":
			if trace.failedBeforeMutation {
				l.runtimeCoverage[item.ID] = auditRuntimeInitialFail
			}
		case "initial_pass":
			if trace.passedBeforeMutation && strings.TrimSpace(selectedValidator) != "" {
				l.runtimeCoverage[item.ID] = auditRuntimeInitialPass
			}
		case "verified_checkpoint":
			if trace.passedBeforeMutation && strings.TrimSpace(selectedValidator) != "" {
				l.runtimeCoverage[item.ID] = auditRuntimeValidator
			}
		case "no_artificial_failure":
			if trace.passedBeforeMutation && !trace.mutatedBeforeFirstValidation {
				l.runtimeCoverage[item.ID] = auditRuntimeInitialPass
			}
		case "recovery_sequence":
			if trace.failedBeforeMutation && trace.mutatedAfterFailure && trace.passedAfterMutation {
				l.runtimeCoverage[item.ID] = auditRuntimeRecovery
			}
		case "audit_before_mutation":
			// The requirement-audit state machine exposes mutation tools only after
			// independently corroborated, typed failing evidence.
			l.runtimeCoverage[item.ID] = auditRuntimeAuditGuard
		case "audit_protocol":
			// Completing the ledger already requires fresh executable evidence for
			// every remaining semantic target. This process clause is therefore
			// attested by the state machine rather than reimplemented in Python.
			l.runtimeCoverage[item.ID] = auditRuntimeAuditProtocol
		case "grounded_repair_lifecycle":
			// Kernel owns the mutation gate, selected-validator replay and saved
			// counterexample replay for every grounded repair generation.
			l.runtimeCoverage[item.ID] = auditRuntimeRepairPolicy
		}
	}
	l.refreshTypedAuditStateLocked()
}

// SemanticContractItems returns only clauses that still need independent
// model-authored evidence. Lifecycle, selected-validator and mutation-scope
// facts already attested by the runtime must never be turned back into repair
// targets because a checker happened to reuse their stable ID.
func (l *RequirementAuditLedger) SemanticContractItems() []RequirementAuditContractItem {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	items := make([]RequirementAuditContractItem, 0, len(l.contractItems))
	for _, item := range l.contractItems {
		if _, covered := l.runtimeCoverage[item.ID]; covered {
			continue
		}
		items = append(items, item)
	}
	return items
}

// RuntimeCoveredFailureLabel detects a checker-local failure label that points
// at a clause already proven by immutable runtime evidence. The label is a
// checker construction error, not new semantic evidence and never a repair
// authorization.
func (l *RequirementAuditLedger) RuntimeCoveredFailureLabel(output string) (string, string, bool) {
	if l == nil {
		return "", "", false
	}
	ids := make([]string, 0, 2)
	if match := auditUntypedRequirementAssertionRe.FindStringSubmatch(output); len(match) == 2 {
		ids = append(ids, strings.ToUpper(match[1]))
	}
	for _, line := range strings.Split(output, "\n") {
		if match := auditUncoveredMarkerRe.FindStringSubmatch(strings.TrimSpace(line)); len(match) == 2 {
			ids = append(ids, strings.ToUpper(match[1]))
		}
	}
	if len(ids) == 0 {
		return "", "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if source, covered := l.runtimeCoverage[id]; covered {
			return id, source, true
		}
	}
	return "", "", false
}

var auditWorkspacePathRe = regexp.MustCompile(
	`(?i)(?:[a-z0-9_.-]+/)*[a-z0-9_.-]+(?:\.[a-z0-9]+|/)`,
)

// auditMutationClauseCoveredByTrace attests only file-safety clauses whose
// named paths can be compared directly with the evidence ledger's complete
// changed-path set. It does not infer a business rule or an allowed artifact.
func auditMutationClauseCoveredByTrace(quote string, changedPaths []string) bool {
	text := strings.ToLower(strings.Join(strings.Fields(quote), " "))
	targets := auditWorkspacePathRe.FindAllString(quote, -1)
	if len(targets) == 0 {
		return false
	}
	allowOnly := containsAny(text,
		"只允許修改", "只能修改", "only modify", "may only modify",
	)
	deny := containsAny(text,
		"不得修改", "不可修改", "must not modify", "do not modify",
	)
	if !allowOnly && !deny {
		return false
	}
	if allowOnly {
		for _, changed := range changedPaths {
			matched := false
			for _, target := range targets {
				if auditPathWithinWorkspaceTarget(changed, target) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
		return true
	}
	for _, changed := range changedPaths {
		for _, target := range targets {
			if auditPathWithinWorkspaceTarget(changed, target) {
				return false
			}
		}
	}
	return true
}

type auditRuntimeTrace struct {
	failedBeforeMutation         bool
	mutatedAfterFailure          bool
	passedAfterMutation          bool
	passedBeforeMutation         bool
	mutatedBeforeFirstValidation bool
	finalPass                    bool
}

func summarizeAuditRuntimeTrace(snapshot EvidenceSnapshot) auditRuntimeTrace {
	result := auditRuntimeTrace{finalPass: snapshot.Verification == VerificationPassed}
	failed := false
	mutated := false
	validationSeen := false
	for _, entry := range snapshot.Entries {
		if entry.MutatesFile && entry.Status == ToolOK {
			if !validationSeen {
				result.mutatedBeforeFirstValidation = true
			}
			if failed {
				mutated = true
				result.mutatedAfterFailure = true
			}
			continue
		}
		if entry.Validation {
			validationSeen = true
		}
		if entry.Validation && entry.Verification == VerificationFailed && !mutated {
			failed = true
			result.failedBeforeMutation = true
			continue
		}
		if entry.Validation && entry.Verification == VerificationPassed {
			if mutated {
				result.passedAfterMutation = true
			} else if !result.mutatedBeforeFirstValidation {
				result.passedBeforeMutation = true
			}
		}
	}
	return result
}

func auditChangedPathsWithinScope(changedPaths, mutationScope []string) bool {
	if len(mutationScope) == 0 {
		return false
	}
	allowed := make([]string, 0, len(mutationScope))
	for _, path := range mutationScope {
		if path = normalizePath(path); path != "" {
			allowed = append(allowed, path)
		}
	}
	if len(allowed) == 0 {
		return false
	}
	for _, changed := range changedPaths {
		changed = normalizePath(changed)
		matched := false
		for _, path := range allowed {
			if auditPathsShareWorkspaceTarget(changed, path) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// auditPathsShareWorkspaceTarget compares the model-visible absolute
// /workspace path recorded by the evidence ledger with the workspace-relative
// path extracted from the user's exclusive mutation clause. Component-boundary
// suffix matching is intentionally limited to relative/absolute pairs; two
// unrelated absolute paths must never become equivalent merely because they
// share a filename.
func auditPathsShareWorkspaceTarget(changed, allowed string) bool {
	changed = normalizePath(changed)
	allowed = normalizePath(allowed)
	if changed == "" || allowed == "" {
		return false
	}
	if changed == allowed || strings.HasPrefix(changed, strings.TrimSuffix(allowed, "/")+"/") {
		return true
	}
	changedAbsolute := strings.HasPrefix(changed, "/")
	allowedAbsolute := strings.HasPrefix(allowed, "/")
	if changedAbsolute == allowedAbsolute {
		return false
	}
	if changedAbsolute {
		return strings.HasSuffix(changed, "/"+strings.TrimPrefix(allowed, "./"))
	}
	return strings.HasSuffix(allowed, "/"+strings.TrimPrefix(changed, "./"))
}

func auditPathWithinWorkspaceTarget(changed, target string) bool {
	changed = normalizePath(changed)
	target = normalizePath(strings.Trim(target, "`'\""))
	if changed == "" || target == "" {
		return false
	}
	if changed == target || strings.HasPrefix(changed, strings.TrimSuffix(target, "/")+"/") {
		return true
	}
	if strings.HasPrefix(changed, "/") && !strings.HasPrefix(target, "/") {
		relative := strings.TrimPrefix(target, "./")
		return strings.HasSuffix(changed, "/"+relative) ||
			strings.Contains(changed, "/"+strings.TrimSuffix(relative, "/")+"/")
	}
	return false
}

func requirementAuditRuntimeClauseKind(quote string) string {
	text := strings.ToLower(strings.Join(strings.Fields(quote), " "))
	if text == "" {
		return ""
	}
	if strings.Contains(text, "recovery_signal") ||
		containsAny(text, "讀懂", "修正根因", "after the failure", "after failure", "repair then", "fix the root cause") {
		if containsAny(text, "再執行", "重新執行", "rerun", "re-run", "revalidate", "re-validate") {
			return "recovery_sequence"
		}
	}
	if containsAny(text, "第一次動作", "首次動作", "first action", "first operation") &&
		containsAny(text, "失敗", "failure", "non-zero", "nonzero") {
		return "initial_failure"
	}
	if containsAny(text, "第一次動作", "首次動作", "第一個動作", "first action", "first operation") &&
		containsAny(text, "驗收", "validator", "validation", "acceptance", "test") &&
		containsAny(text, "通過", "成功", "fresh pass", "passing", "pass") {
		return "initial_pass"
	}
	if containsAny(text, "validator-passing checkpoint", "validator passing checkpoint", "驗收通過的 checkpoint", "驗證通過的 checkpoint") {
		return "verified_checkpoint"
	}
	if containsAny(text, "不得為了製造", "不可為了製造", "must not manufacture", "do not manufacture", "must not create", "do not create") &&
		containsAny(text, "失敗", "failure", "failing") &&
		containsAny(text, "修改", "改動", "mutate", "modify", "edit") {
		return "no_artificial_failure"
	}
	if containsAny(text, "獨立 audit", "independent audit") &&
		containsAny(text, "明示要求", "explicit requirement", "stated requirement") &&
		containsAny(text, "每一條", "每個", "every", "each") {
		return "audit_protocol"
	}
	if containsAny(text, "checker", "檢查器") &&
		containsAny(text, "具體失敗", "concrete failure", "specific failure") &&
		containsAny(text, "修改", "修正", "mutate", "modify", "repair") &&
		containsAny(text, "重跑", "再執行", "rerun", "re-run", "replay") {
		return "grounded_repair_lifecycle"
	}
	if containsAny(text, "只允許在", "只有在", "only after", "only when") &&
		containsAny(text, "修改", "修正", "mutate", "modify", "edit", "repair") &&
		containsAny(text, "audit", "證明", "evidence", "proof") {
		return "audit_before_mutation"
	}
	if containsAny(text, "驗收命令固定", "固定驗收命令", "validation command", "acceptance command") {
		return "selected_validator"
	}
	if containsAny(text,
		"只允許修改", "只能修改", "不得修改", "不可修改",
		"only modify", "must not modify", "do not modify", "may only modify",
	) && auditClauseMentionsWorkspacePath(text) {
		return "mutation_scope"
	}
	return ""
}

func auditClauseMentionsWorkspacePath(text string) bool {
	return strings.Contains(text, "`") || containsAny(text,
		".md", ".json", ".jsonl", ".csv", ".py", ".go", ".ts", ".js",
		"tests/", "test/", "src/", "input/", "output/",
	)
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func (l *RequirementAuditLedger) runtimeCoveredIDsLocked() []string {
	ids := make([]string, 0, len(l.runtimeCoverage))
	for id := range l.runtimeCoverage {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (l *RequirementAuditLedger) ChecksExhausted() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	unique := make(map[string]bool, len(l.pending)+len(l.evidence))
	for _, fingerprint := range l.pending {
		unique[fingerprint] = true
	}
	for _, evidence := range l.evidence {
		unique[evidence.Fingerprint] = true
	}
	if l.corroborationSlotAvailableLocked() {
		return false
	}
	return len(unique) >= l.checkBudgetLocked() ||
		len(l.checkerDrafts) >= maxRequirementAuditDrafts
}

func (l *RequirementAuditLedger) checkBudget() int {
	if l == nil {
		return maxRequirementAuditChecks
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checkBudgetLocked()
}

func (l *RequirementAuditLedger) checkBudgetLocked() int {
	budget := maxRequirementAuditChecks
	semanticTargets := 0
	for _, item := range l.contractItems {
		if _, runtimeCovered := l.runtimeCoverage[item.ID]; runtimeCovered {
			continue
		}
		semanticTargets++
	}
	// A small model may prove one atomic clause per checker. Reserve two
	// additional executable slots for one corrected oracle and the independent
	// corroboration required before a real artifact mutation. Keep a hard cap so
	// a long or over-split contract cannot turn audit into an unbounded loop.
	if needed := semanticTargets + 2; needed > budget {
		budget = needed
	}
	if budget > maxRequirementAuditChecksCap {
		budget = maxRequirementAuditChecksCap
	}
	return budget
}

func (l *RequirementAuditLedger) corroborationSlotAvailableLocked() bool {
	// A first grounded failure may arrive exactly at the ordinary draft limit
	// after bounded checker-construction corrections. Preserve one final slot
	// for the independently-shaped corroboration required to unlock repair.
	if l.mutationUnlocked || len(l.checkerDrafts) > maxRequirementAuditDrafts+1 {
		return false
	}
	partial := 0
	for _, failures := range l.failingCoverage {
		switch len(failures) {
		case 0:
		case 1:
			partial++
		default:
			return false
		}
	}
	return partial == 1
}
