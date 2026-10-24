package resourcevault

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"runtime"
	"sort"
	"sync"
)

type verifiedResource struct {
	entry         ResourceEntry
	payloadDigest [sha256.Size]byte
	aad           []byte
	bindingDigest [sha256.Size]byte
}

// Vault is immutable after construction and safe for concurrent opens when the
// supplied KeyUnwrapper is safe for concurrent use.
type Vault struct {
	resources map[string]verifiedResource
	unwrapper KeyUnwrapper
	limits    Limits
}

// New verifies the detached index signature before decoding or trusting any
// index field. It accepts no filesystem paths.
func New(
	ctx context.Context,
	indexReader io.Reader,
	signatureReader io.Reader,
	keys VerificationKeyProvider,
	unwrapper KeyUnwrapper,
	config Config,
) (*Vault, error) {
	if ctx == nil || indexReader == nil || signatureReader == nil || keys == nil || unwrapper == nil {
		return nil, fmt.Errorf("resource vault requires context, readers, key provider, and key unwrapper")
	}
	if err := validateBundleID(config.ExpectedBundleID); err != nil {
		return nil, fmt.Errorf("invalid expected resource vault bundle id: %w", err)
	}
	if err := validateOpaqueID(config.ExpectedBundleVersion, 128, false); err != nil {
		return nil, fmt.Errorf("invalid expected resource vault bundle version: %w", err)
	}
	if err := validateOpaqueID(config.ExpectedSigningKeyID, 128, false); err != nil {
		return nil, fmt.Errorf("invalid expected resource vault signing key id: %w", err)
	}
	limits, err := normalizeLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	index, err := readAndVerifyIndex(ctx, indexReader, signatureReader, keys, config, limits)
	if err != nil {
		return nil, err
	}
	resources := make(map[string]verifiedResource, len(index.Resources))
	for _, entry := range index.Resources {
		digest, err := decodeSHA256(entry.PayloadSHA256)
		if err != nil {
			return nil, fmt.Errorf("decode verified payload digest: %w", err)
		}
		binding := bindingFor(index, entry)
		aad, err := bindingBytes(binding)
		if err != nil {
			return nil, fmt.Errorf("encode verified resource binding: %w", err)
		}
		resources[entry.ResourceID] = verifiedResource{
			entry:         cloneEntry(entry),
			payloadDigest: digest,
			aad:           aad,
			bindingDigest: sha256.Sum256(aad),
		}
	}
	return &Vault{resources: resources, unwrapper: unwrapper, limits: limits}, nil
}

// Metadata returns a defensive copy of signed, non-secret resource metadata.
func (vault *Vault) Metadata(resourceID string) (ResourceMetadata, error) {
	if vault == nil {
		return ResourceMetadata{}, ErrNotFound
	}
	resource, ok := vault.resources[resourceID]
	if !ok {
		return ResourceMetadata{}, ErrNotFound
	}
	return ResourceMetadata{
		ResourceID:      resource.entry.ResourceID,
		ResourceType:    resource.entry.ResourceType,
		ResourceVersion: resource.entry.ResourceVersion,
		Capabilities:    append([]string(nil), resource.entry.Capabilities...),
		PlaintextBytes:  resource.entry.PlaintextBytes,
	}, nil
}

// Open authenticates and decrypts one externally supplied payload. The exact
// payload byte count and SHA-256 must match the signed index before key unwrap.
func (vault *Vault) Open(ctx context.Context, request ResourceRequest, payloadReader io.Reader) (*Plaintext, error) {
	if vault == nil || ctx == nil || payloadReader == nil {
		return nil, fmt.Errorf("resource vault open requires a vault, context, and payload reader")
	}
	if err := validateOpaqueID(request.ResourceID, 192, false); err != nil {
		return nil, ErrNotFound
	}
	if err := validateOpaqueID(request.ResourceVersion, 128, false); err != nil {
		return nil, fmt.Errorf("invalid requested resource version")
	}
	if err := validateOpaqueID(request.Capability, 128, true); err != nil {
		return nil, ErrUnauthorized
	}
	verified, ok := vault.resources[request.ResourceID]
	if !ok {
		return nil, ErrNotFound
	}
	if request.ResourceVersion != verified.entry.ResourceVersion {
		return nil, fmt.Errorf("requested resource version does not match signed index")
	}
	position := sort.SearchStrings(verified.entry.Capabilities, request.Capability)
	if position == len(verified.entry.Capabilities) || verified.entry.Capabilities[position] != request.Capability {
		return nil, ErrUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := readBounded(payloadReader, verified.entry.PayloadBytes)
	if err != nil {
		return nil, fmt.Errorf("read encrypted resource payload: %w", err)
	}
	defer wipe(raw)
	if int64(len(raw)) != verified.entry.PayloadBytes || !digestMatches(raw, verified.payloadDigest) {
		return nil, fmt.Errorf("encrypted resource payload does not match signed index")
	}
	wrappedKey, err := extractWrappedKey(raw, vault.limits.WrappedKeyBytes)
	if err != nil {
		return nil, err
	}
	defer wipe(wrappedKey)
	dataKey, err := vault.unwrapper.UnwrapKey(ctx, KeyUnwrapRequest{
		KeyID:         verified.entry.KeyID,
		Algorithm:     verified.entry.KeyWrapAlgorithm,
		BindingSHA256: verified.bindingDigest,
		WrappedKey:    wrappedKey,
	})
	if err != nil {
		wipe(dataKey)
		return nil, fmt.Errorf("unwrap resource vault data key: %w", err)
	}
	defer wipe(dataKey)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plaintext, err := decryptPayload(raw, verified, dataKey, vault.limits)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		wipe(plaintext)
		return nil, err
	}
	return newPlaintext(plaintext), nil
}

func normalizeLimits(input Limits) (Limits, error) {
	defaults := DefaultLimits()
	limits := input
	if limits.IndexBytes == 0 {
		limits.IndexBytes = defaults.IndexBytes
	}
	if limits.SignatureBytes == 0 {
		limits.SignatureBytes = defaults.SignatureBytes
	}
	if limits.PayloadBytes == 0 {
		limits.PayloadBytes = defaults.PayloadBytes
	}
	if limits.PlaintextBytes == 0 {
		limits.PlaintextBytes = defaults.PlaintextBytes
	}
	if limits.WrappedKeyBytes == 0 {
		limits.WrappedKeyBytes = defaults.WrappedKeyBytes
	}
	if limits.Resources == 0 {
		limits.Resources = defaults.Resources
	}
	if limits.IndexBytes < 1 || limits.IndexBytes > hardMaxIndexBytes ||
		limits.SignatureBytes < 1 || limits.SignatureBytes > hardMaxSignatureBytes ||
		limits.PayloadBytes < 1 || limits.PayloadBytes > hardMaxPayloadBytes ||
		limits.PlaintextBytes < 1 || limits.PlaintextBytes > hardMaxPlaintextBytes ||
		limits.WrappedKeyBytes < 1 || limits.WrappedKeyBytes > hardMaxWrappedKeyBytes ||
		limits.Resources < 1 || limits.Resources > hardMaxResources {
		return Limits{}, fmt.Errorf("resource vault limits must be positive and may only lower hard limits")
	}
	return limits, nil
}

func cloneEntry(entry ResourceEntry) ResourceEntry {
	entry.Capabilities = append([]string(nil), entry.Capabilities...)
	return entry
}

// Plaintext owns one mutable decrypted buffer. Use is the only supported data
// access method; callers must not retain the borrowed slice after fn returns.
type Plaintext struct {
	mu     sync.Mutex
	buffer []byte
	closed bool
}

func newPlaintext(buffer []byte) *Plaintext {
	plaintext := &Plaintext{buffer: buffer}
	runtime.SetFinalizer(plaintext, func(value *Plaintext) {
		_ = value.Close()
	})
	return plaintext
}

func (plaintext *Plaintext) Len() int {
	if plaintext == nil {
		return 0
	}
	plaintext.mu.Lock()
	defer plaintext.mu.Unlock()
	if plaintext.closed {
		return 0
	}
	return len(plaintext.buffer)
}

// Use lends the mutable plaintext buffer only for fn's duration. fn must not
// retain it, convert it to a string, call other Plaintext methods, or pass it to
// an API that keeps a copy.
func (plaintext *Plaintext) Use(fn func([]byte) error) error {
	if plaintext == nil || fn == nil {
		return ErrClosed
	}
	plaintext.mu.Lock()
	defer plaintext.mu.Unlock()
	if plaintext.closed {
		return ErrClosed
	}
	return fn(plaintext.buffer)
}

// Close performs best-effort zeroing and is idempotent.
func (plaintext *Plaintext) Close() error {
	if plaintext == nil {
		return nil
	}
	plaintext.mu.Lock()
	defer plaintext.mu.Unlock()
	if plaintext.closed {
		return nil
	}
	wipe(plaintext.buffer)
	plaintext.buffer = nil
	plaintext.closed = true
	runtime.SetFinalizer(plaintext, nil)
	return nil
}
