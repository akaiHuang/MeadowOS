//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package sessionrecovery

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type sessionWriterLock struct {
	file *os.File
}

func acquireSessionWriterLock(path string) (*sessionWriterLock, error) {
	return acquireSessionFileLock(path, true)
}

func acquireSessionStateLock(path string) (*sessionWriterLock, error) {
	return acquireSessionFileLock(path, false)
}

func acquireExistingSessionStateLock(path string) (*sessionWriterLock, bool, error) {
	if err := rejectSymlink(path); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open recovery state lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("lock recovery state: %w", err)
	}
	return &sessionWriterLock{file: file}, true, nil
}

func acquireSessionFileLock(path string, failImmediately bool) (*sessionWriterLock, error) {
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open recovery writer lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("protect recovery writer lock: %w", err)
	}
	operation := unix.LOCK_EX
	if failImmediately {
		operation |= unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		if failImmediately && (errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)) {
			return nil, ErrSessionInUse
		}
		return nil, fmt.Errorf("lock recovery state: %w", err)
	}
	return &sessionWriterLock{file: file}, nil
}

func releaseSessionWriterLock(lock *sessionWriterLock) error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}

func releaseSessionStateLock(lock *sessionWriterLock) error {
	return releaseSessionWriterLock(lock)
}
