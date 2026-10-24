package serve

import "fmt"

// publicMessageStream is presentation-only, scoped to one serialized run.
// Runtime response boundaries, rather than usage/tool events or text parsing,
// determine message identity. Older runtimes may keep sending ID-less chunks.
type publicMessageStream struct {
	runID    string
	sequence int
	activeID string
	emit     func(map[string]any)
}

func (s *publicMessageStream) boundary(state string) string {
	switch state {
	case "start":
		s.boundary("end")
		s.sequence++
		s.activeID = fmt.Sprintf("%s:assistant:%d", s.runID, s.sequence)
		s.emit(map[string]any{"sessionUpdate": "agent_message_start", "messageId": s.activeID})
	case "end":
		if s.activeID != "" {
			s.emit(map[string]any{"sessionUpdate": "agent_message_end", "messageId": s.activeID})
			s.activeID = ""
		}
	}
	return s.activeID
}

func (s *publicMessageStream) content(delta string) {
	update := map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": delta},
	}
	if s.activeID != "" {
		update["messageId"] = s.activeID
	}
	s.emit(update)
}
