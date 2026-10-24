package nativeruntime

import (
	"encoding/json"
	"math"
	"unicode/utf8"
)

func versionRangeIntersectsSupported(minMajor, minMinor, maxMajor, maxMinor uint32) bool {
	if minMajor > maxMajor || (minMajor == maxMajor && minMinor > maxMinor) { return false }
	if maxMajor < uint32(ProtocolMajor) || minMajor > uint32(ProtocolMajor) { return false }
	if minMajor == uint32(ProtocolMajor) && minMinor > uint32(ProtocolMinor) { return false }
	if maxMajor == uint32(ProtocolMajor) && maxMinor < 1 { return false }
	return true
}

func validatePayloadV12(payload Payload) (bool, error) {
	switch p := payload.(type) {
	case *ChatRequest:
		return true, validateChatRequestV12(p)
	case *StreamUsage:
		if p == nil || p.StreamSequence == 0 || p.TotalTokens != p.InputTokens+p.OutputTokens { return true, protocolError(StatusInvalidArgument, "stream usage requires sequence and exact total") }
		if p.AcceptedDraftTokens != nil && (p.DraftTokens == nil || *p.AcceptedDraftTokens > *p.DraftTokens) { return true, protocolError(StatusInvalidArgument, "accepted draft tokens exceed draft tokens") }
		return true, nil
	case *StreamCache:
		if p == nil || !validCacheEvent(p.Event) || p.StreamSequence == 0 { return true, protocolError(StatusBadEnum, "invalid stream cache event") }
		if len(p.KeyDigest) != 0 && len(p.KeyDigest) != 32 { return true, protocolError(StatusBadLength, "cache key digest must contain 32 bytes") }
		return true, nil
	case *CancelAck:
		if p == nil || p.TargetStreamID == 0 || !validCancelDisposition(p.Disposition) { return true, protocolError(StatusInvalidArgument, "invalid cancel acknowledgment") }
		if p.Disposition == CancelAccepted && (p.AcceptedSequence == nil || *p.AcceptedSequence == 0) { return true, protocolError(StatusMissingField, "accepted cancellation requires accepted sequence") }
		if p.Disposition != CancelAccepted && p.AcceptedSequence != nil { return true, protocolError(StatusInvalidState, "non-accepted cancellation cannot carry accepted sequence") }
		return true, nil
	case *StreamToken:
		return false, validateOptionalStreamSequence(p.StreamSequence)
	case *StreamTool:
		if err := validateOptionalStreamSequence(p.StreamSequence); err != nil { return false, err }
		if p.DraftSource != nil && !validToolDraftSource(*p.DraftSource) { return false, protocolError(StatusBadEnum, "invalid tool draft source") }
		if p.ArgumentsDelta != nil && uint64(len(*p.ArgumentsDelta)) > MaxToolBytes { return false, protocolError(StatusResourceLimit, "tool argument delta exceeds limit") }
		if p.DraftTotalBytes != nil && *p.DraftTotalBytes > MaxToolBytes { return false, protocolError(StatusResourceLimit, "tool draft exceeds limit") }
		return false, nil
	case *StreamProgress:
		if err:=validateOptionalStreamSequence(p.StreamSequence);err!=nil{return false,err};if !orderedOptionalPair(p.PrefillProcessedTokens,p.PrefillTotalTokens)||!orderedOptionalPair(p.IngestionProcessed,p.IngestionTotal)||!orderedOptionalPair(p.IngestionProcessedBytes,p.IngestionTotalBytes)||!orderedOptionalPair(p.IngestionStartLine,p.IngestionEndLine){return false,protocolError(StatusConflict,"detailed progress counters are inconsistent")};return false,nil
	case *StreamError:
		if err := validateOptionalStreamSequence(p.StreamSequence); err != nil { return false, err }
		if p.Stage != nil && !validErrorStage(*p.Stage) { return false, protocolError(StatusBadEnum, "invalid error stage") }
		if p.Severity != nil && !validErrorSeverity(*p.Severity) { return false, protocolError(StatusBadEnum, "invalid error severity") }
		if uint64(len(p.DetailsBytes)) > MaxTextBytes { return false, protocolError(StatusResourceLimit, "structured error details exceed limit") }
		return false, nil
	case *StreamCompletion:
		if err := validateOptionalStreamSequence(p.StreamSequence); err != nil { return false, err }
		if p.TotalTokens != nil && *p.TotalTokens != p.InputTokens+p.OutputTokens { return false, protocolError(StatusInvalidArgument, "completion total tokens are not exact") }
		return false, nil
	default:
		return false, nil
	}
}

func validateChatRequestV12(p *ChatRequest) error {
	if p == nil { return protocolError(StatusInvalidArgument, "nil chat request") }
	if err := validateIdentifier("model id", p.ModelID, MaxModelIDBytes); err != nil { return err }
	if p.ExpectedModelGeneration == 0 || p.MaxOutputTokens == 0 || p.MaxOutputTokens > MaxOutputTokens { return protocolError(StatusInvalidArgument, "chat request requires model generation and bounded output tokens") }
	if len(p.Messages) == 0 || len(p.Messages) > MaxMessages { return protocolError(StatusResourceLimit, "chat message count must be in [1,%d]", MaxMessages) }
	if len(p.Tools) > MaxTools || len(p.Stop) > MaxStops || len(p.Images) > MaxImages { return protocolError(StatusResourceLimit, "chat request collection exceeds protocol limit") }
	if p.Temperature != nil && (math.IsNaN(*p.Temperature) || math.IsInf(*p.Temperature,0) || *p.Temperature < 0) { return protocolError(StatusBadEnum, "temperature must be finite and non-negative") }
	if p.TopP != nil && (!isFiniteRange(*p.TopP,0,1)) { return protocolError(StatusBadEnum, "top_p must be in [0,1]") }
	if p.PresencePenalty != nil && !isFiniteRange(*p.PresencePenalty,-2,2) { return protocolError(StatusBadEnum, "presence penalty must be in [-2,2]") }
	if p.RepetitionPenalty != nil && (math.IsNaN(*p.RepetitionPenalty) || math.IsInf(*p.RepetitionPenalty,0) || *p.RepetitionPenalty <= 0) { return protocolError(StatusBadEnum, "repetition penalty must be positive") }
	if p.ThinkingMode != nil && *p.ThinkingMode > ThinkingOn { return protocolError(StatusBadEnum, "invalid thinking mode") }
	if p.ReasoningEffort != nil && *p.ReasoningEffort > ReasoningMax { return protocolError(StatusBadEnum, "invalid reasoning effort") }
	if p.SamplingPhase != nil && (*p.SamplingPhase < SamplingSolve || *p.SamplingPhase > SamplingPrecision) { return protocolError(StatusBadEnum, "invalid sampling phase") }
	if p.SessionID != nil { if err := validateIdentifier("session id",*p.SessionID,MaxSessionIDBytes); err != nil { return err } }
	if p.CacheSalt != nil { if err := validateText("cache salt",*p.CacheSalt,MaxSessionIDBytes,false); err != nil { return err } }
	imageRefs := 0
	for _, message := range p.Messages {
		if message.Role < MessageRoleSystem || message.Role > MessageRoleTool { return protocolError(StatusBadEnum, "invalid message role") }
		if len(message.Content) > MaxMessageParts || len(message.ToolCalls) > MaxToolCalls { return protocolError(StatusResourceLimit, "message content exceeds protocol limit") }
		if message.Role == MessageRoleTool && message.ToolCallID == nil { return protocolError(StatusMissingField, "tool message requires tool_call_id") }
		for _, part := range message.Content {
			switch part.Kind {
			case ContentPartText:
				if part.ImageIndex != nil || !utf8.ValidString(part.Text) { return protocolError(StatusBadUTF8, "invalid text content part") }
			case ContentPartImage:
				if part.ImageIndex == nil || int(*part.ImageIndex) >= len(p.Images) { return protocolError(StatusBadLength, "image content part references missing image") }
				imageRefs++
			default: return protocolError(StatusBadEnum, "invalid content part kind")
			}
		}
		for _, call := range message.ToolCalls { if call.ID == "" || call.Name == "" || !json.Valid([]byte(call.Arguments)) { return protocolError(StatusBadUTF8, "message tool call is incomplete or has invalid JSON") } }
	}
	if len(p.Images) > 0 && imageRefs == 0 { return protocolError(StatusInvalidState, "unreferenced image list is forbidden") }
	if len(p.Images) > 0 { if err := validateImages(p.Images); err != nil { return err } }
	seen := make(map[string]struct{},len(p.Tools))
	for _, tool := range p.Tools { if tool.Name == "" || tool.Type == "" || !json.Valid([]byte(tool.ParametersJSON)) { return protocolError(StatusBadUTF8, "tool definition is incomplete or has invalid schema JSON") }; if _,ok:=seen[tool.Name];ok{return protocolError(StatusConflict,"duplicate tool name %q",tool.Name)};seen[tool.Name]=struct{}{} }
	if p.ToolChoice != nil { if !validToolChoiceMode(p.ToolChoice.Mode) { return protocolError(StatusBadEnum,"invalid tool choice") }; if p.ToolChoice.ExtensionJSON!=nil&&!json.Valid([]byte(*p.ToolChoice.ExtensionJSON)){return protocolError(StatusBadUTF8,"tool choice extension is not valid JSON")}; if p.ToolChoice.Mode==ToolChoiceFunction { if p.ToolChoice.FunctionName==nil { return protocolError(StatusMissingField,"specific tool choice requires function name") }; if _,ok:=seen[*p.ToolChoice.FunctionName];!ok{return protocolError(StatusNotFound,"selected tool is not defined")} } }
	for _, stop := range p.Stop { if stop=="" || !utf8.ValidString(stop) { return protocolError(StatusBadUTF8,"stop sequence must be non-empty UTF-8") } }
	if len(p.ChatTemplateKwargs)>0 { encoded,err:=encodeTypedMap(p.ChatTemplateKwargs);if err!=nil{return err};if uint64(len(encoded))>MaxTemplateKwargsBytes{return protocolError(StatusResourceLimit,"chat template kwargs exceed limit")} }
	return nil
}

func validateOptionalStreamSequence(sequence *uint64) error { if sequence != nil && *sequence == 0 { return protocolError(StatusSequenceError,"stream sequence starts at one") }; return nil }
func isFiniteRange(value,min,max float64)bool{return !math.IsNaN(value)&&!math.IsInf(value,0)&&value>=min&&value<=max}
func validToolChoiceMode(value ToolChoiceMode)bool{return value>=ToolChoiceAuto&&value<=ToolChoiceFunction}
func validToolDraftSource(value ToolDraftSource)bool{return value>=ToolDraftNative&&value<=ToolDraftQwenEnvelopeRepair}
func validCacheEvent(value CacheEventKind)bool{return value>=CacheMiss&&value<=CacheBypass}
func validCancelDisposition(value CancelDisposition)bool{return value>=CancelAccepted&&value<=CancelRejected}
func validErrorStage(value ErrorStage)bool{return value>=ErrorHandshake&&value<=ErrorShutdown}
func validErrorSeverity(value ErrorSeverity)bool{return value==ErrorWarning||value==ErrorTerminal}
func payloadRequiresV12(payload Payload)bool{switch payload.(type){case *ChatRequest,*StreamUsage,*StreamCache,*CancelAck:return true;default:return false}}
func orderedOptionalPair(current,total *uint64)bool{if (current==nil)!=(total==nil){return false};return current==nil||*current<=*total}

func validPayloadKindV12(kind PayloadKind) bool {
	switch kind { case PayloadChatRequest,PayloadStreamUsage,PayloadStreamCache,PayloadCancelAck:return true;default:return validPayloadKind(kind) }
}
func isRequestKindV12(kind PayloadKind) bool { if kind==PayloadChatRequest{return true};return isRequestKind(kind) }
func requiresStreamIDV12(kind PayloadKind) bool { switch kind{case PayloadChatRequest,PayloadStreamUsage,PayloadStreamCache,PayloadCancelAck:return true;default:return requiresStreamID(kind)} }
