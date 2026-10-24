package deskevo

import "github.com/hey-meadow/meadow-code/go/core/agent"

type guidanceFanout struct {
	targets      map[string]bool
	events       map[string]agent.GuidanceEvent
	receipt      agent.GuidanceReceipt
	emitted      bool
	outcome      agent.GuidanceDisposition
	messageIndex int
}

// One parent Guide may reach two probes. A sibling cancellation must never
// requeue a Guide that another probe has applied. Raw child events stay in
// their stage reports; only one settled lifecycle reaches the parent queue.
func (r *Runtime) stageGuidance(stage string, event agent.GuidanceEvent) {
	r.mu.Lock()
	fanout := r.guidance[event.ID]
	if fanout == nil || !fanout.targets[stage] || fanout.emitted {
		r.mu.Unlock()
		return
	}
	if event.Disposition != agent.GuidanceApplied && event.Disposition != agent.GuidanceCancelled && event.Disposition != agent.GuidanceRejected {
		r.mu.Unlock()
		return
	}
	fanout.events[stage] = event
	if event.Disposition != agent.GuidanceApplied && len(fanout.events) != len(fanout.targets) {
		r.mu.Unlock()
		return
	}
	fanout.emitted = true
	fanout.outcome = event.Disposition
	r.guideEvents = append(r.guideEvents, event)
	r.mu.Unlock()
	if r.config.Dependencies.Events.OnGuidance != nil {
		r.config.Dependencies.Events.OnGuidance(event)
	}
}
