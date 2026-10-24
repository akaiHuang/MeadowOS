package nativeassettransport

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/protectedmodelbridge"
)

type Parent struct {
	server      *Server
	ctx         context.Context
	cancel      context.CancelFunc
	childSocket *os.File
	childSecret *os.File
	done        chan error

	mu       sync.Mutex
	started  bool
	closed   bool
	finish   sync.Once
	closeErr error
}

func NewParent(
	ctx context.Context,
	resolver AssetResolver,
	manifest VerifiedManifest,
	options Options,
) (*Parent, error) {
	if ctx == nil || resolver == nil || manifest.requiredCount == 0 {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(ErrCanceled, err)
	}
	parentConn, childSocket, err := newSocketPair()
	if err != nil {
		return nil, err
	}
	secretReader, secretWriter, err := os.Pipe()
	if err != nil {
		_ = parentConn.Close()
		_ = childSocket.Close()
		return nil, errors.Join(ErrUnavailable, err)
	}
	var secret [PeerSecretBytes]byte
	if _, err := rand.Read(secret[:]); err != nil {
		clear(secret[:])
		_ = parentConn.Close()
		_ = childSocket.Close()
		_ = secretReader.Close()
		_ = secretWriter.Close()
		return nil, ErrUnavailable
	}
	server, err := NewServer(parentConn, secret[:], resolver, manifest, options)
	if err != nil {
		clear(secret[:])
		_ = childSocket.Close()
		_ = secretReader.Close()
		_ = secretWriter.Close()
		return nil, err
	}
	if err := writeAll(secretWriter, secret[:]); err != nil {
		clear(secret[:])
		_ = secretWriter.Close()
		_ = secretReader.Close()
		_ = childSocket.Close()
		_ = server.Cancel()
		return nil, errors.Join(ErrUnavailable, err)
	}
	clear(secret[:])
	if err := secretWriter.Close(); err != nil {
		_ = secretReader.Close()
		_ = childSocket.Close()
		_ = server.Cancel()
		return nil, errors.Join(ErrUnavailable, err)
	}
	parentContext, cancel := context.WithCancel(ctx)
	return &Parent{
		server:      server,
		ctx:         parentContext,
		cancel:      cancel,
		childSocket: childSocket,
		childSecret: secretReader,
		done:        make(chan error, 1),
	}, nil
}

func (parent *Parent) InheritedFiles() (*os.File, *os.File, error) {
	if parent == nil {
		return nil, nil, ErrInvalidArgument
	}
	parent.mu.Lock()
	defer parent.mu.Unlock()
	if parent.started || parent.closed || parent.childSocket == nil || parent.childSecret == nil {
		return nil, nil, ErrProtocol
	}
	return parent.childSocket, parent.childSecret, nil
}

func (parent *Parent) Start() error {
	if parent == nil {
		return ErrInvalidArgument
	}
	parent.mu.Lock()
	if parent.started || parent.closed || parent.server == nil {
		parent.mu.Unlock()
		return ErrProtocol
	}
	parent.started = true
	parent.closeChildFilesLocked()
	parent.mu.Unlock()
	go func() {
		parent.complete(parent.server.Serve(parent.ctx))
	}()
	return nil
}

func (parent *Parent) Wait(ctx context.Context) error {
	if parent == nil || ctx == nil {
		return ErrInvalidArgument
	}
	parent.mu.Lock()
	started := parent.started
	parent.mu.Unlock()
	if !started {
		return ErrProtocol
	}
	select {
	case err := <-parent.done:
		parent.done <- err
		return err
	case <-ctx.Done():
		return errors.Join(ErrCanceled, ctx.Err())
	}
}

func (parent *Parent) Evidence() (AggregateTerminalEvidence, bool) {
	if parent == nil || parent.server == nil {
		return AggregateTerminalEvidence{}, false
	}
	return parent.server.Evidence()
}

func (parent *Parent) Cancel() error {
	if parent == nil {
		return nil
	}
	parent.mu.Lock()
	if parent.closed {
		err := parent.closeErr
		parent.mu.Unlock()
		return err
	}
	parent.closed = true
	wasStarted := parent.started
	parent.started = true
	parent.closeChildFilesLocked()
	parent.mu.Unlock()
	if parent.cancel != nil {
		parent.cancel()
	}
	err := parent.server.Cancel()
	if !wasStarted {
		parent.complete(errors.Join(ErrCanceled, err))
	}
	parent.mu.Lock()
	parent.closeErr = errors.Join(parent.closeErr, err)
	result := parent.closeErr
	parent.mu.Unlock()
	return result
}

func (parent *Parent) Close() error {
	if parent == nil {
		return nil
	}
	cancelErr := parent.Cancel()
	waitErr := parent.Wait(context.Background())
	if errors.Is(waitErr, ErrCanceled) {
		waitErr = nil
	}
	return errors.Join(cancelErr, waitErr)
}

func (parent *Parent) complete(err error) {
	parent.finish.Do(func() { parent.done <- err })
}

func (parent *Parent) closeChildFilesLocked() {
	if parent.childSocket != nil {
		_ = parent.childSocket.Close()
		parent.childSocket = nil
	}
	if parent.childSecret != nil {
		_ = parent.childSecret.Close()
		parent.childSecret = nil
	}
}

var _ io.Closer = (*Parent)(nil)
var _ protectedmodelbridge.SequentialLease = (protectedmodelbridge.SequentialLease)(nil)
