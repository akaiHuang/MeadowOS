package agent

import (
	"regexp"
	"strings"
)

var (
	auditKeyedSortedAssignmentRe = regexp.MustCompile(
		`(?i)^\s*([a-z_][a-z0-9_]*)\s*=\s*sorted\(.*\bkey\s*=\s*lambda\s+[a-z_][a-z0-9_]*\s*:\s*(.+)\)\s*$`,
	)
	auditKeyedInPlaceSortRe = regexp.MustCompile(
		`(?i)\b([a-z_][a-z0-9_]*)\.sort\(.*\bkey\s*=\s*lambda\s+[a-z_][a-z0-9_]*\s*:\s*(.+)\)\s*$`,
	)
	auditPythonFieldAccessRe = regexp.MustCompile(`(?i)\[\s*['"]([a-z_][a-z0-9_]*)['"]\s*\]`)
	auditPythonAssignmentRe  = regexp.MustCompile(`(?i)^\s*([a-z_][a-z0-9_]*)\s*=\s*(.+)$`)
)

// requirementAuditCrossFieldSortViolation rejects a checker whose expected
// scalar array inherits record order from a differently named source sort key.
// It performs bounded data-flow over checker text only. Correct checkers may
// still sort presentation rows and independently scalar-sort output values;
// an explicit same-field ordering clause also remains authoritative.
func requirementAuditCrossFieldSortViolation(
	authoritativeTask string,
	args map[string]any,
) (outputField string, sourceField string, violated bool) {
	defaultFields := semanticDefaultSortedArrayFields(authoritativeTask)
	if len(defaultFields) == 0 {
		return "", "", false
	}
	script := strings.TrimSpace(stringArgument(args, "script"))
	if script == "" {
		return "", "", false
	}
	lines := auditPythonStatements(script)
	contaminated := map[string]string{}
	for _, line := range lines {
		if variable, source, ok := auditKeyedSort(line, auditKeyedSortedAssignmentRe); ok {
			contaminated[variable] = source
		}
		if variable, source, ok := auditKeyedSort(line, auditKeyedInPlaceSortRe); ok {
			contaminated[variable] = source
		}
	}
	for pass := 0; pass < 3; pass++ {
		changed := false
		for _, line := range lines {
			match := auditPythonAssignmentRe.FindStringSubmatch(line)
			if len(match) != 3 {
				continue
			}
			name := strings.ToLower(match[1])
			rhs := strings.TrimSpace(match[2])
			// A fresh scalar sorted(...) without a key restores the task's
			// default value ordering and must not inherit record contamination.
			if strings.HasPrefix(strings.ToLower(rhs), "sorted(") &&
				!strings.Contains(strings.ToLower(rhs), "key=") &&
				!strings.Contains(strings.ToLower(rhs), "key =") {
				continue
			}
			for upstream, source := range contaminated {
				if name != upstream && pythonIdentifierAppears(rhs, upstream) && contaminated[name] == "" {
					contaminated[name] = source
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	for _, field := range defaultFields {
		for variable, source := range contaminated {
			if auditScriptComparesFieldToVariable(script, field, variable) {
				return field, source, true
			}
		}
	}
	return "", "", false
}

// auditKeyedSort extracts the destination variable and the first named field
// read by a keyed lambda. The lambda may access the field directly
// (`lambda r: r['due']`) or through a lookup such as
// `lambda id: next(r['due'] for r in records if r['id'] == id)`. In both
// shapes the first field is the ordering source; later fields usually belong
// to the lookup predicate and must not hide it.
func auditKeyedSort(line string, pattern *regexp.Regexp) (variable, source string, ok bool) {
	match := pattern.FindStringSubmatch(line)
	if len(match) != 3 {
		return "", "", false
	}
	field := auditPythonFieldAccessRe.FindStringSubmatch(match[2])
	if len(field) != 2 {
		return "", "", false
	}
	return strings.ToLower(match[1]), strings.ToLower(field[1]), true
}

func semanticNamedSourceSortFields(task string) map[string]bool {
	fields := map[string]bool{}
	for _, match := range semanticNamedSourceSortKeyRe.FindAllStringSubmatch(task, -1) {
		if len(match) == 2 {
			fields[strings.ToLower(strings.TrimSpace(match[1]))] = true
		}
	}
	return fields
}

func auditPythonStatements(script string) []string {
	script = strings.ReplaceAll(script, "\r\n", "\n")
	statements := make([]string, 0, strings.Count(script, "\n")+1)
	for _, line := range strings.Split(script, "\n") {
		for _, statement := range strings.Split(line, ";") {
			statement = strings.TrimSpace(statement)
			if statement != "" && !strings.HasPrefix(statement, "#") {
				statements = append(statements, statement)
			}
		}
	}
	return statements
}

func pythonIdentifierAppears(value, identifier string) bool {
	pattern := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(identifier) + `\b`)
	return pattern.MatchString(value)
}

func auditScriptComparesFieldToVariable(script, field, variable string) bool {
	field = regexp.QuoteMeta(field)
	variable = regexp.QuoteMeta(variable)
	access := `(?:[a-z_][a-z0-9_]*\.get\(\s*['"]` + field + `['"]\s*\)|[a-z_][a-z0-9_]*\[\s*['"]` + field + `['"]\s*\])`
	left := regexp.MustCompile(`(?i)` + access + `\s*(?:!=|==)\s*` + variable + `\b`)
	right := regexp.MustCompile(`(?i)\b` + variable + `\s*(?:!=|==)\s*` + access)
	return left.MatchString(script) || right.MatchString(script)
}
