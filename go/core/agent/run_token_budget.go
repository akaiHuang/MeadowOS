package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const MaxRunTokenBudget int64 = 100_000_000

var ErrRunTokenBudget = errors.New("run processed-token budget exhausted")

type runTokenBudgetKey struct{}

// RunTokenUsage counts cumulative processed input/output, including cached
// input, not money or unique context size. Estimates are explicitly separate.
type RunTokenUsage struct {
	Limit              int64 `json:"limit"`
	ChargedTokens      int64 `json:"chargedTokens"`
	ReportedTokens     int64 `json:"reportedTokens"`
	EstimatedTokens    int64 `json:"estimatedTokens"`
	ReservedTokens     int64 `json:"reservedTokens"`
	DispatchedRequests int64 `json:"dispatchedRequests"`
	Exhausted          bool  `json:"exhausted"`
}

type runTokenBudget struct {
	mu    sync.Mutex
	usage RunTokenUsage
}

// WithRunTokenBudget starts a caller-owned meter. Derived inference/advisor/
// child contexts share it; a new explicitly submitted run gets a fresh meter.
func WithRunTokenBudget(parent context.Context, limit int64) (context.Context, error) {
	if limit < 1 || limit > MaxRunTokenBudget {
		return nil, fmt.Errorf("runTokenLimit must be between 1 and %d", MaxRunTokenBudget)
	}
	return context.WithValue(parent, runTokenBudgetKey{}, &runTokenBudget{usage: RunTokenUsage{Limit: limit}}), nil
}

func runTokenMeter(ctx context.Context) *runTokenBudget {
	meter, _ := ctx.Value(runTokenBudgetKey{}).(*runTokenBudget)
	return meter
}

func RunTokenBudgetUsage(ctx context.Context) *RunTokenUsage {
	meter := runTokenMeter(ctx)
	if meter == nil {
		return nil
	}
	meter.mu.Lock()
	defer meter.mu.Unlock()
	usage := meter.usage
	return &usage
}

func runTokenBudgetExhausted(ctx context.Context) bool {
	usage := RunTokenBudgetUsage(ctx)
	return usage != nil && usage.Exhausted
}

func (meter *runTokenBudget) reserve(request provider.ChatRequest) (int64, error) {
	if meter == nil {
		return 0, nil
	}
	prompt := int64(max(0, EstimateRequestPromptTokens(request)))
	meter.mu.Lock()
	defer meter.mu.Unlock()
	remaining := meter.usage.Limit - meter.usage.ChargedTokens - meter.usage.ReservedTokens
	// An uncapped request has no smaller defensible output reservation. Normal
	// product calls always supply the unchanged model-specific output cap.
	reserve := remaining
	if request.MaxTokens != nil && *request.MaxTokens > 0 {
		reserve = prompt + int64(*request.MaxTokens)
	}
	if meter.usage.Exhausted || remaining <= 0 || prompt > remaining || reserve > remaining {
		meter.usage.Exhausted = true
		return 0, ErrRunTokenBudget
	}
	meter.usage.ReservedTokens += reserve
	meter.usage.DispatchedRequests++
	return reserve, nil
}

func (meter *runTokenBudget) settle(reserved int64, result *provider.StreamResult, streamErr error) bool {
	if meter == nil {
		return false
	}
	var charge int64
	reported := false
	if result != nil && result.Usage != nil {
		usage := result.Usage
		charge = max(int64(max(0, usage.TotalTokens)), int64(max(0, usage.PromptTokens))+int64(max(0, usage.CompletionTokens)))
		reported = charge > 0
	}
	if !reported {
		var capacity *provider.ContextLimitError
		if result == nil && errors.As(streamErr, &capacity) {
			// This typed protocol error is explicitly pre-generation, so normal
			// bounded capacity retries do not consume a fictitious reservation.
			charge = 0
		} else {
			charge = reserved
		}
	}
	meter.mu.Lock()
	defer meter.mu.Unlock()
	meter.usage.ReservedTokens -= reserved
	meter.usage.ChargedTokens += charge
	if reported {
		meter.usage.ReportedTokens += charge
	} else {
		meter.usage.EstimatedTokens += charge
	}
	if meter.usage.ChargedTokens >= meter.usage.Limit {
		meter.usage.Exhausted = true
	}
	return meter.usage.Exhausted
}
