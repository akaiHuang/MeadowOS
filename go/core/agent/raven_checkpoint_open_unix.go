//go:build unix

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// Refuse a replaced leaf symlink and avoid blocking if a regular path is
// concurrently replaced by a FIFO. Stat still verifies the opened inode.
func ravenCheckpointOpenFile(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
