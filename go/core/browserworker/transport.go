package browserworker

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
)

// ReadPeerSecretFD reads a one-shot binary secret from a dedicated inherited
// descriptor. The parent must close its writer after sending 32 to 64 random
// bytes. Secrets are never accepted through argv, environment, or the network.
func ReadPeerSecretFD(fd uintptr) ([]byte, error) {
	file := os.NewFile(fd, "meadow-browser-peer-secret")
	if file == nil {
		return nil, errors.New("invalid browser worker secret descriptor")
	}
	defer file.Close()
	secret, err := io.ReadAll(io.LimitReader(file, MaxPeerSecretBytes+1))
	if err != nil {
		return nil, errors.New("read browser worker peer secret")
	}
	if len(secret) < MinPeerSecretBytes || len(secret) > MaxPeerSecretBytes {
		clear(secret)
		return nil, errors.New("invalid browser worker peer secret length")
	}
	return secret, nil
}

// ServeInheritedFD accepts only an anonymous inherited Unix-domain stream such
// as one end of socketpair(2). net.FileConn duplicates the descriptor, so the
// inherited file is closed before the serving loop begins.
func ServeInheritedFD(ctx context.Context, fd uintptr, config Config) error {
	file := os.NewFile(fd, "meadow-browser-worker")
	if file == nil {
		return errors.New("invalid browser worker descriptor")
	}
	connection, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return errors.New("browser worker descriptor is not a connection")
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return errors.New("browser worker requires an inherited Unix-domain stream")
	}
	defer unixConnection.Close()
	return Serve(ctx, unixConnection, config)
}

