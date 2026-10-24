package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

type RecoveryProgressAction string

const (
	RecoveryProgressNone         RecoveryProgressAction = "none"
	RecoveryProgressFreshContext RecoveryProgressAction = "fresh_context"
	RecoveryProgressRestoreBest  RecoveryProgressAction = "restore_best"
)

type RecoveryProgressDecision struct {
	Action             RecoveryProgressAction `json:"action"`
	Episode            int                    `json:"episode"`
	NoProgressEpisodes int                    `json:"no_progress_episodes"`
	Progress           bool                   `json:"progress"`
	Reason             string                 `json:"reason"`
	FailureSignature   string                 `json:"failure_signature,omitempty"`
}

type RecoveryProgressSnapshot struct {
	Active             bool   `json:"active"`
	Episode            int    `json:"episode"`
	NoProgressEpisodes int    `json:"no_progress_episodes"`
	FailureSignature   string `json:"failure_signature,omitempty"`
	Coverage           int    `json:"coverage"`
	MutationCount      int    `json:"mutation_count"`
	FreshRecoveryUsed  bool   `json:"fresh_recovery_used"`
}

// RecoveryProgressTracker measures whether repair episodes increase grounded
// requirement coverage or pass validation. Successful writes and differently
// worded failure signatures alone never reset it.
type RecoveryProgressTracker struct {
	active                bool
	episode               int
	noProgressEpisodes    int
	failureSignature      string
	coverage              int
	mutationFingerprints  map[string]bool
	freshRecoveryUsed     bool
	freshMutationBaseline int
}

func NewRecoveryProgressTracker() *RecoveryProgressTracker {
	return &RecoveryProgressTracker{
		mutationFingerprints:  make(map[string]bool),
		freshMutationBaseline: -1,
	}
}

func (t *RecoveryProgressTracker) ResetForGuidance() {
	if t == nil {
		return
	}
	*t = RecoveryProgressTracker{
		mutationFingerprints:  make(map[string]bool),
		freshMutationBaseline: -1,
	}
}

func (t *RecoveryProgressTracker) BeginFailure(signature string, coverage int) RecoveryProgressDecision {
	if t == nil {
		return RecoveryProgressDecision{}
	}
	signature = normalizeFailureSignature(signature)
	if signature == "" {
		signature = "unknown_failure"
	}
	if !t.active {
		t.active = true
		t.episode++
		t.failureSignature = signature
		t.coverage = coverage
		clear(t.mutationFingerprints)
		t.freshMutationBaseline = -1
		return RecoveryProgressDecision{
			Action: RecoveryProgressNone, Episode: t.episode,
			NoProgressEpisodes: t.noProgressEpisodes, FailureSignature: signature,
			Reason: "failure_episode_started",
		}
	}
	return t.ObserveValidation(signature, false, coverage)
}

func (t *RecoveryProgressTracker) ObserveMutation(execution ToolExecution) {
	if t == nil || !t.active || !execution.Executed || execution.NoChange {
		return
	}
	payload := execution.Diff + "\x00" + strings.Join(execution.ChangedPaths, "\x00")
	if payload == "\x00" {
		payload = execution.Tool + "\x00" + execution.Display
	}
	sum := sha256.Sum256([]byte(payload))
	t.mutationFingerprints[hex.EncodeToString(sum[:])] = true
}

func (t *RecoveryProgressTracker) ObserveValidation(signature string, passed bool, coverage int) RecoveryProgressDecision {
	if t == nil {
		return RecoveryProgressDecision{}
	}
	signature = normalizeFailureSignature(signature)
	if passed {
		decision := RecoveryProgressDecision{
			Action: RecoveryProgressNone, Episode: t.episode,
			NoProgressEpisodes: t.noProgressEpisodes, Progress: t.active,
			Reason: "validation_passed", FailureSignature: signature,
		}
		t.active = false
		t.noProgressEpisodes = 0
		t.failureSignature = ""
		t.coverage = coverage
		clear(t.mutationFingerprints)
		t.freshMutationBaseline = -1
		return decision
	}
	if !t.active {
		return t.BeginFailure(signature, coverage)
	}
	progress := coverage > t.coverage
	t.episode++
	decision := RecoveryProgressDecision{
		Action: RecoveryProgressNone, Episode: t.episode, Progress: progress,
		FailureSignature: signature,
	}
	if progress {
		t.noProgressEpisodes = 0
		t.failureSignature = signature
		t.coverage = coverage
		clear(t.mutationFingerprints)
		t.freshMutationBaseline = -1
		decision.Reason = "grounded_coverage_increased"
		return decision
	}
	if signature != "" {
		t.failureSignature = signature
	}
	t.noProgressEpisodes++
	decision.NoProgressEpisodes = t.noProgressEpisodes
	decision.Reason = "validation_failed_without_coverage_gain"
	if t.noProgressEpisodes == 2 && !t.freshRecoveryUsed {
		t.freshRecoveryUsed = true
		t.freshMutationBaseline = len(t.mutationFingerprints)
		decision.Action = RecoveryProgressFreshContext
	} else if t.noProgressEpisodes == 3 && t.freshMutationBaseline >= 0 &&
		len(t.mutationFingerprints) > t.freshMutationBaseline {
		// A fresh context is useful only when it produces a materially new
		// workspace mutation. Give that new strategy one bounded validation
		// grace; the next no-progress result restores the best checkpoint.
		t.freshMutationBaseline = -1
		decision.Reason = "fresh_recovery_distinct_mutation_grace"
	} else if t.noProgressEpisodes >= 3 {
		decision.Action = RecoveryProgressRestoreBest
	}
	return decision
}

func (t *RecoveryProgressTracker) Snapshot() RecoveryProgressSnapshot {
	if t == nil {
		return RecoveryProgressSnapshot{}
	}
	return RecoveryProgressSnapshot{
		Active: t.active, Episode: t.episode, NoProgressEpisodes: t.noProgressEpisodes,
		FailureSignature: t.failureSignature, Coverage: t.coverage,
		MutationCount: len(t.mutationFingerprints), FreshRecoveryUsed: t.freshRecoveryUsed,
	}
}

var failureVolatileNumber = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
var failureVolatilePath = regexp.MustCompile(`(?:/[^\s:]+)+`)
var failureWhitespace = regexp.MustCompile(`\s+`)

func normalizeFailureSignature(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = failureVolatilePath.ReplaceAllString(value, "<path>")
	value = failureVolatileNumber.ReplaceAllString(value, "<n>")
	value = failureWhitespace.ReplaceAllString(value, " ")
	if len(value) > 1200 {
		value = value[:1200]
	}
	return value
}

func recoveryFailureSignature(execution ToolExecution) string {
	parts := []string{execution.Tool, execution.LLMContent, execution.Display}
	paths := append([]string(nil), execution.ChangedPaths...)
	sort.Strings(paths)
	parts = append(parts, paths...)
	return normalizeFailureSignature(strings.Join(parts, "\n"))
}

func preferRecoveryProgressDecision(current, candidate RecoveryProgressDecision) RecoveryProgressDecision {
	priority := func(action RecoveryProgressAction) int {
		switch action {
		case RecoveryProgressRestoreBest:
			return 3
		case RecoveryProgressFreshContext:
			return 2
		default:
			return 1
		}
	}
	if priority(candidate.Action) > priority(current.Action) ||
		priority(candidate.Action) == priority(current.Action) && candidate.Episode > current.Episode {
		return candidate
	}
	return current
}
