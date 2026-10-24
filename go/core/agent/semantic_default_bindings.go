package agent

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

var (
	semanticChineseStringArrayDefaultRe = regexp.MustCompile(
		`未(?:另外|另行)?明示排序鍵的字串陣列(?:才)?依字典序`,
	)
	semanticEnglishStringArrayDefaultRe = regexp.MustCompile(
		`(?i)string arrays?\s+(?:without|with no)\s+(?:an?\s+)?explicit sort key[^.;\n]*(?:lexicographic|dictionary)`,
	)
	semanticOutputFieldClauseRe = regexp.MustCompile(
		`(?i)\b([a-z][a-z0-9_]*)\s*(是|=|means|is|依|按)\s*([^；;\n。]+)`,
	)
	semanticNamedSourceSortKeyRe = regexp.MustCompile(
		`(?i)\bsort_([a-z][a-z0-9_]*)_(?:ascending|descending)\b`,
	)
)

type semanticDefaultBindingPayload struct {
	Rule           string   `json:"rule"`
	ScalarArrays   []string `json:"scalar_string_array_fields"`
	SourceSortKeys []string `json:"source_sort_keys_not_field_bindings"`
}

// appendSemanticDefaultBindings turns an explicit conditional default from
// the raw task into field-level bindings. The model-authored semantic contract
// remains useful for ambiguity review, but a small model can otherwise copy a
// global default verbatim and then accidentally apply a source-record sort key
// to every derived array. This layer extracts names and scope only; it never
// computes task values or expected artifact contents.
func appendSemanticDefaultBindings(task, reviewedContract string) string {
	reviewedContract = strings.TrimSpace(reviewedContract)
	if reviewedContract == "" || !hasSemanticStringArrayDefault(task) {
		return reviewedContract
	}
	fields := semanticDefaultSortedArrayFields(task)
	if len(fields) == 0 {
		return reviewedContract
	}
	payload := semanticDefaultBindingPayload{
		Rule:         "sort each listed scalar string array by its scalar values unless that field's own authoritative clause names a different key",
		ScalarArrays: fields,
		SourceSortKeys: compactSemanticSortKeys(
			semanticNamedSourceSortKeys(task),
		),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return reviewedContract
	}
	return reviewedContract + "\n\n# Deterministic conditional-default bindings\n" +
		"This JSON is a mechanical field-scope restatement of the raw task, not a new requirement and not an expected answer. Apply it when computing artifacts and when deriving every validator or independent audit checker. A source sort key named for another field may order source-record presentation, but cannot replace the listed scalar-field default unless the raw task explicitly links them.\n" +
		"<field_default_bindings_json>" + string(encoded) + "</field_default_bindings_json>"
}

func hasSemanticStringArrayDefault(task string) bool {
	return semanticChineseStringArrayDefaultRe.MatchString(task) ||
		semanticEnglishStringArrayDefaultRe.MatchString(task)
}

func semanticDefaultSortedArrayFields(task string) []string {
	seen := map[string]bool{}
	fields := make([]string, 0, 4)
	for _, match := range semanticOutputFieldClauseRe.FindAllStringSubmatch(task, -1) {
		if len(match) != 4 {
			continue
		}
		field := strings.ToLower(strings.TrimSpace(match[1]))
		connector := strings.ToLower(strings.TrimSpace(match[2]))
		clause := strings.ToLower(strings.TrimSpace(match[3]))
		if !looksLikeSemanticStringArrayField(field, clause) ||
			hasFieldSpecificSortBinding(connector, clause) || seen[field] {
			continue
		}
		seen[field] = true
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func looksLikeSemanticStringArrayField(field, clause string) bool {
	if strings.Contains(clause, "整數陣列") || strings.Contains(clause, "數值陣列") ||
		strings.Contains(clause, "物件陣列") || strings.Contains(clause, "二維陣列") ||
		strings.Contains(clause, "number array") || strings.Contains(clause, "integer array") ||
		strings.Contains(clause, "object array") || strings.Contains(clause, "nested array") {
		return false
	}
	return strings.HasSuffix(field, "_ids") || strings.HasSuffix(field, "_keys") ||
		strings.HasSuffix(field, "_paths") ||
		strings.Contains(clause, "字串陣列") || strings.Contains(clause, "去重後陣列") ||
		strings.Contains(clause, "陣列") || strings.Contains(clause, "列表") ||
		strings.Contains(clause, "string array") || strings.Contains(clause, " list") ||
		strings.Contains(clause, "unique owners")
}

func hasFieldSpecificSortBinding(connector, clause string) bool {
	if connector == "依" || connector == "按" {
		return true
	}
	return strings.Contains(clause, "排序") || strings.Contains(clause, "字典序") ||
		strings.Contains(clause, "lexicograph") || strings.Contains(clause, "dictionary order") ||
		strings.Contains(clause, " sort") || strings.Contains(clause, " order") ||
		strings.Contains(clause, " due")
}

func semanticNamedSourceSortKeys(task string) []string {
	keys := make([]string, 0, 2)
	for _, match := range semanticNamedSourceSortKeyRe.FindAllString(task, -1) {
		keys = append(keys, strings.ToLower(strings.TrimSpace(match)))
	}
	return keys
}

func compactSemanticSortKeys(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if value == "" || (len(result) > 0 && result[len(result)-1] == value) {
			continue
		}
		result = append(result, value)
	}
	return result
}
