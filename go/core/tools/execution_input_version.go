package tools

import "context"

// ExecutionInputVersionProvider is a trusted tool implementation contract for
// an operation tied to exact local input bytes. It must be side-effect-free,
// bounded and cancellable. A nonempty version is a lowercase SHA256 identity;
// model fields, page annotations and mutable timestamps cannot supply it.
// Returning a version does not change Kind, effect or permission requirements.
type ExecutionInputVersionProvider interface {
	ToolExecutionInputVersion(context.Context, map[string]any) (string, error)
}

type executionInputVersionKey struct{}

// WithExecutionInputVersion binds the executor's checkpointed input identity to
// the tool invocation. The implementing tool must reject changed input before
// its effect and verify the same identity before reporting success.
func WithExecutionInputVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, executionInputVersionKey{}, version)
}

func ExpectedExecutionInputVersion(ctx context.Context) string {
	version, _ := ctx.Value(executionInputVersionKey{}).(string)
	return version
}
