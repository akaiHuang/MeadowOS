//go:build !windows

package searchconnection

import (
	"errors"
	"os"
	"syscall"
)

func validateOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("Search connection files must belong to the current user")
	}
	return nil
}
