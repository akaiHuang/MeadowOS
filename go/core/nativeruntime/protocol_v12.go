package nativeruntime

type MessageRole uint32
const ( MessageRoleSystem MessageRole = 1; MessageRoleUser MessageRole = 2; MessageRoleAssistant MessageRole = 3; MessageRoleTool MessageRole = 4 )
type ContentPartKind uint32
const ( ContentPartText ContentPartKind = 1; ContentPartImage ContentPartKind = 2 )
type ToolChoiceMode uint32
const ( ToolChoiceAuto ToolChoiceMode = 1; ToolChoiceNone ToolChoiceMode = 2; ToolChoiceRequired ToolChoiceMode = 3; ToolChoiceFunction ToolChoiceMode = 4 )
type ThinkingMode uint32
const ( ThinkingAuto ThinkingMode = iota; ThinkingOff; ThinkingOn )
type ReasoningEffort uint32
const ( ReasoningUnspecified ReasoningEffort = iota; ReasoningMinimal; ReasoningLow; ReasoningMedium; ReasoningHigh; ReasoningMax )
type SamplingPhase uint32
const ( SamplingUnspecified SamplingPhase = iota; SamplingSolve; SamplingAudit; SamplingAuditRecovery; SamplingRepair; SamplingRecovery; SamplingFinal; SamplingPrecision )
type TypedValueKind uint32
const ( TypedNull TypedValueKind = iota; TypedBool; TypedI64; TypedU64; TypedF64; TypedUTF8; TypedBytes; TypedList; TypedMap )

type TypedValue struct { Kind TypedValueKind; Bool bool; I64 int64; U64 uint64; F64 float64; UTF8 string; Bytes []byte; List []TypedValue; Map map[string]TypedValue }
type MessageContentPart struct { Kind ContentPartKind; Text string; ImageIndex *uint32; ImageDetail *string; SourceURI *string }
type MessageToolCall struct { ID string; Type string; Name string; Arguments string }
type ChatMessage struct { Role MessageRole; Content []MessageContentPart; ToolCalls []MessageToolCall; ToolCallID *string }
type ToolDefinition struct { Type string; Name string; Description string; ParametersJSON string }
type ToolChoice struct { Mode ToolChoiceMode; FunctionName *string; ExtensionJSON *string }
type ChatRequest struct {
	ModelID string
	ExpectedModelGeneration uint64
	Messages []ChatMessage
	Tools []ToolDefinition
	ToolChoice *ToolChoice
	Stop []string
	MaxOutputTokens uint32
	Temperature *float64
	TopP *float64
	TopK *uint32
	Seed *uint64
	PresencePenalty *float64
	RepetitionPenalty *float64
	ParallelToolCalls *bool
	StreamRequested bool
	IncludeUsage bool
	ThinkingMode *ThinkingMode
	ReasoningEffort *ReasoningEffort
	ReasoningBudgetTokens *uint32
	PreserveThinking *bool
	SessionID *string
	CacheSalt *string
	ChatTemplateKwargs map[string]TypedValue
	SamplingPhase *SamplingPhase
	StreamedToolDraftName *string
	Images []EncodedImage
}
func (*ChatRequest) Kind() PayloadKind { return PayloadChatRequest }

type ToolDraftSource uint32
const ( ToolDraftNative ToolDraftSource = 1; ToolDraftTaggedRepair ToolDraftSource = 2; ToolDraftPlainJSONRepair ToolDraftSource = 3; ToolDraftQwenEnvelopeRepair ToolDraftSource = 4 )
type CacheEventKind uint32
const ( CacheMiss CacheEventKind = 1; CacheFullHit CacheEventKind = 2; CachePartialHit CacheEventKind = 3; CacheWrite CacheEventKind = 4; CacheEvict CacheEventKind = 5; CacheBypass CacheEventKind = 6 )
type CancelDisposition uint32
const ( CancelAccepted CancelDisposition = 1; CancelAlreadyTerminal CancelDisposition = 2; CancelTargetNotFound CancelDisposition = 3; CancelRejected CancelDisposition = 4 )
type ErrorStage uint32
const ( ErrorHandshake ErrorStage = 1; ErrorModelLoad ErrorStage = 2; ErrorRequestValidation ErrorStage = 3; ErrorVisionEncode ErrorStage = 4; ErrorCache ErrorStage = 5; ErrorPrefill ErrorStage = 6; ErrorDecode ErrorStage = 7; ErrorToolStream ErrorStage = 8; ErrorCancel ErrorStage = 9; ErrorShutdown ErrorStage = 10 )
type ErrorSeverity uint32
const ( ErrorWarning ErrorSeverity = 1; ErrorTerminal ErrorSeverity = 2 )
const ( CompletionTruncatedToolDraft uint64 = 1 << iota; CompletionProtocolRepaired; CompletionProtocolRecovered; CompletionProtocolLeak; CompletionUncommittedMutationNarration; CompletionThoughtBurnAborted; CompletionUsageEstimated; CompletionCacheDegraded )
const ( ProgressIncludesActivation uint64 = 1 << iota; ProgressFirstTokenVisible; ProgressIngestionReused )

type StreamUsage struct { InputTokens uint64; OutputTokens uint64; TotalTokens uint64; CachedInputTokens *uint64; DraftTokens *uint64; AcceptedDraftTokens *uint64; ReasoningTokens *uint64; StreamSequence uint64 }
func (*StreamUsage) Kind() PayloadKind { return PayloadStreamUsage }
type StreamCache struct { Event CacheEventKind; KeyDigest []byte; CachedInputTokens *uint64; WrittenTokens *uint64; EvictedTokens *uint64; StreamSequence uint64 }
func (*StreamCache) Kind() PayloadKind { return PayloadStreamCache }
type CancelAck struct { TargetStreamID uint64; Disposition CancelDisposition; AcceptedSequence *uint64 }
func (*CancelAck) Kind() PayloadKind { return PayloadCancelAck }
