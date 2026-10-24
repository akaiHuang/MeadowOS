//go:build unix

package desktopbrowserrpc

import (
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func ReadInherited(fd, secretFD uintptr) (net.Conn, []byte, error) {
	if ValidateDescriptors(int(fd), int(secretFD)) != nil || fd == 0 {
		return nil, nil, ErrInvalid
	}
	unix.CloseOnExec(int(fd))
	unix.CloseOnExec(int(secretFD))
	// Adopt the pipe in nonblocking mode so a malformed inherited writer
	// cannot leave startup waiting indefinitely for the secret or its EOF.
	if err := unix.SetNonblock(int(secretFD), true); err != nil {
		_ = unix.Close(int(fd))
		_ = unix.Close(int(secretFD))
		return nil, nil, ErrInvalid
	}
	file := os.NewFile(fd, "meadow-desktop-browser")
	secretFile := os.NewFile(secretFD, "meadow-desktop-browser-secret")
	if file == nil || secretFile == nil {
		if file != nil {
			_ = file.Close()
		}
		if secretFile != nil {
			_ = secretFile.Close()
		}
		return nil, nil, ErrInvalid
	}
	defer file.Close()
	defer secretFile.Close()
	kind, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || kind != unix.SOCK_STREAM {
		return nil, nil, ErrInvalid
	}
	peer, err := unix.Getpeername(int(fd))
	if err != nil {
		return nil, nil, ErrInvalid
	}
	if _, ok := peer.(*unix.SockaddrUnix); !ok {
		return nil, nil, ErrInvalid
	}
	var secretInfo unix.Stat_t
	if unix.Fstat(int(secretFD), &secretInfo) != nil || secretInfo.Mode&unix.S_IFMT != unix.S_IFIFO {
		return nil, nil, ErrInvalid
	}
	if err := secretFile.SetReadDeadline(time.Now().Add(ioTimeout)); err != nil {
		return nil, nil, unavailable(err)
	}
	secret, err := io.ReadAll(io.LimitReader(secretFile, PeerSecretBytes+1))
	if err != nil || len(secret) != PeerSecretBytes {
		clear(secret)
		return nil, nil, ErrAuthentication
	}
	connection, err := net.FileConn(file)
	if err != nil {
		clear(secret)
		return nil, nil, unavailable(err)
	}
	if _, ok := connection.(*net.UnixConn); !ok {
		_ = connection.Close()
		clear(secret)
		return nil, nil, ErrInvalid
	}
	return connection, secret, nil
}
