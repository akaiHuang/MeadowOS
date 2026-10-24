package serve

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/session"
)

// EngineFactory predates caller contexts and may block in filesystem I/O.
// Bound unfinished construction independently of the daemon's HTTP permits.
const maxSessionCreations = 8

var errSessionCreationCapacity = errors.New("session_creation_capacity_exhausted")

type sessionCreationKey struct {
	cwd, thread string
	channel     sessionApprovalChannel
}

type sessionCreation struct {
	done      chan struct{}
	waiters   int
	completed bool
	sess      *serveSession
	err       error
}

func sessionCreationErrorStatus(err error) int {
	switch {
	case errors.Is(err, errConfigurationLeaseActive):
		return http.StatusConflict
	case errors.Is(err, errSessionCreationCapacity):
		return http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout
	default:
		return http.StatusInternalServerError
	}
}

// matchingSessionLocked preserves single scope within the selected connection.
// Thread callers skip this lookup. Caller holds Server.mu.
func (s *Server) matchingSessionLocked(cwd string, channel sessionApprovalChannel) *serveSession {
	for _, sess := range s.sessions {
		if sess.cwd == cwd && sess.connectionID == s.ConnectionID && sess.effectiveApprovalChannel() == channel {
			return sess
		}
	}
	return nil
}

// createConfiguredSession retains both caller and worker configuration
// admissions. Cancelling a caller cannot expose settings to a still-running
// factory, nor allow that factory to publish an orphan later.
func (s *Server) createConfiguredSession(ctx context.Context, cwd string, channel sessionApprovalChannel, disabledTools []string, single bool) (*serveSession, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	key := sessionCreationKey{cwd: cwd, channel: channel}
	if !single {
		key.thread = session.NewUUID()
	}

	// Keep configuration-before-registry lock order. No I/O is performed here.
	s.configurationMu.Lock()
	if s.configurationLeaseActiveLocked(time.Now()) {
		s.configurationMu.Unlock()
		return nil, false, errConfigurationLeaseActive
	}
	s.configurationAdmissions++ // this caller, through final publication
	s.mu.Lock()
	defer s.endConfigurationAdmission()
	if single {
		if sess := s.matchingSessionLocked(cwd, channel); sess != nil {
			s.mu.Unlock()
			s.configurationMu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			return sess, true, nil
		}
	}
	if s.sessionCreations == nil {
		s.sessionCreations = make(map[sessionCreationKey]*sessionCreation)
	}
	creation := s.sessionCreations[key]
	startWorker := creation == nil
	if startWorker {
		if s.activeSessionCreations >= maxSessionCreations {
			s.mu.Unlock()
			s.configurationMu.Unlock()
			return nil, false, errSessionCreationCapacity
		}
		creation = &sessionCreation{done: make(chan struct{})}
		s.sessionCreations[key] = creation
		s.activeSessionCreations++
		s.configurationAdmissions++ // until actual construction settles
	}
	creation.waiters++
	s.mu.Unlock()
	s.configurationMu.Unlock()
	defer s.releaseSessionCreationWaiter(key, creation)

	if startWorker {
		disabled := append([]string(nil), disabledTools...)
		go s.constructSession(key, creation, cwd, channel, disabled)
	}
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-creation.done:
	}
	// An already cancelled caller must not publish, even when both select
	// branches were ready. Only this short publication holds the registry lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if creation.err != nil {
		return nil, false, creation.err
	}
	if single {
		if sess := s.matchingSessionLocked(cwd, channel); sess != nil {
			return sess, true, nil
		}
	}
	if s.sessions == nil {
		s.sessions = make(map[string]*serveSession)
	}
	s.sessions[creation.sess.id] = creation.sess
	return creation.sess, false, nil
}

func (s *Server) releaseSessionCreationWaiter(key sessionCreationKey, creation *sessionCreation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	creation.waiters--
	if creation.completed && creation.waiters == 0 {
		delete(s.sessionCreations, key)
	}
}

func (s *Server) constructSession(key sessionCreationKey, creation *sessionCreation, cwd string, channel sessionApprovalChannel, disabledTools []string) {
	var sess *serveSession
	var err error
	defer func() {
		// net/http could recover a synchronous factory panic before this worker
		// boundary. Contain it without leaking the panic's potentially private text.
		if recover() != nil {
			err = errors.New("session_construction_panicked")
		}
		s.mu.Lock()
		creation.sess, creation.err, creation.completed = sess, err, true
		s.activeSessionCreations--
		if creation.waiters == 0 {
			delete(s.sessionCreations, key)
		}
		close(creation.done)
		s.mu.Unlock()
		s.endConfigurationAdmission()
	}()
	sess, err = s.buildSession(cwd, channel, disabledTools)
}
