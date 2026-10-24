package keybroker

import (
	"crypto/subtle"
	"encoding/binary"
	"runtime"
)

const (
	recordHeaderSize          = 82
	modelDataKeySize          = 32
	resourceVaultKeySize      = 32
	maxLicenseDeviceStateSize = 1 << 20
	maxStoredRecordSize       = recordHeaderSize + maxLicenseDeviceStateSize
)

var recordMagic = [4]byte{'M', 'K', 'B', '1'}

type namespaceID byte

const (
	namespaceModelData namespaceID = iota + 1
	namespaceResourceVault
	namespaceLicenseDeviceState
)

type recordKind byte

const (
	recordKindKey recordKind = iota + 1
	recordKindState
)

func encodeRecord(namespace namespaceID, kind recordKind, version Version, resourceDigest, binding [32]byte, payload []byte) []byte {
	record := make([]byte, recordHeaderSize+len(payload))
	copy(record[0:4], recordMagic[:])
	record[4] = byte(namespace)
	record[5] = byte(kind)
	binary.BigEndian.PutUint64(record[6:14], uint64(version))
	copy(record[14:46], resourceDigest[:])
	copy(record[46:78], binding[:])
	binary.BigEndian.PutUint32(record[78:82], uint32(len(payload)))
	copy(record[recordHeaderSize:], payload)
	return record
}

func decodeRecord(blob []byte, namespace namespaceID, kind recordKind, version Version, resourceDigest, binding [32]byte) ([]byte, error) {
	if len(blob) < recordHeaderSize || len(blob) > maxStoredRecordSize {
		return nil, ErrCorruptRecord
	}
	if subtle.ConstantTimeCompare(blob[0:4], recordMagic[:]) != 1 ||
		blob[4] != byte(namespace) || blob[5] != byte(kind) ||
		Version(binary.BigEndian.Uint64(blob[6:14])) != version ||
		subtle.ConstantTimeCompare(blob[14:46], resourceDigest[:]) != 1 {
		return nil, ErrCorruptRecord
	}
	if subtle.ConstantTimeCompare(blob[46:78], binding[:]) != 1 {
		return nil, ErrDeviceBindingMismatch
	}
	payloadSize := int(binary.BigEndian.Uint32(blob[78:82]))
	if payloadSize <= 0 || payloadSize != len(blob)-recordHeaderSize {
		return nil, ErrCorruptRecord
	}
	if kind == recordKindKey {
		expectedSize := modelDataKeySize
		if namespace == namespaceResourceVault {
			expectedSize = resourceVaultKeySize
		}
		if payloadSize != expectedSize {
			return nil, ErrCorruptRecord
		}
	} else if kind != recordKindState || payloadSize > maxLicenseDeviceStateSize {
		return nil, ErrCorruptRecord
	}
	payload := make([]byte, payloadSize)
	copy(payload, blob[recordHeaderSize:])
	return payload, nil
}

func zeroBytes(value []byte) {
	clear(value)
	runtime.KeepAlive(value)
}
