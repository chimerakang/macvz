//go:build !darwin

package metrics

import (
	"context"
	"fmt"
	"runtime"
)

// unsupportedMemorySampler stands in on non-macOS builds (e.g. linux CI or
// cross-compilation). MacVz runs micro-VMs only on Apple Silicon, so host
// memory has no meaningful source here; HostMemory reports the limitation
// rather than fabricating a value, and the collector degrades gracefully.
type unsupportedMemorySampler struct{}

// DefaultMemorySampler returns the host memory sampler for the current OS.
func DefaultMemorySampler() MemorySampler { return unsupportedMemorySampler{} }

func (unsupportedMemorySampler) HostMemory(context.Context) (total, used uint64, err error) {
	return 0, 0, fmt.Errorf("host memory sampling unsupported on %s", runtime.GOOS)
}
