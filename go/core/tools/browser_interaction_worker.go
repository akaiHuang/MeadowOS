package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Never reuse the user's preview/CLI browser. Each check owns exactly one
// ephemeral listener/process and tears it down on success, error or cancel.
func startInteractionWorker(ctx context.Context, helper *MeadowBrowserVisualCheck) (string, func(), error) {
	if runtime.GOOS != "darwin" {
		return "", nil, fmt.Errorf("native interaction worker requires macOS")
	}
	binary, err := helper.localBrowserBinary()
	if err != nil {
		return "", nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	logDir := filepath.Join(meadowHomeDir(), "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return "", nil, err
	}
	logFile, err := os.CreateTemp(logDir, "interaction-worker-*.log")
	if err != nil {
		return "", nil, err
	}
	cmd := exec.CommandContext(ctx, binary, "--host", "127.0.0.1", "--port", port, "--preview")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return "", nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); _ = logFile.Close(); close(done) }()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			select {
			case <-done:
				return
			default:
			}
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		})
	}
	base := "http://127.0.0.1:" + port
	ready, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for {
		// The port was released before launch. Verify ownership, not just any
		// successful health endpoint, so a bind race cannot navigate a foreign page.
		probe, stop := context.WithTimeout(ready, time.Second)
		owners, _ := exec.CommandContext(probe, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(cmd.Process.Pid), "-iTCP:"+port, "-sTCP:LISTEN", "-t").Output()
		stop()
		if strings.TrimSpace(string(owners)) == strconv.Itoa(cmd.Process.Pid) {
			if _, err := helper.health(ready, base); err == nil {
				return base, cleanup, nil
			}
		}
		select {
		case <-done:
			cleanup()
			return "", nil, fmt.Errorf("interaction worker exited before ready; log: %s", logFile.Name())
		case <-ready.Done():
			cleanup()
			return "", nil, fmt.Errorf("interaction worker unavailable: %w; log: %s", ready.Err(), logFile.Name())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Observes real RAF callbacks; never replaces the clock or advances game state.
// A nonce-scoped observer lives only in the verification page, not the source file.
func verifyInteractionClock(ctx context.Context, helper *MeadowBrowserVisualCheck, base string) error {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := "__meadow_clock_" + hex.EncodeToString(nonce[:])
	quoted, _ := json.Marshal(key)
	start := `(function(){const s={frames:0,id:0};Object.defineProperty(window,` + string(quoted) + `,{value:s,configurable:true});function tick(){s.frames++;if(s.frames<2)s.id=requestAnimationFrame(tick);}s.id=requestAnimationFrame(tick);return "registered";})()`
	if err := helper.postJSON(ctx, base+"/js", map[string]any{"code": start}, nil); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = helper.postJSON(cleanup, base+"/js", map[string]any{"code": `(function(){const s=window[` + string(quoted) + `];if(s)cancelAnimationFrame(s.id);delete window[` + string(quoted) + `];return true;})()`}, nil)
	}()
	deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		var raw json.RawMessage
		code := `(function(){const s=window[` + string(quoted) + `];return s&&s.frames>=2?` + string(quoted) + `+":ready":"waiting";})()`
		if err := helper.postJSON(deadline, base+"/js", map[string]any{"code": code}, &raw); err != nil {
			return err
		}
		if strings.Contains(interactionDOMSearchText(raw), key+":ready") {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("requestAnimationFrame did not produce two callbacks: %w", deadline.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
