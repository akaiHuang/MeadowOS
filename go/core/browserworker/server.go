package browserworker

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"regexp"
	"sync"
	"time"
)

const (
	defaultHandshakeTimeout = 5 * time.Second
	defaultIdleTimeout      = 60 * time.Second
	defaultWriteTimeout     = 5 * time.Second
	MinPeerSecretBytes      = 32
	MaxPeerSecretBytes      = 64
)

var correlationPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Config struct {
	PeerSecret      []byte
	Executor        Executor
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	WriteTimeout     time.Duration
}

type loadLocalArtifactRequest struct {
	Path string `json:"path"`
}

type loadLocalArtifactResult struct {
	ArtifactID string `json:"artifact_id"`
	Ready      bool   `json:"ready"`
	ContentType string `json:"content_type"`
}

type cancelRequest struct {
	CorrelationID string `json:"correlation_id"`
}

type cancelResult struct {
	State string `json:"state"`
}

type statusRequest struct {
	CorrelationID string `json:"correlation_id,omitempty"`
}

type statusResult struct {
	State     string `json:"state"`
	Operation string `json:"operation,omitempty"`
	Active    int    `json:"active"`
	Retained  int    `json:"retained"`
}

type emptyRequest struct{}

type taskRecord struct {
	operation string
	state     string
	cancel    context.CancelFunc
}

type workerSession struct {
	ctx          context.Context
	conn         *net.UnixConn
	executor     Executor
	policy       *workspacePolicy
	idleTimeout  time.Duration
	writeTimeout time.Duration

	writeMu       sync.Mutex
	nextServerSeq uint32

	mu          sync.Mutex
	seen        map[string]struct{}
	records     map[string]*taskRecord
	recordOrder []string
	active      int
	wait        sync.WaitGroup
}

// Serve accepts only an already-connected Unix-domain stream. It cannot be
// called with a TCP listener or TCP connection.
func Serve(ctx context.Context, conn *net.UnixConn, config Config) error {
	if conn == nil || config.Executor == nil || len(config.PeerSecret) < MinPeerSecretBytes || len(config.PeerSecret) > MaxPeerSecretBytes {
		return errors.New("invalid browser worker configuration")
	}
	handshakeTimeout := config.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultHandshakeTimeout
	}
	idleTimeout := config.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultIdleTimeout
	}
	writeTimeout := config.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultWriteTimeout
	}

	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)

	if err := conn.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	helloEnvelope, err := readEnvelope(conn)
	if err != nil {
		return err
	}
	if helloEnvelope.Version != ProtocolVersion || helloEnvelope.Kind != kindHello || helloEnvelope.Sequence != 1 ||
		!validCorrelationID(helloEnvelope.CorrelationID) || helloEnvelope.Operation != "" || helloEnvelope.Error != nil {
		return errInvalidFrame
	}
	var hello helloRequest
	if err := decodePayload(helloEnvelope.Payload, &hello); err != nil {
		return errInvalidFrame
	}
	if !validPeerSecret(hello.PeerSecret, config.PeerSecret) {
		return errors.New("browser worker peer authentication failed")
	}
	policy, err := newWorkspacePolicy(hello.WorkspaceRoot, hello.AllowedOrigins)
	if err != nil {
		return errors.New("browser worker workspace policy rejected")
	}

	session := &workerSession{
		ctx:           ctx,
		conn:          conn,
		executor:      config.Executor,
		policy:        policy,
		idleTimeout:   idleTimeout,
		writeTimeout:  writeTimeout,
		nextServerSeq: 1,
		seen:          map[string]struct{}{helloEnvelope.CorrelationID: {}},
		records:       make(map[string]*taskRecord),
	}
	if err := session.writeResult(kindHelloAck, helloEnvelope.CorrelationID, helloResult{
		Capabilities: []string{
			OperationLoadLocalArtifact,
			OperationConsoleRuntime,
			OperationDOMSummary,
			OperationScreenshotMetadata,
			OperationCancel,
			OperationStatus,
		},
		Limits: protocolLimits{
			FrameBytes: MaxFrameBytes, ResultBytes: MaxResultBytes,
			FramesPerSession: MaxFramesPerSession, Correlations: MaxCorrelationsPerSession,
			Inflight: MaxInflight, ConsoleEvents: MaxConsoleEvents,
			DOMNodes: MaxDOMNodes, DOMDepth: MaxDOMDepth,
		},
	}); err != nil {
		return err
	}

	expectedSequence := uint32(2)
	for expectedSequence <= MaxFramesPerSession {
		if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			break
		}
		request, readErr := readEnvelope(conn)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				err = nil
			} else {
				err = readErr
			}
			break
		}
		if request.Version != ProtocolVersion || request.Kind != kindRequest || request.Sequence != expectedSequence ||
			!validCorrelationID(request.CorrelationID) || request.Operation == "" || request.Error != nil {
			err = errInvalidFrame
			break
		}
		expectedSequence++
		if !session.reserveCorrelation(request.CorrelationID) {
			_ = session.writeError(request.CorrelationID, "correlation_rejected", "request correlation rejected")
			continue
		}
		switch request.Operation {
		case OperationCancel:
			session.handleCancel(request)
		case OperationStatus:
			session.handleStatus(request)
		case OperationLoadLocalArtifact, OperationConsoleRuntime, OperationDOMSummary, OperationScreenshotMetadata:
			if !session.startTask(request) {
				_ = session.writeError(request.CorrelationID, "busy", "browser worker is at capacity")
			}
		default:
			_ = session.writeError(request.CorrelationID, "operation_rejected", "browser operation rejected")
		}
	}
	session.cancelAll()
	session.wait.Wait()
	return err
}

func (session *workerSession) reserveCorrelation(correlationID string) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.seen) >= MaxCorrelationsPerSession {
		return false
	}
	if _, exists := session.seen[correlationID]; exists {
		return false
	}
	session.seen[correlationID] = struct{}{}
	return true
}

func (session *workerSession) startTask(request envelope) bool {
	session.mu.Lock()
	if session.active >= MaxInflight {
		session.mu.Unlock()
		return false
	}
	taskContext, cancel := context.WithCancel(session.ctx)
	session.records[request.CorrelationID] = &taskRecord{
		operation: request.Operation,
		state:     "running",
		cancel:    cancel,
	}
	session.recordOrder = append(session.recordOrder, request.CorrelationID)
	session.active++
	session.wait.Add(1)
	session.mu.Unlock()

	go func() {
		defer session.wait.Done()
		result, taskErr := session.dispatch(taskContext, request)
		state := "succeeded"
		if errors.Is(taskErr, context.Canceled) {
			state = "cancelled"
		} else if taskErr != nil {
			state = "failed"
		}
		session.completeTask(request.CorrelationID, state)
		if taskErr == nil {
			_ = session.writeResult(kindResult, request.CorrelationID, result)
			return
		}
		code, message := publicTaskError(taskErr)
		_ = session.writeError(request.CorrelationID, code, message)
	}()
	return true
}

func (session *workerSession) dispatch(ctx context.Context, request envelope) (any, error) {
	switch request.Operation {
	case OperationLoadLocalArtifact:
		var payload loadLocalArtifactRequest
		if err := decodePayload(request.Payload, &payload); err != nil {
			return nil, errInvalidFrame
		}
		artifact, err := session.policy.resolveLocalArtifact(payload.Path)
		if err != nil {
			return nil, errPolicyDenied
		}
		result, err := session.executor.LoadLocalArtifact(ctx, artifact, session.policy)
		if err != nil {
			return nil, err
		}
		if err := validateLoadResult(result); err != nil {
			return nil, err
		}
		return loadLocalArtifactResult{
			ArtifactID: artifact.ArtifactID(), Ready: result.Ready,
			ContentType: stringsToLower(result.ContentType),
		}, nil
	case OperationConsoleRuntime:
		var payload ConsoleRuntimeRequest
		if err := decodePayload(request.Payload, &payload); err != nil {
			return nil, errInvalidFrame
		}
		if payload.MaxEvents <= 0 || payload.MaxEvents > MaxConsoleEvents {
			return nil, errPolicyDenied
		}
		result, err := session.executor.ConsoleRuntime(ctx, payload)
		if err != nil {
			return nil, err
		}
		if err := validateConsoleRuntimeResult(result, payload.MaxEvents); err != nil {
			return nil, err
		}
		return result, nil
	case OperationDOMSummary:
		var payload DOMSummaryRequest
		if err := decodePayload(request.Payload, &payload); err != nil {
			return nil, errInvalidFrame
		}
		if payload.MaxNodes <= 0 || payload.MaxNodes > MaxDOMNodes || payload.MaxDepth <= 0 || payload.MaxDepth > MaxDOMDepth {
			return nil, errPolicyDenied
		}
		result, err := session.executor.DOMSummary(ctx, payload)
		if err != nil {
			return nil, err
		}
		if err := validateDOMSummaryResult(result, payload); err != nil {
			return nil, err
		}
		return result, nil
	case OperationScreenshotMetadata:
		var payload emptyRequest
		if err := decodePayload(request.Payload, &payload); err != nil {
			return nil, errInvalidFrame
		}
		result, err := session.executor.ScreenshotMetadata(ctx)
		if err != nil {
			return nil, err
		}
		if err := validateScreenshotMetadata(result); err != nil {
			return nil, err
		}
		return result, nil
	default:
		return nil, errPolicyDenied
	}
}

func (session *workerSession) handleCancel(request envelope) {
	var payload cancelRequest
	if decodePayload(request.Payload, &payload) != nil || !validCorrelationID(payload.CorrelationID) || payload.CorrelationID == request.CorrelationID {
		_ = session.writeError(request.CorrelationID, "request_rejected", "cancel request rejected")
		return
	}
	session.mu.Lock()
	record := session.records[payload.CorrelationID]
	state := "unknown"
	if record != nil {
		state = record.state
		if record.state == "running" && record.cancel != nil {
			record.cancel()
			state = "cancel_requested"
		}
	}
	session.mu.Unlock()
	_ = session.writeResult(kindResult, request.CorrelationID, cancelResult{State: state})
}

func (session *workerSession) handleStatus(request envelope) {
	var payload statusRequest
	if decodePayload(request.Payload, &payload) != nil || (payload.CorrelationID != "" && !validCorrelationID(payload.CorrelationID)) {
		_ = session.writeError(request.CorrelationID, "request_rejected", "status request rejected")
		return
	}
	result := statusResult{State: "summary"}
	session.mu.Lock()
	result.Active = session.active
	result.Retained = len(session.records)
	if payload.CorrelationID != "" {
		result.State = "unknown"
		if record := session.records[payload.CorrelationID]; record != nil {
			result.State = record.state
			result.Operation = record.operation
		}
	}
	session.mu.Unlock()
	_ = session.writeResult(kindResult, request.CorrelationID, result)
}

func (session *workerSession) completeTask(correlationID, state string) {
	session.mu.Lock()
	if record := session.records[correlationID]; record != nil {
		record.state = state
		record.cancel = nil
	}
	if session.active > 0 {
		session.active--
	}
	for len(session.records) > MaxStatusRecords && len(session.recordOrder) > 0 {
		oldest := session.recordOrder[0]
		session.recordOrder = session.recordOrder[1:]
		if record := session.records[oldest]; record != nil && record.state != "running" {
			delete(session.records, oldest)
		}
	}
	session.mu.Unlock()
}

func (session *workerSession) cancelAll() {
	session.mu.Lock()
	defer session.mu.Unlock()
	for _, record := range session.records {
		if record.cancel != nil {
			record.cancel()
		}
	}
}

func (session *workerSession) writeResult(kind, correlationID string, result any) error {
	payload, err := encodePayload(result)
	if err != nil {
		return session.writeError(correlationID, "result_rejected", "browser result exceeded policy")
	}
	return session.write(envelope{
		Version: ProtocolVersion, Kind: kind,
		CorrelationID: correlationID, Payload: payload,
	})
}

func (session *workerSession) writeError(correlationID, code, message string) error {
	return session.write(envelope{
		Version: ProtocolVersion, Kind: kindError,
		CorrelationID: correlationID,
		Error: &wireError{Code: code, Message: message},
	})
}

func (session *workerSession) write(value envelope) error {
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	if session.nextServerSeq == 0 || session.nextServerSeq > MaxFramesPerSession {
		return errors.New("browser worker response sequence exhausted")
	}
	value.Sequence = session.nextServerSeq
	session.nextServerSeq++
	if err := session.conn.SetWriteDeadline(time.Now().Add(session.writeTimeout)); err != nil {
		return err
	}
	return writeEnvelope(session.conn, value)
}

func validPeerSecret(encoded string, expected []byte) bool {
	if len(encoded) == 0 || len(encoded) > base64.RawURLEncoding.EncodedLen(MaxPeerSecretBytes) {
		return false
	}
	candidate, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	defer clear(candidate)
	return len(candidate) == len(expected) && subtle.ConstantTimeCompare(candidate, expected) == 1
}

func validCorrelationID(value string) bool {
	return len(value) == 36 && correlationPattern.MatchString(value)
}

func publicTaskError(err error) (string, string) {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled", "browser request cancelled"
	case errors.Is(err, ErrExecutorUnavailable):
		return "browser_unavailable", "browser executor is unavailable"
	case errors.Is(err, errPolicyDenied), errors.Is(err, errInvalidFrame):
		return "request_rejected", "browser request rejected"
	default:
		return "executor_failed", "browser executor failed"
	}
}

func stringsToLower(value string) string {
	buffer := []byte(value)
	for index, character := range buffer {
		if character >= 'A' && character <= 'Z' {
			buffer[index] = character + ('a' - 'A')
		}
	}
	return string(buffer)
}

var _ = json.RawMessage{}

