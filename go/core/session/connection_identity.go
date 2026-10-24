package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

var (
	ErrConnectionBindingMissing = errors.New("session_connection_binding_missing: start a new chat for the selected connection")
	ErrConnectionMismatch       = errors.New("session_connection_mismatch: select the original connection or start a new chat")
	ErrInvalidConnectionBinding = errors.New("session_connection_binding_invalid")
)

// LoadConnectionID returns the endpoint identity saved outside model-visible
// history. It never infers endpoint ownership from a served model name.
func LoadConnectionID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	connectionID := ""
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec struct {
			Type         string `json:"type"`
			ConnectionID string `json:"connectionId"`
		}
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			// A crash can leave an incomplete later message. Match transcript
			// recovery's handling without inventing an identity from those bytes.
			// An incomplete first binding still leaves the chat unbound, which
			// CheckConnectionID rejects for every explicit connection.
			continue
		}
		if rec.Type != "connection_binding" {
			continue
		}
		if rec.ConnectionID == "" || strings.TrimSpace(rec.ConnectionID) != rec.ConnectionID || (connectionID != "" && connectionID != rec.ConnectionID) {
			return "", ErrInvalidConnectionBinding
		}
		connectionID = rec.ConnectionID
	}
	return connectionID, scanner.Err()
}

// CheckConnectionID deliberately accepts an unbound legacy transcript only in
// legacy mode. Switching between endpoints requires a new chat, not a guessed
// mapping from the last assistant's model label.
func CheckConnectionID(path, connectionID string) error {
	saved, err := LoadConnectionID(path)
	if err != nil {
		return err
	}
	if saved == "" && connectionID != "" {
		return ErrConnectionBindingMissing
	}
	if saved != connectionID {
		return ErrConnectionMismatch
	}
	return nil
}

// BindConnectionID binds an empty transcript before its first request. Existing
// chats must already carry the identical binding; their origin is immutable.
func (w *ChatWriter) BindConnectionID(connectionID string) error {
	if strings.TrimSpace(connectionID) != connectionID {
		return ErrInvalidConnectionBinding
	}
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != 0 {
		return CheckConnectionID(w.Path, connectionID)
	}
	if connectionID == "" {
		return nil
	}
	return w.append(chatRecord{Type: "connection_binding", ConnectionID: connectionID})
}

// ModelBinding persists a deliberate selection separately from generated text.
// It contains only a served model ID; credentials and URLs never enter history.
func (w *ChatWriter) ModelBinding(modelID string) error {
	if modelID == "" || strings.TrimSpace(modelID) != modelID {
		return errors.New("session_model_binding_invalid")
	}
	return w.append(chatRecord{Type: "model_binding", Model: modelID})
}
