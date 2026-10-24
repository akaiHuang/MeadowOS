package daemonruntime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	maxControlResponseBytes = 16 << 20
	maxSSEFrameBytes        = 64 << 20
)

// Hooks is the local-client observation surface. It intentionally mirrors the
// native CLI/Desktop contract without exposing the daemon's private transport.
type Hooks struct {
	OnContent           func(string)
	OnThought           func(string)
	OnProgress          func(provider.InferenceProgress)
	OnToolDraft         func(string, int)
	OnToolDraftProgress func(provider.ToolDraftProgress)
	OnToolCall          func(string, map[string]any)
	OnToolResult        func(string, tools.Result)
	OnToolStart         func(agentcore.ToolStart)
	OnToolComplete      func(agentcore.ToolExecution)
	OnTurn              func(int, *provider.Usage)
	OnGuidance          func(agentcore.GuidanceEvent)
	OnRecovery          func(agentcore.RecoveryStatus)
	Confirm             engine.Confirmer
	AskUser             tools.Asker
}

type Config struct {
	WorkspaceRoot string
	ModelID       string
	Hooks         Hooks
	StateRoot     string
	// ApprovalMode is applied after the remote product session exists and
	// before its first prompt is submitted. Client bindings used to call
	// SetApprovalMode immediately after New; at that point there is no session
	// id yet, so the request was intentionally a no-op.
	ApprovalMode engine.ApprovalMode
}

type persistedState struct {
	ChatID      string `json:"chatId,omitempty"`
	SessionID   string `json:"sessionId,omitempty"`
	LastEventID int64  `json:"lastEventId,omitempty"`
}

type Runtime struct {
	client    *daemontransport.Client
	workspace string
	modelID   string
	hooks     Hooks
	statePath string
	approval  engine.ApprovalMode

	runMu    sync.Mutex
	mu       sync.Mutex
	state    persistedState
	turn     int
	recovery agentcore.RecoveryStatus
}

var _ agentcore.Runtime = (*Runtime)(nil)

func New(config Config) (*Runtime, error) {
	workspace, err := filepath.Abs(strings.TrimSpace(config.WorkspaceRoot))
	if err != nil || workspace == "" {
		return nil, fmt.Errorf("resolve Meadow workspace: %w", err)
	}
	client, err := daemontransport.NewClient()
	if err != nil {
		return nil, err
	}
	stateRoot := strings.TrimSpace(config.StateRoot)
	if stateRoot == "" {
		stateRoot, err = defaultStateRoot()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create Meadow client state: %w", err)
	}
	key := sha256.Sum256([]byte(workspace + "\x00" + strings.TrimSpace(config.ModelID)))
	runtime := &Runtime{
		client:    client,
		workspace: workspace,
		modelID:   strings.TrimSpace(config.ModelID),
		hooks:     config.Hooks,
		statePath: filepath.Join(stateRoot, hex.EncodeToString(key[:16])+".json"),
		approval:  config.ApprovalMode,
	}
	_ = runtime.loadState()
	return runtime, nil
}

func defaultStateRoot() (string, error) {
	if root := strings.TrimSpace(os.Getenv("MEADOW_HOME")); root != "" {
		return filepath.Join(root, "client-sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Meadow home: %w", err)
	}
	return filepath.Join(home, ".meadow", "client-sessions"), nil
}

func (runtime *Runtime) loadState() error {
	payload, err := os.ReadFile(runtime.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state persistedState
	if err := json.Unmarshal(payload, &state); err != nil {
		return err
	}
	runtime.state = state
	return nil
}

func (runtime *Runtime) saveStateLocked() error {
	payload, err := json.Marshal(runtime.state)
	if err != nil {
		return err
	}
	temporary := runtime.statePath + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Rename(temporary, runtime.statePath)
}

func (runtime *Runtime) RunInput(ctx context.Context, input agentcore.RunInput) (*agentcore.RunResult, error) {
	runtime.runMu.Lock()
	defer runtime.runMu.Unlock()

	if err := runtime.ensureSession(ctx); err != nil {
		return nil, err
	}
	sessionID, cursor := runtime.sessionSnapshot()
	if sessionID == "" {
		return nil, errors.New("Meadow Agent did not return a session id")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan remoteEvent, 64)
	streamErrors := make(chan error, 1)
	go runtime.streamEvents(runCtx, sessionID, cursor, events, streamErrors)

	promptDone := make(chan promptOutcome, 1)
	payload := map[string]any{"prompt": promptParts(input.User)}
	if input.Mode != agentcore.RunModeAuto {
		payload["options"] = map[string]any{"mode": string(input.Mode)}
	}
	go func() {
		var response map[string]any
		err := runtime.doJSON(runCtx, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/prompt", payload, &response)
		promptDone <- promptOutcome{response: response, err: err}
	}()

	collector := newRunCollector(input, runtime.hooks)
	var prompt promptOutcome
	var promptReturned bool
	var turnCompleted bool
	var grace <-chan time.Time
	for {
		if promptReturned && turnCompleted {
			return collector.result(prompt.response), prompt.err
		}
		select {
		case event := <-events:
			completed, eventErr := runtime.applyEvent(ctx, sessionID, event, collector)
			if eventErr != nil {
				return collector.result(prompt.response), eventErr
			}
			if completed {
				turnCompleted = true
			}
		case prompt = <-promptDone:
			promptReturned = true
			if prompt.err != nil && !turnCompleted {
				grace = time.After(2 * time.Second)
			} else if !turnCompleted {
				grace = time.After(2 * time.Second)
			}
		case err := <-streamErrors:
			if err != nil && !promptReturned {
				return nil, err
			}
		case <-grace:
			return collector.result(prompt.response), prompt.err
		case <-ctx.Done():
			runtime.cancelSession(sessionID)
			result := collector.result(prompt.response)
			result.Stop = agentcore.StopCancelled
			return result, ctx.Err()
		}
	}
}

type promptOutcome struct {
	response map[string]any
	err      error
}

func promptParts(message provider.Message) []map[string]any {
	parts := make([]map[string]any, 0, len(message.ContentParts)+1)
	if len(message.ContentParts) > 0 {
		for _, part := range message.ContentParts {
			switch part.Type {
			case "text":
				if part.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": part.Text})
				}
			case "image_url":
				if part.ImageURL != nil && strings.TrimSpace(part.ImageURL.URL) != "" {
					image := map[string]any{"url": part.ImageURL.URL}
					if part.ImageURL.Detail != "" {
						image["detail"] = part.ImageURL.Detail
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": image})
				}
			}
		}
	} else if strings.TrimSpace(message.Content) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": message.Content})
	}
	return parts
}

func (runtime *Runtime) ensureSession(ctx context.Context) error {
	if runtime.modelID != "" {
		var activation map[string]any
		if err := runtime.doJSON(ctx, http.MethodPost, "/agent/model", map[string]any{"model": runtime.modelID}, &activation); err != nil {
			return err
		}
	}
	runtime.mu.Lock()
	state := runtime.state
	runtime.mu.Unlock()

	var response map[string]any
	if state.ChatID != "" {
		err := runtime.doJSON(ctx, http.MethodPost, "/session/resume", map[string]any{
			"cwd": runtime.workspace, "chatId": state.ChatID,
		}, &response)
		if err == nil {
			if err := runtime.acceptSessionResponse(response); err != nil {
				return err
			}
			return runtime.configureSession(ctx)
		}
	}
	response = nil
	if err := runtime.doJSON(ctx, http.MethodPost, "/session", map[string]any{
		"cwd": runtime.workspace,
	}, &response); err != nil {
		return err
	}
	if err := runtime.acceptSessionResponse(response); err != nil {
		return err
	}
	return runtime.configureSession(ctx)
}

func (runtime *Runtime) configureSession(ctx context.Context) error {
	if err := runtime.configureSessionModel(ctx); err != nil {
		return err
	}
	if runtime.approval == "" {
		return nil
	}
	sessionID, _ := runtime.sessionSnapshot()
	return runtime.doJSON(ctx, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/approval",
		map[string]any{"mode": string(runtime.approval)}, nil)
}

func (runtime *Runtime) configureSessionModel(ctx context.Context) error {
	if runtime.modelID == "" {
		return nil
	}
	sessionID, _ := runtime.sessionSnapshot()
	return runtime.doJSON(ctx, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/model",
		map[string]any{"modelId": runtime.modelID}, nil)
}

func (runtime *Runtime) acceptSessionResponse(response map[string]any) error {
	sessionID := stringValue(response["sessionId"])
	if sessionID == "" {
		return errors.New("Meadow Agent session response is missing sessionId")
	}
	runtime.mu.Lock()
	runtime.state.SessionID = sessionID
	if chatID := stringValue(response["chatId"]); chatID != "" {
		runtime.state.ChatID = chatID
	}
	if cursor := int64Value(response["replayAfterEventId"]); cursor > runtime.state.LastEventID {
		runtime.state.LastEventID = cursor
	}
	err := runtime.saveStateLocked()
	runtime.mu.Unlock()
	return err
}

func (runtime *Runtime) sessionSnapshot() (string, int64) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.state.SessionID, runtime.state.LastEventID
}

func (runtime *Runtime) rememberEventID(id int64) {
	if id <= 0 {
		return
	}
	runtime.mu.Lock()
	if id > runtime.state.LastEventID {
		runtime.state.LastEventID = id
		_ = runtime.saveStateLocked()
	}
	runtime.mu.Unlock()
}

func (runtime *Runtime) doJSON(ctx context.Context, method, path string, body any, output any) error {
	request, err := runtime.client.NewRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	response, err := runtime.client.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, maxControlResponseBytes))
		return fmt.Errorf("Meadow Agent %s %s returned %s: %s", method, path, response.Status, strings.TrimSpace(string(payload)))
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, maxControlResponseBytes)).Decode(output)
}

type remoteEvent struct {
	ID   int64
	Type string
	Data json.RawMessage
}

func (runtime *Runtime) streamEvents(ctx context.Context, sessionID string, cursor int64, output chan<- remoteEvent, errorsOut chan<- error) {
	for {
		lastID, err := runtime.streamOnce(ctx, sessionID, cursor, output)
		if lastID > cursor {
			cursor = lastID
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			select {
			case errorsOut <- err:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (runtime *Runtime) streamOnce(ctx context.Context, sessionID string, cursor int64, output chan<- remoteEvent) (int64, error) {
	path := "/session/" + url.PathEscape(sessionID) + "/events"
	if cursor > 0 {
		path += "?lastEventId=" + strconv.FormatInt(cursor, 10)
	}
	request, err := runtime.client.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return cursor, err
	}
	request.Header.Set("Accept", "text/event-stream")
	if cursor > 0 {
		request.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
	}
	response, err := runtime.client.HTTP.Do(request)
	if err != nil {
		return cursor, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, maxControlResponseBytes))
		return cursor, fmt.Errorf("Meadow Agent event stream returned %s: %s", response.Status, strings.TrimSpace(string(payload)))
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), maxSSEFrameBytes)
	var eventID int64
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		var envelope struct {
			ID   int64           `json:"id"`
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(data.String()), &envelope); err != nil {
			data.Reset()
			return err
		}
		if envelope.ID > 0 {
			eventID = envelope.ID
		}
		event := remoteEvent{ID: eventID, Type: envelope.Type, Data: envelope.Data}
		data.Reset()
		select {
		case output <- event:
			if event.ID > cursor {
				cursor = event.ID
				runtime.rememberEventID(event.ID)
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return cursor, err
			}
		case strings.HasPrefix(line, "id:"):
			eventID, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id:")), 10, 64)
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := flush(); err != nil {
		return cursor, err
	}
	return cursor, scanner.Err()
}

type runCollector struct {
	input       agentcore.RunInput
	hooks       Hooks
	final       strings.Builder
	turns       int
	toolCalls   int
	seenTools   map[string]bool
	recap       *agentcore.RunRecap
	escalation  *agentcore.ModelEscalation
	recovery    agentcore.RecoveryStatus
	hasRecovery bool
}

func newRunCollector(input agentcore.RunInput, hooks Hooks) *runCollector {
	return &runCollector{input: input, hooks: hooks, seenTools: map[string]bool{}}
}

func (collector *runCollector) result(response map[string]any) *agentcore.RunResult {
	finalText := collector.final.String()
	if finalText == "" {
		finalText = stringValue(response["finalText"])
	}
	messages := append([]provider.Message(nil), collector.input.History...)
	messages = append(messages, collector.input.User)
	if finalText != "" {
		messages = append(messages, provider.Message{Role: "assistant", Content: finalText})
	}
	stop := agentcore.StopCompleted
	switch stringValue(response["stopReason"]) {
	case "max_turns":
		stop = agentcore.StopMaxTurns
	case "cancelled":
		stop = agentcore.StopCancelled
	case "token_limit", "length":
		stop = agentcore.StopTokenLimit
	case "needs_input":
		stop = agentcore.StopNeedsInput
	case "incomplete":
		stop = agentcore.StopIncomplete
	}
	turns := collector.turns
	if turns == 0 {
		turns = 1
	}
	result := &agentcore.RunResult{
		FinalText: finalText, Turns: turns, ToolCalls: collector.toolCalls,
		Stop: stop, Messages: messages, Recap: collector.recap, Escalation: collector.escalation,
	}
	if collector.hasRecovery {
		result.ResumePlan = cloneRecoveryStatus(collector.recovery).ResumePlan
		result.RecoveryWarning = collector.recovery.RecoveryWarning
	}
	return result
}

func (runtime *Runtime) applyEvent(ctx context.Context, sessionID string, event remoteEvent, collector *runCollector) (bool, error) {
	var data map[string]any
	if len(event.Data) > 0 && json.Unmarshal(event.Data, &data) != nil {
		return false, nil
	}
	switch event.Type {
	case "session_update":
		update, _ := data["update"].(map[string]any)
		runtime.applySessionUpdate(update, collector)
	case "inference_progress":
		var progress provider.InferenceProgress
		if payload, err := json.Marshal(data); err == nil && json.Unmarshal(payload, &progress) == nil && collector.hooks.OnProgress != nil {
			collector.hooks.OnProgress(progress)
		}
	case "usage":
		usageData := data
		if nested, ok := data["usage"].(map[string]any); ok {
			usageData = nested
		}
		readToken := func(camel, snake string) int {
			if value, ok := usageData[camel]; ok {
				return intValue(value)
			}
			return intValue(usageData[snake])
		}
		usage := provider.Usage{
			PromptTokens:     readToken("promptTokens", "prompt_tokens"),
			CompletionTokens: readToken("completionTokens", "completion_tokens"),
			TotalTokens:      readToken("totalTokens", "total_tokens"),
		}
		if value, ok := usageData["contextPromptTokens"]; ok {
			usage.ContextPromptTokens = intValue(value)
			usage.ContextPromptTokensSet = true
		}
		turn := intValue(data["turn"])
		if turn == 0 {
			turn = collector.turns
		}
		if collector.hooks.OnTurn != nil {
			collector.hooks.OnTurn(turn, &usage)
		}
	case "permission_request":
		return false, runtime.resolvePermission(ctx, sessionID, data, collector.hooks.Confirm)
	case "user_input_request":
		return false, runtime.resolveUserInput(ctx, sessionID, data, collector.hooks.AskUser)
	case "guidance":
		var guidance agentcore.GuidanceEvent
		payload, _ := json.Marshal(data)
		if json.Unmarshal(payload, &guidance) == nil && collector.hooks.OnGuidance != nil {
			collector.hooks.OnGuidance(guidance)
		}
	case recoveryStatusEvent:
		payload, _ := json.Marshal(data)
		var status agentcore.RecoveryStatus
		if json.Unmarshal(payload, &status) == nil {
			mergeRecoveryStatus(&collector.recovery, status)
			collector.hasRecovery = true
			runtime.setRecoveryStatus(collector.recovery)
			if collector.hooks.OnRecovery != nil {
				collector.hooks.OnRecovery(runtime.RecoveryStatus())
			}
		}
	case "run_recap":
		payload, _ := json.Marshal(valueOrSelf(data, "recap"))
		var recap agentcore.RunRecap
		if json.Unmarshal(payload, &recap) == nil {
			collector.recap = &recap
		}
	case "model_escalation":
		payload, _ := json.Marshal(valueOrSelf(data, "escalation"))
		var escalation agentcore.ModelEscalation
		if json.Unmarshal(payload, &escalation) == nil {
			collector.escalation = &escalation
		}
	case "turn_completed":
		collector.turns++
		return true, nil
	case "stream_error":
		return false, errors.New(stringValue(data["message"]))
	}
	return false, nil
}

func (runtime *Runtime) applySessionUpdate(update map[string]any, collector *runCollector) {
	kind := stringValue(update["sessionUpdate"])
	switch kind {
	case "agent_message_chunk":
		text := nestedText(update["content"])
		collector.final.WriteString(text)
		if collector.hooks.OnContent != nil {
			collector.hooks.OnContent(text)
		}
	case "agent_thought_chunk":
		text := nestedText(update["content"])
		if collector.hooks.OnThought != nil {
			collector.hooks.OnThought(text)
		}
	case "tool_call_draft", "tool_call_draft_progress":
		name := stringValue(update["title"])
		if name == "" {
			name = stringValue(update["name"])
		}
		bytes := intValue(update["bytes"])
		if bytes == 0 {
			bytes = intValue(update["totalBytes"])
		}
		if collector.hooks.OnToolDraft != nil {
			collector.hooks.OnToolDraft(name, bytes)
		}
		if collector.hooks.OnToolDraftProgress != nil {
			collector.hooks.OnToolDraftProgress(provider.ToolDraftProgress{
				Index: intValue(update["index"]), CallID: stringValue(update["toolCallId"]), Name: name,
				ArgumentsDelta: stringValue(update["argumentsDelta"]), TotalBytes: bytes,
			})
		}
	case "tool_call":
		callID := stringValue(update["toolCallId"])
		name := stringValue(update["title"])
		args := mapValue(update["rawInput"])
		toolKind := tools.Kind(stringValue(update["kind"]))
		if !collector.seenTools[callID] {
			collector.seenTools[callID] = true
			collector.toolCalls++
		}
		if collector.hooks.OnToolCall != nil {
			collector.hooks.OnToolCall(name, args)
		}
		if collector.hooks.OnToolStart != nil {
			collector.hooks.OnToolStart(agentcore.ToolStart{Turn: collector.turns + 1, CallID: callID, Tool: name, Args: args, Kind: toolKind})
		}
	case "tool_call_update":
		callID := stringValue(update["toolCallId"])
		name := stringValue(update["title"])
		status := toolStatus(stringValue(update["status"]))
		display := outputText(update["rawOutput"])
		result := tools.Result{
			LLMContent: display, Display: display, Diff: stringValue(update["diff"]),
			ChangedPaths: stringSlice(update["changedPaths"]),
		}
		if status == agentcore.ToolError {
			result.Err = errors.New(display)
		}
		if collector.hooks.OnToolResult != nil {
			collector.hooks.OnToolResult(name, result)
		}
		if collector.hooks.OnToolComplete != nil {
			collector.hooks.OnToolComplete(agentcore.ToolExecution{
				Turn: collector.turns + 1, CallID: callID, Tool: name,
				Args: mapValue(update["rawInput"]), Kind: tools.Kind(stringValue(update["kind"])),
				Status: status, Executed: status == agentcore.ToolOK,
				LLMContent: display, Display: display, Diff: result.Diff,
				ChangedPaths: result.ChangedPaths, Err: result.Err,
			})
		}
	}
}

func (runtime *Runtime) resolvePermission(ctx context.Context, sessionID string, data map[string]any, confirmer engine.Confirmer) error {
	requestID := stringValue(data["requestId"])
	toolCall, _ := data["toolCall"].(map[string]any)
	decision := engine.DecisionDeny
	if confirmer != nil {
		selected, err := confirmer(ctx, engine.ConfirmRequest{
			ToolName: stringValue(toolCall["title"]), Args: mapValue(toolCall["rawInput"]), Kind: tools.Kind(stringValue(toolCall["kind"])),
		})
		if err != nil {
			return err
		}
		decision = selected
	}
	optionID := "reject"
	switch decision {
	case engine.DecisionApprove:
		optionID = "approve"
	case engine.DecisionApproveAlways:
		optionID = "approve_always"
	}
	return runtime.doJSON(ctx, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/permission/"+url.PathEscape(requestID),
		map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}, nil)
}

func (runtime *Runtime) resolveUserInput(ctx context.Context, sessionID string, data map[string]any, asker tools.Asker) error {
	requestID := stringValue(data["requestId"])
	questionValues, _ := data["questions"].([]any)
	questions := make([]tools.Question, 0, len(questionValues))
	for _, value := range questionValues {
		item, _ := value.(map[string]any)
		question := tools.Question{
			Header: stringValue(item["header"]), Question: stringValue(item["question"]),
			MultiSelect: boolValue(item["multiSelect"]), AllowOther: boolValue(item["allowOther"]),
		}
		for _, optionValue := range anySlice(item["options"]) {
			option, _ := optionValue.(map[string]any)
			question.Options = append(question.Options, tools.QuestionOption{
				Label: stringValue(option["label"]), Description: stringValue(option["description"]),
			})
		}
		questions = append(questions, question)
	}
	answers := make([]string, len(questions))
	if asker != nil {
		selected, err := asker(ctx, questions)
		if err != nil {
			return err
		}
		copy(answers, selected)
	}
	wireAnswers := make(map[string]any, len(answers))
	for index, answer := range answers {
		wireAnswers["q"+strconv.Itoa(index)] = answer
	}
	return runtime.doJSON(ctx, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/user-input/"+url.PathEscape(requestID),
		map[string]any{"answers": wireAnswers}, nil)
}

func (runtime *Runtime) SubmitGuidance(guidance agentcore.Guidance) (agentcore.GuidanceReceipt, error) {
	guidance.Text = strings.TrimSpace(guidance.Text)
	if guidance.Text == "" && len(guidance.ContentParts) == 0 {
		return agentcore.GuidanceReceipt{ID: guidance.ID, Mode: guidance.Mode, Disposition: agentcore.GuidanceRejected}, errors.New("guidance content is empty")
	}
	sessionID, _ := runtime.sessionSnapshot()
	if sessionID == "" {
		return agentcore.GuidanceReceipt{ID: guidance.ID, Mode: guidance.Mode, Disposition: agentcore.GuidanceRejected}, errors.New("no active Meadow Agent session")
	}
	if guidance.Mode == "" {
		guidance.Mode = agentcore.GuidanceSteerCurrent
	}
	path := "/session/" + url.PathEscape(sessionID) + "/steer"
	body := map[string]any{"text": guidance.Text, "contentParts": promptParts(provider.Message{Content: guidance.Text, ContentParts: guidance.ContentParts})}
	if guidance.Mode == agentcore.GuidanceQueueNext {
		path = "/session/" + url.PathEscape(sessionID) + "/queue"
		body = map[string]any{"prompt": body["contentParts"]}
	}
	var response map[string]any
	if err := runtime.doJSON(context.Background(), http.MethodPost, path, body, &response); err != nil {
		return agentcore.GuidanceReceipt{ID: guidance.ID, Mode: guidance.Mode, Disposition: agentcore.GuidanceRejected}, err
	}
	receipt := agentcore.GuidanceReceipt{
		ID: guidance.ID, Mode: guidance.Mode, Disposition: agentcore.GuidanceAccepted,
		TargetTurn: intValue(response["targetTurn"]), QueuePosition: intValue(response["queuePosition"]),
	}
	if disposition := stringValue(response["disposition"]); disposition != "" {
		receipt.Disposition = agentcore.GuidanceDisposition(disposition)
	}
	return receipt, nil
}

func (runtime *Runtime) SetApprovalMode(mode engine.ApprovalMode) error {
	return runtime.postSessionControl("approval", map[string]any{"mode": string(mode)})
}

func (runtime *Runtime) SetInferenceControls(thinking provider.ThinkingMode, effort provider.ReasoningEffort) error {
	return runtime.postSessionControl("inference-controls", map[string]any{"thinkingMode": string(thinking), "reasoningEffort": string(effort)})
}

func (runtime *Runtime) SetTwoPass(enabled bool) error {
	return runtime.postSessionControl("inference-controls", map[string]any{"twoPass": enabled})
}

func (runtime *Runtime) postSessionControl(endpoint string, payload map[string]any) error {
	sessionID, _ := runtime.sessionSnapshot()
	if sessionID == "" {
		return nil
	}
	return runtime.doJSON(context.Background(), http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/"+endpoint, payload, nil)
}

func (runtime *Runtime) cancelSession(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = runtime.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/cancel", map[string]any{}, nil)
}

func valueOrSelf(values map[string]any, key string) any {
	if value, ok := values[key]; ok {
		return value
	}
	return values
}

func nestedText(value any) string {
	if content, ok := value.(map[string]any); ok {
		return stringValue(content["text"])
	}
	return stringValue(value)
}

func outputText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		for _, key := range []string{"display", "content", "text", "message"} {
			if text := stringValue(typed[key]); text != "" {
				return text
			}
		}
	}
	payload, _ := json.Marshal(value)
	return string(payload)
}

func mapValue(value any) map[string]any {
	if values, ok := value.(map[string]any); ok {
		return values
	}
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		var values map[string]any
		if json.Unmarshal([]byte(text), &values) == nil {
			return values
		}
	}
	return map[string]any{}
}

func anySlice(value any) []any {
	values, _ := value.([]any)
	return values
}

func stringSlice(value any) []string {
	values := anySlice(value)
	result := make([]string, 0, len(values))
	for _, item := range values {
		if text := stringValue(item); text != "" {
			result = append(result, text)
		}
	}
	return result
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		integer, _ := typed.Int64()
		return int(integer)
	default:
		integer, _ := strconv.Atoi(stringValue(value))
		return integer
	}
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		integer, _ := typed.Int64()
		return integer
	default:
		integer, _ := strconv.ParseInt(stringValue(value), 10, 64)
		return integer
	}
}

func boolValue(value any) bool {
	valueBool, _ := value.(bool)
	return valueBool
}

func toolStatus(status string) agentcore.ToolStatus {
	switch status {
	case "completed", "success", "ok":
		return agentcore.ToolOK
	case "cancelled":
		return agentcore.ToolCancelled
	case "denied":
		return agentcore.ToolDenied
	default:
		return agentcore.ToolError
	}
}
