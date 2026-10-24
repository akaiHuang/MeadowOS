package serve

import "context"

type preferredResumeSessionContextKey struct{}

type preferredResumeSession struct {
	id           string
	allowMissing bool
}

// WithPreferredResumeSessionID pins a resume request to a live session already
// bound by trusted server middleware. HTTP callers cannot provide this value.
// A missing or mismatched session fails closed instead of creating a new handle.
// Only the first resume of an original durable handle may allow a missing live
// session; an existing handle with a different identity is always rejected.
func WithPreferredResumeSessionID(ctx context.Context, sessionID string, allowMissing ...bool) context.Context {
	preferred := preferredResumeSession{id: sessionID}
	if len(allowMissing) > 0 {
		preferred.allowMissing = allowMissing[0]
	}
	return context.WithValue(ctx, preferredResumeSessionContextKey{}, preferred)
}

func preferredResumeSessionID(ctx context.Context) (preferredResumeSession, bool) {
	preferred, ok := ctx.Value(preferredResumeSessionContextKey{}).(preferredResumeSession)
	return preferred, ok
}
