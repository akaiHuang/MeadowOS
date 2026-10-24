package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

const (
	processLogMaxBytes     = 256 << 10
	processLogDefaultBytes = 16 << 10
	processLogMaxReadBytes = 64 << 10
	processStopGrace       = 2 * time.Second
)

type processState string

const (
	processRunning processState = "running"
	processExited  processState = "exited"
)

// boundedProcessLog is a concurrency-safe tail buffer. startOffset tracks
// bytes discarded from the head so process(logs) can report a stable cursor.
type boundedProcessLog struct {
	mu          sync.Mutex
	data        []byte
	startOffset int64
	totalBytes  int64
}

func (b *boundedProcessLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.totalBytes += int64(len(p))
	b.data = append(b.data, p...)
	if len(b.data) > processLogMaxBytes {
		drop := len(b.data) - processLogMaxBytes
		b.data = append([]byte(nil), b.data[drop:]...)
		b.startOffset += int64(drop)
	}
	return len(p), nil
}

func (b *boundedProcessLog) read(offset int64, limit int) (text string, next int64, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 {
		limit = processLogDefaultBytes
	}
	if limit > processLogMaxReadBytes {
		limit = processLogMaxReadBytes
	}
	if offset < b.startOffset {
		offset = b.startOffset
		truncated = true
	}
	if offset > b.totalBytes {
		offset = b.totalBytes
	}
	start := int(offset - b.startOffset)
	end := start + limit
	if end > len(b.data) {
		end = len(b.data)
	}
	return string(b.data[start:end]), b.startOffset + int64(end), truncated
}

func (b *boundedProcessLog) tail(limit int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 || limit > len(b.data) {
		limit = len(b.data)
	}
	return string(b.data[len(b.data)-limit:])
}

type managedProcess struct {
	id                    string
	command               string
	cwd                   string
	cmd                   *exec.Cmd
	ptmx                  *os.File
	stdin                 io.WriteCloser
	log                   *boundedProcessLog
	temporaryDir          string
	notifyOnComplete      bool
	startedAt             time.Time
	notificationStartedAt time.Time
	done                  chan struct{}
	stopWatchdog          func()

	mu        sync.Mutex
	state     processState
	exitCode  int
	errorText string
	endedAt   time.Time
}

// ProcessSnapshot is the model-visible, serializable state of one owned
// background process. It deliberately carries no raw *os.Process handle.
type ProcessSnapshot struct {
	ProcessID        string `json:"process_id"`
	PID              int    `json:"pid,omitempty"`
	Status           string `json:"status"`
	Command          string `json:"command,omitempty"`
	CWD              string `json:"cwd,omitempty"`
	Output           string `json:"output,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	Error            string `json:"error,omitempty"`
	StartedAt        string `json:"started_at,omitempty"`
	EndedAt          string `json:"ended_at,omitempty"`
	NotifyOnComplete bool   `json:"notify_on_complete,omitempty"`
}

func (p *managedProcess) snapshot() ProcessSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot := ProcessSnapshot{
		ProcessID: p.id, Status: string(p.state), Command: p.command, CWD: p.cwd,
		StartedAt:        p.startedAt.UTC().Format(time.RFC3339Nano),
		NotifyOnComplete: p.notifyOnComplete,
	}
	if p.cmd != nil && p.cmd.Process != nil {
		snapshot.PID = p.cmd.Process.Pid
	}
	if p.state == processExited {
		code := p.exitCode
		snapshot.ExitCode = &code
		snapshot.Error = p.errorText
		snapshot.EndedAt = p.endedAt.UTC().Format(time.RFC3339Nano)
	}
	return snapshot
}

// ProcessManager owns all shell-created background processes for one registry
// and one workspace. Its state survives model turns but never crosses sessions.
type ProcessManager struct {
	ws *Workspace

	mu             sync.Mutex
	processes      map[string]*managedProcess
	scratchRoot    string
	notifications  []Notification
	completionSink func(Notification) error
	events         chan Notification
	closed         bool
}

func NewProcessManager(ws *Workspace) *ProcessManager {
	return &ProcessManager{
		ws: ws, processes: map[string]*managedProcess{},
		events: make(chan Notification, 256),
	}
}

func (m *ProcessManager) ensureScratchRoot() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", fmt.Errorf("process manager is closed")
	}
	if m.scratchRoot != "" {
		return m.scratchRoot, nil
	}
	root, err := os.MkdirTemp("", "meadow-session-*")
	if err != nil {
		return "", fmt.Errorf("create Meadow scratch root: %w", err)
	}
	m.scratchRoot = root
	return root, nil
}

func (m *ProcessManager) NewScratchDir() (string, error) {
	root, err := m.ensureScratchRoot()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "command-*")
	if err != nil {
		return "", fmt.Errorf("create command scratch directory: %w", err)
	}
	return dir, nil
}

func (m *ProcessManager) ScratchRoot() (string, error) {
	return m.ensureScratchRoot()
}

func (m *ProcessManager) Start(command, cwd string, env []string, usePTY, temporary, notify bool) (ProcessSnapshot, error) {
	return m.startWithOrigin(command, cwd, env, usePTY, temporary, notify, time.Now())
}

func (m *ProcessManager) startWithOrigin(command, cwd string, env []string, usePTY, temporary, notify bool, origin time.Time) (ProcessSnapshot, error) {
	if strings.TrimSpace(command) == "" {
		return ProcessSnapshot{}, fmt.Errorf("command parameter is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ProcessSnapshot{}, fmt.Errorf("process manager is closed")
	}
	m.mu.Unlock()

	temporaryDir := ""
	if temporary {
		var err error
		temporaryDir, err = m.NewScratchDir()
		if err != nil {
			return ProcessSnapshot{}, err
		}
		cwd = temporaryDir
	}

	cmd := exec.Command(commandShell(), "-c", command)
	cmd.Dir = cwd
	cmd.Env = env
	proc := &managedProcess{
		id: newProcessID(), command: command, cwd: cwd, cmd: cmd,
		log: &boundedProcessLog{}, temporaryDir: temporaryDir,
		notifyOnComplete: notify, startedAt: time.Now(), done: make(chan struct{}),
		notificationStartedAt: origin,
		state:                 processRunning,
	}

	if usePTY {
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
		if err != nil {
			if temporaryDir != "" {
				_ = os.RemoveAll(temporaryDir)
			}
			return ProcessSnapshot{}, fmt.Errorf("start background PTY: %w", err)
		}
		proc.ptmx = ptmx
		proc.stdin = ptmx
	} else {
		configureProcessGroup(cmd)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			if temporaryDir != "" {
				_ = os.RemoveAll(temporaryDir)
			}
			return ProcessSnapshot{}, fmt.Errorf("create background stdin: %w", err)
		}
		proc.stdin = stdin
		cmd.Stdout = proc.log
		cmd.Stderr = proc.log
		if err := cmd.Start(); err != nil {
			_ = stdin.Close()
			if temporaryDir != "" {
				_ = os.RemoveAll(temporaryDir)
			}
			return ProcessSnapshot{}, fmt.Errorf("start background command: %w", err)
		}
	}
	stopWatchdog, err := startProcessWatchdog(os.Getpid(), cmd.Process.Pid)
	if err != nil {
		_ = forceKillProcessTree(cmd.Process)
		if proc.ptmx != nil {
			_ = proc.ptmx.Close()
		}
		if proc.stdin != nil {
			_ = proc.stdin.Close()
		}
		if temporaryDir != "" {
			_ = os.RemoveAll(temporaryDir)
		}
		return ProcessSnapshot{}, fmt.Errorf("start process orphan watchdog: %w", err)
	}
	proc.stopWatchdog = stopWatchdog

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		if proc.stopWatchdog != nil {
			proc.stopWatchdog()
		}
		_ = forceKillProcessTree(cmd.Process)
		if proc.ptmx != nil {
			_ = proc.ptmx.Close()
		}
		if proc.stdin != nil {
			_ = proc.stdin.Close()
		}
		if temporaryDir != "" {
			_ = os.RemoveAll(temporaryDir)
		}
		return ProcessSnapshot{}, fmt.Errorf("process manager closed while command started")
	}
	m.processes[proc.id] = proc
	m.mu.Unlock()

	if usePTY {
		go m.waitPTY(proc)
	} else {
		go m.waitProcess(proc)
	}
	return proc.snapshot(), nil
}

func (m *ProcessManager) waitProcess(proc *managedProcess) {
	m.finish(proc, proc.cmd.Wait())
}

func (m *ProcessManager) waitPTY(proc *managedProcess) {
	_, _ = io.Copy(proc.log, proc.ptmx)
	_ = proc.ptmx.Close()
	m.finish(proc, proc.cmd.Wait())
}

func (m *ProcessManager) finish(proc *managedProcess, waitErr error) {
	defer close(proc.done)
	if proc.stopWatchdog != nil {
		proc.stopWatchdog()
	}
	exitCode := 0
	errorText := ""
	if waitErr != nil {
		exitCode = -1
		errorText = waitErr.Error()
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	proc.mu.Lock()
	proc.state = processExited
	proc.exitCode = exitCode
	proc.errorText = errorText
	proc.endedAt = time.Now()
	proc.mu.Unlock()
	if proc.stdin != nil {
		_ = proc.stdin.Close()
	}
	if proc.temporaryDir != "" {
		_ = os.RemoveAll(proc.temporaryDir)
	}

	if proc.notifyOnComplete {
		snapshot := proc.snapshot()
		payload, _ := json.Marshal(snapshot)
		notification := Notification{
			Source: "process", ID: newNotificationID(),
			StartedAt: &proc.notificationStartedAt,
			Content:   "[BACKGROUND PROCESS UPDATE — observable tool state]\n" + string(payload),
		}
		m.mu.Lock()
		if !m.closed || m.completionSink != nil {
			m.notifications = append(m.notifications, notification)
			sink := m.completionSink
			m.mu.Unlock()
			if sink != nil {
				_ = sink(notification)
			}
			m.mu.Lock()
			select {
			case m.events <- notification:
			default:
				// The in-memory queue remains drainable by an active runtime
				// if a detached UI falls behind the completion stream.
			}
		}
		m.mu.Unlock()
	}
}

func (m *ProcessManager) get(id string) (*managedProcess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	proc, ok := m.processes[id]
	if !ok {
		return nil, fmt.Errorf("unknown process_id %q", id)
	}
	return proc, nil
}

func (m *ProcessManager) Poll(id string) (ProcessSnapshot, error) {
	proc, err := m.get(id)
	if err != nil {
		return ProcessSnapshot{}, err
	}
	snapshot := proc.snapshot()
	snapshot.Output = proc.log.tail(4096)
	return snapshot, nil
}

func (m *ProcessManager) Wait(ctx context.Context, id string, timeout time.Duration) (ProcessSnapshot, error) {
	proc, err := m.get(id)
	if err != nil {
		return ProcessSnapshot{}, err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > shellMaxTimeout {
		timeout = shellMaxTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-proc.done:
		snapshot := proc.snapshot()
		snapshot.Output = proc.log.tail(4096)
		return snapshot, nil
	case <-ctx.Done():
		return proc.snapshot(), ctx.Err()
	case <-timer.C:
		snapshot := proc.snapshot()
		snapshot.Output = proc.log.tail(4096)
		// A bounded observation window ending is not a process failure. Return
		// the current state and output so the model can decide whether to wait,
		// inspect logs, do other work, or stop. Context cancellation above is
		// still an error, and exited processes retain their actual exit code.
		return snapshot, nil
	}
}

func (m *ProcessManager) List() []ProcessSnapshot {
	m.mu.Lock()
	processes := make([]*managedProcess, 0, len(m.processes))
	for _, proc := range m.processes {
		processes = append(processes, proc)
	}
	m.mu.Unlock()
	sort.Slice(processes, func(i, j int) bool {
		return processes[i].startedAt.Before(processes[j].startedAt)
	})
	result := make([]ProcessSnapshot, 0, len(processes))
	for _, proc := range processes {
		result = append(result, proc.snapshot())
	}
	return result
}

type ProcessLogResult struct {
	ProcessID string `json:"process_id"`
	Output    string `json:"output"`
	Next      int64  `json:"next_offset"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (m *ProcessManager) Logs(id string, offset int64, limit int) (ProcessLogResult, error) {
	proc, err := m.get(id)
	if err != nil {
		return ProcessLogResult{}, err
	}
	output, next, truncated := proc.log.read(offset, limit)
	return ProcessLogResult{ProcessID: id, Output: output, Next: next, Truncated: truncated}, nil
}

func (m *ProcessManager) Write(id, data string, eof bool) (ProcessSnapshot, error) {
	proc, err := m.get(id)
	if err != nil {
		return ProcessSnapshot{}, err
	}
	snapshot := proc.snapshot()
	if snapshot.Status != string(processRunning) {
		return snapshot, fmt.Errorf("process %s is not running", id)
	}
	if data != "" {
		if _, err := io.WriteString(proc.stdin, data); err != nil {
			return proc.snapshot(), fmt.Errorf("write process input: %w", err)
		}
	}
	if eof {
		if proc.ptmx != nil {
			_, _ = proc.ptmx.Write([]byte{4}) // terminal EOF (Ctrl-D)
		} else if proc.stdin != nil {
			_ = proc.stdin.Close()
		}
	}
	return proc.snapshot(), nil
}

func (m *ProcessManager) Stop(ctx context.Context, id string) (ProcessSnapshot, error) {
	proc, err := m.get(id)
	if err != nil {
		return ProcessSnapshot{}, err
	}
	if snapshot := proc.snapshot(); snapshot.Status == string(processExited) {
		select {
		case <-proc.done:
			return snapshot, nil
		case <-ctx.Done():
			return snapshot, ctx.Err()
		}
	}
	_ = terminateProcessTree(proc.cmd.Process)
	timer := time.NewTimer(processStopGrace)
	defer timer.Stop()
	select {
	case <-proc.done:
		return proc.snapshot(), nil
	case <-ctx.Done():
		_ = forceKillProcessTree(proc.cmd.Process)
		if proc.ptmx != nil {
			_ = proc.ptmx.Close()
		}
		return proc.snapshot(), ctx.Err()
	case <-timer.C:
		_ = forceKillProcessTree(proc.cmd.Process)
		if proc.ptmx != nil {
			_ = proc.ptmx.Close()
		}
		select {
		case <-proc.done:
			return proc.snapshot(), nil
		case <-ctx.Done():
			return proc.snapshot(), ctx.Err()
		case <-time.After(processStopGrace):
			return proc.snapshot(), fmt.Errorf("process %s did not exit after kill", id)
		}
	}
}

// SetCompletionSink transfers delivery ownership to a durable host. The sink
// runs before the channel hint and outside the process mutex. On disk failure
// PendingNotifications retains the exact completion for host retry.
func (m *ProcessManager) SetCompletionSink(sink func(Notification) error) {
	m.mu.Lock()
	m.completionSink = sink
	m.mu.Unlock()
}
func (m *ProcessManager) PendingNotifications() []Notification {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Notification(nil), m.notifications...)
}
func (m *ProcessManager) DrainNotifications() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Hosted sources use one durable dispatcher; the active model must not also
	// destructively consume the same completion and cause a second automatic turn.
	if m.completionSink != nil {
		return nil
	}
	result := make([]string, 0, len(m.notifications))
	for _, notification := range m.notifications {
		result = append(result, notification.Content)
	}
	m.notifications = nil
	return result
}

func (m *ProcessManager) NotificationEvents() <-chan Notification {
	return m.events
}

func (m *ProcessManager) ClaimNotification(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, notification := range m.notifications {
		if notification.ID != id {
			continue
		}
		m.notifications = append(m.notifications[:i], m.notifications[i+1:]...)
		return notification.Content, true
	}
	return "", false
}

func (m *ProcessManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	processes := make([]*managedProcess, 0, len(m.processes))
	for _, proc := range m.processes {
		processes = append(processes, proc)
	}
	root := m.scratchRoot
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var errs []error
	for _, proc := range processes {
		if _, err := m.Stop(ctx, proc.id); err != nil {
			errs = append(errs, err)
		}
	}
	if root != "" {
		if err := os.RemoveAll(root); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func newProcessID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return "proc_" + hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("proc_%d", time.Now().UnixNano())
}

func newNotificationID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return "notify_" + hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("notify_%d", time.Now().UnixNano())
}

func processJSON(value any) Result {
	payload, err := json.Marshal(value)
	if err != nil {
		return errResult(err)
	}
	return Result{LLMContent: string(payload), Display: string(payload)}
}

// ProcessTool exposes only processes owned by this Meadow registry. It cannot
// inspect or signal arbitrary OS PIDs.
type ProcessTool struct {
	Manager *ProcessManager
}

func FindProcessManager(registry *Registry) (*ProcessManager, bool) {
	if registry == nil {
		return nil, false
	}
	tool, ok := registry.Get("process")
	if !ok {
		return nil, false
	}
	process, ok := tool.(*ProcessTool)
	return process.Manager, ok && process.Manager != nil
}

func (t *ProcessTool) Name() string { return "process" }
func (t *ProcessTool) Description() string {
	return "Inspect, wait for, interact with, or stop background processes started by run_shell_command(background=true). A wait can return status=running when its observation window ends; this is not a failure. Reuse the returned process_id to observe the same process; status=exited includes its actual exit_code."
}
func (t *ProcessTool) Kind() Kind { return KindExecute }
func (t *ProcessTool) ClassifyCall(args map[string]any) Kind {
	switch strings.ToLower(strings.TrimSpace(strArg(args, "action"))) {
	case "list", "poll", "logs", "log", "wait":
		return KindReadOnly
	default:
		return KindExecute
	}
}
func (t *ProcessTool) ParallelSafe(args map[string]any) bool {
	return t.ClassifyCall(args) == KindReadOnly
}
func (t *ProcessTool) Schema() map[string]any {
	return obj(map[string]any{
		"action": map[string]any{
			"type": "string", "enum": []string{"list", "poll", "logs", "log", "wait", "write", "stop", "kill"},
			"description": "Operation to perform.",
		},
		"process_id": str("Opaque process_id returned by run_shell_command."),
		"offset":     num("Byte offset for logs; use next_offset to continue."),
		"limit":      num("Maximum log bytes to return (max 65536)."),
		"timeout":    num("For action=wait, maximum milliseconds for this observation (default 30000, max 600000). If still running, returns its current status and output without stopping or restarting it."),
		"data":       str("Input to send for action=write."),
		"eof":        boolean("Send EOF after data for action=write."),
	}, "action")
}
func (t *ProcessTool) Execute(ctx context.Context, args map[string]any) Result {
	if t.Manager == nil {
		return errResultf("process manager is not configured")
	}
	action := strings.ToLower(strings.TrimSpace(strArg(args, "action")))
	id := strings.TrimSpace(strArg(args, "process_id"))
	switch action {
	case "list":
		return processJSON(map[string]any{"processes": t.Manager.List()})
	case "poll":
		if id == "" {
			return errResultf("process_id is required for poll")
		}
		snapshot, err := t.Manager.Poll(id)
		if err != nil {
			return errResult(err)
		}
		return processJSON(snapshot)
	case "logs", "log":
		if id == "" {
			return errResultf("process_id is required for logs")
		}
		offset, _ := intArg(args, "offset")
		limit, _ := intArg(args, "limit")
		logs, err := t.Manager.Logs(id, int64(offset), limit)
		if err != nil {
			return errResult(err)
		}
		return processJSON(logs)
	case "wait":
		if id == "" {
			return errResultf("process_id is required for wait")
		}
		timeout := 30 * time.Second
		if ms, ok := intArg(args, "timeout"); ok && ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
		snapshot, err := t.Manager.Wait(ctx, id, timeout)
		if err != nil {
			return errResult(err)
		}
		return processJSON(snapshot)
	case "write":
		if id == "" {
			return errResultf("process_id is required for write")
		}
		snapshot, err := t.Manager.Write(id, strArg(args, "data"), boolArg(args, "eof"))
		if err != nil {
			return errResult(err)
		}
		return processJSON(snapshot)
	case "stop", "kill":
		if id == "" {
			return errResultf("process_id is required for stop")
		}
		snapshot, err := t.Manager.Stop(ctx, id)
		if err != nil {
			return errResult(err)
		}
		return processJSON(snapshot)
	default:
		return errResultf("unknown action %q (list|poll|logs|wait|write|stop)", action)
	}
}

func (t *ProcessTool) Close() error {
	if t.Manager == nil {
		return nil
	}
	return t.Manager.Close()
}

func (t *ProcessTool) DrainNotifications() []string {
	if t.Manager == nil {
		return nil
	}
	return t.Manager.DrainNotifications()
}

func (t *ProcessTool) NotificationEvents() <-chan Notification {
	if t.Manager == nil {
		closed := make(chan Notification)
		close(closed)
		return closed
	}
	return t.Manager.NotificationEvents()
}

func (t *ProcessTool) ClaimNotification(id string) (string, bool) {
	if t.Manager == nil {
		return "", false
	}
	return t.Manager.ClaimNotification(id)
}

func processEnvironment(manager *ProcessManager) ([]string, string, error) {
	// Model-controlled commands receive a small explicit runtime allowlist, not
	// the Meadow/Desktop host environment (which can contain API keys, signing
	// credentials, and test canaries). HOME and caches are redirected to the
	// session scratch root below when a process manager is available.
	env := make([]string, 0, 16)
	for _, key := range []string{
		"PATH", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "SHELL",
		"USER", "LOGNAME", "DEVELOPER_DIR", "SDKROOT",
	} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	if manager == nil {
		return env, "", nil
	}
	if manager.ws != nil {
		if desktop := manager.ws.DesktopSession(); desktop != nil {
			env = append(env, desktop.shellEnvironment()...)
		}
	}
	root, err := manager.ScratchRoot()
	if err != nil {
		return nil, "", err
	}
	root = filepath.Clean(root)
	env = append(env,
		"MEADOW_SCRATCH_DIR="+root,
		"HOME="+root,
		"XDG_CACHE_HOME="+filepath.Join(root, ".cache"),
	)
	return env, root, nil
}
