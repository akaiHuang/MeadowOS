package deskevo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
)

// Reflection recovery is an explicit native action, never inferred from prose.
// Only its opaque ID crosses HTTP; every path is derived from the bound project.
type reflectionResumeKey struct{}

func WithReflectionResume(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, reflectionResumeKey{}, runID)
}

func reflectionResumeID(ctx context.Context) string {
	id, _ := ctx.Value(reflectionResumeKey{}).(string)
	return id
}

func ReflectionResumeRequested(ctx context.Context) bool { return reflectionResumeID(ctx) != "" }

var taskIDPattern = regexp.MustCompile(`^task-[0-9]+$`)

type reflectionResume struct {
	Run, Selected, OriginalID string
	Status                    map[string]any
	Manifest                  snapshotManifest
}

func readRecoveryJSON(path string, target any) error {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != filepath.Clean(path) {
		return errors.New("invalid Evo recovery path")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024*1024 {
		return errors.New("invalid Evo recovery record")
	}
	return json.NewDecoder(f).Decode(target)
}

// A failed/explicitly rejected provider finish must not be credited as an
// intentional empty reflection in older installed versions.
func reflectionNeedsRecovery(root, originalID string) bool {
	var report struct {
		Completed bool   `json:"runtime_completed"`
		Error     string `json:"error"`
		Stop      string `json:"stop"`
		Calls     []struct {
			Result struct {
				Finish string `json:"FinishReason"`
			} `json:"result"`
			Error string `json:"error"`
		} `json:"calls"`
	}
	if readRecoveryJSON(filepath.Join(root, "improver", originalID, "experience.seq1", "result.json"), &report) != nil {
		return false
	}
	if !report.Completed || report.Error != "" || report.Stop != "completed" {
		return true
	}
	if len(report.Calls) > 0 {
		last := report.Calls[len(report.Calls)-1]
		return last.Error != "" || last.Result.Finish == "error"
	}
	return false
}

func inspectReflectionResume(state, cwd, chat, model, runID string) (*reflectionResume, error) {
	deny := func() (*reflectionResume, error) {
		return nil, errors.New("此 Evo 任務無法只重試回顧，請保留原始紀錄")
	}
	if !taskIDPattern.MatchString(runID) {
		return deny()
	}
	latest, err := Latest(state, cwd, chat)
	if err != nil {
		return nil, err
	}
	if latest["runId"] != runID || latest["phase"] != "ended" || latest["status"] == "running" || latest["reflectionCompleted"] == true {
		return deny()
	}
	if latest["model"] != model || latest["originalProject"] != cwd {
		return deny()
	}
	originalID := runID
	if previous, ok := latest["recoveryOf"].(string); ok && previous != "" {
		originalID = previous
	}
	if !taskIDPattern.MatchString(originalID) {
		return deny()
	}
	project := filepath.Join(state, "evo", "projects", sum(cwd)[:24])
	root := filepath.Join(project, originalID)
	if !childPath(project, root) {
		return deny()
	}
	var status map[string]any
	if readRecoveryJSON(filepath.Join(root, "status.json"), &status) != nil || status["phase"] != "ended" || status["originalProject"] != cwd || status["model"] != model {
		return deny()
	}
	var summary struct {
		Status     string `json:"status"`
		Errors     []any  `json:"errors"`
		Selections map[string]struct {
			OK       bool   `json:"ok"`
			Fallback bool   `json:"fallback"`
			Base     bool   `json:"chosen_is_base"`
			TreeBase bool   `json:"chosen_tree_is_base"`
			Label    string `json:"chosen_label"`
			Selected string `json:"chosen_run_dir"`
		} `json:"selections"`
		Modes map[string]struct {
			OK     bool `json:"ok"`
			Stages []struct {
				Fallback bool   `json:"fell_back"`
				Error    string `json:"error"`
			} `json:"stages"`
		} `json:"modes"`
	}
	if readRecoveryJSON(filepath.Join(root, "runs", "evolve-meadowevolve-agg1", originalID, "summary.json"), &summary) != nil {
		return deny()
	}
	selection, mode := summary.Selections["seq"], summary.Modes["seq"]
	if summary.Status != "ok" || len(summary.Errors) != 0 || !selection.OK || selection.Fallback || selection.Base || selection.TreeBase || selection.Label != "c1" || !mode.OK || len(mode.Stages) != 3 {
		return deny()
	}
	for _, stage := range mode.Stages {
		if stage.Fallback || stage.Error != "" {
			return deny()
		}
	}
	if !childPath(root, selection.Selected) {
		return deny()
	}
	var adopted struct {
		Completed bool   `json:"runtime_completed"`
		Error     string `json:"error"`
		Stop      string `json:"stop"`
	}
	if readRecoveryJSON(filepath.Join(selection.Selected, "result.json"), &adopted) != nil || !adopted.Completed || adopted.Error != "" || adopted.Stop != "completed" {
		return deny()
	}
	if !reflectionNeedsRecovery(root, originalID) {
		return deny()
	}
	if _, err := os.Lstat(filepath.Join(project, "library", "project-work", "harnesses", originalID)); !os.IsNotExist(err) {
		return deny()
	}
	var manifest snapshotManifest
	if readRecoveryJSON(filepath.Join(root, "snapshot.json"), &manifest) != nil || len(manifest.Files) == 0 || !childPath(root, filepath.Join(root, "source")) {
		return deny()
	}
	return &reflectionResume{Run: root, Selected: selection.Selected, OriginalID: originalID, Status: status, Manifest: manifest}, nil
}

// ValidateReflectionResume rechecks the live source before an admitted recovery.
// It never modifies the original task, selected executor, or project files.
func ValidateReflectionResume(state, cwd, chat, model, runID string) error {
	resume, err := inspectReflectionResume(state, cwd, chat, model, runID)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "meadow-evo-recovery-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	current, err := snapshot(cwd, filepath.Join(tmp, "current"))
	if err != nil {
		return err
	}
	frozen, err := snapshot(filepath.Join(resume.Run, "source"), filepath.Join(tmp, "frozen"))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current.Files, resume.Manifest.Files) || !reflect.DeepEqual(frozen.Files, resume.Manifest.Files) {
		return errors.New("專案或原始副本已變更，無法重試既有回顧")
	}
	return nil
}

// AnnotateReflectionRecovery is a truthful read projection for legacy receipts.
// It does not rewrite the original falsely completed task on disk.
func AnnotateReflectionRecovery(state, cwd, chat, model string, status map[string]any) {
	id, _ := status["runId"].(string)
	if _, err := inspectReflectionResume(state, cwd, chat, model, id); err == nil {
		status["status"] = "incomplete"
		status["reflectionRecoveryAvailable"] = true
		status["error"] = "回顧未完成；既有執行結果已保留，可只重試回顧。"
	}
}
