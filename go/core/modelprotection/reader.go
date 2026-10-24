package modelprotection

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
)

// Reader authenticates and streams plaintext without creating plaintext files.
// Callers must read to EOF or call Close; Close drains and authenticates unread
// chunks before closing an underlying io.Closer.
type Reader struct {
	ctx         context.Context
	source      io.Reader
	closer      io.Closer
	aead        cipher.AEAD
	header      containerHeader
	info        Info
	chunkAAD    []byte
	nextChunk   uint64
	produced    uint64
	plaintext   []byte
	plainOffset int
	finished    bool
	terminalErr error
	closed      bool
	closeErr    error
}

// NewReader parses the envelope, unwraps its data key, and authenticates its
// first chunk before returning. All structural limits are checked before any
// header-controlled allocation.
func NewReader(ctx context.Context, source io.Reader, unwrapper KeyUnwrapper) (*Reader, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	if source == nil || unwrapper == nil {
		return nil, fmt.Errorf("%w: nil source or key unwrapper", ErrInvalidArgument)
	}
	rawHeader := make([]byte, fixedHeaderSize)
	if err := readContainerBytes(source, rawHeader); err != nil {
		return nil, err
	}
	header, err := decodeHeader(rawHeader)
	if err != nil {
		return nil, err
	}
	metadata := make([]byte, int(header.metadataSize))
	if err := readContainerBytes(source, metadata); err != nil {
		return nil, err
	}
	wrappedKey := make([]byte, int(header.wrappedSize))
	if err := readContainerBytes(source, wrappedKey); err != nil {
		return nil, err
	}
	dataKey, err := unwrapper.UnwrapKey(ctx, wrappedKey)
	if err != nil {
		clear(dataKey)
		return nil, fmt.Errorf("%w: %v", ErrKeyUnwrap, err)
	}
	if len(dataKey) != DataKeySize {
		clear(dataKey)
		return nil, fmt.Errorf("%w: unwrapped key must be %d bytes", ErrKeyUnwrap, DataKeySize)
	}
	block, err := aes.NewCipher(dataKey)
	clear(dataKey)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid AES data key", ErrKeyUnwrap)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("modelprotection: create GCM: %w", err)
	}
	reader := &Reader{
		ctx:    ctx,
		source: source,
		aead:   aead,
		header: header,
		info: Info{
			PlaintextSize: header.plaintextSize,
			ChunkSize:     header.chunkSize,
			ChunkCount:    header.chunkCount,
			Metadata:      append([]byte(nil), metadata...),
		},
		chunkAAD: makeChunkAAD(makeBaseAAD(rawHeader, metadata, wrappedKey)),
	}
	if closer, ok := source.(io.Closer); ok {
		reader.closer = closer
	}
	if err := reader.loadNextChunk(); err != nil {
		return nil, err
	}
	if reader.nextChunk == reader.header.chunkCount && len(reader.plaintext) == 0 {
		if err := reader.verifyContainerEnd(); err != nil {
			return nil, err
		}
		reader.finished = true
	}
	return reader, nil
}

// Info returns an independent copy of authenticated container information.
func (reader *Reader) Info() Info {
	info := reader.info
	info.Metadata = append([]byte(nil), reader.info.Metadata...)
	return info
}

// Read returns plaintext only after its containing chunk authenticates.
func (reader *Reader) Read(destination []byte) (int, error) {
	if reader.closed {
		return 0, ErrClosed
	}
	if reader.terminalErr != nil {
		return 0, reader.terminalErr
	}
	if len(destination) == 0 {
		return 0, nil
	}
	for {
		if reader.plainOffset < len(reader.plaintext) {
			remaining := len(reader.plaintext) - reader.plainOffset
			if reader.nextChunk == reader.header.chunkCount && len(destination) >= remaining && !reader.finished {
				if err := reader.verifyContainerEnd(); err != nil {
					clear(reader.plaintext)
					reader.plaintext = nil
					reader.terminalErr = err
					return 0, err
				}
				reader.finished = true
			}
			read := copy(destination, reader.plaintext[reader.plainOffset:])
			reader.plainOffset += read
			if reader.plainOffset == len(reader.plaintext) {
				clear(reader.plaintext)
				reader.plaintext = nil
				reader.plainOffset = 0
			}
			return read, nil
		}
		if reader.nextChunk == reader.header.chunkCount {
			if !reader.finished {
				if err := reader.verifyContainerEnd(); err != nil {
					reader.terminalErr = err
					return 0, err
				}
				reader.finished = true
			}
			return 0, io.EOF
		}
		if err := reader.loadNextChunk(); err != nil {
			reader.terminalErr = err
			return 0, err
		}
	}
}

// Close authenticates and discards any unread plaintext, then closes the
// underlying source when it implements io.Closer.
func (reader *Reader) Close() error {
	if reader.closed {
		return reader.closeErr
	}
	var integrityErr error
	if reader.terminalErr != nil {
		integrityErr = reader.terminalErr
	} else if !reader.finished {
		discard := make([]byte, 32<<10)
		for {
			_, err := reader.Read(discard)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				integrityErr = err
				break
			}
		}
		clear(discard)
	}
	clear(reader.plaintext)
	reader.plaintext = nil
	reader.closed = true
	var sourceErr error
	if reader.closer != nil {
		sourceErr = reader.closer.Close()
	}
	reader.closeErr = errors.Join(integrityErr, sourceErr)
	return reader.closeErr
}

func (reader *Reader) loadNextChunk() error {
	if err := validateContext(reader.ctx); err != nil {
		return err
	}
	if reader.nextChunk >= reader.header.chunkCount || reader.produced > reader.header.plaintextSize {
		return fmt.Errorf("%w: invalid chunk state", ErrInvalidFormat)
	}
	plainSize := expectedPlaintextChunk(reader.header.plaintextSize, reader.produced, reader.header.chunkSize)
	ciphertextSize := int(plainSize) + reader.aead.Overhead()
	ciphertext := make([]byte, ciphertextSize)
	if err := readContainerBytes(reader.source, ciphertext); err != nil {
		return fmt.Errorf("%w: chunk %d", err, reader.nextChunk)
	}
	nonce := makeNonce(reader.header.noncePrefix, reader.nextChunk)
	setChunkAAD(reader.chunkAAD, reader.nextChunk, reader.header.plaintextSize, plainSize)
	plaintext, err := reader.aead.Open(nil, nonce[:], ciphertext, reader.chunkAAD)
	clear(ciphertext)
	if err != nil || len(plaintext) != int(plainSize) {
		clear(plaintext)
		return fmt.Errorf("%w: chunk %d", ErrAuthentication, reader.nextChunk)
	}
	reader.nextChunk++
	reader.produced += uint64(plainSize)
	reader.plaintext = plaintext
	reader.plainOffset = 0
	return nil
}

func (reader *Reader) verifyContainerEnd() error {
	if reader.produced != reader.header.plaintextSize || reader.nextChunk != reader.header.chunkCount {
		return fmt.Errorf("%w: inconsistent final size", ErrInvalidFormat)
	}
	hasMore, err := hasTrailingByte(reader.source)
	if err != nil {
		return fmt.Errorf("modelprotection: verify container EOF: %w", err)
	}
	if hasMore {
		return ErrTrailingData
	}
	return nil
}
