package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const DefaultShellTool = "run_shell_command"

type ValidationSource string

const (
	ValidationSourceNone       ValidationSource = "none"
	ValidationSourceExplicit   ValidationSource = "explicit"
	ValidationSourceRepository ValidationSource = "repository"
	ValidationSourceModel      ValidationSource = "model"
)

// ValidationRequest contains facts known before the first model turn. Command
// selection is deterministic: an explicit user command wins, otherwise a
// repository-native command is discovered only when repository validation is
// required.
type ValidationRequest struct {
	CWD                  string
	ExplicitCommand      string
	RepositoryValidation bool
	ShellTool            string
}

type ValidationDecision struct {
	Required bool               `json:"required"`
	Command  string             `json:"command,omitempty"`
	Source   ValidationSource   `json:"source"`
	Status   VerificationStatus `json:"status"`
	Fresh    bool               `json:"fresh"`
	Reason   string             `json:"reason"`
}

type ValidationToolCheck struct {
	Validation             bool
	Accepted               bool
	ReplayedFailure        bool
	NormalizedExitContract bool
	NormalizedScriptMode   bool
	NormalizedAuditMarker  bool
	Command                string
	Error                  string
	Call                   provider.ToolCall
}

type VerificationObservation struct {
	Validation    bool
	ValidatorID   string
	Status        VerificationStatus
	ExitCode      int
	HasExitCode   bool
	FailureOutput bool
}

// Verifier owns validation command selection and interpretation. It has no
// shell or filesystem mutation capability; ToolExecutor remains the only
// execution boundary.
type Verifier struct {
	cwd              string
	command          string
	source           ValidationSource
	required         bool
	shellTool        string
	fallbackAttempts int
	lastPassingCall  string
	lastPassingArgs  map[string]any
	lastFailedCall   string
	lastFailedOutput string
	mutationEpoch    uint64
	lastFailedEpoch  uint64
}

func NewVerifier(request ValidationRequest) *Verifier {
	v := &Verifier{
		cwd:       strings.TrimSpace(request.CWD),
		shellTool: strings.TrimSpace(request.ShellTool),
	}
	if v.shellTool == "" {
		v.shellTool = DefaultShellTool
	}
	if explicit := strings.TrimSpace(request.ExplicitCommand); explicit != "" {
		v.command = explicit
		v.source = ValidationSourceExplicit
		v.required = true
		return v
	}
	if request.RepositoryValidation {
		v.command = DiscoverRepositoryValidationCommand(request.CWD)
		if v.command != "" {
			v.source = ValidationSourceRepository
			v.required = true
			return v
		}
	}
	v.source = ValidationSourceNone
	return v
}

func (v *Verifier) Command() string {
	if v == nil {
		return ""
	}
	return v.command
}

func (v *Verifier) Source() ValidationSource {
	if v == nil {
		return ValidationSourceNone
	}
	return v.source
}

// BindAuthoritativeCommand promotes a validator discovered later from a named
// TASK/spec source. Delegated-task wrappers often omit the real command, so a
// model-authored narrow checker may have been selected provisionally. A direct
// user or repository-selected validator keeps precedence; only none/model is
// replaced. Promoting the command clears stale pass/fail fingerprints because
// they belong to a weaker validator.
func (v *Verifier) BindAuthoritativeCommand(command string) bool {
	if v == nil {
		return false
	}
	command = strings.TrimSpace(command)
	if command == "" || v.source == ValidationSourceExplicit ||
		v.source == ValidationSourceRepository {
		return false
	}
	v.command = command
	v.source = ValidationSourceExplicit
	v.required = true
	v.fallbackAttempts = 0
	v.lastPassingCall = ""
	v.lastPassingArgs = nil
	v.lastFailedCall = ""
	v.lastFailedOutput = ""
	v.lastFailedEpoch = 0
	return true
}

// InvalidateModelValidatorAfterAuditContradiction retires a model-authored
// acceptance program after an independent audit proves that the generation it
// accepted violates the authoritative contract. Keeping that command selected
// can reintroduce the same wrong oracle immediately after repair (especially
// when the script both generates and checks an artifact). Explicit user and
// repository-selected validators remain authoritative and are never cleared.
func (v *Verifier) InvalidateModelValidatorAfterAuditContradiction() bool {
	if v == nil || v.source != ValidationSourceModel {
		return false
	}
	v.command = ""
	v.source = ValidationSourceNone
	v.required = false
	v.fallbackAttempts = 0
	v.lastPassingCall = ""
	v.lastPassingArgs = nil
	v.lastFailedCall = ""
	v.lastFailedOutput = ""
	v.lastFailedEpoch = 0
	return true
}

// LastPassingCallFingerprint identifies the executable acceptance program,
// not only its interpreter command. It lets a fresh requirement audit
// distinguish new independent evidence from replaying the exact
// model-authored validator that supported the original claim.
func (v *Verifier) LastPassingCallFingerprint() string {
	if v == nil {
		return ""
	}
	return v.lastPassingCall
}

// ReplayLastPassingCall returns the exact executable acceptance program that
// most recently passed. It is used only to revalidate bytes restored from a
// content-addressed checkpoint, not as independent requirement evidence.
func (v *Verifier) ReplayLastPassingCall(id string) (provider.ToolCall, bool) {
	if v == nil || len(v.lastPassingArgs) == 0 || strings.TrimSpace(v.shellTool) == "" {
		return provider.ToolCall{}, false
	}
	arguments, err := json.Marshal(v.lastPassingArgs)
	if err != nil {
		return provider.ToolCall{}, false
	}
	return provider.ToolCall{
		ID: id, Type: "function",
		Function: provider.FunctionCall{Name: v.shellTool, Arguments: string(arguments)},
	}, true
}

// DistinctAuditProgramRequired is intentionally narrow. Conventional
// repository validators such as go test or pytest may be rerun unchanged in a
// fresh context; a bare interpreter plus model-supplied script must be rebuilt
// so the audit is not the original self-check replayed verbatim.
func (v *Verifier) DistinctAuditProgramRequired() bool {
	if v == nil || v.source != ValidationSourceModel {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v.command)) {
	case "python", "python3", "node", "bash", "zsh":
		return true
	default:
		return false
	}
}

// Decision evaluates the selected validator against append-only evidence.
// Passing evidence from a different command never counts as fresh.
func (v *Verifier) Decision(evidence EvidenceSnapshot) ValidationDecision {
	if v == nil || (!v.required && v.command == "") {
		return ValidationDecision{
			Source: ValidationSourceNone, Status: VerificationNotRequired,
			Reason: "verification_not_required",
		}
	}
	decision := ValidationDecision{
		Required: true, Command: v.command, Source: v.source,
		Status: evidence.Verification,
	}
	if v.command == "" {
		decision.Status = VerificationUnverified
		decision.Reason = "repository_validator_unavailable"
		return decision
	}
	if evidence.ValidatorID != "" && evidence.ValidatorID != v.command {
		decision.Status = VerificationUnverified
		decision.Reason = "validator_mismatch"
		return decision
	}
	switch evidence.Verification {
	case VerificationPassed:
		decision.Fresh = evidence.ValidatorID == v.command
		if decision.Fresh {
			decision.Reason = "fresh_validation_passed"
		} else {
			decision.Status = VerificationUnverified
			decision.Reason = "selected_validator_not_run"
		}
	case VerificationFailed:
		decision.Reason = "validation_failed"
	case VerificationStale:
		decision.Reason = "validation_stale_after_edit"
	default:
		decision.Status = VerificationUnverified
		decision.Reason = "selected_validator_not_run"
	}
	return decision
}

// CheckToolCall classifies an exact selected-validator invocation and
// canonicalizes its workspace directory. Other shell commands remain ordinary
// tool calls; ToolExecutor authorization, not Verifier, owns their safety.
func (v *Verifier) CheckToolCall(call provider.ToolCall) ValidationToolCheck {
	check := ValidationToolCheck{Accepted: true, Call: call}
	if v == nil || call.Function.Name != v.shellTool {
		return check
	}
	args := map[string]any{}
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
		return check
	}
	// Verification policy runs before ToolExecutor's schema decoder. Apply the
	// same one-layer, schema-directed repair here so a native model's
	// `"expected_exit_codes":"[0]"` wire shape is judged as the advertised
	// integer array instead of being falsely rejected as a non-zero whitelist.
	normalizeStringifiedStructuredArguments(args, validationToolArgumentSchema)
	tools.NormalizeShellArguments(args)
	got, _ := args["command"].(string)
	got = strings.TrimSpace(got)
	purpose, _ := args["purpose"].(string)
	modelDeclared := strings.EqualFold(strings.TrimSpace(purpose), "validation")
	implicitAssertion := !modelDeclared && hasExecutableValidationFailureSignal(args)
	if implicitAssertion {
		// Qwen occasionally emits a real assertion-based checker but omits the
		// schema metadata. Preserve model ownership of the checker while repairing
		// only that generic protocol field.
		args["purpose"] = "validation"
	}
	if modelDeclared && isTrivialModelDeclaredValidation(args) {
		check.Accepted = false
		check.Error = "ERROR: purpose=validation requires a check that can fail. Add an assertion, raise/throw, non-zero exit, test command, or checker that returns failure when the acceptance condition is false."
		return check
	}
	recognizableValidator := looksLikeValidationCommand(got)
	if (modelDeclared || recognizableValidator) &&
		validationCommandMasksFailure(validationCommandBody(got)) {
		check.Accepted = false
		check.Error = "ERROR: validation commands must preserve the checker's real exit status. Remove trailing echo/printf/true/exit 0, `|| true`, and ordinary pipelines that can mask failure."
		return check
	}
	if validationUsesZeroForAcceptance(args) &&
		!expectsOnlyZeroExit(args) {
		// Conventional validators and assertion-style checker programs use
		// process zero to mean that their acceptance predicate passed. Allowing a
		// model to whitelist exit 1 here turns an AssertionError into false passing
		// evidence and can checkpoint incorrect bytes. Keep raw negative probes
		// such as `curl expected_exit_codes=[7]` intact: they have no embedded
		// assertion/failure program, and their non-zero status is the fact tested.
		args["expected_exit_codes"] = []int{0}
		check.NormalizedExitContract = true
	}
	credibleModelValidator := modelDeclared || implicitAssertion
	if (credibleModelValidator || recognizableValidator) && got != "" &&
		(v.command == "" || v.source == ValidationSourceModel) {
		v.command = got
		v.source = ValidationSourceModel
		v.required = true
	}
	if v.source == ValidationSourceModel &&
		!credibleModelValidator && !recognizableValidator {
		return check
	}
	if v.command == "" {
		return check
	}
	check.Validation = true
	check.Command = v.command
	if v.commandMatches(got) {
		// The shell tool maps /workspace only through its directory argument.
		// Execute the selected command verbatim at that mapped root instead of
		// passing a model-generated `cd /workspace && ...` prefix to zsh.
		args["command"] = v.command
		if directory, _ := args["directory"].(string); strings.TrimSpace(directory) == "" {
			args["directory"] = "/workspace"
		}
		normalized, _ := json.Marshal(args)
		check.Call.Function.Arguments = string(normalized)
		if fingerprint := validationCallFingerprint(args); fingerprint != "" &&
			fingerprint == v.lastFailedCall && v.lastFailedEpoch == v.mutationEpoch {
			check.Accepted = false
			check.Validation = false
			check.ReplayedFailure = true
			check.Error = "ERROR: this exact validator already failed and the workspace has not changed since that failure. Do not rerun it against unchanged bytes. If its output identifies a concrete gap, send one native edit/write call; otherwise run one materially different bounded diagnostic. Rerun this validator only after a successful mutation."
			if strings.TrimSpace(v.lastFailedOutput) != "" {
				check.Error += " Previous failure output:\n" + v.lastFailedOutput
			}
		}
		return check
	}
	check.Validation = false
	return check
}

// validationUsesZeroForAcceptance identifies checker forms whose conventional
// success contract is exit 0. A model cannot turn a mismatching diff, cmp,
// grep, test predicate, or executable checker into passing evidence merely by
// whitelisting its failure exit code. Raw network probes remain excluded: a
// caller may intentionally verify a specific curl transport failure.
func validationUsesZeroForAcceptance(args map[string]any) bool {
	command, _ := args["command"].(string)
	body := validationCommandBody(command)
	if looksLikeValidationCommand(body) || hasExecutableValidationFailureSignal(args) ||
		isExternalCheckerInvocation(body) || directCheckerValidationCommandRe.MatchString(body) {
		return true
	}
	return zeroExitShellPredicateValidationCommandRe.MatchString(body)
}

var validationToolArgumentSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"expected_exit_codes": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "integer"},
		},
	},
}

// InspectToolCall classifies a call without changing the selected validator.
// Post-execution guards use it to inspect the raw transport spelling after
// Observe has already selected the normalized executable command.
func (v *Verifier) InspectToolCall(call provider.ToolCall) ValidationToolCheck {
	if v == nil {
		return ValidationToolCheck{Accepted: true, Call: call}
	}
	clone := *v
	// Inspection classifies a call that already executed. The failed-replay
	// guard is a pre-execution policy and must not erase the concrete non-zero
	// evidence that unlocks a targeted repair.
	clone.lastFailedCall = ""
	return clone.CheckToolCall(call)
}

// CheckIndependentAuditToolCall applies the stronger evidence contract used
// after the first passing validator. Observation-only commands may be useful
// diagnostics, but listing hashes or echoing derived values cannot prove that
// an artifact satisfies the requested contract because those commands have no
// false-condition exit path. Work on a clone so a rejected audit candidate
// cannot replace the selected validator.
func (v *Verifier) CheckIndependentAuditToolCall(call provider.ToolCall) ValidationToolCheck {
	if v == nil {
		return ValidationToolCheck{Accepted: true, Call: call}
	}
	clone := *v
	check := clone.CheckToolCall(call)
	if !check.Accepted || !check.Validation {
		return check
	}
	args := map[string]any{}
	if json.Unmarshal([]byte(check.Call.Function.Arguments), &args) != nil ||
		!hasIndependentValidationFailureSemantics(args) {
		check.Accepted = false
		check.Validation = false
		check.Error = "ERROR: an independent audit validator must be able to fail when the artifact violates the requirement. Use an assertion/comparison, a conventional test command, or an executable checker. Printing files, hashes, counts, or expected-looking values is diagnostic evidence only."
		return check
	}
	*v = clone
	return check
}

func expectsOnlyZeroExit(args map[string]any) bool {
	values, exists := args["expected_exit_codes"]
	if !exists {
		return true
	}
	var codes []any
	switch typed := values.(type) {
	case []any:
		codes = typed
	case []int:
		if len(typed) != 1 {
			return false
		}
		return typed[0] == 0
	default:
		return false
	}
	if len(codes) != 1 {
		return false
	}
	switch value := codes[0].(type) {
	case float64:
		return value == 0
	case int:
		return value == 0
	case json.Number:
		return value.String() == "0"
	default:
		return false
	}
}

// CheckRequiredToolCall is the strict form used only when the controller has
// explicitly entered a forced validation turn. It prevents a draft for a
// different command from being mistaken for the selected repository validator.
func (v *Verifier) CheckRequiredToolCall(call provider.ToolCall) ValidationToolCheck {
	check := v.CheckToolCall(call)
	if v == nil || v.command == "" {
		check.Accepted = false
		check.Error = "ERROR: no validation command is available."
		return check
	}
	if call.Function.Name != v.shellTool {
		check.Accepted = false
		check.Error = fmt.Sprintf("ERROR: validation requires %s with command exactly `%s`.", v.shellTool, v.command)
		return check
	}
	if !check.Validation {
		args := map[string]any{}
		if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
			check.Accepted = false
			check.Error = "ERROR: run_shell_command arguments were invalid JSON. Call run_shell_command again with command exactly: " + v.command
			return check
		}
		got, _ := args["command"].(string)
		check.Accepted = false
		check.Error = fmt.Sprintf("ERROR: wrong validation command. The user asked to run exactly `%s`, but this tool call tried `%s`. Fix the code if needed, then call run_shell_command with command exactly `%s`.", v.command, strings.TrimSpace(got), v.command)
	}
	return check
}

func (v *Verifier) AcceptsOnlyCall(calls []provider.ToolCall) bool {
	if v == nil || v.command == "" || len(calls) != 1 {
		return false
	}
	check := v.CheckRequiredToolCall(calls[0])
	return check.Validation && check.Accepted
}

// NextFallbackCall creates, but never executes, one deterministic validator
// call. The bounded counter makes duplicate validation explicit.
func (v *Verifier) NextFallbackCall(turn, maxAttempts int) (provider.ToolCall, bool) {
	if v == nil || v.command == "" || maxAttempts <= 0 || v.fallbackAttempts >= maxAttempts {
		return provider.ToolCall{}, false
	}
	v.fallbackAttempts++
	arguments, _ := json.Marshal(map[string]any{
		"command": v.command, "directory": "/workspace", "timeout": 60000,
	})
	return provider.ToolCall{
		ID:   fmt.Sprintf("meadow-core-validator-%d", turn),
		Type: "function",
		Function: provider.FunctionCall{
			Name: v.shellTool, Arguments: string(arguments),
		},
	}, true
}

func (v *Verifier) Observe(execution ToolExecution) VerificationObservation {
	observation := VerificationObservation{}
	if v == nil || execution.Tool != v.shellTool {
		return observation
	}
	command, _ := execution.Args["command"].(string)
	command = strings.TrimSpace(command)
	purpose, _ := execution.Args["purpose"].(string)
	modelDeclared := strings.EqualFold(strings.TrimSpace(purpose), "validation")
	recognizableValidator := looksLikeValidationCommand(command)
	credibleModelValidator := (modelDeclared || hasExecutableValidationFailureSignal(execution.Args)) &&
		!isTrivialModelDeclaredValidation(execution.Args)
	if (credibleModelValidator || recognizableValidator) &&
		(v.command == "" || v.source == ValidationSourceModel) {
		v.command = command
		v.source = ValidationSourceModel
		v.required = true
	}
	if v.source == ValidationSourceModel &&
		!credibleModelValidator && !recognizableValidator {
		return observation
	}
	if v.command == "" || !v.commandMatches(command) {
		return observation
	}
	observation.Validation = true
	observation.ValidatorID = v.command
	observation.ExitCode, observation.HasExitCode = executionExitCode(execution.Display)
	observation.FailureOutput = validationOutputHasFailureSignal(execution.LLMContent)
	if execution.Status == ToolOK &&
		expectedValidationExitCode(execution.Args, observation.ExitCode, observation.HasExitCode) &&
		!observation.FailureOutput {
		observation.Status = VerificationPassed
		v.lastPassingCall = validationCallFingerprint(execution.Args)
		v.lastPassingArgs = cloneValidationArgs(execution.Args)
	} else {
		observation.Status = VerificationFailed
		if execution.Executed && observation.HasExitCode {
			v.lastFailedCall = validationCallFingerprint(execution.Args)
			v.lastFailedOutput = boundedValidatorFailureOutput(execution.LLMContent)
			v.lastFailedEpoch = v.mutationEpoch
		}
	}
	return observation
}

func boundedValidatorFailureOutput(content string) string {
	content = strings.TrimSpace(content)
	const maxBytes = 2400
	if len(content) <= maxBytes {
		return content
	}
	return content[:maxBytes] + "…"
}

// validationOutputHasFailureSignal prevents a checker that prints an explicit
// failure verdict but forgets to exit non-zero from becoming passing evidence.
// Match only line-level verdicts, not arbitrary mentions such as test names or
// "0 errors", so successful conventional validators retain their exit-code
// contract.
func validationOutputHasFailureSignal(value string) bool {
	for _, rawLine := range strings.Split(value, "\n") {
		line := strings.ToUpper(strings.TrimSpace(rawLine))
		// The shell result renderer prefixes the first line of each output
		// stream. Remove that transport label once before interpreting the
		// line-level verdict; do not search arbitrary prose or quoted source.
		for _, streamLabel := range []string{"STDOUT:", "STDERR:"} {
			if strings.HasPrefix(line, streamLabel) {
				line = strings.TrimSpace(strings.TrimPrefix(line, streamLabel))
				break
			}
		}
		if line == "FAIL" || line == "FAILED" ||
			line == "VALIDATION FAILED" || line == "VERIFICATION FAILED" {
			return true
		}
		for _, prefix := range []string{
			"FAIL ", "FAIL:", "FAILED ", "FAILED:",
			"ERROR ", "ERROR:", "MISMATCH ", "MISMATCH:",
		} {
			if strings.HasPrefix(line, prefix) {
				return true
			}
		}
	}
	return false
}

// ObserveMutation advances the validator's workspace epoch only for a real,
// successful mutation. A failed, blocked, or no-op write must not make an
// unchanged failed validator eligible for replay.
func (v *Verifier) ObserveMutation(execution ToolExecution) {
	if v == nil || !execution.Executed || execution.Status != ToolOK || execution.NoChange {
		return
	}
	if execution.Effect != EffectWrite && len(execution.ChangedPaths) == 0 {
		return
	}
	v.mutationEpoch++
}

func cloneValidationArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	clone := map[string]any{}
	if json.Unmarshal(encoded, &clone) != nil {
		return nil
	}
	return clone
}

func validationCallFingerprint(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	// Transport-only knobs and purpose labels do not make an acceptance
	// program independent. Keep executable source, working directory and a
	// canonical exit contract so omitted [0] and explicit [0] are identical.
	payload := map[string]any{}
	for _, key := range []string{
		"command", "script", "directory",
	} {
		if value, ok := args[key]; ok {
			payload[key] = value
		}
	}
	if expectsOnlyZeroExit(args) {
		payload["expected_exit_codes"] = []int{0}
	} else if value, ok := args["expected_exit_codes"]; ok {
		payload["expected_exit_codes"] = value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}

var recognizableValidationCommand = regexp.MustCompile(
	`(?i)^(?:env\s+(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)*)?` +
		`(?:` +
		`go\s+(?:test|vet|build)\b|` +
		`python(?:3(?:\.\d+)?)?\s+-m\s+(?:unittest|pytest)\b|` +
		`pytest\b|` +
		`(?:npm|pnpm|yarn|bun)\s+(?:test|run\s+(?:test|build|lint|typecheck))\b|` +
		`cargo\s+(?:test|check|build|clippy)\b|` +
		`swift\s+(?:test|build)\b|` +
		`dotnet\s+(?:test|build)\b|` +
		`(?:mvn|gradle|gradlew)\s+(?:test|check|verify|build)\b|` +
		`make\s+(?:test|check|verify|build)\b|` +
		`(?:ruff\s+check|eslint|tsc\b)` +
		`)`,
)

// looksLikeValidationCommand recognizes only conventional test/build/lint
// command heads. Arbitrary scripts and python -c remain diagnostic unless the
// model explicitly labels them purpose=validation.
func looksLikeValidationCommand(command string) bool {
	command = validationCommandBody(command)
	return recognizableValidationCommand.MatchString(command)
}

func validationCommandBody(command string) string {
	command = stripValidatorOutputMerge(strings.TrimSpace(command))
	for _, separator := range []string{"&&", ";"} {
		if index := strings.LastIndex(command, separator); index >= 0 &&
			workingDirectoryHopRe.MatchString(strings.TrimSpace(command[:index])) {
			command = strings.TrimSpace(command[index+len(separator):])
			break
		}
	}
	return command
}

var shellSequenceSeparator = regexp.MustCompile(`\s*(?:&&|\|\||[;|])\s*`)
var validationFailureMaskingTailRe = regexp.MustCompile(
	`(?i)(?:;|\|\|)\s*(?:echo\b|printf\b|true\b|:\s*(?:$|;)|exit\s+0\b)`,
)
var validationFailureMaskingPipeRe = regexp.MustCompile(`(^|[^|])\|([^|]|$)`)
var inlineInterpreterValidatorRe = regexp.MustCompile(
	`(?is)^\s*(?:python(?:3(?:\.\d+)?)?\s+-c|node\s+-e|(?:ba|z)sh\s+-c)\b`,
)
var observationOnlyValidationCommandRe = regexp.MustCompile(
	`(?i)^\s*(?:env\s+(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)*)?` +
		`(?:python(?:3(?:\.\d+)?)?\s+-m\s+(?:json\.tool|py_compile)\b|node\s+--check\b)`,
)
var interpreterValidationFailureSignalRe = regexp.MustCompile(
	`(?is)` +
		`\bassert(?:\s|\()|` +
		`\braise\s+|` +
		`\bsys\.exit\s*\(|` +
		`\bSystemExit\b|` +
		`\bprocess\.exit(?:Code)?\b|` +
		`\bthrow\s+|` +
		`\bsubprocess\.(?:check_call|check_output)\s*\(|` +
		`\bsubprocess\.run\s*\(.*?\bcheck\s*=\s*True\b|` +
		`\.check_returncode\s*\(|` +
		`\b(?:unittest|pytest)\.(?:main|console_main)\s*\(|` +
		`\bjsonschema\.validate\s*\(|` +
		`\bexit\s*\(\s*[1-9]\d*\s*\)|` +
		`\bexit\s+[1-9]\d*\b`,
)

// Shell predicates are language-specific. In particular, treating a bare `[`
// as a failure signal for every interpreter makes ordinary Python list/index
// syntax look like an executable assertion after an inline script is
// normalized. Keep these patterns out of Python and Node source.
var shellScriptValidationFailureSignalRe = regexp.MustCompile(
	`(?im)` +
		`(?:^|[\s;])set\s+-[^\n;]*e\b|` +
		`(?:^|[\s;])set\s+-o\s+errexit\b|` +
		`(?:^|[\s;])(?:test|\[\[?|diff|cmp|grep)\s+`,
)

func scriptHasValidationFailureSignal(commandBody, source string) bool {
	stripped := stripNonCodeText(source)
	if interpreterValidationFailureSignalRe.MatchString(stripped) {
		return true
	}
	fields := strings.Fields(strings.TrimSpace(commandBody))
	if len(fields) == 0 {
		return false
	}
	head := strings.ToLower(strings.Trim(fields[0], `"'`))
	return (head == "bash" || head == "zsh") &&
		shellScriptValidationFailureSignalRe.MatchString(stripped)
}

// isTrivialModelDeclaredValidator rejects shell no-ops and observation-only
// commands even when the model labels them purpose=validation. Unknown
// project-specific checkers remain eligible so the verifier does not encode
// repository or task semantics.
func isTrivialModelDeclaredValidator(command string) bool {
	command = stripValidatorOutputMerge(strings.TrimSpace(command))
	if command == "" {
		return true
	}
	segments := shellSequenceSeparator.Split(command, -1)
	seen := false
	for _, segment := range segments {
		fields := strings.Fields(strings.TrimSpace(segment))
		if len(fields) == 0 {
			continue
		}
		seen = true
		index := 0
		if fields[index] == "env" {
			index++
			for index < len(fields) && strings.Contains(fields[index], "=") {
				index++
			}
			if index >= len(fields) {
				continue
			}
		}
		head := strings.ToLower(strings.Trim(fields[index], `"'`))
		switch head {
		case "cd", "true", "false", ":", "pwd", "ls", "cat", "head", "tail",
			"wc", "stat", "find", "echo", "printf":
			continue
		default:
			return false
		}
	}
	return seen
}

// isTrivialModelDeclaredValidation additionally rejects inline interpreter
// programs that only read, parse, or print data. Such commands can return zero
// for every artifact and therefore cannot support a completion claim. Unknown
// project-specific checker files remain eligible; only source embedded in the
// tool call must expose a failure signal such as assert, raise, or non-zero
// exit.
func isTrivialModelDeclaredValidation(args map[string]any) bool {
	command, _ := args["command"].(string)
	if isTrivialModelDeclaredValidator(command) {
		return true
	}
	commandBody := validationCommandBody(command)
	if observationOnlyValidationCommandRe.MatchString(commandBody) {
		return true
	}
	script, _ := args["script"].(string)
	if strings.TrimSpace(script) != "" {
		return !scriptHasValidationFailureSignal(commandBody, script)
	}
	if inlineInterpreterValidatorRe.MatchString(commandBody) {
		source := inlineInterpreterValidatorRe.ReplaceAllString(commandBody, "")
		source = strings.TrimSpace(source)
		if len(source) >= 2 &&
			((source[0] == '\'' && source[len(source)-1] == '\'') ||
				(source[0] == '"' && source[len(source)-1] == '"')) {
			source = source[1 : len(source)-1]
		}
		return !scriptHasValidationFailureSignal(commandBody, source)
	}
	return false
}

var temporaryScriptValidationInterpreterRe = regexp.MustCompile(
	`(?i)^(?:python(?:3(?:\.\d+)?)?|node|bash|zsh)$`,
)
var versionedPythonInterpreterRe = regexp.MustCompile(`^python3\.\d+$`)
var directCheckerValidationCommandRe = regexp.MustCompile(
	`(?i)^\s*(?:\.{0,2}/|/)[^\s]*(?:test|check|verify|validate|audit)[^\s]*(?:\s|$)`,
)
var shellPredicateValidationCommandRe = regexp.MustCompile(
	`(?i)(?:^|(?:&&|\|\||;))\s*(?:` +
		`test\s+|\[\[?\s+|diff\s+|cmp\s+|grep\s+|` +
		`(?:sha256sum|sha1sum|md5sum|shasum)\s+(?:[^;&|]*\s)?(?:-c|--check)\b|` +
		`jq\s+(?:[^;&|]*\s)?(?:-e|--exit-status)\b|` +
		`curl\s+(?:[^;&|]*\s)?(?:-f|--fail(?:-with-body)?)\b|` +
		`git\s+diff\s+(?:[^;&|]*\s)?--exit-code\b` +
		`)`,
)

var zeroExitShellPredicateValidationCommandRe = regexp.MustCompile(
	`(?i)(?:^|(?:&&|\|\||;))\s*(?:` +
		`test\s+|\[\[?\s+|diff\s+|cmp\s+|grep\s+|` +
		`(?:sha256sum|sha1sum|md5sum|shasum)\s+(?:[^;&|]*\s)?(?:-c|--check)\b|` +
		`jq\s+(?:[^;&|]*\s)?(?:-e|--exit-status)\b|` +
		`git\s+diff\s+(?:[^;&|]*\s)?--exit-code\b` +
		`)`,
)

// hasExecutableValidationFailureSignal recognizes an interpreter program that
// can fail on a violated assertion even when the model omitted
// purpose=validation. It does not inspect expected answers or infer task
// semantics; it repairs only an unambiguous tool-protocol omission.
func hasExecutableValidationFailureSignal(args map[string]any) bool {
	command, _ := args["command"].(string)
	commandBody := validationCommandBody(command)
	script, _ := args["script"].(string)
	if strings.TrimSpace(script) != "" &&
		temporaryScriptValidationInterpreterRe.MatchString(strings.TrimSpace(commandBody)) {
		return scriptHasValidationFailureSignal(commandBody, script)
	}
	if inlineInterpreterValidatorRe.MatchString(commandBody) {
		source := strings.TrimSpace(inlineInterpreterValidatorRe.ReplaceAllString(commandBody, ""))
		if len(source) >= 2 &&
			((source[0] == '\'' && source[len(source)-1] == '\'') ||
				(source[0] == '"' && source[len(source)-1] == '"')) {
			source = source[1 : len(source)-1]
		}
		return scriptHasValidationFailureSignal(commandBody, source)
	}
	return false
}

// hasIndependentValidationFailureSemantics is intentionally about executable
// behavior, not task meaning. It accepts validators with an observable failure
// path and rejects source-only inspection commands such as sha256sum, ls, cat,
// or echo. External checker files remain model-authored and model-selected.
func hasIndependentValidationFailureSemantics(args map[string]any) bool {
	command, _ := args["command"].(string)
	commandBody := validationCommandBody(command)
	if validationCommandMasksFailure(commandBody) {
		return false
	}
	if looksLikeValidationCommand(commandBody) || hasExecutableValidationFailureSignal(args) {
		return true
	}
	if isExternalCheckerInvocation(commandBody) ||
		directCheckerValidationCommandRe.MatchString(commandBody) ||
		shellPredicateValidationCommandRe.MatchString(commandBody) {
		return true
	}
	return false
}

func validationCommandMasksFailure(command string) bool {
	command = strings.TrimSpace(command)
	return validationFailureMaskingTailRe.MatchString(command) ||
		validationFailureMaskingPipeRe.MatchString(command)
}

func isExternalCheckerInvocation(command string) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) < 2 {
		return false
	}
	head := strings.ToLower(strings.Trim(fields[0], `"'`))
	switch head {
	case "python", "python3", "node", "bash", "zsh", "ruby", "php":
	default:
		if !versionedPythonInterpreterRe.MatchString(head) {
			return false
		}
	}
	for _, field := range fields[1:] {
		field = strings.Trim(field, `"'`)
		if strings.HasPrefix(field, "-") {
			// Inline programs and module execution must expose their own known
			// validation semantics; they are not external checker files.
			if field == "-c" || field == "-e" || field == "-m" {
				return false
			}
			continue
		}
		name := strings.ToLower(filepath.Base(field))
		ext := strings.ToLower(filepath.Ext(name))
		if ext == ".py" || ext == ".js" || ext == ".mjs" || ext == ".cjs" ||
			ext == ".sh" || ext == ".rb" || ext == ".php" {
			return true
		}
		return strings.Contains(name, "test") || strings.Contains(name, "check") ||
			strings.Contains(name, "verify") || strings.Contains(name, "validate") ||
			strings.Contains(name, "audit")
	}
	return false
}

func stripNonCodeText(source string) string {
	var result strings.Builder
	for index := 0; index < len(source); {
		if source[index] == '#' {
			for index < len(source) && source[index] != '\n' {
				index++
			}
			result.WriteByte('\n')
			continue
		}
		if index+1 < len(source) && source[index] == '/' && source[index+1] == '/' {
			index += 2
			for index < len(source) && source[index] != '\n' {
				index++
			}
			result.WriteByte('\n')
			continue
		}
		if index+1 < len(source) && source[index] == '/' && source[index+1] == '*' {
			index += 2
			for index+1 < len(source) && !(source[index] == '*' && source[index+1] == '/') {
				index++
			}
			if index+1 < len(source) {
				index += 2
			}
			result.WriteByte(' ')
			continue
		}
		quote := source[index]
		if quote != '\'' && quote != '"' && quote != '`' {
			result.WriteByte(source[index])
			index++
			continue
		}
		triple := index+2 < len(source) && source[index+1] == quote && source[index+2] == quote
		if triple {
			index += 3
		} else {
			index++
		}
		for index < len(source) {
			if source[index] == '\\' {
				index += 2
				continue
			}
			if triple {
				if index+2 < len(source) && source[index] == quote &&
					source[index+1] == quote && source[index+2] == quote {
					index += 3
					break
				}
			} else if source[index] == quote {
				index++
				break
			}
			index++
		}
		result.WriteByte(' ')
	}
	return result.String()
}

func expectedValidationExitCode(args map[string]any, exitCode int, hasExitCode bool) bool {
	if !hasExitCode {
		return true
	}
	raw, declared := args["expected_exit_codes"]
	if !declared {
		return exitCode == 0
	}
	values, ok := raw.([]any)
	if !ok {
		if typed, typedOK := raw.([]int); typedOK {
			for _, expected := range typed {
				if exitCode == expected {
					return true
				}
			}
		}
		return false
	}
	for _, rawValue := range values {
		switch value := rawValue.(type) {
		case float64:
			if exitCode == int(value) {
				return true
			}
		case int:
			if exitCode == value {
				return true
			}
		}
	}
	return false
}

func CommandMatches(got, want string) bool {
	got = stripValidatorOutputMerge(strings.TrimSpace(got))
	want = stripValidatorOutputMerge(strings.TrimSpace(want))
	if got == want {
		return true
	}
	// `go test ./... -v` and `go test ./... -count=1` execute the same
	// repository-selected package set as `go test ./...`; the extra flags only
	// expose progress or disable Go's test cache. Native coding models commonly
	// add them while debugging. Treat those narrow variants as the selected
	// validator so successful real tests are not discarded as unverified. Never
	// accept narrowing flags such as -run, -short, or a different package list.
	if goValidationPresentationVariant(got, want) {
		return true
	}
	// A model may include a working-directory hop even though the tool also has
	// a directory argument. Only the final command segment may vary this way.
	for _, separator := range []string{"&&", ";"} {
		if index := strings.LastIndex(got, separator); index >= 0 &&
			strings.TrimSpace(got[index+len(separator):]) == want &&
			workingDirectoryHopRe.MatchString(strings.TrimSpace(got[:index])) {
			return true
		}
	}
	return false
}

// commandMatches additionally recognizes the exact command body behind an
// explicitly selected package.json script. Coding models often inspect the
// manifest and invoke its expanded command directly (for example, a bundled
// TypeScript compiler followed by a Node assertion script) instead of spelling
// `node --run test` or `npm test`. The expansion is accepted only when it is an
// exact byte-for-byte match for the named local script; arbitrary commands and
// suffixes remain ordinary diagnostics.
func (v *Verifier) commandMatches(got string) bool {
	if v == nil {
		return false
	}
	return CommandMatches(got, v.command) ||
		packageScriptExpansionMatches(v.cwd, got, v.command)
}

func packageScriptExpansionMatches(cwd, got, selected string) bool {
	scriptName, ok := selectedPackageScript(selected)
	if !ok || strings.TrimSpace(cwd) == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(cwd, "package.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(raw, &manifest) != nil {
		return false
	}
	want := strings.TrimSpace(manifest.Scripts[scriptName])
	if want == "" {
		return false
	}
	got = stripLeadingWorkingDirectoryHop(got)
	return got == want || shellValidationTokenSequenceMatches(got, want)
}

// shellValidationTokenSequenceMatches permits only presentation-equivalent
// quoting of the exact package script: `"path with spaces"` and
// `path\ with\ spaces` become the same argv token. It is intentionally not a
// general shell evaluator. Substitution, expansion, pipes, redirects, OR, and
// unmatched quotes reject the comparison; command and && token counts must be
// identical.
func shellValidationTokenSequenceMatches(got, want string) bool {
	gotTokens, gotOK := conservativeShellValidationTokens(got)
	wantTokens, wantOK := conservativeShellValidationTokens(want)
	if !gotOK || !wantOK || len(gotTokens) != len(wantTokens) {
		return false
	}
	for index := range gotTokens {
		if gotTokens[index] != wantTokens[index] {
			return false
		}
	}
	return true
}

func conservativeShellValidationTokens(command string) ([]string, bool) {
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}
	for index := 0; index < len(command); {
		switch command[index] {
		case ' ', '\t', '\r', '\n':
			flush()
			index++
		case '&':
			if index+1 >= len(command) || command[index+1] != '&' {
				return nil, false
			}
			flush()
			tokens = append(tokens, "&&")
			index += 2
		case '|', ';', '<', '>', '`', '$':
			return nil, false
		case '\\':
			if index+1 >= len(command) {
				return nil, false
			}
			current.WriteByte(command[index+1])
			index += 2
		case '\'', '"':
			quote := command[index]
			index++
			closed := false
			for index < len(command) {
				if command[index] == quote {
					closed = true
					index++
					break
				}
				if quote == '"' && (command[index] == '$' || command[index] == '`') {
					return nil, false
				}
				if command[index] == '\\' && quote == '"' {
					if index+1 >= len(command) {
						return nil, false
					}
					current.WriteByte(command[index+1])
					index += 2
					continue
				}
				current.WriteByte(command[index])
				index++
			}
			if !closed {
				return nil, false
			}
		default:
			current.WriteByte(command[index])
			index++
		}
	}
	flush()
	return tokens, len(tokens) > 0
}

func selectedPackageScript(command string) (string, bool) {
	fields := strings.Fields(stripValidatorOutputMerge(strings.TrimSpace(command)))
	if len(fields) == 3 && fields[0] == "node" && fields[1] == "--run" {
		return fields[2], fields[2] != ""
	}
	if len(fields) == 2 && fields[0] == "node" && strings.HasPrefix(fields[1], "--run=") {
		name := strings.TrimPrefix(fields[1], "--run=")
		return name, name != ""
	}
	if len(fields) < 2 {
		return "", false
	}
	switch fields[0] {
	case "npm", "pnpm", "yarn", "bun":
	default:
		return "", false
	}
	if len(fields) == 2 && fields[1] == "test" {
		return "test", true
	}
	if len(fields) == 3 && fields[1] == "run" && fields[2] != "" {
		return fields[2], true
	}
	return "", false
}

func stripLeadingWorkingDirectoryHop(command string) string {
	command = stripValidatorOutputMerge(strings.TrimSpace(command))
	for _, separator := range []string{"&&", ";"} {
		index := strings.Index(command, separator)
		if index < 0 || !workingDirectoryHopRe.MatchString(strings.TrimSpace(command[:index])) {
			continue
		}
		return strings.TrimSpace(command[index+len(separator):])
	}
	return command
}

func goValidationPresentationVariant(got, want string) bool {
	wantFields := strings.Fields(want)
	gotFields := strings.Fields(got)
	if len(wantFields) < 3 || wantFields[0] != "go" || wantFields[1] != "test" ||
		len(gotFields) < len(wantFields) || gotFields[0] != "go" || gotFields[1] != "test" {
		return false
	}
	filtered := make([]string, 0, len(gotFields))
	for _, field := range gotFields {
		switch field {
		case "-v", "-json", "-count=1":
			continue
		default:
			filtered = append(filtered, field)
		}
	}
	return strings.Join(filtered, " ") == strings.Join(wantFields, " ")
}

// stripValidatorOutputMerge treats a trailing stderr-to-stdout merge as the
// same validator. It changes only presentation, not the command's exit status
// or side effects. CheckToolCall still replaces the model draft with the exact
// selected validator before execution, so no arbitrary suffix is retained.
func stripValidatorOutputMerge(command string) string {
	for strings.HasSuffix(command, " 2>&1") {
		command = strings.TrimSpace(strings.TrimSuffix(command, " 2>&1"))
	}
	return command
}

var workingDirectoryHopRe = regexp.MustCompile(`^cd\s+(?:'[^']+'|"[^"]+"|[^\s;&|]+)$`)

// DiscoverRepositoryValidationCommand chooses a conservative native validator
// from repository structure. It does not inspect task names or generate code.
func DiscoverRepositoryValidationCommand(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	exists := func(name string) bool {
		info, err := os.Stat(filepath.Join(cwd, name))
		return err == nil && !info.IsDir()
	}
	dirExists := func(name string) bool {
		info, err := os.Stat(filepath.Join(cwd, name))
		return err == nil && info.IsDir()
	}
	switch {
	case exists("go.mod"):
		return "go test ./..."
	case exists("Cargo.toml"):
		return "cargo test"
	case exists("package.json"):
		var manifest struct {
			Scripts map[string]string `json:"scripts"`
		}
		raw, err := os.ReadFile(filepath.Join(cwd, "package.json"))
		if err == nil && json.Unmarshal(raw, &manifest) == nil && strings.TrimSpace(manifest.Scripts["test"]) != "" {
			switch {
			case exists("pnpm-lock.yaml"):
				return "pnpm test"
			case exists("yarn.lock"):
				return "yarn test"
			default:
				return "npm test"
			}
		}
	case dirExists("tests"):
		return "python3 -m unittest discover -s tests -v"
	case dirExists("test") && (exists("pyproject.toml") || exists("setup.py") || exists("requirements.txt")):
		return "python3 -m unittest discover -s test -v"
	}
	return ""
}

func executionExitCode(display string) (int, bool) {
	var code int
	if _, err := fmt.Sscanf(strings.TrimSpace(display), "exit %d", &code); err == nil {
		return code, true
	}
	return 0, false
}
