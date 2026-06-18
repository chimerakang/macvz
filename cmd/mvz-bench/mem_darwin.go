// Host memory and per-VM process sampling for the density benchmark. macOS
// only: it parses `vm_stat` for system memory pressure and `ps` for the RSS of
// the per-VM `container-runtime-linux` helper processes, one of which backs
// each running micro-VM.
package main

import (
	"bufio"
	"bytes"
	"os/exec"
	"strconv"
	"strings"
)

// vmmProcessComm is the apple/container helper process that backs one micro-VM.
// Its resident set size approximates the host RAM overhead of that VM.
const vmmProcessComm = "container-runtime-linux"

// hostTotalRAM returns total physical memory in bytes via sysctl.
func hostTotalRAM() (int64, error) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// hostUsedRAM returns an estimate of used physical memory in bytes: the active,
// wired, and compressed pages reported by vm_stat. This is the macOS notion of
// memory that is not reclaimable on demand.
func hostUsedRAM() (int64, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, err
	}
	pageSize := int64(16384)
	var active, wired, compressed int64
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := line[i+len("page size of "):]
				if n, err := strconv.ParseInt(strings.Fields(rest)[0], 10, 64); err == nil {
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
	return (active + wired + compressed) * pageSize, sc.Err()
}

// splitVMStat parses a "Key: 12345." vm_stat line into its key and page count.
func splitVMStat(line string) (key string, val int64, ok bool) {
	i := strings.LastIndex(line, ":")
	if i < 0 {
		return "", 0, false
	}
	key = strings.TrimSpace(line[:i])
	num := strings.TrimSpace(line[i+1:])
	num = strings.TrimSuffix(num, ".")
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return key, n, true
}

// vmmResidentSizes returns the RSS (bytes) of every per-VM helper process,
// one entry per running micro-VM.
func vmmResidentSizes() ([]int64, error) {
	out, err := exec.Command("ps", "-axo", "rss,comm").Output()
	if err != nil {
		return nil, err
	}
	var sizes []int64
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		if !strings.Contains(fields[len(fields)-1], vmmProcessComm) {
			continue
		}
		if kb, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			sizes = append(sizes, kb*1024)
		}
	}
	return sizes, sc.Err()
}
