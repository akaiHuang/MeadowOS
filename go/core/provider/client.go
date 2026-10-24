package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/localipc"
	"github.com/hey-meadow/meadow-code/go/core/telemetry"
)

// Client is a minimal OpenAI-chat-completions client specialised for
// vllm-mlx / mlx_lm.server. It owns the request-shaping pipeline the TS
// MlxLmProvider runs in buildRequest: orphaned-tool-call repair, stop-string
// merging, and Gemma-4 safety overrides.
type Client struct {
	BaseURL            string
	APIKey             string
	CredentialResolver CredentialResolver
	Model              string
	HTTP               *http.Client
	// coreTransport is an explicit composition-time capability, independent of
	// the selected model's display name or the endpoint's loopback address.
	// Only NewCoreClient enables Core management requests.
	coreTransport bool
	// externalCompatible uses only standard OpenAI transport fields. Endpoint
	// capabilities, not a coincidental model name, control image support.
	externalCompatible bool
	// configuredCoreInference enables the verified Core model request controls
	// without opting an independently managed connection into Core lifecycle
	// requests or inheriting the local IPC credential.
	configuredCoreInference bool
	externalImages          bool
	externalProtocol        string
	// Explicit connection capability; never selected by a model display name.
	externalOpenAIReasoningEffort bool
	externalReasoningOffEffort    ReasoningEffort
	// Recorder, when set, captures every request body and raw SSE stream
	// for golden tests.
	Recorder *Recorder
	// Telemetry enqueues actual transport observations without network waits.
	Telemetry *telemetry.Queue
	// LegacyMlxStops injects the mlx_lm.server-era stop strings
	// (<|im_end|>, channel markers, ...). Off by default: vllm-mlx's gemma4
	// tool parser registers its own EOG stops, and a channel-marker stop
	// can cut generation between the thought channel and the answer.
	LegacyMlxStops bool
	// StallTimeout aborts a stream that produces no generation progress
	// (no content/tool-call chunk) for this long. Heartbeats do not count
	// as progress — vllm-mlx has been observed keeping heartbeats alive
	// for 300s while its engine was wedged at 1 chunk. 0 = default 180s.
	StallTimeout time.Duration
	// outputTokenLimit is the latest capability supplied by the owning Core
	// composition. Zero leaves the model-family request ceiling in charge.
	outputTokenLimit atomic.Int64

	// toolCacheScope is generated once per concrete provider Client. One Client
	// is one CLI/Desktop/Kernel runtime session, so append-only tool turns can
	// reuse their prefix without allowing another session to cross-hit tool
	// arguments or hybrid-state cache entries.
	toolCacheScopeOnce sync.Once
	toolCacheScope     string
}

const (
	defaultStallTimeout = 180 * time.Second
	defaultCoreBaseURL  = "http://127.0.0.1:18081/v1"
)

func (c *Client) SetOutputTokenLimit(limit int) {
	c.outputTokenLimit.Store(int64(max(0, limit)))
}

func (c *Client) OutputTokenLimit() int {
	return int(c.outputTokenLimit.Load())
}

// NewClient constructs a generic OpenAI-compatible transport. Model names do
// not opt it into Meadow Core status, cancellation, or activity endpoints.
func NewClient(baseURL, apiKey, model string) *Client {
	return newClient(baseURL, apiKey, model, false)
}

// NewCoreClient constructs a transport for an endpoint the caller knows is
// Meadow Core. Correlation, cancellation, progress and activity leases apply
// to every model served by that Core, including raw upstream model IDs.
// An omitted endpoint uses the fixed Core IPC endpoint, not the generic
// OpenAI-compatible provider default.
func NewCoreClient(baseURL, apiKey, model string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultCoreBaseURL
	}
	return newClient(baseURL, apiKey, model, true)
}

// NewExternalClient constructs an independently configured API connection.
// It never reads Core credentials or emits Core lifecycle requests.
func NewExternalClient(baseURL, apiKey, model string, supportsImages bool) *Client {
	c := newClient(baseURL, apiKey, model, false)
	c.externalCompatible = true
	c.externalImages = supportsImages
	c.APIKey = apiKey
	return c
}

// SetExternalOpenAIReasoningEffort declares a verified external connection's
// standard none/low/medium/high control. Configure before inference starts.
// Native/Core compositions retain their own request contract.
func (c *Client) SetExternalOpenAIReasoningEffort(enabled bool) {
	c.externalOpenAIReasoningEffort = enabled
	c.externalReasoningOffEffort = ""
}

// SetExternalInceptionReasoningEffort binds an explicit connection capability.
// Inception uses instant for non-reasoning requests, rather than OpenAI's none.
func (c *Client) SetExternalInceptionReasoningEffort(enabled bool) {
	c.externalOpenAIReasoningEffort = enabled
	c.externalReasoningOffEffort = ""
	if enabled {
		c.externalReasoningOffEffort = "instant"
	}
}

func (c *Client) applyInferenceControls(req *ChatRequest) {
	if c.externalCompatible && !c.configuredCoreInference && c.externalOpenAIReasoningEffort {
		req.EnableThinking, req.ChatTemplateKwargs, req.Thinking = nil, nil, nil
		req.ReasoningEffort = ""
		switch req.UserThinkingMode {
		case ThinkingOff:
			req.ReasoningEffort = "none"
			if c.externalReasoningOffEffort != "" {
				req.ReasoningEffort = c.externalReasoningOffEffort
			}
		case ThinkingOn:
			switch req.UserReasoningEffort {
			case ReasoningEffortMinimal, ReasoningEffortLow:
				req.ReasoningEffort = ReasoningEffortLow
			case ReasoningEffortHigh, ReasoningEffortMax:
				req.ReasoningEffort = ReasoningEffortHigh
			default:
				req.ReasoningEffort = ReasoningEffortMedium
			}
		}
		return
	}
	ApplyUserInferenceControls(req)
}

// NewConfiguredCoreClient keeps the explicit connection credential and standard
// streaming transport, while retaining Core's native thinking/sampling fields.
// The caller must have an explicitly configured Meadow Core connection.
func NewConfiguredCoreClient(baseURL, apiKey, model string, supportsImages bool) *Client {
	c := NewExternalClient(baseURL, apiKey, model, supportsImages)
	c.configuredCoreInference = true
	return c
}

func (c *Client) supportsInferenceControls(model string) bool {
	if !c.externalCompatible || c.configuredCoreInference {
		return true
	}
	if c.externalOpenAIReasoningEffort {
		return true
	}
	// These providers use official cloud fields, not local chat-template
	// extensions. Preserve their existing controls on API connections.
	profile, _ := ProfileForModel(model)
	return profile.Family == "deepseek" || profile.Family == "xai"
}

func newClient(baseURL, apiKey, model string, coreTransport bool) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if coreTransport {
		apiKey = localipc.CoreAPIKey(baseURL, apiKey)
	}
	if apiKey == "" {
		apiKey = DummyAPIKey
	}
	c := &Client{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		APIKey:        apiKey,
		Model:         model,
		coreTransport: coreTransport,
		// Bound ordinary non-streaming requests. ChatStream uses a request-local
		// copy without this total timeout; its progress watchdog and caller
		// context own the lifetime of the SSE request instead.
		HTTP: &http.Client{Timeout: 20 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	if dir := os.Getenv("MEADOW_PROVIDER_RECORD_DIR"); dir != "" {
		prefix := os.Getenv("MEADOW_PROVIDER_RECORD_PREFIX")
		if prefix == "" {
			prefix = "session"
		}
		if rec, err := NewRecorder(dir, prefix); err == nil {
			c.Recorder = rec
		} else {
			log.Printf("provider recorder unavailable: %v", err)
		}
	}
	return c
}

// BuildRequest applies the MLX request-shaping pipeline in the same order as
// the TS provider: repair orphans, merge stops, then model-conditional
// Gemma-4 overrides.
func (c *Client) BuildRequest(req ChatRequest) ChatRequest {
	if req.Model == "" {
		req.Model = c.Model
	}
	if !c.externalCompatible && len(req.Tools) > 0 && strings.TrimSpace(req.CacheSalt) == "" {
		c.toolCacheScopeOnce.Do(func() {
			var entropy [16]byte
			if _, err := rand.Read(entropy[:]); err == nil {
				c.toolCacheScope = "meadow-session:" + hex.EncodeToString(entropy[:])
				return
			}
			c.toolCacheScope = fmt.Sprintf("meadow-session:%d:%p", time.Now().UnixNano(), c)
		})
		req.CacheSalt = c.toolCacheScope
	}
	req.Messages = RepairOrphanedToolCalls(req.Messages)
	profile, _ := ProfileForModel(req.Model)
	if c.externalCompatible {
		profile = ModelProfile{Streaming: true, Multimodal: c.externalImages}
	}
	req.Messages = FilterPersistedHistoryImages(req.Messages, profile.Multimodal)
	req.Messages = PrepareToolResultMedia(req.Messages, profile.Multimodal)
	if !c.externalCompatible && RequiresFlattenedToolHistory(req.Model) {
		req.Messages = FlattenToolHistory(req.Messages)
	}

	if c.LegacyMlxStops {
		isStaticWebWriteToolStage := len(req.Tools) > 0 && req.ToolChoice != nil &&
			hasSystemPromptFragment(req.Messages, "Static web staged write")
		if !isStaticWebWriteToolStage {
			req.Stop = MergeStopSequences(req.Stop)
		}
	}

	if !c.externalCompatible || c.configuredCoreInference {
		ApplyLocalModelSafetyOverrides(&req)
	}
	if limit := c.OutputTokenLimit(); limit > 0 && (req.MaxTokens == nil || *req.MaxTokens > limit) {
		req.MaxTokens = &limit
	}
	if c.externalCompatible && !c.configuredCoreInference {
		req.CacheSalt = ""
		req.StreamedToolDraft = ""
	}
	if c.supportsInferenceControls(req.Model) {
		c.applyInferenceControls(&req)
	} else {
		req.CacheSalt = ""
		req.EnableThinking, req.ChatTemplateKwargs = nil, nil
		req.Thinking, req.ReasoningEffort = nil, ""
		req.StreamedToolDraft = ""
	}
	return req
}

// StreamResult is the reassembled outcome of one streaming completion.
type StreamResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	// OutputTokenLimit is the final request cap after provider/transport shaping.
	// It is public numeric diagnostics, independent of private reasoning text.
	OutputTokenLimit int
	// Private native continuation state. Never render or include in traces.
	ProviderReasoning *ProviderReasoning `json:"-"`
	// Truncated is true when a tool-call buffer ended mid-JSON, regardless
	// of what finish_reason claims.
	Truncated bool
	Usage     *Usage
	// TranscriptPath is set when a Recorder captured this exchange.
	TranscriptPath string
	// Repaired is true when tool-call-repair promoted a plain-text tool call
	// (the model printed JSON / a <tool_call> tag instead of emitting native
	// tool_calls) into result.ToolCalls.
	Repaired bool
	// ProtocolLeak marks model-emitted tool protocol markup removed from
	// assistant content after it had already reached a visible stream observer.
	ProtocolLeak bool
	// ProtocolRecovered marks protocol markup quarantined before it reached a
	// visible stream observer. Keep this distinct from ProtocolLeak so quality
	// gates can require zero user-visible leakage without hiding raw model drift.
	ProtocolRecovered bool
	// UncommittedMutationNarration marks a model response that described a
	// file mutation through a display/prose protocol instead of sending the
	// mutation as a native tool call. The runtime uses this structural signal
	// to prevent a validator in the same response from running against stale
	// workspace bytes.
	UncommittedMutationNarration bool
}

// StreamHooks are optional per-call observers for one streaming completion.
type StreamHooks struct {
	// OnDelta receives content fragments as they arrive — answer text with
	// thought=false, thought-channel text with thought=true (the wire
	// markers themselves are never delivered).
	OnDelta func(delta string, thought bool)
	// OnPrivateReasoningProgress reports only the presence of real native
	// reasoning bytes. It never carries their content or makes auto/off expose
	// them through OnDelta; observers can distinguish generation from silence.
	OnPrivateReasoningProgress func()
	// OnToolDraft reports tool-call argument bytes accumulating during
	// generation — the only liveness signal while the model writes a big
	// file into a write_file call.
	OnToolDraft func(name string, totalBytes int)
	// OnToolDraftProgress carries native tool argument deltas for a transient
	// editor preview. Callers must never persist these incomplete bytes; the
	// fully decoded tool invocation remains the only commit boundary.
	OnToolDraftProgress func(ToolDraftProgress)
	// OnProgress receives observable Meadow Core runtime state. It never
	// carries hidden reasoning or inferred planning stages.
	OnProgress func(InferenceProgress)
}

type ToolDraftProgress struct {
	Index          int    `json:"index"`
	CallID         string `json:"call_id,omitempty"`
	Name           string `json:"name"`
	ArgumentsDelta string `json:"arguments_delta"`
	TotalBytes     int    `json:"total_bytes"`
}

// ErrThoughtBurn marks a local-template stream rejected for excessive
// unsolicited thought-only output. It is a compatibility heuristic, not
// evidence that native API reasoning has exhausted its token budget.
var ErrThoughtBurn = errors.New("thought channel burn")

// ErrMalformedToolCall is returned when a provider stream contains an
// unfinished or schema-invalid tool-call payload. The engine may retry this
// protocol failure once; ordinary HTTP 422 errors remain ordinary provider
// errors unless the server explicitly identifies malformed tool syntax.
var ErrMalformedToolCall = errors.New("malformed tool call")

// ToolCallRecoveryReason is the machine-readable reason a model-emitted tool
// call was rejected before execution. Runtimes should branch on this value,
// not on localized error text.
type ToolCallRecoveryReason string

const (
	ToolCallRecoveryIncompleteProtocol ToolCallRecoveryReason = "incomplete_protocol"
	ToolCallRecoveryMissingRequired    ToolCallRecoveryReason = "missing_required_arguments"
	ToolCallRecoverySchemaInvalid      ToolCallRecoveryReason = "schema_invalid_arguments"
)

// ToolCallRecoveryAction is a provider recommendation, not an execution
// command. The runtime still owns which registered tool to expose next.
type ToolCallRecoveryAction string

const (
	ToolCallRecoveryRetryNative   ToolCallRecoveryAction = "retry_native_tool_call"
	ToolCallRecoveryShortMutation ToolCallRecoveryAction = "use_short_mutation"
)

// ToolCallRecoveryError keeps malformed-call recovery deterministic across
// model families. It unwraps to ErrMalformedToolCall for existing bounded retry
// policy, while errors.As gives newer runtimes enough information to switch a
// failed long write_file attempt to a shorter edit/chunk strategy.
type ToolCallRecoveryError struct {
	Reason          ToolCallRecoveryReason
	Action          ToolCallRecoveryAction
	ToolName        string
	MissingRequired []string
	ArgumentBytes   int
}

func (e *ToolCallRecoveryError) Error() string {
	if e == nil {
		return ErrMalformedToolCall.Error()
	}
	detail := string(e.Reason)
	if e.ToolName != "" {
		detail += " tool=" + e.ToolName
	}
	if len(e.MissingRequired) > 0 {
		detail += " missing=" + strings.Join(e.MissingRequired, ",")
	}
	return ErrMalformedToolCall.Error() + ": " + detail + " recovery=" + string(e.Action)
}

func (e *ToolCallRecoveryError) Unwrap() error { return ErrMalformedToolCall }

func newToolCallRecoveryError(
	reason ToolCallRecoveryReason,
	toolName string,
	missing []string,
	argumentBytes int,
) error {
	action := ToolCallRecoveryRetryNative
	if toolName == "write_file" {
		action = ToolCallRecoveryShortMutation
	}
	return &ToolCallRecoveryError{
		Reason: reason, Action: action, ToolName: toolName,
		MissingRequired: append([]string(nil), missing...),
		ArgumentBytes:   argumentBytes,
	}
}

// ErrGenerationStall marks a correlated model request that stayed connected
// but made no observable generation progress for the configured watchdog
// window. The Kernel may rebuild one fresh, narrower request; callers must not
// treat this as an ordinary HTTP failure or retry it without a bound.
var ErrGenerationStall = errors.New("generation stalled")

func serverStatusError(status string, raw []byte) error {
	message := strings.TrimSpace(string(raw))
	err := fmt.Errorf("server returned %s: %s", status, message)
	if capacity := contextLimitStatusError(status, raw, err); capacity != nil {
		return capacity
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "malformed") && strings.Contains(lower, "tool") {
		return fmt.Errorf("%w: %v", ErrMalformedToolCall, err)
	}
	return err
}

// ChatStream runs one streaming chat completion, reassembling content and
// tool calls. hooks may be nil.
func (c *Client) ChatStream(
	ctx context.Context,
	req ChatRequest,
	hooks *StreamHooks,
) (result *StreamResult, returnErr error) {
	if c.externalProtocol == "anthropic" {
		return c.chatAnthropic(ctx, req, hooks)
	}
	requestAPIKey, credentialErr := c.resolveRequestCredential(ctx, req.Model)
	if credentialErr != nil {
		return nil, credentialErr
	}
	ctx = context.WithValue(ctx, resolvedCredentialKey{}, requestAPIKey)
	req = c.BuildRequest(req)
	repairTools := append([]Tool(nil), req.Tools...)
	streamedToolDraft := strings.TrimSpace(req.StreamedToolDraft)
	if !c.externalCompatible && UsesStreamedToolTransport(req) {
		// Preserve the registered schema in repairTools, but do not send OpenAI
		// tools/tool_choice to MLX. Qwen's native JSON grammar buffers a large
		// string argument until the closing brace; the equivalent tagged protocol
		// streams continuously and is schema-checked below before execution.
		req.Tools = nil
		// vllm-mlx can run its global auto tool parser even when `tools` is
		// omitted. That parser buffers an entire tagged write_file call before
		// emitting it, defeating Meadow's live draft preview. Explicitly disable
		// server-side tool parsing; repairTools remains the schema/commit authority
		// after the complete tagged call has streamed through ordinary content.
		req.ToolChoice = "none"
		// A complete source file plus the tagged call envelope routinely exceeds
		// 4K tokens for a small web game. 4K caused the model to hit `length`
		// after making visible progress, then retry from scratch forever. Keep a
		// bounded ceiling, but leave enough room to close and validate one source
		// file in a single call.
		streamedArtifactTokenLimit := 8192
		if streamedToolDraft == "edit" && req.UserThinkingMode != ThinkingOn {
			// An edit should contain only an exact old_string and its bounded
			// replacement. Giving it the whole-file budget encouraged weaker local
			// models to restate an 8-12KB document, then stop before the closing tool
			// marker. Keep the live draft useful while forcing the recovery path back
			// to a small transactional patch.
			streamedArtifactTokenLimit = 2048
		}
		if req.MaxTokens == nil || *req.MaxTokens > streamedArtifactTokenLimit {
			limit := streamedArtifactTokenLimit
			req.MaxTokens = &limit
		}
	} else {
		streamedToolDraft = ""
	}
	// Transport caps must precede the final effort resolution. An explicit
	// thinking selection already reserves answer/tool capacity, so it does not
	// use the legacy answer-only 2K edit heuristic above. Remaining caps still
	// apply, including the actual model/backend output capacity.
	if c.supportsInferenceControls(req.Model) {
		c.applyInferenceControls(&req)
	}
	if req.UserThinkingMode == ThinkingOn && SupportsReasoningEffortControl(req.Model) {
		if budget, ok := req.ChatTemplateKwargs["reasoning_budget"].(int); ok && budget < 1 {
			return nil, fmt.Errorf("configured output token capacity cannot fit both explicit thinking and an answer; increase the output limit or select rapid mode")
		}
	}
	defer func() {
		if result != nil && req.MaxTokens != nil {
			result.OutputTokenLimit = *req.MaxTokens
		}
	}()
	watchdogProgress := make(chan struct{}, 1)
	signalWatchdogProgress := func() {
		select {
		case watchdogProgress <- struct{}{}:
		default:
		}
	}
	var lastPrompt, lastPrefill, lastCompletion atomic.Int64
	requestID, observedHooks, stopProgress := c.observeInference(
		ctx,
		req.Model,
		req.Messages,
		hooks,
		func(progress InferenceProgress) {
			prompt := int64(progress.PromptTokens)
			if prompt <= 0 {
				prompt = int64(progress.PrefillTotalTokens)
			}
			if prompt > lastPrompt.Load() {
				lastPrompt.Store(prompt)
			}
			prefill := int64(progress.PrefillProcessedTokens)
			if prefill > lastPrefill.Load() {
				lastPrefill.Store(prefill)
				signalWatchdogProgress()
			}
			completion := int64(progress.CompletionTokens)
			if completion > lastCompletion.Load() {
				lastCompletion.Store(completion)
				signalWatchdogProgress()
			}
		},
	)
	if requestID == "" && c.Telemetry != nil {
		requestID = telemetryRequestID()
	}
	requestStarted := time.Now()
	defer func() {
		stopProgress()
		if requestID == "" || hooks == nil || hooks.OnProgress == nil {
			return
		}
		phase := InferenceCompleted
		if returnErr != nil {
			phase = InferenceFailed
		}
		hooks.OnProgress(InferenceProgress{
			RequestID: requestID,
			Model:     req.Model,
			Phase:     phase,
			Elapsed:   time.Since(requestStarted),
		})
	}()
	// Profiles whose tool-call format has not yet been proven on SSE take a
	// non-streaming completion instead. Meadow 35B is explicitly
	// streaming-capable; older/generic Qwen profiles keep this repair path.
	if prof, ok := ProfileForModel(req.Model); !c.externalCompatible && ok && !prof.Streaming {
		return c.chatNonStreaming(ctx, req, repairTools, streamedToolDraft, observedHooks, requestID)
	}
	req.Stream = true
	if c.usesCoreTransport() || c.externalCompatible {
		req.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	recordedBody := requestBodyForRecorder(req, body)

	// Stall watchdog: cancel the request when generation stops making
	// progress. consumeSSE resets the timer on every real chunk. Before the
	// first chunk the request may legitimately sit in the server's queue
	// behind other sessions (and pay a cold prefill), so the pre-first-chunk
	// window is 3x more lenient than the mid-stream one.
	stall := c.StallTimeout
	if stall <= 0 {
		stall = defaultStallTimeout
	}
	queueGrace := 3 * stall
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopAbortWatch := c.watchCoreRequestCancellation(ctx, requestID)
	defer stopAbortWatch()
	var stalled atomic.Bool
	watchdog := time.NewTimer(queueGrace)
	watchdogDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-watchdogProgress:
				if !watchdog.Stop() {
					select {
					case <-watchdog.C:
					default:
					}
				}
				watchdog.Reset(stall)
			case <-watchdog.C:
				stalled.Store(true)
				cancel()
				return
			case <-watchdogDone:
				return
			}
		}
	}()
	defer func() {
		close(watchdogDone)
		if !watchdog.Stop() {
			select {
			case <-watchdog.C:
			default:
			}
		}
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if requestAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+requestAPIKey)
	}
	if requestID != "" {
		httpReq.Header.Set("X-Meadow-Request-ID", requestID)
	}

	// http.Client.Timeout covers the entire response body, not just connection
	// setup. It used to kill healthy long generations at exactly 20 minutes.
	// Keep the transport, cookie jar and redirect policy, but let the watchdog
	// above and explicit caller cancellation/deadlines govern this stream.
	// Never mutate c.HTTP: status, abort and non-streaming calls share it.
	streamHTTP := *c.HTTP
	streamHTTP.Timeout = 0
	var rawSSE bytes.Buffer
	var responseStatus int
	recorder := c.Recorder
	if IsDeepSeekModel(req.Model) {
		// Raw HTTP recordings contain provider-private reasoning on both sides.
		// Keep it in the dedicated private chat state, never diagnostic corpora.
		recorder = nil
	}
	recording := recorder.beginCompletion(recordedBody, requestID, true)
	defer func() {
		if path, err := recording.finish(rawSSE.Bytes(), responseStatus, returnErr, ctx.Err()); err == nil && path != "" && result != nil {
			result.TranscriptPath = path
		}
	}()
	observation := c.beginTelemetry(ctx, requestID, req.Model, body, true)
	observation.annotate(httpReq)
	observedHooks = observation.wrap(observedHooks)
	defer func() { observation.finish(result, returnErr, responseStatus) }()
	resp, err := streamHTTP.Do(httpReq)
	if resp != nil {
		responseStatus = resp.StatusCode
	}
	if err != nil {
		if stalled.Load() {
			return nil, fmt.Errorf("%w: 模型超過 %s 沒有任何生成進展（server 可能卡死，建議以較窄的新請求重試）", ErrGenerationStall, stall)
		}
		return nil, fmt.Errorf("POST /chat/completions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_, _ = rawSSE.Write(msg)
		return nil, serverStatusError(resp.Status, msg)
	}

	if IsDeepSeekModel(req.Model) && observedHooks != nil && observedHooks.OnDelta != nil {
		privateHooks := *observedHooks
		onDelta := observedHooks.OnDelta
		privateHooks.OnDelta = func(delta string, thought bool) {
			if !thought {
				onDelta(delta, false)
			}
		}
		observedHooks = &privateHooks
	}
	result, err = c.consumeSSE(
		resp.Body, &rawSSE, observedHooks, signalWatchdogProgress,
		len(repairTools) > 0 || IsLFM25Model(req.Model),
		IsLFM25Model(req.Model), IsGemma12Model(req.Model),
		req.UserThinkingMode == ThinkingOn && !IsDeepSeekModel(req.Model),
		req.UserThinkingMode != ThinkingOn && requiresUnsolicitedThoughtGuard(req.Model), streamedToolDraft,
	)
	if result != nil && result.ProviderReasoning != nil {
		if IsDeepSeekModel(req.Model) {
			result.ProviderReasoning.Model = req.Model
		} else {
			result.ProviderReasoning = nil
		}
	}
	if err == nil && result != nil && len(result.ToolCalls) > 0 {
		result.ToolCalls = coerceRecoveredToolCallArguments(result.ToolCalls, repairTools)
	}
	if err != nil && stalled.Load() {
		err = fmt.Errorf("%w: 模型超過 %s 沒有任何生成進展（server 可能卡死，建議以較窄的新請求重試）", ErrGenerationStall, stall)
	}
	// tool-call-repair: when native tool_calls came back empty but the model
	// printed the call as unquoted text (JSON / <tool_call> tag), recover
	// it so the engine still sees a tool call instead of ending as prose. Markdown
	// literal examples are excluded before repair. Gated
	// on the request's registered tool names so prose containing JSON is never
	// mistaken for a call.
	if err == nil && result != nil && len(result.ToolCalls) == 0 {
		if calls, cleaned := RepairTextToolCallsWithSchemas(result.Content, repairTools, req.Model); len(calls) > 0 {
			result.ToolCalls = namespaceRepairedToolCalls(calls, requestID)
			result.Content = cleaned
			result.Repaired = true
		}
	}
	// A large single-file web artifact may reach the generation cap after a
	// complete </html> but before the textual tool envelope closes. Recover
	// only that complete HTML shape; never commit a partial document or another
	// tool's truncated arguments.
	if err == nil && result != nil && len(result.ToolCalls) == 0 &&
		strings.EqualFold(result.FinishReason, "length") &&
		(streamedToolDraft == "write_file" || streamedToolDraft == "*") &&
		toolSchemasHaveName(repairTools, "write_file") {
		if call, ok := RecoverCompleteTruncatedHTMLWrite(result.Content); ok {
			result.ToolCalls = namespaceRepairedToolCalls([]ToolCall{call}, requestID)
			result.ToolCalls = coerceRecoveredToolCallArguments(result.ToolCalls, repairTools)
			result.Content = ""
			result.Repaired = true
			result.ProtocolRecovered = true
		}
	}
	// The native parser retains a best-effort diagnostic object for an
	// incomplete JSON buffer. Never execute that object merely because it now
	// marshals: only a single complete, workspace-scoped HTML write has a safe
	// commit boundary. Every other truncated call becomes an explicit recovery
	// signal for the runtime.
	if err == nil && result != nil && result.Truncated && len(result.ToolCalls) > 0 {
		if len(result.ToolCalls) == 1 {
			if call, ok := RecoverCompleteTruncatedHTMLToolCall(result.ToolCalls[0]); ok {
				result.ToolCalls[0] = call
				result.Repaired = true
				result.ProtocolRecovered = true
			} else {
				return result, newToolCallRecoveryError(
					ToolCallRecoveryIncompleteProtocol,
					result.ToolCalls[0].Function.Name,
					nil,
					len(result.ToolCalls[0].Function.Arguments),
				)
			}
		} else {
			return result, newToolCallRecoveryError(
				ToolCallRecoveryIncompleteProtocol, "", nil, 0,
			)
		}
	}
	if err == nil && result != nil && len(result.ToolCalls) > 0 {
		if failure := firstToolCallSchemaFailure(result.ToolCalls, repairTools); failure != nil {
			return result, newToolCallRecoveryError(
				failure.reason, failure.toolName, failure.missingRequired, failure.argumentBytes,
			)
		}
	}
	if err == nil && result != nil && len(result.ToolCalls) == 0 &&
		(result.Truncated || LooksLikeIncompleteToolCall(result.Content, toolNames(repairTools))) {
		toolName := ""
		if streamedToolDraft != "*" {
			toolName = streamedToolDraft
		} else if detected, ok := firstIncompleteProtocolToolName(result.Content); ok &&
			toolSchemasHaveName(repairTools, detected) {
			// Wildcard transport deliberately keeps all registered tools available,
			// but recovery must not forget which concrete protocol the model began.
			// Returning that name lets the runtime retry with one schema instead of
			// presenting the entire tool surface and reproducing the same ambiguity.
			toolName = detected
		}
		return result, newToolCallRecoveryError(
			ToolCallRecoveryIncompleteProtocol, toolName, nil, len(result.Content),
		)
	}
	if err == nil && result != nil {
		result.UncommittedMutationNarration = LooksLikeMutationNarration(result.Content) ||
			(toolSchemasHaveName(repairTools, "write_file") && LooksLikeCompleteArtifactDraft(result.Content))
	}
	if err == nil && result != nil && (len(result.ToolCalls) > 0 ||
		result.UncommittedMutationNarration || IsLFM25Model(req.Model)) {
		var rawProtocol bool
		result.Content, rawProtocol = CleanNativeToolContent(result.Content)
		result.ProtocolLeak = rawProtocol && !result.ProtocolRecovered
		result.ProtocolRecovered = result.ProtocolRecovered || rawProtocol
	}
	// vllm-mlx can surface an internal scheduler/cache exception as HTTP 200
	// with a final SSE chunk whose finish_reason is "error" and usage is zero.
	// Treating that as an ordinary empty model turn hides the runtime failure
	// and sends the agent through several useless nudge loops.
	if err == nil && result != nil && strings.EqualFold(result.FinishReason, "error") &&
		strings.TrimSpace(result.Content) == "" && len(result.ToolCalls) == 0 {
		return result, errors.New("model server ended generation with finish_reason=error and no output")
	}
	// Some local Core builds did not yet emit the requested terminal usage
	// chunk. Preserve honest non-zero accounting from the correlated runtime
	// status; when a very short request finishes between polls, use the same
	// explicitly approximate tokenizer that powers immediate Desktop progress.
	if err == nil && result != nil && result.Usage == nil && c.usesCoreTransport() {
		promptTokens := int(lastPrompt.Load())
		if promptTokens <= 0 {
			for _, message := range req.Messages {
				promptTokens += EstimateTextTokens(MessageText(message))
			}
		}
		completionTokens := int(lastCompletion.Load())
		if completionTokens <= 0 && strings.TrimSpace(result.Content) != "" {
			completionTokens = EstimateTextTokens(result.Content)
		}
		if promptTokens > 0 || completionTokens > 0 {
			result.Usage = &Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens, TotalTokens: promptTokens + completionTokens}
		}
	}
	return result, err
}

// chatNonStreaming runs one NON-streaming completion for models flagged
// Streaming:false in the registry. It returns the same StreamResult shape so
// the engine is oblivious to the transport, and runs the identical
// tool-call-repair gate as the streaming path — the whole reason this path
// exists is that the full text (Qwen's Python-call / native tool syntax) only
// arrives intact when special tokens are not stripped, which the non-streaming
// detokenizer guarantees.
func (c *Client) chatNonStreaming(
	ctx context.Context,
	req ChatRequest,
	repairTools []Tool,
	streamedToolDraft string,
	hooks *StreamHooks,
	requestID string,
) (result *StreamResult, returnErr error) {
	requestAPIKey, credentialErr := c.resolveRequestCredential(ctx, req.Model)
	if credentialErr != nil {
		return nil, credentialErr
	}
	// Own the non-streaming timeout in the same context the Core cancellation
	// watcher observes. http.Client.Timeout uses a private cancellation context;
	// letting it expire alone leaves the Core request without an explicit abort.
	completionHTTP := *c.HTTP
	var cancel context.CancelFunc
	if completionHTTP.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, completionHTTP.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	completionHTTP.Timeout = 0
	stopAbortWatch := c.watchCoreRequestCancellation(ctx, requestID)
	defer stopAbortWatch()
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	recordedBody := requestBodyForRecorder(req, body)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if requestAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+requestAPIKey)
	}
	if requestID != "" {
		httpReq.Header.Set("X-Meadow-Request-ID", requestID)
	}

	var raw []byte
	var responseStatus int
	recorder := c.Recorder
	if IsDeepSeekModel(req.Model) {
		recorder = nil
	}
	recording := recorder.beginCompletion(recordedBody, requestID, false)
	defer func() {
		if path, err := recording.finish(raw, responseStatus, returnErr, ctx.Err()); err == nil && path != "" && result != nil {
			result.TranscriptPath = path
		}
	}()
	observation := c.beginTelemetry(ctx, requestID, req.Model, body, false)
	observation.annotate(httpReq)
	hooks = observation.wrap(hooks)
	defer func() { observation.finish(result, returnErr, responseStatus) }()
	resp, err := completionHTTP.Do(httpReq)
	if resp != nil {
		responseStatus = resp.StatusCode
	}
	if err != nil {
		return nil, fmt.Errorf("POST /chat/completions: %w", err)
	}
	defer resp.Body.Close()

	raw, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read completion: %w", err)
	}
	// The remote response is now complete. Disarm before local decoding,
	// tool repair, hooks or recorder writes can outlast the transport timeout.
	_ = resp.Body.Close()
	stopAbortWatch()
	if resp.StatusCode != http.StatusOK {
		return nil, serverStatusError(resp.Status, raw)
	}

	var cr ChatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("decode completion: %w", err)
	}
	result = &StreamResult{Usage: cr.Usage}
	if len(cr.Choices) > 0 {
		result.Content = StripThoughtChannel(cr.Choices[0].Message.Content)
		result.ToolCalls = cr.Choices[0].Message.ToolCalls
		result.FinishReason = cr.Choices[0].FinishReason
		if IsDeepSeekModel(req.Model) && cr.Choices[0].Message.ProviderReasoning != nil {
			result.ProviderReasoning = &ProviderReasoning{Model: req.Model, Content: cr.Choices[0].Message.ProviderReasoning.Content}
		}
	}
	// tool-call-repair: same gate as the streaming path (client.go ChatStream).
	if len(result.ToolCalls) > 0 {
		result.ToolCalls = coerceRecoveredToolCallArguments(result.ToolCalls, repairTools)
	}
	if len(result.ToolCalls) == 0 {
		if calls, cleaned := RepairTextToolCallsWithSchemas(result.Content, repairTools, req.Model); len(calls) > 0 {
			result.ToolCalls = namespaceRepairedToolCalls(calls, requestID)
			result.Content = cleaned
			result.Repaired = true
		}
	}
	if len(result.ToolCalls) == 0 && strings.EqualFold(result.FinishReason, "length") &&
		(streamedToolDraft == "write_file" || streamedToolDraft == "*") &&
		toolSchemasHaveName(repairTools, "write_file") {
		if call, ok := RecoverCompleteTruncatedHTMLWrite(result.Content); ok {
			result.ToolCalls = namespaceRepairedToolCalls([]ToolCall{call}, requestID)
			result.ToolCalls = coerceRecoveredToolCallArguments(result.ToolCalls, repairTools)
			result.Content = ""
			result.Repaired = true
			result.ProtocolRecovered = true
		}
	}
	if len(result.ToolCalls) > 0 {
		if failure := firstToolCallSchemaFailure(result.ToolCalls, repairTools); failure != nil {
			return result, newToolCallRecoveryError(
				failure.reason, failure.toolName, failure.missingRequired, failure.argumentBytes,
			)
		}
	}
	if len(result.ToolCalls) == 0 &&
		LooksLikeIncompleteToolCall(result.Content, toolNames(repairTools)) {
		toolName := ""
		if streamedToolDraft != "*" {
			toolName = streamedToolDraft
		} else if detected, ok := firstIncompleteProtocolToolName(result.Content); ok &&
			toolSchemasHaveName(repairTools, detected) {
			toolName = detected
		}
		return result, newToolCallRecoveryError(
			ToolCallRecoveryIncompleteProtocol, toolName, nil, len(result.Content),
		)
	}
	result.UncommittedMutationNarration = LooksLikeMutationNarration(result.Content) ||
		(toolSchemasHaveName(repairTools, "write_file") && LooksLikeCompleteArtifactDraft(result.Content))
	if len(result.ToolCalls) > 0 || result.UncommittedMutationNarration || IsLFM25Model(req.Model) {
		var rawProtocol bool
		result.Content, rawProtocol = CleanNativeToolContent(result.Content)
		// Non-streaming content is cleaned before its one visible OnDelta.
		result.ProtocolRecovered = rawProtocol
		result.ProtocolLeak = false
	}
	if strings.EqualFold(result.FinishReason, "error") && strings.TrimSpace(result.Content) == "" && len(result.ToolCalls) == 0 {
		return result, errors.New("model server ended generation with finish_reason=error and no output")
	}
	// Deliver the prose once so a TUI still renders the turn; there is no
	// incremental tool-draft signal without a stream.
	if hooks != nil && hooks.OnDelta != nil && result.Content != "" {
		hooks.OnDelta(result.Content, false)
	}
	return result, nil
}

// watchCoreRequestCancellation gives local Meadow Core an explicit request-ID
// cancellation signal. Closing only the public HTTP stream is not sufficient
// for SGLang-MLX: its tokenizer can retire the request while the private decode
// worker continues consuming the scheduler. The next chat then overlaps that
// orphan and can wedge or terminate the model server.
//
// The watcher is stopped before ChatStream's normal deferred context cancel,
// so successfully completed requests keep their resident worker and cache.
func (c *Client) watchCoreRequestCancellation(
	ctx context.Context,
	requestID string,
) func() {
	if c == nil || requestID == "" || !c.usesCoreTransport() {
		return func() {}
	}
	done := make(chan struct{})
	var finishOnce sync.Once
	finish := func(abort bool) {
		// Disarming is terminal, just like aborting. A deferred second stop
		// after local postprocessing must never rearm a completed request.
		finishOnce.Do(func() {
			close(done)
			if abort {
				c.abortCoreRequest(requestID)
			}
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			finish(true)
		case <-done:
		}
	}()
	return func() {
		finish(ctx.Err() != nil)
	}
}

func (c *Client) abortCoreRequest(requestID string) {
	body, err := json.Marshal(map[string]string{"request_id": requestID})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/abort_request",
		bytes.NewReader(body),
	)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

func (c *Client) consumeSSE(
	r io.Reader,
	raw *bytes.Buffer,
	hooks *StreamHooks,
	onProgress func(),
	filterToolProtocol bool,
	awaitLeadingHTMLThoughtClose bool,
	filterModalityTokens bool,
	emitReasoningContent bool,
	guardUnsolicitedThought bool,
	streamedToolDraft string,
) (*StreamResult, error) {
	if hooks == nil {
		hooks = &StreamHooks{}
	}
	parser := NewStreamingToolCallParser()
	result := &StreamResult{}
	var content strings.Builder
	var reasoning strings.Builder
	receivedReasoning := false
	defer func() {
		if receivedReasoning {
			result.ProviderReasoning = &ProviderReasoning{Content: reasoning.String()}
		}
	}()
	// The streaming path leaks <|channel> spans that the server only
	// strips on the non-streaming path; the filter splits them out so
	// frontends can render thoughts dimmed (or not at all) — the raw
	// markers never reach OnDelta.
	thoughts := &ThoughtStreamFilter{awaitLeadingHTMLClose: awaitLeadingHTMLThoughtClose}
	toolProtocol := NewNativeToolProtocolStreamFilter(filterToolProtocol)
	modalityTokens := newModalityTokenStreamFilter(filterModalityTokens)
	var thoughtChars, answerChars int
	streamedDraftBytes := 0
	streamedDraftName := ""
	emit := func(delta string) {
		delta = modalityTokens.Feed(delta)
		if delta == "" {
			return
		}
		answer, thought := thoughts.Feed(delta)
		thoughtChars += utf8.RuneCountInString(thought)
		answerChars += utf8.RuneCountInString(answer)
		answer = toolProtocol.Feed(answer)
		if hooks.OnDelta == nil {
			return
		}
		if thought != "" {
			hooks.OnDelta(thought, true)
		}
		if answer != "" {
			hooks.OnDelta(answer, false)
		}
	}
	draftBytes := map[int]int{}
	draftNames := map[int]string{}
	draftCallIDs := map[int]string{}

	scanner := bufio.NewScanner(io.TeeReader(r, raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// A malformed SSE line is a server bug, not a stream killer —
			// skip it and let reassembly continue.
			continue
		}
		// Only REAL progress feeds the stall watchdog: answer text, private
		// reasoning text, tool bytes, a finish reason, or usage. SGLang emits
		// Muse's private reasoning in `reasoning_content`; ignoring it makes a
		// healthy dense-model turn look idle until the 3x queue grace expires.
		// It is private liveness by default. Only an explicit /think on request
		// may forward it to OnDelta's thought channel; auto/off never expose it
		// or persist it as assistant answer text.
		// vllm-mlx also emits a content-free
		// role chunk at t=0 — BEFORE prefill — and counting it collapsed the
		// 360s first-token window to 120s, so any prompt whose prefill ran
		// past 2 minutes (a RAG template, a long resumed history, queueing
		// behind another session) was falsely killed as "stalled".
		progressed := chunk.Usage != nil
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			if progressed && onProgress != nil {
				onProgress()
			}
			continue
		}
		choice := chunk.Choices[0]
		// Some OpenAI-compatible providers (including Heimdallm) flatten
		// native reasoning into `reasoning` instead of `reasoning_content`.
		// Prefer the canonical nonempty field so a compatibility alias does
		// not duplicate a single fragment. Presence-only metadata and empty
		// values remain insufficient to reset the generation watchdog.
		reasoningText := choice.Delta.ReasoningContent
		if (reasoningText == nil || *reasoningText == "") && choice.Delta.Reasoning != nil {
			reasoningText = choice.Delta.Reasoning
		}
		if choice.Delta.Content != "" || (reasoningText != nil && *reasoningText != "") ||
			len(choice.Delta.ToolCalls) > 0 ||
			(choice.FinishReason != nil && *choice.FinishReason != "") {
			progressed = true
		}
		if progressed && onProgress != nil {
			onProgress()
		}
		if choice.Delta.Content != "" {
			content.WriteString(choice.Delta.Content)
			if streamedToolDraft != "" {
				streamedDraftBytes += len(choice.Delta.Content)
				draftName := streamedToolDraft
				if draftName == "*" {
					draftName = ""
					if match := taggedFunctionNameRe.FindStringSubmatch(content.String()); len(match) == 2 {
						draftName = match[1]
					} else if match := taggedToolNameRe.FindStringSubmatch(content.String()); len(match) == 2 {
						draftName = match[1]
					}
				}
				// With wildcard tagged transport the function name often arrives
				// after the opening protocol bytes. Do not emit an anonymous draft
				// that the editor cannot associate with write_file. When the name is
				// first discovered, replay the accumulated prefix once so the live
				// editor receives file_path and the opening content marker too.
				if draftName != "" && hooks.OnToolDraft != nil {
					hooks.OnToolDraft(draftName, streamedDraftBytes)
				}
				if draftName != "" && hooks.OnToolDraftProgress != nil {
					delta := choice.Delta.Content
					if streamedDraftName == "" {
						delta = content.String()
					}
					hooks.OnToolDraftProgress(ToolDraftProgress{
						Index: -1, Name: draftName,
						ArgumentsDelta: delta,
						TotalBytes:     streamedDraftBytes,
					})
					streamedDraftName = draftName
				}
			}
			// Ordinary prose must remain live even when a mutation tool is
			// available. emit()'s protocol filter releases prose but quarantines
			// bytes from the first confirmed tagged-tool marker onward.
			emit(choice.Delta.Content)
		}
		if reasoningText != nil {
			receivedReasoning = true
			reasoning.WriteString(*reasoningText)
			if *reasoningText != "" && hooks.OnPrivateReasoningProgress != nil {
				hooks.OnPrivateReasoningProgress()
			}
			thoughtChars += utf8.RuneCountInString(*reasoningText)
			if emitReasoningContent && hooks.OnDelta != nil {
				hooks.OnDelta(*reasoningText, true)
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			parser.AddChunk(tc.Index, tc.Function.Arguments, tc.ID, tc.Function.Name)
			if tc.ID != "" {
				draftCallIDs[tc.Index] = tc.ID
			}
			if tc.Function.Name != "" {
				draftNames[tc.Index] = tc.Function.Name
			}
			draftBytes[tc.Index] += len(tc.Function.Arguments)
			if hooks.OnToolDraft != nil {
				hooks.OnToolDraft(draftNames[tc.Index], draftBytes[tc.Index])
			}
			if hooks.OnToolDraftProgress != nil && tc.Function.Arguments != "" {
				hooks.OnToolDraftProgress(ToolDraftProgress{
					Index: tc.Index, CallID: draftCallIDs[tc.Index], Name: draftNames[tc.Index],
					ArgumentsDelta: tc.Function.Arguments,
					TotalBytes:     draftBytes[tc.Index],
				})
			}
		}
		// Local-template compatibility is independent of whether thoughts are
		// displayed. Native API reasoning can be valid while hidden; it uses
		// the provider budget, cancellation and actual progress watchdog.
		if guardUnsolicitedThought && thoughtChars > 4000 && answerChars == 0 && len(draftBytes) == 0 {
			return result, fmt.Errorf("思考通道輸出 %d 字元仍無正文或工具呼叫: %w", thoughtChars, ErrThoughtBurn)
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			result.FinishReason = *choice.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read SSE stream: %w", err)
	}
	if tail := modalityTokens.Flush(); tail != "" {
		answer, thought := thoughts.Feed(tail)
		thoughtChars += utf8.RuneCountInString(thought)
		answerChars += utf8.RuneCountInString(answer)
		answer = toolProtocol.Feed(answer)
		if hooks.OnDelta != nil {
			if thought != "" {
				hooks.OnDelta(thought, true)
			}
			if answer != "" {
				hooks.OnDelta(answer, false)
			}
		}
	}
	if hooks.OnDelta != nil {
		if tail := toolProtocol.Feed(thoughts.Flush()); tail != "" {
			hooks.OnDelta(tail, false)
		}
		if tail := toolProtocol.Flush(); tail != "" {
			hooks.OnDelta(tail, false)
		}
	}

	result.Content = StripThoughtChannel(content.String())
	if filterModalityTokens {
		result.Content = stripModalityControlTokens(result.Content)
	}
	result.ProtocolRecovered = toolProtocol.Suppressed()
	result.Truncated = parser.HasIncompleteToolCalls()
	if name, argumentBytes, unresolved := parser.UnresolvedMetadata(); unresolved {
		// A native tool delta is protocol evidence even when the backend omits
		// the function name. Reject the whole batch before exposing any executable
		// calls; surrounding prose and usage remain available for bounded recovery.
		return result, newToolCallRecoveryError(ToolCallRecoveryIncompleteProtocol, name, nil, argumentBytes)
	}
	if strings.EqualFold(result.FinishReason, "tool_calls") && !parser.HasBufferedToolCalls() {
		return result, newToolCallRecoveryError(ToolCallRecoveryIncompleteProtocol, "", nil, 0)
	}
	for _, call := range parser.GetCompletedToolCalls() {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", call.Index)
		}
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:   id,
			Type: "function",
			Function: FunctionCall{
				Name:      call.Name,
				Arguments: MarshalArgs(call.Args),
			},
		})
	}
	return result, nil
}

var modalityControlTokens = []string{
	"<image|>", "<|image>", "<|image|>",
	"<audio|>", "<|audio>", "<|audio|>",
	"<video|>", "<|video>", "<|video|>",
}

func stripModalityControlTokens(text string) string {
	for _, token := range modalityControlTokens {
		text = strings.ReplaceAll(text, token, "")
	}
	return text
}

// modalityTokenStreamFilter removes tokenizer control markers before a live
// delta reaches CLI/Desktop. It retains only a suffix that could be the start
// of a marker so split SSE chunks such as "<ima" + "ge|>" are also safe.
type modalityTokenStreamFilter struct {
	enabled bool
	pending string
}

func newModalityTokenStreamFilter(enabled bool) *modalityTokenStreamFilter {
	return &modalityTokenStreamFilter{enabled: enabled}
}

func (f *modalityTokenStreamFilter) Feed(delta string) string {
	if !f.enabled {
		return delta
	}
	f.pending += delta
	cleaned := stripModalityControlTokens(f.pending)
	hold := 0
	for _, token := range modalityControlTokens {
		for prefixLen := 1; prefixLen < len(token); prefixLen++ {
			if prefixLen <= len(cleaned) && strings.HasSuffix(cleaned, token[:prefixLen]) && prefixLen > hold {
				hold = prefixLen
			}
		}
	}
	if hold == 0 {
		f.pending = ""
		return cleaned
	}
	emitAt := len(cleaned) - hold
	f.pending = cleaned[emitAt:]
	return cleaned[:emitAt]
}

func (f *modalityTokenStreamFilter) Flush() string {
	if !f.enabled {
		return ""
	}
	result := stripModalityControlTokens(f.pending)
	f.pending = ""
	return result
}

// Models lists the served model IDs — doubles as the health check.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	if c.externalProtocol == "anthropic" {
		return c.anthropicModels(ctx)
	}
	requestAPIKey, credentialErr := c.resolveRequestCredential(ctx, c.Model)
	if credentialErr != nil {
		return nil, credentialErr
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if requestAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+requestAPIKey)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("model discovery returned HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}
