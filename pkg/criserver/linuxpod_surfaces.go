package criserver

import (
	"context"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// linuxpod_surfaces.go wires the kubelet-facing CRI surfaces for the
// experimental LinuxPod service (CRI-L4, #129). The default apple/container
// Server has its own implementation in logs.go/streaming.go/stats.go; this file
// is only for LinuxPodService, registered behind --experimental-linuxpod-backend.

// ReopenContainerLog validates the container and asks the backend for the log
// path. The helper owns the actual file writer, so there is no adapter-side pump
// to reopen; a successful ContainerLogPath proves the backend has a log file for
// kubelet to tail. Unsupported logs return Unimplemented via linuxpodToCRIError.
func (s *LinuxPodService) ReopenContainerLog(ctx context.Context, req *runtimeapi.ReopenContainerLogRequest) (*runtimeapi.ReopenContainerLogResponse, error) {
	c, err := s.linuxpodContainer(req.GetContainerId(), "ReopenContainerLog")
	if err != nil {
		return nil, err
	}
	if c.LinuxPod == nil {
		return nil, status.Errorf(codes.Internal, "ReopenContainerLog: container %q has no LinuxPod mapping", c.ID)
	}
	if _, err := s.backend.ContainerLogPath(ctx, linuxpod.Ref{PodID: c.SandboxID, ContainerID: c.LinuxPod.BackendContainerID}); err != nil {
		return nil, linuxpodToCRIError("ReopenContainerLog", err)
	}
	return &runtimeapi.ReopenContainerLogResponse{}, nil
}

// ExecSync runs a non-interactive command through the LinuxPod helper. Streaming
// Exec/Attach/PortForward remain explicit follow-ups (#131/#132).
func (s *LinuxPodService) ExecSync(ctx context.Context, req *runtimeapi.ExecSyncRequest) (*runtimeapi.ExecSyncResponse, error) {
	if len(req.GetCmd()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ExecSync: command is required")
	}
	c, err := s.linuxpodRunningContainer(ctx, req.GetContainerId(), "ExecSync")
	if err != nil {
		return nil, err
	}
	if c.LinuxPod == nil {
		return nil, status.Errorf(codes.Internal, "ExecSync: container %q has no LinuxPod mapping", c.ID)
	}
	if t := req.GetTimeout(); t > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(t)*time.Second)
		defer cancel()
	}
	res, err := s.backend.ExecSync(ctx, linuxpod.ExecRequest{
		PodID:          c.SandboxID,
		ContainerID:    c.LinuxPod.BackendContainerID,
		Command:        req.GetCmd(),
		TimeoutSeconds: int(req.GetTimeout()),
	})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, status.Errorf(codes.DeadlineExceeded, "ExecSync: command timed out after %ds", req.GetTimeout())
		}
		return nil, linuxpodToCRIError("ExecSync", err)
	}
	return &runtimeapi.ExecSyncResponse{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: int32(res.ExitCode)}, nil
}

func (s *LinuxPodService) ContainerStats(ctx context.Context, req *runtimeapi.ContainerStatsRequest) (*runtimeapi.ContainerStatsResponse, error) {
	id := req.GetContainerId()
	if id == "" {
		return nil, statusInvalid("ContainerStats: container id is required")
	}
	c, ok := s.containers.Get(id)
	if !ok {
		return nil, statusNotFound("ContainerStats", id)
	}
	return &runtimeapi.ContainerStatsResponse{Stats: s.linuxpodContainerStats(ctx, &c)}, nil
}

func (s *LinuxPodService) ListContainerStats(ctx context.Context, req *runtimeapi.ListContainerStatsRequest) (*runtimeapi.ListContainerStatsResponse, error) {
	filter := req.GetFilter()
	var items []*runtimeapi.ContainerStats
	for _, c := range s.containers.List() {
		if !matchesContainerStatsFilter(&c, filter) {
			continue
		}
		items = append(items, s.linuxpodContainerStats(ctx, &c))
	}
	return &runtimeapi.ListContainerStatsResponse{Stats: items}, nil
}

func (s *LinuxPodService) PodSandboxStats(ctx context.Context, req *runtimeapi.PodSandboxStatsRequest) (*runtimeapi.PodSandboxStatsResponse, error) {
	id := req.GetPodSandboxId()
	if id == "" {
		return nil, statusInvalid("PodSandboxStats: pod sandbox id is required")
	}
	sb, ok := s.sandboxes.Get(id)
	if !ok {
		return nil, statusNotFound("PodSandboxStats", id)
	}
	return &runtimeapi.PodSandboxStatsResponse{Stats: s.linuxpodPodSandboxStats(ctx, &sb)}, nil
}

func (s *LinuxPodService) ListPodSandboxStats(ctx context.Context, req *runtimeapi.ListPodSandboxStatsRequest) (*runtimeapi.ListPodSandboxStatsResponse, error) {
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
		items = append(items, s.linuxpodPodSandboxStats(ctx, &sb))
	}
	return &runtimeapi.ListPodSandboxStatsResponse{Stats: items}, nil
}

func (s *LinuxPodService) linuxpodContainer(id, method string) (store.Container, error) {
	if id == "" {
		return store.Container{}, status.Errorf(codes.InvalidArgument, "%s: container id is required", method)
	}
	c, ok := s.containers.Get(id)
	if !ok {
		return store.Container{}, status.Errorf(codes.NotFound, "%s: container %q not found", method, id)
	}
	return c, nil
}

func (s *LinuxPodService) linuxpodRunningContainer(ctx context.Context, id, method string) (store.Container, error) {
	c, err := s.linuxpodContainer(id, method)
	if err != nil {
		return store.Container{}, err
	}
	if c.State != store.ContainerRunning {
		return store.Container{}, status.Errorf(codes.FailedPrecondition,
			"%s: container %q is %s, expected Running", method, id, c.State)
	}
	s.reconcileContainerBackendState(ctx, id)
	c, err = s.linuxpodContainer(id, method)
	if err != nil {
		return store.Container{}, err
	}
	if c.State != store.ContainerRunning {
		return store.Container{}, status.Errorf(codes.FailedPrecondition,
			"%s: container %q is %s, expected Running", method, id, c.State)
	}
	return c, nil
}

func (s *LinuxPodService) linuxpodContainerStats(ctx context.Context, c *store.Container) *runtimeapi.ContainerStats {
	cs := &runtimeapi.ContainerStats{
		Attributes: &runtimeapi.ContainerAttributes{
			Id:          c.ID,
			Metadata:    &runtimeapi.ContainerMetadata{Name: c.Metadata.Name, Attempt: c.Metadata.Attempt},
			Labels:      c.Labels,
			Annotations: c.Annotations,
		},
	}
	if c.State != store.ContainerRunning || c.LinuxPod == nil {
		return cs
	}
	sample, err := s.backend.ContainerStats(ctx, linuxpod.Ref{PodID: c.SandboxID, ContainerID: c.LinuxPod.BackendContainerID})
	if err != nil {
		return cs
	}
	cs.Cpu = &runtimeapi.CpuUsage{
		Timestamp:      sample.TimestampNanos,
		UsageNanoCores: &runtimeapi.UInt64Value{Value: sample.CPUUsageNanoCores},
	}
	cs.Memory = &runtimeapi.MemoryUsage{
		Timestamp:       sample.TimestampNanos,
		WorkingSetBytes: &runtimeapi.UInt64Value{Value: sample.MemoryWorkingSetBytes},
		UsageBytes:      &runtimeapi.UInt64Value{Value: sample.MemoryWorkingSetBytes},
	}
	return cs
}

func (s *LinuxPodService) linuxpodPodSandboxStats(ctx context.Context, sb *store.Sandbox) *runtimeapi.PodSandboxStats {
	ps := &runtimeapi.PodSandboxStats{
		Attributes: &runtimeapi.PodSandboxAttributes{
			Id:          sb.ID,
			Metadata:    &runtimeapi.PodSandboxMetadata{Name: sb.Metadata.Name, Uid: sb.Metadata.UID, Namespace: sb.Metadata.Namespace, Attempt: sb.Metadata.Attempt},
			Labels:      sb.Labels,
			Annotations: sb.Annotations,
		},
		Linux: &runtimeapi.LinuxPodSandboxStats{},
	}
	for _, c := range s.containers.ListBySandbox(sb.ID) {
		stats := s.linuxpodContainerStats(ctx, &c)
		ps.Linux.Containers = append(ps.Linux.Containers, stats)
		if stats.Cpu != nil && ps.Linux.Cpu == nil {
			ps.Linux.Cpu = stats.Cpu
		}
		if stats.Memory != nil && ps.Linux.Memory == nil {
			ps.Linux.Memory = stats.Memory
		}
	}
	return ps
}
