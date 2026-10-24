package modelprotection

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"io"
)

// Encrypt writes one complete authenticated container to destination. The
// caller must supply the exact plaintext size; short or long sources fail.
func Encrypt(
	ctx context.Context,
	destination io.Writer,
	plaintext io.Reader,
	plaintextSize uint64,
	wrapper KeyWrapper,
	options *EncryptOptions,
) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if destination == nil || plaintext == nil || wrapper == nil {
		return fmt.Errorf("%w: nil destination, plaintext, or key wrapper", ErrInvalidArgument)
	}
	chunkSize, metadata, err := normalizeEncryptOptions(options)
	if err != nil {
		return err
	}

	dataKey := make([]byte, DataKeySize)
	if _, err := rand.Read(dataKey); err != nil {
		return fmt.Errorf("modelprotection: data-key generation: %w", err)
	}
	defer clear(dataKey)

	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return fmt.Errorf("modelprotection: create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("modelprotection: create GCM: %w", err)
	}
	wrappedKey, err := wrapper.WrapKey(ctx, dataKey)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKeyWrap, err)
	}
	if len(wrappedKey) == 0 || len(wrappedKey) > MaxWrappedKeySize {
		return fmt.Errorf("%w: invalid wrapped-key size", ErrKeyWrap)
	}
	if len(wrappedKey) == len(dataKey) && subtle.ConstantTimeCompare(wrappedKey, dataKey) == 1 {
		return fmt.Errorf("%w: wrapper returned the data key unchanged", ErrKeyWrap)
	}
	wrappedKey = append([]byte(nil), wrappedKey...)

	header := containerHeader{
		chunkSize:     chunkSize,
		metadataSize:  uint32(len(metadata)),
		wrappedSize:   uint32(len(wrappedKey)),
		plaintextSize: plaintextSize,
		chunkCount:    expectedChunkCount(plaintextSize, chunkSize),
	}
	if _, err := rand.Read(header.noncePrefix[:]); err != nil {
		return fmt.Errorf("modelprotection: nonce-prefix generation: %w", err)
	}
	rawHeader := encodeHeader(header)
	baseAAD := makeBaseAAD(rawHeader[:], metadata, wrappedKey)
	chunkAAD := makeChunkAAD(baseAAD)

	if err := writeAll(destination, rawHeader[:]); err != nil {
		return fmt.Errorf("modelprotection: write header: %w", err)
	}
	if err := writeAll(destination, metadata); err != nil {
		return fmt.Errorf("modelprotection: write metadata: %w", err)
	}
	if err := writeAll(destination, wrappedKey); err != nil {
		return fmt.Errorf("modelprotection: write wrapped key: %w", err)
	}

	plaintextBuffer := make([]byte, int(chunkSize))
	defer clear(plaintextBuffer)
	ciphertextBuffer := make([]byte, 0, int(chunkSize)+aead.Overhead())
	var consumed uint64
	for index := uint64(0); index < header.chunkCount; index++ {
		if err := validateContext(ctx); err != nil {
			return err
		}
		plainSize := expectedPlaintextChunk(plaintextSize, consumed, chunkSize)
		plainChunk := plaintextBuffer[:int(plainSize)]
		if plainSize > 0 {
			read, err := io.ReadFull(plaintext, plainChunk)
			if err != nil {
				return fmt.Errorf("%w: chunk %d read %d of %d bytes", ErrSizeMismatch, index, read, plainSize)
			}
		}
		nonce := makeNonce(header.noncePrefix, index)
		setChunkAAD(chunkAAD, index, plaintextSize, plainSize)
		ciphertext := aead.Seal(ciphertextBuffer[:0], nonce[:], plainChunk, chunkAAD)
		if err := writeAll(destination, ciphertext); err != nil {
			return fmt.Errorf("modelprotection: write chunk %d: %w", index, err)
		}
		clear(plainChunk)
		consumed += uint64(plainSize)
	}
	if consumed != plaintextSize {
		return ErrSizeMismatch
	}
	hasMore, err := hasTrailingByte(plaintext)
	if err != nil {
		return fmt.Errorf("modelprotection: verify plaintext EOF: %w", err)
	}
	if hasMore {
		return fmt.Errorf("%w: source exceeds declared size", ErrSizeMismatch)
	}
	return nil
}

func normalizeEncryptOptions(options *EncryptOptions) (uint32, []byte, error) {
	chunkSize := DefaultChunkSize
	var metadata []byte
	if options != nil {
		if options.ChunkSize != 0 {
			chunkSize = options.ChunkSize
		}
		metadata = append([]byte(nil), options.Metadata...)
	}
	if chunkSize == 0 || chunkSize > MaxChunkSize {
		return 0, nil, fmt.Errorf("%w: chunk size must be between 1 and %d", ErrInvalidArgument, MaxChunkSize)
	}
	if len(metadata) > MaxMetadataSize {
		return 0, nil, fmt.Errorf("%w: metadata exceeds %d bytes", ErrInvalidArgument, MaxMetadataSize)
	}
	return chunkSize, metadata, nil
}
