//go:build !windows

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessTree(process *os.Process) error {
	if process == nil {
		return nil
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func forceKillProcessTree(process *os.Process) error {
	if process == nil {
		return nil
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func startProcessWatchdog(parentPID, targetProcessGroup int) (func(), error) {
	if parentPID <= 0 || targetProcessGroup <= 0 {
		return nil, fmt.Errorf("invalid watchdog pid parent=%d target=%d", parentPID, targetProcessGroup)
	}
	ctx, cancel := context.WithCancel(context.Background())
	const script = `
parent="$1"
target="$2"
while kill -0 "$parent" 2>/dev/null && kill -0 "-$target" 2>/dev/null; do
  sleep 1
done
if ! kill -0 "$parent" 2>/dev/null && kill -0 "-$target" 2>/dev/null; then
  kill -TERM "-$target" 2>/dev/null || true
  sleep 2
  kill -KILL "-$target" 2>/dev/null || true
fi
`
	watchdog := exec.CommandContext(
		ctx, "/bin/sh", "-c", script, "meadow-process-watchdog",
		strconv.Itoa(parentPID), strconv.Itoa(targetProcessGroup),
	)
	configureProcessGroup(watchdog)
	if err := watchdog.Start(); err != nil {
		cancel()
		return nil, err
	}
	go func() {
		_ = watchdog.Wait()
	}()
	return cancel, nil
}
