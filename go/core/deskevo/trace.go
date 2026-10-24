package deskevo

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
)

// taskTrace is an append-only metadata stream, separate from the protocol and
// model transcript. Failure to export observability never blocks inference.
type taskTrace struct {
	mu                       sync.Mutex
	file                     *os.File
	err                      error
	taskID, sessionID, model string
	seq                      int
}

func (t *taskTrace) record(kind, stage string, fields map[string]any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return
	}
	t.seq++
	v := map[string]any{"schema": "meadow.evo.timeline.v1", "sequence": t.seq, "task_id": t.taskID, "session_id": t.sessionID, "model": t.model, "kind": kind, "stage": stage, "time": time.Now().UTC().Format(time.RFC3339Nano), "fields": fields}
	b, e := json.Marshal(v)
	if e == nil {
		_, e = t.file.Write(append(b, '\n'))
	}
	t.err = e
}

type stageObserver struct {
	timeline *taskTrace
	stage    string
	next     agenttrace.Observer
}

func (o *stageObserver) Observe(e agenttrace.Event) {
	if e.Type == agenttrace.EventRunStarted {
		o.timeline.record("agent.started", o.stage, map[string]any{"native_run_id": e.RunID})
	}
	if o.next != nil {
		o.next.Observe(e)
	}
}
func (o *stageObserver) ObserveReport(r agenttrace.Report) {
	o.timeline.record("agent.ended", o.stage, map[string]any{"native_run_id": r.RunID, "stop_reason": r.StopReason})
	if o.next != nil {
		o.next.ObserveReport(r)
	}
}
