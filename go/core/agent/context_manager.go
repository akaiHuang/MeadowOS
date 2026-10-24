package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const defaultMemoryBudgetBytes = 16 * 1024
const defaultHistoryBudgetBytes = 512 * 1024
const defaultPreserveRecentMessages = 12
const defaultVisualTokenEstimate = 2048
const messageProtocolTokenEstimate = 8

const compactedToolResultMarker = "[Meadow cold-prefill compacted tool result]"
const duplicateToolResultMarker = "[Meadow duplicate tool result reference]"
const conversationContinuityMarker = "# Conversation continuity"

var ErrPromptContextLimit = fmt.Errorf("model prompt exceeds the active context budget")

// ContextPolicy separates user-global memory from repository-local
// instructions. Project instructions are authoritative for every task in that
// workspace; explicitly scoped global memory is included only for a matching
// task so an unrelated persona/workflow cannot pollute the agent prefix.
type ContextPolicy struct {
	GlobalMemory    string
	ProjectMemory   string
	MaxMemoryBytes  int
	MaxHistoryBytes int
	// MaxHistoryTokens bounds system and conversation messages using a
	// conservative local estimate. Tool schemas and generation are reserved by
	// the product context budget before this value is assigned. Zero keeps the
	// legacy byte-only behavior.
	MaxHistoryTokens       int
	PreserveRecentMessages int
	// MaxToolResultBytes bounds one observation in the model-facing transcript.
	// Zero disables this guard. The canonical transcript and UI/tool callbacks
	// retain the complete result; only the request copy is compacted.
	MaxToolResultBytes  int
	MaxToolResultsBytes int
	// CompleteObservationTools names independently bounded exact contracts.
	// They bypass lossy per-result/aggregate excerpting, like exact file reads,
	// while duplicate addressing and whole-history/provider limits still apply.
	// Product composition derives this list from registered tool capabilities.
	CompleteObservationTools []string
	// MinDuplicateToolResultBytes content-addresses older successful results
	// only when the same tool, canonical arguments, and exact output recur.
	// The newest matching observation remains verbatim. Zero disables it.
	MinDuplicateToolResultBytes int
	// MaxMutationArgumentBytes replaces successfully-persisted file payloads
	// with content-addressed references in the model-facing copy. The canonical
	// transcript and artifact retain the exact bytes. Zero disables this
	// optimization.
	MaxMutationArgumentBytes int
}

type ContextSelection struct {
	Memory                         string
	GlobalIncluded                 bool
	ProjectIncluded                bool
	GlobalReason                   string
	Bytes                          int
	HistoryCompacted               bool
	HistoryInputMessages           int
	HistoryOutputMessages          int
	HistoryDroppedMessages         int
	HistorySupersededFailures      int
	HistoryInputBytes              int
	HistoryOutputBytes             int
	HistoryInputTokens             int
	HistoryOutputTokens            int
	HistoryTokenBudget             int
	ToolResultsCompacted           int
	ToolResultInputBytes           int
	ToolResultOutputBytes          int
	DuplicateToolResultsCompacted  int
	DuplicateToolResultInputBytes  int
	DuplicateToolResultOutputBytes int
	MutationArgumentsCompacted     int
	MutationArgumentInputBytes     int
	MutationArgumentOutputBytes    int
	ContinuityPreserved            bool
}

type ContextManager struct {
	policy ContextPolicy
}

func NewContextManager(policy ContextPolicy) *ContextManager {
	// Own the collection: independent managers and callers must not be able to
	// change another session's exact-observation policy through a shared slice.
	policy.CompleteObservationTools = append([]string(nil), policy.CompleteObservationTools...)
	if policy.MaxMemoryBytes <= 0 {
		policy.MaxMemoryBytes = defaultMemoryBudgetBytes
	}
	if policy.MaxHistoryBytes <= 0 {
		policy.MaxHistoryBytes = defaultHistoryBudgetBytes
	}
	if policy.PreserveRecentMessages <= 0 {
		policy.PreserveRecentMessages = defaultPreserveRecentMessages
	}
	return &ContextManager{policy: policy}
}

// WithHistoryBounds keeps the selected global/project memory while returning
// an independent manager with a smaller active transcript. Model-specific
// harnesses can control TTFT without changing another model family's history.
func (m *ContextManager) WithHistoryBounds(
	maxHistoryBytes, preserveRecentMessages int,
) *ContextManager {
	policy := ContextPolicy{}
	if m != nil {
		policy = m.policy
	}
	policy.MaxHistoryBytes = maxHistoryBytes
	policy.PreserveRecentMessages = preserveRecentMessages
	return NewContextManager(policy)
}

// WithHistoryTokenBounds adds a physical-runtime-aware token ceiling while
// preserving the existing byte, memory, and tool-result policies. Images and
// rendered PDF pages receive a conservative fixed charge so old text history
// is removed before a multimodal request reaches the provider.
func (m *ContextManager) WithHistoryTokenBounds(maxHistoryTokens int) *ContextManager {
	policy := ContextPolicy{}
	if m != nil {
		policy = m.policy
	}
	policy.MaxHistoryTokens = maxHistoryTokens
	return NewContextManager(policy)
}

// WithoutMemory returns an independent manager that omits global and
// repository instructions while retaining the same compaction policy. Narrow
// read-only workers use it when project coding conventions are unrelated to
// their delegated task and would consume scarce small-model prefill budget.
func (m *ContextManager) WithoutMemory() *ContextManager {
	policy := ContextPolicy{}
	if m != nil {
		policy = m.policy
	}
	policy.GlobalMemory = ""
	policy.ProjectMemory = ""
	return NewContextManager(policy)
}

// WithToolResultBounds returns an independent manager that limits any single
// non-exact observation before recent-message preservation is applied. This is
// separate from the total history budget because the newest tool result is
// deliberately protected by Compact and could otherwise exceed the model's
// entire context window by itself.
func (m *ContextManager) WithToolResultBounds(maxToolResultBytes, maxToolResultsBytes int) *ContextManager {
	policy := ContextPolicy{}
	if m != nil {
		policy = m.policy
	}
	policy.MaxToolResultBytes = maxToolResultBytes
	policy.MaxToolResultsBytes = maxToolResultsBytes
	return NewContextManager(policy)
}

// WithCompleteObservationTools replaces exact-contract capabilities in an
// independent manager, preserving memory and all existing context ceilings.
// Replacement permits a new registry to revoke a former tool's capability.
func (m *ContextManager) WithCompleteObservationTools(toolNames []string) *ContextManager {
	policy := ContextPolicy{}
	if m != nil {
		policy = m.policy
	}
	policy.CompleteObservationTools = toolNames
	return NewContextManager(policy)
}

// WithDuplicateToolResultBounds returns an independent manager that replaces
// older byte-identical successful observations with a stable reference. It is
// deliberately exact rather than semantic: different paths, URLs, arguments,
// failures, or even one changed byte remain separate model evidence.
func (m *ContextManager) WithDuplicateToolResultBounds(minDuplicateBytes int) *ContextManager {
	policy := ContextPolicy{}
	if m != nil {
		policy = m.policy
	}
	policy.MinDuplicateToolResultBytes = minDuplicateBytes
	return NewContextManager(policy)
}

// WithMutationArgumentBounds returns an independent manager that keeps large
// successful write payloads in the artifact store and sends only a stable
// content-addressed reference on later model turns. The reference is safe for
// prefix caching and tells the model to read the artifact before editing it.
func (m *ContextManager) WithMutationArgumentBounds(maxMutationArgumentBytes int) *ContextManager {
	policy := ContextPolicy{}
	if m != nil {
		policy = m.policy
	}
	policy.MaxMutationArgumentBytes = maxMutationArgumentBytes
	return NewContextManager(policy)
}

// SelectMemory applies the explicit memory policy without model inference or
// side effects, so the same task and memory files always yield the same prefix.
func (m *ContextManager) SelectMemory(task string) ContextSelection {
	if m == nil {
		return ContextSelection{}
	}
	global := strings.TrimSpace(m.policy.GlobalMemory)
	project := strings.TrimSpace(m.policy.ProjectMemory)
	selection := ContextSelection{ProjectIncluded: project != ""}
	if global != "" {
		if scope, scoped := explicitMemoryScope(global); scoped {
			selection.GlobalIncluded = scopeMatchesTask(scope, task)
			if selection.GlobalIncluded {
				selection.GlobalReason = "explicit_scope_match"
			} else {
				selection.GlobalReason = "explicit_scope_mismatch"
			}
		} else {
			// Unscoped memory preserves the legacy interpretation: it contains
			// general user preferences that should apply to every task.
			selection.GlobalIncluded = true
			selection.GlobalReason = "unscoped_global"
		}
	}
	if !selection.GlobalIncluded {
		global = ""
	}
	selection.Memory = boundedMemory(global, project, m.policy.MaxMemoryBytes)
	selection.Bytes = len(selection.Memory)
	return selection
}

// Apply appends selected memory to a request's existing system prefix while
// preserving canonical message order and all tool-call/result pairs.
func (m *ContextManager) Apply(messages []provider.Message, task string) ([]provider.Message, ContextSelection) {
	out, selection := m.Compact(messages)
	memorySelection := m.SelectMemory(task)
	selection.Memory = memorySelection.Memory
	selection.GlobalIncluded = memorySelection.GlobalIncluded
	selection.ProjectIncluded = memorySelection.ProjectIncluded
	selection.GlobalReason = memorySelection.GlobalReason
	selection.Bytes = memorySelection.Bytes
	if selection.Memory == "" {
		return out, selection
	}
	for index := range out {
		if out[index].Role != "system" {
			continue
		}
		base := strings.TrimSpace(out[index].Content)
		if base == "" {
			out[index].Content = selection.Memory
		} else {
			out[index].Content = base + "\n\n---\n\n" + selection.Memory
		}
		return m.boundAppliedMemory(out, selection)
	}
	out = append([]provider.Message{{Role: "system", Content: selection.Memory}}, out...)
	return m.boundAppliedMemory(out, selection)
}

// boundAppliedMemory accounts for memory after it has been injected into the
// system prefix. Without this second pass, a transcript at the history ceiling
// could exceed the request budget solely because project memory was appended
// after compaction.
func (m *ContextManager) boundAppliedMemory(
	messages []provider.Message, selection ContextSelection,
) ([]provider.Message, ContextSelection) {
	if m == nil {
		return messages, selection
	}
	bounded := compactMessages(messages, m.policy.MaxHistoryBytes, m.policy.PreserveRecentMessages)
	bounded = compactMessagesByTokens(bounded, m.policy.MaxHistoryTokens, m.policy.PreserveRecentMessages)
	if dropped := len(messages) - len(bounded); dropped > 0 {
		selection.HistoryCompacted = true
		selection.HistoryDroppedMessages += dropped
		selection.HistoryOutputMessages -= dropped
		if selection.HistoryOutputMessages < 0 {
			selection.HistoryOutputMessages = 0
		}
	}
	return bounded, selection
}

// Compact bounds resumed conversation history without summarizing or
// interpreting content. System messages and the active turn beginning at the
// latest ordinary user message are retained; assistant/tool call groups remain
// atomic, so compaction never creates a new orphaned tool result or drops the
// task anchor for retained tool work.
func (m *ContextManager) Compact(messages []provider.Message) ([]provider.Message, ContextSelection) {
	selection := ContextSelection{
		HistoryInputMessages: len(messages),
		HistoryInputBytes:    messagesBytes(messages),
		HistoryInputTokens:   messagesEstimatedTokens(messages),
	}
	if m == nil {
		selection.HistoryOutputMessages = len(messages)
		selection.HistoryOutputBytes = selection.HistoryInputBytes
		selection.HistoryOutputTokens = selection.HistoryInputTokens
		return messages, selection
	}
	boundedMessages, mutationStats := compactSuccessfulWriteArguments(
		messages, m.policy.MaxMutationArgumentBytes,
	)
	boundedMessages, duplicateStats := compactDuplicateToolResults(
		boundedMessages, m.policy.MinDuplicateToolResultBytes,
	)
	// Tool-result compaction operates on the already content-addressed request
	// copy. All transforms remain isolated from the canonical log.
	boundedMessages, toolStats := compactOversizedToolResults(
		boundedMessages, m.policy.MaxToolResultBytes, m.policy.MaxToolResultsBytes,
		m.policy.CompleteObservationTools,
	)
	boundedMessages, supersededFailures := dropSupersededFailedToolSegments(boundedMessages)
	selection.HistoryTokenBudget = m.policy.MaxHistoryTokens
	compact := func(input []provider.Message) []provider.Message {
		out := compactMessages(input, m.policy.MaxHistoryBytes, m.policy.PreserveRecentMessages)
		return compactMessagesByTokens(out, m.policy.MaxHistoryTokens, m.policy.PreserveRecentMessages)
	}
	out := compact(boundedMessages)
	if len(out) < len(boundedMessages) {
		if continuity := buildConversationContinuity(messages); continuity != "" {
			withContinuity, synthetic := injectConversationContinuity(boundedMessages, continuity)
			bytesFit := m.policy.MaxHistoryBytes <= 0 || systemMessagesBytes(withContinuity) <= m.policy.MaxHistoryBytes
			tokensFit := m.policy.MaxHistoryTokens <= 0 || systemMessagesEstimatedTokens(withContinuity) <= m.policy.MaxHistoryTokens
			if bytesFit && tokensFit {
				candidate := compact(withContinuity)
				candidateBytesFit := m.policy.MaxHistoryBytes <= 0 || messagesBytes(candidate) <= m.policy.MaxHistoryBytes
				candidateTokensFit := m.policy.MaxHistoryTokens <= 0 || messagesEstimatedTokens(candidate) <= m.policy.MaxHistoryTokens
				if candidateBytesFit && candidateTokensFit {
					out = candidate
					selection.ContinuityPreserved = true
					selection.HistoryDroppedMessages = len(messages) - (len(out) - synthetic)
				}
			}
		}
	}
	selection.HistoryOutputMessages = len(out)
	selection.HistoryOutputBytes = messagesBytes(out)
	selection.HistoryOutputTokens = messagesEstimatedTokens(out)
	if !selection.ContinuityPreserved {
		selection.HistoryDroppedMessages = len(messages) - len(out)
	}
	selection.HistorySupersededFailures = supersededFailures
	selection.ToolResultsCompacted = toolStats.count
	selection.ToolResultInputBytes = toolStats.inputBytes
	selection.ToolResultOutputBytes = toolStats.outputBytes
	selection.DuplicateToolResultsCompacted = duplicateStats.count
	selection.DuplicateToolResultInputBytes = duplicateStats.inputBytes
	selection.DuplicateToolResultOutputBytes = duplicateStats.outputBytes
	selection.MutationArgumentsCompacted = mutationStats.count
	selection.MutationArgumentInputBytes = mutationStats.inputBytes
	selection.MutationArgumentOutputBytes = mutationStats.outputBytes
	selection.HistoryCompacted = selection.HistoryDroppedMessages > 0 ||
		toolStats.count > 0 || duplicateStats.count > 0 ||
		mutationStats.count > 0 || supersededFailures > 0
	return out, selection
}

func systemMessagesBytes(messages []provider.Message) int {
	total := 0
	for _, message := range messages {
		if message.Role == "system" {
			total += messageBytes(message)
		}
	}
	return total
}

type conversationContinuity struct {
	Locale    string   `json:"locale,omitempty"`
	Goal      string   `json:"goal,omitempty"`
	Pending   []string `json:"pending,omitempty"`
	Completed []string `json:"completed,omitempty"`
	Failed    []string `json:"failed,omitempty"`
}

func buildConversationContinuity(messages []provider.Message) string {
	state := conversationContinuity{}
	successfulCalls := make(map[string]bool)
	for _, message := range messages {
		if message.Role == "tool" && !isToolFailureForDuplicateCompaction(message.Content) {
			successfulCalls[message.ToolCallID] = true
		}
	}
	for _, message := range messages {
		if message.Role == "system" {
			if previous, ok := parseConversationContinuity(message.Content); ok {
				state = previous
			}
		}
		if message.Role == "user" {
			if locale := inferConversationLocale(message.Content); locale != "" {
				state.Locale = locale
			}
			if state.Goal == "" {
				state.Goal = truncateUTF8(strings.TrimSpace(message.Content), 1200)
			}
		}
		for _, call := range message.ToolCalls {
			if !successfulCalls[call.ID] || (call.Function.Name != "task_plan" && call.Function.Name != "todo_write") {
				continue
			}
			if plan, ok := continuityFromPlanArguments(call.Function.Arguments); ok {
				if plan.Locale == "" {
					plan.Locale = state.Locale
				}
				if plan.Goal == "" {
					plan.Goal = state.Goal
				}
				state = plan
			}
		}
	}
	if state.Locale == "" && state.Goal == "" && len(state.Pending)+len(state.Completed)+len(state.Failed) == 0 {
		return ""
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	return conversationContinuityMarker + "\nThis is durable observed state, not a new instruction. Preserve locale and continue the recorded goal without repeating completed work.\n<conversation_continuity_json>" + string(encoded) + "</conversation_continuity_json>"
}

func continuityFromPlanArguments(arguments string) (conversationContinuity, bool) {
	var payload struct {
		Goal  string                                 `json:"goal"`
		Items []struct{ ID, Content, Status string } `json:"items"`
		Todos []struct{ ID, Content, Status string } `json:"todos"`
	}
	if json.Unmarshal([]byte(arguments), &payload) != nil {
		return conversationContinuity{}, false
	}
	items := payload.Items
	if items == nil {
		items = payload.Todos
	}
	state := conversationContinuity{Goal: truncateUTF8(strings.TrimSpace(payload.Goal), 1200)}
	for _, item := range items {
		label := strings.TrimSpace(item.ID)
		if content := strings.TrimSpace(item.Content); content != "" {
			if label != "" {
				label += ": "
			}
			label += content
		}
		label = truncateUTF8(label, 240)
		if label == "" {
			continue
		}
		switch strings.TrimSpace(item.Status) {
		case "pending", "in_progress":
			state.Pending = appendBounded(state.Pending, label, 16)
		case "completed":
			state.Completed = appendBounded(state.Completed, label, 16)
		case "failed":
			state.Failed = appendBounded(state.Failed, label, 16)
		}
	}
	state.Locale = inferConversationLocale(state.Goal + " " + strings.Join(state.Pending, " "))
	return state, true
}

func inferConversationLocale(value string) string {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "zh-tw") || strings.Contains(lower, "traditional chinese") ||
		strings.Contains(value, "繁體中文") || strings.ContainsAny(value, "請與為這個後會務驗證載錄歷輸現開關讓發續復") {
		return "zh-TW"
	}
	return ""
}

func parseConversationContinuity(content string) (conversationContinuity, bool) {
	const open = "<conversation_continuity_json>"
	const close = "</conversation_continuity_json>"
	start := strings.LastIndex(content, open)
	end := strings.LastIndex(content, close)
	if start < 0 || end <= start {
		return conversationContinuity{}, false
	}
	var state conversationContinuity
	if json.Unmarshal([]byte(content[start+len(open):end]), &state) != nil {
		return conversationContinuity{}, false
	}
	return state, true
}

func injectConversationContinuity(messages []provider.Message, continuity string) ([]provider.Message, int) {
	out := append([]provider.Message(nil), messages...)
	for index := range out {
		if out[index].Role != "system" {
			continue
		}
		base := out[index].Content
		if marker := strings.Index(base, conversationContinuityMarker); marker >= 0 {
			base = strings.TrimSpace(base[:marker])
		}
		if base == "" {
			out[index].Content = continuity
		} else {
			out[index].Content = base + "\n\n" + continuity
		}
		return out, 0
	}
	return append([]provider.Message{{Role: "system", Content: continuity}}, out...), 1
}

type duplicateToolResultCompactionStats struct {
	count       int
	inputBytes  int
	outputBytes int
}

type toolCallContext struct {
	name      string
	arguments string
}

type duplicateToolResultKey [sha256.Size]byte

type duplicateToolResultCandidate struct {
	key         duplicateToolResultKey
	contentHash [sha256.Size]byte
	toolName    string
	content     string
}

func compactDuplicateToolResults(
	messages []provider.Message, minResultBytes int,
) ([]provider.Message, duplicateToolResultCompactionStats) {
	out := append([]provider.Message(nil), messages...)
	stats := duplicateToolResultCompactionStats{}
	if minResultBytes <= 0 {
		return out, stats
	}

	callContexts := make(map[string]toolCallContext)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			callContexts[call.ID] = toolCallContext{
				name:      call.Function.Name,
				arguments: canonicalToolArguments(call.Function.Arguments),
			}
		}
	}

	candidates := make(map[int]duplicateToolResultCandidate)
	latestByKey := make(map[duplicateToolResultKey]int)
	for index, message := range messages {
		if message.Role != "tool" || len(message.Content) < minResultBytes ||
			isToolFailureForDuplicateCompaction(message.Content) {
			continue
		}
		call, ok := callContexts[message.ToolCallID]
		if !ok || strings.TrimSpace(call.name) == "" {
			continue
		}
		key := exactToolResultKey(call, message.Content)
		candidate := duplicateToolResultCandidate{
			key: key, contentHash: sha256.Sum256([]byte(message.Content)),
			toolName: call.name, content: message.Content,
		}
		candidates[index] = candidate
		latestByKey[key] = index
	}

	for index, candidate := range candidates {
		if latestByKey[candidate.key] == index {
			continue
		}
		reference := fmt.Sprintf(
			"%s\nExact successful %s output for the same arguments is retained verbatim in the newest matching observation.\nContent SHA-256: %x\nOriginal bytes: %d.\nUse the newest full result; never copy this marker into an artifact.",
			duplicateToolResultMarker, candidate.toolName,
			candidate.contentHash, len(candidate.content),
		)
		if len(reference) >= len(candidate.content) {
			continue
		}
		out[index].Content = reference
		stats.count++
		stats.inputBytes += len(candidate.content)
		stats.outputBytes += len(reference)
	}
	return out, stats
}

func canonicalToolArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}
	var value any
	if json.Unmarshal([]byte(trimmed), &value) != nil {
		return trimmed
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return trimmed
	}
	return string(encoded)
}

func exactToolResultKey(call toolCallContext, content string) duplicateToolResultKey {
	digest := sha256.New()
	_, _ = digest.Write([]byte(call.name))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(call.arguments))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(content))
	var key duplicateToolResultKey
	copy(key[:], digest.Sum(nil))
	return key
}

func isToolFailureForDuplicateCompaction(content string) bool {
	trimmed := strings.TrimSpace(content)
	return strings.HasPrefix(trimmed, "TOOL_ERROR ") ||
		strings.HasPrefix(trimmed, "ERROR:") ||
		strings.HasPrefix(trimmed, "[error]")
}

type mutationArgumentCompactionStats struct {
	count       int
	inputBytes  int
	outputBytes int
}

func compactSuccessfulWriteArguments(
	messages []provider.Message, maxPayloadBytes int,
) ([]provider.Message, mutationArgumentCompactionStats) {
	out := append([]provider.Message(nil), messages...)
	stats := mutationArgumentCompactionStats{}
	if maxPayloadBytes <= 0 {
		return out, stats
	}
	successfulWrites := make(map[string]bool)
	for _, message := range messages {
		if message.Role == "tool" && successfulWriteResult(message.Content) {
			successfulWrites[message.ToolCallID] = true
		}
	}
	// Keep the newest successful single-file mutation hot. Small agent models use
	// the just-written source as their immediate working set; replacing it at once
	// can save prompt bytes but provoke a list/read round-trip on the next turn.
	// A multi-file transaction is different: retaining every complete sibling
	// duplicates the whole artifact set. Keep one interaction surface (HTML when
	// present) hot and content-address the other large siblings. Older persisted
	// writes are cold and safe to content-address completely.
	latestSuccessfulWriteMessage := -1
	for messageIndex, message := range messages {
		for _, call := range message.ToolCalls {
			if isPersistedWriteTool(call.Function.Name) && successfulWrites[call.ID] {
				latestSuccessfulWriteMessage = messageIndex
				break
			}
		}
	}
	for messageIndex, message := range messages {
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			continue
		}
		var cloned []provider.ToolCall
		for callIndex, call := range message.ToolCalls {
			if !isPersistedWriteTool(call.Function.Name) || !successfulWrites[call.ID] {
				continue
			}
			if messageIndex == latestSuccessfulWriteMessage && call.Function.Name == "write_file" {
				continue
			}
			compacted, ok := compactMutationArguments(
				call.Function.Name, call.Function.Arguments, maxPayloadBytes,
				messageIndex == latestSuccessfulWriteMessage,
			)
			if !ok {
				continue
			}
			if cloned == nil {
				cloned = append([]provider.ToolCall(nil), message.ToolCalls...)
			}
			cloned[callIndex].Function.Arguments = compacted
			stats.count++
			stats.inputBytes += len(call.Function.Arguments)
			stats.outputBytes += len(compacted)
		}
		if cloned != nil {
			out[messageIndex].ToolCalls = cloned
		}
	}
	return out, stats
}

func isPersistedWriteTool(name string) bool {
	return name == "write_file" || name == "write_files"
}

func successfulWriteResult(content string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "Created ") ||
		strings.HasPrefix(content, "Overwrote ") ||
		strings.HasPrefix(content, "Unchanged ")
}

func compactWriteArguments(arguments string, maxPayloadBytes int) (string, bool) {
	var args map[string]any
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return arguments, false
	}
	content, ok := args["content"].(string)
	if !ok || len(content) <= maxPayloadBytes {
		return arguments, false
	}
	path, _ := args["file_path"].(string)
	args["content"] = compactedWritePayload(path, content)
	encoded, err := json.Marshal(args)
	if err != nil || len(encoded) >= len(arguments) {
		return arguments, false
	}
	return string(encoded), true
}

func compactMutationArguments(
	toolName, arguments string, maxPayloadBytes int, preserveHotSurface bool,
) (string, bool) {
	if toolName == "write_file" {
		return compactWriteArguments(arguments, maxPayloadBytes)
	}
	if toolName != "write_files" {
		return arguments, false
	}
	return compactWriteFilesArguments(arguments, maxPayloadBytes, preserveHotSurface)
}

func compactWriteFilesArguments(
	arguments string, maxPayloadBytes int, preserveHotSurface bool,
) (string, bool) {
	var args map[string]any
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return arguments, false
	}
	type fileArgument struct {
		owner      map[string]any
		pathKey    string
		contentKey string
	}
	files := make([]fileArgument, 0, 4)
	if nested, ok := args["files"].([]any); ok {
		for _, raw := range nested {
			if entry, valid := raw.(map[string]any); valid {
				files = append(files, fileArgument{owner: entry, pathKey: "file_path", contentKey: "content"})
			}
		}
	} else {
		for _, prefix := range []string{"first_file", "second_file"} {
			pathKey, contentKey := prefix+"_path", prefix+"_content"
			if _, pathOK := args[pathKey].(string); pathOK {
				if _, contentOK := args[contentKey].(string); contentOK {
					files = append(files, fileArgument{owner: args, pathKey: pathKey, contentKey: contentKey})
				}
			}
		}
	}
	if len(files) == 0 {
		return arguments, false
	}
	preserveIndex := -1
	if preserveHotSurface {
		preserveIndex = 0
		for index, entry := range files {
			path, _ := entry.owner[entry.pathKey].(string)
			lower := strings.ToLower(strings.TrimSpace(path))
			if strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm") {
				preserveIndex = index
				break
			}
		}
	}
	changed := false
	for index, entry := range files {
		if index == preserveIndex {
			continue
		}
		content, ok := entry.owner[entry.contentKey].(string)
		if !ok || len(content) <= maxPayloadBytes {
			continue
		}
		path, _ := entry.owner[entry.pathKey].(string)
		entry.owner[entry.contentKey] = compactedWritePayload(path, content)
		changed = true
	}
	if !changed {
		return arguments, false
	}
	encoded, err := json.Marshal(args)
	if err != nil || len(encoded) >= len(arguments) {
		return arguments, false
	}
	return string(encoded), true
}

func compactedWritePayload(path, content string) string {
	digest := sha256.Sum256([]byte(content))
	return fmt.Sprintf(
		"%s\nExact write payload is stored once in artifact %s.\nSHA-256: %x\nOriginal bytes: %d.\nUse read_file before editing; never write this marker.",
		compactedToolResultMarker, path, digest, len(content),
	)
}

type toolResultCompactionStats struct {
	count       int
	inputBytes  int
	outputBytes int
}

func compactOversizedToolResults(
	messages []provider.Message, singleBudget, totalBudget int,
	completeObservationTools []string,
) ([]provider.Message, toolResultCompactionStats) {
	out := append([]provider.Message(nil), messages...)
	stats := toolResultCompactionStats{}
	if singleBudget <= 0 {
		return out, stats
	}
	toolNames := make(map[string]string)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			toolNames[call.ID] = call.Function.Name
		}
	}
	completeNames := make(map[string]bool, len(completeObservationTools))
	for _, name := range completeObservationTools {
		if name != "" {
			completeNames[name] = true
		}
	}
	eligible := make([]int, 0)
	for index, message := range messages {
		if message.Role != "tool" {
			continue
		}
		// read_file is an exact mutation source. Its Qwen-specific tool already
		// returns an independently bounded range and tells the model how to page;
		// head/tail rewriting here could turn a context marker into file content.
		name := toolNames[message.ToolCallID]
		if name == "read_file" || completeNames[name] {
			continue
		}
		eligible = append(eligible, index)
	}
	budgets := toolResultBudgets(messages, eligible, singleBudget, totalBudget)
	for position, index := range eligible {
		message := messages[index]
		out[index].Content = compactToolResultForContext(message.Content, budgets[position])
		if out[index].Content == message.Content {
			continue
		}
		stats.count++
		stats.inputBytes += len(message.Content)
		stats.outputBytes += len(out[index].Content)
	}
	return out, stats
}

func toolResultBudgets(
	messages []provider.Message, eligible []int, singleBudget, totalBudget int,
) []int {
	budgets := make([]int, len(eligible))
	for index := range budgets {
		budgets[index] = singleBudget
	}
	if len(eligible) == 0 || totalBudget <= 0 {
		return budgets
	}
	desired := 0
	for _, messageIndex := range eligible {
		desired += min(len(messages[messageIndex].Content), singleBudget)
	}
	if desired <= totalBudget {
		return budgets
	}
	if len(eligible) == 1 {
		budgets[0] = min(singleBudget, totalBudget)
		return budgets
	}
	// The newest observation is normally the result of a narrower follow-up
	// query, so it receives 75% of the aggregate evidence budget. Older results
	// share the remaining 25% and retain enough head/tail context to explain why
	// the follow-up happened.
	desiredByResult := make([]int, len(eligible))
	for index, messageIndex := range eligible {
		desiredByResult[index] = min(len(messages[messageIndex].Content), singleBudget)
		budgets[index] = 0
	}
	latest := len(eligible) - 1
	budgets[latest] = min(desiredByResult[latest], totalBudget*3/4)
	remaining := totalBudget - budgets[latest]
	remaining = distributeToolResultBudget(
		budgets, desiredByResult, 0, latest, remaining,
	)
	if remaining > 0 {
		budgets[latest] += min(remaining, desiredByResult[latest]-budgets[latest])
		remaining = totalBudget
		for index := range budgets {
			remaining -= budgets[index]
		}
	}
	if remaining > 0 {
		_ = distributeToolResultBudget(
			budgets, desiredByResult, 0, len(budgets), remaining,
		)
	}
	return budgets
}

func distributeToolResultBudget(
	budgets, desired []int, start, end, remaining int,
) int {
	for remaining > 0 {
		active := 0
		for index := start; index < end; index++ {
			if budgets[index] < desired[index] {
				active++
			}
		}
		if active == 0 {
			break
		}
		share := max(1, remaining/active)
		before := remaining
		for index := start; index < end && remaining > 0; index++ {
			need := desired[index] - budgets[index]
			if need <= 0 {
				continue
			}
			grant := min(need, share, remaining)
			budgets[index] += grant
			remaining -= grant
		}
		if remaining == before {
			break
		}
	}
	return remaining
}

func compactToolResultForContext(content string, budget int) string {
	if len(content) <= budget {
		return content
	}
	if budget <= 0 {
		return ""
	}
	marker := strings.Join([]string{
		compactedToolResultMarker,
		"Reason: one observation exceeded the active model-context budget.",
		"This is context only. Never copy it into write_file/edit; request a narrower selector, page, range, or query before exact use.",
		"Original bytes: " + strconv.Itoa(len(content)) + ".",
	}, "\n")
	if len(marker) >= budget {
		if len(compactedToolResultMarker) <= budget {
			return compactedToolResultMarker
		}
		return ""
	}
	remaining := budget - len(marker) - len("\nHead:\n\nTail:\n")
	if remaining < 2 {
		return marker
	}
	headBudget := remaining * 3 / 5
	tailBudget := remaining - headBudget
	head := validUTF8ContextPrefix(content, headBudget)
	tail := validUTF8ContextSuffix(content, tailBudget)
	return marker + "\nHead:\n" + strings.TrimSpace(head) +
		"\nTail:\n" + strings.TrimSpace(tail)
}

func validUTF8ContextPrefix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func validUTF8ContextSuffix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	start := len(value) - maxBytes
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

type messageSegment struct {
	start  int
	end    int
	bytes  int
	tokens int
}

// dropSupersededFailedToolSegments removes only an older model-facing copy of
// the same typed tool failure. The canonical transcript remains untouched and
// the newest failed call/result pair stays visible for recovery. Arguments are
// deliberately excluded from the signature: a model can make a cosmetically
// different call that still violates the same schema paths, and retaining both
// large payloads can crowd the actionable error out of a small context window.
func dropSupersededFailedToolSegments(messages []provider.Message) ([]provider.Message, int) {
	segments := nonSystemSegments(messages)
	seen := make(map[string]bool)
	dropped := make([]bool, len(messages))
	droppedFailures := 0
	for index := len(segments) - 1; index >= 0; index-- {
		segment := segments[index]
		signature, ok := failedToolSegmentSignature(messages, segment)
		if !ok {
			continue
		}
		if !seen[signature] {
			seen[signature] = true
			continue
		}
		for messageIndex := segment.start; messageIndex < segment.end; messageIndex++ {
			dropped[messageIndex] = true
		}
		droppedFailures++
	}
	if droppedFailures == 0 {
		return append([]provider.Message(nil), messages...), 0
	}
	out := make([]provider.Message, 0, len(messages))
	for index, message := range messages {
		if !dropped[index] {
			out = append(out, message)
		}
	}
	return out, droppedFailures
}

func failedToolSegmentSignature(messages []provider.Message, segment messageSegment) (string, bool) {
	if segment.start < 0 || segment.end > len(messages) || segment.end-segment.start != 2 {
		return "", false
	}
	callMessage := messages[segment.start]
	resultMessage := messages[segment.start+1]
	if callMessage.Role != "assistant" || len(callMessage.ToolCalls) != 1 ||
		resultMessage.Role != "tool" ||
		resultMessage.ToolCallID != callMessage.ToolCalls[0].ID {
		return "", false
	}
	result := strings.TrimSpace(resultMessage.Content)
	if !strings.HasPrefix(result, "TOOL_ERROR ") {
		return "", false
	}
	signature := normalizeFailureSignature(result)
	if signature == "" {
		return "", false
	}
	return callMessage.ToolCalls[0].Function.Name + "|" + signature, true
}

func compactMessages(messages []provider.Message, budget, preserveRecent int) []provider.Message {
	if len(messages) == 0 {
		return messages
	}
	inputBytes := messagesBytes(messages)
	if budget <= 0 || inputBytes <= budget {
		return append([]provider.Message(nil), messages...)
	}
	selected := make([]bool, len(messages))
	usedBytes := 0
	for index, message := range messages {
		if message.Role == "system" {
			selected[index] = true
			usedBytes += messageBytes(message)
		}
	}
	segments := nonSystemSegments(messages)
	// The latest ordinary user message starts the active turn. Keep it and every
	// following atomic segment together: retaining a trailing assistant/tool pair
	// after dropping its only task anchor creates an invalid model request. This
	// mandatory suffix may exceed the history budget, just as the former newest-
	// segment fallback could; request admission remains responsible for reporting
	// an indivisible working set that does not fit.
	protectedStart := activeTurnSegmentStart(messages, segments)
	protectedMessages := 0
	if protectedStart < len(segments) {
		for index := protectedStart; index < len(segments); index++ {
			segment := segments[index]
			selectSegment(selected, segment)
			usedBytes += segment.bytes
			protectedMessages += segment.end - segment.start
		}
	} else if len(segments) > 0 {
		// Internal/tool-only fixtures have no user task anchor. Preserve the former
		// generic fallback by treating the newest atomic segment as indivisible.
		protectedStart = len(segments) - 1
		segment := segments[protectedStart]
		selectSegment(selected, segment)
		usedBytes += segment.bytes
		protectedMessages += segment.end - segment.start
	}
	// preserveRecent is a recency preference, never permission to exceed the
	// active prompt budget. Extend the mandatory suffix only while it still fits.
	for protectedStart > 0 && protectedMessages < preserveRecent {
		index := protectedStart - 1
		segment := segments[index]
		if usedBytes+segment.bytes > budget {
			break
		}
		selectSegment(selected, segment)
		usedBytes += segment.bytes
		protectedMessages += segment.end - segment.start
		protectedStart = index
	}
	// After satisfying the preferred recent suffix, admit older contiguous
	// segments only while the same budget still has room.
	for index := protectedStart - 1; index >= 0; index-- {
		segment := segments[index]
		if usedBytes+segment.bytes > budget {
			break
		}
		selectSegment(selected, segment)
		usedBytes += segment.bytes
	}
	out := make([]provider.Message, 0, len(messages))
	for index, message := range messages {
		if selected[index] {
			out = append(out, message)
		}
	}
	return out
}

func nonSystemSegments(messages []provider.Message) []messageSegment {
	return nonSystemSegmentsWithEstimator(messages, messageEstimatedTokens)
}

func nonSystemSegmentsWithEstimator(messages []provider.Message, estimate func(provider.Message) int) []messageSegment {
	segments := make([]messageSegment, 0, len(messages))
	for index := 0; index < len(messages); {
		if messages[index].Role == "system" {
			index++
			continue
		}
		end := index + 1
		if messages[index].Role == "assistant" && len(messages[index].ToolCalls) > 0 {
			for end < len(messages) && messages[end].Role == "tool" {
				end++
			}
			// PrepareToolResultMedia emits a provider-only user envelope because
			// strict OpenAI-compatible servers reject image parts on role=tool.
			// Keep that envelope atomic with the assistant/tool exchange that
			// precedes it so compaction can never retain orphaned pixels.
			if end > index+1 && end < len(messages) && provider.IsToolResultMediaMessage(messages[end]) {
				end++
			}
		}
		segment := messageSegment{start: index, end: end}
		for messageIndex := index; messageIndex < end; messageIndex++ {
			segment.bytes += messageBytes(messages[messageIndex])
			segment.tokens += estimate(messages[messageIndex])
		}
		segments = append(segments, segment)
		index = end
	}
	return segments
}

func activeTurnSegmentStart(messages []provider.Message, segments []messageSegment) int {
	for segmentIndex := len(segments) - 1; segmentIndex >= 0; segmentIndex-- {
		segment := segments[segmentIndex]
		for messageIndex := segment.end - 1; messageIndex >= segment.start; messageIndex-- {
			if isOrdinaryUserMessage(messages[messageIndex]) {
				return segmentIndex
			}
		}
	}
	return len(segments)
}

func isOrdinaryUserMessage(message provider.Message) bool {
	if message.Role != "user" || message.RuntimeObservation || message.ToolCallID != "" || provider.IsToolResultMediaMessage(message) {
		return false
	}
	content := strings.TrimSpace(provider.MessageText(message))
	return !(strings.HasPrefix(content, "<tool_response>") && strings.HasSuffix(content, "</tool_response>"))
}

func hasOrdinaryUserMessage(messages []provider.Message) bool {
	for _, message := range messages {
		if isOrdinaryUserMessage(message) {
			return true
		}
	}
	return false
}

func compactMessagesByTokens(messages []provider.Message, budget, preserveRecent int) []provider.Message {
	return compactMessagesByTokensWithEstimator(messages, budget, preserveRecent, messageEstimatedTokens)
}

func compactMessagesByTokensWithEstimator(messages []provider.Message, budget, preserveRecent int, estimate func(provider.Message) int) []provider.Message {
	if len(messages) == 0 || budget <= 0 || messagesEstimatedTokensWithEstimator(messages, estimate) <= budget {
		return append([]provider.Message(nil), messages...)
	}
	selected := make([]bool, len(messages))
	usedTokens := 0
	for index, message := range messages {
		if message.Role == "system" {
			selected[index] = true
			usedTokens += estimate(message)
		}
	}
	segments := nonSystemSegmentsWithEstimator(messages, estimate)
	protectedStart := activeTurnSegmentStart(messages, segments)
	protectedMessages := 0
	if protectedStart < len(segments) {
		for index := protectedStart; index < len(segments); index++ {
			segment := segments[index]
			selectSegment(selected, segment)
			usedTokens += segment.tokens
			protectedMessages += segment.end - segment.start
		}
	} else if len(segments) > 0 {
		protectedStart = len(segments) - 1
		segment := segments[protectedStart]
		selectSegment(selected, segment)
		usedTokens += segment.tokens
		protectedMessages += segment.end - segment.start
	}
	for protectedStart > 0 && protectedMessages < preserveRecent {
		index := protectedStart - 1
		segment := segments[index]
		if usedTokens+segment.tokens > budget {
			break
		}
		selectSegment(selected, segment)
		usedTokens += segment.tokens
		protectedMessages += segment.end - segment.start
		protectedStart = index
	}
	for index := protectedStart - 1; index >= 0; index-- {
		segment := segments[index]
		if usedTokens+segment.tokens > budget {
			break
		}
		selectSegment(selected, segment)
		usedTokens += segment.tokens
	}
	out := make([]provider.Message, 0, len(messages))
	for index, message := range messages {
		if selected[index] {
			out = append(out, message)
		}
	}
	return out
}

func selectSegment(selected []bool, segment messageSegment) {
	for index := segment.start; index < segment.end; index++ {
		selected[index] = true
	}
}

func messagesBytes(messages []provider.Message) int {
	total := 0
	for _, message := range messages {
		total += messageBytes(message)
	}
	return total
}

func messageBytes(message provider.Message) int {
	total := len(message.Role) + len(message.Content) + len(message.ToolCallID)
	for _, call := range message.ToolCalls {
		total += len(call.ID) + len(call.Type) + len(call.Function.Name) + len(call.Function.Arguments)
	}
	return total
}

func systemMessagesEstimatedTokens(messages []provider.Message) int {
	total := 0
	for _, message := range messages {
		if message.Role == "system" {
			total += messageEstimatedTokens(message)
		}
	}
	return total
}

func messagesEstimatedTokens(messages []provider.Message) int {
	return messagesEstimatedTokensWithEstimator(messages, messageEstimatedTokens)
}

func messagesEstimatedTokensWithEstimator(messages []provider.Message, estimate func(provider.Message) int) int {
	total := 0
	for _, message := range messages {
		total += estimate(message)
	}
	return total
}

func messageEstimatedTokens(message provider.Message) int {
	total := messageProtocolTokenEstimate + provider.EstimateTextTokens(message.Role) +
		provider.EstimateTextTokens(provider.MessageText(message)) +
		provider.EstimateTextTokens(message.ToolCallID)
	for _, part := range message.ContentParts {
		if part.Type == "image_url" && part.ImageURL != nil && strings.TrimSpace(part.ImageURL.URL) != "" {
			total += defaultVisualTokenEstimate
		}
	}
	for _, call := range message.ToolCalls {
		total += messageProtocolTokenEstimate + provider.EstimateTextTokens(call.ID) +
			provider.EstimateTextTokens(call.Type) +
			provider.EstimateTextTokens(call.Function.Name) +
			provider.EstimateTextTokens(call.Function.Arguments)
	}
	return total
}

// EstimateRequestPromptTokens estimates the provider's actual wire prompt,
// including native tool schemas and conservative visual-token charges. Tagged
// streamed-tool requests already embed their schemas in a user transport
// instruction, and the provider removes request.Tools before serialization, so
// those schemas must not be charged a second time here. The runtime's exact
// tokenizer remains authoritative after admission.
func EstimateRequestPromptTokens(request provider.ChatRequest) int {
	total := messagesEstimatedTokens(request.Messages)
	for _, message := range request.Messages {
		total += provider.RequestReasoningTokenEstimate(request, message)
	}
	if len(request.Tools) > 0 && !provider.UsesStreamedToolTransport(request) {
		if encoded, err := json.Marshal(request.Tools); err == nil {
			total += provider.EstimateTextTokens(string(encoded))
		}
		total += len(request.Tools) * messageProtocolTokenEstimate
	}
	return total + messageProtocolTokenEstimate
}

// FitRequestPromptBudget gives dynamic tool schemas their exact prompt cost
// before the final admission check. RuntimeContextBudget intentionally keeps a
// conservative tool reserve, but a product registry can legitimately expose a
// larger schema set for one routed turn. In that case, remove older atomic
// history segments rather than rejecting a small current user message.
//
// The returned copy preserves every system message and the active turn from
// the latest ordinary user message through the newest atomic tool segment. If
// that indivisible working set still exceeds the limit, the normal validation
// error remains authoritative.
func FitRequestPromptBudget(request provider.ChatRequest, limit int) (provider.ChatRequest, int) {
	if limit <= 0 || EstimateRequestPromptTokens(request) <= limit {
		return request, 0
	}
	withoutMessages := request
	withoutMessages.Messages = nil
	messageBudget := limit - EstimateRequestPromptTokens(withoutMessages)
	if messageBudget <= 0 {
		return request, 0
	}
	original := request.Messages
	estimate := func(message provider.Message) int {
		return messageEstimatedTokens(message) + provider.RequestReasoningTokenEstimate(request, message)
	}
	bounded := compactMessagesByTokensWithEstimator(original, messageBudget, 1, estimate)
	dropped := len(original) - len(bounded)
	if dropped <= 0 {
		return request, 0
	}
	if continuity := buildConversationContinuity(original); continuity != "" {
		withContinuity, _ := injectConversationContinuity(original, continuity)
		candidate := compactMessagesByTokensWithEstimator(withContinuity, messageBudget, 1, estimate)
		if messagesEstimatedTokensWithEstimator(candidate, estimate) <= messageBudget {
			bounded = candidate
		}
	}
	// Request shaping must never turn a valid user-anchored transcript into a
	// system + assistant/tool-only payload. Keep the original indivisible request
	// so the caller reports a local context-limit error instead of a backend 400.
	if hasOrdinaryUserMessage(original) && !hasOrdinaryUserMessage(bounded) {
		return request, 0
	}
	request.Messages = bounded
	return request, dropped
}

// ValidateRequestPromptBudget fails before inference instead of letting a
// backend reject or crash on an oversized current user/tool segment. Ordinary
// long history is already compacted by ContextManager; reaching this boundary
// means the newest indivisible segment itself must be split or paged.
func ValidateRequestPromptBudget(request provider.ChatRequest, limit int) error {
	if limit <= 0 {
		return nil
	}
	estimated := EstimateRequestPromptTokens(request)
	if estimated <= limit {
		return nil
	}
	return fmt.Errorf(
		"%w: estimated %d tokens, safe prompt limit %d; split the current document or image batch",
		ErrPromptContextLimit, estimated, limit,
	)
}

var (
	zhMemoryScopeRe = regexp.MustCompile(`(?s)針對(.{1,240}?)(?:等)?任務`)
	enMemoryScopeRe = regexp.MustCompile(`(?is)(?:for|when handling|applies to)\s+(.{1,240}?)(?:tasks?|workflows?)`)
	asciiTermRe     = regexp.MustCompile(`[a-z0-9][a-z0-9+_.-]{1,}`)
)

func explicitMemoryScope(memory string) (string, bool) {
	head := firstRunes(memory, 500)
	if match := zhMemoryScopeRe.FindStringSubmatch(head); len(match) == 2 {
		return match[1], true
	}
	if match := enMemoryScopeRe.FindStringSubmatch(head); len(match) == 2 {
		return match[1], true
	}
	return "", false
}

func scopeMatchesTask(scope, task string) bool {
	scope = strings.ToLower(scope)
	task = strings.ToLower(task)
	if strings.TrimSpace(task) == "" {
		return false
	}
	for _, term := range splitScopeTerms(scope) {
		if len([]rune(term)) >= 2 && strings.Contains(task, term) {
			return true
		}
	}
	for _, term := range asciiTermRe.FindAllString(scope, -1) {
		if !memoryStopWords[term] && strings.Contains(task, term) {
			return true
		}
	}
	return false
}

var memoryStopWords = map[string]bool{
	"and": true, "the": true, "with": true, "from": true, "into": true,
	"task": true, "tasks": true, "work": true, "workflow": true,
}

func splitScopeTerms(scope string) []string {
	return strings.FieldsFunc(scope, func(r rune) bool {
		switch r {
		case '、', '，', ',', '/', '|', '；', ';', '（', '）', '(', ')', '或':
			return true
		default:
			return false
		}
	})
}

func boundedMemory(global, project string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if project == "" {
		return truncateUTF8(global, budget)
	}
	project = truncateUTF8(project, budget)
	if global == "" || len(project) >= budget {
		return project
	}
	separator := "\n\n"
	globalBudget := budget - len(project) - len(separator)
	if globalBudget <= 0 {
		return project
	}
	global = truncateUTF8(global, globalBudget)
	if global == "" {
		return project
	}
	return global + separator + project
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	for maxBytes > 0 && !utf8.RuneStart(value[maxBytes]) {
		maxBytes--
	}
	return strings.TrimSpace(value[:maxBytes])
}

func firstRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
