package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const TaskProgressSchemaVersion = "meadow.checkpoint.task-progress.v1"

type ProgressItem struct {
	ID string `json:"id"`
	Summary string `json:"summary"`
}

type FailedAttempt struct {
	AttemptID string `json:"attempt_id"`
	AttemptNumber uint64 `json:"attempt_number"`
	Action string `json:"action"`
	ErrorCode string `json:"error_code"`
	Summary string `json:"summary"`
	Strategy string `json:"strategy,omitempty"`
	At time.Time `json:"at"`
}

// TaskProgressState is the durable, strict crash-recovery domain. It stores
// summaries and stable IDs only; raw prompts, tool arguments and outputs stay
// in ContentReference-bearing event types.
type TaskProgressState struct {
	SchemaVersion string `json:"schema_version"`
	Goal string `json:"goal"`
	Completed []ProgressItem `json:"completed"`
	Pending []ProgressItem `json:"pending"`
	FailedAttempts []FailedAttempt `json:"failed_attempts"`
	RetryCount uint64 `json:"retry_count"`
	NextStrategy string `json:"next_strategy,omitempty"`
}

// TaskProgressUpdate is an optimistic, replay-protected replacement of the
// complete task progress image. ExpectedLastEventID and digest form an
// application-level link on top of the journal record hash chain.
type TaskProgressUpdate struct {
	SchemaVersion string `json:"schema_version"`
	IdempotencyKey string `json:"idempotency_key"`
	ExpectedRevision uint64 `json:"expected_revision"`
	ExpectedLastEventID string `json:"expected_last_event_id,omitempty"`
	ExpectedLastEventDigest string `json:"expected_last_event_digest,omitempty"`
	Progress TaskProgressState `json:"progress"`
}

type IdempotencyRecord struct {
	Key string `json:"key"`
	EventID string `json:"event_id"`
	MutationDigest string `json:"mutation_digest"`
	ProgressRevision uint64 `json:"progress_revision"`
}

type AppliedEventCursor struct {
	EventID string `json:"event_id"`
	Kind EventKind `json:"kind"`
	Digest string `json:"digest"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	ProgressRevision uint64 `json:"progress_revision,omitempty"`
	At time.Time `json:"at"`
}

type ProgressReplayToken struct {
	Revision uint64 `json:"revision"`
	LastEventID string `json:"last_event_id,omitempty"`
	LastEventDigest string `json:"last_event_digest,omitempty"`
}

type ProgressAppendRequest struct {
	EventID string
	At time.Time
	IdempotencyKey string
	Token ProgressReplayToken
	Progress TaskProgressState
}

type ProgressReplayDecision string

const (
	ProgressReplayApply ProgressReplayDecision = "apply"
	ProgressReplayDuplicate ProgressReplayDecision = "duplicate"
)

// TaskProgressToken returns the optimistic replay token callers must attach to
// the next progress update. A zero token is valid for the first domain event.
func TaskProgressToken(state State) ProgressReplayToken {
	token := ProgressReplayToken{Revision: state.ProgressRevision}
	if state.LastAppliedEvent != nil {
		token.LastEventID = state.LastAppliedEvent.EventID
		token.LastEventDigest = state.LastAppliedEvent.Digest
	}
	return token
}

// AppendTaskProgress persists one complete progress image through the existing
// Store journal. It does not connect any central journal or session runtime.
func (s *Store) AppendTaskProgress(request ProgressAppendRequest) (AppendResult, error) {
	event := Event{
		ID: request.EventID,
		Kind: EventTaskProgress,
		At: request.At,
		TaskProgress: &TaskProgressUpdate{
			SchemaVersion: TaskProgressSchemaVersion,
			IdempotencyKey: request.IdempotencyKey,
			ExpectedRevision: request.Token.Revision,
			ExpectedLastEventID: request.Token.LastEventID,
			ExpectedLastEventDigest: request.Token.LastEventDigest,
			Progress: request.Progress,
		},
	}
	return s.Append(event)
}

// EvaluateTaskProgressEvent is the side-effect-free anti-replay gate. Exact
// retries of an idempotency key are duplicates; changed reuse, stale revisions
// and broken event links fail closed.
func EvaluateTaskProgressEvent(state State, event Event) (ProgressReplayDecision, error) {
	if event.Kind != EventTaskProgress || event.TaskProgress == nil {
		return "", errors.New("anti-replay evaluation requires a task progress event")
	}
	update := event.TaskProgress
	if err := validateTaskProgressUpdate(update); err != nil {
		return "", err
	}
	mutationDigest, err := taskProgressMutationDigest(*update)
	if err != nil {
		return "", err
	}
	if previous, exists := state.IdempotencyKeys[update.IdempotencyKey]; exists {
		if previous.MutationDigest != mutationDigest {
			return "", fmt.Errorf("%w: %s", ErrIdempotencyConflict, update.IdempotencyKey)
		}
		return ProgressReplayDuplicate, nil
	}
	if update.ExpectedRevision != state.ProgressRevision {
		return "", fmt.Errorf("%w: expected %d, current %d", ErrStaleProgressRevision, update.ExpectedRevision, state.ProgressRevision)
	}
	current := TaskProgressToken(state)
	if update.ExpectedLastEventID != current.LastEventID || update.ExpectedLastEventDigest != current.LastEventDigest {
		return "", fmt.Errorf("%w: expected event %q at current event %q", ErrReplayRejected, update.ExpectedLastEventID, current.LastEventID)
	}
	return ProgressReplayApply, nil
}

func applyTaskProgress(state *State, update TaskProgressUpdate, eventDigest string, event Event) {
	progress := cloneTaskProgress(&update.Progress)
	state.TaskProgress = progress
	state.ProgressRevision++
	mutationDigest, _ := taskProgressMutationDigest(update)
	state.IdempotencyKeys[update.IdempotencyKey] = IdempotencyRecord{
		Key: update.IdempotencyKey,
		EventID: event.ID,
		MutationDigest: mutationDigest,
		ProgressRevision: state.ProgressRevision,
	}
}

func cloneTaskProgress(progress *TaskProgressState) *TaskProgressState {
	if progress == nil {
		return nil
	}
	cloned := *progress
	cloned.Completed = append([]ProgressItem(nil), progress.Completed...)
	cloned.Pending = append([]ProgressItem(nil), progress.Pending...)
	cloned.FailedAttempts = append([]FailedAttempt(nil), progress.FailedAttempts...)
	return &cloned
}

func taskProgressIdempotencyKey(event Event) string {
	if event.TaskProgress == nil {
		return ""
	}
	return event.TaskProgress.IdempotencyKey
}

func taskProgressMutationDigest(update TaskProgressUpdate) (string, error) {
	identity := struct {
		SchemaVersion string `json:"schema_version"`
		Progress TaskProgressState `json:"progress"`
	}{update.SchemaVersion, update.Progress}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode task progress identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validateTaskProgressUpdate(update *TaskProgressUpdate) error {
	if update == nil || update.SchemaVersion != TaskProgressSchemaVersion {
		return errors.New("invalid task progress schema version")
	}
	if update.IdempotencyKey == "" {
		return errors.New("task progress idempotency key is required")
	}
	linked := update.ExpectedLastEventID != "" || update.ExpectedLastEventDigest != ""
	if linked && (update.ExpectedLastEventID == "" || !validHash(update.ExpectedLastEventDigest)) {
		return errors.New("task progress previous event link is incomplete")
	}
	if !linked && update.ExpectedRevision != 0 {
		return errors.New("task progress revision has no previous event link")
	}
	return validateTaskProgressState(update.Progress)
}

func validateTaskProgressState(progress TaskProgressState) error {
	if progress.SchemaVersion != TaskProgressSchemaVersion || strings.TrimSpace(progress.Goal) == "" {
		return errors.New("task progress requires a versioned non-empty goal")
	}
	items := make(map[string]string, len(progress.Completed)+len(progress.Pending))
	for _, group := range []struct {
		name string
		values []ProgressItem
	}{{"completed", progress.Completed}, {"pending", progress.Pending}} {
		for _, item := range group.values {
			if item.ID == "" || strings.TrimSpace(item.Summary) == "" {
				return fmt.Errorf("task progress %s item is incomplete", group.name)
			}
			if previous, exists := items[item.ID]; exists {
				return fmt.Errorf("task progress item %s appears in both %s and %s", item.ID, previous, group.name)
			}
			items[item.ID] = group.name
		}
	}
	if len(progress.Pending) > 0 && strings.TrimSpace(progress.NextStrategy) == "" {
		return errors.New("task progress with pending work requires next strategy")
	}
	attempts := make(map[string]struct{}, len(progress.FailedAttempts))
	var highest uint64
	for _, attempt := range progress.FailedAttempts {
		if attempt.AttemptID == "" || attempt.AttemptNumber == 0 || strings.TrimSpace(attempt.Action) == "" || strings.TrimSpace(attempt.ErrorCode) == "" || strings.TrimSpace(attempt.Summary) == "" || attempt.At.IsZero() {
			return errors.New("task progress contains an incomplete failed attempt")
		}
		if _, exists := attempts[attempt.AttemptID]; exists {
			return fmt.Errorf("task progress failed attempt %s is duplicated", attempt.AttemptID)
		}
		attempts[attempt.AttemptID] = struct{}{}
		if attempt.AttemptNumber > highest {
			highest = attempt.AttemptNumber
		}
	}
	if progress.RetryCount < highest {
		return errors.New("task progress retry count precedes a failed attempt")
	}
	return nil
}

func validateAppliedEventCursor(cursor AppliedEventCursor) error {
	if cursor.EventID == "" || cursor.At.IsZero() || !validHash(cursor.Digest) || !validEventKind(cursor.Kind) {
		return errors.New("snapshot has invalid last applied event")
	}
	return nil
}

func validEventKind(kind EventKind) bool {
	switch kind {
	case EventTurn, EventToolCall, EventToolResult, EventCancel, EventModelGeneration, EventPermission, EventTerminal, EventTaskProgress:
		return true
	default:
		return false
	}
}

func sanitizeTaskProgressUpdate(update *TaskProgressUpdate, options Options) error {
	var err error
	if update.IdempotencyKey, err = sanitizeIdentifier("task progress idempotency key", update.IdempotencyKey, options.Redactor); err != nil {
		return err
	}
	if update.ExpectedLastEventID != "" {
		if update.ExpectedLastEventID, err = sanitizeIdentifier("task progress previous event id", update.ExpectedLastEventID, options.Redactor); err != nil {
			return err
		}
		update.ExpectedLastEventDigest = strings.ToLower(update.ExpectedLastEventDigest)
	}
	progress := &update.Progress
	progress.Goal, _ = redactAndBound(progress.Goal, options)
	progress.NextStrategy, _ = redactAndBound(progress.NextStrategy, options)
	for index := range progress.Completed {
		if progress.Completed[index].ID, err = sanitizeIdentifier("completed item id", progress.Completed[index].ID, options.Redactor); err != nil {
			return err
		}
		progress.Completed[index].Summary, _ = redactAndBound(progress.Completed[index].Summary, options)
	}
	for index := range progress.Pending {
		if progress.Pending[index].ID, err = sanitizeIdentifier("pending item id", progress.Pending[index].ID, options.Redactor); err != nil {
			return err
		}
		progress.Pending[index].Summary, _ = redactAndBound(progress.Pending[index].Summary, options)
	}
	for index := range progress.FailedAttempts {
		attempt := &progress.FailedAttempts[index]
		if attempt.AttemptID, err = sanitizeIdentifier("failed attempt id", attempt.AttemptID, options.Redactor); err != nil {
			return err
		}
		if attempt.ErrorCode, err = sanitizeLabel(attempt.ErrorCode, options); err != nil {
			return err
		}
		attempt.Action, _ = redactAndBound(attempt.Action, options)
		attempt.Summary, _ = redactAndBound(attempt.Summary, options)
		attempt.Strategy, _ = redactAndBound(attempt.Strategy, options)
		attempt.At = attempt.At.UTC()
	}
	return nil
}
