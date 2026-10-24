package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// InferencePreference is scoped to one exact model identity inside a durable
// chat. Capability validation belongs to the current runtime, not the store.
type InferencePreference struct {
	ThinkingMode    provider.ThinkingMode    `json:"thinkingMode"`
	ReasoningEffort provider.ReasoningEffort `json:"reasoningEffort"`
}

type inferencePreferenceFile struct {
	Version int                            `json:"version"`
	Models  map[string]InferencePreference `json:"models"`
}

// Multiple live handles may refer to the same durable chat after a passive
// reopen. Serialize read-modify-write operations so different model entries
// cannot overwrite each other.
var inferencePreferenceMu sync.Mutex

func inferencePreferencePath(projectRoot, chatID string) (string, error) {
	path, err := ChatPath(projectRoot, chatID)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(path, ".jsonl") + ".inference-controls.json", nil
}

func readInferencePreferences(path string) (inferencePreferenceFile, error) {
	state := inferencePreferenceFile{Version: 1, Models: map[string]InferencePreference{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("invalid inference preferences: %w", err)
	}
	if state.Version != 1 || state.Models == nil {
		return state, fmt.Errorf("unsupported inference preference format")
	}
	return state, nil
}

func LoadInferencePreference(projectRoot, chatID, modelID string) (InferencePreference, bool, error) {
	inferencePreferenceMu.Lock()
	defer inferencePreferenceMu.Unlock()
	path, err := inferencePreferencePath(projectRoot, chatID)
	if err != nil {
		return InferencePreference{}, false, err
	}
	state, err := readInferencePreferences(path)
	if err != nil {
		return InferencePreference{}, false, err
	}
	pref, ok := state.Models[strings.TrimSpace(modelID)]
	return pref, ok, nil
}

func SaveInferencePreference(projectRoot, chatID, modelID string, preference InferencePreference) error {
	inferencePreferenceMu.Lock()
	defer inferencePreferenceMu.Unlock()
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model id required")
	}
	if _, err := provider.ParseThinkingMode(string(preference.ThinkingMode)); err != nil {
		return err
	}
	if _, err := provider.ParseReasoningEffort(string(preference.ReasoningEffort)); err != nil {
		return err
	}
	path, err := inferencePreferencePath(projectRoot, chatID)
	if err != nil {
		return err
	}
	state, err := readInferencePreferences(path)
	if err != nil {
		return err
	}
	state.Models[modelID] = preference
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".inference-controls-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err = file.Write(append(data, '\n')); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	// Rename is the commit boundary. A failure before it leaves the previous
	// preference intact, allowing the caller to leave its live state unchanged.
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// The preference is committed after rename. Best-effort directory sync must
	// not report an uncommitted update after durable/live values can no longer
	// be rolled back atomically.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
