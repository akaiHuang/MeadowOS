// Package deskevo binds the official Harness² coordinator to native sessions.
package deskevo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/evolve"
	"github.com/hey-meadow/meadow-code/go/core/harness"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

type Config struct {
	StateRoot, Python, Node, Worker, Upstream string
	Binding                                   harness.Binding
	Dependencies                              evolve.NativeDependencies
}
type Runtime struct {
	config      Config
	mu          sync.Mutex
	runMu       sync.Mutex
	guides      []provider.Message
	guideEvents []agent.GuidanceEvent
	thinking    provider.ThinkingMode
	effort      provider.ReasoningEffort
	active      map[string]*agent.CodexStyleRuntime
	guidance    map[string]*guidanceFanout
	guideSeq    int
}

func candidateValidationReply(id int, validationErr, cleanupErr error) map[string]any {
	err := validationErr
	if cleanupErr != nil {
		// A real cleanup failure must not be downgraded by an earlier candidate
		// rejection. Do not join its typed error into this infrastructure error.
		err = fmt.Errorf("candidate check cleanup failed: %w", cleanupErr)
	}
	response := map[string]any{"id": id, "result": map[string]any{"valid": err == nil}}
	if err != nil {
		response["error"] = err.Error()
		var rejection *evolve.CandidateValidationError
		if cleanupErr == nil && errors.As(err, &rejection) {
			response["error_kind"] = "candidate_validation_rejected"
		}
	}
	return response
}

// The native context owns admission and Stop. The coordinator receives only
// remaining whole seconds, never a new hour after snapshot/lock preparation.
func coordinatorTimeBudget(ctx context.Context, now time.Time) (total, stage int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	remaining := time.Hour
	if deadline, ok := ctx.Deadline(); ok {
		remaining = min(remaining, deadline.Sub(now))
	}
	total = int(remaining / time.Second)
	if total < 1 {
		return 0, 0, context.DeadlineExceeded
	}
	return total, min(600, total), nil
}

func New(c Config) (*Runtime, error) {
	if c.StateRoot == "" || c.Binding.CWD == "" || c.Dependencies.Client == nil {
		return nil, errors.New("Desktop Evo native binding unavailable")
	}
	for _, p := range []string{c.Python, c.Node, c.Worker, filepath.Join(c.Upstream, "VENDOR-MANIFEST.json")} {
		if !filepath.IsAbs(p) {
			return nil, errors.New("Desktop Evo requires bundled coordination assets")
		}
		if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Desktop Evo asset unavailable: %s", filepath.Base(p))
		}
	}
	return &Runtime{config: c, thinking: provider.ThinkingAuto, effort: provider.ReasoningEffortMedium}, nil
}
func (r *Runtime) ThinkingMode() provider.ThinkingMode {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.thinking
}
func (r *Runtime) ReasoningEffort() provider.ReasoningEffort {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.effort
}
func (r *Runtime) SetThinkingMode(m provider.ThinkingMode) error {
	if _, err := provider.ParseThinkingMode(string(m)); err != nil {
		return err
	}
	profile, known := provider.ProfileForClient(r.config.Dependencies.Client, r.config.Binding.ModelID)
	if m == provider.ThinkingOn && (!known || !profile.ThinkingControl) {
		return errors.New("selected connection has no thinking control")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.thinking, r.effort = provider.NormalizeEffortSelection(m, r.effort)
	return nil
}
func (r *Runtime) SetReasoningEffort(e provider.ReasoningEffort) error {
	if _, err := provider.ParseReasoningEffort(string(e)); err != nil {
		return err
	}
	profile, known := provider.ProfileForClient(r.config.Dependencies.Client, r.config.Binding.ModelID)
	if !known || !profile.ThinkingControl {
		return errors.New("selected connection has no effort control")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.effort = e
	r.thinking = provider.ThinkingModeForEffort(e)
	return nil
}
func (r *Runtime) SubmitGuidance(g agent.Guidance) (agent.GuidanceReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.active) == 0 {
		return agent.GuidanceReceipt{}, errors.New("Evo 正在比較執行結果；目前無法引導此階段，請等候下一次試跑或停止任務")
	}
	if strings.TrimSpace(g.ID) == "" {
		r.guideSeq++
		g.ID = fmt.Sprintf("evo-guidance-%d", r.guideSeq)
	}
	prior := r.guidance[g.ID]
	if prior != nil && (prior.messageIndex < 0 || prior.messageIndex >= len(r.guides) || r.guides[prior.messageIndex].Content != g.Text || !reflect.DeepEqual(r.guides[prior.messageIndex].ContentParts, append([]provider.MessageContentPart(nil), g.ContentParts...))) {
		return agent.GuidanceReceipt{}, errors.New("cannot replace the recorded content of an earlier Guide ID")
	}
	if prior != nil && (!prior.emitted || prior.outcome == agent.GuidanceApplied) {
		return prior.receipt, nil
	}
	fanout := &guidanceFanout{targets: map[string]bool{}, events: map[string]agent.GuidanceEvent{}}
	stages := make([]string, 0, len(r.active))
	for stage := range r.active {
		stages = append(stages, stage)
	}
	sort.Strings(stages)
	var lastErr error
	for _, stage := range stages {
		receipt, err := r.active[stage].SubmitGuidance(g)
		if err != nil {
			lastErr = err
			continue
		}
		fanout.targets[stage] = true
		if fanout.receipt.ID == "" {
			fanout.receipt = receipt
		}
	}
	if len(fanout.targets) > 0 {
		if r.guidance == nil {
			r.guidance = map[string]*guidanceFanout{}
		}
		r.guidance[g.ID] = fanout
		if prior != nil {
			fanout.messageIndex = prior.messageIndex
			return fanout.receipt, nil
		}
		parts := append([]provider.MessageContentPart(nil), g.ContentParts...)
		for i := range parts {
			if parts[i].ImageURL != nil {
				image := *parts[i].ImageURL
				parts[i].ImageURL = &image
			}
		}
		fanout.messageIndex = len(r.guides)
		r.guides = append(r.guides, provider.Message{Role: "user", Content: g.Text, ContentParts: parts})
		return fanout.receipt, nil
	}
	return agent.GuidanceReceipt{}, lastErr
}
func writeJSON(p string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	tmp := p + ".tmp"
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, p)
}
func sum(text string) string { v := sha256.Sum256([]byte(text)); return hex.EncodeToString(v[:]) }
func childPath(root, p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	canonical, e := filepath.EvalSymlinks(p)
	if e != nil || canonical != filepath.Clean(p) {
		return false
	}
	rel, e := filepath.Rel(root, canonical)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func lockProject(ctx context.Context, p string) (func(), error) {
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	for {
		if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			f.Close()
			return nil, e
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (r *Runtime) RunInput(parent context.Context, input agent.RunInput) (result *agent.RunResult, runErr error) {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	r.mu.Lock()
	r.guides = nil
	r.guideEvents = nil
	r.active = map[string]*agent.CodexStyleRuntime{}
	r.guidance = map[string]*guidanceFanout{}
	r.guideSeq = 0
	r.mu.Unlock()
	c := r.config
	b := c.Binding
	ctx, cancel := context.WithTimeout(parent, time.Hour)
	defer cancel()
	project := filepath.Join(c.StateRoot, "evo", "projects", sum(b.CWD)[:24])
	if err := os.MkdirAll(project, 0700); err != nil {
		return nil, err
	}
	unlock, err := lockProject(ctx, filepath.Join(project, "library.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	var recovery *reflectionResume
	if recoveryID := reflectionResumeID(parent); recoveryID != "" {
		if err := ValidateReflectionResume(c.StateRoot, b.CWD, b.ChatID, b.ModelID, recoveryID); err != nil {
			return nil, err
		}
		recovery, err = inspectReflectionResume(c.StateRoot, b.CWD, b.ChatID, b.ModelID, recoveryID)
		if err != nil {
			return nil, err
		}
	}
	output, err := os.MkdirTemp(project, "task-")
	if err != nil {
		return nil, err
	}
	output, err = filepath.EvalSymlinks(output)
	if err != nil {
		return nil, err
	}
	id := filepath.Base(output)
	timelineFile, err := os.OpenFile(filepath.Join(output, "timeline.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	timeline := &taskTrace{file: timelineFile, taskID: id, sessionID: b.SessionID, model: b.ModelID}
	defer timelineFile.Close()
	timeline.record("task.started", "", nil)
	status := map[string]any{"runId": id, "sessionId": b.SessionID, "model": b.ModelID, "phase": "snapshot", "status": "running", "startedAt": time.Now().UTC(), "originalProject": b.CWD}
	if recovery != nil {
		status["recoveryOf"] = recovery.OriginalID
	}
	var publishMu sync.Mutex
	publish := func(phase, state string, extra map[string]any) error {
		publishMu.Lock()
		defer publishMu.Unlock()
		status["phase"], status["status"], status["updatedAt"] = phase, state, time.Now().UTC()
		for k, v := range extra {
			status[k] = v
		}
		timeline.record("task.status", "", map[string]any{"phase": phase, "status": state, "stage": status["stageName"]})
		if err := writeJSON(filepath.Join(output, "status.json"), status); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(project, "latest-"+sum(b.ChatID)[:16]+".json"), status); err != nil {
			return err
		}
		if c.Dependencies.Events.OnEvo != nil {
			copy := map[string]any{}
			for k, v := range status {
				copy[k] = v
			}
			c.Dependencies.Events.OnEvo(copy)
		}
		return nil
	}
	if err = publish("snapshot", "running", nil); err != nil {
		return nil, err
	}
	defer func() {
		r.mu.Lock()
		r.active = nil
		r.mu.Unlock()
		if runErr != nil {
			if ctx.Err() != nil {
				runErr = ctx.Err()
			}
			state := "failed"
			if ctx.Err() != nil {
				state = "interrupted"
			}
			extra := map[string]any{"error": runErr.Error()}
			if errors.Is(runErr, agent.ErrRunTokenBudget) {
				extra["stopReason"] = string(agent.StopTokenBudget)
			}
			publish("ended", state, extra)
		}
		fields := map[string]any{"status": status["status"], "phase": status["phase"], "method_completed": status["methodCompleted"], "library_entries": status["libraryEntries"], "model_calls": status["modelCalls"]}
		if runErr != nil {
			fields["error"] = runErr.Error()
		}
		if status["stopReason"] != nil {
			fields["stop_reason"] = status["stopReason"]
		}
		timeline.record("task.ended", "", fields)
	}()
	source := filepath.Join(output, "source")
	snapshotSource := b.CWD
	if recovery != nil {
		snapshotSource = filepath.Join(recovery.Run, "source")
	}
	manifest, err := snapshot(snapshotSource, source)
	if err != nil {
		return nil, err
	}
	if err = writeJSON(filepath.Join(output, "snapshot.json"), manifest); err != nil {
		return nil, err
	}
	if err = publish("coordinator", "running", map[string]any{"snapshotFiles": manifest.Included, "omittedFiles": len(manifest.Omitted)}); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(output, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	cmd := exec.CommandContext(workerCtx, c.Python, "-I", "-S", "-B", "-c", "import runpy,sys;sys.path.insert(0,sys.argv[1]);runpy.run_path(sys.argv[2],run_name='__main__')", filepath.Dir(c.Worker), c.Worker)
	cmd.Dir = output
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + output, "LANG=en_US.UTF-8", "TZ=UTC0", "PYTHONDONTWRITEBYTECODE=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = log
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = time.Second
	if _, _, err = coordinatorTimeBudget(ctx, time.Now()); err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	waited := false
	defer func() {
		_ = in.Close()
		if !waited {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}()
	encoder := json.NewEncoder(in)
	// The coordinator sees the same raw user text; executor additionally retains
	// the original multipart message and authorized conversation history in Go.
	prompt := input.User.Content
	totalSeconds, stageSeconds, err := coordinatorTimeBudget(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	config := map[string]any{"upstream": c.Upstream, "output": output, "source": source, "library": filepath.Join(project, "library"), "task_id": id, "prompt": prompt, "model": b.ModelID, "contract": evolve.NativeContract(true, true), "max_calls": 64, "max_tokens": 8192, "session_seconds": stageSeconds, "total_seconds": totalSeconds}
	if recovery != nil {
		config["mode"], config["resume_run"], config["resume_selected"], config["original_task_id"] = "resume_reflection", recovery.Run, recovery.Selected, recovery.OriginalID
	}
	if err = encoder.Encode(config); err != nil {
		return nil, err
	}
	var done struct {
		Integration       bool   `json:"integration_complete"`
		Method            bool   `json:"method_completed"`
		Selected          string `json:"selected"`
		Calls             int    `json:"model_calls"`
		LibraryEntries    int    `json:"library_entries"`
		ReflectionOutcome string `json:"reflection_outcome"`
	}
	stages := 0
	totalTurns, totalTools := 0, 0
	tokenBudgetStopped := false
	var stageMu sync.Mutex
	var turnMu sync.Mutex
	type activeStage struct {
		name, mode string
		index      int
	}
	activeStages := map[string]activeStage{}
	publishStages := func(last activeStage) error {
		current := make([]activeStage, 0, len(activeStages))
		for _, stage := range activeStages {
			current = append(current, stage)
		}
		sort.Slice(current, func(i, j int) bool { return current[i].index < current[j].index })
		names := make([]string, 0, len(current))
		for _, stage := range current {
			names = append(names, stage.name)
		}
		phase, name := last.mode, last.name
		if len(current) > 0 {
			phase, name = current[0].mode, strings.Join(names, " + ")
		}
		return publish(phase, "running", map[string]any{"stage": stages, "stageName": name, "activeStages": names})
	}
	admit := func(frame coordinatorFrame) error {
		if frame.Method != "stage" {
			return nil
		}
		if recovery != nil && frame.Mode != "reflection" {
			return errors.New("reflection recovery cannot rerun execution or proposal stages")
		}
		if recovery != nil && (frame.Harness != "" || frame.Harvest != "") {
			return errors.New("reflection recovery cannot activate or harvest a harness")
		}
		if recovery == nil && frame.Mode != "executor" && frame.Mode != "improver" {
			return errors.New("native coordinator stage mode was not admitted")
		}
		if !childPath(output, frame.Root) || !childPath(frame.Root, frame.Workspace) || (frame.Harness != "" && !childPath(frame.Root, frame.Harness)) || frame.Seconds < 1 || frame.Seconds > 600 {
			return errors.New("coordinator stage capability escaped this run")
		}
		return nil
	}
	handle := func(frame coordinatorFrame) (map[string]any, error) {
		if frame.Method == "validate" {
			dir, e := os.MkdirTemp(output, "profile-check-")
			if e != nil {
				return nil, e
			}
			e = writeFiles(dir, frame.Files)
			if e == nil {
				_, e = evolve.ValidateNativeHarness(dir, true, true, c.Dependencies.AskUser != nil)
			}
			return candidateValidationReply(frame.ID, e, os.RemoveAll(dir)), nil
		}
		stage := filepath.Base(frame.Root)
		stageMu.Lock()
		stages++
		state := activeStage{name: stage, mode: frame.Mode, index: stages}
		activeStages[frame.Root] = state
		timeline.record("stage.started", stage, map[string]any{"mode": frame.Mode})
		e := publishStages(state)
		stageMu.Unlock()
		if e != nil {
			return nil, e
		}
		deps := c.Dependencies
		r.mu.Lock()
		deps.Thinking, deps.Effort = r.thinking, r.effort
		r.mu.Unlock()
		deps.Python, deps.Node = c.Python, c.Node
		deps.Observer = &stageObserver{timeline: timeline, stage: stage, next: deps.Observer}
		deps.OnObservation = func(row map[string]any) {
			kind, _ := row["kind"].(string)
			timeline.record(kind, stage, row)
			if c.Dependencies.OnObservation != nil {
				c.Dependencies.OnObservation(row)
			}
		}
		deps.OnRuntime = func(active *agent.CodexStyleRuntime) {
			if frame.Mode != "executor" {
				return
			}
			r.mu.Lock()
			if active == nil {
				delete(r.active, frame.Root)
			} else {
				r.active[frame.Root] = active
			}
			r.mu.Unlock()
		}
		// Probe prose/checkpoints remain private to their independent stages.
		deps.Events.OnContent = nil
		deps.Events.OnThought = nil
		deps.Events.OnCheckpoint = nil
		deps.Events.OnToolDraft = nil
		deps.Events.OnToolDraftProgress = nil
		deps.Events.OnToolStart = nil
		deps.Events.OnToolComplete = nil
		deps.Events.OnToolCall = nil
		deps.Events.OnToolResult = nil
		if c.Dependencies.Events.OnTurn != nil {
			deps.Events.OnTurn = func(turn int, usage *provider.Usage) {
				turnMu.Lock()
				defer turnMu.Unlock()
				c.Dependencies.Events.OnTurn(turn, usage)
			}
		}
		deps.Events.OnGuidance = func(event agent.GuidanceEvent) {
			timeline.record("stage.guidance", stage, map[string]any{"id": event.ID, "disposition": event.Disposition})
			r.stageGuidance(frame.Root, event)
		}
		stageInput := agent.RunInput{User: provider.Message{Role: "user", Content: frame.Task}, IndependentTask: true}
		if frame.Mode == "executor" {
			stageInput = input
			r.mu.Lock()
			guides := append([]provider.Message(nil), r.guides...)
			r.mu.Unlock()
			if len(guides) > 0 {
				stageInput.History = append(append(append([]provider.Message(nil), input.History...), input.User), guides[:len(guides)-1]...)
				stageInput.User = guides[len(guides)-1]
			}
			stageInput.IndependentTask = true
		}
		started := time.Now()
		nativeMode := frame.Mode
		if nativeMode == "reflection" {
			nativeMode = "improver"
		}
		native, e := evolve.RunNative(workerCtx, evolve.NativeConfig{Workspace: frame.Workspace, Harness: frame.Harness, Harvest: frame.Harvest, Mode: nativeMode, Model: b.ModelID, MaxCalls: 64, MaxTokens: 8192, ContextWindowTokens: b.ContextWindowTokens, PromptTokenLimit: b.PromptTokenLimit, Timeout: time.Duration(frame.Seconds) * time.Second}, deps, stageInput)
		report := native.Report
		if report == nil {
			report = evolve.Report{"runtime_completed": false, "calls": []any{}}
		}
		if e != nil {
			report["error"] = e.Error()
		}
		r.mu.Lock()
		report["accepted_guidance"] = append([]provider.Message(nil), r.guides...)
		r.mu.Unlock()
		if err := writeJSON(filepath.Join(frame.Root, "result.json"), report); err != nil {
			return nil, err
		}
		code := 0
		if e != nil || report["runtime_completed"] != true {
			code = 1
		}
		if err := writeJSON(filepath.Join(frame.Root, "receipt.json"), map[string]any{"exit_code": code, "mode": frame.Mode, "elapsed_s": time.Since(started).Seconds()}); err != nil {
			return nil, err
		}
		stageMu.Lock()
		if errors.Is(e, agent.ErrRunTokenBudget) || (native.Run != nil && native.Run.Stop == agent.StopTokenBudget) {
			tokenBudgetStopped = true
		}
		if native.Run != nil {
			totalTurns += native.Run.Turns
			totalTools += native.Run.ToolCalls
		}
		fields := map[string]any{"mode": frame.Mode, "elapsed_ms": time.Since(started).Milliseconds(), "runtime_completed": report["runtime_completed"], "exit_code": code}
		if e != nil {
			fields["error"] = e.Error()
		}
		timeline.record("stage.ended", stage, fields)
		delete(activeStages, frame.Root)
		publishErr := publishStages(state)
		stageMu.Unlock()
		if publishErr != nil {
			return nil, publishErr
		}
		var unavailable *evolve.RuntimeUnavailable
		if errors.As(e, &unavailable) {
			return nil, fmt.Errorf("Desktop Evo 環境啟動失敗，已停止且未進入改善階段：%w", e)
		}
		return map[string]any{"id": frame.ID, "result": report}, nil
	}
	completion, err := serveCoordinator(ctx, in, out, stopWorker, admit, handle)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(completion, &done); err != nil {
		return nil, err
	}
	_ = in.Close()
	err = cmd.Wait()
	waited = true
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("Desktop Evo worker exited: %w", err)
	}
	selectedAllowed := done.Selected == "" || childPath(output, done.Selected)
	if recovery != nil {
		selectedAllowed = done.Selected == recovery.Selected
	}
	if !selectedAllowed {
		return nil, errors.New("Desktop Evo produced no adopted execution; reports retained")
	}
	if done.Selected == "" {
		// Only a completed coordinator with no adopted execution may inherit
		// the authoritative native budget stop. Worker, protocol, validation
		// and filesystem failures retain their own errors above and below.
		usage := agent.RunTokenBudgetUsage(ctx)
		if tokenBudgetStopped || (usage != nil && usage.Exhausted) {
			return nil, fmt.Errorf("Desktop Evo produced no adopted execution; reports retained: %w", agent.ErrRunTokenBudget)
		}
		return nil, errors.New("Desktop Evo produced no adopted execution; reports retained")
	}
	data, err := os.ReadFile(filepath.Join(done.Selected, "result.json"))
	if err != nil {
		return nil, err
	}
	var adopted struct {
		Run       *agent.RunResult `json:"run"`
		Completed bool             `json:"runtime_completed"`
		Error     string           `json:"error"`
	}
	if err = json.Unmarshal(data, &adopted); err != nil {
		return nil, err
	}
	if adopted.Run == nil {
		return nil, errors.New("adopted execution has no native result")
	}
	result = adopted.Run
	if recovery != nil && (!done.Method || (done.ReflectionOutcome != "experience" && done.ReflectionOutcome != "no_entry")) {
		return nil, errors.New("reflection recovery produced no valid completed reflection")
	}
	done.Integration = done.Integration && adopted.Completed && adopted.Error == "" && result.Stop == agent.StopCompleted
	result.Turns = totalTurns
	result.ToolCalls = totalTools
	if !done.Integration {
		result.Stop = agent.StopIncomplete
	}
	// Preserve the parent conversation identity. Candidate tool histories remain
	// in receipts; only the selected raw response is appended to this chat.
	r.mu.Lock()
	guides := append([]provider.Message(nil), r.guides...)
	result.GuidanceEvents = append([]agent.GuidanceEvent(nil), r.guideEvents...)
	r.mu.Unlock()
	result.Messages = append(append([]provider.Message(nil), input.History...), input.User)
	if recovery != nil {
		// The adopted artifact and answer already belong to the original task.
		// The retry only completes reflection; never replay its assistant reply.
		result.FinalText = ""
		result.FinalResponse = nil
	} else {
		result.Messages = append(result.Messages, guides...)
		result.Messages = append(result.Messages, provider.Message{Role: "assistant", Content: result.FinalText})
	}
	if c.Dependencies.Events.OnContent != nil && result.FinalText != "" {
		c.Dependencies.Events.OnContent(result.FinalText)
	}
	state := "incomplete"
	if done.Integration {
		state = "completed"
	}
	extra := map[string]any{"resultWorkspace": filepath.Join(done.Selected, "workspace"), "methodCompleted": done.Method, "modelCalls": done.Calls, "libraryEntries": done.LibraryEntries}
	if recovery != nil {
		extra["reflectionCompleted"], extra["reflectionOutcome"] = done.Integration && done.Method, done.ReflectionOutcome
	}
	if err = publish("ended", state, extra); err != nil {
		return result, err
	}
	return result, nil
}
