package protectedbootstrap

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/keybroker"
	"github.com/hey-meadow/meadow-code/go/core/licensing"
)

const (
	licenseStateSchema       = 1
	maxLicenseStateBytes     = 900 << 10
	maxSecureStateEntries    = 16
	maxAntiRollbackEntries   = 8
	maxLicenseStateKeyBytes  = 512
)

var (
	licenseStateMagic = [8]byte{'M', 'E', 'A', 'D', 'L', 'I', 'C', 1}
	errLicenseState   = errors.New("protected license state unavailable")
)

type licenseSnapshot struct {
	secure       map[string][]byte
	antiRollback map[string]licensing.AntiRollbackState
}

func emptyLicenseSnapshot() licenseSnapshot {
	return licenseSnapshot{
		secure:       make(map[string][]byte),
		antiRollback: make(map[string]licensing.AntiRollbackState),
	}
}

func (snapshot *licenseSnapshot) destroy() {
	if snapshot == nil {
		return
	}
	for key, value := range snapshot.secure {
		clear(value)
		delete(snapshot.secure, key)
	}
	for key := range snapshot.antiRollback {
		delete(snapshot.antiRollback, key)
	}
}

type licenseStateStore struct {
	mu       sync.Mutex
	api      *keybroker.LicenseDeviceState
	resource string
}

func newLicenseStateStore(api *keybroker.LicenseDeviceState, resource string) *licenseStateStore {
	return &licenseStateStore{api: api, resource: resource}
}

func (store *licenseStateStore) LoadSecureValue(ctx context.Context, key string) ([]byte, error) {
	if !validLicenseStateKey(key) {
		return nil, errLicenseState
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	snapshot, _, err := store.load(ctx)
	if err != nil {
		return nil, err
	}
	defer snapshot.destroy()
	value, ok := snapshot.secure[key]
	if !ok {
		return nil, licensing.ErrSecureStoreNotFound
	}
	result := make([]byte, len(value))
	copy(result, value)
	return result, nil
}

func (store *licenseStateStore) StoreSecureValues(ctx context.Context, values map[string][]byte) error {
	if len(values) == 0 || len(values) > maxSecureStateEntries {
		return errLicenseState
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	snapshot, version, err := store.load(ctx)
	if err != nil {
		return err
	}
	defer snapshot.destroy()
	for key, value := range values {
		if !validLicenseStateKey(key) || len(value) == 0 || len(value) > maxLicenseStateBytes {
			return errLicenseState
		}
		copyOfValue := make([]byte, len(value))
		copy(copyOfValue, value)
		if previous := snapshot.secure[key]; previous != nil {
			clear(previous)
		}
		snapshot.secure[key] = copyOfValue
	}
	return store.replace(ctx, snapshot, version)
}

func (store *licenseStateStore) LoadAntiRollback(ctx context.Context, product string) (licensing.AntiRollbackState, error) {
	if !validLicenseStateKey(product) {
		return licensing.AntiRollbackState{}, errLicenseState
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	snapshot, _, err := store.load(ctx)
	if err != nil {
		return licensing.AntiRollbackState{}, err
	}
	defer snapshot.destroy()
	state, ok := snapshot.antiRollback[product]
	if !ok {
		return licensing.AntiRollbackState{}, licensing.ErrAntiRollbackStateNotFound
	}
	return state, nil
}

func (store *licenseStateStore) CompareAndSwapAntiRollback(
	ctx context.Context,
	product string,
	previous *licensing.AntiRollbackState,
	next licensing.AntiRollbackState,
) error {
	if !validLicenseStateKey(product) || next.ObservedUnix <= 0 || next.RevocationEpoch == 0 {
		return errLicenseState
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	snapshot, version, err := store.load(ctx)
	if err != nil {
		return err
	}
	defer snapshot.destroy()
	current, exists := snapshot.antiRollback[product]
	if previous == nil {
		if exists {
			return licensing.ErrAntiRollbackConflict
		}
	} else if !exists || current != *previous {
		return licensing.ErrAntiRollbackConflict
	}
	snapshot.antiRollback[product] = next
	err = store.replace(ctx, snapshot, version)
	if errors.Is(err, keybroker.ErrVersionConflict) {
		return licensing.ErrAntiRollbackConflict
	}
	return err
}

func (store *licenseStateStore) load(ctx context.Context) (licenseSnapshot, keybroker.Version, error) {
	if store == nil || store.api == nil || ctx == nil {
		return licenseSnapshot{}, 0, errLicenseState
	}
	lease, err := store.api.Current(ctx, store.resource)
	if errors.Is(err, keybroker.ErrNotFound) {
		return emptyLicenseSnapshot(), 0, nil
	}
	if err != nil {
		return licenseSnapshot{}, 0, errLicenseState
	}
	defer lease.Destroy()
	version := lease.Version()
	var snapshot licenseSnapshot
	err = lease.WithBytes(func(encoded []byte) error {
		decoded, decodeErr := decodeLicenseSnapshot(encoded)
		if decodeErr != nil {
			return decodeErr
		}
		snapshot = decoded
		return nil
	})
	if err != nil {
		snapshot.destroy()
		return licenseSnapshot{}, 0, errLicenseState
	}
	return snapshot, version, nil
}

func (store *licenseStateStore) replace(ctx context.Context, snapshot licenseSnapshot, expected keybroker.Version) error {
	encoded, err := encodeLicenseSnapshot(snapshot)
	if err != nil {
		return errLicenseState
	}
	defer clear(encoded)
	_, err = store.api.Replace(ctx, store.resource, expected, encoded)
	if err != nil {
		return err
	}
	return nil
}

func encodeLicenseSnapshot(snapshot licenseSnapshot) ([]byte, error) {
	if len(snapshot.secure) > maxSecureStateEntries || len(snapshot.antiRollback) > maxAntiRollbackEntries {
		return nil, errLicenseState
	}
	encoded := make([]byte, 0, 1024)
	encoded = append(encoded, licenseStateMagic[:]...)
	encoded = appendUint16(encoded, licenseStateSchema)
	secureKeys := sortedKeys(snapshot.secure)
	encoded = appendUint16(encoded, uint16(len(secureKeys)))
	for _, key := range secureKeys {
		value := snapshot.secure[key]
		if !validLicenseStateKey(key) || len(value) == 0 {
			clear(encoded)
			return nil, errLicenseState
		}
		encoded = appendBytes16(encoded, []byte(key))
		encoded = appendBytes32(encoded, value)
	}
	antiKeys := make([]string, 0, len(snapshot.antiRollback))
	for key := range snapshot.antiRollback {
		antiKeys = append(antiKeys, key)
	}
	sort.Strings(antiKeys)
	encoded = appendUint16(encoded, uint16(len(antiKeys)))
	for _, key := range antiKeys {
		state := snapshot.antiRollback[key]
		if !validLicenseStateKey(key) || state.ObservedUnix <= 0 || state.RevocationEpoch == 0 {
			clear(encoded)
			return nil, errLicenseState
		}
		encoded = appendBytes16(encoded, []byte(key))
		encoded = appendUint64(encoded, uint64(state.ObservedUnix))
		encoded = appendUint64(encoded, state.RevocationEpoch)
	}
	if len(encoded) > maxLicenseStateBytes {
		clear(encoded)
		return nil, errLicenseState
	}
	return encoded, nil
}

func decodeLicenseSnapshot(encoded []byte) (licenseSnapshot, error) {
	if len(encoded) < len(licenseStateMagic)+6 || len(encoded) > maxLicenseStateBytes ||
		subtle.ConstantTimeCompare(encoded[:len(licenseStateMagic)], licenseStateMagic[:]) != 1 {
		return licenseSnapshot{}, errLicenseState
	}
	cursor := stateCursor{data: encoded[len(licenseStateMagic):]}
	schema, ok := cursor.uint16()
	if !ok || schema != licenseStateSchema {
		return licenseSnapshot{}, errLicenseState
	}
	snapshot := emptyLicenseSnapshot()
	secureCount, ok := cursor.uint16()
	if !ok || secureCount > maxSecureStateEntries {
		return licenseSnapshot{}, errLicenseState
	}
	previousKey := ""
	for index := 0; index < int(secureCount); index++ {
		keyBytes, ok := cursor.bytes16()
		if !ok {
			snapshot.destroy()
			return licenseSnapshot{}, errLicenseState
		}
		key := string(keyBytes)
		value, ok := cursor.bytes32()
		if !ok || !validLicenseStateKey(key) || key <= previousKey || len(value) == 0 {
			snapshot.destroy()
			return licenseSnapshot{}, errLicenseState
		}
		previousKey = key
		snapshot.secure[key] = value
	}
	antiCount, ok := cursor.uint16()
	if !ok || antiCount > maxAntiRollbackEntries {
		snapshot.destroy()
		return licenseSnapshot{}, errLicenseState
	}
	previousKey = ""
	for index := 0; index < int(antiCount); index++ {
		keyBytes, ok := cursor.bytes16()
		if !ok {
			snapshot.destroy()
			return licenseSnapshot{}, errLicenseState
		}
		observed, observedOK := cursor.uint64()
		epoch, epochOK := cursor.uint64()
		key := string(keyBytes)
		if !observedOK || !epochOK || !validLicenseStateKey(key) || key <= previousKey ||
			int64(observed) <= 0 || epoch == 0 {
			snapshot.destroy()
			return licenseSnapshot{}, errLicenseState
		}
		previousKey = key
		snapshot.antiRollback[key] = licensing.AntiRollbackState{
			ObservedUnix:    int64(observed),
			RevocationEpoch: epoch,
		}
	}
	if len(cursor.data) != 0 {
		snapshot.destroy()
		return licenseSnapshot{}, errLicenseState
	}
	return snapshot, nil
}

type stateCursor struct {
	data []byte
}

func (cursor *stateCursor) take(size int) ([]byte, bool) {
	if size < 0 || size > len(cursor.data) {
		return nil, false
	}
	value := cursor.data[:size]
	cursor.data = cursor.data[size:]
	return value, true
}

func (cursor *stateCursor) uint16() (uint16, bool) {
	value, ok := cursor.take(2)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint16(value), true
}

func (cursor *stateCursor) uint64() (uint64, bool) {
	value, ok := cursor.take(8)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint64(value), true
}

func (cursor *stateCursor) bytes16() ([]byte, bool) {
	size, ok := cursor.uint16()
	if !ok || size == 0 || int(size) > maxLicenseStateKeyBytes {
		return nil, false
	}
	value, ok := cursor.take(int(size))
	if !ok {
		return nil, false
	}
	copyOfValue := make([]byte, len(value))
	copy(copyOfValue, value)
	return copyOfValue, true
}

func (cursor *stateCursor) bytes32() ([]byte, bool) {
	lengthBytes, ok := cursor.take(4)
	if !ok {
		return nil, false
	}
	size := binary.BigEndian.Uint32(lengthBytes)
	if size == 0 || size > maxLicenseStateBytes {
		return nil, false
	}
	value, ok := cursor.take(int(size))
	if !ok {
		return nil, false
	}
	copyOfValue := make([]byte, len(value))
	copy(copyOfValue, value)
	return copyOfValue, true
}

func appendUint16(destination []byte, value uint16) []byte {
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	return append(destination, encoded[:]...)
}

func appendUint64(destination []byte, value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(destination, encoded[:]...)
}

func appendBytes16(destination []byte, value []byte) []byte {
	destination = appendUint16(destination, uint16(len(value)))
	return append(destination, value...)
}

func appendBytes32(destination []byte, value []byte) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], uint32(len(value)))
	destination = append(destination, encoded[:]...)
	return append(destination, value...)
}

func sortedKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validLicenseStateKey(value string) bool {
	return value != "" && len(value) <= maxLicenseStateKeyBytes &&
		utf8.ValidString(value) && value == strings.TrimSpace(value) &&
		strings.IndexByte(value, 0) < 0
}
