// Package model owns the local inference server lifecycle (M3): detect, spawn,
// health-check, and restart. The goal is that no user ever types a backend
// launch incantation again — meadow brings its model server up
// and keeps it up.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// Config is the launch recipe for one local model server.
type Config struct {
	BinPath    string // runtime binary; "" = backend-specific discovery
	ModelPath  string // model directory (e.g. ~/Desktop/LLM/gemma-4-26b-a4b-qat-mlx)
	ServedName string // --served-model-name
	// RuntimeBackend selects the local server implementation. Empty derives
	// from the served model profile and preserves vllm-mlx for all existing
	// Meadow 12B/26B/35B recipes.
	RuntimeBackend string
	// LaunchID is passed to MTPLX and echoed from /health. A stable Meadow ID
	// lets a later CLI process adopt only servers Meadow originally launched.
	LaunchID string
	Host     string
	Port     int
	// MemoryPreflight protects the product experience: before spawning a
	// managed model server, Meadow checks that enough reclaimable memory is
	// available for the model plus a system reserve. Hand-started servers are
	// never killed by this check; it only refuses to add more pressure.
	MemoryPreflight bool
	// RuntimeProfile selects backend tuning. vllm-mlx supports Meadow profiles;
	// MTPLX defaults to the validated sustained profile. ExtraArgs still wins.
	RuntimeProfile string
	// MaxTokens caps --max-tokens / --max-request-tokens.
	MaxTokens int
	// WarmPrompts points to a vllm-mlx JSON warm-prompts file. It pre-fills
	// stable agent prefixes at server startup so the first real request can hit
	// the prefix cache. Empty leaves startup cold.
	WarmPrompts string
	// ExtraArgs are appended to the launch command; a flag that also exists
	// in the base recipe overrides it (see mergeArgs).
	ExtraArgs []string
	// LogPath receives the server's combined output.
	LogPath string
	// Env overrides are scoped to this managed server process. This is used by
	// routed Multi-LoRA so two servers can load different startup adapters
	// without racing through process-global environment variables.
	Env map[string]string
}

// Defaults fills the Meadow-standard recipe (the one every M0–M2 session
// used) for anything unset.
func (c *Config) defaults() {
	if c.ServedName == "" {
		c.ServedName = "gemma-4-26b-a4b"
	}
	if c.RuntimeBackend == "" {
		c.RuntimeBackend = runtimeBackendFor(c.ServedName, c.BinPath)
	}
	if c.BinPath == "" {
		c.BinPath = defaultRuntimeBin(c.RuntimeBackend)
	}
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Port == 0 {
		c.Port = 8080
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 16384
		if c.runtimeBackend() == RuntimeBackendMTPLX {
			c.MaxTokens = 8192
		}
	}
	if c.runtimeBackend() == RuntimeBackendMTPLX {
		if c.RuntimeProfile == "" {
			c.RuntimeProfile = "sustained"
		}
		if c.LaunchID == "" {
			c.LaunchID = defaultMTPLXLaunchID(*c)
		}
		if c.Env == nil {
			c.Env = map[string]string{}
		}
		// MTPLX 2.2.0 quickstart forwards this documented server env to the
		// child, while its quickstart argparse surface does not accept the
		// equivalent --app-launch-id flag.
		c.Env["MTPLX_APP_LAUNCH_ID"] = c.LaunchID
	}
	if c.LogPath == "" {
		name := "meadow-vllm-mlx.log"
		if c.runtimeBackend() == RuntimeBackendMTPLX {
			name = "meadow-mtplx.log"
		}
		c.LogPath = filepath.Join(os.TempDir(), name)
	}
}

func (c Config) baseURL() string {
	return fmt.Sprintf("http://%s/v1", c.hostPort())
}

func (c Config) hostPort() string {
	return net.JoinHostPort(strings.Trim(c.Host, "[]"), strconv.Itoa(c.Port))
}

// args builds the launch command line — the exact recipe validated since
// M0 (native tool parser, thinking off by default at the template level),
// with any ExtraArgs from the settings.json modelServer recipe merged on top.
func (c Config) args() []string {
	return c.argsForModelPath(c.ModelPath)
}

func (c Config) argsForModelPath(modelPath string) []string {
	if c.runtimeBackend() == RuntimeBackendMTPLX {
		return c.mtplxArgsForModelPath(modelPath)
	}
	parserArgs := parserArgsForServedName(c.ServedName)
	base := []string{
		"serve", modelPath,
		"--host", c.Host,
		"--port", strconv.Itoa(c.Port),
		"--served-model-name", c.ServedName,
		"--default-chat-template-kwargs", `{"enable_thinking": false}`,
		"--max-tokens", strconv.Itoa(c.MaxTokens),
		"--max-request-tokens", strconv.Itoa(c.MaxTokens),
	}
	base = append(base[:10], append(parserArgs, base[10:]...)...)
	base = mergeArgs(base, multimodalArgsForServedName(c.ServedName))
	base = mergeArgs(base, runtimeProfileArgs(c.RuntimeProfile))
	if c.WarmPrompts != "" {
		base = mergeArgs(base, []string{"--warm-prompts", c.WarmPrompts})
	}
	if len(c.ExtraArgs) == 0 {
		return base
	}
	return mergeArgs(base, c.ExtraArgs)
}

func parserArgsForServedName(servedName string) []string {
	toolParser := "gemma4"
	reasoningParser := "gemma4"
	emitReasoningParser := true
	if prof, ok := provider.ProfileForModel(servedName); ok {
		if prof.ToolParser != "" {
			toolParser = prof.ToolParser
		}
		reasoningParser = prof.ReasoningParser
		emitReasoningParser = prof.ReasoningParser != ""
	}
	args := []string{
		"--enable-auto-tool-choice",
		"--tool-call-parser", toolParser,
	}
	if emitReasoningParser {
		args = append(args, "--reasoning-parser", reasoningParser)
	}
	return args
}

func multimodalArgsForServedName(servedName string) []string {
	if prof, ok := provider.ProfileForModel(servedName); ok && prof.Multimodal {
		return []string{
			"--mllm",
			"--mllm-prefill-step-size", "1024",
		}
	}
	return nil
}

func runtimeProfileArgs(profile string) []string {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "", "single", "default", "custom":
		return nil
	case "standard-q4", "q4", "baseline", "unboosted":
		return []string{
			"--prefill-batch-size", "1",
			"--completion-batch-size", "1",
			"--gpu-memory-utilization", "0.30",
			"--max-kv-size", "65536",
			"--timeout", "300",
		}
	case "product", "memory-safe", "safe":
		return []string{
			"--prefill-batch-size", "1",
			"--completion-batch-size", "1",
			"--gpu-memory-utilization", "0.30",
			"--use-paged-cache",
			"--max-kv-size", "65536",
			"--kv-cache-quantization",
			"--kv-cache-quantization-bits", "4",
			"--kv-cache-quantization-group-size", "64",
			"--kv-cache-min-quantize-tokens", "256",
			"--enable-prefix-cache",
			"--cache-memory-percent", "0.15",
			"--chunked-prefill-tokens", "512",
			"--prefill-step-size", "512",
			"--mllm-prefill-step-size", "512",
			"--timeout", "300",
		}
	case "mxfp4-kv8", "mxfp4", "kv8":
		return []string{
			"--prefill-batch-size", "1",
			"--completion-batch-size", "1",
			"--gpu-memory-utilization", "0.30",
			"--use-paged-cache",
			"--max-kv-size", "65536",
			"--kv-cache-quantization",
			"--kv-cache-quantization-bits", "8",
			"--kv-cache-quantization-group-size", "64",
			"--kv-cache-min-quantize-tokens", "256",
			"--enable-prefix-cache",
			"--cache-memory-percent", "0.15",
			"--chunked-prefill-tokens", "512",
			"--prefill-step-size", "512",
			"--mllm-prefill-step-size", "512",
			"--timeout", "300",
		}
	case "mxfp4-kv8-fast", "mxfp4-kv8-12b", "mxfp4-kv8-threshold":
		return []string{
			"--prefill-batch-size", "1",
			"--completion-batch-size", "1",
			"--gpu-memory-utilization", "0.30",
			"--use-paged-cache",
			"--max-kv-size", "65536",
			"--kv-cache-quantization",
			"--kv-cache-quantization-bits", "8",
			"--kv-cache-quantization-group-size", "64",
			"--kv-cache-min-quantize-tokens", "5000",
			"--enable-prefix-cache",
			"--cache-memory-percent", "0.15",
			"--chunked-prefill-tokens", "512",
			"--prefill-step-size", "512",
			"--mllm-prefill-step-size", "512",
			"--timeout", "300",
		}
	case "mxfp4-kv0", "mxfp4-no-kv", "mxfp4-safe":
		return []string{
			"--prefill-batch-size", "1",
			"--completion-batch-size", "1",
			"--gpu-memory-utilization", "0.30",
			"--use-paged-cache",
			"--max-kv-size", "65536",
			"--enable-prefix-cache",
			"--cache-memory-percent", "0.15",
			"--chunked-prefill-tokens", "512",
			"--prefill-step-size", "512",
			"--mllm-prefill-step-size", "512",
			"--timeout", "300",
		}
	case "prefix", "prefix-cache":
		return []string{
			"--enable-prefix-cache",
			"--cache-memory-percent", "0.20",
			"--timeout", "300",
		}
	case "chunked", "chunked-prefill":
		return []string{
			"--prefill-batch-size", "1",
			"--chunked-prefill-tokens", "512",
			"--prefill-step-size", "512",
			"--mllm-prefill-step-size", "512",
			"--timeout", "300",
		}
	case "cache-slide", "cacheslide":
		// vllm-mlx only reuses exact token prefixes. This profile enables the
		// cache machinery Meadow needs for its application-level "cache slide"
		// strategy: stable memory/RAG/tool prefixes stay at the front, while
		// varying user deltas slide after them.
		return []string{
			"--enable-prefix-cache",
			"--cache-memory-percent", "0.25",
			"--use-paged-cache",
			"--paged-cache-block-size", "64",
			"--chunked-prefill-tokens", "512",
			"--prefill-step-size", "512",
			"--timeout", "300",
		}
	case "throughput":
		return []string{
			"--continuous-batching",
			"--max-num-seqs", "2",
			"--prefill-batch-size", "1",
			"--completion-batch-size", "2",
			"--gpu-memory-utilization", "0.35",
			"--use-paged-cache",
			"--max-kv-size", "65536",
			"--kv-cache-quantization",
			"--kv-cache-quantization-bits", "8",
			"--enable-prefix-cache",
			"--chunked-prefill-tokens", "512",
			"--mllm-prefill-step-size", "512",
			"--timeout", "300",
		}
	case "turbo", "turbo-kv4", "agent-turbo":
		return []string{
			"--continuous-batching",
			"--max-num-seqs", "2",
			"--prefill-batch-size", "1",
			"--completion-batch-size", "2",
			"--gpu-memory-utilization", "0.35",
			"--use-paged-cache",
			"--max-kv-size", "65536",
			"--kv-cache-quantization",
			"--kv-cache-quantization-bits", "4",
			"--kv-cache-quantization-group-size", "64",
			"--kv-cache-min-quantize-tokens", "256",
			"--enable-prefix-cache",
			"--cache-memory-percent", "0.25",
			"--chunked-prefill-tokens", "512",
			"--prefill-step-size", "512",
			"--mllm-prefill-step-size", "512",
			"--timeout", "300",
		}
	default:
		return nil
	}
}

func defaultRuntimeProfileForModel(servedName, modelPath string) string {
	return "standard-q4"
}

func boostRuntimeProfileForModel(servedName, modelPath string) string {
	if is12BModel(servedName, modelPath) {
		return "mxfp4-kv8-fast"
	}
	if strings.Contains(strings.ToLower(modelPath), "mxfp4") {
		return "mxfp4-kv8"
	}
	return "product"
}

func PlanEnablesBoost(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "monthly", "lifetime", "subscription", "perpetual", "paid":
		return true
	default:
		return false
	}
}

// ApplyPlanRuntime turns licensing into a runtime tier. Trial or no paid plan
// stays on the standard Q4 profile; monthly/lifetime unlocks the Boost profile
// (MXFP4 preference plus KV/cache acceleration). MEADOW_VLLM_PROFILE remains
// the developer escape hatch and wins over the plan profile.
func (c *Config) ApplyPlanRuntime(plan string, allowModelVariantSwap bool) {
	if c.runtimeBackend() == RuntimeBackendMTPLX {
		return
	}
	boost := PlanEnablesBoost(plan)
	// MEADOW_MODEL_PATH is the documented developer escape hatch and must stay
	// above license-driven standard/Boost variant selection. ConfigFromSettings
	// applies it last; do not silently replace it again here.
	if allowModelVariantSwap && strings.TrimSpace(os.Getenv("MEADOW_MODEL_PATH")) == "" && strings.TrimSpace(c.ServedName) != "" {
		if p := ResolveModelPathVariant(c.ServedName, boost); p != "" {
			c.ModelPath = p
		}
	}
	if strings.TrimSpace(os.Getenv("MEADOW_VLLM_PROFILE")) != "" {
		return
	}
	if boost {
		c.RuntimeProfile = boostRuntimeProfileForModel(c.ServedName, c.ModelPath)
		return
	}
	c.RuntimeProfile = "standard-q4"
}

func is12BModel(servedName, modelPath string) bool {
	name := strings.ToLower(strings.TrimSpace(servedName))
	path := strings.ToLower(modelPath)
	return name == strings.ToLower(tier12B.ServedName) || strings.Contains(path, "gemma-4-12b")
}

func isMXFP4KV8Profile(profile string) bool {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "mxfp4-kv8", "mxfp4", "kv8":
		return true
	default:
		return false
	}
}

func shouldMigrateLaunchPathToPrimary(modelID, launchPath, primaryPath string) bool {
	if modelID == "" || strings.Contains(modelID, "/") || launchPath == "" || primaryPath == "" {
		return false
	}
	tier, ok := tierByName(modelID)
	if !ok {
		return false
	}
	if filepath.Clean(launchPath) == filepath.Clean(primaryPath) {
		return false
	}
	if !strings.EqualFold(filepath.Base(primaryPath), tier.DirName) {
		return false
	}
	launchBase := filepath.Base(launchPath)
	for i, name := range tier.dirNames() {
		if strings.EqualFold(launchBase, name) {
			return i > 0
		}
	}
	return false
}

func (c Config) launchModelPath() (string, error) {
	if c.runtimeBackend() == RuntimeBackendMTPLX {
		return c.ModelPath, nil
	}
	if !shouldUseTextAlias(c.ModelPath) {
		return c.ModelPath, nil
	}
	return ensureTextModelAlias(c.ModelPath)
}

func shouldUseTextAlias(modelPath string) bool {
	if modelPath == "" {
		return false
	}
	switch strings.ToLower(os.Getenv("MEADOW_VLLM_TEXT_ALIAS")) {
	case "0", "false", "no", "off":
		return false
	}
	lower := strings.ToLower(modelPath)
	base := filepath.Base(lower)
	if !strings.Contains(lower, "gemma-4") && !strings.Contains(lower, "gemma4") {
		return false
	}
	if _, err := os.Stat(filepath.Join(modelPath, "processor_config.json")); os.IsNotExist(err) {
		return true
	}
	return strings.Contains(base, "e2b") || strings.Contains(base, "e4b")
}

func ensureTextModelAlias(modelPath string) (string, error) {
	sum := sha256.Sum256([]byte(modelPath))
	dir := filepath.Join(os.TempDir(), "meadow-vllm-mlx-aliases")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create vllm-mlx model alias dir: %w", err)
	}
	alias := filepath.Join(dir, "mlx-text-"+hex.EncodeToString(sum[:8]))
	if target, err := os.Readlink(alias); err == nil {
		if target == modelPath {
			return alias, nil
		}
		if err := os.Remove(alias); err != nil {
			return "", fmt.Errorf("replace stale vllm-mlx model alias: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect vllm-mlx model alias: %w", err)
	}
	if err := os.Symlink(modelPath, alias); err != nil {
		return "", fmt.Errorf("create vllm-mlx model alias: %w", err)
	}
	return alias, nil
}

// Manager supervises one local model runtime process (vllm-mlx or MTPLX).
type Manager struct {
	Cfg Config
	// OnEvent, when set, receives lifecycle notices ("spawning", "ready",
	// "restarting: <reason>") for UI display.
	OnEvent func(msg string)

	mu              sync.Mutex
	cmd             *exec.Cmd
	processExited   bool
	stopping        bool
	runtimeHealth   runtimeHealthTracker
	ownedRuntimePID int
	// signalProcess is nil in production (syscall.Kill). Tests replace it to
	// verify adopted MTPLX ownership without signalling a real process.
	signalProcess func(int, syscall.Signal) error
}

func NewManager(cfg Config) *Manager {
	cfg.defaults()
	return &Manager{Cfg: cfg}
}

// OwnsRuntime reports whether this Manager can safely supervise and stop the
// current model process. A healthy server discovered on the configured port
// is usable but remains externally owned; the Gateway must not restart it.
func (m *Manager) OwnsRuntime() bool {
	return m.ownsLiveProcess()
}

func (m *Manager) event(msg string) {
	if m.OnEvent != nil {
		m.OnEvent(msg)
	}
}

func (m *Manager) servedModels(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return provider.NewClient(m.Cfg.baseURL(), "", "").Models(ctx)
}

func (m *Manager) matchesServedModel(served []string) bool {
	want := strings.TrimSpace(m.Cfg.ServedName)
	if want == "" {
		return true
	}
	for _, id := range served {
		if strings.EqualFold(strings.TrimSpace(id), want) {
			return true
		}
	}
	return false
}

// Healthy probes GET /v1/models with a short timeout.
func (m *Manager) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Cfg.baseURL()+"/models", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// EnsureRunning is the CLI entry point: reuse a healthy server (whoever
// started it), otherwise spawn one and wait until it answers.
func (m *Manager) EnsureRunning(ctx context.Context) error {
	if m.Healthy(ctx) {
		if served, err := m.servedModels(ctx); err == nil && len(served) > 0 && !m.matchesServedModel(served) {
			return fmt.Errorf("model server at %s is already serving %s, but Meadow needs %s — unload the current model or switch to that model first",
				m.Cfg.hostPort(), strings.Join(served, ", "), m.Cfg.ServedName)
		}
		m.refreshRuntimeOwnership(ctx)
		return nil
	}
	if m.Cfg.ModelPath == "" {
		want := m.Cfg.ServedName
		if want == "" {
			want = "the model"
		}
		hint := "set modelServer.modelPath in ~/.meadow/settings.json, or export MEADOW_MODEL_DIR to the folder holding your model directories"
		if m.Cfg.runtimeBackend() == RuntimeBackendMTPLX {
			hint = "install the MTPLX model artifact under ~/.mtplx/models, or export MEADOW_MTPLX_MODEL_PATH"
		} else if DefaultModelDirName(m.Cfg.ServedName) != "" {
			hint = fmt.Sprintf("run: meadow pull %q", m.Cfg.ServedName)
		}
		return fmt.Errorf("no model server at %s and the %s weights were not found — %s",
			m.Cfg.hostPort(), want, hint)
	}
	if err := m.spawn(); err != nil {
		return err
	}
	return m.waitReady(ctx, 180*time.Second)
}

func (m *Manager) spawn() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil && !m.processExited {
		return nil // already supervising a live process
	}
	if err := m.Cfg.memoryPreflight(); err != nil {
		return err
	}

	modelPath, err := m.Cfg.launchModelPath()
	if err != nil {
		return err
	}

	logFile, err := os.OpenFile(m.Cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open server log: %w", err)
	}

	cmd := exec.Command(m.Cfg.BinPath, m.Cfg.argsForModelPath(modelPath)...)
	cmd.Env = processEnv(m.Cfg.Env)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Detach into its own process group: the server outlives the meadow
	// process that spawned it, so the next CLI start connects instantly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	m.event(fmt.Sprintf("啟動模型伺服器 %s（載入需 30–90 秒）…", filepath.Base(m.Cfg.ModelPath)))
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("launch %s: %w", m.Cfg.BinPath, err)
	}
	m.cmd = cmd
	m.processExited = false
	m.ownedRuntimePID = cmd.Process.Pid
	go func() {
		_ = cmd.Wait() // reap; restart decisions belong to Watch
		logFile.Close()
		m.mu.Lock()
		if m.cmd == cmd {
			m.processExited = true
			m.ownedRuntimePID = 0
		}
		m.mu.Unlock()
	}()
	return nil
}

func processEnv(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return os.Environ()
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func (m *Manager) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.Healthy(ctx) {
			if m.Cfg.runtimeBackend() == RuntimeBackendMTPLX && !m.refreshRuntimeOwnership(ctx) {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			m.event("模型伺服器就緒")
			return nil
		}
		// Surface an early death instead of waiting out the clock.
		m.mu.Lock()
		dead := m.cmd != nil && m.processExited
		m.mu.Unlock()
		if dead {
			return fmt.Errorf("model server exited during startup — see %s", m.Cfg.LogPath)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("model server not ready after %s — see %s", timeout, m.Cfg.LogPath)
}

// Watch supervises until ctx ends: on two consecutive failed health checks
// (or process death) it restarts the server with backoff. Use from daemon
// mode; one-shot CLI runs only need EnsureRunning.
func (m *Manager) Watch(ctx context.Context) {
	const interval = 20 * time.Second
	backoff := []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	failures := 0
	restarts := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		if m.Healthy(ctx) {
			failures = 0
			restarts = 0
			if restarted := m.watchRuntimeHealth(ctx); restarted {
				// A throughput recovery owns its own ready wait. Start the next
				// liveness interval from a clean failure/backoff state.
				failures = 0
				restarts = 0
			}
			continue
		}
		failures++
		if failures < 2 {
			continue
		}
		m.mu.Lock()
		stopping := m.stopping
		m.mu.Unlock()
		if stopping {
			return
		}

		wait := backoff[min(restarts, len(backoff)-1)]
		m.event(fmt.Sprintf("模型伺服器無回應，%s 後重啟（第 %d 次）", wait, restarts+1))
		m.Kill()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := m.spawn(); err != nil {
			m.event("重啟失敗：" + err.Error())
		} else if err := m.waitReady(ctx, 180*time.Second); err != nil {
			m.event("重啟後未就緒：" + err.Error())
		} else {
			m.resetRuntimeHealth()
			m.event("模型伺服器已恢復")
		}
		restarts++
		failures = 0
	}
}

// Kill terminates the supervised process (if any). MTPLX may be adopted by a
// later Meadow CLI through its matching health launch-id; foreign servers are
// never touched.
func (m *Manager) Kill() {
	m.mu.Lock()
	cmd := m.cmd
	ownedPID := m.ownedRuntimePID
	backend := m.Cfg.runtimeBackend()
	m.cmd = nil
	m.processExited = false
	m.ownedRuntimePID = 0
	m.mu.Unlock()

	if backend == RuntimeBackendMTPLX {
		identity, err := m.probeMTPLXIdentity(context.Background())
		if err == nil {
			if identity.LaunchID != m.Cfg.LaunchID || identity.PID <= 1 {
				return
			}
			ownedPID = identity.PID
		} else if cmd == nil || cmd.Process == nil {
			// An adopted PID is safe only while the endpoint still proves the
			// matching launch id. Avoid signalling a reused stale PID.
			return
		}
		if ownedPID > 1 {
			_ = m.signalRuntimeProcess(ownedPID, syscall.SIGTERM)
			return
		}
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Negative pid signals the whole process group (Setpgid above).
	_ = m.signalRuntimeProcess(-cmd.Process.Pid, syscall.SIGTERM)
}

func (m *Manager) signalRuntimeProcess(pid int, signal syscall.Signal) error {
	if m.signalProcess != nil {
		return m.signalProcess(pid, signal)
	}
	return syscall.Kill(pid, signal)
}

// Stop marks the manager as shutting down and kills the supervised server.
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	m.Kill()
}

// ConfigFromSettings derives the launch recipe from the existing
// settings.json model entry: the endpoint decides host/port, and a
// filesystem-path modelId doubles as the model directory. A non-nil launch
// (the settings.json modelServer object) overrides the modelId-derived
// pieces — it is the user's hand-tuned launch command, so the spawned
// server matches what they would have started themselves. Precedence:
// env escape hatch > launch recipe > modelId-derived > defaults; host/port
// always follow the endpoint the client connects to.
func ConfigFromSettings(baseURL, modelID string, launch *LaunchSettings) Config {
	cfg := Config{MemoryPreflight: true, RuntimeBackend: runtimeBackendFor(modelID, "")}
	primaryModelPath := ""
	if endpoint, err := url.Parse(baseURL); err == nil &&
		(endpoint.Scheme == "http" || endpoint.Scheme == "https") {
		if host := endpoint.Hostname(); host != "" {
			cfg.Host = host
		}
		if port := endpoint.Port(); port != "" {
			if parsed, parseErr := strconv.Atoi(port); parseErr == nil {
				cfg.Port = parsed
			}
		}
	}
	if strings.Contains(modelID, "/") {
		cfg.ModelPath = modelID
	} else if modelID != "" {
		cfg.ServedName = modelID
		// A bare served-name default ("Meadow 26B"/"Meadow 12B") carries no
		// path; resolve the weights from the conventional model roots so a
		// fresh install with no settings.json modelServer recipe can still
		// launch. Empty when the weights are absent — the preflight then names
		// the model and where to put it.
		resolvedPath := ResolveModelPath(modelID)
		if cfg.runtimeBackend() == RuntimeBackendMTPLX {
			resolvedPath = resolveMTPLXModelPath(modelID)
		}
		if p := resolvedPath; p != "" {
			cfg.ModelPath = p
			primaryModelPath = p
		}
	}
	migratedLaunchPath := false
	if launch != nil {
		if launch.Backend != "" {
			cfg.RuntimeBackend = launch.Backend
		}
		if launch.BinPath != "" {
			cfg.BinPath = launch.BinPath
			if launch.Backend == "" {
				cfg.RuntimeBackend = runtimeBackendFor(cfg.ServedName, launch.BinPath)
			}
		}
		if launch.ModelPath != "" {
			cfg.ModelPath = launch.ModelPath
		}
		if launch.ServedModelName != "" {
			cfg.ServedName = launch.ServedModelName
		}
		if launch.MaxTokens != 0 {
			cfg.MaxTokens = launch.MaxTokens
		}
		if launch.WarmPrompts != "" {
			cfg.WarmPrompts = launch.WarmPrompts
		}
		if launch.Profile != "" {
			cfg.RuntimeProfile = launch.Profile
		}
		if cfg.Host == "" && launch.Host != "" {
			cfg.Host = launch.Host
		}
		if cfg.Port == 0 && launch.Port != 0 {
			cfg.Port = launch.Port
		}
		cfg.ExtraArgs = append([]string(nil), launch.ExtraArgs...)
	}
	if launch != nil && shouldMigrateLaunchPathToPrimary(modelID, launch.ModelPath, primaryModelPath) {
		cfg.ModelPath = primaryModelPath
		cfg.RuntimeProfile = ""
		cfg.ExtraArgs = nil
		migratedLaunchPath = true
	}
	// Env overrides stay on top of everything: the quick escape hatch.
	if v := os.Getenv("MEADOW_MODEL_PATH"); v != "" {
		cfg.ModelPath = v
	}
	envProfileSet := false
	if cfg.runtimeBackend() == RuntimeBackendMTPLX {
		if v := os.Getenv("MEADOW_MTPLX_MODEL_PATH"); v != "" {
			cfg.ModelPath = expandHome(v)
		}
		if v := os.Getenv("MEADOW_MTPLX_BIN"); v != "" {
			cfg.BinPath = expandHome(v)
		}
		if v := os.Getenv("MEADOW_MTPLX_PROFILE"); v != "" {
			cfg.RuntimeProfile = v
			envProfileSet = true
		}
	} else {
		if v := os.Getenv("MEADOW_VLLM_BIN"); v != "" {
			cfg.BinPath = v
		}
		if v := os.Getenv("MEADOW_VLLM_PROFILE"); v != "" {
			cfg.RuntimeProfile = v
			envProfileSet = true
		}
	}
	if cfg.RuntimeProfile == "" && cfg.runtimeBackend() != RuntimeBackendMTPLX && (launch == nil || migratedLaunchPath) {
		cfg.RuntimeProfile = defaultRuntimeProfileForModel(cfg.ServedName, cfg.ModelPath)
	}
	if !envProfileSet && is12BModel(cfg.ServedName, cfg.ModelPath) && isMXFP4KV8Profile(cfg.RuntimeProfile) {
		cfg.RuntimeProfile = "mxfp4-kv8-fast"
	}
	if cfg.runtimeBackend() != RuntimeBackendMTPLX {
		if v := os.Getenv("MEADOW_VLLM_WARM_PROMPTS"); v != "" {
			cfg.WarmPrompts = expandHome(v)
		}
	}
	cfg.defaults()
	return cfg
}

// ConfigForExternalGateway describes an already-supervised Meadow Core
// endpoint. It deliberately never resolves or stats local model weights: the
// gateway owns model activation and the CLI must not block on ~/Desktop/LLM
// before it can send the first request.
func ConfigForExternalGateway(baseURL, servedName string) Config {
	cfg := Config{
		ServedName:     strings.TrimSpace(servedName),
		RuntimeBackend: runtimeBackendFor(servedName, ""),
	}
	if endpoint, err := url.Parse(baseURL); err == nil &&
		(endpoint.Scheme == "http" || endpoint.Scheme == "https") {
		if host := endpoint.Hostname(); host != "" {
			cfg.Host = host
		}
		if port := endpoint.Port(); port != "" {
			if parsed, parseErr := strconv.Atoi(port); parseErr == nil {
				cfg.Port = parsed
			}
		}
	}
	return cfg
}
