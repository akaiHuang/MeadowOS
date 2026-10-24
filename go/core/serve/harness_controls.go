package serve

import (
	"encoding/json"
	"fmt"
	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"io"
	"net/http"

	harnesscore "github.com/hey-meadow/meadow-code/go/core/harness"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

type harnessModeOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type harnessControlView struct {
	SessionID              string              `json:"sessionId"`
	ModelID                string              `json:"modelId"`
	SelectedMode           string              `json:"selectedMode"`
	EffectiveMode          string              `json:"effectiveMode"`
	ActiveMode             string              `json:"activeMode"`
	Running                bool                `json:"running"`
	AppliesNextRun         bool                `json:"appliesNextRun"`
	PreferenceStatus       string              `json:"preferenceStatus"`
	RecoveryIdentityStatus string              `json:"recoveryIdentityStatus,omitempty"`
	Modes                  []harnessModeOption `json:"modes"`
}

func (sess *serveSession) snapshotHarnessModeLocked() string {
	override := sess.harnessMode
	if override == "" {
		override = sess.defaultHarnessMode
	}
	model := ""
	if sess.engine != nil {
		model = sess.engine.ModelID
	}
	if sess.resolveHarnessMode != nil {
		return sess.resolveHarnessMode(model, override)
	}
	return string(harnesscore.ResolveMode(model, override))
}

func (sess *serveSession) modelFamilyHarnessModeLocked() string {
	model := ""
	if sess.engine != nil {
		model = sess.engine.ModelID
	}
	return string(harnesscore.ResolveMode(model, ""))
}

func (sess *serveSession) harnessControlsLocked() harnessControlView {
	active := ""
	if sess.activeHarnessMode != nil {
		active = *sess.activeHarnessMode
	}
	view := harnessControlView{
		SessionID: sess.id, ModelID: sess.engine.ModelID,
		SelectedMode: sess.harnessMode, EffectiveMode: sess.snapshotHarnessModeLocked(), ActiveMode: active,
		Running: sess.running || sess.activeHarnessMode != nil, AppliesNextRun: true, PreferenceStatus: sess.harnessPreferenceStatus,
		RecoveryIdentityStatus: sess.harnessRecoveryStatus,
		Modes: []harnessModeOption{
			{ID: "", Label: "Meadow 預設", Description: "沿用目前模型與部署設定。"},
			{ID: "codex-style", Label: "Meadow Coding", Description: "既有模型主導的工具執行架構。"},
			{ID: "raven-code", Label: "Raven-Code（實驗）", Description: "使用 Memory、Planning、Capability、Action 座席與共享工具執行架構；尚未證明所有任務品質更高。"},
			{ID: "kernel", Label: "模型家族架構", Description: "使用既有模型家族執行架構。"},
		},
	}
	if buildinfo.DesktopVariant == "evo" {
		view.Modes = append(view.Modes, harnessModeOption{ID: "evo", Label: "Meadow Evo", Description: "提案、試跑、合併與經驗反思；在專案副本中執行。"})
	}
	return view
}

func (s *Server) handleHarnessControls(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		sess.mu.Lock()
		view := sess.harnessControlsLocked()
		sess.mu.Unlock()
		writeJSON(w, view)
		return
	}
	var body struct {
		Mode *string `json:"mode"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Mode == nil {
		writeErr(w, http.StatusBadRequest, "mode required")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "invalid harness controls")
		return
	}
	if _, err := harnesscore.ParseMode(*body.Mode); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess.mu.Lock()
	if err := session.SaveHarnessPreference(sess.cwd, queueChatID(sess), session.HarnessPreference{Mode: *body.Mode}); err != nil {
		sess.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("save harness controls: %v", err))
		return
	}
	sess.harnessMode, sess.harnessPreferenceStatus = *body.Mode, "saved"
	view := sess.harnessControlsLocked()
	sess.mu.Unlock()
	sess.broadcast("harness_controls_changed", view)
	writeJSON(w, view)
}

// Restore is passive. Corrupt/unavailable preferences never enable Raven.
func restoreSessionHarnessControlsLocked(sess *serveSession) {
	sess.harnessMode, sess.harnessPreferenceStatus = "", "default"
	preference, found, err := session.LoadHarnessPreference(sess.cwd, queueChatID(sess))
	if err != nil {
		sess.harnessPreferenceStatus = "unavailable"
		return
	}
	if found {
		sess.harnessMode, sess.harnessPreferenceStatus = preference.Mode, "restored"
	}
}

func (sess *serveSession) admitHarnessModeLocked(mode string) {
	selected := mode
	if sess.resolveHarnessMode != nil && sess.engine != nil {
		selected = sess.resolveHarnessMode(sess.engine.ModelID, mode)
	}
	sess.activeHarnessMode = &selected
}

// Execution identity and the checkpoint are one append+fsync in the actual
// chat journal. Next-run preferences are deliberately not recovery evidence.
func (sess *serveSession) persistHarnessRunStart(checkpointID string) error {
	sess.mu.Lock()
	mode := ""
	modelID := sess.engine.ModelID
	if sess.activeHarnessMode != nil {
		mode = *sess.activeHarnessMode
	}
	sess.mu.Unlock()
	if sess.chat == nil {
		return fmt.Errorf("run start has no chat journal")
	}
	return sess.chat.RunStartedCheckpointWithModel(checkpointID, mode, modelID)
}
