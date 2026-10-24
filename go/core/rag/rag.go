// Package rag does prefill-stage template retrieval: once the intent router
// classifies a "create" task, the engine retrieves a proven working example
// of that artifact and injects it into the action prompt as a reference the
// model adapts (not copies). This attacks the one limit the deterministic
// harness cannot — generation quality — by handing a weaker local model the
// structural knowledge it lacks (a complete, internally-consistent example),
// the single most reliable way to lift output quality without retraining.
//
// Templates are JSON: built-in ones ship embedded; user/flywheel ones load
// from ~/.meadow/templates so every successful generation can become a
// future retrieval.
package rag

import (
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed templates/*.json templates/*.html
var builtin embed.FS

//go:embed corpus/*.json
var corpusFS embed.FS

//go:embed profiles/*.json
var profileFS embed.FS

// Template is a proven artifact plus the metadata used to retrieve it. It comes
// in two flavors: an EXAMPLE template carries Content (a complete working
// artifact, e.g. a snake HTML the model adapts structurally), while a KNOWLEDGE
// template carries Execution + Rubric (the distilled "how to do it" + quality
// bar for an office/engineering/3D task — domain knowledge the local model
// lacks, not a thing to copy). Inject formats each flavor differently.
type Template struct {
	ID       string   `json:"id,omitempty"`
	Profile  string   `json:"profile,omitempty"`
	Phases   []string `json:"phases,omitempty"`
	Type     string   `json:"type"`     // matches a router intent id (e.g. static_web_artifact)
	Title    string   `json:"title"`    // human label
	Keywords []string `json:"keywords"` // request terms that should surface this
	Source   string   `json:"source,omitempty"`
	Version  string   `json:"version,omitempty"`
	Updated  string   `json:"updated_at,omitempty"`
	Content  string   `json:"content"` // EXAMPLE flavor: the working artifact
	// ContentFile keeps large examples reviewable and syntax-testable. Embedded
	// template JSON may point to a sidecar in the same templates directory.
	ContentFile string `json:"content_file,omitempty"`
	// KNOWLEDGE flavor (corpus entries): execution guidance + the quality bar a
	// good output must satisfy. Present instead of Content.
	Execution string   `json:"execution,omitempty"`
	Rubric    []string `json:"quality_rubric,omitempty"`
}

// Store holds the loaded templates.
type Store struct {
	templates []Template
}

// Match retains retrieval evidence separately from model generation so a
// failed task can be assigned to retrieval or fusion rather than guessed at.
type Match struct {
	Template     *Template `json:"-"`
	Score        int       `json:"score"`
	KeywordHits  int       `json:"keyword_hits"`
	IntentMatch  bool      `json:"intent_match"`
	ProfileMatch bool      `json:"profile_match"`
	PhaseMatch   bool      `json:"phase_match"`
}

// SearchRequest scopes retrieval without changing the generic Store.Search
// contract. Profile-only procedure cards are invisible unless the caller opts
// into that exact harness profile, and recovery-only cards are invisible on
// the initial turn.
type SearchRequest struct {
	Text string
	// PriorityText contains observed lifecycle and failure signals. Matching
	// terms remain part of normal lexical retrieval, but receive an additional
	// ranking bonus so a long task specification cannot drown out the procedure
	// required by the current recovery state.
	PriorityText string
	// FailureText is a more specific observed failure signature. When it
	// matches a card's machine-oriented keyword it outranks the broader
	// lifecycle reason in PriorityText.
	FailureText    string
	Intent         string
	Profile        string
	Phase          string
	RequireProfile bool
	Limit          int
	// IncludeSourceIDs is an evaluation/debug allowlist. Empty preserves the
	// normal retrieval corpus; non-empty limits retrieval to these stable IDs
	// so controlled RAG ablations can isolate one procedure without rewriting
	// the embedded profile.
	IncludeSourceIDs []string
	// DirectSourceIDs is a lifecycle-event route, not a lexical hint. Matching
	// stable IDs are eligible without keyword overlap, while normal profile,
	// phase, include, and exclude boundaries still apply.
	DirectSourceIDs  []string
	ExcludeSourceIDs []string
}

// Load reads the embedded templates plus any *.json in extraDir (the
// flywheel: successful outputs saved there become retrievable). extraDir
// may be "".
func Load(extraDir string) *Store {
	s := &Store{}
	s.addEmbedDir(builtin, "templates")  // EXAMPLE templates (snake HTML, …)
	s.addEmbedDir(corpusFS, "corpus")    // KNOWLEDGE corpus (office/eng/3D, 861)
	s.addEmbedDir(profileFS, "profiles") // model/harness-specific procedure cards
	if extraDir != "" {
		if files, err := filepath.Glob(filepath.Join(extraDir, "*.json")); err == nil {
			for _, f := range files {
				if b, err := os.ReadFile(f); err == nil {
					s.add(b)
				}
			}
		}
	}
	return s
}

func (s *Store) addEmbedDir(fsys embed.FS, dir string) {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if b, err := fsys.ReadFile(dir + "/" + e.Name()); err == nil {
			templates := decodeTemplates(b)
			for i := range templates {
				if strings.TrimSpace(templates[i].Content) == "" && strings.TrimSpace(templates[i].ContentFile) != "" {
					name := filepath.Base(templates[i].ContentFile)
					if content, readErr := fsys.ReadFile(dir + "/" + name); readErr == nil {
						templates[i].Content = string(content)
					}
				}
			}
			s.addTemplates(templates)
		}
	}
}

// add ingests one JSON file. Corpus files are an ARRAY of entries; template
// files are a single OBJECT — try the array shape first, fall back to one.
func (s *Store) add(b []byte) {
	s.addTemplates(decodeTemplates(b))
}

func decodeTemplates(b []byte) []Template {
	var arr []Template
	if json.Unmarshal(b, &arr) == nil && len(arr) > 0 {
		return arr
	}
	var t Template
	if json.Unmarshal(b, &t) == nil {
		return []Template{t}
	}
	return nil
}

func (s *Store) addTemplates(templates []Template) {
	for i := range templates {
		if valid(&templates[i]) {
			s.templates = append(s.templates, templates[i])
		}
	}
}

// valid keeps an entry only if it can actually be injected — either an example
// (Content) or distilled knowledge (Execution).
func valid(t *Template) bool {
	return strings.TrimSpace(t.Content) != "" || strings.TrimSpace(t.Execution) != ""
}

// Len reports how many templates are loaded.
func (s *Store) Len() int { return len(s.templates) }

// Best returns the highest-scoring template for a request, or nil. A
// KEYWORD match is required — a bare type match is not enough, so a "踩地雷"
// request never gets the snake template just because both are static web.
func (s *Store) Best(userText, intent string) *Template {
	matches := s.Search(userText, intent, 1)
	if len(matches) == 0 {
		return nil
	}
	return matches[0].Template
}

// Search returns the ranked Top-K keyword-backed evidence. Type agreement is
// only a tiebreak bonus: a bare intent match still cannot retrieve unrelated
// knowledge. Equal scores prefer newer metadata while preserving corpus order
// when dates are absent or equal.
func (s *Store) Search(userText, intent string, limit int) []Match {
	return s.SearchRequest(SearchRequest{Text: userText, Intent: intent, Limit: limit})
}

// SearchRequest returns ranked evidence for one explicit model/harness phase.
// Generic corpus entries remain available to every profile. Profile-tagged
// entries require an exact profile match, preventing Qwen-specific guidance
// from leaking into the independently versioned Gemma pipelines.
func (s *Store) SearchRequest(request SearchRequest) []Match {
	if request.Limit <= 0 {
		return nil
	}
	lt := strings.ToLower(strings.TrimSpace(request.Text + "\n" + request.PriorityText + "\n" + request.FailureText))
	priorityText := strings.ToLower(request.PriorityText)
	failureText := strings.ToLower(request.FailureText)
	excluded := make(map[string]bool, len(request.ExcludeSourceIDs))
	for _, id := range request.ExcludeSourceIDs {
		excluded[strings.TrimSpace(id)] = true
	}
	included := make(map[string]bool, len(request.IncludeSourceIDs))
	for _, id := range request.IncludeSourceIDs {
		if id = strings.TrimSpace(id); id != "" {
			included[id] = true
		}
	}
	direct := make(map[string]bool, len(request.DirectSourceIDs))
	for _, id := range request.DirectSourceIDs {
		if id = strings.TrimSpace(id); id != "" {
			direct[id] = true
		}
	}
	// Query-local document frequency makes a specific term such as "snake"
	// outrank broad corpus terms such as "game" or "html" without requiring a
	// second embedding model on the thin path.
	docFreq := make(map[string]int)
	for i := range s.templates {
		if !templateInScope(&s.templates[i], request.Profile, request.Phase, request.RequireProfile, included, excluded) {
			continue
		}
		seen := make(map[string]bool)
		for _, kw := range s.templates[i].Keywords {
			norm := strings.ToLower(strings.TrimSpace(kw))
			if norm != "" && keywordMatches(lt, norm) && !seen[norm] {
				docFreq[norm]++
				seen[norm] = true
			}
		}
	}
	var matches []Match
	for i := range s.templates {
		t := &s.templates[i]
		if !templateInScope(t, request.Profile, request.Phase, request.RequireProfile, included, excluded) {
			continue
		}
		hits := 0
		score := 0
		directMatch := direct[TemplateID(t)]
		seen := make(map[string]bool)
		for _, kw := range t.Keywords {
			norm := strings.ToLower(strings.TrimSpace(kw))
			if norm != "" && keywordMatches(lt, norm) && !seen[norm] {
				hits++
				seen[norm] = true
				if freq := docFreq[norm]; freq > 0 {
					score += 1000 / freq
				}
				if priorityText != "" && keywordMatches(priorityText, norm) {
					score += 4000
				}
				if failureText != "" && keywordMatches(failureText, norm) {
					score += 6000
				}
			}
		}
		if hits == 0 && !directMatch {
			continue // no keyword overlap → wrong template, skip
		}
		if directMatch {
			// Explicit lifecycle routing must outrank any coincidental lexical
			// overlap while remaining visible as a zero-keyword direct match.
			score += 100000
		}
		intentMatch := t.Type != "" && t.Type == request.Intent
		profileMatch := t.Profile != "" && t.Profile == request.Profile
		phaseMatch := len(t.Phases) > 0 && containsFold(t.Phases, request.Phase)
		score += hits * 10
		if intentMatch {
			score++
		}
		// During recovery, a card distilled for this exact model/profile should
		// outrank broad domain knowledge. Lexical overlap is still mandatory.
		if profileMatch {
			score += 2000
		}
		if phaseMatch {
			score += 1000
		}
		matches = append(matches, Match{
			Template: t, Score: score, KeywordHits: hits, IntentMatch: intentMatch,
			ProfileMatch: profileMatch, PhaseMatch: phaseMatch,
		})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return evidenceDate(matches[i].Template).After(evidenceDate(matches[j].Template))
	})
	// Broad lexical matches that score far below the best evidence add prompt
	// noise rather than useful alternatives. Keep close-scoring sources so
	// version conflicts and multi-source evidence still survive Top-K.
	if len(matches) > 1 {
		floor := matches[0].Score * 2 / 3
		for len(matches) > 1 && matches[len(matches)-1].Score < floor {
			matches = matches[:len(matches)-1]
		}
	}
	if len(matches) > request.Limit {
		matches = matches[:request.Limit]
	}
	return matches
}

func templateInScope(t *Template, profile, phase string, requireProfile bool, included, excluded map[string]bool) bool {
	if t == nil {
		return false
	}
	id := TemplateID(t)
	if excluded[id] || (len(included) > 0 && !included[id]) {
		return false
	}
	if requireProfile && (profile == "" || t.Profile != profile) {
		return false
	}
	if t.Profile != "" && t.Profile != profile {
		return false
	}
	return len(t.Phases) == 0 || containsFold(t.Phases, phase)
}

func containsFold(values []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}

// TemplateID returns stable retrieval provenance even for older corpus entries
// that predate explicit IDs.
func TemplateID(t *Template) string {
	if t == nil {
		return ""
	}
	if value := strings.TrimSpace(t.ID); value != "" {
		return value
	}
	return strings.Join([]string{
		strings.TrimSpace(t.Source), strings.TrimSpace(t.Type),
		strings.TrimSpace(t.Title), strings.TrimSpace(t.Version),
	}, "|")
}

func keywordMatches(text, keyword string) bool {
	if keyword == "" {
		return false
	}
	allASCII := true
	for i := 0; i < len(keyword); i++ {
		if keyword[i] >= 0x80 {
			allASCII = false
			break
		}
	}
	if !allASCII {
		return strings.Contains(text, keyword)
	}
	for start := 0; start <= len(text)-len(keyword); {
		rel := strings.Index(text[start:], keyword)
		if rel < 0 {
			return false
		}
		pos := start + rel
		end := pos + len(keyword)
		leftOK := pos == 0 || !asciiWord(text[pos-1]) || !asciiWord(keyword[0])
		rightOK := end == len(text) || !asciiWord(text[end]) || !asciiWord(keyword[len(keyword)-1])
		if leftOK && rightOK {
			return true
		}
		start = pos + 1
	}
	return false
}

func asciiWord(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

func evidenceDate(t *Template) time.Time {
	if t == nil || strings.TrimSpace(t.Updated) == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(t.Updated)); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// CompactEvidence formats ranked evidence for the thin small-model path. It
// never injects a full example artifact. Source/version/date labels are kept so
// the model can resolve conflicts instead of treating every chunk as equally
// current or authoritative.
func CompactEvidence(matches []Match, maxRunes int) string {
	if len(matches) == 0 || maxRunes <= 0 {
		return ""
	}
	var blocks []string
	for i, match := range matches {
		t := match.Template
		if t == nil {
			continue
		}
		body := strings.TrimSpace(t.Execution)
		if body == "" && strings.TrimSpace(t.Content) != "" {
			body = "This match is a verified example artifact. Use its named structure only; the full artifact is intentionally omitted from thin context."
		}
		if body == "" {
			continue
		}
		meta := []string{"title=" + strings.TrimSpace(t.Title)}
		if s := strings.TrimSpace(t.Source); s != "" {
			meta = append(meta, "source="+s)
		}
		if s := strings.TrimSpace(t.Version); s != "" {
			meta = append(meta, "version="+s)
		}
		if s := strings.TrimSpace(t.Updated); s != "" {
			meta = append(meta, "updated="+s)
		}
		blocks = append(blocks, "[E"+itoa(i+1)+"] "+strings.Join(meta, " | ")+"\n"+body)
	}
	if len(blocks) == 0 {
		return ""
	}
	text := "【Meadow compact RAG evidence】\n" +
		"Use only supported facts. Prefer the newer applicable source when versions conflict; if decisive evidence is missing, clarify instead of guessing. Combine evidence when the task requires it.\n" +
		strings.Join(blocks, "\n\n")
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	if maxRunes == 1 {
		return string(runes[:1])
	}
	return string(runes[:maxRunes-1]) + "…"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Inject formats a template as a reference block for the action prompt.
// The wording is load-bearing: an earlier "改寫這個範例…只改用戶要不一樣的
// 地方" phrasing made a 12B read the task as "modify on request" and stop to
// ASK the user what to change instead of building — a clean run that wrote
// nothing. This version is imperative: build NOW with sensible defaults,
// treat the example as a STRUCTURE reference (not a thing to edit), and never
// ask back.
func Inject(t *Template) string {
	if t == nil {
		return ""
	}
	if strings.TrimSpace(t.Content) != "" {
		return injectExample(t)
	}
	return injectKnowledge(t)
}

// injectExample frames a complete working artifact as a STRUCTURE reference.
func injectExample(t *Template) string {
	return "下面是一個已驗證可正常運作的「" + t.Title + "」，當作「結構參考」：\n" +
		"```html\n" + t.Content + "\n```\n" +
		"現在請「立刻」依用戶這次的要求，用 write_file 產出一個完整、內部一致、" +
		"可直接執行的單一 HTML 檔案。學上面範例的結構（元素 id 要和 JS getElementById 對應、" +
		"遊戲迴圈、鍵盤事件監聽的寫法），但用你自己的程式碼重寫，不要逐字照抄、也不要只寫骨架。" +
		"用戶若沒指定顏色/規則/大小，就自己用合理預設值直接做完——" +
		"不要反問用戶、不要停下來等進一步說明。"
}

// injectKnowledge frames distilled domain knowledge (execution + quality bar)
// for a non-example task — what a strong model knows about doing this kind of
// work, handed to a weaker one. It is guidance to APPLY, not text to copy, and
// (like injectExample) is imperative: do the work now with sensible defaults.
func injectKnowledge(t *Template) string {
	var b strings.Builder
	b.WriteString("參考「" + t.Title + "」的專業執行知識與品質標準（這是領域知識，用來指導你怎麼做，不是要照抄的範例）：\n")
	b.WriteString("【執行方式】\n")
	b.WriteString(strings.TrimSpace(t.Execution))
	b.WriteString("\n")
	if len(t.Rubric) > 0 {
		b.WriteString("【品質標準（你的產出必須滿足）】\n")
		for _, r := range t.Rubric {
			if s := strings.TrimSpace(r); s != "" {
				b.WriteString("- ")
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("現在依用戶這次的要求，套用上面的執行方式與品質標準，直接把產出做完——" +
		"用合理預設值，不要反問用戶、不要只寫骨架。")
	return b.String()
}
