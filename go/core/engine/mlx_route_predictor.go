package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type MLXRoutePredictorConfig struct {
	PythonPath  string
	ScriptPath  string
	ModelPath   string
	AdapterPath string
	MaxTokens   int
	Temperature float64
	Timeout     time.Duration
	RouteFormat string
}

type MLXRoutePredictor struct {
	cfg     MLXRoutePredictorConfig
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	scanner *bufio.Scanner
	stderr  *bytes.Buffer
}

func NewMLXRoutePredictor(cfg MLXRoutePredictorConfig) (*MLXRoutePredictor, error) {
	cfg.PythonPath = firstNonEmpty(cfg.PythonPath, os.Getenv("MEADOW_ROUTE_PREDICTOR_PYTHON"), "python3")
	cfg.ModelPath = firstNonEmpty(cfg.ModelPath, os.Getenv("MEADOW_ROUTE_PREDICTOR_MODEL"), os.Getenv("MEADOW_CALL_MODEL"))
	cfg.AdapterPath = firstNonEmpty(cfg.AdapterPath, os.Getenv("MEADOW_ROUTE_PREDICTOR_ADAPTER"), os.Getenv("MEADOW_CALL_ADAPTER_PATH"))
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = envInt("MEADOW_ROUTE_PREDICTOR_MAX_TOKENS", 96)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Duration(envInt("MEADOW_ROUTE_PREDICTOR_TIMEOUT_MS", 20_000)) * time.Millisecond
	}
	if cfg.ScriptPath == "" {
		cfg.ScriptPath = os.Getenv("MEADOW_ROUTE_PREDICTOR_SCRIPT")
	}
	if cfg.ScriptPath == "" {
		return nil, errors.New("missing MLX route predictor script path")
	}
	if info, err := os.Stat(cfg.ScriptPath); err != nil {
		return nil, fmt.Errorf("MLX route predictor script is not readable: %w", err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("MLX route predictor script is a directory: %s", cfg.ScriptPath)
	}
	if cfg.AdapterPath == "" {
		return nil, errors.New("missing MLX route predictor adapter path")
	}
	if info, err := os.Stat(cfg.AdapterPath); err != nil {
		return nil, fmt.Errorf("MLX route predictor adapter is not readable: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("MLX route predictor adapter path is not a directory: %s", cfg.AdapterPath)
	}
	for _, name := range []string{"adapter_config.json", "adapters.safetensors"} {
		if info, err := os.Stat(filepath.Join(cfg.AdapterPath, name)); err != nil {
			return nil, fmt.Errorf("MLX route predictor adapter is missing %s: %w", name, err)
		} else if info.IsDir() {
			return nil, fmt.Errorf("MLX route predictor adapter entry is a directory: %s", filepath.Join(cfg.AdapterPath, name))
		}
	}
	return &MLXRoutePredictor{cfg: cfg}, nil
}

func (p *MLXRoutePredictor) PredictRoute(ctx context.Context, userText string, availableToolNames []string) (string, error) {
	if p == nil {
		return "", errors.New("nil MLX route predictor")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.startLocked(); err != nil {
		p.stopLocked()
		return "", err
	}

	payload := map[string]any{
		"user_text":       userText,
		"available_tools": availableToolNames,
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		p.stopLocked()
		return "", fmt.Errorf("write predictor request: %w", err)
	}

	timeout := p.cfg.Timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	readCh := make(chan predictorReadResult, 1)
	scanner := p.scanner
	go func() {
		readCh <- readPredictorResponse(scanner)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		stderrBuffer := p.stderr
		p.stopLocked()
		<-readCh
		return "", predictorError(ctx.Err(), stderrText(stderrBuffer))
	case <-timer.C:
		stderrBuffer := p.stderr
		p.stopLocked()
		<-readCh
		return "", predictorError(fmt.Errorf("MLX route predictor timed out after %s", timeout), stderrText(stderrBuffer))
	case result := <-readCh:
		if result.err != nil {
			stderrBuffer := p.stderr
			p.stopLocked()
			return "", predictorError(result.err, stderrText(stderrBuffer))
		}
		if !result.response.OK {
			return result.response.Route, fmt.Errorf("MLX route predictor: %s", result.response.Error)
		}
		return result.response.Route, nil
	}
}

func (p *MLXRoutePredictor) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopLocked()
}

func (p *MLXRoutePredictor) startLocked() error {
	if p.cmd != nil && p.cmd.Process != nil {
		return nil
	}
	args := []string{
		p.cfg.ScriptPath,
		"--serve",
		"--adapter-path", p.cfg.AdapterPath,
		"--max-tokens", strconv.Itoa(p.cfg.MaxTokens),
	}
	if p.cfg.ModelPath != "" {
		args = append(args, "--model", p.cfg.ModelPath)
	}
	if strings.TrimSpace(p.cfg.RouteFormat) != "" {
		args = append(args, "--route-format", strings.TrimSpace(p.cfg.RouteFormat))
	}
	if p.cfg.Temperature > 0 {
		args = append(args, "--temperature", strconv.FormatFloat(p.cfg.Temperature, 'f', -1, 64))
	}
	cmd := exec.Command(p.cfg.PythonPath, args...)
	cmd.Env = os.Environ()
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	p.cmd = cmd
	p.stdin = stdin
	p.stdout = stdout
	p.scanner = scanner
	p.stderr = stderr
	return nil
}

func stderrText(stderr *bytes.Buffer) string {
	if stderr == nil {
		return ""
	}
	// exec.Cmd.Wait waits for the internal stderr copy goroutine to finish.
	// Callers therefore stop and wait for the worker before reading this buffer;
	// reading it while the copy goroutine is active is a data race.
	return strings.TrimSpace(stderr.String())
}

func predictorError(err error, stderr string) error {
	if strings.TrimSpace(stderr) == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
}

type predictorResponse struct {
	OK    bool   `json:"ok"`
	Route string `json:"route"`
	Error string `json:"error"`
}

type predictorReadResult struct {
	response predictorResponse
	err      error
}

func readPredictorResponse(scanner *bufio.Scanner) predictorReadResult {
	if scanner == nil {
		return predictorReadResult{err: errors.New("MLX route predictor is not running")}
	}
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return predictorReadResult{err: err}
		}
		return predictorReadResult{err: errors.New("MLX route predictor exited without response")}
	}
	var response predictorResponse
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		return predictorReadResult{err: err}
	}
	return predictorReadResult{response: response}
}

func (p *MLXRoutePredictor) stopLocked() error {
	var err error
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	// Close the parent read end before clearing scanner state. This unblocks an
	// in-flight Scanner.Scan even when the worker is stuck and has not exited.
	if p.stdout != nil {
		_ = p.stdout.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		err = p.cmd.Wait()
	}
	p.cmd = nil
	p.stdin = nil
	p.stdout = nil
	p.scanner = nil
	p.stderr = nil
	return err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
