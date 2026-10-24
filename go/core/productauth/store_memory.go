package productauth

import (
	"context"
	"errors"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/licensing"
)

type MemoryStore struct {
	mu       sync.Mutex
	refresh  string
	lease    string
	denial   string
	values   map[string][]byte
	rollback map[string]licensing.AntiRollbackState
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{values: make(map[string][]byte), rollback: make(map[string]licensing.AntiRollbackState)}
}

func (store *MemoryStore) LoadRefreshToken(context.Context) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.refresh == "" {
		return "", ErrRefreshNotFound
	}
	return store.refresh, nil
}

func (store *MemoryStore) LoadLease(context.Context) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lease == "" {
		return "", ErrLeaseNotFound
	}
	return store.lease, nil
}

func (store *MemoryStore) StoreAuthorization(_ context.Context, refresh, lease string) error {
	if refresh == "" || lease == "" {
		return errors.New("productauth: incomplete authorization state")
	}
	store.mu.Lock()
	if store.refresh != refresh {
		delete(store.values, pendingRefreshKey)
	}
	store.refresh, store.lease, store.denial = refresh, lease, ""
	store.mu.Unlock()
	return nil
}

func (store *MemoryStore) loadAuthorization(context.Context) (authorizationSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return authorizationSnapshot{refresh: store.refresh, lease: store.lease, denial: store.denial}, nil
}

func (store *MemoryStore) storeAuthorizationIfCurrent(_ context.Context, expected authorizationSnapshot, refresh, lease string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if (authorizationSnapshot{refresh: store.refresh, lease: store.lease, denial: store.denial}) != expected {
		return errAuthorizationStoreChanged
	}
	if store.refresh != refresh {
		delete(store.values, pendingRefreshKey)
	}
	store.refresh, store.lease, store.denial = refresh, lease, ""
	return nil
}

func (store *MemoryStore) denyAuthorizationIfCurrent(_ context.Context, expected authorizationSnapshot, reason string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if (authorizationSnapshot{refresh: store.refresh, lease: store.lease, denial: store.denial}) != expected {
		return errAuthorizationStoreChanged
	}
	store.refresh, store.lease, store.denial = "", "", reason
	delete(store.values, storedLeaseKey)
	delete(store.values, pendingRefreshKey)
	return nil
}

func (store *MemoryStore) reservePendingRefresh(_ context.Context, expected authorizationSnapshot, candidate []byte) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if (authorizationSnapshot{refresh: store.refresh, lease: store.lease, denial: store.denial}) != expected {
		return nil, errAuthorizationStoreChanged
	}
	if current, ok := store.values[pendingRefreshKey]; ok {
		return append([]byte(nil), current...), nil
	}
	store.values[pendingRefreshKey] = append([]byte(nil), candidate...)
	return append([]byte(nil), candidate...), nil
}

func (store *MemoryStore) LoadDenial(context.Context) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.denial == "" {
		return "", ErrRefreshNotFound
	}
	return store.denial, nil
}

func (store *MemoryStore) StoreDenial(_ context.Context, reason string) error {
	store.mu.Lock()
	store.denial = reason
	store.mu.Unlock()
	return nil
}

func (store *MemoryStore) ClearDenial(context.Context) error {
	store.mu.Lock()
	store.denial = ""
	store.mu.Unlock()
	return nil
}

func (store *MemoryStore) ClearAuthorization(context.Context) error {
	store.mu.Lock()
	store.refresh, store.lease = "", ""
	delete(store.values, storedLeaseKey)
	delete(store.values, pendingRefreshKey)
	store.mu.Unlock()
	return nil
}

func (store *MemoryStore) LoadSecureValue(_ context.Context, key string) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.values[key]
	if !ok {
		return nil, licensing.ErrSecureStoreNotFound
	}
	return append([]byte(nil), value...), nil
}

func (store *MemoryStore) StoreSecureValues(_ context.Context, values map[string][]byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for key, value := range values {
		store.values[key] = append([]byte(nil), value...)
	}
	return nil
}

func (store *MemoryStore) LoadAntiRollback(_ context.Context, product string) (licensing.AntiRollbackState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	state, ok := store.rollback[product]
	if !ok {
		return licensing.AntiRollbackState{}, licensing.ErrAntiRollbackStateNotFound
	}
	return state, nil
}

func (store *MemoryStore) CompareAndSwapAntiRollback(_ context.Context, product string, previous *licensing.AntiRollbackState, next licensing.AntiRollbackState) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	current, exists := store.rollback[product]
	if previous == nil {
		if exists {
			return licensing.ErrAntiRollbackConflict
		}
	} else if !exists || current != *previous {
		return licensing.ErrAntiRollbackConflict
	}
	store.rollback[product] = next
	return nil
}
