// Package agent contains the model-agnostic orchestration policies being
// extracted from Meadow's legacy Engine.
package agent

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/artifactvalidator"
)

type VerificationStatus string

const (
	VerificationNotRequired VerificationStatus = "not_required"
	VerificationUnverified  VerificationStatus = "unverified"
	VerificationPassed      VerificationStatus = "passed"
	VerificationFailed      VerificationStatus = "failed"
	VerificationStale       VerificationStatus = "stale"
)

type ToolStatus string

const (
	ToolOK                   ToolStatus = "ok"
	ToolError                ToolStatus = "error"
	ToolBlocked              ToolStatus = "blocked"
	ToolDenied               ToolStatus = "denied"
	ToolUnknown              ToolStatus = "unknown_tool"
	ToolConfirmationRequired ToolStatus = "confirmation_required"
)

// ToolObservation is a structured fact produced by the tool boundary. It does
// not contain model prose or full tool output.
type ToolObservation struct {
	Turn         int
	CallID       string
	Tool         string
	Status       ToolStatus
	Path         string
	MutatesFile  bool
	Validation   bool
	ValidatorID  string
	ExitCode     int
	HasExitCode  bool
	Verification VerificationStatus
	DurationMS   int64
}

type Evidence struct {
	Sequence     int                `json:"sequence"`
	Turn         int                `json:"turn,omitempty"`
	CallID       string             `json:"call_id,omitempty"`
	Tool         string             `json:"tool"`
	Status       ToolStatus         `json:"status"`
	Path         string             `json:"path,omitempty"`
	MutatesFile  bool               `json:"mutates_file,omitempty"`
	Validation   bool               `json:"validation,omitempty"`
	ValidatorID  string             `json:"validator_id,omitempty"`
	ExitCode     int                `json:"exit_code,omitempty"`
	HasExitCode  bool               `json:"has_exit_code,omitempty"`
	DurationMS   int64              `json:"duration_ms,omitempty"`
	Verification VerificationStatus `json:"verification,omitempty"`
}

type EvidenceSnapshot struct {
	Entries                []Evidence         `json:"entries,omitempty"`
	ChangedPaths           []string           `json:"changed_paths,omitempty"`
	VerifiableChangedPaths []string           `json:"verifiable_changed_paths,omitempty"`
	Verification           VerificationStatus `json:"verification"`
	ValidatorID            string             `json:"validator_id,omitempty"`
	ValidatorExitCode      int                `json:"validator_exit_code,omitempty"`
	HasValidatorExitCode   bool               `json:"has_validator_exit_code,omitempty"`
	DuplicateValidations   int                `json:"duplicate_validations"`
}

// EvidenceLedger is append-only within one run. A successful code mutation
// invalidates prior passing verification; only a later validator can make the
// evidence fresh again.
type EvidenceLedger struct {
	mu sync.Mutex

	sequence              int
	entries               []Evidence
	changedPaths          map[string]bool
	verifiableChanged     map[string]bool
	verification          VerificationStatus
	validatorID           string
	validatorExitCode     int
	hasValidatorExitCode  bool
	freshPassingValidator bool
	duplicateValidations  int
}

func NewEvidenceLedger() *EvidenceLedger {
	return &EvidenceLedger{
		changedPaths:      map[string]bool{},
		verifiableChanged: map[string]bool{},
		verification:      VerificationNotRequired,
	}
}

func (l *EvidenceLedger) Record(observation ToolObservation) Evidence {
	if l == nil {
		return Evidence{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sequence++
	entry := Evidence{
		Sequence:    l.sequence,
		Turn:        observation.Turn,
		CallID:      observation.CallID,
		Tool:        observation.Tool,
		Status:      observation.Status,
		Path:        normalizePath(observation.Path),
		MutatesFile: observation.MutatesFile,
		Validation:  observation.Validation,
		ValidatorID: observation.ValidatorID,
		ExitCode:    observation.ExitCode,
		HasExitCode: observation.HasExitCode,
		DurationMS:  observation.DurationMS,
	}

	if observation.Status == ToolOK && observation.MutatesFile && entry.Path != "" {
		l.changedPaths[entry.Path] = true
		if RequiresVerification(entry.Path) {
			l.verifiableChanged[entry.Path] = true
			if l.verification == VerificationPassed {
				l.verification = VerificationStale
			} else {
				l.verification = VerificationUnverified
			}
			l.freshPassingValidator = false
		}
	}

	if observation.Validation {
		if observation.Status == ToolOK && l.freshPassingValidator {
			l.duplicateValidations++
		}
		l.validatorID = observation.ValidatorID
		l.validatorExitCode = observation.ExitCode
		l.hasValidatorExitCode = observation.HasExitCode
		verification := observation.Verification
		if verification == "" {
			if observation.Status == ToolOK && (!observation.HasExitCode || observation.ExitCode == 0) {
				verification = VerificationPassed
			} else {
				verification = VerificationFailed
			}
		}
		if verification == VerificationPassed {
			l.verification = VerificationPassed
			l.freshPassingValidator = true
		} else {
			l.verification = VerificationFailed
			l.freshPassingValidator = false
		}
	}

	// Read-only work after an explicit validator does not invalidate that
	// validator. Only a later verifiable mutation above may make it stale.
	if len(l.verifiableChanged) == 0 && !observation.Validation && l.validatorID == "" {
		l.verification = VerificationNotRequired
	}
	entry.Verification = l.verification
	l.entries = append(l.entries, entry)
	return entry
}

func (l *EvidenceLedger) Snapshot() EvidenceSnapshot {
	if l == nil {
		return EvidenceSnapshot{Verification: VerificationNotRequired}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return EvidenceSnapshot{
		Entries:                append([]Evidence(nil), l.entries...),
		ChangedPaths:           sortedKeys(l.changedPaths),
		VerifiableChangedPaths: sortedKeys(l.verifiableChanged),
		Verification:           l.verification,
		ValidatorID:            l.validatorID,
		ValidatorExitCode:      l.validatorExitCode,
		HasValidatorExitCode:   l.hasValidatorExitCode,
		DuplicateValidations:   l.duplicateValidations,
	}
}

// RecordArtifactValidation appends a structural artifact check to the run
// ledger. A passing syntax.* check proves only that the file parses; it cannot
// prove task semantics, calculations, tests, or acceptance requirements, so it
// deliberately leaves task verification unverified. Failed syntax remains
// authoritative negative evidence.
func RecordArtifactValidation(l *EvidenceLedger, turn int, callID, path string, result artifactvalidator.Result) bool {
	if l == nil || result.Status == artifactvalidator.StatusNotApplicable {
		return false
	}
	status := ToolError
	verification := VerificationFailed
	if result.Status == artifactvalidator.StatusPassed || result.Status == artifactvalidator.StatusRepaired {
		status = ToolOK
		verification = VerificationPassed
	}
	validatorID := strings.TrimSpace(result.ValidatorID)
	if validatorID == "" {
		validatorID = "artifact_validation"
	}
	validation := true
	if status == ToolOK && strings.HasPrefix(strings.ToLower(validatorID), "syntax.") {
		validation = false
		verification = ""
	}
	l.Record(ToolObservation{
		Turn: turn, CallID: callID, Tool: "artifact_validation",
		Status: status, Path: path, Validation: validation,
		ValidatorID: validatorID, Verification: verification,
	})
	return true
}

// RequiresVerification is deliberately language-agnostic. Pure prose and flat
// data files are exempt; unfamiliar extensions remain verifiable by default.
func RequiresVerification(path string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(path)))
	ext := strings.ToLower(filepath.Ext(base))
	if _, ok := nonCodeExtensions[ext]; ok {
		return false
	}
	if ext == "" {
		_, prose := nonCodeFilenames[base]
		return !prose
	}
	return true
}

var nonCodeExtensions = map[string]struct{}{
	".md": {}, ".markdown": {}, ".mdx": {}, ".rst": {}, ".txt": {},
	".text": {}, ".adoc": {}, ".asciidoc": {}, ".org": {}, ".log": {},
	".csv": {}, ".tsv": {},
}

var nonCodeFilenames = map[string]struct{}{
	"license": {}, "licence": {}, "notice": {}, "authors": {},
	"contributors": {}, "changelog": {}, "codeowners": {},
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
