package criserver

import (
	"context"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// sharedNetnsFakeRuntime is a fakeRuntime that additionally implements the
// pause-VM shared-netns create/join capability (SharedPodNetworkRuntime). It models the
// hypothetical future apple/container that can join a second container to an
// existing sandbox VM's network namespace, so the test can prove the adapter's
// multi-container admission logic is already correct against such a runtime —
// the only missing piece today is the runtime primitive itself.
type sharedNetnsFakeRuntime struct {
	*fakeRuntime
	supported bool
	reason    string
	joined    []joinedContainer
}

type joinedContainer struct {
	sandboxWorkloadID string
	spec              types.ContainerSpec
}

func (f *sharedNetnsFakeRuntime) SupportsSharedPodNetwork() (bool, string) {
	return f.supported, f.reason
}

func (f *sharedNetnsFakeRuntime) CreateInPodSandbox(_ context.Context, sandboxWorkloadID string, spec types.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.joined = append(f.joined, joinedContainer{sandboxWorkloadID: sandboxWorkloadID, spec: spec})
	f.statuses[spec.Name] = runtime.Status{ID: spec.Name, Phase: runtime.PhaseCreated}
	return spec.Name, nil
}

// newMultiContainerServer builds a server with the experimental #82 probe enabled
// and a single Ready sandbox, mirroring newServerWithRuntime.
func newMultiContainerServer(t *testing.T, rt ContainerRuntime) (*Server, string) {
	t.Helper()
	s := New(Options{Runtime: rt, MultiContainer: true})
	resp, err := s.RunPodSandbox(context.Background(), &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	return s, resp.GetPodSandboxId()
}

// Default (probe off): the second container is rejected with FailedPrecondition,
// and the message points the operator at the experimental flag rather than just
// refusing. This is the honest single-container behavior on apple/container.
func TestMultiContainerDefaultRejectsWithFlagHint(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()

	if _, err := s.CreateContainer(ctx, createReq(sandboxID, "app")); err != nil {
		t.Fatalf("first CreateContainer: %v", err)
	}
	_, err := s.CreateContainer(ctx, createReq(sandboxID, "sidecar"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second CreateContainer: code = %v, want FailedPrecondition", status.Code(err))
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "--experimental-multi-container") {
		t.Errorf("rejection should point at the experimental flag; got %q", msg)
	}
	if len(rt.created) != 1 {
		t.Errorf("a rejected second container must not create a workload; creates=%d", len(rt.created))
	}
}

// Probe on, runtime without the capability (apple/container today): the second
// container is rejected with Unimplemented and the message names the missing
// primitive, turning the rejection into an actionable capability statement. No
// workload is created.
func TestMultiContainerProbeRejectsUnsupportedRuntime(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newMultiContainerServer(t, rt)
	ctx := context.Background()

	if _, err := s.CreateContainer(ctx, createReq(sandboxID, "app")); err != nil {
		t.Fatalf("first CreateContainer: %v", err)
	}
	_, err := s.CreateContainer(ctx, createReq(sandboxID, "sidecar"))
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("second CreateContainer: code = %v, want Unimplemented", status.Code(err))
	}
	msg := status.Convert(err).Message()
	for _, want := range []string{"shared network namespace", "one Linux kernel per container", "pause-VM"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic missing %q; got %q", want, msg)
		}
	}
	if len(rt.created) != 1 {
		t.Errorf("a rejected second container must not create a workload; creates=%d", len(rt.created))
	}
}

// Probe on, runtime that IMPLEMENTS the pause-VM join capability: the second
// container is admitted only through CreateInPodSandbox, with the first workload
// passed as the sandbox VM target. apple/container never reaches this path (it
// does not implement the interface), so production cannot be misled.
func TestMultiContainerProbeCreatesSecondContainerInsideSandbox(t *testing.T) {
	rt := &sharedNetnsFakeRuntime{fakeRuntime: newFakeRuntime(), supported: true}
	s, sandboxID := newMultiContainerServer(t, rt)
	ctx := context.Background()

	first, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("first CreateContainer: %v", err)
	}
	if _, err := s.CreateContainer(ctx, createReq(sandboxID, "sidecar")); err != nil {
		t.Fatalf("second CreateContainer with a capable runtime should be admitted: %v", err)
	}
	if len(rt.created) != 1 {
		t.Fatalf("expected first workload to use normal Create only, got %d normal creates", len(rt.created))
	}
	if len(rt.joined) != 1 {
		t.Fatalf("expected second workload to use CreateInPodSandbox, got %d joins", len(rt.joined))
	}
	if got, want := rt.joined[0].sandboxWorkloadID, store.DeriveWorkloadID(first.GetContainerId()); got != want {
		t.Errorf("join sandbox workload = %q, want first workload %q", got, want)
	}
	if got := rt.joined[0].spec.Name; got == rt.joined[0].sandboxWorkloadID {
		t.Errorf("joined container reused sandbox workload id %q", got)
	}
}

// A runtime that implements the capability but reports false still rejects with its
// own reason, so a half-honest runtime cannot slip a second container through.
func TestMultiContainerProbeRejectsRuntimeReportingFalse(t *testing.T) {
	rt := &sharedNetnsFakeRuntime{fakeRuntime: newFakeRuntime(), supported: false, reason: "future-build: netns join not yet wired"}
	s, sandboxID := newMultiContainerServer(t, rt)
	ctx := context.Background()

	if _, err := s.CreateContainer(ctx, createReq(sandboxID, "app")); err != nil {
		t.Fatalf("first CreateContainer: %v", err)
	}
	_, err := s.CreateContainer(ctx, createReq(sandboxID, "sidecar"))
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("second CreateContainer: code = %v, want Unimplemented", status.Code(err))
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "netns join not yet wired") {
		t.Errorf("rejection should carry the runtime's own reason; got %q", msg)
	}
}

// Restart path is unchanged by the probe: once the prior container has Exited it
// no longer blocks a new one, regardless of the multi-container flag.
func TestMultiContainerProbeDoesNotBlockRestartAfterExit(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newMultiContainerServer(t, rt)
	ctx := context.Background()

	cResp, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("first CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id, Timeout: 1}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	// The prior container is Exited; a replacement (kubelet restartPolicy) must be
	// admitted without hitting the multi-container probe.
	if _, err := s.CreateContainer(ctx, createReq(sandboxID, "app")); err != nil {
		t.Fatalf("restart CreateContainer after exit should be admitted: %v", err)
	}
}

func TestMultiContainerInfo(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		s := New(Options{Runtime: newFakeRuntime()})
		if got := s.multiContainerInfo(); !strings.Contains(got, "disabled") {
			t.Errorf("info = %q, want disabled", got)
		}
	})
	t.Run("enabled_unsupported", func(t *testing.T) {
		s := New(Options{Runtime: newFakeRuntime(), MultiContainer: true})
		if got := s.multiContainerInfo(); !strings.Contains(got, "runtime unsupported") {
			t.Errorf("info = %q, want runtime unsupported", got)
		}
	})
	t.Run("enabled_supported", func(t *testing.T) {
		rt := &sharedNetnsFakeRuntime{fakeRuntime: newFakeRuntime(), supported: true}
		s := New(Options{Runtime: rt, MultiContainer: true})
		if got := s.multiContainerInfo(); !strings.Contains(got, "create/join support") {
			t.Errorf("info = %q, want pause-VM support", got)
		}
	})
}
