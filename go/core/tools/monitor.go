package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	MonitorToolName             = "monitor"
	monitorLineLimit            = 500
	monitorBatchLimit           = 3000
	monitorReadLimit            = 64 << 10
	monitorPollInterval         = 200 * time.Millisecond
	monitorRateCapacity         = 10
	monitorRateRefill           = 2 * time.Second
	monitorOverloadKillDuration = 30 * time.Second
	monitorDefaultTimeout       = 10 * time.Hour
	monitorMaxTimeout           = 10 * time.Hour
)

type MonitorSnapshot struct {
	ProcessID   string `json:"process_id"`
	Description string `json:"description"`
	Status      string `json:"status"`
	StartedAt   string `json:"started_at"`
	EndedAt     string `json:"ended_at,omitempty"`
	Suppressed  uint64 `json:"suppressed_events,omitempty"`
}

type monitorRun struct {
	processID   string
	description string
	startedAt   time.Time
	cancel      context.CancelFunc

	mu         sync.Mutex
	status     string
	endedAt    time.Time
	suppressed uint64
}

func (m *monitorRun) snapshot() MonitorSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := MonitorSnapshot{
		ProcessID: m.processID, Description: m.description, Status: m.status,
		StartedAt:  m.startedAt.UTC().Format(time.RFC3339Nano),
		Suppressed: m.suppressed,
	}
	if !m.endedAt.IsZero() {
		result.EndedAt = m.endedAt.UTC().Format(time.RFC3339Nano)
	}
	return result
}

// MonitorTool turns selected stdout lines from one owned background process
// into bounded, claimable observations. The model chooses the command and what
// to do with events; the tool owns only execution, rate limits, and lifecycle.
type MonitorTool struct {
	Manager *ProcessManager

	mu            sync.Mutex
	runs          map[string]*monitorRun
	notifications []Notification
	events        chan Notification
	closed        bool
}

func NewMonitorTool(manager *ProcessManager) *MonitorTool {
	return &MonitorTool{
		Manager: manager, runs: map[string]*monitorRun{},
		events: make(chan Notification, 256),
	}
}

func RegisterMonitorTool(registry *Registry) *MonitorTool {
	if registry == nil {
		return nil
	}
	if existing, ok := registry.Get(MonitorToolName); ok {
		if monitor, ok := existing.(*MonitorTool); ok {
			return monitor
		}
		return nil
	}
	manager, ok := FindProcessManager(registry)
	if !ok {
		return nil
	}
	monitor := NewMonitorTool(manager)
	registry.Register(monitor)
	return monitor
}

func (t *MonitorTool) Name() string { return MonitorToolName }
func (t *MonitorTool) Description() string {
	return "Start a background monitor whose selected stdout lines return as live observable events while you keep working. Use a selective line-buffered command; noisy output is rate-limited and sustained overload is stopped."
}
func (t *MonitorTool) Kind() Kind { return KindExecute }
func (t *MonitorTool) CompanionTools() []string {
	return []string{"process"}
}
func (t *MonitorTool) Schema() map[string]any {
	return obj(map[string]any{
		"command":     str("Long-running shell command or script. Each newline-delimited stdout record is an event; filter aggressively."),
		"description": str("Short human-readable description included with every event."),
		"timeout":     num("Deadline in milliseconds (default and max 36000000 = 10 hours)."),
		"persistent":  boolean("Run until process(action=stop) or session end; ignores timeout."),
	}, "command", "description")
}

func (t *MonitorTool) Execute(_ context.Context, args map[string]any) Result {
	if t == nil || t.Manager == nil {
		return errResultf("monitor process manager is not configured")
	}
	command := strings.TrimSpace(strArg(args, "command"))
	description := strings.TrimSpace(strArg(args, "description"))
	if command == "" || description == "" {
		return errResultf("command and description are required")
	}
	if hasUntrackedBackground(command) {
		return errResultf("monitor owns background execution; remove &, nohup, disown, or setsid")
	}
	timeout := monitorDefaultTimeout
	if ms, ok := intArg(args, "timeout"); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
		if timeout > monitorMaxTimeout && !boolArg(args, "persistent") {
			return errResultf("timeout exceeds maximum %s; use persistent=true for a session-length monitor", monitorMaxTimeout)
		}
	}
	if boolArg(args, "persistent") {
		timeout = 0
	}

	env, _, err := processEnvironment(t.Manager)
	if err != nil {
		return errResult(err)
	}
	snapshot, err := t.Manager.Start(command, t.Manager.ws.Root, env, false, false, false)
	if err != nil {
		return errResult(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	run := &monitorRun{
		processID: snapshot.ProcessID, description: description,
		startedAt: time.Now(), cancel: cancel, status: "running",
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), processStopGrace)
		defer stopCancel()
		_, _ = t.Manager.Stop(stopCtx, snapshot.ProcessID)
		return errResultf("monitor is closed")
	}
	t.runs[run.processID] = run
	t.mu.Unlock()

	go t.pump(runCtx, run, timeout)
	payload, _ := json.Marshal(map[string]any{
		"task_id": snapshot.ProcessID, "process_id": snapshot.ProcessID,
		"description": description, "persistent": timeout == 0,
		"timeout_ms": timeout.Milliseconds(),
	})
	return Result{
		LLMContent:    "Monitor started. Control is returning to the user now; do not poll or sleep. Live events and completion will wake the session as fresh observable state.\n" + string(payload),
		Display:       fmt.Sprintf("monitor %s started as %s", description, snapshot.ProcessID),
		ReturnControl: true,
	}
}

func (t *MonitorTool) List() []MonitorSnapshot {
	t.mu.Lock()
	runs := make([]*monitorRun, 0, len(t.runs))
	for _, run := range t.runs {
		runs = append(runs, run)
	}
	t.mu.Unlock()
	result := make([]MonitorSnapshot, 0, len(runs))
	for _, run := range runs {
		result = append(result, run.snapshot())
	}
	return result
}

func (t *MonitorTool) pump(ctx context.Context, run *monitorRun, timeout time.Duration) {
	ticker := time.NewTicker(monitorPollInterval)
	defer ticker.Stop()
	var deadline <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		deadline = timer.C
		defer timer.Stop()
	}
	limiter := newMonitorLimiter(time.Now())
	offset := int64(0)
	partial := ""

	for {
		select {
		case <-ctx.Done():
			t.finishMonitor(run, "cancelled", nil)
			return
		case <-deadline:
			stopCtx, cancel := context.WithTimeout(context.Background(), 2*processStopGrace)
			_, _ = t.Manager.Stop(stopCtx, run.processID)
			cancel()
			t.finishMonitor(run, "timed_out", map[string]any{"timeout_ms": timeout.Milliseconds()})
			return
		case now := <-ticker.C:
			logs, err := t.Manager.Logs(run.processID, offset, monitorReadLimit)
			if err == nil {
				offset = logs.Next
				partial = t.emitMonitorOutput(run, partial, logs.Output, false, limiter, now)
			}
			snapshot, pollErr := t.Manager.Poll(run.processID)
			if pollErr != nil {
				t.finishMonitor(run, "failed", map[string]any{"error": pollErr.Error()})
				return
			}
			if limiter.shouldKill(now) {
				stopCtx, cancel := context.WithTimeout(context.Background(), 2*processStopGrace)
				_, _ = t.Manager.Stop(stopCtx, run.processID)
				cancel()
				t.finishMonitor(run, "overloaded", map[string]any{
					"suppressed_events": limiter.suppressed,
					"guidance":          "Restart with a more selective line-buffered filter.",
				})
				return
			}
			if snapshot.Status == string(processExited) {
				partial = t.emitMonitorOutput(run, partial, "", true, limiter, now)
				_ = partial
				status := "completed"
				if snapshot.ExitCode == nil || *snapshot.ExitCode != 0 {
					status = "failed"
				}
				t.finishMonitor(run, status, snapshot)
				return
			}
		}
	}
}

func (t *MonitorTool) emitMonitorOutput(
	run *monitorRun,
	partial, chunk string,
	flush bool,
	limiter *monitorLimiter,
	now time.Time,
) string {
	lines, remaining := monitorLines(partial+chunk, flush)
	if len(lines) == 0 {
		return remaining
	}
	eventText := truncateMonitorBatch(strings.Join(lines, "\n"))
	allowed, recovered := limiter.take(now)
	run.mu.Lock()
	run.suppressed = limiter.suppressedTotal
	run.mu.Unlock()
	if !allowed {
		return remaining
	}
	if recovered > 0 {
		eventText = fmt.Sprintf("[%d monitor events suppressed because output was too noisy]\n%s", recovered, eventText)
	}
	t.publish(map[string]any{
		"type": "monitor_event", "process_id": run.processID,
		"description": run.description, "event": eventText,
	})
	return remaining
}

func (t *MonitorTool) finishMonitor(run *monitorRun, status string, detail any) {
	run.mu.Lock()
	if run.status != "running" {
		run.mu.Unlock()
		return
	}
	run.status = status
	run.endedAt = time.Now()
	run.mu.Unlock()
	t.publish(map[string]any{
		"type": "monitor_completed", "process_id": run.processID,
		"description": run.description, "status": status, "detail": detail,
	})
}

func (t *MonitorTool) publish(value map[string]any) {
	payload, _ := json.Marshal(value)
	notification := Notification{
		Source: MonitorToolName, ID: newNotificationID(),
		Content: "[MEADOW MONITOR UPDATE — observable tool state]\n" + string(payload),
	}
	t.mu.Lock()
	if !t.closed {
		t.notifications = append(t.notifications, notification)
		select {
		case t.events <- notification:
		default:
		}
	}
	t.mu.Unlock()
}

func (t *MonitorTool) DrainNotifications() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]string, 0, len(t.notifications))
	for _, notification := range t.notifications {
		result = append(result, notification.Content)
	}
	t.notifications = nil
	return result
}

func (t *MonitorTool) NotificationEvents() <-chan Notification { return t.events }

func (t *MonitorTool) ClaimNotification(id string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for index, notification := range t.notifications {
		if notification.ID != id {
			continue
		}
		t.notifications = append(t.notifications[:index], t.notifications[index+1:]...)
		return notification.Content, true
	}
	return "", false
}

func (t *MonitorTool) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	runs := make([]*monitorRun, 0, len(t.runs))
	for _, run := range t.runs {
		runs = append(runs, run)
	}
	t.notifications = nil
	t.mu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*processStopGrace)
	defer cancel()
	for _, run := range runs {
		if run.snapshot().Status == "running" {
			_, _ = t.Manager.Stop(stopCtx, run.processID)
		}
	}
	return nil
}

func monitorLines(value string, flush bool) ([]string, string) {
	parts := strings.Split(value, "\n")
	remaining := ""
	if !flush {
		remaining = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	lines := make([]string, 0, len(parts))
	for _, line := range parts {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, truncateMonitorLine(line))
		}
	}
	return lines, remaining
}

func truncateMonitorLine(value string) string {
	if len(value) <= monitorLineLimit {
		return value
	}
	end := monitorLineLimit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "...(truncated)"
}

func truncateMonitorBatch(value string) string {
	if len(value) <= monitorBatchLimit {
		return value
	}
	end := monitorBatchLimit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "\n...(truncated)"
}

type monitorLimiter struct {
	capacity         int
	tokens           int
	lastRefill       time.Time
	suppressionStart time.Time
	lastSuppression  time.Time
	suppressed       uint64
	suppressedTotal  uint64
}

func newMonitorLimiter(now time.Time) *monitorLimiter {
	return &monitorLimiter{
		capacity: monitorRateCapacity, tokens: monitorRateCapacity, lastRefill: now,
	}
}

func (l *monitorLimiter) take(now time.Time) (allowed bool, recovered uint64) {
	if elapsed := now.Sub(l.lastRefill); elapsed >= monitorRateRefill {
		refills := int(elapsed / monitorRateRefill)
		l.tokens = min(l.capacity, l.tokens+refills)
		l.lastRefill = l.lastRefill.Add(time.Duration(refills) * monitorRateRefill)
	}
	if l.tokens == 0 {
		l.suppressed++
		l.suppressedTotal++
		if l.suppressionStart.IsZero() {
			l.suppressionStart = now
		}
		l.lastSuppression = now
		return false, 0
	}
	l.tokens--
	recovered = l.suppressed
	l.suppressed = 0
	if recovered == 0 || now.Sub(l.lastSuppression) > 3*monitorRateRefill {
		l.suppressionStart = time.Time{}
	}
	return true, recovered
}

func (l *monitorLimiter) shouldKill(now time.Time) bool {
	return !l.suppressionStart.IsZero() &&
		now.Sub(l.suppressionStart) >= monitorOverloadKillDuration
}
