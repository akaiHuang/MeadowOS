package agent

import "strings"

// publicMessageEmitter marks presentation boundaries without changing provider
// text, tool execution or task completion. An end closes one public response;
// it does not mean the user's task has finished. The host owns optional IDs.
type publicMessageEmitter struct {
	onContent  func(string)
	onBoundary func(string) string
	started    bool
	id         string
	text       strings.Builder
}

func (m *publicMessageEmitter) Content(delta string) {
	if delta == "" {
		return
	}
	if !m.started {
		m.started = true
		m.id = ""
		m.text.Reset()
		if m.onBoundary != nil {
			m.id = m.onBoundary("start")
		}
	}
	if m.onBoundary != nil {
		m.text.WriteString(delta)
	}
	if m.onContent != nil {
		m.onContent(delta)
	}
}

func (m *publicMessageEmitter) End() {
	if m.started && m.onBoundary != nil {
		m.onBoundary("end")
	}
	m.started = false
}

// ID remains available after End so the accepted transcript can retain the
// exact identity sent to the UI, outside the provider wire representation.
func (m *publicMessageEmitter) ID() string { return m.id }

// TranscriptContent preserves the bytes sent on this public response channel.
// Provider result normalization must not silently change an already displayed
// acknowledgement on reopen. Legacy callers keep the provider result unchanged.
func (m *publicMessageEmitter) TranscriptContent(fallback string) string {
	if m.onBoundary != nil && m.text.Len() > 0 {
		return m.text.String()
	}
	return fallback
}
