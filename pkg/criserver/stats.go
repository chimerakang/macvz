package criserver

import (
	"context"
	"errors"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// statusInvalid and statusNotFound build the InvalidArgument / NotFound errors the
// stats surfaces return, keeping the call sites terse.
func statusInvalid(msg string) error { return status.Error(codes.InvalidArgument, msg) }
func statusNotFound(method, id string) error {
	return status.Errorf(codes.NotFound, "%s: %q not found", method, id)
}

// This file implements the CRI-P6 stats surfaces (#78): ContainerStats,
// ListContainerStats, PodSandboxStats, and ListPodSandboxStats. kubelet's Summary
// API consumes these to publish node and Pod metrics.
//
// Samples come from the runtime's optional Stater capability (apple/container
// `container stats`). When the runtime cannot sample a workload — it is not
// running, or exposes no metrics — that workload is reported with nil stats
// blocks rather than zeros, so a consumer never mistakes "unobservable" for
// "idle". Filesystem (writable layer), swap, and PSI stats are left unset: the
// micro-VM runtime exposes no honest source for them in this phase.

// statsCapable returns the runtime's stats capability when it implements one. The
// container lifecycle runtime and the stats runtime are the same object in
// production (*container.Driver), but the capability is optional so a runtime
// without stats degrades gracefully instead of failing the surface.
func (s *Server) statsCapable() (statsRuntime, bool) {
	sr, ok := s.containerRuntime.(statsRuntime)
	return sr, ok
}

// ContainerStats returns a resource-usage sample for one container. An unknown
// container is NotFound; a container with no sampleable metrics (not running, or
// the runtime exposes none) returns a ContainerStats carrying only its attributes,
// which is the honest "known but unobservable" answer kubelet tolerates.
func (s *Server) ContainerStats(ctx context.Context, req *runtimeapi.ContainerStatsRequest) (*runtimeapi.ContainerStatsResponse, error) {
	id := req.GetContainerId()
	if id == "" {
		return nil, statusInvalid("ContainerStats: container id is required")
	}
	c, ok := s.containers.Get(id)
	if !ok {
		return nil, statusNotFound("ContainerStats", id)
	}
	return &runtimeapi.ContainerStatsResponse{Stats: s.containerStats(ctx, &c)}, nil
}

// ListContainerStats returns stats for every container matching the filter. A
// container without sampleable metrics still appears, carrying only attributes, so
// the kubelet sees the full container set.
func (s *Server) ListContainerStats(ctx context.Context, req *runtimeapi.ListContainerStatsRequest) (*runtimeapi.ListContainerStatsResponse, error) {
	filter := req.GetFilter()
	var items []*runtimeapi.ContainerStats
	for _, c := range s.containers.List() {
		if !matchesContainerStatsFilter(&c, filter) {
			continue
		}
		items = append(items, s.containerStats(ctx, &c))
	}
	return &runtimeapi.ListContainerStatsResponse{Stats: items}, nil
}

// PodSandboxStats returns stats for one sandbox, aggregating its single
// container's CPU and memory at the Pod level (CRI-P6 keeps one container per Pod).
func (s *Server) PodSandboxStats(ctx context.Context, req *runtimeapi.PodSandboxStatsRequest) (*runtimeapi.PodSandboxStatsResponse, error) {
	id := req.GetPodSandboxId()
	if id == "" {
		return nil, statusInvalid("PodSandboxStats: pod sandbox id is required")
	}
	sb, ok := s.sandboxes.Get(id)
	if !ok {
		return nil, statusNotFound("PodSandboxStats", id)
	}
	return &runtimeapi.PodSandboxStatsResponse{Stats: s.podSandboxStats(ctx, &sb)}, nil
}

// ListPodSandboxStats returns stats for every sandbox matching the filter.
func (s *Server) ListPodSandboxStats(ctx context.Context, req *runtimeapi.ListPodSandboxStatsRequest) (*runtimeapi.ListPodSandboxStatsResponse, error) {
	filter := req.GetFilter()
	var items []*runtimeapi.PodSandboxStats
	for _, sb := range s.sandboxes.List() {
		if filter != nil {
			if filter.GetId() != "" && filter.GetId() != sb.ID {
				continue
			}
			if !labelsMatch(sb.Labels, filter.GetLabelSelector()) {
				continue
			}
		}
		items = append(items, s.podSandboxStats(ctx, &sb))
	}
	return &runtimeapi.ListPodSandboxStatsResponse{Stats: items}, nil
}

// containerStats builds the CRI stats for one container, sampling the runtime when
// it can. The Cpu/Memory blocks are nil when no sample is available.
func (s *Server) containerStats(ctx context.Context, c *store.Container) *runtimeapi.ContainerStats {
	cs := &runtimeapi.ContainerStats{
		Attributes: &runtimeapi.ContainerAttributes{
			Id:          c.ID,
			Metadata:    &runtimeapi.ContainerMetadata{Name: c.Metadata.Name, Attempt: c.Metadata.Attempt},
			Labels:      c.Labels,
			Annotations: c.Annotations,
		},
	}
	if sample, ok := s.sampleStats(ctx, c); ok {
		cs.Cpu = toCRICPUUsage(sample)
		cs.Memory = toCRIMemoryUsage(sample)
	}
	return cs
}

// podSandboxStats builds the CRI stats for one sandbox by lifting its single
// container's sample to the Pod level. With no running container the Linux stats
// block is still returned (attributes only) so the sandbox is visible to kubelet.
func (s *Server) podSandboxStats(ctx context.Context, sb *store.Sandbox) *runtimeapi.PodSandboxStats {
	ps := &runtimeapi.PodSandboxStats{
		Attributes: &runtimeapi.PodSandboxAttributes{
			Id:          sb.ID,
			Metadata:    &runtimeapi.PodSandboxMetadata{Name: sb.Metadata.Name, Uid: sb.Metadata.UID, Namespace: sb.Metadata.Namespace, Attempt: sb.Metadata.Attempt},
			Labels:      sb.Labels,
			Annotations: sb.Annotations,
		},
		Linux: &runtimeapi.LinuxPodSandboxStats{},
	}
	containers := s.containers.ListBySandbox(sb.ID)
	var perContainer []*runtimeapi.ContainerStats
	for i := range containers {
		c := containers[i]
		stats := s.containerStats(ctx, &c)
		perContainer = append(perContainer, stats)
		// CRI-P6 keeps one container per Pod, so the single container's sample is the
		// Pod's CPU/memory; aggregation across containers is out of scope.
		if stats.Cpu != nil && ps.Linux.Cpu == nil {
			ps.Linux.Cpu = stats.Cpu
		}
		if stats.Memory != nil && ps.Linux.Memory == nil {
			ps.Linux.Memory = stats.Memory
		}
	}
	ps.Linux.Containers = perContainer
	return ps
}

// sampleStats samples one container's resource usage via the runtime's Stater
// capability. It returns ok=false when the runtime has no stats capability, the
// container is not running, or the sample is unavailable — all "no honest sample"
// cases that must not be reported as zero usage.
func (s *Server) sampleStats(ctx context.Context, c *store.Container) (runtime.ResourceStats, bool) {
	if c.State != store.ContainerRunning {
		return runtime.ResourceStats{}, false
	}
	sr, ok := s.statsCapable()
	if !ok {
		return runtime.ResourceStats{}, false
	}
	sample, err := sr.Stats(ctx, c.WorkloadID)
	if err != nil {
		// ErrStatsUnavailable (and a vanished workload) are expected "skip this one"
		// signals, not surface failures.
		return runtime.ResourceStats{}, false
	}
	return sample, true
}

func toCRICPUUsage(s runtime.ResourceStats) *runtimeapi.CpuUsage {
	return &runtimeapi.CpuUsage{
		Timestamp:            s.Timestamp.UnixNano(),
		UsageCoreNanoSeconds: &runtimeapi.UInt64Value{Value: s.CPUUsageCoreNanoSeconds},
	}
}

func toCRIMemoryUsage(s runtime.ResourceStats) *runtimeapi.MemoryUsage {
	mem := &runtimeapi.MemoryUsage{
		Timestamp:       s.Timestamp.UnixNano(),
		WorkingSetBytes: &runtimeapi.UInt64Value{Value: s.MemoryUsageBytes},
		UsageBytes:      &runtimeapi.UInt64Value{Value: s.MemoryUsageBytes},
	}
	// AvailableBytes is meaningful only against a known limit.
	if s.MemoryLimitBytes > 0 {
		avail := uint64(0)
		if s.MemoryLimitBytes > s.MemoryUsageBytes {
			avail = s.MemoryLimitBytes - s.MemoryUsageBytes
		}
		mem.AvailableBytes = &runtimeapi.UInt64Value{Value: avail}
	}
	return mem
}

// matchesContainerStatsFilter applies a CRI ContainerStatsFilter (id, sandbox id,
// label selector). A nil filter matches all.
func matchesContainerStatsFilter(c *store.Container, f *runtimeapi.ContainerStatsFilter) bool {
	if f == nil {
		return true
	}
	if f.GetId() != "" && f.GetId() != c.ID {
		return false
	}
	if f.GetPodSandboxId() != "" && f.GetPodSandboxId() != c.SandboxID {
		return false
	}
	return labelsMatch(c.Labels, f.GetLabelSelector())
}

// labelsMatch reports whether have contains every key/value in want.
func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// statsErrIsUnavailable reports whether err is the runtime's "no sample" signal,
// used by tests to assert the skip path.
func statsErrIsUnavailable(err error) bool {
	return errors.Is(err, runtime.ErrStatsUnavailable)
}
