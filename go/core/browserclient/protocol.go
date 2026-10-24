package browserclient

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/browserworker"
)

type Operation string

const (
	OperationLoad   Operation = Operation(browserworker.OperationLoadLocalArtifact)
	OperationCancel Operation = Operation(browserworker.OperationCancel)
	OperationStatus Operation = Operation(browserworker.OperationStatus)
)

type InspectKind string

const (
	InspectConsoleRuntime     InspectKind = InspectKind(browserworker.OperationConsoleRuntime)
	InspectDOMSummary         InspectKind = InspectKind(browserworker.OperationDOMSummary)
	InspectScreenshotMetadata InspectKind = InspectKind(browserworker.OperationScreenshotMetadata)
)

const (
	kindHello    = "hello"
	kindHelloAck = "hello_ack"
	kindRequest  = "request"
	kindResult   = "result"
	kindError    = "error"
)

var (
	errFrameTooLarge = errors.New("browser client frame exceeds limit")
	errInvalidFrame  = errors.New("invalid browser client frame")
	correlationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	artifactIDPattern = regexp.MustCompile(`^artifact-[0-9a-f]{16}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	safeDOMToken = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	wireErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

type envelope struct {
	Version       int             `json:"version"`
	Kind          string          `json:"kind"`
	Sequence      uint32          `json:"sequence"`
	CorrelationID string          `json:"correlation_id"`
	Operation     string          `json:"operation,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Error         *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type helloRequest struct {
	PeerSecret     string   `json:"peer_secret"`
	WorkspaceRoot  string   `json:"workspace_root"`
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
}

type helloResult struct {
	Capabilities []string       `json:"capabilities"`
	Limits       protocolLimits `json:"limits"`
}

type protocolLimits struct {
	FrameBytes       int `json:"frame_bytes"`
	ResultBytes      int `json:"result_bytes"`
	FramesPerSession int `json:"frames_per_session"`
	Correlations     int `json:"correlations_per_session"`
	Inflight         int `json:"inflight"`
	ConsoleEvents    int `json:"console_events"`
	DOMNodes         int `json:"dom_nodes"`
	DOMDepth         int `json:"dom_depth"`
}

type Limits struct {
	FrameBytes       int
	ResultBytes      int
	FramesPerSession int
	Correlations     int
	Inflight         int
	ConsoleEvents    int
	DOMNodes         int
	DOMDepth         int
}

type LoadRequest struct {
	// CorrelationID may be preallocated with NewCorrelationID when another
	// goroutine must cancel or inspect this request while it is running.
	CorrelationID string
	Path          string
}

type LoadResult struct {
	CorrelationID string
	ArtifactID    string
	Ready         bool
	ContentType   string
}

type InspectRequest struct {
	CorrelationID string
	Kind          InspectKind
	MaxEvents     int
	MaxNodes      int
	MaxDepth      int
}

type InspectResult struct {
	CorrelationID string
	Kind          InspectKind
	Console       *ConsoleRuntimeResult
	DOM           *DOMSummaryResult
	Screenshot    *ScreenshotMetadataResult
}

type CancelResult struct {
	CorrelationID string
	State         string
}

type StatusResult struct {
	CorrelationID string
	State         string
	Operation     Operation
	Active        int
	Retained      int
}

type ConsoleRuntimeResult = browserworker.ConsoleRuntimeResult
type RuntimeSnapshot = browserworker.RuntimeSnapshot
type ConsoleEvent = browserworker.ConsoleEvent
type DOMSummaryResult = browserworker.DOMSummaryResult
type DOMNode = browserworker.DOMNode
type DOMBounds = browserworker.DOMBounds
type ScreenshotMetadataResult = browserworker.ScreenshotMetadataResult

type loadLocalArtifactRequest struct {
	Path string `json:"path"`
}

type loadLocalArtifactResult struct {
	ArtifactID  string `json:"artifact_id"`
	Ready       bool   `json:"ready"`
	ContentType string `json:"content_type"`
}

type consoleRuntimeRequest struct {
	MaxEvents int `json:"max_events"`
}

type domSummaryRequest struct {
	MaxNodes int `json:"max_nodes"`
	MaxDepth int `json:"max_depth"`
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

func NewCorrelationID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", clientError(ErrorUnavailable, "", "", "generate browser correlation", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded), nil
}

func readEnvelope(reader io.Reader) (envelope, error) {
	var sizeBytes [4]byte
	if _, err := io.ReadFull(reader, sizeBytes[:]); err != nil {
		return envelope{}, err
	}
	size := binary.BigEndian.Uint32(sizeBytes[:])
	if size == 0 {
		return envelope{}, errInvalidFrame
	}
	if size > browserworker.MaxFrameBytes {
		return envelope{}, errFrameTooLarge
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return envelope{}, err
	}
	var value envelope
	if err := decodeStrict(raw, &value); err != nil {
		return envelope{}, errInvalidFrame
	}
	return value, nil
}

func marshalEnvelope(value envelope) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errInvalidFrame
	}
	if len(raw) == 0 || len(raw) > browserworker.MaxFrameBytes {
		return nil, errFrameTooLarge
	}
	framed := make([]byte, 4+len(raw))
	binary.BigEndian.PutUint32(framed[:4], uint32(len(raw)))
	copy(framed[4:], raw)
	return framed, nil
}

func encodePayload(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errInvalidFrame
	}
	if len(raw) == 0 || len(raw) > browserworker.MaxResultBytes {
		return nil, errFrameTooLarge
	}
	return raw, nil
}

func decodePayload(raw json.RawMessage, target any) error {
	if len(raw) == 0 || len(raw) > browserworker.MaxResultBytes {
		return errInvalidFrame
	}
	return decodeStrict(raw, target)
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errInvalidFrame
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func validatePrivateConnection(connection net.Conn) (*net.UnixConn, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return nil, clientError(ErrorAuthentication, "", "", "private browser transport rejected", ErrAuthentication)
	}
	if !anonymousUnixAddress(unixConnection.LocalAddr()) || !anonymousUnixAddress(unixConnection.RemoteAddr()) {
		return nil, clientError(ErrorAuthentication, "", "", "named browser socket rejected", ErrAuthentication)
	}
	return unixConnection, nil
}

func anonymousUnixAddress(address net.Addr) bool {
	if address == nil {
		return true
	}
	unixAddress, ok := address.(*net.UnixAddr)
	if !ok || unixAddress.Network() != "unix" {
		return false
	}
	return unixAddress.Name == "" || unixAddress.Name == "@"
}

func validateLoadRequest(request LoadRequest) error {
	if request.CorrelationID != "" && !validCorrelationID(request.CorrelationID) {
		return clientError(ErrorInvalid, OperationLoad, "", "load correlation rejected", ErrInvalid)
	}
	if request.Path == "" || len(request.Path) > browserworker.MaxPathBytes || strings.IndexByte(request.Path, 0) >= 0 ||
		filepath.IsAbs(request.Path) || strings.Contains(request.Path, "://") {
		return clientError(ErrorInvalid, OperationLoad, request.CorrelationID, "load path rejected", ErrInvalid)
	}
	clean := filepath.Clean(request.Path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return clientError(ErrorInvalid, OperationLoad, request.CorrelationID, "load path rejected", ErrInvalid)
	}
	return nil
}

func validateInspectRequest(request InspectRequest) error {
	if request.CorrelationID != "" && !validCorrelationID(request.CorrelationID) {
		return clientError(ErrorInvalid, Operation(request.Kind), "", "inspect correlation rejected", ErrInvalid)
	}
	switch request.Kind {
	case InspectConsoleRuntime:
		if request.MaxEvents <= 0 || request.MaxEvents > browserworker.MaxConsoleEvents {
			return clientError(ErrorLimit, Operation(request.Kind), request.CorrelationID, "console event limit rejected", ErrLimit)
		}
	case InspectDOMSummary:
		if request.MaxNodes <= 0 || request.MaxNodes > browserworker.MaxDOMNodes ||
			request.MaxDepth <= 0 || request.MaxDepth > browserworker.MaxDOMDepth {
			return clientError(ErrorLimit, Operation(request.Kind), request.CorrelationID, "DOM limit rejected", ErrLimit)
		}
	case InspectScreenshotMetadata:
	default:
		return clientError(ErrorInvalid, "", request.CorrelationID, "inspect operation rejected", ErrInvalid)
	}
	return nil
}

func validateHelloResult(result helloResult) error {
	required := map[string]bool{
		browserworker.OperationLoadLocalArtifact: false,
		browserworker.OperationConsoleRuntime: false,
		browserworker.OperationDOMSummary: false,
		browserworker.OperationScreenshotMetadata: false,
		browserworker.OperationCancel: false,
		browserworker.OperationStatus: false,
	}
	if len(result.Capabilities) != len(required) {
		return errInvalidFrame
	}
	for _, capability := range result.Capabilities {
		seen, exists := required[capability]
		if !exists || seen {
			return errInvalidFrame
		}
		required[capability] = true
	}
	limits := result.Limits
	if limits.FrameBytes <= 0 || limits.FrameBytes > browserworker.MaxFrameBytes ||
		limits.ResultBytes <= 0 || limits.ResultBytes > browserworker.MaxResultBytes ||
		limits.FramesPerSession <= 1 || limits.FramesPerSession > browserworker.MaxFramesPerSession ||
		limits.Correlations <= 1 || limits.Correlations > browserworker.MaxCorrelationsPerSession ||
		limits.Inflight <= 0 || limits.Inflight > browserworker.MaxInflight ||
		limits.ConsoleEvents <= 0 || limits.ConsoleEvents > browserworker.MaxConsoleEvents ||
		limits.DOMNodes <= 0 || limits.DOMNodes > browserworker.MaxDOMNodes ||
		limits.DOMDepth <= 0 || limits.DOMDepth > browserworker.MaxDOMDepth {
		return errInvalidFrame
	}
	return nil
}

func validateLoadResult(result loadLocalArtifactResult) error {
	if !artifactIDPattern.MatchString(result.ArtifactID) {
		return errInvalidFrame
	}
	switch result.ContentType {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "application/pdf":
		return nil
	default:
		return errInvalidFrame
	}
}

func validateConsoleResult(result ConsoleRuntimeResult, limit int) error {
	switch result.Runtime.State {
	case "idle", "loading", "ready", "failed", "closed":
	default:
		return errInvalidFrame
	}
	if result.Runtime.JavaScriptContexts < 0 || result.Runtime.JavaScriptContexts > 10000 ||
		result.Runtime.PendingRequests < 0 || result.Runtime.PendingRequests > 100000 ||
		len(result.Events) > limit || len(result.Events) > browserworker.MaxConsoleEvents {
		return errInvalidFrame
	}
	var previous uint32
	for _, event := range result.Events {
		if event.Sequence == 0 || event.Sequence <= previous || event.Line < 0 || event.Line > 10000000 ||
			event.Column < 0 || event.Column > 1000000 {
			return errInvalidFrame
		}
		previous = event.Sequence
		switch event.Level {
		case "debug", "info", "warning", "error":
		default:
			return errInvalidFrame
		}
		switch event.Category {
		case "console", "runtime", "network", "security":
		default:
			return errInvalidFrame
		}
	}
	return nil
}

func validateDOMResult(result DOMSummaryResult, request InspectRequest) error {
	if len(result.Nodes) > request.MaxNodes || len(result.Nodes) > browserworker.MaxDOMNodes {
		return errInvalidFrame
	}
	for position, node := range result.Nodes {
		if node.Index != position || node.ParentIndex < -1 || node.ParentIndex >= position || node.Depth < 0 ||
			node.Depth > request.MaxDepth || node.ChildCount < 0 || node.ChildCount > browserworker.MaxDOMNodes ||
			!safeDOMToken.MatchString(strings.ToLower(node.Tag)) {
			return errInvalidFrame
		}
		if node.Role != "" && !safeDOMToken.MatchString(strings.ToLower(node.Role)) {
			return errInvalidFrame
		}
		if node.Bounds.X < -1000000 || node.Bounds.X > 1000000 || node.Bounds.Y < -1000000 || node.Bounds.Y > 1000000 ||
			node.Bounds.Width < 0 || node.Bounds.Width > 100000 || node.Bounds.Height < 0 || node.Bounds.Height > 100000 {
			return errInvalidFrame
		}
	}
	return nil
}

func validateScreenshotResult(result ScreenshotMetadataResult) error {
	if result.Width <= 0 || result.Width > 32768 || result.Height <= 0 || result.Height > 32768 ||
		result.ByteSize <= 0 || result.ByteSize > 512<<20 || !sha256Pattern.MatchString(result.SHA256) {
		return errInvalidFrame
	}
	switch result.Format {
	case "png", "jpeg", "webp":
		return nil
	default:
		return errInvalidFrame
	}
}

func validateCancelResult(result cancelResult) error {
	switch result.State {
	case "unknown", "running", "cancel_requested", "succeeded", "failed", "cancelled":
		return nil
	default:
		return errInvalidFrame
	}
}

func validateStatusResult(result statusResult, limits protocolLimits) error {
	switch result.State {
	case "summary", "unknown", "running", "succeeded", "failed", "cancelled":
	default:
		return errInvalidFrame
	}
	if result.Active < 0 || result.Active > limits.Inflight || result.Retained < 0 || result.Retained > browserworker.MaxStatusRecords {
		return errInvalidFrame
	}
	if result.Operation != "" && !taskOperation(Operation(result.Operation)) {
		return errInvalidFrame
	}
	if result.State == "summary" && result.Operation != "" {
		return errInvalidFrame
	}
	return nil
}

func validateServerEnvelope(value envelope, expectedSequence uint32) error {
	if value.Version != browserworker.ProtocolVersion || value.Sequence != expectedSequence ||
		value.Sequence == 0 || value.Sequence > browserworker.MaxFramesPerSession ||
		!validCorrelationID(value.CorrelationID) || value.Operation != "" {
		return errInvalidFrame
	}
	switch value.Kind {
	case kindResult:
		if value.Error != nil || len(value.Payload) == 0 || len(value.Payload) > browserworker.MaxResultBytes {
			return errInvalidFrame
		}
	case kindError:
		if value.Error == nil || len(value.Payload) != 0 || !wireErrorCodePattern.MatchString(value.Error.Code) ||
			value.Error.Message == "" || len(value.Error.Message) > 1024 {
			return errInvalidFrame
		}
	default:
		return errInvalidFrame
	}
	return nil
}

func validCorrelationID(value string) bool {
	return len(value) == 36 && correlationIDPattern.MatchString(value)
}

func taskOperation(operation Operation) bool {
	switch operation {
	case OperationLoad, Operation(InspectConsoleRuntime), Operation(InspectDOMSummary), Operation(InspectScreenshotMetadata):
		return true
	default:
		return false
	}
}

func limitsFromWire(value protocolLimits) Limits {
	return Limits{
		FrameBytes: value.FrameBytes, ResultBytes: value.ResultBytes,
		FramesPerSession: value.FramesPerSession, Correlations: value.Correlations,
		Inflight: value.Inflight, ConsoleEvents: value.ConsoleEvents,
		DOMNodes: value.DOMNodes, DOMDepth: value.DOMDepth,
	}
}

func protocolFailure(operation Operation, correlationID, message string) error {
	return clientError(ErrorProtocol, operation, correlationID, message, ErrProtocol)
}

func limitFailure(operation Operation, correlationID, message string) error {
	return clientError(ErrorLimit, operation, correlationID, message, ErrLimit)
}

var _ = fmt.Sprintf
