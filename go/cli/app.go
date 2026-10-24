package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"

	"github.com/hey-meadow/meadow-code/go/cli/theme"
	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/taskruntime"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

type uiState int

const (
	stateComposing uiState = iota
	stateRunning
	stateConfirming
	stateQuestioning
	stateMenu
)

type runningInputMode int

const (
	runningInputSteer runningInputMode = iota
	runningInputQueue
)

type queuedInput struct {
	ID   string
	Text string
}

// Options carries everything Run needs from main.
type Options struct {
	Runtime     RuntimeBinding
	Settings    *session.Settings
	Chat        *session.ChatWriter
	ProjectRoot string
	BaseURL     string
	Version     string
	// History pre-loads a resumed session (-resume flag).
	History             []provider.Message
	InitialPromptTokens int
	TaskScheduler       *taskruntime.Scheduler
	ModelChoices        []ModelChoice
}

// ModelChoice is one product model exposed by /model.
type ModelChoice struct {
	ID          string
	Description string
}

// ModelSwitchRequest tells the command entrypoint to activate another shared
// Core model and restart the model-specific Hermes runtime on the same chat.
type ModelSwitchRequest struct {
	ModelID   string
	SessionID string
}

func (e *ModelSwitchRequest) Error() string { return "switch Meadow model to " + e.ModelID }

type EditionSwitchRequest struct {
	Profile string
}

func (e *EditionSwitchRequest) Error() string { return "switch Meadow edition to " + e.Profile }

// App is the Bubble Tea model for the whole TUI.
type App struct {
	runtime     agentcore.Runtime
	binding     RuntimeBinding
	settings    *session.Settings
	chat        *session.ChatWriter
	projectRoot string
	baseURL     string
	version     string

	state    uiState
	theme    theme.Theme
	styles   theme.Styles
	md       *glamour.TermRenderer
	vp       viewport.Model
	ta       textarea.Model
	width    int
	height   int
	ready    bool
	quitting bool
	// scrollPinned is set when the user moves the transcript away from the
	// latest line. Resize and new tokens must not yank them back to the top
	// or the bottom until they press End or send another message.
	scrollPinned bool

	entries   []entry
	streaming string
	// thinking holds the in-flight thought-channel text, rendered dimmed;
	// on turn end it is preserved into history as an entryThought.
	thinking string
	// Tool draft state is transient and never persisted as an executed call.
	draftName                 string
	draftBytes                int
	draftArguments            string
	draftIndex                int
	inferenceProgress         *provider.InferenceProgress
	lastDocumentReadySourceID string
	lastLiveProgress          time.Time
	activePromptTokens        int
	history                   []provider.Message
	runMode                   agentcore.RunMode
	// attachments are normalized local images waiting for the next prompt.
	// They are intentionally kept outside history until the turn is accepted.
	attachments []provider.MessageContentPart

	eventBus         *runtimeEventBus
	eventGeneration  uint64
	cancelRun        context.CancelFunc
	eventContext     context.Context
	cancelEvents     context.CancelFunc
	pendingConfirm   *confirmRequestMsg
	pendingAsk       *askUserRequestMsg
	askQuestion      int
	askCursor        int
	askSelected      map[int]bool
	askAnswers       []string
	menu             menuState
	slashSuggestions menuState
	menuPrevTheme    string

	totalTurns       int
	totalToolCalls   int
	lastPromptTokens int
	runStart         time.Time
	persistedTo      int // history index already written to the chat log

	taskScheduler     *taskruntime.Scheduler
	taskContext       context.Context
	taskCancel        context.CancelFunc
	pendingTasks      []taskruntime.Event
	activeTaskID      string
	activeRunID       string
	activeTaskCancel  context.CancelFunc
	activeTaskDetail  string
	taskRuntimeEvents chan tea.Msg

	notificationEvents   <-chan tools.Notification
	pendingNotifications []tools.Notification
	runningInputMode     runningInputMode
	queuedInputs         []queuedInput
	guidanceSequence     uint64

	// validationPassed is UI-only observable state. It never controls the
	// agent loop; it lets the activity line distinguish active work from the
	// short period where real validation passed and the model is composing
	// its final response.
	validationPassed bool
	// recoveryPending is UI-only observable state. A failed tool result has
	// already been returned to the runtime transcript; the next model signal
	// or action clears it. It never selects a recovery strategy.
	recoveryPending bool
	recoveryTool    string

	modelChoices  []ModelChoice
	modelSwitch   *ModelSwitchRequest
	editionSwitch *EditionSwitchRequest
}

// Run starts the TUI and blocks until exit.
func Run(opts Options) error {
	th := theme.ByName(opts.Settings.Theme())
	a := &App{
		binding:          opts.Runtime,
		settings:         opts.Settings,
		chat:             opts.Chat,
		projectRoot:      opts.ProjectRoot,
		baseURL:          opts.BaseURL,
		version:          opts.Version,
		theme:            th,
		styles:           th.Styles(),
		eventBus:         newRuntimeEventBus(64),
		history:          opts.History,
		persistedTo:      len(opts.History),
		lastPromptTokens: opts.InitialPromptTokens,
		taskScheduler:    opts.TaskScheduler,
		modelChoices:     append([]ModelChoice(nil), opts.ModelChoices...),
	}
	if a.taskScheduler != nil || a.binding.WatchNotifications != nil {
		a.taskContext, a.taskCancel = context.WithCancel(context.Background())
	}
	if a.taskScheduler != nil {
		a.taskRuntimeEvents = make(chan tea.Msg, 64)
		go a.taskScheduler.Run(a.taskContext)
	}
	if a.binding.WatchNotifications != nil {
		a.notificationEvents = a.binding.WatchNotifications(a.taskContext)
	}
	// Interactive Run owns the active writer. /resume may replace it, so the
	// closure must read a.chat at exit rather than capture the initial pointer.
	defer func() {
		if a.taskCancel != nil {
			a.taskCancel()
		}
		if a.chat != nil {
			_ = a.chat.Close()
		}
	}()
	runtime, err := a.binding.newRuntime(runtimeEvents(a.eventBus))
	if err != nil {
		return err
	}
	a.runtime = runtime
	a.entries = []entry{a.bannerEntry()}
	if len(opts.History) > 0 {
		a.entries = append(a.entries, rebuildEntries(opts.History)...)
	}
	programOptions := []tea.ProgramOption{tea.WithAltScreen()}
	// Mouse reporting makes ordinary terminal drag-selection impossible.
	// Keep native text selection as the default; users who prefer wheel events
	// inside Meadow's viewport can opt in explicitly.
	if mouseTrackingEnabled() {
		programOptions = append(programOptions, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(a, programOptions...)
	_, err = p.Run()
	if err == nil && a.editionSwitch != nil {
		return a.editionSwitch
	}
	if err == nil && a.modelSwitch != nil {
		return a.modelSwitch
	}
	return err
}

func (a *App) Init() tea.Cmd {
	commands := []tea.Cmd{textarea.Blink}
	if a.taskScheduler != nil {
		commands = append(commands, waitForTaskEvent(a.taskContext, a.taskScheduler.Events()))
		commands = append(commands, waitForTaskRuntimeEvent(a.taskContext, a.taskRuntimeEvents))
	}
	if a.notificationEvents != nil {
		commands = append(commands, waitForBackgroundNotification(a.taskContext, a.notificationEvents))
	}
	return tea.Batch(commands...)
}

func (a *App) setSize(w, h int) {
	a.width, a.height = w, h
	if !a.ready {
		a.ta = textarea.New()
		a.ta.Placeholder = "輸入訊息…（/help 看指令）"
		a.ta.ShowLineNumbers = false
		a.ta.CharLimit = 0
		a.ta.SetHeight(1)
		a.ta.Focus()
		a.vp = viewport.New(w, 10)
		a.ready = true
	}
	a.ta.SetWidth(w - 4)
	a.md = newMarkdownRenderer(w-2, a.theme)
	a.layout()
}

// layout recomputes the viewport height from the fixed chrome around it.
func (a *App) layout() {
	chrome := 3 // header + separator + status line
	switch a.state {
	case stateComposing:
		chrome += a.ta.Height() + 1
		if a.slashSuggestions.kind == menuCommand {
			chrome += strings.Count(renderMenu(a.slashSuggestions, a.styles), "\n") + 1
		}
	case stateConfirming:
		if a.pendingConfirm != nil {
			chrome += strings.Count(renderConfirm(a.pendingConfirm.req, a.styles, a.width), "\n") + 2
		}
	case stateQuestioning:
		if a.pendingAsk != nil {
			chrome += strings.Count(renderAskUser(*a.pendingAsk, a.askQuestion, a.askCursor, a.askSelected, a.styles, a.width), "\n") + 2
		}
	case stateMenu:
		chrome += strings.Count(renderMenu(a.menu, a.styles), "\n") + 1
	case stateRunning:
		// The transcript stores one stable anchor, then View expands it into the
		// animated activity block. Reserve its current row count so the optional
		// large-input excerpt cannot push input/status below the terminal.
		chrome += a.ta.Height() + a.activityRowCount() - 1
	}
	h := a.height - chrome
	if h < 3 {
		h = 3
	}
	a.vp.Width = a.width
	a.vp.Height = h
	a.refreshTranscript()
}

func (a *App) refreshTranscript() {
	content := renderTranscriptWithActivity(
		a.entries, a.transcriptStreaming(), a.thinking, a.styles, a.md, a.width,
		a.state == stateRunning,
	)
	a.vp.SetContent(content)
	if !a.scrollPinned {
		a.vp.GotoBottom()
	}
}

func (a *App) pinScroll() {
	a.scrollPinned = true
}

func (a *App) followLatest() {
	a.scrollPinned = false
	if a.ready {
		a.vp.GotoBottom()
	}
}

func (a *App) canArrowScrollTranscript() bool {
	if !a.ready {
		return false
	}
	if a.state != stateComposing && a.state != stateRunning {
		return false
	}
	if a.slashSuggestions.kind == menuCommand {
		return false
	}
	return strings.TrimSpace(a.ta.Value()) == ""
}

func (a *App) scrollTranscript(direction int) {
	if !a.ready {
		return
	}
	if direction < 0 {
		a.vp.HalfPageUp()
		a.pinScroll()
		return
	}
	a.vp.HalfPageDown()
	if a.vp.AtBottom() {
		a.scrollPinned = false
	} else {
		a.pinScroll()
	}
}

func isWheelMouse(msg tea.MouseMsg) bool {
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown,
		tea.MouseButtonWheelLeft, tea.MouseButtonWheelRight:
		return true
	}
	switch msg.Type {
	case tea.MouseWheelUp, tea.MouseWheelDown:
		return true
	}
	return false
}

func (a *App) bannerEntry() entry {
	return entry{kind: entryBanner, text: a.theme.Banner()}
}

func (a *App) applyTheme(name string) {
	a.theme = theme.ByName(name)
	a.styles = a.theme.Styles()
	if a.width > 0 {
		a.md = newMarkdownRenderer(a.width-2, a.theme)
	}
	// The banner is pre-colored; recolor it with the new palette.
	for i := range a.entries {
		if a.entries[i].kind == entryBanner {
			a.entries[i].text = a.theme.Banner()
		}
	}
	a.refreshTranscript()
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.setSize(msg.Width, msg.Height)
		if !a.scrollPinned {
			a.vp.GotoBottom()
		}
		return a, nil

	case tea.KeyMsg:
		return a.handleKey(msg)

	case tea.MouseMsg:
		if a.ready && isWheelMouse(msg) {
			before := a.vp.YOffset
			var cmd tea.Cmd
			a.vp, cmd = a.vp.Update(msg)
			if a.vp.YOffset < before {
				a.pinScroll()
			} else if a.vp.AtBottom() {
				a.scrollPinned = false
			} else if a.vp.YOffset > before {
				a.pinScroll()
			}
			return a, cmd
		}
		return a, nil

	case taskEventMsg:
		nextEvent := waitForTaskEvent(a.taskContext, a.taskScheduler.Events())
		if a.state == stateMenu && a.menu.kind == menuTasks {
			a.menu = a.taskMenu()
			a.layout()
		}
		if msg.event.Type != taskruntime.EventDue {
			return a, nextEvent
		}
		if a.activeTaskID != "" {
			a.pendingTasks = append(a.pendingTasks, msg.event)
			return a, nextEvent
		}
		run := a.startScheduledTask(msg.event)
		return a, tea.Batch(nextEvent, run)

	case taskEventStreamEndedMsg:
		return a, nil

	case taskActivityMsg:
		nextEvent := waitForTaskRuntimeEvent(a.taskContext, a.taskRuntimeEvents)
		if msg.taskID == a.activeTaskID && msg.runID == a.activeRunID {
			a.activeTaskDetail = msg.text
			if a.state == stateMenu && a.menu.kind == menuTasks {
				a.menu = a.taskMenu()
				a.layout()
			}
			a.refreshTranscript()
		}
		return a, nextEvent

	case scheduledRunFinishedMsg:
		nextEvent := waitForTaskRuntimeEvent(a.taskContext, a.taskRuntimeEvents)
		command := a.finishScheduledTask(msg)
		return a, tea.Batch(nextEvent, command)

	case taskRuntimeStreamEndedMsg:
		return a, nil

	case backgroundNotificationMsg:
		nextEvent := waitForBackgroundNotification(a.taskContext, a.notificationEvents)
		if a.state != stateComposing {
			a.pendingNotifications = append(a.pendingNotifications, msg.notification)
			return a, nextEvent
		}
		run, started := a.startBackgroundWake(msg.notification)
		if !started {
			return a, nextEvent
		}
		return a, tea.Batch(nextEvent, run)

	case backgroundEventStreamEndedMsg:
		return a, nil

	case runtimeEventStreamEndedMsg:
		return a, nil

	case contentDeltaMsg:
		// Answer text arriving means the thinking phase is over — keep the
		// musing in history (dimmed) instead of discarding it.
		a.saveThinking()
		a.recoveryPending, a.recoveryTool = false, ""
		a.streaming += msg.delta
		a.lastLiveProgress = time.Now()
		a.refreshTranscript()
		return a, a.waitForRuntimeEvent()

	case thoughtDeltaMsg:
		a.recoveryPending, a.recoveryTool = false, ""
		a.thinking += msg.delta
		a.lastLiveProgress = time.Now()
		a.refreshTranscript()
		return a, a.waitForRuntimeEvent()

	case inferenceProgressMsg:
		previousRows := a.activityRowCount()
		progress := msg.progress
		a.inferenceProgress = &progress
		if progress.Phase == provider.InferenceDocumentReady &&
			strings.TrimSpace(progress.IngestionSourceID) != "" &&
			progress.IngestionSourceID != a.lastDocumentReadySourceID {
			verb := "文件索引完成"
			if progress.IngestionReused {
				verb = "已重用文件索引"
			}
			a.entries = append(a.entries, entry{
				kind: entrySystem,
				text: fmt.Sprintf("✓ %s · %s · %d chunks · 回覆可追溯到原文行號",
					verb, progress.IngestionSourceID, progress.IngestionTotal),
			})
			a.lastDocumentReadySourceID = progress.IngestionSourceID
			a.refreshTranscript()
		}
		// Keep the footer tied to Core's exact admitted prompt instead of the
		// earlier local estimate. Some OpenAI-compatible streams omit usage in
		// their final chunk, so the progress side channel is authoritative here.
		if progress.PromptTokens > 0 {
			a.lastPromptTokens = progress.PromptTokens
		}
		a.lastLiveProgress = time.Now()
		if a.activityRowCount() != previousRows {
			a.layout()
		}
		return a, a.waitForRuntimeEvent()

	case toolDraftMsg:
		a.recoveryPending, a.recoveryTool = false, ""
		a.draftName, a.draftBytes = msg.name, msg.bytes
		a.lastLiveProgress = time.Now()
		return a, a.waitForRuntimeEvent()

	case toolDraftProgressMsg:
		a.recoveryPending, a.recoveryTool = false, ""
		progress := msg.progress
		if progress.Index != a.draftIndex || (progress.Name != "" && progress.Name != a.draftName) {
			a.draftArguments = ""
		}
		a.draftIndex = progress.Index
		if progress.Name != "" {
			a.draftName = progress.Name
		}
		a.draftBytes = progress.TotalBytes
		a.draftArguments += progress.ArgumentsDelta
		a.lastLiveProgress = time.Now()
		a.refreshTranscript()
		return a, a.waitForRuntimeEvent()

	case toolStartMsg:
		// Seal model-visible narration before the tool row. This gives the
		// transcript the same ordered shape as Hermes:
		// message.delta -> tool.start -> tool.complete -> message.delta.
		a.saveThinking()
		a.flushStreaming()
		a.recoveryPending, a.recoveryTool = false, ""
		a.draftName, a.draftBytes, a.draftArguments, a.draftIndex = "", 0, "", 0
		a.lastLiveProgress = time.Now()
		a.entries = append(a.entries, entry{
			kind: entryTool, toolID: msg.start.CallID, toolName: msg.start.Tool,
			toolSummary: toolCallSummary(msg.start.Tool, msg.start.Args),
			toolStarted: time.Now(),
		})
		a.totalToolCalls++
		a.refreshTranscript()
		return a, a.waitForRuntimeEvent()

	case toolCompleteMsg:
		execution := msg.execution
		index := -1
		for i := len(a.entries) - 1; i >= 0; i-- {
			if a.entries[i].kind == entryTool && a.entries[i].toolID == execution.CallID {
				index = i
				break
			}
		}
		if index < 0 {
			// Unknown/blocked/cancelled calls can finish before execution emits a
			// start event. They still deserve a truthful, stable transcript row.
			a.entries = append(a.entries, entry{
				kind: entryTool, toolID: execution.CallID, toolName: execution.Tool,
				toolSummary: toolCallSummary(execution.Tool, execution.Args),
				toolStarted: time.Now(),
			})
			index = len(a.entries) - 1
			a.totalToolCalls++
		}
		e := &a.entries[index]
		a.lastLiveProgress = time.Now()
		e.toolDone = true
		e.toolOK = execution.Status == agentcore.ToolOK
		e.toolDurationMS = execution.DurationMS
		e.toolDetail = oneLine(a.userFacingText(toolExecutionDisplay(execution)), 120)
		e.toolDiff = execution.Diff
		if execution.Status != agentcore.ToolOK {
			a.recoveryPending = true
			a.recoveryTool = execution.Tool
		}
		if passingValidationSignal(execution) {
			a.validationPassed = true
		}
		a.refreshTranscript()
		return a, a.waitForRuntimeEvent()

	case turnDoneMsg:
		a.saveThinking()
		a.draftName, a.draftBytes, a.draftArguments, a.draftIndex = "", 0, "", 0
		a.inferenceProgress = nil
		a.lastLiveProgress = time.Now()
		a.flushStreaming()
		a.totalTurns++
		if contextPromptTokens, ok := msg.usage.ContextPromptSize(); ok && contextPromptTokens > 0 {
			a.lastPromptTokens = contextPromptTokens
		}
		return a, a.waitForRuntimeEvent()

	case guidanceEventMsg:
		queued := a.reconcileGuidanceEvent(msg.event)
		a.lastLiveProgress = time.Now()
		a.refreshTranscript()
		if queued && a.state == stateComposing {
			if command, started := a.startNextPendingWork(); started {
				return a, command
			}
		}
		return a, a.waitForRuntimeEvent()

	case toolCallMsg:
		a.flushStreaming()
		a.entries = append(a.entries, entry{
			kind: entryTool, toolName: msg.name,
			toolSummary: toolCallSummary(msg.name, msg.args),
		})
		a.totalToolCalls++
		a.refreshTranscript()
		return a, a.waitForRuntimeEvent()

	case toolResultMsg:
		a.lastLiveProgress = time.Now()
		for i := len(a.entries) - 1; i >= 0; i-- {
			if a.entries[i].kind == entryTool && !a.entries[i].toolDone {
				a.entries[i].toolDone = true
				a.entries[i].toolOK = msg.res.Err == nil
				if msg.res.Err != nil && msg.res.Display != "" {
					a.entries[i].toolSummary += "  " + oneLine(msg.res.Display, 60)
				}
				if msg.res.Err != nil {
					a.recoveryPending = true
					a.recoveryTool = msg.name
				}
				break
			}
		}
		a.refreshTranscript()
		return a, a.waitForRuntimeEvent()

	case confirmRequestMsg:
		m := msg
		a.pendingConfirm = &m
		a.state = stateConfirming
		a.layout()
		return a, a.waitForRuntimeEvent()

	case askUserRequestMsg:
		m := msg
		a.pendingAsk = &m
		a.askQuestion = 0
		a.askCursor = 0
		a.askSelected = map[int]bool{}
		a.askAnswers = nil
		a.state = stateQuestioning
		a.layout()
		return a, a.waitForRuntimeEvent()

	case tickMsg:
		if a.state == stateRunning || a.state == stateConfirming || a.state == stateQuestioning {
			return a, tick()
		}
		return a, nil

	case runFinishedMsg:
		a.saveThinking()
		a.flushStreaming()
		a.draftName, a.draftBytes, a.draftArguments, a.draftIndex = "", 0, "", 0
		a.inferenceProgress = nil
		a.activePromptTokens = 0
		a.cancelRun = nil
		if a.cancelEvents != nil {
			a.cancelEvents()
			a.cancelEvents = nil
		}
		composed := msg.result != nil && msg.result.FinalResponse != nil
		if composed {
			a.upsertFinalResponse(msg.result.FinalResponse)
		}
		if msg.err != nil && !composed {
			a.entries = append(a.entries, entry{kind: entrySystem, text: "錯誤：" + msg.err.Error()})
		}
		if msg.result != nil {
			for _, event := range msg.result.GuidanceEvents {
				a.reconcileGuidanceEvent(event)
			}
			a.history = msg.result.Messages
			a.persistNewTurns()
			if recap := formatRunRecap(msg.result.Recap); recap != "" {
				a.entries = append(a.entries, entry{kind: entrySystem, text: recap})
			}
			if !composed {
				switch msg.result.Stop {
				case engine.StopLoopDetected:
					a.entries = append(a.entries, entry{kind: entrySystem,
						text: fmt.Sprintf("偵測到迴圈（%s），已停止。", msg.result.Loop)})
				case engine.StopAuditInconclusive:
					a.entries = append(a.entries, entry{kind: entrySystem,
						text: "指定驗收已通過，但獨立需求審核未能收斂；已保留通過版本，尚未宣稱完整完成。"})
				case engine.StopMaxTurns:
					a.entries = append(a.entries, entry{kind: entrySystem, text: "達到回合上限，已停止。"})
				case engine.StopTokenLimit:
					a.entries = append(a.entries, entry{kind: entrySystem, text: "達到 session token 上限，已停止。"})
				case engine.StopCancelled:
					a.entries = append(a.entries, entry{kind: entrySystem, text: "已取消。"})
				case engine.StopEmpty:
					a.entries = append(a.entries, entry{kind: entrySystem, text: "⚠ 這次沒有任何產出，換個說法再試一次。"})
				case engine.StopIncomplete:
					a.entries = append(a.entries, entry{kind: entrySystem,
						text: "⚠ 未完成：沒能產出任務要求的檔案。重試，或記憶體足夠時改用 Meadow 26B。"})
				case engine.StopNeedsInput:
					a.entries = append(a.entries, entry{kind: entrySystem,
						text: "需要你的回答才能安全繼續；請回覆上方問題後再送出任務。"})
				}
			}
		}
		a.state = stateComposing
		a.ta.Focus()
		a.layout()
		if command, ok := a.startNextPendingWork(); ok {
			return a, tea.Batch(textarea.Blink, command)
		}
		return a, textarea.Blink
	}

	var cmd tea.Cmd
	if a.state == stateComposing && a.ready {
		a.ta, cmd = a.ta.Update(msg)
	}
	return a, cmd
}

func toolExecutionDisplay(execution agentcore.ToolExecution) string {
	if display := strings.TrimSpace(execution.Display); display != "" {
		return display
	}
	if execution.Err != nil {
		return execution.Err.Error()
	}
	if execution.Status != agentcore.ToolOK {
		return strings.TrimSpace(execution.LLMContent)
	}
	return ""
}

func formatRunRecap(recap *agentcore.RunRecap) string {
	if recap == nil || recap.ToolCalls == 0 {
		return ""
	}
	status := map[string]string{
		"completed": "完成", "partial": "部分完成", "failed": "未完成",
		"cancelled": "已取消", "needs_input": "等待你的回答",
	}[recap.Status]
	if status == "" {
		status = recap.Status
	}
	lines := []string{
		fmt.Sprintf("執行摘要 · %s", status),
		fmt.Sprintf("工具 %d 次 · 成功 %d · 失敗 %d", recap.ToolCalls, recap.SuccessfulTools, recap.FailedTools),
	}
	if len(recap.ChangedPaths) > 0 {
		lines = append(lines, "變更檔案："+strings.Join(recap.ChangedPaths, "、"))
	}
	for _, check := range recap.Checks {
		label := "未通過"
		if check.Status == "passed" {
			label = "通過"
		}
		line := fmt.Sprintf("驗證 %s · %s", check.Tool, label)
		if check.Summary != "" {
			line += " · " + check.Summary
		}
		lines = append(lines, line)
	}
	if recap.RecoveredFailures > 0 {
		lines = append(lines, fmt.Sprintf("已根據錯誤更換策略並恢復 %d 個失敗", recap.RecoveredFailures))
	}
	for _, issue := range recap.Outstanding {
		line := "尚未解決：" + issue.Tool
		if issue.Summary != "" {
			line += " · " + issue.Summary
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// saveThinking preserves the current musing into history as a dimmed
// entry — the user asked for thoughts to stay reviewable, not vanish.
func (a *App) saveThinking() {
	if strings.TrimSpace(a.thinking) == "" {
		a.thinking = ""
		return
	}
	a.entries = append(a.entries, entry{kind: entryThought, text: a.thinking})
	a.thinking = ""
	a.refreshTranscript()
}

func (a *App) flushStreaming() {
	// Deltas are already thought-filtered by the provider; this second
	// strip is a belt-and-braces pass for anything that slipped through.
	text := provider.StripThoughtChannel(a.streaming)
	a.streaming = ""
	if strings.TrimSpace(text) == "" {
		return
	}
	a.entries = append(a.entries, entry{kind: entryAssistant, text: a.userFacingText(text)})
	a.refreshTranscript()
}

func (a *App) upsertFinalResponse(response *agentcore.FinalResponse) {
	if response == nil || strings.TrimSpace(response.Text) == "" {
		return
	}
	text := strings.TrimSpace(a.userFacingText(response.Text))
	if len(a.entries) > 0 && a.entries[len(a.entries)-1].kind == entryAssistant {
		a.entries[len(a.entries)-1].text = text
	} else {
		a.entries = append(a.entries, entry{kind: entryAssistant, text: text})
	}
	a.refreshTranscript()
}

func (a *App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "pgup", "ctrl+u", "shift+up", "alt+up":
		a.scrollTranscript(-1)
		return a, nil
	case "pgdown", "ctrl+d", "shift+down", "alt+down":
		a.scrollTranscript(1)
		return a, nil
	case "up":
		if a.canArrowScrollTranscript() {
			a.scrollTranscript(-1)
			return a, nil
		}
	case "down":
		if a.canArrowScrollTranscript() {
			a.scrollTranscript(1)
			return a, nil
		}
	case "home", "ctrl+home":
		a.vp.GotoTop()
		a.pinScroll()
		return a, nil
	case "end", "ctrl+end":
		a.followLatest()
		return a, nil
	}

	switch a.state {
	case stateRunning:
		// The composer stays live mid-run. Tab switches between steering the
		// active task and queueing a later independent task.
		switch msg.String() {
		case "ctrl+c":
			if strings.TrimSpace(a.ta.Value()) != "" {
				a.ta.Reset()
				return a, nil
			}
			if a.cancelRun != nil {
				a.cancelRun()
			}
			return a, nil
		case "tab":
			if a.runningInputMode == runningInputSteer {
				a.runningInputMode = runningInputQueue
			} else {
				a.runningInputMode = runningInputSteer
			}
			a.layout()
			return a, nil
		case "enter":
			text := strings.TrimSpace(a.ta.Value())
			if text == "" {
				return a, nil
			}
			a.ta.Reset()
			a.ta.SetHeight(1)
			if strings.HasPrefix(text, "/queue") {
				a.handleQueueCommand(text)
				a.refreshTranscript()
				a.layout()
				return a, nil
			}
			if a.runningInputMode == runningInputQueue {
				a.enqueueNextInput(text)
				a.refreshTranscript()
				a.layout()
				return a, nil
			}
			guidance := agentcore.Guidance{
				ID: a.nextGuidanceID(), Text: text, Mode: agentcore.GuidanceSteerCurrent,
			}
			var receipt agentcore.GuidanceReceipt
			var err error
			if a.binding.SubmitGuidance != nil {
				receipt, err = a.binding.SubmitGuidance(guidance)
			} else if a.binding.Steer != nil {
				err = a.binding.Steer(text)
				receipt = agentcore.GuidanceReceipt{
					ID: guidance.ID, Mode: guidance.Mode,
					Disposition: agentcore.GuidanceAccepted,
				}
			} else {
				err = fmt.Errorf("目前 runtime 不支援執行中的追加指令")
			}
			if err != nil {
				// A run can finish between the key event and the runtime call.
				// Preserve the user's text explicitly as the next task rather
				// than silently leaking it into another transcript.
				a.enqueueNextInputWithID(guidance.ID, text)
				a.entries = append(a.entries, entry{
					kind: entrySystem, text: "本輪剛好已結束；這則訊息已改排入下一題。",
				})
				a.refreshTranscript()
				a.layout()
				return a, nil
			}
			a.entries = append(a.entries, entry{
				kind: entrySteer, text: text, guidanceID: receipt.ID,
				guidanceDisposition: string(receipt.Disposition),
				guidanceTargetTurn:  receipt.TargetTurn,
			})
			a.refreshTranscript()
			a.layout()
			return a, nil
		}
		var cmd tea.Cmd
		a.ta, cmd = a.ta.Update(msg)
		return a, cmd

	case stateConfirming:
		if a.pendingConfirm == nil {
			a.state = stateComposing
			return a, nil
		}
		switch msg.String() {
		case "y", "Y", "enter":
			a.pendingConfirm.resp <- engine.DecisionApprove
		case "a", "A":
			if a.pendingConfirm.req.Kind == tools.KindDestructive {
				return a, nil
			}
			a.pendingConfirm.resp <- engine.DecisionApproveAlways
		case "n", "N", "esc":
			a.pendingConfirm.resp <- engine.DecisionDeny
		default:
			return a, nil
		}
		a.pendingConfirm = nil
		a.state = stateRunning
		a.layout()
		return a, nil

	case stateQuestioning:
		return a.handleAskUserKey(msg)

	case stateMenu:
		return a.handleMenuKey(msg)
	}

	// stateComposing
	switch msg.String() {
	case "ctrl+c":
		if strings.TrimSpace(a.ta.Value()) == "" {
			a.quitting = true
			return a, tea.Quit
		}
		a.ta.Reset()
		a.slashSuggestions = menuState{}
		return a, nil
	case "ctrl+g":
		a.menu = a.taskMenu()
		a.state = stateMenu
		a.layout()
		return a, nil
	case "alt+enter", "ctrl+j":
		a.ta.InsertString("\n")
		a.ta.SetHeight(min(a.ta.LineCount(), 6))
		a.layout()
		return a, nil
	case "enter":
		text := strings.TrimSpace(a.ta.Value())
		if text == "" {
			return a, nil
		}
		if a.slashSuggestions.kind == menuCommand && len(a.slashSuggestions.items) > 0 {
			text = a.slashSuggestions.items[a.slashSuggestions.cursor].value
		}
		a.ta.Reset()
		a.ta.SetHeight(1)
		a.slashSuggestions = menuState{}
		if strings.HasPrefix(text, "/") {
			return a.runSlash(text)
		}
		return a.submit(text)
	case "up":
		if a.slashSuggestions.kind == menuCommand && a.slashSuggestions.cursor > 0 {
			a.slashSuggestions.cursor--
			a.layout()
			return a, nil
		}
	case "down":
		if a.slashSuggestions.kind == menuCommand &&
			a.slashSuggestions.cursor < len(a.slashSuggestions.items)-1 {
			a.slashSuggestions.cursor++
			a.layout()
			return a, nil
		}
	case "tab":
		if a.slashSuggestions.kind == menuCommand && len(a.slashSuggestions.items) > 0 {
			a.ta.SetValue(a.slashSuggestions.items[a.slashSuggestions.cursor].value + " ")
			a.ta.CursorEnd()
			a.slashSuggestions = menuState{}
			a.layout()
			return a, nil
		}
	case "esc":
		if a.slashSuggestions.kind == menuCommand {
			a.slashSuggestions = menuState{}
			a.layout()
			return a, nil
		}
	}

	hadSuggestions := a.slashSuggestions.kind == menuCommand
	var cmd tea.Cmd
	a.ta, cmd = a.ta.Update(msg)
	a.slashSuggestions = slashCommandSuggestions(a.ta.Value())
	if hadSuggestions || a.slashSuggestions.kind == menuCommand {
		a.layout()
	}
	if a.ta.LineCount() != a.ta.Height() {
		a.ta.SetHeight(min(max(a.ta.LineCount(), 1), 6))
		a.layout()
	}
	return a, cmd
}

func (a *App) handleMenuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if a.menu.cursor > 0 {
			a.menu.cursor--
		}
	case "down", "j":
		if a.menu.cursor < len(a.menu.items)-1 {
			a.menu.cursor++
		}
	case "esc", "ctrl+c", "ctrl+g":
		if a.menu.kind == menuTheme {
			a.applyTheme(a.menuPrevTheme)
		}
		a.state = stateComposing
		a.menu = menuState{}
		a.layout()
		return a, textarea.Blink
	case "enter":
		return a.menuSelect()
	default:
		return a, nil
	}
	if a.menu.kind == menuTheme {
		a.applyTheme(a.menu.items[a.menu.cursor].value)
	}
	a.layout()
	return a, nil
}

func (a *App) menuSelect() (tea.Model, tea.Cmd) {
	if len(a.menu.items) == 0 {
		a.state = stateComposing
		return a, nil
	}
	chosen := a.menu.items[a.menu.cursor]
	kind := a.menu.kind
	a.menu = menuState{}
	a.state = stateComposing

	switch kind {
	case menuTheme:
		a.applyTheme(chosen.value)
		if err := a.settings.SetTheme(chosen.value); err != nil {
			a.entries = append(a.entries, entry{kind: entrySystem, text: "主題已切換，但寫入 settings 失敗：" + err.Error()})
		} else {
			a.entries = append(a.entries, entry{kind: entrySystem, text: "主題切換為 " + chosen.value + "，已存入 settings.json。"})
		}
	case menuApproval:
		mode, err := engine.ParseApprovalMode(chosen.value)
		if err == nil {
			if a.binding.SetApprovalMode == nil {
				err = fmt.Errorf("目前 runtime 的 approval mode 已鎖定")
			} else {
				err = a.binding.SetApprovalMode(mode)
			}
		}
		if err == nil {
			if serr := a.settings.SetApprovalMode(chosen.value); serr != nil {
				a.entries = append(a.entries, entry{kind: entrySystem, text: "approval 已切換，但寫入 settings 失敗：" + serr.Error()})
			} else {
				a.entries = append(a.entries, entry{kind: entrySystem, text: "approval mode 切換為 " + chosen.value + "。"})
			}
		} else {
			a.entries = append(a.entries, entry{kind: entrySystem, text: "approval 無法切換：" + err.Error()})
		}
	case menuThink:
		mode, err := provider.ParseThinkingMode(chosen.value)
		if err == nil {
			if a.binding.SetThinkingMode == nil {
				err = fmt.Errorf("目前 runtime 不支援 thinking 控制")
			} else {
				err = a.binding.SetThinkingMode(mode)
			}
		}
		if err != nil {
			a.entries = append(a.entries, entry{kind: entrySystem, text: "thinking 無法切換：" + err.Error()})
		} else {
			a.entries = append(a.entries, entry{kind: entrySystem, text: thinkingModeNotice(mode, a.binding.reasoningEffort())})
		}
	case menuEffort:
		effort, err := provider.ParseReasoningEffort(chosen.value)
		if err == nil {
			if a.binding.SetReasoningEffort == nil {
				err = fmt.Errorf("目前 runtime 不支援 reasoning effort")
			} else {
				err = a.binding.SetReasoningEffort(effort)
			}
		}
		if err != nil {
			a.entries = append(a.entries, entry{kind: entrySystem, text: "effort 無法切換：" + err.Error()})
		} else {
			a.entries = append(a.entries, entry{kind: entrySystem, text: effortNotice(effort)})
		}
	case menuResume:
		msgs, err := session.LoadHistory(chosen.value)
		var resumedChat *session.ChatWriter
		if err == nil && a.chat != nil && a.chat.SessionID != chosen.sessionID {
			resumedChat, err = session.OpenChatWriter(a.projectRoot, chosen.sessionID)
		}
		if err != nil {
			a.entries = append(a.entries, entry{kind: entrySystem, text: "載入失敗：" + err.Error()})
		} else {
			if resumedChat != nil {
				previous := a.chat
				a.chat = resumedChat
				_ = previous.Close()
			}
			a.history = msgs
			if promptTokens, usageErr := session.LoadLastPromptTokens(chosen.value); usageErr == nil {
				a.lastPromptTokens = promptTokens
			} else {
				a.lastPromptTokens = 0
			}
			a.persistedTo = len(msgs)
			a.entries = rebuildEntries(msgs)
			notice := "已恢復對話（之後的訊息會接著存）。"
			if a.chat == nil {
				notice = "已恢復對話（目前停用 chat log，後續訊息不會儲存）。"
			}
			a.entries = append(a.entries, entry{kind: entrySystem, text: notice})
		}
	case menuTasks:
		// Read-only pane: Enter closes it.
	case menuModel:
		if chosen.value == "" || chosen.value == a.binding.ModelID {
			a.entries = append(a.entries, entry{kind: entrySystem, text: "目前已使用 " + a.binding.ModelID + "。"})
			break
		}
		sessionID := ""
		if a.chat != nil {
			sessionID = a.chat.SessionID
		}
		a.modelSwitch = &ModelSwitchRequest{ModelID: chosen.value, SessionID: sessionID}
		a.quitting = true
		return a, tea.Quit
	}
	a.refreshTranscript()
	a.layout()
	return a, textarea.Blink
}

func (a *App) runSlash(text string) (tea.Model, tea.Cmd) {
	a.slashSuggestions = menuState{}
	command, args, _ := strings.Cut(strings.TrimSpace(text), " ")
	if command == "/loop" {
		if !a.binding.SchedulerEnabled || a.taskScheduler == nil {
			notice := "目前 runtime 尚未啟用模型可用的 scheduler；請使用 Meadow 35B kernel。"
			a.entries = append(a.entries, entry{kind: entrySystem, text: notice})
			a.refreshTranscript()
			return a, nil
		}
		if strings.TrimSpace(args) == "" {
			notice := "用法：/loop [間隔] <任務>\n例如：/loop 5m 檢查測試，若失敗就分析原因"
			a.entries = append(a.entries, entry{kind: entrySystem, text: notice})
			a.refreshTranscript()
			return a, nil
		}
		return a.submitPrompt(text, loopScheduleInstruction(args))
	}
	menu, notice, quit := a.dispatchSlash(text)
	if quit {
		a.quitting = true
		return a, tea.Quit
	}
	if notice != "" {
		a.entries = append(a.entries, entry{kind: entrySystem, text: notice})
		a.refreshTranscript()
	}
	if menu.kind != menuNone {
		a.menu = menu
		a.menuPrevTheme = a.theme.Name
		a.state = stateMenu
	}
	a.layout()
	return a, nil
}

func (a *App) submit(text string) (tea.Model, tea.Cmd) {
	return a.submitPrompt(text, text)
}

func (a *App) nextGuidanceID() string {
	a.guidanceSequence++
	return fmt.Sprintf("ui-guidance-%d", a.guidanceSequence)
}

// reconcileGuidanceEvent is idempotent because a terminal event can arrive
// through the live callback, RunResult, or both. Cancelled guidance becomes an
// explicit independent task so accepted user text is never silently dropped.
func (a *App) reconcileGuidanceEvent(event agentcore.GuidanceEvent) bool {
	text := ""
	for i := len(a.entries) - 1; i >= 0; i-- {
		if a.entries[i].kind != entrySteer || a.entries[i].guidanceID != event.ID {
			continue
		}
		a.entries[i].guidanceDisposition = string(event.Disposition)
		a.entries[i].guidanceTargetTurn = event.TargetTurn
		text = a.entries[i].text
		break
	}
	if event.Disposition != agentcore.GuidanceCancelled || strings.TrimSpace(text) == "" {
		return false
	}
	for i := range a.entries {
		if a.entries[i].kind == entryQueued && a.entries[i].guidanceID == event.ID {
			return false
		}
	}
	a.enqueueNextInputWithID(event.ID, text)
	a.entries = append(a.entries, entry{
		kind: entrySystem, text: "本輪已在安全邊界結束；未套用的引導已改排入下一題。",
	})
	return true
}

func (a *App) enqueueNextInput(text string) {
	a.enqueueNextInputWithID(a.nextGuidanceID(), text)
}

func (a *App) enqueueNextInputWithID(id, text string) {
	item := queuedInput{ID: id, Text: strings.TrimSpace(text)}
	a.queuedInputs = append(a.queuedInputs, item)
	a.entries = append(a.entries, entry{
		kind: entryQueued, text: item.Text, guidanceID: item.ID,
		guidanceDisposition: string(agentcore.GuidanceQueued),
		queuePosition:       len(a.queuedInputs),
	})
}

func (a *App) reindexQueuedEntries() {
	positions := make(map[string]int, len(a.queuedInputs))
	for i, item := range a.queuedInputs {
		positions[item.ID] = i + 1
	}
	for i := range a.entries {
		if a.entries[i].kind != entryQueued ||
			a.entries[i].guidanceDisposition != string(agentcore.GuidanceQueued) {
			continue
		}
		a.entries[i].queuePosition = positions[a.entries[i].guidanceID]
	}
}

func (a *App) cancelQueuedInput(position int) error {
	if position < 1 || position > len(a.queuedInputs) {
		return fmt.Errorf("佇列位置必須介於 1 和 %d", len(a.queuedInputs))
	}
	item := a.queuedInputs[position-1]
	a.queuedInputs = append(a.queuedInputs[:position-1], a.queuedInputs[position:]...)
	for i := range a.entries {
		if a.entries[i].kind == entryQueued && a.entries[i].guidanceID == item.ID {
			a.entries[i].guidanceDisposition = string(agentcore.GuidanceCancelled)
			a.entries[i].queuePosition = 0
			break
		}
	}
	a.reindexQueuedEntries()
	return nil
}

func (a *App) moveQueuedInput(from, to int) error {
	size := len(a.queuedInputs)
	if from < 1 || from > size || to < 1 || to > size {
		return fmt.Errorf("來源與目標位置必須介於 1 和 %d", size)
	}
	if from == to {
		return nil
	}
	item := a.queuedInputs[from-1]
	a.queuedInputs = append(a.queuedInputs[:from-1], a.queuedInputs[from:]...)
	target := to - 1
	a.queuedInputs = append(a.queuedInputs, queuedInput{})
	copy(a.queuedInputs[target+1:], a.queuedInputs[target:])
	a.queuedInputs[target] = item
	a.reindexQueuedEntries()
	return nil
}

func (a *App) handleQueueCommand(text string) {
	fields := strings.Fields(text)
	message := ""
	switch {
	case len(fields) == 1:
		if len(a.queuedInputs) == 0 {
			message = "佇列目前是空的。"
			break
		}
		lines := []string{"目前待辦："}
		for i, item := range a.queuedInputs {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, item.Text))
		}
		message = strings.Join(lines, "\n")
	case len(fields) == 3 && fields[1] == "cancel":
		position, err := strconv.Atoi(fields[2])
		if err == nil {
			err = a.cancelQueuedInput(position)
		}
		if err != nil {
			message = "取消失敗：" + err.Error()
		} else {
			message = fmt.Sprintf("已取消佇列第 %d 題。", position)
		}
	case len(fields) == 4 && fields[1] == "move":
		from, fromErr := strconv.Atoi(fields[2])
		to, toErr := strconv.Atoi(fields[3])
		if fromErr != nil || toErr != nil {
			message = "移動失敗：位置必須是整數。"
		} else if err := a.moveQueuedInput(from, to); err != nil {
			message = "移動失敗：" + err.Error()
		} else {
			message = fmt.Sprintf("已將佇列第 %d 題移至第 %d 題。", from, to)
		}
	default:
		message = "佇列指令：/queue、/queue cancel <位置>、/queue move <來源> <目標>"
	}
	a.entries = append(a.entries, entry{kind: entrySystem, text: message})
}

func (a *App) startNextQueuedInput() (tea.Cmd, bool) {
	if a.state != stateComposing || len(a.queuedInputs) == 0 {
		return nil, false
	}
	item := a.queuedInputs[0]
	a.queuedInputs = a.queuedInputs[1:]
	for i := range a.entries {
		if a.entries[i].kind == entryQueued && a.entries[i].guidanceID == item.ID {
			a.entries[i].guidanceDisposition = string(agentcore.GuidanceStarted)
			a.entries[i].queuePosition = 0
			break
		}
	}
	a.reindexQueuedEntries()
	_, command := a.submitPromptInput(item.Text, item.Text, true)
	return command, true
}

func (a *App) submitPrompt(displayText, modelPrompt string) (tea.Model, tea.Cmd) {
	return a.submitPromptInput(displayText, modelPrompt, false)
}

func (a *App) submitPromptInput(displayText, modelPrompt string, independentTask bool) (tea.Model, tea.Cmd) {
	user := provider.Message{Role: "user", Content: modelPrompt}
	if len(a.attachments) > 0 {
		parts := make([]provider.MessageContentPart, 0, len(a.attachments)+1)
		if strings.TrimSpace(modelPrompt) != "" {
			parts = append(parts, provider.MessageContentPart{Type: "text", Text: modelPrompt})
		}
		parts = append(parts, a.attachments...)
		user.ContentParts = parts
		if strings.TrimSpace(displayText) == "" {
			displayText = fmt.Sprintf("[附加圖片 %d 張]", len(a.attachments))
		} else {
			displayText += fmt.Sprintf("\n[附加圖片 %d 張]", len(a.attachments))
		}
		a.attachments = nil
	}
	a.followLatest()
	a.entries = append(a.entries, entry{kind: entryUser, text: displayText})
	a.refreshTranscript()
	a.state = stateRunning
	a.runningInputMode = runningInputSteer
	a.runStart = time.Now()
	a.lastLiveProgress = a.runStart
	a.validationPassed = false
	a.recoveryPending, a.recoveryTool = false, ""
	a.inferenceProgress = nil
	a.activePromptTokens = provider.EstimateTextTokens(modelPrompt)

	if a.chat != nil {
		_ = a.chat.User(displayText)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancelRun = cancel
	a.eventGeneration = a.eventBus.beginRun()
	a.eventContext, a.cancelEvents = context.WithCancel(context.Background())
	a.layout()
	return a, tea.Batch(
		runSessionMessage(ctx, a.runtime, user, a.history, independentTask, a.runMode),
		a.waitForRuntimeEvent(),
		tick(),
	)
}

func (a *App) waitForRuntimeEvent() tea.Cmd {
	if a.eventContext == nil {
		return nil
	}
	return waitForRuntimeEvent(a.eventContext, a.eventBus, a.eventGeneration)
}

type tickMsg time.Time

// tick keeps the elapsed counter and spinner moving while the engine works
// — without it the screen freezes during the first long prefill.
func tick() tea.Cmd {
	return tea.Tick(activityTickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

const activityTickInterval = 120 * time.Millisecond

func mouseTrackingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_TUI_MOUSE_TRACKING"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// waitingPhrases rotate every three seconds. They are deliberately neutral:
// the spinner communicates activity while the text keeps a professional,
// conversational tone without pretending to expose hidden reasoning.
var waitingPhrases = []string{
	"[準備請求...]",
	"[模型處理中...]",
	"[等待模型訊號...]",
	"[仍在處理...]",
	"[等待可觀測進度...]",
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var waitingKaomojis = []string{
	"( •_•)",
	"( •_•)>⌐■-■",
	"(⌐■_■)",
	"( •̀ᴗ•́ )",
	"( •_•)ゞ",
}

func waitingPhrase(elapsed time.Duration) string {
	index := int(elapsed/(3*time.Second)) % len(waitingPhrases)
	return waitingPhrases[index]
}

func waitingKaomoji(ticks int) string {
	// Hold each expression long enough to read while the scan and dot
	// animations continue at their faster cadence.
	index := (ticks / 8) % len(waitingKaomojis)
	return waitingKaomojis[index]
}

func runningLabel(ticks int) string {
	frames := [...]string{"[執行中.  ]", "[執行中.. ]", "[執行中...]"}
	return frames[(ticks/3)%len(frames)]
}

func (a *App) userFacingText(text string) string {
	root := strings.TrimSpace(a.projectRoot)
	if root == "" {
		return text
	}
	root = filepath.ToSlash(filepath.Clean(root))
	// Protect already-resolved physical paths before translating model-facing
	// aliases. Without this, a root ending in "/workspace" is expanded again
	// and rendered as "/root/root/file".
	parts := strings.Split(text, root)
	const rootPlaceholder = "\x00MEADOW_PROJECT_ROOT\x00"
	for index := range parts {
		for _, alias := range []string{"/workspace", "/workdir"} {
			// Some model-generated shell commands contain //workspace. Handle
			// that form first so the display does not retain a double slash.
			parts[index] = strings.ReplaceAll(parts[index], "/"+alias+"/", rootPlaceholder+"/")
			parts[index] = strings.ReplaceAll(parts[index], "/"+alias, rootPlaceholder)
			parts[index] = strings.ReplaceAll(parts[index], alias+"/", rootPlaceholder+"/")
			parts[index] = strings.ReplaceAll(parts[index], alias, rootPlaceholder)
		}
		parts[index] = strings.ReplaceAll(parts[index], rootPlaceholder, root)
	}
	return strings.Join(parts, root)
}

func passingValidationSignal(execution agentcore.ToolExecution) bool {
	if execution.Status != agentcore.ToolOK {
		return false
	}
	if purpose, _ := execution.Args["purpose"].(string); strings.EqualFold(strings.TrimSpace(purpose), "validation") {
		return true
	}
	content := strings.ToUpper(execution.LLMContent)
	for _, marker := range []string{
		"VALIDATION_OK",
		"ALL_CHECKS_PASSED",
		"所有檢查通過",
		"全部檢查通過",
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

// persistNewTurns appends the turns produced by the last Run to the chat
// log (TS-compatible JSONL).
func (a *App) persistNewTurns() {
	if a.chat == nil {
		return
	}
	start := a.persistedTo + 1 // +1 skips the user message Chat.User already wrote
	if start > len(a.history) {
		start = len(a.history)
	}
	for _, m := range a.history[start:] {
		switch m.Role {
		case "user":
			_ = a.chat.User(m.Content)
		case "assistant":
			_ = a.chat.Assistant(a.binding.ModelID, m.Content, m.ToolCalls, nil)
		case "tool":
			name := ""
			for _, prev := range a.history {
				for _, tc := range prev.ToolCalls {
					if tc.ID == m.ToolCallID {
						name = tc.Function.Name
					}
				}
			}
			_ = a.chat.ToolResult(m.ToolCallID, name, m.Content)
		}
	}
	a.persistedTo = len(a.history)
}

// rebuildEntries reconstructs transcript entries from a loaded history.
func rebuildEntries(msgs []provider.Message) []entry {
	var out []entry
	for _, m := range msgs {
		switch m.Role {
		case "user":
			out = append(out, entry{kind: entryUser, text: m.Content})
		case "assistant":
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, entry{kind: entryAssistant, text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				out = append(out, entry{kind: entryTool, toolName: tc.Function.Name,
					toolSummary: "", toolDone: true, toolOK: true})
			}
		}
	}
	return out
}

func (a *App) View() string {
	if !a.ready || a.quitting {
		return ""
	}
	st := a.styles

	header := a.theme.Logo() + "  " + st.Dim.Render(
		fmt.Sprintf("%s · %s · %s", a.version, a.binding.ModelID, shortPath(a.projectRoot)))
	sep := st.Separator.Render(strings.Repeat("─", max(a.width-1, 10)))

	transcript := a.vp.View()
	var bottom string
	switch a.state {
	case stateConfirming:
		if a.pendingConfirm != nil {
			bottom = renderConfirm(a.pendingConfirm.req, st, a.width)
		}
	case stateQuestioning:
		if a.pendingAsk != nil {
			bottom = renderAskUser(*a.pendingAsk, a.askQuestion, a.askCursor, a.askSelected, st, a.width)
		}
	case stateMenu:
		bottom = renderMenu(a.menu, st)
	case stateRunning:
		transcript = strings.Replace(transcript, activityAnchor, a.activityLineAt(time.Now()), 1)
		mode := "[引導本輪]"
		if a.runningInputMode == runningInputQueue {
			mode = "[排入下一題]"
		}
		bottom = st.Secondary.Render(mode+" Tab 切換 · /queue 管理 ") +
			st.Prompt.Render("› ") + a.ta.View()
	default:
		if a.slashSuggestions.kind == menuCommand {
			bottom = renderMenu(a.slashSuggestions, st) + st.Prompt.Render("› ") + a.ta.View()
		} else {
			bottom = st.Prompt.Render("› ") + a.ta.View()
		}
	}

	passMode := "single"
	if a.binding.twoPass() {
		passMode = "2-pass"
	}
	contextStatus := a.contextUsageLabel()
	if a.width > 0 && a.width < 52 {
		contextStatus = formatTokenCountCompact(a.lastPromptTokens) + " tok"
	}
	status := st.StatusBar.Render(fmt.Sprintf("%s · %s · %s · %d 輪",
		a.binding.mode(), passMode, contextStatus, a.totalTurns))
	if taskStatus := a.taskStatusLabel(); taskStatus != "" {
		status += st.StatusBar.Render(" · " + taskStatus)
	}
	if queued := len(a.queuedInputs); queued > 0 {
		status += st.StatusBar.Render(fmt.Sprintf(" · 待辦 %d", queued))
	}
	if a.scrollPinned || !a.vp.AtBottom() {
		status += st.StatusBar.Render(fmt.Sprintf(" · 捲動 %d%% · End 回最新 · PgUp/PgDn", int(a.vp.ScrollPercent()*100)))
	}

	return strings.Join([]string{header, sep, transcript, bottom, status}, "\n")
}

func (a *App) activeTool() (*entry, int) {
	var latest *entry
	count := 0
	for i := range a.entries {
		e := &a.entries[i]
		if e.kind != entryTool || e.toolDone {
			continue
		}
		latest = e
		count++
	}
	return latest, count
}

func (a *App) contextUsageLabel() string {
	used := formatTokenCountCompact(a.lastPromptTokens)
	if a.binding.ContextWindowTokens <= 0 {
		return used + " tok"
	}
	return used + "/" + formatContextWindowCompact(a.binding.ContextWindowTokens) + " context"
}

func formatContextWindowCompact(value int) string {
	// Context-window product names use binary power-of-two labels familiar to
	// users (128K, 256K, 1M), while live token usage remains an exact decimal
	// compact count. The admitted 131,072-token workspace is therefore 128K.
	for _, named := range []struct {
		tokens int
		label  string
	}{
		{tokens: 1_048_576, label: "1M"},
		{tokens: 524_288, label: "512K"},
		{tokens: 262_144, label: "256K"},
		{tokens: 131_072, label: "128K"},
		{tokens: 65_536, label: "64K"},
		{tokens: 32_768, label: "32K"},
		{tokens: 16_384, label: "16K"},
	} {
		if value == named.tokens {
			return named.label
		}
	}
	return formatTokenCountCompact(value)
}

func formatTokenCountCompact(value int) string {
	if value < 0 {
		return "-" + formatTokenCountCompact(-value)
	}
	if value < 1_000 {
		return fmt.Sprintf("%d", value)
	}

	type unit struct {
		size   float64
		suffix string
	}
	for _, candidate := range []unit{
		{size: 1_000_000_000, suffix: "B"},
		{size: 1_000_000, suffix: "M"},
		{size: 1_000, suffix: "K"},
	} {
		if float64(value) < candidate.size {
			continue
		}
		scaled := float64(value) / candidate.size
		var text string
		switch {
		case scaled < 10:
			text = fmt.Sprintf("%.2f", scaled)
		case scaled < 100:
			text = fmt.Sprintf("%.1f", scaled)
		default:
			text = fmt.Sprintf("%.0f", scaled)
		}
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
		return text + candidate.suffix
	}
	return fmt.Sprintf("%d", value)
}
