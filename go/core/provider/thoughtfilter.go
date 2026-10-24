package provider

import "strings"

// Gemma-4 thought-channel markers as they appear on the wire. The server
// strips them on the non-streaming path only; streaming deltas leak them,
// so every consumer of onDelta needs this filter.
const (
	thoughtOpen      = "<|channel>"
	thoughtClose     = "<channel|>"
	thoughtHTMLOpen  = "<think>"
	thoughtHTMLClose = "</think>"
)

type thoughtMarkerPair struct {
	open  string
	close string
}

var thoughtMarkerPairs = []thoughtMarkerPair{
	{open: thoughtOpen, close: thoughtClose},
	{open: thoughtHTMLOpen, close: thoughtHTMLClose},
}

// ThoughtStreamFilter splits a streaming delta sequence into answer text
// and thought text (<|channel>…<channel|> spans). Markers can be split
// across deltas, so the filter holds back any tail that could be a partial
// marker until the next delta resolves it. Feed returns the printable
// answer part and the thought part of each delta; Flush returns whatever
// held-back tail turned out to be plain answer text.
type ThoughtStreamFilter struct {
	inThought   bool
	closeMarker string
	pending     string
	// Some OpenAI-compatible LFM2.5 servers prefill the opening <think> token
	// in the chat template and therefore stream only its closing </think>.
	// Hold that leading segment until the close arrives; if it never arrives,
	// Flush releases it as ordinary answer text so a plain completion is safe.
	awaitLeadingHTMLClose bool
}

func (f *ThoughtStreamFilter) Feed(delta string) (answer, thought string) {
	s := f.pending + delta
	f.pending = ""
	var out, th strings.Builder
	if f.awaitLeadingHTMLClose {
		if closeIndex := strings.Index(s, thoughtHTMLClose); closeIndex >= 0 {
			th.WriteString(s[:closeIndex])
			s = s[closeIndex+len(thoughtHTMLClose):]
			f.awaitLeadingHTMLClose = false
		} else {
			f.pending = s
			return "", ""
		}
	}
	for {
		if f.inThought {
			if i := strings.Index(s, f.closeMarker); i >= 0 {
				th.WriteString(s[:i])
				s = s[i+len(f.closeMarker):]
				f.inThought = false
				f.closeMarker = ""
				continue
			}
			keep := partialMarkerTail(s, f.closeMarker)
			th.WriteString(s[:len(s)-len(keep)])
			f.pending = keep
			return out.String(), th.String()
		}
		open, openPair := firstThoughtMarker(s, true)
		stray, strayPair := firstThoughtMarker(s, false)
		// A stray close without an open is wire noise — swallow it.
		if stray >= 0 && (open < 0 || stray < open) {
			out.WriteString(s[:stray])
			s = s[stray+len(strayPair.close):]
			continue
		}
		if open >= 0 {
			out.WriteString(s[:open])
			s = s[open+len(openPair.open):]
			f.inThought = true
			f.closeMarker = openPair.close
			continue
		}
		markers := make([]string, 0, len(thoughtMarkerPairs)*2)
		for _, pair := range thoughtMarkerPairs {
			markers = append(markers, pair.open, pair.close)
		}
		keep := longestPartialTail(s, markers...)
		out.WriteString(s[:len(s)-len(keep)])
		f.pending = keep
		return out.String(), th.String()
	}
}

func firstThoughtMarker(s string, opening bool) (int, thoughtMarkerPair) {
	index := -1
	var selected thoughtMarkerPair
	for _, pair := range thoughtMarkerPairs {
		marker := pair.close
		if opening {
			marker = pair.open
		}
		candidate := strings.Index(s, marker)
		if candidate >= 0 && (index < 0 || candidate < index) {
			index = candidate
			selected = pair
		}
	}
	return index, selected
}

// Flush releases a held-back tail that never completed into a marker.
// Call once when the stream ends.
func (f *ThoughtStreamFilter) Flush() string {
	if f.inThought {
		// Unterminated thought span: drop it, matching StripThoughtChannel.
		f.pending = ""
		f.closeMarker = ""
		return ""
	}
	if f.awaitLeadingHTMLClose {
		out := f.pending
		f.pending = ""
		f.awaitLeadingHTMLClose = false
		return out
	}
	out := f.pending
	f.pending = ""
	return out
}

// partialMarkerTail returns the longest suffix of s that is a proper
// prefix of marker ("" when none).
func partialMarkerTail(s, marker string) string {
	maxN := min(len(s), len(marker)-1)
	for n := maxN; n > 0; n-- {
		if strings.HasSuffix(s, marker[:n]) {
			return s[len(s)-n:]
		}
	}
	return ""
}

func longestPartialTail(s string, markers ...string) string {
	longest := ""
	for _, m := range markers {
		if t := partialMarkerTail(s, m); len(t) > len(longest) {
			longest = t
		}
	}
	return longest
}
