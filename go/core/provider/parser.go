package provider

import (
	"encoding/json"
	"sort"
	"strings"
)

// StreamingToolCallParser is a faithful port of
// packages/core/src/core/openaiContentGenerator/streamingToolCallParser.ts.
//
// It reassembles tool-call argument JSON that arrives as inconsistent
// streaming chunks: empty strings, partial fragments, cumulative replays,
// missing IDs, and colliding indices. Parsing is attempted only when the
// JSON nesting depth returns to zero; unclosed strings are auto-closed as a
// repair of truncated output.
type StreamingToolCallParser struct {
	buffers       map[int]string
	depths        map[int]int
	inStrings     map[int]bool
	escapes       map[int]bool
	meta          map[int]*toolCallMeta
	idToIndex     map[string]int
	order         []int // insertion order of indices, mirrors JS Map iteration
	nextAvailable int
}

type toolCallMeta struct {
	ID   string
	Name string
}

// ParseResult mirrors ToolCallParseResult in the TS implementation.
type ParseResult struct {
	Complete bool
	Value    map[string]any
	Err      error
	Repaired bool
}

// CompletedToolCall is one fully reassembled tool call.
type CompletedToolCall struct {
	ID    string
	Name  string
	Args  map[string]any
	Index int
}

func NewStreamingToolCallParser() *StreamingToolCallParser {
	p := &StreamingToolCallParser{}
	p.Reset()
	return p
}

func (p *StreamingToolCallParser) ensure(index int) {
	if _, ok := p.buffers[index]; !ok {
		p.buffers[index] = ""
		p.depths[index] = 0
		p.inStrings[index] = false
		p.escapes[index] = false
		p.meta[index] = &toolCallMeta{}
		p.order = append(p.order, index)
	}
}

// AddChunk processes one streaming delta for a tool call. id and name are
// optional ("" when absent in the chunk).
func (p *StreamingToolCallParser) AddChunk(index int, chunk, id, name string) ParseResult {
	actual := index

	if id != "" {
		if mapped, ok := p.idToIndex[id]; ok {
			actual = mapped
		} else {
			if buf, ok := p.buffers[index]; ok {
				m := p.meta[index]
				if strings.TrimSpace(buf) != "" && p.depths[index] == 0 &&
					m != nil && m.ID != "" && m.ID != id && jsonValid(buf) {
					// Complete tool call with a different ID already lives at
					// this index — relocate the new call.
					actual = p.findNextAvailableIndex()
				}
			}
			p.idToIndex[id] = actual
		}
	} else {
		// Continuation chunk without an ID: route to the incomplete call.
		if buf, ok := p.buffers[index]; ok {
			if p.depths[index] > 0 || strings.TrimSpace(buf) == "" {
				actual = index
			} else if jsonValid(buf) {
				actual = p.findMostRecentIncompleteIndex()
			} else {
				actual = index
			}
		}
	}

	p.ensure(actual)

	m := p.meta[actual]
	if id != "" {
		m.ID = id
	}
	if name != "" {
		m.Name = name
	}

	curBuffer := p.buffers[actual]
	depth := p.depths[actual]
	inString := p.inStrings[actual]
	escape := p.escapes[actual]

	curTrimmed := strings.TrimSpace(curBuffer)
	chunkTrimmed := strings.TrimSpace(chunk)
	newBuffer := curBuffer

	if chunk != "" {
		switch {
		case curTrimmed != "" && chunkTrimmed != "" &&
			strings.HasPrefix(chunk, curBuffer) && len(chunk) > len(curBuffer):
			// Some OpenAI-compatible servers stream arguments cumulatively
			// instead of incrementally — replace, don't concatenate.
			newBuffer = chunk
			depth, inString, escape = scanJSONState(newBuffer, 0, false, false)
		case curTrimmed != "" && chunk == curBuffer:
			// Exact cumulative replay — keep existing state.
		default:
			newBuffer = curBuffer + chunk
			depth, inString, escape = scanJSONState(chunk, depth, inString, escape)
		}
	}

	p.buffers[actual] = newBuffer
	p.depths[actual] = depth
	p.inStrings[actual] = inString
	p.escapes[actual] = escape

	if depth == 0 && strings.TrimSpace(newBuffer) != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(newBuffer), &parsed); err == nil {
			return ParseResult{Complete: true, Value: parsed}
		} else {
			if inString {
				var repaired map[string]any
				if rerr := json.Unmarshal([]byte(newBuffer+`"`), &repaired); rerr == nil {
					return ParseResult{Complete: true, Value: repaired, Repaired: true}
				}
			}
			return ParseResult{Complete: false, Err: err}
		}
	}

	return ParseResult{Complete: false}
}

// scanJSONState walks text updating depth / in-string / escape state.
// Character handling order matches the TS scanJsonState exactly: depth is
// counted with the pre-toggle inString value, the quote toggles after, and
// escape reflects whether the *next* character is escaped.
func scanJSONState(text string, depth int, inString, escape bool) (int, bool, bool) {
	for _, ch := range text {
		if !inString {
			switch ch {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if ch == '"' && !escape {
			inString = !inString
		}
		escape = ch == '\\' && !escape
	}
	return depth, inString, escape
}

func jsonValid(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

// GetCompletedToolCalls returns every buffered call that has a name and a
// non-empty buffer, repairing truncated JSON when needed. Call after the
// stream finishes.
func (p *StreamingToolCallParser) GetCompletedToolCalls() []CompletedToolCall {
	var out []CompletedToolCall
	for _, index := range p.orderedIndices() {
		buf := p.buffers[index]
		m := p.meta[index]
		if m == nil || m.Name == "" || strings.TrimSpace(buf) == "" {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(buf), &args); err != nil {
			repaired := false
			if p.inStrings[index] {
				if rerr := json.Unmarshal([]byte(buf+`"`), &args); rerr == nil {
					repaired = true
				}
			}
			if !repaired {
				args = bestEffortParseObject(buf)
			}
		}
		out = append(out, CompletedToolCall{ID: m.ID, Name: m.Name, Args: args, Index: index})
	}
	return out
}

func (p *StreamingToolCallParser) HasBufferedToolCalls() bool {
	for index, buf := range p.buffers {
		m := p.meta[index]
		if m != nil && m.Name != "" && strings.TrimSpace(buf) != "" {
			return true
		}
	}
	return false
}

// UnresolvedMetadata reports native fragments that cannot name an executable
// call at the terminal boundary. Check only after reassembly: arguments may
// legally precede a later name/ID. A complete JSON object without a name is
// still incomplete protocol, not an ordinary text answer. Never infer a tool
// from its arguments or merge a negative-index orphan into another call.
func (p *StreamingToolCallParser) UnresolvedMetadata() (name string, argumentBytes int, unresolved bool) {
	for _, index := range p.orderedIndices() {
		meta, buffer := p.meta[index], p.buffers[index]
		if index < 0 || meta == nil || strings.TrimSpace(meta.Name) == "" {
			return "", len(buffer), true
		}
		if strings.TrimSpace(buffer) == "" {
			return meta.Name, 0, true
		}
	}
	return "", 0, false
}

// HasIncompleteToolCalls reports whether any named buffer ended mid-JSON
// (unclosed braces or string) — the truncation signal finish_reason may lie
// about.
func (p *StreamingToolCallParser) HasIncompleteToolCalls() bool {
	for index := range p.buffers {
		m := p.meta[index]
		if m == nil || m.Name == "" {
			continue
		}
		if p.depths[index] > 0 || p.inStrings[index] {
			return true
		}
	}
	return false
}

func (p *StreamingToolCallParser) findNextAvailableIndex() int {
	for {
		buf, ok := p.buffers[p.nextAvailable]
		if !ok {
			n := p.nextAvailable
			p.nextAvailable++
			return n
		}
		m := p.meta[p.nextAvailable]
		if strings.TrimSpace(buf) == "" || p.depths[p.nextAvailable] > 0 || m == nil || m.ID == "" {
			return p.nextAvailable
		}
		if !jsonValid(buf) {
			return p.nextAvailable
		}
		if p.depths[p.nextAvailable] == 0 {
			p.nextAvailable++
			continue
		}
		p.nextAvailable++
	}
}

func (p *StreamingToolCallParser) findMostRecentIncompleteIndex() int {
	maxIndex := -1
	for index, buf := range p.buffers {
		m := p.meta[index]
		if m != nil && m.ID != "" && (p.depths[index] > 0 || strings.TrimSpace(buf) == "") {
			if index > maxIndex {
				maxIndex = index
			}
		} else if strings.TrimSpace(buf) != "" && !jsonValid(buf) {
			if index > maxIndex {
				maxIndex = index
			}
		}
	}
	if maxIndex >= 0 {
		return maxIndex
	}
	return p.findNextAvailableIndex()
}

func (p *StreamingToolCallParser) orderedIndices() []int {
	out := make([]int, len(p.order))
	copy(out, p.order)
	sort.Ints(out)
	return out
}

func (p *StreamingToolCallParser) Reset() {
	p.buffers = map[int]string{}
	p.depths = map[int]int{}
	p.inStrings = map[int]bool{}
	p.escapes = map[int]bool{}
	p.meta = map[int]*toolCallMeta{}
	p.idToIndex = map[string]int{}
	p.order = nil
	p.nextAvailable = 0
}

func (p *StreamingToolCallParser) GetBuffer(index int) string {
	return p.buffers[index]
}

// bestEffortParseObject is the safeJsonParse equivalent: try strict, then a
// truncation-oriented repair (close open string, strip dangling comma, close
// open braces/brackets), then give up with an empty object.
func bestEffortParseObject(s string) map[string]any {
	var v map[string]any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	depth, inString, _ := scanJSONState(s, 0, false, false)
	repaired := s
	if inString {
		repaired += `"`
	}
	repaired = strings.TrimRight(repaired, " \t\r\n")
	repaired = strings.TrimSuffix(repaired, ",")
	for i := 0; i < depth; i++ {
		// Closing with '}' covers the dominant truncation case: arguments
		// objects. Mixed array nesting that this gets wrong still falls
		// through to the empty-object fallback below.
		repaired += "}"
	}
	if json.Unmarshal([]byte(repaired), &v) == nil {
		return v
	}
	return map[string]any{}
}
