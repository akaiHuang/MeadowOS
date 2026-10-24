package browserworker

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	ProtocolVersion          = 1
	MaxFrameBytes            = 64 << 10
	MaxResultBytes           = 48 << 10
	MaxFramesPerSession      = 4096
	MaxCorrelationsPerSession = 1024
	MaxInflight              = 8
	MaxStatusRecords         = 128
	MaxAllowedOrigins        = 16
	MaxPathBytes             = 4096
	MaxURLBytes              = 2048
	MaxConsoleEvents         = 128
	MaxDOMNodes              = 256
	MaxDOMDepth              = 16
)

const (
	kindHello    = "hello"
	kindHelloAck = "hello_ack"
	kindRequest  = "request"
	kindResult   = "result"
	kindError    = "error"
)

const (
	OperationLoadLocalArtifact  = "load_local_artifact"
	OperationConsoleRuntime     = "console_runtime"
	OperationDOMSummary         = "dom_summary"
	OperationScreenshotMetadata = "screenshot_metadata"
	OperationCancel             = "cancel"
	OperationStatus             = "status"
)

var (
	errFrameTooLarge = errors.New("browser worker frame exceeds limit")
	errInvalidFrame  = errors.New("invalid browser worker frame")
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
	PeerSecret    string   `json:"peer_secret"`
	WorkspaceRoot string   `json:"workspace_root"`
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

func readEnvelope(reader io.Reader) (envelope, error) {
	var sizeBytes [4]byte
	if _, err := io.ReadFull(reader, sizeBytes[:]); err != nil {
		return envelope{}, err
	}
	size := binary.BigEndian.Uint32(sizeBytes[:])
	if size == 0 {
		return envelope{}, errInvalidFrame
	}
	if size > MaxFrameBytes {
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

func writeEnvelope(writer io.Writer, value envelope) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode browser worker frame: %w", err)
	}
	if len(raw) == 0 || len(raw) > MaxFrameBytes {
		return errFrameTooLarge
	}
	var sizeBytes [4]byte
	binary.BigEndian.PutUint32(sizeBytes[:], uint32(len(raw)))
	if _, err := writer.Write(sizeBytes[:]); err != nil {
		return err
	}
	_, err = writer.Write(raw)
	return err
}

func encodePayload(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxResultBytes {
		return nil, errFrameTooLarge
	}
	return raw, nil
}

func decodePayload(raw json.RawMessage, target any) error {
	if len(raw) == 0 || len(raw) > MaxResultBytes {
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

