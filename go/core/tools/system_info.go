package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// SystemInfo observes the environment executing the Agent, rather than the
// browser/client device or an assumed physical host. It never invokes a shell.
// Register with registry.Register(&SystemInfo{WS: workspace}).
type SystemInfo struct{ WS *Workspace }

var systemInfoWorkers = make(chan struct{}, 2)

func (*SystemInfo) Name() string { return "system_info" }
func (*SystemInfo) Description() string {
	return "Read the current machine/VM/container where this Agent executes: OS, architecture, kernel, CPU, memory, workspace filesystem and observable Linux cgroup limits. Use for this computer's specifications or available resources. Values describe the executing guest/container, never an assumed physical host or the user's browser device. Visible kernel totals and cgroup limits are separate; unknown/unreadable fields and hidden ancestor limits are explicit. Read-only; no commands, network, credentials or environment dump."
}
func (*SystemInfo) Schema() map[string]any {
	schema := obj(map[string]any{})
	schema["additionalProperties"] = false
	return schema
}
func (*SystemInfo) Kind() Kind                       { return KindReadOnly }
func (*SystemInfo) ParallelSafe(map[string]any) bool { return true }
func (*SystemInfo) SubtaskSafe() bool                { return true }
func (*SystemInfo) CompleteObservation() bool        { return true }
func (t *SystemInfo) ToolWorkspace() *Workspace      { return t.WS }
func (*SystemInfo) ForWorkspace(ws *Workspace) Tool  { return &SystemInfo{WS: ws} }

type systemInfoReport struct {
	SchemaVersion int            `json:"schema_version"`
	ObservedAt    string         `json:"observed_at"`
	Scope         string         `json:"scope"`
	OS            map[string]any `json:"os"`
	CPU           map[string]any `json:"cpu"`
	Memory        map[string]any `json:"memory"`
	Disk          map[string]any `json:"workspace_filesystem"`
	Isolation     map[string]any `json:"isolation"`
	Desktop       map[string]any `json:"desktop_context"`
	Limitations   []string       `json:"limitations"`
}

// The probe keeps Linux fixtures independent of the test runner's host and
// prevents tests from depending on the developer machine's actual resources.
type systemInfoProbe struct {
	readFile func(string) ([]byte, error)
	stat     func(string) (os.FileInfo, error)
	getenv   func(string) string
	numCPU   int
}

func systemInfoReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maximum = 1 << 20
	b, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if len(b) > maximum {
		return nil, fmt.Errorf("observation exceeds %d bytes", maximum)
	}
	return b, err
}

func (t *SystemInfo) Execute(ctx context.Context, args map[string]any) Result {
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	if len(args) != 0 {
		return errResultf("system_info takes no arguments")
	}
	return systemInfoObserve(ctx, systemInfoWorkers, 5*time.Second, t.observe)
}

// Kernel filesystem reads (including network-mounted workspaces) cannot be
// interrupted portably. Stop waiting on cancellation/timeout, bound stranded
// workers, and never publish a late observation to an already-ended call.
func systemInfoObserve(ctx context.Context, slots chan struct{}, timeout time.Duration, observe func(context.Context) Result) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	select {
	case slots <- struct{}{}:
	default:
		return errResultf("system_info unavailable: bounded observation workers are still waiting for earlier OS reads")
	}
	observed := make(chan Result, 1)
	go func() {
		defer func() { <-slots }()
		observed <- observe(ctx)
	}()
	select {
	case result := <-observed:
		if err := ctx.Err(); err != nil {
			return errResult(err)
		}
		return result
	case <-ctx.Done():
		return errResult(ctx.Err())
	}
}

func (t *SystemInfo) observe(ctx context.Context) Result {
	p := systemInfoProbe{readFile: systemInfoReadFile, stat: os.Stat, getenv: os.Getenv, numCPU: runtime.NumCPU()}
	report := systemInfoReport{
		SchemaVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339),
		Scope:  "agent_execution_environment",
		OS:     map[string]any{"name": runtime.GOOS, "architecture": runtime.GOARCH, "architecture_source": "agent_process_target", "kernel_release": nil},
		CPU:    map[string]any{"runtime_visible_logical_cpus": p.numCPU, "model": nil},
		Memory: map[string]any{"kernel_visible_total_bytes": nil, "kernel_visible_available_bytes": nil}, Disk: map[string]any{},
		Isolation: map[string]any{"physical_host": "not_observed", "virtual_machine": "unknown"},
		Desktop:   systemInfoDesktopContext(p),
		Limitations: []string{
			"Guest/container observations do not identify the physical host OS, hardware, total resources or VM configuration.",
			"Filesystem capacity describes the workspace mount, which may be shared; it is not a VM image size or dedicated allocation.",
		},
	}
	collectSystemInfoPlatform(ctx, p, &report)
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	path, source := "", "process_working_directory"
	if t != nil && t.WS != nil && t.WS.Root != "" {
		path, source = t.WS.Root, "workspace_root"
	} else {
		path, _ = os.Getwd()
	}
	if path != "" {
		path, _ = filepath.Abs(path)
		report.Disk = systemInfoDisk(path)
		report.Disk["source"] = source
	} else {
		report.Disk["status"] = "unavailable"
	}
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return errResult(err)
	}
	return Result{LLMContent: string(payload), Display: "System information: " + runtime.GOOS + "/" + runtime.GOARCH}
}

func systemInfoDesktopContext(p systemInfoProbe) map[string]any {
	result := map[string]any{"source": "agent_process_environment", "meadow_os_session": "unknown"}
	if p.getenv("MEADOW_OS_SESSION") == "1" {
		result["meadow_os_session"] = "declared_by_session_marker"
	}
	for _, name := range []string{"XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP"} {
		if value := systemInfoText(p.getenv(name)); value != "" {
			result[strings.ToLower(name)] = value
		}
	}
	return result
}

func systemInfoText(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, value)
	if len(value) > 256 {
		value = string([]rune(value)[:min(128, len([]rune(value)))])
	}
	return value
}
