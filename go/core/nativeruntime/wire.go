package nativeruntime

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"sort"
	"unicode/utf8"
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

type wireField struct {
	id    FieldID
	typ   WireType
	flags FieldFlags
	value []byte
}

type fieldRule struct {
	typ        WireType
	required   bool
	exactBytes int
}

func EncodeFrame(frame *Frame) ([]byte, error) {
	if err := ValidateFrame(frame); err != nil {
		return nil, err
	}
	payload, err := encodePayload(frame.Payload)
	if err != nil {
		return nil, err
	}
	if uint64(len(payload)) > MaxFramePayloadBytes {
		return nil, protocolError(StatusResourceLimit, "frame payload exceeds %d bytes", MaxFramePayloadBytes)
	}
	out := make([]byte, FrameHeaderBytes+len(payload))
	binary.LittleEndian.PutUint32(out[0:4], Magic)
	binary.LittleEndian.PutUint16(out[4:6], FrameHeaderBytes)
	binary.LittleEndian.PutUint16(out[6:8], frame.Protocol.Major)
	binary.LittleEndian.PutUint16(out[8:10], frame.Protocol.Minor)
	binary.LittleEndian.PutUint16(out[10:12], uint16(frame.Class))
	binary.LittleEndian.PutUint32(out[12:16], frame.Flags)
	binary.LittleEndian.PutUint64(out[16:24], frame.StreamID)
	binary.LittleEndian.PutUint64(out[24:32], frame.Sequence)
	binary.LittleEndian.PutUint64(out[32:40], frame.CorrelationID)
	binary.LittleEndian.PutUint32(out[40:44], uint32(frame.Payload.Kind()))
	binary.LittleEndian.PutUint32(out[44:48], 0)
	binary.LittleEndian.PutUint64(out[48:56], uint64(len(payload)))
	copy(out[FrameHeaderBytes:], payload)
	binary.LittleEndian.PutUint32(out[60:64], crc32.Checksum(payload, crc32cTable))
	binary.LittleEndian.PutUint32(out[56:60], crc32.Checksum(out[:FrameHeaderBytes], crc32cTable))
	return out, nil
}

func DecodeFrame(data []byte) (*Frame, error) {
	if len(data) < FrameHeaderBytes {
		return nil, protocolError(StatusBufferTooSmall, "frame is shorter than %d-byte header", FrameHeaderBytes)
	}
	if binary.LittleEndian.Uint32(data[0:4]) != Magic {
		return nil, protocolError(StatusBadMagic, "frame magic is not MRT1")
	}
	if binary.LittleEndian.Uint16(data[4:6]) != FrameHeaderBytes {
		return nil, protocolError(StatusBadHeader, "frame header size is not %d", FrameHeaderBytes)
	}
	if binary.LittleEndian.Uint32(data[44:48]) != 0 {
		return nil, protocolError(StatusBadHeader, "reserved header word must be zero")
	}
	payloadBytes := binary.LittleEndian.Uint64(data[48:56])
	if payloadBytes > MaxFramePayloadBytes {
		return nil, protocolError(StatusResourceLimit, "frame payload exceeds %d bytes", MaxFramePayloadBytes)
	}
	if payloadBytes != uint64(len(data)-FrameHeaderBytes) {
		return nil, protocolError(StatusBadLength, "frame payload length does not match buffer")
	}
	header := make([]byte, FrameHeaderBytes)
	copy(header, data[:FrameHeaderBytes])
	wantHeaderCRC := binary.LittleEndian.Uint32(header[56:60])
	for i := 56; i < 60; i++ {
		header[i] = 0
	}
	if crc32.Checksum(header, crc32cTable) != wantHeaderCRC {
		return nil, protocolError(StatusBadChecksum, "header CRC32C mismatch")
	}
	payloadData := data[FrameHeaderBytes:]
	if crc32.Checksum(payloadData, crc32cTable) != binary.LittleEndian.Uint32(data[60:64]) {
		return nil, protocolError(StatusBadChecksum, "payload CRC32C mismatch")
	}
	kind := PayloadKind(binary.LittleEndian.Uint32(data[40:44]))
	if !validPayloadKindV12(kind) {
		return nil, protocolError(StatusBadEnum, "unknown payload kind 0x%x", uint32(kind))
	}
	payload, err := decodePayload(kind, payloadData)
	if err != nil {
		return nil, err
	}
	frame := &Frame{
		Protocol: Version{
			Major: binary.LittleEndian.Uint16(data[6:8]),
			Minor: binary.LittleEndian.Uint16(data[8:10]),
		},
		Class:         FrameClass(binary.LittleEndian.Uint16(data[10:12])),
		Flags:         binary.LittleEndian.Uint32(data[12:16]),
		StreamID:      binary.LittleEndian.Uint64(data[16:24]),
		Sequence:      binary.LittleEndian.Uint64(data[24:32]),
		CorrelationID: binary.LittleEndian.Uint64(data[32:40]),
		Payload:       payload,
	}
	if err := ValidateFrame(frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func encodePayload(payload Payload) ([]byte, error) {
	if err := ValidatePayload(payload); err != nil {
		return nil, err
	}
	fields := make([]wireField, 0, 12)
	switch p := payload.(type) {
	case *HandshakeRequest:
		fields = append(fields,
			requiredU32(FieldClientABIMajor, p.ClientABIMajor), requiredU32(FieldClientABIMinor, p.ClientABIMinor),
			requiredU32(FieldMinProtocolMajor, p.MinProtocolMajor), requiredU32(FieldMinProtocolMinor, p.MinProtocolMinor),
			requiredU32(FieldMaxProtocolMajor, p.MaxProtocolMajor), requiredU32(FieldMaxProtocolMinor, p.MaxProtocolMinor),
			requiredU64(FieldRequiredCapabilities, uint64(p.RequiredCapabilities)), requiredUTF8(FieldClientBuildID, p.ClientBuildID),
			requiredBytes(FieldClientNonce, p.ClientNonce[:]))
	case *HandshakeResponse:
		fields = append(fields,
			requiredU32(FieldSelectedProtocolMajor, p.SelectedProtocolMajor), requiredU32(FieldSelectedProtocolMinor, p.SelectedProtocolMinor),
			requiredU32(FieldRuntimeABIMajor, p.RuntimeABIMajor), requiredU32(FieldRuntimeABIMinor, p.RuntimeABIMinor),
			requiredUTF8(FieldRuntimeBuildID, p.RuntimeBuildID), requiredBytes(FieldRuntimeNonce, p.RuntimeNonce[:]))
	case *CapabilityRequest:
		fields = append(fields, requiredU64(FieldRequiredCapabilities, uint64(p.Required)), requiredU64(FieldDesiredCapabilities, uint64(p.Desired)))
	case *CapabilityResponse:
		fields = append(fields, requiredU64(FieldOfferedCapabilities, uint64(p.Offered)), requiredU64(FieldNegotiatedCapabilities, uint64(p.Negotiated)))
	case *HealthRequest:
	case *HealthResponse:
		fields = append(fields, requiredU32(FieldHealthState, uint32(p.State)), requiredU64(FieldUptimeMS, p.UptimeMS),
			requiredU32(FieldActiveRequests, p.ActiveRequests), requiredU64(FieldResidentBytes, p.ResidentBytes),
			requiredU64(FieldRuntimeGeneration, p.RuntimeGeneration), requiredU32(FieldModelState, uint32(p.ModelState)))
		fields = appendOptionalString(fields, FieldHealthMessage, p.Message)
		fields = appendOptionalString(fields, FieldModelID, p.ModelID)
	case *ModelLoad:
		fields = append(fields, requiredUTF8(FieldModelID, p.ModelID), requiredBytes(FieldModelProfileDigest, p.ProfileDigest[:]),
			requiredU32(FieldBackend, uint32(p.Backend)), requiredU64(FieldMemoryLimitBytes, p.MemoryLimitBytes))
	case *ModelUnload:
		fields = append(fields, requiredU64(FieldExpectedModelGeneration, p.ExpectedGeneration))
	case *ModelSwitch:
		fields = append(fields, requiredUTF8(FieldModelID, p.ModelID), requiredBytes(FieldModelProfileDigest, p.ProfileDigest[:]),
			requiredU32(FieldBackend, uint32(p.Backend)), requiredU64(FieldMemoryLimitBytes, p.MemoryLimitBytes),
			requiredU64(FieldExpectedModelGeneration, p.ExpectedGeneration))
	case *ModelStatePayload:
		fields = append(fields, requiredU64(FieldModelGeneration, p.Generation), requiredU32(FieldModelState, uint32(p.State)), requiredU64(FieldModelResidentBytes, p.ResidentBytes))
		fields = appendOptionalString(fields, FieldModelID, p.ModelID)
		if p.Backend != nil {
			fields = append(fields, optionalU32(FieldBackend, uint32(*p.Backend)))
		}
		if p.ErrorCode != nil {
			fields = append(fields, optionalU32(FieldModelErrorCode, *p.ErrorCode))
		}
		fields = appendOptionalString(fields, FieldModelErrorMessage, p.ErrorMessage)
	case *TextRequest:
		fields = encodeInferenceFields(fields, p.Prompt, p.System, p.MaxOutputTokens, p.Temperature, p.TopP, p.Seed, p.ThinkMode, p.SessionID)
	case *VisionRequest:
		fields = encodeInferenceFields(fields, p.Prompt, p.System, p.MaxOutputTokens, p.Temperature, p.TopP, p.Seed, p.ThinkMode, p.SessionID)
		imageList, err := encodeImageList(p.Images)
		if err != nil {
			return nil, err
		}
		fields = append(fields, requiredBytes(FieldImageList, imageList))
	case *ChatRequest:
		messages, err := encodeMessageList(p.Messages)
		if err != nil { return nil, err }
		fields = append(fields, requiredUTF8(FieldModelID, p.ModelID), requiredU64(FieldExpectedModelGeneration, p.ExpectedModelGeneration), requiredBytes(FieldMessageList, messages), requiredU32(FieldMaxOutputTokens, p.MaxOutputTokens), requiredBool(FieldStreamRequested, p.StreamRequested), requiredBool(FieldIncludeUsage, p.IncludeUsage))
		if len(p.Tools) > 0 { tools, err := encodeToolList(p.Tools); if err != nil { return nil, err }; fields = append(fields, requiredBytes(FieldToolList, tools)) }
		if p.ToolChoice != nil { choice, err := encodeToolChoice(*p.ToolChoice); if err != nil { return nil, err }; fields = append(fields, requiredBytes(FieldToolChoice, choice)) }
		if len(p.Stop) > 0 { stop, err := encodeStringList(p.Stop, MaxStops); if err != nil { return nil, err }; fields = append(fields, requiredBytes(FieldStopList, stop)) }
		if p.Temperature != nil { fields = append(fields, optionalF64(FieldTemperature, *p.Temperature)) }
		if p.TopP != nil { fields = append(fields, optionalF64(FieldTopP, *p.TopP)) }
		if p.TopK != nil { fields = append(fields, optionalU32(FieldTopK, *p.TopK)) }
		if p.Seed != nil { fields = append(fields, optionalU64(FieldSeed, *p.Seed)) }
		if p.PresencePenalty != nil { fields = append(fields, optionalF64(FieldPresencePenalty, *p.PresencePenalty)) }
		if p.RepetitionPenalty != nil { fields = append(fields, optionalF64(FieldRepetitionPenalty, *p.RepetitionPenalty)) }
		if p.ParallelToolCalls != nil { fields = append(fields, optionalBool(FieldParallelToolCalls, *p.ParallelToolCalls)) }
		if p.ThinkingMode != nil { fields = append(fields, optionalU32(FieldThinkingMode, uint32(*p.ThinkingMode))) }
		if p.ReasoningEffort != nil { fields = append(fields, optionalU32(FieldReasoningEffort, uint32(*p.ReasoningEffort))) }
		if p.ReasoningBudgetTokens != nil { fields = append(fields, optionalU32(FieldReasoningBudgetTokens, *p.ReasoningBudgetTokens)) }
		if p.PreserveThinking != nil { fields = append(fields, optionalBool(FieldPreserveThinking, *p.PreserveThinking)) }
		fields = appendOptionalString(fields, FieldSessionID, p.SessionID)
		fields = appendOptionalString(fields, FieldCacheSalt, p.CacheSalt)
		if len(p.ChatTemplateKwargs) > 0 { kwargs, err := encodeTypedMap(p.ChatTemplateKwargs); if err != nil { return nil, err }; fields = append(fields, requiredBytes(FieldChatTemplateKwargs, kwargs)) }
		if p.SamplingPhase != nil { fields = append(fields, optionalU32(FieldSamplingPhase, uint32(*p.SamplingPhase))) }
		fields = appendOptionalString(fields, FieldStreamedToolDraftName, p.StreamedToolDraftName)
		if len(p.Images) > 0 { images, err := encodeImageList(p.Images); if err != nil { return nil, err }; fields = append(fields, requiredBytes(FieldImageList, images)) }
	case *CancelRequest:
		fields = append(fields, requiredU64(FieldTargetStreamID, p.TargetStreamID), requiredU32(FieldCancelReason, uint32(p.Reason)))
		fields = appendOptionalString(fields, FieldCancelMessage, p.Message)
	case *StreamToken:
		fields = append(fields, requiredU32(FieldTokenID, p.TokenID), requiredU64(FieldTokenIndex, p.Index), requiredU32(FieldStreamChannel, uint32(p.Channel)))
		fields = appendOptionalString(fields, FieldTokenUTF8, p.Text)
		if p.Logprob != nil {
			fields = append(fields, optionalF64(FieldTokenLogprob, *p.Logprob))
		}
		if p.ChoiceIndex != nil { fields = append(fields, optionalU32(FieldChoiceIndex, *p.ChoiceIndex)) }
		if p.StreamSequence != nil { fields = append(fields, optionalU64(FieldStreamSequence, *p.StreamSequence)) }
	case *StreamTool:
		fields = append(fields, requiredU32(FieldToolEventKind, uint32(p.EventKind)), requiredUTF8(FieldToolCallID, p.CallID), requiredU64(FieldToolSequence, p.Sequence))
		fields = appendOptionalString(fields, FieldToolName, p.Name)
		fields = appendOptionalString(fields, FieldToolDataUTF8, p.Data)
		if p.ChoiceIndex != nil { fields = append(fields, optionalU32(FieldChoiceIndex, *p.ChoiceIndex)) }
		if p.ToolCallIndex != nil { fields = append(fields, optionalU32(FieldToolCallIndex, *p.ToolCallIndex)) }
		fields = appendOptionalString(fields, FieldToolCallType, p.ToolCallType)
		fields = appendOptionalString(fields, FieldToolArgumentsDelta, p.ArgumentsDelta)
		if p.DraftTotalBytes != nil { fields = append(fields, optionalU64(FieldToolDraftTotalBytes, *p.DraftTotalBytes)) }
		if p.DraftSource != nil { fields = append(fields, optionalU32(FieldToolDraftSource, uint32(*p.DraftSource))) }
		if p.StreamSequence != nil { fields = append(fields, optionalU64(FieldStreamSequence, *p.StreamSequence)) }
	case *StreamProgress:
		fields = append(fields, requiredU32(FieldProgressPhase, uint32(p.Phase)), requiredF64(FieldProgressFraction, p.Fraction))
		fields = appendOptionalString(fields, FieldProgressMessage, p.Message)
		if p.Current != nil {
			fields = append(fields, optionalU64(FieldProgressCurrent, *p.Current), optionalU64(FieldProgressTotal, *p.Total))
		}
		fields = appendProgressV12(fields, p)
	case *StreamError:
		fields = append(fields, requiredU32(FieldErrorCode, p.Code), requiredUTF8(FieldErrorDomain, p.Domain), requiredUTF8(FieldErrorMessage, p.Message), requiredBool(FieldErrorRetryable, p.Retryable))
		fields = appendOptionalString(fields, FieldErrorDetailsUTF8, p.Details)
		if p.Stage != nil { fields = append(fields, optionalU32(FieldErrorStage, uint32(*p.Stage))) }
		if p.Severity != nil { fields = append(fields, optionalU32(FieldErrorSeverity, uint32(*p.Severity))) }
		if p.RetryAfterMS != nil { fields = append(fields, optionalU64(FieldRetryAfterMS, *p.RetryAfterMS)) }
		if p.UnderlyingStatus != nil { fields = append(fields, optionalU32(FieldUnderlyingStatus, *p.UnderlyingStatus)) }
		if p.FailedFieldID != nil { fields = append(fields, optionalU32(FieldFailedFieldID, *p.FailedFieldID)) }
		if len(p.DetailsBytes) > 0 { fields = append(fields, requiredBytes(FieldErrorDetailsBytes, p.DetailsBytes)) }
		if p.StreamSequence != nil { fields = append(fields, optionalU64(FieldStreamSequence, *p.StreamSequence)) }
	case *StreamCompletion:
		fields = append(fields, requiredU32(FieldFinishReason, uint32(p.Reason)), requiredU64(FieldInputTokens, p.InputTokens), requiredU64(FieldOutputTokens, p.OutputTokens))
		fields = appendOptionalString(fields, FieldCompletionMessage, p.Message)
		if p.CachedInputTokens != nil {
			fields = append(fields, optionalU64(FieldCachedInputTokens, *p.CachedInputTokens))
		}
		if p.DraftTokens != nil {
			fields = append(fields, optionalU64(FieldDraftTokens, *p.DraftTokens))
		}
		if p.AcceptedDraftTokens != nil {
			fields = append(fields, optionalU64(FieldAcceptedDraftTokens, *p.AcceptedDraftTokens))
		}
		if p.TotalTokens != nil { fields = append(fields, optionalU64(FieldTotalTokens, *p.TotalTokens)) }
		if p.ReasoningTokens != nil { fields = append(fields, optionalU64(FieldReasoningTokens, *p.ReasoningTokens)) }
		if p.CompletionFlags != nil { fields = append(fields, optionalU64(FieldCompletionFlags, *p.CompletionFlags)) }
		if p.StreamSequence != nil { fields = append(fields, optionalU64(FieldStreamSequence, *p.StreamSequence)) }
	case *StreamUsage:
		fields = append(fields, requiredU64(FieldInputTokens, p.InputTokens), requiredU64(FieldOutputTokens, p.OutputTokens), requiredU64(FieldTotalTokens, p.TotalTokens), requiredU64(FieldStreamSequence, p.StreamSequence))
		if p.CachedInputTokens != nil { fields = append(fields, optionalU64(FieldCachedInputTokens, *p.CachedInputTokens)) }
		if p.DraftTokens != nil { fields = append(fields, optionalU64(FieldDraftTokens, *p.DraftTokens)) }
		if p.AcceptedDraftTokens != nil { fields = append(fields, optionalU64(FieldAcceptedDraftTokens, *p.AcceptedDraftTokens)) }
		if p.ReasoningTokens != nil { fields = append(fields, optionalU64(FieldReasoningTokens, *p.ReasoningTokens)) }
	case *StreamCache:
		fields = append(fields, requiredU32(FieldCacheEventKind, uint32(p.Event)), requiredU64(FieldStreamSequence, p.StreamSequence))
		if len(p.KeyDigest) > 0 { fields = append(fields, requiredBytes(FieldCacheKeyDigest, p.KeyDigest)) }
		if p.CachedInputTokens != nil { fields = append(fields, optionalU64(FieldCachedInputTokens, *p.CachedInputTokens)) }
		if p.WrittenTokens != nil { fields = append(fields, optionalU64(FieldCacheWrittenTokens, *p.WrittenTokens)) }
		if p.EvictedTokens != nil { fields = append(fields, optionalU64(FieldCacheEvictedTokens, *p.EvictedTokens)) }
	case *CancelAck:
		fields = append(fields, requiredU64(FieldTargetStreamID, p.TargetStreamID), requiredU32(FieldCancelDisposition, uint32(p.Disposition)))
		if p.AcceptedSequence != nil { fields = append(fields, optionalU64(FieldCancelAcceptedSequence, *p.AcceptedSequence)) }
	default:
		return nil, protocolError(StatusInvalidArgument, "payload type %T is not encodable", payload)
	}
	return encodeFields(fields)
}

func decodePayload(kind PayloadKind, data []byte) (Payload, error) {
	parsed, err := parseFields(data)
	if err != nil {
		return nil, err
	}
	fields, err := selectKnownFields(kind, parsed)
	if err != nil {
		return nil, err
	}
	switch kind {
	case PayloadHandshakeRequest:
		p := &HandshakeRequest{
			ClientABIMajor: getU32(fields, FieldClientABIMajor), ClientABIMinor: getU32(fields, FieldClientABIMinor),
			MinProtocolMajor: getU32(fields, FieldMinProtocolMajor), MinProtocolMinor: getU32(fields, FieldMinProtocolMinor),
			MaxProtocolMajor: getU32(fields, FieldMaxProtocolMajor), MaxProtocolMinor: getU32(fields, FieldMaxProtocolMinor),
			RequiredCapabilities: CapabilityMask(getU64(fields, FieldRequiredCapabilities)), ClientBuildID: getString(fields, FieldClientBuildID),
		}
		copy(p.ClientNonce[:], fields[FieldClientNonce].value)
		return p, nil
	case PayloadHandshakeResponse:
		p := &HandshakeResponse{
			SelectedProtocolMajor: getU32(fields, FieldSelectedProtocolMajor), SelectedProtocolMinor: getU32(fields, FieldSelectedProtocolMinor),
			RuntimeABIMajor: getU32(fields, FieldRuntimeABIMajor), RuntimeABIMinor: getU32(fields, FieldRuntimeABIMinor), RuntimeBuildID: getString(fields, FieldRuntimeBuildID),
		}
		copy(p.RuntimeNonce[:], fields[FieldRuntimeNonce].value)
		return p, nil
	case PayloadCapabilityRequest:
		return &CapabilityRequest{Required: CapabilityMask(getU64(fields, FieldRequiredCapabilities)), Desired: CapabilityMask(getU64(fields, FieldDesiredCapabilities))}, nil
	case PayloadCapabilityResponse:
		return &CapabilityResponse{Offered: CapabilityMask(getU64(fields, FieldOfferedCapabilities)), Negotiated: CapabilityMask(getU64(fields, FieldNegotiatedCapabilities))}, nil
	case PayloadHealthRequest:
		return &HealthRequest{}, nil
	case PayloadHealthResponse:
		return &HealthResponse{State: HealthState(getU32(fields, FieldHealthState)), UptimeMS: getU64(fields, FieldUptimeMS), ActiveRequests: getU32(fields, FieldActiveRequests),
			ResidentBytes: getU64(fields, FieldResidentBytes), Message: optionalString(fields, FieldHealthMessage), RuntimeGeneration: getU64(fields, FieldRuntimeGeneration),
			ModelID: optionalString(fields, FieldModelID), ModelState: ModelLifecycleState(getU32(fields, FieldModelState))}, nil
	case PayloadModelLoad:
		p := &ModelLoad{ModelID: getString(fields, FieldModelID), Backend: Backend(getU32(fields, FieldBackend)), MemoryLimitBytes: getU64(fields, FieldMemoryLimitBytes)}
		copy(p.ProfileDigest[:], fields[FieldModelProfileDigest].value)
		return p, nil
	case PayloadModelUnload:
		return &ModelUnload{ExpectedGeneration: getU64(fields, FieldExpectedModelGeneration)}, nil
	case PayloadModelSwitch:
		p := &ModelSwitch{ModelID: getString(fields, FieldModelID), Backend: Backend(getU32(fields, FieldBackend)), MemoryLimitBytes: getU64(fields, FieldMemoryLimitBytes), ExpectedGeneration: getU64(fields, FieldExpectedModelGeneration)}
		copy(p.ProfileDigest[:], fields[FieldModelProfileDigest].value)
		return p, nil
	case PayloadModelState:
		return &ModelStatePayload{ModelID: optionalString(fields, FieldModelID), Backend: optionalBackend(fields, FieldBackend), Generation: getU64(fields, FieldModelGeneration),
			State: ModelLifecycleState(getU32(fields, FieldModelState)), ResidentBytes: getU64(fields, FieldModelResidentBytes), ErrorCode: optionalU32Value(fields, FieldModelErrorCode), ErrorMessage: optionalString(fields, FieldModelErrorMessage)}, nil
	case PayloadTextRequest:
		return decodeTextRequest(fields), nil
	case PayloadVisionRequest:
		images, imageErr := decodeImageList(fields[FieldImageList].value)
		if imageErr != nil {
			return nil, imageErr
		}
		p := &VisionRequest{Images: images}
		decodeInferenceFields(fields, &p.Prompt, &p.System, &p.MaxOutputTokens, &p.Temperature, &p.TopP, &p.Seed, &p.ThinkMode, &p.SessionID)
		return p, nil
	case PayloadChatRequest:
		messages, err := decodeMessageList(fields[FieldMessageList].value); if err != nil { return nil, err }
		p := &ChatRequest{ModelID:getString(fields,FieldModelID), ExpectedModelGeneration:getU64(fields,FieldExpectedModelGeneration), Messages:messages, MaxOutputTokens:getU32(fields,FieldMaxOutputTokens), Temperature:optionalF64Value(fields,FieldTemperature), TopP:optionalF64Value(fields,FieldTopP), TopK:optionalU32Value(fields,FieldTopK), Seed:optionalU64Value(fields,FieldSeed), PresencePenalty:optionalF64Value(fields,FieldPresencePenalty), RepetitionPenalty:optionalF64Value(fields,FieldRepetitionPenalty), ParallelToolCalls:optionalBoolValue(fields,FieldParallelToolCalls), StreamRequested:getBool(fields,FieldStreamRequested), IncludeUsage:getBool(fields,FieldIncludeUsage), SessionID:optionalString(fields,FieldSessionID), CacheSalt:optionalString(fields,FieldCacheSalt), StreamedToolDraftName:optionalString(fields,FieldStreamedToolDraftName)}
		if value, ok := fields[FieldToolList]; ok { p.Tools, err = decodeToolList(value.value); if err != nil { return nil, err } }
		if value, ok := fields[FieldToolChoice]; ok { p.ToolChoice, err = decodeToolChoice(value.value); if err != nil { return nil, err } }
		if value, ok := fields[FieldStopList]; ok { p.Stop, err = decodeStringList(value.value,MaxStops); if err != nil { return nil, err } }
		if value := optionalU32Value(fields,FieldThinkingMode); value != nil { converted:=ThinkingMode(*value); p.ThinkingMode=&converted }
		if value := optionalU32Value(fields,FieldReasoningEffort); value != nil { converted:=ReasoningEffort(*value); p.ReasoningEffort=&converted }
		p.ReasoningBudgetTokens=optionalU32Value(fields,FieldReasoningBudgetTokens); p.PreserveThinking=optionalBoolValue(fields,FieldPreserveThinking)
		if value, ok := fields[FieldChatTemplateKwargs]; ok { p.ChatTemplateKwargs, err = decodeTypedMap(value.value); if err != nil { return nil, err } }
		if value := optionalU32Value(fields,FieldSamplingPhase); value != nil { converted:=SamplingPhase(*value); p.SamplingPhase=&converted }
		if value, ok := fields[FieldImageList]; ok { p.Images, err = decodeImageList(value.value); if err != nil { return nil, err } }
		return p,nil
	case PayloadCancelRequest:
		return &CancelRequest{TargetStreamID: getU64(fields, FieldTargetStreamID), Reason: CancelReason(getU32(fields, FieldCancelReason)), Message: optionalString(fields, FieldCancelMessage)}, nil
	case PayloadStreamToken:
		return &StreamToken{TokenID: getU32(fields, FieldTokenID), Index: getU64(fields, FieldTokenIndex), Text: optionalString(fields, FieldTokenUTF8), Channel: StreamChannel(getU32(fields, FieldStreamChannel)), Logprob: optionalF64Value(fields, FieldTokenLogprob), ChoiceIndex:optionalU32Value(fields,FieldChoiceIndex), StreamSequence:optionalU64Value(fields,FieldStreamSequence)}, nil
	case PayloadStreamTool:
		p:=&StreamTool{EventKind: ToolEventKind(getU32(fields, FieldToolEventKind)), CallID: getString(fields, FieldToolCallID), Name: optionalString(fields, FieldToolName), Data: optionalString(fields, FieldToolDataUTF8), Sequence: getU64(fields, FieldToolSequence),ChoiceIndex:optionalU32Value(fields,FieldChoiceIndex),ToolCallIndex:optionalU32Value(fields,FieldToolCallIndex),ToolCallType:optionalString(fields,FieldToolCallType),ArgumentsDelta:optionalString(fields,FieldToolArgumentsDelta),DraftTotalBytes:optionalU64Value(fields,FieldToolDraftTotalBytes),StreamSequence:optionalU64Value(fields,FieldStreamSequence)};if value:=optionalU32Value(fields,FieldToolDraftSource);value!=nil{converted:=ToolDraftSource(*value);p.DraftSource=&converted};return p,nil
	case PayloadStreamProgress:
		return decodeProgressV12(fields), nil
	case PayloadStreamError:
		p:=&StreamError{Code:getU32(fields,FieldErrorCode),Domain:getString(fields,FieldErrorDomain),Message:getString(fields,FieldErrorMessage),Retryable:getBool(fields,FieldErrorRetryable),Details:optionalString(fields,FieldErrorDetailsUTF8),RetryAfterMS:optionalU64Value(fields,FieldRetryAfterMS),UnderlyingStatus:optionalU32Value(fields,FieldUnderlyingStatus),FailedFieldID:optionalU32Value(fields,FieldFailedFieldID),StreamSequence:optionalU64Value(fields,FieldStreamSequence)};if value:=optionalU32Value(fields,FieldErrorStage);value!=nil{converted:=ErrorStage(*value);p.Stage=&converted};if value:=optionalU32Value(fields,FieldErrorSeverity);value!=nil{converted:=ErrorSeverity(*value);p.Severity=&converted};if value,ok:=fields[FieldErrorDetailsBytes];ok{p.DetailsBytes=append([]byte(nil),value.value...)};return p,nil
	case PayloadStreamCompletion:
		return &StreamCompletion{Reason: FinishReason(getU32(fields, FieldFinishReason)), InputTokens: getU64(fields, FieldInputTokens), OutputTokens: getU64(fields, FieldOutputTokens), Message: optionalString(fields, FieldCompletionMessage), CachedInputTokens: optionalU64Value(fields, FieldCachedInputTokens), DraftTokens: optionalU64Value(fields, FieldDraftTokens), AcceptedDraftTokens: optionalU64Value(fields, FieldAcceptedDraftTokens),TotalTokens:optionalU64Value(fields,FieldTotalTokens),ReasoningTokens:optionalU64Value(fields,FieldReasoningTokens),CompletionFlags:optionalU64Value(fields,FieldCompletionFlags),StreamSequence:optionalU64Value(fields,FieldStreamSequence)}, nil
	case PayloadStreamUsage:
		return &StreamUsage{InputTokens:getU64(fields,FieldInputTokens),OutputTokens:getU64(fields,FieldOutputTokens),TotalTokens:getU64(fields,FieldTotalTokens),CachedInputTokens:optionalU64Value(fields,FieldCachedInputTokens),DraftTokens:optionalU64Value(fields,FieldDraftTokens),AcceptedDraftTokens:optionalU64Value(fields,FieldAcceptedDraftTokens),ReasoningTokens:optionalU64Value(fields,FieldReasoningTokens),StreamSequence:getU64(fields,FieldStreamSequence)},nil
	case PayloadStreamCache:
		p:=&StreamCache{Event:CacheEventKind(getU32(fields,FieldCacheEventKind)),CachedInputTokens:optionalU64Value(fields,FieldCachedInputTokens),WrittenTokens:optionalU64Value(fields,FieldCacheWrittenTokens),EvictedTokens:optionalU64Value(fields,FieldCacheEvictedTokens),StreamSequence:getU64(fields,FieldStreamSequence)};if value,ok:=fields[FieldCacheKeyDigest];ok{p.KeyDigest=append([]byte(nil),value.value...)};return p,nil
	case PayloadCancelAck:
		return &CancelAck{TargetStreamID:getU64(fields,FieldTargetStreamID),Disposition:CancelDisposition(getU32(fields,FieldCancelDisposition)),AcceptedSequence:optionalU64Value(fields,FieldCancelAcceptedSequence)},nil
	default:
		return nil, protocolError(StatusBadEnum, "unsupported payload kind 0x%x", uint32(kind))
	}
}

func encodeInferenceFields(fields []wireField, prompt string, system *string, maxOutput uint32, temperature, topP *float64, seed *uint64, think *bool, session *string) []wireField {
	fields = append(fields, requiredUTF8(FieldPromptUTF8, prompt), requiredU32(FieldMaxOutputTokens, maxOutput))
	fields = appendOptionalString(fields, FieldSystemUTF8, system)
	if temperature != nil {
		fields = append(fields, optionalF64(FieldTemperature, *temperature))
	}
	if topP != nil {
		fields = append(fields, optionalF64(FieldTopP, *topP))
	}
	if seed != nil {
		fields = append(fields, optionalU64(FieldSeed, *seed))
	}
	if think != nil {
		fields = append(fields, optionalBool(FieldThinkMode, *think))
	}
	return appendOptionalString(fields, FieldSessionID, session)
}

func decodeTextRequest(fields map[FieldID]wireField) *TextRequest {
	p := &TextRequest{}
	decodeInferenceFields(fields, &p.Prompt, &p.System, &p.MaxOutputTokens, &p.Temperature, &p.TopP, &p.Seed, &p.ThinkMode, &p.SessionID)
	return p
}

func decodeInferenceFields(fields map[FieldID]wireField, prompt *string, system **string, maxOutput *uint32, temperature, topP **float64, seed **uint64, think **bool, session **string) {
	*prompt = getString(fields, FieldPromptUTF8)
	*system = optionalString(fields, FieldSystemUTF8)
	*maxOutput = getU32(fields, FieldMaxOutputTokens)
	*temperature = optionalF64Value(fields, FieldTemperature)
	*topP = optionalF64Value(fields, FieldTopP)
	*seed = optionalU64Value(fields, FieldSeed)
	*think = optionalBoolValue(fields, FieldThinkMode)
	*session = optionalString(fields, FieldSessionID)
}

func encodeImageList(images []EncodedImage) ([]byte, error) {
	if err := validateImages(images); err != nil {
		return nil, err
	}
	total := ImageListHeaderBytes
	for _, image := range images {
		total += align8(ImageRecordHeaderBytes + len(image.Data))
	}
	out := make([]byte, total)
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(images)))
	pos := ImageListHeaderBytes
	for _, image := range images {
		binary.LittleEndian.PutUint32(out[pos:pos+4], uint32(image.Format))
		binary.LittleEndian.PutUint32(out[pos+4:pos+8], 0)
		binary.LittleEndian.PutUint32(out[pos+8:pos+12], image.Width)
		binary.LittleEndian.PutUint32(out[pos+12:pos+16], image.Height)
		binary.LittleEndian.PutUint64(out[pos+16:pos+24], uint64(len(image.Data)))
		copy(out[pos+ImageRecordHeaderBytes:], image.Data)
		pos += align8(ImageRecordHeaderBytes + len(image.Data))
	}
	return out, nil
}

func decodeImageList(data []byte) ([]EncodedImage, error) {
	if len(data) < ImageListHeaderBytes {
		return nil, protocolError(StatusBadLength, "image list is shorter than its header")
	}
	if binary.LittleEndian.Uint32(data[4:8]) != 0 {
		return nil, protocolError(StatusBadHeader, "image list reserved word must be zero")
	}
	count := binary.LittleEndian.Uint32(data[0:4])
	if count == 0 || count > MaxImages {
		return nil, protocolError(StatusResourceLimit, "image count must be in [1,%d]", MaxImages)
	}
	images := make([]EncodedImage, 0, count)
	pos := ImageListHeaderBytes
	for i := uint32(0); i < count; i++ {
		if len(data)-pos < ImageRecordHeaderBytes {
			return nil, protocolError(StatusBadLength, "image %d record header is truncated", i)
		}
		if binary.LittleEndian.Uint32(data[pos+4:pos+8]) != 0 {
			return nil, protocolError(StatusBadHeader, "image %d flags must be zero", i)
		}
		encodedBytes := binary.LittleEndian.Uint64(data[pos+16 : pos+24])
		if encodedBytes > uint64(len(data)-pos-ImageRecordHeaderBytes) {
			return nil, protocolError(StatusBadLength, "image %d encoded data is truncated", i)
		}
		recordBytes := ImageRecordHeaderBytes + int(encodedBytes)
		aligned := align8(recordBytes)
		if aligned > len(data)-pos {
			return nil, protocolError(StatusBadLength, "image %d aligned record is truncated", i)
		}
		for _, value := range data[pos+recordBytes : pos+aligned] {
			if value != 0 {
				return nil, protocolError(StatusBadHeader, "image %d padding must be zero", i)
			}
		}
		encoded := make([]byte, int(encodedBytes))
		copy(encoded, data[pos+ImageRecordHeaderBytes:pos+recordBytes])
		images = append(images, EncodedImage{Format: ImageFormat(binary.LittleEndian.Uint32(data[pos : pos+4])), Width: binary.LittleEndian.Uint32(data[pos+8 : pos+12]), Height: binary.LittleEndian.Uint32(data[pos+12 : pos+16]), Data: encoded})
		pos += aligned
	}
	if pos != len(data) {
		return nil, protocolError(StatusBadLength, "image list has trailing bytes")
	}
	if err := validateImages(images); err != nil {
		return nil, err
	}
	return images, nil
}

func encodeFields(fields []wireField) ([]byte, error) {
	if len(fields) > MaxFields {
		return nil, protocolError(StatusResourceLimit, "payload has more than %d fields", MaxFields)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].id < fields[j].id })
	total := 0
	var previous FieldID
	for i, field := range fields {
		if i > 0 && field.id <= previous {
			return nil, protocolError(StatusDuplicateOrUnsortedField, "field %d is duplicated", field.id)
		}
		if uint64(len(field.value)) > uint64(^uint32(0)) {
			return nil, protocolError(StatusBadLength, "field %d is too large", field.id)
		}
		previous = field.id
		total += align8(TLVHeaderBytes + len(field.value))
	}
	if uint64(total) > MaxFramePayloadBytes {
		return nil, protocolError(StatusResourceLimit, "TLV payload exceeds frame limit")
	}
	out := make([]byte, total)
	pos := 0
	for _, field := range fields {
		binary.LittleEndian.PutUint16(out[pos:pos+2], uint16(field.id))
		out[pos+2] = byte(field.typ)
		out[pos+3] = byte(field.flags)
		binary.LittleEndian.PutUint32(out[pos+4:pos+8], uint32(len(field.value)))
		copy(out[pos+TLVHeaderBytes:], field.value)
		pos += align8(TLVHeaderBytes + len(field.value))
	}
	return out, nil
}

func parseFields(data []byte) ([]wireField, error) {
	fields := make([]wireField, 0, 12)
	pos := 0
	var previous FieldID
	for pos < len(data) {
		if len(fields) == MaxFields {
			return nil, protocolError(StatusResourceLimit, "payload has more than %d fields", MaxFields)
		}
		if len(data)-pos < TLVHeaderBytes {
			return nil, protocolError(StatusBadLength, "truncated TLV header")
		}
		id := FieldID(binary.LittleEndian.Uint16(data[pos : pos+2]))
		typ := WireType(data[pos+2])
		flags := FieldFlags(data[pos+3])
		if len(fields) > 0 && id <= previous {
			return nil, protocolError(StatusDuplicateOrUnsortedField, "field %d is duplicated or out of order", id)
		}
		if typ < WireU32 || typ > WireBytes {
			return nil, protocolError(StatusBadEnum, "field %d has invalid wire type %d", id, typ)
		}
		if flags&^KnownFieldFlags != 0 {
			return nil, protocolError(StatusBadHeader, "field %d has unknown flag bits", id)
		}
		valueBytes := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		fieldBytes := TLVHeaderBytes + valueBytes
		aligned := align8(fieldBytes)
		if aligned > len(data)-pos {
			return nil, protocolError(StatusBadLength, "field %d is truncated", id)
		}
		value := data[pos+TLVHeaderBytes : pos+fieldBytes]
		if err := validateWireValue(id, typ, value); err != nil {
			return nil, err
		}
		for _, padding := range data[pos+fieldBytes : pos+aligned] {
			if padding != 0 {
				return nil, protocolError(StatusBadHeader, "field %d padding must be zero", id)
			}
		}
		copyValue := make([]byte, len(value))
		copy(copyValue, value)
		fields = append(fields, wireField{id: id, typ: typ, flags: flags, value: copyValue})
		previous = id
		pos += aligned
	}
	return fields, nil
}

func validateWireValue(id FieldID, typ WireType, value []byte) error {
	switch typ {
	case WireU32:
		if len(value) != 4 {
			return protocolError(StatusBadLength, "u32 field %d must contain 4 bytes", id)
		}
	case WireU64, WireF64:
		if len(value) != 8 {
			return protocolError(StatusBadLength, "64-bit field %d must contain 8 bytes", id)
		}
		if typ == WireF64 && !finite(math.Float64frombits(binary.LittleEndian.Uint64(value))) {
			return protocolError(StatusInvalidArgument, "f64 field %d must be finite", id)
		}
	case WireBool:
		if len(value) != 1 || value[0] > 1 {
			return protocolError(StatusBadLength, "bool field %d must be one byte containing 0 or 1", id)
		}
	case WireUTF8:
		if !utf8.Valid(value) {
			return protocolError(StatusBadUTF8, "field %d is not valid UTF-8", id)
		}
		for _, b := range value {
			if b == 0 {
				return protocolError(StatusBadUTF8, "field %d contains NUL", id)
			}
		}
	}
	return nil
}

func selectKnownFields(kind PayloadKind, parsed []wireField) (map[FieldID]wireField, error) {
	rules := schemaFor(kind)
	known := make(map[FieldID]wireField, len(rules))
	for _, field := range parsed {
		rule, ok := rules[field.id]
		if !ok {
			if field.flags&FieldFlagCritical != 0 {
				return nil, protocolError(StatusUnknownCriticalField, "payload 0x%x has unknown critical field %d", uint32(kind), field.id)
			}
			continue
		}
		if field.typ != rule.typ {
			return nil, protocolError(StatusBadEnum, "field %d has wire type %d, expected %d", field.id, field.typ, rule.typ)
		}
		if rule.exactBytes != 0 && len(field.value) != rule.exactBytes {
			return nil, protocolError(StatusBadLength, "field %d must contain exactly %d bytes", field.id, rule.exactBytes)
		}
		known[field.id] = field
	}
	for id, rule := range rules {
		if rule.required {
			if _, ok := known[id]; !ok {
				return nil, protocolError(StatusMissingField, "payload 0x%x is missing field %d", uint32(kind), id)
			}
		}
	}
	return known, nil
}

func schemaFor(kind PayloadKind) map[FieldID]fieldRule {
	r := func(typ WireType) fieldRule { return fieldRule{typ: typ, required: true} }
	o := func(typ WireType) fieldRule { return fieldRule{typ: typ} }
	exact := func(typ WireType, bytes int) fieldRule { return fieldRule{typ: typ, required: true, exactBytes: bytes} }
	inference := func(vision bool) map[FieldID]fieldRule {
		m := map[FieldID]fieldRule{FieldPromptUTF8: r(WireUTF8), FieldSystemUTF8: o(WireUTF8), FieldMaxOutputTokens: r(WireU32), FieldTemperature: o(WireF64), FieldTopP: o(WireF64), FieldSeed: o(WireU64), FieldThinkMode: o(WireBool), FieldSessionID: o(WireUTF8)}
		if vision {
			m[FieldImageList] = r(WireBytes)
		}
		return m
	}
	switch kind {
	case PayloadHandshakeRequest:
		return map[FieldID]fieldRule{FieldClientABIMajor: r(WireU32), FieldClientABIMinor: r(WireU32), FieldMinProtocolMajor: r(WireU32), FieldMinProtocolMinor: r(WireU32), FieldMaxProtocolMajor: r(WireU32), FieldMaxProtocolMinor: r(WireU32), FieldRequiredCapabilities: r(WireU64), FieldClientBuildID: r(WireUTF8), FieldClientNonce: exact(WireBytes, 32)}
	case PayloadHandshakeResponse:
		return map[FieldID]fieldRule{FieldSelectedProtocolMajor: r(WireU32), FieldSelectedProtocolMinor: r(WireU32), FieldRuntimeABIMajor: r(WireU32), FieldRuntimeABIMinor: r(WireU32), FieldRuntimeBuildID: r(WireUTF8), FieldRuntimeNonce: exact(WireBytes, 32)}
	case PayloadCapabilityRequest:
		return map[FieldID]fieldRule{FieldRequiredCapabilities: r(WireU64), FieldDesiredCapabilities: r(WireU64)}
	case PayloadCapabilityResponse:
		return map[FieldID]fieldRule{FieldOfferedCapabilities: r(WireU64), FieldNegotiatedCapabilities: r(WireU64)}
	case PayloadHealthRequest:
		return map[FieldID]fieldRule{}
	case PayloadHealthResponse:
		return map[FieldID]fieldRule{FieldHealthState: r(WireU32), FieldUptimeMS: r(WireU64), FieldActiveRequests: r(WireU32), FieldResidentBytes: r(WireU64), FieldHealthMessage: o(WireUTF8), FieldRuntimeGeneration: r(WireU64), FieldModelID: o(WireUTF8), FieldModelState: r(WireU32)}
	case PayloadModelLoad:
		return map[FieldID]fieldRule{FieldModelID: r(WireUTF8), FieldModelProfileDigest: exact(WireBytes, 32), FieldBackend: r(WireU32), FieldMemoryLimitBytes: r(WireU64)}
	case PayloadModelUnload:
		return map[FieldID]fieldRule{FieldExpectedModelGeneration: r(WireU64)}
	case PayloadModelSwitch:
		return map[FieldID]fieldRule{FieldModelID: r(WireUTF8), FieldModelProfileDigest: exact(WireBytes, 32), FieldBackend: r(WireU32), FieldMemoryLimitBytes: r(WireU64), FieldExpectedModelGeneration: r(WireU64)}
	case PayloadModelState:
		return map[FieldID]fieldRule{FieldModelID: o(WireUTF8), FieldBackend: o(WireU32), FieldModelGeneration: r(WireU64), FieldModelState: r(WireU32), FieldModelResidentBytes: r(WireU64), FieldModelErrorCode: o(WireU32), FieldModelErrorMessage: o(WireUTF8)}
	case PayloadTextRequest:
		return inference(false)
	case PayloadVisionRequest:
		return inference(true)
	case PayloadChatRequest:
		return map[FieldID]fieldRule{FieldModelID:r(WireUTF8),FieldExpectedModelGeneration:r(WireU64),FieldMessageList:r(WireBytes),FieldToolList:o(WireBytes),FieldToolChoice:o(WireBytes),FieldStopList:o(WireBytes),FieldMaxOutputTokens:r(WireU32),FieldTemperature:o(WireF64),FieldTopP:o(WireF64),FieldTopK:o(WireU32),FieldSeed:o(WireU64),FieldPresencePenalty:o(WireF64),FieldRepetitionPenalty:o(WireF64),FieldParallelToolCalls:o(WireBool),FieldStreamRequested:r(WireBool),FieldIncludeUsage:r(WireBool),FieldThinkingMode:o(WireU32),FieldReasoningEffort:o(WireU32),FieldReasoningBudgetTokens:o(WireU32),FieldPreserveThinking:o(WireBool),FieldSessionID:o(WireUTF8),FieldCacheSalt:o(WireUTF8),FieldChatTemplateKwargs:o(WireBytes),FieldSamplingPhase:o(WireU32),FieldStreamedToolDraftName:o(WireUTF8),FieldImageList:o(WireBytes)}
	case PayloadCancelRequest:
		return map[FieldID]fieldRule{FieldTargetStreamID: r(WireU64), FieldCancelReason: r(WireU32), FieldCancelMessage: o(WireUTF8)}
	case PayloadStreamToken:
		return map[FieldID]fieldRule{FieldTokenID:r(WireU32),FieldTokenIndex:r(WireU64),FieldTokenUTF8:o(WireUTF8),FieldStreamChannel:r(WireU32),FieldTokenLogprob:o(WireF64),FieldChoiceIndex:o(WireU32),FieldStreamSequence:o(WireU64)}
	case PayloadStreamTool:
		return map[FieldID]fieldRule{FieldToolEventKind:r(WireU32),FieldToolCallID:r(WireUTF8),FieldToolName:o(WireUTF8),FieldToolDataUTF8:o(WireUTF8),FieldToolSequence:r(WireU64),FieldChoiceIndex:o(WireU32),FieldToolCallIndex:o(WireU32),FieldToolCallType:o(WireUTF8),FieldToolArgumentsDelta:o(WireUTF8),FieldToolDraftTotalBytes:o(WireU64),FieldToolDraftSource:o(WireU32),FieldStreamSequence:o(WireU64)}
	case PayloadStreamProgress:
		return progressSchemaV12(r,o)
	case PayloadStreamError:
		return map[FieldID]fieldRule{FieldErrorCode:r(WireU32),FieldErrorDomain:r(WireUTF8),FieldErrorMessage:r(WireUTF8),FieldErrorRetryable:r(WireBool),FieldErrorDetailsUTF8:o(WireUTF8),FieldErrorStage:o(WireU32),FieldErrorSeverity:o(WireU32),FieldRetryAfterMS:o(WireU64),FieldUnderlyingStatus:o(WireU32),FieldFailedFieldID:o(WireU32),FieldErrorDetailsBytes:o(WireBytes),FieldStreamSequence:o(WireU64)}
	case PayloadStreamCompletion:
		return map[FieldID]fieldRule{FieldFinishReason:r(WireU32),FieldInputTokens:r(WireU64),FieldOutputTokens:r(WireU64),FieldCompletionMessage:o(WireUTF8),FieldCachedInputTokens:o(WireU64),FieldDraftTokens:o(WireU64),FieldAcceptedDraftTokens:o(WireU64),FieldTotalTokens:o(WireU64),FieldReasoningTokens:o(WireU64),FieldCompletionFlags:o(WireU64),FieldStreamSequence:o(WireU64)}
	case PayloadStreamUsage:
		return map[FieldID]fieldRule{FieldInputTokens:r(WireU64),FieldOutputTokens:r(WireU64),FieldTotalTokens:r(WireU64),FieldCachedInputTokens:o(WireU64),FieldDraftTokens:o(WireU64),FieldAcceptedDraftTokens:o(WireU64),FieldReasoningTokens:o(WireU64),FieldStreamSequence:r(WireU64)}
	case PayloadStreamCache:
		return map[FieldID]fieldRule{FieldCacheEventKind:r(WireU32),FieldCacheKeyDigest:o(WireBytes),FieldCachedInputTokens:o(WireU64),FieldCacheWrittenTokens:o(WireU64),FieldCacheEvictedTokens:o(WireU64),FieldStreamSequence:r(WireU64)}
	case PayloadCancelAck:
		return map[FieldID]fieldRule{FieldTargetStreamID:r(WireU64),FieldCancelDisposition:r(WireU32),FieldCancelAcceptedSequence:o(WireU64)}
	default:
		return map[FieldID]fieldRule{}
	}
}

func scalarField(id FieldID, typ WireType, flags FieldFlags, bytes int) wireField { return wireField{id: id, typ: typ, flags: flags, value: make([]byte, bytes)} }
func requiredU32(id FieldID, value uint32) wireField { f := scalarField(id, WireU32, FieldFlagCritical, 4); binary.LittleEndian.PutUint32(f.value, value); return f }
func optionalU32(id FieldID, value uint32) wireField { f := scalarField(id, WireU32, FieldFlagNone, 4); binary.LittleEndian.PutUint32(f.value, value); return f }
func requiredU64(id FieldID, value uint64) wireField { f := scalarField(id, WireU64, FieldFlagCritical, 8); binary.LittleEndian.PutUint64(f.value, value); return f }
func optionalU64(id FieldID, value uint64) wireField { f := scalarField(id, WireU64, FieldFlagNone, 8); binary.LittleEndian.PutUint64(f.value, value); return f }
func requiredF64(id FieldID, value float64) wireField { f := scalarField(id, WireF64, FieldFlagCritical, 8); binary.LittleEndian.PutUint64(f.value, math.Float64bits(value)); return f }
func optionalF64(id FieldID, value float64) wireField { f := scalarField(id, WireF64, FieldFlagNone, 8); binary.LittleEndian.PutUint64(f.value, math.Float64bits(value)); return f }
func requiredBool(id FieldID, value bool) wireField { b := byte(0); if value { b = 1 }; return wireField{id: id, typ: WireBool, flags: FieldFlagCritical, value: []byte{b}} }
func optionalBool(id FieldID, value bool) wireField { b := byte(0); if value { b = 1 }; return wireField{id: id, typ: WireBool, flags: FieldFlagNone, value: []byte{b}} }
func requiredUTF8(id FieldID, value string) wireField { return wireField{id: id, typ: WireUTF8, flags: FieldFlagCritical, value: []byte(value)} }
func requiredBytes(id FieldID, value []byte) wireField { copyValue := append([]byte(nil), value...); return wireField{id: id, typ: WireBytes, flags: FieldFlagCritical, value: copyValue} }
func appendOptionalString(fields []wireField, id FieldID, value *string) []wireField { if value != nil { fields = append(fields, wireField{id: id, typ: WireUTF8, flags: FieldFlagNone, value: []byte(*value)}) }; return fields }

func getU32(fields map[FieldID]wireField, id FieldID) uint32 { return binary.LittleEndian.Uint32(fields[id].value) }
func getU64(fields map[FieldID]wireField, id FieldID) uint64 { return binary.LittleEndian.Uint64(fields[id].value) }
func getF64(fields map[FieldID]wireField, id FieldID) float64 { return math.Float64frombits(getU64(fields, id)) }
func getBool(fields map[FieldID]wireField, id FieldID) bool { return fields[id].value[0] == 1 }
func getString(fields map[FieldID]wireField, id FieldID) string { return string(fields[id].value) }
func optionalString(fields map[FieldID]wireField, id FieldID) *string { if _, ok := fields[id]; !ok { return nil }; value := getString(fields, id); return &value }
func optionalU32Value(fields map[FieldID]wireField, id FieldID) *uint32 { if _, ok := fields[id]; !ok { return nil }; value := getU32(fields, id); return &value }
func optionalU64Value(fields map[FieldID]wireField, id FieldID) *uint64 { if _, ok := fields[id]; !ok { return nil }; value := getU64(fields, id); return &value }
func optionalF64Value(fields map[FieldID]wireField, id FieldID) *float64 { if _, ok := fields[id]; !ok { return nil }; value := getF64(fields, id); return &value }
func optionalBoolValue(fields map[FieldID]wireField, id FieldID) *bool { if _, ok := fields[id]; !ok { return nil }; value := getBool(fields, id); return &value }
func optionalBackend(fields map[FieldID]wireField, id FieldID) *Backend { if _, ok := fields[id]; !ok { return nil }; value := Backend(getU32(fields, id)); return &value }
