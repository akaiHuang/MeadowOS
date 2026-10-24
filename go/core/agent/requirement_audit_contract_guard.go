package agent

import (
	"regexp"
	"sort"
	"strings"
)

var (
	auditChineseExclusiveFieldsRe = regexp.MustCompile(`只需包含(?:此題)?(?:驗收)?欄位`)
	auditEnglishExclusiveFieldsRe = regexp.MustCompile(`(?i)(?:must|should)?\s*(?:only|contain only)\s+(?:contain\s+)?(?:the\s+)?(?:validation\s+|output\s+)?fields?`)
	auditJSONArtifactNameRe       = regexp.MustCompile(`(?i)[a-z0-9_.-]+\.json`)
)

type requirementAuditExclusiveFields struct {
	Artifact string
	Fields   []string
}

// requirementAuditOutOfContractOutputField rejects an acceptance checker that
// makes a field mandatory on a JSON artifact whose authoritative contract
// explicitly limits its output fields. It extracts names and access scope only;
// it does not infer values or decide whether a broader artifact is correct.
func requirementAuditOutOfContractOutputField(
	authoritativeTask string,
	args map[string]any,
) (artifact string, field string, allowed []string, violated bool) {
	contract := auditExclusiveJSONFields(authoritativeTask)
	if contract.Artifact == "" || len(contract.Fields) == 0 {
		return "", "", nil, false
	}
	script := strings.TrimSpace(stringArgument(args, "script"))
	if script == "" {
		return "", "", nil, false
	}
	loadedVariables := auditJSONArtifactVariables(script, contract.Artifact)
	if len(loadedVariables) == 0 {
		return "", "", nil, false
	}
	allowedSet := make(map[string]bool, len(contract.Fields))
	for _, name := range contract.Fields {
		allowedSet[strings.ToLower(name)] = true
	}
	for _, variable := range loadedVariables {
		for _, accessed := range auditPythonMappingFields(script, variable) {
			if !allowedSet[strings.ToLower(accessed)] {
				return contract.Artifact, accessed, append([]string(nil), contract.Fields...), true
			}
		}
	}
	return "", "", nil, false
}

func auditExclusiveJSONFields(task string) requirementAuditExclusiveFields {
	location := auditChineseExclusiveFieldsRe.FindStringIndex(task)
	if location == nil {
		location = auditEnglishExclusiveFieldsRe.FindStringIndex(task)
	}
	if location == nil {
		return requirementAuditExclusiveFields{}
	}
	prefixStart := location[0] - 240
	if prefixStart < 0 {
		prefixStart = 0
	}
	names := auditJSONArtifactNameRe.FindAllString(task[prefixStart:location[0]], -1)
	if len(names) == 0 {
		return requirementAuditExclusiveFields{}
	}
	artifact := strings.ToLower(names[len(names)-1])
	scope := task[location[1]:]
	if stop := strings.IndexAny(scope, "。\n"); stop >= 0 {
		// A line break immediately after the marker introduces the field clauses,
		// so only treat it as a boundary after non-whitespace content appears.
		for stop >= 0 && strings.TrimSpace(scope[:stop]) == "" && scope[stop] == '\n' {
			next := strings.IndexAny(scope[stop+1:], "。\n")
			if next < 0 {
				stop = -1
				break
			}
			stop += 1 + next
		}
		if stop >= 0 {
			scope = scope[:stop]
		}
	}
	seen := map[string]bool{}
	fields := make([]string, 0, 4)
	for _, match := range semanticOutputFieldClauseRe.FindAllStringSubmatch(scope, -1) {
		if len(match) != 4 {
			continue
		}
		field := strings.ToLower(strings.TrimSpace(match[1]))
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return requirementAuditExclusiveFields{Artifact: artifact, Fields: fields}
}

func auditJSONArtifactVariables(script, artifact string) []string {
	artifactPattern := regexp.QuoteMeta(artifact)
	seen := map[string]bool{}
	result := make([]string, 0, 2)
	appendVariable := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	withLoad := regexp.MustCompile(`(?is)with\s+open\(\s*['"][^'"]*` + artifactPattern + `['"][^)]*\)\s+as\s+([a-z_][a-z0-9_]*)\s*:[^\n]{0,160}?\b([a-z_][a-z0-9_]*)\s*=\s*json\.load\(\s*([a-z_][a-z0-9_]*)\s*\)`)
	for _, match := range withLoad.FindAllStringSubmatch(script, -1) {
		if len(match) == 4 && strings.EqualFold(match[1], match[3]) {
			appendVariable(match[2])
		}
	}
	directLoad := regexp.MustCompile(`(?im)\b([a-z_][a-z0-9_]*)\s*=\s*json\.load\(\s*open\(\s*['"][^'"]*` + artifactPattern + `['"]`)
	for _, match := range directLoad.FindAllStringSubmatch(script, -1) {
		if len(match) == 2 {
			appendVariable(match[1])
		}
	}
	pathLoad := regexp.MustCompile(`(?im)\b([a-z_][a-z0-9_]*)\s*=\s*json\.loads?\([^\r\n]*['"][^'"]*` + artifactPattern + `['"]`)
	for _, match := range pathLoad.FindAllStringSubmatch(script, -1) {
		if len(match) == 2 {
			appendVariable(match[1])
		}
	}
	return result
}

func auditPythonMappingFields(script, variable string) []string {
	variable = regexp.QuoteMeta(variable)
	pattern := regexp.MustCompile(`(?i)\b` + variable + `(?:\.get\(\s*['"]([a-z_][a-z0-9_]*)['"]|\[\s*['"]([a-z_][a-z0-9_]*)['"]\s*\])`)
	seen := map[string]bool{}
	result := make([]string, 0, 4)
	for _, match := range pattern.FindAllStringSubmatch(script, -1) {
		field := ""
		if len(match) > 1 {
			field = match[1]
		}
		if field == "" && len(match) > 2 {
			field = match[2]
		}
		field = strings.ToLower(strings.TrimSpace(field))
		if field != "" && !seen[field] {
			seen[field] = true
			result = append(result, field)
		}
	}
	return result
}
