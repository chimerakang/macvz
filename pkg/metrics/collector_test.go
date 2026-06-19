package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	dto "github.com/prometheus/client_model/go"
)

// timeAt returns a fixed instant offset by sec seconds, for deterministic
// rate-window math.
func timeAt(sec int) time.Time { return time.Unix(1700000000+int64(sec), 0) }

// fakeMem is a fixed host-memory sampler. When err is set, HostMemory fails so
// tests can exercise graceful node-memory degradation.
type fakeMem struct {
	total, used uint64
	err         error
}

func (m fakeMem) HostMemory(context.Context) (uint64, uint64, error) {
	return m.total, m.used, m.err
}

// statsMap is a StatsFunc backed by a map keyed by workload ID. IDs absent from
// the map report as unavailable, mimicking a runtime that cannot sample them.
func statsMap(m map[string]runtime.ResourceStats) StatsFunc {
	return func(_ context.Context, id string) (runtime.ResourceStats, bool) {
		rs, ok := m[id]
		return rs, ok
	}
}

func onePod() []PodInput {
	return []PodInput{{
		Namespace: "default", Name: "web", UID: "uid-1",
		Containers: []ContainerInput{{Name: "app", WorkloadID: "default/web/app"}},
	}}
}

func TestSummaryReportsNodeAndPodStats(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 16 << 30, used: 4 << 30})
	stats := statsMap(map[string]runtime.ResourceStats{
		"default/web/app": {CPUUsageCoreNanoSeconds: 5_000_000_000, MemoryUsageBytes: 100 << 20, MemoryLimitBytes: 512 << 20},
	})

	s := c.Summary(context.Background(), onePod(), stats)

	if s.Node.NodeName != "mac-1" {
		t.Errorf("node name = %q, want mac-1", s.Node.NodeName)
	}
	if s.Node.Memory == nil || *s.Node.Memory.WorkingSetBytes != 4<<30 {
		t.Fatalf("node working set = %v, want %d", s.Node.Memory, uint64(4<<30))
	}
	if got := *s.Node.Memory.AvailableBytes; got != 12<<30 {
		t.Errorf("node available = %d, want %d", got, uint64(12<<30))
	}
	if s.Node.CPU == nil || *s.Node.CPU.UsageCoreNanoSeconds != 5_000_000_000 {
		t.Fatalf("node CPU core-ns = %v, want 5e9", s.Node.CPU)
	}
	if len(s.Pods) != 1 {
		t.Fatalf("pods = %d, want 1", len(s.Pods))
	}
	p := s.Pods[0]
	if p.PodRef.Name != "web" || p.PodRef.Namespace != "default" || p.PodRef.UID != "uid-1" {
		t.Errorf("pod ref = %+v", p.PodRef)
	}
	if p.CPU == nil || *p.CPU.UsageCoreNanoSeconds != 5_000_000_000 {
		t.Errorf("pod CPU = %v", p.CPU)
	}
	if p.Memory == nil || *p.Memory.WorkingSetBytes != 100<<20 {
		t.Errorf("pod memory = %v", p.Memory)
	}
	if len(p.Containers) != 1 || p.Containers[0].Name != "app" {
		t.Fatalf("containers = %+v", p.Containers)
	}
	cs := p.Containers[0]
	if cs.Memory == nil || *cs.Memory.AvailableBytes != (512-100)<<20 {
		t.Errorf("container available = %v, want %d", cs.Memory, uint64((512-100)<<20))
	}
	// First sample: no prior point, so no nanocore rate yet.
	if cs.CPU.UsageNanoCores != nil {
		t.Errorf("expected no nanocore rate on first sample, got %d", *cs.CPU.UsageNanoCores)
	}
}

func TestSummaryComputesNanocoreRateAcrossSamples(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 1 << 30})
	pods := onePod()

	// Two samples 1s apart with +2 core-seconds of CPU should read ~2 cores.
	c.prev["default/web/app"] = cpuSample{at: timeAt(0), coreNanoSeconds: 1_000_000_000}
	stats := statsMap(map[string]runtime.ResourceStats{
		"default/web/app": {Timestamp: timeAt(1), CPUUsageCoreNanoSeconds: 3_000_000_000},
	})

	s := c.Summary(context.Background(), pods, stats)
	cs := s.Pods[0].Containers[0]
	if cs.CPU == nil || cs.CPU.UsageNanoCores == nil {
		t.Fatalf("expected a nanocore rate, got %v", cs.CPU)
	}
	if got := *cs.CPU.UsageNanoCores; got != 2_000_000_000 {
		t.Errorf("nanocores = %d, want 2e9 (2 cores)", got)
	}
}

func TestSummaryDegradesWhenRuntimeCannotSample(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 1 << 30})
	// statsMap with no entries -> every workload is unavailable.
	s := c.Summary(context.Background(), onePod(), statsMap(nil))

	p := s.Pods[0]
	if *p.CPU.UsageCoreNanoSeconds != 0 {
		t.Errorf("unsampled pod CPU = %d, want 0", *p.CPU.UsageCoreNanoSeconds)
	}
	if p.Containers[0].CPU != nil || p.Containers[0].Memory != nil {
		t.Error("unsampled container should report no CPU/memory")
	}
	// Node memory still comes from the host sampler.
	if s.Node.Memory == nil {
		t.Error("node memory should still be reported when workloads are unsampled")
	}
}

func TestSummaryOmitsNodeMemoryOnSamplerError(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{err: context.DeadlineExceeded})
	s := c.Summary(context.Background(), onePod(), statsMap(nil))
	if s.Node.Memory != nil {
		t.Errorf("node memory should be omitted when sampler fails, got %v", s.Node.Memory)
	}
}

func TestResourceMetricsEmitsStandardFamilies(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 2 << 30})
	stats := statsMap(map[string]runtime.ResourceStats{
		"default/web/app": {CPUUsageCoreNanoSeconds: 4_000_000_000, MemoryUsageBytes: 50 << 20},
	})

	families := c.ResourceMetrics(context.Background(), onePod(), stats, true)
	byName := map[string]*dto.MetricFamily{}
	for _, f := range families {
		byName[f.GetName()] = f
	}

	for _, want := range []string{
		"node_cpu_usage_seconds_total", "node_memory_working_set_bytes",
		"pod_cpu_usage_seconds_total", "pod_memory_working_set_bytes",
		"container_cpu_usage_seconds_total", "container_memory_working_set_bytes",
		"macvz_node_pods", "macvz_runtime_ready",
	} {
		if byName[want] == nil {
			t.Errorf("missing metric family %q", want)
		}
	}

	if got := byName["container_cpu_usage_seconds_total"].Metric[0].Counter.GetValue(); got != 4.0 {
		t.Errorf("container cpu seconds = %v, want 4.0", got)
	}
	if got := byName["macvz_node_pods"].Metric[0].Gauge.GetValue(); got != 1 {
		t.Errorf("node pods = %v, want 1", got)
	}
	if got := byName["macvz_runtime_ready"].Metric[0].Gauge.GetValue(); got != 1 {
		t.Errorf("runtime ready = %v, want 1", got)
	}
	// Container series carry pod/namespace/container labels for attribution.
	labels := map[string]string{}
	for _, l := range byName["container_cpu_usage_seconds_total"].Metric[0].Label {
		labels[l.GetName()] = l.GetValue()
	}
	if labels["pod"] != "web" || labels["namespace"] != "default" || labels["container"] != "app" {
		t.Errorf("container labels = %v", labels)
	}
}

func TestForgetEvictsStaleRateState(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 1 << 30})
	c.prev["gone/old/c"] = cpuSample{coreNanoSeconds: 1}
	c.prev[nodeCPUKey] = cpuSample{coreNanoSeconds: 2}

	c.Summary(context.Background(), onePod(), statsMap(nil))

	if _, ok := c.prev["gone/old/c"]; ok {
		t.Error("stale workload rate state should be evicted")
	}
	if _, ok := c.prev[nodeCPUKey]; !ok {
		t.Error("node rate state must be preserved across scrapes")
	}
}
