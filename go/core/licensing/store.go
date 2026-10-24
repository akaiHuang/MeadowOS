package licensing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var ErrNotFound = errors.New("not found")

type Store interface {
	View(context.Context, func(State) error) error
	Update(context.Context, func(*State) error) error
}

type JSONStore struct {
	mu    sync.Mutex
	path  string
	state State
}

func NewJSONStore(path string) (*JSONStore, error) {
	store := &JSONStore{path: path}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *JSONStore) View(ctx context.Context, fn func(State) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	copyState := cloneState(s.state)
	return fn(copyState)
}

func (s *JSONStore) Update(ctx context.Context, fn func(*State) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state.ensure()
	if err := fn(&s.state); err != nil {
		return err
	}
	return s.saveLocked()
}

func (s *JSONStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.state.ensure()
			return nil
		}
		return err
	}
	if len(data) == 0 {
		s.state.ensure()
		return nil
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return err
	}
	s.state.ensure()
	return nil
}

func (s *JSONStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func cloneState(s State) State {
	s.ensure()
	out := State{
		Users:         make(map[string]User, len(s.Users)),
		Devices:       make(map[string]Device, len(s.Devices)),
		Entitlements:  make(map[string]Entitlement, len(s.Entitlements)),
		Purchases:     make(map[string]Purchase, len(s.Purchases)),
		AuthCodes:     make(map[string]AuthCode, len(s.AuthCodes)),
		Sessions:      make(map[string]Session, len(s.Sessions)),
		WebhookEvents: make(map[string]WebhookEvent, len(s.WebhookEvents)),
		AuditEvents:   append([]AuditEvent(nil), s.AuditEvents...),
	}
	for k, v := range s.Users {
		out.Users[k] = v
	}
	for k, v := range s.Devices {
		out.Devices[k] = v
	}
	for k, v := range s.Entitlements {
		out.Entitlements[k] = v
	}
	for k, v := range s.Purchases {
		out.Purchases[k] = v
	}
	for k, v := range s.AuthCodes {
		out.AuthCodes[k] = v
	}
	for k, v := range s.Sessions {
		out.Sessions[k] = v
	}
	for k, v := range s.WebhookEvents {
		out.WebhookEvents[k] = v
	}
	return out
}
