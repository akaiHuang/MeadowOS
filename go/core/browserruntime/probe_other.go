//go:build !darwin || !cgo

package browserruntime

import "context"

func nativeProbe(_ context.Context, _ string) (Identity, error) {
	return Identity{}, NewError(Unknown, "loaded Browser executable verification requires native macOS process inspection")
}
