// Package telemetry exports actual execution observations through the signed
// Meadow account boundary. It never owns provider credentials or billing.
package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const Schema = "meadow.telemetry.v1"
const MaxEventBytes = 512 << 10
const MaxBatchBytes = 1 << 20

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}
type Event struct {
	ID           string         `json:"id"`
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id,omitempty"`
	RequestID    string         `json:"request_id,omitempty"`
	Kind         string         `json:"kind"`
	At           time.Time      `json:"at"`
	Model        string         `json:"model,omitempty"`
	Status       string         `json:"status,omitempty"`
	DurationMS   int64          `json:"duration_ms,omitempty"`
	Usage        *Usage         `json:"usage,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	Diagnostic   bool           `json:"diagnostic,omitempty"`
	Payload      map[string]any `json:"payload,omitempty"`
}
type Exporter interface {
	ExportTelemetry(context.Context, string, []byte) error
}
type Config struct {
	Directory string
	Exporter  Exporter
	// Principal returns a cached, verified account identity without network I/O.
	Principal     func() string
	Diagnostic    bool
	Capacity      int
	InboxCapacity int
	MaxSpoolBytes int
	FlushInterval time.Duration
	ExportTimeout time.Duration
	MaxBackoff    time.Duration
	// ClassifyFailure is a trusted, CPU-only classifier. Only allowlisted
	// constants reach Stats and the bounded state-change channel.
	ClassifyFailure func(error) string
}
type storedEvent struct {
	Principal string `json:"principal"`
	Event     Event  `json:"event"`
}
type Stats struct {
	Accepted             uint64
	Dropped              uint64
	Exported             uint64
	Failures             uint64
	Pending              int64
	LastFailureCode      string
	StatusChangesDropped uint64
}
type Queue struct {
	ownerMu              sync.Mutex
	runOwners            map[string]string
	emitMu               sync.RWMutex
	config               Config
	inbox                chan storedEvent
	stop                 chan struct{}
	done                 chan struct{}
	closed               atomic.Bool
	accepted             atomic.Uint64
	dropped              atomic.Uint64
	exported             atomic.Uint64
	failures             atomic.Uint64
	pending              atomic.Int64
	failure              atomic.Pointer[exportFailure]
	stateChanges         chan ExportStateChange
	statusChangesDropped atomic.Uint64
}
type exportResult struct {
	ids       []string
	err       error
	principal string
}

// New recovers a bounded private outbox. Emitters never touch disk or network.
func New(config Config) (*Queue, error) {
	if config.Exporter == nil || config.Principal == nil || strings.TrimSpace(config.Directory) == "" {
		return nil, errors.New("telemetry exporter, principal and private directory are required")
	}
	if config.Capacity <= 0 {
		config.Capacity = 1024
	}
	if config.Capacity > 4096 {
		return nil, errors.New("telemetry capacity exceeds bound")
	}
	if config.InboxCapacity <= 0 {
		config.InboxCapacity = 128
	}
	if config.InboxCapacity > 1024 {
		return nil, errors.New("telemetry inbox exceeds bound")
	}
	if config.MaxSpoolBytes <= 0 {
		config.MaxSpoolBytes = 8 << 20
	}
	if config.MaxSpoolBytes > 32<<20 {
		return nil, errors.New("telemetry spool exceeds bound")
	}
	if config.FlushInterval <= 0 {
		config.FlushInterval = time.Second
	}
	if config.ExportTimeout <= 0 {
		config.ExportTimeout = 5 * time.Second
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = time.Minute
	}
	if err := os.MkdirAll(config.Directory, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(config.Directory, 0700); err != nil {
		return nil, err
	}
	q := &Queue{runOwners: make(map[string]string), config: config, inbox: make(chan storedEvent, config.InboxCapacity), stop: make(chan struct{}), done: make(chan struct{}), stateChanges: make(chan ExportStateChange, 8)}
	go q.run()
	return q, nil
}
func ID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
func (q *Queue) Diagnostics() bool { return q != nil && q.config.Diagnostic }

// Emit is bounded and fail-open. It returns false for offline account identity,
// invalid/oversize events or a full queue; observations never delay a model call.
func (q *Queue) Emit(event Event) bool {
	if q == nil {
		return false
	}
	return q.EmitFor(q.Principal(), event)
}
func (q *Queue) Principal() string {
	if q == nil {
		return ""
	}
	return q.config.Principal()
}
func (q *Queue) RunPrincipal(runID string) string {
	if q == nil {
		return ""
	}
	q.ownerMu.Lock()
	defer q.ownerMu.Unlock()
	if principal, ok := q.runOwners[runID]; ok {
		return principal
	}
	return q.Principal()
}

// EmitFor preserves the verified session that owned the operation at admission.
func (q *Queue) EmitFor(principal string, event Event) bool {
	if q == nil || q.closed.Load() {
		return false
	}
	if principal == "" {
		q.dropped.Add(1)
		return false
	}
	event = Project(event, q.config.Diagnostic)
	if dropped := q.dropped.Load(); dropped > 0 {
		event.Metadata["dropped_events"] = dropped
	}
	body, err := json.Marshal(event)
	if err == nil && len(body) > MaxEventBytes && event.Payload != nil {
		event.Payload = map[string]any{"input": map[string]any{"omitted": "diagnostic_event_limit"}, "output": map[string]any{"omitted": "diagnostic_event_limit"}}
		event.Metadata["payload_truncated"] = true
		body, err = json.Marshal(event)
	}
	if err != nil || len(body) > MaxEventBytes || event.ID == "" || event.TraceID == "" || event.SpanID == "" {
		q.dropped.Add(1)
		return false
	}
	// Detach maps from callers before ownership passes to the worker.
	var immutable Event
	if json.Unmarshal(body, &immutable) != nil {
		q.dropped.Add(1)
		return false
	}
	q.emitMu.RLock()
	defer q.emitMu.RUnlock()
	if q.closed.Load() {
		return false
	}
	select {
	case q.inbox <- storedEvent{Principal: principal, Event: immutable}:
		q.accepted.Add(1)
		return true
	default:
		q.dropped.Add(1)
		return false
	}
}
func (q *Queue) Stats() Stats {
	if q == nil {
		return Stats{}
	}
	stats := Stats{Accepted: q.accepted.Load(), Dropped: q.dropped.Load(), Exported: q.exported.Load(), Failures: q.failures.Load(), Pending: q.pending.Load(), StatusChangesDropped: q.statusChangesDropped.Load()}
	if failure := q.failure.Load(); failure != nil && failure.principal == q.Principal() {
		stats.LastFailureCode = failure.code
	}
	return stats
}
func (q *Queue) Close(ctx context.Context) error {
	if q == nil {
		return nil
	}
	q.emitMu.Lock()
	if q.closed.CompareAndSwap(false, true) {
		close(q.stop)
	}
	q.emitMu.Unlock()
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (q *Queue) run() {
	defer close(q.done)
	defer close(q.stateChanges)
	records := map[string]storedEvent{}
	sizes := map[string]int{}
	total := 0
	persist := func(item storedEvent) {
		key := ID(item.Principal, item.Event.ID)
		if _, ok := records[key]; ok {
			return
		}
		data, err := json.Marshal(item)
		// Native auth subjects rotate on a fresh login. Old sessions never gain
		// a new identity, and their offline backlog must not starve a new one.
		if err == nil && item.Principal == q.Principal() {
			for len(records) >= q.config.Capacity || total+len(data) > q.config.MaxSpoolBytes {
				oldest := ""
				for id, existing := range records {
					if existing.Principal != item.Principal && (oldest == "" || existing.Event.At.Before(records[oldest].Event.At)) {
						oldest = id
					}
				}
				if oldest == "" {
					break
				}
				if removeErr := os.Remove(filepath.Join(q.config.Directory, oldest+".json")); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					q.failures.Add(1)
					break
				}
				total -= sizes[oldest]
				delete(sizes, oldest)
				delete(records, oldest)
				q.dropped.Add(1)
			}
		}
		if err != nil || len(records) >= q.config.Capacity || total+len(data) > q.config.MaxSpoolBytes {
			q.dropped.Add(1)
			return
		}
		tmp := filepath.Join(q.config.Directory, key+".tmp")
		dst := filepath.Join(q.config.Directory, key+".json")
		if err = os.WriteFile(tmp, data, 0600); err == nil {
			err = os.Rename(tmp, dst)
		}
		if err != nil {
			_ = os.Remove(tmp)
			q.failures.Add(1)
			q.dropped.Add(1)
			return
		}
		records[key] = item
		sizes[key] = len(data)
		total += len(data)
		q.pending.Store(int64(len(records)))
	}
	entries, _ := os.ReadDir(q.config.Directory)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		if len(key) != 64 {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() > MaxEventBytes+1024 {
			q.dropped.Add(1)
			continue
		}
		data, err := os.ReadFile(filepath.Join(q.config.Directory, entry.Name()))
		var item storedEvent
		if err != nil || json.Unmarshal(data, &item) != nil || key != ID(item.Principal, item.Event.ID) || len(records) >= q.config.Capacity || total+len(data) > q.config.MaxSpoolBytes {
			q.dropped.Add(1)
			continue
		}
		// Diagnostic consent is re-evaluated after restart, never inherited silently.
		item.Event = Project(item.Event, q.config.Diagnostic)
		records[key] = item
		sizes[key] = len(data)
		total += len(data)
	}
	q.pending.Store(int64(len(records)))
	timer := time.NewTicker(q.config.FlushInterval)
	defer timer.Stop()
	results := make(chan exportResult, 1)
	var active bool
	var cancel context.CancelFunc
	next := time.Time{}
	backoff := q.config.FlushInterval
	flush := func() {
		if active || time.Now().Before(next) || len(records) == 0 {
			return
		}
		principal := q.config.Principal()
		if principal == "" {
			return
		}
		keys := make([]string, 0, len(records))
		for key, item := range records {
			if item.Principal == principal {
				keys = append(keys, key)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return records[keys[i]].Event.At.Before(records[keys[j]].Event.At) })
		events := []Event{}
		ids := []string{}
		length := 0
		for _, key := range keys {
			data, _ := json.Marshal(records[key].Event)
			if len(events) == 32 || length+len(data) > MaxBatchBytes-128 {
				break
			}
			events = append(events, records[key].Event)
			ids = append(ids, key)
			length += len(data)
		}
		if len(events) == 0 {
			return
		}
		payload, _ := json.Marshal(struct {
			Schema string  `json:"schema"`
			Events []Event `json:"events"`
		}{Schema, events})
		ctx, stop := context.WithTimeout(context.Background(), q.config.ExportTimeout)
		cancel = stop
		active = true
		go func() {
			err := q.config.Exporter.ExportTelemetry(ctx, principal, payload)
			stop()
			results <- exportResult{ids: ids, err: err, principal: principal}
		}()
	}
	for {
		select {
		case item := <-q.inbox:
			persist(item)
		case result := <-results:
			active = false
			cancel = nil
			q.recordExportState(result.principal, result.err)
			if result.err != nil {
				q.failures.Add(1)
				next = time.Now().Add(backoff)
				backoff = min(backoff*2, q.config.MaxBackoff)
			} else {
				backoff = q.config.FlushInterval
				next = time.Time{}
				for _, key := range result.ids {
					if err := os.Remove(filepath.Join(q.config.Directory, key+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
						q.failures.Add(1)
						continue
					}
					total -= sizes[key]
					delete(sizes, key)
					delete(records, key)
					q.exported.Add(1)
				}
				q.pending.Store(int64(len(records)))
			}
		case <-timer.C:
			flush()
		case <-q.stop:
			if cancel != nil {
				cancel()
			}
			for {
				select {
				case item := <-q.inbox:
					persist(item)
				default:
					return
				}
			}
		}
	}
}
