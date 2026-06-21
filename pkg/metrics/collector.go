// Package metrics reports MacVz node and Pod resource usage to Kubernetes.
//
// It assembles the two metric surfaces the Virtual Kubelet kubelet server
// exposes (issue #25):
//
//   - the Summary API (GET /stats/summary), the kubelet stats schema consumed
//     by `kubectl top` and older metrics-server scrape modes; and
//   - the resource-metrics endpoint (GET /metrics/resource), the Prometheus
//     text format consumed by metrics-server's default scrape mode.
//
// Per-workload CPU and memory come from the runtime via the optional
// runtime.Stater capability; node memory comes from the host; node disk and
// image-cache accounting come from the optional runtime.DiskReporter capability.
// A Collector owns the small amount of state needed to turn cumulative CPU
// counters into the instantaneous nanocore rates the Summary schema expects.
// Everything degrades gracefully: a workload the runtime cannot sample is
// reported without CPU/memory rather than failing the whole scrape, and a host
// or runtime that cannot report node-level resources simply omits those fields.
package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// MemorySampler reports host RAM for node-level memory metrics. It is an
// interface so the darwin implementation can be swapped for a fake in tests and
// stubbed out on unsupported platforms.
type MemorySampler interface {
	// HostMemory returns total physical and in-use ("working set") bytes.
	HostMemory(ctx context.Context) (total, used uint64, err error)
}

// ContainerInput identifies one container for stats collection: its Kubernetes
// name, the runtime workload backing it, and when it started.
type ContainerInput struct {
	Name       string
	WorkloadID string
	StartTime  time.Time
}

// PodInput is the snapshot of a tracked Pod the collector needs to attribute
// per-container stats back to the right Pod in the Summary schema.
type PodInput struct {
	Namespace  string
	Name       string
	UID        string
	StartTime  time.Time
	Containers []ContainerInput
}

// StatsFunc fetches a resource sample for a workload. ok is false when the
// runtime cannot sample it (missing, not running, or no Stater capability), so
// the collector can skip that workload's CPU/memory without failing.
type StatsFunc func(ctx context.Context, workloadID string) (stats runtime.ResourceStats, ok bool)

// DiskSample is the node-level disk snapshot for one scrape: the filesystem
// backing micro-VM/image storage and the local image cache. Each surface
// carries its own ok flag so a node that can report one but not the other still
// contributes what it has.
type DiskSample struct {
	NodeFS   runtime.FilesystemUsage
	NodeFSOK bool
	Images   runtime.ImageCacheUsage
	ImagesOK bool
}

// DiskFunc fetches the node disk snapshot for a scrape. It is nil when the
// runtime exposes no DiskReporter capability, in which case the collector omits
// all filesystem and image-cache accounting.
type DiskFunc func(ctx context.Context) DiskSample

// nodeCPUKey is the reserved key under which the node-aggregate CPU sample is
// stored in the rate cache; no workload ID can collide with it.
const nodeCPUKey = "\x00node"

// Collector builds the kubelet Summary and Prometheus resource metrics for this
// node. It is safe for concurrent use.
type Collector struct {
	nodeName string
	mem      MemorySampler

	mu   sync.Mutex
	prev map[string]cpuSample // keyed by workload ID (+ nodeCPUKey)
}

// cpuSample records a cumulative CPU counter and when it was read, so the next
// sample can be differenced into a nanocore rate.
type cpuSample struct {
	at              time.Time
	coreNanoSeconds uint64
}

// NewCollector returns a Collector for nodeName using mem for host memory. A nil
// sampler disables node memory reporting (the node entry omits memory).
func NewCollector(nodeName string, mem MemorySampler) *Collector {
	return &Collector{
		nodeName: nodeName,
		mem:      mem,
		prev:     make(map[string]cpuSample),
	}
}

// snapshot is the raw, per-scrape data gathered once and shared by both the
// Summary and resource-metrics builders so a scrape samples each workload once.
type snapshot struct {
	at        time.Time
	memOK     bool
	memTotal  uint64
	memUsed   uint64
	disk      DiskSample
	pods      []podSnapshot
	nodeCPUNs uint64 // sum of container cumulative CPU, core-nanoseconds
	nodeMemWS uint64 // sum of container working-set bytes
}

type podSnapshot struct {
	ref        statsv1alpha1.PodReference
	start      time.Time
	containers []containerSnapshot
	cpuNs      uint64
	memWS      uint64
}

type containerSnapshot struct {
	name  string
	start time.Time
	stats runtime.ResourceStats
	ok    bool
}

// gather samples host memory, disk, and every workload once. diskFn is nil when
// the runtime cannot report disk, in which case no filesystem/image accounting
// is collected.
func (c *Collector) gather(ctx context.Context, at time.Time, pods []PodInput, statsFn StatsFunc, diskFn DiskFunc) snapshot {
	s := snapshot{at: at}

	if c.mem != nil {
		if total, used, err := c.mem.HostMemory(ctx); err == nil {
			s.memOK, s.memTotal, s.memUsed = true, total, used
		}
	}

	if diskFn != nil {
		s.disk = diskFn(ctx)
	}

	for _, p := range pods {
		ps := podSnapshot{
			ref:   statsv1alpha1.PodReference{Name: p.Name, Namespace: p.Namespace, UID: p.UID},
			start: p.StartTime,
		}
		for _, cn := range p.Containers {
			cs := containerSnapshot{name: cn.Name, start: cn.StartTime}
			if rs, ok := statsFn(ctx, cn.WorkloadID); ok {
				cs.stats, cs.ok = rs, true
				ps.cpuNs += rs.CPUUsageCoreNanoSeconds
				ps.memWS += rs.MemoryUsageBytes
			}
			ps.containers = append(ps.containers, cs)
		}
		s.nodeCPUNs += ps.cpuNs
		s.nodeMemWS += ps.memWS
		s.pods = append(s.pods, ps)
	}
	return s
}

// rate differences a cumulative CPU counter against the previous sample for key
// and returns the nanocore rate. It updates the cache. ok is false on the first
// sample for a key or when the counter went backwards (a workload restart or
// node pod-set change), in which case no rate is emitted for this scrape.
func (c *Collector) rate(key string, now time.Time, coreNanoSeconds uint64) (nanoCores uint64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, had := c.prev[key]
	c.prev[key] = cpuSample{at: now, coreNanoSeconds: coreNanoSeconds}
	if !had || coreNanoSeconds < prev.coreNanoSeconds {
		return 0, false
	}
	elapsed := now.Sub(prev.at).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	// coreNanoSeconds is CPU-core-nanoseconds; its per-second rate is nanocores.
	return uint64(float64(coreNanoSeconds-prev.coreNanoSeconds) / elapsed), true
}

// forget drops rate-cache entries for workloads no longer present so the cache
// does not grow without bound as Pods come and go.
func (c *Collector) forget(live map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.prev {
		if k == nodeCPUKey {
			continue
		}
		if _, ok := live[k]; !ok {
			delete(c.prev, k)
		}
	}
}

// Summary builds the kubelet stats Summary for this node from the given Pods.
// diskFn is nil when the runtime reports no disk usage.
func (c *Collector) Summary(ctx context.Context, pods []PodInput, statsFn StatsFunc, diskFn DiskFunc) *statsv1alpha1.Summary {
	now := time.Now()
	s := c.gather(ctx, now, pods, statsFn, diskFn)

	live := make(map[string]struct{}, len(pods))
	for _, p := range pods {
		for _, cn := range p.Containers {
			live[cn.WorkloadID] = struct{}{}
		}
	}
	c.forget(live)

	summary := &statsv1alpha1.Summary{
		Node: c.nodeStats(s),
		Pods: make([]statsv1alpha1.PodStats, 0, len(s.pods)),
	}
	for _, ps := range s.pods {
		summary.Pods = append(summary.Pods, c.podStats(ps, now))
	}
	return summary
}

func (c *Collector) nodeStats(s snapshot) statsv1alpha1.NodeStats {
	mt := metav1.NewTime(s.at)
	ns := statsv1alpha1.NodeStats{
		NodeName:  c.nodeName,
		StartTime: mt,
		CPU:       c.cpuStats(nodeCPUKey, s.at, s.nodeCPUNs),
	}
	if s.memOK {
		avail := uint64(0)
		if s.memTotal > s.memUsed {
			avail = s.memTotal - s.memUsed
		}
		ns.Memory = &statsv1alpha1.MemoryStats{
			Time:            mt,
			AvailableBytes:  u64(avail),
			UsageBytes:      u64(s.memUsed),
			WorkingSetBytes: u64(s.memUsed),
		}
	}

	// Node filesystem stats drive disk-pressure eviction. Image-cache accounting
	// is independent: if statfs fails but the runtime can list cached images,
	// still report runtime.imageFs.usedBytes rather than dropping the sample.
	if s.disk.NodeFSOK {
		ns.Fs = fsStats(s.disk.NodeFS, s.at)
	}
	if s.disk.ImagesOK {
		ns.Runtime = &statsv1alpha1.RuntimeStats{ImageFs: imageFsStats(s.disk, s.at)}
	}
	return ns
}

// fsStats maps a runtime filesystem sample to the kubelet FsStats schema, using
// fallback for the timestamp when the sample did not carry one.
func fsStats(fu runtime.FilesystemUsage, fallback time.Time) *statsv1alpha1.FsStats {
	at := fu.Timestamp
	if at.IsZero() {
		at = fallback
	}
	out := &statsv1alpha1.FsStats{
		Time:           metav1.NewTime(at),
		CapacityBytes:  u64(fu.TotalBytes),
		UsedBytes:      u64(fu.UsedBytes),
		AvailableBytes: u64(fu.AvailableBytes),
	}
	if fu.TotalInodes > 0 {
		free := uint64(0)
		if fu.TotalInodes > fu.UsedInodes {
			free = fu.TotalInodes - fu.UsedInodes
		}
		out.Inodes = u64(fu.TotalInodes)
		out.InodesUsed = u64(fu.UsedInodes)
		out.InodesFree = u64(free)
	}
	return out
}

func imageFsStats(d DiskSample, fallback time.Time) *statsv1alpha1.FsStats {
	at := d.Images.Timestamp
	if at.IsZero() {
		at = fallback
	}
	out := &statsv1alpha1.FsStats{
		Time:      metav1.NewTime(at),
		UsedBytes: u64(d.Images.TotalBytes),
	}
	if d.NodeFSOK {
		base := fsStats(d.NodeFS, fallback)
		base.Time = metav1.NewTime(at)
		base.UsedBytes = out.UsedBytes
		return base
	}
	return out
}

func (c *Collector) podStats(ps podSnapshot, now time.Time) statsv1alpha1.PodStats {
	out := statsv1alpha1.PodStats{
		PodRef:    ps.ref,
		StartTime: metav1.NewTime(ps.start),
		CPU:       c.cpuStats(cpuKey(ps.ref), now, ps.cpuNs),
		Memory:    workingSetMemory(now, ps.memWS),
	}
	for _, cs := range ps.containers {
		out.Containers = append(out.Containers, c.containerStats(ps.ref, cs, now))
	}
	return out
}

func (c *Collector) containerStats(ref statsv1alpha1.PodReference, cs containerSnapshot, now time.Time) statsv1alpha1.ContainerStats {
	out := statsv1alpha1.ContainerStats{
		Name:      cs.name,
		StartTime: metav1.NewTime(cs.start),
	}
	if !cs.ok {
		return out // runtime could not sample this container; report it bare.
	}
	out.CPU = c.cpuStats(cpuKey(ref)+"/"+cs.name, cs.stats.Timestamp, cs.stats.CPUUsageCoreNanoSeconds)
	out.Memory = memoryStats(cs.stats)
	return out
}

// cpuStats builds a CPUStats with the cumulative counter and, when a prior
// sample exists, the derived nanocore rate.
func (c *Collector) cpuStats(key string, at time.Time, coreNanoSeconds uint64) *statsv1alpha1.CPUStats {
	out := &statsv1alpha1.CPUStats{
		Time:                 metav1.NewTime(at),
		UsageCoreNanoSeconds: u64(coreNanoSeconds),
	}
	if nc, ok := c.rate(key, at, coreNanoSeconds); ok {
		out.UsageNanoCores = u64(nc)
	}
	return out
}

func memoryStats(rs runtime.ResourceStats) *statsv1alpha1.MemoryStats {
	out := &statsv1alpha1.MemoryStats{
		Time:            metav1.NewTime(rs.Timestamp),
		UsageBytes:      u64(rs.MemoryUsageBytes),
		WorkingSetBytes: u64(rs.MemoryUsageBytes),
	}
	if rs.MemoryLimitBytes > 0 {
		avail := uint64(0)
		if rs.MemoryLimitBytes > rs.MemoryUsageBytes {
			avail = rs.MemoryLimitBytes - rs.MemoryUsageBytes
		}
		out.AvailableBytes = u64(avail)
	}
	return out
}

func workingSetMemory(at time.Time, bytes uint64) *statsv1alpha1.MemoryStats {
	return &statsv1alpha1.MemoryStats{
		Time:            metav1.NewTime(at),
		UsageBytes:      u64(bytes),
		WorkingSetBytes: u64(bytes),
	}
}

// cpuKey is the rate-cache key for a Pod's aggregate CPU.
func cpuKey(ref statsv1alpha1.PodReference) string {
	return ref.Namespace + "/" + ref.Name
}

func u64(v uint64) *uint64 { return &v }
