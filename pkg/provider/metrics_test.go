package provider

import (
	"context"
	"testing"

	"github.com/chimerakang/macvz/pkg/metrics"
	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// statingRuntime augments the recording runtime with the optional Stater
// capability so the provider's metrics path can be exercised end to end.
type statingRuntime struct {
	*recordingRuntime
	stats map[string]runtime.ResourceStats
}

func (s *statingRuntime) Stats(_ context.Context, id string) (runtime.ResourceStats, error) {
	rs, ok := s.stats[id]
	if !ok {
		return runtime.ResourceStats{}, runtime.ErrStatsUnavailable
	}
	return rs, nil
}

// fixedMem is a deterministic host-memory sampler for provider metrics tests.
type fixedMem struct{ total, used uint64 }

func (m fixedMem) HostMemory(context.Context) (uint64, uint64, error) {
	return m.total, m.used, nil
}

// trackPod inserts a running Pod with one backing workload directly into the
// provider store, bypassing the runtime side effects of CreatePod.
func (p *Provider) trackPod(ns, name, uid, container, workloadID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pods[podKey(ns, name)] = &podState{
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		},
		workloads: []workload{{container: container, id: workloadID}},
	}
}

func TestStatsSummaryThroughProvider(t *testing.T) {
	rt := &statingRuntime{
		recordingRuntime: newRecordingRuntime(),
		stats: map[string]runtime.ResourceStats{
			"w-app": {CPUUsageCoreNanoSeconds: 9_000_000_000, MemoryUsageBytes: 64 << 20},
		},
	}
	p := New("mac-1", rt, WithCollector(metrics.NewCollector("mac-1", fixedMem{total: 8 << 30, used: 2 << 30})))
	p.trackPod("default", "web", "uid-1", "app", "w-app")

	s, err := p.StatsSummary(context.Background())
	if err != nil {
		t.Fatalf("StatsSummary: %v", err)
	}
	if len(s.Pods) != 1 || s.Pods[0].PodRef.Name != "web" {
		t.Fatalf("pods = %+v", s.Pods)
	}
	if s.Pods[0].CPU == nil || *s.Pods[0].CPU.UsageCoreNanoSeconds != 9_000_000_000 {
		t.Errorf("pod CPU = %v", s.Pods[0].CPU)
	}
	if s.Node.Memory == nil || *s.Node.Memory.WorkingSetBytes != 2<<30 {
		t.Errorf("node memory = %v", s.Node.Memory)
	}
}

func TestMetricsResourceThroughProvider(t *testing.T) {
	rt := &statingRuntime{
		recordingRuntime: newRecordingRuntime(),
		stats:            map[string]runtime.ResourceStats{"w-app": {CPUUsageCoreNanoSeconds: 1_000_000_000, MemoryUsageBytes: 1 << 20}},
	}
	p := New("mac-1", rt, WithCollector(metrics.NewCollector("mac-1", fixedMem{total: 8 << 30, used: 1 << 30})))
	p.trackPod("default", "web", "uid-1", "app", "w-app")

	families, err := p.MetricsResource(context.Background())
	if err != nil {
		t.Fatalf("MetricsResource: %v", err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "container_cpu_usage_seconds_total" {
			found = true
		}
	}
	if !found {
		t.Error("expected container_cpu_usage_seconds_total family")
	}
}

// A runtime without the Stater capability must still produce a summary, just
// without per-workload CPU/memory.
func TestStatsSummaryWithoutStaterDegrades(t *testing.T) {
	p := New("mac-1", newRecordingRuntime(), WithCollector(metrics.NewCollector("mac-1", fixedMem{total: 4 << 30, used: 1 << 30})))
	p.trackPod("default", "web", "uid-1", "app", "w-app")

	s, err := p.StatsSummary(context.Background())
	if err != nil {
		t.Fatalf("StatsSummary: %v", err)
	}
	if len(s.Pods) != 1 {
		t.Fatalf("pods = %d, want 1", len(s.Pods))
	}
	if s.Pods[0].Containers[0].CPU != nil {
		t.Error("container CPU should be absent without a Stater runtime")
	}
}
