//go:build !darwin && !linux

package container

import (
	"fmt"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// statfsUsage is unavailable on platforms without statfs(2). MacVz targets
// Apple Silicon macOS, so this stub only exists to keep cross-builds honest:
// it reports the limitation rather than fabricating disk numbers.
func statfsUsage(path string) (runtime.FilesystemUsage, error) {
	return runtime.FilesystemUsage{}, fmt.Errorf("%w: statfs not supported on this platform (path %q)", runtime.ErrDiskUsageUnavailable, path)
}
