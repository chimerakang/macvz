package runtime

import (
	"context"
	"errors"
	"time"
)

// ErrDiskUsageUnavailable means the runtime cannot report disk or image-cache
// usage right now (e.g. the storage path is unreadable, or the host runtime
// exposes no size data). Like ErrStatsUnavailable, the metrics layer treats it
// as "skip this surface" so an unobservable disk never fails a whole scrape.
var ErrDiskUsageUnavailable = errors.New("runtime: disk usage unavailable")

// FilesystemUsage is a point-in-time sample of the filesystem backing MacVz's
// micro-VM and image storage. It feeds node-level disk accounting: the Summary
// API's filesystem stats (used by disk-pressure eviction) and the node's
// ephemeral-storage capacity.
type FilesystemUsage struct {
	// Path is the filesystem location sampled (a mountpoint or a path on it).
	Path string
	// Timestamp is when the sample was taken.
	Timestamp time.Time
	// TotalBytes is the filesystem's total size.
	TotalBytes uint64
	// UsedBytes is bytes consumed (total minus all free blocks).
	UsedBytes uint64
	// AvailableBytes is bytes available to MacVz (free blocks minus any reserve);
	// it can be smaller than TotalBytes-UsedBytes on filesystems with a reserve.
	AvailableBytes uint64
	// TotalInodes and UsedInodes describe inode pressure; 0 when the filesystem
	// does not report inodes.
	TotalInodes uint64
	UsedInodes  uint64
}

// ImageCacheUsage reports the size and population of the local OCI image store.
// It is node-level accounting for how much disk pulled images consume, so an
// operator (or a management UI) can see image-cache growth over a node's life.
//
// TotalBytes is the sum of the runtime's per-image reported sizes; when images
// share layers this is an upper bound on the bytes actually on disk. An empty
// store is a valid sample (Count 0, TotalBytes 0), not an error.
type ImageCacheUsage struct {
	// Timestamp is when the sample was taken.
	Timestamp time.Time
	// TotalBytes is the summed size of locally cached images.
	TotalBytes uint64
	// Count is the number of cached images.
	Count int
}

// DiskReporter is an optional Runtime capability that reports node disk and
// image-cache accounting. A Runtime that cannot sample disk simply does not
// implement it; the metrics layer then degrades gracefully and reports only the
// resources it can observe (e.g. CPU and memory from Stater).
type DiskReporter interface {
	// NodeFilesystem reports usage of the filesystem backing micro-VM and image
	// storage, or ErrDiskUsageUnavailable when it cannot be sampled (wrapped,
	// comparable with errors.Is).
	NodeFilesystem(ctx context.Context) (FilesystemUsage, error)
	// ImageCacheUsage reports the size and count of locally cached images, or
	// ErrDiskUsageUnavailable when the image store cannot be inspected.
	ImageCacheUsage(ctx context.Context) (ImageCacheUsage, error)
}
