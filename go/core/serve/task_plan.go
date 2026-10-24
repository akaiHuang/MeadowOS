package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const taskPlanSnapshotLimit = 256 << 10
const taskPlanEvidenceLimit = 128

// These are presentation facts, not an item-completion verdict. Even an
// executed passing check only proves the explicitly tested tool scope.
type taskPlanEvidence struct {
	CallID          string   `json:"callId"`
	ToolName        string   `json:"toolName"`
	Status          string   `json:"status"`
	Executed        bool     `json:"executed"`
	Scope           string   `json:"scope"`
	SourceFreshness string   `json:"sourceFreshness,omitempty"`
	SourceSHA256    string   `json:"sourceSha256,omitempty"`
	SourcePath      string   `json:"sourcePath,omitempty"`
	RequirementIDs  []string `json:"requirementIds,omitempty"`
}

type taskPlanRun struct {
	Running    bool   `json:"running"`
	StopReason string `json:"stopReason"`
	RunID      string `json:"runId"`
}

type taskPlanSnapshot struct {
	OK             bool                `json:"ok"`
	SessionID      string              `json:"sessionId"`
	Plan           tools.TaskPlanState `json:"plan"`
	PlanCallID     string              `json:"planCallId"`
	Run            taskPlanRun         `json:"run"`
	Evidence       []taskPlanEvidence  `json:"evidence"`
	RestoreWarning string              `json:"restoreWarning,omitempty"`
}

type taskPlanReceipt struct {
	Evidence     taskPlanEvidence     `json:"evidence"`
	ArgumentsSHA string               `json:"argumentsSha256"`
	OutputSHA    string               `json:"outputSha256"`
	PlanState    *tools.TaskPlanState `json:"-"`
}

type taskPlanDiskSnapshot struct {
	Version       int               `json:"version"`
	CWD           string            `json:"cwd"`
	ChatID        string            `json:"chatId"`
	Snapshot      taskPlanSnapshot  `json:"snapshot"`
	PlanOutputSHA string            `json:"planOutputSha256"`
	Receipts      []taskPlanReceipt `json:"receipts"`
}

func emptyTaskPlanSnapshot(id string) taskPlanSnapshot {
	return taskPlanSnapshot{OK: true, SessionID: id, Plan: tools.TaskPlanState{Items: []tools.TodoItem{}}, Evidence: []taskPlanEvidence{}}
}

func (s *Server) handleTaskPlan(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	sess.mu.Lock()
	result := sess.taskPlanSnapshotLocked()
	sess.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, result)
}

func (sess *serveSession) taskPlanSnapshotLocked() taskPlanSnapshot {
	result := sess.taskPlan
	result.Plan = *tools.CloneTaskPlanState(&sess.taskPlan.Plan)
	result.Evidence = append([]taskPlanEvidence(nil), result.Evidence...)
	result.OK, result.SessionID = true, sess.id
	if result.Plan.Items == nil {
		result.Plan.Items = []tools.TodoItem{}
	}
	if result.Evidence == nil {
		result.Evidence = []taskPlanEvidence{}
	}
	result.Run.Running = sess.running
	if sess.activeRunID != "" {
		result.Run.RunID = sess.activeRunID
	}
	return result
}

// Keep typed lifecycle facts pending until the exact call and result are
// committed to the chat's stable boundary. Drafts, registry changes, failed
// checkpoints and a model's prose never update the authoritative read model.
func (sess *serveSession) noteTaskPlanExecution(execution agentcore.ToolExecution) {
	if strings.TrimSpace(execution.CallID) == "" {
		return
	}
	args, err := json.Marshal(execution.Args)
	if err != nil {
		return
	}
	if execution.RawArguments != "" {
		// The advertised tool may normalize wire aliases before execution.
		// Preserve the original assistant-call binding; normalized Args alone
		// cannot attest those exact raw arguments.
		args = []byte(execution.RawArguments)
	}
	// Compare JSON values, independent of whether an in-process producer used
	// typed structs/slices or the wire decoder used map/slice values.
	var canonical map[string]any
	if json.Unmarshal(args, &canonical) != nil {
		return
	}
	args, err = json.Marshal(canonical)
	if err != nil {
		return
	}
	receipt := taskPlanReceipt{Evidence: taskPlanExecutionEvidence(execution), ArgumentsSHA: taskPlanSHA(args), OutputSHA: taskPlanSHA([]byte(execution.LLMContent))}
	if execution.Status == agentcore.ToolOK && execution.Executed {
		receipt.PlanState = tools.CloneTaskPlanState(execution.TaskPlanState)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.taskPlanPending == nil {
		sess.taskPlanPending = map[string]taskPlanReceipt{}
	}
	if execution.Tool == tools.TaskPlanToolName || execution.Tool == "todo_write" {
		if execution.Status != agentcore.ToolOK || !execution.Executed || receipt.PlanState == nil {
			if sess.taskPlanRejected == nil {
				sess.taskPlanRejected = map[string]bool{}
			}
			if len(sess.taskPlanRejected) < 256 {
				sess.taskPlanRejected[execution.CallID] = true
			}
		}
	}
	if len(sess.taskPlanPending) >= 256 {
		return
	}
	if prior, exists := sess.taskPlanPending[execution.CallID]; exists && !reflect.DeepEqual(prior, receipt) {
		// Reused/conflicting IDs must not link a result to another invocation.
		delete(sess.taskPlanPending, execution.CallID)
		if sess.taskPlanRejected == nil {
			sess.taskPlanRejected = map[string]bool{}
		}
		if len(sess.taskPlanRejected) < 256 {
			sess.taskPlanRejected[execution.CallID] = true
		}
		return
	}
	sess.taskPlanPending[execution.CallID] = receipt
}

func taskPlanExecutionEvidence(execution agentcore.ToolExecution) taskPlanEvidence {
	evidence := taskPlanEvidence{CallID: execution.CallID, ToolName: execution.Tool, Status: string(execution.Status), Executed: execution.Executed, Scope: "recorded"}
	if execution.Status == agentcore.ToolOK && execution.Executed && typedBehaviorChecksPassed(execution.BehaviorEvidence) {
		evidence.Scope = "executed-checks-passed"
		evidence.SourceFreshness = "not-revalidated"
	}
	if len(execution.FileObservations) == 1 {
		observation := execution.FileObservations[0]
		evidence.SourcePath, evidence.SourceSHA256 = observation.Path, observation.SHA256
	}
	return evidence
}

func typedBehaviorChecksPassed(raw map[string]any) bool {
	if len(raw) == 0 {
		return false
	}
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > 64<<10 {
		return false
	}
	var typed struct {
		Steps []struct {
			Action string `json:"action"`
			Status string `json:"status"`
		} `json:"steps"`
	}
	if json.Unmarshal(encoded, &typed) != nil || len(typed.Steps) == 0 {
		return false
	}
	assertions := 0
	for _, step := range typed.Steps {
		if step.Status != "passed" {
			return false
		}
		switch step.Action {
		case "assert_text", "assert_dom", "assert_state":
			assertions++
		}
	}
	return assertions > 0
}

// A binding only exists for one uniquely paired assistant call followed by one
// tool result. Canonical argument encoding is compared with typed execution
// metadata; output bytes are compared exactly, without interpreting prose.
func hasTaskPlanBoundary(history []provider.Message) bool {
	for _, message := range history {
		if message.Role == "user" && message.TaskBoundary {
			return true
		}
	}
	return false
}

func taskPlanAmbiguousCallID(history []provider.Message, id string) bool {
	if id == "" {
		return false
	}
	calls, results := 0, 0
	for _, message := range history {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if call.ID == id {
					calls++
				}
			}
		}
		if message.Role == "tool" && message.ToolCallID == id {
			results++
		}
	}
	return calls > 1 || results > 1
}

func taskPlanScopedHistory(history []provider.Message) []provider.Message {
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == "user" && history[index].TaskBoundary {
			return history[index:]
		}
	}
	return history
}

func taskPlanHistoryBindings(history []provider.Message) map[string]taskPlanReceipt {
	counts, results := map[string]int{}, map[string]int{}
	for _, message := range history {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				counts[call.ID]++
			}
		}
		if message.Role == "tool" {
			results[message.ToolCallID]++
		}
	}
	pending := map[string]taskPlanReceipt{}
	bindings := map[string]taskPlanReceipt{}
	for _, message := range taskPlanScopedHistory(history) {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if call.ID == "" || counts[call.ID] != 1 || results[call.ID] != 1 {
					continue
				}
				var args map[string]any
				if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
					continue
				}
				encoded, err := json.Marshal(args)
				if err != nil {
					continue
				}
				pending[call.ID] = taskPlanReceipt{Evidence: taskPlanEvidence{CallID: call.ID, ToolName: call.Function.Name}, ArgumentsSHA: taskPlanSHA(encoded)}
			}
		}
		if message.Role == "tool" {
			if receipt, exists := pending[message.ToolCallID]; exists {
				receipt.OutputSHA = taskPlanSHA([]byte(message.Content))
				bindings[message.ToolCallID] = receipt
				delete(pending, message.ToolCallID)
			}
		}
	}
	return bindings
}

func taskPlanReceiptBound(receipt, binding taskPlanReceipt) bool {
	return receipt.Evidence.CallID != "" && receipt.Evidence.CallID == binding.Evidence.CallID && receipt.Evidence.ToolName == binding.Evidence.ToolName && receipt.ArgumentsSHA == binding.ArgumentsSHA && receipt.OutputSHA == binding.OutputSHA
}

func (sess *serveSession) commitTaskPlanHistory(history []provider.Message) error {
	// The private read model crosses the same stable boundary as public history.
	// Serialize candidates with lifecycle writes; failed persistence leaves both
	// the visible snapshot and all pending facts unchanged for a safe retry.
	sess.taskPlanPersistMu.Lock()
	defer sess.taskPlanPersistMu.Unlock()
	sess.mu.Lock()
	defer sess.mu.Unlock()
	planHistory := append([]provider.Message(nil), history...)
	for index := range planHistory {
		if planHistory[index].Role == "tool" && sess.taskPlanRejected[planHistory[index].ToolCallID] {
			planHistory[index].Content = "task plan result not committed"
		}
	}
	parsed := tools.RestoreTaskPlanHistory(planHistory)
	bindings := taskPlanHistoryBindings(history)
	previous := sess.taskPlanSnapshotLocked()
	candidate := previous
	outputSHA := sess.taskPlanOutputSHA
	receipts := append([]taskPlanReceipt(nil), sess.taskPlanReceipts...)
	// Invalid later callbacks cannot rewrite the last known successful live
	// plan. Ambiguous IDs cannot acquire a new evidence association, however;
	// a cold reader will independently reject the now-nonunique history pair.
	filteredReceipts := receipts[:0]
	for _, receipt := range receipts {
		if !taskPlanAmbiguousCallID(history, receipt.Evidence.CallID) {
			filteredReceipts = append(filteredReceipts, receipt)
		}
	}
	receipts = filteredReceipts
	if hasTaskPlanBoundary(history) {
		if _, belongs := bindings[candidate.PlanCallID]; candidate.PlanCallID != "" && !belongs {
			candidate = emptyTaskPlanSnapshot(sess.id)
			candidate.Run = previous.Run
			outputSHA = ""
		}
		filtered := receipts[:0]
		for _, receipt := range receipts {
			if binding, exists := bindings[receipt.Evidence.CallID]; exists && taskPlanReceiptBound(receipt, binding) {
				filtered = append(filtered, receipt)
			}
		}
		receipts = filtered
	}
	consumed := make(map[string]bool)
	sawPlanOutcome := parsed.Found && parsed.CallID != candidate.PlanCallID
	if parsed.Found && parsed.CallID != candidate.PlanCallID {
		// New native plans require the typed executed lifecycle fact. History
		// alone is used only by the explicitly legacy cold-restore path.
		pending, exists := sess.taskPlanPending[parsed.CallID]
		accepted := exists && !sess.taskPlanRejected[parsed.CallID] &&
			pending.Evidence.Status == string(agentcore.ToolOK) && pending.Evidence.Executed &&
			pending.PlanState != nil && taskPlanReceiptBound(pending, bindings[parsed.CallID])
		if accepted && reflect.DeepEqual(*pending.PlanState, parsed.State) {
			candidate.Plan, candidate.PlanCallID = *tools.CloneTaskPlanState(pending.PlanState), parsed.CallID
			candidate.RestoreWarning = ""
			outputSHA = parsed.OutputSHA256
		}
	}
	for id, receipt := range sess.taskPlanPending {
		if receipt.Evidence.ToolName == tools.TaskPlanToolName || receipt.Evidence.ToolName == "todo_write" {
			sawPlanOutcome = true
		}
		binding, exists := bindings[id]
		if !exists || !taskPlanReceiptBound(receipt, binding) {
			continue
		}
		receipts = appendTaskPlanReceipt(receipts, receipt)
		consumed[id] = true
	}
	// A conflicting callback may have removed its pending receipt. Keep a
	// bounded, exact history binding as a negative lifecycle fact nevertheless.
	for id := range sess.taskPlanRejected {
		if binding, exists := bindings[id]; exists {
			binding.Evidence.Status, binding.Evidence.Scope = "rejected", "recorded"
			receipts = appendTaskPlanReceipt(receipts, binding)
		}
	}
	candidate.Evidence = taskPlanVisibleEvidence(receipts)
	changed := sawPlanOutcome || !reflect.DeepEqual(previous, candidate) || !reflect.DeepEqual(receipts, sess.taskPlanReceipts)
	if changed {
		if err := sess.writeTaskPlanDiskLocked(candidate, outputSHA, receipts); err != nil {
			return err
		}
		sess.taskPlan, sess.taskPlanOutputSHA, sess.taskPlanReceipts = candidate, outputSHA, receipts
	}
	for id := range consumed {
		delete(sess.taskPlanPending, id)
	}
	if changed {
		sess.broadcastLocked("task_plan_update", map[string]any{"sessionId": sess.id, "chatId": sess.queueKey, "runId": candidate.Run.RunID, "revision": candidate.Plan.Revision, "reason": "plan"})
	}
	return nil
}

func taskPlanVisibleEvidence(receipts []taskPlanReceipt) []taskPlanEvidence {
	evidence := make([]taskPlanEvidence, 0, len(receipts))
	for _, receipt := range receipts {
		if receipt.Evidence.ToolName != tools.TaskPlanToolName && receipt.Evidence.ToolName != "todo_write" {
			evidence = append(evidence, receipt.Evidence)
		}
	}
	return evidence
}

func appendTaskPlanReceipt(receipts []taskPlanReceipt, incoming taskPlanReceipt) []taskPlanReceipt {
	for index, receipt := range receipts {
		if receipt.Evidence.CallID == incoming.Evidence.CallID {
			if !taskPlanReceiptBound(receipt, incoming) {
				return append(receipts[:index:index], receipts[index+1:]...)
			}
			return receipts
		}
	}
	receipts = append(receipts, incoming)
	if len(receipts) > taskPlanEvidenceLimit {
		receipts = receipts[len(receipts)-taskPlanEvidenceLimit:]
	}
	return receipts
}

func taskPlanSHA(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func (sess *serveSession) taskPlanDiskPath() string {
	chatID := sess.queueKey
	if chatID == "" {
		chatID = sess.id
	}
	return filepath.Join(session.PrimaryDir(), "task-plans", taskPlanSHA([]byte(sess.cwd+"\x00"+chatID))+".json")
}

func (sess *serveSession) persistTaskPlanSnapshot() error {
	sess.taskPlanPersistMu.Lock()
	defer sess.taskPlanPersistMu.Unlock()
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.writeTaskPlanDiskLocked(sess.taskPlanSnapshotLocked(), sess.taskPlanOutputSHA, sess.taskPlanReceipts)
}

// Caller holds both persistence and session locks. Candidate is never visible
// until its atomic, private write succeeds. It remains a derived presentation
// record: its plan and receipts must still bind to the public durable history.
func (sess *serveSession) writeTaskPlanDiskLocked(snapshot taskPlanSnapshot, outputSHA string, receipts []taskPlanReceipt) error {
	chatID := sess.queueKey
	if chatID == "" {
		chatID = sess.id
	}
	disk := taskPlanDiskSnapshot{Version: 1, CWD: sess.cwd, ChatID: chatID, Snapshot: snapshot, PlanOutputSHA: outputSHA, Receipts: receipts}
	encoded, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	if len(encoded) > taskPlanSnapshotLimit {
		return errors.New("task plan presentation snapshot exceeds bounded storage")
	}
	path := sess.taskPlanDiskPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".task-plan-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// Restore typed evidence only from our bounded, session-bound ledger and only
// while its exact call/result pair remains in the durable full display history.
// Legacy chats retain their recorded plan, with no invented validator receipt.
func (sess *serveSession) restoreTaskPlanSnapshot(history []provider.Message) {
	result := emptyTaskPlanSnapshot(sess.id)
	var restored []taskPlanReceipt
	outputSHA := ""
	encoded, err := readTaskPlanDisk(sess.taskPlanDiskPath())
	if errors.Is(err, os.ErrNotExist) {
		// Legacy chats had no typed ledger. Recover only recorded bookkeeping;
		// no tool execution or validator pass can be invented from its prose.
		if parsed := tools.RestoreTaskPlanHistory(history); parsed.Found {
			result.Plan, result.PlanCallID, outputSHA = parsed.State, parsed.CallID, parsed.OutputSHA256
		}
	} else if err != nil {
		result.RestoreWarning = "任務清單記錄無法讀取；未恢復未確認的計畫或驗證證據。"
	} else {
		var disk taskPlanDiskSnapshot
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&disk)
		var trailing any
		if decodeErr == nil && decoder.Decode(&trailing) != io.EOF {
			decodeErr = errors.New("trailing task plan data")
		}
		chatID := sess.queueKey
		if chatID == "" {
			chatID = sess.id
		}
		bindings := taskPlanHistoryBindings(history)
		valid := decodeErr == nil && disk.Version == 1 && disk.CWD == sess.cwd && disk.ChatID == chatID &&
			len(disk.Receipts) <= taskPlanEvidenceLimit && len(disk.Snapshot.RestoreWarning) <= 4096
		if valid && hasTaskPlanBoundary(history) && disk.Snapshot.PlanCallID != "" {
			if _, belongs := bindings[disk.Snapshot.PlanCallID]; !belongs {
				// A trusted explicit task boundary retires the old plan. It is
				// ordinary isolation, not corruption or an implicit completion.
				disk.Snapshot.Plan = result.Plan
				disk.Snapshot.PlanCallID, disk.PlanOutputSHA = "", ""
				disk.Receipts = nil
			}
		}
		if valid && disk.Snapshot.PlanCallID != "" {
			binding, exists := bindings[disk.Snapshot.PlanCallID]
			valid = exists && (binding.Evidence.ToolName == tools.TaskPlanToolName || binding.Evidence.ToolName == "todo_write") &&
				binding.OutputSHA == disk.PlanOutputSHA && validateTaskPlanDiskState(disk.Snapshot.Plan, history, disk.Snapshot.PlanCallID)
		}
		if valid && disk.Snapshot.PlanCallID == "" {
			valid = disk.PlanOutputSHA == "" && disk.Snapshot.Plan.Revision == 0 && len(disk.Snapshot.Plan.Items) == 0 && disk.Snapshot.Plan.Goal == ""
		}
		if valid {
			seen := make(map[string]bool)
			for _, receipt := range disk.Receipts {
				if seen[receipt.Evidence.CallID] || !validTaskPlanReceipt(receipt) {
					valid = false
					break
				}
				seen[receipt.Evidence.CallID] = true
				if binding, exists := bindings[receipt.Evidence.CallID]; exists && taskPlanReceiptBound(receipt, binding) {
					restored = appendTaskPlanReceipt(restored, receipt)
				}
			}
		}
		if valid {
			result.Plan, result.PlanCallID, outputSHA = *tools.CloneTaskPlanState(&disk.Snapshot.Plan), disk.Snapshot.PlanCallID, disk.PlanOutputSHA
			result.Run = disk.Snapshot.Run
			result.RestoreWarning = disk.Snapshot.RestoreWarning
			result.Run.Running = false // passive history opening never invokes a model
			result.Evidence = taskPlanVisibleEvidence(restored)
		} else {
			restored = nil
			result.RestoreWarning = "任務清單記錄與已保存的對話不一致；未恢復未確認的計畫或驗證證據。"
		}
	}
	sess.mu.Lock()
	sess.taskPlan, sess.taskPlanReceipts, sess.taskPlanOutputSHA = result, restored, outputSHA
	sess.taskPlanPending, sess.taskPlanRejected = nil, nil
	sess.mu.Unlock()
}

func readTaskPlanDisk(path string) ([]byte, error) {
	// O_NONBLOCK and opened-descriptor validation bound FIFO/device corruption
	// even if a file is replaced between path inspection and opening it.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > taskPlanSnapshotLimit {
		return nil, errors.New("invalid task plan state file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() > taskPlanSnapshotLimit || !os.SameFile(info, opened) {
		return nil, errors.New("changed task plan state file")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, taskPlanSnapshotLimit+1))
	if err != nil {
		return nil, err
	}
	if len(encoded) > taskPlanSnapshotLimit {
		return nil, errors.New("task plan state exceeds bounded storage")
	}
	return encoded, nil
}

func validateTaskPlanDiskState(state tools.TaskPlanState, history []provider.Message, callID string) bool {
	validator := tools.NewTaskPlan()
	if validator.RestorePlanState(state) != nil {
		return false
	}
	// Reconstruct the exact saved boundary, including canonical result revision.
	// Reapplying the same args onto a disk-provided revision would preserve a
	// corrupted revision and let it outrank actual committed history.
	for index, message := range history {
		if message.Role == "tool" && message.ToolCallID == callID {
			parsed := tools.RestoreTaskPlanHistory(history[:index+1])
			return parsed.Found && parsed.CallID == callID && reflect.DeepEqual(parsed.State, state)
		}
	}
	return false
}

func validTaskPlanReceipt(receipt taskPlanReceipt) bool {
	e := receipt.Evidence
	if e.CallID == "" || len(e.CallID) > 2048 || e.ToolName == "" || len(e.ToolName) > 256 || len(e.SourcePath) > 4096 || len(e.RequirementIDs) > 16 {
		return false
	}
	for _, digest := range []string{receipt.ArgumentsSHA, receipt.OutputSHA} {
		if len(digest) != 64 {
			return false
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return false
		}
	}
	if e.SourceSHA256 != "" {
		if len(e.SourceSHA256) != 64 {
			return false
		}
		if _, err := hex.DecodeString(e.SourceSHA256); err != nil {
			return false
		}
	}
	switch agentcore.ToolStatus(e.Status) {
	case agentcore.ToolOK, agentcore.ToolError, agentcore.ToolBlocked, agentcore.ToolDenied, agentcore.ToolUnknown, agentcore.ToolConfirmationRequired, agentcore.ToolCancelled:
	case "rejected":
		if e.Executed || e.Scope != "recorded" {
			return false
		}
	default:
		return false
	}
	for _, id := range e.RequirementIDs {
		if id == "" || len(id) > 2048 {
			return false
		}
	}
	if e.SourceFreshness != "" && e.SourceFreshness != "not-revalidated" {
		return false
	}
	if e.Scope != "recorded" && e.Scope != "executed-checks-passed" {
		return false
	}
	if e.Scope == "executed-checks-passed" && (e.Status != string(agentcore.ToolOK) || !e.Executed || e.SourceFreshness != "not-revalidated") {
		return false
	}
	return true
}

func (sess *serveSession) taskPlanRunEventLocked(data any) {
	payload, ok := data.(map[string]any)
	if !ok {
		return
	}
	if id, _ := payload["sessionId"].(string); id != "" && id != sess.id {
		return
	}
	status, _ := payload["status"].(string)
	if status != "running" && status != "completed" {
		return
	}
	sess.taskPlan.Run.Running = status == "running"
	sess.taskPlan.Run.RunID, _ = payload["runId"].(string)
	sess.taskPlan.Run.StopReason, _ = payload["stopReason"].(string)
	sess.broadcastLocked("task_plan_update", map[string]any{"sessionId": sess.id, "chatId": sess.queueKey, "runId": sess.taskPlan.Run.RunID, "revision": sess.taskPlan.Plan.Revision, "reason": "run"})
}

func taskPlanPersistenceError(err error) error {
	return fmt.Errorf("persist task plan presentation: %w", err)
}
