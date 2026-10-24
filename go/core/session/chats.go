package session

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// chatRecord is one JSONL line in the TS chat format (verified against real
// files under ~/.meadow/projects/*/chats/). parts entries carry exactly one
// of text / functionCall / functionResponse.
type chatRecord struct {
	UUID                    string                  `json:"uuid"`
	ParentUUID              *string                 `json:"parentUuid"`
	SessionID               string                  `json:"sessionId"`
	Timestamp               string                  `json:"timestamp"`
	Type                    string                  `json:"type"` // user | assistant | tool_result | usage | system | compaction
	CWD                     string                  `json:"cwd,omitempty"`
	Version                 string                  `json:"version,omitempty"`
	GitBranch               string                  `json:"gitBranch,omitempty"`
	Model                   string                  `json:"model,omitempty"`
	ConnectionID            string                  `json:"connectionId,omitempty"`
	Message                 *chatMsg                `json:"message,omitempty"`
	Usage                   *chatUsage              `json:"usageMetadata,omitempty"`
	RunState                *chatRunState           `json:"runState,omitempty"`
	CheckpointID            string                  `json:"checkpointId,omitempty"`
	GuidanceID              string                  `json:"guidanceId,omitempty"`
	RequestID               string                  `json:"requestId,omitempty"`
	MessageID               string                  `json:"messageId,omitempty"`
	TaskBoundary            bool                    `json:"taskBoundary,omitempty"`
	Thought                 *DisplayThought         `json:"thought,omitempty"`
	VerificationCorrections []DisplayTestCorrection `json:"verificationCorrections,omitempty"`
	// ProviderState is protocol continuation, deliberately outside visible
	// message parts, thought receipts, search and display history.
	ProviderState *provider.ProviderReasoning `json:"providerState,omitempty"`
}

type chatRunState struct {
	Status      string  `json:"status"`
	StopReason  string  `json:"stopReason,omitempty"`
	HarnessMode *string `json:"harnessMode,omitempty"`
}

type chatMsg struct {
	Role  string     `json:"role"` // user | model
	Parts []chatPart `json:"parts"`
}

type chatPart struct {
	Text string `json:"text,omitempty"`
	// Attachment is a durable, relative filename under the session artifact
	// directory.  The binary itself is never embedded in the JSONL transcript;
	// this keeps history small while allowing Desktop to render the original
	// image after a resume.
	Attachment       string            `json:"attachment,omitempty"`
	FunctionCall     *chatFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *chatFunctionResp `json:"functionResponse,omitempty"`
}

type chatFunctionCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type chatFunctionResp struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type chatUsage struct {
	PromptTokenCount            int    `json:"promptTokenCount"`
	CandidatesTokenCount        int    `json:"candidatesTokenCount"`
	TotalTokenCount             int    `json:"totalTokenCount"`
	ContextPromptTokenCount     *int   `json:"contextPromptTokenCount,omitempty"`
	CoveredAssistantCount       int    `json:"coveredAssistantCount,omitempty"`
	AssistantUsageCoverage      string `json:"assistantUsageCoverage,omitempty"`
	AssistantCoveragePriorCount int    `json:"assistantCoveragePriorCount,omitempty"`
}

// ProjectUsageEstimate reconstructs historical usage from durable chat JSONL.
// Estimated contains legacy turns where the provider omitted usageMetadata;
// Reported contains the real counts embedded by newer providers.
type ProjectUsageEstimate struct {
	Reported       provider.Usage
	Estimated      provider.Usage
	ReportedTurns  int64
	EstimatedTurns int64
	SessionIDs     []string
}

// ChatWriter appends TS-compatible records to one session's JSONL file.
type ChatWriter struct {
	SessionID string
	Path      string

	cwd              string
	gitBranch        string
	version          string
	lastUUID         *string
	file             *os.File
	seenCheckpoints  map[string]bool
	seenGuidance     map[string]bool
	lastCheckpointID string
}

// NewChatWriter creates projects/<dir>/chats/<session-uuid>.jsonl.
func NewChatWriter(projectRoot string) (*ChatWriter, error) {
	dir, err := ChatsDir(projectRoot)
	if err != nil {
		return nil, err
	}
	sessionID := NewUUID()
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &ChatWriter{
		SessionID:       sessionID,
		Path:            path,
		cwd:             projectRoot,
		gitBranch:       gitBranch(projectRoot),
		version:         buildinfo.Version,
		file:            f,
		seenCheckpoints: map[string]bool{},
		seenGuidance:    map[string]bool{},
	}, nil
}

// OpenChatWriter creates or reopens chats/<sessionID>.jsonl for appending.
// Serve sessions persist under their own id on the first prompt; resume
// keeps writing the original chat file so one thread stays one file. An
// existing file is scanned for its last record uuid so the parentUuid
// chain stays linked across restarts.
func OpenChatWriter(projectRoot, sessionID string) (*ChatWriter, error) {
	path, err := ChatPath(projectRoot, sessionID)
	if err != nil {
		return nil, err
	}
	last, checkpoints, guidance, stable := scanChatState(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &ChatWriter{
		SessionID:        sessionID,
		Path:             path,
		cwd:              projectRoot,
		gitBranch:        gitBranch(projectRoot),
		version:          buildinfo.Version,
		lastUUID:         last,
		file:             f,
		seenCheckpoints:  checkpoints,
		seenGuidance:     guidance,
		lastCheckpointID: stable,
	}, nil
}

func scanChatState(path string) (*string, map[string]bool, map[string]bool, string) {
	checkpoints := map[string]bool{}
	guidance := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return nil, checkpoints, guidance, ""
	}
	defer f.Close()
	var last *string
	stable := ""
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec chatRecord
		if json.Unmarshal(scanner.Bytes(), &rec) == nil && rec.UUID != "" {
			id := rec.UUID
			last = &id
			if rec.Type == "checkpoint" && rec.CheckpointID != "" {
				checkpoints[rec.CheckpointID] = true
				stable = rec.CheckpointID
			}
			if rec.Type == "guidance" && rec.GuidanceID != "" {
				guidance[rec.GuidanceID] = true
			}
		}
	}
	return last, checkpoints, guidance, stable
}

func (w *ChatWriter) Close() error {
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

func (w *ChatWriter) append(rec chatRecord) error {
	if rec.ProviderState != nil {
		// Existing chats may predate private provider continuation. Restrict the
		// file before writing any private bytes, and fail closed on chmod errors.
		if err := w.file.Chmod(0o600); err != nil {
			return fmt.Errorf("protect private provider history: %w", err)
		}
	}
	rec.UUID = NewUUID()
	rec.ParentUUID = w.lastUUID
	rec.SessionID = w.SessionID
	rec.Timestamp = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	rec.CWD = w.cwd
	rec.Version = w.version
	rec.GitBranch = w.gitBranch
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := w.file.Write(append(b, '\n')); err != nil {
		return err
	}
	// A stable tool/model boundary is useful only when it survives a process
	// crash. Keep the append-only transcript and the filesystem in agreement.
	if err := w.file.Sync(); err != nil {
		return err
	}
	id := rec.UUID
	w.lastUUID = &id
	if rec.Type == "checkpoint" && rec.CheckpointID != "" {
		w.seenCheckpoints[rec.CheckpointID] = true
		w.lastCheckpointID = rec.CheckpointID
	}
	if rec.Type == "guidance" && rec.GuidanceID != "" {
		w.seenGuidance[rec.GuidanceID] = true
	}
	return nil
}

func (w *ChatWriter) User(text string) error {
	return w.UserWithAttachments(text, nil)
}

// UserWithAttachments persists the human-visible text plus zero or more
// session-local attachment filenames. The JSONL keeps only bounded relative
// references; LoadHistory safely reconstructs provider image parts from the
// per-session artifact directory when the conversation is reopened.
func (w *ChatWriter) UserWithAttachments(text string, attachments []string) error {
	return w.UserMessage(provider.Message{Role: "user", Content: text}, attachments)
}

// UserWithRequestID preserves the validated transport delivery identity for
// display reconciliation after resume. It is never part of provider history.
func (w *ChatWriter) UserWithRequestID(text string, attachments []string, requestID string) error {
	return w.userMessage(provider.Message{Role: "user", Content: text}, attachments, requestID)
}

// BackgroundObservation is model-visible tool context, never a human message.
// Keep it in recovery history without presenting a fabricated user bubble.
func (w *ChatWriter) BackgroundObservation(text string) error {
	return w.append(chatRecord{Type: "background_observation", Message: &chatMsg{Role: "user", Parts: []chatPart{{Text: text}}}})
}

// UserMessage preserves trusted local task metadata outside model text/wire.
func (w *ChatWriter) UserMessage(message provider.Message, attachments []string) error {
	return w.userMessage(message, attachments, "")
}

func (w *ChatWriter) userMessage(message provider.Message, attachments []string, requestID string) error {
	parts := []chatPart{{Text: message.Content}}
	for _, name := range attachments {
		if strings.TrimSpace(name) == "" {
			continue
		}
		parts = append(parts, chatPart{Attachment: filepath.Base(name)})
	}
	return w.append(chatRecord{
		Type: "user", TaskBoundary: message.TaskBoundary, RequestID: requestID,
		Message: &chatMsg{Role: "user", Parts: parts},
	})
}

// Guidance is model-visible continuation context but not a second user bubble.
// Desktop already rendered the live queued message; keeping a distinct record
// prevents duplicate presentation while preserving the contract after resume.
func (w *ChatWriter) Guidance(text string) error {
	return w.GuidanceCheckpoint("", "", text, nil)
}

// GuidanceCheckpoint persists one applied guidance delivery exactly once.
// checkpointID binds the delivery to the stable transcript that consumed it;
// attachments are durable session-local filenames, never inline bytes.
func (w *ChatWriter) GuidanceCheckpoint(guidanceID, checkpointID, text string, attachments []string) error {
	return w.GuidanceCheckpointWithRequestID(guidanceID, checkpointID, text, attachments, "")
}

// GuidanceCheckpointWithRequestID preserves one conversation inbox delivery's
// transport identity in the same canonical guidance record, never a second
// user append. Request metadata is presentation-only, not provider history.
func (w *ChatWriter) GuidanceCheckpointWithRequestID(guidanceID, checkpointID, text string, attachments []string, requestID string) error {
	if guidanceID != "" && w.seenGuidance[guidanceID] {
		return nil
	}
	parts := []chatPart{{Text: text}}
	for _, name := range attachments {
		if strings.TrimSpace(name) != "" {
			parts = append(parts, chatPart{Attachment: filepath.Base(name)})
		}
	}
	return w.append(chatRecord{
		Type: "guidance", GuidanceID: guidanceID, CheckpointID: checkpointID, RequestID: requestID,
		Message: &chatMsg{Role: "user", Parts: parts},
	})
}

// StableCheckpointIdentity is deterministic for one durable chat transcript.
// It names a message boundary, not a provider token cursor.
func StableCheckpointIdentity(sessionID string, messages []provider.Message) string {
	var taskBoundaries []int
	for index, message := range messages {
		if message.Role == "user" && message.TaskBoundary {
			taskBoundaries = append(taskBoundaries, index)
		}
	}
	payload, _ := json.Marshal(struct {
		Version        string             `json:"version"`
		SessionID      string             `json:"session_id"`
		Messages       []provider.Message `json:"messages"`
		TaskBoundaries []int              `json:"task_boundaries,omitempty"`
	}{Version: "meadow.stable-checkpoint.v1", SessionID: sessionID, Messages: messages, TaskBoundaries: taskBoundaries})
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("checkpoint-%x", digest[:16])
}

func (w *ChatWriter) StableCheckpoint(checkpointID string) error {
	checkpointID = strings.TrimSpace(checkpointID)
	if checkpointID == "" {
		return fmt.Errorf("stable checkpoint identity is required")
	}
	if w.seenCheckpoints[checkpointID] {
		w.lastCheckpointID = checkpointID
		return nil
	}
	return w.append(chatRecord{Type: "checkpoint", CheckpointID: checkpointID})
}

func (w *ChatWriter) LastStableCheckpointID() string { return w.lastCheckpointID }

// Assistant records one model turn (text and/or tool calls) with usage.
func (w *ChatWriter) Assistant(model, text string, toolCalls []provider.ToolCall, usage *provider.Usage) error {
	return w.AssistantMessage(model, provider.Message{Role: "assistant", Content: text, ToolCalls: toolCalls}, usage)
}

// AssistantMessage persists native provider continuation alongside an ordinary
// assistant record. Only exact-origin DeepSeek state is eligible, and only
// LoadHistory restores it; no display or export message serializes the field.
func (w *ChatWriter) AssistantMessage(model string, message provider.Message, usage *provider.Usage) error {
	text, toolCalls := message.Content, message.ToolCalls
	var parts []chatPart
	if strings.TrimSpace(text) != "" || (message.MessageID != "" && text != "") {
		parts = append(parts, chatPart{Text: text})
	}
	for _, tc := range toolCalls {
		args := map[string]any{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		parts = append(parts, chatPart{FunctionCall: &chatFunctionCall{
			ID: tc.ID, Name: tc.Function.Name, Args: args,
		}})
	}
	if len(parts) == 0 {
		parts = []chatPart{{Text: ""}}
	}
	rec := chatRecord{
		Type:      "assistant",
		MessageID: message.MessageID,
		Model:     model,
		Message:   &chatMsg{Role: "model", Parts: parts},
	}
	if provider.IsDeepSeekModel(model) && message.ProviderReasoning != nil &&
		model == message.ProviderReasoning.Model {
		state := *message.ProviderReasoning
		rec.ProviderState = &state
	}
	rec.Usage = durableChatUsage(usage)
	return w.append(rec)
}

// Usage records one logical model turn independently of any assistant
// message. Codex-style runs can contain multiple assistant boundaries (primary
// plus Advisor), or terminate after a billed request without producing one.
// coveredAssistantCount prevents those already-accounted assistant messages
// from being re-estimated as legacy missing usage.
func (w *ChatWriter) Usage(usage *provider.Usage, coveredAssistantCount int) error {
	if usage == nil {
		return nil
	}
	durable := durableChatUsage(usage)
	if coveredAssistantCount > 0 {
		durable.CoveredAssistantCount = coveredAssistantCount
	}
	durable.AssistantUsageCoverage = usage.AssistantUsageCoverage
	durable.AssistantCoveragePriorCount = usage.AssistantCoveragePriorCount
	return w.append(chatRecord{Type: "usage", Usage: durable})
}

func durableChatUsage(usage *provider.Usage) *chatUsage {
	if usage == nil {
		return nil
	}
	durable := &chatUsage{
		PromptTokenCount:     usage.PromptTokens,
		CandidatesTokenCount: usage.CompletionTokens,
		TotalTokenCount:      usage.TotalTokens,
	}
	if usage.ContextPromptTokensSet {
		contextPromptTokens := usage.ContextPromptTokens
		durable.ContextPromptTokenCount = &contextPromptTokens
	}
	return durable
}

func (w *ChatWriter) ToolResult(callID, toolName, output string) error {
	return w.append(chatRecord{
		Type: "tool_result",
		Message: &chatMsg{Role: "user", Parts: []chatPart{{
			FunctionResponse: &chatFunctionResp{
				ID: callID, Name: toolName,
				Response: map[string]any{"output": output},
			},
		}}},
	})
}

// Compaction appends a semantic checkpoint without deleting the earlier audit
// trail. LoadHistory treats the newest checkpoint as the start of model-facing
// context; LoadDisplayHistory keeps the full human-visible conversation.
func (w *ChatWriter) Compaction(summary string) error {
	return w.append(chatRecord{
		Type:    "compaction",
		Message: &chatMsg{Role: "model", Parts: []chatPart{{Text: summary}}},
	})
}

// RunStarted and RunFinished bracket one foreground or queued turn. A missing
// terminal marker means the process stopped mid-run and the durable transcript
// can be resumed from its newest stable checkpoint.
func (w *ChatWriter) RunStarted() error {
	return w.RunStartedCheckpoint(w.lastCheckpointID)
}

func (w *ChatWriter) RunStartedCheckpoint(checkpointID string) error {
	return w.append(chatRecord{Type: "run_state", CheckpointID: checkpointID, RunState: &chatRunState{Status: "running"}})
}

// RunStartedCheckpointWithHarnessMode commits execution identity in the same
// durable record as its run start. A failed future admission cannot overwrite
// the mode of a preceding interrupted run. The record UUID and initial
// checkpoint identify this particular start even as later checkpoints advance.
func (w *ChatWriter) RunStartedCheckpointWithHarnessMode(checkpointID, mode string) error {
	return w.RunStartedCheckpointWithModel(checkpointID, mode, "")
}

// RunStartedCheckpointWithModel keeps the admitted model with its run identity,
// including a crash before the first assistant response reaches the journal.
func (w *ChatWriter) RunStartedCheckpointWithModel(checkpointID, mode, modelID string) error {
	if checkpointID == "" || mode == "" || !validHarnessPreference(mode) {
		return errors.New("run start requires a checkpoint and explicit harness mode")
	}
	return w.append(chatRecord{Type: "run_state", Model: modelID, CheckpointID: checkpointID, RunState: &chatRunState{Status: "running", HarnessMode: &mode}})
}

func (w *ChatWriter) RunFinished(stopReason string) error {
	return w.RunFinishedCheckpoint(stopReason, w.lastCheckpointID)
}

func (w *ChatWriter) RunFinishedCheckpoint(stopReason, checkpointID string) error {
	return w.append(chatRecord{Type: "run_state", CheckpointID: checkpointID, RunState: &chatRunState{
		Status: "finished", StopReason: strings.TrimSpace(stopReason),
	}})
}

func (w *ChatWriter) RunInterrupted(stopReason, checkpointID string) error {
	return w.append(chatRecord{Type: "run_state", CheckpointID: checkpointID, RunState: &chatRunState{
		Status: "interrupted", StopReason: strings.TrimSpace(stopReason),
	}})
}

type RunRecovery struct {
	Interrupted         bool
	CheckpointID        string
	StopReason          string
	HarnessMode         string
	RunStartID          string
	StartedCheckpointID string
}

// ErrInvalidHarnessRunIdentity prevents execution recovery with a relabeled or
// malformed runtime. Display-only history remains readable for inspection.
var ErrInvalidHarnessRunIdentity = errors.New("invalid durable harness run identity")

func LoadRunRecovery(path string) (RunRecovery, error) {
	f, err := os.Open(path)
	if err != nil {
		return RunRecovery{}, err
	}
	defer f.Close()
	var result RunRecovery
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec chatRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			var header struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(scanner.Bytes(), &header) == nil && header.Type == "run_state" {
				return RunRecovery{}, fmt.Errorf("%w: invalid run state: %v", ErrInvalidHarnessRunIdentity, err)
			}
			continue
		}
		if rec.Type == "checkpoint" && rec.CheckpointID != "" {
			result.CheckpointID = rec.CheckpointID
		}
		if rec.Type != "run_state" || rec.RunState == nil {
			continue
		}
		if rec.RunState.Status == "running" {
			result.HarnessMode = ""
			result.RunStartID = rec.UUID
			result.StartedCheckpointID = rec.CheckpointID
			if rec.RunState.HarnessMode != nil {
				mode := *rec.RunState.HarnessMode
				if mode == "" || !validHarnessPreference(mode) || rec.CheckpointID == "" || rec.UUID == "" {
					return RunRecovery{}, ErrInvalidHarnessRunIdentity
				}
				result.HarnessMode = mode
			} else {
				var fields struct {
					RunState map[string]json.RawMessage `json:"runState"`
				}
				_ = json.Unmarshal(scanner.Bytes(), &fields)
				if _, present := fields.RunState["harnessMode"]; present {
					return RunRecovery{}, fmt.Errorf("%w: null mode", ErrInvalidHarnessRunIdentity)
				}
			}
		} else if rec.RunState.HarnessMode != nil {
			// Runtime identity may only originate at a run start. A terminal
			// marker cannot relabel an earlier run or introduce an unbound mode.
			return RunRecovery{}, fmt.Errorf("%w: mode outside run start", ErrInvalidHarnessRunIdentity)
		}
		if rec.CheckpointID != "" {
			result.CheckpointID = rec.CheckpointID
		}
		result.StopReason = rec.RunState.StopReason
		result.Interrupted = rec.RunState.Status == "running" || rec.RunState.Status == "interrupted"
	}
	return result, scanner.Err()
}

// LoadInterruptedRun reports whether the newest persisted run started but did
// not write a terminal marker. Older transcripts without run markers are not
// treated as interrupted.
func LoadInterruptedRun(path string) (bool, error) {
	recovery, err := LoadRunRecovery(path)
	return recovery.Interrupted, err
}

func HasGuidanceCheckpoint(path, guidanceID string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec chatRecord
		if json.Unmarshal(scanner.Bytes(), &rec) == nil && rec.Type == "guidance" && rec.GuidanceID == guidanceID {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// SessionInfo summarizes one stored chat for /resume listings.
type SessionInfo struct {
	SessionID string
	Path      string
	Modified  time.Time
	FirstUser string
}

// SessionSearchResult is one human-visible durable chat match. Tool results,
// system records and compaction checkpoints are intentionally excluded so a
// search result always opens on words the user could actually see in chat.
type SessionSearchResult struct {
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title"`
	Modified  time.Time `json:"modified"`
	Role      string    `json:"role"`
	Snippet   string    `json:"snippet"`
}

// DisplayMessage is the durable human-visible dialogue shape used by Desktop
// resume. Provider messages deliberately omit presentation metadata; keeping
// the original record timestamp here prevents the UI from inventing a new
// time whenever a stored thread is reopened.
type DisplayMessage struct {
	MessageID        string              `json:"messageId,omitempty"`
	RequestID        string              `json:"requestId,omitempty"`
	SourceComparison json.RawMessage     `json:"sourceComparison,omitempty"`
	Role             string              `json:"role"`
	Content          string              `json:"content"`
	Timestamp        string              `json:"timestamp,omitempty"`
	Attachments      []string            `json:"attachments,omitempty"`
	Thought          *DisplayThought     `json:"thought,omitempty"`
	ToolCallCount    int                 `json:"toolCallCount,omitempty"`
	RunOutcomes      []DisplayRunOutcome `json:"runOutcomes,omitempty"`
}

// DisplayRunOutcome is a durable terminal receipt, attached to the preceding
// visible row so reopening a chat preserves its original chronological boundary.
// "finished" says the run stopped, not that its task succeeded.
type DisplayRunOutcome struct {
	Status      string                  `json:"status"`
	StopReason  string                  `json:"stopReason,omitempty"`
	Timestamp   string                  `json:"timestamp,omitempty"`
	Corrections []DisplayTestCorrection `json:"corrections,omitempty"`
}

// DisplayThought is received provider reasoning, kept outside model history.
// Sequence identifies the last received delta so an active resume can skip
// already restored SSE chunks without changing their whitespace or language.
type DisplayThought struct {
	Text      string `json:"text"`
	Observed  bool   `json:"observed"`
	State     string `json:"state"`
	RunID     string `json:"runId"`
	SegmentID string `json:"segmentId"`
	Sequence  int    `json:"sequence"`
}

func (w *ChatWriter) Thought(snapshot DisplayThought) error {
	if !snapshot.Observed || snapshot.Text == "" || snapshot.RunID == "" || snapshot.SegmentID == "" {
		return fmt.Errorf("thought receipt requires observed text and run identity")
	}
	return w.append(chatRecord{Type: "thought", Thought: &snapshot})
}

// ListSessions returns the project's stored chats, newest first.
func ListSessions(projectRoot string) ([]SessionInfo, error) {
	dir := filepath.Join(PrimaryDir(), "projects", ProjectDirName(projectRoot), "chats")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		si := SessionInfo{
			SessionID: strings.TrimSuffix(e.Name(), ".jsonl"),
			Path:      filepath.Join(dir, e.Name()),
			Modified:  info.ModTime(),
		}
		si.FirstUser = firstUserText(si.Path)
		if strings.TrimSpace(si.FirstUser) == "" {
			continue
		}
		out = append(out, si)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// SearchSessions searches visible user and assistant text newest-first.
// The result limit is deliberately small because Desktop queries several
// user-added projects concurrently while the command palette is open.
func SearchSessions(projectRoot, query string, limit int) ([]SessionSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	infos, err := ListSessions(projectRoot)
	if err != nil {
		return nil, err
	}
	needle := []rune(strings.ToLower(query))
	results := make([]SessionSearchResult, 0, min(limit, len(infos)))
	for _, info := range infos {
		role, matched := "user", info.FirstUser
		matchAt := runeIndex([]rune(strings.ToLower(matched)), needle)
		if matchAt < 0 {
			messages, loadErr := LoadDisplayMessages(info.Path)
			if loadErr != nil {
				continue
			}
			matched = ""
			for _, message := range messages {
				candidate := strings.TrimSpace(message.Content)
				if candidate == "" {
					continue
				}
				at := runeIndex([]rune(strings.ToLower(candidate)), needle)
				if at >= 0 {
					role, matched, matchAt = message.Role, candidate, at
					break
				}
			}
		}
		if matchAt < 0 {
			continue
		}
		results = append(results, SessionSearchResult{
			SessionID: info.SessionID, Title: strings.TrimSpace(info.FirstUser),
			Modified: info.Modified, Role: role, Snippet: searchSnippet(matched, matchAt, len(needle)),
		})
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

func runeIndex(haystack, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	for start := 0; start+len(needle) <= len(haystack); start++ {
		matched := true
		for index := range needle {
			if haystack[start+index] != needle[index] {
				matched = false
				break
			}
		}
		if matched {
			return start
		}
	}
	return -1
}

func searchSnippet(text string, matchAt, matchLength int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= 150 {
		return string(runes)
	}
	start := matchAt - 55
	if start < 0 {
		start = 0
	}
	end := start + 150
	if minimum := matchAt + matchLength + 35; end < minimum {
		end = minimum
	}
	if end > len(runes) {
		end = len(runes)
		start = max(0, end-150)
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(runes) {
		suffix = "…"
	}
	return prefix + string(runes[start:end]) + suffix
}

// ChatUsageFingerprint cheaply detects whether a project's durable chats have
// changed. Desktop can poll usage without rescanning large JSONL files every
// few seconds.
func ChatUsageFingerprint(projectRoot string) (string, error) {
	dir := filepath.Join(PrimaryDir(), "projects", ProjectDirName(projectRoot), "chats")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var fingerprint strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(&fingerprint, "%s:%d:%d;", entry.Name(), info.Size(), info.ModTime().UnixNano())
	}
	return fingerprint.String(), nil
}

// EstimateProjectUsage makes old "0 tokens · N turns" chats useful without
// pretending the result is exact. Missing prompt usage is approximated from
// the accumulated model-visible transcript; completion usage comes from the
// assistant message itself. A compaction checkpoint resets the running
// context estimate just like LoadHistory.
func EstimateProjectUsage(projectRoot string) (ProjectUsageEstimate, error) {
	dir := filepath.Join(PrimaryDir(), "projects", ProjectDirName(projectRoot), "chats")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return ProjectUsageEstimate{}, nil
	}
	if err != nil {
		return ProjectUsageEstimate{}, err
	}

	var total ProjectUsageEstimate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		total.SessionIDs = append(total.SessionIDs, strings.TrimSuffix(entry.Name(), ".jsonl"))
		contextTokens := 0
		type assistantContribution struct {
			reported  *provider.Usage
			estimated *provider.Usage
		}
		assistantContributions := make([]assistantContribution, 0)
		futureCoverage := ""
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			var rec chatRecord
			if json.Unmarshal(scanner.Bytes(), &rec) != nil {
				continue
			}
			messageTokens := estimateChatMessageTokens(rec.Message)
			switch rec.Type {
			case "compaction":
				contextTokens = messageTokens
			case "assistant":
				coveredByFutureUsage := false
				if futureCoverage != "" {
					coveredByFutureUsage = futureCoverage[0] == '1'
					futureCoverage = futureCoverage[1:]
				}
				contribution := assistantContribution{}
				if !coveredByFutureUsage && rec.Usage != nil && (rec.Usage.TotalTokenCount > 0 || rec.Usage.PromptTokenCount > 0 || rec.Usage.CandidatesTokenCount > 0) {
					prompt := rec.Usage.PromptTokenCount
					completion := rec.Usage.CandidatesTokenCount
					tokens := rec.Usage.TotalTokenCount
					if tokens <= 0 {
						tokens = prompt + completion
					}
					usage := provider.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: tokens}
					contribution.reported = &usage
				} else if !coveredByFutureUsage {
					prompt := contextTokens
					if prompt <= 0 {
						prompt = 1
					}
					usage := provider.Usage{
						PromptTokens: prompt, CompletionTokens: messageTokens, TotalTokens: prompt + messageTokens,
					}
					contribution.estimated = &usage
				}
				assistantContributions = append(assistantContributions, contribution)
				contextTokens += messageTokens
			case "usage":
				if rec.Usage == nil {
					continue
				}
				coverage := rec.Usage.AssistantUsageCoverage
				prior := rec.Usage.AssistantCoveragePriorCount
				if coverage == "" && rec.Usage.CoveredAssistantCount > 0 {
					coverage = strings.Repeat("1", rec.Usage.CoveredAssistantCount)
					prior = len(coverage)
				}
				prior = min(max(prior, 0), len(coverage))
				availablePrior := min(prior, len(assistantContributions))
				coverageStart := prior - availablePrior
				assistantStart := len(assistantContributions) - availablePrior
				for index := 0; index < availablePrior; index++ {
					if coverage[coverageStart+index] == '1' {
						assistantContributions[assistantStart+index] = assistantContribution{}
					}
				}
				futureCoverage += coverage[prior:]
				prompt := rec.Usage.PromptTokenCount
				completion := rec.Usage.CandidatesTokenCount
				tokens := rec.Usage.TotalTokenCount
				if tokens <= 0 {
					tokens = prompt + completion
				}
				total.Reported.PromptTokens += prompt
				total.Reported.CompletionTokens += completion
				total.Reported.TotalTokens += tokens
				total.ReportedTurns++
			case "user", "guidance", "background_observation":
				// A future coverage suffix belongs only to the logical turn that
				// emitted it. If an errored run never persisted its pending
				// assistant, do not let that suffix consume an unrelated answer.
				futureCoverage = ""
				contextTokens += messageTokens
			default:
				contextTokens += messageTokens
			}
		}
		for _, contribution := range assistantContributions {
			if contribution.reported != nil {
				total.Reported.PromptTokens += contribution.reported.PromptTokens
				total.Reported.CompletionTokens += contribution.reported.CompletionTokens
				total.Reported.TotalTokens += contribution.reported.TotalTokens
				total.ReportedTurns++
			}
			if contribution.estimated != nil {
				total.Estimated.PromptTokens += contribution.estimated.PromptTokens
				total.Estimated.CompletionTokens += contribution.estimated.CompletionTokens
				total.Estimated.TotalTokens += contribution.estimated.TotalTokens
				total.EstimatedTurns++
			}
		}
		scanErr := scanner.Err()
		_ = file.Close()
		if scanErr != nil {
			return ProjectUsageEstimate{}, scanErr
		}
	}
	return total, nil
}

func estimateChatMessageTokens(message *chatMsg) int {
	if message == nil {
		return 0
	}
	tokens := 0
	for _, part := range message.Parts {
		tokens += provider.EstimateTextTokens(part.Text)
		if part.FunctionCall != nil {
			tokens += provider.EstimateTextTokens(part.FunctionCall.Name)
			if raw, err := json.Marshal(part.FunctionCall.Args); err == nil {
				tokens += provider.EstimateTextTokens(string(raw))
			}
		}
		if part.FunctionResponse != nil {
			tokens += provider.EstimateTextTokens(part.FunctionResponse.Name)
			if raw, err := json.Marshal(part.FunctionResponse.Response); err == nil {
				tokens += provider.EstimateTextTokens(string(raw))
			}
		}
	}
	return tokens
}

func firstUserText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec chatRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		if rec.Type == "user" && rec.Message != nil {
			for _, p := range rec.Message.Parts {
				if p.Text != "" {
					return p.Text
				}
			}
		}
	}
	return ""
}

// LoadHistory rebuilds provider messages from a stored chat (including TS
// CLI sessions — that is the zero-migration promise).
func LoadHistory(path string) ([]provider.Message, error) {
	return loadHistory(path, true)
}

// LoadDisplayHistory reconstructs the full dialogue for Desktop. Compaction
// checkpoints alter only future model context and remain invisible here.
func LoadDisplayHistory(path string) ([]provider.Message, error) {
	return loadHistory(path, false)
}

// LoadDisplayMessages reconstructs dialogue with its original timestamps and
// terminal run receipts. Tool-only assistant records, tool results, system
// telemetry and compaction checkpoints are not separate presentation rows.
func LoadDisplayMessages(path string) ([]DisplayMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var messages []DisplayMessage
	thoughtRows := map[string]int{}
	pendingThought := -1
	terminalIDs := map[string]bool{}
	var pendingCorrections []DisplayTestCorrection
	comparisonCalls := map[string]bool{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec chatRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		if rec.Type == "verification_corrections" {
			pendingCorrections = append(pendingCorrections, rec.VerificationCorrections...)
			continue
		}
		if rec.Type == "run_state" && rec.RunState != nil {
			if rec.RunState.Status == "finished" || rec.RunState.Status == "interrupted" {
				comparisonCalls = map[string]bool{}
			}
			if len(messages) > 0 && (rec.RunState.Status == "finished" || rec.RunState.Status == "interrupted") &&
				(rec.UUID == "" || !terminalIDs[rec.UUID]) {
				row := &messages[len(messages)-1]
				row.RunOutcomes = append(row.RunOutcomes, DisplayRunOutcome{
					Status: rec.RunState.Status, StopReason: rec.RunState.StopReason, Timestamp: rec.Timestamp,
					Corrections: pendingCorrections,
				})
				// Replay may repeat one record. Distinct terminal records survive,
				// even when a retry produces no new visible message or identical text.
				terminalIDs[rec.UUID] = true
			}
			pendingCorrections = nil
			// An outcome must not acquire a later run's answer through a pending
			// thought row. No terminal state is inferred from running/unknown.
			pendingThought = -1
			continue
		}
		if rec.Type == "thought" && rec.Thought != nil && rec.Thought.Observed && rec.Thought.Text != "" {
			thought := *rec.Thought
			if thought.State == "streaming" {
				// Disk alone cannot establish that a writer is still alive. The
				// attached live session may restore this status in its response.
				thought.State = "incomplete"
			}
			key := thought.RunID + "\x00" + thought.SegmentID
			if row, found := thoughtRows[key]; found {
				if thought.Sequence >= messages[row].Thought.Sequence {
					messages[row].Thought = &thought
				}
			} else {
				pendingThought = len(messages)
				thoughtRows[key] = pendingThought
				messages = append(messages, DisplayMessage{Role: "assistant", Timestamp: rec.Timestamp, Thought: &thought})
			}
			continue
		}
		if rec.Message == nil {
			continue
		}
		if rec.Type == "user" {
			comparisonCalls = map[string]bool{}
		}
		if rec.Type == "assistant" {
			for _, part := range rec.Message.Parts {
				if call := part.FunctionCall; call != nil && call.Name == "source_compare" && call.ID != "" {
					comparisonCalls[call.ID] = true
				}
			}
		}
		if rec.Type == "tool_result" {
			for _, part := range rec.Message.Parts {
				response := part.FunctionResponse
				if response == nil || response.Name != "source_compare" || !comparisonCalls[response.ID] {
					continue
				}
				delete(comparisonCalls, response.ID)
				output, _ := response.Response["output"].(string)
				var report struct {
					Type    string            `json:"type"`
					Version int               `json:"version"`
					Checks  []json.RawMessage `json:"checks"`
				}
				if len(output) > 160<<10 || json.Unmarshal([]byte(output), &report) != nil || report.Type != "source_comparison" || report.Version != 1 || len(report.Checks) < 1 || len(report.Checks) > 12 {
					continue
				}
				messages = append(messages, DisplayMessage{Role: "tool", Timestamp: rec.Timestamp, SourceComparison: json.RawMessage(output)})
				pendingThought = -1
			}
			continue
		}
		role := ""
		switch rec.Type {
		case "user", "guidance":
			role = "user"
		case "assistant":
			role = "assistant"
		default:
			continue
		}
		var content strings.Builder
		var attachments []string
		toolCallCount := 0
		for _, part := range rec.Message.Parts {
			content.WriteString(part.Text)
			if role == "assistant" && part.FunctionCall != nil {
				toolCallCount++
			}
			if name := strings.TrimSpace(part.Attachment); name != "" {
				attachments = append(attachments, filepath.Base(name))
			}
		}
		if role == "assistant" && strings.TrimSpace(content.String()) == "" && rec.MessageID == "" {
			continue
		}
		if role == "user" {
			if rec.Type == "user" {
				pendingCorrections = nil
			}
			pendingThought = -1
		} else if pendingThought >= 0 {
			messages[pendingThought].Content = content.String()
			messages[pendingThought].MessageID = rec.MessageID
			messages[pendingThought].ToolCallCount = toolCallCount
			pendingThought = -1
			continue
		}
		requestID := ""
		if rec.Type == "user" || rec.Type == "guidance" {
			requestID = rec.RequestID
		}
		messages = append(messages, DisplayMessage{
			Role: role, Content: content.String(), Timestamp: rec.Timestamp,
			MessageID:   rec.MessageID,
			RequestID:   requestID,
			Attachments: attachments, ToolCallCount: toolCallCount,
		})
	}
	return messages, scanner.Err()
}

func loadHistory(path string, applyCompaction bool) ([]provider.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var msgs []provider.Message
	attachmentRefs := map[int][]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec chatRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil || rec.Message == nil {
			continue
		}
		switch rec.Type {
		case "compaction":
			if !applyCompaction {
				continue
			}
			msgs = nil
			attachmentRefs = map[int][]string{}
			for _, p := range rec.Message.Parts {
				if strings.TrimSpace(p.Text) != "" {
					msgs = append(msgs, provider.Message{Role: "system", Content: "[Meadow conversation compact]\n" + p.Text})
				}
			}
		case "user", "guidance", "background_observation":
			firstMessage := len(msgs)
			var attachments []string
			for _, p := range rec.Message.Parts {
				if p.Text != "" {
					msgs = append(msgs, provider.Message{Role: "user", Content: p.Text, TaskBoundary: rec.Type == "user" && rec.TaskBoundary})
				}
				if strings.TrimSpace(p.Attachment) != "" {
					attachments = append(attachments, p.Attachment)
				}
			}
			if applyCompaction && len(attachments) > 0 {
				if firstMessage == len(msgs) {
					msgs = append(msgs, provider.Message{Role: "user", TaskBoundary: rec.Type == "user" && rec.TaskBoundary})
				}
				attachmentRefs[firstMessage] = attachments
			}
		case "assistant":
			m := provider.Message{Role: "assistant"}
			if applyCompaction && rec.ProviderState != nil && provider.IsDeepSeekModel(rec.Model) &&
				rec.ProviderState.Model == rec.Model {
				state := *rec.ProviderState
				m.ProviderReasoning = &state
			}
			for _, p := range rec.Message.Parts {
				if p.Text != "" {
					m.Content += p.Text
				}
				if p.FunctionCall != nil {
					m.ToolCalls = append(m.ToolCalls, provider.ToolCall{
						ID: p.FunctionCall.ID, Type: "function",
						Function: provider.FunctionCall{
							Name:      p.FunctionCall.Name,
							Arguments: provider.MarshalArgs(p.FunctionCall.Args),
						},
					})
				}
			}
			msgs = append(msgs, m)
		case "tool_result":
			for _, p := range rec.Message.Parts {
				if p.FunctionResponse == nil {
					continue
				}
				output, _ := p.FunctionResponse.Response["output"].(string)
				msgs = append(msgs, provider.Message{
					Role:       "tool",
					ToolCallID: p.FunctionResponse.ID,
					Content:    output,
				})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return msgs, err
	}
	if applyCompaction {
		msgs = rehydrateHistoryImages(path, msgs, attachmentRefs)
	}
	return msgs, nil
}

// LoadLastPromptTokens returns the newest real provider-reported prompt usage
// stored in a chat. It lets a resumed CLI restore its context meter without
// estimating tokens from characters.
func LoadLastPromptTokens(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	last := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec chatRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil || rec.Usage == nil {
			continue
		}
		if rec.Usage.ContextPromptTokenCount != nil {
			if *rec.Usage.ContextPromptTokenCount > 0 {
				last = *rec.Usage.ContextPromptTokenCount
			}
			continue
		}
		if rec.Usage.PromptTokenCount > 0 {
			last = rec.Usage.PromptTokenCount
		}
	}
	return last, scanner.Err()
}

// NewUUID returns a random v4 UUID string.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func gitBranch(root string) string {
	head, err := os.ReadFile(filepath.Join(root, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(head))
	if strings.HasPrefix(s, "ref: refs/heads/") {
		return strings.TrimPrefix(s, "ref: refs/heads/")
	}
	return ""
}
