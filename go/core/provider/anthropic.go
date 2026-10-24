package provider

// Native Messages protocol, based on the official Messages, streaming, vision,
// and client tool-use contracts at https://platform.claude.com/docs/en/api/messages.
// Extended thinking and server-side tools are intentionally not requested: their
// private continuation blocks require a separate durable history contract.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
	"github.com/hey-meadow/meadow-code/go/core/telemetry"
)

const anthropicVersion = "2023-06-01"
const anthropicStreamLimit = 32 << 20

// NewExternalProtocolClient selects a wire protocol explicitly. Empty protocol
// preserves the existing OpenAI-compatible client; names never select protocols.
func NewExternalProtocolClient(baseURL, key, model string, supportsImages bool, protocol string) (*Client, error) {
	if protocol != "" && protocol != "openai-compatible" && protocol != "anthropic" {
		return nil, errors.New("unsupported provider protocol")
	}
	c := NewExternalClient(baseURL, key, model, supportsImages)
	c.externalProtocol = protocol
	return c, nil
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}
type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Messages    []anthropicMessage `json:"messages"`
	System      []map[string]any   `json:"system,omitempty"`
	Tools       []map[string]any   `json:"tools,omitempty"`
	ToolChoice  map[string]any     `json:"tool_choice,omitempty"`
	Stop        []string           `json:"stop_sequences,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	TopK        *int               `json:"top_k,omitempty"`
	Thinking    map[string]string  `json:"thinking"`
	Stream      bool               `json:"stream"`
}

func (c *Client) buildAnthropicRequest(req ChatRequest) (anthropicRequest, ChatRequest, error) {
	if req.UserThinkingMode == ThinkingOn {
		return anthropicRequest{}, req, errors.New("Anthropic extended thinking is not supported by this connection")
	}
	req = c.BuildRequest(req)
	wire := anthropicRequest{Model: req.Model, MaxTokens: 4096, Stream: true, Thinking: map[string]string{"type": "disabled"}, Stop: req.Stop, Temperature: req.Temperature, TopP: req.TopP, TopK: req.TopK}
	if req.MaxTokens != nil {
		wire.MaxTokens = *req.MaxTokens
	}
	if wire.MaxTokens <= 0 || wire.Model == "" {
		return wire, req, errors.New("Anthropic model and positive output limit are required")
	}
	for _, message := range req.Messages {
		blocks, err := c.anthropicContent(message)
		if err != nil {
			return wire, req, err
		}
		role := message.Role
		switch role {
		case "system", "developer":
			if len(message.ToolCalls) > 0 {
				return wire, req, errors.New("Anthropic system message contains tool calls")
			}
			for _, block := range blocks {
				if block["type"] != "text" {
					return wire, req, errors.New("Anthropic system messages require text")
				}
			}
			wire.System = append(wire.System, blocks...)
			continue
		case "tool":
			if message.ToolCallID == "" {
				return wire, req, errors.New("Anthropic tool result requires a call identity")
			}
			resultBlock := map[string]any{"type": "tool_result", "tool_use_id": message.ToolCallID}
			if len(blocks) > 0 {
				resultBlock["content"] = blocks
			}
			blocks = []map[string]any{resultBlock}
			role = "user"
		case "assistant":
			for _, call := range message.ToolCalls {
				var input map[string]json.RawMessage
				if call.ID == "" || call.Function.Name == "" || json.Unmarshal([]byte(call.Function.Arguments), &input) != nil || input == nil {
					return wire, req, errors.New("Anthropic tool history requires complete object arguments")
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": json.RawMessage(call.Function.Arguments)})
			}
		case "user":
		default:
			return wire, req, errors.New("unsupported Anthropic message role")
		}
		if len(blocks) == 0 {
			return wire, req, errors.New("Anthropic conversation message requires nonempty content")
		}
		if len(wire.Messages) > 0 && wire.Messages[len(wire.Messages)-1].Role == role {
			wire.Messages[len(wire.Messages)-1].Content = append(wire.Messages[len(wire.Messages)-1].Content, blocks...)
		} else {
			wire.Messages = append(wire.Messages, anthropicMessage{Role: role, Content: blocks})
		}
	}
	if len(wire.Messages) == 0 {
		return wire, req, errors.New("Anthropic request requires conversation messages")
	}
	for _, tool := range req.Tools {
		if tool.Type != "" && tool.Type != "function" || tool.Function.Name == "" {
			return wire, req, errors.New("unsupported Anthropic tool definition")
		}
		schema := tool.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		wire.Tools = append(wire.Tools, map[string]any{"name": tool.Function.Name, "description": tool.Function.Description, "input_schema": schema})
	}
	if req.ToolChoice != nil {
		switch choice := req.ToolChoice.(type) {
		case string:
			switch choice {
			case "auto", "none":
				wire.ToolChoice = map[string]any{"type": choice}
			case "required":
				wire.ToolChoice = map[string]any{"type": "any"}
			default:
				return wire, req, errors.New("unsupported Anthropic tool choice")
			}
		default:
			data, err := json.Marshal(choice)
			if err != nil {
				return wire, req, errors.New("invalid Anthropic tool choice")
			}
			var named struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(data, &named) != nil || named.Type != "function" || named.Function.Name == "" {
				return wire, req, errors.New("unsupported Anthropic tool choice")
			}
			if !toolSchemasHaveName(req.Tools, named.Function.Name) {
				return wire, req, errors.New("Anthropic tool choice is not registered")
			}
			wire.ToolChoice = map[string]any{"type": "tool", "name": named.Function.Name}
		}
	}
	if req.ParallelToolCalls != nil && len(wire.Tools) > 0 {
		if wire.ToolChoice == nil {
			wire.ToolChoice = map[string]any{"type": "auto"}
		}
		if wire.ToolChoice["type"] != "none" {
			wire.ToolChoice["disable_parallel_tool_use"] = !*req.ParallelToolCalls
		}
	}
	return wire, req, nil
}

func (c *Client) anthropicContent(message Message) ([]map[string]any, error) {
	if len(message.ContentParts) == 0 {
		if message.Content == "" {
			return nil, nil
		}
		return []map[string]any{{"type": "text", "text": message.Content}}, nil
	}
	blocks := make([]map[string]any, 0, len(message.ContentParts))
	for _, part := range message.ContentParts {
		switch part.Type {
		case "text":
			if part.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			}
		case "image_url":
			if !c.externalImages || part.ImageURL == nil {
				return nil, errors.New("Anthropic connection does not support this image input")
			}
			value := part.ImageURL.URL
			source := map[string]any{}
			if strings.HasPrefix(value, "data:") {
				header, payload, ok := strings.Cut(value, ",")
				if !ok || !strings.HasSuffix(header, ";base64") || len(payload) > 28<<20 {
					return nil, errors.New("Anthropic image data is invalid")
				}
				mime := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
				if mime != "image/png" && mime != "image/jpeg" && mime != "image/gif" && mime != "image/webp" {
					return nil, errors.New("Anthropic image format is unsupported")
				}
				if data, err := base64.StdEncoding.DecodeString(payload); err != nil || len(data) == 0 {
					return nil, errors.New("Anthropic image data is invalid")
				}
				source = map[string]any{"type": "base64", "media_type": mime, "data": payload}
			} else {
				u, err := url.Parse(value)
				if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
					return nil, errors.New("Anthropic remote image requires an HTTPS URL")
				}
				source = map[string]any{"type": "url", "url": value}
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": source})
		default:
			return nil, errors.New("unsupported Anthropic content part")
		}
	}
	return blocks, nil
}

func anthropicDiagnosticBody(body []byte, key string) []byte {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return nil
	}
	var scrub func(any) any
	scrub = func(v any) any {
		switch row := v.(type) {
		case map[string]any:
			if row["type"] == "image" {
				if source, ok := row["source"].(map[string]any); ok {
					raw, _ := source["url"].(string)
					if source["type"] == "base64" {
						mime, _ := source["media_type"].(string)
						data, _ := source["data"].(string)
						raw = "data:" + mime + ";base64," + data
					}
					image, _ := json.Marshal(map[string]any{"type": "image_url", "image_url": map[string]any{"url": raw}})
					return map[string]any{"type": "image", "source": telemetry.DiagnosticPayload(image, key)}
				}
			}
			for k, item := range row {
				row[k] = scrub(item)
			}
		case []any:
			for i, item := range row {
				row[i] = scrub(item)
			}
		}
		return v
	}
	data, _ := json.Marshal(scrub(value))
	redacted, _ := json.Marshal(telemetry.DiagnosticPayload(data, key))
	return redacted
}

func (c *Client) chatAnthropic(ctx context.Context, request ChatRequest, hooks *StreamHooks) (result *StreamResult, returnErr error) {
	key, err := c.resolveRequestCredential(ctx, request.Model)
	if err != nil {
		return nil, err
	}
	if key == "" || strings.ContainsAny(key, "\r\n\x00") {
		return nil, errors.New("Anthropic API credential is required")
	}
	wire, req, err := c.buildAnthropicRequest(request)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, errors.New("Anthropic request could not be encoded")
	}
	if len(body) > 32<<20 {
		return nil, errors.New("Anthropic request exceeds transport limit")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	requestID := telemetryRequestID()
	started := time.Now()
	status := 0
	defer func() {
		if result != nil {
			result.OutputTokenLimit = wire.MaxTokens
		}
		if hooks != nil && hooks.OnProgress != nil {
			phase := InferenceCompleted
			if returnErr != nil {
				phase = InferenceFailed
			}
			hooks.OnProgress(InferenceProgress{RequestID: requestID, Model: req.Model, Phase: phase, Elapsed: time.Since(started)})
		}
	}()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("Anthropic endpoint is invalid")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", key)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("X-Meadow-Request-ID", requestID)
	diagnostic := anthropicDiagnosticBody(body, key)
	observation := c.beginTelemetry(ctx, requestID, req.Model, diagnostic, true)
	if observation != nil {
		observation.secret = key
	}
	observation.annotate(httpReq)
	observed := observation.wrap(hooks)
	defer func() { observation.finish(result, returnErr, status) }()
	recording := c.Recorder.beginCompletion(diagnostic, requestID, true)
	var raw bytes.Buffer
	defer func() {
		if path, err := recording.finish(raw.Bytes(), status, returnErr, ctx.Err()); err == nil && path != "" && result != nil {
			result.TranscriptPath = path
		}
	}()
	stall := c.StallTimeout
	if stall <= 0 {
		stall = defaultStallTimeout
	}
	progress := make(chan struct{}, 1)
	done := make(chan struct{})
	var stalled atomic.Bool
	go func() {
		timer := time.NewTimer(3 * stall)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-progress:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(stall)
			case <-timer.C:
				stalled.Store(true)
				cancel()
				return
			}
		}
	}()
	defer close(done)
	signal := func() {
		select {
		case progress <- struct{}{}:
		default:
		}
	}
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(httpReq)
	if response != nil {
		status = response.StatusCode
	}
	if err != nil {
		if ctx.Err() != nil {
			if stalled.Load() {
				return nil, ErrGenerationStall
			}
			return nil, ctx.Err()
		}
		return nil, errors.New("Anthropic connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Anthropic API returned HTTP %d", response.StatusCode)
	}
	result, err = consumeAnthropicSSE(io.TeeReader(io.LimitReader(response.Body, anthropicStreamLimit+1), &raw), observed, signal)
	if ctx.Err() != nil {
		if stalled.Load() {
			err = ErrGenerationStall
		} else {
			err = ctx.Err()
		}
	}
	if err == nil && result != nil {
		if failure := firstToolCallSchemaFailure(result.ToolCalls, req.Tools); failure != nil {
			err = newToolCallRecoveryError(failure.reason, failure.toolName, failure.missingRequired, failure.argumentBytes)
		}
	}
	if err != nil && result != nil {
		result.ToolCalls = nil
	}
	return result, err
}

func (c *Client) anthropicModels(ctx context.Context) ([]string, error) {
	key, err := c.resolveRequestCredential(ctx, c.Model)
	if err != nil {
		return nil, err
	}
	kind := modelconnection.KindProviderAPI
	if strings.HasPrefix(c.BaseURL, "http://") {
		kind = modelconnection.KindLocalAPI
	}
	models, err := modelconnection.DiscoverModels(ctx, modelconnection.Connection{Kind: kind, Protocol: modelconnection.ProtocolAnthropic, BaseURL: c.BaseURL}, key)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids, nil
}

type anthropicUsage struct {
	Input      *int `json:"input_tokens"`
	Output     *int `json:"output_tokens"`
	CacheRead  *int `json:"cache_read_input_tokens"`
	CacheWrite *int `json:"cache_creation_input_tokens"`
}
type anthropicBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
}
type anthropicEvent struct {
	Type    string `json:"type"`
	Index   *int   `json:"index"`
	Message struct {
		Role  string         `json:"role"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	Block anthropicBlock `json:"content_block"`
	Delta struct {
		Type        string  `json:"type"`
		Text        string  `json:"text"`
		PartialJSON string  `json:"partial_json"`
		StopReason  *string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
}

func consumeAnthropicSSE(reader io.Reader, hooks *StreamHooks, progress func()) (*StreamResult, error) {
	result := &StreamResult{}
	started, stopped, final := false, false, false
	terminalReason := ""
	type blockState struct {
		block     anthropicBlock
		arguments strings.Builder
		closed    bool
		hasDelta  bool
	}
	blocks := map[int]*blockState{}
	nextIndex := 0
	seenIDs := map[string]bool{}
	input, output, cacheRead, cacheWrite := 0, 0, 0, 0
	hasUsage := false
	updateUsage := func(usage anthropicUsage) error {
		for _, pair := range []struct {
			src *int
			dst *int
		}{{usage.Input, &input}, {usage.Output, &output}, {usage.CacheRead, &cacheRead}, {usage.CacheWrite, &cacheWrite}} {
			if pair.src != nil {
				if *pair.src < 0 {
					return errors.New("Anthropic usage is invalid")
				}
				*pair.dst = *pair.src
				hasUsage = true
			}
		}
		if hasUsage {
			maxInt := int(^uint(0) >> 1)
			if input > maxInt-cacheRead || input+cacheRead > maxInt-cacheWrite || input+cacheRead+cacheWrite > maxInt-output {
				return errors.New("Anthropic usage exceeds limit")
			}
			prompt := input + cacheRead + cacheWrite
			result.Usage = &Usage{PromptTokens: prompt, CompletionTokens: output, TotalTokens: prompt + output}
		}
		return nil
	}
	handle := func(data string) error {
		var event anthropicEvent
		if json.Unmarshal([]byte(data), &event) != nil {
			return errors.New("Anthropic stream event is invalid")
		}
		if stopped {
			return errors.New("Anthropic stream contains data after completion")
		}
		switch event.Type {
		case "ping":
			return nil
		case "error":
			return errors.New("Anthropic stream returned a provider error")
		case "message_start":
			if started || event.Message.Role != "assistant" {
				return errors.New("Anthropic message start is invalid")
			}
			started = true
			return updateUsage(event.Message.Usage)
		case "content_block_start":
			if !started || final || event.Index == nil || *event.Index != nextIndex {
				return errors.New("Anthropic content block order is invalid")
			}
			if nextIndex > 0 && !blocks[nextIndex-1].closed {
				return errors.New("Anthropic content blocks overlap")
			}
			nextIndex++
			if event.Block.Type != "text" && event.Block.Type != "tool_use" {
				return errors.New("unsupported Anthropic response content")
			}
			if event.Block.Type == "tool_use" && (event.Block.ID == "" || event.Block.Name == "" || seenIDs[event.Block.ID]) {
				return errors.New("Anthropic tool identity is invalid")
			}
			state := &blockState{block: event.Block}
			blocks[*event.Index] = state
			if event.Block.Type == "tool_use" {
				seenIDs[event.Block.ID] = true
			} else if event.Block.Text != "" {
				result.Content += event.Block.Text
				if hooks != nil && hooks.OnDelta != nil {
					hooks.OnDelta(event.Block.Text, false)
				}
				progress()
			}
		case "content_block_delta":
			if !started || final || event.Index == nil {
				return errors.New("Anthropic delta order is invalid")
			}
			block := blocks[*event.Index]
			if block == nil || block.closed {
				return errors.New("Anthropic delta has no open content block")
			}
			if block.block.Type == "text" && event.Delta.Type == "text_delta" {
				result.Content += event.Delta.Text
				if event.Delta.Text != "" {
					if hooks != nil && hooks.OnDelta != nil {
						hooks.OnDelta(event.Delta.Text, false)
					}
					progress()
				}
			}
			if block.block.Type == "tool_use" && event.Delta.Type == "input_json_delta" {
				var initial map[string]json.RawMessage
				if json.Unmarshal(block.block.Input, &initial) != nil || initial == nil || len(initial) != 0 {
					return errors.New("Anthropic tool input conflicts with streamed arguments")
				}
				block.hasDelta = true
				block.arguments.WriteString(event.Delta.PartialJSON)
				if block.arguments.Len() > 8<<20 {
					return errors.New("Anthropic tool input exceeds limit")
				}
				if hooks != nil {
					if hooks.OnToolDraft != nil {
						hooks.OnToolDraft(block.block.Name, block.arguments.Len())
					}
					if hooks.OnToolDraftProgress != nil {
						hooks.OnToolDraftProgress(ToolDraftProgress{Index: *event.Index, CallID: block.block.ID, Name: block.block.Name, ArgumentsDelta: event.Delta.PartialJSON, TotalBytes: block.arguments.Len()})
					}
				}
				if event.Delta.PartialJSON != "" {
					progress()
				}
			} else if block.block.Type != "text" || event.Delta.Type != "text_delta" {
				return errors.New("unsupported Anthropic content delta")
			}
		case "content_block_stop":
			if !started || final || event.Index == nil {
				return errors.New("Anthropic block stop is invalid")
			}
			block := blocks[*event.Index]
			if block == nil || block.closed {
				return errors.New("Anthropic block stop has no open block")
			}
			block.closed = true
		case "message_delta":
			if !started {
				return errors.New("Anthropic final delta is invalid")
			}
			for _, block := range blocks {
				if !block.closed {
					return errors.New("Anthropic message ended with open content block")
				}
			}
			if err := updateUsage(event.Usage); err != nil {
				return err
			}
			if event.Delta.StopReason != nil {
				if final && terminalReason != *event.Delta.StopReason {
					return errors.New("Anthropic stream contains conflicting terminal reasons")
				}
				switch *event.Delta.StopReason {
				case "end_turn", "stop_sequence":
					result.FinishReason = "stop"
				case "tool_use":
					result.FinishReason = "tool_calls"
				case "max_tokens":
					result.FinishReason = "length"
				default:
					return errors.New("unsupported Anthropic stop reason")
				}
				final = true
				terminalReason = *event.Delta.StopReason
			}
		case "message_stop":
			if !started || !final {
				return errors.New("Anthropic message stopped without terminal reason")
			}
			stopped = true
		default: // Unknown standalone events are forward-compatible; they do not complete a message.
		}
		return nil
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var frame strings.Builder
	total := 0
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > anthropicStreamLimit {
			return result, errors.New("Anthropic stream exceeds limit")
		}
		if line == "" {
			if frame.Len() > 0 {
				data := strings.TrimSuffix(frame.String(), "\n")
				frame.Reset()
				if err := handle(data); err != nil {
					return result, err
				}
				if stopped {
					break
				}
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			frame.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			frame.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return result, errors.New("Anthropic stream read failed")
	}
	if !stopped {
		return result, errors.New("Anthropic stream ended before message_stop")
	}
	calls := []ToolCall{}
	for index := 0; index < nextIndex; index++ {
		block := blocks[index]
		if block == nil || !block.closed {
			return result, errors.New("Anthropic stream contains incomplete content")
		}
		if block.block.Type == "tool_use" {
			arguments := string(block.block.Input)
			if block.hasDelta {
				arguments = block.arguments.String()
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal([]byte(arguments), &obj) != nil || obj == nil {
				return result, errors.New("Anthropic tool input is not a complete JSON object")
			}
			calls = append(calls, ToolCall{ID: block.block.ID, Type: "function", Function: FunctionCall{Name: block.block.Name, Arguments: arguments}})
		}
	}
	if len(calls) > 0 && result.FinishReason != "tool_calls" {
		return result, errors.New("Anthropic tool response has incompatible stop reason")
	}
	if len(calls) == 0 && result.FinishReason == "tool_calls" {
		return result, errors.New("Anthropic tool stop has no tool calls")
	}
	result.ToolCalls = calls
	return result, nil
}
