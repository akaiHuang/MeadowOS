package nativeassettransport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/protectedmodelbridge"
)

type Server struct {
	conn     *net.UnixConn
	resolver AssetResolver
	manifest VerifiedManifest
	options  Options

	mu          sync.Mutex
	secret      [PeerSecretBytes]byte
	started     bool
	closed      bool
	serveStop   context.CancelFunc
	activeLease protectedmodelbridge.SequentialLease
	evidence    AggregateTerminalEvidence
	ready       bool
	closeOnce   sync.Once
	closeErr    error
}

func NewServer(
	conn *net.UnixConn,
	peerSecret []byte,
	resolver AssetResolver,
	manifest VerifiedManifest,
	options Options,
) (*Server, error) {
	if conn == nil || resolver == nil || len(peerSecret) != PeerSecretBytes ||
		manifest.requiredCount == 0 || len(manifest.manifest.Assets) == 0 {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, ErrInvalidArgument
	}
	if options.ChunkBytes > MaxChunkBytes {
		_ = conn.Close()
		return nil, ErrResourceLimit
	}
	server := &Server{conn: conn, resolver: resolver, manifest: manifest, options: options}
	copy(server.secret[:], peerSecret)
	return server, nil
}

func (server *Server) Serve(ctx context.Context) (result error) {
	if server == nil || ctx == nil {
		return ErrInvalidArgument
	}
	server.mu.Lock()
	if server.started || server.closed || server.conn == nil {
		server.mu.Unlock()
		return ErrProtocol
	}
	server.started = true
	serveContext, cancel := context.WithCancel(ctx)
	server.serveStop = cancel
	var secret [PeerSecretBytes]byte
	copy(secret[:], server.secret[:])
	clear(server.secret[:])
	server.mu.Unlock()

	succeeded := false
	defer func() {
		cancel()
		clear(secret[:])
		server.mu.Lock()
		server.closed = true
		server.serveStop = nil
		active := server.activeLease
		server.activeLease = nil
		server.mu.Unlock()
		_ = server.conn.Close()
		if !succeeded && active != nil {
			result = errors.Join(result, active.Cancel())
		}
	}()

	watchDone := make(chan struct{})
	go func() {
		select {
		case <-serveContext.Done():
			_ = server.conn.SetDeadline(time.Now())
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	var session [SessionIDBytes]byte
	var challenge [ChallengeBytes]byte
	if _, err := rand.Read(session[:]); err != nil {
		return ErrUnavailable
	}
	if _, err := rand.Read(challenge[:]); err != nil {
		return ErrUnavailable
	}
	defer clear(challenge[:])
	hello, err := encodeHello(server.manifest, challenge)
	if err != nil {
		return err
	}
	defer clear(hello)
	proof := helloProof(hello)
	if err := writeFrame(server.conn, &secret, session, FrameServerHello, 0, hello); err != nil {
		return err
	}
	auth, err := readFrame(server.conn, &secret, &session, 1, MaxControlBytes)
	if err != nil {
		return err
	}
	if auth.typeID != FrameClientAuth || len(auth.payload) != sha256.Size ||
		subtle.ConstantTimeCompare(auth.payload, proof[:]) != 1 {
		clear(auth.payload)
		return ErrAuthentication
	}
	clear(auth.payload)

	sequence := uint64(2)
	assetEvidence := make([]AssetTerminalEvidence, 0, server.manifest.requiredCount)
	for expectedOrdinal := uint32(0); expectedOrdinal < server.manifest.requiredCount; expectedOrdinal++ {
		request, err := readFrame(server.conn, &secret, &session, sequence, MaxControlBytes)
		if err != nil {
			return err
		}
		sequence++
		if request.typeID == FrameCancel {
			clear(request.payload)
			return ErrCanceled
		}
		if request.typeID != FrameAssetRequest {
			clear(request.payload)
			return ErrProtocol
		}
		ordinal, role, assetID, err := decodeAssetRequest(request.payload)
		clear(request.payload)
		if err != nil {
			return err
		}
		if ordinal < expectedOrdinal {
			return ErrDuplicateAsset
		}
		if ordinal > expectedOrdinal {
			return ErrOutOfOrderAsset
		}
		asset := server.manifest.manifest.Assets[expectedOrdinal]
		if role != asset.Role || assetID != asset.AssetID || !asset.Required {
			return ErrBindingRejected
		}
		evidence, err := server.serveAsset(serveContext, &secret, session, &sequence, asset)
		if err != nil {
			return err
		}
		assetEvidence = append(assetEvidence, evidence)
	}
	if uint32(len(assetEvidence)) != server.manifest.requiredCount {
		return ErrIncompleteManifest
	}
	aggregate := aggregateEvidence(server.manifest, session, assetEvidence)
	aggregatePayload := encodeAggregateTerminal(aggregate)
	if err := writeFrame(server.conn, &secret, session, FrameAggregateEOF, sequence,
		aggregatePayload[:]); err != nil {
		return err
	}
	sequence++
	aggregateAck, err := readFrame(server.conn, &secret, &session, sequence, MaxControlBytes)
	if err != nil {
		return err
	}
	validAggregate := aggregateAck.typeID == FrameAggregateTerminalAck &&
		len(aggregateAck.payload) == len(aggregatePayload) &&
		subtle.ConstantTimeCompare(aggregateAck.payload, aggregatePayload[:]) == 1
	clear(aggregateAck.payload)
	if !validAggregate {
		return ErrTerminalAck
	}
	server.mu.Lock()
	server.evidence = cloneAggregateEvidence(aggregate)
	server.ready = true
	server.mu.Unlock()
	succeeded = true
	return nil
}

func (server *Server) serveAsset(
	ctx context.Context,
	secret *[PeerSecretBytes]byte,
	session [SessionIDBytes]byte,
	sequence *uint64,
	asset SignedAsset,
) (AssetTerminalEvidence, error) {
	lease, err := server.resolver.ResolveSignedAsset(ctx, asset)
	if err != nil || lease == nil {
		if lease != nil {
			_ = lease.Cancel()
		}
		return AssetTerminalEvidence{}, errors.Join(ErrMissingAsset, err)
	}
	success := false
	server.mu.Lock()
	if server.activeLease != nil {
		server.mu.Unlock()
		_ = lease.Cancel()
		return AssetTerminalEvidence{}, ErrProtocol
	}
	server.activeLease = lease
	server.mu.Unlock()
	defer func() {
		server.mu.Lock()
		if server.activeLease == lease {
			server.activeLease = nil
		}
		server.mu.Unlock()
		if !success {
			_ = lease.Cancel()
		}
	}()
	info := lease.Info()
	if !asset.matchLease(info) {
		return AssetTerminalEvidence{}, ErrBindingRejected
	}
	chunkSize := server.options.ChunkBytes
	if chunkSize == 0 {
		chunkSize = info.PreferredChunkBytes
	}
	if chunkSize == 0 || chunkSize > MaxChunkBytes || chunkSize > info.MaxChunkBytes {
		return AssetTerminalEvidence{}, ErrResourceLimit
	}
	proof := assetProof(asset, server.manifest.canonicalDigest)
	begin := encodeAssetBegin(asset, proof, chunkSize, info.MaxChunkBytes)
	if err := writeFrame(server.conn, secret, session, FrameAssetBegin, *sequence, begin); err != nil {
		clear(begin)
		return AssetTerminalEvidence{}, err
	}
	clear(begin)
	*sequence++

	streamHash := sha256.New()
	offset := uint64(0)
	chunkIndex := uint64(0)
	for offset < asset.PlaintextSize {
		if err := ctx.Err(); err != nil {
			return AssetTerminalEvidence{}, errors.Join(ErrCanceled, err)
		}
		requested := chunkSize
		if remaining := asset.PlaintextSize - offset; uint64(requested) > remaining {
			requested = uint32(remaining)
		}
		expectedOffset := offset
		var callbackErr error
		read, consumeErr := lease.ConsumeNext(ctx, requested,
			func(callbackContext context.Context, callbackOffset uint64, data []byte) error {
				if callbackContext == nil || callbackContext.Err() != nil ||
					callbackOffset != expectedOffset || len(data) == 0 ||
					len(data) > int(requested) || len(data) > int(MaxChunkBytes) {
					callbackErr = ErrProtocol
					return callbackErr
				}
				prefix := encodeChunkPrefix(asset.Ordinal, chunkIndex, callbackOffset, data)
				_, _ = streamHash.Write(data)
				if err := writeFrame(server.conn, secret, session, FrameChunk, *sequence,
					prefix[:], data); err != nil {
					callbackErr = err
					return err
				}
				*sequence++
				ack, err := readFrame(server.conn, secret, &session, *sequence, MaxControlBytes)
				if err != nil {
					callbackErr = err
					return err
				}
				*sequence++
				if ack.typeID == FrameCancel {
					clear(ack.payload)
					callbackErr = ErrCanceled
					return callbackErr
				}
				if ack.typeID != FrameChunkAck || len(ack.payload) != 56 {
					clear(ack.payload)
					callbackErr = ErrProtocol
					return callbackErr
				}
				ackOrdinal := binary.BigEndian.Uint32(ack.payload[0:4])
				ackIndex := binary.BigEndian.Uint64(ack.payload[8:16])
				ackOffset := binary.BigEndian.Uint64(ack.payload[16:24])
				digestMatches := subtle.ConstantTimeCompare(ack.payload[24:56], prefix[32:64]) == 1
				clear(ack.payload)
				if ackOrdinal != asset.Ordinal || ackIndex != chunkIndex {
					callbackErr = ErrReplay
					return callbackErr
				}
				if ackOffset != callbackOffset+uint64(len(data)) || !digestMatches {
					callbackErr = ErrProtocol
					return callbackErr
				}
				return nil
			})
		if callbackErr != nil {
			return AssetTerminalEvidence{}, callbackErr
		}
		if consumeErr != nil {
			return AssetTerminalEvidence{}, errors.Join(ErrProtocol, consumeErr)
		}
		if read == 0 || read > requested {
			return AssetTerminalEvidence{}, ErrProtocol
		}
		offset += uint64(read)
		chunkIndex++
	}
	if offset != asset.PlaintextSize || lease.ConsumedBytes() != asset.PlaintextSize {
		return AssetTerminalEvidence{}, ErrProtocol
	}
	var streamDigest [sha256.Size]byte
	copy(streamDigest[:], streamHash.Sum(nil))
	evidenceDigest := assetEvidenceDigest(
		server.manifest, session, asset, offset, chunkIndex, streamDigest)
	terminal := encodeAssetTerminal(asset, offset, chunkIndex, streamDigest, evidenceDigest)
	if err := writeFrame(server.conn, secret, session, FrameAssetEOF, *sequence,
		terminal[:]); err != nil {
		return AssetTerminalEvidence{}, err
	}
	*sequence++
	ack, err := readFrame(server.conn, secret, &session, *sequence, MaxControlBytes)
	if err != nil {
		return AssetTerminalEvidence{}, err
	}
	*sequence++
	valid := ack.typeID == FrameAssetTerminalAck && len(ack.payload) == len(terminal) &&
		subtle.ConstantTimeCompare(ack.payload, terminal[:]) == 1
	clear(ack.payload)
	if !valid {
		return AssetTerminalEvidence{}, ErrTerminalAck
	}
	if err := lease.Close(); err != nil {
		return AssetTerminalEvidence{}, errors.Join(ErrProtocol, err)
	}
	success = true
	return AssetTerminalEvidence{
		Ordinal:        asset.Ordinal,
		AssetID:        asset.AssetID,
		Role:           asset.Role,
		PlaintextBytes: offset,
		ChunkCount:     chunkIndex,
		AssetDigest:    asset.Binding.AssetDigest,
		StreamDigest:   streamDigest,
		EvidenceDigest: evidenceDigest,
	}, nil
}

func (server *Server) Evidence() (AggregateTerminalEvidence, bool) {
	if server == nil {
		return AggregateTerminalEvidence{}, false
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !server.ready {
		return AggregateTerminalEvidence{}, false
	}
	return cloneAggregateEvidence(server.evidence), true
}

func (server *Server) Cancel() error {
	if server == nil {
		return nil
	}
	server.closeOnce.Do(func() {
		server.mu.Lock()
		if server.serveStop != nil {
			server.serveStop()
		}
		if !server.started {
			clear(server.secret[:])
			server.closed = true
		}
		active := server.activeLease
		server.mu.Unlock()
		if server.conn != nil {
			_ = server.conn.SetDeadline(time.Now())
			server.closeErr = errors.Join(server.closeErr, server.conn.Close())
		}
		if active != nil {
			server.closeErr = errors.Join(server.closeErr, active.Cancel())
		}
	})
	return server.closeErr
}

func (server *Server) Close() error { return server.Cancel() }

func cloneAggregateEvidence(evidence AggregateTerminalEvidence) AggregateTerminalEvidence {
	cloned := evidence
	cloned.Assets = append([]AssetTerminalEvidence(nil), evidence.Assets...)
	return cloned
}
