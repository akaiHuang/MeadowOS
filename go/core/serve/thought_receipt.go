package serve

import (
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/session"
)

// thoughtReceipt persists only the provider bytes actually delivered to
// OnThought. It is a display journal, never a provider message or checkpoint
// input. Batching avoids adding an fsync to every streamed token.
type thoughtReceipt struct {
	runID, segmentID string
	text             strings.Builder
	sequence         int
	persistedBytes   int
	lastWrite        time.Time
	state            string
	write            func(session.DisplayThought) error
}

func newThoughtReceipt(runID string, write func(session.DisplayThought) error) *thoughtReceipt {
	if runID == "" {
		runID = session.NewUUID()
	}
	return &thoughtReceipt{runID: runID, segmentID: runID + ":thought-1", write: write}
}

func (r *thoughtReceipt) append(delta string) error {
	if delta == "" {
		return nil
	}
	r.text.WriteString(delta)
	r.sequence++
	if r.persistedBytes == 0 || r.text.Len()-r.persistedBytes >= 4096 || time.Since(r.lastWrite) >= 250*time.Millisecond {
		return r.flush("streaming")
	}
	return nil
}

func (r *thoughtReceipt) flush(state string) error {
	if r.text.Len() == 0 || (r.persistedBytes == r.text.Len() && r.state == state) {
		return nil
	}
	if err := r.write(session.DisplayThought{
		Text: r.text.String(), Observed: true, State: state,
		RunID: r.runID, SegmentID: r.segmentID, Sequence: r.sequence,
	}); err != nil {
		return err
	}
	r.persistedBytes, r.state, r.lastWrite = r.text.Len(), state, time.Now()
	return nil
}

func thoughtTerminalState(stop string) string {
	switch stop {
	case "end_turn":
		return "complete"
	case "cancelled":
		return "stopped"
	default:
		return "incomplete"
	}
}

// A disk snapshot marked streaming may have survived a process crash. Only a
// matching live run proves that receiving reasoning is still in progress.
func restoreLiveThoughtState(messages []session.DisplayMessage, runID string, running bool) {
	if !running || runID == "" {
		return
	}
	for i := range messages {
		thought := messages[i].Thought
		if thought != nil && thought.RunID == runID && thought.State == "incomplete" {
			thought.State = "streaming"
		}
	}
}
