package mempalace

import (
	"bytes"
	"context"
	"embed"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed shim/meadow_http_shim_msgpack.py
var shimFS embed.FS

const shimEmbedPath = "shim/meadow_http_shim_msgpack.py"

// Sidecar starts and supervises the local MemPalace shim — the Python service the
// Client talks to. The shim is embedded in the binary and extracted on first use,
// so semantic memory works without the user hand-starting it. Everything is
// best-effort: a missing python interpreter or a failed start just leaves the
// Client to fail-open (recall returns nothing rather than erroring).
type Sidecar struct {
	BaseURL string // shim base url, e.g. http://127.0.0.1:8891
	Port    int    // shim --port
}

// MakeSidecar returns a sidecar when MemPalace is enabled and auto-start is not
// turned off (MEADOW_MEMPALACE_SIDECAR=0), else nil. A nil sidecar is a no-op the
// caller can ignore — useful when the user runs the shim themselves or remotely.
func MakeSidecar() *Sidecar {
	if !enabled() {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_MEMPALACE_SIDECAR"))) {
	case "0", "false", "no", "off":
		return nil
	}
	base := defaultURL
	if v := os.Getenv("MEADOW_MEMPALACE_URL"); v != "" {
		base = v
	}
	return newSidecar(base)
}

func newSidecar(baseURL string) *Sidecar {
	baseURL = strings.TrimRight(baseURL, "/")
	port := defaultPort
	if u, err := url.Parse(baseURL); err == nil {
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				port = n
			}
		}
	}
	return &Sidecar{BaseURL: baseURL, Port: port}
}

const defaultPort = 8891

// EnsureRunning makes the shim reachable: a no-op when /healthz already answers,
// otherwise it extracts the embedded shim and spawns it detached (so it survives
// this process), waiting up to ~12s for health. Best-effort — every failure
// returns nil so it never blocks or breaks startup.
func (s *Sidecar) EnsureRunning(ctx context.Context) error {
	if s == nil || s.healthy(ctx, 800*time.Millisecond) {
		return nil
	}
	script, err := extractShim()
	if err != nil {
		return nil
	}
	py := pythonBin()
	if py == "" {
		return nil
	}

	cmd := exec.Command(py, script, "--port", strconv.Itoa(s.Port))
	if logf, err := os.Create(filepath.Join(filepath.Dir(script), "shim.log")); err == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	// Detach into its own process group so it keeps running after a one-shot CLI
	// exits — the shim is a persistent sidecar, reused by later invocations.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil
	}
	go func() { _ = cmd.Wait() }() // reap on eventual exit; do not block

	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if s.healthy(ctx, time.Second) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

func (s *Sidecar) healthy(ctx context.Context, timeout time.Duration) bool {
	c := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// extractShim writes the embedded shim to ~/.meadow/mempalace and returns its
// path, rewriting only when missing or changed (so a binary upgrade ships a new
// shim). Falls back to the temp dir when the home dir is unavailable.
func extractShim() (string, error) {
	data, err := shimFS.ReadFile(shimEmbedPath)
	if err != nil {
		return "", err
	}
	base, err := os.UserHomeDir()
	if err != nil {
		base = os.TempDir()
	} else {
		base = filepath.Join(base, ".meadow", "mempalace")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(base, "meadow_http_shim_msgpack.py")
	if cur, err := os.ReadFile(dst); err != nil || !bytes.Equal(cur, data) {
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", err
		}
	}
	return dst, nil
}

func pythonBin() string {
	// python3.11 first: the shim's mlx-embeddings stack is pinned there on M1.
	for _, name := range []string{"python3.11", "python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}
