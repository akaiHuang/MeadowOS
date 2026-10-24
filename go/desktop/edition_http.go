package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	editioncfg "github.com/hey-meadow/meadow-code/go/core/edition"
)

// apiPrefixes are routed to the daemon/native contract instead of static
// assets. Segment matching keeps files such as /commercial.css static.
var apiPrefixes = []string{
	"/agent", "/auth", "/browser", "/capabilities", "/commercial", "/devices",
	"/entitlement", "/health", "/license", "/model", "/native",
	"/permission", "/referral", "/session", "/sessions", "/tasks",
	"/update", "/usage", "/vcs", "/workspace",
}

// desktopEditionPersistMu serializes edition persistence across every Desktop
// handler in this process. The real app installs a single handler, but keeping
// this lock process-local also prevents a future second window/server from
// racing the same atomic override file.
var desktopEditionPersistMu sync.Mutex

func isAPIPath(path string) bool {
	for _, prefix := range apiPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func editionRestartCommand(executable string, environment []string) *exec.Cmd {
	command := exec.Command(executable)
	command.Env = editioncfg.CleanEnvironment(environment)
	return command
}

// desktopEditionSwitch intercepts the Desktop-owned edition lifecycle route.
// In protected builds it must wrap the Agent proxy, otherwise /native/edition
// is forwarded to agentd and becomes a 404.
func desktopEditionSwitch(next http.Handler, restart func()) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	var switchMu sync.Mutex
	switchingProfile := ""
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/native/edition" {
			next.ServeHTTP(writer, request)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 4<<10)
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		var body struct {
			Profile string `json:"profile"`
		}
		if err := decoder.Decode(&body); err != nil {
			writeEditionError(writer, http.StatusBadRequest, "profile required")
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			writeEditionError(writer, http.StatusBadRequest, "invalid edition request")
			return
		}
		profile, ok := editioncfg.Normalize(body.Profile)
		if !ok {
			writeEditionError(writer, http.StatusBadRequest, "unsupported profile")
			return
		}
		switchMu.Lock()
		if switchingProfile != "" {
			if switchingProfile != profile {
				switchMu.Unlock()
				writeEditionError(writer, http.StatusConflict, "edition switch already in progress")
				return
			}
			switchMu.Unlock()
			writeEditionResponse(writer, profile, restart != nil)
			return
		}
		desktopEditionPersistMu.Lock()
		err := editioncfg.Persist(profile)
		desktopEditionPersistMu.Unlock()
		if err != nil {
			switchMu.Unlock()
			writeEditionError(writer, http.StatusInternalServerError, "unable to persist edition")
			return
		}
		if restart != nil {
			switchingProfile = profile
		}
		switchMu.Unlock()
		writeEditionResponse(writer, profile, restart != nil)
		if restart != nil {
			go func() {
				time.Sleep(250 * time.Millisecond)
				restart()
			}()
		}
	})
}

func writeEditionResponse(writer http.ResponseWriter, profile string, restarting bool) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"ok": true, "profile": profile, "restarting": restarting,
	})
}

func writeEditionError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": message})
}
