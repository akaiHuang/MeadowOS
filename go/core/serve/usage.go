// usage.go — cross-session token/time accounting, aggregated per workspace
// cwd (Meadow extension to the serve contract). Every turn already emits a
// "usage" SSE frame from runPrompt's OnTurn hook; here we also fold that
// turn's counts into a package-level per-cwd tally so the desktop can show
// lifetime totals in the settings modal and a per-project line in the
// sidebar.
//
// Endpoints (both additive, never breaking the TS contract):
//
//	GET /usage?cwd=...  → one cwd's tally
//	GET /usage          → {projects:[…all cwds…], totals:{…}}
//
// Persistence: the tally is mirrored to <PrimaryDir>/usage.json best-effort
// — loaded once at process start, rewritten after every turn — so totals
// survive a daemon restart instead of resetting to zero. Persistence errors
// never affect a prompt or an HTTP response.
package serve

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

// cwdUsage is one workspace's lifetime tally. JSON tags are the on-disk and
// wire shape (camelCase, matching the rest of the serve contract). elapsedMs
// is the summed wall time of every turn fed in via addTurn.
type cwdUsage struct {
	CWD              string `json:"cwd"`
	TotalTokens      int64  `json:"totalTokens"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
	// Estimated* is the legacy portion reconstructed from chats whose provider
	// did not emit usage. It is included in the totals above and exposed so the
	// UI can mark the value as approximate instead of presenting it as exact.
	EstimatedTokens           int64 `json:"estimatedTokens,omitempty"`
	EstimatedPromptTokens     int64 `json:"estimatedPromptTokens,omitempty"`
	EstimatedCompletionTokens int64 `json:"estimatedCompletionTokens,omitempty"`
	EstimatedTurns            int64 `json:"estimatedTurns,omitempty"`
	Turns                     int64 `json:"turns"`
	ElapsedMs                 int64 `json:"elapsedMs"`
	// FirstAt/LastAt are RFC3339 stamps; empty until the first turn lands.
	FirstAt string `json:"firstAt,omitempty"`
	LastAt  string `json:"lastAt,omitempty"`
}

// usageStore is the process-wide accumulator. The map is keyed by cwd; the
// sessions count is tracked separately (distinct session ids seen per cwd)
// because one cwd hosts many threads over its lifetime. Guarded by mu for
// concurrent turns across sessions.
type usageStore struct {
	mu                  sync.Mutex
	byCwd               map[string]*cwdUsage
	sessions            map[string]map[string]struct{} // cwd -> set of session ids
	loaded              bool
	backfillFingerprint map[string]string
}

var usage = &usageStore{
	byCwd:               map[string]*cwdUsage{},
	sessions:            map[string]map[string]struct{}{},
	backfillFingerprint: map[string]string{},
}

// usageFilePath is the persistence target under the primary home dir. It
// honours MEADOW_HOME (tests point that at a TempDir) via session.PrimaryDir.
func usageFilePath() string {
	return filepath.Join(session.PrimaryDir(), "usage.json")
}

// usageDiskShape is the JSON document written to usage.json: the per-cwd
// tallies plus the session-id sets so a restart keeps an accurate distinct
// session count instead of recounting from zero.
type usageDiskShape struct {
	Projects []*cwdUsage         `json:"projects"`
	Sessions map[string][]string `json:"sessions,omitempty"`
}

// load reads usage.json once (idempotent). Best-effort: a missing or corrupt
// file simply starts the tally empty.
func (u *usageStore) load() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.loaded {
		return
	}
	u.loaded = true
	b, err := os.ReadFile(usageFilePath())
	if err != nil {
		return
	}
	var doc usageDiskShape
	if json.Unmarshal(b, &doc) != nil {
		return
	}
	for _, p := range doc.Projects {
		if p == nil || p.CWD == "" {
			continue
		}
		cp := *p
		u.byCwd[p.CWD] = &cp
	}
	for cwd, ids := range doc.Sessions {
		set := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			set[id] = struct{}{}
		}
		u.sessions[cwd] = set
	}
}

// snapshotLocked builds the on-disk document. Caller holds u.mu.
func (u *usageStore) snapshotLocked() usageDiskShape {
	doc := usageDiskShape{
		Projects: make([]*cwdUsage, 0, len(u.byCwd)),
		Sessions: make(map[string][]string, len(u.sessions)),
	}
	for _, c := range u.byCwd {
		cp := *c
		doc.Projects = append(doc.Projects, &cp)
	}
	for cwd, set := range u.sessions {
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		doc.Sessions[cwd] = ids
	}
	return doc
}

// persist writes the current tally to disk best-effort via a temp-file
// rename so a crash mid-write can't truncate the file. Snapshot is taken
// under the lock; the file I/O runs unlocked so a slow disk never blocks a
// turn.
func (u *usageStore) persist() {
	u.mu.Lock()
	doc := u.snapshotLocked()
	u.mu.Unlock()

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	path := usageFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// addTurn folds one completed turn into its cwd's tally and persists. usage
// may be nil (the model reported no counts) — we still count the turn, its
// time, and the session so "turns" and elapsed stay honest. elapsed is that
// turn's measured wall time.
func (u *usageStore) addTurn(cwd, sessionID string, usg *provider.Usage, elapsed time.Duration) {
	if cwd == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	u.mu.Lock()
	c := u.byCwd[cwd]
	if c == nil {
		c = &cwdUsage{CWD: cwd, FirstAt: now}
		u.byCwd[cwd] = c
	}
	if c.FirstAt == "" {
		c.FirstAt = now
	}
	if usg != nil {
		c.TotalTokens += int64(usg.TotalTokens)
		c.PromptTokens += int64(usg.PromptTokens)
		c.CompletionTokens += int64(usg.CompletionTokens)
	}
	c.Turns++
	if ms := elapsed.Milliseconds(); ms > 0 {
		c.ElapsedMs += ms
	}
	c.LastAt = now
	if sessionID != "" {
		set := u.sessions[cwd]
		if set == nil {
			set = map[string]struct{}{}
			u.sessions[cwd] = set
		}
		set[sessionID] = struct{}{}
	}
	u.mu.Unlock()

	u.persist()
}

// backfillProject repairs the historical "0 tokens · N turns" state left by
// providers that streamed text but omitted usage. File metadata prevents a
// deep scan on Desktop's five-second refresh loop. Real stored usage is used
// only when no aggregate exists; otherwise only missing-usage estimates are
// added, preventing double counting with usage.json.
func (u *usageStore) backfillProject(cwd string) {
	if cwd == "" {
		return
	}
	fingerprint, err := session.ChatUsageFingerprint(cwd)
	if err != nil || fingerprint == "" {
		return
	}
	u.mu.Lock()
	if u.backfillFingerprint == nil {
		u.backfillFingerprint = map[string]string{}
	}
	if u.backfillFingerprint[cwd] == fingerprint {
		u.mu.Unlock()
		return
	}
	u.mu.Unlock()

	estimate, err := session.EstimateProjectUsage(cwd)
	if err != nil {
		return
	}
	u.mu.Lock()
	c := u.byCwd[cwd]
	if c == nil {
		c = &cwdUsage{CWD: cwd}
		u.byCwd[cwd] = c
	}
	if c.TotalTokens == 0 {
		c.PromptTokens = int64(estimate.Reported.PromptTokens)
		c.CompletionTokens = int64(estimate.Reported.CompletionTokens)
		c.TotalTokens = int64(estimate.Reported.TotalTokens)
	}
	estimatedPrompt := int64(estimate.Estimated.PromptTokens)
	estimatedCompletion := int64(estimate.Estimated.CompletionTokens)
	estimatedTotal := int64(estimate.Estimated.TotalTokens)
	c.PromptTokens += estimatedPrompt - c.EstimatedPromptTokens
	c.CompletionTokens += estimatedCompletion - c.EstimatedCompletionTokens
	c.TotalTokens += estimatedTotal - c.EstimatedTokens
	c.EstimatedPromptTokens = estimatedPrompt
	c.EstimatedCompletionTokens = estimatedCompletion
	c.EstimatedTokens = estimatedTotal
	c.EstimatedTurns = estimate.EstimatedTurns
	allTurns := estimate.ReportedTurns + estimate.EstimatedTurns
	if c.Turns < allTurns {
		c.Turns = allTurns
	}
	set := u.sessions[cwd]
	if set == nil {
		set = map[string]struct{}{}
		u.sessions[cwd] = set
	}
	for _, id := range estimate.SessionIDs {
		set[id] = struct{}{}
	}
	u.backfillFingerprint[cwd] = fingerprint
	u.mu.Unlock()
	u.persist()
}

// view is the wire shape for GET /usage?cwd= — a cwdUsage plus the distinct
// session count, which lives outside the struct so the on-disk struct stays
// a pure tally.
type usageView struct {
	cwdUsage
	Sessions int `json:"sessions"`
}

// viewFor returns the tally for one cwd (zero-valued if never seen).
func (u *usageStore) viewFor(cwd string) usageView {
	u.mu.Lock()
	defer u.mu.Unlock()
	v := usageView{cwdUsage: cwdUsage{CWD: cwd}, Sessions: len(u.sessions[cwd])}
	if c := u.byCwd[cwd]; c != nil {
		v.cwdUsage = *c
	}
	return v
}

// all returns every cwd's view plus a combined total, for GET /usage.
func (u *usageStore) all() ([]usageView, usageView) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]usageView, 0, len(u.byCwd))
	total := usageView{cwdUsage: cwdUsage{CWD: ""}}
	for cwd, c := range u.byCwd {
		v := usageView{cwdUsage: *c, Sessions: len(u.sessions[cwd])}
		out = append(out, v)
		total.TotalTokens += c.TotalTokens
		total.PromptTokens += c.PromptTokens
		total.CompletionTokens += c.CompletionTokens
		total.Turns += c.Turns
		total.ElapsedMs += c.ElapsedMs
		total.Sessions += v.Sessions
		if total.FirstAt == "" || (c.FirstAt != "" && c.FirstAt < total.FirstAt) {
			total.FirstAt = c.FirstAt
		}
		if c.LastAt > total.LastAt {
			total.LastAt = c.LastAt
		}
	}
	return out, total
}

// handleUsage serves both GET /usage and GET /usage?cwd=... The scoped form
// returns a single tally; the unscoped form returns the full project list
// plus a totals roll-up.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	usage.load()
	if cwd := r.URL.Query().Get("cwd"); cwd != "" {
		usage.backfillProject(cwd)
		writeJSON(w, usage.viewFor(cwd))
		return
	}
	projects, totals := usage.all()
	writeJSON(w, map[string]any{"projects": projects, "totals": totals})
}
