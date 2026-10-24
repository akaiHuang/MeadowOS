package keybroker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"
)

// Version identifies an immutable namespace record. Version zero means that
// no record has been created.
type Version uint64

var (
	ErrUnavailable           = errors.New("keybroker backend unavailable")
	ErrInteractionRequired   = errors.New("keybroker user interaction required")
	ErrNotFound              = errors.New("keybroker secret not found")
	ErrVersionConflict       = errors.New("keybroker version conflict")
	ErrDeviceBindingMismatch = errors.New("keybroker device binding mismatch")
	ErrCorruptRecord         = errors.New("keybroker record is corrupt")
	ErrInvalidArgument       = errors.New("keybroker argument is invalid")
	ErrDestroyed             = errors.New("keybroker lease is destroyed")
	ErrClosed                = errors.New("keybroker is closed")
)

// DeviceBinding is an opaque digest of an attested, stable device identity.
// The raw device identity must be resolved outside this package and must not
// be passed through command arguments or environment variables.
type DeviceBinding struct {
	digest [32]byte
}

// DeviceBindingFromDigest accepts a SHA-256-sized digest produced by the
// trusted device identity provider. An all-zero digest is rejected.
func DeviceBindingFromDigest(digest [32]byte) (DeviceBinding, error) {
	if isZeroDigest(digest) {
		return DeviceBinding{}, ErrInvalidArgument
	}
	return DeviceBinding{digest: digest}, nil
}

func isZeroDigest(digest [32]byte) bool {
	var zero [32]byte
	return subtle.ConstantTimeCompare(digest[:], zero[:]) == 1
}

// Lease owns sensitive bytes returned by a namespace API. Call Destroy as
// soon as possible. WithBytes provides a temporary copy which is zeroed when
// the callback returns; the callback must not retain or copy it.
type Lease struct {
	mu        sync.Mutex
	value     []byte
	version   Version
	destroyed bool
}

func newLease(version Version, value []byte) *Lease {
	lease := &Lease{value: value, version: version}
	runtime.SetFinalizer(lease, func(value *Lease) {
		value.Destroy()
	})
	return lease
}

// Version returns the immutable record version represented by the lease.
func (l *Lease) Version() Version {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.version
}

// WithBytes invokes fn with a temporary copy of the secret.
func (l *Lease) WithBytes(fn func([]byte) error) error {
	if l == nil || fn == nil {
		return ErrInvalidArgument
	}
	l.mu.Lock()
	if l.destroyed {
		l.mu.Unlock()
		return ErrDestroyed
	}
	temporary := make([]byte, len(l.value))
	copy(temporary, l.value)
	l.mu.Unlock()
	defer zeroBytes(temporary)
	return fn(temporary)
}

// Destroy zeroes the lease's retained secret. It is safe to call repeatedly.
func (l *Lease) Destroy() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.destroyed {
		zeroBytes(l.value)
		l.value = nil
		l.destroyed = true
	}
	l.mu.Unlock()
	runtime.SetFinalizer(l, nil)
}

type brokerState struct {
	mu       sync.Mutex
	executor commandExecutor
	binding  [32]byte
	closed   bool
}

// Broker exposes separate APIs so records cannot cross model, vault, and
// license namespaces accidentally.
type Broker struct {
	state         *brokerState
	modelData     ModelDataKeys
	resourceVault ResourceVaultKeys
	licenseDevice LicenseDeviceState
}

func newBroker(executor commandExecutor, binding DeviceBinding) (*Broker, error) {
	if executor == nil || isZeroDigest(binding.digest) {
		return nil, ErrInvalidArgument
	}
	state := &brokerState{executor: executor, binding: binding.digest}
	broker := &Broker{state: state}
	broker.modelData.store = versionedStore{state: state, namespace: namespaceModelData, kind: recordKindKey}
	broker.resourceVault.store = versionedStore{state: state, namespace: namespaceResourceVault, kind: recordKindKey}
	broker.licenseDevice.store = versionedStore{state: state, namespace: namespaceLicenseDeviceState, kind: recordKindState}
	return broker, nil
}

// ModelDataKeys returns the model-data namespace API.
func (b *Broker) ModelDataKeys() *ModelDataKeys {
	if b == nil {
		return nil
	}
	return &b.modelData
}

// ResourceVaultKeys returns the resource-vault namespace API.
func (b *Broker) ResourceVaultKeys() *ResourceVaultKeys {
	if b == nil {
		return nil
	}
	return &b.resourceVault
}

// LicenseDeviceState returns the license-device-state namespace API.
func (b *Broker) LicenseDeviceState() *LicenseDeviceState {
	if b == nil {
		return nil
	}
	return &b.licenseDevice
}

// Close prevents new operations, zeroes the binding digest, and asks the
// backend to release sensitive process memory.
func (b *Broker) Close() error {
	if b == nil || b.state == nil {
		return nil
	}
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	if b.state.closed {
		return nil
	}
	b.state.closed = true
	zeroBytes(b.state.binding[:])
	err := b.state.executor.close()
	b.state.executor = nil
	return err
}

// ModelDataKeys manages versioned 256-bit model-data keys.
type ModelDataKeys struct {
	store versionedStore
}

func (m *ModelDataKeys) Current(ctx context.Context, resource string) (*Lease, error) {
	return m.store.current(ctx, resource)
}

func (m *ModelDataKeys) At(ctx context.Context, resource string, version Version) (*Lease, error) {
	return m.store.at(ctx, resource, version)
}

// Rotate creates expected+1. Use expected zero for first creation.
func (m *ModelDataKeys) Rotate(ctx context.Context, resource string, expected Version) (*Lease, error) {
	return m.store.rotateKey(ctx, resource, expected, modelDataKeySize)
}

// ResourceVaultKeys manages versioned 256-bit resource-vault keys.
type ResourceVaultKeys struct {
	store versionedStore
}

func (r *ResourceVaultKeys) Current(ctx context.Context, resource string) (*Lease, error) {
	return r.store.current(ctx, resource)
}

func (r *ResourceVaultKeys) At(ctx context.Context, resource string, version Version) (*Lease, error) {
	return r.store.at(ctx, resource, version)
}

// Rotate creates expected+1. Use expected zero for first creation.
func (r *ResourceVaultKeys) Rotate(ctx context.Context, resource string, expected Version) (*Lease, error) {
	return r.store.rotateKey(ctx, resource, expected, resourceVaultKeySize)
}

// LicenseDeviceState stores opaque, versioned license state bound to the
// broker's device digest.
type LicenseDeviceState struct {
	store versionedStore
}

func (l *LicenseDeviceState) Current(ctx context.Context, resource string) (*Lease, error) {
	return l.store.current(ctx, resource)
}

func (l *LicenseDeviceState) At(ctx context.Context, resource string, version Version) (*Lease, error) {
	return l.store.at(ctx, resource, version)
}

// Replace stores expected+1 without mutating the caller-owned state buffer.
// The caller remains responsible for zeroing its source buffer.
func (l *LicenseDeviceState) Replace(ctx context.Context, resource string, expected Version, state []byte) (Version, error) {
	return l.store.replaceState(ctx, resource, expected, state)
}

type versionedStore struct {
	state     *brokerState
	namespace namespaceID
	kind      recordKind
}

func (s *versionedStore) current(ctx context.Context, resource string) (*Lease, error) {
	if s == nil || s.state == nil {
		return nil, ErrUnavailable
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := s.readyLocked(ctx); err != nil {
		return nil, err
	}
	digest, err := digestResource(resource)
	if err != nil {
		return nil, err
	}
	version, payload, err := s.loadLatestLocked(ctx, digest)
	if err != nil {
		return nil, err
	}
	return newLease(version, payload), nil
}

func (s *versionedStore) at(ctx context.Context, resource string, version Version) (*Lease, error) {
	if s == nil || s.state == nil || version == 0 {
		return nil, ErrInvalidArgument
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := s.readyLocked(ctx); err != nil {
		return nil, err
	}
	digest, err := digestResource(resource)
	if err != nil {
		return nil, err
	}
	payload, err := s.loadAtLocked(ctx, digest, version)
	if err != nil {
		return nil, err
	}
	return newLease(version, payload), nil
}

func (s *versionedStore) rotateKey(ctx context.Context, resource string, expected Version, keySize int) (*Lease, error) {
	if s == nil || s.state == nil || s.kind != recordKindKey {
		return nil, ErrUnavailable
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := s.readyLocked(ctx); err != nil {
		return nil, err
	}
	digest, err := digestResource(resource)
	if err != nil {
		return nil, err
	}
	current, payload, err := s.loadLatestLocked(ctx, digest)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	zeroBytes(payload)
	if errors.Is(err, ErrNotFound) {
		current = 0
	}
	if current != expected || current == ^Version(0) {
		return nil, ErrVersionConflict
	}
	next := current + 1
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		zeroBytes(key)
		return nil, fmt.Errorf("keybroker random source failed: %w", err)
	}
	record := encodeRecord(s.namespace, s.kind, next, digest, s.state.binding, key)
	defer zeroBytes(record)
	_, err = s.state.executor.execute(ctx, &securityCommand{
		operation: commandAdd,
		account:   accountFor(s.namespace, digest, next),
		value:     record,
	})
	if err == nil {
		return newLease(next, key), nil
	}
	zeroBytes(key)
	if !errors.Is(err, errEntryExists) {
		return nil, publicBackendError(err)
	}
	existing, err := s.loadAtLocked(ctx, digest, next)
	if err != nil {
		return nil, err
	}
	latest, latestPayload, err := s.loadLatestLocked(ctx, digest)
	zeroBytes(latestPayload)
	if err != nil || latest != next {
		zeroBytes(existing)
		return nil, ErrVersionConflict
	}
	return newLease(next, existing), nil
}

func (s *versionedStore) replaceState(ctx context.Context, resource string, expected Version, state []byte) (Version, error) {
	if s == nil || s.state == nil || s.kind != recordKindState {
		return 0, ErrUnavailable
	}
	if len(state) == 0 || len(state) > maxLicenseDeviceStateSize {
		return 0, ErrInvalidArgument
	}
	stateCopy := make([]byte, len(state))
	copy(stateCopy, state)
	defer zeroBytes(stateCopy)
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := s.readyLocked(ctx); err != nil {
		return 0, err
	}
	digest, err := digestResource(resource)
	if err != nil {
		return 0, err
	}
	current, payload, err := s.loadLatestLocked(ctx, digest)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	zeroBytes(payload)
	if errors.Is(err, ErrNotFound) {
		current = 0
	}
	if current != expected || current == ^Version(0) {
		return 0, ErrVersionConflict
	}
	next := current + 1
	record := encodeRecord(s.namespace, s.kind, next, digest, s.state.binding, stateCopy)
	defer zeroBytes(record)
	_, err = s.state.executor.execute(ctx, &securityCommand{
		operation: commandAdd,
		account:   accountFor(s.namespace, digest, next),
		value:     record,
	})
	if err == nil {
		return next, nil
	}
	if !errors.Is(err, errEntryExists) {
		return 0, publicBackendError(err)
	}
	existing, err := s.loadAtLocked(ctx, digest, next)
	if err != nil {
		return 0, err
	}
	defer zeroBytes(existing)
	if subtle.ConstantTimeCompare(existing, stateCopy) != 1 {
		return 0, ErrVersionConflict
	}
	latest, latestPayload, err := s.loadLatestLocked(ctx, digest)
	zeroBytes(latestPayload)
	if err != nil || latest != next {
		return 0, ErrVersionConflict
	}
	return next, nil
}

func (s *versionedStore) loadLatestLocked(ctx context.Context, digest [32]byte) (Version, []byte, error) {
	result, err := s.state.executor.execute(ctx, &securityCommand{
		operation: commandReadLatest,
		account:   accountPrefix(s.namespace, digest),
	})
	if err != nil {
		return 0, nil, publicBackendError(err)
	}
	defer zeroBytes(result.value)
	payload, err := decodeRecord(result.value, s.namespace, s.kind, result.version, digest, s.state.binding)
	if err != nil {
		return 0, nil, err
	}
	return result.version, payload, nil
}

func (s *versionedStore) loadAtLocked(ctx context.Context, digest [32]byte, version Version) ([]byte, error) {
	result, err := s.state.executor.execute(ctx, &securityCommand{
		operation: commandRead,
		account:   accountFor(s.namespace, digest, version),
	})
	if err != nil {
		return nil, publicBackendError(err)
	}
	defer zeroBytes(result.value)
	return decodeRecord(result.value, s.namespace, s.kind, version, digest, s.state.binding)
}

func (s *versionedStore) readyLocked(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.state.closed || s.state.executor == nil {
		return ErrClosed
	}
	return nil
}

func digestResource(resource string) ([32]byte, error) {
	if resource == "" || len(resource) > 512 || !utf8.ValidString(resource) ||
		strings.TrimSpace(resource) != resource || strings.IndexByte(resource, 0) >= 0 {
		return [32]byte{}, ErrInvalidArgument
	}
	return sha256.Sum256([]byte(resource)), nil
}

func accountPrefix(namespace namespaceID, digest [32]byte) string {
	return fmt.Sprintf("mkey/1/%02x/%x/", byte(namespace), digest)
}

func accountFor(namespace namespaceID, digest [32]byte, version Version) string {
	return fmt.Sprintf("%s%020d", accountPrefix(namespace, digest), uint64(version))
}

func publicBackendError(err error) error {
	switch {
	case errors.Is(err, errEntryNotFound):
		return ErrNotFound
	case errors.Is(err, errEntryExists):
		return ErrVersionConflict
	case errors.Is(err, ErrUnavailable):
		return ErrUnavailable
	default:
		return fmt.Errorf("keybroker backend operation failed: %w", err)
	}
}
