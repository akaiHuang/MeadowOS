//go:build !darwin || !cgo

package browserworker

// NewExecutor fails closed on platforms that cannot bind the native macOS
// WebKit executor. There is deliberately no shell/browser-process fallback.
func NewExecutor() (ManagedExecutor, error) {
	return nil, ErrExecutorUnavailable
}
