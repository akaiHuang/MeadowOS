package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"github.com/hey-meadow/meadow-code/go/core/telemetry"
	"net/http"
	"time"
)

type completionTelemetry struct {
	queue     *telemetry.Queue
	event     telemetry.Event
	started   time.Time
	first     time.Time
	secret    string
	principal string
}

func telemetryRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "m" + hex.EncodeToString(value[:])
	}
	return telemetry.ID(time.Now().String())[:32]
}
func (c *Client) beginTelemetry(ctx context.Context, requestID, model string, body []byte, streaming bool) *completionTelemetry {
	if c.Telemetry == nil {
		return nil
	}
	runID := agenttrace.RunIDFromContext(ctx)
	principal := c.Telemetry.RunPrincipal(runID)
	parent := ""
	if runID == "" {
		runID = requestID
	} else {
		parent = telemetry.ID(runID, "run")[:16]
	}
	started := time.Now()
	event := telemetry.Event{ID: telemetry.ID(requestID, "start"), TraceID: telemetry.ID(runID)[:32], SpanID: telemetry.ID(requestID)[:16], ParentSpanID: parent, RequestID: requestID, Kind: "provider.requested", At: started.UTC(), Model: model, Metadata: map[string]any{"source": "provider", "streaming": streaming}}
	if c.Telemetry.Diagnostics() {
		event.Diagnostic = true
		event.Payload = map[string]any{"input": telemetry.DiagnosticPayload(body, c.APIKey)}
	}
	event = telemetry.Project(event, c.Telemetry.Diagnostics())
	c.Telemetry.EmitFor(principal, event)
	return &completionTelemetry{queue: c.Telemetry, event: event, started: started, secret: c.APIKey, principal: principal}
}
func (t *completionTelemetry) wrap(hooks *StreamHooks) *StreamHooks {
	if t == nil {
		return hooks
	}
	result := StreamHooks{}
	if hooks != nil {
		result = *hooks
	}
	next := result.OnDelta
	result.OnDelta = func(delta string, thought bool) {
		if delta != "" && t.first.IsZero() {
			t.first = time.Now()
		}
		if next != nil {
			next(delta, thought)
		}
	}
	return &result
}
func (t *completionTelemetry) finish(result *StreamResult, err error, status int) {
	if t == nil {
		return
	}
	event := t.event
	event.ID = telemetry.ID(event.RequestID, "finish")
	event.Kind = "provider.finished"
	event.At = time.Now().UTC()
	event.DurationMS = time.Since(t.started).Milliseconds()
	event.Status = "success"
	event.Metadata = map[string]any{"source": "provider", "http_status": status}
	if t.event.Payload != nil {
		event.Payload = map[string]any{}
		for key, value := range t.event.Payload {
			event.Payload[key] = value
		}
	}
	if !t.first.IsZero() {
		event.Metadata["ttft_ms"] = t.first.Sub(t.started).Milliseconds()
	}
	if err != nil {
		event.Kind = "provider.failed"
		event.Status = "error"
		code := "completion"
		switch {
		case errors.Is(err, context.Canceled):
			code = "canceled"
			event.Status = "canceled"
		case errors.Is(err, context.DeadlineExceeded):
			code = "deadline_exceeded"
		case errors.Is(err, ErrGenerationStall):
			code = "generation_stall"
		case status == 0:
			code = "transport"
		case status >= 400:
			code = "http_status"
		}
		event.Metadata["error_code"] = code
	}
	if result != nil {
		event.Metadata["finish_reason"] = result.FinishReason
		event.Metadata["response_bytes"] = len(result.Content)
		if result.Usage != nil {
			event.Usage = &telemetry.Usage{InputTokens: result.Usage.PromptTokens, OutputTokens: result.Usage.CompletionTokens, TotalTokens: result.Usage.TotalTokens}
		}
		if t.queue.Diagnostics() {
			output, _ := json.Marshal(map[string]any{"content": result.Content, "tool_calls": result.ToolCalls})
			event.Diagnostic = true
			if event.Payload == nil {
				event.Payload = map[string]any{}
			}
			event.Payload["output"] = telemetry.DiagnosticPayload(output, t.secret)
		}
	}
	t.queue.EmitFor(t.principal, event)
}

func (t *completionTelemetry) annotate(request *http.Request) {
	if t == nil {
		return
	}
	request.Header.Set("X-Meadow-Trace-ID", t.event.TraceID)
	request.Header.Set("X-Meadow-Span-ID", t.event.SpanID)
	request.Header.Set("traceparent", "00-"+t.event.TraceID+"-"+t.event.SpanID+"-01")
}
