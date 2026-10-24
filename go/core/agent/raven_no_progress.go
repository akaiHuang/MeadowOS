package agent

// Native adaptation of Raven's per-turn no-progress ladder, Apache-2.0.
// Source: EverMind-AI/Raven 3632e604, raven/agent/loop/no_progress.py.
// This compares actual call/result bytes, not task meaning or success claims.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

type ravenNoProgressAction uint8

const (
	ravenProgressRun ravenNoProgressAction = iota
	ravenProgressRefuse
	ravenProgressEnd
)

type ravenProgressFreeze struct{ repeats, novelty int }
type ravenProgressSpend struct{ refusals, establishments int }

func (s ravenProgressSpend) used() int { return s.refusals + max(0, s.establishments-1) }

type ravenNoProgressGuard struct {
	nudgeAt, refuseAt, maxNudges, maxRefusals int
	seen, streak, noveltyAt                   map[[32]byte]int
	nudged                                    map[[32]byte]bool
	frozen                                    map[[32]byte]ravenProgressFreeze
	spend                                     map[[32]byte]ravenProgressSpend
	novelty                                   int
	armedKey                                  [32]byte
	armedTool                                 string
	armedSeen                                 int
}

func newRavenNoProgressGuard() *ravenNoProgressGuard {
	return &ravenNoProgressGuard{
		nudgeAt: 8, refuseAt: 12, maxNudges: 2, maxRefusals: 3,
		seen: map[[32]byte]int{}, streak: map[[32]byte]int{}, noveltyAt: map[[32]byte]int{},
		nudged: map[[32]byte]bool{}, frozen: map[[32]byte]ravenProgressFreeze{}, spend: map[[32]byte]ravenProgressSpend{},
	}
}

// JSON map encoding has deterministic key ordering; include pixels rather than
// only dimensions so changing images cannot be mistaken for an unchanged read.
func ravenProgressHash(tool string, args map[string]any, result string, parts []provider.MessageContentPart, withResult bool) [32]byte {
	a, _ := json.Marshal(args)
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write(a)
	if withResult {
		h.Write([]byte{0})
		h.Write([]byte(result))
		h.Write([]byte{0})
		b, _ := json.Marshal(parts)
		h.Write(b)
	}
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key
}

func (g *ravenNoProgressGuard) Check(tool string, args map[string]any) (ravenNoProgressAction, string) {
	pair := ravenProgressHash(tool, args, "", nil, false)
	f, ok := g.frozen[pair]
	if !ok {
		return ravenProgressRun, ""
	}
	s := g.spend[pair]
	if s.used() >= g.maxRefusals {
		return ravenProgressEnd, fmt.Sprintf("[Raven loop] %s was not run. This exact call exhausted its per-turn no-progress allowance (%d refusals, %d re-establishments after novel results). Preserve the observed work and report the unresolved blocker; no further action is authorized by this observation.", tool, s.refusals, max(0, s.establishments-1))
	}
	if f.novelty != g.novelty {
		delete(g.frozen, pair)
		return ravenProgressRun, ""
	}
	s.refusals++
	g.spend[pair] = s
	return ravenProgressRefuse, fmt.Sprintf("[Raven loop] %s was not run: identical arguments returned identical evidence %d times, with no new result between them. Reuse the observed evidence, ask a different question, or report the blocker. New evidence allows reevaluation; %d no-progress chances remain in this turn.", tool, f.repeats, max(0, g.maxRefusals-s.used()))
}

func (g *ravenNoProgressGuard) Record(tool string, args map[string]any, rawResult string, parts []provider.MessageContentPart) {
	key := ravenProgressHash(tool, args, rawResult, parts, true)
	g.seen[key]++
	if g.seen[key] >= g.nudgeAt && !g.nudged[key] && len(g.nudged) < g.maxNudges {
		g.armedKey, g.armedTool, g.armedSeen = key, tool, g.seen[key]
	}
	lastNovelty, known := g.noveltyAt[key]
	if !known {
		g.novelty++
	}
	if !known || lastNovelty != g.novelty {
		g.streak[key] = 1
	} else {
		g.streak[key]++
	}
	g.noveltyAt[key] = g.novelty
	if g.streak[key] >= g.refuseAt {
		pair := ravenProgressHash(tool, args, "", nil, false)
		if _, frozen := g.frozen[pair]; !frozen {
			s := g.spend[pair]
			s.establishments++
			g.spend[pair] = s
			g.frozen[pair] = ravenProgressFreeze{g.streak[key], g.novelty}
		}
	}
}

func (g *ravenNoProgressGuard) TakeNudge() string {
	if g.armedTool == "" {
		return ""
	}
	text := fmt.Sprintf("[Raven loop] %s returned identical evidence for identical arguments %d times this turn. An unchanged successful read does not advance the task. Check the expectation or use existing evidence; choose useful work within the original budget.", g.armedTool, g.armedSeen)
	g.nudged[g.armedKey] = true
	g.armedTool = ""
	return text
}
