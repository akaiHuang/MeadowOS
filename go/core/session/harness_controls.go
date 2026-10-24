package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// HarnessPreference belongs to one durable chat, independently of its model.
// Empty Mode keeps the deployment/model-family default.
type HarnessPreference struct {
	Mode string `json:"mode"`
}

type harnessPreferenceFile struct {
	Version int `json:"version"`
	HarnessPreference
}

var harnessPreferenceMu sync.Mutex

func validHarnessPreference(mode string) bool {
	return mode == "" || mode == "kernel" || mode == "codex-style" || mode == "raven-code" || mode == "evo"
}

func harnessPreferencePath(projectRoot, chatID string) (string, error) {
	path, err := ChatPath(projectRoot, chatID)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(path, ".jsonl") + ".harness-controls.json", nil
}

func LoadHarnessPreference(projectRoot, chatID string) (HarnessPreference, bool, error) {
	harnessPreferenceMu.Lock()
	defer harnessPreferenceMu.Unlock()
	path, err := harnessPreferencePath(projectRoot, chatID)
	if err != nil {
		return HarnessPreference{}, false, err
	}
	return readHarnessPreference(path)
}

func readHarnessPreference(path string) (HarnessPreference, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return HarnessPreference{}, false, nil
	}
	if err != nil {
		return HarnessPreference{}, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return HarnessPreference{}, false, fmt.Errorf("harness preference must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return HarnessPreference{}, false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return HarnessPreference{}, false, fmt.Errorf("harness preference changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return HarnessPreference{}, false, fmt.Errorf("harness preference exceeds read bound")
	}
	fields, err := uniqueHarnessFields(data)
	if err != nil || len(fields) != 2 || len(fields["mode"]) == 0 || bytes.Equal(bytes.TrimSpace(fields["mode"]), []byte("null")) {
		return HarnessPreference{}, false, fmt.Errorf("invalid explicit harness preference fields")
	}
	var state harnessPreferenceFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return HarnessPreference{}, false, fmt.Errorf("invalid harness preference: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return HarnessPreference{}, false, fmt.Errorf("invalid trailing harness preference data")
	}
	if state.Version != 1 || !validHarnessPreference(state.Mode) {
		return HarnessPreference{}, false, fmt.Errorf("unsupported harness preference")
	}
	return state.HarnessPreference, true, nil
}

func uniqueHarnessFields(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, fmt.Errorf("harness preference must be an object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, fmt.Errorf("invalid harness preference key")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate harness preference key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing harness preference data")
	}
	return fields, nil
}

func SaveHarnessPreference(projectRoot, chatID string, preference HarnessPreference) error {
	if !validHarnessPreference(preference.Mode) {
		return fmt.Errorf("invalid harness preference mode")
	}
	harnessPreferenceMu.Lock()
	defer harnessPreferenceMu.Unlock()
	path, err := harnessPreferencePath(projectRoot, chatID)
	if err != nil {
		return err
	}
	return writeHarnessPreference(path, preference)
}

func writeHarnessPreference(path string, preference HarnessPreference) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("harness preference must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.MarshalIndent(harnessPreferenceFile{Version: 1, HarnessPreference: preference}, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".harness-controls-*")
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
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
