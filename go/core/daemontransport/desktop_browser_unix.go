//go:build unix

package daemontransport

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/hey-meadow/meadow-code/go/core/desktopbrowserrpc"
	"golang.org/x/sys/unix"
)

func prepareDesktopBrowserLaunchPlatform(ctx context.Context, handler http.Handler) (*desktopBrowserLaunch, error) {
	if handler == nil {
		return nil, fmt.Errorf("Desktop Browser handler is required")
	}
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("create Desktop Browser socketpair: %w", err)
	}
	unix.CloseOnExec(descriptors[0])
	unix.CloseOnExec(descriptors[1])
	parent := os.NewFile(uintptr(descriptors[0]), "meadow-desktop-browser-parent")
	child := os.NewFile(uintptr(descriptors[1]), "meadow-desktop-browser-child")
	connection, err := net.FileConn(parent)
	_ = parent.Close()
	if err != nil {
		_ = child.Close()
		return nil, fmt.Errorf("adopt Desktop Browser socketpair: %w", err)
	}
	secretReader, secretWriter, err := os.Pipe()
	if err != nil {
		_ = child.Close()
		_ = connection.Close()
		return nil, err
	}
	secret := make([]byte, desktopbrowserrpc.PeerSecretBytes)
	if _, err = rand.Read(secret); err == nil {
		_, err = secretWriter.Write(secret)
	}
	_ = secretWriter.Close()
	if err != nil {
		clear(secret)
		_ = secretReader.Close()
		_ = child.Close()
		_ = connection.Close()
		return nil, err
	}
	serverCtx, cancel := context.WithCancel(ctx)
	bridge := &desktopBrowserLaunch{connection: connection, child: child, secret: secretReader, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer clear(secret)
		_ = desktopbrowserrpc.ServeWithReady(serverCtx, connection, secret, handler, func() { close(bridge.ready) })
		bridge.close()
	}()
	return bridge, nil
}
