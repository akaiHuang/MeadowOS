//go:build darwin || linux

package browserruntime

import (
	"fmt"
	"os"
	"syscall"
)

func fileNode(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("selected executable vnode is unavailable")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}
