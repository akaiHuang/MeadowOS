//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package taskruntime

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var ErrSchedulerInUse = errors.New("task scheduler is already owned by another Meadow process")

type schedulerFileLock struct {
	file *os.File
}

func acquireSchedulerFileLock(path string) (*schedulerFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open task scheduler lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("protect task scheduler lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		owner := schedulerLockOwner(file)
		_ = file.Close()
		if owner != "" {
			return nil, fmt.Errorf("%w (%s)", ErrSchedulerInUse, owner)
		}
		return nil, ErrSchedulerInUse
	}
	if err := writeSchedulerLockOwner(file); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	return &schedulerFileLock{file: file}, nil
}

func releaseSchedulerFileLock(lock *schedulerFileLock) error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}

func writeSchedulerLockOwner(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate task scheduler lock: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek task scheduler lock: %w", err)
	}
	owner := fmt.Sprintf("pid=%d started=%s", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if _, err := file.WriteString(owner + "\n"); err != nil {
		return fmt.Errorf("write task scheduler lock: %w", err)
	}
	return file.Sync()
}

func schedulerLockOwner(file *os.File) string {
	if _, err := file.Seek(0, 0); err != nil {
		return ""
	}
	data := make([]byte, 512)
	n, _ := file.Read(data)
	return strings.TrimSpace(string(data[:n]))
}
