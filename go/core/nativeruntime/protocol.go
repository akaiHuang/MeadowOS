package nativeruntime

const (
	ABIMajor uint32 = 1
	ABIMinor uint32 = 0

	ProtocolMajor uint16 = 1
	ProtocolMinor uint16 = 2

	Magic            uint32 = 0x3154524d // MRT1 in little-endian order.
	FrameHeaderBytes        = 64
	TLVHeaderBytes          = 8
	ImageListHeaderBytes    = 8
	ImageRecordHeaderBytes  = 24

	MaxFramePayloadBytes uint64 = 256 << 20
	MaxTextBytes         uint64 = 8 << 20
	MaxImageBytes        uint64 = 240 << 20
	MaxToolBytes         uint64 = 8 << 20
	MaxOutputTokens      uint32 = 131072
	MaxFields                   = 64
	MaxActiveRequests           = 128
	MaxModelIDBytes             = 256
	MaxSessionIDBytes           = 128
	MaxBuildIDBytes             = 128
	MaxToolNameBytes            = 128
	MaxToolCallIDBytes          = 128
	MaxErrorDomainBytes         = 128
	MaxErrorMessageBytes        = 16384
	MaxProgressMessageBytes     = 4096
	MaxImages                   = 16
	MaxImageDimension    uint32 = 32768
	MaxMessages                 = 256
	MaxMessageParts             = 512
	MaxTools                    = 256
	MaxToolCalls                = 128
	MaxStops                    = 64
	MaxNestedRecordBytes uint64 = 16 << 20
	MaxTemplateKwargsBytes uint64 = 1 << 20
	MaxTypedValueDepth              = 64
)

type Version struct {
	Major uint16
	Minor uint16
}

func CurrentVersion() Version {
	return Version{Major: ProtocolMajor, Minor: ProtocolMinor}
}

type Status uint32

const (
	StatusOK Status = iota
	StatusInvalidArgument
	StatusBufferTooSmall
	StatusUnsupportedABI
	StatusUnsupportedProtocol
	StatusBadMagic
	StatusBadHeader
	StatusBadLength
	StatusBadChecksum
	StatusBadEnum
	StatusBadUTF8
	StatusDuplicateOrUnsortedField
	StatusMissingField
	StatusUnknownCriticalField
	StatusInvalidState
	StatusSequenceError
	StatusCapabilityDenied
	StatusNotFound
	StatusConflict
	StatusResourceLimit
	StatusConnectionPoisoned
	StatusInternalError
)

type Direction uint8

const (
	DirectionCoreToRuntime Direction = 1
	DirectionRuntimeToCore Direction = 2
)

type FrameClass uint16

const (
	FrameClassRequest FrameClass = 1
	FrameClassEvent   FrameClass = 2
)

type PayloadKind uint32

const (
	PayloadHandshakeRequest  PayloadKind = 0x0100
	PayloadCapabilityRequest PayloadKind = 0x0101
	PayloadHealthRequest     PayloadKind = 0x0102
	PayloadModelLoad         PayloadKind = 0x0200
	PayloadModelUnload       PayloadKind = 0x0201
	PayloadModelSwitch       PayloadKind = 0x0202
	PayloadTextRequest       PayloadKind = 0x0300
	PayloadVisionRequest     PayloadKind = 0x0301
	PayloadCancelRequest     PayloadKind = 0x0302
	PayloadChatRequest       PayloadKind = 0x0303

	PayloadHandshakeResponse  PayloadKind = 0x8100
	PayloadCapabilityResponse PayloadKind = 0x8101
	PayloadHealthResponse     PayloadKind = 0x8102
	PayloadModelState         PayloadKind = 0x8200
	PayloadStreamToken        PayloadKind = 0x8300
	PayloadStreamTool         PayloadKind = 0x8301
	PayloadStreamProgress     PayloadKind = 0x8302
	PayloadStreamUsage        PayloadKind = 0x8303
	PayloadStreamCache        PayloadKind = 0x8304
	PayloadCancelAck          PayloadKind = 0x8305
	PayloadStreamError        PayloadKind = 0x83fe
	PayloadStreamCompletion   PayloadKind = 0x83ff
)

type WireType uint8

const (
	WireU32   WireType = 1
	WireU64   WireType = 2
	WireF64   WireType = 3
	WireBool  WireType = 4
	WireUTF8  WireType = 5
	WireBytes WireType = 6
)

type FieldFlags uint8

const (
	FieldFlagNone     FieldFlags = 0
	FieldFlagCritical FieldFlags = 1
	KnownFieldFlags              = FieldFlagCritical
)

type FieldID uint16

const (
	FieldClientABIMajor          FieldID = 1
	FieldClientABIMinor          FieldID = 2
	FieldMinProtocolMajor        FieldID = 3
	FieldMinProtocolMinor        FieldID = 4
	FieldMaxProtocolMajor        FieldID = 5
	FieldMaxProtocolMinor        FieldID = 6
	FieldRequiredCapabilities    FieldID = 7
	FieldClientBuildID           FieldID = 8
	FieldClientNonce             FieldID = 9
	FieldSelectedProtocolMajor   FieldID = 10
	FieldSelectedProtocolMinor   FieldID = 11
	FieldRuntimeABIMajor         FieldID = 12
	FieldRuntimeABIMinor         FieldID = 13
	FieldRuntimeBuildID          FieldID = 14
	FieldRuntimeNonce            FieldID = 15
	FieldDesiredCapabilities     FieldID = 20
	FieldOfferedCapabilities     FieldID = 21
	FieldNegotiatedCapabilities  FieldID = 22
	FieldHealthState             FieldID = 30
	FieldUptimeMS                FieldID = 31
	FieldActiveRequests          FieldID = 32
	FieldResidentBytes           FieldID = 33
	FieldHealthMessage           FieldID = 34
	FieldRuntimeGeneration       FieldID = 35
	FieldModelID                 FieldID = 40
	FieldModelProfileDigest      FieldID = 41
	FieldBackend                 FieldID = 42
	FieldMemoryLimitBytes        FieldID = 43
	FieldExpectedModelGeneration FieldID = 44
	FieldModelGeneration         FieldID = 45
	FieldModelState              FieldID = 46
	FieldModelResidentBytes      FieldID = 47
	FieldModelErrorCode          FieldID = 48
	FieldModelErrorMessage       FieldID = 49
	FieldPromptUTF8              FieldID = 60
	FieldSystemUTF8              FieldID = 61
	FieldMaxOutputTokens         FieldID = 62
	FieldTemperature             FieldID = 63
	FieldTopP                    FieldID = 64
	FieldSeed                    FieldID = 65
	FieldThinkMode               FieldID = 66
	FieldImageList               FieldID = 67
	FieldSessionID               FieldID = 68
	FieldTokenID                 FieldID = 80
	FieldTokenIndex              FieldID = 81
	FieldTokenUTF8               FieldID = 82
	FieldStreamChannel           FieldID = 83
	FieldTokenLogprob            FieldID = 84
	FieldToolEventKind           FieldID = 90
	FieldToolCallID              FieldID = 91
	FieldToolName                FieldID = 92
	FieldToolDataUTF8            FieldID = 93
	FieldToolSequence            FieldID = 94
	FieldProgressPhase           FieldID = 100
	FieldProgressFraction        FieldID = 101
	FieldProgressMessage         FieldID = 102
	FieldProgressCurrent         FieldID = 103
	FieldProgressTotal           FieldID = 104
	FieldErrorCode               FieldID = 110
	FieldErrorDomain             FieldID = 111
	FieldErrorMessage            FieldID = 112
	FieldErrorRetryable          FieldID = 113
	FieldErrorDetailsUTF8        FieldID = 114
	FieldFinishReason            FieldID = 120
	FieldInputTokens             FieldID = 121
	FieldOutputTokens            FieldID = 122
	FieldCompletionMessage       FieldID = 123
	FieldCachedInputTokens       FieldID = 124
	FieldDraftTokens             FieldID = 125
	FieldAcceptedDraftTokens     FieldID = 126
	FieldTargetStreamID          FieldID = 130
	FieldCancelReason            FieldID = 131
	FieldCancelMessage           FieldID = 132
	FieldMessageList             FieldID = 133
	FieldToolList                FieldID = 134
	FieldToolChoice              FieldID = 135
	FieldStopList                FieldID = 136
	FieldTopK                    FieldID = 137
	FieldPresencePenalty         FieldID = 138
	FieldRepetitionPenalty       FieldID = 139
	FieldParallelToolCalls       FieldID = 140
	FieldStreamRequested         FieldID = 141
	FieldIncludeUsage            FieldID = 142
	FieldThinkingMode            FieldID = 143
	FieldReasoningEffort         FieldID = 144
	FieldReasoningBudgetTokens   FieldID = 145
	FieldPreserveThinking        FieldID = 146
	FieldCacheSalt               FieldID = 147
	FieldChatTemplateKwargs      FieldID = 148
	FieldSamplingPhase           FieldID = 149
	FieldStreamedToolDraftName   FieldID = 150
	FieldChoiceIndex             FieldID = 151
	FieldToolCallIndex           FieldID = 152
	FieldToolCallType            FieldID = 153
	FieldToolArgumentsDelta      FieldID = 154
	FieldToolDraftTotalBytes     FieldID = 155
	FieldToolDraftSource         FieldID = 156
	FieldCompletionFlags         FieldID = 157
	FieldStreamSequence          FieldID = 158
	FieldTotalTokens             FieldID = 159
	FieldReasoningTokens         FieldID = 160
	FieldCacheEventKind          FieldID = 161
	FieldCacheKeyDigest          FieldID = 162
	FieldCacheWrittenTokens      FieldID = 163
	FieldCacheEvictedTokens      FieldID = 164
	FieldCancelDisposition       FieldID = 165
	FieldCancelAcceptedSequence  FieldID = 166
	FieldErrorStage              FieldID = 167
	FieldErrorSeverity           FieldID = 168
	FieldRetryAfterMS            FieldID = 169
	FieldUnderlyingStatus        FieldID = 170
	FieldFailedFieldID           FieldID = 171
	FieldErrorDetailsBytes       FieldID = 172
	FieldElapsedUS               FieldID = 173
	FieldPromptTokens            FieldID = 174
	FieldCachedTokens            FieldID = 175
	FieldRemainingTokens         FieldID = 176
	FieldPrefillProcessedTokens  FieldID = 177
	FieldPrefillTotalTokens      FieldID = 178
	FieldCompletionTokens        FieldID = 179
	FieldCacheHitType            FieldID = 180
	FieldTTFTUS                  FieldID = 181
	FieldActivationUS            FieldID = 182
	FieldProgressFlags           FieldID = 183
	FieldPromptEstimatedTokens   FieldID = 184
	FieldReadingSource           FieldID = 185
	FieldReadingExcerpt          FieldID = 186
	FieldPrefillObservedTokens   FieldID = 187
	FieldPrefillObservedElapsedUS FieldID = 188
	FieldIngestionSourceID       FieldID = 189
	FieldIngestionSourceName     FieldID = 190
	FieldIngestionProcessed      FieldID = 191
	FieldIngestionTotal          FieldID = 192
	FieldIngestionProcessedBytes FieldID = 193
	FieldIngestionTotalBytes     FieldID = 194
	FieldIngestionStartLine      FieldID = 195
	FieldIngestionEndLine        FieldID = 196
	FieldIngestionNote           FieldID = 197
)

type CapabilityMask uint64

const (
	CapabilityText CapabilityMask = 1 << iota
	CapabilityVision
	CapabilityStreamToken
	CapabilityStreamTool
	CapabilityStreamProgress
	CapabilityCancel
	CapabilityModelSwitch
	CapabilityHealth
	CapabilityBackendSGLang
	CapabilityBackendMTPLX
	CapabilityThinkMode
	CapabilitySessionCache
	CapabilityStructuredMessages
	CapabilityToolDefinitions
	CapabilityToolChoice
	CapabilityStopSequences
	CapabilityExtendedSampling
	CapabilityReasoningControls
	CapabilityMultipartVision
	CapabilityStreamUsage
	CapabilityStreamCache
	CapabilityCancelAck
	CapabilityToolDraftMetadata
	CapabilityStructuredErrors
	CapabilityDetailedProgress
	CapabilityParallelToolCalls
	CapabilityChatTemplateKwargs
	CapabilityCacheSalt

	KnownCapabilities CapabilityMask = (1 << 28) - 1
)

func (m CapabilityMask) Has(capability CapabilityMask) bool {
	return m&capability == capability
}

type Backend uint32

const (
	BackendAuto     Backend = 1
	BackendBaseline Backend = 2
	BackendSGLang   Backend = 3
	BackendMTPLX    Backend = 4
)

type ModelLifecycleState uint32

const (
	ModelEmpty     ModelLifecycleState = 1
	ModelLoading   ModelLifecycleState = 2
	ModelReady     ModelLifecycleState = 3
	ModelSwitching ModelLifecycleState = 4
	ModelUnloading ModelLifecycleState = 5
	ModelFailed    ModelLifecycleState = 6
)

type HealthState uint32

const (
	HealthStarting HealthState = 1
	HealthReady    HealthState = 2
	HealthDegraded HealthState = 3
	HealthFailed   HealthState = 4
	HealthStopping HealthState = 5
)

type StreamChannel uint32

const (
	StreamAnswer    StreamChannel = 1
	StreamReasoning StreamChannel = 2
	StreamCode      StreamChannel = 3
)

type ToolEventKind uint32

const (
	ToolBegin         ToolEventKind = 1
	ToolArgumentDelta ToolEventKind = 2
	ToolEnd           ToolEventKind = 3
)

type ProgressPhase uint32

const (
	ProgressQueued      ProgressPhase = 1
	ProgressLoading     ProgressPhase = 2
	ProgressPrefill     ProgressPhase = 3
	ProgressDecoding    ProgressPhase = 4
	ProgressToolPrepare ProgressPhase = 5
	ProgressFinalizing  ProgressPhase = 6
	ProgressVisionEncode ProgressPhase = 7
)

type FinishReason uint32

const (
	FinishStop      FinishReason = 1
	FinishLength    FinishReason = 2
	FinishCancelled FinishReason = 3
	FinishToolCall  FinishReason = 4
)

type CancelReason uint32

const (
	CancelUser       CancelReason = 1
	CancelSuperseded CancelReason = 2
	CancelTimeout    CancelReason = 3
	CancelShutdown   CancelReason = 4
)

type ImageFormat uint32

const (
	ImageJPEG ImageFormat = 1
	ImagePNG  ImageFormat = 2
	ImageWEBP ImageFormat = 3
	ImageHEIF ImageFormat = 4
	ImageAVIF ImageFormat = 5
)

type PendingModelOperation uint32

const (
	PendingModelNone PendingModelOperation = iota
	PendingModelLoad
	PendingModelUnload
	PendingModelSwitch
)

type Payload interface {
	Kind() PayloadKind
}

type Frame struct {
	Protocol      Version
	Class         FrameClass
	Flags         uint32
	StreamID      uint64
	Sequence      uint64
	CorrelationID uint64
	Payload       Payload
}

type HandshakeRequest struct {
	ClientABIMajor       uint32
	ClientABIMinor       uint32
	MinProtocolMajor     uint32
	MinProtocolMinor     uint32
	MaxProtocolMajor     uint32
	MaxProtocolMinor     uint32
	RequiredCapabilities CapabilityMask
	ClientBuildID        string
	ClientNonce          [32]byte
}

func (*HandshakeRequest) Kind() PayloadKind { return PayloadHandshakeRequest }

type HandshakeResponse struct {
	SelectedProtocolMajor uint32
	SelectedProtocolMinor uint32
	RuntimeABIMajor       uint32
	RuntimeABIMinor       uint32
	RuntimeBuildID        string
	RuntimeNonce          [32]byte
}

func (*HandshakeResponse) Kind() PayloadKind { return PayloadHandshakeResponse }

type CapabilityRequest struct {
	Required CapabilityMask
	Desired  CapabilityMask
}

func (*CapabilityRequest) Kind() PayloadKind { return PayloadCapabilityRequest }

type CapabilityResponse struct {
	Offered    CapabilityMask
	Negotiated CapabilityMask
}

func (*CapabilityResponse) Kind() PayloadKind { return PayloadCapabilityResponse }

type HealthRequest struct{}

func (*HealthRequest) Kind() PayloadKind { return PayloadHealthRequest }

type HealthResponse struct {
	State             HealthState
	UptimeMS          uint64
	ActiveRequests    uint32
	ResidentBytes     uint64
	Message           *string
	RuntimeGeneration uint64
	ModelID           *string
	ModelState        ModelLifecycleState
}

func (*HealthResponse) Kind() PayloadKind { return PayloadHealthResponse }

type ModelLoad struct {
	ModelID          string
	ProfileDigest    [32]byte
	Backend          Backend
	MemoryLimitBytes uint64
}

func (*ModelLoad) Kind() PayloadKind { return PayloadModelLoad }

type ModelUnload struct {
	ExpectedGeneration uint64
}

func (*ModelUnload) Kind() PayloadKind { return PayloadModelUnload }

type ModelSwitch struct {
	ModelID           string
	ProfileDigest     [32]byte
	Backend           Backend
	MemoryLimitBytes  uint64
	ExpectedGeneration uint64
}

func (*ModelSwitch) Kind() PayloadKind { return PayloadModelSwitch }

type ModelStatePayload struct {
	ModelID       *string
	Backend       *Backend
	Generation    uint64
	State         ModelLifecycleState
	ResidentBytes uint64
	ErrorCode     *uint32
	ErrorMessage  *string
}

func (*ModelStatePayload) Kind() PayloadKind { return PayloadModelState }

type TextRequest struct {
	Prompt          string
	System          *string
	MaxOutputTokens uint32
	Temperature     *float64
	TopP            *float64
	Seed            *uint64
	ThinkMode       *bool
	SessionID       *string
}

func (*TextRequest) Kind() PayloadKind { return PayloadTextRequest }

type EncodedImage struct {
	Format ImageFormat
	Width  uint32
	Height uint32
	Data   []byte
}

type VisionRequest struct {
	Prompt          string
	System          *string
	MaxOutputTokens uint32
	Temperature     *float64
	TopP            *float64
	Seed            *uint64
	ThinkMode       *bool
	Images          []EncodedImage
	SessionID       *string
}

func (*VisionRequest) Kind() PayloadKind { return PayloadVisionRequest }

type CancelRequest struct {
	TargetStreamID uint64
	Reason         CancelReason
	Message        *string
}

func (*CancelRequest) Kind() PayloadKind { return PayloadCancelRequest }

type StreamToken struct {
	TokenID  uint32
	Index    uint64
	Text     *string
	Channel  StreamChannel
	Logprob  *float64
	ChoiceIndex *uint32
	StreamSequence *uint64
}

func (*StreamToken) Kind() PayloadKind { return PayloadStreamToken }

type StreamTool struct {
	EventKind ToolEventKind
	CallID    string
	Name      *string
	Data      *string
	Sequence  uint64
	ChoiceIndex *uint32
	ToolCallIndex *uint32
	ToolCallType *string
	ArgumentsDelta *string
	DraftTotalBytes *uint64
	DraftSource *ToolDraftSource
	StreamSequence *uint64
}

func (*StreamTool) Kind() PayloadKind { return PayloadStreamTool }

type StreamProgress struct {
	Phase    ProgressPhase
	Fraction float64
	Message  *string
	Current  *uint64
	Total    *uint64
	StreamSequence *uint64
	ElapsedUS *uint64
	PromptTokens *uint64
	CachedTokens *uint64
	RemainingTokens *uint64
	PrefillProcessedTokens *uint64
	PrefillTotalTokens *uint64
	CompletionTokens *uint64
	CacheHitType *string
	TTFTUS *uint64
	ActivationUS *uint64
	ProgressFlags *uint64
	PromptEstimatedTokens *uint64
	ReadingSource *string
	ReadingExcerpt *string
	PrefillObservedTokens *uint64
	PrefillObservedElapsedUS *uint64
	IngestionSourceID *string
	IngestionSourceName *string
	IngestionProcessed *uint64
	IngestionTotal *uint64
	IngestionProcessedBytes *uint64
	IngestionTotalBytes *uint64
	IngestionStartLine *uint64
	IngestionEndLine *uint64
	IngestionNote *string
}

func (*StreamProgress) Kind() PayloadKind { return PayloadStreamProgress }

type StreamError struct {
	Code      uint32
	Domain    string
	Message   string
	Retryable bool
	Details   *string
	Stage *ErrorStage
	Severity *ErrorSeverity
	RetryAfterMS *uint64
	UnderlyingStatus *uint32
	FailedFieldID *uint32
	DetailsBytes []byte
	StreamSequence *uint64
}

func (*StreamError) Kind() PayloadKind { return PayloadStreamError }

type StreamCompletion struct {
	Reason              FinishReason
	InputTokens         uint64
	OutputTokens        uint64
	Message             *string
	CachedInputTokens   *uint64
	DraftTokens         *uint64
	AcceptedDraftTokens *uint64
	TotalTokens *uint64
	ReasoningTokens *uint64
	CompletionFlags *uint64
	StreamSequence *uint64
}

func (*StreamCompletion) Kind() PayloadKind { return PayloadStreamCompletion }
