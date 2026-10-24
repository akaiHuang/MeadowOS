package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// RequiredEvidenceBinding binds a successful validator to artifacts mutated
// during this task. MutationPathArgs must all name paths written by the run.
// MatchPrior* additionally ties a later evidence stage to a path argument from
// an earlier required stage (for example visual.file_path == build.html_path).
type RequiredEvidenceBinding struct {
	MutationPathArgs []string
	// AllowPreexistingPaths is reserved for explicit validation-only tasks where
	// the user asked the agent not to rewrite an existing artifact. Successful
	// validators and MatchPrior* still bind every stage to the same artifact.
	AllowPreexistingPaths bool
	// RequireAnyMutationPath is used by follow-up repair tasks: sibling source
	// files may predate the current run, but at least one artifact in the bound
	// validator pair must have been genuinely mutated now.
	RequireAnyMutationPath bool
	MatchArg               string
	MatchPriorTool         string
	MatchPriorArg          string
}

// TaskProfile describes product behavior, not a solution to a particular
// prompt. The router may select a profile, while TaskController owns the
// model-agnostic execution state that persists across turns.
type TaskProfile struct {
	Enabled                 bool
	Intent                  string
	RequiresToolAction      bool
	RequiresInspection      bool
	RequiresMutation        bool
	RequiresNetworkEvidence bool
	// AuthoritativeTask is the immutable user task used only to ground
	// model-authored acceptance coverage quotes. The controller never interprets
	// its business meaning or derives a solution from it.
	AuthoritativeTask     string
	EvidenceTools         []string
	RequiredEvidenceTools []string
	// RequiredEvidenceByMutationExtension activates evidence stages only after
	// the run actually mutates a matching artifact type. This keeps open-ended
	// semantic routing model-led for chat and planning, while making a concrete
	// saved artifact acquire the verifier appropriate to its observed type.
	// Keys are lowercase extensions including the leading dot.
	RequiredEvidenceByMutationExtension map[string][]string
	MaxConsecutiveFailures              int
	TaskStartPrompt                     string
	MutationRequiredPrompt              string
	MutationRecoveryPrompt              string
	// InitialTool narrows the first action turn to one capability for profiles
	// that can safely begin a new artifact without inspecting existing state.
	// It does not force transport grammar: model-native auto tool calling still
	// decides whether and how to call the sole exposed tool.
	InitialTool string
	// InitialToolUntilMutations keeps that narrow capability surface until the
	// requested number of successful mutations exists. It is opt-in for model
	// families that can batch sibling artifact writes in one response; a model
	// that emits only one call still receives the same single tool on its next
	// turn instead of the whole registry.
	InitialToolUntilMutations int
	// MaxRequiredEvidenceFailures bounds failures of a mandatory evidence
	// provider independently of unrelated successful reads. This prevents a
	// dead browser/research service from being hidden by read -> retry loops.
	MaxRequiredEvidenceFailures int
	// RequiredEvidenceFailureLimits overrides the shared bound for one tool.
	// A local compiler may need several repair rounds, while an unavailable
	// external visual service should stop after a much smaller retry budget.
	RequiredEvidenceFailureLimits map[string]int
	// RequiredEvidenceBindings prevents evidence from an unrelated pre-existing
	// artifact or URL from satisfying this task's completion contract.
	RequiredEvidenceBindings map[string]RequiredEvidenceBinding
	// RequiredEvidenceRepairTools maps a failed required validator to the one
	// mutation tool that is allowed to repair its saved inputs. This keeps a
	// small model on the compiler diagnostic instead of falling back to an
	// unbounded read/list loop. The validator is forced again after a successful
	// repair mutation.
	RequiredEvidenceRepairTools map[string]string
	// RequiredArtifactRoot and RequiredArtifactPathArgs define a host-enforced
	// destination for standalone outputs. The model still chooses filenames and
	// contents, but cannot redirect a session artifact into the selected project.
	RequiredArtifactRoot     string
	RequiredArtifactPathArgs map[string][]string
	// ForceRequiredEvidenceAfterMutations turns an otherwise prompt-only
	// artifact workflow into a bounded stage machine. Once this many successful
	// mutations have happened, the next missing required evidence tool is chosen
	// explicitly. A failed evidence tool releases the lock until the model makes
	// a repair mutation, after which the same evidence stage is forced again.
	ForceRequiredEvidenceAfterMutations int
	// ForceRequiredEvidenceImmediately starts an explicit validation-only task
	// at its first validator without requiring a synthetic mutation first.
	ForceRequiredEvidenceImmediately bool
	// ForceRequiredEvidenceAfterInspections allows validation-first work on an
	// existing artifact to ground itself with a bounded number of successful
	// reads before the controller locks onto the first required validator. This
	// is distinct from a repair workflow: no mutation is required unless the
	// validator reports a concrete defect.
	ForceRequiredEvidenceAfterInspections int
	// AutoExecuteDeterministicEvidence lets the Kernel run a required evidence
	// call without another model round only when every argument is mechanically
	// derivable from this run's own mutated artifacts. It never applies to a
	// semantic validator whose assertions still have to be authored by the model.
	AutoExecuteDeterministicEvidence bool
	// DeterministicRequiredEvidenceArgs lets a product profile provide a complete,
	// task-grounded acceptance contract for a required evidence tool. The Kernel
	// never invents these arguments; it only binds file_path to the single HTML
	// artifact mutated in this run. This is useful for small models that can repair
	// a concrete validator failure but are unreliable at serializing a large
	// nested interaction schema.
	DeterministicRequiredEvidenceArgs map[string]map[string]any
	// RepairPostActionInteractionImmediately skips the validator-call retry for
	// a rendered-DOM assertion that failed only after one or more interactions.
	// It is intended for model families whose long validator rewrite turn is
	// more expensive than grounding a repair on the already-built source. Call
	// errors, missing selectors, and initial-state assertions still retry the
	// validator because they do not prove an artifact defect.
	RepairPostActionInteractionImmediately bool
	// ReplayPostActionInteractionAfterRepair preserves a model-authored,
	// actually-executed interaction acceptance call when a post-action assertion
	// exposes an artifact defect. After a real source mutation and the earlier
	// build/visual stages pass again, the Kernel may execute those exact same
	// steps without asking the model to regenerate (or weaken) them. Invalid
	// calls and initial-state assertion failures are never replay candidates.
	ReplayPostActionInteractionAfterRepair bool
	// ModelDirectedEvidenceRecovery keeps the safe task capability set open
	// after a validator returns semantic evidence about the artifact.  The
	// controller retains the diagnostic and still blocks false completion, but
	// the model decides whether the next useful action is read, edit, rebuild,
	// or a corrected validator call.  Native schema/argument errors remain
	// narrowly retried because they contain no evidence of an artifact defect.
	ModelDirectedEvidenceRecovery bool
	// RequireInteractionCoverageQuotes rejects a successful browser interaction
	// call when its coverage rows are not exact substrings of AuthoritativeTask.
	// The model still chooses the scenarios and assertions; the harness only
	// prevents invented acceptance claims from satisfying completion.
	RequireInteractionCoverageQuotes bool
	// RequiredInteractionCoverageQuotes is an optional, mechanically extracted
	// list of explicit user quotes that every successful interaction plan must
	// cover. It is intentionally not a semantic oracle: profiles may populate it
	// only from literal text in the authoritative request.
	RequiredInteractionCoverageQuotes []string
	// Existing-artifact repairs should not reread the same complete sources
	// forever. After the bounded inspection floor, expose exactly one mutation
	// tool until a real change occurs; the model still authors the edit content.
	ForceMutationToolAfterInspections  string
	MinInspectionsBeforeForcedMutation int
}

// TaskProfileResolver translates a route into generic success requirements.
// Keeping it outside TaskController prevents the kernel from importing a
// product-specific intent taxonomy.
type TaskProfileResolver func(task string, route TurnRoute) TaskProfile

type TaskPhase string

const (
	TaskPhaseReady    TaskPhase = "ready"
	TaskPhaseInspect  TaskPhase = "inspect"
	TaskPhaseAct      TaskPhase = "act"
	TaskPhaseRecover  TaskPhase = "recover"
	TaskPhaseVerify   TaskPhase = "verify"
	TaskPhaseAudit    TaskPhase = "audit"
	TaskPhaseDeliver  TaskPhase = "deliver"
	TaskPhaseComplete TaskPhase = "complete"
	TaskPhaseStalled  TaskPhase = "stalled"
)

type TaskState struct {
	Enabled                 bool      `json:"enabled"`
	Intent                  string    `json:"intent,omitempty"`
	Phase                   TaskPhase `json:"phase"`
	SuccessfulToolCalls     int       `json:"successful_tool_calls"`
	SuccessfulInspections   int       `json:"successful_inspections"`
	SuccessfulMutations     int       `json:"successful_mutations"`
	SuccessfulEvidenceCalls int       `json:"successful_evidence_calls"`
	ToolFailures            int       `json:"tool_failures"`
	ConsecutiveToolFailures int       `json:"consecutive_tool_failures"`
	RecoveryDirectives      int       `json:"recovery_directives"`
	RepairEvidenceTool      string    `json:"repair_evidence_tool,omitempty"`
	RepairTool              string    `json:"repair_tool,omitempty"`
}

type TaskDecision struct {
	Required bool
	Halt     bool
	Kind     NudgeKind
	Reason   string
	Detail   string
	Prompt   string
	Phase    TaskPhase
}

// TaskController is a small, deterministic replacement for the implicit
// state hidden inside a giant agent loop. It never examines expected answers
// or task-specific filenames; it only consumes structured tool/evidence facts.
type TaskController struct {
	profile                           TaskProfile
	evidenceTools                     map[string]bool
	requiredEvidenceTools             []string
	requiredEvidenceSet               map[string]bool
	requiredEvidenceFails             map[string]int
	requiredEvidenceFailureSignatures map[string]string
	requiredEvidenceRepair            map[string]bool
	requiredEvidenceErrors            map[string]string
	requiredEvidenceCallRetryTool     string
	requiredEvidenceCallRetryError    string
	requiredEvidenceCallRetryReadPath string
	requiredEvidenceCallRetryReadUsed bool
	requiredEvidenceAssertionRetries  map[string]int
	requiredRepairTools               map[string]string
	replayRequiredEvidenceArgs        map[string]map[string]any
	repairToolErrors                  map[string]string
	repairToolFailures                map[string]int
	repairSourceRefreshPath           string
	repairRewriteTargetPath           string
	artifactPathRepairTool            string
	artifactPathRepairErr             string
	observedEvidenceTools             map[string]bool
	observedEvidenceArgs              map[string]map[string]string
	observedInteractionCoverageQuotes map[string]bool
	mutatedPaths                      map[string]bool
	latestMutationPathByEvidenceTool  map[string]string
	deferredEvidenceRequested         bool
	state                             TaskState
}

func NewTaskController(profile TaskProfile) *TaskController {
	if profile.MaxConsecutiveFailures <= 0 {
		profile.MaxConsecutiveFailures = 5
	}
	controller := &TaskController{
		profile:                           profile,
		evidenceTools:                     make(map[string]bool),
		requiredEvidenceSet:               make(map[string]bool),
		requiredEvidenceFails:             make(map[string]int),
		requiredEvidenceFailureSignatures: make(map[string]string),
		requiredEvidenceRepair:            make(map[string]bool),
		requiredEvidenceErrors:            make(map[string]string),
		requiredEvidenceAssertionRetries:  make(map[string]int),
		requiredRepairTools:               make(map[string]string),
		replayRequiredEvidenceArgs:        make(map[string]map[string]any),
		repairToolErrors:                  make(map[string]string),
		repairToolFailures:                make(map[string]int),
		observedEvidenceTools:             make(map[string]bool),
		observedEvidenceArgs:              make(map[string]map[string]string),
		observedInteractionCoverageQuotes: make(map[string]bool),
		mutatedPaths:                      make(map[string]bool),
		latestMutationPathByEvidenceTool:  make(map[string]string),
		state: TaskState{
			Enabled: profile.Enabled,
			Intent:  strings.TrimSpace(profile.Intent),
			Phase:   TaskPhaseReady,
		},
	}
	for _, name := range profile.EvidenceTools {
		if name = strings.TrimSpace(name); name != "" {
			controller.evidenceTools[name] = true
		}
	}
	for _, name := range profile.RequiredEvidenceTools {
		if name = strings.TrimSpace(name); name != "" && !controller.evidenceTools[name] {
			controller.evidenceTools[name] = true
		}
		if name != "" && !containsString(controller.requiredEvidenceTools, name) {
			controller.requiredEvidenceTools = append(controller.requiredEvidenceTools, name)
			controller.requiredEvidenceSet[name] = true
		}
	}
	controller.refreshPhase(EvidenceSnapshot{}, nil, ValidationDecision{})
	return controller
}

func (c *TaskController) Enabled() bool {
	return c != nil && c.profile.Enabled
}

func (c *TaskController) Observe(execution ToolExecution) TaskDecision {
	if !c.Enabled() {
		return TaskDecision{}
	}
	_, activeRepairTool := c.pendingRepairStage()
	repairNoChange := execution.Status == ToolOK && execution.NoChange &&
		activeRepairTool != "" && execution.Tool == activeRepairTool
	if repairNoChange {
		// A mutation-shaped call that writes the exact bytes already on disk did
		// not repair the validator failure.  Treat it as a structured repair
		// failure even though the file tool itself executed successfully.  If it
		// were accepted as ordinary ToolOK, ForcedTaskTool would select the same
		// mutation forever while every successful no-op reset the generic failure
		// counter.
		execution.Status = ToolError
		execution.LLMContent = noChangeRepairError(execution, c.repairRewriteTargetPath)
	}
	if execution.Status != ToolOK {
		c.state.ToolFailures++
		c.state.ConsecutiveToolFailures++
		c.state.Phase = TaskPhaseRecover
		if c.isArtifactPathTool(execution.Tool) {
			c.artifactPathRepairTool = artifactPathRepairTool(execution.Tool, execution.LLMContent)
		}
		if c.artifactPathRepairTool != "" {
			c.artifactPathRepairErr = boundedRepairError(execution.LLMContent)
		}
		if c.requiredEvidenceSet[execution.Tool] {
			if c.requiredEvidenceCallRetryTool == execution.Tool {
				c.clearRequiredEvidenceCallRetry()
			}
			if unrecoverableEvidenceInfrastructureFailure(execution.Tool, execution.LLMContent) {
				c.requiredEvidenceFails[execution.Tool]++
				c.requiredEvidenceErrors[execution.Tool] = boundedRepairError(execution.LLMContent)
				c.state.Phase = TaskPhaseStalled
				return TaskDecision{
					Required: true, Halt: true, Kind: NudgeStrategyChange,
					Reason: "required_evidence_infrastructure_unavailable", Phase: TaskPhaseStalled,
					Prompt: fmt.Sprintf("Required evidence tool `%s` is unavailable because its runtime dependency is missing. Preserve the saved source and report the exact unavailable dependency. Do not edit correct application code to compensate and do not claim verification passed.", execution.Tool),
				}
			}
			callRetry := requiredEvidenceNeedsCallRetry(execution)
			if c.profile.ModelDirectedEvidenceRecovery &&
				interactionAssertionFailure(execution.LLMContent) {
				callRetry = false
			}
			if c.profile.RepairPostActionInteractionImmediately &&
				postActionInteractionAssertionFailure(execution.LLMContent) {
				callRetry = false
				if c.profile.ReplayPostActionInteractionAfterRepair && execution.Executed {
					if copied, ok := cloneToolArgs(execution.Args); ok {
						c.replayRequiredEvidenceArgs[execution.Tool] = copied
					}
				}
			}
			assertionRetryExhausted := interactionAssertionFailure(execution.LLMContent) &&
				c.requiredEvidenceAssertionRetries[execution.Tool] > 0
			if callRetry && !assertionRetryExhausted {
				// A malformed validator call or an incompatible validator option
				// says nothing about the saved artifact. Retry the same evidence
				// tool once with corrected arguments; never mutate source to
				// compensate. A second assertion failure is treated as artifact
				// evidence so this branch cannot become a validator-only loop.
				if interactionAssertionFailure(execution.LLMContent) {
					c.requiredEvidenceAssertionRetries[execution.Tool]++
				}
				c.requiredEvidenceCallRetryTool = execution.Tool
				c.requiredEvidenceCallRetryError = boundedRepairError(execution.LLMContent)
				c.requiredEvidenceCallRetryReadPath = ""
				c.requiredEvidenceCallRetryReadUsed = false
				if execution.Tool == "meadow_browser_interaction_check" && execution.Executed &&
					(strings.Contains(execution.LLMContent, "INTERACTION_CHECK_CALL_INVALID") ||
						interactionSelectorMissing(execution.LLMContent)) {
					// A missing/incorrect CSS selector is a validator-call defect, not
					// evidence that application source is wrong.  The rendered-control
					// excerpt is normally enough to correct it, but small models may need
					// one bounded read of the exact HTML under test.  Preserve that narrow
					// option without reopening arbitrary workspace or shell tools.
					c.requiredEvidenceCallRetryReadPath = normalizeEvidencePath(
						stringMapValue(execution.Args, "file_path"),
					)
				}
			} else {
				c.requiredEvidenceRepair[execution.Tool] = true
				c.requiredEvidenceErrors[execution.Tool] = boundedRepairError(execution.LLMContent)
				if repairTool := c.repairToolForRequiredEvidence(execution); repairTool != "" {
					c.requiredRepairTools[execution.Tool] = repairTool
					if (repairTool == "edit" || repairTool == "write_file") && c.repairRewriteTargetPath == "" {
						c.repairRewriteTargetPath = c.requiredEvidenceRepairTarget(execution)
					}
					if repairTool == "edit" && c.repairRewriteTargetPath != "" {
						// Mutation arguments may already be content-addressed out of the
						// model-facing history. Refresh exactly the validator-named source
						// before asking for an old_string replacement; do not make the
						// model rediscover it through list/read wandering.
						c.repairSourceRefreshPath = c.repairRewriteTargetPath
					}
				}
			}
		} else if evidenceTool, repairTool := c.pendingRepairStage(); repairTool != "" && execution.Tool == repairTool {
			// A schema-invalid or otherwise failed repair attempt must not release
			// the stage. Preserve its structured error so the next compacted turn
			// still knows the required arguments.
			c.repairToolErrors[repairTool] = boundedRepairError(execution.LLMContent)
			c.repairToolFailures[repairTool]++
			if repairTool == "edit" && editFailureNeedsSourceRefresh(execution.LLMContent) {
				c.repairSourceRefreshPath = typedToolFailurePath(execution.LLMContent)
				c.repairRewriteTargetPath = normalizeEvidencePath(c.repairSourceRefreshPath)
				if c.repairToolFailures[repairTool] >= 2 {
					// Exact replacement is the safest first repair, but small models can
					// repeatedly fail to produce a distinct or matching replacement even
					// after reading the current source. Escalate to one complete rewrite
					// of the same session-owned source instead of looping forever.
					c.requiredRepairTools[evidenceTool] = "write_file"
					c.repairToolErrors["write_file"] = boundedRepairError(execution.LLMContent)
				}
			} else if repairTool == "write_file" && repairNoChange {
				// A full-file fallback that produced identical bytes has exhausted
				// that strategy.  Re-ground once on the exact source and return to a
				// minimal edit.  A second no-op rewrite is a deterministic loop and
				// must halt instead of alternating edit/read/write forever.
				if c.repairToolFailures[repairTool] >= 2 {
					c.state.Phase = TaskPhaseStalled
					return TaskDecision{
						Required: true, Halt: true, Kind: NudgeStrategyChange,
						Reason: "repair_no_change_exhausted", Phase: TaskPhaseStalled,
						Prompt: "The validator repair produced identical saved bytes twice. Preserve the artifact and report that the current model could not derive a materially different fix from the available diagnostic. Do not keep rewriting the same file or claim verification passed.",
					}
				}
				c.requiredRepairTools[evidenceTool] = "edit"
				c.repairSourceRefreshPath = c.repairRewriteTargetPath
				if c.repairSourceRefreshPath == "" {
					c.repairSourceRefreshPath = normalizeEvidencePath(stringMapValue(execution.Args, "file_path"))
					c.repairRewriteTargetPath = c.repairSourceRefreshPath
				}
			}
		}
		if limit := c.requiredEvidenceFailureLimit(execution.Tool); limit > 0 {
			// A repairable validator can fail several times while the model is
			// genuinely moving through different compiler/assertion diagnostics.
			// Bound repeated attempts at the *same* failure, not the total number
			// of distinct defects discovered in one artifact. MaxTurns remains the
			// outer bound for a long sequence of distinct repairs.
			c.resetRequiredEvidenceFailureBudgetOnProgress(execution)
			c.requiredEvidenceFails[execution.Tool]++
			if c.requiredEvidenceFails[execution.Tool] >= limit {
				c.state.Phase = TaskPhaseStalled
				return TaskDecision{
					Required: true, Halt: true, Kind: NudgeStrategyChange,
					Reason: "required_evidence_failures_exhausted", Phase: TaskPhaseStalled,
					Prompt: fmt.Sprintf("Required evidence tool `%s` failed %d times. Preserve completed work and report that this external verification is unavailable; do not substitute an unrelated validator or keep retrying.", execution.Tool, c.requiredEvidenceFails[execution.Tool]),
				}
			}
		}
		if c.state.ConsecutiveToolFailures >= c.profile.MaxConsecutiveFailures {
			c.state.Phase = TaskPhaseStalled
			return TaskDecision{
				Required: true, Halt: true, Kind: NudgeStrategyChange,
				Reason: "consecutive_tool_failures_exhausted", Phase: TaskPhaseStalled,
			}
		}
		// Two failures are enough to make the strategy change explicit. The
		// NudgePolicy still owns the bounded retry budget.
		if c.state.ConsecutiveToolFailures >= 2 {
			c.state.RecoveryDirectives++
			prompt := ComposeNudge(NudgeInput{
				Kind: NudgeStrategyChange, ToolName: execution.Tool,
				Failure: boundedFailure(execution),
			})
			if typedPrompt := structuredToolRecoveryPrompt(execution); typedPrompt != "" {
				prompt = typedPrompt
			}
			return TaskDecision{
				Required: true, Kind: NudgeStrategyChange,
				Reason: "repeated_tool_failure", Phase: TaskPhaseRecover,
				Prompt: prompt,
			}
		}
		return TaskDecision{}
	}

	if execution.Executed {
		c.state.SuccessfulToolCalls++
	}
	if c.requiredEvidenceCallRetryReadPath != "" && !c.requiredEvidenceCallRetryReadUsed &&
		execution.Tool == "read_file" && execution.Executed {
		path := normalizeEvidencePath(stringMapValue(execution.Args, "file_path"))
		if path == c.requiredEvidenceCallRetryReadPath {
			c.requiredEvidenceCallRetryReadUsed = true
		}
	}
	if execution.Tool == c.artifactPathRepairTool {
		c.artifactPathRepairTool = ""
		c.artifactPathRepairErr = ""
	}
	if c.repairSourceRefreshPath != "" && execution.Tool == "read_file" && execution.Executed {
		if path, _ := execution.Args["file_path"].(string); normalizeEvidencePath(path) == normalizeEvidencePath(c.repairSourceRefreshPath) {
			c.repairSourceRefreshPath = ""
		}
	}
	evidenceToolExecuted := c.evidenceTools[execution.Tool] && execution.Executed
	if execution.Tool == "run_shell_command" && c.requiredEvidenceSet[execution.Tool] {
		// A successful diagnostic command is not acceptance evidence. Required
		// shell evidence must have been classified by Verifier as a falsifiable
		// validation and must carry a real zero exit contract.
		evidenceToolExecuted = evidenceToolExecuted && execution.Validation &&
			execution.ValidationHasExitCode && execution.ValidationExitCode == 0 &&
			!execution.ValidationFailureOutput
	}
	requiredEvidencePosition := c.requiredEvidencePosition(execution.Tool)
	if execution.Executed && (execution.Effect == EffectWrite || len(execution.ChangedPaths) > 0) {
		c.activateRequiredEvidenceForMutation(execution)
		// Any source mutation invalidates build/visual evidence. A required
		// evidence tool that itself emits an artifact (the TS build emits JS)
		// keeps its own and earlier stages but invalidates every later stage.
		start := 0
		if requiredEvidencePosition >= 0 {
			start = requiredEvidencePosition + 1
		}
		for index := start; index < len(c.requiredEvidenceTools); index++ {
			name := c.requiredEvidenceTools[index]
			delete(c.observedEvidenceTools, name)
			delete(c.observedEvidenceArgs, name)
			if name == "meadow_browser_interaction_check" {
				clear(c.observedInteractionCoverageQuotes)
			}
			delete(c.requiredEvidenceAssertionRetries, name)
			if c.requiredEvidenceCallRetryTool == name {
				c.clearRequiredEvidenceCallRetry()
			}
		}
		// A real source mutation is the only action that releases a failed
		// evidence stage for a deterministic retry. Successful reads must not.
		for name := range c.requiredEvidenceRepair {
			delete(c.requiredEvidenceRepair, name)
			delete(c.requiredEvidenceErrors, name)
			delete(c.requiredRepairTools, name)
		}
		for name := range c.repairToolErrors {
			delete(c.repairToolErrors, name)
		}
		for name := range c.repairToolFailures {
			delete(c.repairToolFailures, name)
		}
		c.repairSourceRefreshPath = ""
		c.repairRewriteTargetPath = ""
	}
	if execution.Effect == EffectWrite && execution.Executed {
		for _, changedPath := range execution.ChangedPaths {
			if normalized := normalizeEvidencePath(changedPath); normalized != "" {
				c.mutatedPaths[normalized] = true
			}
		}
		// Native file tools intentionally report their direct target through
		// arguments rather than Result.ChangedPaths (the latter is reserved for
		// indirect writes such as shell/compiler output). Record the real tool
		// contract here so artifact-bound validators behave identically in tests
		// and in the packaged CLI/Desktop runtime.
		for _, argName := range []string{"file_path", "path", "filename"} {
			if raw, ok := execution.Args[argName].(string); ok {
				if normalized := normalizeEvidencePath(raw); normalized != "" {
					c.mutatedPaths[normalized] = true
				}
			}
		}
	}
	if requiredEvidencePosition > 0 {
		for index := 0; index < requiredEvidencePosition; index++ {
			if !c.observedEvidenceTools[c.requiredEvidenceTools[index]] {
				evidenceToolExecuted = false
				break
			}
		}
	}
	if evidenceToolExecuted {
		delete(c.requiredEvidenceAssertionRetries, execution.Tool)
		if c.profile.RequireInteractionCoverageQuotes && execution.Tool == "meadow_browser_interaction_check" {
			quotes, reason := interactionCoverageTaskQuotes(execution.Args, c.profile.AuthoritativeTask)
			if reason != "" {
				c.state.Phase = TaskPhaseVerify
				c.state.RecoveryDirectives++
				return TaskDecision{
					Required: true, Kind: NudgeEvidenceRequired,
					Reason: "interaction_coverage_not_grounded", Detail: reason, Phase: TaskPhaseVerify,
					Prompt: "The interaction checker ran, but its acceptance coverage was not grounded in the user's exact task: " + reason + ". Re-run `meadow_browser_interaction_check` with one coverage row for each behavior tested by this call. Copy each short requirement as an exact substring of the user request and bind it to real action step(s) plus a later assertion step; do not invent or paraphrase requirements.",
				}
			}
			for quote := range quotes {
				c.observedInteractionCoverageQuotes[quote] = true
			}
			if missing := c.missingInteractionCoverageQuotes(); len(missing) > 0 {
				c.state.Phase = TaskPhaseVerify
				c.state.RecoveryDirectives++
				return TaskDecision{
					Required: true, Kind: NudgeEvidenceRequired,
					Reason: "interaction_coverage_remaining", Detail: strings.Join(missing, ", "), Phase: TaskPhaseVerify,
					Prompt: "The last compact interaction check passed. Continue with another small `meadow_browser_interaction_check` call for only the remaining exact user requirement(s): " + quotedList(missing) + ". Keep the call compact, bind each action to a later assertion, and do not repeat already-passed coverage unless it is a necessary setup step.",
				}
			}
		}
		if reason := c.requiredEvidenceBindingFailure(execution); reason != "" {
			c.state.Phase = TaskPhaseVerify
			c.state.RecoveryDirectives++
			return TaskDecision{
				Required: true, Kind: NudgeEvidenceRequired,
				Reason: "required_evidence_artifact_mismatch", Detail: reason, Phase: TaskPhaseVerify,
				Prompt: "The validator succeeded for the wrong artifact: " + reason + ". Re-run `" + execution.Tool + "` against the files created by this task. Do not validate an unrelated pre-existing file or URL.",
			}
		}
	}
	if (execution.Effect == EffectRead && (execution.Executed || c.state.SuccessfulInspections > 0)) ||
		evidenceToolExecuted {
		c.state.SuccessfulInspections++
	}
	if execution.Effect == EffectWrite && execution.Executed {
		// A transactional batch tool can persist several independent artifacts in
		// one native call. Count its distinct observed paths so a two-file task can
		// advance to build without fabricating a second model round. Ordinary file
		// tools omit ChangedPaths and therefore retain the historical +1 behavior.
		mutationCount := 1
		if len(execution.ChangedPaths) > 1 {
			uniquePaths := make(map[string]bool, len(execution.ChangedPaths))
			for _, changedPath := range execution.ChangedPaths {
				if normalized := normalizeEvidencePath(changedPath); normalized != "" {
					uniquePaths[normalized] = true
				}
			}
			if len(uniquePaths) > mutationCount {
				mutationCount = len(uniquePaths)
			}
		}
		c.state.SuccessfulMutations += mutationCount
	}
	if evidenceToolExecuted {
		c.state.SuccessfulEvidenceCalls++
		c.observedEvidenceTools[execution.Tool] = true
		c.observedEvidenceArgs[execution.Tool] = normalizedStringArgs(execution.Args)
		if len(c.missingRequiredEvidenceTools()) == 0 {
			c.deferredEvidenceRequested = false
		}
		delete(c.requiredEvidenceFails, execution.Tool)
		delete(c.requiredEvidenceFailureSignatures, execution.Tool)
		if c.requiredEvidenceCallRetryTool == execution.Tool {
			c.clearRequiredEvidenceCallRetry()
		}
		delete(c.requiredEvidenceRepair, execution.Tool)
		delete(c.requiredEvidenceErrors, execution.Tool)
		delete(c.requiredRepairTools, execution.Tool)
		delete(c.replayRequiredEvidenceArgs, execution.Tool)
	}
	c.state.ConsecutiveToolFailures = 0
	c.refreshPhase(EvidenceSnapshot{}, nil, ValidationDecision{})
	return TaskDecision{}
}

func (c *TaskController) activateRequiredEvidenceForMutation(execution ToolExecution) {
	if c == nil || len(c.profile.RequiredEvidenceByMutationExtension) == 0 {
		return
	}
	paths := append([]string(nil), execution.ChangedPaths...)
	if execution.Effect == EffectWrite {
		for _, argName := range []string{"file_path", "path", "filename"} {
			if raw, ok := execution.Args[argName].(string); ok {
				paths = append(paths, raw)
			}
		}
	}
	for _, path := range paths {
		normalizedPath := normalizeEvidencePath(path)
		ext := strings.ToLower(filepath.Ext(normalizedPath))
		if ext == "" {
			continue
		}
		for _, name := range c.profile.RequiredEvidenceByMutationExtension[ext] {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			// Keep the most recently observed matching artifact as the canonical
			// validator target. This is especially important when write_file safely
			// resolves a collision to a numbered sibling: later evidence must follow
			// the actual normalized file_path rather than an older artifact from the
			// same task.
			c.latestMutationPathByEvidenceTool[name] = normalizedPath
			if c.requiredEvidenceSet[name] {
				continue
			}
			c.evidenceTools[name] = true
			c.requiredEvidenceSet[name] = true
			c.requiredEvidenceTools = append(c.requiredEvidenceTools, name)
		}
	}
}

func interactionCoverageTaskQuotes(args map[string]any, authoritativeTask string) (map[string]bool, string) {
	task := normalizeContractText(authoritativeTask)
	if task == "" {
		return nil, "authoritative task text is unavailable"
	}
	rows, ok := args["coverage"].([]any)
	if !ok || len(rows) == 0 {
		return nil, "coverage is missing"
	}
	coveredQuotes := make(map[string]bool, len(rows))
	for index, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("coverage row %d is not an object", index+1)
		}
		quote := strings.TrimSpace(stringMapValue(row, "requirement"))
		if quote == "" {
			return nil, fmt.Sprintf("coverage row %d has no requirement quote", index+1)
		}
		normalizedQuote := normalizeContractText(quote)
		if !strings.Contains(task, normalizedQuote) {
			return nil, fmt.Sprintf("coverage row %d requirement %q is not an exact task substring", index+1, quote)
		}
		coveredQuotes[normalizedQuote] = true
	}
	return coveredQuotes, ""
}

func (c *TaskController) missingInteractionCoverageQuotes() []string {
	if c == nil {
		return nil
	}
	var missing []string
	for _, required := range c.profile.RequiredInteractionCoverageQuotes {
		required = strings.TrimSpace(required)
		if required == "" {
			continue
		}
		if !c.observedInteractionCoverageQuotes[normalizeContractText(required)] {
			missing = append(missing, required)
		}
	}
	return missing
}

func quotedList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, fmt.Sprintf("%q", item))
	}
	return strings.Join(quoted, ", ")
}

func (c *TaskController) resetRequiredEvidenceFailureBudgetOnProgress(execution ToolExecution) {
	if c == nil || strings.TrimSpace(c.profile.RequiredEvidenceRepairTools[execution.Tool]) == "" {
		return
	}
	signature := requiredEvidenceFailureProgressSignature(execution.LLMContent)
	if signature == "" {
		return
	}
	previous := c.requiredEvidenceFailureSignatures[execution.Tool]
	if previous != "" && previous != signature {
		c.requiredEvidenceFails[execution.Tool] = 0
	}
	c.requiredEvidenceFailureSignatures[execution.Tool] = signature
}

func requiredEvidenceFailureProgressSignature(content string) string {
	// Tool output can contain formatting-only whitespace changes. Collapse it
	// before comparing so only a materially different diagnostic resets the
	// repeated-failure budget.
	content = strings.Join(strings.Fields(strings.TrimSpace(content)), " ")
	const maxBytes = 6000
	if len(content) > maxBytes {
		content = content[:maxBytes]
	}
	return content
}

func unrecoverableEvidenceInfrastructureFailure(tool, content string) bool {
	content = strings.ToLower(content)
	switch strings.TrimSpace(tool) {
	case "typescript_web_build":
		return strings.Contains(content, "typescript compiler not found") ||
			strings.Contains(content, "configured typescript compiler is unavailable")
	case "meadow_browser_visual_check", "meadow_browser_interaction_check":
		return strings.Contains(content, "visual_check_unavailable") ||
			strings.Contains(content, "meadow browser is unavailable") ||
			strings.Contains(content, "meadow browser did not become ready") ||
			strings.Contains(content, "browser_diagnostics_unavailable") ||
			strings.Contains(content, "not allowed to use restricted network port") ||
			strings.Contains(content, "stale restricted-port state")
	default:
		return false
	}
}

func requiredEvidenceNeedsCallRetry(execution ToolExecution) bool {
	if typedToolFailureCode(execution.LLMContent) == "invalid_tool_arguments" {
		return true
	}
	if execution.Tool == "meadow_browser_visual_check" {
		requireCanvas, _ := execution.Args["require_canvas"].(bool)
		content := strings.ToLower(execution.LLMContent)
		return requireCanvas && (strings.Contains(content, "no visible canvas") ||
			strings.Contains(content, "did not find a visible canvas"))
	}
	if execution.Tool == "meadow_browser_interaction_check" {
		return interactionAssertionFailure(execution.LLMContent) ||
			strings.Contains(execution.LLMContent, "INTERACTION_CHECK_CALL_INVALID") ||
			strings.Contains(execution.LLMContent, "INTERACTION_COVERAGE_INVALID")
	}
	return false
}

func interactionAssertionFailure(content string) bool {
	content = strings.TrimSpace(content)
	return strings.Contains(content, "INTERACTION_CHECK_FAILED at step ") &&
		(strings.Contains(content, "(assert_dom)") || strings.Contains(content, "(assert_game_state)"))
}

func interactionGameStateAssertionFailure(content string) bool {
	content = strings.ToLower(strings.TrimSpace(content))
	return (strings.Contains(content, "interaction_check_failed") ||
		strings.Contains(content, "interaction_human_review_required")) &&
		strings.Contains(content, "assert_game_state")
}

func postActionInteractionAssertionFailure(content string) bool {
	content = strings.TrimSpace(content)
	const prefix = "INTERACTION_CHECK_FAILED at step "
	if !strings.Contains(content, prefix) ||
		(!strings.Contains(content, "(assert_dom)") && !strings.Contains(content, "(assert_game_state)")) {
		return false
	}
	// A post-action assertion that names a selector absent from the rendered DOM
	// is still ambiguous: the model may simply have invented the selector. Give
	// it one grounded validator-call retry before authorizing a source edit.
	// A repeated failure on the corrected assertion still advances to repair via
	// the normal bounded assertion retry counter.
	if interactionSelectorMissing(content) {
		return false
	}
	start := strings.Index(content, prefix) + len(prefix)
	end := start
	for end < len(content) && content[end] >= '0' && content[end] <= '9' {
		end++
	}
	if end == start {
		return false
	}
	// Step 1 is a precondition authored by the model and may itself be wrong.
	// Any later assertion follows an actual fill/click/press_key sequence and therefore
	// provides behavioral evidence about the saved artifact.
	return strings.TrimSpace(content[start:end]) != "1"
}

func interactionSelectorMissing(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	if !strings.Contains(lower, "selector") {
		return false
	}
	return strings.Contains(lower, "was not found") ||
		strings.Contains(lower, "did not match") ||
		strings.Contains(lower, "selector not found") ||
		strings.Contains(lower, "missing selector")
}

func (c *TaskController) requiredEvidenceRepairTarget(execution ToolExecution) string {
	if c == nil {
		return ""
	}
	if target := normalizeEvidencePath(typedToolFailurePath(execution.LLMContent)); target != "" {
		return target
	}
	// Browser validators operate on the built HTML, while the repairable source
	// of truth is the TypeScript input already observed by the successful build.
	// Reuse that exact controller fact instead of asking the model to rediscover
	// a sibling path or accidentally edit the generated JavaScript/HTML.
	binding, ok := c.profile.RequiredEvidenceBindings[execution.Tool]
	if !ok || strings.TrimSpace(binding.MatchPriorTool) == "" {
		return ""
	}
	priorArgs := c.observedEvidenceArgs[binding.MatchPriorTool]
	return normalizeEvidencePath(priorArgs["source_path"])
}

func (c *TaskController) requiredEvidenceBindingFailure(execution ToolExecution) string {
	if c == nil {
		return ""
	}
	binding, ok := c.profile.RequiredEvidenceBindings[execution.Tool]
	if !ok {
		return ""
	}
	args := normalizedStringArgs(execution.Args)
	mutatedPathObserved := false
	for _, argName := range binding.MutationPathArgs {
		artifactPath := args[argName]
		if artifactPath == "" {
			return fmt.Sprintf("missing required path argument %q", argName)
		}
		if c.mutatedPaths[artifactPath] {
			mutatedPathObserved = true
		}
		if !binding.AllowPreexistingPaths && !c.mutatedPaths[artifactPath] {
			return fmt.Sprintf("%s=%q was not mutated by this task", argName, artifactPath)
		}
	}
	if binding.RequireAnyMutationPath && !mutatedPathObserved {
		return fmt.Sprintf("none of %v were mutated by this repair task", binding.MutationPathArgs)
	}
	if binding.MatchPriorTool != "" {
		currentPath := args[binding.MatchArg]
		priorPath := c.observedEvidenceArgs[binding.MatchPriorTool][binding.MatchPriorArg]
		if currentPath == "" || priorPath == "" || currentPath != priorPath {
			return fmt.Sprintf(
				"%s=%q does not match %s.%s=%q",
				binding.MatchArg, currentPath, binding.MatchPriorTool, binding.MatchPriorArg, priorPath,
			)
		}
	}
	return ""
}

func normalizedStringArgs(args map[string]any) map[string]string {
	normalized := make(map[string]string, len(args))
	for name, value := range args {
		if text, ok := value.(string); ok {
			normalized[name] = normalizeEvidencePath(text)
		}
	}
	return normalized
}

func normalizeEvidencePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return filepath.Clean(value)
}

// structuredToolRecoveryPrompt keeps a machine-readable tool failure on its
// advertised recovery path. After a second failure, the generic strategy
// nudge can otherwise make a small model inspect tool internals or switch to
// edit/write even though the executor already supplied an exact native-call
// correction. This only preserves protocol metadata; it does not repair or
// invent task arguments.
func structuredToolRecoveryPrompt(execution ToolExecution) string {
	content := strings.TrimSpace(execution.LLMContent)
	if !strings.HasPrefix(content, "TOOL_ERROR ") {
		return ""
	}
	var payload struct {
		Code           string `json:"code"`
		RequiredAction string `json:"required_action"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(content, "TOOL_ERROR ")), &payload) != nil ||
		strings.TrimSpace(payload.Code) == "" || strings.TrimSpace(payload.RequiredAction) == "" {
		return ""
	}
	toolName := strings.TrimSpace(execution.Tool)
	if toolName == "" {
		return ""
	}
	return fmt.Sprintf(
		"(Structured tool recovery：最近的 `%s` 失敗已提供 code=`%s`、required_action=`%s` 與 required_args/issues。下一個回應只能送出一次修正後的原生 `%s` tool call，逐項遵守該 structured error；不要改用 shell/read 搜尋工具實作，不要改用 edit/write 繞過指定工具，也不要用文字解說。)",
		toolName, strings.TrimSpace(payload.Code), strings.TrimSpace(payload.RequiredAction), toolName,
	)
}

// EvaluateFinalAttempt decides whether model prose is allowed to terminate the
// run. Verification remains owned by Verifier; this controller covers the
// earlier product failure where the model simply stopped before acting. The
// optional verifiedExistingArtifact flag may only come from a complete
// independent audit plus a fresh passing selected validator; it prevents a
// correct checkpoint from being mutated merely to satisfy an action counter.
func (c *TaskController) EvaluateFinalAttempt(
	evidence EvidenceSnapshot,
	deliverables []Deliverable,
	validation ValidationDecision,
	verifiedExistingArtifact ...bool,
) TaskDecision {
	if !c.Enabled() {
		return TaskDecision{}
	}
	c.refreshPhase(evidence, deliverables, validation)
	preexistingArtifactVerified := len(verifiedExistingArtifact) > 0 && verifiedExistingArtifact[0]

	if c.state.ConsecutiveToolFailures > 0 {
		return c.continueDecision(
			NudgeStrategyChange,
			"unfinished_after_tool_failure",
			ComposeNudge(NudgeInput{Kind: NudgeStrategyChange}),
			TaskPhaseRecover,
		)
	}
	if c.profile.RequiresToolAction && c.state.SuccessfulToolCalls == 0 {
		prompt := ComposeNudge(NudgeInput{Kind: NudgeTaskStart})
		if strings.TrimSpace(c.profile.TaskStartPrompt) != "" {
			prompt = c.profile.TaskStartPrompt
		}
		return c.continueDecision(
			NudgeTaskStart,
			"task_not_started",
			prompt,
			c.state.Phase,
		)
	}
	if c.profile.RequiresInspection && c.state.SuccessfulInspections == 0 {
		return c.continueDecision(
			NudgeEvidenceRequired,
			"inspection_required",
			ComposeNudge(NudgeInput{Kind: NudgeEvidenceRequired}),
			TaskPhaseInspect,
		)
	}
	if c.profile.RequiresNetworkEvidence && c.state.SuccessfulEvidenceCalls == 0 {
		return c.continueDecision(
			NudgeEvidenceRequired,
			"network_evidence_required",
			ComposeNudge(NudgeInput{Kind: NudgeEvidenceRequired}),
			TaskPhaseInspect,
		)
	}
	if missing := c.missingRequiredEvidenceTools(); len(missing) > 0 {
		// Dynamic artifact evidence is deliberately deferred while the model is
		// still making a coherent batch of source changes. Once it attempts to
		// finish, lock the next turn to the exact missing evidence stage so the
		// Kernel can execute mechanically derivable calls without another semantic
		// tool-selection round.
		c.deferredEvidenceRequested = true
		quoted := make([]string, 0, len(missing))
		for _, name := range missing {
			quoted = append(quoted, "`"+name+"`")
		}
		return c.continueDecision(
			NudgeEvidenceRequired,
			"required_evidence_tools_missing",
			"Required execution evidence is incomplete. Before answering, use these exact tools in order: "+strings.Join(quoted, ", ")+". Continue from their real results; do not replace them with prose or an unrelated validator.",
			TaskPhaseInspect,
		)
	}
	if c.profile.RequiresMutation && c.state.SuccessfulMutations == 0 &&
		!deliverablesSatisfied(deliverables) && !preexistingArtifactVerified {
		if target := firstMissingDeliverable(deliverables); target != "" {
			return c.continueDecision(
				NudgeNamedDeliverable,
				"deliverable_not_created",
				ComposeNudge(NudgeInput{Kind: NudgeNamedDeliverable, Target: target}),
				TaskPhaseAct,
			)
		}
		prompt := ComposeNudge(NudgeInput{Kind: NudgeTaskContinue})
		if strings.TrimSpace(c.profile.MutationRequiredPrompt) != "" {
			prompt = c.profile.MutationRequiredPrompt
		}
		return c.continueDecision(
			NudgeTaskContinue,
			"required_mutation_missing",
			prompt,
			TaskPhaseAct,
		)
	}
	if target := firstMissingDeliverable(deliverables); target != "" {
		return c.continueDecision(
			NudgeNamedDeliverable,
			"deliverable_missing",
			ComposeNudge(NudgeInput{Kind: NudgeNamedDeliverable, Target: target}),
			TaskPhaseDeliver,
		)
	}
	return TaskDecision{Phase: c.state.Phase}
}

func (c *TaskController) requiredEvidenceFailureLimit(tool string) int {
	if c == nil || !c.requiredEvidenceSet[tool] {
		return 0
	}
	if limit := c.profile.RequiredEvidenceFailureLimits[tool]; limit > 0 {
		return limit
	}
	return c.profile.MaxRequiredEvidenceFailures
}

func (c *TaskController) requiredEvidencePosition(tool string) int {
	if c == nil {
		return -1
	}
	for index, name := range c.requiredEvidenceTools {
		if name == tool {
			return index
		}
	}
	return -1
}

// ProtocolMutationRecoveryPrompt lets a product profile make the generic
// false-save recovery concrete without teaching the kernel an intent taxonomy.
func (c *TaskController) ProtocolMutationRecoveryPrompt(fallback string) string {
	if c != nil && strings.TrimSpace(c.profile.MutationRecoveryPrompt) != "" {
		return c.profile.MutationRecoveryPrompt
	}
	return fallback
}

// ForcedRequiredEvidenceTool returns the exact next evidence stage when a
// profile has opted into deterministic stage locking. It deliberately returns
// no tool while the latest evidence failure still needs a source repair, so
// the model can edit from compiler diagnostics instead of blindly retrying.
func (c *TaskController) ForcedRequiredEvidenceTool() string {
	if c == nil || c.state.ConsecutiveToolFailures > 0 {
		return ""
	}
	if !c.profile.ForceRequiredEvidenceImmediately && !c.deferredEvidenceRequested {
		if minimum := c.profile.ForceRequiredEvidenceAfterInspections; minimum > 0 {
			if c.state.SuccessfulInspections < minimum {
				return ""
			}
		} else if c.profile.ForceRequiredEvidenceAfterMutations <= 0 ||
			c.state.SuccessfulMutations < c.profile.ForceRequiredEvidenceAfterMutations {
			return ""
		}
	}
	for _, name := range c.requiredEvidenceTools {
		if c.observedEvidenceTools[name] {
			continue
		}
		if c.requiredEvidenceRepair[name] {
			return ""
		}
		return name
	}
	return ""
}

// DeterministicTaskCall returns a call only when every argument is already an
// observed controller fact. This is the agent equivalent of caching or
// precomputing a fixed render stage: asking the model to repeat a selected
// compiler invocation or rediscover an exact failed source path is pure
// recomputation. Interaction assertions remain model-owned unless the selected
// product profile supplied the complete task-grounded contract up front.
func (c *TaskController) DeterministicTaskCall(callID string) (provider.ToolCall, bool) {
	if c == nil || !c.profile.AutoExecuteDeterministicEvidence {
		return provider.ToolCall{}, false
	}
	if c.repairSourceRefreshPath != "" {
		arguments, err := json.Marshal(map[string]string{"file_path": c.repairSourceRefreshPath})
		if err != nil {
			return provider.ToolCall{}, false
		}
		return provider.ToolCall{
			ID: callID, Type: "function",
			Function: provider.FunctionCall{Name: "read_file", Arguments: string(arguments)},
		}, true
	}
	forcedTool := c.ForcedRequiredEvidenceTool()
	if forcedTool == "read_file" {
		paths := make([]string, 0, len(c.mutatedPaths))
		for path := range c.mutatedPaths {
			if path != "" {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		if len(paths) == 1 {
			arguments, err := json.Marshal(map[string]string{"file_path": paths[0]})
			if err != nil {
				return provider.ToolCall{}, false
			}
			return provider.ToolCall{
				ID: callID, Type: "function",
				Function: provider.FunctionCall{Name: "read_file", Arguments: string(arguments)},
			}, true
		}
	}
	if forcedTool == "meadow_browser_visual_check" {
		// A successful build already bound its exact HTML input. The default
		// visual check derives canvas requirements from that saved file, so asking
		// a large model to repeat only {file_path: build.html_path} is pure
		// scheduling overhead. Keep interaction checks model-owned because their
		// ordered actions and DOM assertions carry task semantics.
		binding, ok := c.profile.RequiredEvidenceBindings[forcedTool]
		if !ok {
			return provider.ToolCall{}, false
		}
		artifactPath := ""
		if binding.MatchPriorTool != "" && binding.MatchPriorArg != "" {
			if binding.MatchArg != "file_path" || !c.observedEvidenceTools[binding.MatchPriorTool] {
				return provider.ToolCall{}, false
			}
			artifactPath = normalizeEvidencePath(
				c.observedEvidenceArgs[binding.MatchPriorTool][binding.MatchPriorArg],
			)
		} else {
			directFilePath := false
			for _, arg := range binding.MutationPathArgs {
				if arg == "file_path" {
					directFilePath = true
					break
				}
			}
			if !directFilePath {
				return provider.ToolCall{}, false
			}
			artifactPath = normalizeEvidencePath(c.latestMutationPathByEvidenceTool[forcedTool])
			if artifactPath == "" {
				paths := make([]string, 0, len(c.mutatedPaths))
				for path := range c.mutatedPaths {
					ext := strings.ToLower(filepath.Ext(path))
					if ext == ".html" || ext == ".htm" {
						paths = append(paths, path)
					}
				}
				sort.Strings(paths)
				if len(paths) != 1 {
					return provider.ToolCall{}, false
				}
				artifactPath = paths[0]
			}
		}
		if artifactPath == "" || (!binding.AllowPreexistingPaths && !c.mutatedPaths[artifactPath]) {
			return provider.ToolCall{}, false
		}
		arguments, err := json.Marshal(map[string]string{"file_path": artifactPath})
		if err != nil {
			return provider.ToolCall{}, false
		}
		return provider.ToolCall{
			ID: callID, Type: "function",
			Function: provider.FunctionCall{Name: forcedTool, Arguments: string(arguments)},
		}, true
	}
	if forcedTool == "meadow_browser_interaction_check" &&
		len(c.profile.DeterministicRequiredEvidenceArgs[forcedTool]) > 0 {
		binding, ok := c.profile.RequiredEvidenceBindings[forcedTool]
		if !ok {
			return provider.ToolCall{}, false
		}
		directFilePath := false
		for _, arg := range binding.MutationPathArgs {
			if arg == "file_path" {
				directFilePath = true
				break
			}
		}
		if !directFilePath {
			return provider.ToolCall{}, false
		}
		paths := c.mutatedPathsWithExtension(".html")
		if len(paths) != 1 || (!binding.AllowPreexistingPaths && !c.mutatedPaths[paths[0]]) {
			return provider.ToolCall{}, false
		}
		args, ok := cloneToolArgs(c.profile.DeterministicRequiredEvidenceArgs[forcedTool])
		if !ok {
			return provider.ToolCall{}, false
		}
		args["file_path"] = paths[0]
		arguments, err := json.Marshal(args)
		if err != nil {
			return provider.ToolCall{}, false
		}
		return provider.ToolCall{
			ID: callID, Type: "function",
			Function: provider.FunctionCall{Name: forcedTool, Arguments: string(arguments)},
		}, true
	}
	if forcedTool == "meadow_browser_interaction_check" &&
		c.profile.ReplayPostActionInteractionAfterRepair {
		args := c.replayRequiredEvidenceArgs[forcedTool]
		if len(args) == 0 {
			return provider.ToolCall{}, false
		}
		binding, ok := c.profile.RequiredEvidenceBindings[forcedTool]
		if !ok || binding.MatchArg == "" || binding.MatchPriorTool == "" ||
			binding.MatchPriorArg == "" || !c.observedEvidenceTools[binding.MatchPriorTool] {
			return provider.ToolCall{}, false
		}
		currentPath := normalizeEvidencePath(stringMapValue(args, binding.MatchArg))
		priorPath := normalizeEvidencePath(
			c.observedEvidenceArgs[binding.MatchPriorTool][binding.MatchPriorArg],
		)
		if currentPath == "" || currentPath != priorPath ||
			(!binding.AllowPreexistingPaths && !c.mutatedPaths[currentPath]) {
			return provider.ToolCall{}, false
		}
		arguments, err := json.Marshal(args)
		if err != nil {
			return provider.ToolCall{}, false
		}
		return provider.ToolCall{
			ID: callID, Type: "function",
			Function: provider.FunctionCall{Name: forcedTool, Arguments: string(arguments)},
		}, true
	}
	if forcedTool != "typescript_web_build" {
		return provider.ToolCall{}, false
	}
	htmlPaths := c.mutatedPathsWithExtension(".html")
	tsPaths := c.mutatedPathsWithExtension(".ts")
	if len(htmlPaths) != 1 || len(tsPaths) != 1 {
		return provider.ToolCall{}, false
	}
	arguments, err := json.Marshal(map[string]string{
		"html_path": htmlPaths[0], "source_path": tsPaths[0],
	})
	if err != nil {
		return provider.ToolCall{}, false
	}
	return provider.ToolCall{
		ID: callID, Type: "function",
		Function: provider.FunctionCall{Name: "typescript_web_build", Arguments: string(arguments)},
	}, true
}

func cloneToolArgs(args map[string]any) (map[string]any, bool) {
	if len(args) == 0 {
		return nil, false
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, false
	}
	var copied map[string]any
	if json.Unmarshal(encoded, &copied) != nil || len(copied) == 0 {
		return nil, false
	}
	return copied, true
}

func (c *TaskController) mutatedPathsWithExtension(extension string) []string {
	if c == nil {
		return nil
	}
	paths := make([]string, 0, len(c.mutatedPaths))
	for path := range c.mutatedPaths {
		if strings.EqualFold(filepath.Ext(path), extension) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// ForcedTaskTool returns the exact next task-stage tool. Unlike the legacy
// evidence-only lock, a failed validator stays locked to its configured repair
// mutation even after invalid arguments or context compaction.
func (c *TaskController) ForcedTaskTool() string {
	if c == nil {
		return ""
	}
	if c.artifactPathRepairTool != "" {
		return c.artifactPathRepairTool
	}
	if c.requiredEvidenceCallRetryTool != "" {
		return c.requiredEvidenceCallRetryTool
	}
	if c.repairSourceRefreshPath != "" {
		return "read_file"
	}
	if _, repairTool := c.pendingRepairStage(); repairTool != "" {
		return repairTool
	}
	if tool := c.forcedInitialRepairMutationTool(); tool != "" {
		return tool
	}
	if initialTool := strings.TrimSpace(c.profile.InitialTool); initialTool != "" {
		if c.state.SuccessfulToolCalls == 0 ||
			(c.profile.InitialToolUntilMutations > 0 &&
				c.state.SuccessfulMutations < c.profile.InitialToolUntilMutations) {
			return initialTool
		}
	}
	return c.ForcedRequiredEvidenceTool()
}

// ForcedTaskTools returns the controller-selected primary stage plus any
// tightly bounded companion needed to complete that stage.  A selector-invalid
// interaction retry may expose one read of the exact HTML being validated; it
// never exposes edit, shell, list, or an unrelated file.  After the read
// succeeds the tool surface contracts back to the interaction validator.
func (c *TaskController) ForcedTaskTools() []string {
	primary := c.ForcedTaskTool()
	if primary == "" {
		return nil
	}
	result := []string{primary}
	if primary == "meadow_browser_interaction_check" &&
		c.requiredEvidenceCallRetryReadPath != "" &&
		!c.requiredEvidenceCallRetryReadUsed {
		result = append(result, "read_file")
	}
	if primary == "read_file" {
		// Simple-write locks to a read-back after the first save. Keep
		// write_file visible so a retry does not surface as unknown tool.
		if _, ok := c.profile.RequiredEvidenceBindings["read_file"]; ok {
			result = append(result, "write_file")
		}
	}
	return result
}

// EvidenceBackedRepairTool returns only a mutation selected from a concrete
// failed required-evidence result. It deliberately excludes initial writes,
// generic protocol recovery, and malformed validator-call retries. Kernel
// safety guards can use this narrow signal without treating every forced task
// tool as permission to mutate existing work.
func (c *TaskController) EvidenceBackedRepairTool() string {
	if c == nil {
		return ""
	}
	_, repairTool := c.pendingRepairStage()
	return repairTool
}

func (c *TaskController) forcedInitialRepairMutationTool() string {
	if c == nil || c.state.SuccessfulMutations > 0 {
		return ""
	}
	tool := strings.TrimSpace(c.profile.ForceMutationToolAfterInspections)
	minimum := c.profile.MinInspectionsBeforeForcedMutation
	if tool == "" || minimum <= 0 || c.state.SuccessfulInspections < minimum {
		return ""
	}
	return tool
}

// ModelLedProgressGuidance reports a mechanically observed lack of progress
// without selecting a tool or authoring task semantics. Model-led runtimes use
// this bounded envelope to keep repeated inspections from hiding a still-
// missing named deliverable.
func (c *TaskController) ModelLedProgressGuidance(deliverables []Deliverable) []string {
	if c == nil || !c.Enabled() {
		return nil
	}
	if c.state.Phase == TaskPhaseVerify && c.state.SuccessfulMutations > 0 {
		return []string{
			"OBSERVED_VERIFICATION_GAP: saved deliverable bytes changed, but no fresh fail-capable acceptance result proves the current bytes satisfy the user's explicit constraints. Reading unchanged inputs or the output again is inspection, not validation. Choose and execute an independent checker against the authoritative inputs and saved output; if it fails, repair the evidenced discrepancy and rerun the checker before completion.",
		}
	}
	if c.state.SuccessfulMutations > 0 || c.state.SuccessfulInspections < 3 {
		return nil
	}
	missing := missingDeliverablePaths(deliverables)
	if len(missing) == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf(
			"OBSERVED_PROGRESS: %d successful inspections and 0 workspace mutations; requested outputs are still missing: %s. Choose the next action yourself, but do not reread unchanged inputs or only print a derived result; make durable progress toward the saved deliverable.",
			c.state.SuccessfulInspections,
			strings.Join(missing, ", "),
		),
	}
}

// ActiveRepairGuidance is durable model-visible state. It is intentionally
// injected even for model-led profiles because it reports an observed tool
// failure; it does not invent a plan or choose semantic task content.
func (c *TaskController) ActiveRepairGuidance() []string {
	if c == nil {
		return nil
	}
	if c.artifactPathRepairTool != "" {
		guidance := []string{
			"ARTIFACT_PATH_REPAIR_ACTIVE: retry only `" + c.artifactPathRepairTool + "` now with every artifact path under `" + strings.TrimSpace(c.profile.RequiredArtifactRoot) + "`. Do not write into /workspace/session or the project root.",
			"Rejected path call: " + c.artifactPathRepairErr,
		}
		if c.artifactPathRepairTool == "write_file" &&
			(strings.Contains(c.artifactPathRepairErr, "artifact_edit_requires_session_copy") ||
				strings.Contains(c.artifactPathRepairErr, "edit_file_not_found")) {
			guidance = append(guidance, "The session copy does not exist yet. Create it now with `write_file` and the complete content already read/generated; do not call `edit` until that write succeeds.")
		}
		return guidance
	}
	if c.requiredEvidenceCallRetryTool != "" {
		primary := "EVIDENCE_CALL_RETRY_ACTIVE: the previous validator call did not prove an artifact defect. Call `" + c.requiredEvidenceCallRetryTool + "` now with corrected native arguments. Do not edit, rewrite, use shell, or claim completion."
		if c.requiredEvidenceCallRetryReadPath != "" && !c.requiredEvidenceCallRetryReadUsed {
			primary += " If the rendered-control excerpt is insufficient, one `read_file` call is allowed only for `" + c.requiredEvidenceCallRetryReadPath + "`; after that read, retry the validator immediately."
		} else {
			primary += " Do not read or list files."
		}
		guidance := []string{primary}
		if diagnostic := strings.TrimSpace(c.requiredEvidenceCallRetryError); diagnostic != "" {
			guidance = append(guidance, "Rejected validator call: "+diagnostic)
		}
		if c.requiredEvidenceCallRetryTool == "meadow_browser_visual_check" {
			guidance = append(guidance, "If the saved page intentionally uses DOM/CSS rather than a canvas and the user did not require canvas, retry with require_canvas=false. Do not change the artifact merely to satisfy an inapplicable canvas option.")
		} else if c.requiredEvidenceCallRetryTool == "meadow_browser_interaction_check" {
			if strings.Contains(c.requiredEvidenceCallRetryError, "INTERACTION_COVERAGE_INVALID") {
				guidance = append(guidance, "The coverage contract was malformed, so no application defect was proven. Retry the same interaction validator only. Step numbers are 1-based: if steps[0] is press_key and steps[1] is assert_game_state, use action_steps:[1] and assertion_steps:[2]. action_steps may reference only fill/click/context_click/press_key; assertion_steps may reference only later assert_dom/assert_game_state steps. Every row must quote a literal user requirement, bind it to a real interaction action, and identify a later assertion step. Do not edit the artifact or weaken the requested behavior.")
			} else if strings.Contains(c.requiredEvidenceCallRetryError, "INTERACTION_CHECK_CALL_INVALID") ||
				interactionSelectorMissing(c.requiredEvidenceCallRetryError) {
				guidance = append(guidance, "The acceptance-test selector did not exist in the rendered page. Correct only that validator selector using the rendered controls or the exact saved HTML; a valid CSS class/position selector is allowed when a requested control has no id. Retain every user-required post-action assertion. Do not edit the artifact merely to create an invented selector.")
			} else if strings.Contains(c.requiredEvidenceCallRetryError, "step 1 (assert_dom)") {
				guidance = append(guidance, "The first precondition assertion failed before any interaction. Use the observed rendered DOM excerpt to correct only the initial-state assertion, then keep every user-required post-action assertion. Do not weaken or remove the checks for requested behavior.")
			} else {
				guidance = append(guidance, "A rendered-DOM assertion failed, but one validator-call retry is allowed before changing source. Rebuild the ordered acceptance call from the user's literal requested states and the observed DOM excerpt; keep all fill/click steps and every requested assertion. Do not weaken the expected behavior or edit the artifact on this retry.")
			}
		}
		return guidance
	}
	if tool := c.forcedInitialRepairMutationTool(); tool != "" {
		return []string{
			"EXISTING_REPAIR_ACTION_REQUIRED: the current artifact sources have already been inspected. Call only `" + tool + "` now and implement the user's requested change using exact current bytes. Do not read/list again, do not submit an identical replacement, and do not validate before one real mutation succeeds.",
		}
	}
	if forced := c.ForcedRequiredEvidenceTool(); forced != "" {
		switch forced {
		case "presentation_artifact":
			return []string{
				"REQUIRED_PRESENTATION_ACTIVE: call only `presentation_artifact` now and create the complete user-requested deck from the authoritative inputs and computed results. A JSON file, script, or prose summary does not replace this typed deliverable.",
			}
		case "run_shell_command":
			return []string{
				"REQUIRED_VALIDATION_ACTIVE: call only `run_shell_command` now with `purpose=validation`. Run a bounded executable acceptance check that rereads the authoritative inputs and existing requested outputs, exits non-zero on any mismatch, and does not create or rewrite deliverables. Do not use a print-only diagnostic and do not answer in prose.",
			}
		}
	}
	if c.profile.ModelDirectedEvidenceRecovery {
		for _, evidenceTool := range c.requiredEvidenceTools {
			if !c.requiredEvidenceRepair[evidenceTool] ||
				strings.TrimSpace(c.requiredRepairTools[evidenceTool]) != "" {
				continue
			}
			guidance := []string{
				"MODEL_DIRECTED_EVIDENCE_RECOVERY: `" + evidenceTool + "` reported a concrete verification failure. Keep the user's requested behavior and choose the smallest relevant safe capability yourself: inspect grounded source when needed, edit only if the artifact is wrong, or retry the validator if its assertion was wrong. Do not repeat an identical failed call and do not claim completion until fresh evidence passes.",
			}
			if diagnostic := strings.TrimSpace(c.requiredEvidenceErrors[evidenceTool]); diagnostic != "" {
				guidance = append(guidance, "Validator diagnostic: "+diagnostic)
				if evidenceTool == "meadow_browser_interaction_check" &&
					strings.Contains(diagnostic, "window.render_game_to_text() is missing") {
					guidance = append(guidance,
						"MISSING_GAME_STATE_HOOK: the validator calls the exact global property `window.render_game_to_text`. A plain or duplicated `function getState()` is unreachable and does not repair this failure. Make one source mutation that assigns `window.render_game_to_text = () => JSON.stringify({running, paused, score, direction, head:{x,y}})` using the artifact's real state variables. Also assign the requested `window.advanceTime = (ms) => { ... }` to deterministic ticks. Do not add another unassigned helper with a different name, and do not retry validation until both exact window properties exist in the saved source.")
				}
			}
			return guidance
		}
	}
	if c.repairSourceRefreshPath != "" {
		return []string{
			"REPAIR_SOURCE_REFRESH_ACTIVE: the previous edit did not make a real source change. Call only `read_file` now with `file_path`=`" + c.repairSourceRefreshPath + "`. Do not guess another old_string and do not read any other file.",
			"After that exact read succeeds, the next turn will be locked back to `edit` with the validator diagnostic preserved. The next `new_string` must actually correct the diagnostic and must differ from `old_string`.",
		}
	}
	evidenceTool, repairTool := c.pendingRepairStage()
	if evidenceTool == "" || repairTool == "" {
		return nil
	}
	guidance := []string{
		"REPAIR_STAGE_ACTIVE: the saved artifact failed `" + evidenceTool + "`. Call only `" + repairTool + "` now, using the exact diagnostic below. Do not read/list, do not retry the validator unchanged, and do not claim completion.",
	}
	if c.repairRewriteTargetPath != "" {
		guidance = append(guidance, "REPAIR_TARGET_LOCK: `"+repairTool+".file_path` must be exactly `"+c.repairRewriteTargetPath+"`; do not modify a sibling HTML/TypeScript file that the validator did not identify as defective.")
	}
	if diagnostic := strings.TrimSpace(c.requiredEvidenceErrors[evidenceTool]); diagnostic != "" {
		guidance = append(guidance, "Validator diagnostic: "+diagnostic)
	}
	if repairError := strings.TrimSpace(c.repairToolErrors[repairTool]); repairError != "" {
		guidance = append(guidance, "Previous repair call was rejected. Correct its native arguments exactly: "+repairError)
	}
	if repairTool == "write_file" && c.profile.RequiredEvidenceRepairTools[evidenceTool] == "edit" {
		target := c.repairRewriteTargetPath
		if target == "" {
			target = "the same saved source file from the latest read"
		}
		guidance = append(guidance, "EXACT_EDIT_EXHAUSTED: rewrite only `"+target+"` with complete corrected content from the latest read. Preserve all working features, do not use placeholders, and make the validator-reported behavior materially different before retrying it.")
	}
	return guidance
}

// ForcedTaskCallViolation binds a validator-directed repair (and any later
// full-file fallback) to the exact source named by its structured diagnostic.
// Without this guard a small model can rewrite a sibling HTML shell while the
// active repair target is TypeScript, destroying previously working UI.
func (c *TaskController) ForcedTaskCallViolation(tool string, args map[string]any) string {
	tool = strings.TrimSpace(tool)
	if c == nil {
		return ""
	}
	if tool == "read_file" && c.requiredEvidenceCallRetryTool == "meadow_browser_interaction_check" &&
		c.requiredEvidenceCallRetryReadPath != "" && !c.requiredEvidenceCallRetryReadUsed {
		actual := normalizeEvidencePath(stringMapValue(args, "file_path"))
		if actual == "" || actual == c.requiredEvidenceCallRetryReadPath {
			return ""
		}
		return fmt.Sprintf(
			"EVIDENCE_CALL_RETRY_READ_MISMATCH: read_file.file_path must equal the exact HTML under validation %q, got %q",
			c.requiredEvidenceCallRetryReadPath, actual,
		)
	}
	if (tool != "edit" && tool != "write_file") || c.repairRewriteTargetPath == "" {
		return ""
	}
	_, repairTool := c.pendingRepairStage()
	if repairTool != tool {
		return ""
	}
	actual := normalizeEvidencePath(stringMapValue(args, "file_path"))
	// Missing/wrong-typed file_path belongs to the tool schema validator, which
	// returns the complete required-args contract.  The target lock only guards
	// a concrete competing path; otherwise it would replace a useful structured
	// schema error with a vague path mismatch and strand small-model recovery.
	if actual == "" {
		return ""
	}
	if actual == c.repairRewriteTargetPath {
		return ""
	}
	return fmt.Sprintf(
		"REPAIR_TARGET_MISMATCH: the validator identified %q as the repair source; %s.file_path must equal that exact source, got %q",
		c.repairRewriteTargetPath, tool, actual,
	)
}

func stringMapValue(args map[string]any, key string) string {
	if value, ok := args[key].(string); ok {
		return value
	}
	return ""
}

// RestoreRequiredEvidenceFailure reconstructs the unresolved repair envelope
// from retained provider history. This makes the lock independent of whether
// the original large tool pair survived context compaction.
func (c *TaskController) RestoreRequiredEvidenceFailure(tool, content string) {
	c.restoreRequiredEvidenceFailure(tool, content, nil)
}

func (c *TaskController) restoreRequiredEvidenceFailure(tool, content string, args map[string]any) {
	if c == nil || !c.requiredEvidenceSet[tool] {
		return
	}
	repairTool := strings.TrimSpace(c.profile.RequiredEvidenceRepairTools[tool])
	execution := ToolExecution{Tool: tool, Status: ToolError, Executed: true, Args: args, LLMContent: content}
	callRetry := requiredEvidenceNeedsCallRetry(execution)
	if c.profile.ModelDirectedEvidenceRecovery && interactionAssertionFailure(content) {
		callRetry = false
	}
	if c.profile.RepairPostActionInteractionImmediately && postActionInteractionAssertionFailure(content) {
		callRetry = false
	}
	if callRetry {
		c.requiredEvidenceCallRetryTool = tool
		c.requiredEvidenceCallRetryError = boundedRepairError(content)
		c.requiredEvidenceCallRetryReadPath = ""
		c.requiredEvidenceCallRetryReadUsed = false
		if tool == "meadow_browser_interaction_check" &&
			(strings.Contains(content, "INTERACTION_CHECK_CALL_INVALID") || interactionSelectorMissing(content)) {
			c.requiredEvidenceCallRetryReadPath = normalizeEvidencePath(stringMapValue(args, "file_path"))
		}
		c.state.Phase = TaskPhaseRecover
		return
	}
	if repairTool == "" {
		return
	}
	c.requiredEvidenceRepair[tool] = true
	c.requiredEvidenceErrors[tool] = boundedRepairError(content)
	c.requiredRepairTools[tool] = repairTool
	if (repairTool == "edit" || repairTool == "write_file") && c.repairRewriteTargetPath == "" {
		c.repairRewriteTargetPath = normalizeEvidencePath(typedToolFailurePath(content))
	}
	c.state.RepairEvidenceTool = tool
	c.state.RepairTool = repairTool
	c.state.Phase = TaskPhaseRecover
}

func (c *TaskController) clearRequiredEvidenceCallRetry() {
	if c == nil {
		return
	}
	c.requiredEvidenceCallRetryTool = ""
	c.requiredEvidenceCallRetryError = ""
	c.requiredEvidenceCallRetryReadPath = ""
	c.requiredEvidenceCallRetryReadUsed = false
}

// RestoreRepairToolFailure keeps the latest native-call schema error paired
// with an already-restored validator failure.
func (c *TaskController) RestoreRepairToolFailure(tool, content string) {
	if c == nil {
		return
	}
	_, repairTool := c.pendingRepairStage()
	if repairTool == "" || strings.TrimSpace(tool) != repairTool {
		return
	}
	c.repairToolErrors[repairTool] = boundedRepairError(content)
	if repairTool == "edit" && editFailureNeedsSourceRefresh(content) {
		c.repairSourceRefreshPath = typedToolFailurePath(content)
		c.repairRewriteTargetPath = normalizeEvidencePath(c.repairSourceRefreshPath)
	}
	c.state.RepairTool = repairTool
	c.state.Phase = TaskPhaseRecover
}

func typedToolFailurePath(content string) string {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "TOOL_ERROR ") {
		return ""
	}
	var payload struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(content, "TOOL_ERROR ")), &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Path)
}

func editFailureNeedsSourceRefresh(content string) bool {
	switch typedToolFailureCode(content) {
	case "edit_old_string_not_found", "edit_no_change", "repair_no_change", "prior_read_required", "prior_read_stale":
		return true
	default:
		return false
	}
}

func noChangeRepairError(execution ToolExecution, lockedPath string) string {
	path := normalizeEvidencePath(stringMapValue(execution.Args, "file_path"))
	if path == "" {
		path = normalizeEvidencePath(lockedPath)
	}
	payload, err := json.Marshal(map[string]string{
		"code":            "repair_no_change",
		"path":            path,
		"required_action": "read_current_source_then_apply_a_materially_different_minimal_edit",
	})
	if err != nil {
		return "TOOL_ERROR {\"code\":\"repair_no_change\",\"required_action\":\"apply_a_materially_different_repair\"}"
	}
	return "TOOL_ERROR " + string(payload)
}

func (c *TaskController) RestoreArtifactPathFailure(tool, content string) {
	if c == nil || !c.isArtifactPathTool(tool) {
		return
	}
	c.artifactPathRepairTool = artifactPathRepairTool(tool, content)
	if c.artifactPathRepairTool == "" {
		return
	}
	c.artifactPathRepairErr = boundedRepairError(content)
	c.state.Phase = TaskPhaseRecover
}

func artifactPathRepairTool(tool, content string) string {
	switch typedToolFailureCode(content) {
	case "artifact_path_outside_session":
		return strings.TrimSpace(tool)
	case "artifact_edit_requires_session_copy":
		return "write_file"
	case "edit_file_not_found":
		return "write_file"
	default:
		return ""
	}
}

func (c *TaskController) repairToolForRequiredEvidence(execution ToolExecution) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.profile.RequiredEvidenceRepairTools[execution.Tool])
}

func (c *TaskController) pendingRepairStage() (string, string) {
	if c == nil {
		return "", ""
	}
	for _, evidenceTool := range c.requiredEvidenceTools {
		if !c.requiredEvidenceRepair[evidenceTool] {
			continue
		}
		repairTool := strings.TrimSpace(c.requiredRepairTools[evidenceTool])
		if repairTool == "" {
			repairTool = strings.TrimSpace(c.profile.RequiredEvidenceRepairTools[evidenceTool])
		}
		if repairTool != "" {
			return evidenceTool, repairTool
		}
	}
	return "", ""
}

func boundedRepairError(content string) string {
	content = strings.TrimSpace(content)
	const maxBytes = 6000
	if len(content) <= maxBytes {
		return content
	}
	return content[:maxBytes] + "…"
}

func (c *TaskController) isArtifactPathTool(tool string) bool {
	if c == nil || strings.TrimSpace(c.profile.RequiredArtifactRoot) == "" {
		return false
	}
	return len(c.profile.RequiredArtifactPathArgs[tool]) > 0
}

func (c *TaskController) ArtifactPathPolicy() (string, map[string][]string) {
	if c == nil {
		return "", nil
	}
	return c.profile.RequiredArtifactRoot, c.profile.RequiredArtifactPathArgs
}

func (c *TaskController) missingRequiredEvidenceTools() []string {
	if c == nil || len(c.requiredEvidenceTools) == 0 {
		return nil
	}
	missing := make([]string, 0, len(c.requiredEvidenceTools))
	for _, name := range c.requiredEvidenceTools {
		if !c.observedEvidenceTools[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (c *TaskController) Guidance(evidence EvidenceSnapshot, deliverables []Deliverable, validation ValidationDecision) []string {
	if !c.Enabled() {
		return nil
	}
	c.refreshPhase(evidence, deliverables, validation)
	guidance := []string{
		"Autonomous execution contract: keep working until the requested result exists and is supported by tool evidence; do not use prose as a substitute for action.",
		"Inspect real workspace or source state before changing an existing artifact. After a failed call, read the error, change the approach, and do not repeat the same failed call unchanged.",
		"For run_shell_command, set directory to /workspace and use paths relative to that directory inside the command; /workspace is a tool alias, not a literal shell path.",
		"When a shell command is the final test, build, lint, or acceptance check, set purpose=validation. Parsing a file or printing it back proves syntax only, not that the task requirements are correct.",
		"Browser screenshots are human-review artifacts by default. For coding tasks, use build output, runtime diagnostics, DOM/canvas probes, and interaction assertions. If the user explicitly asks for image analysis or a visual comparison, use inspect_image or meadow_browser_visual_check with include_model_image=true and reason only from attached still-image pixels; do not treat them as proof of motion or interaction.",
		"A final answer must be concise and evidence-backed. Never claim a file, command, test, source, or result that the tools did not confirm.",
		"Current execution phase: " + string(c.state.Phase) + ".",
	}
	if validation.Required && !validation.Fresh {
		guidance = append(guidance, "Completion is blocked until the selected validator passes after the latest code change.")
	}
	if missing := missingDeliverablePaths(deliverables); len(missing) > 0 {
		guidance = append(guidance, "Completion is blocked until these requested outputs exist: "+strings.Join(missing, ", ")+".")
	}
	return guidance
}

func (c *TaskController) State() TaskState {
	if c == nil {
		return TaskState{}
	}
	return c.state
}

// Snapshot refreshes the observable phase from current generic execution
// facts without selecting a next action or changing the model-authored plan.
func (c *TaskController) Snapshot(evidence EvidenceSnapshot, deliverables []Deliverable, validation ValidationDecision) TaskState {
	if c == nil {
		return TaskState{}
	}
	c.refreshPhase(evidence, deliverables, validation)
	return c.state
}

// ResetRecovery starts one fresh model strategy without discarding successful
// inspection, mutation, or evidence facts already observed in this run.
func (c *TaskController) ResetRecovery() {
	if !c.Enabled() {
		return
	}
	c.state.ConsecutiveToolFailures = 0
	if c.state.Phase == TaskPhaseStalled {
		c.state.Phase = TaskPhaseRecover
	}
}

func (c *TaskController) continueDecision(kind NudgeKind, reason, prompt string, phase TaskPhase) TaskDecision {
	c.state.RecoveryDirectives++
	c.state.Phase = phase
	return TaskDecision{Required: true, Kind: kind, Reason: reason, Prompt: prompt, Phase: phase}
}

func (c *TaskController) refreshPhase(evidence EvidenceSnapshot, deliverables []Deliverable, validation ValidationDecision) {
	if !c.Enabled() {
		return
	}
	repairEvidenceTool, repairTool := c.pendingRepairStage()
	c.state.RepairEvidenceTool = repairEvidenceTool
	c.state.RepairTool = repairTool
	switch {
	case c.state.ConsecutiveToolFailures >= c.profile.MaxConsecutiveFailures:
		c.state.Phase = TaskPhaseStalled
	case c.artifactPathRepairTool != "":
		c.state.Phase = TaskPhaseRecover
	case repairTool != "":
		c.state.Phase = TaskPhaseRecover
	case c.state.ConsecutiveToolFailures > 0:
		c.state.Phase = TaskPhaseRecover
	case c.profile.RequiresToolAction && c.state.SuccessfulToolCalls == 0:
		if c.profile.RequiresInspection {
			c.state.Phase = TaskPhaseInspect
		} else {
			c.state.Phase = TaskPhaseAct
		}
	case c.profile.RequiresInspection && c.state.SuccessfulInspections == 0:
		c.state.Phase = TaskPhaseInspect
	case c.profile.RequiresNetworkEvidence && c.state.SuccessfulEvidenceCalls == 0:
		c.state.Phase = TaskPhaseInspect
	case len(c.missingRequiredEvidenceTools()) > 0:
		c.state.Phase = TaskPhaseInspect
	case c.profile.RequiresMutation && c.state.SuccessfulMutations == 0 && !deliverablesSatisfied(deliverables):
		c.state.Phase = TaskPhaseAct
	case firstMissingDeliverable(deliverables) != "":
		c.state.Phase = TaskPhaseDeliver
	case validation.Required && !validation.Fresh:
		c.state.Phase = TaskPhaseVerify
	case len(evidence.VerifiableChangedPaths) > 0 &&
		(evidence.Verification == VerificationUnverified ||
			evidence.Verification == VerificationStale):
		// The final evidence gate rejects verifiable workspace mutations until a
		// fresh acceptance check observes the current bytes. Keep the controller
		// on the same contract so VerifiedFastFinal cannot stop immediately after
		// creating a code or structured-data artifact and then be downgraded by
		// the deferred finalizer. The model still owns the checker and its task
		// semantics; this branch only prevents contradictory lifecycle state.
		c.state.Phase = TaskPhaseVerify
	case evidence.Verification == VerificationFailed:
		c.state.Phase = TaskPhaseRecover
	default:
		c.state.Phase = TaskPhaseComplete
	}
}

func firstMissingDeliverable(deliverables []Deliverable) string {
	for _, deliverable := range deliverables {
		if !deliverable.Exists && strings.TrimSpace(deliverable.Path) != "" {
			return strings.TrimSpace(deliverable.Path)
		}
	}
	return ""
}

func deliverablesSatisfied(deliverables []Deliverable) bool {
	if len(deliverables) == 0 {
		return false
	}
	for _, deliverable := range deliverables {
		if !deliverable.Exists {
			return false
		}
	}
	return true
}

func missingDeliverablePaths(deliverables []Deliverable) []string {
	paths := make([]string, 0, len(deliverables))
	for _, deliverable := range deliverables {
		if !deliverable.Exists && strings.TrimSpace(deliverable.Path) != "" {
			paths = append(paths, strings.TrimSpace(deliverable.Path))
		}
	}
	sort.Strings(paths)
	return paths
}

func boundedFailure(execution ToolExecution) string {
	text := strings.TrimSpace(execution.LLMContent)
	if text == "" && execution.Err != nil {
		text = execution.Err.Error()
	}
	const maxRunes = 240
	runes := []rune(text)
	if len(runes) > maxRunes {
		text = string(runes[:maxRunes-1]) + "…"
	}
	if text == "" {
		text = fmt.Sprintf("%s returned status %s", execution.Tool, execution.Status)
	}
	return text
}
