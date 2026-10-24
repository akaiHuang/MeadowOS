package checkpoint

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	mu sync.Mutex
	directory string
	taskID string
	options Options
	journal *os.File
	writerLock *os.File
	state State
	lastSequence uint64
	lastHash string
	journalOffset int64
	closed bool
	poisoned error
}

// Open acquires an exclusive writer marker, recovers the bounded journal tail,
// and opens the journal for append. On recovery failure, Store is nil and the
// returned RecoveryResult still identifies the last trusted boundary.
func Open(directory, taskID string, options Options) (*Store, RecoveryResult, error) {
	options = normalizeOptions(options)
	if err := validateTaskID(taskID, options.Redactor); err != nil {
		return nil, RecoveryResult{}, err
	}
	directory = filepath.Clean(directory)
	if directory == "." || directory == string(filepath.Separator) {
		return nil, RecoveryResult{}, errors.New("checkpoint directory must be explicit")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, RecoveryResult{}, fmt.Errorf("create checkpoint directory: %w", err)
	}
	if err := requireDirectory(directory); err != nil {
		return nil, RecoveryResult{}, err
	}
	writerLock, err := acquireWriterLock(directory, options)
	if err != nil {
		return nil, RecoveryResult{}, err
	}
	recovery, recoverErr := Recover(directory, taskID, options)
	if recoverErr != nil {
		cleanupErr := releaseWriterLock(directory, writerLock)
		return nil, recovery, errors.Join(recoverErr, cleanupErr)
	}
	journalPath := filepath.Join(directory, JournalFileName)
	if err := rejectSymlink(journalPath); err != nil {
		cleanupErr := releaseWriterLock(directory, writerLock)
		return nil, recovery, errors.Join(err, cleanupErr)
	}
	journal, err := os.OpenFile(journalPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		cleanupErr := releaseWriterLock(directory, writerLock)
		return nil, recovery, errors.Join(fmt.Errorf("open checkpoint journal: %w", err), cleanupErr)
	}
	info, err := journal.Stat()
	if err != nil {
		cleanupErr := errors.Join(journal.Close(), releaseWriterLock(directory, writerLock))
		return nil, recovery, errors.Join(fmt.Errorf("stat checkpoint journal: %w", err), cleanupErr)
	}
	if !info.Mode().IsRegular() || info.Size() != recovery.Boundary.LastGoodOffset {
		cleanupErr := errors.Join(journal.Close(), releaseWriterLock(directory, writerLock))
		return nil, recovery, errors.Join(errors.New("checkpoint journal changed during open"), cleanupErr)
	}
	store := &Store{
		directory: directory,
		taskID: taskID,
		options: options,
		journal: journal,
		writerLock: writerLock,
		state: cloneState(recovery.State),
		lastSequence: recovery.Boundary.LastSequence,
		lastHash: recovery.Boundary.LastHash,
		journalOffset: recovery.Boundary.LastGoodOffset,
	}
	return store, recovery, nil
}

func (s *Store) State() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return State{}, err
	}
	return cloneState(s.state), nil
}

func (s *Store) Append(event Event) (AppendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return AppendResult{}, err
	}
	sanitized, err := sanitizeEvent(event, s.options)
	if err != nil {
		return AppendResult{}, err
	}
	trial := cloneState(s.state)
	applied, err := Apply(&trial, sanitized)
	if err != nil {
		return AppendResult{}, err
	}
	if !applied {
		return AppendResult{Applied: false, Sequence: s.lastSequence, Hash: s.lastHash}, nil
	}
	record := JournalRecord{
		Version: JournalVersion,
		TaskID: s.taskID,
		Sequence: s.lastSequence + 1,
		PreviousHash: s.lastHash,
		Event: sanitized,
	}
	record.Hash, err = journalRecordHash(record)
	if err != nil {
		return AppendResult{}, err
	}
	encoded, err := encodeJournalRecord(record)
	if err != nil {
		return AppendResult{}, err
	}
	if len(encoded) > s.options.Limits.MaxRecordBytes {
		return AppendResult{}, fmt.Errorf("checkpoint journal record is %d bytes; limit is %d", len(encoded), s.options.Limits.MaxRecordBytes)
	}
	if err := writeFull(s.journal, encoded); err != nil {
		return AppendResult{Sequence: record.Sequence, Hash: record.Hash}, s.poison(err, record.Sequence)
	}
	if err := s.journal.Sync(); err != nil {
		return AppendResult{Sequence: record.Sequence, Hash: record.Hash}, s.poison(err, record.Sequence)
	}
	s.state = trial
	s.lastSequence = record.Sequence
	s.lastHash = record.Hash
	s.journalOffset += int64(len(encoded))
	return AppendResult{Applied: true, Sequence: record.Sequence, Hash: record.Hash}, nil
}

// SaveSnapshot atomically replaces snapshot.json with a state image covering
// the current durable journal offset. The journal is never compacted or
// truncated.
func (s *Store) SaveSnapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{
		Version: SnapshotVersion,
		TaskID: s.taskID,
		JournalOffset: s.journalOffset,
		ThroughSequence: s.lastSequence,
		ThroughHash: s.lastHash,
		State: cloneState(s.state),
		CreatedAt: s.options.Clock().UTC(),
	}
	var err error
	snapshot.Hash, err = snapshotHash(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if err := writeSnapshotAtomic(s.directory, snapshot, s.options); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var closeErr error
	if s.journal != nil {
		closeErr = s.journal.Close()
		s.journal = nil
	}
	lockErr := releaseWriterLock(s.directory, s.writerLock)
	s.writerLock = nil
	return errors.Join(closeErr, lockErr)
}

func (s *Store) usable() error {
	if s.closed || s.journal == nil {
		return ErrClosed
	}
	if s.poisoned != nil {
		return errors.Join(ErrPoisoned, s.poisoned)
	}
	return nil
}

func (s *Store) poison(cause error, sequence uint64) error {
	s.poisoned = errors.Join(
		ErrDurabilityUncertain,
		fmt.Errorf("append sequence %d: %w", sequence, cause),
	)
	return s.poisoned
}

func validateTaskID(taskID string, redactor Redactor) error {
	_, err := sanitizeIdentifier("task id", taskID, redactor)
	return err
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect checkpoint directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("checkpoint directory must be a real directory, not a symlink")
	}
	return nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect checkpoint path %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("checkpoint path %s is a symlink", path)
	}
	return nil
}

func acquireWriterLock(directory string, options Options) (*os.File, error) {
	path := filepath.Join(directory, WriterLockName)
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%w: %s", ErrWriterLocked, path)
	}
	if err != nil {
		return nil, fmt.Errorf("create checkpoint writer lock: %w", err)
	}
	owner := fmt.Sprintf("pid=%d opened=%s\n", os.Getpid(), options.Clock().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
	if err := writeFull(file, []byte(owner)); err != nil {
		cleanupErr := errors.Join(file.Close(), os.Remove(path))
		return nil, errors.Join(fmt.Errorf("write checkpoint writer lock: %w", err), cleanupErr)
	}
	if err := file.Sync(); err != nil {
		cleanupErr := errors.Join(file.Close(), os.Remove(path))
		return nil, errors.Join(fmt.Errorf("sync checkpoint writer lock: %w", err), cleanupErr)
	}
	return file, nil
}

func releaseWriterLock(directory string, file *os.File) error {
	if file == nil {
		return nil
	}
	path := filepath.Join(directory, WriterLockName)
	return errors.Join(file.Close(), os.Remove(path))
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
