package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

const (
	mtplxHealthMinCompletionTokens = 64
	mtplxHealthMinDecodeTokS       = 20.0
	mtplxHealthSlowSamples         = 2
)

// RuntimeHealthState describes measured inference health, not merely whether
// the HTTP server answers. A runtime can be live while its decode throughput
// has collapsed by an order of magnitude.
type RuntimeHealthState string

const (
	RuntimeHealthUnsupported  RuntimeHealthState = "unsupported"
	RuntimeHealthInsufficient RuntimeHealthState = "insufficient"
	RuntimeHealthBusy         RuntimeHealthState = "busy"
	RuntimeHealthHealthy      RuntimeHealthState = "healthy"
	RuntimeHealthDegraded     RuntimeHealthState = "degraded"
)

// RuntimeHealthSample is intentionally small and model-output-free so it can
// be logged or surfaced in product diagnostics without exposing prompts.
type RuntimeHealthSample struct {
	State             RuntimeHealthState
	Model             string
	ActiveRequests    int
	RequestsCompleted int64
	CompletionTokens  int
	DecodeTokS        float64
	Warmup            bool
	LaunchID          string
	PID               int
}

type mtplxHealthResponse struct {
	Model             string `json:"model"`
	ActiveRequests    int    `json:"active_requests"`
	RequestsCompleted int64  `json:"requests_completed"`
	Startup           struct {
		LaunchID string `json:"launch_id"`
		PID      int    `json:"pid"`
	} `json:"startup"`
}

type mtplxRuntimeIdentity struct {
	LaunchID string
	PID      int
}

type mtplxMetricsResponse struct {
	Latest *struct {
		CompletionTokens  int      `json:"completion_tokens"`
		DecodeTokS        *float64 `json:"decode_tok_s"`
		DisplayDecodeTokS *float64 `json:"display_decode_tok_s"`
		Warmup            bool     `json:"warmup"`
	} `json:"latest"`
}

func (c Config) runtimeRootURL() string {
	return fmt.Sprintf("http://%s", c.hostPort())
}

func getRuntimeJSON(ctx context.Context, client *http.Client, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

func (m *Manager) probeMTPLXHealth(ctx context.Context) (mtplxHealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var health mtplxHealthResponse
	err := getRuntimeJSON(ctx, &http.Client{Timeout: 3 * time.Second}, m.Cfg.runtimeRootURL()+"/health", &health)
	return health, err
}

func (m *Manager) probeMTPLXIdentity(ctx context.Context) (mtplxRuntimeIdentity, error) {
	health, err := m.probeMTPLXHealth(ctx)
	if err != nil {
		return mtplxRuntimeIdentity{}, err
	}
	return mtplxRuntimeIdentity{LaunchID: health.Startup.LaunchID, PID: health.Startup.PID}, nil
}

func (m *Manager) refreshRuntimeOwnership(ctx context.Context) bool {
	if m.Cfg.runtimeBackend() != RuntimeBackendMTPLX {
		return true
	}
	identity, err := m.probeMTPLXIdentity(ctx)
	if err != nil || identity.LaunchID == "" || identity.LaunchID != m.Cfg.LaunchID || identity.PID <= 1 {
		return false
	}
	m.mu.Lock()
	m.ownedRuntimePID = identity.PID
	m.mu.Unlock()
	return true
}

// ProbeRuntimeHealth reads MTPLX's native health and metrics endpoints. It
// never generates tokens and never mutates server state. Non-MTPLX models are
// explicitly unsupported so the validated Gemma 12B/26B vllm-mlx path incurs
// no additional requests or policy changes.
func (m *Manager) ProbeRuntimeHealth(ctx context.Context) (RuntimeHealthSample, error) {
	if m.Cfg.runtimeBackend() != RuntimeBackendMTPLX {
		return RuntimeHealthSample{State: RuntimeHealthUnsupported, Model: m.Cfg.ServedName}, nil
	}

	health, err := m.probeMTPLXHealth(ctx)
	if err != nil {
		return RuntimeHealthSample{}, err
	}
	sample := RuntimeHealthSample{
		State:             RuntimeHealthInsufficient,
		Model:             health.Model,
		ActiveRequests:    health.ActiveRequests,
		RequestsCompleted: health.RequestsCompleted,
		LaunchID:          health.Startup.LaunchID,
		PID:               health.Startup.PID,
	}
	if health.ActiveRequests > 0 {
		sample.State = RuntimeHealthBusy
		return sample, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	root := m.Cfg.runtimeRootURL()
	var metrics mtplxMetricsResponse
	if err := getRuntimeJSON(ctx, client, root+"/metrics", &metrics); err != nil {
		return RuntimeHealthSample{}, err
	}
	if metrics.Latest == nil {
		return sample, nil
	}
	sample.CompletionTokens = metrics.Latest.CompletionTokens
	sample.Warmup = metrics.Latest.Warmup
	if metrics.Latest.DisplayDecodeTokS != nil {
		sample.DecodeTokS = *metrics.Latest.DisplayDecodeTokS
	} else if metrics.Latest.DecodeTokS != nil {
		sample.DecodeTokS = *metrics.Latest.DecodeTokS
	}
	if sample.Warmup || sample.CompletionTokens < mtplxHealthMinCompletionTokens ||
		sample.DecodeTokS <= 0 || math.IsNaN(sample.DecodeTokS) || math.IsInf(sample.DecodeTokS, 0) {
		return sample, nil
	}
	if sample.DecodeTokS < mtplxHealthMinDecodeTokS {
		sample.State = RuntimeHealthDegraded
	} else {
		sample.State = RuntimeHealthHealthy
	}
	return sample, nil
}

type runtimeHealthAction uint8

const (
	runtimeHealthNoop runtimeHealthAction = iota
	runtimeHealthWarn
	runtimeHealthClearCache
	runtimeHealthRestart
	runtimeHealthRecovered
)

type runtimeHealthTracker struct {
	lastRequestsCompleted int64
	slowSamples           int
	cleanupPending        bool
}

func (t *runtimeHealthTracker) observe(sample RuntimeHealthSample, owned bool) runtimeHealthAction {
	if sample.State != RuntimeHealthHealthy && sample.State != RuntimeHealthDegraded {
		return runtimeHealthNoop
	}
	if sample.RequestsCompleted < t.lastRequestsCompleted {
		// The endpoint was restarted outside this Manager (or recovered after
		// process death). Its monotonic request counter belongs to a new epoch.
		t.lastRequestsCompleted = 0
		t.slowSamples = 0
		t.cleanupPending = false
	}
	if sample.RequestsCompleted <= t.lastRequestsCompleted {
		return runtimeHealthNoop
	}
	t.lastRequestsCompleted = sample.RequestsCompleted

	if sample.State == RuntimeHealthHealthy {
		recovered := t.cleanupPending || t.slowSamples > 0
		t.slowSamples = 0
		t.cleanupPending = false
		if recovered {
			return runtimeHealthRecovered
		}
		return runtimeHealthNoop
	}

	if owned && t.cleanupPending {
		t.slowSamples = 0
		t.cleanupPending = false
		return runtimeHealthRestart
	}
	t.slowSamples++
	if t.slowSamples < mtplxHealthSlowSamples {
		return runtimeHealthNoop
	}
	t.slowSamples = 0
	if !owned {
		return runtimeHealthWarn
	}
	return runtimeHealthClearCache
}

func (m *Manager) ownsLiveProcess() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return (m.cmd != nil && m.cmd.Process != nil && !m.processExited) || m.ownedRuntimePID > 1
}

func (m *Manager) ownsRuntimeSample(sample RuntimeHealthSample) bool {
	if m.Cfg.runtimeBackend() == RuntimeBackendMTPLX {
		return sample.PID > 1 && sample.LaunchID != "" && sample.LaunchID == m.Cfg.LaunchID
	}
	return m.ownsLiveProcess()
}

func (m *Manager) observeRuntimeHealth(sample RuntimeHealthSample) runtimeHealthAction {
	owned := m.ownsRuntimeSample(sample)
	m.mu.Lock()
	defer m.mu.Unlock()
	if owned && sample.PID > 1 && sample.LaunchID == m.Cfg.LaunchID {
		m.ownedRuntimePID = sample.PID
	}
	return m.runtimeHealth.observe(sample, owned)
}

func (m *Manager) resetRuntimeHealth() {
	m.mu.Lock()
	m.runtimeHealth = runtimeHealthTracker{}
	m.mu.Unlock()
}

func (m *Manager) markRuntimeCleanupPending() {
	m.mu.Lock()
	m.runtimeHealth.cleanupPending = true
	m.mu.Unlock()
}

func (m *Manager) postMTPLXCacheClear(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.Cfg.runtimeRootURL()+"/admin/cache/clear", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cache clear returned status %d", resp.StatusCode)
	}
	return nil
}

func (m *Manager) waitRuntimeStopped(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !m.Healthy(ctx) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return !m.Healthy(ctx)
}

// watchRuntimeHealth is called only after the ordinary liveness probe passes.
// It returns true when it completed a managed-process restart.
func (m *Manager) watchRuntimeHealth(ctx context.Context) bool {
	sample, err := m.ProbeRuntimeHealth(ctx)
	if err != nil {
		// Runtime metrics are supplementary. A metrics schema/network failure
		// must not make a healthy inference endpoint flap.
		return false
	}
	action := m.observeRuntimeHealth(sample)
	switch action {
	case runtimeHealthRecovered:
		m.event(fmt.Sprintf("MTPLX 推理速度已恢復（%.1f tok/s）", sample.DecodeTokS))
	case runtimeHealthWarn:
		m.event(fmt.Sprintf("外部 MTPLX 服務持續退化（%.1f tok/s）；Meadow 不會重啟非自身管理的程序", sample.DecodeTokS))
	case runtimeHealthClearCache:
		// Recheck immediately before mutation. Never clear while MTPLX reports
		// an in-flight request, even when Meadow owns the process.
		fresh, probeErr := m.ProbeRuntimeHealth(ctx)
		if probeErr != nil || fresh.ActiveRequests > 0 || !m.ownsRuntimeSample(fresh) {
			return false
		}
		if err := m.postMTPLXCacheClear(ctx); err != nil {
			m.event("MTPLX cache 清理失敗：" + err.Error())
			return false
		}
		m.markRuntimeCleanupPending()
		m.event(fmt.Sprintf("MTPLX 推理速度持續偏低（%.1f tok/s），已在閒置時清理 runtime cache", sample.DecodeTokS))
	case runtimeHealthRestart:
		fresh, probeErr := m.ProbeRuntimeHealth(ctx)
		if probeErr != nil || fresh.ActiveRequests > 0 || !m.ownsRuntimeSample(fresh) {
			return false
		}
		m.event(fmt.Sprintf("MTPLX cache 清理後仍退化（%.1f tok/s），正在重啟 Meadow 管理的模型服務", sample.DecodeTokS))
		m.Kill()
		if !m.waitRuntimeStopped(ctx, 10*time.Second) {
			m.event("MTPLX 重啟取消：舊服務在 10 秒內未停止")
			return false
		}
		if err := m.spawn(); err != nil {
			m.event("MTPLX 重啟失敗：" + err.Error())
			return false
		}
		if err := m.waitReady(ctx, 180*time.Second); err != nil {
			m.event("MTPLX 重啟後未就緒：" + err.Error())
			return false
		}
		m.resetRuntimeHealth()
		m.event("MTPLX 模型服務已從吞吐退化中恢復")
		return true
	}
	return false
}

// WatchRuntimeHealth monitors measured MTPLX throughput without taking over
// ordinary liveness supervision. Interactive CLI uses this because it can be
// attached to a hand-started MTPLX server: such a server receives diagnostics
// only, while a process actually spawned by this Manager remains eligible for
// the bounded cache-clear/restart recovery above.
func (m *Manager) WatchRuntimeHealth(ctx context.Context) {
	if m.Cfg.runtimeBackend() != RuntimeBackendMTPLX {
		return
	}
	// Establish a request-counter baseline immediately, then sample at the
	// same cadence as the daemon's liveness supervisor.
	m.watchRuntimeHealth(ctx)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.watchRuntimeHealth(ctx)
		}
	}
}
