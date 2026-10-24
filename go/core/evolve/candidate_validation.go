package evolve

import "fmt"

// CandidateValidationError rejects candidate-authored content which the model
// can repair. Only deterministic policy, bounds and parse checks may produce
// this type; filesystem, runtime and transport failures must remain untyped.
type CandidateValidationError struct {
	Reason string
}

func (e *CandidateValidationError) Error() string { return e.Reason }

func candidateValidationf(format string, args ...any) error {
	return &CandidateValidationError{Reason: fmt.Sprintf(format, args...)}
}
