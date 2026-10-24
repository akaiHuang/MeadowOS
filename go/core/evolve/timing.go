package evolve

import (
	"context"
	"errors"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// RuntimeUnavailable is admission failure, not task evidence for an improver.
type RuntimeUnavailable struct {
	Runtime string
	Cause   error
}

func (e *RuntimeUnavailable) Error() string {
	return "Evo " + e.Runtime + " runtime unavailable: " + e.Cause.Error()
}
func (e *RuntimeUnavailable) Unwrap() error { return e.Cause }

var ErrModelProgressTimeout = errors.New("Evo model generation progress timeout")

// requestTiming observes client-side boundaries. It cannot infer upstream queue
// or compute time from HTTP headers. Only generated content/tool bytes or rising
// token counters reset the progress deadline.
type requestTiming struct {
	mu                 sync.Mutex
	start, first, last time.Time
	metrics            map[string]any
	notify             func(map[string]any)
	wake               chan struct{}
}

func newRequestTiming(notify func(map[string]any)) *requestTiming {
	return &requestTiming{start: time.Now(), metrics: map[string]any{}, notify: notify, wake: make(chan struct{}, 1)}
}

func (t *requestTiming) event(kind string, extra map[string]any) {
	row := map[string]any{"kind": kind, "elapsed_ms": time.Since(t.start).Milliseconds()}
	for k, v := range extra {
		row[k] = v
	}
	if t.notify != nil {
		t.notify(row)
	}
}

func (t *requestTiming) boundary(key string) {
	t.mu.Lock()
	_, seen := t.metrics[key]
	if !seen {
		t.metrics[key] = time.Since(t.start).Milliseconds()
	}
	t.mu.Unlock()
	if !seen {
		t.event("model.transport", map[string]any{"boundary": key})
	}
}

func (t *requestTiming) progress(kind string) {
	now := time.Now()
	t.mu.Lock()
	first := t.first.IsZero()
	if first {
		t.first = now
		t.metrics["first_generation_signal_ms"] = now.Sub(t.start).Milliseconds()
		t.metrics["first_generation_signal_kind"] = kind
	}
	lastLogged, _ := t.metrics["last_progress_log_ms"].(int64)
	elapsed := now.Sub(t.start).Milliseconds()
	log := first || elapsed-lastLogged >= 1000
	if log {
		t.metrics["last_progress_log_ms"] = elapsed
	}
	t.last = now
	t.metrics["last_generation_signal_ms"] = elapsed
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
	if log {
		t.event("model.progress", map[string]any{"signal": kind, "first": first})
	}
}

func (t *requestTiming) wrap(h *provider.StreamHooks) *provider.StreamHooks {
	out := provider.StreamHooks{}
	if h != nil {
		out = *h
	}
	delta, draft, draftProgress, progress := out.OnDelta, out.OnToolDraft, out.OnToolDraftProgress, out.OnProgress
	privateProgress := out.OnPrivateReasoningProgress
	out.OnPrivateReasoningProgress = func() {
		t.progress("private_reasoning")
		if privateProgress != nil {
			privateProgress()
		}
	}
	var firstToolName sync.Once
	namedTool := func(name string) {
		if name != "" {
			firstToolName.Do(func() { t.progress("tool_name") })
		}
	}
	out.OnDelta = func(s string, thought bool) {
		if s != "" {
			kind := "answer"
			if thought {
				kind = "reasoning"
			}
			t.progress(kind)
		}
		if delta != nil {
			delta(s, thought)
		}
	}
	out.OnToolDraft = func(n string, size int) {
		namedTool(n)
		if size > 0 {
			t.progress("tool_arguments")
		}
		if draft != nil {
			draft(n, size)
		}
	}
	out.OnToolDraftProgress = func(p provider.ToolDraftProgress) {
		namedTool(p.Name)
		if p.ArgumentsDelta != "" {
			t.progress("tool_arguments")
		}
		if draftProgress != nil {
			draftProgress(p)
		}
	}
	var pm sync.Mutex
	var tokens, prefill int
	out.OnProgress = func(p provider.InferenceProgress) {
		pm.Lock()
		increased := p.CompletionTokens > tokens || p.PrefillProcessedTokens > prefill
		if p.CompletionTokens > tokens {
			tokens = p.CompletionTokens
		}
		if p.PrefillProcessedTokens > prefill {
			prefill = p.PrefillProcessedTokens
		}
		pm.Unlock()
		if increased {
			t.progress("token_counter")
		}
		if progress != nil {
			progress(p)
		}
	}
	return &out
}

func (t *requestTiming) context(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { t.boundary("http_connection_ready_ms") },
		WroteRequest: func(v httptrace.WroteRequestInfo) {
			if v.Err == nil {
				t.boundary("http_request_written_ms")
			}
		},
		GotFirstResponseByte: func() { t.boundary("http_first_response_byte_ms") },
	})
}

func (t *requestTiming) watch(ctx context.Context, cancel context.CancelCauseFunc, first, idle time.Duration) func() {
	if first <= 0 {
		first = 60 * time.Second
	}
	if idle <= 0 {
		idle = 60 * time.Second
	}
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		timer := time.NewTimer(first)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.wake:
			case <-timer.C:
			}
			t.mu.Lock()
			last, signal := t.last, !t.first.IsZero()
			t.mu.Unlock()
			deadline := t.start.Add(first)
			phase := "first_generation"
			if signal {
				deadline = last.Add(idle)
				phase = "stream_idle"
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				t.event("model.timeout", map[string]any{"phase": phase})
				cancel(ErrModelProgressTimeout)
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(remaining)
		}
	}()
	return func() { close(done); <-exited }
}

func (t *requestTiming) snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]any{"scope": "client HTTP and generated-signal boundaries; upstream queue/compute split unknown"}
	for k, v := range t.metrics {
		if k != "last_progress_log_ms" {
			out[k] = v
		}
	}
	return out
}
