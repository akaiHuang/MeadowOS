package browserclient

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/browserworker"
)

const (
	defaultHandshakeTimeout = 5 * time.Second
	defaultWriteTimeout     = 5 * time.Second
)

type Config struct {
	PeerSecret      []byte
	WorkspaceRoot   string
	AllowedOrigins  []string
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
}

type Client struct {
	conn         *net.UnixConn
	writeTimeout time.Duration
	capabilities map[string]struct{}
	limits       protocolLimits

	writeMu       sync.Mutex
	nextClientSeq uint32

	mu                sync.Mutex
	expectedServerSeq uint32
	seen              map[string]struct{}
	pending           map[string]*pendingCall
	activeTasks       int
	closing           bool
	failure           error

	done     chan struct{}
	failOnce sync.Once
	wait     sync.WaitGroup
}

type pendingCall struct {
	operation Operation
	task      bool
	response  chan callResponse
}

type callResponse struct {
	payload []byte
	err     error
}

func New(ctx context.Context, connection net.Conn, config Config) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	unixConnection, err := validatePrivateConnection(connection)
	if err != nil {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, err
	}
	normalized, err := normalizeConfig(config)
	if err != nil {
		_ = unixConnection.Close()
		return nil, err
	}
	secret := append([]byte(nil), normalized.PeerSecret...)
	defer clear(secret)

	correlationID, err := NewCorrelationID()
	if err != nil {
		_ = unixConnection.Close()
		return nil, err
	}
	helloPayload, err := encodePayload(helloRequest{
		PeerSecret: base64.RawURLEncoding.EncodeToString(secret),
		WorkspaceRoot: normalized.WorkspaceRoot,
		AllowedOrigins: append([]string(nil), normalized.AllowedOrigins...),
	})
	if err != nil {
		_ = unixConnection.Close()
		return nil, limitFailure("", correlationID, "browser hello exceeded protocol limit")
	}
	hello := envelope{
		Version: browserworker.ProtocolVersion,
		Kind: kindHello,
		Sequence: 1,
		CorrelationID: correlationID,
		Payload: helloPayload,
	}
	deadline := time.Now().Add(normalized.HandshakeTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := ctx.Err(); err != nil {
		_ = unixConnection.Close()
		return nil, contextError("", correlationID, err)
	}
	if err := unixConnection.SetDeadline(deadline); err != nil {
		_ = unixConnection.Close()
		return nil, wrapUnavailable("", correlationID, "set browser handshake deadline", err)
	}
	framed, err := marshalEnvelope(hello)
	if err != nil {
		_ = unixConnection.Close()
		return nil, limitFailure("", correlationID, "browser hello frame rejected")
	}
	if err := writeAll(unixConnection, framed); err != nil {
		_ = unixConnection.Close()
		if ctx.Err() != nil {
			return nil, contextError("", correlationID, ctx.Err())
		}
		return nil, wrapUnavailable("", correlationID, "write browser hello", err)
	}
	ack, err := readEnvelope(unixConnection)
	if err != nil {
		_ = unixConnection.Close()
		if ctx.Err() != nil {
			return nil, contextError("", correlationID, ctx.Err())
		}
		if errors.Is(err, errFrameTooLarge) || errors.Is(err, errInvalidFrame) {
			return nil, protocolFailure("", correlationID, "browser hello response rejected")
		}
		return nil, clientError(ErrorAuthentication, "", correlationID, "browser peer authentication failed", ErrAuthentication)
	}
	if ack.Version != browserworker.ProtocolVersion || ack.Kind != kindHelloAck || ack.Sequence != 1 ||
		ack.CorrelationID != correlationID || ack.Operation != "" || ack.Error != nil {
		_ = unixConnection.Close()
		return nil, protocolFailure("", correlationID, "browser hello response rejected")
	}
	var helloResultValue helloResult
	if decodePayload(ack.Payload, &helloResultValue) != nil || validateHelloResult(helloResultValue) != nil {
		_ = unixConnection.Close()
		return nil, protocolFailure("", correlationID, "browser capabilities rejected")
	}
	if err := unixConnection.SetDeadline(time.Time{}); err != nil {
		_ = unixConnection.Close()
		return nil, wrapUnavailable("", correlationID, "clear browser handshake deadline", err)
	}

	client := &Client{
		conn: unixConnection,
		writeTimeout: normalized.WriteTimeout,
		capabilities: make(map[string]struct{}, len(helloResultValue.Capabilities)),
		limits: helloResultValue.Limits,
		nextClientSeq: 2,
		expectedServerSeq: 2,
		seen: map[string]struct{}{correlationID: {}},
		pending: make(map[string]*pendingCall),
		done: make(chan struct{}),
	}
	for _, capability := range helloResultValue.Capabilities {
		client.capabilities[capability] = struct{}{}
	}
	client.wait.Add(1)
	go client.readLoop()
	return client, nil
}

func (client *Client) Load(ctx context.Context, request LoadRequest) (*LoadResult, error) {
	if err := validateLoadRequest(request); err != nil {
		return nil, err
	}
	payload, correlationID, err := client.call(
		ctx,
		OperationLoad,
		request.CorrelationID,
		loadLocalArtifactRequest{Path: request.Path},
		true,
	)
	if err != nil {
		return nil, err
	}
	var result loadLocalArtifactResult
	if decodePayload(payload, &result) != nil || validateLoadResult(result) != nil {
		err := protocolFailure(OperationLoad, correlationID, "load result rejected")
		client.fail(err)
		return nil, err
	}
	return &LoadResult{
		CorrelationID: correlationID,
		ArtifactID: result.ArtifactID,
		Ready: result.Ready,
		ContentType: result.ContentType,
	}, nil
}

func (client *Client) Inspect(ctx context.Context, request InspectRequest) (*InspectResult, error) {
	if err := validateInspectRequest(request); err != nil {
		return nil, err
	}
	operation := Operation(request.Kind)
	var wireRequest any
	switch request.Kind {
	case InspectConsoleRuntime:
		wireRequest = consoleRuntimeRequest{MaxEvents: request.MaxEvents}
	case InspectDOMSummary:
		wireRequest = domSummaryRequest{MaxNodes: request.MaxNodes, MaxDepth: request.MaxDepth}
	case InspectScreenshotMetadata:
		wireRequest = emptyRequest{}
	}
	payload, correlationID, err := client.call(ctx, operation, request.CorrelationID, wireRequest, true)
	if err != nil {
		return nil, err
	}
	result := &InspectResult{CorrelationID: correlationID, Kind: request.Kind}
	switch request.Kind {
	case InspectConsoleRuntime:
		var value ConsoleRuntimeResult
		if decodePayload(payload, &value) != nil || validateConsoleResult(value, request.MaxEvents) != nil {
			err := protocolFailure(operation, correlationID, "console result rejected")
			client.fail(err)
			return nil, err
		}
		result.Console = &value
	case InspectDOMSummary:
		var value DOMSummaryResult
		if decodePayload(payload, &value) != nil || validateDOMResult(value, request) != nil {
			err := protocolFailure(operation, correlationID, "DOM result rejected")
			client.fail(err)
			return nil, err
		}
		result.DOM = &value
	case InspectScreenshotMetadata:
		var value ScreenshotMetadataResult
		if decodePayload(payload, &value) != nil || validateScreenshotResult(value) != nil {
			err := protocolFailure(operation, correlationID, "screenshot metadata rejected")
			client.fail(err)
			return nil, err
		}
		result.Screenshot = &value
	}
	return result, nil
}

func (client *Client) Cancel(ctx context.Context, targetCorrelationID string) (*CancelResult, error) {
	if !validCorrelationID(targetCorrelationID) {
		return nil, clientError(ErrorInvalid, OperationCancel, "", "cancel target rejected", ErrInvalid)
	}
	payload, correlationID, err := client.call(
		ctx,
		OperationCancel,
		"",
		cancelRequest{CorrelationID: targetCorrelationID},
		false,
	)
	if err != nil {
		return nil, err
	}
	var result cancelResult
	if decodePayload(payload, &result) != nil || validateCancelResult(result) != nil {
		err := protocolFailure(OperationCancel, correlationID, "cancel result rejected")
		client.fail(err)
		return nil, err
	}
	return &CancelResult{CorrelationID: correlationID, State: result.State}, nil
}

// Status returns the worker summary when targetCorrelationID is empty, or the
// retained state for one task correlation otherwise.
func (client *Client) Status(ctx context.Context, targetCorrelationID string) (*StatusResult, error) {
	if targetCorrelationID != "" && !validCorrelationID(targetCorrelationID) {
		return nil, clientError(ErrorInvalid, OperationStatus, "", "status target rejected", ErrInvalid)
	}
	payload, correlationID, err := client.call(
		ctx,
		OperationStatus,
		"",
		statusRequest{CorrelationID: targetCorrelationID},
		false,
	)
	if err != nil {
		return nil, err
	}
	var result statusResult
	if decodePayload(payload, &result) != nil || validateStatusResult(result, client.limits) != nil {
		err := protocolFailure(OperationStatus, correlationID, "status result rejected")
		client.fail(err)
		return nil, err
	}
	return &StatusResult{
		CorrelationID: correlationID,
		State: result.State,
		Operation: Operation(result.Operation),
		Active: result.Active,
		Retained: result.Retained,
	}, nil
}

func (client *Client) Limits() Limits {
	if client == nil {
		return Limits{}
	}
	return limitsFromWire(client.limits)
}

func (client *Client) HasCapability(operation Operation) bool {
	if client == nil {
		return false
	}
	_, ok := client.capabilities[string(operation)]
	return ok
}

func (client *Client) Err() error {
	if client == nil {
		return ErrClosed
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.failure
	}

// Close closes the inherited stream. EOF causes browserworker to cancel every
// active task and wait for task termination; there is no alternate close
// transport or unbounded shutdown operation.
func (client *Client) Close() error {
	if client == nil {
		return nil
	}
	client.mu.Lock()
	client.closing = true
	client.mu.Unlock()
	client.fail(clientError(ErrorClosed, "", "", "browser client closed", ErrClosed))
	client.wait.Wait()
	return nil
}

func (client *Client) call(
	ctx context.Context,
	operation Operation,
	requestedCorrelationID string,
	request any,
	task bool,
) ([]byte, string, error) {
	if client == nil {
		return nil, "", clientError(ErrorClosed, operation, "", "browser client closed", ErrClosed)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, requestedCorrelationID, contextError(operation, requestedCorrelationID, err)
	}
	payload, err := encodePayload(request)
	if err != nil {
		return nil, requestedCorrelationID, limitFailure(operation, requestedCorrelationID, "browser request exceeded protocol limit")
	}
	correlationID, pending, err := client.register(operation, requestedCorrelationID, task)
	if err != nil {
		return nil, correlationID, err
	}
	requestEnvelope := envelope{
		Version: browserworker.ProtocolVersion,
		Kind: kindRequest,
		CorrelationID: correlationID,
		Operation: string(operation),
		Payload: payload,
	}
	if err := client.send(ctx, requestEnvelope); err != nil {
		client.dropPending(correlationID)
		return nil, correlationID, err
	}
	for {
		select {
		case response := <-pending.response:
			return response.payload, correlationID, response.err
		case <-ctx.Done():
			return nil, correlationID, contextError(operation, correlationID, ctx.Err())
		case <-client.done:
			select {
			case response := <-pending.response:
				return response.payload, correlationID, response.err
			default:
				return nil, correlationID, client.connectionError(operation, correlationID)
			}
		}
	}
}

func (client *Client) register(operation Operation, requestedCorrelationID string, task bool) (string, *pendingCall, error) {
	correlationID := requestedCorrelationID
	if correlationID == "" {
		var err error
		correlationID, err = NewCorrelationID()
		if err != nil {
			return "", nil, err
		}
	}
	if !validCorrelationID(correlationID) {
		return correlationID, nil, clientError(ErrorInvalid, operation, correlationID, "request correlation rejected", ErrInvalid)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.failure != nil || client.closing {
		return correlationID, nil, client.connectionErrorLocked(operation, correlationID)
	}
	if len(client.seen) >= client.limits.Correlations {
		return correlationID, nil, limitFailure(operation, correlationID, "browser correlation limit reached")
	}
	if _, exists := client.seen[correlationID]; exists {
		return correlationID, nil, clientError(ErrorInvalid, operation, correlationID, "request correlation already used", ErrInvalid)
	}
	if task && client.activeTasks >= client.limits.Inflight {
		return correlationID, nil, clientError(ErrorBusy, operation, correlationID, "browser worker is at capacity", ErrBusy)
	}
	pending := &pendingCall{
		operation: operation,
		task: task,
		response: make(chan callResponse, 1),
	}
	client.seen[correlationID] = struct{}{}
	client.pending[correlationID] = pending
	if task {
		client.activeTasks++
	}
	return correlationID, pending, nil
}

func (client *Client) dropPending(correlationID string) {
	client.mu.Lock()
	defer client.mu.Unlock()
	pending := client.pending[correlationID]
	if pending == nil {
		return
	}
	delete(client.pending, correlationID)
	if pending.task && client.activeTasks > 0 {
		client.activeTasks--
	}
}

func (client *Client) send(ctx context.Context, value envelope) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return contextError(Operation(value.Operation), value.CorrelationID, err)
	}
	if client.nextClientSeq == 0 || client.nextClientSeq > uint32(client.limits.FramesPerSession) {
		return limitFailure(Operation(value.Operation), value.CorrelationID, "browser request sequence exhausted")
	}
	value.Sequence = client.nextClientSeq
	framed, err := marshalEnvelope(value)
	if err != nil {
		if errors.Is(err, errFrameTooLarge) {
			return limitFailure(Operation(value.Operation), value.CorrelationID, "browser request frame exceeded limit")
		}
		return protocolFailure(Operation(value.Operation), value.CorrelationID, "browser request frame rejected")
	}
	deadline := time.Now().Add(client.writeTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := client.conn.SetWriteDeadline(deadline); err != nil {
		failure := wrapUnavailable(Operation(value.Operation), value.CorrelationID, "set browser write deadline", err)
		client.fail(failure)
		return failure
	}
	if err := writeAll(client.conn, framed); err != nil {
		if ctx.Err() != nil {
			failure := contextError(Operation(value.Operation), value.CorrelationID, ctx.Err())
			client.fail(failure)
			return failure
		}
		failure := wrapUnavailable(Operation(value.Operation), value.CorrelationID, "write browser request", err)
		client.fail(failure)
		return failure
	}
	client.nextClientSeq++
	if err := client.conn.SetWriteDeadline(time.Time{}); err != nil {
		failure := wrapUnavailable(Operation(value.Operation), value.CorrelationID, "clear browser write deadline", err)
		client.fail(failure)
		return failure
	}
	return nil
}

func (client *Client) readLoop() {
	defer client.wait.Done()
	for {
		value, err := readEnvelope(client.conn)
		if err != nil {
			select {
			case <-client.done:
				return
			default:
			}
			if errors.Is(err, errFrameTooLarge) {
				client.fail(limitFailure("", "", "browser response frame exceeded limit"))
			} else if errors.Is(err, errInvalidFrame) {
				client.fail(protocolFailure("", "", "browser response frame rejected"))
			} else if errors.Is(err, io.EOF) {
				client.fail(wrapUnavailable("", "", "read browser response", io.EOF))
			} else {
				client.fail(wrapUnavailable("", "", "read browser response", err))
			}
			return
		}
		client.mu.Lock()
		expectedSequence := client.expectedServerSeq
		client.mu.Unlock()
		if validateServerEnvelope(value, expectedSequence) != nil {
			client.fail(protocolFailure("", value.CorrelationID, "browser response sequence or envelope rejected"))
			return
		}
		client.mu.Lock()
		client.expectedServerSeq++
		pending := client.pending[value.CorrelationID]
		if pending != nil {
			delete(client.pending, value.CorrelationID)
			if pending.task && client.activeTasks > 0 {
				client.activeTasks--
			}
		}
		client.mu.Unlock()
		if pending == nil {
			client.fail(protocolFailure("", value.CorrelationID, "browser response correlation rejected"))
			return
		}
		response := callResponse{payload: append([]byte(nil), value.Payload...)}
		if value.Kind == kindError {
			response.payload = nil
			response.err = remoteError(pending.operation, value.CorrelationID, value.Error)
		}
		select {
		case pending.response <- response:
		case <-client.done:
			return
		default:
			client.fail(protocolFailure(pending.operation, value.CorrelationID, "browser response delivery rejected"))
			return
		}
	}
}

func (client *Client) fail(err error) {
	if err == nil {
		err = clientError(ErrorUnavailable, "", "", "browser worker unavailable", ErrUnavailable)
	}
	client.failOnce.Do(func() {
		client.mu.Lock()
		client.failure = err
		client.closing = true
		client.mu.Unlock()
		close(client.done)
		_ = client.conn.Close()
	})
}

func (client *Client) connectionError(operation Operation, correlationID string) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.connectionErrorLocked(operation, correlationID)
}

func (client *Client) connectionErrorLocked(operation Operation, correlationID string) error {
	if client.failure != nil {
		return client.failure
	}
	return clientError(ErrorClosed, operation, correlationID, "browser client closed", ErrClosed)
}

func normalizeConfig(config Config) (Config, error) {
	if len(config.PeerSecret) < browserworker.MinPeerSecretBytes || len(config.PeerSecret) > browserworker.MaxPeerSecretBytes {
		return Config{}, clientError(ErrorAuthentication, "", "", "browser peer secret rejected", ErrAuthentication)
	}
	if config.WorkspaceRoot == "" || len(config.WorkspaceRoot) > browserworker.MaxPathBytes ||
		strings.IndexByte(config.WorkspaceRoot, 0) >= 0 || !filepath.IsAbs(config.WorkspaceRoot) {
		return Config{}, clientError(ErrorInvalid, "", "", "browser workspace rejected", ErrInvalid)
	}
	if len(config.AllowedOrigins) > browserworker.MaxAllowedOrigins {
		return Config{}, limitFailure("", "", "browser origin limit exceeded")
	}
	for _, origin := range config.AllowedOrigins {
		if origin == "" || len(origin) > browserworker.MaxURLBytes || strings.IndexByte(origin, 0) >= 0 {
			return Config{}, clientError(ErrorInvalid, "", "", "browser origin rejected", ErrInvalid)
		}
	}
	if config.HandshakeTimeout <= 0 {
		config.HandshakeTimeout = defaultHandshakeTimeout
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	config.PeerSecret = append([]byte(nil), config.PeerSecret...)
	config.AllowedOrigins = append([]string(nil), config.AllowedOrigins...)
	return config, nil
}
