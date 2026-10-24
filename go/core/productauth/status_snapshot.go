package productauth

import (
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/hey-meadow/meadow-code/go/core/keybroker"
	"github.com/hey-meadow/meadow-code/go/core/licensing"
)

// A status snapshot lives only for one synchronous Status attempt. The fresh
// final version check is required before returning any authenticated result.
// Nothing here caches an authorization decision between requests.
type statusSnapshotStore interface {
	beginStatusSnapshot(context.Context) (context.Context, error)
	finishStatusSnapshot(context.Context) error
	endStatusSnapshot(context.Context)
}

type statusSnapshotContextKey struct{}

type brokerStatusSnapshot struct {
	store   *BrokerStore
	doc     brokerStateDocument
	version keybroker.Version
	invalid bool
}

func (store *BrokerStore) beginStatusSnapshot(ctx context.Context) (context.Context, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	doc, version, err := store.loadUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, statusSnapshotContextKey{}, &brokerStatusSnapshot{
		store: store, doc: doc, version: version,
	}), nil
}

func (store *BrokerStore) statusSnapshot(ctx context.Context) *brokerStatusSnapshot {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Value(statusSnapshotContextKey{}).(*brokerStatusSnapshot)
	if value == nil || value.store != store {
		return nil
	}
	return value
}

func (store *BrokerStore) finishStatusSnapshot(ctx context.Context) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	snapshot := store.statusSnapshot(ctx)
	if snapshot == nil || snapshot.invalid {
		return errAuthorizationStoreChanged
	}
	doc, version, err := store.loadUnlocked(ctx)
	if err != nil {
		return err
	}
	if version != snapshot.version || !sameBrokerStateDocument(doc, snapshot.doc) {
		snapshot.invalid = true
		return errAuthorizationStoreChanged
	}
	return nil
}

func sameBrokerStateDocument(left, right brokerStateDocument) bool {
	return left.Schema == right.Schema && left.Refresh == right.Refresh &&
		left.Lease == right.Lease && left.Denial == right.Denial &&
		left.RegisteredGeneral == right.RegisteredGeneral &&
		maps.Equal(left.Values, right.Values) && maps.Equal(left.Rollback, right.Rollback)
}

func (store *BrokerStore) endStatusSnapshot(ctx context.Context) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if snapshot := store.statusSnapshot(ctx); snapshot != nil {
		snapshot.invalid = true
		snapshot.doc = brokerStateDocument{}
		snapshot.version = 0
	}
}

// The broker's Replace checks the latest immutable version before appending.
// CAS the entire request document directly instead of reopening it twice. A
// competing writer invalidates the whole verification, so a stale snapshot can
// never update its successor's authorization or rollback highwater.
func (store *BrokerStore) compareAndSwapStatusRollback(ctx context.Context, snapshot *brokerStatusSnapshot, product string, previous *licensing.AntiRollbackState, next licensing.AntiRollbackState) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if snapshot.invalid {
		return errAuthorizationStoreChanged
	}
	current, exists := snapshot.doc.Rollback[product]
	if previous == nil {
		if exists {
			return licensing.ErrAntiRollbackConflict
		}
	} else if !exists || current != *previous {
		return licensing.ErrAntiRollbackConflict
	}
	doc := snapshot.doc
	doc.Rollback = make(map[string]licensing.AntiRollbackState, len(snapshot.doc.Rollback)+1)
	for key, value := range snapshot.doc.Rollback {
		doc.Rollback[key] = value
	}
	doc.Rollback[product] = next
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	defer clear(payload)
	version, err := store.state.Replace(ctx, store.resource, snapshot.version, payload)
	if errors.Is(err, keybroker.ErrVersionConflict) {
		snapshot.invalid = true
		return errAuthorizationStoreChanged
	}
	if err != nil {
		return err
	}
	snapshot.doc, snapshot.version = doc, version
	return nil
}
