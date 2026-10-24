package agent

import (
	"strings"
	"unicode"
)

const semanticWanderingMinimumRunes = 2400

// semanticWanderingTracker detects a response that keeps emitting the same
// reasoning-shaped text without reaching a tool call or answer. It consumes
// only user-visible content deltas, never hidden chain of thought.
type semanticWanderingTracker struct {
	text        []rune
	lastChecked int
}

func newSemanticWanderingTracker() *semanticWanderingTracker {
	return &semanticWanderingTracker{}
}

func (t *semanticWanderingTracker) Observe(delta string) bool {
	if t == nil || delta == "" {
		return false
	}
	t.text = append(t.text, []rune(delta)...)
	if len(t.text) < semanticWanderingMinimumRunes || len(t.text)-t.lastChecked < 320 {
		return false
	}
	t.lastChecked = len(t.text)
	return semanticWanderingDetected(string(t.text))
}

func semanticWanderingDetected(text string) bool {
	normalized := normalizeSemanticWanderingText(text)
	runes := []rune(normalized)
	if len(runes) < semanticWanderingMinimumRunes {
		return false
	}
	const width = 36
	const stride = 9
	if len(runes) < width {
		return false
	}
	counts := make(map[string]int, len(runes)/stride)
	total := 0
	duplicates := 0
	maxCount := 0
	for start := 0; start+width <= len(runes); start += stride {
		shingle := string(runes[start : start+width])
		counts[shingle]++
		count := counts[shingle]
		if count > 1 {
			duplicates++
		}
		if count > maxCount {
			maxCount = count
		}
		total++
	}
	if total == 0 {
		return false
	}
	return maxCount >= 6 && float64(duplicates)/float64(total) >= 0.28
}

func normalizeSemanticWanderingText(text string) string {
	var builder strings.Builder
	builder.Grow(len(text))
	space := false
	for _, r := range strings.ToLower(text) {
		if unicode.IsSpace(r) {
			if !space {
				builder.WriteRune(' ')
				space = true
			}
			continue
		}
		space = false
		builder.WriteRune(r)
	}
	return strings.TrimSpace(builder.String())
}
