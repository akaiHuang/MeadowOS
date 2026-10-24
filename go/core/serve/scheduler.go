package serve

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

// ScheduleTask is one autonomous task: run Prompt through the agent every
// Interval, unprompted.
type ScheduleTask struct {
	Name     string `json:"name"`
	Interval string `json:"interval"` // Go duration string, e.g. "30m", "2h"
	Prompt   string `json:"prompt"`
}

// Schedule is the on-disk config (~/.meadow/schedule.json).
type Schedule struct {
	Tasks []ScheduleTask `json:"tasks"`
}

// Scheduler wakes the daemon on a fixed tick and runs due tasks from the
// config file through the normal engine path — autonomy without a user prompt
// (the OpenClaw heartbeat analogue). The reactive serve contract is untouched;
// this only ADDS unprompted runs on a dedicated, yolo-mode session.
type Scheduler struct {
	srv  *Server
	path string
	tick time.Duration
	now  func() time.Time

	mu      sync.Mutex
	lastRun map[string]time.Time
	sess    *serveSession
}

const defaultSchedulerTick = time.Minute

// NewScheduler builds a scheduler that reads path each tick. path missing is
// fine — the scheduler simply idles until the file appears.
func NewScheduler(srv *Server, path string) *Scheduler {
	return &Scheduler{
		srv:     srv,
		path:    path,
		tick:    defaultSchedulerTick,
		now:     time.Now,
		lastRun: map[string]time.Time{},
	}
}

// DefaultSchedulePath returns the schedule.json under the first ~/.meadow-chain
// dir that already has one, else the primary home dir's path.
func DefaultSchedulePath() string {
	dirs := session.HomeDirs()
	for _, d := range dirs {
		p := filepath.Join(d, "schedule.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if len(dirs) > 0 {
		return filepath.Join(dirs[0], "schedule.json")
	}
	return "schedule.json"
}

// Run ticks until ctx is cancelled. A long task run blocks the next tick (the
// ticker drops ticks while runDue is busy) — autonomy is intentionally serial.
func (sc *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(sc.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.runDue(ctx)
		}
	}
}

// loadSchedule reads and parses the config; a missing file is an empty
// schedule (not an error) so the scheduler can idle until one is created.
func loadSchedule(path string) (Schedule, error) {
	var s Schedule
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// due reports whether task should run at now, given its recorded last run. A
// task never seen runs once immediately; a malformed/zero interval is skipped
// (so a bad config can't spin the agent).
func (sc *Scheduler) due(task ScheduleTask, now time.Time) bool {
	d, err := time.ParseDuration(task.Interval)
	if err != nil || d <= 0 {
		return false
	}
	last, ok := sc.lastRun[task.Name]
	if !ok {
		return true
	}
	return now.Sub(last) >= d
}

func (sc *Scheduler) runDue(ctx context.Context) {
	sched, err := loadSchedule(sc.path) // re-read each tick so edits take effect live
	if err != nil {
		log.Printf("[scheduler] load %s: %v", sc.path, err)
		return
	}
	now := sc.now()
	for _, task := range sched.Tasks {
		if task.Name == "" || task.Prompt == "" {
			continue
		}
		sc.mu.Lock()
		ready := sc.due(task, now)
		previousRun, previouslyRan := sc.lastRun[task.Name]
		if ready {
			sc.lastRun[task.Name] = now
		}
		sc.mu.Unlock()
		if ready && !sc.runTask(ctx, task) {
			// A brief settings lease postpones admission, not the schedule's
			// next due time. Retry on the ordinary next scheduler tick.
			sc.mu.Lock()
			if previouslyRan {
				sc.lastRun[task.Name] = previousRun
			} else {
				delete(sc.lastRun, task.Name)
			}
			sc.mu.Unlock()
		}
	}
}

// runTask runs one task on the scheduler's dedicated session, mirroring the
// handlePrompt contract (FIFO promptMu, turn mark, cancel wiring) minus HTTP
// and chat persistence.
func (sc *Scheduler) runTask(ctx context.Context, task ScheduleTask) bool {
	if !sc.srv.beginConfigurationAdmission() {
		return false
	}
	defer sc.srv.endConfigurationAdmission()
	sess, err := sc.ensureSession()
	if err != nil {
		log.Printf("[scheduler] task %q: %v", task.Name, err)
		return true
	}
	if sess.connectionID != sc.srv.CurrentConnectionID() {
		// Keep the scheduler session bound to its original connection. Retry on
		// a later tick if that connection becomes selected again.
		log.Printf("[scheduler] task %q: %v", task.Name, session.ErrConnectionMismatch)
		return false
	}
	sess.promptMu.Lock()
	defer sess.promptMu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	sess.mu.Lock()
	sess.cancelRun = cancel
	sess.activeInferenceControls = snapshotSessionInferenceControlsLocked(sess)
	sess.admitHarnessModeLocked(sess.snapshotHarnessModeLocked())
	mark := len(sess.history)
	sess.turnMarks = append(sess.turnMarks, mark)
	sess.mu.Unlock()
	defer cancel()
	defer func() {
		sess.mu.Lock()
		sess.cancelRun = nil
		sess.activeRunID = ""
		sess.activeInferenceControls = nil
		sess.activeHarnessMode = nil
		sess.mu.Unlock()
	}()

	log.Printf("[scheduler] running task %q", task.Name)
	_ = sc.srv.runPrompt(runCtx, sess, task.Prompt, mark)
	return true
}

// ensureSession lazily creates the scheduler's own session (separate from any
// user session) and forces yolo mode — unattended runs have no frontend to
// answer permission prompts, so anything needing approval would otherwise be
// declined.
func (sc *Scheduler) ensureSession() (*serveSession, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.sess != nil {
		return sc.sess, nil
	}
	sess, err := sc.srv.createSession(sc.srv.WorkspaceCwd)
	if err != nil {
		return nil, err
	}
	sess.engine.Mode = engine.ModeYolo
	sc.sess = sess
	return sess, nil
}
