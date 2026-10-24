package checkpoint

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Recover performs read-only, bounded recovery. It never creates, truncates,
// rewrites, or quarantines checkpoint data.
func Recover(directory, taskID string, options Options) (RecoveryResult, error) {
	options = normalizeOptions(options)
	result := RecoveryResult{
		State: newState(),
		Boundary: RecoveryBoundary{LastHash: ZeroHash},
	}
	if err := validateTaskID(taskID, options.Redactor); err != nil {
		return result, err
	}
	directory = filepath.Clean(directory)
	snapshotPath := filepath.Join(directory, SnapshotFileName)
	snapshot, found, err := readSnapshot(snapshotPath, taskID, options)
	if err != nil {
		result.Boundary.FailureOffset = 0
		return result, &RecoveryError{
			Kind: RecoverySnapshotCorrupt,
			Path: snapshotPath,
			Boundary: result.Boundary,
			Cause: errors.Join(ErrCorruptCheckpoint, err),
		}
	}
	if found {
		result.State = cloneState(snapshot.State)
		result.Boundary = RecoveryBoundary{
			SnapshotUsed: true,
			LastGoodOffset: snapshot.JournalOffset,
			LastSequence: snapshot.ThroughSequence,
			LastHash: snapshot.ThroughHash,
		}
	}
	journalPath := filepath.Join(directory, JournalFileName)
	if err := rejectSymlink(journalPath); err != nil {
		result.Boundary.FailureOffset = result.Boundary.LastGoodOffset
		return result, journalRecoveryError(journalPath, result.Boundary, err)
	}
	journal, err := os.Open(journalPath)
	if errors.Is(err, os.ErrNotExist) {
		if result.Boundary.LastGoodOffset == 0 {
			return result, nil
		}
		result.Boundary.FailureOffset = 0
		return result, journalRecoveryError(journalPath, result.Boundary, errors.New("snapshot references a missing journal"))
	}
	if err != nil {
		result.Boundary.FailureOffset = result.Boundary.LastGoodOffset
		return result, journalRecoveryError(journalPath, result.Boundary, err)
	}
	defer journal.Close()
	info, err := journal.Stat()
	if err != nil {
		result.Boundary.FailureOffset = result.Boundary.LastGoodOffset
		return result, journalRecoveryError(journalPath, result.Boundary, err)
	}
	if !info.Mode().IsRegular() {
		result.Boundary.FailureOffset = result.Boundary.LastGoodOffset
		return result, journalRecoveryError(journalPath, result.Boundary, errors.New("journal is not a regular file"))
	}
	if info.Size() < result.Boundary.LastGoodOffset {
		result.Boundary.FailureOffset = info.Size()
		return result, journalRecoveryError(journalPath, result.Boundary, errors.New("journal is shorter than the snapshot boundary"))
	}
	tailBytes := info.Size() - result.Boundary.LastGoodOffset
	if tailBytes > options.Limits.MaxTailBytes {
		result.Boundary.FailureOffset = result.Boundary.LastGoodOffset
		return result, &RecoveryError{
			Kind: RecoveryBoundExceeded,
			Path: journalPath,
			Boundary: result.Boundary,
			Cause: fmt.Errorf("%w: journal tail is %d bytes; limit is %d", ErrRecoveryLimit, tailBytes, options.Limits.MaxTailBytes),
		}
	}
	if _, err := journal.Seek(result.Boundary.LastGoodOffset, io.SeekStart); err != nil {
		result.Boundary.FailureOffset = result.Boundary.LastGoodOffset
		return result, journalRecoveryError(journalPath, result.Boundary, err)
	}
	reader := bufio.NewReaderSize(journal, options.Limits.MaxRecordBytes+1)
	for result.Boundary.LastGoodOffset < info.Size() {
		start := result.Boundary.LastGoodOffset
		if result.ReplayedRecords+result.DuplicateRecords >= options.Limits.MaxRecords {
			result.Boundary.FailureOffset = start
			return result, &RecoveryError{
				Kind: RecoveryBoundExceeded,
				Path: journalPath,
				Boundary: result.Boundary,
				Cause: fmt.Errorf("%w: replay record limit is %d", ErrRecoveryLimit, options.Limits.MaxRecords),
			}
		}
		line, readErr := reader.ReadBytes('\n')
		if len(line) > options.Limits.MaxRecordBytes {
			result.Boundary.FailureOffset = start
			return result, journalRecoveryError(journalPath, result.Boundary, fmt.Errorf("record exceeds %d bytes", options.Limits.MaxRecordBytes))
		}
		if readErr != nil {
			result.Boundary.FailureOffset = start
			if errors.Is(readErr, io.EOF) && len(line) > 0 {
				return result, journalRecoveryError(journalPath, result.Boundary, errors.New("journal ends with an incomplete record"))
			}
			return result, journalRecoveryError(journalPath, result.Boundary, readErr)
		}
		if len(line) == 1 {
			result.Boundary.FailureOffset = start
			return result, journalRecoveryError(journalPath, result.Boundary, errors.New("journal contains an empty record"))
		}
		var record JournalRecord
		if err := decodeStrict(bytes.TrimSuffix(line, []byte{'\n'}), &record); err != nil {
			result.Boundary.FailureOffset = start
			return result, journalRecoveryError(journalPath, result.Boundary, err)
		}
		if err := validateJournalRecord(record, taskID, result.Boundary, options); err != nil {
			result.Boundary.FailureOffset = start
			return result, journalRecoveryError(journalPath, result.Boundary, err)
		}
		trial := cloneState(result.State)
		applied, err := Apply(&trial, record.Event)
		if err != nil {
			result.Boundary.FailureOffset = start
			return result, journalRecoveryError(journalPath, result.Boundary, err)
		}
		result.State = trial
		if applied {
			result.ReplayedRecords++
		} else {
			result.DuplicateRecords++
		}
		result.Boundary.LastGoodOffset += int64(len(line))
		result.Boundary.LastSequence = record.Sequence
		result.Boundary.LastHash = record.Hash
	}
	return result, nil
}

func readSnapshot(path, taskID string, options Options) (Snapshot, bool, error) {
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
	if !info.Mode().IsRegular() {
		return Snapshot{}, false, errors.New("snapshot is not a regular file")
	}
	if info.Size() > options.Limits.MaxSnapshotBytes {
		return Snapshot{}, false, fmt.Errorf("%w: snapshot is %d bytes; limit is %d", ErrRecoveryLimit, info.Size(), options.Limits.MaxSnapshotBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, false, err
	}
	if int64(len(data)) > options.Limits.MaxSnapshotBytes {
		return Snapshot{}, false, fmt.Errorf("%w: snapshot grew beyond limit", ErrRecoveryLimit)
	}
	var snapshot Snapshot
	if err := decodeStrict(data, &snapshot); err != nil {
		return Snapshot{}, false, err
	}
	if snapshot.Version != SnapshotVersion {
		return Snapshot{}, false, fmt.Errorf("unsupported snapshot version %q", snapshot.Version)
	}
	if snapshot.TaskID != taskID {
		return Snapshot{}, false, fmt.Errorf("snapshot task id %q does not match %q", snapshot.TaskID, taskID)
	}
	if snapshot.JournalOffset < 0 || snapshot.CreatedAt.IsZero() {
		return Snapshot{}, false, errors.New("snapshot has invalid metadata")
	}
	if snapshot.ThroughSequence == 0 {
		if snapshot.ThroughHash != ZeroHash || snapshot.JournalOffset != 0 {
			return Snapshot{}, false, errors.New("empty snapshot has a non-empty journal boundary")
		}
	} else if !validHash(snapshot.ThroughHash) || snapshot.ThroughHash == ZeroHash {
		return Snapshot{}, false, errors.New("snapshot has invalid through hash")
	}
	expected, err := snapshotHash(snapshot)
	if err != nil {
		return Snapshot{}, false, err
	}
	if snapshot.Hash != expected {
		return Snapshot{}, false, errors.New("snapshot hash mismatch")
	}
	if snapshot.ThroughSequence > 0 && len(snapshot.State.AppliedEvents) == 0 {
		return Snapshot{}, false, errors.New("snapshot boundary has no replay deduplication state")
	}
	if err := validateState(snapshot.State, options); err != nil {
		return Snapshot{}, false, err
	}
	return snapshot, true, nil
}

func validateJournalRecord(record JournalRecord, taskID string, boundary RecoveryBoundary, options Options) error {
	if record.Version != JournalVersion {
		return fmt.Errorf("unsupported journal version %q", record.Version)
	}
	if record.TaskID != taskID {
		return fmt.Errorf("journal task id %q does not match %q", record.TaskID, taskID)
	}
	if record.Sequence != boundary.LastSequence+1 {
		return fmt.Errorf("journal sequence %d does not follow %d", record.Sequence, boundary.LastSequence)
	}
	if record.PreviousHash != boundary.LastHash {
		return errors.New("journal previous hash does not match trusted boundary")
	}
	if !validHash(record.Hash) || !validHash(record.PreviousHash) {
		return errors.New("journal record has an invalid hash")
	}
	expected, err := journalRecordHash(record)
	if err != nil {
		return err
	}
	if record.Hash != expected {
		return errors.New("journal record hash mismatch")
	}
	sanitized, err := sanitizeEvent(record.Event, options)
	if err != nil {
		return err
	}
	if !eventsEqual(record.Event, sanitized) {
		return errors.New("journal event contains unredacted or non-canonical data")
	}
	return nil
}

func journalRecoveryError(path string, boundary RecoveryBoundary, cause error) error {
	return &RecoveryError{
		Kind: RecoveryJournalCorrupt,
		Path: path,
		Boundary: boundary,
		Cause: errors.Join(ErrCorruptCheckpoint, cause),
	}
}

func encodeJournalRecord(record JournalRecord) ([]byte, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode checkpoint journal record: %w", err)
	}
	return append(encoded, '\n'), nil
}

func journalRecordHash(record JournalRecord) (string, error) {
	record.Hash = ""
	return hashJSON(record)
}

func snapshotHash(snapshot Snapshot) (string, error) {
	snapshot.Hash = ""
	return hashJSON(snapshot)
}

func hashJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:]), nil
}

func eventsEqual(left, right Event) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("checkpoint JSON contains trailing data")
		}
		return err
	}
	return nil
}

func writeSnapshotAtomic(directory string, snapshot Snapshot, options Options) error {
	if err := validateState(snapshot.State, options); err != nil {
		return err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode checkpoint snapshot: %w", err)
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > options.Limits.MaxSnapshotBytes {
		return fmt.Errorf("checkpoint snapshot is %d bytes; limit is %d", len(encoded), options.Limits.MaxSnapshotBytes)
	}
	target := filepath.Join(directory, SnapshotFileName)
	if err := rejectSymlink(target); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".snapshot-*")
	if err != nil {
		return fmt.Errorf("create checkpoint snapshot temp file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set checkpoint snapshot permissions: %w", err)
	}
	if err := writeFull(temporary, encoded); err != nil {
		temporary.Close()
		return fmt.Errorf("write checkpoint snapshot: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync checkpoint snapshot: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close checkpoint snapshot: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("atomically replace checkpoint snapshot: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync checkpoint directory after snapshot rename: %w", err)
	}
	return nil
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
