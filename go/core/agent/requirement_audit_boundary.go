package agent

import (
	"regexp"
	"strings"
)

type requirementAuditBoundaryObligation struct {
	Field            string
	Value            string
	CollectionValues bool
}

var requirementAuditCollectionBoundaryPatterns = []*regexp.Regexp{
	regexp.MustCompile("(?i)[`]?([a-z_][a-z0-9_]*)[`]?\\s*(?:的值|values?)\\s*(?:必須|需|應|must\\s+be|are\\s+required\\s+to\\s+be)\\s*(?:嚴格|strictly\\s+)?(?:大於|高於|超過|greater\\s+than|more\\s+than|above|>)\\s*(-?[0-9]+(?:\\.[0-9]+)?)"),
	regexp.MustCompile("(?i)(?:values?\\s+(?:of|in)\\s+)[`]?([a-z_][a-z0-9_]*)[`]?\\s*(?:must\\s+be|are\\s+required\\s+to\\s+be)\\s*(?:strictly\\s+)?(?:greater\\s+than|more\\s+than|above|>)\\s*(-?[0-9]+(?:\\.[0-9]+)?)"),
}

var requirementAuditBoundaryPatterns = []*regexp.Regexp{
	// Explicit strict numeric boundaries. Exercising only an allowed interior
	// value cannot prove these clauses; the named threshold itself is the
	// smallest task-agnostic fixture that distinguishes > from >= (or < from <=).
	regexp.MustCompile(`(?i)[` + "`" + `]?([a-z_][a-z0-9_]*)[` + "`" + `]?\s*(?:must\s+be\s+)?(?:strictly\s+)?(?:greater\s+than|more\s+than|above|>)\s*(-?[0-9]+(?:\.[0-9]+)?)`),
	regexp.MustCompile(`(?i)[` + "`" + `]?([a-z_][a-z0-9_]*)[` + "`" + `]?\s*(?:must\s+be\s+)?(?:strictly\s+)?(?:less\s+than|below|<)\s*(-?[0-9]+(?:\.[0-9]+)?)`),
	regexp.MustCompile("(?i)[`]?([a-z_][a-z0-9_]*)[`]?\\s*(?:必須|需|應)\\s*(?:嚴格)?(?:大於|高於|超過)\\s*(-?[0-9]+(?:\\.[0-9]+)?)"),
	regexp.MustCompile("(?i)[`]?([a-z_][a-z0-9_]*)[`]?\\s*(?:必須|需|應)\\s*(?:嚴格)?(?:小於|低於)\\s*(-?[0-9]+(?:\\.[0-9]+)?)"),
}

var requirementAuditPositiveBoundaryPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)[` + "`" + `]?([a-z_][a-z0-9_]*)[` + "`" + `]?\s*(?:must\s+be|is\s+required\s+to\s+be)\s+(?:strictly\s+)?positive\b`),
	regexp.MustCompile("(?i)[`]?([a-z_][a-z0-9_]*)[`]?\\s*(?:必須|需|應)(?:為|是)?(?:嚴格)?(?:正數|正值)"),
}

var requirementAuditForbiddenSideErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile("(?i)[`]?([a-z_][a-z0-9_]*)[`]?\\s*(?:<|>|less\\s+than|greater\\s+than|below|above)\\s*-?[0-9]+(?:\\.[0-9]+)?\\s*(?:時|則|即|者)\\s*(?:必須|需|應)?\\s*(?:拋出|丟出|回傳|返回)?\\s*(?:ValueError|error|錯誤|無效|拒絕)"),
	regexp.MustCompile(`(?i)\b(?:if|when)\s+[` + "`" + `]?([a-z_][a-z0-9_]*)[` + "`" + `]?\s*(?:<|>|less\s+than|greater\s+than|below|above)\s*-?[0-9]+(?:\.[0-9]+)?\s*,?\s*(?:must\s+|should\s+|shall\s+)?(?:raise|throw|return)\s+(?:ValueError|an?\s+error)\b`),
	regexp.MustCompile(`(?i)[` + "`" + `]?([a-z_][a-z0-9_]*)[` + "`" + `]?\s*(?:<|>|less\s+than|greater\s+than|below|above)\s*-?[0-9]+(?:\.[0-9]+)?\s+(?:must\s+|should\s+|shall\s+)?(?:raise|throw|return)\s+(?:ValueError|an?\s+error)\b`),
}

func requirementAuditComparisonDefinesRejectedSide(quote string) bool {
	for _, pattern := range requirementAuditForbiddenSideErrorPatterns {
		if pattern.MatchString(quote) {
			return true
		}
	}
	return false
}

func parseRequirementAuditBoundaryObligation(quote string) (requirementAuditBoundaryObligation, bool) {
	// A clause such as `amount < 0 must raise ValueError` names the rejected
	// side of the boundary. Requiring the exact threshold (0) to fail reverses
	// that contract because the strict inequality excludes 0. These clauses are
	// still structurally bound by their executable invalid-side fixture and
	// domain-error assertion; only valid-domain clauses such as `amount must be
	// greater than 0` need the threshold-specific guard below.
	if requirementAuditComparisonDefinesRejectedSide(quote) {
		return requirementAuditBoundaryObligation{}, false
	}
	for _, pattern := range requirementAuditCollectionBoundaryPatterns {
		match := pattern.FindStringSubmatch(quote)
		if len(match) == 3 {
			return requirementAuditBoundaryObligation{
				Field: strings.TrimSpace(match[1]), Value: strings.TrimSpace(match[2]),
				CollectionValues: true,
			}, true
		}
	}
	for _, pattern := range requirementAuditBoundaryPatterns {
		match := pattern.FindStringSubmatch(quote)
		if len(match) == 3 {
			return requirementAuditBoundaryObligation{
				Field: strings.TrimSpace(match[1]),
				Value: strings.TrimSpace(match[2]),
			}, true
		}
	}
	for _, pattern := range requirementAuditPositiveBoundaryPatterns {
		match := pattern.FindStringSubmatch(quote)
		if len(match) == 2 {
			return requirementAuditBoundaryObligation{Field: strings.TrimSpace(match[1]), Value: "0"}, true
		}
	}
	return requirementAuditBoundaryObligation{}, false
}

func requirementAuditScriptExercisesBoundary(
	authoritativeTask, contractQuote, script string,
	obligation requirementAuditBoundaryObligation,
) bool {
	script = requirementAuditExecutableLines(script)
	return requirementAuditScriptContainsBoundaryFixtureForTask(
		authoritativeTask, script, obligation,
	) &&
		requirementAuditScriptProvesBoundaryRejection(contractQuote, script)
}

func requirementAuditScriptContainsBoundaryFixtureForTask(
	authoritativeTask, script string,
	obligation requirementAuditBoundaryObligation,
) bool {
	if requirementAuditScriptContainsBoundaryFixture(script, obligation) {
		return true
	}
	return requirementAuditPositionalBoundaryFixture(authoritativeTask, script, obligation)
}

func requirementAuditScriptContainsBoundaryFixture(
	script string,
	obligation requirementAuditBoundaryObligation,
) bool {
	field := regexp.QuoteMeta(strings.TrimSpace(obligation.Field))
	value := regexp.QuoteMeta(strings.TrimSpace(obligation.Value))
	if field == "" || value == "" {
		return false
	}
	// Accept the common executable fixture spellings without interpreting the
	// expected business result: mapping/object fields, keyword arguments and
	// direct variable assignments. A threshold mentioned only in prose or an Rn
	// marker is not executable boundary evidence.
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)["']` + field + `["']\s*:\s*` + value + `(?:\D|$)`),
		regexp.MustCompile(`(?i)\b` + field + `\s*=\s*` + value + `(?:\D|$)`),
	}
	if obligation.CollectionValues {
		patterns = []*regexp.Regexp{
			regexp.MustCompile(`(?i)\b` + field + `\s*=\s*\{[^}\r\n]*:\s*` + value + `(?:\D|$)`),
			regexp.MustCompile(`(?i)["']` + field + `["']\s*:\s*\{[^}\r\n]*:\s*` + value + `(?:\D|$)`),
		}
	}
	for _, pattern := range patterns {
		if pattern.MatchString(script) {
			return true
		}
	}
	return false
}

var requirementAuditContractSignatureRe = regexp.MustCompile(
	`(?i)\b([a-z_][a-z0-9_]*)\s*\(([^()\r\n]{1,500})\)`,
)

func requirementAuditPositionalBoundaryFixture(
	authoritativeTask, script string,
	obligation requirementAuditBoundaryObligation,
) bool {
	field := strings.TrimSpace(obligation.Field)
	if field == "" {
		return false
	}
	script = requirementAuditExecutableLines(script)
	for _, signature := range requirementAuditContractSignatureRe.FindAllStringSubmatch(authoritativeTask, -1) {
		if len(signature) != 3 {
			continue
		}
		parameters := splitRequirementAuditCallArguments(signature[2])
		parameterIndex := -1
		for index, parameter := range parameters {
			parameter = strings.TrimSpace(strings.SplitN(parameter, "=", 2)[0])
			if parameter == field {
				parameterIndex = index
				break
			}
		}
		if parameterIndex < 0 {
			continue
		}
		for _, callable := range requirementAuditCallableAliases(script, signature[1]) {
			for _, arguments := range requirementAuditCalls(script, callable) {
				if parameterIndex >= len(arguments) ||
					!requirementAuditArgumentContainsThreshold(arguments[parameterIndex], obligation) {
					continue
				}
				return true
			}
		}
	}
	return false
}

func requirementAuditCallableAliases(script, callable string) []string {
	aliases := []string{callable}
	seen := map[string]bool{callable: true}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*([A-Za-z_]\w*)\s*=\s*(?:[A-Za-z_]\w*\.)*` + regexp.QuoteMeta(callable) + `\b`),
		regexp.MustCompile(`(?m)^\s*from\s+[^\r\n]+\s+import\s+` + regexp.QuoteMeta(callable) + `\s+as\s+([A-Za-z_]\w*)\b`),
	}
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringSubmatch(script, -1) {
			if len(match) == 2 && !seen[match[1]] {
				seen[match[1]] = true
				aliases = append(aliases, match[1])
			}
		}
	}
	return aliases
}

func requirementAuditCalls(script, callable string) [][]string {
	pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(callable) + `\s*\(`)
	var calls [][]string
	for _, location := range pattern.FindAllStringIndex(script, -1) {
		open := strings.IndexByte(script[location[0]:location[1]], '(') + location[0]
		if open < location[0] {
			continue
		}
		if body, ok := requirementAuditBalancedCallBody(script, open); ok {
			calls = append(calls, splitRequirementAuditCallArguments(body))
		}
	}
	return calls
}

func requirementAuditBalancedCallBody(value string, open int) (string, bool) {
	depth := 0
	var quote byte
	escaped := false
	for index := open; index < len(value); index++ {
		char := value[index]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		switch char {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return value[open+1 : index], true
			}
		}
	}
	return "", false
}

func splitRequirementAuditCallArguments(value string) []string {
	var arguments []string
	start := 0
	depth := 0
	var quote byte
	escaped := false
	for index := 0; index < len(value); index++ {
		char := value[index]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		switch char {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				arguments = append(arguments, strings.TrimSpace(value[start:index]))
				start = index + 1
			}
		}
	}
	if tail := strings.TrimSpace(value[start:]); tail != "" || len(arguments) > 0 {
		arguments = append(arguments, tail)
	}
	return arguments
}

func requirementAuditArgumentContainsThreshold(
	argument string,
	obligation requirementAuditBoundaryObligation,
) bool {
	value := regexp.QuoteMeta(strings.TrimSpace(obligation.Value))
	if value == "" {
		return false
	}
	if obligation.CollectionValues {
		return regexp.MustCompile(`(?s)^\s*\{.*?:\s*` + value + `\s*(?:,|})`).MatchString(argument)
	}
	return regexp.MustCompile(`^\s*` + value + `\s*$`).MatchString(argument)
}

func requirementAuditScriptProvesBoundaryRejection(contractQuote, script string) bool {
	// A strict input-domain clause is not proved merely because an incidental
	// arithmetic/runtime error happens at the forbidden threshold. In
	// particular, accepting ZeroDivisionError as success only proves the
	// implementation divided by zero; it does not prove that it validates the
	// named field. Require either an explicit domain-error expectation or an
	// executable rejection predicate.
	// A tuple that accepts both a domain error and an incidental runtime error
	// makes the two behaviors observationally identical and cannot prove input
	// validation. Reject that checker shape before recognizing ValueError.
	mixedDomainRuntimeError := regexp.MustCompile(
		`(?is)except\s*\([^)]*(?:ValueError[^)]*(?:ZeroDivisionError|OverflowError|ArithmeticError|RuntimeError|Exception)|(?:ZeroDivisionError|OverflowError|ArithmeticError|RuntimeError|Exception)[^)]*ValueError)[^)]*\)\s*:`,
	)
	if mixedDomainRuntimeError.MatchString(script) {
		return false
	}
	domainErrorPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*except\s+(?:\(\s*)?ValueError\b`),
		regexp.MustCompile(`(?i)(?:pytest\.)?raises\s*\(\s*ValueError\b`),
		regexp.MustCompile(`(?i)assertRaises\s*\(\s*ValueError\b`),
	}
	for _, pattern := range domainErrorPatterns {
		if pattern.MatchString(script) {
			return true
		}
	}
	if !requirementAuditBoundaryContractAllowsSentinel(contractQuote) {
		return false
	}
	rejectionPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?im)^\s*assert\s+not\s+[^#\r\n]+`),
		regexp.MustCompile(`(?im)^\s*assert\s+[^#\r\n]+\s+(?:is|==)\s+(?:False|None)\b`),
		regexp.MustCompile(`(?im)^\s*assert\s+(?:len\s*\([^\r\n]+\)|[^#\r\n]+)\s*==\s*0\b`),
		regexp.MustCompile(`(?im)^\s*assert\s+[^#\r\n]+\s*==\s*(?:\{\}|\[\]|\(\))\s*(?:,|$)`),
	}
	for _, pattern := range rejectionPatterns {
		if pattern.MatchString(script) {
			return true
		}
	}
	return false
}

func requirementAuditBoundaryContractAllowsSentinel(contractQuote string) bool {
	contractQuote = strings.ToLower(strings.Join(strings.Fields(contractQuote), " "))
	return containsAny(contractQuote,
		"回傳 false", "返回 false", "回傳空", "返回空", "空結果", "空集合",
		"以 false 拒絕", "以空結果拒絕", "return false", "returns false",
		"return empty", "returns empty", "empty result", "empty collection",
	)
}

func requirementAuditExecutableLines(script string) string {
	lines := strings.Split(script, "\n")
	executable := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		executable = append(executable, line)
	}
	return strings.Join(executable, "\n")
}

func (l *RequirementAuditLedger) bindCoveredIDsLocked(
	script string,
	ids []string,
) (bound, unbound []string) {
	structurallyBound, structurallyUnbound := requirementAuditBoundCoveredIDs(script, ids)
	unbound = append(unbound, structurallyUnbound...)
	quotes := make(map[string]string, len(l.contractItems))
	for _, item := range l.contractItems {
		quotes[item.ID] = item.Quote
	}
	for _, id := range structurallyBound {
		obligation, required := parseRequirementAuditBoundaryObligation(quotes[id])
		if required && !requirementAuditScriptExercisesBoundary(
			l.authoritativeTask, quotes[id], script, obligation,
		) {
			unbound = append(unbound, id)
			continue
		}
		bound = append(bound, id)
	}
	return bound, unbound
}
