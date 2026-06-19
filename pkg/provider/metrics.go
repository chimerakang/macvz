package provider

import (
	"context"

	"github.com/chimerakang/macvz/pkg/metrics"
	"github.com/chimerakang/macvz/pkg/runtime"
	dto "github.com/prometheus/client_model/go"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// StatsSummary serves the kubelet Summary API (GET /stats/summary) wired into
// the Virtual Kubelet kubelet server. It snapshots the tracked Pods and lets
// the collector sample each backing workload, so `kubectl top` and metrics-
// server can observe node and Pod resource usage (#25).
func (p *Provider) StatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	pods := p.metricsPodInputs()
	return p.collector.Summary(ctx, pods, p.statsFunc()), nil
}

// MetricsResource serves the Prometheus resource-metrics endpoint
// (GET /metrics/resource) consumed by metrics-server's default scrape mode.
func (p *Provider) MetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	pods := p.metricsPodInputs()
	ready, _ := p.runtimeReady(ctx)
	return p.collector.ResourceMetrics(ctx, pods, p.statsFunc(), ready), nil
}

// metricsPodInputs snapshots the tracked Pods into the collector's input shape.
// It copies under the read lock so collection (which performs runtime I/O) runs
// without holding it.
func (p *Provider) metricsPodInputs() []metrics.PodInput {
	p.mu.RLock()
	defer p.mu.RUnlock()

	inputs := make([]metrics.PodInput, 0, len(p.pods))
	for _, st := range p.pods {
		pod := st.pod
		// A Pod that can never run on this node has no workloads to sample.
		if st.terminalStatus != nil {
			continue
		}
		start := pod.CreationTimestamp.Time
		if pod.Status.StartTime != nil {
			start = pod.Status.StartTime.Time
		}
		in := metrics.PodInput{
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			UID:        string(pod.UID),
			StartTime:  start,
			Containers: make([]metrics.ContainerInput, 0, len(st.workloads)),
		}
		for _, w := range st.workloads {
			in.Containers = append(in.Containers, metrics.ContainerInput{
				Name:       w.container,
				WorkloadID: w.id,
				StartTime:  start,
			})
		}
		inputs = append(inputs, in)
	}
	return inputs
}

// statsFunc adapts the runtime's optional Stater capability to the collector's
// StatsFunc. When the runtime does not implement Stater, or cannot sample a
// given workload, ok is false and the collector reports that workload without
// CPU/memory rather than failing the scrape.
func (p *Provider) statsFunc() metrics.StatsFunc {
	stater, ok := p.rt.(runtime.Stater)
	if !ok {
		return func(context.Context, string) (runtime.ResourceStats, bool) {
			return runtime.ResourceStats{}, false
		}
	}
	return func(ctx context.Context, id string) (runtime.ResourceStats, bool) {
		rs, err := stater.Stats(ctx, id)
		if err != nil {
			return runtime.ResourceStats{}, false
		}
		return rs, true
	}
}
