package sealedasset

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/nativeassettransport"
	"github.com/hey-meadow/meadow-code/go/core/protectedmodelbridge"
)

type Lease struct {
	mu sync.Mutex

	reader     io.ReadCloser
	identity   assetIdentity
	metadata   fileMetadata
	aead       cipher.AEAD
	baseNonce  [12]byte
	streamHash hash.Hash

	nextChunk    uint64
	pending      []byte
	pendingIndex int
	consumed     uint64
	streamValid  bool
	closed       bool
	canceled     bool
	closeErr     error
}

func OpenLease(
	ctx context.Context,
	reader io.ReadCloser,
	verified VerifiedProductManifest,
	ordinal uint32,
	keys KeyProvider,
) (*Lease, error) {
	if ctx == nil || reader == nil || keys == nil {
		if reader != nil {
			_ = reader.Close()
		}
		return nil, ErrInvalidArgument
	}
	manifest := verified.SignedManifest()
	if int(ordinal) >= len(manifest.Assets) || manifest.Assets[ordinal].Ordinal != ordinal {
		_ = reader.Close()
		return nil, ErrAssetRejected
	}
	identity := identityFor(verified, manifest.Assets[ordinal])
	var preamble [preambleBytes]byte
	if _, err := io.ReadFull(reader, preamble[:]); err != nil ||
		subtle.ConstantTimeCompare(preamble[0:8], sealedMagic[:]) != 1 ||
		binary.BigEndian.Uint16(preamble[8:10]) != FormatVersion ||
		binary.BigEndian.Uint16(preamble[10:12]) != algorithmAES256GCM {
		zero(preamble[:])
		_ = reader.Close()
		return nil, ErrIntegrity
	}
	var salt [32]byte
	var baseNonce [12]byte
	copy(salt[:], preamble[12:44])
	copy(baseNonce[:], preamble[44:56])
	metadataBytes := binary.BigEndian.Uint32(preamble[56:60])
	zero(preamble[:])
	if metadataBytes == 0 || metadataBytes > maxEncryptedMetadata+32 {
		zero(salt[:])
		zero(baseNonce[:])
		_ = reader.Close()
		return nil, ErrIntegrity
	}
	master, err := keys.MasterKey(ctx)
	if err != nil || len(master) != MasterKeyBytes {
		zero(master)
		zero(salt[:])
		zero(baseNonce[:])
		_ = reader.Close()
		return nil, ErrKeyUnavailable
	}
	headerKey := deriveKey(master, salt[:], "header", identity)
	chunkKey := deriveKey(master, salt[:], "chunk", identity)
	zero(master)
	zero(salt[:])
	defer zero(headerKey)
	defer zero(chunkKey)
	headerAEAD, err := newGCM(headerKey)
	if err != nil {
		zero(baseNonce[:])
		_ = reader.Close()
		return nil, ErrKeyUnavailable
	}
	chunkAEAD, err := newGCM(chunkKey)
	if err != nil {
		zero(baseNonce[:])
		_ = reader.Close()
		return nil, ErrKeyUnavailable
	}
	ciphertext := make([]byte, metadataBytes)
	if _, err := io.ReadFull(reader, ciphertext); err != nil {
		zero(ciphertext)
		zero(baseNonce[:])
		_ = reader.Close()
		return nil, ErrIntegrity
	}
	plaintextSize := uint64(len(ciphertext) - headerAEAD.Overhead())
	headerAAD := aad(identity, headerAADKind, 0, plaintextSize)
	headerNonce := nonceFor(baseNonce, 0)
	plaintext, err := headerAEAD.Open(nil, headerNonce[:], ciphertext, headerAAD)
	zero(ciphertext)
	zero(headerAAD)
	if err != nil {
		zero(baseNonce[:])
		_ = reader.Close()
		return nil, ErrIntegrity
	}
	var metadata fileMetadata
	decodeErr := decodeStrictJSON(plaintext, &metadata)
	zero(plaintext)
	if decodeErr != nil || !metadataMatches(metadata, identity) {
		zero(baseNonce[:])
		_ = reader.Close()
		return nil, ErrIntegrity
	}
	return &Lease{
		reader:     reader,
		identity:   identity,
		metadata:   metadata,
		aead:       chunkAEAD,
		baseNonce:  baseNonce,
		streamHash: sha256.New(),
	}, nil
}

func metadataMatches(metadata fileMetadata, identity assetIdentity) bool {
	if metadata.ChunkBytes == 0 || metadata.ChunkBytes > nativeassettransport.MaxChunkBytes {
		return false
	}
	expected := metadataFor(identity, metadata.ChunkBytes)
	return metadata.SchemaVersion == expected.SchemaVersion &&
		metadata.FormatVersion == expected.FormatVersion &&
		metadata.ModelID == expected.ModelID &&
		metadata.ProfileRevision == expected.ProfileRevision &&
		metadata.Backend == expected.Backend &&
		metadata.ProfileDigest == expected.ProfileDigest &&
		metadata.AssetSetDigest == expected.AssetSetDigest &&
		metadata.ManifestDigest == expected.ManifestDigest &&
		metadata.CanonicalManifestDigest == expected.CanonicalManifestDigest &&
		metadata.Ordinal == expected.Ordinal && metadata.AssetID == expected.AssetID &&
		metadata.Role == expected.Role && metadata.Required == expected.Required &&
		metadata.PlaintextBytes == expected.PlaintextBytes &&
		metadata.PlaintextSHA256 == expected.PlaintextSHA256 &&
		metadata.ChunkBytes > 0 && metadata.ChunkBytes <= nativeassettransport.MaxChunkBytes &&
		metadata.ChunkCount == chunkCount(metadata.PlaintextBytes, metadata.ChunkBytes)
}

func (lease *Lease) Info() protectedmodelbridge.AssetInfo {
	if lease == nil {
		return protectedmodelbridge.AssetInfo{}
	}
	return protectedmodelbridge.AssetInfo{
		AccessMode: protectedmodelbridge.AccessSequentialAuthenticated,
		Binding: protectedmodelbridge.ProfileBinding{
			ModelID:         lease.identity.modelID,
			ProfileDigest:   lease.identity.profileDigest,
			ProfileRevision: lease.identity.profileRevision,
			Backend:         lease.identity.asset.Binding.Backend,
		},
		PlaintextSize:       lease.metadata.PlaintextBytes,
		PreferredChunkBytes: lease.metadata.ChunkBytes,
		MaxChunkBytes:       lease.metadata.ChunkBytes,
	}
}

func (lease *Lease) ConsumedBytes() uint64 {
	if lease == nil {
		return 0
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.consumed
}

func (lease *Lease) ConsumeNext(
	ctx context.Context,
	maximum uint32,
	consumer protectedmodelbridge.ChunkConsumer,
) (uint32, error) {
	if lease == nil || ctx == nil || maximum == 0 || maximum > nativeassettransport.MaxChunkBytes || consumer == nil {
		return 0, ErrInvalidArgument
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.canceled {
		return 0, ErrCanceled
	}
	if lease.closed {
		return 0, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		lease.closeErr = errors.Join(lease.closeErr, lease.drainLocked(), err)
		lease.canceled = true
		lease.closed = true
		return 0, errors.Join(ErrCanceled, err)
	}
	if len(lease.pending) == 0 {
		if lease.nextChunk >= lease.metadata.ChunkCount {
			return 0, io.EOF
		}
		if err := lease.loadChunkLocked(); err != nil {
			lease.failLocked(err)
			return 0, err
		}
	}
	available := len(lease.pending) - lease.pendingIndex
	take := int(maximum)
	if available < take {
		take = available
	}
	view := lease.pending[lease.pendingIndex : lease.pendingIndex+take]
	offset := lease.consumed
	err := consumer(ctx, offset, view)
	zero(view)
	if err != nil {
		lease.pendingIndex += take
		lease.closeErr = errors.Join(lease.closeErr, lease.drainLocked(), err)
		lease.closed = true
		return 0, errors.Join(protectedmodelbridge.ErrConsumer, err)
	}
	lease.pendingIndex += take
	lease.consumed += uint64(take)
	if lease.pendingIndex == len(lease.pending) {
		zero(lease.pending)
		lease.pending = nil
		lease.pendingIndex = 0
	}
	return uint32(take), nil
}

func (lease *Lease) loadChunkLocked() error {
	if lease.nextChunk >= lease.metadata.ChunkCount {
		return io.EOF
	}
	remaining := lease.metadata.PlaintextBytes - lease.nextChunk*uint64(lease.metadata.ChunkBytes)
	plainBytes := uint64(lease.metadata.ChunkBytes)
	if remaining < plainBytes {
		plainBytes = remaining
	}
	var encoded [4]byte
	if _, err := io.ReadFull(lease.reader, encoded[:]); err != nil {
		return ErrIntegrity
	}
	cipherBytes := binary.BigEndian.Uint32(encoded[:])
	zero(encoded[:])
	if uint64(cipherBytes) != plainBytes+uint64(lease.aead.Overhead()) {
		return ErrIntegrity
	}
	ciphertext := make([]byte, cipherBytes)
	if _, err := io.ReadFull(lease.reader, ciphertext); err != nil {
		zero(ciphertext)
		return ErrIntegrity
	}
	chunkAAD := aad(lease.identity, chunkAADKind, lease.nextChunk, plainBytes)
	nonce := nonceFor(lease.baseNonce, lease.nextChunk+1)
	plaintext, err := lease.aead.Open(nil, nonce[:], ciphertext, chunkAAD)
	zero(ciphertext)
	zero(chunkAAD)
	if err != nil || uint64(len(plaintext)) != plainBytes {
		zero(plaintext)
		return ErrIntegrity
	}
	_, _ = lease.streamHash.Write(plaintext)
	lease.pending = plaintext
	lease.pendingIndex = 0
	lease.nextChunk++
	if lease.nextChunk == lease.metadata.ChunkCount {
		var trailing [1]byte
		count, readErr := lease.reader.Read(trailing[:])
		zero(trailing[:])
		actual := lease.streamHash.Sum(nil)
		expected, decodeErr := hex.DecodeString(lease.metadata.PlaintextSHA256)
		valid := readErr == io.EOF && count == 0 && decodeErr == nil &&
			len(expected) == sha256.Size &&
			subtle.ConstantTimeCompare(actual, expected) == 1
		zero(actual)
		zero(expected)
		if !valid {
			zero(lease.pending)
			lease.pending = nil
			return ErrIntegrity
		}
		lease.streamValid = true
	}
	return nil
}

func (lease *Lease) drainLocked() error {
	if lease.reader == nil {
		return lease.closeErr
	}
	if len(lease.pending) != 0 {
		zero(lease.pending)
		lease.pending = nil
		lease.pendingIndex = 0
	}
	var drainErr error
	for lease.nextChunk < lease.metadata.ChunkCount {
		if err := lease.loadChunkLocked(); err != nil {
			drainErr = errors.Join(drainErr, err)
			break
		}
		zero(lease.pending)
		lease.pending = nil
		lease.pendingIndex = 0
	}
	if !lease.streamValid {
		drainErr = errors.Join(drainErr, ErrIntegrity)
	}
	drainErr = errors.Join(drainErr, lease.reader.Close())
	lease.reader = nil
	zero(lease.baseNonce[:])
	return drainErr
}

func (lease *Lease) failLocked(err error) {
	if len(lease.pending) != 0 {
		zero(lease.pending)
		lease.pending = nil
	}
	if lease.reader != nil {
		_ = lease.reader.Close()
		lease.reader = nil
	}
	zero(lease.baseNonce[:])
	lease.closeErr = errors.Join(lease.closeErr, err)
	lease.closed = true
}

func (lease *Lease) Cancel() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		lease.canceled = true
		return lease.closeErr
	}
	lease.canceled = true
	lease.closeErr = errors.Join(lease.closeErr, lease.drainLocked())
	lease.closed = true
	return lease.closeErr
}

func (lease *Lease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return lease.closeErr
	}
	lease.closeErr = errors.Join(lease.closeErr, lease.drainLocked())
	lease.closed = true
	return lease.closeErr
}

var _ protectedmodelbridge.SequentialLease = (*Lease)(nil)
