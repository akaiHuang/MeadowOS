package serve

import (
	"encoding/json"
	"fmt"
	"net/http"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

type inferenceController interface {
	ThinkingMode() provider.ThinkingMode
	SetThinkingMode(provider.ThinkingMode) error
	ReasoningEffort() provider.ReasoningEffort
	SetReasoningEffort(provider.ReasoningEffort) error
}

type inferenceControlView struct {
	SessionID         string                   `json:"sessionId"`
	ModelID           string                   `json:"modelId"`
	Supported         bool                     `json:"supported"`
	ThinkingSupported bool                     `json:"thinkingSupported"`
	EffortSupported   bool                     `json:"effortSupported"`
	Maturity          string                   `json:"maturity"`
	ThinkingMode      provider.ThinkingMode    `json:"thinkingMode"`
	ReasoningEffort   provider.ReasoningEffort `json:"reasoningEffort"`
	AppliesNextTurn   bool                     `json:"appliesNextTurn"`
	Running           bool                     `json:"running"`
	Note              string                   `json:"note"`
	EffectiveEffort   provider.ReasoningEffort `json:"effectiveEffort"`
	EffectiveProfile  provider.EffortProfile   `json:"effectiveProfile"`
	EffortProfiles    []provider.EffortProfile `json:"effortProfiles"`
	OutputLimitSource string                   `json:"outputLimitSource"`
	PreferenceStatus  string                   `json:"preferenceStatus,omitempty"`
}

func normalizeInferenceControls(mode provider.ThinkingMode, effort provider.ReasoningEffort) (provider.ThinkingMode, provider.ReasoningEffort) {
	return provider.NormalizeEffortSelection(mode, effort)
}

func inferenceControlCapability(model string) (bool, string, string) {
	if p, _ := provider.ProfileForModel(model); p.Family == "xai" && p.ThinkingControl {
		return true, "available", "使用 xAI 原生推理強度；急速/關閉在 Grok 4.5–4.7 對應 low，4.3 對應 none；全力對應 xhigh。"
	}
	if !provider.SupportsThinkingControl(model) {
		return false, "unavailable", "此模型目前沒有可安全分離的 reasoning channel，會沿用產品驗證過的非 Think 設定。"
	}
	if provider.SupportsReasoningEffortControl(model) {
		return true, "experimental", "此模型可調整 Think 與五段 effort，目前標為實驗性；較高 effort 會增加等待時間。"
	}
	return true, "available", "此模型提供可分離的 reasoning channel；設定會從下一輪開始生效。"
}

func sessionInferenceControlView(sess *serveSession) inferenceControlView {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sessionInferenceControlViewLocked(sess)
}

func sessionInferenceControlViewLocked(sess *serveSession) inferenceControlView {
	model := sess.engine.ModelID
	mode, effort := normalizeInferenceControls(sess.thinkingMode, sess.reasoningEffort)
	running := sess.running
	preferenceStatus := sess.inferencePreferenceStatus
	outputLimit := 0
	if limits, ok := sess.engine.Client.(interface{ OutputTokenLimit() int }); ok {
		outputLimit = limits.OutputTokenLimit()
	}
	supported, maturity, note := inferenceControlCapability(model)
	effortSupported := provider.SupportsReasoningEffortControl(model)
	if declared, ok := provider.ProfileForClient(sess.engine.Client, model); ok {
		supported = declared.ThinkingControl
		effortSupported = declared.ReasoningEffortLevels == 5
		if supported && maturity == "unavailable" {
			maturity = "experimental"
			note = "使用此連線明確宣告的推理控制，從下一輪生效。"
		}
	}
	profiles := provider.EffortProfiles(model, outputLimit)
	if effortSupported && len(profiles) == 0 {
		for _, level := range []provider.ReasoningEffort{provider.ReasoningEffortMinimal, provider.ReasoningEffortLow, provider.ReasoningEffortMedium, provider.ReasoningEffortHigh, provider.ReasoningEffortMax} {
			p := provider.ResolveEffortProfile(model, level, outputLimit)
			p.ThinkingMode = provider.ThinkingModeForEffort(level)
			p.Description = "由外部模型服務控制推理強度。"
			profiles = append(profiles, p)
		}
	}
	profile := provider.ResolveInferenceEffortProfile(model, mode, effort, outputLimit)
	outputLimitSource := "unknown"
	if outputLimit > 0 {
		outputLimitSource = "runtime"
	} else if profile.MaxOutputTokens > 0 {
		outputLimitSource = "model_profile"
	}
	if preferenceStatus == "invalid" || preferenceStatus == "unavailable" {
		note += " 已儲存的設定無法讀取或不適用此模型，目前沿用預設設定。"
	}
	return inferenceControlView{
		SessionID: sess.id, ModelID: model, Supported: supported || effortSupported,
		ThinkingSupported: supported, EffortSupported: effortSupported,
		Maturity: maturity, ThinkingMode: mode, ReasoningEffort: effort,
		AppliesNextTurn: true, Running: running, Note: note,
		EffectiveEffort: profile.Effort, EffectiveProfile: profile,
		EffortProfiles: profiles, OutputLimitSource: outputLimitSource,
		PreferenceStatus: preferenceStatus,
	}
}

func (s *Server) handleInferenceControls(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	if r.Method == http.MethodGet {
		s.refreshInferenceOutputLimit(sess)
		writeJSON(w, sessionInferenceControlView(sess))
		return
	}

	var body struct {
		ThinkingMode    *string `json:"thinkingMode"`
		ReasoningEffort *string `json:"reasoningEffort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.ThinkingMode == nil && body.ReasoningEffort == nil) {
		writeErr(w, http.StatusBadRequest, "thinkingMode or reasoningEffort required")
		return
	}

	var parsedMode provider.ThinkingMode
	var parsedEffort provider.ReasoningEffort
	if body.ThinkingMode != nil {
		parsed, err := provider.ParseThinkingMode(*body.ThinkingMode)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		parsedMode = parsed
	}
	if body.ReasoningEffort != nil {
		parsed, err := provider.ParseReasoningEffort(*body.ReasoningEffort)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		parsedEffort = parsed
	}
	s.refreshInferenceOutputLimit(sess)

	// Resolve, persist and publish under one lock. An effort-only request uses
	// the latest preference and atomically selects its mode, including Off ->
	// high; a concurrent model switch cannot validate against another family.
	sess.mu.Lock()
	model := sess.engine.ModelID
	nextMode, nextEffort := normalizeInferenceControls(sess.thinkingMode, sess.reasoningEffort)
	supported, _, _ := inferenceControlCapability(model)
	effortSupported := provider.SupportsReasoningEffortControl(model)
	if p, ok := provider.ProfileForClient(sess.engine.Client, model); ok {
		supported = p.ThinkingControl
		effortSupported = p.ReasoningEffortLevels == 5
	}
	if body.ReasoningEffort != nil {
		if !effortSupported {
			sess.mu.Unlock()
			writeErr(w, http.StatusUnprocessableEntity, fmt.Sprintf("%s does not support reasoning effort controls", model))
			return
		}
		nextEffort = parsedEffort
		if body.ThinkingMode == nil {
			nextMode = provider.ThinkingModeForEffort(nextEffort)
		}
	}
	if body.ThinkingMode != nil {
		nextMode = parsedMode
	}
	nextMode, nextEffort = normalizeInferenceControls(nextMode, nextEffort)
	if nextMode == provider.ThinkingOn && !supported {
		sess.mu.Unlock()
		writeErr(w, http.StatusUnprocessableEntity, fmt.Sprintf("%s does not support safe thinking controls", model))
		return
	}
	if err := session.SaveInferencePreference(sess.cwd, sess.queueKey, model, session.InferencePreference{
		ThinkingMode: nextMode, ReasoningEffort: nextEffort,
	}); err != nil {
		sess.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("save inference controls: %v", err))
		return
	}
	sess.thinkingMode = nextMode
	sess.reasoningEffort = nextEffort
	sess.inferencePreferenceStatus = "saved"
	view := sessionInferenceControlViewLocked(sess)
	sess.mu.Unlock()
	sess.broadcast("inference_controls_changed", view)
	writeJSON(w, view)
}

func (s *Server) refreshInferenceOutputLimit(sess *serveSession) {
	if s.InferenceOutputTokenLimit == nil {
		return
	}
	// Read-only views remain available during configuration changes. Retain the
	// bound client's cached limit when it cannot observe its own connection.
	if !s.beginConfigurationAdmission() {
		return
	}
	defer s.endConfigurationAdmission()
	if sess.connectionID != s.CurrentConnectionID() {
		return
	}
	sess.mu.Lock()
	model := sess.engine.ModelID
	sess.mu.Unlock()
	// Runtime observation must not hold the session lock or start generation.
	limit := s.InferenceOutputTokenLimit(model)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.engine.ModelID != model {
		return
	}
	if setter, ok := sess.engine.Client.(interface{ SetOutputTokenLimit(int) }); ok {
		setter.SetOutputTokenLimit(max(0, limit))
	}
}

// restoreSessionInferenceControlsLocked is passive: it only reads preferences.
// A caller must hold sess.mu once the session has been published to other work.
func restoreSessionInferenceControlsLocked(sess *serveSession) {
	sess.thinkingMode = provider.ThinkingAuto
	sess.reasoningEffort = provider.ReasoningEffortMedium
	sess.inferencePreferenceStatus = "default"
	pref, found, err := session.LoadInferencePreference(sess.cwd, sess.queueKey, sess.engine.ModelID)
	if err != nil {
		sess.inferencePreferenceStatus = "unavailable"
		return
	}
	if !found {
		return
	}
	capability, _ := provider.ProfileForClient(sess.engine.Client, sess.engine.ModelID)
	mode, modeErr := provider.ParseThinkingMode(string(pref.ThinkingMode))
	effort, effortErr := provider.ParseReasoningEffort(string(pref.ReasoningEffort))
	if modeErr != nil || effortErr != nil ||
		(mode == provider.ThinkingOn && !capability.ThinkingControl) ||
		(effort != provider.ReasoningEffortMedium && capability.ReasoningEffortLevels != 5) {
		sess.inferencePreferenceStatus = "invalid"
		return
	}
	sess.thinkingMode, sess.reasoningEffort = normalizeInferenceControls(mode, effort)
	sess.inferencePreferenceStatus = "restored"
}

func applySessionInferenceControls(sess *serveSession, runtime agentcore.Runtime) error {
	sess.mu.Lock()
	selection := snapshotSessionInferenceControlsLocked(sess)
	sess.mu.Unlock()
	return applyInferenceControlSelection(*selection, runtime)
}

type sessionInferenceSelection struct {
	model           string
	mode            provider.ThinkingMode
	effort          provider.ReasoningEffort
	effortSupported *bool
}

func snapshotSessionInferenceControlsLocked(sess *serveSession) *sessionInferenceSelection {
	mode, effort := normalizeInferenceControls(sess.thinkingMode, sess.reasoningEffort)
	capability, _ := provider.ProfileForClient(sess.engine.Client, sess.engine.ModelID)
	supported := capability.ReasoningEffortLevels == 5
	return &sessionInferenceSelection{model: sess.engine.ModelID, mode: mode, effort: effort, effortSupported: &supported}
}

func applyInferenceControlSelection(selection sessionInferenceSelection, runtime agentcore.Runtime) error {
	controller, ok := runtime.(inferenceController)
	if !ok {
		return nil
	}
	supported := provider.SupportsReasoningEffortControl(selection.model)
	if selection.effortSupported != nil {
		supported = *selection.effortSupported
	}
	if supported {
		if err := controller.SetReasoningEffort(selection.effort); err != nil {
			return err
		}
	}
	return controller.SetThinkingMode(selection.mode)
}
