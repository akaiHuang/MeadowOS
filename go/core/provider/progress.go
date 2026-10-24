package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	activeAgentHeartbeatInterval  = 30 * time.Second
	inferenceByteProgressInterval = time.Second
)

// InferenceProgress is observable runtime state from Meadow Core. Phases are
// transport/runtime facts, not model-generated reasoning.
type InferenceProgress struct {
	RequestID              string
	Model                  string
	Phase                  string
	Elapsed                time.Duration
	PromptTokens           int
	CachedTokens           int
	RemainingTokens        int
	PrefillProcessedTokens int
	PrefillTotalTokens     int
	CompletionTokens       int
	// BytesReceived counts observed transport bytes, including stream framing.
	// It is not a token count, visible output, or evidence of semantic progress.
	BytesReceived           int64
	CacheHitType            string
	TTFT                    time.Duration
	Activation              time.Duration
	IncludesActivation      bool
	FirstTokenVisible       bool
	PromptEstimatedTokens   int
	ReadingSource           string
	ReadingExcerpt          string
	PrefillObservedTokens   int
	PrefillObservedElapsed  time.Duration
	IngestionSourceID       string
	IngestionSourceName     string
	IngestionProcessed      int
	IngestionTotal          int
	IngestionProcessedBytes int
	IngestionTotalBytes     int
	IngestionStartLine      int
	IngestionEndLine        int
	IngestionNote           string
	IngestionReused         bool
}

const (
	InferenceConnecting     = "connecting"
	InferenceWaiting        = "waiting"
	InferenceStreaming      = "streaming"
	InferenceModelLoading   = "model_loading"
	InferenceModelReady     = "model_ready"
	InferenceQueued         = "queued"
	InferenceCacheLookup    = "cache_lookup"
	InferencePrefill        = "prefill"
	InferenceFirstToken     = "first_token"
	InferenceDecoding       = "decoding"
	InferenceCompleted      = "completed"
	InferenceFailed         = "failed"
	InferenceDocumentIngest = "document_ingest"
	InferenceDocumentReady  = "document_ready"
	// Agent phases are emitted by H_G after model inference completes. They are
	// observable execution states, not inferred chain-of-thought stages.
	InferenceAgentAudit      = "agent_requirement_audit"
	InferenceAgentCheckpoint = "agent_checkpoint"
	InferenceAgentRecovery   = "agent_recovery"
	InferenceAgentRollback   = "agent_rollback"
	InferenceAgentVerifying  = "agent_verifying"
	// Validation outcome phases are emitted only after an executed selected
	// validator has returned a typed result. They let product surfaces explain
	// why the Agent is repairing or auditing, and let evaluation inject a real
	// user steer at a phase boundary instead of guessing a model turn number.
	InferenceAgentValidationFailed = "agent_validation_failed"
	InferenceAgentValidationPassed = "agent_validation_passed"
)

var progressRequestSequence atomic.Uint64

type coreRuntimeStatus struct {
	RuntimeBackend string `json:"runtime_backend"`
	ModelManager   struct {
		Models []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"models"`
	} `json:"model_manager"`
	Engines []struct {
		Model    string `json:"model"`
		Requests []struct {
			RequestID              string   `json:"request_id"`
			Phase                  string   `json:"phase"`
			ElapsedSeconds         *float64 `json:"elapsed_s"`
			UpstreamElapsedSeconds *float64 `json:"elapsed_seconds"`
			BytesReceived          int64    `json:"bytes_received"`
			PromptTokens           int      `json:"prompt_tokens"`
			CompletionTokens       int      `json:"completion_tokens"`
			TTFTSeconds            *float64 `json:"ttft_s"`
			ActivationSeconds      float64  `json:"activation_s"`
			CacheHitType           string   `json:"cache_hit_type"`
			CachedTokens           int      `json:"cached_tokens"`
			PrefillProcessedTokens int      `json:"prefill_processed_tokens"`
			PrefillTotalTokens     int      `json:"prefill_total_tokens"`
		} `json:"requests"`
	} `json:"engines"`
}

type mtplxFlightSnapshot struct {
	Enabled bool `json:"enabled"`
	Active  []struct {
		RequestID       string  `json:"rid"`
		Model           string  `json:"model"`
		Phase           string  `json:"phase"`
		ElapsedSeconds  float64 `json:"elapsed_s"`
		PromptTokens    int     `json:"prompt_tokens"`
		GeneratedTokens int     `json:"gen_tokens"`
		Prefill         *struct {
			TokensDone      int     `json:"tokens_done"`
			TokensTotal     int     `json:"tokens_total"`
			CachedTokens    int     `json:"cached_tokens"`
			ElapsedSeconds  float64 `json:"elapsed_s"`
			PrefillTokensPS float64 `json:"prefill_tok_s"`
		} `json:"prefill"`
	} `json:"active"`
}

func (c *Client) observeInference(
	ctx context.Context,
	model string,
	messages []Message,
	hooks *StreamHooks,
	onRuntimeProgress func(InferenceProgress),
) (string, *StreamHooks, func()) {
	hasProgressHook := hooks != nil && hooks.OnProgress != nil
	if !c.usesCoreTransport() {
		return "", hooks, func() {}
	}

	requestID := fmt.Sprintf(
		"g%08x%03x",
		uint32(time.Now().UnixMilli()),
		progressRequestSequence.Add(1)&0xfff,
	)
	// Request identity is a transport requirement, not a progress feature.
	// Keep it when hooks are absent or Core status is temporarily unavailable,
	// so cancellation can always target the completion that was dispatched.
	if !hasProgressHook && onRuntimeProgress == nil {
		return requestID, hooks, func() {}
	}
	started := time.Now()
	reading := newInferenceReadingTrace(messages)
	observed := StreamHooks{}
	if hooks != nil {
		observed = *hooks
	}
	emitProgress := func(progress InferenceProgress) {
		progress.PromptEstimatedTokens = reading.totalTokens
		progress.ReadingSource, progress.ReadingExcerpt = reading.excerpt(progress)
		if onRuntimeProgress != nil {
			onRuntimeProgress(progress)
		}
		if hasProgressHook {
			hooks.OnProgress(progress)
		}
	}
	var firstToken sync.Once
	var firstVisibleTTFT atomic.Int64
	var residentStarted atomic.Int64
	streamingTransport := true
	if profile, ok := ProfileForModel(model); ok {
		streamingTransport = profile.Streaming
	}
	emitFirstToken := func(
		ttft time.Duration,
		includesActivation bool,
		visible bool,
	) {
		firstToken.Do(func() {
			if visible {
				firstVisibleTTFT.Store(int64(ttft))
			}
			emitProgress(InferenceProgress{
				RequestID:          requestID,
				Model:              model,
				Phase:              InferenceFirstToken,
				Elapsed:            ttft,
				TTFT:               ttft,
				IncludesActivation: includesActivation,
				FirstTokenVisible:  visible,
			})
		})
	}
	originalDelta := observed.OnDelta
	observed.OnDelta = func(delta string, thought bool) {
		if delta != "" && !thought {
			firstTokenStarted := residentStarted.Load()
			firstTokenElapsed := time.Since(started)
			includesActivation := firstTokenStarted == 0
			if firstTokenStarted != 0 {
				firstTokenElapsed = time.Since(
					time.Unix(0, firstTokenStarted),
				)
			}
			emitFirstToken(firstTokenElapsed, includesActivation, true)
		}
		if originalDelta != nil {
			originalDelta(delta, thought)
		}
	}

	// Even an explicitly configured Core must supply its runtime contract
	// before its status can be exposed as observable progress. Failure to read
	// status must not discard the request's cancellation identity.
	status, err := c.fetchCoreRuntimeStatus(ctx)
	if err != nil {
		return requestID, hooks, func() {}
	}
	emitProgress(InferenceProgress{
		RequestID: requestID,
		Model:     model,
		Phase:     InferenceConnecting,
	})
	if initial, ok := status.progress(
		requestID,
		model,
		time.Since(started),
	); ok {
		markResidentStart(&residentStarted, initial)
		emitProgress(initial)
	}

	watchContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watchInferenceProgress(
			watchContext,
			requestID,
			model,
			started,
			&residentStarted,
			emitProgress,
			emitFirstToken,
			&firstVisibleTTFT,
			streamingTransport,
		)
	}()
	return requestID, &observed, func() {
		cancel()
		<-done
	}
}

type inferenceReadingSpan struct {
	start   int
	end     int
	source  string
	text    []rune
	visible bool
}

type inferenceReadingTrace struct {
	totalTokens int
	spans       []inferenceReadingSpan
}

func newInferenceReadingTrace(messages []Message) inferenceReadingTrace {
	trace := inferenceReadingTrace{}
	for _, message := range messages {
		rawText := MessageText(message)
		tokens := EstimateTextTokens(rawText)
		if tokens <= 0 {
			continue
		}
		span := inferenceReadingSpan{
			start: trace.totalTokens,
			end:   trace.totalTokens + tokens,
		}
		switch message.Role {
		case "user":
			span.source, span.visible = "使用者資料", true
		case "tool":
			span.source, span.visible = "工具讀取資料", true
		case "system":
			span.source = "固定規則"
		case "assistant":
			span.source = "先前回覆"
		default:
			span.source = "對話背景"
		}
		if span.visible {
			span.text = []rune(normalizeReadingText(rawText))
		}
		trace.spans = append(trace.spans, span)
		trace.totalTokens = span.end
	}
	return trace
}

func (trace inferenceReadingTrace) excerpt(
	progress InferenceProgress,
) (string, string) {
	if progress.Phase != InferencePrefill || trace.totalTokens <= 0 {
		return "", ""
	}
	exactTotal := progress.PromptTokens
	if exactTotal <= 0 {
		exactTotal = progress.CachedTokens + progress.PrefillTotalTokens
	}
	absoluteProcessed := progress.CachedTokens + progress.PrefillProcessedTokens
	if exactTotal <= 0 || absoluteProcessed <= 0 {
		return "", ""
	}
	estimatedPosition := int(math.Ceil(
		float64(min(absoluteProcessed, exactTotal)) /
			float64(exactTotal) * float64(trace.totalTokens),
	))
	estimatedPosition = min(max(estimatedPosition, 1), trace.totalTokens)
	for _, span := range trace.spans {
		if estimatedPosition <= span.start || estimatedPosition > span.end {
			continue
		}
		if !span.visible || len(span.text) == 0 {
			return span.source, ""
		}
		positionInSpan := estimatedPosition - span.start
		ratio := math.Min(
			float64(positionInSpan)/float64(max(span.end-span.start, 1)),
			1,
		)
		end := min(max(int(math.Ceil(ratio*float64(len(span.text)))), 1), len(span.text))
		start := max(end-120, 0)
		return span.source, string(span.text[start:end])
	}
	return "對話背景", ""
}

// EstimateTextTokens provides an immediate, explicitly approximate local count
// before the model tokenizer reports exact request tokens. CJK/non-ASCII runes
// are close to one token for the supported local models; ASCII averages four
// bytes per token. Product UI replaces this estimate with Core facts promptly.
func EstimateTextTokens(text string) int {
	asciiBytes := 0
	nonASCII := 0
	for _, r := range text {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if r <= unicode.MaxASCII {
				asciiBytes++
			}
			continue
		}
		if r <= unicode.MaxASCII {
			asciiBytes++
		} else {
			nonASCII++
		}
	}
	return int(math.Ceil(float64(asciiBytes)/4.0)) + nonASCII
}

func normalizeReadingText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	spacePending := false
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		text = text[size:]
		if r == utf8.RuneError && size == 1 {
			continue
		}
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			spacePending = b.Len() > 0
			continue
		}
		if spacePending {
			b.WriteByte(' ')
			spacePending = false
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func (c *Client) usesCoreTransport() bool {
	return c != nil && c.coreTransport
}

// MaintainActivityLease keeps an already-loaded local Meadow Core model
// resident while an agent run is paused outside inference, for example while
// waiting for a permission decision, a user answer, or expert guidance.  The
// Core heartbeat never loads or switches a model and is ignored by non-Core
// OpenAI-compatible servers.
func (c *Client) MaintainActivityLease(ctx context.Context, model string) {
	if c == nil || !c.usesCoreTransport() {
		return
	}
	heartbeat := func() {
		body, err := json.Marshal(map[string]string{"model": model})
		if err != nil {
			return
		}
		requestContext, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(
			requestContext,
			http.MethodPost,
			strings.TrimRight(c.BaseURL, "/")+"/activity/heartbeat",
			bytes.NewReader(body),
		)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Content-Type", "application/json")
		client := c.HTTP
		if client == nil {
			client = http.DefaultClient
		}
		response, err := client.Do(req)
		if err != nil {
			return
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	}
	heartbeat()
	ticker := time.NewTicker(activeAgentHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			heartbeat()
		}
	}
}

// inferenceProgressDeduper preserves immediate phase/token observations while
// bounding byte-only transport updates. Time passing alone is never progress.
// Each watcher owns this state; OnDelta callbacks do not access it.
type inferenceProgressDeduper struct {
	lastKey   string
	lastBytes int64
	lastEmit  time.Time
}

func (d *inferenceProgressDeduper) allow(progress InferenceProgress, now time.Time) bool {
	key := fmt.Sprintf(
		"%s:%d:%d:%d:%d:%s:%d:%t",
		progress.Phase,
		progress.PromptTokens,
		progress.CachedTokens,
		progress.PrefillProcessedTokens,
		progress.CompletionTokens,
		progress.CacheHitType,
		progress.TTFT,
		progress.FirstTokenVisible,
	)
	if key == d.lastKey && (progress.BytesReceived == d.lastBytes || now.Sub(d.lastEmit) < inferenceByteProgressInterval) {
		return false
	}
	d.lastKey, d.lastBytes, d.lastEmit = key, progress.BytesReceived, now
	return true
}

func (c *Client) watchInferenceProgress(
	ctx context.Context,
	requestID string,
	model string,
	started time.Time,
	residentStarted *atomic.Int64,
	emit func(InferenceProgress),
	emitFirstToken func(time.Duration, bool, bool),
	firstVisibleTTFT *atomic.Int64,
	streamingTransport bool,
) {
	ticker := time.NewTicker(125 * time.Millisecond)
	defer ticker.Stop()
	deduper := inferenceProgressDeduper{}
	cacheReported := false
	prefillObservation := prefillRateObservation{}
	for {
		status, err := c.fetchCoreRuntimeStatus(ctx)
		var progress InferenceProgress
		var ok bool
		if err == nil {
			progress, ok = status.progress(requestID, model, time.Since(started))
			if strings.EqualFold(
				strings.TrimSpace(status.RuntimeBackend),
				"mtplx",
			) {
				// Core's generic status still owns model loading, while MTPLX's
				// flight recorder owns exact prefill/decode counters. Prefer the
				// latter when active so a buffered tool call is not shown as idle.
				if flight, flightOK, _ := c.fetchMTPLXFlightProgress(
					ctx,
					requestID,
					model,
					time.Since(started),
				); flightOK {
					progress, ok = flight, true
				}
			}
		} else {
			progress, ok, _ = c.fetchMTPLXFlightProgress(
				ctx,
				requestID,
				model,
				time.Since(started),
			)
		}
		if ok {
			markResidentStart(residentStarted, progress)
			prefillObservation.annotate(&progress, time.Now())
			if progress.CacheHitType != "" && !cacheReported {
				cache := progress
				cache.Phase = InferenceCacheLookup
				emit(cache)
				cacheReported = true
			}
			if progress.Phase == InferenceDecoding && progress.TTFT > 0 {
				if streamingTransport {
					// Scheduler TTFT can point at a hidden channel marker.
					// Streaming transports use the filtered OnDelta clock
					// once available. Before that, retain the runtime TTFT
					// as an explicitly non-visible model metric so a long
					// structured tool call is not presented as silence.
					visibleTTFT := time.Duration(firstVisibleTTFT.Load())
					if visibleTTFT > 0 {
						progress.TTFT = visibleTTFT
						progress.FirstTokenVisible = true
					} else {
						progress.FirstTokenVisible = false
					}
				} else {
					// Non-streaming transports cannot expose a visible-token
					// clock before completion, so label this model metric
					// explicitly rather than presenting it as visible TTFT.
					emitFirstToken(progress.TTFT, false, false)
				}
			}
			if deduper.allow(progress, time.Now()) {
				emit(progress)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Client) fetchMTPLXFlightProgress(
	ctx context.Context,
	requestID string,
	model string,
	elapsed time.Duration,
) (InferenceProgress, bool, error) {
	var snapshot mtplxFlightSnapshot
	requestContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		requestContext,
		http.MethodGet,
		strings.TrimRight(c.BaseURL, "/")+"/mtplx/flight",
		nil,
	)
	if err != nil {
		return InferenceProgress{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	response, err := c.HTTP.Do(req)
	if err != nil {
		return InferenceProgress{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return InferenceProgress{}, false, fmt.Errorf(
			"MTPLX flight endpoint returned %s",
			response.Status,
		)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&snapshot); err != nil {
		return InferenceProgress{}, false, err
	}
	if !snapshot.Enabled || len(snapshot.Active) == 0 {
		return InferenceProgress{}, false, nil
	}

	selected := -1
	for index, active := range snapshot.Active {
		if active.RequestID == requestID {
			selected = index
			break
		}
		if selected < 0 && strings.EqualFold(strings.TrimSpace(active.Model), strings.TrimSpace(model)) {
			selected = index
		}
	}
	if selected < 0 && len(snapshot.Active) == 1 {
		selected = 0
	}
	if selected < 0 {
		return InferenceProgress{}, false, nil
	}

	active := snapshot.Active[selected]
	progress := InferenceProgress{
		RequestID:        requestID,
		Model:            model,
		Elapsed:          elapsed,
		PromptTokens:     active.PromptTokens,
		RemainingTokens:  active.PromptTokens,
		CompletionTokens: active.GeneratedTokens,
	}
	if active.ElapsedSeconds > 0 {
		progress.Elapsed = time.Duration(active.ElapsedSeconds * float64(time.Second))
	}
	switch strings.ToLower(strings.TrimSpace(active.Phase)) {
	case "decode", "decoding", "generation":
		progress.Phase = InferenceDecoding
	case "queued":
		progress.Phase = InferenceQueued
	default:
		progress.Phase = InferencePrefill
	}
	if active.Prefill != nil {
		progress.CachedTokens = max(active.Prefill.CachedTokens, 0)
		progress.RemainingTokens = max(progress.PromptTokens-progress.CachedTokens, 0)
		progress.PrefillTotalTokens = progress.RemainingTokens
		processed := max(active.Prefill.TokensDone, 0)
		if processed > progress.RemainingTokens && progress.CachedTokens > 0 {
			processed = max(processed-progress.CachedTokens, 0)
		}
		progress.PrefillProcessedTokens = min(processed, progress.PrefillTotalTokens)
		if progress.CachedTokens > 0 {
			progress.CacheHitType = "hit"
		} else {
			progress.CacheHitType = "miss"
		}
		if active.Prefill.ElapsedSeconds > 0 {
			progress.TTFT = time.Duration(
				active.Prefill.ElapsedSeconds * float64(time.Second),
			)
		}
	} else if progress.Phase == InferencePrefill {
		progress.PrefillTotalTokens = max(progress.PromptTokens, 0)
	}
	return progress, true, nil
}

type prefillRateObservation struct {
	startedAt      time.Time
	processedStart int
}

func (observation *prefillRateObservation) annotate(
	progress *InferenceProgress,
	now time.Time,
) {
	if progress == nil || progress.Phase != InferencePrefill ||
		progress.PrefillProcessedTokens <= 0 {
		return
	}
	if observation.startedAt.IsZero() ||
		progress.PrefillProcessedTokens < observation.processedStart {
		observation.startedAt = now
		observation.processedStart = progress.PrefillProcessedTokens
		return
	}
	progress.PrefillObservedTokens =
		progress.PrefillProcessedTokens - observation.processedStart
	progress.PrefillObservedElapsed = now.Sub(observation.startedAt)
}

func markResidentStart(started *atomic.Int64, progress InferenceProgress) {
	if progress.Phase == InferenceConnecting || progress.Phase == InferenceModelLoading {
		return
	}
	residentAt := time.Now()
	switch progress.Phase {
	case InferenceQueued, InferencePrefill, InferenceDecoding:
		if progress.Elapsed > 0 {
			// The scheduler request clock starts only after model activation.
			// Reconstruct that point without subtracting activation again.
			residentAt = residentAt.Add(-progress.Elapsed)
		}
	}
	started.CompareAndSwap(0, residentAt.UnixNano())
}

func (c *Client) fetchCoreRuntimeStatus(ctx context.Context) (coreRuntimeStatus, error) {
	var status coreRuntimeStatus
	requestContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		requestContext,
		http.MethodGet,
		strings.TrimRight(c.BaseURL, "/")+"/status",
		nil,
	)
	if err != nil {
		return status, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	response, err := c.HTTP.Do(req)
	if err != nil {
		return status, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return status, fmt.Errorf("Core status returned %s", response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err != nil {
		return status, err
	}
	backend := strings.ToLower(strings.TrimSpace(status.RuntimeBackend))
	hasKnownBackend := backend == "sglang-mlx" || backend == "vllm-mlx"
	if !hasKnownBackend && len(status.ModelManager.Models) == 0 {
		return status, fmt.Errorf("status endpoint is not Meadow Core")
	}
	return status, nil
}

func (status coreRuntimeStatus) progress(
	requestID string,
	model string,
	elapsed time.Duration,
) (InferenceProgress, bool) {
	base := InferenceProgress{
		RequestID: requestID,
		Model:     model,
		Elapsed:   elapsed,
	}
	modelState := ""
	for _, candidate := range status.ModelManager.Models {
		if candidate.ID == model {
			modelState = candidate.Status
			break
		}
	}
	if modelState == "loading" {
		base.Phase = InferenceModelLoading
		return base, true
	}
	if modelState == "unloaded" || modelState == "unloading" {
		// The watcher starts immediately before this request is submitted.
		// A selected model that Core confirms is not resident must be loaded
		// before the request can proceed, even while MLX's synchronous load
		// temporarily blocks the status endpoint.
		base.Phase = InferenceModelLoading
		return base, true
	}
	for _, engine := range status.Engines {
		if engine.Model != model {
			continue
		}
		for _, request := range engine.Requests {
			if request.RequestID != requestID {
				continue
			}
			elapsedSeconds := 0.0
			if request.ElapsedSeconds != nil {
				elapsedSeconds = *request.ElapsedSeconds
			} else if request.UpstreamElapsedSeconds != nil {
				elapsedSeconds = *request.UpstreamElapsedSeconds
			}
			base.Elapsed = time.Duration(elapsedSeconds * float64(time.Second))
			base.BytesReceived = max(request.BytesReceived, 0)
			base.PromptTokens = request.PromptTokens
			base.CachedTokens = request.CachedTokens
			base.RemainingTokens = max(request.PromptTokens-request.CachedTokens, 0)
			base.PrefillProcessedTokens = request.PrefillProcessedTokens
			base.PrefillTotalTokens = request.PrefillTotalTokens
			base.CompletionTokens = request.CompletionTokens
			base.CacheHitType = request.CacheHitType
			base.Activation = time.Duration(
				request.ActivationSeconds * float64(time.Second),
			)
			if request.TTFTSeconds != nil {
				base.TTFT = time.Duration(
					*request.TTFTSeconds * float64(time.Second),
				)
			}
			switch request.Phase {
			case "queued":
				base.Phase = InferenceQueued
			case "prefill":
				base.Phase = InferencePrefill
			case "generation":
				base.Phase = InferenceDecoding
			default:
				base.Phase = request.Phase
			}
			return base, true
		}
		base.Phase = InferenceModelReady
		return base, true
	}
	if modelState == "loaded" {
		base.Phase = InferenceModelReady
		return base, true
	}
	return InferenceProgress{}, false
}
