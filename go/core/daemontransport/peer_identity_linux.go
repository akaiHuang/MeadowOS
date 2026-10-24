//go:build linux

package daemontransport

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func verifyPeerSupport(policy peerPolicy) error {
	if policy.RequireSigned || strings.TrimSpace(policy.ExpectedTeamID) != "" || fixedProductTransport() {
		return fmt.Errorf("signed public Meadow Agent peer identity is not implemented for Linux")
	}
	return nil
}

func verifyPeerIdentity(connection net.Conn, policy peerPolicy) error {
	if err := verifyPeerSupport(policy); err != nil {
		return err
	}
	_, err := peerPIDFromConn(connection)
	return err
}

func validateLinuxPeerCredentials(credentials *unix.Ucred, expectedUID uint32) (int, error) {
	if credentials == nil || credentials.Uid != expectedUID || credentials.Pid <= 1 {
		return 0, fmt.Errorf("unsafe Meadow Agent Linux peer identity")
	}
	return int(credentials.Pid), nil
}

// Read the kernel credentials of this exact connected socket. Never trust a
// reported JSON PID or a fresh connection when authenticating an HTTP reply.
func peerPIDFromConn(connection net.Conn) (int, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("Meadow Agent socket is not Unix-domain")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("inspect Agent peer socket: %w", err)
	}
	var credentials *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("access Agent peer socket: %w", err)
	}
	if credentialErr != nil {
		return 0, fmt.Errorf("resolve Agent peer identity: %w", credentialErr)
	}
	return validateLinuxPeerCredentials(credentials, uint32(os.Geteuid()))
}

func daemonPeerPID() (int, error) {
	path, err := DefaultSocketPath()
	if err != nil {
		return 0, err
	}
	connection, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return 0, err
	}
	defer connection.Close()
	return peerPIDFromConn(connection)
}
