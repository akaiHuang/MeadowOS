package provider

import "strings"

// ModelProfile is the internal operating config for one served model — the
// single source of truth that maps a Meadow-branded served name to the
// transport and parser quirks of its underlying weights.
//
// Branding stays at the served name; the base family and the
// vllm-mlx parser flags live here, internal to the core. The same table is the
// home for the launch-side parser/path mapping the serve script consumes, so a
// new model is described in exactly one place.
type ModelProfile struct {
	Name string // served-model-name, e.g. "Meadow 35B"
	// Family is the internal base architecture ("gemma4" | "qwen3"). Used to
	// pick parsers and transport; never surfaced to the user.
	Family string
	// ToolParser / ReasoningParser are the vllm-mlx --tool-call-parser /
	// --reasoning-parser flags this model is served with.
	ToolParser      string
	ReasoningParser string
	// Backend identifies a runtime whose wire behavior differs from the base
	// architecture. Keep these quirks capability-scoped: an MTPLX-served Qwen
	// can stream native OpenAI tool-call deltas, while other Qwen servers still
	// need the non-streaming repair path.
	Backend string
	// Streaming is false only for profiles whose current SSE path cannot carry
	// native tool calls intact. Meadow 35B's current qwen3_coder server emits
	// complete OpenAI tool-call deltas and is therefore streaming-capable;
	// older/generic Qwen profiles retain the repair-oriented non-stream path
	// until their own wire contracts are proven.
	Streaming bool
	// Multimodal marks MLX-VLM/vllm-mlx MLLM artifacts. These need --mllm at
	// launch and request-level thinking suppression because the MLLM path does
	// not currently merge default_chat_template_kwargs into enable_thinking.
	Multimodal bool
	// ThinkingControl and ReasoningEffortLevels describe request controls that
	// were verified against the actual shipped template/runtime. A reasoning
	// parser alone is not evidence that either control changes model behavior.
	ThinkingControl       bool
	ReasoningEffortLevels int
}

// modelProfiles is the registry. Add a row to onboard a model; nothing else in
// the core needs to learn its name.
var modelProfiles = []ModelProfile{
	{Name: "MiniCPM-V 4.7 35B-A3B", Family: "minicpm", ToolParser: "qwen3_coder", ReasoningParser: "qwen3", Streaming: true, Multimodal: true, ThinkingControl: true, ReasoningEffortLevels: 5},
	{Name: "Meadow Coder 35B", Family: "ornith", ToolParser: "qwen3_coder", ReasoningParser: "qwen3", Streaming: true, Multimodal: true, ThinkingControl: true, ReasoningEffortLevels: 5},
	{Name: "Ornith Coding Agent", Family: "ornith", ToolParser: "qwen3_coder", ReasoningParser: "qwen3", Streaming: true, Multimodal: true, ThinkingControl: true, ReasoningEffortLevels: 5},
	{Name: "Ornith-1.5-35B-A3B", Family: "ornith", ToolParser: "qwen3_coder", ReasoningParser: "qwen3", Streaming: true, Multimodal: true, ThinkingControl: true, ReasoningEffortLevels: 5},
	{Name: "Ornith-1.0-35B", Family: "ornith", ToolParser: "qwen3_coder", ReasoningParser: "qwen3", Streaming: true, Multimodal: true, ThinkingControl: true, ReasoningEffortLevels: 5},
	// Explicit legacy id used only for reproducible comparisons with the
	// replaced Qwen3.6 Heretic weights. Product aliases below must not select
	// this profile now that the public 35B row is Ornith.
	{Name: "Qwen3.6 35B Legacy", Family: "qwen3", ToolParser: "qwen3_coder", Streaming: true},
	{Name: "Meadow 30B", Family: "qwen3", ToolParser: "qwen3_coder", ReasoningParser: "nemotron_3", Backend: "mlx-vlm", Streaming: true, Multimodal: true, ThinkingControl: true, ReasoningEffortLevels: 5},
	{Name: "Nemotron 3 Nano Omni 30B-A3B", Family: "qwen3", ToolParser: "qwen3_coder", ReasoningParser: "nemotron_3", Backend: "mlx-vlm", Streaming: true, Multimodal: true, ThinkingControl: true, ReasoningEffortLevels: 5},
	{Name: "Muse Glimmer 30B", Family: "muse", ToolParser: "muse", ReasoningParser: "muse", Streaming: true},
	{Name: "qwen36-35b-mtplx", Family: "qwen3", ToolParser: "mtplx", Backend: "mtplx", Streaming: true},
	{Name: "Meadow 9B Vision", Family: "qwen3", ToolParser: "qwen3_coder", Streaming: false, Multimodal: true},
	{Name: "Meadow 9B", Family: "qwen3", ToolParser: "qwen3_coder", Streaming: false},
	{Name: "Meadow 12B", Family: "gemma4", ToolParser: "gemma4", Streaming: true},
	{Name: "Meadow 26B", Family: "gemma4", ToolParser: "gemma4", ReasoningParser: "gemma4", Streaming: true},
	// The current Meadow 27B package is launched through the MLX-VLM path and
	// accepts native image inputs. Keep the raw Qwen3.8 alias text-only unless
	// its independently supplied artifact also declares a vision tower.
	{Name: "Meadow 27B", Family: "qwen3", ToolParser: "qwen3_coder", ReasoningParser: "qwen3", Streaming: true, Multimodal: true},
	{Name: "Qwen3.8-27B", Family: "qwen3", ToolParser: "qwen3_coder", ReasoningParser: "qwen3", Streaming: true},
	// Existing sessions may persist either retired public label. Both resolve
	// to the current coding product and must receive Ornith's wire contract.
	{Name: "Meadow Q", Family: "ornith", ToolParser: "qwen3_coder", ReasoningParser: "qwen3", Streaming: true, Multimodal: true, ThinkingControl: true},
	{Name: "Meadow 35B", Family: "ornith", ToolParser: "qwen3_coder", ReasoningParser: "qwen3", Streaming: true, Multimodal: true, ThinkingControl: true},
	{Name: "Meadow Lightning⚡️", Family: "lfm2.5", ToolParser: "lfm2", Streaming: true},
	{Name: "Meadow Lightning", Family: "lfm2.5", ToolParser: "lfm2", Streaming: true},
}

// IsMTPLXModel reports whether the served id explicitly selects the MTPLX
// runtime. It deliberately does not match generic Meadow 35B/Qwen ids: those
// may still be served by vllm-mlx and must retain their existing transport.
func IsMTPLXModel(model string) bool {
	prof, ok := ProfileForModel(model)
	return ok && prof.Backend == "mtplx"
}

// RequiresFlattenedToolHistory reports whether the model needs Gemma-4's
// control-token history workaround. Feeding that private format to Qwen/MTPLX
// forces the server to rewrite every historical tool turn and can route valid
// calls through its parser fallback path.
func RequiresFlattenedToolHistory(model string) bool {
	if prof, ok := ProfileForModel(model); ok {
		return prof.Family == "gemma4"
	}
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(m, "gemma-4") || strings.Contains(m, "gemma4") ||
		strings.Contains(m, "gemma_4")
}

// SuppressThoughtDisplay reports production profiles whose native thinking is
// disabled. Wire-level thought markers may still leak from a model/template;
// clients must discard that private channel instead of presenting it as UI.
func SuppressThoughtDisplay(model string) bool {
	profile, ok := ProfileForModel(model)
	if !ok {
		return false
	}
	if profile.Family == "deepseek" {
		return true
	}
	switch profile.Name {
	case "Meadow 12B", "Meadow 26B", "Meadow 27B", "Qwen3.8-27B", "Qwen3.6 35B Legacy", "Muse Glimmer 30B", "Meadow Lightning⚡️", "Meadow Lightning":
		return true
	default:
		return false
	}
}

// IsDeepSeekModel matches the native API contract, not local DeepSeek weights.
func IsDeepSeekModel(model string) bool {
	profile, ok := ProfileForModel(model)
	return ok && profile.Family == "deepseek"
}

// requiresUnsolicitedThoughtGuard limits a local-template compatibility rule
// to the families it was written for. A native API may legitimately reason
// in Auto or its lowest effort, even when the UI hides that channel.
func requiresUnsolicitedThoughtGuard(model string) bool {
	profile, known := ProfileForModel(model)
	if !known {
		return false
	}
	switch profile.Family {
	case "ornith", "qwen3", "gemma4", "muse", "lfm2.5":
		return true
	default:
		return false
	}
}

// IsOrnithModel identifies the isolated 35B coding-agent family. It must not
// inherit the old Office35 thought suppression or Qwen sampling policy.
func IsOrnithModel(model string) bool {
	profile, ok := ProfileForModel(model)
	return ok && profile.Family == "ornith"
}

// IsNemotronModel identifies the current Meadow 30B MoE artifact.  Keep this
// separate from the retired Muse Glimmer alias: both occupy the same product
// tier, but their native tool grammar and useful context budgets differ.
func IsNemotronModel(model string) bool {
	profile, ok := ProfileForModel(model)
	if ok {
		return profile.ReasoningParser == "nemotron_3"
	}
	normalized := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(normalized, "nemotron")
}

// IsQwen38Model identifies the independently tuned Qwen3.8 27B coding
// product. It must not inherit the older Qwen3.6 office sampling profile.
func IsQwen38Model(model string) bool {
	profile, ok := ProfileForModel(model)
	if ok && (profile.Name == "Meadow 27B" || profile.Name == "Qwen3.8-27B") {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(normalized, "qwen3.8") || strings.Contains(normalized, "qwen3_8")
}

// IsGemma12Model is the compatibility-named predicate for Meadow's Gemma 4
// agent pipeline. The public 26B-A4B replacement shares Gemma's wire/history
// contract with the retired 12B model, so both intentionally select it.
func IsGemma12Model(model string) bool {
	profile, ok := ProfileForModel(model)
	if ok && profile.Family == "gemma4" {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(normalized, "gemma-4-12b") ||
		strings.Contains(normalized, "gemma4-12b") ||
		strings.Contains(normalized, "gemma_4_12b") ||
		strings.Contains(normalized, "gemma-4-26b") ||
		strings.Contains(normalized, "gemma4-26b") ||
		strings.Contains(normalized, "gemma_4_26b")
}

// qwen3FallbackProfile covers a qwen3-family model served under a raw or
// path-shaped id that does not carry the Meadow brand — it still needs the
// non-streaming treatment, so size/family hints route it here.
func qwen3FallbackProfile(model string) ModelProfile {
	return ModelProfile{
		Name:       model,
		Family:     "qwen3",
		ToolParser: "qwen3_coder",
		Streaming:  false,
		Multimodal: looksMultimodalModelID(model),
	}
}

func looksMultimodalModelID(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(m, "vision") ||
		strings.Contains(m, "vlm") ||
		strings.Contains(m, "mllm") ||
		strings.Contains(m, "mmproj")
}

// ProfileForModel resolves a served model id to its profile. Matching is
// case-insensitive and substring-tolerant so a brand name still matches when
// the server echoes a path-suffixed id. ok is false (with a streaming default)
// when nothing in the registry and no qwen3 hint matches — an unknown model is
// treated as a clean-streaming Gemma-style model.
func ProfileForModel(model string) (ModelProfile, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ModelProfile{Streaming: true}, false
	}
	// xAI chat models use native OpenAI tool calls and image input. Do not
	// apply a local model's template/parser/sampling overrides to them.
	switch m {
	case "deepseek-flash", "deepseek-v4-pro":
		return ModelProfile{Name: model, Family: "deepseek", Streaming: true, Multimodal: m == "deepseek-flash", ThinkingControl: true, ReasoningEffortLevels: 5}, true
	case "grok-4.7", "grok-4.6", "grok-4.5", "grok-4.3", "grok-build-0.1":
		return ModelProfile{Name: model, Family: "xai", Streaming: true, Multimodal: true, ThinkingControl: m != "grok-build-0.1", ReasoningEffortLevels: func() int {
			if m == "grok-build-0.1" {
				return 0
			}
			return 5
		}()}, true
	}
	for _, p := range modelProfiles {
		if strings.Contains(m, strings.ToLower(p.Name)) {
			return p, true
		}
	}
	if strings.Contains(m, "lfm2.5") || strings.Contains(m, "lfm-2.5") {
		return ModelProfile{Name: model, Family: "lfm2.5", ToolParser: "lfm2", Streaming: true}, true
	}
	if strings.Contains(m, "27b") || strings.Contains(m, "35b") ||
		strings.Contains(m, "9b") ||
		strings.Contains(m, "qwen3") || strings.Contains(m, "qwen3.5") ||
		strings.Contains(m, "qwen3_5") || strings.Contains(m, "qwythos") ||
		strings.Contains(m, "claude-mythos") {
		return qwen3FallbackProfile(model), true
	}
	return ModelProfile{Name: model, Streaming: true}, false
}
