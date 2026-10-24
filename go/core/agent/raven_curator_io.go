package agent

import (
	"context"
	"errors"
)

var errRavenCuratorIOBusy = errors.New("native Curator private I/O capacity is occupied")

// At most two blocked private-filesystem operations can remain across runs.
// A caller never waits for an occupied worker or for stalled filesystem I/O
// after Stop. This bounds the caller's wait, not the kernel's physical drain.
type ravenCuratorIOPool struct{ slots chan struct{} }

var nativeRavenCuratorIO = &ravenCuratorIOPool{slots: make(chan struct{}, 2)}

type ravenCuratorIOResult[T any] struct {
	value T
	err   error
}

func ravenCuratorBoundedIO[T any](ctx context.Context, pool *ravenCuratorIOPool, operation func() (T, error)) (T, error) {
	var zero T
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	select {
	case pool.slots <- struct{}{}:
	case <-ctx.Done():
		return zero, ctx.Err()
	default:
		return zero, errRavenCuratorIOBusy
	}
	result := make(chan ravenCuratorIOResult[T], 1)
	go func() {
		value, err := func() (T, error) {
			defer func() { <-pool.slots }()
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
			return operation()
		}()
		result <- ravenCuratorIOResult[T]{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case outcome := <-result:
		// A late outcome cannot revive an expired stage, update its selected
		// context or trigger another model admission. Worker-owned private
		// state is discarded; durable writers gate publication on this ctx.
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		return outcome.value, outcome.err
	}
}
