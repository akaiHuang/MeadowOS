package agent

import (
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

var auditDirectCallTupleUnpackRe = regexp.MustCompile(
	`(?m)^\s*[A-Za-z_]\w*(?:\s*,\s*[A-Za-z_]\w*)+\s*=\s*[A-Za-z_]\w*\s*\(`,
)

var auditUntypedUncoveredMarkerRe = regexp.MustCompile(`(?i)\bUNCOVERED\s+(R\d+)\b`)

var (
	auditContractCallableRe             = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	auditContractSignatureRe            = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(([^)\r\n]*)\)`)
	auditDeclaredContainerTypeRe        = regexp.MustCompile(`(?i)\b([A-Za-z_][A-Za-z0-9_]*)\s*(?:是|must\s+be|is|are)\s*(?:an?\s+)?(list|dict|mapping)\b`)
	auditPythonFunctionDefinitionRe     = regexp.MustCompile(`(?m)^\s*(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	auditUntypedRequirementAssertionRe  = regexp.MustCompile(`(?im)^\s*AssertionError\s*:\s*(R\d+)\b`)
	auditPythonKeyErrorRe               = regexp.MustCompile(`(?m)^KeyError:\s*['"]([^'"]+)['"]\s*$`)
	auditPythonFileNotFoundRe           = regexp.MustCompile(`(?m)^FileNotFoundError:.*['"]([^'"]+)['"]\s*$`)
	auditPythonModuleNotFoundRe         = regexp.MustCompile(`(?m)^ModuleNotFoundError:\s+No module named ['"]([^'"]+)['"]\s*$`)
	auditPythonTraceFileRe              = regexp.MustCompile(`(?m)^\s*File\s+['"]([^'"]+)['"],\s+line\s+\d+`)
	auditPythonCheckerSyntaxErrorRe     = regexp.MustCompile(`(?s)File\s+['"][^'"\r\n]*\.meadow-script-[^'"\r\n]*['"].*?(?:SyntaxError|IndentationError|TabError):`)
	auditPythonIncidentalExceptionRe    = regexp.MustCompile(`(?m)^(KeyError|IndexError|AttributeError|NameError|UnboundLocalError):(?:\s|$)`)
	auditPythonMappingKeyRe             = regexp.MustCompile(`['"]([^'"\r\n]{1,120})['"]\s*:`)
	auditPythonFlatMappingRe            = regexp.MustCompile(`\{[^{}\r\n]{1,800}\}`)
	auditPythonTopLevelAlwaysFailRe     = regexp.MustCompile(`(?m)^assert\s+(?:False|0)\b`)
	auditPythonPositionalGetErrorRe     = regexp.MustCompile(`(?im)^AttributeError:\s*['\"](?:tuple|list)['\"] object has no attribute ['\"]get['\"]\s*$`)
	auditPythonPositionalRecordRe       = regexp.MustCompile(`(?s)\[\s*(?:\(\s*['\"][^'\"\r\n]+['\"]\s*,|\[\s*['\"][^'\"\r\n]+['\"]\s*,)`)
	auditPythonExpectedGotStringRe      = regexp.MustCompile(`(?i)AssertionError:\s*expected\s+['\"]([^'\"]*)['\"]\s+(?:but\s+)?got\s+['\"]([^'\"]*)['\"]`)
	auditPythonExpectedGotQueryRe       = regexp.MustCompile(`(?i)AssertionError:\s*expected\s+['\"]?([^\s'\"]*)['\"]?\s+got\s+['\"]?([^\s'\"]*)['\"]?`)
	auditPythonExpectedGotListRe        = regexp.MustCompile(`(?i)AssertionError:\s*expected\s+(\[[^\r\n]*\])\s+got\s+(\[[^\r\n]*\])`)
	auditPythonOmittedValueFixtureRe    = regexp.MustCompile(`(?m)(?:\bNone\b|:\s*\[\s*\])`)
	auditPythonStringMappingPairRe      = regexp.MustCompile(`['\"]([^'\"\r\n]{1,120})['\"]\s*:\s*['\"]([^'\"\r\n]{0,240})['\"]`)
	auditPythonNumericMappingPairRe     = regexp.MustCompile(`['\"]([^'\"\r\n]{1,120})['\"]\s*:\s*(-?\d+(?:\.\d+)?)\b`)
	auditPythonQuotedStringRe           = regexp.MustCompile(`['\"]([^'\"\r\n]*)['\"]`)
	auditPythonTraceDirectCallLineRe    = regexp.MustCompile(`(?m)^\s+([A-Za-z_][A-Za-z0-9_]*)\((.*)\)\s*$`)
	auditPythonTraceAssignedCallLineRe  = regexp.MustCompile(`(?m)^\s+(?:[A-Za-z_][A-Za-z0-9_]*\s*=\s*)?([A-Za-z_][A-Za-z0-9_]*)\((.*)\)\s*$`)
	auditPythonNumericBoolMappingPairRe = regexp.MustCompile(`['\"][^'\"\r\n]{1,120}['\"]\s*:\s*[01]\b`)
	auditIntervalMinuteExpectationRe    = regexp.MustCompile(`(?s)\b([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*(\[\s*\{.*?\}\s*,?\s*\])\s*\)\s*==\s*(\d+(?:\.0+)?)\b`)
	auditIntervalAssignedExpectationRe  = regexp.MustCompile(`(?s)\b([A-Za-z_][A-Za-z0-9_]*)\s*=\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*(\[\s*\{.*?\}\s*,?\s*\])\s*\)\s*\r?\n\s*assert\s+([A-Za-z_][A-Za-z0-9_]*)\s*==\s*(\d+(?:\.0+)?)\b`)
	auditPythonStartClockFieldRe        = regexp.MustCompile(`['\"]start['\"]\s*:\s*['\"](\d{2}:\d{2})['\"]`)
	auditPythonEndClockFieldRe          = regexp.MustCompile(`['\"]end['\"]\s*:\s*['\"](\d{2}:\d{2})['\"]`)
	auditAggregateRatioContractRe       = regexp.MustCompile(`(?i)\b([A-Za-z_][A-Za-z0-9_]*)\s*[（(]\s*([A-Za-z_][A-Za-z0-9_]*)\s+(-?\d+(?:\.\d+)?)\s*(?:或|or)\s*(-?\d+(?:\.\d+)?)\s*(?:的比例|ratio|rate)[^）)]*?(\d+)\s*位\s*[）)]`)
	auditPythonListComprehensionRe      = regexp.MustCompile(`(?s)\b([A-Za-z_][A-Za-z0-9_]*)\s*=\s*\[\s*\{(.*?)\}\s*for\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s+range\(\s*(\d+)\s*\)\s*\]`)
	auditPythonValueErrorTryRe          = regexp.MustCompile(`(?s)\btry\s*:\s*\n(.*?)\bexcept\s+ValueError\s*:`)
	auditPythonMappingListLiteralRe     = regexp.MustCompile(`(?s)\{[^{}]*['"][^'"\r\n]+['"]\s*:\s*\[([^\[\]]*)\][^{}]*\}`)
	auditMappingPrecedenceChainRe       = regexp.MustCompile(`(?i)\b[A-Za-z_][A-Za-z0-9_]*(?:\s*>\s*[A-Za-z_][A-Za-z0-9_]*){1,5}\b`)
	auditPythonBoolMappingPairRe        = regexp.MustCompile(`['"]([^'"\r\n]*)['"]\s*:\s*(True|False)\b`)
	auditPythonStringCollectionAssignRe = regexp.MustCompile(
		`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(\[[^\r\n]*\]|\{[^\r\n]*\})\s*$`,
	)
)

var (
	auditISOSecondLiteralRe = regexp.MustCompile(
		`['"]\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}['"]`,
	)
	auditGotISOMinuteRe = regexp.MustCompile(
		`(?i)got\s+['"]?\d{4}-\d{2}-\d{2}T\d{2}:\d{2}['"]?(?:\s|$)`,
	)
	auditNamedDurationSignatureRe = regexp.MustCompile(
		`\b([A-Za-z_]\w*)\s*\([^\n)]*\b([A-Za-z_][A-Za-z0-9]*_(hours|minutes))\b[^\n)]*\)`,
	)
	auditDirectDurationExpectationRe = regexp.MustCompile(
		`(?s)\b([A-Za-z_]\w*)\s*\(\s*['"](\d{4}-\d{2}-\d{2}T\d{2}:\d{2})['"]\s*,\s*(\d+(?:\.\d+)?)\s*\).*?(?:==|!=)\s*['"](\d{4}-\d{2}-\d{2}T\d{2}:\d{2})['"]`,
	)
	auditDurationCallLiteralRe = regexp.MustCompile(
		`\b([A-Za-z_]\w*)\s*\(\s*['"](\d{4}-\d{2}-\d{2}T\d{2}:\d{2})['"]\s*,\s*(\d+(?:\.\d+)?)\s*\)`,
	)
	auditDurationFixtureRowRe = regexp.MustCompile(
		`\(\s*['"](\d{4}-\d{2}-\d{2}T\d{2}:\d{2})['"]\s*,\s*(\d+(?:\.\d+)?)\s*,\s*['"](\d{4}-\d{2}-\d{2}T\d{2}:\d{2})['"](?:\s*,|\s*\))`,
	)
)

type auditDurationUnitMismatch struct {
	Function     string
	Identifier   string
	DeclaredUnit string
	InferredUnit string
	Value        string
	Start        string
	Expected     string
}

// requirementAuditCheckerCorrectionHint diagnoses a checker-construction
// mistake without deciding the artifact's business answer. Tuple-unpacking a
// direct call before establishing its documented outer type commonly turns a
// mapping into its keys; the resulting string assertions say nothing about the
// artifact. The model still authors the corrected predicate and expected data.
func requirementAuditCheckerCorrectionHint(
	authoritativeTask string,
	call provider.ToolCall,
	execution ToolExecution,
	contractItems ...RequirementAuditContractItem,
) string {
	if (call.Function.Name != DefaultShellTool && call.Function.Name != RequirementAuditCheckToolName) ||
		!execution.Executed || execution.Status == ToolOK {
		return ""
	}
	args := execution.Args
	if len(args) == 0 {
		args = map[string]any{}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return ""
		}
	}
	script := stringArgument(args, "script")
	output := strings.ToLower(execution.LLMContent)
	if auditDirectCallTupleUnpackRe.MatchString(script) &&
		containsAny(output, "traceback", "assertionerror", "<class 'str'>", "<class \"str\">") {
		return "The checker destructured one function return before establishing its authoritative outer type. That can iterate a mapping into key strings and create a checker-only failure. In the corrected checker assign the call result to one variable, assert the contract's documented outer type/keys, then compare its fields. Destructure only if the authoritative contract explicitly specifies a positional tuple/list return."
	}
	contract := strings.ToLower(strings.Join(strings.Fields(authoritativeTask), " "))
	if auditPythonPositionalGetErrorRe.MatchString(execution.LLMContent) &&
		auditPythonPositionalRecordRe.MatchString(script) &&
		!containsAny(contract,
			"tuple", "tuples", "元組", "二元組", "positional",
			"(start, end)", "(start,end)", "[start, end]", "[start,end]",
		) {
		return "The checker supplied each named-field record as a positional tuple/list, and the observed failure is only that this fixture has no mapping `.get` method. The authoritative contract does not require a positional container. Rebuild the fixture as mappings with the advertised field names (or copy the record shape from an authoritative example), then recompute the expected value; do not treat this container-shape exception as an artifact discrepancy."
	}
	minutePrecision := containsAny(contract,
		"iso 分鐘字串", "iso分鐘字串", "minute-precision iso",
		"iso minute string", "iso minutes string",
	)
	if minutePrecision && auditISOSecondLiteralRe.MatchString(script) &&
		auditGotISOMinuteRe.MatchString(execution.LLMContent) {
		return "The checker introduced seconds into an expected timestamp even though the authoritative contract requires a minute-precision ISO string. Use the contract's exact output precision (`YYYY-MM-DDTHH:MM`) and do not treat omitted seconds as an artifact failure."
	}
	if hint := requirementAuditAllowedListItemRejectionCorrectionHint(
		authoritativeTask, script,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditPrecedenceExpectationCorrectionHint(
		authoritativeTask, script, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditIntervalMinuteExpectationCorrectionHint(
		authoritativeTask, script, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditInvalidIntervalNormalOutputCorrectionHint(
		authoritativeTask, script, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditAggregateRatioExpectationCorrectionHint(
		authoritativeTask, script, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditCappedNumericExpectationCorrectionHint(
		authoritativeTask, script, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if mismatch, ok := requirementAuditDurationUnitMismatch(authoritativeTask, script); ok {
		return requirementAuditDurationUnitCorrection(mismatch)
	}
	if hint := requirementAuditWrongContainerFixtureCorrectionHint(
		authoritativeTask, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditBoolValueFixtureCorrectionHint(
		authoritativeTask, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditCollapsedDuplicateMappingCorrectionHint(
		authoritativeTask, script, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditQueryAssociationCorrectionHint(
		authoritativeTask, script, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditNormalizedIdentifierOutputCorrectionHint(
		authoritativeTask, script, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditOmissionCorrectionHint(
		authoritativeTask, script, execution.LLMContent,
	); hint != "" {
		return hint
	}
	if hint := requirementAuditBoundaryExceptionCorrection(
		authoritativeTask, script, execution.LLMContent, contractItems...,
	); hint != "" {
		return hint
	}
	if !strings.Contains(strings.ToUpper(execution.LLMContent), "AUDIT_UNCOVERED") {
		if marker := auditUntypedUncoveredMarkerRe.FindStringSubmatch(execution.LLMContent); len(marker) == 2 {
			return "The checker reported `UNCOVERED " + strings.ToUpper(marker[1]) + "` only inside an assertion/traceback, so the failure is not bound to the typed audit protocol. In one materially different checker, handle the mismatch branch explicitly, print exactly `AUDIT_UNCOVERED " + strings.ToUpper(marker[1]) + ": <exact authoritative clause>` before exiting non-zero, and do not cite unrelated IDs or rely on the assertion message itself."
		}
		if marker := auditUntypedRequirementAssertionRe.FindStringSubmatch(execution.LLMContent); len(marker) == 2 {
			id := strings.ToUpper(marker[1])
			for _, item := range requirementAuditCorrectionContractItems(authoritativeTask, contractItems) {
				if item.ID != id {
					continue
				}
				return "The artifact-loading checker already localized its assertion mismatch to `" + id + "`, but an assertion label is not typed audit evidence. Build one materially different bounded checker that imports or reads the current artifact instead of reimplementing its contract callable. On the mismatch branch print exactly `AUDIT_UNCOVERED " + id + ": " + item.Quote + "` before exiting non-zero; do not cite other IDs or modify the artifact first."
			}
		}
	}
	if exception := requirementAuditRequiredExceptionTrace(authoritativeTask, execution.LLMContent); exception != "" {
		return "The checker terminated with `" + exception + "` while the authoritative contract explicitly includes a required-`" + exception + "` case. First isolate the call that raised. If it is the invalid-input fixture, catch exactly `" + exception + "`, fail when that exception is absent or a different exception appears, and continue to the remaining predicates and typed audit marker. If it is a valid-input fixture, preserve the non-zero result and emit one exact typed uncovered marker for the contradicted clause. Do not change the artifact based on an unclassified traceback."
	}
	return ""
}

// requirementAuditAllowedListItemRejectionCorrectionHint catches a checker
// that treats a homogeneous literal list as invalid even though the
// authoritative contract explicitly permits that item type. The checker must
// visibly encode a ValueError-as-success branch; ordinary valid-input calls
// are left untouched.
func requirementAuditAllowedListItemRejectionCorrectionHint(authoritativeTask, script string) string {
	contract := strings.ToLower(authoritativeTask)
	allowed := map[string]bool{
		"int":    containsAny(contract, "非 bool int", "non-bool int", "non bool int"),
		"float":  containsAny(contract, "有限 float", "finite float"),
		"string": containsAny(contract, "允許 str", "allow str", "allows str", "permitted str"),
		"bool":   containsAny(contract, "、bool", ", bool", "，bool", " and bool"),
	}
	for _, tryMatch := range auditPythonValueErrorTryRe.FindAllStringSubmatch(script, -1) {
		if len(tryMatch) != 2 {
			continue
		}
		expectsValueError := containsAny(tryMatch[1],
			"assert False", "AUDIT_UNCOVERED", "exit(1)", "sys.exit(1)", "SystemExit(1)",
		) || requirementAuditTryStatusExpectsValueError(script, tryMatch[1])
		if !expectsValueError {
			continue
		}
		for _, listMatch := range auditPythonMappingListLiteralRe.FindAllStringSubmatch(tryMatch[1], -1) {
			if len(listMatch) != 2 {
				continue
			}
			kind, ok := requirementAuditHomogeneousLiteralKind(listMatch[1])
			if !ok || !allowed[kind] {
				continue
			}
			return "The checker expects ValueError for a list whose literal items are all `" + kind +
				"`, but the authoritative contract explicitly permits that item type. Treat this fixture as a valid-input case and assert its encoded output; use a genuinely disallowed type or a non-finite float for the rejection branch. This reversed type expectation is not artifact evidence."
		}
	}
	return ""
}

// requirementAuditTryStatusExpectsValueError recognizes a checker that stores
// the normal-return and ValueError branches in a status variable, then later
// asserts that the ValueError branch won. This is semantically the same as an
// inline expected-exception assertion, despite moving the failure predicate
// outside the try block.
func requirementAuditTryStatusExpectsValueError(script, tryBody string) bool {
	assignmentRe := regexp.MustCompile(
		`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*['"]([^'"\r\n]*)['"]\s*$`,
	)
	for _, normal := range assignmentRe.FindAllStringSubmatch(tryBody, -1) {
		if len(normal) != 3 {
			continue
		}
		variable := normal[1]
		exceptionAssignmentRe := regexp.MustCompile(
			`(?m)^\s*except\s+ValueError\s*:\s*\r?\n\s+` + regexp.QuoteMeta(variable) +
				`\s*=\s*['"]([^'"\r\n]*)['"]\s*$`,
		)
		exceptionAssignment := exceptionAssignmentRe.FindStringSubmatch(script)
		if len(exceptionAssignment) != 2 || exceptionAssignment[1] == normal[2] {
			continue
		}
		expected := regexp.QuoteMeta(exceptionAssignment[1])
		assertExpectedRe := regexp.MustCompile(
			`(?m)^\s*assert\s+` + regexp.QuoteMeta(variable) + `\s*==\s*['"]` + expected + `['"](?:\s|,|$)`,
		)
		if assertExpectedRe.MatchString(script) {
			return true
		}
		ifExpectedRe := regexp.MustCompile(
			`(?m)^\s*if\s+` + regexp.QuoteMeta(variable) + `\s*!=\s*['"]` + expected + `['"]\s*:`,
		)
		if ifExpectedRe.MatchString(script) && containsAny(script, "AUDIT_UNCOVERED", "exit(1)", "SystemExit(1)") {
			return true
		}
	}
	return false
}

func requirementAuditHomogeneousLiteralKind(source string) (string, bool) {
	items := strings.Split(source, ",")
	kind := ""
	for _, raw := range items {
		item := strings.TrimSpace(raw)
		if item == "" {
			return "", false
		}
		current := ""
		switch {
		case item == "True" || item == "False":
			current = "bool"
		case (strings.HasPrefix(item, "'") && strings.HasSuffix(item, "'")) ||
			(strings.HasPrefix(item, `"`) && strings.HasSuffix(item, `"`)):
			current = "string"
		default:
			if _, err := strconv.Atoi(item); err == nil {
				current = "int"
			} else if value, err := strconv.ParseFloat(item, 64); err == nil && !math.IsInf(value, 0) && !math.IsNaN(value) {
				current = "float"
			}
		}
		if current == "" || (kind != "" && kind != current) {
			return "", false
		}
		kind = current
	}
	return kind, kind != ""
}

type requirementAuditAssignedCall struct {
	result    string
	arguments []string
	offset    int
}

// requirementAuditPrecedenceExpectationCorrectionHint checks only fully
// literal boolean mapping fixtures when the authoritative contract explicitly
// declares a high-to-low precedence chain, False suppression, and a sorted
// enabled-key output. It does not execute or mirror the artifact.
func requirementAuditPrecedenceExpectationCorrectionHint(authoritativeTask, script, output string) string {
	contract := strings.ToLower(strings.Join(strings.Fields(authoritativeTask), " "))
	if !containsAny(output, "AssertionError", "AUDIT_UNCOVERED") ||
		!containsAny(contract, "優先", "precedence", "priority", "override") ||
		!containsAny(contract,
			"false 必須能關閉", "false必須能關閉", "false can disable", "false must disable",
			"false overrides", "false 覆寫", "false覆寫",
		) ||
		!containsAny(contract, "排序", "sorted", "sort") ||
		!containsAny(contract,
			"enabled=true", "enabled = true", "enabled feature", "enabled key",
			"值為 true", "值是 true", "true 的 feature", "true 的 key",
		) {
		return ""
	}
	chainText := auditMappingPrecedenceChainRe.FindString(contract)
	if chainText == "" {
		return ""
	}
	precedence := strings.Split(chainText, ">")
	for index := range precedence {
		precedence[index] = strings.TrimSpace(precedence[index])
	}
	for _, signature := range auditContractSignatureRe.FindAllStringSubmatch(authoritativeTask, -1) {
		if len(signature) != 3 {
			continue
		}
		parameters := requirementAuditSplitPythonArguments(signature[2])
		parameterIndex := map[string]int{}
		for index, parameter := range parameters {
			parameterIndex[strings.ToLower(strings.TrimSpace(strings.SplitN(parameter, "=", 2)[0]))] = index
		}
		allLayersBound := len(precedence) >= 2
		for _, layer := range precedence {
			if _, ok := parameterIndex[layer]; !ok {
				allLayersBound = false
				break
			}
		}
		if !allLayersBound {
			continue
		}
		for _, fixture := range requirementAuditAssignedCalls(script, signature[1]) {
			if len(fixture.arguments) != len(parameters) {
				continue
			}
			layers := make(map[string]map[string]bool, len(precedence))
			valid := true
			for _, layer := range precedence {
				parsed, ok := requirementAuditPythonBoolMappingArgument(
					script, fixture.offset, fixture.arguments[parameterIndex[layer]],
				)
				if !ok {
					valid = false
					break
				}
				layers[layer] = parsed
			}
			if !valid {
				continue
			}
			enabled := requirementAuditPrecedenceEnabledKeys(precedence, layers)
			if mismatch := requirementAuditResultExpectationMismatch(script, fixture.result, enabled); mismatch != "" {
				return "The checker expected " + mismatch + " from `" + signature[1] +
					"`, but its own literal mappings resolve to " + requirementAuditQuotedList(enabled) +
					" under the authoritative `" + strings.Join(precedence, " > ") +
					"` precedence and False-suppression rules. Recompute each key from the highest layer that defines it, then compare the sorted enabled keys; this checker-only precedence mismatch is not artifact evidence."
			}
		}
	}
	return ""
}

func requirementAuditAssignedCalls(script, callable string) []requirementAuditAssignedCall {
	pattern := regexp.MustCompile(
		`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*` + regexp.QuoteMeta(callable) + `\s*\(`,
	)
	var calls []requirementAuditAssignedCall
	for _, location := range pattern.FindAllStringSubmatchIndex(script, -1) {
		if len(location) != 4 {
			continue
		}
		open := strings.IndexByte(script[location[0]:location[1]], '(') + location[0]
		if open < location[0] {
			continue
		}
		body, ok := requirementAuditBalancedCallBody(script, open)
		if !ok {
			continue
		}
		calls = append(calls, requirementAuditAssignedCall{
			result:    script[location[2]:location[3]],
			arguments: requirementAuditSplitPythonArguments(body),
			offset:    location[0],
		})
	}
	return calls
}

func requirementAuditPythonBoolMappingArgument(script string, callOffset int, raw string) (map[string]bool, bool) {
	if parsed, ok := requirementAuditPythonBoolMappingLiteral(raw); ok {
		return parsed, true
	}
	identifier := strings.TrimSpace(raw)
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(identifier) ||
		callOffset < 0 || callOffset > len(script) {
		return nil, false
	}
	assignmentRe := regexp.MustCompile(
		`(?m)^\s*` + regexp.QuoteMeta(identifier) + `\s*=\s*(\{[^\r\n]*\})\s*(?:#.*)?$`,
	)
	matches := assignmentRe.FindAllStringSubmatch(script[:callOffset], -1)
	if len(matches) == 0 {
		return nil, false
	}
	return requirementAuditPythonBoolMappingLiteral(matches[len(matches)-1][1])
}

func requirementAuditPythonBoolMappingLiteral(raw string) (map[string]bool, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return nil, false
	}
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	if inner == "" {
		return map[string]bool{}, true
	}
	if strings.ContainsAny(inner, "{}[]()") {
		return nil, false
	}
	keys := auditPythonMappingKeyRe.FindAllStringSubmatch(raw, -1)
	pairs := auditPythonBoolMappingPairRe.FindAllStringSubmatch(raw, -1)
	if len(keys) == 0 || len(keys) != len(pairs) {
		return nil, false
	}
	parsed := make(map[string]bool, len(pairs))
	for _, pair := range pairs {
		if len(pair) != 3 {
			return nil, false
		}
		if _, exists := parsed[pair[1]]; exists {
			return nil, false
		}
		parsed[pair[1]] = pair[2] == "True"
	}
	return parsed, true
}

func requirementAuditPrecedenceEnabledKeys(precedence []string, layers map[string]map[string]bool) []string {
	keys := map[string]bool{}
	for _, layer := range layers {
		for key := range layer {
			keys[key] = true
		}
	}
	enabled := make([]string, 0, len(keys))
	for key := range keys {
		for _, layer := range precedence {
			value, present := layers[layer][key]
			if !present {
				continue
			}
			if value {
				enabled = append(enabled, key)
			}
			break
		}
	}
	slices.Sort(enabled)
	return enabled
}

// requirementAuditCappedNumericExpectationCorrectionHint rejects a literal
// fixture that copies an input amount into an output expectation even though
// the same call supplies a smaller keyed cap and the authoritative contract
// explicitly says the output is capped. This catches interactions where a
// threshold exception changes eligibility but does not erase an independent
// maximum.
func requirementAuditCappedNumericExpectationCorrectionHint(authoritativeTask, script, output string) string {
	contract := strings.ToLower(strings.Join(strings.Fields(authoritativeTask), " "))
	if !containsAny(output, "AssertionError", "AUDIT_UNCOVERED") ||
		!containsAny(contract,
			"為上限", "上限為", "不得超過", "不超過",
			"capped at", "capped by", "must not exceed", "at most",
		) {
		return ""
	}
	for _, signature := range auditContractSignatureRe.FindAllStringSubmatch(authoritativeTask, -1) {
		if len(signature) != 3 {
			continue
		}
		for _, fixture := range requirementAuditAssignedCalls(script, signature[1]) {
			expectationRe := regexp.MustCompile(
				`(?m)^\s*assert\s+` + regexp.QuoteMeta(fixture.result) +
					`\s*\[\s*0\s*\]\s*\[\s*['\"]([^'\"\r\n]+)['\"]\s*\]\s*==\s*(-?\d+(?:\.\d+)?)\b`,
			)
			expectation := expectationRe.FindStringSubmatch(script[fixture.offset:])
			if len(expectation) != 3 || !strings.Contains(contract, strings.ToLower(expectation[1])) {
				continue
			}
			expected, err := strconv.ParseFloat(expectation[2], 64)
			if err != nil {
				continue
			}
			for capIndex, capArgument := range fixture.arguments {
				caps, ok := requirementAuditPythonNumericMappingLiteral(capArgument)
				if !ok {
					continue
				}
				for recordIndex, recordArgument := range fixture.arguments {
					if recordIndex == capIndex {
						continue
					}
					record, ok := requirementAuditSingleMappingListLiteral(recordArgument)
					if !ok || !requirementAuditMappingContainsNumber(record, expected) {
						continue
					}
					for _, pair := range auditPythonStringMappingPairRe.FindAllStringSubmatch(record, -1) {
						if len(pair) != 3 {
							continue
						}
						capValue, found := caps[pair[2]]
						if !found || expected <= capValue {
							continue
						}
						return "The checker expected `" + expectation[1] + "`=" + expectation[2] +
							" by copying a literal input value, but the same fixture supplies the smaller keyed cap " +
							strconv.FormatFloat(capValue, 'f', -1, 64) +
							" and the authoritative contract says this output is capped. A separate threshold or eligibility boundary does not remove that maximum. Recompute the combined expectation with the cap; this checker-only rule-interaction mismatch is not artifact evidence."
					}
				}
			}
		}
	}
	return ""
}

func requirementAuditPythonNumericMappingLiteral(raw string) (map[string]float64, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' || strings.ContainsAny(raw[1:len(raw)-1], "{}[]()") {
		return nil, false
	}
	pairs := auditPythonNumericMappingPairRe.FindAllStringSubmatch(raw, -1)
	if len(pairs) == 0 {
		return nil, false
	}
	parsed := make(map[string]float64, len(pairs))
	for _, pair := range pairs {
		if len(pair) != 3 {
			return nil, false
		}
		value, err := strconv.ParseFloat(pair[2], 64)
		if err != nil {
			return nil, false
		}
		parsed[pair[1]] = value
	}
	return parsed, true
}

func requirementAuditSingleMappingListLiteral(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return "", false
	}
	mappings := auditPythonFlatMappingRe.FindAllString(raw, -1)
	returnValue := ""
	if len(mappings) == 1 {
		returnValue = mappings[0]
	}
	return returnValue, returnValue != ""
}

func requirementAuditMappingContainsNumber(mapping string, expected float64) bool {
	for _, pair := range auditPythonNumericMappingPairRe.FindAllStringSubmatch(mapping, -1) {
		if len(pair) != 3 {
			continue
		}
		value, err := strconv.ParseFloat(pair[2], 64)
		if err == nil && value == expected {
			return true
		}
	}
	return false
}

func requirementAuditResultExpectationMismatch(script, result string, actual []string) string {
	collections := map[string][]string{}
	for _, match := range auditPythonStringCollectionAssignRe.FindAllStringSubmatch(script, -1) {
		if len(match) != 3 {
			continue
		}
		if values, ok := requirementAuditParseStringCollection(match[2]); ok {
			collections[match[1]] = values
		}
	}
	resultPattern := `(?:set\(\s*` + regexp.QuoteMeta(result) + `\s*\)|` + regexp.QuoteMeta(result) + `)`
	// Stop at the first matching flat collection delimiter. A greedy `.*]`
	// also consumes list-shaped text repeated inside an assertion message, so
	// the parser mistakes `expected [..], f"expected [..]"` for one malformed
	// oracle and silently misses a mechanically provable checker error.
	rightPattern := `(\[[^\]\r\n]*\]|\{[^}\r\n]*\}|[A-Za-z_][A-Za-z0-9_]*)`
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*assert\s+(` + resultPattern + `)\s*==\s*` + rightPattern),
		regexp.MustCompile(`(?m)^\s*if\s+(` + resultPattern + `)\s*!=\s*` + rightPattern + `\s*:`),
	}
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringSubmatch(script, -1) {
			if len(match) != 3 {
				continue
			}
			expected, ok := collections[match[2]]
			if strings.HasPrefix(match[2], "[") || strings.HasPrefix(match[2], "{") {
				expected, ok = requirementAuditParseStringCollection(match[2])
			}
			if !ok {
				continue
			}
			actualComparable := slices.Clone(actual)
			expectedComparable := slices.Clone(expected)
			if strings.HasPrefix(strings.TrimSpace(match[1]), "set(") || strings.HasPrefix(match[2], "{") {
				slices.Sort(actualComparable)
				slices.Sort(expectedComparable)
			}
			if !slices.Equal(actualComparable, expectedComparable) {
				return requirementAuditQuotedList(expected)
			}
		}
	}
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "assert ") {
			continue
		}
		membership := regexp.MustCompile(
			`['"]([^'"\r\n]+)['"]\s+(not\s+in|in)\s+` + regexp.QuoteMeta(result) + `\b`,
		)
		for _, match := range membership.FindAllStringSubmatch(trimmed, -1) {
			if len(match) != 3 {
				continue
			}
			present := slices.Contains(actual, match[1])
			if (match[2] == "in" && !present) || (match[2] == "not in" && present) {
				return "membership `" + match[1] + " " + match[2] + " result`"
			}
		}
	}
	return ""
}

func requirementAuditParseStringCollection(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || !((raw[0] == '[' && raw[len(raw)-1] == ']') ||
		(raw[0] == '{' && raw[len(raw)-1] == '}')) {
		return nil, false
	}
	inner := raw[1 : len(raw)-1]
	values := requirementAuditQuotedStrings(inner)
	remainder := auditPythonQuotedStringRe.ReplaceAllString(inner, "")
	remainder = strings.NewReplacer(",", "", " ", "", "\t", "", "\r", "", "\n", "").Replace(remainder)
	if remainder != "" || (strings.TrimSpace(inner) != "" && len(values) == 0) {
		return nil, false
	}
	return values, true
}

func requirementAuditQuotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func requirementAuditIntervalMinuteExpectationCorrectionHint(authoritativeTask, script, output string) string {
	contract := strings.ToLower(authoritativeTask)
	if !containsAny(contract, "minute", "minutes", "分鐘") ||
		!strings.Contains(contract, "start") || !strings.Contains(contract, "end") ||
		!strings.Contains(output, "AssertionError") {
		return ""
	}
	type intervalExpectation struct {
		function string
		fixture  string
		expected string
	}
	expectations := make([]intervalExpectation, 0, 4)
	for _, match := range auditIntervalMinuteExpectationRe.FindAllStringSubmatch(script, -1) {
		if len(match) == 4 {
			expectations = append(expectations, intervalExpectation{match[1], match[2], match[3]})
		}
	}
	for _, match := range auditIntervalAssignedExpectationRe.FindAllStringSubmatch(script, -1) {
		if len(match) == 6 && match[1] == match[4] {
			expectations = append(expectations, intervalExpectation{match[2], match[3], match[5]})
		}
	}
	for _, expectation := range expectations {
		if !strings.Contains(authoritativeTask, expectation.function+"(") {
			continue
		}
		expected, err := strconv.ParseFloat(expectation.expected, 64)
		if err != nil {
			continue
		}
		spans := make([][2]int, 0, 4)
		valid := true
		for _, mapping := range auditPythonFlatMappingRe.FindAllString(expectation.fixture, -1) {
			startMatch := auditPythonStartClockFieldRe.FindStringSubmatch(mapping)
			endMatch := auditPythonEndClockFieldRe.FindStringSubmatch(mapping)
			if len(startMatch) != 2 || len(endMatch) != 2 {
				valid = false
				break
			}
			start, startOK := requirementAuditClockMinute(startMatch[1])
			end, endOK := requirementAuditClockMinute(endMatch[1])
			if !startOK || !endOK || start >= end {
				valid = false
				break
			}
			spans = append(spans, [2]int{start, end})
		}
		if !valid || len(spans) == 0 {
			continue
		}
		slices.SortFunc(spans, func(left, right [2]int) int {
			if left[0] != right[0] {
				return left[0] - right[0]
			}
			return left[1] - right[1]
		})
		total := 0
		start, end := spans[0][0], spans[0][1]
		for _, span := range spans[1:] {
			if span[0] <= end {
				if span[1] > end {
					end = span[1]
				}
				continue
			}
			total += end - start
			start, end = span[0], span[1]
		}
		total += end - start
		if expected == float64(total) {
			continue
		}
		return "The checker expected " + expectation.expected + " minutes, but its own `[start,end)` interval fixture has " +
			strconv.Itoa(total) + " union minutes after sorting and merging overlap/adjacency. Recompute the expected value directly from the fixture endpoints and correct the checker; this arithmetic mismatch is not artifact evidence."
	}
	return ""
}

// requirementAuditInvalidIntervalNormalOutputCorrectionHint rejects a
// numeric-output expectation for a literal interval that the authoritative
// contract explicitly places outside the valid domain. It derives only the
// start/end ordering from the checker fixture and leaves all valid-domain
// business behavior to the model-authored checker.
func requirementAuditInvalidIntervalNormalOutputCorrectionHint(authoritativeTask, script, output string) string {
	contract := strings.ToLower(strings.Join(strings.Fields(authoritativeTask), " "))
	if !strings.Contains(output, "AssertionError") ||
		!strings.Contains(contract, "start") || !strings.Contains(contract, "end") ||
		!containsAny(contract, "valueerror", "raise error", "throws error", "錯誤") ||
		!containsAny(contract,
			"start 必須小於 end", "start必須小於end", "start < end", "start<end",
			"start must be less than end", "start must precede end",
			"反向區間", "reversed interval", "reverse interval",
		) {
		return ""
	}
	type intervalExpectation struct {
		function string
		fixture  string
	}
	expectations := make([]intervalExpectation, 0, 4)
	for _, match := range auditIntervalMinuteExpectationRe.FindAllStringSubmatch(script, -1) {
		if len(match) == 4 {
			expectations = append(expectations, intervalExpectation{match[1], match[2]})
		}
	}
	for _, match := range auditIntervalAssignedExpectationRe.FindAllStringSubmatch(script, -1) {
		if len(match) == 6 && match[1] == match[4] {
			expectations = append(expectations, intervalExpectation{match[2], match[3]})
		}
	}
	for _, expectation := range expectations {
		if !strings.Contains(authoritativeTask, expectation.function+"(") {
			continue
		}
		for _, mapping := range auditPythonFlatMappingRe.FindAllString(expectation.fixture, -1) {
			startMatch := auditPythonStartClockFieldRe.FindStringSubmatch(mapping)
			endMatch := auditPythonEndClockFieldRe.FindStringSubmatch(mapping)
			if len(startMatch) != 2 || len(endMatch) != 2 {
				continue
			}
			start, startOK := requirementAuditClockMinute(startMatch[1])
			end, endOK := requirementAuditClockMinute(endMatch[1])
			if !startOK || !endOK || start < end {
				continue
			}
			return "The checker expects a normal numeric output for a literal interval whose `start` is not earlier than `end`, but the authoritative contract requires that invalid ordering to be rejected with `ValueError`. Catch exactly `ValueError` as the success branch and fail if the call returns normally; this reversed exception expectation is not artifact evidence."
		}
	}
	return ""
}

// requirementAuditAggregateRatioExpectationCorrectionHint rejects a checker
// whose deterministic fixture cannot produce the expected ratio declared by
// the contract. It derives the value only from a bounded range comprehension,
// its conditional source values, and the contract's qualifying values; it
// never executes or interprets the artifact implementation.
func requirementAuditAggregateRatioExpectationCorrectionHint(authoritativeTask, script, output string) string {
	if !strings.Contains(output, "AssertionError") {
		return ""
	}
	for _, contract := range auditAggregateRatioContractRe.FindAllStringSubmatch(authoritativeTask, -1) {
		if len(contract) != 6 {
			continue
		}
		outputField, sourceField := contract[1], contract[2]
		qualifyingOne, errOne := strconv.ParseFloat(contract[3], 64)
		qualifyingTwo, errTwo := strconv.ParseFloat(contract[4], 64)
		digits, digitsErr := strconv.Atoi(contract[5])
		if errOne != nil || errTwo != nil || digitsErr != nil || digits < 0 || digits > 12 {
			continue
		}
		expectationRe := regexp.MustCompile(
			`(?m)^\s*assert\s+[A-Za-z_][A-Za-z0-9_]*\s*\[\s*['"]` + regexp.QuoteMeta(outputField) +
				`['"]\s*\]\s*==\s*(-?\d+(?:\.\d+)?)\s*$`,
		)
		expectedMatch := expectationRe.FindStringSubmatch(script)
		if len(expectedMatch) != 2 {
			continue
		}
		expected, expectedErr := strconv.ParseFloat(expectedMatch[1], 64)
		if expectedErr != nil {
			continue
		}
		for _, fixture := range auditPythonListComprehensionRe.FindAllStringSubmatch(script, -1) {
			if len(fixture) != 5 {
				continue
			}
			rowsName, mapping, indexName := fixture[1], fixture[2], fixture[3]
			total, totalErr := strconv.Atoi(fixture[4])
			if totalErr != nil || total <= 0 {
				continue
			}
			callRe := regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\s*\(\s*` + regexp.QuoteMeta(rowsName) + `\s*\)`)
			if !callRe.MatchString(script) {
				continue
			}
			valueRe := regexp.MustCompile(
				`['"]` + regexp.QuoteMeta(sourceField) + `['"]\s*:\s*(-?\d+(?:\.\d+)?)\s+if\s+` +
					regexp.QuoteMeta(indexName) + `\s*<\s*(\d+)\s+else\s+(-?\d+(?:\.\d+)?)`,
			)
			values := valueRe.FindStringSubmatch(mapping)
			if len(values) != 4 {
				continue
			}
			firstValue, firstErr := strconv.ParseFloat(values[1], 64)
			threshold, thresholdErr := strconv.Atoi(values[2])
			secondValue, secondErr := strconv.ParseFloat(values[3], 64)
			if firstErr != nil || thresholdErr != nil || secondErr != nil || threshold < 0 {
				continue
			}
			firstCount := min(threshold, total)
			qualifies := func(value float64) bool {
				return value == qualifyingOne || value == qualifyingTwo
			}
			qualifiedCount := 0
			if qualifies(firstValue) {
				qualifiedCount += firstCount
			}
			if qualifies(secondValue) {
				qualifiedCount += total - firstCount
			}
			scale := math.Pow10(digits)
			actual := math.RoundToEven((float64(qualifiedCount)/float64(total))*scale) / scale
			if math.Abs(expected-actual) <= 1e-12 {
				continue
			}
			return "The checker expected " + expectedMatch[1] + " for `" + outputField +
				"`, but its own " + strconv.Itoa(total) + "-row fixture has " +
				strconv.Itoa(qualifiedCount) + " qualifying `" + sourceField + "` values under the authoritative " +
				"`" + contract[3] + " or " + contract[4] + "` rule, so the stated ties-to-even result is " +
				strconv.FormatFloat(actual, 'f', digits, 64) + ". Recompute the ratio from the fixture before comparing the artifact; this checker-only aggregate mismatch is not artifact evidence."
		}
	}
	return ""
}

func requirementAuditClockMinute(value string) (int, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func requirementAuditCorrectionContractItems(
	authoritativeTask string,
	contractItems []RequirementAuditContractItem,
) []RequirementAuditContractItem {
	if len(contractItems) > 0 {
		return contractItems
	}
	return extractAtomicContractItems(authoritativeTask)
}

func requirementAuditBoundaryExceptionCorrection(
	authoritativeTask, script, output string,
	contractItems ...RequirementAuditContractItem,
) string {
	if strings.Contains(strings.ToUpper(output), "AUDIT_UNCOVERED") {
		return ""
	}
	if !containsAny(output, "ZeroDivisionError", "OverflowError") {
		return ""
	}
	for _, item := range requirementAuditCorrectionContractItems(authoritativeTask, contractItems) {
		obligation, required := parseRequirementAuditBoundaryObligation(item.Quote)
		if !required || !requirementAuditScriptContainsBoundaryFixtureForTask(
			authoritativeTask, script, obligation,
		) {
			continue
		}
		return "The checker exercised the explicit threshold for `" + obligation.Field +
			"`, but an incidental arithmetic exception occurred before it emitted typed evidence. The next checker must test only this one threshold call in at most 12 lines; do not wrap it in unittest or retest other requirements. Use three explicit outcomes: `except ValueError` is covered; `except (ZeroDivisionError, OverflowError, ArithmeticError)` prints exactly `AUDIT_UNCOVERED " + item.ID + ": " + item.Quote + "` and exits non-zero; the `else` branch prints that same marker and exits non-zero. Do not accept an incidental runtime error as proof that the named field was validated."
	}
	return ""
}

// requirementAuditCheckerSelfError identifies only construction failures that
// are mechanically attributable to the checker fixture itself. Those attempts
// remain bounded by the draft budget but must not consume the smaller semantic
// evidence budget. This function never decides the artifact's expected answer.
func requirementAuditCheckerSelfError(script, output string) string {
	if auditPythonCheckerSyntaxErrorRe.MatchString(output) {
		return "checker_python_syntax_error"
	}
	if moduleMatch := auditPythonModuleNotFoundRe.FindStringSubmatch(output); len(moduleMatch) == 2 {
		traceFiles := auditPythonTraceFileRe.FindAllStringSubmatch(output, -1)
		checkerOnly := len(traceFiles) > 0
		for _, match := range traceFiles {
			if len(match) != 2 || !strings.Contains(match[1], ".meadow-script-") {
				checkerOnly = false
				break
			}
		}
		module := moduleMatch[1]
		importPattern := regexp.MustCompile(`(?m)^\s*(?:from\s+` + regexp.QuoteMeta(module) + `(?:\.|\s)|import\s+` + regexp.QuoteMeta(module) + `\b)`)
		if checkerOnly && importPattern.MatchString(script) {
			return "checker_module_import_error:" + module
		}
	}
	if !strings.Contains(strings.ToUpper(output), "AUDIT_UNCOVERED") {
		if fileMatch := auditPythonFileNotFoundRe.FindStringSubmatch(output); len(fileMatch) == 2 {
			missingPath := fileMatch[1]
			if index := strings.LastIndex(missingPath, "/workspace/"); index >= 0 {
				missingPath = missingPath[index:]
			}
			if strings.Contains(script, missingPath) {
				return "checker_fixture_missing_file:" + missingPath
			}
		}
	}
	if keyMatch := auditPythonKeyErrorRe.FindStringSubmatch(output); len(keyMatch) == 2 {
		missingKey := keyMatch[1]
		if requirementAuditHasInconsistentMappingFixture(script, missingKey) {
			return "checker_fixture_missing_mapping_key:" + missingKey
		}
	}
	if !strings.Contains(strings.ToUpper(output), "AUDIT_UNCOVERED") &&
		auditPythonIncidentalExceptionRe.MatchString(output) &&
		requirementAuditTraceEndsInChecker(output) {
		return "checker_boundary_incidental_exception"
	}
	return ""
}

// A structured checker that reaches a top-level unconditional failure cannot
// ever produce passing evidence: either an earlier call raises or this line
// does. This is especially important for expected-exception clauses, where an
// uncaught correct ValueError would otherwise be replayed forever as an
// apparent artifact failure. Indented assertions inside try/conditional blocks
// remain valid and are intentionally not matched.
func requirementAuditStructuredCheckerAlwaysFails(script string) bool {
	return auditPythonTopLevelAlwaysFailRe.MatchString(
		canonicalRequirementAuditScript(script),
	)
}

func requirementAuditTraceEndsInChecker(output string) bool {
	traceFiles := auditPythonTraceFileRe.FindAllStringSubmatch(output, -1)
	if len(traceFiles) == 0 {
		return false
	}
	last := traceFiles[len(traceFiles)-1]
	return len(last) == 2 && strings.Contains(last[1], ".meadow-script-")
}

func requirementAuditOmissionCorrectionHint(authoritativeTask, script, output string) string {
	lowerTask := strings.ToLower(authoritativeTask)
	if !containsAny(lowerTask,
		"skip", "omit", "ignore", "drop", "exclude", "filter out",
		"略過", "忽略", "排除", "省略", "不納入", "移除",
	) {
		return ""
	}
	if !containsAny(lowerTask,
		"保留 key", "保留鍵", "keep key", "retain key", "bare key",
		"without =", "without an equals", "不含 =", "無等號",
	) && auditPythonOmittedValueFixtureRe.MatchString(script) {
		if match := auditPythonExpectedGotStringRe.FindStringSubmatch(output); len(match) == 3 &&
			requirementAuditExpectedAddsBareQueryKey(match[1], match[2]) {
			return "The checker retained a bare query key for a value that the authoritative contract says to skip/omit. Omission removes that key-value contribution entirely unless another exact clause explicitly says to keep a bare key. Recompute the expected query without the omitted key; use a separate non-omitted fixture for encoding and ordering checks, and do not treat the invented placeholder as an artifact discrepancy."
		}
	}
	if !auditPythonKeyErrorRe.MatchString(output) || !requirementAuditTraceEndsInChecker(output) {
		return ""
	}
	if !strings.Contains(script, "[") {
		return ""
	}
	return "The checker indexed an output key after its only source record was removed. A skip/omit clause does not require an empty placeholder unless another exact authoritative clause says to retain one. For a fixture whose only record is excluded, test absence with `key not in result` or `result == {}`; do not index the omitted key. Then continue with a separate valid-record fixture for any remaining aggregation requirements."
}

func requirementAuditExpectedAddsBareQueryKey(expected, actual string) bool {
	expectedParts := strings.Split(expected, "&")
	actualParts := strings.Split(actual, "&")
	if actual == "" {
		actualParts = nil
	}
	used := make([]bool, len(expectedParts))
	for _, actualPart := range actualParts {
		matched := false
		for index, expectedPart := range expectedParts {
			if !used[index] && expectedPart == actualPart {
				used[index] = true
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	extra := 0
	for index, expectedPart := range expectedParts {
		if used[index] {
			continue
		}
		if expectedPart == "" || strings.Contains(expectedPart, "=") {
			return false
		}
		extra++
	}
	return extra > 0
}

func requirementAuditWrongContainerFixtureCorrectionHint(authoritativeTask, output string) string {
	if !strings.Contains(output, "Traceback") || !strings.Contains(output, "ValueError") {
		return ""
	}
	containerTypes := map[string]string{}
	for _, match := range auditDeclaredContainerTypeRe.FindAllStringSubmatch(authoritativeTask, -1) {
		if len(match) != 3 {
			continue
		}
		kind := strings.ToLower(match[2])
		if kind == "mapping" {
			kind = "dict"
		}
		containerTypes[match[1]] = kind
	}
	if len(containerTypes) == 0 {
		return ""
	}
	for _, signature := range auditContractSignatureRe.FindAllStringSubmatch(authoritativeTask, -1) {
		if len(signature) != 3 {
			continue
		}
		parameters := requirementAuditSplitPythonArguments(signature[2])
		for _, call := range auditPythonTraceDirectCallLineRe.FindAllStringSubmatch(output, -1) {
			if len(call) != 3 || call[1] != signature[1] {
				continue
			}
			arguments := requirementAuditSplitPythonArguments(call[2])
			if len(arguments) != len(parameters) {
				continue
			}
			for index, parameter := range parameters {
				parameter = strings.TrimSpace(parameter)
				requiredKind := containerTypes[parameter]
				if requiredKind == "" {
					continue
				}
				actualKind, certain := requirementAuditPythonLiteralContainerKind(arguments[index])
				if !certain || actualKind == requiredKind {
					continue
				}
				return "The checker made an uncaught call with parameter `" + parameter + "` as a " + actualKind +
					", even though the authoritative contract explicitly requires a " + requiredKind +
					". The observed ValueError is therefore correct rejection of the checker fixture, not an artifact discrepancy. Use a " +
					requiredKind + " for the valid-domain call; put deliberately wrong-container calls inside an exact ValueError assertion."
			}
		}
	}
	return ""
}

func requirementAuditBoolValueFixtureCorrectionHint(authoritativeTask, output string) string {
	contract := strings.ToLower(strings.Join(strings.Fields(authoritativeTask), " "))
	if !strings.Contains(output, "Traceback") || !strings.Contains(output, "ValueError") ||
		!containsAny(contract,
			"value 必須是真正 bool", "value必須是真正bool", "value 必須是 bool", "value必須是bool",
			"value must be a real bool", "value must be an actual bool", "value must be a genuine bool",
		) {
		return ""
	}
	callables := map[string]bool{}
	for _, signature := range auditContractSignatureRe.FindAllStringSubmatch(authoritativeTask, -1) {
		if len(signature) == 3 {
			callables[signature[1]] = true
		}
	}
	for _, call := range auditPythonTraceAssignedCallLineRe.FindAllStringSubmatch(output, -1) {
		if len(call) != 3 || !callables[call[1]] || !auditPythonNumericBoolMappingPairRe.MatchString(call[2]) {
			continue
		}
		return "The checker called the contract function with mapping values written as numeric `0`/`1`, but the authoritative contract requires genuine Python `bool` values. The observed `ValueError` therefore validates input rejection rather than a precedence defect. Use `False`/`True` for a valid precedence fixture, or catch `ValueError` and bind the checker only to the scalar-type requirement; this invalid fixture is not artifact evidence."
	}
	return ""
}

func requirementAuditSplitPythonArguments(raw string) []string {
	parts := make([]string, 0, 4)
	start := 0
	depth := 0
	var quote rune
	escaped := false
	for index, char := range raw {
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
		switch char {
		case '\'', '"':
			quote = char
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(raw[start:index]))
				start = index + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(raw[start:]))
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}

func requirementAuditPythonLiteralContainerKind(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	switch raw[0] {
	case '[':
		return "list", true
	case '{':
		return "dict", true
	case '\'', '"':
		return "string", true
	}
	if raw == "None" || raw == "True" || raw == "False" ||
		(raw[0] >= '0' && raw[0] <= '9') || raw[0] == '-' || raw[0] == '+' {
		return "scalar", true
	}
	return "", false
}

func requirementAuditCollapsedDuplicateMappingCorrectionHint(authoritativeTask, script, output string) string {
	contract := strings.ToLower(authoritativeTask)
	if !containsAny(contract, "duplicate", "duplicates", "unique", "不可重複", "不得重複", "重複 key", "重複鍵") ||
		!containsAny(contract, "key", "keys", "鍵") ||
		!containsAny(strings.ToLower(output), "assertionerror", "duplicate", "重複") {
		return ""
	}
	for _, rawMapping := range auditPythonFlatMappingRe.FindAllString(script, -1) {
		seen := map[string]bool{}
		for _, match := range auditPythonMappingKeyRe.FindAllStringSubmatch(rawMapping, -1) {
			if len(match) != 2 {
				continue
			}
			if seen[match[1]] {
				return "The checker tried to represent duplicate keys with one Python dict literal. Python collapses an exact duplicate literal key before the contract function is called, so this fixture cannot exercise duplicate-key rejection. Use two distinct source keys that normalize to the same authoritative key (for example one padded and one stripped spelling when trimming is specified), or another input representation explicitly supported by the contract; do not treat this checker-only collapse as an artifact discrepancy."
			}
			seen[match[1]] = true
		}
	}
	return ""
}

func requirementAuditQueryAssociationCorrectionHint(authoritativeTask, script, output string) string {
	contract := strings.ToLower(authoritativeTask)
	if !containsAny(contract,
		"按 key 排序", "按key排序", "依 key 排序", "依key排序",
		"sort by key", "sorted by key", "sort keys", "sorted keys",
	) {
		return ""
	}
	match := auditPythonExpectedGotQueryRe.FindStringSubmatch(output)
	if len(match) != 3 {
		return ""
	}
	expected, expectedOK := requirementAuditSingleValueQueryMap(match[1])
	actual, actualOK := requirementAuditSingleValueQueryMap(match[2])
	if !expectedOK || !actualOK || len(actual) < 2 || requirementAuditEqualStringMap(expected, actual) ||
		!requirementAuditSameKeysAndValueBag(expected, actual) {
		return ""
	}
	for _, rawMapping := range auditPythonFlatMappingRe.FindAllString(script, -1) {
		source := map[string]string{}
		valid := true
		for _, pair := range auditPythonStringMappingPairRe.FindAllStringSubmatch(rawMapping, -1) {
			if len(pair) != 3 {
				continue
			}
			key := strings.TrimSpace(pair[1])
			if _, exists := source[key]; exists {
				valid = false
				break
			}
			source[key] = pair[2]
		}
		if valid && len(source) == len(actual) && requirementAuditEqualStringMap(source, actual) {
			return "The checker sorted mapping keys but reassigned values between those keys in its expected query. Sorting changes pair order only; it never transfers a source value to a different key. Preserve every source key-value association, then sort the intact pairs by the authoritative normalized key before encoding; do not treat the checker-only value swap as an artifact discrepancy."
		}
	}
	return ""
}

func requirementAuditNormalizedIdentifierOutputCorrectionHint(authoritativeTask, script, output string) string {
	contract := strings.ToLower(authoritativeTask)
	if !containsAny(contract,
		"輸出 id 使用正規化", "輸出id使用正規化", "輸出 id 為正規化", "輸出id為正規化",
		"output id uses the normalized", "output id is the normalized", "normalized id in output",
	) || !strings.Contains(contract, "casefold") ||
		!containsAny(script, `["id"]`, `['id']`) {
		return ""
	}
	match := auditPythonExpectedGotListRe.FindStringSubmatch(output)
	if len(match) != 3 {
		return ""
	}
	expected := requirementAuditQuotedStrings(match[1])
	actual := requirementAuditQuotedStrings(match[2])
	if len(expected) == 0 || len(expected) != len(actual) {
		return ""
	}
	changed := false
	for index := range expected {
		if !requirementAuditASCII(expected[index]) || !requirementAuditASCII(actual[index]) {
			return ""
		}
		normalized := strings.ToLower(strings.TrimSpace(expected[index]))
		if normalized != expected[index] {
			changed = true
		}
		if normalized != actual[index] {
			return ""
		}
	}
	if !changed {
		return ""
	}
	return "The checker expected original identifier casing even though the authoritative contract explicitly requires normalized casefolded IDs in the output. Apply the stated strip/casefold normalization to each expected output ID; sorting by normalized ID and the output representation are separate clauses, so do not preserve raw casing unless another exact clause says to do so."
}

func requirementAuditQuotedStrings(raw string) []string {
	matches := auditPythonQuotedStringRe.FindAllStringSubmatch(raw, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) == 2 {
			values = append(values, match[1])
		}
	}
	return values
}

func requirementAuditASCII(value string) bool {
	for _, char := range value {
		if char > 127 {
			return false
		}
	}
	return true
}

func requirementAuditSingleValueQueryMap(raw string) (map[string]string, bool) {
	parsed, err := url.ParseQuery(raw)
	if err != nil || len(parsed) == 0 {
		return nil, false
	}
	result := make(map[string]string, len(parsed))
	for key, values := range parsed {
		if len(values) != 1 {
			return nil, false
		}
		result[key] = values[0]
	}
	return result, true
}

func requirementAuditEqualStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func requirementAuditSameKeysAndValueBag(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	values := map[string]int{}
	for key, value := range left {
		if _, ok := right[key]; !ok {
			return false
		}
		values[value]++
	}
	for _, value := range right {
		values[value]--
		if values[value] < 0 {
			return false
		}
	}
	for _, count := range values {
		if count != 0 {
			return false
		}
	}
	return true
}

func requirementAuditHasInconsistentMappingFixture(script, requiredKey string) bool {
	requiredKey = strings.TrimSpace(requiredKey)
	if requiredKey == "" {
		return false
	}
	var referenceKeys map[string]bool
	var candidates []map[string]bool
	for _, rawMapping := range auditPythonFlatMappingRe.FindAllString(script, -1) {
		keys := map[string]bool{}
		for _, match := range auditPythonMappingKeyRe.FindAllStringSubmatch(rawMapping, -1) {
			if len(match) == 2 {
				keys[match[1]] = true
			}
		}
		if len(keys) < 3 {
			continue
		}
		if keys[requiredKey] && (referenceKeys == nil || len(keys) > len(referenceKeys)) {
			referenceKeys = keys
		}
		candidates = append(candidates, keys)
	}
	if referenceKeys == nil {
		return false
	}
	for _, keys := range candidates {
		if keys[requiredKey] {
			continue
		}
		overlap := 0
		for key := range keys {
			if referenceKeys[key] {
				overlap++
			}
		}
		if overlap >= 2 {
			return true
		}
	}
	return false
}

// requirementAuditShadowedContractCallable rejects a checker that defines the
// callable named by the authoritative task instead of importing the current
// artifact. Such a program only tests its own duplicate implementation and can
// manufacture passing coverage even while the artifact is wrong. The rule is
// structural and task-agnostic: it never derives an expected business value.
func requirementAuditShadowedContractCallable(authoritativeTask, script string) string {
	contractCallables := map[string]bool{}
	for _, match := range auditContractCallableRe.FindAllStringSubmatch(authoritativeTask, -1) {
		if len(match) == 2 {
			contractCallables[match[1]] = true
		}
	}
	for _, match := range auditPythonFunctionDefinitionRe.FindAllStringSubmatch(script, -1) {
		if len(match) == 2 && contractCallables[match[1]] {
			return match[1]
		}
	}
	return ""
}

func requirementAuditRequiredExceptionTrace(authoritativeTask, output string) string {
	if !strings.Contains(strings.ToLower(output), "traceback") {
		return ""
	}
	for _, exception := range []string{
		"ValueError", "TypeError", "KeyError", "IndexError", "RuntimeError", "FileNotFoundError",
	} {
		if !strings.Contains(authoritativeTask, exception) {
			continue
		}
		if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(exception) + `(?:\s*:|\s*$)`).MatchString(output) {
			return exception
		}
	}
	return ""
}

func requirementAuditDurationUnitMismatch(authoritativeTask, script string) (auditDurationUnitMismatch, bool) {
	contracts := make([]auditDurationUnitMismatch, 0, 2)
	seen := map[string]bool{}
	for _, match := range auditNamedDurationSignatureRe.FindAllStringSubmatch(authoritativeTask, -1) {
		if len(match) != 4 {
			continue
		}
		key := match[1] + "\x00" + match[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		contracts = append(contracts, auditDurationUnitMismatch{
			Function: match[1], Identifier: match[2], DeclaredUnit: match[3],
		})
	}
	for _, contract := range contracts {
		for _, match := range auditDirectDurationExpectationRe.FindAllStringSubmatch(script, -1) {
			if len(match) != 5 || match[1] != contract.Function {
				continue
			}
			if mismatch, ok := durationFixtureUnitMismatch(contract, match[2], match[3], match[4]); ok {
				return mismatch, true
			}
		}
		if mismatch, ok := durationCallUnitLabelMismatch(contract, script); ok {
			return mismatch, true
		}
	}
	// Table-driven checkers commonly call the contract function with loop
	// variables, so the literal fixtures are not adjacent to the call. Only use
	// table rows when the task exposes one unambiguous named-duration function
	// and that function is actually invoked by the checker.
	if len(contracts) == 1 && strings.Contains(script, contracts[0].Function+"(") {
		for _, match := range auditDurationFixtureRowRe.FindAllStringSubmatch(script, -1) {
			if len(match) != 4 {
				continue
			}
			if mismatch, ok := durationFixtureUnitMismatch(contracts[0], match[1], match[2], match[3]); ok {
				return mismatch, true
			}
		}
	}
	return auditDurationUnitMismatch{}, false
}

func durationFixtureUnitMismatch(
	contract auditDurationUnitMismatch,
	startText, valueText, expectedText string,
) (auditDurationUnitMismatch, bool) {
	start, startErr := time.Parse("2006-01-02T15:04", startText)
	expected, expectedErr := time.Parse("2006-01-02T15:04", expectedText)
	value, valueErr := strconv.ParseFloat(valueText, 64)
	if startErr != nil || expectedErr != nil || valueErr != nil || value <= 0 {
		return auditDurationUnitMismatch{}, false
	}
	declaredDuration := time.Duration(value * float64(time.Hour))
	inferredDuration := time.Duration(value * float64(time.Minute))
	inferredUnit := "minutes"
	if contract.DeclaredUnit == "minutes" {
		declaredDuration, inferredDuration = inferredDuration, declaredDuration
		inferredUnit = "hours"
	}
	delta := expected.Sub(start)
	if delta != inferredDuration || delta == declaredDuration {
		return auditDurationUnitMismatch{}, false
	}
	contract.InferredUnit = inferredUnit
	contract.Value = valueText
	contract.Start = startText
	contract.Expected = expectedText
	return contract, true
}

func durationCallUnitLabelMismatch(
	contract auditDurationUnitMismatch,
	script string,
) (auditDurationUnitMismatch, bool) {
	lines := strings.Split(script, "\n")
	for _, indexes := range auditDurationCallLiteralRe.FindAllStringSubmatchIndex(script, -1) {
		if len(indexes) != 8 || script[indexes[2]:indexes[3]] != contract.Function {
			continue
		}
		value := script[indexes[6]:indexes[7]]
		lineIndex := strings.Count(script[:indexes[0]], "\n")
		firstLine := lineIndex - 4
		if firstLine < 0 {
			firstLine = 0
		}
		lastLine := lineIndex + 2
		if lastLine > len(lines) {
			lastLine = len(lines)
		}
		context := strings.Join(lines[firstLine:lastLine], "\n")
		escapedValue := regexp.QuoteMeta(value)
		oppositeUnitPattern := `(?i)(?:\b` + escapedValue + `\s*min(?:ute)?s?\b|` + escapedValue + `\s*分鐘)`
		inferredUnit := "minutes"
		if contract.DeclaredUnit == "minutes" {
			oppositeUnitPattern = `(?i)(?:\b` + escapedValue + `\s*h(?:ou)?rs?\b|` + escapedValue + `\s*小時)`
			inferredUnit = "hours"
		}
		if !regexp.MustCompile(oppositeUnitPattern).MatchString(context) {
			continue
		}
		contract.InferredUnit = inferredUnit
		contract.Value = value
		contract.Start = script[indexes[4]:indexes[5]]
		return contract, true
	}
	return auditDurationUnitMismatch{}, false
}

func requirementAuditDurationUnitCorrection(mismatch auditDurationUnitMismatch) string {
	detail := "fixture value " + mismatch.Value
	if mismatch.Start != "" && mismatch.Expected != "" {
		detail += " from " + mismatch.Start + " to " + mismatch.Expected
	} else if mismatch.Start != "" {
		detail += " at " + mismatch.Start
	}
	return "The checker changed the unit named by authoritative parameter `" + mismatch.Identifier +
		"`: " + detail + " was labeled or derived as " + mismatch.InferredUnit + " even though the parameter names " +
		mismatch.DeclaredUnit + ". Preserve the named unit when deriving fixtures and expected timestamps; do not reinterpret `_" +
		mismatch.DeclaredUnit + "` as " + mismatch.InferredUnit + "."
}

func requirementAuditPassingEvidencePrompt(
	callID string,
	coveredIDs, unboundIDs []string,
	boundaryGuidance ...string,
) string {
	covered := strings.Join(coveredIDs, ",")
	if len(unboundIDs) > 0 {
		prompt := "(Audit checker `" + callID + "` exited 0 but did not executable-bind `" + strings.Join(unboundIDs, ",") + "`. Select only IDs actually asserted by the script, and exercise every explicit strict boundary at the named threshold. For a strict input-domain boundary, assert deliberate rejection at the threshold; an incidental arithmetic error such as ZeroDivisionError does not prove validation of that field. Use `run_requirement_check` with only `requirement_ids` and raw Python `script`; do not replay the selected validator, add untested IDs, or modify artifacts."
		if len(boundaryGuidance) > 0 && strings.TrimSpace(boundaryGuidance[0]) != "" {
			prompt += " " + strings.TrimSpace(boundaryGuidance[0])
		}
		if covered != "" {
			prompt += " Typed IDs recorded from this checker: " + covered + "."
		}
		return prompt + ")"
	}
	if covered == "" {
		return "(Audit checker `" + callID + "` exited 0 but was not bound to structured requirement IDs. Rerun the same bounded read-only predicates with `requirement_ids` listing only the stable IDs visibly asserted by the script. Do not print audit protocol markers, replay the selected validator, add untested IDs, or modify artifacts.)"
	}
	return "(Audit checker `" + callID + "` passed and recorded structured IDs `" + covered + "`, but the ledger still has uncovered clauses. Run one materially different read-only checker for the remaining IDs, selecting only clauses that program visibly asserts in `requirement_ids`. Do not print audit protocol markers, replay the selected validator, or modify artifacts.)"
}

// requirementAuditBoundaryEvidenceGuidance turns the already parsed atomic
// contract into a concrete checker obligation for a small model. It supplies
// no business answer: the model still constructs the input and the artifact
// still decides whether the named threshold is deliberately rejected.
func (l *RequirementAuditLedger) requirementAuditBoundaryEvidenceGuidance(ids []string) string {
	if l == nil || len(ids) == 0 {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[strings.ToUpper(strings.TrimSpace(id))] = true
	}
	parts := make([]string, 0, len(ids))
	for _, item := range l.contractItems {
		if !wanted[item.ID] {
			continue
		}
		obligation, ok := parseRequirementAuditBoundaryObligation(item.Quote)
		if !ok {
			continue
		}
		fixture := "set field `" + obligation.Field + "` to the exact threshold `" + obligation.Value + "`"
		if obligation.CollectionValues {
			fixture = "set one value inside `" + obligation.Field + "` to the exact threshold `" + obligation.Value + "`"
		}
		parts = append(parts,
			"Missing atomic clause `"+item.ID+": "+item.Quote+"`: "+fixture+
				" and put the contract call itself inside the rejection check. Count an explicit domain rejection such as `ValueError` (or a documented false/empty rejection) as covered. If it returns normally or raises an incidental arithmetic/runtime error, exit non-zero. Select only `"+item.ID+"` in requirement_ids for this localized checker.",
		)
	}
	return strings.Join(parts, " ")
}
