package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type RouteShadowRecord struct {
	Time                 string   `json:"time"`
	Source               string   `json:"source"`
	SessionID            string   `json:"session_id,omitempty"`
	Model                string   `json:"model"`
	CWD                  string   `json:"cwd"`
	UserText             string   `json:"user_text"`
	DeterministicIntent  string   `json:"deterministic_intent"`
	DeterministicTools   []string `json:"deterministic_tools"`
	DeterministicCompact string   `json:"deterministic_compact"`
	PredictedRaw         string   `json:"predicted_raw,omitempty"`
	PredictedIntent      string   `json:"predicted_intent,omitempty"`
	PredictedTools       []string `json:"predicted_tools,omitempty"`
	HasPrediction        bool     `json:"has_prediction"`
	Agreement            bool     `json:"agreement"`
	Promoted             bool     `json:"promoted,omitempty"`
	PromotionReason      string   `json:"promotion_reason,omitempty"`
	Error                string   `json:"error,omitempty"`
}

type RouteShadowLogger struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

func NewRouteShadowLogger(path string) (*RouteShadowLogger, error) {
	if path == "" {
		return nil, nil
	}
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &RouteShadowLogger{
		file: f,
		enc:  json.NewEncoder(f),
	}, nil
}

func (l *RouteShadowLogger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}

func (l *RouteShadowLogger) Hook(source, sessionID, model, cwd string) func(RouteShadowResult) {
	if l == nil {
		return nil
	}
	return func(res RouteShadowResult) {
		l.Log(source, sessionID, model, cwd, res)
	}
}

func (l *RouteShadowLogger) Log(source, sessionID, model, cwd string, res RouteShadowResult) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	hasPrediction := res.PredictedRaw != "" || res.PredictedIntent != "" ||
		len(res.PredictedTools) > 0 || res.Err != ""
	_ = l.enc.Encode(RouteShadowRecord{
		Time:                 time.Now().UTC().Format(time.RFC3339Nano),
		Source:               source,
		SessionID:            sessionID,
		Model:                model,
		CWD:                  cwd,
		UserText:             res.UserText,
		DeterministicIntent:  res.DeterministicIntent,
		DeterministicTools:   res.DeterministicTools,
		DeterministicCompact: res.DeterministicCompact,
		PredictedRaw:         res.PredictedRaw,
		PredictedIntent:      res.PredictedIntent,
		PredictedTools:       res.PredictedTools,
		HasPrediction:        hasPrediction,
		Agreement:            res.Agreement,
		Promoted:             res.Promoted,
		PromotionReason:      res.PromotionReason,
		Error:                res.Err,
	})
}
