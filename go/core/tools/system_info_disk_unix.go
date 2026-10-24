//go:build linux || darwin

package tools

import (
	"math"

	"golang.org/x/sys/unix"
)

func systemInfoDisk(path string) map[string]any {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return map[string]any{"status": "unavailable", "error": "workspace filesystem statistics unavailable"}
	}
	blockSize := uint64(stat.Bsize)
	if stat.Bsize <= 0 || uint64(stat.Blocks) > math.MaxUint64/blockSize || uint64(stat.Bavail) > math.MaxUint64/blockSize || uint64(stat.Bfree) > math.MaxUint64/blockSize {
		return map[string]any{"status": "unavailable", "error": "invalid filesystem counters"}
	}
	return map[string]any{
		"status": "observed", "total_bytes": uint64(stat.Blocks) * blockSize,
		"available_unprivileged_bytes": uint64(stat.Bavail) * blockSize,
		"free_bytes":                   uint64(stat.Bfree) * blockSize,
	}
}
