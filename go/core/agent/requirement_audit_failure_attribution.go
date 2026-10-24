package agent

import (
	"regexp"
	"sort"
	"strings"
)

type requirementAuditFailureAttribution struct {
	RequirementID string
	Field         string
	Violation     string
	Expected      string
	Actual        string
}

var (
	auditExpectedGotArrayRe = regexp.MustCompile(
		`(?i)AUDIT_UNCOVERED\s+R?[a-z_][a-z0-9_]*\s*:\s*expected\s*(\[[^\r\n]*\])\s+got\s*(\[[^\r\n]*\])`,
	)
	auditNamedExpectedGotArrayRe = regexp.MustCompile(
		`(?i)([a-z_][a-z0-9_]*)\s+mismatch\s*:\s*(\[[^\r\n]*\])\s+(?:vs|!=)\s*(\[[^\r\n]*\])`,
	)
	auditNamedGotExpectedArrayRe = regexp.MustCompile(
		`(?i)([a-z_][a-z0-9_]*)\s+mismatch\s*:\s*got\s*(\[[^\r\n]*\])\s*,?\s*expected\s*(\[[^\r\n]*\])`,
	)
	auditNamedArrayDifferenceRe = regexp.MustCompile(
		`(?i)\b([a-z_][a-z0-9_]*)\s*:\s*(\[[^\r\n]*\])\s*!=\s*(\[[^\r\n]*\])`,
	)
	auditRequirementArrayDifferenceRe = regexp.MustCompile(
		`(?i)\b(R[0-9]+)\s*:\s*(\[[^\r\n]*\])\s*!=\s*(\[[^\r\n]*\])`,
	)
	auditInvalidMarkerFieldRe = regexp.MustCompile(
		`(?i)AUDIT_UNCOVERED\s+R?([a-z_][a-z0-9_]*)\s*:`,
	)
	auditUncoveredOrderMarkerRe = regexp.MustCompile(
		`(?im)^AUDIT_UNCOVERED\s+R[0-9]+\s*:\s*([^\r\n]*)$`,
	)
	auditOrderLanguageRe = regexp.MustCompile(
		`(?i)(?:字典序|字典順序|lexicograph|dictionary\s+order|alphabetical|not\s+sorted|unsorted|sort(?:ed|ing)?\s+order)`,
	)
	auditDirectSortedValueRe = regexp.MustCompile(`(?i)\bsorted\s*\(`)
	auditQuotedArrayItemRe   = regexp.MustCompile(`['"]([^'"]*)['"]`)
)

// inferRequirementAuditFailure performs one deliberately narrow semantic
// attribution. If an executed checker reports two scalar-string arrays with
// identical membership, the expected array is dictionary-sorted, and the
// authoritative task applies its explicit conditional string-array default to
// that output field, then the observable failure is ordering rather than
// membership. This maps already-proved evidence to the structurally extracted
// requirement ID; it never computes an expected answer or guesses a value.
func inferRequirementAuditFailure(
	authoritativeTask string,
	items []RequirementAuditContractItem,
	script string,
	output string,
) requirementAuditFailureAttribution {
	defaultFields := semanticDefaultSortedArrayFields(authoritativeTask)
	field, expected, actual := auditArrayMismatch(items, defaultFields, script, output)
	markerOnly := false
	if field == "" {
		field = auditOrderMarkerField(defaultFields, script, output)
		markerOnly = field != ""
	}
	if field == "" || !stringSliceContains(defaultFields, field) {
		return requirementAuditFailureAttribution{}
	}
	if !markerOnly {
		if expected == "" || actual == "" || expected == actual {
			return requirementAuditFailureAttribution{}
		}
		expectedValues := auditQuotedArrayValues(expected)
		actualValues := auditQuotedArrayValues(actual)
		if len(expectedValues) == 0 || len(expectedValues) != len(actualValues) ||
			!sameStringMultiset(expectedValues, actualValues) ||
			!sort.StringsAreSorted(expectedValues) {
			return requirementAuditFailureAttribution{}
		}
	}
	requirementID := ""
	for _, item := range items {
		if hasSemanticStringArrayDefault(item.Quote) {
			requirementID = item.ID
			break
		}
	}
	if requirementID == "" {
		return requirementAuditFailureAttribution{}
	}
	return requirementAuditFailureAttribution{
		RequirementID: requirementID,
		Field:         field,
		Violation:     "order",
		Expected:      truncateUTF8(expected, 1000),
		Actual:        truncateUTF8(actual, 1000),
	}
}

// auditOrderMarkerField repairs only protocol metadata, not task meaning. A
// small model can prove a default-order mismatch and still attach the wrong R
// number. When the emitted clause names one structurally default-sorted field,
// explicitly describes ordering, and the executable checker compares that
// same field using a sorted-value predicate, bind the failure to the contract's
// conditional-default requirement. CheckToolCall has already rejected
// cross-field source-sort contamination before this function can run.
func auditOrderMarkerField(defaultFields []string, script, output string) string {
	comparedField := auditUniqueComparedDefaultField(script, defaultFields)
	if comparedField == "" || !auditDirectSortedValueRe.MatchString(stripNonCodeText(script)) {
		return ""
	}
	matchedField := ""
	for _, match := range auditUncoveredOrderMarkerRe.FindAllStringSubmatch(output, -1) {
		if len(match) != 2 || !auditOrderLanguageRe.MatchString(match[1]) {
			continue
		}
		clause := strings.ToLower(match[1])
		field := ""
		for _, candidate := range defaultFields {
			if !pythonIdentifierAppears(clause, candidate) {
				continue
			}
			if field != "" {
				field = ""
				break
			}
			field = candidate
		}
		if field == "" || !strings.EqualFold(field, comparedField) {
			continue
		}
		if matchedField != "" && !strings.EqualFold(matchedField, field) {
			return ""
		}
		matchedField = field
	}
	return matchedField
}

func auditArrayMismatch(
	items []RequirementAuditContractItem,
	defaultFields []string,
	script string,
	output string,
) (field, expected, actual string) {
	if match := auditExpectedGotArrayRe.FindStringSubmatch(output); len(match) == 3 {
		marker := auditInvalidMarkerFieldRe.FindStringSubmatch(output)
		if len(marker) == 2 {
			return strings.ToLower(marker[1]), match[1], match[2]
		}
	}
	if match := auditNamedExpectedGotArrayRe.FindStringSubmatch(output); len(match) == 4 {
		// Assertion messages from the model conventionally print actual first
		// (`answer[field]`) and independently recomputed expected second.
		return strings.ToLower(match[1]), match[3], match[2]
	}
	if match := auditNamedGotExpectedArrayRe.FindStringSubmatch(output); len(match) == 4 {
		return strings.ToLower(match[1]), match[3], match[2]
	}
	if match := auditNamedArrayDifferenceRe.FindStringSubmatch(output); len(match) == 4 {
		// Python assertion messages commonly use `field: actual != expected`.
		if !auditRequirementIDRe.MatchString(match[1]) {
			return strings.ToLower(match[1]), match[3], match[2]
		}
	}
	if match := auditRequirementArrayDifferenceRe.FindStringSubmatch(output); len(match) == 4 {
		field := auditContractFieldForID(items, defaultFields, strings.ToUpper(match[1]))
		if field == "" {
			field = auditUniqueComparedDefaultField(script, defaultFields)
		}
		// Compact checkers commonly emit `actual != expected`.
		return field, match[3], match[2]
	}
	return "", "", ""
}

// inferCorroboratedRequirementAuditOrderFailure handles a smaller model's
// second, correctly-executed checker when it repeats a proven order predicate
// but prints the membership requirement ID. A marker-only failure is not
// enough on its own: the executable checker must compare the named default-
// sorted field to a variable derived by sorted(...), and a prior independent
// checker must already have shown the same field has identical members in a
// different order. This preserves the two-check repair gate without trusting
// model-authored protocol metadata or treating a membership error as sorting.
func inferCorroboratedRequirementAuditOrderFailure(
	authoritativeTask string,
	items []RequirementAuditContractItem,
	script string,
	output string,
	priorEvidence map[string]requirementAuditEvidence,
) requirementAuditFailureAttribution {
	defaultFields := semanticDefaultSortedArrayFields(authoritativeTask)
	field := auditMarkerDefaultField(defaultFields, output)
	if field == "" || !auditScriptComparesFieldToSortedValue(script, field) {
		return requirementAuditFailureAttribution{}
	}
	var prior requirementAuditEvidence
	for _, evidence := range priorEvidence {
		if !credibleAuditFailure(evidence) || evidence.FailureViolation != "order" ||
			!strings.EqualFold(evidence.FailureField, field) ||
			evidence.FailureExpected == "" || evidence.FailureActual == "" {
			continue
		}
		prior = evidence
		break
	}
	if prior.CallID == "" {
		return requirementAuditFailureAttribution{}
	}
	requirementID := auditStringArrayDefaultRequirementID(items)
	if requirementID == "" {
		return requirementAuditFailureAttribution{}
	}
	return requirementAuditFailureAttribution{
		RequirementID: requirementID,
		Field:         field,
		Violation:     "order",
		Expected:      prior.FailureExpected,
		Actual:        prior.FailureActual,
	}
}

func auditMarkerDefaultField(defaultFields []string, output string) string {
	matched := ""
	for _, marker := range auditUncoveredOrderMarkerRe.FindAllStringSubmatch(output, -1) {
		if len(marker) != 2 {
			continue
		}
		field := ""
		for _, candidate := range defaultFields {
			if !pythonIdentifierAppears(marker[1], candidate) {
				continue
			}
			if field != "" {
				field = ""
				break
			}
			field = candidate
		}
		if field == "" {
			continue
		}
		if matched != "" && !strings.EqualFold(matched, field) {
			return ""
		}
		matched = field
	}
	return matched
}

func auditScriptComparesFieldToSortedValue(script, field string) bool {
	fieldPattern := regexp.QuoteMeta(field)
	access := `(?:[a-z_][a-z0-9_]*\.get\(\s*['"]` + fieldPattern + `['"]\s*\)|[a-z_][a-z0-9_]*\[\s*['"]` + fieldPattern + `['"]\s*\])`
	directLeft := regexp.MustCompile(`(?i)` + access + `\s*(?:!=|==)\s*sorted\s*\(`)
	directRight := regexp.MustCompile(`(?i)sorted\s*\([^\r\n]*\)\s*(?:!=|==)\s*` + access)
	if directLeft.MatchString(script) || directRight.MatchString(script) {
		return true
	}
	assignment := regexp.MustCompile(`(?im)^\s*([a-z_][a-z0-9_]*)\s*=\s*sorted\s*\(`)
	for _, match := range assignment.FindAllStringSubmatch(script, -1) {
		if len(match) == 2 && auditScriptComparesFieldToVariable(script, field, match[1]) {
			return true
		}
	}
	return false
}

func auditStringArrayDefaultRequirementID(items []RequirementAuditContractItem) string {
	for _, item := range items {
		if hasSemanticStringArrayDefault(item.Quote) {
			return item.ID
		}
	}
	return ""
}

func auditContractFieldForID(
	items []RequirementAuditContractItem,
	defaultFields []string,
	id string,
) string {
	quote := ""
	for _, item := range items {
		if strings.EqualFold(item.ID, id) {
			quote = strings.ToLower(item.Quote)
			break
		}
	}
	if quote == "" {
		return ""
	}
	matched := ""
	for _, field := range defaultFields {
		if !pythonIdentifierAppears(quote, field) {
			continue
		}
		if matched != "" {
			return ""
		}
		matched = field
	}
	return matched
}

func auditUniqueComparedDefaultField(script string, defaultFields []string) string {
	matched := ""
	for _, field := range defaultFields {
		quoted := regexp.QuoteMeta(field)
		access := regexp.MustCompile(`(?i)(?:\.get\(\s*['"]` + quoted + `['"]\s*\)|\[\s*['"]` + quoted + `['"]\s*\])\s*!=`)
		if !access.MatchString(script) {
			continue
		}
		if matched != "" {
			return ""
		}
		matched = field
	}
	return matched
}

func auditQuotedArrayValues(value string) []string {
	matches := auditQuotedArrayItemRe.FindAllStringSubmatch(value, -1)
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) == 2 {
			result = append(result, match[1])
		}
	}
	return result
}

func sameStringMultiset(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func stringSliceContains(values []string, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == target {
			return true
		}
	}
	return false
}
