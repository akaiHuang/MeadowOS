package serve

import (
	"github.com/hey-meadow/meadow-code/go/core/deskevo"
	"net/http"
	"os"
)

func (s *Server) handleEvoStatus(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, 404, "unknown session")
		return
	}
	sess.mu.Lock()
	cwd, chat, running, model := sess.cwd, sess.queueKey, sess.running, sess.engine.ModelID
	sess.mu.Unlock()
	status, err := deskevo.Latest(os.Getenv("MEADOW_HOME"), cwd, chat)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(status) > 0 {
		deskevo.AnnotateReflectionRecovery(os.Getenv("MEADOW_HOME"), cwd, chat, model, status)
		status["sessionId"] = sess.id
		if status["status"] == "running" && !running {
			status["status"] = "interrupted"
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status)
}
