package model

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const (
	RuntimeBackendVLLMMLX = "vllm-mlx"
	RuntimeBackendMTPLX   = "mtplx"
)

func runtimeBackendFor(servedName, binPath string) string {
	if provider.IsMTPLXModel(servedName) {
		return RuntimeBackendMTPLX
	}
	base := strings.ToLower(filepath.Base(strings.TrimSpace(binPath)))
	if base == "mtplx" || strings.HasPrefix(base, "mtplx-") {
		return RuntimeBackendMTPLX
	}
	return RuntimeBackendVLLMMLX
}

func (c Config) runtimeBackend() string {
	backend := strings.ToLower(strings.TrimSpace(c.RuntimeBackend))
	if backend == RuntimeBackendMTPLX {
		return RuntimeBackendMTPLX
	}
	if backend == RuntimeBackendVLLMMLX {
		return RuntimeBackendVLLMMLX
	}
	return runtimeBackendFor(c.ServedName, c.BinPath)
}

func defaultRuntimeBin(backend string) string {
	if backend != RuntimeBackendMTPLX {
		if p, err := exec.LookPath("vllm-mlx"); err == nil {
			return p
		}
		return "/opt/homebrew/bin/vllm-mlx"
	}
	if p, err := exec.LookPath("mtplx"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	var candidates []string
	if legacyDesktopDiscoveryEnabled() {
		if matches, _ := filepath.Glob(filepath.Join(home, "Desktop", "LLM", "bench-envs", "mtplx-*", "bin", "mtplx")); len(matches) > 0 {
			sort.Strings(matches)
			for i := len(matches) - 1; i >= 0; i-- {
				candidates = append(candidates, matches[i])
			}
		}
	}
	candidates = append(candidates, filepath.Join(home, ".local", "bin", "mtplx"))
	for _, candidate := range candidates {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return filepath.Join(home, ".local", "bin", "mtplx")
}

func resolveMTPLXModelPath(servedName string) string {
	if !provider.IsMTPLXModel(servedName) {
		return ""
	}
	if override := strings.TrimSpace(os.Getenv("MEADOW_MTPLX_MODEL_PATH")); override != "" {
		if path := expandHome(override); ModelPresent(path) {
			return path
		}
	}
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".mtplx", "models")
	preferred := filepath.Join(root, "Youssofal--Qwen3.6-35B-A3B-MTPLX-Optimized-Speed")
	if ModelPresent(preferred) {
		return preferred
	}
	patterns := []string{"*Qwen3.6-35B*MTPLX*", "*Qwen3_6-35B*MTPLX*", "*35B*MTPLX*"}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		sort.Strings(matches)
		for _, match := range matches {
			if ModelPresent(match) {
				return match
			}
		}
	}
	return ""
}

func defaultMTPLXLaunchID(c Config) string {
	identity := strings.Join([]string{c.Host, c.ServedName, c.ModelPath, strconv.Itoa(c.Port)}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return "meadow-go-mtplx-v1-" + hex.EncodeToString(sum[:8])
}

func (c Config) mtplxArgsForModelPath(modelPath string) []string {
	profile := strings.TrimSpace(c.RuntimeProfile)
	if profile == "" {
		profile = "sustained"
	}
	base := []string{
		"quickstart",
		"--model", modelPath,
		"--mtp",
		"--depth", "1",
		"--profile", profile,
		"--fan-mode", "default",
		"--host", c.Host,
		"--port", strconv.Itoa(c.Port),
		"--model-id", c.ServedName,
		"--scheduler-mode", "serial",
		"--batching-preset", "solo",
		"--max-tokens", strconv.Itoa(c.MaxTokens),
		"--default-temperature", "0",
		"--default-top-p", "0.95",
		"--default-top-k", "20",
		"--reasoning", "off",
		"--warmup-tokens", "16",
		"--no-stats-footer",
		"--yes",
	}
	return mergeArgs(base, c.ExtraArgs)
}
