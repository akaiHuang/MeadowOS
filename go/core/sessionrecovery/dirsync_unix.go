//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package sessionrecovery

import (
	"errors"
	"os"
)

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
