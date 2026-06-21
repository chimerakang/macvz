package criserver

import (
	"context"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func sampleStatsFixture() runtime.ResourceStats {
	return runtime.ResourceStats{
		Timestamp:               time.Unix(1700000000, 0),
		CPUUsageCoreNanoSeconds: 1234567,
		MemoryUsageBytes:        64 << 20,
		MemoryLimitBytes:        256 << 20,
	}
}

func TestContainerStatsConversion(t *testing.T) {
	rt := newFakeRuntime()
	rt.statsSample = sampleStatsFixture()
	s, sandboxID := newServerWithRuntime(t, rt)
	id := startedContainer(t, s, sandboxID)

	resp, err := s.ContainerStats(context.Background(), &runtimeapi.ContainerStatsRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	st := resp.GetStats()
	if st.GetAttributes().GetId() != id {
		t.Errorf("attributes id = %q, want %q", st.GetAttributes().GetId(), id)
	}
	if got := st.GetCpu().GetUsageCoreNanoSeconds().GetValue(); got != 1234567 {
		t.Errorf("cpu usage = %d, want 1234567", got)
	}
	if got := st.GetMemory().GetWorkingSetBytes().GetValue(); got != 64<<20 {
		t.Errorf("working set = %d, want %d", got, 64<<20)
	}
	if got := st.GetMemory().GetAvailableBytes().GetValue(); got != (256-64)<<20 {
		t.Errorf("available = %d, want %d", got, (256-64)<<20)
	}
	if st.GetCpu().GetTimestamp() != time.Unix(1700000000, 0).UnixNano() {
		t.Errorf("cpu timestamp mismatch")
	}
}

func TestContainerStatsUnavailableHasNoSample(t *testing.T) {
	rt := newFakeRuntime()
	rt.statsErr = runtime.ErrStatsUnavailable
	s, sandboxID := newServerWithRuntime(t, rt)
	id := startedContainer(t, s, sandboxID)

	resp, err := s.ContainerStats(context.Background(), &runtimeapi.ContainerStatsRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	// Known but unobservable: attributes present, no faked CPU/memory zeros.
	if resp.GetStats().GetAttributes().GetId() != id {
		t.Error("expected attributes for an unobservable container")
	}
	if resp.GetStats().GetCpu() != nil || resp.GetStats().GetMemory() != nil {
		t.Error("expected nil cpu/memory for an unavailable sample, not zeros")
	}
}

func TestContainerStatsNotRunningHasNoSample(t *testing.T) {
	rt := newFakeRuntime()
	rt.statsSample = sampleStatsFixture()
	s, sandboxID := newServerWithRuntime(t, rt)
	// Created but not started.
	cResp, _ := s.CreateContainer(context.Background(), createReq(sandboxID, "app"))
	id := cResp.GetContainerId()

	resp, err := s.ContainerStats(context.Background(), &runtimeapi.ContainerStatsRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	if resp.GetStats().GetCpu() != nil {
		t.Error("a non-running container must report no CPU sample")
	}
}

func TestContainerStatsNotFound(t *testing.T) {
	rt := newFakeRuntime()
	s, _ := newServerWithRuntime(t, rt)
	_, err := s.ContainerStats(context.Background(), &runtimeapi.ContainerStatsRequest{ContainerId: "nope"})
	wantCode(t, err, codes.NotFound)
}

func TestListContainerStatsFilters(t *testing.T) {
	rt := newFakeRuntime()
	rt.statsSample = sampleStatsFixture()
	s, sandboxID := newServerWithRuntime(t, rt)
	id := startedContainer(t, s, sandboxID)

	resp, err := s.ListContainerStats(context.Background(), &runtimeapi.ListContainerStatsRequest{
		Filter: &runtimeapi.ContainerStatsFilter{Id: id},
	})
	if err != nil {
		t.Fatalf("ListContainerStats: %v", err)
	}
	if len(resp.GetStats()) != 1 {
		t.Fatalf("got %d stats, want 1", len(resp.GetStats()))
	}

	// A filter that matches nothing returns an empty list, not an error.
	resp, err = s.ListContainerStats(context.Background(), &runtimeapi.ListContainerStatsRequest{
		Filter: &runtimeapi.ContainerStatsFilter{Id: "other"},
	})
	if err != nil || len(resp.GetStats()) != 0 {
		t.Fatalf("filtered ListContainerStats = %v (err=%v), want empty", resp.GetStats(), err)
	}
}

func TestPodSandboxStatsAggregatesContainer(t *testing.T) {
	rt := newFakeRuntime()
	rt.statsSample = sampleStatsFixture()
	s, sandboxID := newServerWithRuntime(t, rt)
	startedContainer(t, s, sandboxID)

	resp, err := s.PodSandboxStats(context.Background(), &runtimeapi.PodSandboxStatsRequest{PodSandboxId: sandboxID})
	if err != nil {
		t.Fatalf("PodSandboxStats: %v", err)
	}
	lin := resp.GetStats().GetLinux()
	if lin == nil {
		t.Fatal("expected linux pod sandbox stats")
	}
	if got := lin.GetCpu().GetUsageCoreNanoSeconds().GetValue(); got != 1234567 {
		t.Errorf("pod cpu = %d, want the container's sample lifted to the pod", got)
	}
	if len(lin.GetContainers()) != 1 {
		t.Errorf("pod stats should list its 1 container, got %d", len(lin.GetContainers()))
	}
}

func TestListPodSandboxStats(t *testing.T) {
	rt := newFakeRuntime()
	rt.statsSample = sampleStatsFixture()
	s, sandboxID := newServerWithRuntime(t, rt)
	startedContainer(t, s, sandboxID)

	resp, err := s.ListPodSandboxStats(context.Background(), &runtimeapi.ListPodSandboxStatsRequest{})
	if err != nil {
		t.Fatalf("ListPodSandboxStats: %v", err)
	}
	if len(resp.GetStats()) != 1 {
		t.Fatalf("got %d sandbox stats, want 1", len(resp.GetStats()))
	}
	if resp.GetStats()[0].GetAttributes().GetId() != sandboxID {
		t.Errorf("sandbox stats id = %q, want %q", resp.GetStats()[0].GetAttributes().GetId(), sandboxID)
	}
}
