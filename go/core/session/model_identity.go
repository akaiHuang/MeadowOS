package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// LoadLastAssistantModel reads the latest durable model selection or admitted
// run, falling back to assistant identities in legacy chats. User content,
// compaction and provider-private state cannot select a model.
func LoadLastAssistantModel(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	modelID := ""
	for scanner.Scan() {
		var record struct {
			Type     string        `json:"type"`
			Model    string        `json:"model"`
			Message  *chatMsg      `json:"message"`
			RunState *chatRunState `json:"runState"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		if record.Type == "model_binding" || (record.Type == "run_state" && record.RunState != nil && record.RunState.Status == "running" && record.Model != "") {
			if record.Model == "" || strings.TrimSpace(record.Model) != record.Model {
				return "", errors.New("session_model_binding_invalid")
			}
			modelID = record.Model
			continue
		}
		if record.Type != "assistant" || record.Message == nil {
			continue
		}
		modelID = strings.TrimSpace(record.Model)
	}
	return modelID, scanner.Err()
}
