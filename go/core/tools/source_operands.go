package tools

import "context"

// SourceOperand is a private typed receipt from a successful indexed-source
// tool, not a model-authored URL or prose citation. Hash refers to the exact
// LF-normalized indexed text, not original PDF pixels or workbook formatting.
type SourceOperand struct {
	SourceID   string
	TextSHA256 string
}

type sourceOperandsContextKey struct{}

func ContextWithSourceOperands(ctx context.Context, sources []SourceOperand) context.Context {
	return context.WithValue(ctx, sourceOperandsContextKey{}, append([]SourceOperand(nil), sources...))
}

func SourceOperandsFromContext(ctx context.Context) []SourceOperand {
	if ctx == nil {
		return nil
	}
	sources, _ := ctx.Value(sourceOperandsContextKey{}).([]SourceOperand)
	return append([]SourceOperand(nil), sources...)
}

// RunCallLimitProvider declares a resource bound, not a workflow. The model
// chooses the arguments and whether to use the tool; the executor bounds calls.
type RunCallLimitProvider interface{ RunCallLimit() int }

// ArgumentValidator validates cross-field constraints without reading sources or
// causing effects. The executor runs it before reserving a bounded execution.
type ArgumentValidator interface{ ValidateArguments(map[string]any) error }
