package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LaunchSettings mirrors the optional settings.json "modelServer" object —
// the user's hand-tuned local-runtime launch recipe. When present it beats the
// modelId-derived recipe, so the daemon spawns the exact server the user
// would have started by hand (qat weights, kv quantization, batching, …).
//
//	"modelServer": {
//	  "backend": "vllm-mlx",
//	  "binPath": "/opt/homebrew/bin/vllm-mlx",
//	  "modelPath": "~/Desktop/LLM/gemma-4-26b-a4b-qat-mlx",
//	  "servedModelName": "Meadow 26B",
//	  "profile": "turbo",
//	  "maxTokens": 16384,
//	  "warmPrompts": "~/.meadow/cache-warm-prompts.json",
//	  "extraArgs": ["--continuous-batching", "--max-num-seqs", "2", …]
//	}
//
// host/port should agree with the provider baseUrl; the endpoint wins on
// conflict because that is where the client (and health probe) connects.
type LaunchSettings struct {
	BinPath         string   `json:"binPath"`
	ModelPath       string   `json:"modelPath"`
	ServedModelName string   `json:"servedModelName"`
	Backend         string   `json:"backend"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	MaxTokens       int      `json:"maxTokens"`
	Profile         string   `json:"profile"`
	WarmPrompts     string   `json:"warmPrompts"`
	ExtraArgs       []string `json:"extraArgs"`
}

// ParseLaunchSettings decodes the raw settings.json "modelServer" value as
// loaded by core/session (JSON numbers arrive as float64). Returns nil when
// the object is absent or carries nothing usable. extraArgs entries may be
// numbers or bools — they stringify the way the shell command would.
func ParseLaunchSettings(raw map[string]any) *LaunchSettings {
	if len(raw) == 0 {
		return nil
	}
	str := func(key string) string {
		v, _ := raw[key].(string)
		return v
	}
	num := func(key string) int {
		switch v := raw[key].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return 0
	}
	l := &LaunchSettings{
		BinPath:         expandHome(str("binPath")),
		ModelPath:       expandHome(str("modelPath")),
		ServedModelName: str("servedModelName"),
		Backend:         strings.ToLower(strings.TrimSpace(str("backend"))),
		Host:            str("host"),
		Port:            num("port"),
		MaxTokens:       num("maxTokens"),
		Profile:         strings.ToLower(strings.TrimSpace(str("profile"))),
		WarmPrompts:     expandHome(str("warmPrompts")),
	}
	if arr, ok := raw["extraArgs"].([]any); ok {
		for _, v := range arr {
			l.ExtraArgs = append(l.ExtraArgs, argString(v))
		}
	}
	if l.BinPath == "" && l.ModelPath == "" && l.ServedModelName == "" && l.Backend == "" &&
		l.Host == "" && l.Port == 0 && l.MaxTokens == 0 && l.Profile == "" &&
		l.WarmPrompts == "" && len(l.ExtraArgs) == 0 {
		return nil
	}
	return l
}

// Matches reports whether modelID names the model this recipe launches —
// by model directory path or by served name. A nil recipe matches nothing.
// Callers use it to drop the recipe when the user explicitly picked a
// different model (CLI -model flag, desktop model picker).
func (l *LaunchSettings) Matches(modelID string) bool {
	if l == nil {
		return false
	}
	return (l.ModelPath != "" && modelID == l.ModelPath) ||
		(l.ServedModelName != "" && modelID == l.ServedModelName)
}

func argString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// expandHome resolves a leading ~ the way the shell would have when the
// user typed the launch command by hand.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// mergeArgs overlays extra onto the base argv: an extra flag replaces the
// same base flag (and its values) in place, new flags append at the end, and
// positional tokens keep their spot. So extraArgs may carry just the tuning
// flags or a paste of the full hand command — either way no flag duplicates.
func mergeArgs(base, extra []string) []string {
	isFlag := func(s string) bool { return strings.HasPrefix(s, "--") }

	// Group extra into per-flag clusters (flag token + its value tokens).
	clusters := map[string][]string{}
	var order, lead []string
	cur := ""
	for _, a := range extra {
		if isFlag(a) {
			cur = a
			if _, seen := clusters[cur]; !seen {
				order = append(order, cur)
			}
		} else if cur == "" {
			lead = append(lead, a) // positional in extra: keep, append last
		}
		if cur != "" {
			clusters[cur] = append(clusters[cur], a)
		}
	}

	consumed := map[string]bool{}
	var out []string
	for i := 0; i < len(base); i++ {
		a := base[i]
		if !isFlag(a) {
			out = append(out, a)
			continue
		}
		if c, ok := clusters[a]; ok {
			if !consumed[a] {
				out = append(out, c...)
				consumed[a] = true
			}
			for i+1 < len(base) && !isFlag(base[i+1]) {
				i++ // skip the overridden base values
			}
			continue
		}
		out = append(out, a)
	}
	for _, f := range order {
		if !consumed[f] {
			out = append(out, clusters[f]...)
		}
	}
	return append(out, lead...)
}
