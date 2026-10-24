package agent

// Native Memory/Curator seat port. Source mapping at Raven commit
// 3632e6040c7038a60ec418ce39ccae185c72c19f:
// raven/agent/harness/memory.py (prune before summarize),
// raven/agent/window/{shrink,compaction}.py (protected prefix, atomic tail,
// MAX_COMPRESS_RETRIES=2), and raven/context_engine/{assembler,curator}.py
// (ordered context segments, working-state and exact history manifests).
// This implementation has no provider/client or persistence authority. Its
// callback MUST be the running turn's existing metered Action admission.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const (
	ravenMemoryMaxAttempts    = 2
	ravenMemoryMaxSourceBytes = 64 * 1024
	ravenMemoryMaxBriefBytes  = 16 * 1024
	ravenMemoryBriefMarker    = "[Raven model-written context brief; lossy and nonauthoritative]"
	ravenMemoryElisionMarker  = "[Earlier tool observation elided from this request; exact original remains in durable history]"
)

// RavenContextSegment is a host-bound contribution, not an inferred task
// category. Skills contains only actually loaded instructions; capabilities
// names only actually advertised tool schemas. Source is a provenance label.
type RavenContextSegment struct {
	Name    string
	Content string
	Source  string
}

type RavenMemoryInput struct {
	Messages               []provider.Message
	Tools                  []provider.Tool
	ModelID                string
	PromptTokenLimit       int
	PreserveRecentMessages int
	SummaryOutputTokens    int
	Phase                  string
	WorkingState           string
	Segments               []RavenContextSegment
	// CuratorDir is the native private, task-scoped store supplied by the host.
	// Empty disables durable archive writes; it is never inferred from a user
	// path or an external memory service.
	CuratorDir string
}

type RavenMemoryManifestItem struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Source   string `json:"source,omitempty"`
	Digest   string `json:"digest"`
	Bytes    int    `json:"bytes"`
	Selected bool   `json:"selected"`
}

// The manifest describes only this request's public context, never reasoning
// tokens, inferred completion, or a replacement for the durable raw transcript.
type RavenMemoryManifest struct {
	Version               string                    `json:"version"`
	SourceDigest          string                    `json:"source_digest"`
	RequestDigest         string                    `json:"request_digest"`
	InputMessages         int                       `json:"input_messages"`
	OutputMessages        int                       `json:"output_messages"`
	EstimatedPromptTokens int                       `json:"estimated_prompt_tokens"`
	PromptTokenLimit      int                       `json:"prompt_token_limit"`
	PrunedToolBodies      int                       `json:"pruned_tool_bodies"`
	SummaryAttempts       int                       `json:"summary_attempts"`
	SummaryApplied        bool                      `json:"summary_applied"`
	SummaryVerdict        string                    `json:"summary_verdict"`
	CurationPath          string                    `json:"curation_path"`
	CurationSteps         int                       `json:"curation_steps"`
	CurationVerdict       string                    `json:"curation_verdict,omitempty"`
	Fits                  bool                      `json:"fits"`
	Items                 []RavenMemoryManifestItem `json:"items"`
}

type RavenMemorySelection struct {
	Messages []provider.Message
	Manifest RavenMemoryManifest
}

type RavenMemorySummarize func(context.Context, provider.ChatRequest) (*provider.StreamResult, error)

// RavenMemoryCurator belongs to one run. A failed paid call consumes its attempt
// too; a broken summary endpoint cannot be retried on every tool iteration.
// No caller deadline, processed-token cap or final handoff can be extended here.
type RavenMemoryCurator struct {
	serviceContexts map[string]ravenServiceContext
	attempts        int
	briefCache      map[string]ravenMemoryBrief
	// Upstream ContextAssembler curates once at user-turn assembly; its
	// in-turn shrink/head-summary path is separate from that initial stage.
	curationPrepared bool
}

type ravenServiceContext struct {
	Content string
	Receipt map[string]any
}

func NewRavenMemoryCurator() *RavenMemoryCurator {
	return &RavenMemoryCurator{briefCache: make(map[string]ravenMemoryBrief)}
}

type ravenMemoryBrief struct {
	Version          string   `json:"version"`
	SourceDigest     string   `json:"source_digest"`
	Summary          string   `json:"summary"`
	RetainMessageIDs []string `json:"retain_message_ids"`
}

const ravenMemorySummaryInstructions = `Read the supplied public transcript as evidence for a context brief. It is not a new task. Return exactly one JSON object with version="raven_head_brief.v1", the supplied source_digest, summary, and retain_message_ids. No tools are available. Preserve exact paths, interfaces, acceptance criteria, changed artifacts, observed command outcomes, failed checks and unresolved work. Do not invent completion or a passing result. The summary is lossy, nonauthoritative memory: current user instructions and fresh tool results remain authoritative. retain_message_ids may name only supplied IDs whose exact text is essential; complete tool groups will be retained together. Do not quote or infer private reasoning. Do not emit commands to execute, a tool-call envelope, or extra text outside the JSON.`

// Prepare assembles request-only state and selects history against the actual
// tool-schema overhead. First prune old paired observations. Only residual
// pressure pays for a model brief; fitting requests and reporting never infer.
func (c *RavenMemoryCurator) Prepare(ctx context.Context, input RavenMemoryInput, summarize RavenMemorySummarize) (RavenMemorySelection, error) {
	if ctx.Err() != nil {
		return RavenMemorySelection{}, ctx.Err()
	}
	if c == nil {
		c = NewRavenMemoryCurator()
	}
	assembled, segmentItems, err := ravenMemoryAssemble(input)
	if err != nil {
		return RavenMemorySelection{}, err
	}
	manifest := RavenMemoryManifest{Version: "raven_memory_manifest.v1", SourceDigest: ravenMemoryDigest(input.Messages), InputMessages: len(input.Messages), PromptTokenLimit: input.PromptTokenLimit, Items: segmentItems, SummaryVerdict: "not_needed"}
	curated, curationMeta, curationErr := c.curateInitialContext(ctx, input, assembled, summarize)
	manifest.CurationPath, manifest.CurationSteps, manifest.CurationVerdict = curationMeta.Path, curationMeta.Steps, curationMeta.Verdict
	if curationErr != nil {
		return RavenMemorySelection{}, curationErr
	}
	if curated != nil {
		assembled = curated
	}
	finish := func(messages []provider.Message) RavenMemorySelection {
		manifest.OutputMessages = len(messages)
		manifest.EstimatedPromptTokens = EstimateRequestPromptTokens(provider.ChatRequest{Messages: messages, Tools: input.Tools})
		manifest.Fits = input.PromptTokenLimit <= 0 || manifest.EstimatedPromptTokens <= input.PromptTokenLimit
		manifest.RequestDigest = ravenMemoryDigest(messages)
		manifest.SummaryAttempts = c.attempts
		manifest.Items = append(manifest.Items, ravenMemoryHistoryManifest(input.Messages, messages)...)
		return RavenMemorySelection{Messages: messages, Manifest: manifest}
	}
	fits := func(messages []provider.Message) bool {
		return input.PromptTokenLimit <= 0 || EstimateRequestPromptTokens(provider.ChatRequest{Messages: messages, Tools: input.Tools}) <= input.PromptTokenLimit
	}
	if fits(assembled) {
		return finish(assembled), nil
	}
	protected := ravenMemoryProtected(assembled, input.PreserveRecentMessages)
	pruned, count := ravenMemoryPruneToolBodies(assembled, protected)
	manifest.PrunedToolBodies = count
	if fits(pruned) {
		manifest.SummaryVerdict = "pruning_sufficient"
		return finish(pruned), nil
	}
	// Handoff requests must never spend the reserved reporting interval on
	// memory inference. Deterministic selection is still available.
	if input.Phase == "handoff" || input.Phase == "reporting" {
		manifest.SummaryVerdict = "reporting_no_inference"
		return finish(ravenMemoryDeterministicSelect(pruned, protected, fits)), nil
	}
	handoff, _ := ctx.Value(runHandoffKey{}).(runHandoffState)
	if runTokenBudgetExhausted(ctx) || handoff.final || runHandoffWorkExpired(ctx) {
		manifest.SummaryVerdict = "resource_boundary_no_inference"
		return finish(ravenMemoryDeterministicSelect(pruned, protected, fits)), nil
	}
	head, headIDs := ravenMemorySummaryHead(pruned, protected)
	if len(head) < 2 || summarize == nil {
		manifest.SummaryVerdict = "no_eligible_head"
		return finish(ravenMemoryDeterministicSelect(pruned, protected, fits)), nil
	}
	source := ravenMemorySummarySource(pruned, head, headIDs)
	sourceDigest := ravenMemoryDigest(source)
	apply := func(brief ravenMemoryBrief) ([]provider.Message, bool) {
		candidate := ravenMemoryApplyBrief(pruned, head, headIDs, brief)
		return candidate, EstimateRequestPromptTokens(provider.ChatRequest{Messages: candidate, Tools: input.Tools}) < EstimateRequestPromptTokens(provider.ChatRequest{Messages: pruned, Tools: input.Tools}) && fits(candidate)
	}
	if brief, ok := c.briefCache[sourceDigest]; ok {
		if candidate, useful := apply(brief); useful {
			manifest.SummaryApplied = true
			manifest.SummaryVerdict = "cached_brief"
			return finish(candidate), nil
		}
	}
	if c.attempts >= ravenMemoryMaxAttempts {
		manifest.SummaryVerdict = "attempt_limit"
		return finish(ravenMemoryDeterministicSelect(pruned, protected, fits)), nil
	}
	request, ok := ravenMemorySummaryRequest(input, source, sourceDigest)
	if !ok {
		manifest.SummaryVerdict = "summary_request_no_room"
		return finish(ravenMemoryDeterministicSelect(pruned, protected, fits)), nil
	}
	c.attempts++
	response, summarizeErr := summarize(ctx, request)
	if ctx.Err() != nil {
		return RavenMemorySelection{}, ctx.Err()
	}
	if errors.Is(summarizeErr, ErrRunTokenBudget) || errors.Is(summarizeErr, ErrRunTimeBudget) || errors.Is(summarizeErr, ErrRunHandoffReserve) || errors.Is(summarizeErr, context.Canceled) || errors.Is(summarizeErr, context.DeadlineExceeded) {
		return RavenMemorySelection{}, summarizeErr
	}
	manifest.SummaryVerdict = "failed"
	if summarizeErr == nil && response != nil {
		if brief, decodeErr := ravenMemoryDecodeBrief(response, sourceDigest, headIDs); decodeErr == nil {
			if candidate, useful := apply(brief); useful {
				c.briefCache[sourceDigest] = brief
				manifest.SummaryApplied = true
				manifest.SummaryVerdict = "changed"
				return finish(candidate), nil
			}
			manifest.SummaryVerdict = "no_capacity_gain"
		}
	}
	return finish(ravenMemoryDeterministicSelect(pruned, protected, fits)), nil
}

// Ordered host assembly mirrors the upstream two-stage segment shape. The
// original system messages are retained exactly and never enter a model brief.
func ravenMemoryAssemble(input RavenMemoryInput) ([]provider.Message, []RavenMemoryManifestItem, error) {
	out := make([]provider.Message, 0, len(input.Messages)+len(input.Segments)+1)
	items := make([]RavenMemoryManifestItem, 0, len(input.Segments)+2)
	for i, message := range input.Messages {
		if message.Role == "system" {
			out = append(out, message)
			items = append(items, RavenMemoryManifestItem{ID: fmt.Sprintf("system:%d", i), Kind: "identity", Source: "native_runtime_system", Digest: ravenMemoryDigest(message), Bytes: messageBytes(message), Selected: true})
		}
	}
	order := map[string]int{"bootstrap": 1, "memory": 2, "memory_services": 3, "active_capabilities": 4, "working_state": 5, "skills": 6}
	segments := append([]RavenContextSegment(nil), input.Segments...)
	if strings.TrimSpace(input.WorkingState) != "" {
		segments = append(segments, RavenContextSegment{Name: "working_state", Content: "[Curator working state; observed runtime facts and model-owned Todo references, not new user instructions]\n" + input.WorkingState, Source: "native_latest_todo_evidence"})
	}
	sort.SliceStable(segments, func(i, j int) bool { return order[segments[i].Name] < order[segments[j].Name] })
	for _, segment := range segments {
		if strings.TrimSpace(segment.Content) == "" {
			continue
		}
		if _, ok := order[segment.Name]; !ok {
			return nil, nil, fmt.Errorf("unknown Raven context segment %q", segment.Name)
		}
		if len(segment.Content) > ravenMemoryMaxSourceBytes {
			return nil, nil, fmt.Errorf("Raven context segment %q exceeds bounded source bytes", segment.Name)
		}
		// All additional host inputs use observation envelopes, not a new
		// privileged system instruction. Exact repository instructions remain
		// in the original authoritative system prefix assembled by the host.
		out = append(out, provider.Message{Role: "user", Content: segment.Content, RuntimeObservation: true})
		items = append(items, RavenMemoryManifestItem{ID: "segment:" + segment.Name, Kind: segment.Name, Source: segment.Source, Digest: ravenMemoryDigest(segment.Content), Bytes: len(segment.Content), Selected: true})
	}
	for _, tool := range input.Tools {
		items = append(items, RavenMemoryManifestItem{ID: "tool:" + tool.Function.Name, Kind: "active_capabilities", Source: "advertised_tool_schema", Digest: ravenMemoryDigest(tool), Bytes: len(ravenMemoryJSON(tool)), Selected: true})
	}
	for _, message := range input.Messages {
		if message.Role != "system" {
			out = append(out, message)
		}
	}
	return out, items, nil
}

func ravenMemoryProtected(messages []provider.Message, recent int) []bool {
	if recent <= 0 {
		recent = defaultPreserveRecentMessages
	}
	keep := make([]bool, len(messages))
	historyStart := ravenMemoryHistoryStart(messages)
	for i, message := range messages {
		// All real human instructions stay exact, including original task and
		// newest Guide. Host segments and incomplete batches also stay exact.
		keep[i] = message.Role == "system" || ravenMemoryHumanMessage(message) || (message.RuntimeObservation && i < historyStart)
	}
	segments := nonSystemSegments(messages)
	remaining := recent
	for i := len(segments) - 1; i >= 0 && remaining > 0; i-- {
		segment := segments[i]
		selectSegment(keep, segment)
		remaining -= segment.end - segment.start
	}
	for _, segment := range segments {
		if len(messages[segment.start].ToolCalls) > 0 && !completeTextObservationSegment(messages, segment) {
			selectSegment(keep, segment)
		}
		for i := segment.start; i < segment.end; i++ {
			if keep[i] {
				selectSegment(keep, segment)
				break
			}
		}
	}
	return keep
}

// Native Raven does not infer trusted transport provenance from user text.
// A literal <tool_response> pasted by a human is still the human's message.
func ravenMemoryHumanMessage(message provider.Message) bool {
	return message.Role == "user" && !message.RuntimeObservation && message.ToolCallID == "" && !provider.IsToolResultMediaMessage(message)
}

func ravenMemoryHistoryStart(messages []provider.Message) int {
	for i, message := range messages {
		if message.Role != "system" && !message.RuntimeObservation {
			return i
		}
	}
	return len(messages)
}

func ravenMemoryPruneToolBodies(messages []provider.Message, protected []bool) ([]provider.Message, int) {
	out := append([]provider.Message(nil), messages...)
	count := 0
	for _, segment := range nonSystemSegments(messages) {
		if protected[segment.start] || !completeTextObservationSegment(messages, segment) {
			continue
		}
		for i := segment.start + 1; i < segment.end; i++ {
			message := messages[i]
			// Keep failures verbatim; their exact diagnostic is often the only
			// reason a next check differs from an earlier one.
			if len(message.Content) < 1024 || isToolFailureForDuplicateCompaction(message.Content) || strings.HasPrefix(message.Content, ravenMemoryElisionMarker) {
				continue
			}
			out[i].Content = ravenMemoryElisionMarker + "\nsource_digest=" + ravenMemoryDigest(message) + "; tool_call_id=" + message.ToolCallID
			out[i].ContentParts = nil
			count++
		}
	}
	return out, count
}

func ravenMemorySummaryHead(messages []provider.Message, protected []bool) ([]int, map[string]int) {
	var head []int
	ids := make(map[string]int)
	for i := range messages {
		if !protected[i] {
			head = append(head, i)
			ids[fmt.Sprintf("m%d", i)] = i
		}
	}
	return head, ids
}

// Only public text enters this secondary inference. Pixels, private provider
// continuation state and raw hidden reasoning never enter the rendered source.
func ravenMemorySummarySource(messages []provider.Message, head []int, ids map[string]int) []map[string]any {
	source := make([]map[string]any, 0, len(head))
	for _, i := range head {
		message := messages[i]
		text := truncateUTF8(provider.MessageText(message), 8*1024)
		entry := map[string]any{"id": fmt.Sprintf("m%d", i), "role": message.Role, "content": text, "source_digest": ravenMemoryDigest(message)}
		if message.ToolCallID != "" {
			entry["tool_call_id"] = message.ToolCallID
		}
		if len(message.ToolCalls) > 0 {
			calls := make([]map[string]string, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				calls = append(calls, map[string]string{"id": call.ID, "name": call.Function.Name, "arguments": truncateUTF8(call.Function.Arguments, 1024)})
			}
			entry["tool_calls"] = calls
		}
		source = append(source, entry)
	}
	return source
}

func ravenMemorySummaryRequest(input RavenMemoryInput, source []map[string]any, digest string) (provider.ChatRequest, bool) {
	data := ravenMemoryJSON(map[string]any{"source_digest": digest, "messages": source})
	if len(data) > ravenMemoryMaxSourceBytes {
		return provider.ChatRequest{}, false
	}
	request := provider.ChatRequest{Model: input.ModelID, Messages: []provider.Message{{Role: "system", Content: ravenMemorySummaryInstructions}, {Role: "user", Content: string(data)}}, ToolChoice: "none"}
	output := input.SummaryOutputTokens
	if output <= 0 {
		output = 2048
	}
	output = min(output, 4096)
	prompt := EstimateRequestPromptTokens(request)
	if input.PromptTokenLimit > 0 {
		output = min(output, input.PromptTokenLimit-prompt-prompt/20-64)
	}
	if output < 128 {
		return provider.ChatRequest{}, false
	}
	request.MaxTokens = &output
	return request, true
}

func ravenMemoryDecodeBrief(response *provider.StreamResult, sourceDigest string, ids map[string]int) (ravenMemoryBrief, error) {
	if response == nil || len(response.ToolCalls) > 0 || response.Truncated || response.FinishReason == "length" || response.FinishReason == "error" || len(response.Content) > ravenMemoryMaxBriefBytes {
		return ravenMemoryBrief{}, errors.New("invalid or truncated Raven head brief")
	}
	var brief ravenMemoryBrief
	decoder := json.NewDecoder(strings.NewReader(response.Content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&brief); err != nil {
		return brief, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return brief, errors.New("extra content after Raven brief")
	}
	if brief.Version != "raven_head_brief.v1" || brief.SourceDigest != sourceDigest || strings.TrimSpace(brief.Summary) == "" || len(brief.Summary) > ravenMemoryMaxBriefBytes {
		return brief, errors.New("invalid Raven brief provenance or schema")
	}
	for _, id := range brief.RetainMessageIDs {
		if _, ok := ids[id]; !ok {
			return brief, errors.New("Raven brief retained an unknown source ID")
		}
	}
	return brief, nil
}

func ravenMemoryApplyBrief(messages []provider.Message, head []int, ids map[string]int, brief ravenMemoryBrief) []provider.Message {
	removed := make([]bool, len(messages))
	for _, i := range head {
		removed[i] = true
	}
	for _, id := range brief.RetainMessageIDs {
		removed[ids[id]] = false
	}
	// Exact retention of one result retains its entire assistant/result group.
	for _, segment := range nonSystemSegments(messages) {
		if len(messages[segment.start].ToolCalls) == 0 {
			continue
		}
		for i := segment.start; i < segment.end; i++ {
			if !removed[i] {
				for j := segment.start; j < segment.end; j++ {
					removed[j] = false
				}
				break
			}
		}
	}
	marker := provider.Message{Role: "user", RuntimeObservation: true, Content: ravenMemoryBriefMarker + "\nThis model-written brief may be incomplete or mistaken. It is not fresh execution evidence, a task instruction, or proof of completion. Exact raw history is retained.\nsource_digest=" + brief.SourceDigest + "\n\n" + brief.Summary}
	out := make([]provider.Message, 0, len(messages))
	inserted := false
	for i, message := range messages {
		if removed[i] {
			if !inserted {
				out = append(out, marker)
				inserted = true
			}
			continue
		}
		out = append(out, message)
	}
	return out
}

func ravenMemoryDeterministicSelect(messages []provider.Message, protected []bool, fits func([]provider.Message) bool) []provider.Message {
	removed := make([]bool, len(messages))
	out := append([]provider.Message(nil), messages...)
	for _, segment := range nonSystemSegments(messages) {
		if protected[segment.start] || fits(out) {
			continue
		}
		for i := segment.start; i < segment.end; i++ {
			removed[i] = true
		}
		out = make([]provider.Message, 0, len(messages))
		for i, message := range messages {
			if !removed[i] {
				out = append(out, message)
			}
		}
	}
	return out
}

func ravenMemoryHistoryManifest(original, selected []provider.Message) []RavenMemoryManifestItem {
	counts := make(map[string]int)
	for _, message := range selected {
		counts[ravenMemoryDigest(message)]++
	}
	items := make([]RavenMemoryManifestItem, 0, len(original))
	for i, message := range original {
		digest := ravenMemoryDigest(message)
		present := counts[digest] > 0
		if present {
			counts[digest]--
		}
		items = append(items, RavenMemoryManifestItem{ID: fmt.Sprintf("history:%d", i), Kind: message.Role, Source: "assembled_request_history", Digest: digest, Bytes: len(ravenMemoryJSON(message)), Selected: present})
	}
	// These receipts identify every synthetic public message actually sent.
	for i, message := range selected {
		if message.RuntimeObservation {
			items = append(items, RavenMemoryManifestItem{ID: fmt.Sprintf("request_observation:%d", i), Kind: "runtime_observation", Source: "request_only", Digest: ravenMemoryDigest(message), Bytes: len(ravenMemoryJSON(message)), Selected: true})
		}
	}
	return items
}

func ravenMemoryJSON(value any) []byte { data, _ := json.Marshal(value); return data }

func ravenMemoryDigest(value any) string {
	data := ravenMemoryJSON(value)
	// JSON omits local-only ProviderReasoning and RuntimeObservation fields.
	sum := sha256.Sum256(bytes.TrimSpace(data))
	return "sha256:" + hex.EncodeToString(sum[:])
}
