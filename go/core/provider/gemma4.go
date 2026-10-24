package provider

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Defaults mirroring applyMeadowLocalDefaults in provider/mlx.ts.
const (
	DefaultBaseURL = "http://localhost:8080/v1"
	DummyAPIKey    = "meadow-local"
)

// Gemma-4 local safety values from provider/mlx.ts lines 46-54.
const (
	gemma4Temperature            = 0.2
	gemma4ActionTemperature      = 0.1
	gemma4RepetitionPenalty      = 1.12
	gemma4DefaultMaxTokens       = 4096
	qwenDefaultMaxTokens         = 4096
	qwenNativeTemperature        = 0.7
	qwenNativeTopP               = 0.8
	qwenNativeTopK               = 20
	qwenNativePresence           = 1.5
	qwenNativeRepetition         = 1.0
	nemotron30Temperature        = 0.6
	nemotron30TopP               = 0.95
	nemotron30Presence           = 0.0
	nemotron30Repetition         = 1.0
	nemotron30DefaultMaxTokens   = 8192
	qwenAuditTemperature         = 0.55
	qwenAuditRecoveryTemperature = 0.75
	qwenRepairTemperature        = 0.15
	qwenRecoveryTemperature      = 0.45
	qwenFinalTemperature         = 0.2
	qwenLiteralTemperature       = 0.0
	qwenLiteralTopP              = 1.0
	qwenLiteralTopK              = 1
	qwenLiteralMaxTokens         = 2048
	qwenPrecisionPresence        = 0.0
	qwenAuditPresence            = 0.2
	qwenAuditRecoveryPresence    = 0.5
	qwenAuditMaxTokens           = 1536
	qwenAuditRecoveryMaxTokens   = 1024
	qwenRepairMaxTokens          = 2048
	qwenRecoveryMaxTokens        = 2048
	qwenFinalMaxTokens           = 1024
	museSolveTemperature         = 0.2
	// Real model probes showed 0.1 suppresses Muse's native tool call even
	// when the registry contains only typescript_web_build. Keep the proven
	// 0.2 candidate distribution and bound recovery by output tokens instead.
	musePrecisionTemperature = 0.2
	museDefaultMaxTokens     = 4096
	museAuditMaxTokens       = 1024
	museRepairMaxTokens      = 2048
	museRecoveryMaxTokens    = 2048
	museInteractionMaxTokens = 1024
	museFinalMaxTokens       = 512
	lfm25Temperature         = 0.1
	lfm25TopK                = 50
	lfm25RepetitionPenalty   = 1.1
	lfm25DefaultMaxTokens    = 2048
	lfm25CheckpointMaxTokens = 3072
	ornithTemperature        = 0.6
	ornithTopP               = 0.95
	ornithTopK               = 20
	ornithRepetitionPenalty  = 1.0
	ornithDefaultMaxTokens   = 8192
	qwen38Temperature        = 1.0
	qwen38TopP               = 0.95
	qwen38TopK               = 20
	qwen38DefaultMaxTokens   = 2048
	qwen38WriteMaxTokens     = 8192
	qwen38FinalMaxTokens     = 1024
	mtplxChatMaxTokens       = 2048
	mtplxActionMaxTokens     = 512
	mtplxEditMaxTokens       = 1024
	mtplxWriteMaxTokens      = 8192
	qwythosTemperature       = 0.6
	qwythosTopP              = 0.95
	qwythosTopK              = 20
	qwythosRepetitionPenalty = 1.05
	qwythosDefaultMaxTokens  = 8192
)

const (
	qwenSamplingLegacy         = "legacy"
	qwenSamplingNativeParallel = "native-parallel"
	qwenSamplingNativeSerial   = "native-serial"
)

// defaultStopSequences mitigate Gemma-4/Meadow-2B token bleeding into
// chat-template control tokens.
var defaultStopSequences = []string{
	"<|im_end|>",
	"<|im_start|>",
	"<|channel|>",
	"<|channel>",
	"<channel|>",
	"\nuser\n",
	"\nassistant\n",
}

// Matches the raw checkpoint ids (gemma-4-*) and the Meadow-branded served
// names ("Meadow 26B" / "Meadow 12B" …) — the desktop/CLI resolve the
// server's --served-model-name, so brand names MUST route as local Gemma 4
// or the router/profile/safety chain silently turns off (M1-gate 12B bug).
var gemma4ModelRe = regexp.MustCompile(`(?i)gemma[-_]?4|gemma4|gemma-4|meadow`)

func IsLocalGemma4Model(model string) bool {
	if profile, ok := ProfileForModel(model); ok {
		return profile.Family == "gemma4"
	}
	return gemma4ModelRe.MatchString(model)
}

func IsMeadowLocalModel(model string) bool {
	if IsLocalGemma4Model(model) {
		return true
	}
	if prof, ok := ProfileForModel(model); ok {
		return prof.Family == "gemma4" || prof.Family == "qwen3"
	}
	return false
}

func envInt(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func envFloat(name string) (float64, bool) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil
}

func qwenSamplingProfile() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_QWEN_SAMPLING_PROFILE"))) {
	case qwenSamplingLegacy, "current":
		return qwenSamplingLegacy
	case qwenSamplingNativeParallel:
		return qwenSamplingNativeParallel
	case qwenSamplingNativeSerial:
		return qwenSamplingNativeSerial
	case "native", "production", "":
		return qwenSamplingNativeParallel
	default:
		return qwenSamplingNativeParallel
	}
}

// QwenParallelToolCallsEnabled keeps the model-facing parallel_tool_calls flag
// and the executor's scheduling policy aligned. Production Qwen keeps parallel
// tool-call generation enabled because the local checkpoint's native tool
// template becomes unreliable when the flag is false. The executor may schedule
// only calls the model emitted in the same response; dependent calls remain
// separate model turns. Serial remains available as an explicit diagnostic
// profile.
func QwenParallelToolCallsEnabled(model string) bool {
	profile, ok := ProfileForModel(model)
	if !ok || profile.Family != "qwen3" || IsQwythosModel(model) {
		return false
	}
	switch qwenSamplingProfile() {
	case qwenSamplingLegacy, qwenSamplingNativeParallel:
		return true
	default:
		return false
	}
}

func gemma4MaxTokens() int { return envInt("MEADOW_LOCAL_MAX_TOKENS", gemma4DefaultMaxTokens) }
func gemma4ActionMaxTokens() int {
	return envInt("MEADOW_LOCAL_ACTION_MAX_TOKENS", gemma4DefaultMaxTokens)
}
func gemma4StaticWebWriteMaxTokens() int {
	return envInt("MEADOW_LOCAL_STATIC_WEB_WRITE_MAX_TOKENS", gemma4DefaultMaxTokens)
}
func qwenMaxTokens() int { return envInt("MEADOW_QWEN_MAX_TOKENS", qwenDefaultMaxTokens) }
func museMaxTokens() int { return envInt("MEADOW_MUSE_MAX_TOKENS", museDefaultMaxTokens) }
func qwythosMaxTokens() int {
	return envInt("MEADOW_QWYTHOS_MAX_TOKENS", qwythosDefaultMaxTokens)
}

func IsMuseModel(model string) bool {
	profile, ok := ProfileForModel(model)
	return ok && profile.Family == "muse"
}

func IsLFM25Model(model string) bool {
	profile, ok := ProfileForModel(model)
	return ok && profile.Family == "lfm2.5"
}

func IsQwythosModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(m, "meadow 9b") ||
		strings.Contains(m, "meadow-9b") ||
		strings.Contains(m, "meadow_9b") ||
		strings.Contains(m, "qwythos") ||
		strings.Contains(m, "claude-mythos")
}

func IsQwythosMultimodalModel(model string) bool {
	if prof, ok := ProfileForModel(model); ok && prof.Multimodal {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(model))
	return IsQwythosModel(model) && (strings.Contains(m, "vision") ||
		strings.Contains(m, "vlm") ||
		strings.Contains(m, "mllm") ||
		strings.Contains(m, "mmproj"))
}

func LocalModelMaxTokens(model string) int {
	if prof, ok := ProfileForModel(model); ok && prof.Family == "qwen3" {
		if IsQwythosModel(model) {
			return qwythosMaxTokens()
		}
		return qwenMaxTokens()
	}
	return gemma4MaxTokens()
}

func hasSystemPromptFragment(msgs []Message, fragment string) bool {
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, fragment) {
			return true
		}
	}
	return false
}

func hasPromptFragment(msgs []Message, fragment string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, fragment) {
			return true
		}
	}
	return false
}

func forcedToolChoiceName(toolChoice any) string {
	tc, ok := toolChoice.(map[string]any)
	if !ok {
		return ""
	}
	fn, ok := tc["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

func actionMaxTokenCeiling(req *ChatRequest) int {
	if forcedToolChoiceName(req.ToolChoice) == "write_file" &&
		hasSystemPromptFragment(req.Messages, "Static web staged write") {
		return gemma4StaticWebWriteMaxTokens()
	}
	return gemma4ActionMaxTokens()
}

// webIntentRe mirrors the deterministic web_search trigger in the router. Local
// models often answer plausible stale facts from memory unless clearly forced.
var webIntentRe = regexp.MustCompile(`(?i)查|搜尋|搜一下|搜索|查詢|查一下|上網|最新|最近|今天|今日|今年|現任|即時|新聞|價格|價錢|股價|股票|天氣|氣象|匯率|跑分|分數|評測|評價|排名|賽程|發布|釋出|上市|法規|法律|政策|google|benchmark|20(2[5-9]|[3-9][0-9])年|\b(search|latest|recent|today|news|weather|stock|price|pricing|exchange rate|benchmark|score|ranking|schedule|release|released|version|law|regulation|policy|20(2[5-9]|[3-9][0-9]))\b`)

func lastUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && !msgs[i].RuntimeObservation {
			return MessageText(msgs[i])
		}
	}
	return ""
}

func forceWebSearchOnIntent(req *ChatRequest) {
	if os.Getenv("MEADOW_FORCE_WEBSEARCH") == "0" || req.ToolChoice != nil {
		return
	}
	hasWebSearch := false
	for _, t := range req.Tools {
		if t.Function.Name == "web_search" {
			hasWebSearch = true
			break
		}
	}
	if hasWebSearch && webIntentRe.MatchString(lastUserText(req.Messages)) {
		req.ToolChoice = map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "web_search"},
		}
	}
}

// ApplyGemma4SafetyOverrides ports applyLocalGemma4SafetyOverrides: cap
// max_tokens and temperature (action requests — tools plus an explicit
// tool_choice — get the tighter caps), default the repetition penalty, and
// force web_search on clear live-web intent.
func ApplyGemma4SafetyOverrides(req *ChatRequest) {
	isAction := len(req.Tools) > 0 && req.ToolChoice != nil
	if SuppressThoughtDisplay(req.Model) {
		// Gemma 4's explicit thought path is not a useful private scratchpad in
		// the current MLX template: it can consume the whole output budget without
		// producing a tool call. Keep both wire forms aligned because different
		// server templates honor different fields.
		disabled := false
		req.EnableThinking = &disabled
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = map[string]any{}
		}
		req.ChatTemplateKwargs["enable_thinking"] = false
		req.ChatTemplateKwargs["preserve_thinking"] = false
	}

	ceiling := LocalModelMaxTokens(req.Model)
	if isAction {
		ceiling = actionMaxTokenCeiling(req)
	}
	if req.MaxTokens == nil || *req.MaxTokens > ceiling {
		c := ceiling
		req.MaxTokens = &c
	}

	if forced := os.Getenv("MEADOW_FORCE_TEMP"); forced != "" {
		// SSD self-sampling override — bypass the cap to sample diverse
		// rollouts; unset means production capping below. Doubles as the
		// A/B knob for testing Gemma 4's recommended temp=1.0 against our
		// loop-suppressing 0.1/0.2 ceilings.
		if t, err := strconv.ParseFloat(forced, 64); err == nil {
			req.Temperature = &t
		}
	} else {
		tCeiling := gemma4Temperature
		if isAction || req.ProtocolRecovery {
			tCeiling = gemma4ActionTemperature
		}
		if req.Temperature == nil || *req.Temperature > tCeiling {
			t := tCeiling
			req.Temperature = &t
		}
	}

	// Gemma 4's model card recommends top_p=0.95, top_k=64 in all uses.
	// We leave these to the server default in production but expose env
	// overrides so the recommended sampling can be A/B'd against ours.
	if v := os.Getenv("MEADOW_FORCE_TOP_P"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			req.TopP = &f
		}
	}
	if v := os.Getenv("MEADOW_FORCE_TOP_K"); v != "" {
		if k, err := strconv.Atoi(v); err == nil {
			req.TopK = &k
		}
	}

	if req.RepetitionPenalty == nil {
		rp := gemma4RepetitionPenalty
		req.RepetitionPenalty = &rp
	}
	if forced, ok := envFloat("MEADOW_FORCE_REPETITION_PENALTY"); ok {
		req.RepetitionPenalty = &forced
	}

	forceWebSearchOnIntent(req)
}

// ApplyQwenSafetyOverrides keeps Qwen-family Meadow models inside the tighter
// vllm-mlx serving limits used by the 27B/35B MLX builds. The engine may ask
// for an 8K action budget for Gemma, but a Qwen server capped at 4K rejects the
// request before the model can emit a tool call.
func ApplyQwenSafetyOverrides(req *ChatRequest) {
	if IsQwythosModel(req.Model) {
		ApplyQwythosSafetyOverrides(req)
		forceWebSearchOnIntent(req)
		return
	}
	isAction := len(req.Tools) > 0 && req.ToolChoice != nil

	ceiling := qwenMaxTokens()
	if IsMTPLXModel(req.Model) {
		ceiling = mtplxMaxTokenCeiling(req)
	}
	if IsMTPLXModel(req.Model) || SuppressThoughtDisplay(req.Model) {
		// Keep streaming Qwen requests aligned with the non-thinking launch
		// profile. The server accepts both forms and some chat-template paths
		// only honor the nested value, so make the wire contract explicit.
		if req.EnableThinking == nil {
			disabled := false
			req.EnableThinking = &disabled
		}
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = map[string]any{}
		}
		if _, exists := req.ChatTemplateKwargs["enable_thinking"]; !exists {
			req.ChatTemplateKwargs["enable_thinking"] = *req.EnableThinking
		}
	}
	if req.MaxTokens == nil || *req.MaxTokens > ceiling {
		c := ceiling
		req.MaxTokens = &c
	}

	switch qwenSamplingProfile() {
	case qwenSamplingLegacy:
		tCeiling := gemma4Temperature
		if isAction {
			tCeiling = gemma4ActionTemperature
		}
		if req.Temperature == nil || *req.Temperature > tCeiling {
			t := tCeiling
			req.Temperature = &t
		}
		if req.RepetitionPenalty == nil {
			rp := qwenNativeRepetition
			req.RepetitionPenalty = &rp
		}
	default:
		temperature := qwenNativeTemperature
		topP := qwenNativeTopP
		topK := qwenNativeTopK
		presence := qwenNativePresence
		if req.ProtocolRecovery {
			presence = 0
		}
		repetition := qwenNativeRepetition
		req.Temperature = &temperature
		req.TopP = &topP
		req.TopK = &topK
		req.PresencePenalty = &presence
		req.RepetitionPenalty = &repetition
		parallel := QwenParallelToolCallsEnabled(req.Model)
		req.ParallelToolCalls = &parallel
	}
	applyQwenPhaseSampling(req)

	if forced, ok := envFloat("MEADOW_FORCE_TEMP"); ok {
		req.Temperature = &forced
	}
	if forced, ok := envFloat("MEADOW_FORCE_TOP_P"); ok {
		req.TopP = &forced
	}
	if value := strings.TrimSpace(os.Getenv("MEADOW_FORCE_TOP_K")); value != "" {
		if forced, err := strconv.Atoi(value); err == nil {
			req.TopK = &forced
		}
	}
	if forced, ok := envFloat("MEADOW_FORCE_PRESENCE_PENALTY"); ok {
		req.PresencePenalty = &forced
	}
	if forced, ok := envFloat("MEADOW_FORCE_REPETITION_PENALTY"); ok {
		req.RepetitionPenalty = &forced
	}
	// Meadow 35B's qwen35-office-v1 harness is model-led: the provider may
	// shape sampling and transport, but it must not choose a semantic tool.
	// The Qwen office prompt requires network evidence for actual web tasks.
	if !modelOwnsWebToolChoice(req.Model) {
		forceWebSearchOnIntent(req)
	}
}

// ApplyNemotron30SafetyOverrides follows the shipped checkpoint's own
// generation_config instead of inheriting Qwen3.6's much narrower top-k and
// high presence penalty. Nemotron's chat template defaults reasoning on; auto
// mode must preserve that default so the 30B agent does not silently lose its
// planning pass before a tool call. Explicit user controls are applied later.
func ApplyNemotron30SafetyOverrides(req *ChatRequest) {
	ceiling := envInt("MEADOW_NEMOTRON30_MAX_TOKENS", nemotron30DefaultMaxTokens)
	if req.MaxTokens == nil || *req.MaxTokens > ceiling {
		limit := ceiling
		req.MaxTokens = &limit
	}
	temperature := nemotron30Temperature
	topP := nemotron30TopP
	presence := nemotron30Presence
	repetition := nemotron30Repetition
	req.Temperature = &temperature
	req.TopP = &topP
	// The checkpoint does not publish a top_k override. Do not inherit the
	// Qwen3.6 product default of 20, which removes useful code candidates.
	req.TopK = nil
	req.PresencePenalty = &presence
	req.RepetitionPenalty = &repetition
	parallel := QwenParallelToolCallsEnabled(req.Model)
	req.ParallelToolCalls = &parallel

	if forced, ok := envFloat("MEADOW_FORCE_TEMP"); ok {
		req.Temperature = &forced
	}
	if forced, ok := envFloat("MEADOW_FORCE_TOP_P"); ok {
		req.TopP = &forced
	}
	if value := strings.TrimSpace(os.Getenv("MEADOW_FORCE_TOP_K")); value != "" {
		if forced, err := strconv.Atoi(value); err == nil {
			req.TopK = &forced
		}
	}
	if forced, ok := envFloat("MEADOW_FORCE_PRESENCE_PENALTY"); ok {
		req.PresencePenalty = &forced
	}
	if forced, ok := envFloat("MEADOW_FORCE_REPETITION_PENALTY"); ok {
		req.RepetitionPenalty = &forced
	}
	if !modelOwnsWebToolChoice(req.Model) {
		forceWebSearchOnIntent(req)
	}
}

// applyQwenPhaseSampling keeps the model's native exploratory profile for the
// solve phase, then narrows only the controls that directly affect precision
// and runaway output in audit/repair/final turns. It intentionally leaves
// top_p, top_k, and repetition_penalty at the checkpoint's native defaults so
// the first A/B has one attributable change family. This remains opt-in until
// the held-out K3 A/B passes: set MEADOW_QWEN_PHASE_SAMPLING=1 for treatment;
// unset it (or set 0) for the fixed-profile control group.
func applyQwenPhaseSampling(req *ChatRequest) {
	// Literal-copy creation is explicitly selected by the shared router after
	// it has reduced the action surface to write_file. Greedy decoding removes
	// the exploration variance that otherwise invents fluent business details.
	// Keep this product precision envelope independent of the experimental
	// audit/repair phase flag below.
	if req.SamplingPhase == SamplingPhasePrecision {
		temperature := qwenLiteralTemperature
		topP := qwenLiteralTopP
		topK := qwenLiteralTopK
		presence := qwenPrecisionPresence
		req.Temperature = &temperature
		req.TopP = &topP
		req.TopK = &topK
		req.PresencePenalty = &presence
		if req.MaxTokens == nil || *req.MaxTokens > qwenLiteralMaxTokens {
			limit := qwenLiteralMaxTokens
			req.MaxTokens = &limit
		}
		return
	}
	if strings.TrimSpace(os.Getenv("MEADOW_QWEN_PHASE_SAMPLING")) != "1" {
		return
	}
	var temperature, presence float64
	var maxTokens int
	switch req.SamplingPhase {
	case SamplingPhaseAudit:
		temperature, presence, maxTokens = qwenAuditTemperature, qwenAuditPresence, qwenAuditMaxTokens
	case SamplingPhaseAuditRecovery:
		temperature, presence, maxTokens = qwenAuditRecoveryTemperature, qwenAuditRecoveryPresence, qwenAuditRecoveryMaxTokens
	case SamplingPhaseRepair:
		temperature, presence, maxTokens = qwenRepairTemperature, qwenPrecisionPresence, qwenRepairMaxTokens
	case SamplingPhaseRecovery:
		temperature, presence, maxTokens = qwenRecoveryTemperature, qwenPrecisionPresence, qwenRecoveryMaxTokens
	case SamplingPhaseFinal:
		temperature, presence, maxTokens = qwenFinalTemperature, qwenPrecisionPresence, qwenFinalMaxTokens
	default:
		return
	}
	req.Temperature = &temperature
	req.PresencePenalty = &presence
	if req.MaxTokens == nil || *req.MaxTokens > maxTokens {
		limit := maxTokens
		req.MaxTokens = &limit
	}
}

// ApplyMuseSafetyOverrides preserves Muse's proven solve profile while
// bounding audit, repair, and final turns. Muse is model-led: the Kernel
// narrows the exposed registry to one tool instead of serializing a named
// tool_choice. Consequently the action-only Gemma cap cannot identify a Muse
// repair turn; SamplingPhase is the authoritative structural signal here.
//
// The 2K repair ceiling is intentional. A live 1K probe exhausted two turns
// before Muse reached the required edit call; 2K still halves the generic 4K
// allowance while leaving enough hidden deliberation for one native action.
func ApplyMuseSafetyOverrides(req *ChatRequest) {
	maxTokens := museMaxTokens()
	temperature := museSolveTemperature
	switch req.SamplingPhase {
	case SamplingPhaseAudit, SamplingPhaseAuditRecovery:
		maxTokens = museAuditMaxTokens
		temperature = musePrecisionTemperature
	case SamplingPhaseRepair:
		maxTokens = museRepairMaxTokens
		temperature = musePrecisionTemperature
	case SamplingPhaseRecovery:
		maxTokens = museRecoveryMaxTokens
		temperature = musePrecisionTemperature
	case SamplingPhaseFinal:
		maxTokens = museFinalMaxTokens
		temperature = musePrecisionTemperature
	}
	// Kernel labels any single-tool stage as recovery, including the first
	// required write of a new artifact. A complete source file lives inside the
	// write_file JSON argument, so that stage must retain the solve allowance;
	// only short evidence and edit turns use the phase ceiling above.
	if requestHasTool(req, "write_file") || requestHasTool(req, "write_files") ||
		requestHasTool(req, "research_artifact") {
		maxTokens = museMaxTokens()
	} else if requestHasTool(req, "meadow_browser_interaction_check") {
		// Interaction calls contain a bounded declarative step list rather than
		// source code. The live 30B probes complete these calls in a few hundred
		// tokens; a 1K ceiling prevents a failed validator-recovery turn from
		// spending minutes on hidden prose while preserving normal validation.
		maxTokens = museInteractionMaxTokens
		// A real expert-guidance A/B showed that Muse can spend the full 1K
		// reconciling a newly accepted instruction with the locked validation
		// stage before it reaches recipient grammar. Reuse the existing 2K
		// recovery ceiling only for that boundary and its one compact retry.
		// This does not select steps or author validator arguments.
		if req.GuidanceAction {
			maxTokens = museRecoveryMaxTokens
		}
	}
	if req.MaxTokens == nil || *req.MaxTokens > maxTokens {
		limit := maxTokens
		req.MaxTokens = &limit
	}
	req.Temperature = &temperature
	if req.RepetitionPenalty == nil {
		repetition := gemma4RepetitionPenalty
		req.RepetitionPenalty = &repetition
	}
	if forced, ok := envFloat("MEADOW_FORCE_TEMP"); ok {
		req.Temperature = &forced
	}
	if forced, ok := envFloat("MEADOW_FORCE_TOP_P"); ok {
		req.TopP = &forced
	}
	if value := strings.TrimSpace(os.Getenv("MEADOW_FORCE_TOP_K")); value != "" {
		if forced, err := strconv.Atoi(value); err == nil {
			req.TopK = &forced
		}
	}
	if forced, ok := envFloat("MEADOW_FORCE_REPETITION_PENALTY"); ok {
		req.RepetitionPenalty = &forced
	}
	forceWebSearchOnIntent(req)
}

// ApplyLFM25SafetyOverrides preserves LFM2.5's native Pythonic tool-call
// format. Ordinary turns use auto when no choice is supplied. A current
// MLX/SGLang runtime also parses required/named checkpoints natively, so an
// explicit Kernel checkpoint choice must be preserved rather than silently
// weakened back to auto.
//
// Sampling follows Liquid AI's published LFM2.5 defaults for tool use and
// grounded extraction. Environment overrides remain available for controlled
// A/B tests, but the product path starts from the checkpoint's native profile.
func ApplyLFM25SafetyOverrides(req *ChatRequest) {
	checkpointRequired := len(req.Tools) == 1 &&
		(req.ToolChoice == "required" || forcedToolChoiceName(req.ToolChoice) != "")
	if req.MaxTokens == nil {
		limit := lfm25DefaultMaxTokens
		req.MaxTokens = &limit
	} else if checkpointRequired && *req.MaxTokens > lfm25CheckpointMaxTokens {
		limit := lfm25CheckpointMaxTokens
		req.MaxTokens = &limit
	} else if !checkpointRequired && *req.MaxTokens > lfm25DefaultMaxTokens {
		limit := lfm25DefaultMaxTokens
		req.MaxTokens = &limit
	}
	temperature := lfm25Temperature
	topK := lfm25TopK
	repetition := lfm25RepetitionPenalty
	parallel := false
	req.Temperature = &temperature
	req.TopK = &topK
	req.RepetitionPenalty = &repetition
	req.ParallelToolCalls = &parallel
	if req.SamplingPhase == SamplingPhaseFinal {
		// Meadow's LFM template preserves native thinking for tool use but can
		// pre-close the channel for a grounded final draft. Send both fields:
		// SGLang versions differ in which OpenAI-compatible form they honor.
		disabled := false
		if req.EnableThinking == nil {
			req.EnableThinking = &disabled
		}
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = map[string]any{}
		}
		if _, exists := req.ChatTemplateKwargs["enable_thinking"]; !exists {
			req.ChatTemplateKwargs["enable_thinking"] = *req.EnableThinking
		}
	}
	if len(req.Tools) > 0 && req.ToolChoice == nil {
		req.ToolChoice = "auto"
	}

	if forced, ok := envFloat("MEADOW_FORCE_TEMP"); ok {
		req.Temperature = &forced
	}
	if value := strings.TrimSpace(os.Getenv("MEADOW_FORCE_TOP_K")); value != "" {
		if forced, err := strconv.Atoi(value); err == nil {
			req.TopK = &forced
		}
	}
	if forced, ok := envFloat("MEADOW_FORCE_REPETITION_PENALTY"); ok {
		req.RepetitionPenalty = &forced
	}
}

// mtplxMaxTokenCeiling keeps short tool turns bounded without changing the
// budgets of Gemma 4 or Qwen models served by another backend. A full-file
// write receives the server's 8K response budget because its entire artifact
// lives inside one JSON argument; truncating that argument produces an
// unclosed tool call and no executable file.
func mtplxMaxTokenCeiling(req *ChatRequest) int {
	if len(req.Tools) == 0 {
		return mtplxChatMaxTokens
	}
	switch forcedToolChoiceName(req.ToolChoice) {
	case "write_file", "research_artifact":
		return mtplxWriteMaxTokens
	case "edit":
		return mtplxEditMaxTokens
	}
	// Some recovery stages deliberately expose both edit and write_file. They
	// cannot force one tool, but a valid full-file write still needs the write
	// budget; otherwise MTPLX truncates the JSON payload at the short-tool cap.
	if requestHasTool(req, "write_file") || requestHasTool(req, "research_artifact") {
		return mtplxWriteMaxTokens
	}
	if requestHasTool(req, "edit") {
		return mtplxEditMaxTokens
	}
	return mtplxActionMaxTokens
}

func requestHasTool(req *ChatRequest, name string) bool {
	for _, tool := range req.Tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

// ApplyQwythosSafetyOverrides follows Qwythos' Qwen3.5-style chat defaults but
// keeps tool/action turns deterministic. The model-card sampling values help
// open-ended prose; they make local tool loops more likely to guess paths or
// print fake transcripts, so forced-tool turns stay at the action temperature.
func ApplyQwythosSafetyOverrides(req *ChatRequest) {
	ceiling := qwythosMaxTokens()
	if req.MaxTokens == nil || *req.MaxTokens > ceiling {
		c := ceiling
		req.MaxTokens = &c
	}
	if IsQwythosMultimodalModel(req.Model) {
		disabled := false
		if req.EnableThinking == nil {
			req.EnableThinking = &disabled
			if req.ChatTemplateKwargs == nil {
				req.ChatTemplateKwargs = map[string]any{}
			}
			if _, exists := req.ChatTemplateKwargs["enable_thinking"]; !exists {
				req.ChatTemplateKwargs["enable_thinking"] = false
			}
		}
	}

	isAction := len(req.Tools) > 0 && req.ToolChoice != nil
	isFinalSummary := hasPromptFragment(req.Messages, "Return the final answer now. Do not call tools.")
	if isAction {
		t := gemma4ActionTemperature
		req.Temperature = &t
	} else if isFinalSummary {
		t := gemma4Temperature
		req.Temperature = &t
	} else if req.Temperature == nil || *req.Temperature == gemma4ActionTemperature || *req.Temperature == gemma4Temperature {
		t := qwythosTemperature
		req.Temperature = &t
	}
	if req.TopP == nil {
		f := qwythosTopP
		req.TopP = &f
	}
	if req.TopK == nil {
		k := qwythosTopK
		req.TopK = &k
	}
	if req.RepetitionPenalty == nil || *req.RepetitionPenalty == 1.0 {
		rp := qwythosRepetitionPenalty
		req.RepetitionPenalty = &rp
	}
	forceWebSearchOnIntent(req)
}

func ApplyLocalModelSafetyOverrides(req *ChatRequest) {
	if profile, ok := ProfileForModel(req.Model); ok && profile.Family == "minicpm" {
		// Published MiniCPM sampling, with the native template's instruction
		// mode as the fast default. Explicit effort is applied afterwards.
		temperature, topP, topK, disabled, parallel := 1.0, 0.95, 20, false, false
		req.Temperature, req.TopP, req.TopK = &temperature, &topP, &topK
		req.ParallelToolCalls = &parallel
		if req.MaxTokens == nil || *req.MaxTokens > 8192 {
			limit := 8192
			req.MaxTokens = &limit
		}
		if req.EnableThinking == nil {
			req.EnableThinking = &disabled
		}
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = map[string]any{}
		}
		req.ChatTemplateKwargs["enable_thinking"] = *req.EnableThinking
		if len(req.Tools) > 0 && req.ToolChoice == nil {
			req.ToolChoice = "auto"
		}
		return
	}
	if IsQwen38Model(req.Model) {
		ApplyQwen38SafetyOverrides(req)
		return
	}
	if IsOrnithModel(req.Model) {
		ApplyOrnithSafetyOverrides(req)
		return
	}
	if IsMuseModel(req.Model) {
		ApplyMuseSafetyOverrides(req)
		return
	}
	if IsLFM25Model(req.Model) {
		ApplyLFM25SafetyOverrides(req)
		return
	}
	if IsNemotronModel(req.Model) {
		ApplyNemotron30SafetyOverrides(req)
		return
	}
	if prof, ok := ProfileForModel(req.Model); ok && prof.Family == "qwen3" {
		ApplyQwenSafetyOverrides(req)
		return
	}
	if IsLocalGemma4Model(req.Model) {
		ApplyGemma4SafetyOverrides(req)
	}
}

// ApplyQwen38SafetyOverrides follows the Qwen3.8 checkpoint's native sampling
// while disabling explicit thinking for the first product profile. Bare A/B
// probes showed the same model completed inspect -> write -> validate in five
// turns with thinking disabled, while low effort could consume its whole
// response budget before issuing a mutation. The coding Kernel still owns
// safety and evidence; it does not select the model's next semantic action.
func ApplyQwen38SafetyOverrides(req *ChatRequest) {
	maxTokens := qwen38DefaultMaxTokens
	if requestHasTool(req, "write_file") || requestHasTool(req, "write_files") {
		maxTokens = qwen38WriteMaxTokens
	} else if req.SamplingPhase == SamplingPhaseFinal {
		maxTokens = qwen38FinalMaxTokens
	}
	if req.MaxTokens == nil || *req.MaxTokens > maxTokens {
		limit := maxTokens
		req.MaxTokens = &limit
	}

	temperature := qwen38Temperature
	topP := qwen38TopP
	topK := qwen38TopK
	repetition := qwenNativeRepetition
	presence := 0.0
	parallel := QwenParallelToolCallsEnabled(req.Model)
	disabled := false
	req.Temperature = &temperature
	req.TopP = &topP
	req.TopK = &topK
	req.RepetitionPenalty = &repetition
	req.PresencePenalty = &presence
	req.ParallelToolCalls = &parallel
	req.EnableThinking = &disabled
	// vllm-mlx's Qwen3.5/3.8 path can keep emitting reasoning_content when
	// only the template boolean is false. Explicit Think Off already sends the
	// native `none` tier and completes large streamed write_file calls; make the
	// product auto profile carry the same backend contract. A user-selected
	// Think On still overrides this later in ApplyUserInferenceControls.
	req.ReasoningEffort = "none"
	if req.ChatTemplateKwargs == nil {
		req.ChatTemplateKwargs = map[string]any{}
	}
	req.ChatTemplateKwargs["enable_thinking"] = false
	req.ChatTemplateKwargs["preserve_thinking"] = false
	if len(req.Tools) > 0 && req.ToolChoice == nil {
		req.ToolChoice = "auto"
	}

	if forced, ok := envFloat("MEADOW_FORCE_TEMP"); ok {
		req.Temperature = &forced
	}
	if forced, ok := envFloat("MEADOW_FORCE_TOP_P"); ok {
		req.TopP = &forced
	}
	if value := strings.TrimSpace(os.Getenv("MEADOW_FORCE_TOP_K")); value != "" {
		if forced, err := strconv.Atoi(value); err == nil {
			req.TopK = &forced
		}
	}
}

// ApplyOrnithSafetyOverrides keeps Ornith's published sampling/tool-use
// distribution while defaulting ordinary product turns to direct output.
// Explicit /think remains a request-level override applied after this profile.
// This prevents short planning prompts from consuming the full token budget in
// reasoning_content without ever producing a visible answer.
func ApplyOrnithSafetyOverrides(req *ChatRequest) {
	temperature := ornithTemperature
	topP := ornithTopP
	topK := ornithTopK
	repetition := ornithRepetitionPenalty
	parallel := false
	disabled := false
	req.Temperature = &temperature
	req.TopP = &topP
	req.TopK = &topK
	req.RepetitionPenalty = &repetition
	req.ParallelToolCalls = &parallel
	if req.MaxTokens == nil || *req.MaxTokens > ornithDefaultMaxTokens {
		limit := ornithDefaultMaxTokens
		req.MaxTokens = &limit
	}
	if req.EnableThinking == nil {
		req.EnableThinking = &disabled
	}
	if req.ChatTemplateKwargs == nil {
		req.ChatTemplateKwargs = map[string]any{}
	}
	if _, exists := req.ChatTemplateKwargs["enable_thinking"]; !exists {
		req.ChatTemplateKwargs["enable_thinking"] = *req.EnableThinking
	}
	if !*req.EnableThinking {
		if _, exists := req.ChatTemplateKwargs["preserve_thinking"]; !exists {
			req.ChatTemplateKwargs["preserve_thinking"] = false
		}
	}
	if len(req.Tools) > 0 && req.ToolChoice == nil {
		req.ToolChoice = "auto"
	}
	if forced, ok := envFloat("MEADOW_FORCE_TEMP"); ok {
		req.Temperature = &forced
	}
	if forced, ok := envFloat("MEADOW_FORCE_TOP_P"); ok {
		req.TopP = &forced
	}
	if value := strings.TrimSpace(os.Getenv("MEADOW_FORCE_TOP_K")); value != "" {
		if forced, err := strconv.Atoi(value); err == nil {
			req.TopK = &forced
		}
	}
}

func modelOwnsWebToolChoice(model string) bool {
	profile, ok := ProfileForModel(model)
	return ok && profile.Family == "qwen3" && profile.Streaming
}

// StripThoughtChannel removes Gemma-4 thought-channel segments from model
// output. Port of the chat template's strip_thinking macro: for every
// '<channel|>'-delimited part, text from an unclosed '<|channel>' marker
// onward is dropped. Streaming responses leak these markers because the
// server strips them only on the non-streaming path.
func StripThoughtChannel(text string) string {
	filter := &ThoughtStreamFilter{}
	if closeIndex := strings.Index(text, thoughtHTMLClose); closeIndex >= 0 &&
		!strings.Contains(text[:closeIndex], thoughtHTMLOpen) {
		// LFM2.5 may omit the opening marker because the server's chat template
		// prefills it. Treat everything before the first close as hidden thought.
		text = text[closeIndex+len(thoughtHTMLClose):]
	}
	answer, _ := filter.Feed(text)
	answer += filter.Flush()
	return strings.TrimSpace(answer)
}

// Gemma-4 native tool-history control-token markers, from chat_template.jinja.
const (
	gemma4ToolCallOpen      = "<|tool_call>call:"
	gemma4ToolCallClose     = "<tool_call|>"
	gemma4ToolResponseOpen  = "<|tool_response>response:"
	gemma4ToolResponseClose = "<tool_response|>"
	gemma4Quote             = `<|"|>`
)

// FlattenToolHistory folds each assistant tool-call turn and the tool
// results that follow it into a single assistant message whose content is
// the *native* Gemma-4 control-token sequence the chat template would emit.
//
// Why this is necessary: vllm-mlx serves the 26B through its MLLM path,
// which rebuilds messages for the template and drops the assistant
// `tool_calls` field; role:"tool" messages are then skipped by the template
// itself. Native-format history therefore vanishes and the model loops on
// the first turn forever. Flattening into content keeps the history alive.
//
// Why native tokens (not "[Calling tool: ...]" prose): the model continues
// in whatever format it sees in history. Prose flattening trains it to emit
// prose tool calls the server's gemma4 parser can't read; native-token
// flattening keeps it emitting real <|tool_call> calls that parse cleanly.
func FlattenToolHistory(msgs []Message) []Message {
	nameByID := map[string]string{}
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					nameByID[tc.ID] = tc.Function.Name
				}
			}
		}
	}

	out := make([]Message, 0, len(msgs))
	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			var b strings.Builder
			if clean := StripThoughtChannel(m.Content); clean != "" {
				b.WriteString(clean)
			}
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				b.WriteString(gemma4ToolCallOpen + tc.Function.Name + args + gemma4ToolCallClose)
			}
			j := i + 1
			for j < len(msgs) && msgs[j].Role == "tool" {
				name := nameByID[msgs[j].ToolCallID]
				if name == "" {
					name = "unknown"
				}
				b.WriteString(gemma4ToolResponseOpen + name + "{value:" +
					gemma4Quote + msgs[j].Content + gemma4Quote + "}" + gemma4ToolResponseClose)
				j++
			}
			out = append(out, Message{Role: "assistant", Content: b.String()})
			i = j
			continue
		}
		if m.Role == "assistant" {
			out = append(out, Message{Role: "assistant", Content: StripThoughtChannel(m.Content)})
		} else {
			out = append(out, m)
		}
		i++
	}
	return out
}

// MergeStopSequences unions the request's stop strings with the defaults.
func MergeStopSequences(existing []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range defaultStopSequences {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range existing {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// RepairOrphanedToolCalls appends a synthetic tool response right after any
// assistant message whose tool_calls lack one — mlx_lm.server 400s on
// orphans instead of tolerating them like real OpenAI.
func RepairOrphanedToolCalls(msgs []Message) []Message {
	responded := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			responded[m.ToolCallID] = true
		}
	}

	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m)
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && !responded[tc.ID] {
				out = append(out, Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    "[tool execution cancelled]",
				})
				responded[tc.ID] = true
			}
		}
	}
	return out
}
