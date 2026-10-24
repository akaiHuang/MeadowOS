package nativeruntime

import "sync"

type ProtocolState struct {
	mu sync.Mutex

	poisoned bool
	coreSequence    uint64
	runtimeSequence uint64

	handshakeRequest    *HandshakeRequest
	handshakeCorrelation uint64
	handshakeComplete   bool
	selectedMinor uint16
	capabilityRequest    *CapabilityRequest
	capabilityCorrelation uint64
	capabilitiesComplete bool
	negotiated           CapabilityMask

	modelState      ModelLifecycleState
	modelID         string
	backend         Backend
	modelGeneration uint64
	pendingModel    *pendingModelState
	runtimeGeneration uint64

	pendingHealth map[uint64]struct{}
	active        map[uint64]*activeRequest
	pendingCancels map[uint64]pendingCancelState
	terminal map[uint64]struct{}
}

type pendingModelState struct {
	operation      PendingModelOperation
	correlationID  uint64
	modelID        string
	backend        Backend
	generation     uint64
}

type activeRequest struct {
	correlationID uint64
	nextToken     uint64
	progress      float64
	cancelled     bool
	sawTool       bool
	tools         map[string]*toolStreamState
	nextStreamSequence uint64
	lastUsageInput uint64
	lastUsageOutput uint64
}

const capabilityV11Mask CapabilityMask = (1 << 12) - 1

type pendingCancelState struct { target uint64; disposition CancelDisposition; acceptedSequence *uint64 }

type toolStreamState struct {
	nextSequence uint64
	open         bool
	choiceIndex *uint32
	toolCallIndex *uint32
	toolCallType *string
	draftTotalBytes *uint64
	draftBytes uint64
}

type ProtocolSnapshot struct {
	Poisoned             bool
	CoreSequence         uint64
	RuntimeSequence      uint64
	HandshakeComplete    bool
	CapabilitiesComplete bool
	Negotiated           CapabilityMask
	ModelState           ModelLifecycleState
	ModelID              string
	Backend              Backend
	ModelGeneration      uint64
	RuntimeGeneration    uint64
	PendingModel         PendingModelOperation
	ActiveRequests       int
}

func NewProtocolState() *ProtocolState {
	return &ProtocolState{modelState: ModelEmpty, pendingHealth: make(map[uint64]struct{}), active: make(map[uint64]*activeRequest), pendingCancels:make(map[uint64]pendingCancelState), terminal:make(map[uint64]struct{})}
}

func (s *ProtocolState) Accept(direction Direction, data []byte) (*Frame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return nil, protocolError(StatusConnectionPoisoned, "connection is poisoned")
	}
	frame, err := DecodeFrame(data)
	if err != nil {
		return nil, s.poison(err)
	}
	if s.handshakeComplete && (frame.Protocol.Major!=ProtocolMajor || frame.Protocol.Minor!=s.selectedMinor){return nil,s.poison(protocolError(StatusUnsupportedProtocol,"frame protocol does not match negotiated 1.%d",s.selectedMinor))}
	if err := s.acceptFrame(direction, frame); err != nil {
		return nil, s.poison(err)
	}
	return frame, nil
}

func (s *ProtocolState) Snapshot() ProtocolSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := PendingModelNone
	if s.pendingModel != nil {
		pending = s.pendingModel.operation
	}
	return ProtocolSnapshot{Poisoned: s.poisoned, CoreSequence: s.coreSequence, RuntimeSequence: s.runtimeSequence,
		HandshakeComplete: s.handshakeComplete, CapabilitiesComplete: s.capabilitiesComplete, Negotiated: s.negotiated,
		ModelState: s.modelState, ModelID: s.modelID, Backend: s.backend, ModelGeneration: s.modelGeneration,
		RuntimeGeneration: s.runtimeGeneration, PendingModel: pending, ActiveRequests: len(s.active)}
}

func (s *ProtocolState) acceptFrame(direction Direction, frame *Frame) error {
	switch direction {
	case DirectionCoreToRuntime:
		if frame.Class != FrameClassRequest {
			return protocolError(StatusInvalidState, "core may only send request frames")
		}
		if frame.Sequence != s.coreSequence+1 {
			return protocolError(StatusSequenceError, "core sequence %d is not next after %d", frame.Sequence, s.coreSequence)
		}
		if err := s.acceptCore(frame); err != nil {
			return err
		}
		s.coreSequence = frame.Sequence
		return nil
	case DirectionRuntimeToCore:
		if frame.Class != FrameClassEvent {
			return protocolError(StatusInvalidState, "runtime may only send event frames")
		}
		if frame.Sequence != s.runtimeSequence+1 {
			return protocolError(StatusSequenceError, "runtime sequence %d is not next after %d", frame.Sequence, s.runtimeSequence)
		}
		if err := s.acceptRuntime(frame); err != nil {
			return err
		}
		s.runtimeSequence = frame.Sequence
		return nil
	default:
		return protocolError(StatusBadEnum, "invalid direction %d", direction)
	}
}

func (s *ProtocolState) acceptCore(frame *Frame) error {
	switch p := frame.Payload.(type) {
	case *HandshakeRequest:
		if s.handshakeRequest != nil || s.handshakeComplete || s.coreSequence != 0 || s.runtimeSequence != 0 {
			return protocolError(StatusConflict, "handshake request is only valid as the first frame")
		}
		s.handshakeRequest = p
		s.handshakeCorrelation = frame.CorrelationID
		return nil
	case *CapabilityRequest:
		if !s.handshakeComplete || s.capabilityRequest != nil || s.capabilitiesComplete {
			return protocolError(StatusInvalidState, "capability request requires one completed handshake")
		}
		if !p.Desired.Has(s.handshakeRequest.RequiredCapabilities) {
			return protocolError(StatusCapabilityDenied, "desired capabilities omit handshake requirements")
		}
		if s.selectedMinor<2&&(p.Desired|p.Required)&^capabilityV11Mask!=0{return protocolError(StatusCapabilityDenied,"protocol 1.1 cannot negotiate v1.2 capabilities")}
		s.capabilityRequest = p
		s.capabilityCorrelation = frame.CorrelationID
		return nil
	case *HealthRequest:
		if !s.handshakeComplete {
			return protocolError(StatusInvalidState, "health request requires handshake")
		}
		if s.capabilitiesComplete && !s.negotiated.Has(CapabilityHealth) {
			return protocolError(StatusCapabilityDenied, "health capability was not negotiated")
		}
		if _, exists := s.pendingHealth[frame.CorrelationID]; exists {
			return protocolError(StatusConflict, "duplicate pending health correlation")
		}
		s.pendingHealth[frame.CorrelationID] = struct{}{}
		return nil
	case *ModelLoad:
		if err := s.requireLifecycleIdle(); err != nil {
			return err
		}
		if s.modelState != ModelEmpty && s.modelState != ModelFailed {
			return protocolError(StatusInvalidState, "model load requires empty or failed state")
		}
		if err := s.requireBackend(p.Backend); err != nil {
			return err
		}
		s.pendingModel = &pendingModelState{operation: PendingModelLoad, correlationID: frame.CorrelationID, modelID: p.ModelID, backend: p.Backend}
		return nil
	case *ModelUnload:
		if err := s.requireLifecycleIdle(); err != nil {
			return err
		}
		if s.modelState != ModelReady && s.modelState != ModelFailed {
			return protocolError(StatusInvalidState, "model unload requires ready or failed state")
		}
		if p.ExpectedGeneration != s.modelGeneration {
			return protocolError(StatusConflict, "model unload generation does not match current generation")
		}
		s.pendingModel = &pendingModelState{operation: PendingModelUnload, correlationID: frame.CorrelationID, modelID: s.modelID, backend: s.backend}
		return nil
	case *ModelSwitch:
		if err := s.requireLifecycleIdle(); err != nil {
			return err
		}
		if !s.negotiated.Has(CapabilityModelSwitch) {
			return protocolError(StatusCapabilityDenied, "model switch capability was not negotiated")
		}
		if s.modelState != ModelReady || p.ExpectedGeneration != s.modelGeneration {
			return protocolError(StatusConflict, "model switch requires ready state and matching generation")
		}
		if err := s.requireBackend(p.Backend); err != nil {
			return err
		}
		s.pendingModel = &pendingModelState{operation: PendingModelSwitch, correlationID: frame.CorrelationID, modelID: p.ModelID, backend: p.Backend}
		return nil
	case *TextRequest:
		return s.startInference(frame, false, p.ThinkMode, p.SessionID)
	case *VisionRequest:
		return s.startInference(frame, true, p.ThinkMode, p.SessionID)
	case *ChatRequest:
		return s.startChatInference(frame,p)
	case *CancelRequest:
		if !s.capabilitiesComplete || !s.negotiated.Has(CapabilityCancel) {
			return protocolError(StatusCapabilityDenied, "cancel capability was not negotiated")
		}
		if s.selectedMinor<2{active,ok:=s.active[p.TargetStreamID];if !ok{return protocolError(StatusNotFound,"cancel target stream %d is not active",p.TargetStreamID)};if active.cancelled{return protocolError(StatusConflict,"cancel target stream %d was already cancelled",p.TargetStreamID)};active.cancelled=true;return nil}
		if !s.negotiated.Has(CapabilityCancelAck){return protocolError(StatusCapabilityDenied,"v1.2 cancellation requires cancel-ack capability")}
		if _,exists:=s.pendingCancels[frame.CorrelationID];exists{return protocolError(StatusConflict,"duplicate cancel correlation")}
		pending:=pendingCancelState{target:p.TargetStreamID,disposition:CancelTargetNotFound}
		if active,ok:=s.active[p.TargetStreamID];ok{if active.cancelled{pending.disposition=CancelRejected}else{active.cancelled=true;accepted:=s.runtimeSequence+1;pending.disposition=CancelAccepted;pending.acceptedSequence=&accepted}}else if _,ok:=s.terminal[p.TargetStreamID];ok{pending.disposition=CancelAlreadyTerminal}
		s.pendingCancels[frame.CorrelationID]=pending
		return nil
	default:
		return protocolError(StatusInvalidState, "payload %T is not a core request", frame.Payload)
	}
}

func (s *ProtocolState) acceptRuntime(frame *Frame) error {
	switch p := frame.Payload.(type) {
	case *HandshakeResponse:
		if s.handshakeRequest == nil || s.handshakeComplete || frame.CorrelationID != s.handshakeCorrelation {
			return protocolError(StatusInvalidState, "handshake response has no matching request")
		}
		if !versionRangeContains(s.handshakeRequest.MinProtocolMajor, s.handshakeRequest.MinProtocolMinor, s.handshakeRequest.MaxProtocolMajor, s.handshakeRequest.MaxProtocolMinor, p.SelectedProtocolMajor, p.SelectedProtocolMinor) {
			return protocolError(StatusUnsupportedProtocol, "selected protocol is outside requested range")
		}
		if p.SelectedProtocolMinor<2&&s.handshakeRequest.RequiredCapabilities&^capabilityV11Mask!=0{return protocolError(StatusCapabilityDenied,"protocol 1.1 cannot satisfy v1.2 handshake capabilities")}
		if frame.Protocol.Minor!=uint16(p.SelectedProtocolMinor){return protocolError(StatusUnsupportedProtocol,"handshake frame and selected minor disagree")}
		s.selectedMinor=uint16(p.SelectedProtocolMinor)
		s.handshakeComplete = true
		return nil
	case *CapabilityResponse:
		if !s.handshakeComplete || s.capabilityRequest == nil || s.capabilitiesComplete || frame.CorrelationID != s.capabilityCorrelation {
			return protocolError(StatusInvalidState, "capability response has no matching request")
		}
		required := s.handshakeRequest.RequiredCapabilities | s.capabilityRequest.Required
		if s.selectedMinor<2&&(p.Offered|p.Negotiated|required)&^capabilityV11Mask!=0{return protocolError(StatusCapabilityDenied,"protocol 1.1 capability response contains v1.2 bits")}
		if !p.Negotiated.Has(required) || !s.capabilityRequest.Desired.Has(p.Negotiated) {
			return protocolError(StatusCapabilityDenied, "capability negotiation omitted requirements or exceeded desired set")
		}
		s.negotiated = p.Negotiated
		s.capabilitiesComplete = true
		return nil
	case *HealthResponse:
		if _, ok := s.pendingHealth[frame.CorrelationID]; !ok {
			return protocolError(StatusInvalidState, "health response has no matching request")
		}
		if int(p.ActiveRequests) != len(s.active) || p.ModelState != s.modelState {
			return protocolError(StatusInvalidState, "health response disagrees with tracked active/model state")
		}
		if s.modelState == ModelEmpty {
			if p.ModelID != nil {
				return protocolError(StatusInvalidState, "empty health state cannot identify a model")
			}
		} else if p.ModelID == nil || *p.ModelID != s.modelID {
			return protocolError(StatusInvalidState, "health response model id disagrees with tracked model")
		}
		if p.RuntimeGeneration < s.runtimeGeneration {
			return protocolError(StatusSequenceError, "runtime generation moved backwards")
		}
		s.runtimeGeneration = p.RuntimeGeneration
		delete(s.pendingHealth, frame.CorrelationID)
		return nil
	case *ModelStatePayload:
		return s.acceptModelState(frame, p)
	case *StreamToken:
		active, err := s.requireActive(frame)
		if err != nil {
			return err
		}
		if !s.negotiated.Has(CapabilityStreamToken) {
			return protocolError(StatusCapabilityDenied, "token stream capability was not negotiated")
		}
		if err:=acceptStreamSequence(active,p.StreamSequence,s.selectedMinor);err!=nil{return err}
		if p.Index != active.nextToken {
			return protocolError(StatusSequenceError, "token index %d is not next index %d", p.Index, active.nextToken)
		}
		active.nextToken++
		return nil
	case *StreamTool:
		active, err := s.requireActive(frame)
		if err != nil {
			return err
		}
		if !s.negotiated.Has(CapabilityStreamTool) {
			return protocolError(StatusCapabilityDenied, "tool stream capability was not negotiated")
		}
		if err:=acceptStreamSequence(active,p.StreamSequence,s.selectedMinor);err!=nil{return err};return acceptToolStream(active,p,s.negotiated)
	case *StreamProgress:
		active, err := s.requireActive(frame)
		if err != nil {
			return err
		}
		if !s.negotiated.Has(CapabilityStreamProgress) {
			return protocolError(StatusCapabilityDenied, "progress stream capability was not negotiated")
		}
		if err:=acceptStreamSequence(active,p.StreamSequence,s.selectedMinor);err!=nil{return err}
		if p.Fraction < active.progress {
			return protocolError(StatusSequenceError, "progress fraction moved backwards")
		}
		active.progress = p.Fraction
		return nil
	case *StreamUsage:
		active,err:=s.requireActive(frame);if err!=nil{return err};if !s.negotiated.Has(CapabilityStreamUsage){return protocolError(StatusCapabilityDenied,"stream usage capability was not negotiated")};sequence:=p.StreamSequence;if err:=acceptStreamSequence(active,&sequence,s.selectedMinor);err!=nil{return err};if p.InputTokens<active.lastUsageInput||p.OutputTokens<active.lastUsageOutput{return protocolError(StatusSequenceError,"usage counters moved backwards")};active.lastUsageInput=p.InputTokens;active.lastUsageOutput=p.OutputTokens;return nil
	case *StreamCache:
		active,err:=s.requireActive(frame);if err!=nil{return err};if !s.negotiated.Has(CapabilityStreamCache){return protocolError(StatusCapabilityDenied,"stream cache capability was not negotiated")};sequence:=p.StreamSequence;return acceptStreamSequence(active,&sequence,s.selectedMinor)
	case *CancelAck:
		if s.selectedMinor<2||!s.negotiated.Has(CapabilityCancelAck){return protocolError(StatusCapabilityDenied,"cancel acknowledgment was not negotiated")};pending,ok:=s.pendingCancels[frame.CorrelationID];if !ok||pending.target!=p.TargetStreamID||frame.StreamID!=p.TargetStreamID{return protocolError(StatusInvalidState,"cancel acknowledgment has no matching request")};if pending.disposition!=p.Disposition{return protocolError(StatusConflict,"cancel acknowledgment disposition disagrees with tracked target")};if p.Disposition==CancelAccepted{if p.AcceptedSequence==nil||pending.acceptedSequence==nil{return protocolError(StatusMissingField,"accepted cancellation requires sequence")};if *p.AcceptedSequence!=*pending.acceptedSequence{return protocolError(StatusSequenceError,"cancel accepted sequence does not match acceptance point")}};delete(s.pendingCancels,frame.CorrelationID);return nil
	case *StreamError:
		active, err := s.requireActive(frame); if err != nil {
			return err
		}
		if err:=acceptStreamSequence(active,p.StreamSequence,s.selectedMinor);err!=nil{return err}
		if p.Severity==nil||*p.Severity==ErrorTerminal{delete(s.active,frame.StreamID);s.markTerminal(frame.StreamID)}
		return nil
	case *StreamCompletion:
		active, err := s.requireActive(frame)
		if err != nil {
			return err
		}
		for _, tool := range active.tools {
			if tool.open {
				return protocolError(StatusInvalidState, "completion arrived with an open tool stream")
			}
		}
		if p.Reason == FinishToolCall && !active.sawTool {
			return protocolError(StatusInvalidState, "tool-call finish requires at least one tool event")
		}
		if err:=acceptStreamSequence(active,p.StreamSequence,s.selectedMinor);err!=nil{return err}
		if active.cancelled&&p.Reason!=FinishCancelled{return protocolError(StatusInvalidState,"cancelled request must finish as cancelled")}
		delete(s.active, frame.StreamID)
		s.markTerminal(frame.StreamID)
		return nil
	default:
		return protocolError(StatusInvalidState, "payload %T is not a runtime event", frame.Payload)
	}
}

func (s *ProtocolState) startInference(frame *Frame, vision bool, think *bool, session *string) error {
	if !s.capabilitiesComplete || s.modelState != ModelReady || s.pendingModel != nil {
		return protocolError(StatusInvalidState, "inference requires negotiated capabilities and ready model")
	}
	required := CapabilityText | CapabilityStreamToken
	if vision {
		required = CapabilityVision | CapabilityStreamToken
	}
	if !s.negotiated.Has(required) {
		return protocolError(StatusCapabilityDenied, "required inference capabilities were not negotiated")
	}
	if think != nil && *think && !s.negotiated.Has(CapabilityThinkMode) {
		return protocolError(StatusCapabilityDenied, "think mode capability was not negotiated")
	}
	if s.negotiated.Has(CapabilitySessionCache) {
		if session == nil {
			return protocolError(StatusMissingField, "negotiated session cache requires session id")
		}
	} else if session != nil {
		return protocolError(StatusCapabilityDenied, "session id is forbidden without session cache capability")
	}
	if len(s.active) >= MaxActiveRequests {
		return protocolError(StatusResourceLimit, "active request limit %d reached", MaxActiveRequests)
	}
	if _, exists := s.active[frame.StreamID]; exists {
		return protocolError(StatusConflict, "stream %d is already active", frame.StreamID)
	}
	for _, active := range s.active {
		if active.correlationID == frame.CorrelationID {
			return protocolError(StatusConflict, "correlation %d is already active", frame.CorrelationID)
		}
	}
	s.active[frame.StreamID] = &activeRequest{correlationID: frame.CorrelationID, progress: -1, tools: make(map[string]*toolStreamState),nextStreamSequence:1}
	return nil
}

func (s *ProtocolState) startChatInference(frame *Frame,p *ChatRequest)error{
	if s.selectedMinor<2{return protocolError(StatusUnsupportedProtocol,"structured chat requires protocol 1.2")}
	if !s.capabilitiesComplete||s.modelState!=ModelReady||s.pendingModel!=nil{return protocolError(StatusInvalidState,"chat inference requires negotiated capabilities and ready model")}
	if p.ModelID!=s.modelID||p.ExpectedModelGeneration!=s.modelGeneration{return protocolError(StatusConflict,"chat request model identity/generation does not match active model")}
	required:=CapabilityText|CapabilityStreamToken|CapabilityStructuredMessages
	if len(p.Tools)>0||chatHasToolHistory(p){required|=CapabilityToolDefinitions};if p.ToolChoice!=nil{required|=CapabilityToolChoice};if len(p.Stop)>0{required|=CapabilityStopSequences};if p.TopK!=nil||p.PresencePenalty!=nil||p.RepetitionPenalty!=nil{required|=CapabilityExtendedSampling};if p.ThinkingMode!=nil||p.ReasoningEffort!=nil||p.ReasoningBudgetTokens!=nil||p.PreserveThinking!=nil{required|=CapabilityReasoningControls};if len(p.Images)>0{required|=CapabilityVision|CapabilityMultipartVision};if p.IncludeUsage{required|=CapabilityStreamUsage};if p.ParallelToolCalls!=nil&&*p.ParallelToolCalls{required|=CapabilityParallelToolCalls};if len(p.ChatTemplateKwargs)>0{required|=CapabilityChatTemplateKwargs};if p.CacheSalt!=nil{required|=CapabilityCacheSalt}
	if !s.negotiated.Has(required){return protocolError(StatusCapabilityDenied,"structured chat capabilities were not fully negotiated")}
	if s.negotiated.Has(CapabilitySessionCache)&&p.SessionID==nil{return protocolError(StatusMissingField,"session cache requires session id")};if !s.negotiated.Has(CapabilitySessionCache)&&p.SessionID!=nil{return protocolError(StatusCapabilityDenied,"session id requires session cache capability")}
	if len(s.active)>=MaxActiveRequests{return protocolError(StatusResourceLimit,"active request limit %d reached",MaxActiveRequests)};if _,exists:=s.active[frame.StreamID];exists{return protocolError(StatusConflict,"stream %d is already active",frame.StreamID)}
	for _,active:=range s.active{if active.correlationID==frame.CorrelationID{return protocolError(StatusConflict,"correlation %d is already active",frame.CorrelationID)}}
	s.active[frame.StreamID]=&activeRequest{correlationID:frame.CorrelationID,progress:-1,tools:make(map[string]*toolStreamState),nextStreamSequence:1};return nil
}

func acceptStreamSequence(active *activeRequest,sequence *uint64,minor uint16)error{if minor<2{return nil};if sequence==nil{return protocolError(StatusMissingField,"protocol 1.2 stream event requires stream sequence")};if *sequence!=active.nextStreamSequence{return protocolError(StatusSequenceError,"stream sequence %d is not next %d",*sequence,active.nextStreamSequence)};active.nextStreamSequence++;return nil}
func(s *ProtocolState)markTerminal(stream uint64){s.terminal[stream]=struct{}{};if len(s.terminal)>MaxActiveRequests*2{for id:=range s.terminal{delete(s.terminal,id);break}}}
func chatHasToolHistory(p *ChatRequest)bool{for _,message:=range p.Messages{if message.Role==MessageRoleTool||len(message.ToolCalls)>0{return true}};return false}

func (s *ProtocolState) requireLifecycleIdle() error {
	if !s.capabilitiesComplete {
		return protocolError(StatusInvalidState, "model lifecycle requires capability negotiation")
	}
	if s.pendingModel != nil || len(s.active) != 0 {
		return protocolError(StatusConflict, "model lifecycle cannot overlap another lifecycle operation or active inference")
	}
	return nil
}

func (s *ProtocolState) requireBackend(backend Backend) error {
	if backend == BackendSGLang && !s.negotiated.Has(CapabilityBackendSGLang) {
		return protocolError(StatusCapabilityDenied, "SGLang backend capability was not negotiated")
	}
	if backend == BackendMTPLX && !s.negotiated.Has(CapabilityBackendMTPLX) {
		return protocolError(StatusCapabilityDenied, "MTPLX backend capability was not negotiated")
	}
	return nil
}

func (s *ProtocolState) acceptModelState(frame *Frame, payload *ModelStatePayload) error {
	pending := s.pendingModel
	if pending == nil || frame.CorrelationID != pending.correlationID {
		return protocolError(StatusInvalidState, "model state has no matching lifecycle request")
	}
	if pending.generation == 0 {
		if payload.Generation <= s.modelGeneration {
			return protocolError(StatusSequenceError, "new model generation must strictly increase")
		}
		pending.generation = payload.Generation
	} else if payload.Generation != pending.generation {
		return protocolError(StatusSequenceError, "model generation changed within one lifecycle operation")
	}
	if payload.State != ModelEmpty && (payload.ModelID == nil || *payload.ModelID != pending.modelID) {
		return protocolError(StatusInvalidState, "model state model id does not match lifecycle target")
	}
	if payload.Backend != nil && *payload.Backend != pending.backend {
		return protocolError(StatusInvalidState, "model state backend does not match lifecycle target")
	}
	terminal := false
	switch pending.operation {
	case PendingModelLoad:
		if payload.State != ModelLoading && payload.State != ModelReady && payload.State != ModelFailed {
			return protocolError(StatusInvalidState, "load emitted invalid model state %d", payload.State)
		}
		terminal = payload.State == ModelReady || payload.State == ModelFailed
	case PendingModelSwitch:
		if payload.State != ModelSwitching && payload.State != ModelReady && payload.State != ModelFailed {
			return protocolError(StatusInvalidState, "switch emitted invalid model state %d", payload.State)
		}
		terminal = payload.State == ModelReady || payload.State == ModelFailed
	case PendingModelUnload:
		if payload.State != ModelUnloading && payload.State != ModelEmpty && payload.State != ModelFailed {
			return protocolError(StatusInvalidState, "unload emitted invalid model state %d", payload.State)
		}
		terminal = payload.State == ModelEmpty || payload.State == ModelFailed
	default:
		return protocolError(StatusInternalError, "unknown pending model operation")
	}
	s.modelGeneration = payload.Generation
	s.modelState = payload.State
	if payload.State == ModelEmpty {
		s.modelID = ""
		s.backend = 0
	} else {
		s.modelID = pending.modelID
		s.backend = pending.backend
	}
	if terminal {
		s.pendingModel = nil
	}
	return nil
}

func (s *ProtocolState) requireActive(frame *Frame) (*activeRequest, error) {
	active, ok := s.active[frame.StreamID]
	if !ok {
		if _,terminal:=s.terminal[frame.StreamID];terminal{return nil,protocolError(StatusConflict,"stream %d already emitted a terminal event",frame.StreamID)}
		return nil, protocolError(StatusNotFound, "stream %d is not active", frame.StreamID)
	}
	if frame.CorrelationID != active.correlationID {
		return nil, protocolError(StatusConflict, "stream correlation does not match active request")
	}
	return active, nil
}

func acceptToolStream(active *activeRequest,payload *StreamTool,capabilities CapabilityMask) error {
	usesDraft:=payload.ChoiceIndex!=nil||payload.ToolCallIndex!=nil||payload.ToolCallType!=nil||payload.ArgumentsDelta!=nil||payload.DraftTotalBytes!=nil||payload.DraftSource!=nil
	if usesDraft&&!capabilities.Has(CapabilityToolDraftMetadata){return protocolError(StatusCapabilityDenied,"tool draft metadata capability was not negotiated")}
	state, exists := active.tools[payload.CallID]
	switch payload.EventKind {
	case ToolBegin:
		if exists || payload.Sequence != 0 {
			return protocolError(StatusSequenceError, "tool begin must create a new call at sequence zero")
		}
		open:=0;for _,other:=range active.tools{if other.open{open++};if payload.ChoiceIndex!=nil&&payload.ToolCallIndex!=nil&&optionalU32Equal(payload.ChoiceIndex,other.choiceIndex)&&optionalU32Equal(payload.ToolCallIndex,other.toolCallIndex){return protocolError(StatusConflict,"duplicate tool call index")}}
		if open>0&&!capabilities.Has(CapabilityParallelToolCalls){return protocolError(StatusCapabilityDenied,"parallel tool calls were not negotiated")}
		state=&toolStreamState{nextSequence:1,open:true,choiceIndex:cloneU32(payload.ChoiceIndex),toolCallIndex:cloneU32(payload.ToolCallIndex),toolCallType:cloneString(payload.ToolCallType),draftTotalBytes:cloneU64(payload.DraftTotalBytes)}
		if payload.ArgumentsDelta!=nil{state.draftBytes=uint64(len(*payload.ArgumentsDelta))}
		if state.draftTotalBytes!=nil&&state.draftBytes>*state.draftTotalBytes{return protocolError(StatusBadLength,"tool draft exceeded declared size")}
		active.tools[payload.CallID] = state
		active.sawTool = true
		return nil
	case ToolArgumentDelta, ToolEnd:
		if !exists || !state.open || payload.Sequence != state.nextSequence {
			return protocolError(StatusSequenceError, "tool event is missing begin or has non-contiguous sequence")
		}
		if !optionalU32Compatible(payload.ChoiceIndex,state.choiceIndex)||!optionalU32Compatible(payload.ToolCallIndex,state.toolCallIndex)||!optionalStringCompatible(payload.ToolCallType,state.toolCallType)||!optionalU64Compatible(payload.DraftTotalBytes,state.draftTotalBytes){return protocolError(StatusConflict,"tool draft metadata changed within a call")}
		if payload.ArgumentsDelta!=nil{state.draftBytes+=uint64(len(*payload.ArgumentsDelta));if state.draftTotalBytes!=nil&&state.draftBytes>*state.draftTotalBytes{return protocolError(StatusBadLength,"tool draft exceeded declared size")}}
		state.nextSequence++
		if payload.EventKind == ToolEnd {
			if state.draftTotalBytes!=nil&&state.draftBytes!=*state.draftTotalBytes{return protocolError(StatusBadLength,"tool draft ended before declared size")}
			state.open = false
		}
		return nil
	default:
		return protocolError(StatusBadEnum, "invalid tool event kind")
	}
}
func cloneU32(value *uint32)*uint32{if value==nil{return nil};v:=*value;return &v}
func cloneU64(value *uint64)*uint64{if value==nil{return nil};v:=*value;return &v}
func cloneString(value *string)*string{if value==nil{return nil};v:=*value;return &v}
func optionalU32Equal(a,b *uint32)bool{return a!=nil&&b!=nil&&*a==*b}
func optionalU32Compatible(next,current *uint32)bool{return next==nil||current!=nil&&*next==*current}
func optionalU64Compatible(next,current *uint64)bool{return next==nil||current!=nil&&*next==*current}
func optionalStringCompatible(next,current *string)bool{return next==nil||current!=nil&&*next==*current}

func (s *ProtocolState) poison(err error) error {
	s.poisoned = true
	return err
}
