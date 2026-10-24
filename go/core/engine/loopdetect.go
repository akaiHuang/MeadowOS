package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
)

// Thresholds from the TS loopDetectionService.
const (
	toolCallLoopThreshold = 5
	contentChunkSize      = 50
	contentLoopThreshold  = 10
	maxContentHistory     = 1000
	fileReadThreshold     = 8
	fileReadWindow        = 15
	stagnationThreshold   = 8
)

type LoopType = agentcore.LoopType

const (
	LoopIdenticalToolCalls = agentcore.LoopIdenticalToolCalls
	LoopContentChanting    = agentcore.LoopContentChanting
	LoopFileReads          = agentcore.LoopFileReads
	LoopActionStagnation   = agentcore.LoopActionStagnation
)

// loopDetector ports four of the five TS detectors. The repetitive-thoughts
// detector is omitted: the Go client strips the Gemma-4 thought channel
// before content reaches the engine, so there is no thought stream to feed.
type loopDetector struct {
	detected LoopType

	// identical tool calls
	lastCallHash  string
	sameCallCount int

	// content chanting
	contentHistory string
	chunkIndices   map[string][]int
	inCodeBlock    bool

	// file-read loop + stagnation
	recentToolNames    []string
	hasSeenNonReadTool bool
	lastToolName       string
	sameNameStreak     int
}

func newLoopDetector() *loopDetector {
	return &loopDetector{chunkIndices: map[string][]int{}}
}

func (d *loopDetector) Detected() LoopType { return d.detected }

// AddToolCall feeds one model-requested call; returns the loop type when a
// detector fires. Tool calls count as progress, so content tracking resets.
func (d *loopDetector) AddToolCall(name, rawArgs string) LoopType {
	if d.detected != "" {
		return d.detected
	}
	d.resetContent()
	d.inCodeBlock = false

	// 1. consecutive identical calls (name + exact args)
	sum := sha256.Sum256([]byte(name + ":" + rawArgs))
	h := hex.EncodeToString(sum[:])
	if h == d.lastCallHash {
		d.sameCallCount++
	} else {
		d.lastCallHash = h
		d.sameCallCount = 1
	}
	if d.sameCallCount >= toolCallLoopThreshold {
		d.detected = LoopIdenticalToolCalls
		return d.detected
	}

	// 4. file-read loop, with the TS cold-start exemption: until any
	// non-read tool fires, reads are exploration.
	readLike := name == "read_file" || name == "list_directory" ||
		strings.HasPrefix(name, "read_") || strings.HasPrefix(name, "list_")
	if !readLike {
		d.hasSeenNonReadTool = true
	}
	d.recentToolNames = append(d.recentToolNames, name)
	if len(d.recentToolNames) > fileReadWindow {
		d.recentToolNames = d.recentToolNames[len(d.recentToolNames)-fileReadWindow:]
	}
	if d.hasSeenNonReadTool {
		reads := 0
		for _, n := range d.recentToolNames {
			if n == "read_file" || n == "list_directory" ||
				strings.HasPrefix(n, "read_") || strings.HasPrefix(n, "list_") {
				reads++
			}
		}
		if reads >= fileReadThreshold {
			d.detected = LoopFileReads
			return d.detected
		}
	}

	// 5. action stagnation: same tool name regardless of args.
	if name == d.lastToolName {
		d.sameNameStreak++
	} else {
		d.lastToolName = name
		d.sameNameStreak = 1
	}
	if d.sameNameStreak >= stagnationThreshold {
		d.detected = LoopActionStagnation
		return d.detected
	}
	return ""
}

// AddContent feeds streamed text; sliding-window chunk hashing catches
// chanting. Code blocks are exempt (the TS rule) because repeated code
// patterns are legitimate.
func (d *loopDetector) AddContent(delta string) LoopType {
	if d.detected != "" {
		return d.detected
	}
	if strings.Contains(delta, "```") {
		d.inCodeBlock = !d.inCodeBlock
		d.resetContent()
		return ""
	}
	if d.inCodeBlock {
		return ""
	}
	d.contentHistory += delta
	if len(d.contentHistory) > maxContentHistory*2 {
		// Keep the tail; indices shift, so restart tracking cleanly.
		d.contentHistory = d.contentHistory[len(d.contentHistory)-maxContentHistory:]
		d.chunkIndices = map[string][]int{}
	}
	for i := 0; i+contentChunkSize <= len(d.contentHistory); i++ {
		chunk := d.contentHistory[i : i+contentChunkSize]
		sum := sha256.Sum256([]byte(chunk))
		h := hex.EncodeToString(sum[:8])
		idx := d.chunkIndices[h]
		if len(idx) > 0 && idx[len(idx)-1] >= i {
			continue // already recorded this window position
		}
		idx = append(idx, i)
		d.chunkIndices[h] = idx
		if len(idx) >= contentLoopThreshold {
			recent := idx[len(idx)-contentLoopThreshold:]
			span := recent[len(recent)-1] - recent[0]
			avg := float64(span) / float64(contentLoopThreshold-1)
			if avg <= 1.5*float64(contentChunkSize) {
				d.detected = LoopContentChanting
				return d.detected
			}
		}
	}
	return ""
}

// resetContent clears chant tracking without touching the code-block flag —
// the ``` toggle in AddContent must survive its own reset.
func (d *loopDetector) resetContent() {
	d.contentHistory = ""
	d.chunkIndices = map[string][]int{}
}
