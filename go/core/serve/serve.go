// Package serve implements the daemon HTTP/SSE contract (M3) — the same
// wire protocol the TS CLI's serve mode speaks, so the existing Electron
// desktop frontend connects without changing a line. Endpoints:
//
//	GET  /health, GET /capabilities
//	POST /session
//	POST /session/{id}/prompt        (FIFO per session)
//	GET  /session/{id}/events        (SSE, Last-Event-ID replay, heartbeat)
//	POST /session/{id}/permission/{requestId}, POST /permission/{requestId}
//	POST /session/{id}/cancel, POST /session/{id}/heartbeat
//
// Event envelope: {"v":1,"type":...,"data":...,"id":N} with ACP-shaped
// session_update payloads ({sessionId, update:{sessionUpdate,...}}).
package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/deskevo"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	harnesscore "github.com/hey-meadow/meadow-code/go/core/harness"
	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/rag"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/sessionrecovery"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	eventRingSize     = 8000
	subscriberQueue   = 256
	heartbeatInterval = 15 * time.Second
	// Match the protected agent boundary. Normal compressed image and PDF
	// prompts fit, while an unbounded local request cannot force
	// the public serve path or Desktop bridge to retain arbitrary memory.
	maxPromptRequestBytes       int64 = 256 << 20
	maxDirectPromptImageBytes         = 1 << 20
	maxPromptImageBatchBytes          = 20 << 20
	maxPromptImageCount               = 16
	maxPromptImageDimension           = 32768
	maxPromptImagePixels        int64 = 25_000_000
	maxPromptImageBytesPerPixel int64 = 8
	maxPromptImagesDecodedBytes int64 = 256 << 20
)

// EngineFactory builds one engine per serve session, rooted at cwd.
type EngineFactory func(cwd string) (*engine.Engine, error)

// RuntimeConfig is the session-owned configuration passed to an optional
// Hermes-Go runtime factory. The legacy Engine remains the compatibility
// envelope for model selection, prefix warming, approval mode, and 12B/26B.
// Compatibility aliases keep the daemon API stable while Harness Core owns
// the transport-neutral runtime contract.
type RuntimeConfig = harnesscore.Binding
type RuntimeHooks = harnesscore.Events
type RuntimeFactory = harnesscore.RuntimeFactory

// Server hosts the daemon contract over one workspace.
type Server struct {
	WorkspaceCwd       string
	NewEngine          EngineFactory
	NewRuntime         RuntimeFactory
	RequireToolAccount func(context.Context) error
	// ConnectionID is the selected identity for new sessions. After startup,
	// only ApplyConfigurationLease may change it; existing session bindings stay
	// immutable even when another connection uses the same model name.
	ConnectionID string
	// DefaultHarnessMode is a frozen deployment override. Chat choices are
	// resolved against it before admission and never change an active runtime.
	DefaultHarnessMode string
	// ConversationInboxEnabled opts one product into model-led continuous
	// conversation admission. Other products retain ordinary queue semantics.
	ConversationInboxEnabled bool
	// ResolveHarnessMode is a passive product capability resolver, shared by
	// admission/status and the runtime factory so reported and actual modes agree.
	ResolveHarnessMode func(modelID, override string) string
	// IndexAttachmentDocuments receives only protected, server-extracted text.
	// Ranking failures must retain ordinary original-file/preview access.
	IndexAttachmentDocuments func(context.Context, string, string, []rag.AttachmentInput) ([]rag.AttachmentReceipt, error)
	OnRouteShadow            func(sessionID, modelID, cwd string, res engine.RouteShadowResult)
	// InferenceOutputTokenLimit reads the existing runtime-control capability.
	// It must be bounded, passive and return zero for unknown/mismatched models.
	InferenceOutputTokenLimit func(modelID string) int
	// ResumeModelAllowed is a passive catalog check. Nil preserves the current
	// engine binding for legacy hosts without an explicit admission contract.
	ResumeModelAllowed func(modelID string) bool
	// BackgroundWakeEnabled is an explicit host capability. A completion is
	// delivered through an ordinary model turn, never rewritten as an answer.
	BackgroundWakeEnabled   bool
	BackgroundWakeAdmission func(context.Context, string) error

	mu       sync.Mutex
	sessions map[string]*serveSession
	// Private construction slots outlive cancelled HTTP callers until the
	// legacy factory actually settles, without retaining the registry mutex.
	sessionCreations       map[sessionCreationKey]*sessionCreation
	activeSessionCreations int
	// configurationMu serializes configuration leases with work admission. It
	// precedes mu and serveSession.mu in the lock order.
	configurationMu         sync.Mutex
	configurationLeaseID    string
	configurationLeaseUntil time.Time
	configurationAdmissions int
	wakeContext             context.Context
	wakeCancel              context.CancelFunc
	wakeClosed              bool
	wakeWorkers             sync.WaitGroup
	wakeStoreMu             sync.Mutex
	wakeRestoreMu           sync.Mutex
	wakeRestorerStarted     bool
	workerSlots             chan struct{}
}

func NewServer(workspaceCwd string, factory EngineFactory) *Server {
	// Restore the persisted per-cwd usage tally so lifetime token/time
	// totals survive a daemon restart (best-effort; usage.go, idempotent).
	usage.load()
	return &Server{
		WorkspaceCwd: workspaceCwd,
		NewEngine:    factory,
		sessions:     map[string]*serveSession{},
	}
}

// event is one ring-buffer entry.
type event struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
	Data any    `json:"data"`
}

// The approval channel describes the caller's ability to answer a permission
// request, independently of which actions the approval mode permits. It is
// immutable for a live session; omitted values retain the Desktop contract.
type sessionApprovalChannel string

const (
	approvalChannelInteractive sessionApprovalChannel = "interactive"
	approvalChannelNone        sessionApprovalChannel = "none"
)

var errApprovalChannelUnavailable = errors.New("approval channel unavailable")

const approvalChannelBlockedContent = `{"status":"blocked","code":"approval_channel_unavailable","executed":false,"message":"This action requires approval, but this session has no approval channel. The entire tool call was not executed; this is not a user decline. Repeating the unchanged action cannot gain approval. Choose a permitted alternative or report the approval blocker.","validation_hint":"For small checks, reuse existing tests or installed runtimes with standard-library assertions or import smoke checks. Avoid unnecessary dependency installation and cleanup/deletion in the validation command; preserve fixtures if cleanup needs approval. Do not disguise a restricted action or bypass its safeguards."}`

func parseApprovalChannel(value string) (sessionApprovalChannel, error) {
	switch sessionApprovalChannel(value) {
	case "", approvalChannelInteractive:
		return approvalChannelInteractive, nil
	case approvalChannelNone:
		return approvalChannelNone, nil
	default:
		return "", errors.New("invalid_approval_channel")
	}
}

type serveSession struct {
	id                       string
	connectionID             string
	cwd                      string
	engine                   *engine.Engine
	disabledTools            []string
	indexAttachmentDocuments func(context.Context, string, string, []rag.AttachmentInput) ([]rag.AttachmentReceipt, error)
	// activeRuntime is non-nil only while a Hermes-Go turn is running. It is
	// the authoritative target for mid-run guidance.
	activeRuntime            agentcore.Runtime
	conversationInboxMu      sync.Mutex
	conversationReceipts     map[string]conversationInboxReceipt
	conversationInboxEnabled bool
	resolveHarnessMode       func(modelID, override string) string
	harnessMode              string
	defaultHarnessMode       string
	harnessPreferenceStatus  string
	harnessRecoveryStatus    string
	activeHarnessMode        *string

	mu          sync.Mutex
	nextEventID int64
	ring        []event
	subscribers map[chan event]struct{}
	history     []provider.Message
	// Foreground plan presentation is separate from provider history. Only
	// durably committed call/results populate it; reading it invokes no model.
	taskPlan          taskPlanSnapshot
	taskPlanPending   map[string]taskPlanReceipt
	taskPlanRejected  map[string]bool
	taskPlanReceipts  []taskPlanReceipt
	taskPlanOutputSHA string
	taskPlanPersistMu sync.Mutex
	// turnMarks records len(history) at the start of each prompt. Steers
	// and nudges insert extra user messages mid-turn, so counting user
	// roles can't recover turn boundaries — these marks can.
	turnMarks               []int
	promptMu                sync.Mutex // serializes prompts (FIFO)
	cancelRun               context.CancelFunc
	warmCancel              context.CancelFunc // in-flight prefix warm; prompts cancel it
	permissions             map[string]chan engine.Decision
	restoredPermissions     map[string]sessionrecovery.PermissionCheckpoint
	restoredPermissionCalls map[string]sessionrecovery.ToolCallCheckpoint
	restoredApprovalActions map[string]int
	permissionSeq           uint64
	approvalChannel         sessionApprovalChannel
	// Serialize explicit mode changes and wake-policy snapshots. Never acquire
	// this while holding mu: policy persistence takes its own session snapshot.
	approvalModeMu            sync.Mutex
	approveAllAlways          bool
	approveAlways             map[string]bool
	userInputs                map[string]pendingUserInput
	locationRequests          map[string]*pendingLocationRequest
	locationRunID             string
	locationAskedRunID        string
	locationRunContext        context.Context
	locationGrants            map[string]locationGrant
	lastSeen                  time.Time
	lastPromptTokens          int
	lastPromptTokensEstimated bool
	// Inference controls are conversation-scoped preferences. Hermes runtimes
	// are rebuilt for each prompt, so these values are applied to the fresh
	// runtime immediately before RunInput instead of changing model residency.
	thinkingMode              provider.ThinkingMode
	reasoningEffort           provider.ReasoningEffort
	inferencePreferenceStatus string
	activeInferenceControls   *sessionInferenceSelection
	// researchState is the compact accepted ledger from the previous turn. It
	// is offered to Kernel on the next turn, which restores it only when the new
	// user message is detected as a correction/retry of that research task.
	researchState *agentcore.ResearchStateSnapshot
	// running and runReplayAfterEventID make a durable chat re-attachable
	// while its prompt continues in the background. The replay cursor points
	// just after the persisted primary user message, so Desktop can repaint
	// JSONL history and replay only the in-flight agent UI without duplicates.
	running               bool
	runReplayAfterEventID int64
	// queueKey follows the durable chat id (not necessarily the live session
	// id after resume). queuedPrompts and queuePump are protected by mu.
	queueKey         string
	queuedPrompts    []*queuedPrompt
	queuePump        bool
	queuePaused      bool
	cancelGeneration uint64
	backgroundTasks  []backgroundTaskRecord
	researchSources  []researchSourceRecord
	placeResults     []placeRecord
	// chat persists turns to ~/.meadow (TS JSONL format). Lazily opened on
	// the first prompt (chat file id = session id) so empty sessions never
	// land in the sidebar; resume arrives with the original chat's writer.
	// Only touched under promptMu (or before the session is registered),
	// so no extra locking. Best-effort: persistence errors never fail a
	// prompt. resume.go holds the append logic.
	chat                  *session.ChatWriter
	recoveryPlan          *sessionrecovery.ResumePlan
	stableCheckpointID    string
	stableCheckpointErr   error
	activeRunID           string
	activeRunCheckpointID string
	// interruptedTaskPending follows the shared conversation history, including
	// queue-origin turns. The next turn consumes it once so the model can decide
	// whether the new request continues or replaces the cancelled task.
	interruptedTaskPending bool
	// promptRequests binds an optional Desktop transport idempotency key to
	// exactly one prompt payload and result. It is intentionally in-memory and
	// session-scoped: its job is to survive HTTP disconnect/retry while the
	// Agent process and conversation remain alive, without changing legacy
	// clients that omit the header.
	promptRequests          map[string]*promptRequestRecord
	promptAdmissions        map[*promptAdmission]struct{}
	cancelledPromptRequests map[string]time.Time
	activePromptRequestID   string
	appliedGuidance         map[string]bool
	queuePersistenceErr     error
	wakeStarted             bool
	wakeCancelledBefore     *time.Time
	wakeSignal              chan struct{}
	wakeSourceRetry         bool
	workerBroker            *tools.BackgroundTaskBroker
}

// pendingUserInput is one in-flight ask_user_question round: the asker
// goroutine blocks on ch while the frontend answers over HTTP.
type pendingUserInput struct {
	ch  chan []string
	num int // number of questions, for answer-record ordering
}

func (s *serveSession) failStableCheckpoint(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.stableCheckpointErr == nil {
		s.stableCheckpointErr = err
	}
	cancel := s.cancelRun
	s.mu.Unlock()
	s.broadcast("recovery_status", agentcore.RecoveryStatus{
		SessionID: s.id, RecoveryWarning: err.Error(), Degraded: true, MutationFailClosed: true,
	})
	if cancel != nil {
		cancel()
	}
}

const supersededUserInputAnswer = "Superseded by newer expert guidance; follow the latest guidance instead."

func (s *serveSession) supersedePendingUserInputs() {
	s.mu.Lock()
	pending := make(map[string]pendingUserInput, len(s.userInputs))
	for requestID, input := range s.userInputs {
		pending[requestID] = input
	}
	s.mu.Unlock()
	for requestID, input := range pending {
		answers := make([]string, input.num)
		for index := range answers {
			answers[index] = supersededUserInputAnswer
		}
		select {
		case input.ch <- answers:
			s.broadcast("user_input_resolved", map[string]any{
				"sessionId": s.id, "requestId": requestID,
				"superseded": true,
			})
		default:
			// A concurrently submitted explicit answer already won.  The newer
			// guidance is still authoritative at the next model turn.
		}
	}
}

func (s *serveSession) broadcast(evType string, data any) {
	s.mu.Lock()
	s.broadcastLocked(evType, data)
	if evType == "run_state" {
		s.taskPlanRunEventLocked(data)
	}
	s.mu.Unlock()
	if evType == "run_state" {
		// Terminal lifecycle presentation is durable, but never changes the
		// model-owned task status or fabricates successful evidence.
		if err := s.persistTaskPlanSnapshot(); err != nil {
			s.broadcast("stream_error", map[string]any{"message": taskPlanPersistenceError(err).Error()})
		}
	}
}

// broadcastLocked makes request admission/resolution and their receipt atomic.
// Callers must hold mu; subscribers must never see a request after its resolution.
func (s *serveSession) broadcastLocked(evType string, data any) {
	s.nextEventID++
	ev := event{ID: s.nextEventID, Type: evType, Data: data}
	s.ring = append(s.ring, ev)
	if len(s.ring) > eventRingSize {
		s.ring = s.ring[len(s.ring)-eventRingSize:]
	}
	for ch := range s.subscribers {
		select {
		case ch <- ev:
		default:
			if !criticalSessionEvent(evType, data) {
				continue
			}
			enqueueCriticalSessionEvent(ch, ev)
		}
	}
}

func enqueueCriticalSessionEvent(ch chan event, ev event) {
	// Preserve already-buffered usage/terminal facts by evicting stale progress
	// first. A slow UI must not see turn_completed while silently missing the
	// final context/billing snapshot immediately before it.
	buffered := make([]event, 0, len(ch))
	for {
		select {
		case queued := <-ch:
			buffered = append(buffered, queued)
		default:
			goto drained
		}
	}

drained:
	drop := -1
	for index, queued := range buffered {
		if !criticalSessionEvent(queued.Type, queued.Data) {
			drop = index
			break
		}
	}
	if drop < 0 && len(buffered) > 0 {
		drop = 0
	}
	for index, queued := range buffered {
		if index != drop {
			ch <- queued
		}
	}
	ch <- ev
}

func criticalSessionEvent(evType string, data any) bool {
	switch evType {
	case "turn_completed", "stream_error", "run_state", "task_plan_update", "queue_changed", "usage",
		"permission_request", "user_input_request", "model_switched", "harness_controls_changed", "evo_state",
		"location_request", "location_resolved", "place_results":
		return true
	case "session_update":
		payload, ok := data.(map[string]any)
		if !ok {
			return false
		}
		update, ok := payload["update"].(map[string]any)
		if !ok {
			return false
		}
		kind, _ := update["sessionUpdate"].(string)
		return kind == "tool_call_update"
	default:
		return false
	}
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/configuration/lease", s.handleConfigurationLeaseAcquire)
	mux.HandleFunc("DELETE /agent/configuration/lease", s.handleConfigurationLeaseRelease)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /capabilities", s.handleCapabilities)
	mux.HandleFunc("POST /session", s.handleCreateSession)
	// M6: open a live session preloaded from a stored chat (resume.go).
	mux.HandleFunc("POST /session/resume", s.handleResume)
	mux.HandleFunc("POST /session/{id}/prompt", s.handlePrompt)
	mux.HandleFunc("POST /session/{id}/attachment-collections", s.handleAttachmentCollectionCreate)
	mux.HandleFunc("PUT /session/{id}/attachment-collections/{collection}/images/{image}", s.handleAttachmentImageUpload)
	mux.HandleFunc("GET /session/{id}/attachment-collections/{collection}", s.handleAttachmentCollectionList)
	mux.HandleFunc("GET /session/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /session/{id}/permission/{requestId}", s.handlePermission)
	mux.HandleFunc("POST /permission/{requestId}", s.handlePermissionGlobal)
	mux.HandleFunc("POST /session/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /session/{id}/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("POST /session/{id}/steer", s.handleSteer)
	mux.HandleFunc("GET /session/{id}/context", s.handleContextStatus)
	mux.HandleFunc("POST /session/{id}/compact", s.handleCompactContext)
	mux.HandleFunc("GET /session/{id}/queue", s.handleQueueList)
	mux.HandleFunc("POST /session/{id}/queue", s.handleQueueEnqueue)
	mux.HandleFunc("POST /session/{id}/queue/{queueId}/move", s.handleQueueMove)
	mux.HandleFunc("POST /session/{id}/queue/{queueId}/guide", s.handleQueueGuide)
	mux.HandleFunc("POST /session/{id}/queue/{queueId}/edit", s.handleQueueEdit)
	mux.HandleFunc("DELETE /session/{id}/queue/{queueId}", s.handleQueueCancel)
	mux.HandleFunc("GET /session/{id}/tasks", s.handleBackgroundTasks)
	mux.HandleFunc("GET /session/{id}/task-plan", s.handleTaskPlan)
	mux.HandleFunc("GET /session/{id}/sources", s.handleResearchSources)
	mux.HandleFunc("GET /session/{id}/places", s.handlePlaces)
	mux.HandleFunc("GET /session/{id}/location", s.handleLocationRequests)
	mux.HandleFunc("POST /session/{id}/location/{requestId}", s.handleLocationResponse)
	s.registerMapRoutes(mux)
	mux.HandleFunc("POST /session/{id}/source-preview", s.handleSourcePreview)
	mux.HandleFunc("GET /tasks", s.handleAllBackgroundTasks)
	mux.HandleFunc("POST /session/{id}/approval", s.handleApproval)
	mux.HandleFunc("POST /session/{id}/model", s.handleModelSwitch)
	mux.HandleFunc("GET /session/{id}/inference-controls", s.handleInferenceControls)
	mux.HandleFunc("GET /session/{id}/harness-controls", s.handleHarnessControls)
	mux.HandleFunc("GET /session/{id}/evo", s.handleEvoStatus)
	mux.HandleFunc("POST /session/{id}/harness-controls", s.handleHarnessControls)
	mux.HandleFunc("POST /session/{id}/inference-controls", s.handleInferenceControls)
	mux.HandleFunc("POST /session/{id}/rollback", s.handleRollback)
	mux.HandleFunc("POST /session/{id}/user-input/{requestId}", s.handleUserInput)
	mux.HandleFunc("GET /sessions", s.handleListSessions)
	// M8: cross-session token/time accounting per workspace cwd (usage.go).
	mux.HandleFunc("GET /usage", s.handleUsage)
	// M5 desktop extensions: PTY terminals (pty.go), stored-chat history +
	// git write actions (extras.go), workspace browse/search (workspace.go).
	mux.HandleFunc("POST /session/{id}/term", s.handleTermCreate)
	mux.HandleFunc("GET /session/{id}/term/{termId}/stream", s.handleTermStream)
	mux.HandleFunc("POST /session/{id}/term/{termId}/write", s.handleTermWrite)
	mux.HandleFunc("POST /session/{id}/term/{termId}/resize", s.handleTermResize)
	mux.HandleFunc("DELETE /session/{id}/term/{termId}", s.handleTermKill)
	s.registerExtraRoutes(mux)
	s.registerWorkspaceRoutes(mux)
	return s.configurationAdmissionMiddleware(mux)
}

// warmPrefixCache prefills the session's system prompt with a 1-token
// completion so vllm-mlx's prefix cache is hot before the first real
// prompt (which then only pays for its own delta). Skipped when the
// engine has no system prompt; failures are silent by design.
//
// The warm MUST yield to a real prompt: prefill is serial on vllm-mlx
// (prefill-batch-size 1), so a user message arriving mid-warm would
// otherwise queue behind it and pay both cold prefills back to back.
// handlePrompt cancels via warmCancel before taking the turn.
func (s *serveSession) warmPrefixCache() {
	if s.engine == nil || s.engine.Client == nil || s.engine.SystemPrompt == "" {
		return
	}
	if !prefixWarmCanYieldWithoutWorkerRecycle(s.engine.ModelID) {
		// Gemma 26B and Ornith 35B must recycle their pinned MLX worker after
		// an interrupted decode. A user prompt commonly arrives while this
		// speculative warm is still running, so cancelling the warm destroys
		// the resident model and makes the real first turn pay a full reload.
		// Keep the optimization only for runtimes whose abort is non-destructive.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	s.mu.Lock()
	s.warmCancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.warmCancel = nil
		s.mu.Unlock()
	}()
	one := 1
	_, _ = s.engine.Client.ChatStream(ctx, provider.ChatRequest{
		Model: s.engine.ModelID,
		Messages: []provider.Message{
			{Role: "system", Content: s.engine.SystemPrompt},
			{Role: "user", Content: "ok"},
		},
		MaxTokens: &one,
		Stream:    true,
	}, nil)
}

func warmPrefixOnSessionOpen() bool {
	return strings.TrimSpace(os.Getenv("MEADOW_WARM_PREFIX_ON_SESSION_OPEN")) == "1"
}

func (s *Server) warmPrefixCacheOnOpen(sess *serveSession) {
	if !warmPrefixOnSessionOpen() || !s.beginConfigurationAdmission() {
		return
	}
	// Retain admission before spawning: warmCancel is installed inside the
	// goroutine, so inspecting it alone leaves a session-open race.
	go func() {
		defer s.endConfigurationAdmission()
		sess.warmPrefixCache()
	}()
}

func prefixWarmCanYieldWithoutWorkerRecycle(modelID string) bool {
	return !provider.IsGemma12Model(modelID) && !provider.IsOrnithModel(modelID)
}

// askViaEvents is the serve-mode Asker for ask_user_question: it
// broadcasts the questions as a user_input_request SSE frame and blocks
// until the frontend posts answers (or the turn is cancelled).
func (s *serveSession) askViaEvents(ctx context.Context, questions []tools.Question) ([]string, error) {
	if s.effectiveApprovalChannel() == approvalChannelNone {
		return nil, errApprovalChannelUnavailable
	}
	requestID := session.NewUUID()
	pending := pendingUserInput{ch: make(chan []string, 1), num: len(questions)}
	s.mu.Lock()
	s.userInputs[requestID] = pending
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.userInputs, requestID)
		s.mu.Unlock()
	}()

	qs := make([]map[string]any, len(questions))
	for i, q := range questions {
		opts := make([]map[string]any, len(q.Options))
		for j, o := range q.Options {
			opts[j] = map[string]any{"label": o.Label, "description": o.Description}
		}
		qs[i] = map[string]any{
			"id":           fmt.Sprintf("q%d", i),
			"header":       q.Header,
			"question":     q.Question,
			"options":      opts,
			"multiSelect":  q.MultiSelect,
			"allowOther":   q.AllowOther,
			"presentation": q.Presentation,
		}
	}
	s.broadcast("user_input_request", map[string]any{
		"sessionId": s.id,
		"requestId": requestID,
		"questions": qs,
	})

	select {
	case answers := <-pending.ch:
		return answers, nil
	case <-ctx.Done():
		s.broadcast("user_input_resolved", map[string]any{
			"sessionId": s.id, "requestId": requestID, "cancelled": true,
		})
		return nil, ctx.Err()
	}
}

func (s *serveSession) confirmTool(ctx context.Context, req engine.ConfirmRequest) (engine.Decision, error) {
	if s.effectiveApprovalChannel() == approvalChannelNone {
		return engine.DecisionDeny, errApprovalChannelUnavailable
	}
	if s.consumeRestoredApproval(req) {
		return engine.DecisionApprove, nil
	}
	requestID := s.stablePermissionRequestID(req)
	ch := make(chan engine.Decision, 1)
	s.mu.Lock()
	s.permissions[requestID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.permissions, requestID)
		s.mu.Unlock()
	}()

	options := []map[string]any{
		{"optionId": "approve", "name": "允許", "kind": "allow_once"},
		{"optionId": "reject", "name": "拒絕", "kind": "reject_once"},
	}
	if req.Kind != tools.KindDestructive {
		options = append(options,
			map[string]any{"optionId": "approve_always", "name": "一律允許", "kind": "allow_always"},
		)
	}
	s.broadcast("permission_request", map[string]any{
		"requestId": requestID,
		"sessionId": s.id,
		"toolCall": map[string]any{
			"title": req.ToolName, "kind": string(req.Kind), "rawInput": req.Args,
		},
		"options": options,
	})
	select {
	case decision := <-ch:
		return decision, nil
	case <-ctx.Done():
		// Cancellation can race with a browser vote. Remove this request and
		// emit the same terminal event as an explicit cancel so the Desktop
		// cannot leave a stale permission card behind.
		s.cancelPermission(requestID, ch)
		return engine.DecisionDeny, ctx.Err()
	}
}

func (s *serveSession) effectiveApprovalChannel() sessionApprovalChannel {
	if s.approvalChannel == "" {
		return approvalChannelInteractive
	}
	return s.approvalChannel
}

func permissionActionDigest(toolName string, args map[string]any) string {
	raw, _ := json.Marshal(args)
	return sessionrecovery.DigestAction(toolName, raw)
}

func (s *serveSession) consumeRestoredApproval(req engine.ConfirmRequest) bool {
	digest := permissionActionDigest(req.ToolName, req.Args)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restoredApprovalActions[digest] <= 0 {
		return false
	}
	s.restoredApprovalActions[digest]--
	return true
}

func (s *serveSession) stablePermissionRequestID(req engine.ConfirmRequest) string {
	raw, _ := json.Marshal(req.Args)
	s.mu.Lock()
	s.permissionSeq++
	sequence := s.permissionSeq
	checkpointID := s.activeRunCheckpointID
	s.mu.Unlock()
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", s.queueKey, checkpointID, req.ToolName, sequence, raw)))
	return fmt.Sprintf("permission-%x", digest[:12])
}

// cancelPermission atomically closes one pending permission request. It is
// intentionally idempotent: either the cancel endpoint or confirmTool's
// context watcher may win the race, but only the winner emits a terminal
// permission_resolved event and wakes the waiting engine call.
func (s *serveSession) cancelPermission(requestID string, expected chan engine.Decision) bool {
	s.mu.Lock()
	ch, ok := s.permissions[requestID]
	if ok && expected != nil && ch != expected {
		ok = false
	}
	if ok {
		delete(s.permissions, requestID)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- engine.DecisionDeny:
	default:
	}
	s.broadcast("permission_resolved", map[string]any{
		"sessionId": s.id,
		"requestId": requestID,
		"cancelled": true,
		"outcome":   map[string]any{"outcome": "cancelled", "optionId": "reject"},
	})
	return true
}

// cancelPendingPermissions closes every permission card belonging to a run.
// A single stop button may race with several parallel web_search requests;
// resolve them all in one atomic pass so no card remains actionable.
func (s *serveSession) cancelPendingPermissions() int {
	s.mu.Lock()
	pending := make(map[string]chan engine.Decision, len(s.permissions))
	for requestID, ch := range s.permissions {
		pending[requestID] = ch
		delete(s.permissions, requestID)
	}
	s.mu.Unlock()
	for requestID, ch := range pending {
		select {
		case ch <- engine.DecisionDeny:
		default:
		}
		s.broadcast("permission_resolved", map[string]any{
			"sessionId": s.id,
			"requestId": requestID,
			"cancelled": true,
			"outcome":   map[string]any{"outcome": "cancelled", "optionId": "reject"},
		})
	}
	return len(pending)
}

// authorizeTool preserves the Desktop approval contract for KernelRuntime.
// Read/interaction tools pass, plan blocks mutation, auto-edit allows edits,
// yolo allows non-destructive work, and full-access grants all tool kinds.
func (s *serveSession) authorizeTool(ctx context.Context, name string, args map[string]any, kind tools.Kind) agentcore.Authorization {
	s.mu.Lock()
	mode := s.engine.Mode
	if owner, ok := tools.BackgroundOwner(ctx); ok && tools.BackgroundTaskID(ctx) != "" && tools.BackgroundWorkerID(ctx) != "" {
		if owner.ChatID != queueChatID(s) || owner.WorkspaceCWD != s.cwd || owner.ConnectionID != s.connectionID {
			s.mu.Unlock()
			return agentcore.Authorization{Status: agentcore.ToolBlocked, LLMContent: "Background worker owner changed; action was not executed."}
		}
		mode = intersectWorkerApprovalMode(engine.ApprovalMode(owner.ApprovalMode), mode)
	}
	// "Always" is scoped to the exact tool. A decision for web_fetch cannot
	// grant run_shell_command or write_file authority later in the same turn.
	// approveAllAlways is retained only so old persisted/in-memory session
	// shapes remain readable; it no longer participates in authorization.
	alreadyApproved := s.approveAlways[name]
	s.mu.Unlock()

	// The explicit grant is applied only after worker owner validation and the
	// intersection with its admission policy, so a later broader parent cannot
	// upgrade an already restricted worker. Tool/account validation is separate.
	if mode == engine.ModeFullAccess {
		return agentcore.Authorization{Allowed: true, Status: agentcore.ToolOK}
	}
	if kind == tools.KindReadOnly || kind == tools.KindInteract {
		return agentcore.Authorization{Allowed: true, Status: agentcore.ToolOK}
	}
	if mode == engine.ModePlan {
		return agentcore.Authorization{
			Status:     agentcore.ToolBlocked,
			LLMContent: "ERROR: plan mode blocked this non-read-only tool call. Present the plan to the user instead.",
		}
	}
	// Yolo delegates ordinary edits and execution, but destructive operations
	// remain one-shot confirmations. A broad mode or prior approval must never
	// authorize deletion/reset/wipe without confirmation in these modes.
	if mode == engine.ModeYolo && kind != tools.KindDestructive {
		return agentcore.Authorization{Allowed: true, Status: agentcore.ToolOK}
	}
	needsConfirmation := kind == tools.KindDestructive ||
		(mode != engine.ModeYolo && !(mode == engine.ModeAutoEdit && kind == tools.KindEdit))
	// Destructive operations are one-shot approvals by construction. A prior
	// "always allow" vote for ordinary edits/commands must never authorize a
	// later delete, reset, or wipe without a fresh explicit confirmation.
	if !needsConfirmation || (kind != tools.KindDestructive && alreadyApproved) {
		return agentcore.Authorization{Allowed: true, Status: agentcore.ToolOK}
	}
	var decision engine.Decision
	var err error
	if _, worker := tools.BackgroundOwner(ctx); worker && tools.BackgroundTaskID(ctx) != "" && tools.BackgroundWorkerID(ctx) != "" {
		decision, err = s.confirmWorkerTool(ctx, engine.ConfirmRequest{ToolName: name, Args: args, Kind: kind})
	} else {
		decision, err = s.confirmTool(ctx, engine.ConfirmRequest{ToolName: name, Args: args, Kind: kind})
	}
	if errors.Is(err, errApprovalChannelUnavailable) {
		return agentcore.Authorization{Status: agentcore.ToolBlocked, LLMContent: approvalChannelBlockedContent}
	}
	if err != nil {
		return agentcore.Authorization{
			Status:     agentcore.ToolDenied,
			LLMContent: "Tool confirmation did not complete. The tool was not executed.",
		}
	}
	if decision == engine.DecisionDeny {
		return agentcore.Authorization{
			Status:     agentcore.ToolDenied,
			LLMContent: "User declined this tool call. Ask for direction or try another approach.",
		}
	}
	if decision == engine.DecisionApproveAlways && kind != tools.KindDestructive {
		s.mu.Lock()
		s.approveAlways[name] = true
		s.mu.Unlock()
	}
	return agentcore.Authorization{Allowed: true, Status: agentcore.ToolOK}
}

// handleUserInput resolves a pending ask_user_question round. Answers are
// keyed by the broadcast question ids (q0, q1, …); multi-select answers
// may be arrays and are joined for the tool.
func (s *Server) handleUserInput(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	var body struct {
		Answers map[string]any `json:"answers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Answers == nil {
		writeErr(w, http.StatusBadRequest, "answers object required")
		return
	}
	requestID := r.PathValue("requestId")
	sess.mu.Lock()
	pending, ok := sess.userInputs[requestID]
	sess.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown or already-resolved user-input request")
		return
	}

	answers := make([]string, pending.num)
	for i := range answers {
		switch v := body.Answers[fmt.Sprintf("q%d", i)].(type) {
		case string:
			answers[i] = v
		case []any:
			parts := make([]string, 0, len(v))
			for _, p := range v {
				parts = append(parts, fmt.Sprint(p))
			}
			answers[i] = strings.Join(parts, ", ")
		case nil:
			answers[i] = ""
		default:
			answers[i] = fmt.Sprint(v)
		}
	}

	sess.broadcast("user_input_resolved", map[string]any{
		"sessionId": sess.id, "requestId": requestID, "answers": body.Answers,
	})
	select {
	case pending.ch <- answers:
	default: // duplicate vote; first one won
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleListSessions lists the workspace's stored chats, newest first
// (M5: sidebar thread list for the Wails desktop).
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	cwd := r.URL.Query().Get("cwd")
	if cwd == "" {
		cwd = s.WorkspaceCwd
	}
	infos, err := session.ListSessions(cwd)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		SessionID     string `json:"sessionId"`
		LiveSessionID string `json:"liveSessionId,omitempty"`
		Title         string `json:"title"`
		Modified      string `json:"modified"`
		Running       bool   `json:"running"`
	}
	type liveState struct {
		id      string
		running bool
	}
	live := map[string]liveState{}
	s.mu.Lock()
	for _, sess := range s.sessions {
		if sess.cwd != cwd || sess.queueKey == "" {
			continue
		}
		sess.mu.Lock()
		live[sess.queueKey] = liveState{id: sess.id, running: sess.running}
		sess.mu.Unlock()
	}
	s.mu.Unlock()
	out := []item{}
	for _, si := range infos {
		title := si.FirstUser
		if r := []rune(title); len(r) > 80 {
			title = string(r[:80]) + "…"
		}
		ls := live[si.SessionID]
		out = append(out, item{
			SessionID: si.SessionID, LiveSessionID: ls.id,
			Title: title, Modified: si.Modified.UTC().Format(time.RFC3339),
			Running: ls.running,
		})
	}
	writeJSON(w, map[string]any{"sessions": out})
}

// handleRollback truncates the session's conversation history by the last
// N turns (Meadow extension). The desktop checkpoint-revert flow restores
// files server-side, then calls this so the model's context matches the
// reverted filesystem. Rejected while a prompt is in flight.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	var body struct {
		NumTurns int `json:"numTurns"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.NumTurns <= 0 {
		writeErr(w, http.StatusBadRequest, "numTurns must be a positive integer")
		return
	}
	if !sess.promptMu.TryLock() {
		writeErr(w, http.StatusConflict, "a prompt is in flight; cancel it before rolling back")
		return
	}
	defer sess.promptMu.Unlock()

	sess.mu.Lock()
	n := body.NumTurns
	if n > len(sess.turnMarks) {
		n = len(sess.turnMarks)
	}
	if n > 0 {
		mark := sess.turnMarks[len(sess.turnMarks)-n]
		sess.history = sess.history[:mark]
		sess.turnMarks = sess.turnMarks[:len(sess.turnMarks)-n]
	}
	remaining := len(sess.turnMarks)
	sess.mu.Unlock()

	writeJSON(w, map[string]any{"turnsRolledBack": n, "remainingTurns": remaining})
}

// handleApproval switches the session's approval mode (Meadow extension).
func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "mode required")
		return
	}
	mode, err := engine.ParseApprovalMode(body.Mode)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess.approvalModeMu.Lock()
	defer sess.approvalModeMu.Unlock()
	sess.mu.Lock()
	previousMode := sess.engine.Mode
	sess.engine.Mode = mode
	sess.mu.Unlock()
	// An authenticated explicit mode change is a user grant, unlike an
	// automatic wake. Persist it before acknowledging it so resumed chats use
	// the chosen policy rather than their previous owner charter.
	if s.BackgroundWakeEnabled {
		if err := s.saveWakeOwner(sess, true); err != nil {
			sess.mu.Lock()
			sess.engine.Mode = previousMode
			sess.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	sess.broadcast("approval_mode_changed", map[string]any{
		"sessionId": sess.id, "mode": string(mode),
	})
	writeJSON(w, map[string]any{"mode": string(mode)})
}

// handleModelSwitch sets the session's model id (TS contract has this
// route; with a single local server the id must match a served model).
func (s *Server) handleModelSwitch(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	var body struct {
		ModelID string `json:"modelId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ModelID == "" {
		writeErr(w, http.StatusBadRequest, "modelId required")
		return
	}
	if !sess.promptMu.TryLock() {
		writeErr(w, http.StatusConflict, "cannot switch model while a turn is running")
		return
	}
	defer sess.promptMu.Unlock()
	sess.mu.Lock()
	if sess.running || sess.activeInferenceControls != nil {
		sess.mu.Unlock()
		writeErr(w, http.StatusConflict, "cannot switch model while a turn is running")
		return
	}
	if sess.chat == nil {
		chat, err := sess.openBoundChatWriter(sess.queueKey)
		if err != nil {
			sess.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		sess.chat = chat
	}
	if err := sess.chat.ModelBinding(body.ModelID); err != nil {
		sess.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	bindSessionModelLocked(sess, body.ModelID)
	sess.mu.Unlock()
	sess.broadcast("model_switched", map[string]any{
		"sessionId": sess.id, "modelId": body.ModelID,
	})
	writeJSON(w, map[string]any{"modelId": body.ModelID})
}

// bindSessionModelLocked changes only the conversation binding and capacity;
// provider activation belongs to the next prompt's runtime boundary.
func bindSessionModelLocked(sess *serveSession, modelID string) {
	if strings.TrimSpace(sess.engine.ModelID) != strings.TrimSpace(modelID) {
		if limits, ok := sess.engine.Client.(interface{ SetOutputTokenLimit(int) }); ok {
			// Capacity belongs to the runtime/model that advertised it. A model
			// switch must not present that value as the target model's capability.
			limits.SetOutputTokenLimit(0)
		}
	}
	sess.engine.ModelID = modelID
	contextWindow := modelmgr.ResolveRuntimeContextWindowTokens(modelID, 0)
	contextBudget := modelmgr.RuntimeBudgetFor(contextWindow)
	sess.engine.ContextWindowTokens = contextBudget.WindowTokens
	sess.engine.SessionTokenLimit = contextBudget.PromptLimitTokens
	if sess.engine.ContextManager != nil {
		sess.engine.ContextManager = sess.engine.ContextManager.WithHistoryTokenBounds(contextBudget.MessageLimitTokens)
	}
	restoreSessionInferenceControlsLocked(sess)
}

// handleSteer submits guidance to the active runtime. A late steer is rejected
// rather than leaking into the next independent task.
func (s *Server) handleSteer(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	var body struct {
		Text   string           `json:"text"`
		Prompt []map[string]any `json:"prompt"`
	}
	if err := decodeBoundedJSONBody(w, r, maxPromptRequestBytes, &body); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "guidance request is too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid guidance body")
		return
	}
	if len(body.Prompt) == 0 && strings.TrimSpace(body.Text) != "" {
		body.Prompt = []map[string]any{{"type": "text", "text": body.Text}}
	}
	materializedPrompt, err := sess.prepareIncomingAttachments(r.Context(), body.Prompt)
	if err != nil {
		writeCodedErr(w, http.StatusUnprocessableEntity, attachmentUnreadableCode, err.Error())
		return
	}
	body.Prompt = materializedPrompt
	defer cleanupPromptPDFTemps(body.Prompt)
	msg, err := sess.promptBlocks(body.Prompt)
	if err != nil {
		writeCodedErr(w, http.StatusUnprocessableEntity, attachmentUnreadableCode, err.Error())
		return
	}
	if !messageHasUserContent(msg) {
		writeErr(w, http.StatusBadRequest, "guidance content required")
		return
	}
	receipt, err := submitSessionGuidance(sess, msg, "")
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	publishSessionGuidance(sess, msg)
	writeJSON(w, map[string]any{
		"id": receipt.ID, "mode": receipt.Mode, "disposition": receipt.Disposition,
		"targetTurn": receipt.TargetTurn, "queuePosition": receipt.QueuePosition,
	})
}

func submitSessionGuidance(sess *serveSession, msg provider.Message, guidanceID string) (agentcore.GuidanceReceipt, error) {
	sess.mu.Lock()
	active := sess.activeRuntime
	sess.mu.Unlock()
	if active == nil {
		return agentcore.GuidanceReceipt{}, errors.New("no active run to steer")
	}
	receipt := agentcore.GuidanceReceipt{
		ID: guidanceID, Mode: agentcore.GuidanceSteerCurrent, Disposition: agentcore.GuidanceAccepted,
	}
	text := provider.MessageText(msg)
	if submitter, ok := active.(interface {
		SubmitGuidance(agentcore.Guidance) (agentcore.GuidanceReceipt, error)
	}); ok {
		return submitter.SubmitGuidance(agentcore.Guidance{
			ID: guidanceID, Text: text, ContentParts: msg.ContentParts, Mode: agentcore.GuidanceSteerCurrent,
		})
	} else if steerer, ok := active.(interface{ Steer(string) }); ok {
		if strings.TrimSpace(text) == "" || len(msg.ContentParts) > 0 {
			return agentcore.GuidanceReceipt{}, errors.New("active runtime does not accept multimodal guidance")
		}
		steerer.Steer(text)
	} else {
		return agentcore.GuidanceReceipt{}, errors.New("active runtime does not accept guidance")
	}
	return receipt, nil
}

func publishSessionGuidance(sess *serveSession, msg provider.Message) {
	// A pending ask_user_question belongs to the superseded model plan.  Let the
	// tool return immediately so the runtime can reach the next turn and apply
	// the newly accepted guidance instead of deadlocking behind its old prompt.
	sess.supersedePendingUserInputs()
	sess.supersedeLocationRequests()
	update := map[string]any{
		"sessionUpdate": "user_message_chunk",
		"content":       map[string]any{"type": "text", "text": userMessageDisplayText(msg)},
		"_steer":        true,
	}
	if attachments := messageAttachmentPayload(msg); len(attachments) > 0 {
		update["attachments"] = attachments
	}
	sess.broadcast("session_update", map[string]any{
		"sessionId": sess.id,
		"update":    update,
	})
}

var errRequestBodyTooLarge = errors.New("request body exceeds the configured limit")

// decodeBoundedJSONBody applies the same transport bound used by the primary
// prompt endpoint before JSON allocates large base64 strings. It also rejects
// trailing JSON values so steer and queue admission cannot bypass validation
// by smuggling a second document after the first.
func decodeBoundedJSONBody(w http.ResponseWriter, r *http.Request, limit int64, into any) error {
	if limit <= 0 || r.ContentLength > limit {
		return errRequestBodyTooLarge
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := decoder.Decode(into); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return errRequestBodyTooLarge
		}
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request contains more than one JSON value")
		}
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return errRequestBodyTooLarge
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg, "status": status})
}

const attachmentUnreadableCode = "ATTACHMENT_UNREADABLE"

func writeCodedErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg, "status": status, "code": code})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"v":    1,
		"mode": "http-bridge",
		"product": map[string]any{
			"name":    buildinfo.ProductName,
			"edition": buildinfo.Edition,
			"version": buildinfo.Version,
			"label":   buildinfo.Label(),
		},
		"features": []string{
			"session_scope_override",
			"permission_vote",
			"session_approval_channel",
		},
		"approvalChannels": []string{string(approvalChannelInteractive), string(approvalChannelNone)},
		"protocolVersions": map[string]any{
			"current":   "1.0.0",
			"supported": []string{"1.0.0"},
		},
		"modelServices": []any{},
		"workspaceCwd":  s.WorkspaceCwd,
	})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD             string   `json:"cwd"`
		SessionScope    string   `json:"sessionScope"`
		ApprovalChannel string   `json:"approvalChannel"`
		DisabledTools   []string `json:"disabledTools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid_session_request")
		return
	}
	if len(body.DisabledTools) > 32 || (len(body.DisabledTools) > 0 && body.SessionScope != "thread") {
		writeErr(w, http.StatusBadRequest, "disabledTools requires a new thread scope and at most 32 capability names")
		return
	}
	for _, name := range body.DisabledTools {
		if name == "" || len(name) > 100 || strings.ContainsAny(name, " \t\r\n/") {
			writeErr(w, http.StatusBadRequest, "invalid disabled tool name")
			return
		}
	}
	channel, err := parseApprovalChannel(body.ApprovalChannel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.SessionScope != "" && body.SessionScope != "single" && body.SessionScope != "thread" {
		writeErr(w, http.StatusBadRequest, "invalid_session_scope")
		return
	}
	cwd := body.CWD
	if cwd == "" {
		cwd = s.WorkspaceCwd
	}
	if !strings.HasPrefix(cwd, "/") {
		writeErr(w, http.StatusBadRequest, "cwd must be absolute")
		return
	}

	sess, attached, err := s.createConfiguredSession(r.Context(), cwd, channel, body.DisabledTools, body.SessionScope != "thread")
	if err != nil {
		writeErr(w, sessionCreationErrorStatus(err), err.Error())
		return
	}

	// Opening a conversation must not activate a model. Development latency
	// probes may explicitly opt into the old speculative prefix warm-up.
	if !attached {
		s.warmPrefixCacheOnOpen(sess)
		go s.startQueuePump(sess)
	}
	sess.mu.Lock()
	modelID := sess.engine.ModelID
	sess.mu.Unlock()

	writeJSON(w, map[string]any{
		"sessionId":       sess.id,
		"workspaceCwd":    cwd,
		"modelId":         modelID,
		"connectionId":    sess.connectionID,
		"attached":        attached,
		"approvalChannel": channel,
		"createdAt":       time.Now().UTC().Format(time.RFC3339),
	})
}

// buildSession prepares an unpublished session. All factory and persistence I/O
// stays outside Server.mu; only createConfiguredSession may publish it.
func (s *Server) buildSession(cwd string, channel sessionApprovalChannel, disabledTools []string) (*serveSession, error) {
	eng, err := s.NewEngine(cwd)
	if err != nil {
		return nil, err
	}
	if s.RequireToolAccount != nil {
		eng.RequireToolAccount = s.RequireToolAccount
	}
	sess := &serveSession{
		id: session.NewUUID(), cwd: cwd, engine: eng,
		connectionID:             s.ConnectionID,
		indexAttachmentDocuments: s.IndexAttachmentDocuments,
		subscribers:              map[chan event]struct{}{},
		permissions:              map[string]chan engine.Decision{},
		restoredPermissions:      map[string]sessionrecovery.PermissionCheckpoint{},
		restoredPermissionCalls:  map[string]sessionrecovery.ToolCallCheckpoint{},
		restoredApprovalActions:  map[string]int{},
		approveAlways:            map[string]bool{},
		userInputs:               map[string]pendingUserInput{},
		lastSeen:                 time.Now(),
		thinkingMode:             provider.ThinkingAuto,
		reasoningEffort:          provider.ReasoningEffortMedium,
		appliedGuidance:          map[string]bool{},
		defaultHarnessMode:       s.DefaultHarnessMode,
		conversationInboxEnabled: s.ConversationInboxEnabled,
		resolveHarnessMode:       s.ResolveHarnessMode,
		approvalChannel:          channel,
		disabledTools:            append([]string(nil), disabledTools...),
	}
	sess.queueKey = sess.id
	restoreSessionInferenceControlsLocked(sess)
	restoreSessionHarnessControlsLocked(sess)
	if err := sess.loadQueue(); err != nil {
		return nil, fmt.Errorf("load background queue: %w", err)
	}
	if err := sess.loadWorkCenter(); err != nil {
		return nil, fmt.Errorf("load work center: %w", err)
	}
	// ask_user_question interacts through the serve contract: questions
	// broadcast as SSE, answers arrive over the user-input endpoint.
	if t, ok := eng.Registry.Get("ask_user_question"); ok {
		if ask, isAsk := t.(*tools.AskUser); isAsk {
			ask.Ask = sess.askViaEvents
		}
	}
	sess.bindLocationTool()
	sess.bindAttachmentImageTools()
	// Binding may copy the registry. Revoke after all binding, while the
	// candidate is still private, so later registration cannot revive a tool.
	eng.Registry = eng.Registry.Excluding(sess.disabledTools)
	return sess, nil
}

// createSession is the lock-taking wrapper for non-HTTP callers (the autonomous
// scheduler). It registers the session and starts the prefix warm.
func (s *Server) createSession(cwd string) (*serveSession, error) {
	sess, _, err := s.createConfiguredSession(context.Background(), cwd, approvalChannelInteractive, nil, false)
	if err != nil {
		return nil, err
	}
	s.warmPrefixCacheOnOpen(sess)
	return sess, nil
}

func (s *Server) session(id string) *serveSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// promptBlocks converts the ACP content-block prompt array into the provider
// message used by the engine. Text-only prompts stay a plain string. Image
// blocks and rendered PDF pages become OpenAI-compatible multipart content.
// The legacy helper preserves visual behavior; session callers select known
// text-only PDF input from the current model's capability.
func promptBlocks(raw []map[string]any) (provider.Message, error) {
	return promptBlocksForModel(raw, "")
}

func promptBlocksForModel(raw []map[string]any, model string, clients ...any) (provider.Message, error) {
	var textParts []string
	var contentParts []provider.MessageContentPart
	imgs := 0
	collections := 0
	var pdfNames []string
	for _, block := range raw {
		switch t, _ := block["type"].(string); t {
		case "text":
			if txt, _ := block["text"].(string); txt != "" {
				textParts = append(textParts, txt)
				contentParts = append(contentParts, provider.MessageContentPart{Type: "text", Text: txt})
			}
		case "attachment_collection":
			if summary := stringValue(block["_attachment_summary"]); summary != "" {
				collections++
				textParts = append(textParts, summary)
				contentParts = append(contentParts, provider.MessageContentPart{Type: "text", Text: summary, AttachmentKind: "attachment_collection", AttachmentName: stringValue(block["collection_id"])})
			}
		case "image", "image_url", "input_image":
			if url := imageURLFromPromptBlock(block); url != "" {
				imgs++
				contentParts = append(contentParts, provider.MessageContentPart{
					Type:     "image_url",
					ImageURL: &provider.MessageImageURL{URL: url},
				})
			}
		case "file", "input_file":
			if name, _, ok := promptDocumentBlock(block); ok {
				context, err := promptDocumentContext(block)
				if err != nil {
					return provider.Message{}, err
				}
				pdfNames = append(pdfNames, promptDocumentDisplay(block))
				textParts = append(textParts, context)
				contentParts = append(contentParts, provider.MessageContentPart{Type: "text", Text: context, AttachmentKind: "document_text", AttachmentName: name})
				continue
			}
			if name, _, ok := promptPDFBlock(block); ok {
				pdfNames = append(pdfNames, promptPDFDocumentDisplay(block))
				if pdfTextOnlyModel(model, clients...) {
					context, err := promptPDFTextContext(block)
					if err != nil {
						return provider.Message{}, err
					}
					textParts = append(textParts, context)
					contentParts = append(contentParts, provider.MessageContentPart{Type: "text", Text: context, AttachmentKind: "document_text", AttachmentName: name})
					continue
				}
				if context := promptPDFDocumentContext(block); context != "" {
					textParts = append(textParts, context)
					contentParts = append(contentParts, provider.MessageContentPart{Type: "text", Text: context, AttachmentKind: "document_text", AttachmentName: name})
				}
				pages, err := promptPDFVisualPages(block)
				if err != nil {
					return provider.Message{}, err
				}
				for _, page := range pages {
					data, err := os.ReadFile(page.FilePath)
					if err != nil || len(data) == 0 || len(data) > promptPDFPageMaxBytes {
						return provider.Message{}, fmt.Errorf("PDF %s 的第 %d 頁影像無法讀取", name, page.Number)
					}
					observation := "原始頁面影像；請直接觀察文字、圖表與版面，不要假設另有文字抽取"
					if file, _ := block["file"].(map[string]any); stringValue(file["pdf_text_status"]) == "available" {
						observation = "原始頁面影像；可搭配已保留的文字抽取，圖表與版面仍需直接觀察"
					}
					contentParts = append(contentParts,
						provider.MessageContentPart{
							Type: "text",
							Text: fmt.Sprintf(
								"PDF「%s」第 %d/%d 頁（%s）。",
								name, page.Number, page.Count, observation,
							),
						},
						provider.MessageContentPart{
							Type: "image_url",
							ImageURL: &provider.MessageImageURL{
								URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data),
							},
							AttachmentKind: "pdf_page", AttachmentName: name,
							PageNumber: page.Number, PageCount: page.Count,
						},
					)
					imgs++
				}
			}
		}
	}
	// Keep the actual image only in ContentParts. Content is the durable text
	// view rendered by Desktop and must never grow a synthetic attachment
	// sentence that the model can echo back to the user.
	content := strings.Join(textParts, "\n")
	if len(pdfNames) > 0 {
		attachmentLine := "附件：" + strings.Join(pdfNames, "、")
		if strings.TrimSpace(content) == "" {
			content = attachmentLine
		} else {
			content += "\n" + attachmentLine
		}
	}
	msg := provider.Message{Role: "user", Content: content}
	if imgs > 0 || len(pdfNames) > 0 || collections > 0 {
		msg.ContentParts = contentParts
	}
	return msg, nil
}

func promptDisplayText(raw []map[string]any) string {
	var textParts []string
	var pdfNames []string
	for _, block := range raw {
		switch kind, _ := block["type"].(string); kind {
		case "text":
			if text := strings.TrimSpace(stringValue(block["text"])); text != "" {
				textParts = append(textParts, text)
			}
		case "file", "input_file":
			if _, _, ok := promptDocumentBlock(block); ok {
				pdfNames = append(pdfNames, promptDocumentDisplay(block))
				continue
			}
			if _, _, ok := promptPDFBlock(block); ok {
				pdfNames = append(pdfNames, promptPDFDocumentDisplay(block))
			}
		}
	}
	content := strings.Join(textParts, "\n")
	if len(pdfNames) == 0 {
		return content
	}
	attachmentLine := "附件：" + strings.Join(pdfNames, "、")
	if content == "" {
		return attachmentLine
	}
	return content + "\n" + attachmentLine
}

// Canonical Content includes actual document text so context accounting sees
// the same bytes as the model wire. Display removes exactly the server-owned
// document parts, retaining the human request and durable attachment paths.
func userMessageDisplayText(msg provider.Message) string {
	text := provider.MessageText(msg)
	for _, part := range msg.ContentParts {
		if part.AttachmentKind == "document_text" || part.AttachmentKind == "attachment_collection" {
			text = strings.ReplaceAll(text, part.Text, "")
		}
	}
	return strings.TrimSpace(text)
}

func messageHasUserContent(msg provider.Message) bool {
	if strings.TrimSpace(provider.MessageText(msg)) != "" {
		return true
	}
	return len(messageAttachmentPayload(msg)) > 0
}

func messageAttachmentPayload(msg provider.Message) []map[string]any {
	attachments := make([]map[string]any, 0)
	for i, part := range msg.ContentParts {
		if part.AttachmentKind == "attachment_collection" {
			attachments = append(attachments, map[string]any{"type": "attachment_collection", "collection_id": part.AttachmentName, "name": "圖片附件"})
			continue
		}
		if part.Type != "image_url" || part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
			continue
		}
		name := fmt.Sprintf("圖片 %d", i+1)
		if part.AttachmentKind == "pdf_page" {
			name = fmt.Sprintf("%s · 第 %d/%d 頁", part.AttachmentName, part.PageNumber, part.PageCount)
		}
		attachments = append(attachments, map[string]any{"url": part.ImageURL.URL, "name": name})
	}
	return attachments
}

// validatePromptImages is the single server-side boundary for vision prompts.
// Clients compress before sending, but the daemon must remain safe for CLI,
// API and queued callers that bypass the Desktop composer.
func validatePromptImages(msg provider.Message, model string, clients ...any) error {
	directImages := 0
	pdfPages := 0
	for _, part := range msg.ContentParts {
		if part.Type != "image_url" || part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
			continue
		}
		if part.AttachmentKind == "pdf_page" {
			pdfPages++
		} else {
			directImages++
		}
	}
	if directImages+pdfPages == 0 {
		return nil
	}
	if directImages+pdfPages > maxPromptImageCount {
		return fmt.Errorf("圖片與 PDF 頁面合計最多 %d 張；請拆成較小批次", maxPromptImageCount)
	}
	if directImages > 4 {
		return fmt.Errorf("一次最多附 4 張圖片")
	}
	if pdfPages > promptPDFMaxPages {
		return fmt.Errorf("一次最多直接觀看 %d 頁 PDF", promptPDFMaxPages)
	}
	var client any
	if len(clients) > 0 {
		client = clients[0]
	}
	profile, known := provider.ProfileForClient(client, model)
	if !known || !profile.Multimodal {
		return fmt.Errorf("目前模型 %s 不支援圖片辨識；請切換到模型清單中標示支援圖片的 Vision 模型", model)
	}
	totalBytes := 0
	remainingDecodedBytes := maxPromptImagesDecodedBytes
	for _, part := range msg.ContentParts {
		if part.Type != "image_url" || part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
			continue
		}
		limit := maxDirectPromptImageBytes
		if part.AttachmentKind == "pdf_page" {
			limit = promptPDFPageMaxBytes
		}
		payload, err := decodePromptImageDataURL(part.ImageURL.URL, limit, remainingDecodedBytes)
		if errors.Is(err, errPromptImageTooLarge) {
			if part.AttachmentKind == "pdf_page" {
				return fmt.Errorf("PDF %s 的第 %d 頁影像超過 1 MB", part.AttachmentName, part.PageNumber)
			}
			return fmt.Errorf("圖片過大；請先壓縮到 1 MB 以下")
		}
		if err != nil {
			return err
		}
		totalBytes += len(payload.data)
		if totalBytes > maxPromptImageBatchBytes {
			return fmt.Errorf("圖片與 PDF 頁面合計過大；請拆成較小批次")
		}
		remainingDecodedBytes -= payload.decodedBytes
	}
	return nil
}

var errPromptImageTooLarge = errors.New("prompt image exceeds encoded byte limit")

type decodedPromptImage struct {
	data         []byte
	mimeType     string
	decodedBytes int64
}

// decodePromptImageDataURL is the shared ingress validator for both inference
// and durable attachment persistence. It verifies the declared MIME against
// the actual codec, bounds decoded memory before allocating the full image,
// and then performs a complete decode so a valid-looking truncated header is
// never persisted or sent to the model.
func decodePromptImageDataURL(raw string, byteLimit int, remainingDecodedBytes int64) (decodedPromptImage, error) {
	raw = strings.TrimSpace(raw)
	header, encoded, ok := strings.Cut(strings.TrimPrefix(raw, "data:"), ",")
	if !strings.HasPrefix(raw, "data:") || !ok || header == "" || encoded == "" {
		return decodedPromptImage{}, fmt.Errorf("圖片必須是有界的 base64 data URL")
	}
	metadata := strings.Split(header, ";")
	if len(metadata) != 2 || metadata[1] != "base64" {
		return decodedPromptImage{}, fmt.Errorf("圖片只支援不含額外參數的 base64 data URL")
	}
	declaredMIME := ""
	switch metadata[0] {
	case "image/jpeg", "image/jpg":
		declaredMIME = "image/jpeg"
	case "image/png":
		declaredMIME = "image/png"
	default:
		return decodedPromptImage{}, fmt.Errorf("圖片格式 %s 不支援；請使用 JPEG 或 PNG", metadata[0])
	}
	if byteLimit <= 0 || len(encoded) > base64.StdEncoding.EncodedLen(byteLimit) {
		return decodedPromptImage{}, errPromptImageTooLarge
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return decodedPromptImage{}, fmt.Errorf("圖片資料解碼失敗: %w", err)
	}
	if len(data) == 0 {
		return decodedPromptImage{}, fmt.Errorf("圖片資料不得為空")
	}
	if len(data) > byteLimit {
		return decodedPromptImage{}, errPromptImageTooLarge
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return decodedPromptImage{}, fmt.Errorf("圖片內容不是完整有效的 JPEG 或 PNG: %w", err)
	}
	actualMIME := ""
	switch format {
	case "jpeg":
		actualMIME = "image/jpeg"
	case "png":
		actualMIME = "image/png"
	default:
		return decodedPromptImage{}, fmt.Errorf("圖片實際格式 %s 不支援；請使用 JPEG 或 PNG", format)
	}
	if actualMIME != declaredMIME {
		return decodedPromptImage{}, fmt.Errorf("圖片宣稱的 MIME %s 與實際 %s 位元不一致", metadata[0], actualMIME)
	}
	decodedBytes, safe := promptImageDecodedBytes(config.Width, config.Height)
	if !safe || decodedBytes > remainingDecodedBytes {
		return decodedPromptImage{}, fmt.Errorf("圖片解碼尺寸 %dx%d 超過安全上限；請縮小圖片或拆成較小批次", config.Width, config.Height)
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format {
		if err == nil {
			err = fmt.Errorf("設定格式 %s 與完整解碼格式 %s 不一致", format, decodedFormat)
		}
		return decodedPromptImage{}, fmt.Errorf("圖片內容不是完整有效的 JPEG 或 PNG: %w", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != config.Width || bounds.Dy() != config.Height {
		return decodedPromptImage{}, fmt.Errorf("圖片解碼尺寸不一致")
	}
	return decodedPromptImage{data: data, mimeType: actualMIME, decodedBytes: decodedBytes}, nil
}

func promptImageDecodedBytes(width, height int) (int64, bool) {
	if width <= 0 || height <= 0 || width > maxPromptImageDimension || height > maxPromptImageDimension {
		return 0, false
	}
	pixels := int64(width) * int64(height)
	if pixels <= 0 || pixels > maxPromptImagePixels {
		return 0, false
	}
	return pixels * maxPromptImageBytesPerPixel, true
}

func imageURLFromPromptBlock(block map[string]any) string {
	if imageURL, _ := block["image_url"].(map[string]any); imageURL != nil {
		if url, _ := imageURL["url"].(string); strings.TrimSpace(url) != "" {
			return url
		}
	}
	if source, _ := block["source"].(map[string]any); source != nil {
		if url, _ := source["url"].(string); strings.TrimSpace(url) != "" {
			return url
		}
		data, _ := source["data"].(string)
		if strings.TrimSpace(data) == "" {
			return ""
		}
		mime, _ := source["mimeType"].(string)
		if mime == "" {
			mime, _ = source["media_type"].(string)
		}
		if mime == "" {
			mime = "image/png"
		}
		if strings.HasPrefix(data, "data:") {
			return data
		}
		return "data:" + mime + ";base64," + data
	}
	if url, _ := block["url"].(string); strings.TrimSpace(url) != "" {
		return url
	}
	return ""
}

// persistPromptAttachments keeps the normalized browser image outside the
// JSONL transcript.  The provider still receives the original data URL for
// this turn; the returned names are small durable references used by Desktop
// history and the attachment endpoint. Remote URLs are intentionally rejected
// by the shared validator: only bounded, fully-decoded data URLs are safe to
// send through the native runtime or persist.
func persistPromptAttachments(cwd, chatID string, msg provider.Message) ([]string, error) {
	if chatID == "" {
		return nil, nil
	}
	type pendingAttachment struct {
		part    provider.MessageContentPart
		payload decodedPromptImage
	}
	pending := make([]pendingAttachment, 0)
	directImages := 0
	pdfPages := 0
	for _, part := range msg.ContentParts {
		if part.Type != "image_url" || part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
			continue
		}
		if part.AttachmentKind == "pdf_page" {
			pdfPages++
		} else {
			directImages++
		}
	}
	if directImages+pdfPages > maxPromptImageCount {
		return nil, fmt.Errorf("圖片與 PDF 頁面合計最多 %d 張；請拆成較小批次", maxPromptImageCount)
	}
	if directImages > 4 {
		return nil, fmt.Errorf("一次最多附 4 張圖片")
	}
	if pdfPages > promptPDFMaxPages {
		return nil, fmt.Errorf("一次最多直接觀看 %d 頁 PDF", promptPDFMaxPages)
	}
	totalBytes := 0
	remainingDecodedBytes := maxPromptImagesDecodedBytes
	for _, part := range msg.ContentParts {
		if part.Type != "image_url" || part.ImageURL == nil {
			continue
		}
		raw := strings.TrimSpace(part.ImageURL.URL)
		if raw == "" {
			continue
		}
		maxBytes := maxDirectPromptImageBytes
		if part.AttachmentKind == "pdf_page" {
			maxBytes = promptPDFPageMaxBytes
		}
		payload, err := decodePromptImageDataURL(raw, maxBytes, remainingDecodedBytes)
		if errors.Is(err, errPromptImageTooLarge) {
			if part.AttachmentKind == "pdf_page" {
				return nil, fmt.Errorf("PDF %s 的第 %d 頁影像超過 1 MB", part.AttachmentName, part.PageNumber)
			}
			return nil, fmt.Errorf("圖片壓縮後仍超過 1 MB")
		}
		if err != nil {
			return nil, err
		}
		totalBytes += len(payload.data)
		if totalBytes > maxPromptImageBatchBytes {
			return nil, fmt.Errorf("圖片與 PDF 頁面合計過大；請拆成較小批次")
		}
		remainingDecodedBytes -= payload.decodedBytes
		pending = append(pending, pendingAttachment{part: part, payload: payload})
	}
	if len(pending) == 0 {
		return nil, nil
	}
	dir, err := session.ArtifactsDir(cwd, chatID)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(pending))
	for _, attachment := range pending {
		ext := ".jpg"
		switch attachment.payload.mimeType {
		case "image/png":
			ext = ".png"
		}
		name := fmt.Sprintf("attachment-%s-%d%s", session.NewUUID(), len(refs)+1, ext)
		if attachment.part.AttachmentKind == "pdf_page" {
			name = fmt.Sprintf("pdf-page-%03d-%s%s", attachment.part.PageNumber, session.NewUUID(), ext)
		}
		if err := os.WriteFile(filepath.Join(dir, name), attachment.payload.data, 0o600); err != nil {
			for _, ref := range refs {
				_ = os.Remove(filepath.Join(dir, ref))
			}
			return nil, err
		}
		refs = append(refs, name)
	}
	return refs, nil
}

func inferenceProgressPayload(sessionID string, contextWindowTokens int, progress provider.InferenceProgress) map[string]any {
	payload := map[string]any{
		"sessionId": sessionID, "serverTimeMs": time.Now().UnixMilli(),
		"requestId": progress.RequestID, "model": progress.Model, "phase": progress.Phase,
		"elapsedMs":    progress.Elapsed.Milliseconds(),
		"promptTokens": progress.PromptTokens, "cachedTokens": progress.CachedTokens,
		"remainingTokens":        progress.RemainingTokens,
		"prefillProcessedTokens": progress.PrefillProcessedTokens,
		"prefillTotalTokens":     progress.PrefillTotalTokens,
		"completionTokens":       progress.CompletionTokens,
		"cacheHitType":           progress.CacheHitType,
		"ttftMs":                 progress.TTFT.Milliseconds(),
		"activationMs":           progress.Activation.Milliseconds(),
		"includesActivation":     progress.IncludesActivation,
		"firstTokenVisible":      progress.FirstTokenVisible,
		"promptEstimatedTokens":  progress.PromptEstimatedTokens,
		"readingSource":          progress.ReadingSource, "readingExcerpt": progress.ReadingExcerpt,
		"prefillObservedTokens":    progress.PrefillObservedTokens,
		"prefillObservedElapsedMs": progress.PrefillObservedElapsed.Milliseconds(),
		"ingestionSourceId":        progress.IngestionSourceID,
		"ingestionSourceName":      progress.IngestionSourceName,
		"ingestionProcessed":       progress.IngestionProcessed,
		"ingestionTotal":           progress.IngestionTotal,
		"ingestionProcessedBytes":  progress.IngestionProcessedBytes,
		"ingestionTotalBytes":      progress.IngestionTotalBytes,
		"ingestionStartLine":       progress.IngestionStartLine,
		"ingestionEndLine":         progress.IngestionEndLine,
		"ingestionNote":            progress.IngestionNote,
		"ingestionReused":          progress.IngestionReused,
	}
	// Transport bytes are separate from model tokens. Local engines retain
	// their existing token telemetry and do not need this optional counter.
	if progress.BytesReceived > 0 {
		payload["bytesReceived"] = progress.BytesReceived
	}
	// The live runtime capacity is authoritative. The model-family fallback is
	// used only before a worker has reported its physical context window.
	if tokens := modelmgr.ResolveRuntimeContextWindowTokens(progress.Model, contextWindowTokens); tokens > 0 {
		payload["contextWindowTokens"] = tokens
	}
	return payload
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	requestID := r.Header.Get(promptRequestIDHeader)
	replayRequestID, replayOnly := promptReplayOnlyRequestID(r.Context())
	if replayOnly && (!validPromptRequestID(replayRequestID) || requestID != replayRequestID) {
		writeCodedErr(w, http.StatusConflict, "prompt_replay_unavailable", errPromptReplayUnavailable.Error())
		return
	}
	if requestID != "" && !validPromptRequestID(requestID) {
		writeErr(w, http.StatusBadRequest, "invalid prompt request ID")
		return
	}
	admission := sess.beginPromptAdmission(requestID)
	handedOff := false
	defer func() {
		if !handedOff {
			sess.endPromptAdmission(admission)
		}
	}()
	if r.ContentLength > maxPromptRequestBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "prompt request is too large")
		return
	}
	var body struct {
		Prompt  []map[string]any `json:"prompt"`
		Options struct {
			TwoPass                  bool   `json:"twoPass"`
			Mode                     string `json:"mode"`
			RunTimeoutMs             *int64 `json:"runTimeoutMs"`
			RunTokenLimit            *int64 `json:"runTokenLimit"`
			EvoResumeReflectionRunID string `json:"evoResumeReflectionRunId"`
		} `json:"options"`
	}
	promptHash := sha256.New()
	decoder := json.NewDecoder(io.TeeReader(
		http.MaxBytesReader(w, r.Body, maxPromptRequestBytes),
		promptHash,
	))
	decodeErr := decoder.Decode(&body)
	if decodeErr == nil {
		var extra any
		if trailingErr := decoder.Decode(&extra); !errors.Is(trailingErr, io.EOF) {
			if trailingErr == nil {
				decodeErr = errors.New("prompt request contains more than one JSON value")
			} else {
				decodeErr = trailingErr
			}
		}
	}
	if decodeErr != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(decodeErr, &maxBytesErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "prompt request is too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "prompt must be a non-empty array of content blocks")
		return
	}
	if len(body.Prompt) == 0 {
		writeErr(w, http.StatusBadRequest, "prompt must be a non-empty array of content blocks")
		return
	}
	if err := validateRunBudgets(body.Options.RunTimeoutMs, body.Options.RunTokenLimit); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var requestFingerprint [sha256.Size]byte
	copy(requestFingerprint[:], promptHash.Sum(nil))

	// Desktop retries a prompt with the same opaque request ID when the HTTP
	// connection disappears before the blocking response arrives. Claim that
	// ID before PDF materialization or attachment persistence so a replay can
	// never duplicate either those side effects or the model turn. Clients that
	// omit the header retain the original synchronous API behavior.
	var promptRecord *promptRequestRecord
	if requestID != "" {
		claim := sess.claimPromptRequest
		if replayOnly {
			claim = sess.replayPromptRequest
		}
		claimedRecord, replay, claimErr := claim(requestID, requestFingerprint)
		promptRecord = claimedRecord
		if errors.Is(claimErr, errPromptReplayUnavailable) {
			writeCodedErr(w, http.StatusConflict, "prompt_replay_unavailable", claimErr.Error())
			return
		}
		if errors.Is(claimErr, errPromptRequestConflict) {
			writeErr(w, http.StatusConflict, claimErr.Error())
			return
		}
		if errors.Is(claimErr, errPromptRequestLimit) {
			writeErr(w, http.StatusTooManyRequests, claimErr.Error())
			return
		}
		if claimErr != nil {
			writeErr(w, http.StatusInternalServerError, "prompt idempotency unavailable")
			return
		}
		if replay {
			waitPromptRequest(w, r, promptRecord)
			return
		}
	}
	rejectPrompt := func(status int, message string) {
		result := promptRequestResult{status: status, errorMessage: message}
		if status == http.StatusUnprocessableEntity {
			result.errorCode = attachmentUnreadableCode
		}
		sess.completePromptRequest(promptRecord, result)
		writePromptRequestResult(w, result)
	}
	// A transport replay must return its original terminal result even after a
	// successful reflection changed latest.json and is no longer retryable.
	if body.Options.EvoResumeReflectionRunID != "" {
		sess.mu.Lock()
		cwd, chat, model, mode, running := sess.cwd, sess.queueKey, sess.engine.ModelID, sess.snapshotHarnessModeLocked(), sess.running
		sess.mu.Unlock()
		if mode != "evo" || running {
			rejectPrompt(http.StatusConflict, "請在 Evo 任務停止後重試回顧")
			return
		}
		if err := deskevo.ValidateReflectionResume(os.Getenv("MEADOW_HOME"), cwd, chat, model, body.Options.EvoResumeReflectionRunID); err != nil {
			rejectPrompt(http.StatusConflict, err.Error())
			return
		}
	}

	cancelPreparedPrompt := func() bool {
		if !sess.promptAdmissionCancelled(admission) {
			return false
		}
		result := promptRequestResult{status: http.StatusOK, stopReason: "cancelled"}
		sess.completePromptRequest(promptRecord, result)
		writePromptRequestResult(w, result)
		return true
	}
	if cancelPreparedPrompt() {
		return
	}

	runMode := agentcore.RunModeAuto
	switch strings.TrimSpace(body.Options.Mode) {
	case "", "auto":
	case string(agentcore.RunModeResearch):
		runMode = agentcore.RunModeResearch
	default:
		rejectPrompt(http.StatusBadRequest, "options.mode must be auto or research")
		return
	}
	attachmentModel := sess.attachmentModelID()
	materializedPrompt, err := sess.prepareIncomingAttachments(r.Context(), body.Prompt)
	if cancelPreparedPrompt() {
		cleanupPromptPDFTemps(materializedPrompt)
		return
	}
	if err != nil {
		rejectPrompt(http.StatusUnprocessableEntity, err.Error())
		return
	}
	body.Prompt = materializedPrompt
	defer cleanupPromptPDFTemps(body.Prompt)
	userMsg, err := sess.promptBlocks(body.Prompt)
	if err != nil {
		rejectPrompt(http.StatusUnprocessableEntity, err.Error())
		return
	}
	text := userMessageDisplayText(userMsg)
	if !messageHasUserContent(userMsg) {
		rejectPrompt(http.StatusBadRequest, "prompt has no user content")
		return
	}
	chatID := sess.queueKey
	if chatID == "" {
		chatID = sess.id
	}
	attachmentRefs, err := persistPromptAttachments(sess.cwd, chatID, userMsg)
	if err != nil {
		rejectPrompt(http.StatusUnprocessableEntity, err.Error())
		return
	}

	// The turn must survive the HTTP client: a closed window or timed-out
	// request must not kill a running task. The run owns a background
	// context cancelled via /cancel or an explicit caller deadline. The handler
	// waits for the turn but detaches when the client goes away — events keep landing
	// in the ring and the chat log either way.
	done := make(chan promptRequestResult, 1)
	runID := session.NewUUID()
	handedOff = true
	go func() {
		defer sess.endPromptAdmission(admission)
		turnStarted := time.Now()
		stop := "error"
		checkpointID := ""
		// A real prompt outranks the background prefix warm: cancel it so
		// the serial prefill slot frees immediately.
		sess.mu.Lock()
		if sess.warmCancel != nil {
			sess.warmCancel()
		}
		sess.mu.Unlock()
		// FIFO per session; the goroutine (not the handler) holds the lock
		// so a detached client cannot leave it dangling.
		sess.promptMu.Lock()
		defer sess.promptMu.Unlock()
		ctx, cancel, _ := newRunBudgetContext(body.Options.RunTimeoutMs, body.Options.RunTokenLimit)
		if body.Options.EvoResumeReflectionRunID != "" {
			ctx = deskevo.WithReflectionResume(ctx, body.Options.EvoResumeReflectionRunID)
		}
		defer cancel()
		// Register the terminal finalizer after promptMu.Unlock so LIFO defer
		// order clears this turn's cancel/run state before the next prompt can
		// acquire the session. Chat persistence and checkpoint setup below may
		// still fail early; every such path publishes exactly one terminal state.
		defer func() {
			sess.mu.Lock()
			sess.cancelRun = nil
			if sess.activeRunID == runID {
				sess.activeRunID = ""
				sess.activePromptRequestID = ""
			}
			sess.activeRunCheckpointID = ""
			sess.running = false
			sess.activeInferenceControls = nil
			sess.activeHarnessMode = nil
			sess.mu.Unlock()
			sess.broadcast("turn_completed", map[string]any{
				"sessionId":    sess.id,
				"runId":        runID,
				"origin":       "foreground",
				"stopReason":   stop,
				"checkpointId": checkpointID,
				"elapsedMs":    time.Since(turnStarted).Milliseconds(),
			})
			sess.mu.Lock()
			sess.runReplayAfterEventID = sess.nextEventID
			sess.mu.Unlock()
			sess.broadcast("run_state", map[string]any{
				"sessionId": sess.id, "chatId": sess.queueKey, "runId": runID,
				"status": "completed", "stopReason": stop,
			})
			result := promptRequestResult{
				status:       http.StatusOK,
				stopReason:   stop,
				checkpointID: checkpointID,
			}
			sess.completePromptRequest(promptRecord, result)
			done <- result
			s.startQueuePump(sess)
		}()
		// Preparation precedes the FIFO lock. A model switch in that interval
		// cannot reuse attachment input selected for a different capability.
		if sess.attachmentModelID() != attachmentModel {
			sess.broadcast("stream_error", map[string]any{"message": "模型在附件準備期間已變更，請重新送出以使用目前模型"})
			return
		}
		if err := validatePromptImages(userMsg, sess.attachmentModelID(), sess.attachmentClient()); err != nil {
			sess.broadcast("stream_error", map[string]any{"message": err.Error()})
			return
		}
		admitted, admissionErr := sess.admitForegroundPrompt(admission, runID, cancel)
		if admissionErr != nil {
			sess.broadcast("stream_error", map[string]any{"message": admissionErr.Error()})
			return
		}
		if !admitted {
			stop = "cancelled"
			return
		}
		sess.broadcast("run_state", map[string]any{
			"sessionId": sess.id, "chatId": sess.queueKey, "runId": runID, "status": "running",
		})

		// Chat persistence: open the log on the first prompt and record
		// the user message up front (CLI parity) so GET /sessions lists
		// the new conversation immediately.
		if sess.chat == nil {
			cw, chatErr := sess.openBoundChatWriter(sess.id)
			if chatErr != nil {
				sess.broadcast("stream_error", map[string]any{"message": chatErr.Error()})
				return
			}
			sess.chat = cw
		}
		runMessages := append(append([]provider.Message(nil), sess.history...), userMsg)
		runCheckpointID := session.StableCheckpointIdentity(chatID, runMessages)
		if chatErr := sess.chat.UserWithRequestID(text, attachmentRefs, requestID); chatErr != nil {
			sess.broadcast("stream_error", map[string]any{"message": chatErr.Error()})
			return
		}
		if chatErr := persistAttachmentCollectionContext(sess.chat, body.Prompt); chatErr != nil {
			sess.broadcast("stream_error", map[string]any{"message": chatErr.Error()})
			return
		}
		if chatErr := sess.persistHarnessRunStart(runCheckpointID); chatErr != nil {
			sess.broadcast("stream_error", map[string]any{"message": chatErr.Error()})
			return
		}

		sess.mu.Lock()
		sess.activeRunCheckpointID = runCheckpointID
		sess.stableCheckpointErr = nil
		mark := len(sess.history)
		sess.turnMarks = append(sess.turnMarks, mark)
		sess.mu.Unlock()

		stop = s.runPromptMessageWithRequestID(ctx, sess, userMsg, mark, body.Options.TwoPass, runMode, true, requestID)
		stop = sess.settleForegroundExecution(ctx, stop)
		if err := sess.pauseQueueForBudget(stop); err != nil {
			stop = "error"
			sess.broadcast("stream_error", map[string]any{"message": err.Error()})
		}
		sess.mu.Lock()
		checkpointID = sess.stableCheckpointID
		checkpointErr := sess.stableCheckpointErr
		sess.mu.Unlock()
		if checkpointErr != nil {
			stop = "error"
		}
		var terminalErr error
		if checkpointErr == nil {
			if stop == "error" || isRunBudgetStop(stop) {
				terminalErr = sess.chat.RunInterrupted(stop, checkpointID)
			} else {
				terminalErr = sess.chat.RunFinishedCheckpoint(stop, checkpointID)
			}
		}
		if terminalErr != nil {
			stop = "error"
			sess.broadcast("stream_error", map[string]any{"message": terminalErr.Error()})
		}
	}()

	select {
	case result := <-done:
		writePromptRequestResult(w, result)
	case <-r.Context().Done():
		// Client disconnected; the turn keeps running to completion.
	}
}

// runPrompt wires engine events into ACP session_update frames and runs
// one turn to completion. mark is len(history) at the turn start — the
// persistence slice point for the turn's new messages.
func (s *Server) runPrompt(ctx context.Context, sess *serveSession, text string, mark int) string {
	return s.runPromptMessage(ctx, sess, provider.Message{Role: "user", Content: text}, mark, false, agentcore.RunModeAuto, false)
}

func (s *Server) runPromptMessage(ctx context.Context, sess *serveSession, userMsg provider.Message, mark int, twoPass bool, runMode agentcore.RunMode, foreground bool) (stopReason string) {
	return s.runPromptMessageWithRequestID(ctx, sess, userMsg, mark, twoPass, runMode, foreground, "")
}

func (s *Server) runPromptMessageWithRequestID(ctx context.Context, sess *serveSession, userMsg provider.Message, mark int, twoPass bool, runMode agentcore.RunMode, foreground bool, requestID string) (stopReason string) {
	if foreground && s.BackgroundWakeEnabled {
		if err := s.refreshWakeOwnerForUserAdmission(sess); err != nil {
			sess.broadcast("background_wake_blocked", map[string]any{"message": err.Error()})
			return "error"
		}
	}
	if automatic, _ := ctx.Value(backgroundCompletionContextKey{}).(bool); automatic {
		if err := s.ensureWakeOwner(sess); err != nil {
			sess.broadcast("background_wake_blocked", map[string]any{"message": err.Error()})
			return "error"
		}
	}
	ctx = s.withNativeWorkerOwner(ctx, sess)
	s.startBackgroundWatcher(sess)
	if err := protectPromptDocumentFiles(sess.engine.Registry, sess.cwd, queueChatID(sess), nil); err != nil {
		return "error"
	}
	text := provider.MessageText(userMsg)
	startedRunID := ""
	sess.mu.Lock()
	if !foreground || sess.activeRunID == "" {
		// Queue and recovery callers share this entry point. Their receipts
		// must not reuse the preceding foreground run's identity.
		sess.activeRunID = session.NewUUID()
		startedRunID = sess.activeRunID
	}
	interruptedTask := sess.interruptedTaskPending
	sess.interruptedTaskPending = false
	inferenceSelection := sess.activeInferenceControls
	if inferenceSelection == nil {
		inferenceSelection = snapshotSessionInferenceControlsLocked(sess)
	}
	ownedHarnessSelection := sess.activeHarnessMode == nil
	if ownedHarnessSelection {
		sess.admitHarnessModeLocked(sess.snapshotHarnessModeLocked())
	}
	harnessMode := *sess.activeHarnessMode
	sess.mu.Unlock()
	if ownedHarnessSelection {
		defer func() { sess.mu.Lock(); sess.activeHarnessMode = nil; sess.mu.Unlock() }()
	}
	if deskevo.ReflectionResumeRequested(ctx) && harnessMode != "evo" {
		sess.broadcast("stream_error", map[string]any{"message": "執行方式已變更，回顧尚未重試；請切回 Evo 後重試"})
		return "error"
	}
	locationRunID := sess.beginLocationRun(ctx, foreground)
	defer sess.finishLocationRun(locationRunID)
	if startedRunID != "" {
		// Clients use run_state to switch their event scope before accepting
		// thought/content/tool chunks from a promoted queue or recovery run.
		sess.broadcast("run_state", map[string]any{
			"sessionId": sess.id, "chatId": sess.queueKey,
			"runId": startedRunID, "status": "running",
		})
	}
	eventScope := func() (chatID, runID string) {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.queueKey, sess.activeRunID
	}
	su := func(update map[string]any) {
		if background, _ := ctx.Value(backgroundCompletionContextKey{}).(bool); background {
			update["backgroundCompletion"] = true
		}
		chatID, runID := eventScope()
		update["chatId"] = chatID
		update["runId"] = runID
		// Only echo the validated delivery identity on its own user row.
		// Presentation reconciliation must never depend on matching user text.
		if update["sessionUpdate"] == "user_message_chunk" && requestID != "" {
			update["requestId"] = requestID
		}
		sess.broadcast("session_update", map[string]any{
			"sessionId": sess.id,
			"update":    update,
		})
	}
	chunk := func(kind, text string) {
		su(map[string]any{
			"sessionUpdate": kind,
			"content":       map[string]any{"type": "text", "text": text},
		})
	}
	_, thoughtRunID := eventScope()
	thought := newThoughtReceipt(thoughtRunID, func(snapshot session.DisplayThought) error {
		if sess.chat == nil {
			return nil
		}
		return sess.chat.Thought(snapshot)
	})
	var thoughtWriteErr error
	flushThought := func(state string) {
		if err := thought.flush(state); err != nil {
			thoughtWriteErr = err
			sess.failStableCheckpoint(err)
		}
	}
	defer func() {
		flushThought(thoughtTerminalState(stopReason))
		if thoughtWriteErr != nil {
			stopReason = "error"
		}
	}()

	callIDs := map[string]string{} // tool name -> last ACP toolCallId
	callArgs := map[string]map[string]any{}
	latestDraftIDs := map[string]string{}
	draftIDsByCall := map[string]string{}
	callDraftIDs := map[string]string{}
	activeDraftIDs := map[string]string{}
	draftBases := map[string]string{}
	draftBytes := map[string]int{}
	var callSeq int
	var draftSeq int

	// turnClock measures each turn's wall time for the per-cwd usage tally:
	// OnTurn fires once per turn, so the delta since the last fire (or the
	// run start) is that turn's elapsed time.
	turnClock := time.Now()
	var lastTurnUsage *provider.Usage
	// Runtime transcripts may inject/replace system messages even when resumed
	// disk history has none. Persist outputs by their logical ordinal, not by a
	// raw transcript index that changes when non-output messages are inserted.
	persistedThrough := persistedOutputCount(sess.history[:mark])
	onCheckpoint := func(messages []provider.Message) {
		flushThought("streaming")
		if sess.checkpointPersistenceError() != nil {
			return
		}
		checkpointID := session.StableCheckpointIdentity(sess.queueKey, messages)
		if err := s.commitAppliedGuidance(sess, checkpointID); err != nil {
			sess.failStableCheckpoint(err)
			return
		}
		err := persistCheckpointOutputBoundary(&persistedThrough, sess.checkpointPersistenceError(), func(from int) (int, error) {
			return sess.persistMessages(sess.engine.ModelID, messages, from, nil)
		}, func() error { return sess.chat.StableCheckpoint(checkpointID) })
		if err != nil {
			sess.failStableCheckpoint(err)
			return
		}
		sess.mu.Lock()
		sess.stableCheckpointID = checkpointID
		sess.mu.Unlock()
		if err := sess.commitTaskPlanHistory(messages); err != nil {
			sess.failStableCheckpoint(taskPlanPersistenceError(err))
			return
		}
		sess.broadcast("stable_checkpoint", map[string]any{"sessionId": sess.id, "chatId": sess.queueKey, "checkpointId": checkpointID})
	}

	_, publicRunID := eventScope()
	publicMessages := publicMessageStream{runID: publicRunID, emit: su}
	onContent := publicMessages.content
	onMessageBoundary := publicMessages.boundary
	defer onMessageBoundary("end")
	onThought := func(delta string) {
		if delta == "" {
			return
		}
		if err := thought.append(delta); err != nil {
			thoughtWriteErr = err
			sess.failStableCheckpoint(err)
		}
		su(map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": delta},
			"thoughtId":     thought.segmentID, "thoughtSequence": thought.sequence,
			"thoughtState": "streaming", "observed": true,
		})
	}
	onProgress := func(progress provider.InferenceProgress) {
		observed := progress.PromptTokens
		estimated := observed <= 0
		if observed <= 0 {
			observed = progress.PromptEstimatedTokens
		}
		if observed <= 0 {
			observed = progress.PrefillTotalTokens
		}
		if observed > 0 {
			sess.mu.Lock()
			sess.lastPromptTokens = observed
			sess.lastPromptTokensEstimated = estimated
			sess.mu.Unlock()
		}
		sess.broadcast("inference_progress", inferenceProgressPayload(sess.id, sess.engine.ContextWindowTokens, progress))
	}
	// OnToolDraftProgress below carries both the short byte-count status and the
	// editor delta. Do not emit the parallel OnToolDraft callback as a duplicate
	// event for every provider chunk.
	onToolDraft := func(string, int) {}
	onToolDraftProgress := func(progress provider.ToolDraftProgress) {
		// This is an ephemeral editor preview only. Incomplete argument bytes are
		// never written to disk or added to conversation history.
		chatID, runID := eventScope()
		draftScope := "call:" + progress.CallID
		if progress.CallID == "" {
			draftScope = fmt.Sprintf("index:%s:%d", progress.Name, progress.Index)
		}
		draftID := activeDraftIDs[draftScope]
		if draftID == "" || progress.TotalBytes < draftBytes[draftScope] {
			draftSeq++
			draftID = fmt.Sprintf("%s:%s:draft-%d", sess.id, runID, draftSeq)
			activeDraftIDs[draftScope] = draftID
			draftBases[draftID] = draftScope
		}
		draftBytes[draftScope] = progress.TotalBytes
		latestDraftIDs[progress.Name] = draftID
		if progress.CallID != "" {
			draftIDsByCall[progress.CallID] = draftID
		}
		update := map[string]any{
			"sessionUpdate": "tool_call_draft", "title": progress.Name,
			"index": progress.Index, "bytes": progress.TotalBytes,
			"argumentsDelta": progress.ArgumentsDelta, "draftId": draftID,
			"chatId": chatID, "runId": runID,
		}
		if progress.CallID != "" {
			update["toolCallId"] = progress.CallID
		}
		su(update)
	}
	onLegacyToolCall := func(name string, args map[string]any) {
		callSeq++
		id := fmt.Sprintf("call_%d", callSeq)
		callIDs[name] = id
		callArgs[id] = args
		su(map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": id, "title": name,
			"kind": name, "status": "in_progress", "rawInput": args,
		})
	}
	onLegacyToolResult := func(name string, result tools.Result) {
		status := "completed"
		if result.Err != nil {
			status = "failed"
		}
		id := callIDs[name]
		su(map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": id,
			"title": name, "kind": name, "status": status, "rawInput": callArgs[id],
			"rawOutput": result.Display, "changedPaths": result.ChangedPaths,
		})
		if result.Err == nil {
			sess.recordPlaceResults(agentcore.ToolExecution{
				CallID: id, Tool: name, Status: agentcore.ToolOK, Executed: true,
				Places: tools.ClonePlaceCollection(result.Places),
			})
		}
	}
	onToolStart := func(start agentcore.ToolStart) {
		draftID := draftIDsByCall[start.CallID]
		if draftID == "" {
			draftID = latestDraftIDs[start.Tool]
		}
		if draftID != "" {
			callDraftIDs[start.CallID] = draftID
			if base := draftBases[draftID]; base != "" {
				delete(activeDraftIDs, base)
				delete(draftBytes, base)
			}
			delete(latestDraftIDs, start.Tool)
		}
		update := map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": start.CallID,
			"title": start.Tool, "kind": start.Tool, "status": "in_progress",
			"rawInput": start.Args,
		}
		if draftID != "" {
			update["draftId"] = draftID
		}
		su(update)
	}
	onToolComplete := func(execution agentcore.ToolExecution) {
		sess.noteTaskPlanExecution(execution)
		status := "completed"
		if execution.Status != agentcore.ToolOK {
			status = "failed"
		}
		update := map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": execution.CallID,
			"title": execution.Tool, "kind": execution.Tool, "status": status,
			"rawInput": execution.Args, "rawOutput": execution.Display, "durationMs": execution.DurationMS,
			"diff": execution.Diff, "changedPaths": execution.ChangedPaths,
			"evidenceSources": presentationEvidenceSources(execution),
		}
		if draftID := callDraftIDs[execution.CallID]; draftID != "" {
			update["draftId"] = draftID
			delete(callDraftIDs, execution.CallID)
			delete(draftIDsByCall, execution.CallID)
		}
		su(update)
		sess.recordResearchSources(execution)
		sess.recordPlaceResults(execution)
	}
	onTurn := func(turn int, usg *provider.Usage) {
		flushThought("streaming")
		// Fold this turn into the cwd lifetime tally first — count the
		// turn + its time even when the model reported no token usage,
		// so "turns"/elapsedMs stay honest (usage.go tolerates a nil).
		elapsed := time.Since(turnClock)
		turnClock = time.Now()
		usage.addTurn(sess.cwd, sess.id, usg, elapsed)

		if usg == nil {
			lastTurnUsage = nil
			return
		}
		copyUsage := *usg
		if usg.LogicalTurnUsage && sess.chat != nil {
			lastTurnUsage = nil
			if sess.checkpointPersistenceError() == nil {
				if err := sess.chat.Usage(&copyUsage, usg.CoveredAssistantCount); err != nil {
					sess.failStableCheckpoint(err)
				}
			}
		} else {
			lastTurnUsage = &copyUsage
		}
		sess.mu.Lock()
		if contextPromptTokens, ok := usageContextPromptTokens(usg); ok && contextPromptTokens > 0 {
			sess.lastPromptTokens = contextPromptTokens
			sess.lastPromptTokensEstimated = false
		}
		sess.mu.Unlock()
		payload := map[string]any{
			"sessionId":        sess.id,
			"turn":             turn,
			"promptTokens":     usg.PromptTokens,
			"completionTokens": usg.CompletionTokens,
			"totalTokens":      usg.TotalTokens,
		}
		if contextPromptTokens, ok := usageContextPromptTokens(usg); ok {
			payload["contextPromptTokens"] = contextPromptTokens
		}
		if sess.engine.SessionTokenLimit > 0 {
			payload["maxTokens"] = sess.engine.SessionTokenLimit
		}
		sess.broadcast("usage", payload)
	}
	onGuidance := func(event agentcore.GuidanceEvent) {
		s.settleGuidanceQueue(sess, event)
		chatID, runID := eventScope()
		sess.broadcast("guidance", map[string]any{
			"sessionId": sess.id, "chatId": chatID, "runId": runID, "id": event.ID, "mode": event.Mode,
			"disposition": event.Disposition, "targetTurn": event.TargetTurn,
			"queuePosition": event.QueuePosition,
		})
	}

	sess.engine.Events = engine.Events{
		OnMessageBoundary: onMessageBoundary,
		OnContent:         onContent, OnThought: onThought, OnProgress: onProgress,
		OnToolDraft: onToolDraft, OnToolDraftProgress: onToolDraftProgress,
		OnToolCall:   onLegacyToolCall,
		OnToolResult: onLegacyToolResult, OnTurn: onTurn,
		OnRouteShadow: func(res engine.RouteShadowResult) {
			if s.OnRouteShadow != nil {
				s.OnRouteShadow(sess.id, sess.engine.ModelID, sess.cwd, res)
			}
		},
	}
	sess.engine.Confirm = sess.confirmTool
	if sess.effectiveApprovalChannel() == approvalChannelNone {
		// The legacy Engine already has a truthful, structured
		// confirmation_required result for callers without a confirmer.
		sess.engine.Confirm = nil
	}

	if len(userMsg.ContentParts) > 0 {
		attachments := make([]map[string]any, 0)
		for i, part := range userMsg.ContentParts {
			if part.Type != "image_url" || part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
				continue
			}
			attachments = append(attachments, map[string]any{
				"url": part.ImageURL.URL, "name": fmt.Sprintf("圖片 %d", i+1),
			})
		}
		if len(attachments) > 0 {
			su(map[string]any{
				"sessionUpdate": "user_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
				"attachments":   attachments,
			})
		} else {
			chunk("user_message_chunk", text)
		}
	} else {
		chunk("user_message_chunk", text)
	}
	sess.mu.Lock()
	sess.runReplayAfterEventID = sess.nextEventID
	sess.mu.Unlock()
	sess.engine.TwoPass = twoPass
	// Bind at the common turn boundary so legacy/unknown-family fallback also
	// requires an explicit reuse choice. Never carry it into the next prompt.
	sess.mu.Lock()
	baseRegistry := sess.engine.Registry
	sess.engine.Registry = rag.RegistryWithArtifactChoice(baseRegistry, sess.askViaEvents)
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		sess.engine.Registry = baseRegistry
		sess.mu.Unlock()
	}()
	var runtime agentcore.Runtime = sess.engine
	if s.NewRuntime != nil {
		candidate, runtimeErr := s.NewRuntime(RuntimeConfig{
			SessionID: sess.id, ChatID: sess.queueKey, CWD: sess.cwd, ModelID: sess.engine.ModelID,
			ConnectionID:        sess.connectionID,
			HarnessMode:         harnessMode,
			ContextWindowTokens: sess.engine.ContextWindowTokens,
			PromptTokenLimit:    sess.engine.SessionTokenLimit,
			SystemPrompt:        s.runtimeConversationPrompt(sess), ContextManager: sess.engine.ContextManager,
			Registry: sess.engine.Registry, DisabledTools: append([]string(nil), sess.disabledTools...), AskUser: sess.askViaEvents,
			Authorize:          sess.authorizeTool,
			RequireToolAccount: sess.engine.RequireToolAccount,
		}, RuntimeHooks{
			OnMessageBoundary: onMessageBoundary,
			OnContent:         onContent, OnThought: onThought, OnProgress: onProgress,
			OnToolDraft: onToolDraft, OnToolDraftProgress: onToolDraftProgress,
			OnToolStart:    onToolStart,
			OnToolComplete: onToolComplete, OnTurn: onTurn, OnGuidance: onGuidance,
			OnCheckpoint: onCheckpoint, OnResumePlan: recoveryPlanCallback(sess),
			OnEvo: func(data map[string]any) { sess.broadcast("evo_state", data) },
		})
		if runtimeErr != nil {
			sess.broadcast("stream_error", map[string]any{"message": runtimeErr.Error()})
			return "error"
		}
		if candidate != nil {
			runtime = candidate
		} else if harnessMode == string(harnesscore.ModeRavenCode) {
			sess.broadcast("stream_error", map[string]any{"message": "Raven-Code runtime is unavailable; no fallback runtime was started"})
			return "error"
		}
	} else if harnessMode == string(harnesscore.ModeRavenCode) {
		sess.broadcast("stream_error", map[string]any{"message": "Raven-Code runtime is unavailable; no fallback runtime was started"})
		return "error"
	}
	if err := applyInferenceControlSelection(*inferenceSelection, runtime); err != nil {
		sess.broadcast("stream_error", map[string]any{"message": err.Error()})
		return "error"
	}
	sess.mu.Lock()
	sess.activeRuntime = runtime
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		if sess.activeRuntime == runtime {
			sess.activeRuntime = nil
		}
		sess.mu.Unlock()
	}()
	sess.mu.Lock()
	researchContinuation := sess.researchState
	var recordedTaskPlan *tools.TaskPlanState
	if sess.taskPlan.PlanCallID != "" {
		recordedTaskPlan = tools.CloneTaskPlanState(&sess.taskPlan.Plan)
	}
	sess.mu.Unlock()
	res, err := runtime.RunInput(ctx, agentcore.RunInput{
		User: userMsg, History: sess.history, Mode: runMode,
		InterruptedTask:      interruptedTask,
		ResearchContinuation: researchContinuation,
		RecordedTaskPlan:     recordedTaskPlan,
	})
	recapPublished := false
	publishRunRecap := func() {
		if !recapPublished && res != nil && res.Recap != nil {
			recapPublished = true
			sess.broadcast("run_recap", map[string]any{
				"sessionId": sess.id,
				"recap":     res.Recap,
			})
		}
	}
	// Failure and Stop paths still carry factual partial/rejected delivery
	// receipts. Publish them before the terminal HTTP/SSE outcome, while
	// retaining the ordinary successful run's existing event order below.
	defer publishRunRecap()
	if res != nil {
		if pauseErr := sess.pauseQueueForRunHandoff(res.Recap); pauseErr != nil {
			sess.failStableCheckpoint(pauseErr)
		}
	}
	// Durable journal failure remains a real failure even when the run also
	// reached its time budget; cancellation must not hide lost recovery state.
	if errors.Is(err, agentcore.ErrRecoveryCheckpoint) {
		sess.preserveCancelledPrompt(userMsg, mark, res)
		onCheckpoint(sess.history)
		sess.broadcast("stream_error", map[string]any{"message": err.Error()})
		return "error"
	}
	if ctx.Err() != nil {
		sess.preserveCancelledPrompt(userMsg, mark, res)
		onCheckpoint(sess.history)
		if agentcore.RunTimeBudgetExpired(ctx) {
			return string(agentcore.StopTimeBudget)
		}
		return "cancelled"
	}
	if err != nil {
		if errors.Is(err, agentcore.ErrRunTokenBudget) {
			sess.preserveCancelledPrompt(userMsg, mark, res)
			onCheckpoint(sess.history)
			return string(agentcore.StopTokenBudget)
		}
		// A foreground Stop is an expected control-flow outcome, not a provider
		// failure.  The outer prompt handler already publishes turn_completed
		// with stopReason=cancelled.  Broadcasting stream_error here first made
		// Desktop show a red `context canceled` banner, discard editor drafts and
		// enter its model-recovery path even though the user only pressed Stop.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			sess.preserveCancelledPrompt(userMsg, mark, res)
			// Cancellation can arrive immediately after guidance was applied but
			// before the next model response. Persist that safe transcript and its
			// guidance receipt before returning, just like a normal terminal turn.
			// Otherwise the UI remains "guiding" and restart can replay the item.
			onCheckpoint(sess.history)
			return "cancelled"
		}
		// Opted-in runtimes retain observed public partial replies with their
		// presentation identity. Keep that safe transcript available on passive
		// reopen even when the provider transport failed after displaying text.
		if res != nil {
			for _, message := range res.Messages {
				if message.Role == "assistant" && message.MessageID != "" {
					// RunResult is the runtime's complete transcript. Its leading
					// system policy may differ from sess.history, so raw mark offsets
					// must not be used to insert another copy of the human message.
					sess.mu.Lock()
					sess.history = append([]provider.Message(nil), res.Messages...)
					sess.interruptedTaskPending = true
					sess.mu.Unlock()
					onCheckpoint(sess.history)
					break
				}
			}
		}
		sess.broadcast("stream_error", map[string]any{"message": err.Error()})
		if errors.Is(err, agentcore.ErrPromptContextLimit) {
			return "token_limit"
		}
		return "error"
	}
	publishRecoveryWarning(sess, res.RecoveryWarning)
	if res.Stop == agentcore.StopCancelled || res.Stop == agentcore.StopTimeBudget || res.Stop == agentcore.StopTokenBudget {
		sess.preserveCancelledPrompt(userMsg, mark, res)
	} else {
		sess.history = res.Messages
	}
	sess.mu.Lock()
	if res.ResearchState != nil {
		snapshot := *res.ResearchState
		sess.researchState = &snapshot
	} else {
		// A non-research turn retires the previous ledger. This prevents a later
		// generic "try again" from reviving evidence that belongs to an older,
		// unrelated task in the same chat.
		sess.researchState = nil
	}
	sess.mu.Unlock()
	if res.Escalation != nil {
		sess.broadcast("model_escalation", map[string]any{
			"sessionId": sess.id, "sourceModel": res.Escalation.SourceModel,
			"recommendedModel": res.Escalation.RecommendedModel,
			"reason":           res.Escalation.Reason, "resumePrompt": res.Escalation.ResumePrompt,
		})
	}
	publishRunRecap()
	// Persist the turn's assistant/tool messages (the user message was
	// logged at prompt start). Cancelled turns persist their partial work
	// too — the chat log mirrors the live history.
	persistErr := persistCheckpointOutputBoundary(&persistedThrough, sess.checkpointPersistenceError(), func(from int) (int, error) {
		return sess.persistMessages(sess.engine.ModelID, sess.history, from, lastTurnUsage)
	}, nil)
	if persistErr != nil {
		sess.failStableCheckpoint(persistErr)
	} else {
		onCheckpoint(sess.history)
	}
	if sess.checkpointPersistenceError() == nil {
		if err := sess.persistVerificationCorrections(res.Recap); err != nil {
			sess.failStableCheckpoint(err)
		}
	}
	sess.mu.Lock()
	checkpointErr := sess.stableCheckpointErr
	sess.mu.Unlock()
	if checkpointErr != nil {
		return "error"
	}

	switch res.Stop {
	case agentcore.StopCancelled:
		return "cancelled"
	case agentcore.StopTimeBudget:
		return "time_budget"
	case agentcore.StopTokenBudget:
		return "token_budget"
	case agentcore.StopMaxTurns:
		return "max_iterations"
	case agentcore.StopLoopDetected:
		return "loop_detected"
	case agentcore.StopTokenLimit:
		return "token_limit"
	case agentcore.StopEmpty:
		// No prose and no files — must NOT read as success on the desktop.
		return "empty"
	case agentcore.StopIncomplete:
		// Model claimed done but the task-named deliverable is not on disk —
		// honest "未完成", never a green ✓ 完成.
		return "incomplete"
	case agentcore.StopNeedsInput:
		return "needs_input"
	case agentcore.StopAuditInconclusive:
		return "audit_inconclusive"
	default:
		return "end_turn"
	}
}

// preserveCancelledPrompt keeps the primary user request in the live model
// history even when a provider aborts with context.Canceled before returning a
// normal transcript. The user message is already durable in JSONL; this closes
// the in-memory gap that previously made a following "continue" turn appear to
// have no task to continue.
func (sess *serveSession) preserveCancelledPrompt(userMsg provider.Message, mark int, result *agentcore.RunResult) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	// An identified public response from this run proves the runtime admitted
	// the current input and returned its complete transcript. Runtime policy
	// can prepend system messages, so the caller's raw history offset is not a
	// valid place to check for (or insert) that already-present human message.
	if result != nil && sess.activeRunID != "" {
		prefix := sess.activeRunID + ":assistant:"
		for _, message := range result.Messages {
			if message.Role == "assistant" && strings.HasPrefix(message.MessageID, prefix) {
				sess.history = append([]provider.Message(nil), result.Messages...)
				sess.interruptedTaskPending = true
				return
			}
		}
	}
	history := append([]provider.Message(nil), sess.history...)
	if result != nil && len(result.Messages) >= mark {
		history = append([]provider.Message(nil), result.Messages...)
	}
	primaryPresent := mark >= 0 && mark < len(history) && history[mark].Role == "user"
	if !primaryPresent {
		if mark >= 0 && mark < len(history) {
			history = append(history[:mark], append([]provider.Message{userMsg}, history[mark:]...)...)
		} else {
			history = append(history, userMsg)
		}
	}
	sess.history = history
	sess.interruptedTaskPending = true
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Last-Event-ID replay from the ring. Native SSE retries send the
	// header; the frontend's hard reconnects can't set headers on
	// EventSource, so they pass ?lastEventId= instead (core.js).
	var lastID int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		fmt.Sscanf(v, "%d", &lastID)
	} else if v := r.URL.Query().Get("lastEventId"); v != "" {
		fmt.Sscanf(v, "%d", &lastID)
	}

	ch := make(chan event, subscriberQueue)
	sess.mu.Lock()
	var backlog []event
	for _, ev := range sess.ring {
		if ev.ID > lastID {
			backlog = append(backlog, ev)
		}
	}
	sess.subscribers[ch] = struct{}{}
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		delete(sess.subscribers, ch)
		sess.mu.Unlock()
	}()

	writeFrame := func(ev event) bool {
		payload, err := json.Marshal(map[string]any{
			"v": 1, "type": ev.Type, "data": ev.Data, "id": ev.ID,
		})
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, payload); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	for _, ev := range backlog {
		if !writeFrame(ev) {
			return
		}
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev := <-ch:
			if !writeFrame(ev) {
				return
			}
		}
	}
}

func (s *Server) resolvePermission(sess *serveSession, requestID string, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid outcome body")
		return
	}
	decision := engine.DecisionDeny
	if body.Outcome.Outcome == "selected" {
		switch body.Outcome.OptionID {
		case "approve":
			decision = engine.DecisionApprove
		case "approve_always":
			decision = engine.DecisionApproveAlways
		}
	}
	sess.mu.Lock()
	ch, ok := sess.permissions[requestID]
	sess.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "permission already resolved")
		return
	}
	if err := resolveRestoredPermission(sess, requestID, decision); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "persist restored permission: "+err.Error())
		return
	}
	sess.mu.Lock()
	ch, ok = sess.permissions[requestID]
	if ok {
		delete(sess.permissions, requestID)
	}
	sess.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "permission already resolved")
		return
	}
	ch <- decision
	sess.broadcast("permission_resolved", map[string]any{
		"sessionId": sess.id,
		"requestId": requestID,
		"outcome":   map[string]any{"outcome": body.Outcome.Outcome, "optionId": body.Outcome.OptionID},
	})
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	s.resolvePermission(sess, r.PathValue("requestId"), w, r)
}

// handlePermissionGlobal serves the sessionless route by scanning sessions
// for the pending request id.
func (s *Server) handlePermissionGlobal(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("requestId")
	s.mu.Lock()
	var target *serveSession
	for _, sess := range s.sessions {
		sess.mu.Lock()
		_, ok := sess.permissions[requestID]
		sess.mu.Unlock()
		if ok {
			target = sess
			break
		}
	}
	s.mu.Unlock()
	if target == nil {
		writeErr(w, http.StatusNotFound, "permission already resolved")
		return
	}
	// The sessionless compatibility route shares the session mutation boundary.
	// Its enclosing admission pins the selected connection through resolution.
	if target.connectionID != s.CurrentConnectionID() {
		writeErr(w, http.StatusConflict, session.ErrConnectionMismatch.Error())
		return
	}
	s.resolvePermission(target, requestID, w, r)
}

// settleForegroundExecution closes cancellation admission before terminal disk
// persistence. An accepted manual Stop wins over a normal result, while an own
// deadline firing after runtime completion cannot rewrite that settled outcome.
// Real runtime/checkpoint failures retain priority. The same frozen reason is
// subsequently written to the durable log and emitted to clients.
func (sess *serveSession) settleForegroundExecution(ctx context.Context, stop string) string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.cancelRun = nil
	if stop != "error" && stop != "token_limit" && errors.Is(context.Cause(ctx), context.Canceled) {
		return "cancelled"
	}
	return stop
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	var body struct {
		RequestID string `json:"requestId"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid cancellation request")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid cancellation request")
		return
	}
	if body.RequestID != "" && !validPromptRequestID(body.RequestID) {
		writeErr(w, http.StatusBadRequest, "invalid prompt request ID")
		return
	}
	sess.mu.Lock()
	if err := sess.rememberPromptCancellationLocked(body.RequestID); err != nil {
		sess.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "too many retained prompt cancellations")
		return
	}
	if body.RequestID == "" {
		sess.cancelGeneration++
	}
	cancelled := body.RequestID != ""
	matchingAdmission := false
	for admission := range sess.promptAdmissions {
		if body.RequestID == "" || admission.requestID == body.RequestID {
			admission.cancelled = true
			cancelled = true
			if record := sess.promptRequests[admission.requestID]; record == nil || !record.completed {
				matchingAdmission = true
			}
		}
	}
	// Capture and settle exactly the run visible at this cancellation boundary.
	// A scoped late Stop may retire its pending request but never a successor.
	targetCurrent := body.RequestID == "" || sess.activePromptRequestID == body.RequestID
	pendingPermissions := 0
	if targetCurrent {
		pendingPermissions = sess.cancelRunCardsLocked()
		for _, item := range sess.queuedPrompts {
			if item.Status == "running" {
				item.Status = "cancelling"
				cancelled = true
			}
		}
		if sess.cancelRun != nil {
			cancelled = true
			sess.cancelRun()
		}
		if cancelled {
			sess.broadcastLocked("run_state", map[string]any{
				"sessionId": sess.id, "chatId": sess.queueKey, "runId": sess.activeRunID, "status": "cancelling",
			})
		}
	}
	// Current/session Stop closes future queue admission even between promotion
	// and context installation. An old scoped Stop must not pause a successor.
	pauseQueue := targetCurrent || matchingAdmission || sess.latestCompletedPromptOwnsIdleQueueLocked(body.RequestID)
	var pauseErr error
	if pauseQueue {
		cutoff := time.Now()
		sess.wakeCancelledBefore = &cutoff
		sess.queuePaused = true
		pauseErr = sess.persistQueueLocked()
	}
	sess.mu.Unlock()
	if pauseErr != nil {
		sess.broadcast("work_center_error", map[string]any{"sessionId": sess.id, "operation": "pause_queue", "message": pauseErr.Error()})
		writeErr(w, http.StatusInternalServerError, "run stopped but queue pause could not be persisted")
		return
	}
	if pauseQueue {
		sess.mu.Lock()
		broker := sess.workerBroker
		sess.mu.Unlock()
		if broker != nil {
			if err := broker.CancelAll(); err != nil {
				sess.broadcast("work_center_error", map[string]any{"operation": "cancel_background_workers", "message": err.Error()})
			}
		}
		sess.broadcastQueue()
	}
	writeJSON(w, map[string]any{"ok": true, "cancelling": cancelled, "permissionsCancelled": pendingPermissions})
}

func usageContextPromptTokens(usg *provider.Usage) (int, bool) {
	return usg.ContextPromptSize()
}

func sessionContextTokens(sess *serveSession) (used, max int, estimated bool) {
	sess.mu.Lock()
	used = sess.lastPromptTokens
	estimated = sess.lastPromptTokensEstimated
	history := append([]provider.Message(nil), sess.history...)
	modelID := sess.engine.ModelID
	limit := sess.engine.SessionTokenLimit
	sess.mu.Unlock()
	if used <= 0 {
		estimated = true
		used = provider.EstimateTextTokens(sess.engine.SystemPrompt)
		for _, message := range history {
			used += provider.EstimateTextTokens(provider.MessageText(message))
			for _, call := range message.ToolCalls {
				used += provider.EstimateTextTokens(call.Function.Name + " " + call.Function.Arguments)
			}
		}
	}
	max = modelmgr.ResolveRuntimeContextWindowTokens(modelID, sess.engine.ContextWindowTokens)
	if max <= 0 {
		max = limit
	}
	return used, max, estimated
}

func (s *Server) handleContextStatus(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	used, max, estimated := sessionContextTokens(sess)
	sess.mu.Lock()
	running := sess.running
	historyLen := len(sess.history)
	modelID := sess.engine.ModelID
	sess.mu.Unlock()
	writeJSON(w, map[string]any{
		"sessionId": sess.id, "chatId": sess.queueKey,
		"connectionId": sess.connectionID, "modelId": modelID,
		"workspaceCwd": sess.cwd,
		"usedTokens":   used, "contextWindowTokens": max,
		"estimated": estimated, "running": running,
		"compactable": !running && historyLen > 1,
	})
}

func (s *Server) handleCompactContext(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	if !sess.promptMu.TryLock() {
		writeErr(w, http.StatusConflict, "context cannot be compacted while a prompt is running")
		return
	}
	defer sess.promptMu.Unlock()
	before, max, _ := sessionContextTokens(sess)
	sess.mu.Lock()
	history := append([]provider.Message(nil), sess.history...)
	sess.mu.Unlock()
	if len(history) < 2 {
		writeJSON(w, map[string]any{"ok": true, "unchanged": true, "beforeTokens": before, "afterTokens": before, "contextWindowTokens": max})
		return
	}
	sess.broadcast("context_compaction", map[string]any{"sessionId": sess.id, "status": "running", "beforeTokens": before, "contextWindowTokens": max})
	// Manual checkpointing is itself a model request. Apply the same prompt
	// budget as a normal turn so a nearly-full conversation can always be
	// compacted instead of failing at the exact moment compaction is needed.
	if sess.engine.ContextManager != nil && sess.engine.SessionTokenLimit > 0 {
		const compactionPromptReserve = 3000
		compactionHistoryLimit := sess.engine.SessionTokenLimit - compactionPromptReserve
		if compactionHistoryLimit < 1 {
			compactionHistoryLimit = 1
		}
		selected, _ := sess.engine.ContextManager.
			WithHistoryTokenBounds(compactionHistoryLimit).
			Compact(history)
		if len(selected) > 0 {
			history = selected
		}
	}
	transcript := compactTranscript(history)
	maxOut := 2400
	temperature := 0.1
	result, err := sess.engine.Client.ChatStream(r.Context(), provider.ChatRequest{
		Model: sess.engine.ModelID,
		Messages: []provider.Message{
			{Role: "system", Content: "Create a faithful compact checkpoint for continuing this conversation. Preserve: user goals, explicit constraints and preferences, decisions, completed and failed actions, artifact/file paths, validation evidence, source URLs, unresolved errors, and ordered pending tasks. Remove repetition and verbose tool output. Do not add facts. Return only the checkpoint in the user's language."},
			{Role: "user", Content: transcript},
		},
		MaxTokens: &maxOut, Temperature: &temperature,
	}, &provider.StreamHooks{OnProgress: func(progress provider.InferenceProgress) {
		sess.broadcast("inference_progress", inferenceProgressPayload(sess.id, sess.engine.ContextWindowTokens, progress))
	}})
	if err != nil || result == nil || strings.TrimSpace(result.Content) == "" {
		message := "empty compaction checkpoint"
		if err != nil {
			message = err.Error()
		}
		sess.broadcast("context_compaction", map[string]any{"sessionId": sess.id, "status": "failed", "message": message})
		writeErr(w, http.StatusBadGateway, message)
		return
	}
	summary := strings.TrimSpace(result.Content)
	sess.mu.Lock()
	sess.history = []provider.Message{{Role: "system", Content: "[Meadow conversation compact]\n" + summary}}
	sess.turnMarks = nil
	sess.lastPromptTokens = provider.EstimateTextTokens(summary)
	sess.lastPromptTokensEstimated = true
	sess.mu.Unlock()
	if sess.chat != nil {
		_ = sess.chat.Compaction(summary)
	}
	after, _, _ := sessionContextTokens(sess)
	payload := map[string]any{"sessionId": sess.id, "status": "completed", "beforeTokens": before, "afterTokens": after, "contextWindowTokens": max}
	sess.broadcast("context_compaction", payload)
	writeJSON(w, payload)
}

func compactTranscript(history []provider.Message) string {
	var b strings.Builder
	for _, message := range history {
		role := strings.ToUpper(message.Role)
		text := provider.MessageText(message)
		if message.Role == "tool" && len(text) > 8000 {
			text = text[:8000] + "\n[tool output truncated for compaction]"
		}
		if strings.TrimSpace(text) != "" {
			fmt.Fprintf(&b, "\n%s:\n%s\n", role, text)
		}
		for _, call := range message.ToolCalls {
			fmt.Fprintf(&b, "\nTOOL CALL %s:\n%s\n", call.Function.Name, call.Function.Arguments)
		}
	}
	return strings.TrimSpace(b.String())
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	sess.mu.Lock()
	sess.lastSeen = time.Now()
	sess.mu.Unlock()
	writeJSON(w, map[string]any{"lastSeenAt": time.Now().UnixMilli()})
}
