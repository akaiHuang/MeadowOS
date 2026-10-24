package productauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/keybroker"
	"github.com/hey-meadow/meadow-code/go/core/licensing"
)

const brokerStateSchema = "meadow.product-auth.keychain-state.v1"

type brokerStateDocument struct {
	Schema            string                                 `json:"schema"`
	Refresh           string                                 `json:"refresh,omitempty"`
	Lease             string                                 `json:"lease,omitempty"`
	Denial            string                                 `json:"denial,omitempty"`
	Values            map[string]string                      `json:"values,omitempty"`
	Rollback          map[string]licensing.AntiRollbackState `json:"rollback,omitempty"`
	RegisteredGeneral bool                                   `json:"registered_general,omitempty"`
}

type BrokerStore struct {
	state    *keybroker.LicenseDeviceState
	resource string
	mu       sync.Mutex
}

func NewBrokerStore(state *keybroker.LicenseDeviceState, resource string) (*BrokerStore, error) {
	if state == nil || !validOpaque(resource, 160) {
		return nil, errors.New("productauth: Keychain state binding is invalid")
	}
	return &BrokerStore{state: state, resource: resource}, nil
}

func (store *BrokerStore) LoadRefreshToken(ctx context.Context) (string, error) {
	doc, _, err := store.load(ctx)
	if err != nil {
		return "", err
	}
	if doc.Refresh == "" {
		return "", ErrRefreshNotFound
	}
	return doc.Refresh, nil
}

func (store *BrokerStore) StoreRefreshToken(ctx context.Context, token string) error {
	if token == "" || len(token) > 8192 {
		return errors.New("productauth: refresh credential has invalid size")
	}
	return store.mutate(ctx, func(doc *brokerStateDocument) error { doc.Refresh = token; return nil })
}

func (store *BrokerStore) LoadLease(ctx context.Context) (string, error) {
	doc, _, err := store.load(ctx)
	if err != nil {
		return "", err
	}
	if doc.Lease == "" {
		return "", ErrLeaseNotFound
	}
	return doc.Lease, nil
}

func (store *BrokerStore) StoreAuthorization(ctx context.Context, refresh, lease string) error {
	if refresh == "" || len(refresh) > 8192 || lease == "" || len(lease) > 32<<10 {
		return errors.New("productauth: authorization state has invalid size")
	}
	return store.mutate(ctx, func(doc *brokerStateDocument) error {
		if doc.Refresh != refresh {
			delete(doc.Values, pendingRefreshKey)
		}
		doc.Refresh, doc.Lease, doc.Denial, doc.RegisteredGeneral = refresh, lease, "", false
		return nil
	})
}

func (store *BrokerStore) loadAuthorization(ctx context.Context) (authorizationSnapshot, error) {
	doc, _, err := store.load(ctx)
	return authorizationSnapshot{refresh: doc.Refresh, lease: doc.Lease, denial: doc.Denial}, err
}

func (store *BrokerStore) storeAuthorizationIfCurrent(ctx context.Context, expected authorizationSnapshot, refresh, lease string) error {
	return store.mutate(ctx, func(doc *brokerStateDocument) error {
		if (authorizationSnapshot{refresh: doc.Refresh, lease: doc.Lease, denial: doc.Denial}) != expected {
			return errAuthorizationStoreChanged
		}
		if doc.Refresh != refresh {
			delete(doc.Values, pendingRefreshKey)
		}
		doc.Refresh, doc.Lease, doc.Denial, doc.RegisteredGeneral = refresh, lease, "", false
		return nil
	})
}

func (store *BrokerStore) denyAuthorizationIfCurrent(ctx context.Context, expected authorizationSnapshot, reason string) error {
	return store.mutate(ctx, func(doc *brokerStateDocument) error {
		if (authorizationSnapshot{refresh: doc.Refresh, lease: doc.Lease, denial: doc.Denial}) != expected {
			return errAuthorizationStoreChanged
		}
		doc.Refresh, doc.Lease, doc.Denial = "", "", reason
		delete(doc.Values, storedLeaseKey)
		delete(doc.Values, pendingRefreshKey)
		return nil
	})
}

func (store *BrokerStore) reservePendingRefresh(ctx context.Context, expected authorizationSnapshot, candidate []byte) ([]byte, error) {
	var reserved []byte
	err := store.mutate(ctx, func(doc *brokerStateDocument) error {
		if (authorizationSnapshot{refresh: doc.Refresh, lease: doc.Lease, denial: doc.Denial}) != expected {
			return errAuthorizationStoreChanged
		}
		if current, ok := doc.Values[pendingRefreshKey]; ok {
			decoded, err := base64.RawStdEncoding.DecodeString(current)
			if err != nil || base64.RawStdEncoding.EncodeToString(decoded) != current {
				return errors.New("productauth: pending refresh encoding is corrupt")
			}
			reserved = decoded
		} else {
			if doc.Values == nil {
				doc.Values = make(map[string]string)
			}
			doc.Values[pendingRefreshKey] = base64.RawStdEncoding.EncodeToString(candidate)
			reserved = append([]byte(nil), candidate...)
		}
		return nil
	})
	return reserved, err
}

func (store *BrokerStore) LoadDenial(ctx context.Context) (string, error) {
	doc, _, err := store.load(ctx)
	if err != nil {
		return "", err
	}
	if doc.Denial == "" {
		return "", ErrRefreshNotFound
	}
	return doc.Denial, nil
}

func (store *BrokerStore) StoreDenial(ctx context.Context, reason string) error {
	if !validOpaque(reason, 128) {
		return errors.New("productauth: denial reason is invalid")
	}
	return store.mutate(ctx, func(doc *brokerStateDocument) error { doc.Denial = reason; return nil })
}

func (store *BrokerStore) ClearDenial(ctx context.Context) error {
	return store.mutate(ctx, func(doc *brokerStateDocument) error { doc.Denial = ""; return nil })
}

func (store *BrokerStore) ClearAuthorization(ctx context.Context) error {
	return store.mutate(ctx, func(doc *brokerStateDocument) error {
		doc.Refresh, doc.Lease = "", ""
		delete(doc.Values, storedLeaseKey)
		delete(doc.Values, pendingRefreshKey)
		return nil
	})
}

func (store *BrokerStore) RegisteredGeneral(ctx context.Context) (bool, error) {
	doc, _, err := store.load(ctx)
	return doc.RegisteredGeneral, err
}

func (store *BrokerStore) MarkRegisteredGeneral(ctx context.Context) error {
	return store.mutate(ctx, func(doc *brokerStateDocument) error { doc.RegisteredGeneral = true; return nil })
}

func (store *BrokerStore) LoadSecureValue(ctx context.Context, key string) ([]byte, error) {
	doc, _, err := store.load(ctx)
	if err != nil {
		return nil, err
	}
	encoded, ok := doc.Values[key]
	if !ok {
		return nil, licensing.ErrSecureStoreNotFound
	}
	value, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || base64.RawStdEncoding.EncodeToString(value) != encoded {
		return nil, errors.New("productauth: Keychain secure value is corrupt")
	}
	return value, nil
}

func (store *BrokerStore) StoreSecureValues(ctx context.Context, values map[string][]byte) error {
	return store.mutate(ctx, func(doc *brokerStateDocument) error {
		if doc.Values == nil {
			doc.Values = make(map[string]string)
		}
		for key, value := range values {
			if key == "" || len(value) == 0 {
				return errors.New("productauth: secure value is invalid")
			}
			doc.Values[key] = base64.RawStdEncoding.EncodeToString(value)
		}
		return nil
	})
}

func (store *BrokerStore) LoadAntiRollback(ctx context.Context, product string) (licensing.AntiRollbackState, error) {
	doc, _, err := store.load(ctx)
	if err != nil {
		return licensing.AntiRollbackState{}, err
	}
	state, ok := doc.Rollback[product]
	if !ok {
		return licensing.AntiRollbackState{}, licensing.ErrAntiRollbackStateNotFound
	}
	return state, nil
}

func (store *BrokerStore) CompareAndSwapAntiRollback(ctx context.Context, product string, previous *licensing.AntiRollbackState, next licensing.AntiRollbackState) error {
	if snapshot := store.statusSnapshot(ctx); snapshot != nil {
		return store.compareAndSwapStatusRollback(ctx, snapshot, product, previous, next)
	}
	return store.mutate(ctx, func(doc *brokerStateDocument) error {
		if doc.Rollback == nil {
			doc.Rollback = make(map[string]licensing.AntiRollbackState)
		}
		current, exists := doc.Rollback[product]
		if previous == nil {
			if exists {
				return licensing.ErrAntiRollbackConflict
			}
		} else if !exists || current != *previous {
			return licensing.ErrAntiRollbackConflict
		}
		doc.Rollback[product] = next
		return nil
	})
}

func (store *BrokerStore) mutate(ctx context.Context, update func(*brokerStateDocument) error) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for attempt := 0; attempt < 4; attempt++ {
		doc, version, err := store.loadUnlocked(ctx)
		if err != nil {
			return err
		}
		if err := update(&doc); err != nil {
			return err
		}
		payload, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		_, err = store.state.Replace(ctx, store.resource, version, payload)
		clear(payload)
		if errors.Is(err, keybroker.ErrVersionConflict) {
			continue
		}
		return err
	}
	return keybroker.ErrVersionConflict
}

func (store *BrokerStore) load(ctx context.Context) (brokerStateDocument, keybroker.Version, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if snapshot := store.statusSnapshot(ctx); snapshot != nil {
		if err := ctx.Err(); err != nil {
			return brokerStateDocument{}, 0, err
		}
		if snapshot.invalid {
			return brokerStateDocument{}, 0, errAuthorizationStoreChanged
		}
		return snapshot.doc, snapshot.version, nil
	}
	return store.loadUnlocked(ctx)
}

func (store *BrokerStore) loadUnlocked(ctx context.Context) (brokerStateDocument, keybroker.Version, error) {
	lease, err := store.state.Current(ctx, store.resource)
	if errors.Is(err, keybroker.ErrNotFound) {
		return emptyBrokerState(), 0, nil
	}
	if err != nil {
		return brokerStateDocument{}, 0, err
	}
	defer lease.Destroy()
	doc := brokerStateDocument{}
	if err := lease.WithBytes(func(payload []byte) error {
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&doc); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return errors.New("trailing Keychain state")
		}
		return nil
	}); err != nil {
		return brokerStateDocument{}, 0, fmt.Errorf("productauth: decode Keychain state: %w", err)
	}
	if doc.Schema != brokerStateSchema {
		return brokerStateDocument{}, 0, errors.New("productauth: Keychain state schema is unsupported")
	}
	if doc.Values == nil {
		doc.Values = make(map[string]string)
	}
	if doc.Rollback == nil {
		doc.Rollback = make(map[string]licensing.AntiRollbackState)
	}
	return doc, lease.Version(), nil
}

func emptyBrokerState() brokerStateDocument {
	return brokerStateDocument{Schema: brokerStateSchema, Values: make(map[string]string), Rollback: make(map[string]licensing.AntiRollbackState)}
}
