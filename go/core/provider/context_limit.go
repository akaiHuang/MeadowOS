package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrUpstreamContextLimit is an admission rejection before any model stream.
// Only validated server resource counts, never task text, trigger recovery.
var ErrUpstreamContextLimit = errors.New("upstream context capacity exceeded")

type ContextLimitError struct {
	PromptTokens, GenerationTokens, ContextWindow int
	Cause                                         error
}

func (e *ContextLimitError) Error() string {
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return ErrUpstreamContextLimit.Error()
}
func (e *ContextLimitError) Unwrap() error { return ErrUpstreamContextLimit }

func contextLimitStatusError(status string, raw []byte, cause error) error {
	if status != "400 Bad Request" {
		return nil
	}
	var body struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	const format = "Request needs %d context tokens (%d prompt + %d max generation), but MAX_KV_SIZE is %d."
	var total, prompt, generation, window int
	detail := strings.TrimSpace(body.Detail)
	n, err := fmt.Sscanf(detail, format, &total, &prompt, &generation, &window)
	// The exact grammar, plausible bounds and arithmetic avoid treating an
	// arbitrary 400, malformed count or trailing server prose as permission to retry.
	if err != nil || n != 4 || detail != fmt.Sprintf(format, total, prompt, generation, window) ||
		prompt <= 0 || generation <= 0 || window <= generation ||
		prompt > 1<<30 || generation > 1<<30 || window > 1<<30 ||
		total != prompt+generation || total <= window {
		return nil
	}
	return &ContextLimitError{PromptTokens: prompt, GenerationTokens: generation, ContextWindow: window, Cause: cause}
}
