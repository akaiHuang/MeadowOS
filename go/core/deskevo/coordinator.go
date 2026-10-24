package deskevo

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const nativeStageConcurrency = 2

type coordinatorFrame struct {
	Method                                        string `json:"method"`
	ID                                            int    `json:"id"`
	Root, Workspace, Task, Mode, Harness, Harvest string
	Seconds                                       int
	Files                                         map[string]string
	Result                                        json.RawMessage
	Error                                         string
}

// serveCoordinator keeps reading while native stages run. Only the official
// coordinator owns dependencies; this bridge limits independent stages to two.
// stop cancels the owned worker and all stage contexts, not the parent session.
func serveCoordinator(ctx context.Context, input io.Writer, output io.Reader, stop func(), admit func(coordinatorFrame) error, handle func(coordinatorFrame) (map[string]any, error)) (result json.RawMessage, runErr error) {
	var mu sync.Mutex
	var workers sync.WaitGroup
	pending := map[int]string{}
	var fatal error
	encoder := json.NewEncoder(input)
	capacity := make(chan struct{}, nativeStageConcurrency)
	abort := func(err error) {
		mu.Lock()
		if fatal == nil {
			fatal = err
		}
		mu.Unlock()
		stop()
	}
	defer func() {
		// Preserve a protocol failure before cancellation makes an in-flight
		// reply fail with a secondary broken-pipe error.
		mu.Lock()
		if fatal == nil && runErr != nil {
			fatal = runErr
		}
		mu.Unlock()
		if runErr != nil {
			stop()
		}
		workers.Wait()
		mu.Lock()
		defer mu.Unlock()
		if fatal != nil {
			runErr = fatal
		}
		if ctx.Err() != nil {
			runErr = ctx.Err()
		}
	}()
	respond := func(frame coordinatorFrame) {
		defer workers.Done()
		defer func() { <-capacity }()
		reply, err := handle(frame)
		if err != nil {
			abort(err)
			return
		}
		mu.Lock()
		delete(pending, frame.ID)
		if fatal == nil {
			err = encoder.Encode(reply)
		}
		mu.Unlock()
		if err != nil {
			abort(err)
		}
	}
	scan := bufio.NewScanner(output)
	scan.Buffer(make([]byte, 64*1024), 128*1024*1024)
	terminal := false
	for scan.Scan() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		mu.Lock()
		previousFailure := fatal
		mu.Unlock()
		if previousFailure != nil {
			return nil, previousFailure
		}
		if terminal {
			return nil, errors.New("coordinator emitted operations after completion")
		}
		var frame coordinatorFrame
		if err := json.Unmarshal(scan.Bytes(), &frame); err != nil {
			return nil, errors.New("invalid native coordinator frame")
		}
		switch frame.Method {
		case "stage", "validate":
			if frame.ID < 1 {
				return nil, errors.New("invalid native coordinator request identity")
			}
			if err := admit(frame); err != nil {
				return nil, err
			}
			mu.Lock()
			_, duplicate := pending[frame.ID]
			overlap := false
			if frame.Method == "stage" {
				for _, root := range pending {
					if root != "" && (root == frame.Root || childPath(root, frame.Root) || childPath(frame.Root, root)) {
						overlap = true
					}
				}
			}
			if duplicate || overlap {
				mu.Unlock()
				return nil, errors.New("native coordinator reused an active request identity or stage root")
			}
			pending[frame.ID] = frame.Root
			mu.Unlock()
			if frame.Method == "validate" {
				reply, err := handle(frame)
				if err != nil {
					return nil, err
				}
				mu.Lock()
				delete(pending, frame.ID)
				err = encoder.Encode(reply)
				mu.Unlock()
				if err != nil {
					return nil, err
				}
				continue
			}
			select {
			case capacity <- struct{}{}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			mu.Lock()
			previousFailure = fatal
			mu.Unlock()
			if previousFailure != nil {
				<-capacity
				return nil, previousFailure
			}
			workers.Add(1)
			go respond(frame)
		case "done":
			mu.Lock()
			unfinished := len(pending) != 0
			mu.Unlock()
			if unfinished {
				return nil, errors.New("coordinator completed with pending native operations")
			}
			terminal, result = true, frame.Result
		case "failed":
			return nil, fmt.Errorf("Desktop Evo coordinator: %s", frame.Error)
		default:
			return nil, errors.New("unknown native coordinator operation")
		}
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	if !terminal {
		return nil, errors.New("coordinator disconnected before completion")
	}
	return result, nil
}
