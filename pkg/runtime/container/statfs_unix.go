//go:build darwin || linux

package container

import (
	"syscall"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// statfsUsage samples the filesystem containing path via statfs(2). It is the
// production NodeFilesystem source on macOS (and compiles on linux for CI/cross
// builds). The block-count fields are cast through uint64 so the same code
// compiles across the darwin/linux differences in Statfs_t field widths.
func statfsUsage(path string) (runtime.FilesystemUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return runtime.FilesystemUsage{}, err
	}
	bsize := uint64(st.Bsize)
	total := uint64(st.Blocks) * bsize
	// Bfree counts all free blocks; Bavail counts those available to a
	// non-privileged caller (Bfree minus the filesystem reserve).
	used := (uint64(st.Blocks) - uint64(st.Bfree)) * bsize
	avail := uint64(st.Bavail) * bsize

	inodes := uint64(st.Files)
	var usedInodes uint64
	if inodes >= uint64(st.Ffree) {
		usedInodes = inodes - uint64(st.Ffree)
	}

	return runtime.FilesystemUsage{
		Path:           path,
		Timestamp:      time.Now(),
		TotalBytes:     total,
		UsedBytes:      used,
		AvailableBytes: avail,
		TotalInodes:    inodes,
		UsedInodes:     usedInodes,
	}, nil
}
