package sessionrecovery

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu             sync.Mutex
	directory      string
	sessionID      string
	options        Options
	writerLock     *sessionWriterLock
	file           *os.File
	state          State
	journalBytes   int64
	journalRecords int
	closed         bool
	poisoned       bool
}

const sessionStateLockFileName = "state.lock"

func Open(root, sessionID string, options Options) (*Store, Recovery, error) {
	options = normalizeOptions(options)
	directory, err := ensureSessionDirectory(root, sessionID)
	if err != nil {
		return nil, Recovery{}, err
	}
	writerLock, err := acquireSessionWriterLock(filepath.Join(directory, "writer.lock"))
	if err != nil {
		return nil, Recovery{}, err
	}
	releaseWriterLock := true
	defer func() {
		if releaseWriterLock {
			_ = releaseSessionWriterLock(writerLock)
		}
	}()
	recovery, err := recoverDirectory(directory, sessionID, options)
	if err != nil {
		return nil, recovery, err
	}
	if err := pruneOrphanedEvidence(directory, recovery.State); err != nil {
		return nil, recovery, err
	}
	journalPath := filepath.Join(directory, JournalFileName)
	if err := rejectSymlink(journalPath); err != nil {
		return nil, recovery, err
	}
	file, err := os.OpenFile(journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, recovery, fmt.Errorf("open recovery journal: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, recovery, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, recovery, err
	}
	store := &Store{
		directory:      directory,
		sessionID:      sessionID,
		options:        options,
		writerLock:     writerLock,
		file:           file,
		state:          cloneState(recovery.State),
		journalBytes:   info.Size(),
		journalRecords: recovery.PhysicalRecords,
	}
	releaseWriterLock = false
	return store, recovery, nil
}

// Recover is read-only. It reports the last trusted boundary and never repairs,
// truncates, or quarantines a corrupt journal.
func Recover(root, sessionID string, options Options) (Recovery, error) {
	options = normalizeOptions(options)
	if !validIdentifier(sessionID) {
		return Recovery{}, errors.New("session id is invalid")
	}
	directory := filepath.Join(filepath.Clean(root), sessionID)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		state := newState()
		return Recovery{State: state}, nil
	}
	if err != nil {
		return Recovery{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Recovery{}, errors.New("session recovery directory is not a safe directory")
	}
	return recoverDirectoryReadOnly(directory, sessionID, options)
}

func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state)
}

func (s *Store) persistEvidenceLocked(reference *ContentReference) (*ContentReference, error) {
	if reference == nil || reference.Redacted || reference.Sensitive || reference.Content == "" {
		return metadataOnlyReference(reference), nil
	}
	if int64(len(reference.Content)) > s.options.Limits.MaxEvidenceBytes {
		return nil, fmt.Errorf("%w: durable evidence exceeds limit", ErrStateLimit)
	}
	directory := filepath.Join(s.directory, EvidenceDirectoryName)
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := requireSafeDirectory(directory); err != nil {
		return nil, err
	}
	clean := *reference
	clean.StorageRef = evidenceStorageRef(clean.SHA256)
	path := filepath.Join(s.directory, filepath.FromSlash(clean.StorageRef))
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	data := []byte(clean.Content)
	if existing, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(existing, data) {
			return nil, fmt.Errorf("%w: evidence digest collision or tamper", ErrCorruptJournal)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if err := atomicWriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return &clean, nil
}

func (s *Store) SaveSnapshot() (resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return err
	}
	stateLock, err := acquireSessionStateLock(filepath.Join(s.directory, sessionStateLockFileName))
	if err != nil {
		return err
	}
	defer func() {
		if lockErr := releaseSessionStateLock(stateLock); lockErr != nil {
			s.poisoned = true
			resultErr = errors.Join(resultErr, fmt.Errorf("release recovery state lock: %w", lockErr))
		}
	}()
	bounded, err := boundedState(s.state, s.options.Limits)
	if err != nil {
		return err
	}
	return s.writeSnapshotLocked(bounded)
}

func (s *Store) Compact(reason string) (resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return err
	}
	if sanitizeText(reason, s.options) == "" {
		return errors.New("compaction reason is required")
	}
	stateLock, err := acquireSessionStateLock(filepath.Join(s.directory, sessionStateLockFileName))
	if err != nil {
		return err
	}
	defer func() {
		if lockErr := releaseSessionStateLock(stateLock); lockErr != nil {
			s.poisoned = true
			resultErr = errors.Join(resultErr, fmt.Errorf("release recovery state lock: %w", lockErr))
		}
	}()
	bounded, err := boundedState(s.state, s.options.Limits)
	if err != nil {
		return err
	}
	if err := s.writeSnapshotLocked(bounded); err != nil {
		return err
	}
	if err := s.file.Close(); err != nil {
		s.poisoned = true
		return err
	}
	s.file = nil
	journalPath := filepath.Join(s.directory, JournalFileName)
	if err := atomicReplaceEmpty(journalPath); err != nil {
		s.file, _ = os.OpenFile(journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if s.file == nil {
			s.poisoned = true
		}
		return err
	}
	file, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.poisoned = true
		return err
	}
	s.file = file
	s.state = bounded
	s.journalBytes = 0
	s.journalRecords = 0
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var fileErr error
	if s.file != nil {
		fileErr = s.file.Close()
		s.file = nil
	}
	lockErr := releaseSessionWriterLock(s.writerLock)
	s.writerLock = nil
	return errors.Join(fileErr, lockErr)
}

func (s *Store) appendEventLocked(event Event) (result RecordResult, resultErr error) {
	if err := s.usableLocked(); err != nil {
		return RecordResult{}, err
	}
	trial := cloneState(s.state)
	if err := applyEvent(&trial, event); err != nil {
		return RecordResult{}, err
	}
	record := JournalRecord{
		Version:      JournalVersion,
		SessionID:    s.sessionID,
		Sequence:     s.state.LastSequence + 1,
		PreviousHash: s.state.LastHash,
		Event:        event,
	}
	hash, err := journalRecordHash(record)
	if err != nil {
		return RecordResult{}, err
	}
	record.Hash = hash
	encoded, err := json.Marshal(record)
	if err != nil {
		return RecordResult{}, err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > s.options.Limits.MaxRecordBytes {
		return RecordResult{}, errors.New("recovery event exceeds record limit")
	}
	if s.journalBytes+int64(len(encoded)) > s.options.Limits.MaxJournalBytes || s.journalRecords+1 > s.options.Limits.MaxJournalRecords {
		return RecordResult{}, ErrCompactionRequired
	}
	stateLock, err := acquireSessionStateLock(filepath.Join(s.directory, sessionStateLockFileName))
	if err != nil {
		return RecordResult{}, err
	}
	defer func() {
		if lockErr := releaseSessionStateLock(stateLock); lockErr != nil {
			s.poisoned = true
			resultErr = errors.Join(resultErr, fmt.Errorf("release recovery state lock: %w", lockErr))
		}
	}()
	if err := writeFull(s.file, encoded); err != nil {
		s.poisoned = true
		return RecordResult{}, fmt.Errorf("append recovery event: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		s.poisoned = true
		return RecordResult{}, fmt.Errorf("sync recovery event: %w", err)
	}
	trial.LastSequence = record.Sequence
	trial.LastHash = record.Hash
	s.state = trial
	s.journalBytes += int64(len(encoded))
	s.journalRecords++
	return RecordResult{Applied: true, Sequence: record.Sequence, Hash: record.Hash}, nil
}

func (s *Store) writeSnapshotLocked(state State) error {
	snapshot := Snapshot{
		Version:         SnapshotVersion,
		SessionID:       s.sessionID,
		ThroughSequence: state.LastSequence,
		ThroughHash:     state.LastHash,
		State:           state,
		CreatedAt:       s.options.Clock().UTC(),
	}
	hash, err := snapshotHash(snapshot)
	if err != nil {
		return err
	}
	snapshot.Hash = hash
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > s.options.Limits.MaxSnapshotBytes {
		return ErrStateLimit
	}
	return atomicWriteFile(filepath.Join(s.directory, SnapshotFileName), encoded, 0o600)
}

func (s *Store) usableLocked() error {
	if s.closed {
		return ErrClosed
	}
	if s.poisoned || s.file == nil {
		return ErrPoisoned
	}
	return nil
}

func recoverDirectory(directory, sessionID string, options Options) (Recovery, error) {
	stateLock, err := acquireSessionStateLock(filepath.Join(directory, sessionStateLockFileName))
	if err != nil {
		return Recovery{}, err
	}
	recovery, recoverErr := recoverDirectoryLocked(directory, sessionID, options)
	lockErr := releaseSessionStateLock(stateLock)
	if lockErr != nil {
		lockErr = fmt.Errorf("release recovery state lock: %w", lockErr)
	}
	return recovery, errors.Join(recoverErr, lockErr)
}

func recoverDirectoryReadOnly(directory, sessionID string, options Options) (Recovery, error) {
	// Older sessions may not have a state lock yet. Recover must remain a
	// read-only operation, so only participate in locking when the lock already
	// exists; Open creates it before allowing any new writes.
	stateLock, exists, err := acquireExistingSessionStateLock(filepath.Join(directory, sessionStateLockFileName))
	if err != nil {
		return Recovery{}, err
	}
	if !exists {
		return recoverDirectoryLocked(directory, sessionID, options)
	}
	recovery, recoverErr := recoverDirectoryLocked(directory, sessionID, options)
	lockErr := releaseSessionStateLock(stateLock)
	if lockErr != nil {
		lockErr = fmt.Errorf("release recovery state lock: %w", lockErr)
	}
	return recovery, errors.Join(recoverErr, lockErr)
}

func recoverDirectoryLocked(directory, sessionID string, options Options) (Recovery, error) {
	state := newState()
	recovery := Recovery{State: state}
	snapshot, found, err := readSnapshot(filepath.Join(directory, SnapshotFileName), sessionID, options)
	if err != nil {
		recovery.Corrupt = true
		return recovery, err
	}
	if found {
		state = cloneState(snapshot.State)
		recovery.State = state
		recovery.SnapshotUsed = true
	}
	journalPath := filepath.Join(directory, JournalFileName)
	if err := rejectSymlink(journalPath); err != nil {
		recovery.Corrupt = true
		return recovery, err
	}
	file, err := os.Open(journalPath)
	if errors.Is(err, os.ErrNotExist) {
		if err := hydrateStateEvidence(directory, &state, options); err != nil {
			recovery.Corrupt = true
			return recovery, err
		}
		recovery.State = state
		recovery.CrashDetected = crashDetected(state)
		return recovery, nil
	}
	if err != nil {
		return recovery, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return recovery, err
	}
	if !info.Mode().IsRegular() || info.Size() > options.Limits.MaxJournalBytes {
		recovery.Corrupt = true
		return recovery, fmt.Errorf("%w: journal file is unsafe or exceeds limit", ErrCorruptJournal)
	}
	reader := bufio.NewReaderSize(file, options.Limits.MaxRecordBytes+1)
	var previous *JournalRecord
	anchored := !found
	physicalRecords := 0
	for {
		start := recovery.TrustedOffset
		line, readErr := reader.ReadBytes('\n')
		if errors.Is(readErr, io.EOF) && len(line) == 0 {
			break
		}
		if len(line) > options.Limits.MaxRecordBytes || !bytes.HasSuffix(line, []byte{'\n'}) {
			recovery.Corrupt = true
			recovery.FailureOffset = start
			return recovery, fmt.Errorf("%w: incomplete or oversized record", ErrCorruptJournal)
		}
		if readErr != nil {
			recovery.Corrupt = true
			recovery.FailureOffset = start
			return recovery, fmt.Errorf("%w: %v", ErrCorruptJournal, readErr)
		}
		physicalRecords++
		recovery.PhysicalRecords = physicalRecords
		if physicalRecords > options.Limits.MaxJournalRecords {
			recovery.Corrupt = true
			recovery.FailureOffset = start
			return recovery, fmt.Errorf("%w: record limit exceeded", ErrCorruptJournal)
		}
		var record JournalRecord
		if err := decodeStrict(bytes.TrimSuffix(line, []byte{'\n'}), &record); err != nil {
			recovery.Corrupt = true
			recovery.FailureOffset = start
			return recovery, fmt.Errorf("%w: %v", ErrCorruptJournal, err)
		}
		if err := validateRecord(record, sessionID); err != nil {
			recovery.Corrupt = true
			recovery.FailureOffset = start
			return recovery, err
		}
		if previous != nil {
			if record.Sequence != previous.Sequence+1 || record.PreviousHash != previous.Hash {
				recovery.Corrupt = true
				recovery.FailureOffset = start
				return recovery, fmt.Errorf("%w: sequence or hash chain break", ErrCorruptJournal)
			}
		} else if !found {
			if record.Sequence != 1 || record.PreviousHash != ZeroHash {
				recovery.Corrupt = true
				recovery.FailureOffset = start
				return recovery, fmt.Errorf("%w: first record has no valid origin", ErrCorruptJournal)
			}
		} else if record.Sequence > snapshot.ThroughSequence+1 {
			recovery.Corrupt = true
			recovery.FailureOffset = start
			return recovery, fmt.Errorf("%w: journal starts after snapshot boundary", ErrCorruptJournal)
		}

		if found && record.Sequence <= snapshot.ThroughSequence {
			if record.Sequence == snapshot.ThroughSequence {
				if record.Hash != snapshot.ThroughHash {
					recovery.Corrupt = true
					recovery.FailureOffset = start
					return recovery, fmt.Errorf("%w: snapshot anchor mismatch", ErrCorruptJournal)
				}
				anchored = true
			}
		} else {
			if found && !anchored {
				if record.Sequence != snapshot.ThroughSequence+1 || record.PreviousHash != snapshot.ThroughHash {
					recovery.Corrupt = true
					recovery.FailureOffset = start
					return recovery, fmt.Errorf("%w: tail is not linked to snapshot", ErrCorruptJournal)
				}
				anchored = true
			}
			trial := cloneState(state)
			if err := applyEvent(&trial, record.Event); err != nil {
				recovery.Corrupt = true
				recovery.FailureOffset = start
				return recovery, fmt.Errorf("%w: %v", ErrCorruptJournal, err)
			}
			trial.LastSequence = record.Sequence
			trial.LastHash = record.Hash
			state = trial
			recovery.ReplayedRecords++
		}
		previous = &record
		recovery.TrustedOffset += int64(len(line))
	}
	if found && previous != nil && previous.Sequence < snapshot.ThroughSequence {
		recovery.Corrupt = true
		return recovery, fmt.Errorf("%w: journal overlap does not reach snapshot boundary", ErrCorruptJournal)
	}
	reconstructUnknownEffectGuards(&state)
	if len(state.ActionGuards) > options.Limits.MaxActionGuards {
		recovery.Corrupt = true
		recovery.State = state
		return recovery, fmt.Errorf("%w: reconstructed action guards exceed limit", ErrStateLimit)
	}
	if err := hydrateStateEvidence(directory, &state, options); err != nil {
		recovery.Corrupt = true
		return recovery, err
	}
	recovery.State = state
	recovery.CrashDetected = crashDetected(state)
	return recovery, nil
}

func readSnapshot(path, sessionID string, options Options) (Snapshot, bool, error) {
	if err := rejectSymlink(path); err != nil {
		return Snapshot{}, false, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > options.Limits.MaxSnapshotBytes {
		return Snapshot{}, false, ErrCorruptSnapshot
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, false, err
	}
	var snapshot Snapshot
	if err := decodeStrict(data, &snapshot); err != nil {
		return Snapshot{}, false, fmt.Errorf("%w: %v", ErrCorruptSnapshot, err)
	}
	if snapshot.Version != SnapshotVersion || snapshot.SessionID != sessionID || snapshot.CreatedAt.IsZero() ||
		snapshot.ThroughSequence != snapshot.State.LastSequence || snapshot.ThroughHash != snapshot.State.LastHash {
		return Snapshot{}, false, ErrCorruptSnapshot
	}
	for _, call := range snapshot.State.ToolCalls {
		if call.InputVersion != "" && !validHash(call.InputVersion) {
			return Snapshot{}, false, ErrCorruptSnapshot
		}
	}
	expected, err := snapshotHash(snapshot)
	if err != nil || expected != snapshot.Hash {
		return Snapshot{}, false, ErrCorruptSnapshot
	}
	return snapshot, true, nil
}

func validateRecord(record JournalRecord, sessionID string) error {
	if record.Version != JournalVersion || record.SessionID != sessionID || record.Sequence == 0 || !validHash(record.PreviousHash) || !validHash(record.Hash) {
		return ErrCorruptJournal
	}
	expected, err := journalRecordHash(record)
	if err != nil || expected != record.Hash {
		return ErrCorruptJournal
	}
	return validateEvent(record.Event)
}

func journalRecordHash(record JournalRecord) (string, error) {
	identity := struct {
		Version      string `json:"version"`
		SessionID    string `json:"session_id"`
		Sequence     uint64 `json:"sequence"`
		PreviousHash string `json:"previous_hash"`
		Event        Event  `json:"event"`
	}{record.Version, record.SessionID, record.Sequence, record.PreviousHash, record.Event}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func snapshotHash(snapshot Snapshot) (string, error) {
	identity := struct {
		Version         string    `json:"version"`
		SessionID       string    `json:"session_id"`
		ThroughSequence uint64    `json:"through_sequence"`
		ThroughHash     string    `json:"through_hash"`
		State           State     `json:"state"`
		CreatedAt       time.Time `json:"created_at"`
	}{snapshot.Version, snapshot.SessionID, snapshot.ThroughSequence, snapshot.ThroughHash, snapshot.State, snapshot.CreatedAt}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func normalizeOptions(options Options) Options {
	defaults := DefaultOptions()
	if options.Limits.MaxRecordBytes <= 0 {
		options.Limits.MaxRecordBytes = defaults.Limits.MaxRecordBytes
	}
	if options.Limits.MaxJournalBytes <= 0 {
		options.Limits.MaxJournalBytes = defaults.Limits.MaxJournalBytes
	}
	if options.Limits.MaxJournalRecords <= 0 {
		options.Limits.MaxJournalRecords = defaults.Limits.MaxJournalRecords
	}
	if options.Limits.MaxSnapshotBytes <= 0 {
		options.Limits.MaxSnapshotBytes = defaults.Limits.MaxSnapshotBytes
	}
	if options.Limits.MaxSummaryBytes <= 0 {
		options.Limits.MaxSummaryBytes = defaults.Limits.MaxSummaryBytes
	}
	if options.Limits.MaxRecentTurns <= 0 {
		options.Limits.MaxRecentTurns = defaults.Limits.MaxRecentTurns
	}
	if options.Limits.MaxToolResults <= 0 {
		options.Limits.MaxToolResults = defaults.Limits.MaxToolResults
	}
	if options.Limits.MaxActionGuards <= 0 {
		options.Limits.MaxActionGuards = defaults.Limits.MaxActionGuards
	}
	if options.Limits.MaxRecentRecaps <= 0 {
		options.Limits.MaxRecentRecaps = defaults.Limits.MaxRecentRecaps
	}
	if options.Limits.MaxPendingPermissions <= 0 {
		options.Limits.MaxPendingPermissions = defaults.Limits.MaxPendingPermissions
	}
	if options.Limits.MaxQueuedGuidance <= 0 {
		options.Limits.MaxQueuedGuidance = defaults.Limits.MaxQueuedGuidance
	}
	if options.Limits.MaxAttachments <= 0 {
		options.Limits.MaxAttachments = defaults.Limits.MaxAttachments
	}
	if options.Limits.MaxEvidenceBytes <= 0 {
		options.Limits.MaxEvidenceBytes = defaults.Limits.MaxEvidenceBytes
	}
	if options.Clock == nil {
		options.Clock = defaults.Clock
	}
	return options
}

func hydrateStateEvidence(directory string, state *State, options Options) error {
	for callID, result := range state.ToolResults {
		hydrated, err := hydrateEvidenceReference(directory, result.Output, options)
		if err != nil {
			return fmt.Errorf("%w: tool result %s evidence: %v", ErrCorruptJournal, callID, err)
		}
		result.Output = hydrated
		state.ToolResults[callID] = result
	}
	for index := range state.RecentRecaps {
		hydrated, err := hydrateEvidenceReference(directory, state.RecentRecaps[index].Summary, options)
		if err != nil {
			return fmt.Errorf("%w: recap %s evidence: %v", ErrCorruptJournal, state.RecentRecaps[index].RunID, err)
		}
		state.RecentRecaps[index].Summary = hydrated
	}
	return nil
}

func pruneOrphanedEvidence(directory string, state State) error {
	evidenceDirectory := filepath.Join(directory, EvidenceDirectoryName)
	info, err := os.Lstat(evidenceDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("recovery evidence path is not a safe directory")
	}

	referenced := make(map[string]struct{})
	remember := func(reference *ContentReference) {
		if reference == nil || reference.StorageRef == "" {
			return
		}
		referenced[filepath.Base(filepath.FromSlash(reference.StorageRef))] = struct{}{}
	}
	for _, result := range state.ToolResults {
		remember(result.Output)
	}
	for _, recap := range state.RecentRecaps {
		remember(recap.Summary)
	}

	entries, err := os.ReadDir(evidenceDirectory)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		name := entry.Name()
		if _, ok := referenced[name]; ok {
			continue
		}
		digest := strings.TrimSuffix(name, ".blob")
		if len(digest) != sha256.Size*2 || name != digest+".blob" {
			return fmt.Errorf("unexpected recovery evidence entry: %s", name)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return fmt.Errorf("unexpected recovery evidence entry: %s", name)
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !entryInfo.Mode().IsRegular() {
			return fmt.Errorf("recovery evidence entry is not a regular file: %s", name)
		}
		if err := os.Remove(filepath.Join(evidenceDirectory, name)); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(evidenceDirectory)
	}
	return nil
}

func hydrateEvidenceReference(directory string, reference *ContentReference, options Options) (*ContentReference, error) {
	if reference == nil || reference.StorageRef == "" {
		return reference, nil
	}
	if reference.StorageRef != evidenceStorageRef(reference.SHA256) {
		return nil, errors.New("invalid evidence locator")
	}
	path := filepath.Join(directory, filepath.FromSlash(reference.StorageRef))
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != reference.Bytes || info.Size() > options.Limits.MaxEvidenceBytes {
		return nil, errors.New("evidence size or type mismatch")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != reference.SHA256 || containsSensitive(string(data)) {
		return nil, errors.New("evidence digest mismatch or sensitive content")
	}
	clean := *reference
	clean.Content = string(data)
	return &clean, nil
}

func ensureSessionDirectory(root, sessionID string) (string, error) {
	if !validIdentifier(sessionID) {
		return "", errors.New("session id is invalid")
	}
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := requireSafeDirectory(root); err != nil {
		return "", err
	}
	directory := filepath.Join(root, sessionID)
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := requireSafeDirectory(directory); err != nil {
		return "", err
	}
	return directory, nil
}

func requireSafeDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("recovery path is not a safe directory")
	}
	return os.Chmod(path, 0o700)
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("recovery file must not be a symlink")
	}
	return nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".recovery-tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := writeFull(temporary, data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func atomicReplaceEmpty(path string) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".recovery-journal-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func newEventID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(value[:]), nil
}

func crashDetected(state State) bool {
	return state.CurrentTurn != nil && state.CurrentTurn.Status == TurnRunning && state.Terminal == nil
}

var _ Recorder = (*Store)(nil)
