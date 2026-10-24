package agenttrace

import "context"

type runContextKey struct{}

// Context binds model transport observations to this exact admitted run.
func (e *Emitter) Context(ctx context.Context) context.Context {
	if e == nil {
		return ctx
	}
	return context.WithValue(ctx, runContextKey{}, e.meta.RunID)
}
func RunIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(runContextKey{}).(string)
	return id
}
