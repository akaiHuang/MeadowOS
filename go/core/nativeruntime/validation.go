package nativeruntime

import (
	"fmt"
	"math"
	"unicode/utf8"
)

type ProtocolError struct {
	Status  Status
	Context string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("native runtime protocol status %d: %s", e.Status, e.Context)
}

func protocolError(status Status, format string, args ...any) error {
	return &ProtocolError{Status: status, Context: fmt.Sprintf(format, args...)}
}

func ValidateFrame(frame *Frame) error {
	if frame == nil || frame.Payload == nil {
		return protocolError(StatusInvalidArgument, "frame and payload are required")
	}
	if frame.Protocol.Major != ProtocolMajor || frame.Protocol.Minor < 1 || frame.Protocol.Minor > ProtocolMinor {
		return protocolError(StatusUnsupportedProtocol, "protocol %d.%d is outside supported range %d.1-%d.%d", frame.Protocol.Major, frame.Protocol.Minor, ProtocolMajor, ProtocolMajor, ProtocolMinor)
	}
	if frame.Class != FrameClassRequest && frame.Class != FrameClassEvent {
		return protocolError(StatusBadEnum, "invalid frame class %d", frame.Class)
	}
	if frame.Flags != 0 {
		return protocolError(StatusBadHeader, "frame flags must be zero")
	}
	if frame.Sequence == 0 {
		return protocolError(StatusSequenceError, "sender sequence starts at one")
	}
	if frame.CorrelationID == 0 {
		return protocolError(StatusBadHeader, "correlation id must be non-zero")
	}
	kind := frame.Payload.Kind()
	if !validPayloadKindV12(kind) {
		return protocolError(StatusBadEnum, "unknown payload kind 0x%x", uint32(kind))
	}
	if frame.Protocol.Minor < 2 && payloadRequiresV12(frame.Payload) {
		return protocolError(StatusUnsupportedProtocol, "payload 0x%x requires protocol 1.2", uint32(kind))
	}
	if frame.Class == FrameClassRequest && !isRequestKindV12(kind) {
		return protocolError(StatusInvalidState, "event payload 0x%x carried by request frame", uint32(kind))
	}
	if frame.Class == FrameClassEvent && isRequestKindV12(kind) {
		return protocolError(StatusInvalidState, "request payload 0x%x carried by event frame", uint32(kind))
	}
	if requiresStreamIDV12(kind) && frame.StreamID == 0 {
		return protocolError(StatusInvalidArgument, "payload 0x%x requires a non-zero stream id", uint32(kind))
	}
	return ValidatePayload(frame.Payload)
}

func ValidatePayload(payload Payload) error {
	if payload == nil {
		return protocolError(StatusInvalidArgument, "payload is required")
	}
	if handled, err := validatePayloadV12(payload); handled || err != nil { return err }
	switch p := payload.(type) {
	case *HandshakeRequest:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil handshake request")
		}
		if p.ClientABIMajor != ABIMajor {
			return protocolError(StatusUnsupportedABI, "client ABI major %d is unsupported", p.ClientABIMajor)
		}
		if !versionRangeIntersectsSupported(p.MinProtocolMajor, p.MinProtocolMinor, p.MaxProtocolMajor, p.MaxProtocolMinor) {
			return protocolError(StatusUnsupportedProtocol, "client protocol range excludes %d.1-%d.%d", ProtocolMajor, ProtocolMajor, ProtocolMinor)
		}
		if err := validateCapabilities(p.RequiredCapabilities); err != nil {
			return err
		}
		return validateIdentifier("client build id", p.ClientBuildID, MaxBuildIDBytes)
	case *HandshakeResponse:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil handshake response")
		}
		if p.SelectedProtocolMajor != uint32(ProtocolMajor) || p.SelectedProtocolMinor < 1 || p.SelectedProtocolMinor > uint32(ProtocolMinor) {
			return protocolError(StatusUnsupportedProtocol, "selected protocol is outside %d.1-%d.%d", ProtocolMajor, ProtocolMajor, ProtocolMinor)
		}
		if p.RuntimeABIMajor != ABIMajor {
			return protocolError(StatusUnsupportedABI, "runtime ABI major %d is unsupported", p.RuntimeABIMajor)
		}
		return validateIdentifier("runtime build id", p.RuntimeBuildID, MaxBuildIDBytes)
	case *CapabilityRequest:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil capability request")
		}
		if err := validateCapabilities(p.Required | p.Desired); err != nil {
			return err
		}
		if !p.Desired.Has(p.Required) {
			return protocolError(StatusCapabilityDenied, "required capabilities are not a subset of desired capabilities")
		}
		return nil
	case *CapabilityResponse:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil capability response")
		}
		if err := validateCapabilities(p.Offered | p.Negotiated); err != nil {
			return err
		}
		if !p.Offered.Has(p.Negotiated) {
			return protocolError(StatusCapabilityDenied, "negotiated capabilities are not a subset of offered capabilities")
		}
		return nil
	case *HealthRequest:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil health request")
		}
		return nil
	case *HealthResponse:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil health response")
		}
		if !validHealthState(p.State) || !validModelState(p.ModelState) {
			return protocolError(StatusBadEnum, "invalid health or model state")
		}
		if p.ActiveRequests > MaxActiveRequests {
			return protocolError(StatusResourceLimit, "active request count exceeds %d", MaxActiveRequests)
		}
		if p.Message != nil {
			if err := validateText("health message", *p.Message, MaxProgressMessageBytes, true); err != nil {
				return err
			}
		}
		if p.ModelID != nil {
			return validateIdentifier("model id", *p.ModelID, MaxModelIDBytes)
		}
		return nil
	case *ModelLoad:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil model load")
		}
		return validateModelSpec(p.ModelID, p.ProfileDigest, p.Backend, p.MemoryLimitBytes)
	case *ModelUnload:
		if p == nil || p.ExpectedGeneration == 0 {
			return protocolError(StatusInvalidArgument, "model unload requires a non-zero expected generation")
		}
		return nil
	case *ModelSwitch:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil model switch")
		}
		if p.ExpectedGeneration == 0 {
			return protocolError(StatusInvalidArgument, "model switch requires a non-zero expected generation")
		}
		return validateModelSpec(p.ModelID, p.ProfileDigest, p.Backend, p.MemoryLimitBytes)
	case *ModelStatePayload:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil model state")
		}
		if !validModelState(p.State) {
			return protocolError(StatusBadEnum, "invalid model state %d", p.State)
		}
		if p.Generation == 0 {
			return protocolError(StatusInvalidArgument, "model generation must be non-zero")
		}
		if p.State == ModelEmpty {
			if p.ModelID != nil || p.Backend != nil {
				return protocolError(StatusInvalidState, "empty model state cannot carry model id or backend")
			}
		} else if p.ModelID == nil {
			return protocolError(StatusMissingField, "non-empty model state requires model id")
		}
		if p.ModelID != nil {
			if err := validateIdentifier("model id", *p.ModelID, MaxModelIDBytes); err != nil {
				return err
			}
		}
		if p.Backend != nil && !validBackend(*p.Backend) {
			return protocolError(StatusBadEnum, "invalid backend %d", *p.Backend)
		}
		if p.State == ModelFailed {
			if p.ErrorCode == nil || *p.ErrorCode == 0 || p.ErrorMessage == nil {
				return protocolError(StatusMissingField, "failed model state requires non-zero error code and message")
			}
			return validateText("model error message", *p.ErrorMessage, MaxErrorMessageBytes, false)
		}
		if p.ErrorCode != nil || p.ErrorMessage != nil {
			return protocolError(StatusInvalidState, "model error fields are only valid for failed state")
		}
		return nil
	case *TextRequest:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil text request")
		}
		return validateInference(p.Prompt, p.System, p.MaxOutputTokens, p.Temperature, p.TopP, p.SessionID)
	case *VisionRequest:
		if p == nil {
			return protocolError(StatusInvalidArgument, "nil vision request")
		}
		if err := validateInference(p.Prompt, p.System, p.MaxOutputTokens, p.Temperature, p.TopP, p.SessionID); err != nil {
			return err
		}
		return validateImages(p.Images)
	case *CancelRequest:
		if p == nil || p.TargetStreamID == 0 {
			return protocolError(StatusInvalidArgument, "cancel requires a non-zero target stream id")
		}
		if !validCancelReason(p.Reason) {
			return protocolError(StatusBadEnum, "invalid cancel reason %d", p.Reason)
		}
		if p.Message != nil {
			return validateText("cancel message", *p.Message, MaxProgressMessageBytes, true)
		}
		return nil
	case *StreamToken:
		if p == nil || !validStreamChannel(p.Channel) {
			return protocolError(StatusBadEnum, "invalid token stream channel")
		}
		if p.Text != nil {
			if err := validateText("token text", *p.Text, MaxTextBytes, true); err != nil {
				return err
			}
		}
		if p.Logprob != nil && !finite(*p.Logprob) {
			return protocolError(StatusInvalidArgument, "token logprob must be finite")
		}
		return nil
	case *StreamTool:
		if p == nil || !validToolEvent(p.EventKind) {
			return protocolError(StatusBadEnum, "invalid tool event kind")
		}
		if err := validateIdentifier("tool call id", p.CallID, MaxToolCallIDBytes); err != nil {
			return err
		}
		if p.Name != nil {
			if err := validateIdentifier("tool name", *p.Name, MaxToolNameBytes); err != nil {
				return err
			}
		}
		if p.Data != nil {
			if err := validateText("tool data", *p.Data, MaxToolBytes, true); err != nil {
				return err
			}
		}
		if p.ArgumentsDelta != nil {
			if err := validateText("tool arguments delta", *p.ArgumentsDelta, MaxToolBytes, false); err != nil {
				return err
			}
		}
		if p.Data != nil && p.ArgumentsDelta != nil {
			return protocolError(StatusInvalidState, "legacy tool data and v1.2 arguments delta are mutually exclusive")
		}
		if p.EventKind == ToolBegin && p.Name == nil {
			return protocolError(StatusMissingField, "tool begin requires tool name")
		}
		if p.EventKind == ToolArgumentDelta && (p.Data == nil || *p.Data == "") && (p.ArgumentsDelta == nil || *p.ArgumentsDelta == "") {
			return protocolError(StatusMissingField, "tool argument delta requires non-empty legacy data or v1.2 arguments delta")
		}
		return nil
	case *StreamProgress:
		if p == nil || !validProgressPhase(p.Phase) {
			return protocolError(StatusBadEnum, "invalid progress phase")
		}
		if !finite(p.Fraction) || p.Fraction < 0 || p.Fraction > 1 {
			return protocolError(StatusInvalidArgument, "progress fraction must be in [0,1]")
		}
		if p.Message != nil {
			if err := validateText("progress message", *p.Message, MaxProgressMessageBytes, true); err != nil {
				return err
			}
		}
		if (p.Current == nil) != (p.Total == nil) {
			return protocolError(StatusMissingField, "progress current and total must appear together")
		}
		if p.Total != nil && (*p.Total == 0 || *p.Current > *p.Total) {
			return protocolError(StatusInvalidArgument, "invalid progress current/total")
		}
		return nil
	case *StreamError:
		if p == nil || p.Code == 0 {
			return protocolError(StatusInvalidArgument, "stream error requires non-zero code")
		}
		if err := validateIdentifier("error domain", p.Domain, MaxErrorDomainBytes); err != nil {
			return err
		}
		if err := validateText("error message", p.Message, MaxErrorMessageBytes, false); err != nil {
			return err
		}
		if p.Details != nil {
			return validateText("error details", *p.Details, MaxErrorMessageBytes, true)
		}
		return nil
	case *StreamCompletion:
		if p == nil || !validFinishReason(p.Reason) {
			return protocolError(StatusBadEnum, "invalid finish reason")
		}
		if p.Message != nil {
			if err := validateText("completion message", *p.Message, MaxErrorMessageBytes, true); err != nil {
				return err
			}
		}
		if p.CachedInputTokens != nil && *p.CachedInputTokens > p.InputTokens {
			return protocolError(StatusInvalidArgument, "cached input tokens exceed input tokens")
		}
		if p.AcceptedDraftTokens != nil {
			if p.DraftTokens == nil || *p.AcceptedDraftTokens > *p.DraftTokens {
				return protocolError(StatusInvalidArgument, "accepted draft tokens require and cannot exceed draft tokens")
			}
		}
		return nil
	default:
		return protocolError(StatusInvalidArgument, "payload type %T is not part of ABI v1.1", payload)
	}
}

func validateInference(prompt string, system *string, maxOutput uint32, temperature, topP *float64, sessionID *string) error {
	if err := validateText("prompt", prompt, MaxTextBytes, false); err != nil {
		return err
	}
	total := uint64(len(prompt))
	if system != nil {
		if err := validateText("system", *system, MaxTextBytes, true); err != nil {
			return err
		}
		total += uint64(len(*system))
	}
	if total > MaxTextBytes {
		return protocolError(StatusResourceLimit, "combined system and prompt exceed %d bytes", MaxTextBytes)
	}
	if maxOutput == 0 || maxOutput > MaxOutputTokens {
		return protocolError(StatusResourceLimit, "max output tokens must be in [1,%d]", MaxOutputTokens)
	}
	if temperature != nil && (!finite(*temperature) || *temperature < 0 || *temperature > 5) {
		return protocolError(StatusInvalidArgument, "temperature must be finite and in [0,5]")
	}
	if topP != nil && (!finite(*topP) || *topP <= 0 || *topP > 1) {
		return protocolError(StatusInvalidArgument, "top-p must be finite and in (0,1]")
	}
	if sessionID != nil {
		return validateText("session id", *sessionID, MaxSessionIDBytes, false)
	}
	return nil
}

func validateImages(images []EncodedImage) error {
	if len(images) == 0 || len(images) > MaxImages {
		return protocolError(StatusResourceLimit, "vision request requires 1..%d inline images", MaxImages)
	}
	total := uint64(ImageListHeaderBytes)
	for i, image := range images {
		if !validImageFormat(image.Format) {
			return protocolError(StatusBadEnum, "image %d has invalid format %d", i, image.Format)
		}
		if image.Width == 0 || image.Height == 0 || image.Width > MaxImageDimension || image.Height > MaxImageDimension {
			return protocolError(StatusResourceLimit, "image %d dimensions are invalid", i)
		}
		if len(image.Data) == 0 || uint64(len(image.Data)) > MaxImageBytes {
			return protocolError(StatusResourceLimit, "image %d encoded data is invalid", i)
		}
		total += uint64(align8(ImageRecordHeaderBytes + len(image.Data)))
		if total > MaxImageBytes {
			return protocolError(StatusResourceLimit, "encoded image list exceeds %d bytes", MaxImageBytes)
		}
	}
	return nil
}

func validateModelSpec(modelID string, digest [32]byte, backend Backend, memoryLimit uint64) error {
	if err := validateIdentifier("model id", modelID, MaxModelIDBytes); err != nil {
		return err
	}
	if !validBackend(backend) {
		return protocolError(StatusBadEnum, "invalid backend %d", backend)
	}
	if memoryLimit == 0 {
		return protocolError(StatusInvalidArgument, "memory limit must be non-zero")
	}
	var nonzero byte
	for _, value := range digest {
		nonzero |= value
	}
	if nonzero == 0 {
		return protocolError(StatusInvalidArgument, "model profile digest must be a non-zero SHA-256 value")
	}
	return nil
}

func validateText(name, value string, max uint64, allowEmpty bool) error {
	if !allowEmpty && value == "" {
		return protocolError(StatusInvalidArgument, "%s must not be empty", name)
	}
	if uint64(len(value)) > max {
		return protocolError(StatusResourceLimit, "%s exceeds %d bytes", name, max)
	}
	if !utf8.ValidString(value) {
		return protocolError(StatusBadUTF8, "%s is not valid UTF-8", name)
	}
	for _, r := range value {
		if r == 0 {
			return protocolError(StatusBadUTF8, "%s contains NUL", name)
		}
	}
	return nil
}

func validateIdentifier(name, value string, max int) error {
	if err := validateText(name, value, uint64(max), false); err != nil {
		return err
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return protocolError(StatusInvalidArgument, "%s contains a control character", name)
		}
	}
	return nil
}

func validateCapabilities(mask CapabilityMask) error {
	if mask&^KnownCapabilities != 0 {
		return protocolError(StatusCapabilityDenied, "unknown capability bits 0x%x", uint64(mask&^KnownCapabilities))
	}
	return nil
}

func versionRangeContains(minMajor, minMinor, maxMajor, maxMinor, major, minor uint32) bool {
	if minMajor > maxMajor || (minMajor == maxMajor && minMinor > maxMinor) {
		return false
	}
	belowMin := major < minMajor || (major == minMajor && minor < minMinor)
	aboveMax := major > maxMajor || (major == maxMajor && minor > maxMinor)
	return !belowMin && !aboveMax
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func validPayloadKind(kind PayloadKind) bool {
	switch kind {
	case PayloadHandshakeRequest, PayloadCapabilityRequest, PayloadHealthRequest,
		PayloadModelLoad, PayloadModelUnload, PayloadModelSwitch,
		PayloadTextRequest, PayloadVisionRequest, PayloadCancelRequest,
		PayloadHandshakeResponse, PayloadCapabilityResponse, PayloadHealthResponse,
		PayloadModelState, PayloadStreamToken, PayloadStreamTool,
		PayloadStreamProgress, PayloadStreamError, PayloadStreamCompletion:
		return true
	default:
		return false
	}
}

func isRequestKind(kind PayloadKind) bool { return kind < 0x8000 }

func requiresStreamID(kind PayloadKind) bool {
	switch kind {
	case PayloadTextRequest, PayloadVisionRequest, PayloadStreamToken, PayloadStreamTool,
		PayloadStreamProgress, PayloadStreamError, PayloadStreamCompletion:
		return true
	default:
		return false
	}
}

func validBackend(v Backend) bool { return v >= BackendAuto && v <= BackendMTPLX }
func validModelState(v ModelLifecycleState) bool { return v >= ModelEmpty && v <= ModelFailed }
func validHealthState(v HealthState) bool { return v >= HealthStarting && v <= HealthStopping }
func validStreamChannel(v StreamChannel) bool { return v >= StreamAnswer && v <= StreamCode }
func validToolEvent(v ToolEventKind) bool { return v >= ToolBegin && v <= ToolEnd }
func validProgressPhase(v ProgressPhase) bool { return v >= ProgressQueued && v <= ProgressVisionEncode }
func validFinishReason(v FinishReason) bool { return v >= FinishStop && v <= FinishToolCall }
func validCancelReason(v CancelReason) bool { return v >= CancelUser && v <= CancelShutdown }
func validImageFormat(v ImageFormat) bool { return v >= ImageJPEG && v <= ImageAVIF }

func align8(value int) int { return (value + 7) &^ 7 }
