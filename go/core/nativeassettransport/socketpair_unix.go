//go:build darwin || linux

package nativeassettransport

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func newSocketPair() (*net.UnixConn, *os.File, error) {
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, errors.Join(ErrUnavailable, err)
	}
	unix.CloseOnExec(descriptors[0])
	unix.CloseOnExec(descriptors[1])
	parentFile := os.NewFile(uintptr(descriptors[0]), "meadow-native-asset-parent")
	childFile := os.NewFile(uintptr(descriptors[1]), "meadow-native-asset-child")
	if parentFile == nil || childFile == nil {
		if parentFile != nil {
			_ = parentFile.Close()
		} else {
			_ = unix.Close(descriptors[0])
		}
		if childFile != nil {
			_ = childFile.Close()
		} else {
			_ = unix.Close(descriptors[1])
		}
		return nil, nil, ErrUnavailable
	}
	connection, err := net.FileConn(parentFile)
	_ = parentFile.Close()
	if err != nil {
		_ = childFile.Close()
		return nil, nil, errors.Join(ErrUnavailable, err)
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		_ = childFile.Close()
		return nil, nil, ErrUnavailable
	}
	return unixConnection, childFile, nil
}
