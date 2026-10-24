package nativeassettransport

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

const (
	ProtocolVersion uint16 = 2
	HeaderBytes            = 80
	PeerSecretBytes        = 32
	SessionIDBytes         = 16
	ChallengeBytes         = 32

	MaxChunkBytes       uint32 = 4 << 20
	MaxControlBytes     uint32 = 512
	MaxWirePayloadBytes uint32 = MaxChunkBytes + 128

	ChildSocketExtraFileIndex = 0
	ChildSecretExtraFileIndex = 1
	ChildSocketFD             = 3
	ChildSecretFD             = 4
)

var (
	ErrInvalidArgument    = errors.New("native asset transport: invalid argument")
	ErrUnavailable        = errors.New("native asset transport: unavailable")
	ErrBindingRejected    = errors.New("native asset transport: binding rejected")
	ErrIncompleteManifest = errors.New("native asset transport: incomplete required manifest")
	ErrAuthentication     = errors.New("native asset transport: authentication failed")
	ErrProtocol           = errors.New("native asset transport: protocol violation")
	ErrReplay             = errors.New("native asset transport: replay or sequence mismatch")
	ErrDuplicateAsset     = errors.New("native asset transport: duplicate asset request")
	ErrOutOfOrderAsset    = errors.New("native asset transport: out-of-order asset request")
	ErrMissingAsset       = errors.New("native asset transport: required asset unavailable")
	ErrResourceLimit      = errors.New("native asset transport: resource limit")
	ErrCanceled           = errors.New("native asset transport: canceled")
	ErrPeerClosed         = errors.New("native asset transport: peer closed")
	ErrTerminalAck        = errors.New("native asset transport: terminal acknowledgement rejected")
)

var (
	wireMagic              = [8]byte{'M', 'D', 'W', 'A', 'S', 'T', '0', '2'}
	macDomain              = []byte("meadow-native-asset-frame-v2\x00")
	helloLabel             = []byte("meadow-native-asset-hello-v2\x00")
	assetEvidenceLabel     = []byte("meadow-native-asset-evidence-v2\x00")
	aggregateEvidenceLabel = []byte("meadow-native-asset-aggregate-v2\x00")
)

type FrameType uint16

const (
	FrameServerHello          FrameType = 1
	FrameClientAuth           FrameType = 2
	FrameAssetRequest         FrameType = 3
	FrameAssetBegin           FrameType = 4
	FrameChunk                FrameType = 5
	FrameChunkAck             FrameType = 6
	FrameAssetEOF             FrameType = 7
	FrameAssetTerminalAck     FrameType = 8
	FrameAggregateEOF         FrameType = 9
	FrameAggregateTerminalAck FrameType = 10
	FrameCancel               FrameType = 11
)

type Options struct {
	ChunkBytes uint32
}

type wireFrame struct {
	typeID  FrameType
	seq     uint64
	session [SessionIDBytes]byte
	payload []byte
}

func encodeHello(
	verified VerifiedManifest,
	challenge [ChallengeBytes]byte,
) ([]byte, error) {
	manifest := verified.manifest
	bytesRequired := 2 + len(manifest.ModelID) + 32 + 4 + 8 + 32 + 32 + 4 + 4 + 32
	for _, asset := range manifest.Assets {
		bytesRequired += 4 + 4 + 4 + 2 + 2 + 8 + 32 + len(asset.AssetID)
	}
	if bytesRequired > MaxManifestBytes || bytesRequired > math.MaxUint32 {
		return nil, ErrResourceLimit
	}
	payload := make([]byte, bytesRequired)
	offset := 0
	binary.BigEndian.PutUint16(payload[offset:offset+2], uint16(len(manifest.ModelID)))
	offset += 2
	copy(payload[offset:], manifest.ModelID)
	offset += len(manifest.ModelID)
	copy(payload[offset:offset+32], manifest.ProfileDigest[:])
	offset += 32
	binary.BigEndian.PutUint32(payload[offset:offset+4], uint32(manifest.Backend))
	offset += 4
	binary.BigEndian.PutUint64(payload[offset:offset+8], manifest.ProfileRevision)
	offset += 8
	copy(payload[offset:offset+32], manifest.ManifestDigest[:])
	offset += 32
	copy(payload[offset:offset+32], verified.canonicalDigest[:])
	offset += 32
	binary.BigEndian.PutUint32(payload[offset:offset+4], uint32(len(manifest.Assets)))
	offset += 4
	binary.BigEndian.PutUint32(payload[offset:offset+4], verified.requiredCount)
	offset += 4
	for _, asset := range manifest.Assets {
		binary.BigEndian.PutUint32(payload[offset:offset+4], asset.Ordinal)
		offset += 4
		binary.BigEndian.PutUint32(payload[offset:offset+4], uint32(asset.Role))
		offset += 4
		if asset.Required {
			binary.BigEndian.PutUint32(payload[offset:offset+4], 1)
		}
		offset += 4
		binary.BigEndian.PutUint16(payload[offset:offset+2], uint16(len(asset.AssetID)))
		offset += 4
		binary.BigEndian.PutUint64(payload[offset:offset+8], asset.PlaintextSize)
		offset += 8
		copy(payload[offset:offset+32], asset.Binding.AssetDigest[:])
		offset += 32
		copy(payload[offset:], asset.AssetID)
		offset += len(asset.AssetID)
	}
	copy(payload[offset:offset+32], challenge[:])
	return payload, nil
}

func helloProof(payload []byte) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write(helloLabel)
	_, _ = hash.Write(payload)
	var proof [sha256.Size]byte
	copy(proof[:], hash.Sum(nil))
	return proof
}

func encodeAssetRequest(asset SignedAsset) []byte {
	payload := make([]byte, 12+len(asset.AssetID))
	binary.BigEndian.PutUint32(payload[0:4], asset.Ordinal)
	binary.BigEndian.PutUint32(payload[4:8], uint32(asset.Role))
	binary.BigEndian.PutUint16(payload[8:10], uint16(len(asset.AssetID)))
	copy(payload[12:], asset.AssetID)
	return payload
}

func decodeAssetRequest(payload []byte) (uint32, AssetRole, string, error) {
	if len(payload) < 12 || binary.BigEndian.Uint16(payload[10:12]) != 0 {
		return 0, 0, "", ErrProtocol
	}
	idBytes := int(binary.BigEndian.Uint16(payload[8:10]))
	if idBytes == 0 || idBytes > MaxAssetIDBytes || len(payload) != 12+idBytes {
		return 0, 0, "", ErrProtocol
	}
	assetID := string(payload[12:])
	if !validIdentifier(assetID, MaxAssetIDBytes) {
		return 0, 0, "", ErrProtocol
	}
	return binary.BigEndian.Uint32(payload[0:4]),
		AssetRole(binary.BigEndian.Uint32(payload[4:8])), assetID, nil
}

func assetProof(asset SignedAsset, canonical [sha256.Size]byte) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("meadow-native-asset-binding-v2\x00"))
	_, _ = hash.Write(canonical[:])
	writeHashUint32(hash, asset.Ordinal)
	writeHashUint32(hash, uint32(asset.Role))
	writeHashString(hash, asset.AssetID)
	writeHashUint64(hash, asset.PlaintextSize)
	_, _ = hash.Write(asset.Binding.AssetDigest[:])
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func encodeAssetBegin(asset SignedAsset, proof [sha256.Size]byte, preferred, maximum uint32) []byte {
	payload := make([]byte, 92+len(asset.AssetID))
	binary.BigEndian.PutUint32(payload[0:4], asset.Ordinal)
	binary.BigEndian.PutUint32(payload[4:8], uint32(asset.Role))
	binary.BigEndian.PutUint16(payload[8:10], uint16(len(asset.AssetID)))
	binary.BigEndian.PutUint64(payload[12:20], asset.PlaintextSize)
	binary.BigEndian.PutUint32(payload[20:24], preferred)
	binary.BigEndian.PutUint32(payload[24:28], maximum)
	copy(payload[28:60], asset.Binding.AssetDigest[:])
	copy(payload[60:92], proof[:])
	copy(payload[92:], asset.AssetID)
	return payload
}

func encodeChunkPrefix(ordinal uint32, index, offset uint64, data []byte) [64]byte {
	var prefix [64]byte
	binary.BigEndian.PutUint32(prefix[0:4], ordinal)
	binary.BigEndian.PutUint64(prefix[8:16], index)
	binary.BigEndian.PutUint64(prefix[16:24], offset)
	binary.BigEndian.PutUint32(prefix[24:28], uint32(len(data)))
	digest := sha256.Sum256(data)
	copy(prefix[32:64], digest[:])
	return prefix
}

func encodeChunkAck(ordinal uint32, index, nextOffset uint64, digest []byte) [56]byte {
	var payload [56]byte
	binary.BigEndian.PutUint32(payload[0:4], ordinal)
	binary.BigEndian.PutUint64(payload[8:16], index)
	binary.BigEndian.PutUint64(payload[16:24], nextOffset)
	copy(payload[24:56], digest)
	return payload
}

func encodeAssetTerminal(
	asset SignedAsset,
	total, chunks uint64,
	streamDigest [sha256.Size]byte,
	evidenceDigest [sha256.Size]byte,
) [120]byte {
	var payload [120]byte
	binary.BigEndian.PutUint32(payload[0:4], asset.Ordinal)
	binary.BigEndian.PutUint32(payload[4:8], uint32(asset.Role))
	binary.BigEndian.PutUint64(payload[8:16], total)
	binary.BigEndian.PutUint64(payload[16:24], chunks)
	copy(payload[24:56], streamDigest[:])
	copy(payload[56:88], asset.Binding.AssetDigest[:])
	copy(payload[88:120], evidenceDigest[:])
	return payload
}

func writeFrame(
	w io.Writer,
	secret *[PeerSecretBytes]byte,
	session [SessionIDBytes]byte,
	typeID FrameType,
	sequence uint64,
	parts ...[]byte,
) error {
	if w == nil || secret == nil || typeID < FrameServerHello || typeID > FrameCancel {
		return ErrInvalidArgument
	}
	var payloadBytes uint64
	for _, part := range parts {
		if uint64(len(part)) > uint64(math.MaxUint32)-payloadBytes {
			return ErrResourceLimit
		}
		payloadBytes += uint64(len(part))
	}
	if payloadBytes > uint64(MaxWirePayloadBytes) && payloadBytes > MaxManifestBytes {
		return ErrResourceLimit
	}
	var header [HeaderBytes]byte
	copy(header[0:8], wireMagic[:])
	binary.BigEndian.PutUint16(header[8:10], ProtocolVersion)
	binary.BigEndian.PutUint16(header[10:12], uint16(typeID))
	binary.BigEndian.PutUint64(header[16:24], sequence)
	binary.BigEndian.PutUint32(header[24:28], uint32(payloadBytes))
	copy(header[32:48], session[:])
	mac := hmac.New(sha256.New, secret[:])
	_, _ = mac.Write(macDomain)
	_, _ = mac.Write(header[:48])
	for _, part := range parts {
		_, _ = mac.Write(part)
	}
	sum := mac.Sum(nil)
	copy(header[48:80], sum)
	clear(sum)
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	for _, part := range parts {
		if err := writeAll(w, part); err != nil {
			return err
		}
	}
	return nil
}

func readFrame(
	r io.Reader,
	secret *[PeerSecretBytes]byte,
	expectedSession *[SessionIDBytes]byte,
	expectedSequence uint64,
	maxPayload uint32,
) (wireFrame, error) {
	var frame wireFrame
	if r == nil || secret == nil || maxPayload == 0 {
		return frame, ErrInvalidArgument
	}
	var header [HeaderBytes]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame, mapPeerError(err)
	}
	if subtle.ConstantTimeCompare(header[0:8], wireMagic[:]) != 1 ||
		binary.BigEndian.Uint16(header[8:10]) != ProtocolVersion ||
		binary.BigEndian.Uint32(header[12:16]) != 0 ||
		binary.BigEndian.Uint32(header[28:32]) != 0 {
		return frame, ErrProtocol
	}
	typeID := FrameType(binary.BigEndian.Uint16(header[10:12]))
	if typeID < FrameServerHello || typeID > FrameCancel {
		return frame, ErrProtocol
	}
	sequence := binary.BigEndian.Uint64(header[16:24])
	if sequence != expectedSequence {
		return frame, ErrReplay
	}
	copy(frame.session[:], header[32:48])
	if expectedSession != nil &&
		subtle.ConstantTimeCompare(frame.session[:], expectedSession[:]) != 1 {
		return wireFrame{}, ErrReplay
	}
	payloadBytes := binary.BigEndian.Uint32(header[24:28])
	if payloadBytes > maxPayload {
		return wireFrame{}, ErrResourceLimit
	}
	frame.payload = make([]byte, int(payloadBytes))
	if _, err := io.ReadFull(r, frame.payload); err != nil {
		clear(frame.payload)
		return wireFrame{}, mapPeerError(err)
	}
	mac := hmac.New(sha256.New, secret[:])
	_, _ = mac.Write(macDomain)
	_, _ = mac.Write(header[:48])
	_, _ = mac.Write(frame.payload)
	expectedMAC := mac.Sum(nil)
	authenticated := hmac.Equal(header[48:80], expectedMAC)
	clear(expectedMAC)
	if !authenticated {
		clear(frame.payload)
		return wireFrame{}, ErrAuthentication
	}
	frame.typeID = typeID
	frame.seq = sequence
	return frame, nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := w.Write(data)
		if err != nil {
			return mapPeerError(err)
		}
		if written <= 0 || written > len(data) {
			return ErrPeerClosed
		}
		data = data[written:]
	}
	return nil
}

func mapPeerError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrPeerClosed
	}
	return errors.Join(ErrPeerClosed, err)
}
