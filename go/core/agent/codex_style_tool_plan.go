package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	codexStyleToolSearchName      = "tool_search"
	codexStyleAdvisorToolName     = "advisor"
	codexStyleInteractionToolName = "meadow_browser_interaction_check"
	codexStyleDiscoveryNamesBytes = 1536
)

const codexStyleToolSearchDescription = "Search available and deferred tool metadata. Results distinguish tools already exposed in this model request from deferred schemas activated for the next model step. Discovery does not execute tools or verify service readiness."

type codexStyleToolExposure uint8

const (
	codexStyleToolDirect codexStyleToolExposure = iota
	codexStyleToolDeferred
	codexStyleToolExplicit
	codexStyleToolHidden
)

type codexStyleToolPlan struct {
	registry          *tools.Registry
	directNames       []string
	deferredNames     []string
	explicitNames     []string
	activated         map[string]bool
	searchTool        *tools.Registry
	advisorTool       *tools.Registry
	advisorEnabled    bool
	correctionTool    *tools.Registry
	correctionEnabled bool
}

type codexStyleToolSearch struct {
	deferredNames []string
}

func (*codexStyleToolSearch) Name() string { return codexStyleToolSearchName }
func (t *codexStyleToolSearch) Description() string {
	if len(t.deferredNames) == 0 {
		return codexStyleToolSearchDescription
	}
	// Names reveal which capabilities exist without loading their schemas or
	// selecting a workflow. JSON quoting keeps complete IDs unambiguous. Omit
	// whole entries at the bound; every omitted tool remains searchable.
	names := make([]string, 0, len(t.deferredNames))
	encoded := []byte("[]")
	for _, name := range t.deferredNames {
		candidate, _ := json.Marshal(append(names, name))
		if len(candidate) <= codexStyleDiscoveryNamesBytes {
			names = append(names, name)
			encoded = candidate
		}
	}
	description := fmt.Sprintf("%s\nDiscovery catalog (%d tools; %d shown): %s.",
		codexStyleToolSearchDescription, len(t.deferredNames), len(names), encoded)
	if omitted := len(t.deferredNames) - len(names); omitted > 0 {
		description += fmt.Sprintf(" %d more deferred tools remain searchable by capability.", omitted)
	}
	return description
}
func (*codexStyleToolSearch) Kind() tools.Kind { return tools.Kind("read") }
func (*codexStyleToolSearch) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "A short capability-oriented search query, preferably using tool-domain terms.",
			},
			"limit": map[string]any{
				"type":    "integer",
				"minimum": 1,
				"maximum": 10,
			},
		},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}
func (*codexStyleToolSearch) Execute(context.Context, map[string]any) tools.Result {
	return tools.Result{LLMContent: "ERROR: tool_search must be executed by the Codex-style step planner."}
}

// codexStyleAdvisor is a model-owned control tool. Its empty schema mirrors
// Claude Code's Advisor boundary: the primary model decides when an independent
// review is warranted, while the runtime only performs the requested second
// inference. Execute is never called through ToolExecutor.
type codexStyleAdvisor struct{}

func (*codexStyleAdvisor) Name() string { return codexStyleAdvisorToolName }
func (*codexStyleAdvisor) Description() string {
	return "Request one independent strategy review before continuing. Use when repeated errors are not converging, when you may be preserving a failed assumption, before a major change of approach, or before claiming completion on difficult work. Call advisor alone with no arguments. The reviewer returns advice only; you remain responsible for the next action. Do not use for simple questions or routine successful steps."
}
func (*codexStyleAdvisor) Kind() tools.Kind { return tools.Kind("read") }
func (*codexStyleAdvisor) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}
func (*codexStyleAdvisor) Execute(context.Context, map[string]any) tools.Result {
	return tools.Result{LLMContent: "ERROR: advisor must be executed by the Codex-style runtime."}
}

func newCodexStyleToolPlan(registry *tools.Registry) *codexStyleToolPlan {
	plan := &codexStyleToolPlan{
		registry:       registry,
		activated:      map[string]bool{},
		searchTool:     tools.NewRegistry(&codexStyleToolSearch{}),
		advisorTool:    tools.NewRegistry(&codexStyleAdvisor{}),
		correctionTool: tools.NewRegistry(&verificationCorrectTool{}),
	}
	if registry == nil {
		return plan
	}
	for _, name := range registry.Names() {
		tool, ok := registry.Get(name)
		if !ok || tool == nil {
			continue
		}
		switch codexStyleDefaultToolExposure(tool) {
		case codexStyleToolDeferred:
			plan.deferredNames = append(plan.deferredNames, name)
		case codexStyleToolExplicit:
			plan.explicitNames = append(plan.explicitNames, name)
		case codexStyleToolHidden:
			continue
		default:
			plan.directNames = append(plan.directNames, name)
		}
	}
	// Companions can already have direct schemas (for example process belongs
	// to run_shell_command). Keep the discovery inventory focused on tools whose
	// schemas are actually absent from the first model step.
	direct := registry.Select(plan.directNames)
	var inventory []string
	for _, name := range plan.deferredNames {
		if _, exposed := direct.Get(name); !exposed {
			inventory = append(inventory, name)
		}
	}
	plan.searchTool.Register(&codexStyleToolSearch{deferredNames: inventory})
	return plan
}

func (p *codexStyleToolPlan) enableAdvisor() {
	if p != nil {
		p.advisorEnabled = true
	}
}

func codexStyleDefaultToolExposure(tool tools.Tool) codexStyleToolExposure {
	name := strings.ToLower(strings.TrimSpace(tool.Name()))
	if name == "" {
		return codexStyleToolHidden
	}
	// Driving an arbitrary page is an explicit user capability. Verification
	// tools are ordinary model-selectable instruments: exposing one never makes
	// it a harness-owned completion gate.
	if name == "browse_page" {
		return codexStyleToolExplicit
	}
	// Basic retrieval and presentation delivery are direct product capabilities
	// whenever registered. The full schema lets the model act without assuming
	// a presentation service needs a CLI installation. Exposure never executes
	// a tool or widens its edit authorization.
	// Visibility is global metadata, independent of the prompt; the model still
	// decides whether to use either tool and all execution boundaries remain.
	if name == tools.SkillReadToolName || name == "web_search" || name == "web_fetch" || name == "presentation_templates" || name == "presentation_artifact" || name == tools.PresentationFromMarkdownToolName {
		return codexStyleToolDirect
	}
	// This is capability exposure metadata, not user-prompt intent routing.
	// Local coding primitives remain direct. Expensive or domain-specific
	// capabilities are discovered by the model through tool_search.
	if codexStyleDeferredCoreExtension(name) ||
		(strings.HasPrefix(name, "meadow_browser_") && name != "meadow_browser_dom") ||
		strings.HasPrefix(name, "presentation_") ||
		name == "read_experience" ||
		strings.HasPrefix(name, "web_") ||
		strings.HasPrefix(name, "document_") ||
		strings.Contains(name, "research") ||
		strings.Contains(name, "download_image") ||
		strings.Contains(name, "pdf") ||
		strings.Contains(name, "typescript_web_build") {
		return codexStyleToolDeferred
	}
	return codexStyleToolDirect
}

func codexStyleDeferredCoreExtension(name string) bool {
	switch name {
	case "process", "todo_write", "ask_user_question", "code_outline":
		return true
	default:
		return false
	}
}

// activateExplicitReferences mirrors Codex's deterministic explicit MCP/tool
// activation. It matches registered capability names only; natural-language
// task semantics belong to the model, not a phrase classifier in the harness.
func (p *codexStyleToolPlan) activateExplicitReferences(text string) {
	if p == nil {
		return
	}
	normalized := strings.ToLower(text)
	for _, names := range [][]string{p.deferredNames, p.explicitNames} {
		for _, name := range names {
			nameLower := strings.ToLower(name)
			spaced := strings.ReplaceAll(nameLower, "_", " ")
			dashed := strings.ReplaceAll(nameLower, "_", "-")
			if strings.Contains(normalized, nameLower) ||
				(spaced != nameLower && strings.Contains(normalized, spaced)) ||
				(dashed != nameLower && strings.Contains(normalized, dashed)) {
				p.activated[name] = true
			}
		}
	}
}

func (p *codexStyleToolPlan) modelVisibleSchemas() []provider.Tool {
	if p == nil || p.registry == nil {
		return nil
	}
	names := append([]string(nil), p.directNames...)
	for _, name := range p.deferredNames {
		if p.activated[name] {
			names = append(names, name)
		}
	}
	for _, name := range p.explicitNames {
		if p.activated[name] {
			names = append(names, name)
		}
	}
	schemas := codexStyleToolSchemas(p.registry.Select(names))
	if len(p.deferredNames) > 0 {
		schemas = append(schemas, codexStyleToolSchemas(p.searchTool)...)
	}
	if p.advisorEnabled {
		schemas = append(schemas, codexStyleToolSchemas(p.advisorTool)...)
	}
	if p.correctionEnabled {
		schemas = append(schemas, codexStyleToolSchemas(p.correctionTool)...)
	}
	// Keep request-local source enums at the end. Their exact values remain
	// visible and enforced; a new message must not invalidate every fixed tool
	// definition preceding the changing operand in the model's chat template.
	stable := make([]provider.Tool, 0, len(schemas))
	var scoped []provider.Tool
	for _, schema := range schemas {
		tool, _ := p.registry.Get(schema.Function.Name)
		if dynamic, ok := tool.(tools.RunScopedSchemaProvider); ok && dynamic.RunScopedSchema() {
			scoped = append(scoped, schema)
		} else {
			stable = append(stable, schema)
		}
	}
	return append(stable, scoped...)
}

// Claim checks need known names, not only the schemas loaded into this model
// step. Deferral must not turn an already registered capability into an unknown
// name for the existing unsupported-success check. This metadata-only union is
// never sent to the model and grants neither tool exposure nor authorization.
func codexStyleClaimToolNames(visible []provider.Tool, registry *tools.Registry) []provider.Tool {
	// Preserve the existing no-tools/answer-only claim-check boundary.
	if len(visible) == 0 {
		return nil
	}
	var result []provider.Tool
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		result = append(result, provider.Tool{Type: "function", Function: provider.ToolSchema{Name: name}})
	}
	for _, schema := range visible {
		add(schema.Function.Name)
	}
	if registry != nil {
		for _, name := range registry.Names() {
			add(name)
		}
	}
	return result
}

func (p *codexStyleToolPlan) activate(arguments string, requestSchemas ...[]provider.Tool) string {
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return "ERROR: invalid tool_search arguments: " + err.Error()
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" {
		return "ERROR: tool_search requires a non-empty capability query."
	}
	if input.Limit <= 0 {
		input.Limit = 5
	}
	if input.Limit > 10 {
		input.Limit = 10
	}
	// Capture before changing activation: two searches in the same response
	// must not mistake the first search's next-step schemas for schemas that
	// were actually present in the provider request that produced both calls.
	var exposed []provider.Tool
	if len(requestSchemas) > 0 {
		exposed = requestSchemas[0]
	} else {
		exposed = p.modelVisibleSchemas()
	}
	available := codexStyleExposedToolNames(exposed)
	matches := p.search(input.Query, input.Limit, exposed)
	if len(matches) == 0 {
		return fmt.Sprintf("No deferred tools matched %q, and no already-available tool matched. Try a broader capability query or continue with the tools already available.", input.Query)
	}
	var existing, activated, pending []string
	for _, match := range matches {
		switch {
		case available[match.name]:
			existing = append(existing, fmt.Sprintf("- %s [already_available]: %s", match.name, match.description))
		case p.activated[match.name]:
			pending = append(pending, fmt.Sprintf("- %s [already_activated; available_next_step]: %s", match.name, match.description))
		default:
			p.activated[match.name] = true
			activated = append(activated, fmt.Sprintf("- %s [newly_activated; available_next_step]: %s", match.name, match.description))
		}
	}
	lines := make([]string, 0, len(matches)+3)
	if len(existing) > 0 {
		lines = append(lines, "Already available tools (schemas present in this model request):")
		lines = append(lines, existing...)
	}
	if len(activated) > 0 {
		lines = append(lines, "Activated deferred tools for the next model step:")
		lines = append(lines, activated...)
	}
	if len(pending) > 0 {
		lines = append(lines, "Previously activated tools awaiting the next model step:")
		lines = append(lines, pending...)
	}
	return strings.Join(lines, "\n")
}

func codexStyleExposedToolNames(schemas []provider.Tool) map[string]bool {
	names := make(map[string]bool, len(schemas))
	for _, schema := range schemas {
		names[schema.Function.Name] = true
	}
	return names
}

type codexStyleToolMatch struct {
	name        string
	description string
	score       float64
}

func (p *codexStyleToolPlan) search(query string, limit int, requestSchemas ...[]provider.Tool) []codexStyleToolMatch {
	if p == nil || p.registry == nil {
		return nil
	}
	queryTerms := codexStyleSearchTerms(query)
	if len(queryTerms) == 0 {
		return nil
	}
	type document struct {
		name        string
		description string
		terms       []string
		frequency   map[string]int
	}
	var exposed []provider.Tool
	if len(requestSchemas) > 0 {
		exposed = requestSchemas[0]
	} else {
		exposed = p.modelVisibleSchemas()
	}
	eligible := make(map[string]bool, len(p.deferredNames)+len(exposed))
	for _, name := range p.deferredNames {
		eligible[name] = true
	}
	for _, schema := range exposed {
		name := schema.Function.Name
		tool, ok := p.registry.Get(name)
		if !ok || tool == nil {
			continue
		}
		exposure := codexStyleDefaultToolExposure(tool)
		if exposure != codexStyleToolHidden && (exposure != codexStyleToolExplicit || p.activated[name]) {
			eligible[name] = true
		}
	}
	documents := make([]document, 0, len(eligible))
	documentFrequency := map[string]int{}
	totalLength := 0
	for _, name := range p.registry.Names() {
		if !eligible[name] {
			continue
		}
		tool, ok := p.registry.Get(name)
		if !ok || tool == nil {
			continue
		}
		description := strings.TrimSpace(tool.Description())
		terms := codexStyleSearchTerms(codexStyleToolSearchText(tool))
		frequency := map[string]int{}
		seen := map[string]bool{}
		for _, term := range terms {
			frequency[term]++
			if !seen[term] {
				documentFrequency[term]++
				seen[term] = true
			}
		}
		documents = append(documents, document{name: name, description: description, terms: terms, frequency: frequency})
		totalLength += len(terms)
	}
	if len(documents) == 0 {
		return nil
	}
	averageLength := float64(totalLength) / float64(len(documents))
	if averageLength == 0 {
		averageLength = 1
	}
	const k1 = 1.2
	const b = 0.75
	matches := make([]codexStyleToolMatch, 0, len(documents))
	for _, document := range documents {
		score := 0.0
		for _, term := range queryTerms {
			frequency := float64(document.frequency[term])
			if frequency == 0 {
				continue
			}
			df := float64(documentFrequency[term])
			idf := math.Log(1 + (float64(len(documents))-df+0.5)/(df+0.5))
			lengthRatio := float64(len(document.terms)) / averageLength
			score += idf * (frequency * (k1 + 1)) / (frequency + k1*(1-b+b*lengthRatio))
		}
		if score > 0 {
			matches = append(matches, codexStyleToolMatch{name: document.name, description: document.description, score: score})
		}
	}
	// An exact registered capability reference is stronger than another tool's
	// description mentioning that name. This compares metadata only; ordinary
	// capability queries retain their BM25 order. Unactivated explicit-only
	// tools are not part of this searchable capability collection.
	exactName := strings.TrimSpace(query)
	sort.SliceStable(matches, func(i, j int) bool {
		iExact := strings.EqualFold(matches[i].name, exactName)
		jExact := strings.EqualFold(matches[j].name, exactName)
		if iExact != jExact {
			return iExact
		}
		if matches[i].score == matches[j].score {
			return matches[i].name < matches[j].name
		}
		return matches[i].score > matches[j].score
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

// codexStyleToolSearchText keeps metadata discovery cheap without making it
// shallow. A model often searches for a capability using an argument concept
// (for example "CSS selector" or "output path") that appears only in the JSON
// schema. Index those model-visible names and descriptions as well as the tool's
// top-level metadata, matching the information a loaded schema would expose.
func codexStyleToolSearchText(tool tools.Tool) string {
	if tool == nil {
		return ""
	}
	parts := make([]string, 0, 16)
	appendAlias := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		parts = append(parts, value)
		if spaced := strings.ReplaceAll(value, "_", " "); spaced != value {
			parts = append(parts, spaced)
		}
		if dashed := strings.ReplaceAll(value, "_", "-"); dashed != value {
			parts = append(parts, dashed)
		}
	}
	appendAlias(tool.Name())
	if description := strings.TrimSpace(tool.Description()); description != "" {
		parts = append(parts, description)
	}
	codexStyleAppendToolSchemaSearchText(&parts, tool.Schema(), appendAlias)
	return strings.Join(parts, " ")
}

func codexStyleAppendToolSchemaSearchText(parts *[]string, value any, appendAlias func(string)) {
	switch typed := value.(type) {
	case map[string]any:
		if properties, ok := typed["properties"].(map[string]any); ok {
			names := make([]string, 0, len(properties))
			for name := range properties {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				appendAlias(name)
				codexStyleAppendToolSchemaSearchText(parts, properties[name], appendAlias)
			}
		}
		for _, key := range []string{"description", "title"} {
			if text, ok := typed[key].(string); ok && strings.TrimSpace(text) != "" {
				*parts = append(*parts, text)
			}
		}
		if enum, ok := typed["enum"]; ok {
			codexStyleAppendToolSchemaSearchText(parts, enum, appendAlias)
		}
		for _, key := range []string{"items", "oneOf", "anyOf", "allOf"} {
			if nested, ok := typed[key]; ok {
				codexStyleAppendToolSchemaSearchText(parts, nested, appendAlias)
			}
		}
	case []any:
		for _, item := range typed {
			codexStyleAppendToolSchemaSearchText(parts, item, appendAlias)
		}
	case []string:
		for _, item := range typed {
			appendAlias(item)
		}
	case string:
		appendAlias(typed)
	}
}

// codexStyleExplicitAnswerOnlyBoundary recognizes a narrow user-owned
// execution boundary, not a task router. It never decides how to solve a task;
// it only withholds side-effecting capabilities when the user explicitly says
// that this turn must be an answer and nothing else.
func codexStyleExplicitAnswerOnlyBoundary(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return false
	}
	for _, prefix := range []string{
		"只回覆", "只回复", "只回答", "只輸出", "只输出", "僅回覆", "仅回复", "僅回答", "仅回答",
		"請只回覆", "请只回复", "請只回答", "请只回答", "請只輸出", "请只输出",
		"only reply", "only respond", "only answer", "reply only with", "respond only with", "answer only with",
		"please only reply", "please only respond", "please only answer", "just reply", "just respond", "just answer",
	} {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	for _, phrase := range []string{
		"不要使用工具", "不要呼叫工具", "不要调用工具", "無需使用工具", "无需使用工具",
		"do not use tools", "don't use tools", "without using tools",
	} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}

func codexStyleSearchTerms(value string) []string {
	value = strings.ToLower(value)
	terms := make([]string, 0, 16)
	var word []rune
	flush := func() {
		if len(word) > 0 {
			terms = append(terms, string(word))
			word = word[:0]
		}
	}
	var previousHan rune
	for _, r := range value {
		switch {
		case unicode.Is(unicode.Han, r):
			flush()
			terms = append(terms, string(r))
			if previousHan != 0 {
				terms = append(terms, string([]rune{previousHan, r}))
			}
			previousHan = r
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			previousHan = 0
			word = append(word, r)
		default:
			previousHan = 0
			flush()
		}
	}
	flush()
	return terms
}
