package tools

import (
	"context"
	"fmt"
	"time"
)

const directoryIOMaxWorkers = 2
const directoryIOTimeout = 10 * time.Second

// Filesystem calls can block in OS permission handling and have no cancellable
// ReadDir API. Bound the number of outstanding pure-read operations globally.
// A caller can stop waiting; its worker retains the slot until the syscall
// really returns, and cannot publish a late result to that caller.
type directoryIOPool struct {
	slots   chan struct{}
	timeout time.Duration
}

var sharedDirectoryIO = &directoryIOPool{slots: make(chan struct{}, directoryIOMaxWorkers), timeout: directoryIOTimeout}

func (p *directoryIOPool) run(ctx context.Context, path string, read func() Result) Result {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return errResult(fmt.Errorf("directory listing cancelled or timed out: %w", err))
	}
	// Do not start one blocked goroutine per request, or wait for a blocked
	// worker to release its slot. A later request receives an explicit error.
	select {
	case p.slots <- struct{}{}:
	default:
		return errResult(&RecoveryError{Code: "DIRECTORY_IO_BUSY", Path: path,
			Message: "Directory listing is unavailable: both bounded filesystem workers are still waiting for an earlier OS read. No new read was started."})
	}
	if err := ctx.Err(); err != nil {
		<-p.slots
		return errResult(fmt.Errorf("directory listing cancelled or timed out: %w", err))
	}
	observed := make(chan Result, 1)
	go func() {
		defer func() { <-p.slots }()
		observed <- read()
	}()
	select {
	case result := <-observed:
		if err := ctx.Err(); err != nil {
			return errResult(fmt.Errorf("directory listing cancelled or timed out: %w", err))
		}
		return result
	case <-ctx.Done():
		return errResult(fmt.Errorf("directory listing cancelled or timed out: %w", ctx.Err()))
	}
}
