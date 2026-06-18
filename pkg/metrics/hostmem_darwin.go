//go:build darwin

package metrics

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// darwinMemorySampler reports host RAM via macOS system tools, mirroring the
// density benchmark's accounting: total comes from `sysctl hw.memsize`, and the
// working set is the active, wired, and compressed pages from `vm_stat` — the
// macOS notion of memory that is not reclaimable on demand.
type darwinMemorySampler struct{}

// DefaultMemorySampler returns the host memory sampler for the current OS.
func DefaultMemorySampler() MemorySampler { return darwinMemorySampler{} }

func (darwinMemorySampler) HostMemory(ctx context.Context) (total, used uint64, err error) {
	total, err = hostTotalRAM(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("read total memory: %w", err)
	}
	used, err = hostUsedRAM(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("read used memory: %w", err)
	}
	if used > total {
		used = total
	}
	return total, used, nil
}

func hostTotalRAM(ctx context.Context) (uint64, error) {
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

func hostUsedRAM(ctx context.Context) (uint64, error) {
	out, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		return 0, err
	}
	pageSize := uint64(16384)
	var active, wired, compressed uint64
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := line[i+len("page size of "):]
				if n, err := strconv.ParseUint(strings.Fields(rest)[0], 10, 64); err == nil {
					pageSize = n
				}
			}
			continue
		}
		key, val, ok := splitVMStat(line)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(key, "Pages active"):
			active = val
		case strings.HasPrefix(key, "Pages wired down"):
			wired = val
		case strings.HasPrefix(key, "Pages occupied by compressor"):
			compressed = val
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return (active + wired + compressed) * pageSize, nil
}

// splitVMStat parses a "Key: 12345." vm_stat line into its key and page count.
func splitVMStat(line string) (key string, val uint64, ok bool) {
	i := strings.LastIndex(line, ":")
	if i < 0 {
		return "", 0, false
	}
	key = strings.TrimSpace(line[:i])
	num := strings.TrimSuffix(strings.TrimSpace(line[i+1:]), ".")
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return key, n, true
}
