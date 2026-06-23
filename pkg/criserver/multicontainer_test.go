package criserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/network"
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
	supported    bool
	reason       string
	joinReturnID string
	joined       []joinedContainer
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
	id := spec.Name
	if f.joinReturnID != "" {
		id = f.joinReturnID
	}
	f.joined = append(f.joined, joinedContainer{sandboxWorkloadID: sandboxWorkloadID, spec: spec})
	f.statuses[id] = runtime.Status{ID: id, Phase: runtime.PhaseCreated}
	return id, nil
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

// The runtime capability may return an addressable workload ID that differs from
// the requested spec.Name. Persist and use that runtime-owned ID for later
// Start/Stop/Destroy calls; otherwise the adapter-side #86 path would be ready
// only for helpers that happen to mirror the adapter's desired name.
func TestMultiContainerProbeUsesRuntimeReturnedJoinedWorkloadID(t *testing.T) {
	rt := &sharedNetnsFakeRuntime{
		fakeRuntime:  newFakeRuntime(),
		supported:    true,
		joinReturnID: "linuxpod-joined-sidecar-1",
	}
	s, sandboxID := newMultiContainerServer(t, rt)
	ctx := context.Background()

	if _, err := s.CreateContainer(ctx, createReq(sandboxID, "app")); err != nil {
		t.Fatalf("first CreateContainer: %v", err)
	}
	sidecar, err := s.CreateContainer(ctx, createReq(sandboxID, "sidecar"))
	if err != nil {
		t.Fatalf("sidecar CreateContainer: %v", err)
	}
	rec, ok := s.containers.Get(sidecar.GetContainerId())
	if !ok {
		t.Fatalf("sidecar record not persisted")
	}
	if rec.WorkloadID != rt.joinReturnID {
		t.Fatalf("joined workloadID = %q, want runtime return %q", rec.WorkloadID, rt.joinReturnID)
	}

	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: sidecar.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer(sidecar): %v", err)
	}
	if got := rt.started[len(rt.started)-1]; got != rt.joinReturnID {
		t.Errorf("StartContainer used workload %q, want runtime return %q", got, rt.joinReturnID)
	}
	if _, err := s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: sidecar.GetContainerId()}); err != nil {
		t.Fatalf("RemoveContainer(sidecar): %v", err)
	}
	if got := rt.destroyed[len(rt.destroyed)-1]; got != rt.joinReturnID {
		t.Errorf("RemoveContainer destroyed workload %q, want runtime return %q", got, rt.joinReturnID)
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

// newNetworkedMultiContainerServer builds a multi-container-enabled server with a
// real PodIPAM and a fake Pod network, so the honest networking contract (one Pod
// IP shared across containers) can be asserted.
func newNetworkedMultiContainerServer(t *testing.T, rt ContainerRuntime) (*Server, *network.PodIPAM, *fakePodNet) {
	t.Helper()
	ipam, err := network.NewPodIPAM(testPodCIDR)
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	pnet := newFakePodNet()
	s := New(Options{Runtime: rt, IPAM: ipam, PodNetwork: pnet, MultiContainer: true})
	s.vmIPPollAttempts = 3
	s.vmIPPollInterval = time.Millisecond
	return s, ipam, pnet
}

func createStartNamed(t *testing.T, s *Server, sandboxID, name string) string {
	t.Helper()
	ctx := context.Background()
	cResp, err := s.CreateContainer(ctx, createReq(sandboxID, name))
	if err != nil {
		t.Fatalf("CreateContainer(%s): %v", name, err)
	}
	id := cResp.GetContainerId()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer(%s): %v", name, err)
	}
	return id
}

// A joined container shares the sandbox owner's single Pod IP: the Pod network is
// attached exactly once (by the owner's start), the joined container's start
// allocates no second IP and triggers no second attach, and PodSandboxStatus
// reports one Pod IP for the whole Pod. This is the core Kubernetes Pod contract.
func TestMultiContainerSharesOnePodIP(t *testing.T) {
	rt := &sharedNetnsFakeRuntime{fakeRuntime: newFakeRuntime(), supported: true}
	rt.startIP = "192.168.64.9"
	s, ipam, pnet := newNetworkedMultiContainerServer(t, rt)

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"
	wantIP := ipam.IP(key)
	if wantIP == "" {
		t.Fatalf("RunPodSandbox did not reserve a Pod IP")
	}

	createStartNamed(t, s, sandboxID, "app")     // sandbox owner
	createStartNamed(t, s, sandboxID, "sidecar") // joined container

	if pnet.attachCalls != 1 {
		t.Errorf("Pod network attached %d times, want exactly 1 (joined container must not re-attach)", pnet.attachCalls)
	}
	if len(rt.joined) != 1 {
		t.Fatalf("expected exactly one CreateInPodSandbox join, got %d", len(rt.joined))
	}
	if got := sandboxNetworkIP(t, s, sandboxID); got != wantIP {
		t.Errorf("PodSandboxStatus Pod IP = %q, want the single reserved IP %q", got, wantIP)
	}
	// Both containers carry the same Pod identity / IP key.
	if got := ipam.IP(key); got != wantIP {
		t.Errorf("a joined container changed the Pod IP: %q != %q", got, wantIP)
	}
}

// Stopping one container in a multi-container Pod keeps the shared Pod network up
// while another container is still live; only the last container draining tears it
// down. This preserves sandbox-vs-workload lifecycle semantics.
func TestMultiContainerStopKeepsNetworkUntilLastDrains(t *testing.T) {
	rt := &sharedNetnsFakeRuntime{fakeRuntime: newFakeRuntime(), supported: true}
	rt.startIP = "192.168.64.10"
	s, _, pnet := newNetworkedMultiContainerServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"
	ownerID := createStartNamed(t, s, sandboxID, "app")
	sidecarID := createStartNamed(t, s, sandboxID, "sidecar")

	// Stop the sidecar: the owner is still live, so the Pod network must stay up.
	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: sidecarID, Timeout: 1}); err != nil {
		t.Fatalf("StopContainer(sidecar): %v", err)
	}
	if _, ok := pnet.isAttached(key); !ok {
		t.Errorf("Pod network detached after stopping only the sidecar; the owner is still live")
	}

	// Stop the owner: now the Pod has drained, so the network is released.
	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: ownerID, Timeout: 1}); err != nil {
		t.Fatalf("StopContainer(owner): %v", err)
	}
	if _, ok := pnet.isAttached(key); ok {
		t.Errorf("Pod network still attached after the last container drained")
	}
}

// The shared namespace lifetime must not be tied to the first workload
// container. If the sandbox owner is stopped while a joined sidecar is still
// running, the Pod network must stay attached until the sidecar drains too.
func TestMultiContainerStopOwnerFirstKeepsNetworkUntilSidecarDrains(t *testing.T) {
	rt := &sharedNetnsFakeRuntime{fakeRuntime: newFakeRuntime(), supported: true}
	rt.startIP = "192.168.64.12"
	s, _, pnet := newNetworkedMultiContainerServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"
	ownerID := createStartNamed(t, s, sandboxID, "app")
	sidecarID := createStartNamed(t, s, sandboxID, "sidecar")

	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: ownerID, Timeout: 1}); err != nil {
		t.Fatalf("StopContainer(owner): %v", err)
	}
	if _, ok := pnet.isAttached(key); !ok {
		t.Errorf("Pod network detached after stopping the owner while sidecar is still live")
	}

	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: sidecarID, Timeout: 1}); err != nil {
		t.Fatalf("StopContainer(sidecar): %v", err)
	}
	if _, ok := pnet.isAttached(key); ok {
		t.Errorf("Pod network still attached after owner and sidecar both drained")
	}
}

// A failed join leaves no leaked CRI state, Pod IP, or workload: the existing
// owner and its single attachment are untouched, and no partial second container
// record or join is recorded.
func TestMultiContainerFailedJoinNoLeak(t *testing.T) {
	rt := &sharedNetnsFakeRuntime{fakeRuntime: newFakeRuntime(), supported: true}
	rt.startIP = "192.168.64.11"
	s, ipam, pnet := newNetworkedMultiContainerServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"
	createStartNamed(t, s, sandboxID, "app")
	wantIP := ipam.IP(key)

	// The runtime now fails the join.
	rt.createErr = context.DeadlineExceeded
	_, err := s.CreateContainer(ctx, createReq(sandboxID, "sidecar"))
	if err == nil {
		t.Fatalf("CreateContainer(sidecar) should fail when the join errors")
	}

	listed, err := s.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{PodSandboxId: sandboxID},
	})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if n := len(listed.GetContainers()); n != 1 {
		t.Errorf("failed join leaked a container record: have %d, want 1", n)
	}
	if len(rt.joined) != 0 {
		t.Errorf("failed join recorded %d joins, want 0", len(rt.joined))
	}
	if got := ipam.IP(key); got != wantIP {
		t.Errorf("failed join disturbed the Pod IP: %q != %q", got, wantIP)
	}
	if _, ok := pnet.isAttached(key); !ok {
		t.Errorf("failed join detached the still-live owner's Pod network")
	}
	if pnet.attachCalls != 1 {
		t.Errorf("attachCalls = %d after a failed join, want 1 (owner only)", pnet.attachCalls)
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
