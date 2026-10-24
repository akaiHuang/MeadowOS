package telemetry

import (
	"fmt"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"time"
)

var observedKinds = map[agenttrace.EventType]bool{agenttrace.EventRunStarted: true, agenttrace.EventRunFinished: true, agenttrace.EventTurnStarted: true, agenttrace.EventTurnFinished: true, agenttrace.EventModelRequested: true, agenttrace.EventModelFirstSignal: true, agenttrace.EventModelFinished: true, agenttrace.EventModelFailed: true, agenttrace.EventToolRequested: true, agenttrace.EventToolStarted: true, agenttrace.EventToolFinished: true}

func (q *Queue) Observe(source agenttrace.Event) {
	if q == nil || !observedKinds[source.Type] {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, source.Time)
	if err != nil {
		return
	}
	q.ownerMu.Lock()
	if source.Type == agenttrace.EventRunStarted {
		if len(q.runOwners) >= 1024 {
			q.ownerMu.Unlock()
			q.dropped.Add(1)
			return
		}
		q.runOwners[source.RunID] = q.Principal()
	}
	principal, owned := q.runOwners[source.RunID]
	if source.Type == agenttrace.EventRunFinished {
		delete(q.runOwners, source.RunID)
	}
	q.ownerMu.Unlock()
	if !owned {
		q.dropped.Add(1)
		return
	}
	traceID := ID(source.RunID)[:32]
	root := ID(source.RunID, "run")[:16]
	span := ID(source.RunID, fmt.Sprint(source.Sequence))[:16]
	parent := root
	switch source.Type {
	case agenttrace.EventRunStarted, agenttrace.EventRunFinished:
		span = root
		parent = ""
	case agenttrace.EventTurnStarted, agenttrace.EventTurnFinished:
		span = ID(source.RunID, "turn", fmt.Sprint(source.Turn))[:16]
	case agenttrace.EventToolRequested, agenttrace.EventToolStarted, agenttrace.EventToolFinished:
		if call, ok := source.Fields["call_id"].(string); ok {
			span = ID(source.RunID, "tool", call)[:16]
		}
	}
	metadata := map[string]any{"source": "harness", "runtime": string(source.Runtime), "turn": source.Turn, "sequence": source.Sequence, "elapsed_ms": source.ElapsedMS}
	for key, value := range source.Fields {
		if key == "tool" || key == "name" {
			key = "tool_name"
		}
		if key == "error_class" {
			key = "error_code"
		}
		metadata[key] = value
	}
	event := Event{ID: ID(source.RunID, fmt.Sprint(source.Sequence)), TraceID: traceID, SpanID: span, ParentSpanID: parent, Kind: string(source.Type), At: at, Model: source.Model, Metadata: metadata}
	if source.Type == agenttrace.EventRunFinished {
		event.DurationMS = source.ElapsedMS
	}
	if elapsed, ok := source.Fields["duration_ms"].(int64); ok {
		event.DurationMS = elapsed
	}
	if status, ok := source.Fields["status"].(string); ok {
		switch status {
		case "success", "error", "canceled":
			event.Status = status
		case "ok":
			event.Status = "success"
		case "blocked", "denied", "unknown_tool", "confirmation_required":
			event.Status = "error"
			event.Metadata["error_code"] = status
		}
	}
	q.EmitFor(principal, event)
}
func (q *Queue) ObserveReport(agenttrace.Report) {}

type combinedObserver struct{ local, remote agenttrace.Observer }

func Combine(local, remote agenttrace.Observer) agenttrace.Observer {
	if remote == nil {
		return local
	}
	return combinedObserver{local, remote}
}
func (o combinedObserver) Observe(event agenttrace.Event) {
	if o.local != nil {
		o.local.Observe(event)
	}
	o.remote.Observe(event)
}
func (o combinedObserver) ObserveReport(report agenttrace.Report) {
	if o.local != nil {
		o.local.ObserveReport(report)
	}
	o.remote.ObserveReport(report)
}
