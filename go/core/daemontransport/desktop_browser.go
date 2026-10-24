package daemontransport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
)

var ErrDesktopBrowserReuse = errors.New("existing Meadow Agent has no authenticated Desktop Browser capability; preserve it and start a new Desktop-owned agent only after its work is idle")

type desktopBrowserLaunch struct {
	connection    net.Conn
	child, secret *os.File
	cancel        context.CancelFunc
	ready         chan struct{}
	done          chan struct{}
	closeOnce     sync.Once
	filesOnce     sync.Once
}

func (bridge *desktopBrowserLaunch) authenticated() bool {
	if bridge == nil {
		return true
	}
	select {
	case <-bridge.done:
		return false
	default:
	}
	select {
	case <-bridge.ready:
		return true
	default:
		return false
	}
}

func (bridge *desktopBrowserLaunch) inheritedFiles() []*os.File {
	return []*os.File{bridge.child, bridge.secret}
}
func (bridge *desktopBrowserLaunch) closeInherited() {
	if bridge != nil {
		bridge.filesOnce.Do(func() { _ = bridge.child.Close(); _ = bridge.secret.Close() })
	}
}
func (bridge *desktopBrowserLaunch) close() {
	if bridge != nil {
		bridge.closeOnce.Do(func() { close(bridge.done); bridge.cancel(); _ = bridge.connection.Close(); bridge.closeInherited() })
	}
}

func rejectDesktopBrowserReuse(config LaunchConfig) error {
	if config.BrowserHandler != nil {
		return ErrDesktopBrowserReuse
	}
	return nil
}

// Keep the protected build's same-user and signed-peer policy on the new
// inherited channel as well as the existing Agent listener. The socketpair is
// created by Desktop before spawn; the child authenticates that parent peer.
func VerifyDesktopBrowserPeer(connection net.Conn) error {
	policy := peerPolicy{RequireSigned: protectedTransportBuild() || productsecurity.Required()}
	if err := verifyPeerSupport(policy); err != nil {
		return err
	}
	return verifyPeerIdentity(connection, policy)
}

// The handler is a parent-owned capability, never a path or endpoint supplied
// by a tool argument or environment variable.
func prepareDesktopBrowserLaunch(ctx context.Context, handler http.Handler) (*desktopBrowserLaunch, error) {
	return prepareDesktopBrowserLaunchPlatform(ctx, handler)
}
