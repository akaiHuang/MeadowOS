// Package modelprotection implements a versioned, authenticated container for
// encrypted model assets.
package modelprotection

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// DataKeySize is the only supported AES data-key size.
	DataKeySize = 32

	// DefaultChunkSize bounds plaintext retained by a streaming reader or writer.
	DefaultChunkSize uint32 = 4 << 20
	// MaxChunkSize prevents hostile headers from causing unbounded allocations.
	MaxChunkSize uint32 = 64 << 20
	// MaxMetadataSize prevents hostile headers from causing unbounded allocations.
	MaxMetadataSize = 16 << 20
	// MaxWrappedKeySize prevents hostile wrappers from causing unbounded allocations.
	MaxWrappedKeySize = 1 << 20

	fixedHeaderSize = 64
	formatVersion   = 1
	algorithmAESGCM = 1
	noncePrefixSize = 4
	chunkAADSize    = 20

	containerAADDomain = "meadow/model-protection/container/aes-256-gcm/v1\x00"
)

var (
	containerMagic = [8]byte{'M', 'D', 'L', 'P', 'R', 'O', 'T', 0}

	ErrInvalidArgument = errors.New("modelprotection: invalid argument")
	ErrInvalidFormat   = errors.New("modelprotection: invalid container format")
	ErrUnsupported     = errors.New("modelprotection: unsupported container")
	ErrAuthentication  = errors.New("modelprotection: authentication failed")
	ErrTruncated       = errors.New("modelprotection: truncated container")
	ErrTrailingData    = errors.New("modelprotection: trailing container data")
	ErrSizeMismatch    = errors.New("modelprotection: plaintext size mismatch")
	ErrKeyWrap         = errors.New("modelprotection: data-key wrapping failed")
	ErrKeyUnwrap       = errors.New("modelprotection: data-key unwrapping failed")
	ErrClosed          = errors.New("modelprotection: reader is closed")
)

// KeyWrapper protects a freshly generated data key for storage in a container.
// Implementations must not return the key unchanged.
type KeyWrapper interface {
	WrapKey(context.Context, []byte) ([]byte, error)
}

// KeyUnwrapper recovers a data key from a container's wrapped-key field.
type KeyUnwrapper interface {
	UnwrapKey(context.Context, []byte) ([]byte, error)
}

// EncryptOptions controls container construction.
type EncryptOptions struct {
	ChunkSize uint32
	Metadata  []byte
}

// Info describes an authenticated container. NewReader authenticates the first
// chunk before returning this information to the caller.
type Info struct {
	PlaintextSize uint64
	ChunkSize     uint32
	ChunkCount    uint64
	Metadata      []byte
}

type containerHeader struct {
	chunkSize     uint32
	metadataSize  uint32
	wrappedSize   uint32
	noncePrefix   [noncePrefixSize]byte
	plaintextSize uint64
	chunkCount    uint64
}

func encodeHeader(header containerHeader) [fixedHeaderSize]byte {
	var raw [fixedHeaderSize]byte
	copy(raw[0:8], containerMagic[:])
	binary.BigEndian.PutUint16(raw[8:10], formatVersion)
	binary.BigEndian.PutUint16(raw[10:12], fixedHeaderSize)
	binary.BigEndian.PutUint16(raw[12:14], algorithmAESGCM)
	binary.BigEndian.PutUint16(raw[14:16], 0)
	binary.BigEndian.PutUint32(raw[16:20], header.chunkSize)
	binary.BigEndian.PutUint32(raw[20:24], header.metadataSize)
	binary.BigEndian.PutUint32(raw[24:28], header.wrappedSize)
	copy(raw[28:32], header.noncePrefix[:])
	binary.BigEndian.PutUint64(raw[32:40], header.plaintextSize)
	binary.BigEndian.PutUint64(raw[40:48], header.chunkCount)
	return raw
}

func decodeHeader(raw []byte) (containerHeader, error) {
	if len(raw) != fixedHeaderSize {
		return containerHeader{}, ErrTruncated
	}
	if string(raw[0:8]) != string(containerMagic[:]) {
		return containerHeader{}, fmt.Errorf("%w: bad magic", ErrInvalidFormat)
	}
	if binary.BigEndian.Uint16(raw[8:10]) != formatVersion ||
		binary.BigEndian.Uint16(raw[10:12]) != fixedHeaderSize ||
		binary.BigEndian.Uint16(raw[12:14]) != algorithmAESGCM {
		return containerHeader{}, ErrUnsupported
	}
	if binary.BigEndian.Uint16(raw[14:16]) != 0 || !allZero(raw[48:64]) {
		return containerHeader{}, fmt.Errorf("%w: reserved fields are non-zero", ErrInvalidFormat)
	}
	header := containerHeader{
		chunkSize:     binary.BigEndian.Uint32(raw[16:20]),
		metadataSize:  binary.BigEndian.Uint32(raw[20:24]),
		wrappedSize:   binary.BigEndian.Uint32(raw[24:28]),
		plaintextSize: binary.BigEndian.Uint64(raw[32:40]),
		chunkCount:    binary.BigEndian.Uint64(raw[40:48]),
	}
	copy(header.noncePrefix[:], raw[28:32])
	if header.chunkSize == 0 || header.chunkSize > MaxChunkSize {
		return containerHeader{}, fmt.Errorf("%w: invalid chunk size", ErrInvalidFormat)
	}
	if header.metadataSize > MaxMetadataSize {
		return containerHeader{}, fmt.Errorf("%w: metadata is too large", ErrInvalidFormat)
	}
	if header.wrappedSize == 0 || header.wrappedSize > MaxWrappedKeySize {
		return containerHeader{}, fmt.Errorf("%w: invalid wrapped-key size", ErrInvalidFormat)
	}
	if header.chunkCount != expectedChunkCount(header.plaintextSize, header.chunkSize) {
		return containerHeader{}, fmt.Errorf("%w: inconsistent chunk count", ErrInvalidFormat)
	}
	return header, nil
}

func expectedChunkCount(total uint64, chunkSize uint32) uint64 {
	if total == 0 {
		// An authenticated empty chunk protects the envelope of an empty asset.
		return 1
	}
	return 1 + (total-1)/uint64(chunkSize)
}

func expectedPlaintextChunk(total, produced uint64, chunkSize uint32) uint32 {
	remaining := total - produced
	if remaining > uint64(chunkSize) {
		return chunkSize
	}
	return uint32(remaining)
}

func makeBaseAAD(rawHeader, metadata, wrappedKey []byte) []byte {
	base := make([]byte, len(containerAADDomain)+len(rawHeader)+len(metadata)+len(wrappedKey))
	offset := copy(base, containerAADDomain)
	offset += copy(base[offset:], rawHeader)
	offset += copy(base[offset:], metadata)
	copy(base[offset:], wrappedKey)
	return base
}

func makeChunkAAD(base []byte) []byte {
	aad := make([]byte, len(base)+chunkAADSize)
	copy(aad, base)
	return aad
}

func setChunkAAD(aad []byte, index, total uint64, plaintextSize uint32) {
	offset := len(aad) - chunkAADSize
	binary.BigEndian.PutUint64(aad[offset:offset+8], index)
	binary.BigEndian.PutUint64(aad[offset+8:offset+16], total)
	binary.BigEndian.PutUint32(aad[offset+16:offset+20], plaintextSize)
}

func makeNonce(prefix [noncePrefixSize]byte, index uint64) [12]byte {
	var nonce [12]byte
	copy(nonce[0:noncePrefixSize], prefix[:])
	binary.BigEndian.PutUint64(nonce[noncePrefixSize:], index)
	return nonce
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func readContainerBytes(reader io.Reader, destination []byte) error {
	read, err := io.ReadFull(reader, destination)
	if err != nil {
		return fmt.Errorf("%w: read %d of %d bytes", ErrTruncated, read, len(destination))
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written < 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func hasTrailingByte(reader io.Reader) (bool, error) {
	var probe [1]byte
	for emptyReads := 0; emptyReads < 100; emptyReads++ {
		read, err := reader.Read(probe[:])
		if read < 0 || read > len(probe) {
			return false, fmt.Errorf("%w: invalid reader count", ErrInvalidFormat)
		}
		if read > 0 {
			return true, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
	}
	return false, io.ErrNoProgress
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}
