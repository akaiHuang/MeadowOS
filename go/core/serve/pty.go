// PTY terminal endpoints (M5-3: desktop terminal panel). A session can
// own any number of interactive shells, each on its own pty:
//
//	POST   /session/{id}/term                    {cols,rows,cwd?} → {termId}
//	GET    /session/{id}/term/{termId}/stream    SSE: data / exit frames
//	POST   /session/{id}/term/{termId}/write     {dataB64}
//	POST   /session/{id}/term/{termId}/resize    {cols,rows}
//	DELETE /session/{id}/term/{termId}
//
// Stream frames carry base64 payloads ({"type":"data","data":b64}) so raw
// terminal control bytes can't corrupt SSE framing; process exit emits
// {"type":"exit","code":N} and ends the stream.
//
// Terminals live in a package-level registry keyed sessionId+termId —
// deliberately not a Server/serveSession field, so this file stays purely
// additive to serve.go. Terminal lifetime is decoupled from chat-session
// lifetime: a terminal may outlive (or precede) its chat session, and an
// exited terminal keeps its backlog replayable until DELETE.
package serve

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/hey-meadow/meadow-code/go/core/session"
)

const (
	termBacklogMax = 256 << 10 // replay buffer per terminal
	termChunkSize  = 32 << 10  // pty master read size
	termSubQueue   = 256       // per-subscriber chunk queue
	termMaxDim     = 4096      // cols/rows clamp (pty wants uint16)
)

// terms is the package-level terminal registry, keyed by
// termKey(sessionID, termID). Both ids are UUIDs, so "/" cannot collide.
var terms sync.Map

func termKey(sessionID, termID string) string {
	return sessionID + "/" + termID
}

// termFromPath resolves the {id}/{termId} route values to a live terminal.
func termFromPath(r *http.Request) (*ptyTerm, bool) {
	v, ok := terms.Load(termKey(r.PathValue("id"), r.PathValue("termId")))
	if !ok {
		return nil, false
	}
	return v.(*ptyTerm), true
}

// ptyTerm is one shell on a pty. A single pump goroutine owns the master
// read side: it appends to a capped backlog (replayed to late stream
// subscribers) and fans live chunks out to current subscribers.
type ptyTerm struct {
	ptmx *os.File
	cmd  *exec.Cmd

	mu      sync.Mutex
	backlog []byte
	subs    map[chan []byte]struct{}

	exitCode int           // valid once done is closed
	done     chan struct{} // closed by pump after exitCode is stored
}

// pump drains the pty master until the process side closes (EOF on Linux,
// EIO on macOS), then reaps the child and publishes its exit code.
func (t *ptyTerm) pump() {
	buf := make([]byte, termChunkSize)
	for {
		n, err := t.ptmx.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			t.mu.Lock()
			t.backlog = append(t.backlog, chunk...)
			if len(t.backlog) > termBacklogMax {
				t.backlog = t.backlog[len(t.backlog)-termBacklogMax:]
			}
			for ch := range t.subs {
				select {
				case ch <- chunk:
				default: // slow stream: drop; the backlog still has the bytes
				}
			}
			t.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	code := 0
	if err := t.cmd.Wait(); err != nil {
		code = -1 // non-exit error (e.g. wait failure)
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode() // -1 when signal-killed
		}
	}
	t.mu.Lock()
	t.exitCode = code
	t.mu.Unlock()
	close(t.done) // happens-after the exitCode store: readers gate on done
	_ = t.ptmx.Close()
}

// subscribe atomically snapshots the backlog and registers a live chunk
// channel, so the caller sees every byte exactly once (replay + live).
func (t *ptyTerm) subscribe() ([]byte, chan []byte) {
	ch := make(chan []byte, termSubQueue)
	t.mu.Lock()
	backlog := append([]byte(nil), t.backlog...)
	t.subs[ch] = struct{}{}
	t.mu.Unlock()
	return backlog, ch
}

func (t *ptyTerm) unsubscribe(ch chan []byte) {
	t.mu.Lock()
	delete(t.subs, ch)
	t.mu.Unlock()
}

// kill force-terminates the shell. Closing the master unblocks the pump,
// which then reaps the process and broadcasts the exit frame.
func (t *ptyTerm) kill() {
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = t.ptmx.Close()
}

// termDim clamps a requested terminal dimension into pty's uint16 range,
// substituting def when unset.
func termDim(n, def int) uint16 {
	if n <= 0 {
		return uint16(def)
	}
	if n > termMaxDim {
		return termMaxDim
	}
	return uint16(n)
}

// handleTermCreate spawns the user's shell ($SHELL, fallback /bin/zsh) on
// a fresh pty rooted at cwd (default: the workspace) and registers it
// under the session id. Response: {termId}.
func (s *Server) handleTermCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
		CWD  string `json:"cwd"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	cwd := body.CWD
	if cwd == "" {
		cwd = s.WorkspaceCwd
	}
	if cwd != "" {
		if !filepath.IsAbs(cwd) {
			writeErr(w, http.StatusBadRequest, "cwd must be absolute")
			return
		}
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			writeErr(w, http.StatusBadRequest, "cwd is not a directory: "+cwd)
			return
		}
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: termDim(body.Cols, 80),
		Rows: termDim(body.Rows, 24),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	term := &ptyTerm{
		ptmx: ptmx,
		cmd:  cmd,
		subs: map[chan []byte]struct{}{},
		done: make(chan struct{}),
	}
	go term.pump()

	termID := session.NewUUID()
	terms.Store(termKey(r.PathValue("id"), termID), term)
	writeJSON(w, map[string]any{"termId": termID})
}

// handleTermStream serves the terminal output as SSE. Each frame's data is
// {"type":"data","data":<base64>}; the backlog replays first, then live
// chunks. When the process exits the stream flushes queued chunks, sends
// {"type":"exit","code":N}, and ends.
func (s *Server) handleTermStream(w http.ResponseWriter, r *http.Request) {
	term, ok := termFromPath(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown terminal")
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

	backlog, ch := term.subscribe()
	defer term.unsubscribe(ch)

	writeFrame := func(v any) bool {
		payload, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	writeData := func(b []byte) bool {
		return writeFrame(map[string]any{
			"type": "data",
			"data": base64.StdEncoding.EncodeToString(b),
		})
	}
	if len(backlog) > 0 && !writeData(backlog) {
		return
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
		case b := <-ch:
			if !writeData(b) {
				return
			}
		case <-term.done:
			// The pump finished all sends before closing done, so any
			// remaining output is already queued on ch: flush it so the
			// final bytes can't lose a select race against the exit frame.
			for {
				select {
				case b := <-ch:
					if !writeData(b) {
						return
					}
					continue
				default:
				}
				break
			}
			// exitCode is safe to read unlocked: stored before close(done).
			writeFrame(map[string]any{"type": "exit", "code": term.exitCode})
			return
		}
	}
}

// handleTermWrite feeds base64-decoded bytes to the shell's stdin.
func (s *Server) handleTermWrite(w http.ResponseWriter, r *http.Request) {
	term, ok := termFromPath(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown terminal")
		return
	}
	var body struct {
		DataB64 string `json:"dataB64"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.DataB64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "dataB64 must be valid base64")
		return
	}
	select {
	case <-term.done:
		writeErr(w, http.StatusGone, "terminal exited")
		return
	default:
	}
	if _, err := term.ptmx.Write(data); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleTermResize sets the pty window size (frontend xterm.js fit hook).
func (s *Server) handleTermResize(w http.ResponseWriter, r *http.Request) {
	term, ok := termFromPath(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown terminal")
		return
	}
	var body struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Cols <= 0 || body.Rows <= 0 {
		writeErr(w, http.StatusBadRequest, "cols and rows must be positive")
		return
	}
	if err := pty.Setsize(term.ptmx, &pty.Winsize{
		Cols: termDim(body.Cols, 80),
		Rows: termDim(body.Rows, 24),
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleTermKill removes the terminal from the registry and terminates the
// shell; live streams receive the trailing exit frame from the pump.
func (s *Server) handleTermKill(w http.ResponseWriter, r *http.Request) {
	v, ok := terms.LoadAndDelete(termKey(r.PathValue("id"), r.PathValue("termId")))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown terminal")
		return
	}
	v.(*ptyTerm).kill()
	writeJSON(w, map[string]any{"ok": true})
}
