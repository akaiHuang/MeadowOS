package serve

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/session"
)

const configurationLeaseDuration = 15 * time.Second

var errConfigurationLeaseActive = errors.New("configuration_lease_active")

var (
	ErrConfigurationLeaseMismatch = errors.New("configuration_lease_mismatch")
	ErrConfigurationNotIdle       = errors.New("agent_not_idle")
)

// ApplyConfigurationLease commits one prepared, bounded configuration change.
// apply must be atomic on error and must not perform network I/O or re-enter
// configuration admission/accessors. Holding configurationMu pins the lease
// until commit completes, even if its original deadline passes meanwhile.
// Existing session identities never change; the lease remains owned by the
// caller until explicitly released or expired after this method returns.
func (s *Server) ApplyConfigurationLease(leaseID string, apply func() (string, error)) error {
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	if !s.configurationLeaseActiveLocked(time.Now()) || leaseID != s.configurationLeaseID {
		return ErrConfigurationLeaseMismatch
	}
	if !s.configurationIdleLocked() {
		return ErrConfigurationNotIdle
	}
	if apply == nil {
		return errors.New("configuration_apply_unavailable")
	}
	connectionID, err := apply()
	if err != nil {
		return err
	}
	s.ConnectionID = connectionID
	return nil
}

// CurrentConnectionID is the selected identity for new work. A configuration
// callback must not call this accessor while it holds the lease commit lock.
func (s *Server) CurrentConnectionID() string {
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	return s.ConnectionID
}

// configurationLeaseActiveLocked expires an abandoned lease without timers or
// persistent state. A crashed client can delay new work for at most 15 seconds.
func (s *Server) configurationLeaseActiveLocked(now time.Time) bool {
	if s.configurationLeaseID != "" && !now.Before(s.configurationLeaseUntil) {
		s.configurationLeaseID = ""
		s.configurationLeaseUntil = time.Time{}
	}
	return s.configurationLeaseID != ""
}

func (s *Server) beginConfigurationAdmission() bool {
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	if s.configurationLeaseActiveLocked(time.Now()) {
		return false
	}
	s.configurationAdmissions++
	return true
}

func (s *Server) endConfigurationAdmission() {
	s.configurationMu.Lock()
	s.configurationAdmissions--
	s.configurationMu.Unlock()
}

// WithConfigurationAdmission protects work hosted outside the session router,
// such as model activation. Admission starts before body decoding and remains
// held through the handler, without retaining a mutex across network I/O.
func (s *Server) WithConfigurationAdmission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.beginConfigurationAdmission() {
			writeErr(w, http.StatusConflict, errConfigurationLeaseActive.Error())
			return
		}
		defer s.endConfigurationAdmission()
		if err := s.configurationSessionBindingError(r); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// This runs before ServeMux has populated PathValue. Admission already prevents
// configuration publication until the request (or its detached work) settles.
func (s *Server) configurationSessionBindingError(r *http.Request) error {
	if configurationObservationOrCancellation(r) {
		return nil
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 || parts[0] != "" || parts[1] != "session" || parts[2] == "" {
		return nil
	}
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[parts[2]]; sess != nil && sess.connectionID != s.ConnectionID {
		return session.ErrConnectionMismatch
	}
	return nil
}

func configurationObservationOrCancellation(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if r.Method != http.MethodPost {
		return false
	}
	parts := strings.Split(r.URL.Path, "/")
	return len(parts) == 4 && parts[0] == "" && parts[1] == "session" &&
		parts[2] != "" && parts[2] != "." && parts[2] != ".." &&
		(parts[3] == "cancel" || parts[3] == "heartbeat")
}

func (s *Server) configurationAdmissionMiddleware(next http.Handler) http.Handler {
	admitted := s.WithConfigurationAdmission(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read-only observation and cancellation remain available while settings
		// are being saved. Every other mutation is conservatively an admission.
		if configurationObservationOrCancellation(r) || r.URL.Path == "/agent/configuration/lease" {
			next.ServeHTTP(w, r)
			return
		}
		admitted.ServeHTTP(w, r)
	})
}

// WithSessionConfigurationAdmission protects session-side wrappers, including
// journal writes, before they prepare a mutation. Observations, cancellation and
// lease control retain the session router's exemptions.
func (s *Server) WithSessionConfigurationAdmission(next http.Handler) http.Handler {
	return s.configurationAdmissionMiddleware(next)
}

// configurationIdleLocked is a whole-Agent check, not a current-workspace
// snapshot. configurationMu remains held until the lease has been recorded.
func (s *Server) configurationIdleLocked() bool {
	if s.configurationAdmissions != 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		sess.mu.Lock()
		busy := sess.running || sess.activeRuntime != nil || sess.cancelRun != nil ||
			sess.warmCancel != nil || len(sess.promptAdmissions) != 0 || sess.queuePump ||
			len(sess.queuedPrompts) != 0
		if !busy && sess.workerBroker != nil {
			busy = sess.workerBroker.HasActive()
		}
		if !busy {
			for _, task := range sess.backgroundTasks {
				if !backgroundTaskTerminal(task.Status) {
					busy = true
					break
				}
			}
		}
		sess.mu.Unlock()
		if busy {
			return false
		}
	}
	return true
}

func (s *Server) handleConfigurationLeaseAcquire(w http.ResponseWriter, r *http.Request) {
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	if s.configurationLeaseActiveLocked(time.Now()) || !s.configurationIdleLocked() {
		writeErr(w, http.StatusConflict, "agent_not_idle")
		return
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		writeErr(w, http.StatusInternalServerError, "configuration_lease_unavailable")
		return
	}
	s.configurationLeaseID = hex.EncodeToString(token[:])
	s.configurationLeaseUntil = time.Now().Add(configurationLeaseDuration)
	writeJSON(w, map[string]any{
		"leaseId": s.configurationLeaseID, "expiresInSeconds": int(configurationLeaseDuration / time.Second),
	})
}

func (s *Server) handleConfigurationLeaseRelease(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LeaseID string `json:"leaseId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || body.LeaseID == "" {
		writeErr(w, http.StatusBadRequest, "leaseId required")
		return
	}
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	if !s.configurationLeaseActiveLocked(time.Now()) || body.LeaseID != s.configurationLeaseID {
		writeErr(w, http.StatusConflict, "configuration_lease_mismatch")
		return
	}
	s.configurationLeaseID = ""
	s.configurationLeaseUntil = time.Time{}
	writeJSON(w, map[string]any{"released": true})
}
