package criserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// fakePodNet is a PodNetwork that records attach/detach calls and allows error
// injection, so the CRI-P5 lifecycle tests stay hermetic — no pf, no route, no
// micro-VM.
type fakePodNet struct {
	mu          sync.Mutex
	attached    map[string]podnet.Endpoint
	detached    []string
	attachCalls int
	attachErr   error
	detachErr   error
}

func newFakePodNet() *fakePodNet {
	return &fakePodNet{attached: map[string]podnet.Endpoint{}}
}

func (f *fakePodNet) Attach(_ context.Context, ep podnet.Endpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachCalls++
	if f.attachErr != nil {
		return f.attachErr
	}
	f.attached[ep.PodKey] = ep
	return nil
}

func (f *fakePodNet) Detach(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.detachErr != nil {
		return f.detachErr
	}
	f.detached = append(f.detached, key)
	delete(f.attached, key)
	return nil
}

func (f *fakePodNet) isAttached(key string) (podnet.Endpoint, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ep, ok := f.attached[key]
	return ep, ok
}

const testPodCIDR = "10.244.1.0/24"

// newNetworkedServer builds a server with in-memory stores, the given runtime, a
// real PodIPAM over testPodCIDR, and a fake Pod network. VM-IP polling is shortened
// so the attach path runs instantly.
func newNetworkedServer(t *testing.T, rt ContainerRuntime) (*Server, *network.PodIPAM, *fakePodNet) {
	t.Helper()
	ipam, err := network.NewPodIPAM(testPodCIDR)
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	pnet := newFakePodNet()
	s := New(Options{Runtime: rt, IPAM: ipam, PodNetwork: pnet})
	s.vmIPPollAttempts = 3
	s.vmIPPollInterval = time.Millisecond
	return s, ipam, pnet
}

func runSandbox(t *testing.T, s *Server, name, namespace, uid string) string {
	t.Helper()
	resp, err := s.RunPodSandbox(context.Background(), &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: name, Namespace: namespace, Uid: uid},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	return resp.GetPodSandboxId()
}

// startContainerInSandbox creates and starts the single container of a sandbox,
// returning its container ID.
func startContainerInSandbox(t *testing.T, s *Server, sandboxID string) string {
	t.Helper()
	ctx := context.Background()
	cResp, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	return id
}

func sandboxNetworkIP(t *testing.T, s *Server, sandboxID string) string {
	t.Helper()
	resp, err := s.PodSandboxStatus(context.Background(), &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	return resp.GetStatus().GetNetwork().GetIp()
}

func TestStatusNetworkReadyReflectsConfiguration(t *testing.T) {
	// Default skeleton: no networking wired -> NetworkReady false.
	off := New(Options{})
	resp, err := off.Status(context.Background(), &runtimeapi.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := networkReadyCond(resp); got {
		t.Errorf("NetworkReady = true with no network configured, want false")
	}

	// Both IPAM and PodNetwork wired -> NetworkReady true.
	on, _, _ := newNetworkedServer(t, newFakeRuntime())
	resp, err = on.Status(context.Background(), &runtimeapi.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := networkReadyCond(resp); !got {
		t.Errorf("NetworkReady = false with network configured, want true")
	}
}

// TestStatusNetworkNotReadyWhenHalfConfigured proves NetworkReady is honest: a Pod
// IP allocator with no host path (or vice versa) cannot produce a reachable Pod,
// so it must not report ready.
func TestStatusNetworkNotReadyWhenHalfConfigured(t *testing.T) {
	ipam, _ := network.NewPodIPAM(testPodCIDR)
	s := New(Options{Runtime: newFakeRuntime(), IPAM: ipam}) // no PodNetwork
	resp, err := s.Status(context.Background(), &runtimeapi.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if networkReadyCond(resp) {
		t.Errorf("NetworkReady = true with IPAM but no PodNetwork, want false")
	}
}

func networkReadyCond(resp *runtimeapi.StatusResponse) bool {
	for _, c := range resp.GetStatus().GetConditions() {
		if c.GetType() == runtimeapi.NetworkReady {
			return c.GetStatus()
		}
	}
	return false
}

// TestSandboxNetworkLifecycleSuccess is the end-to-end happy path: the sandbox
// reserves a Pod IP, the IP is withheld from status until the container starts and
// the network attaches, and the attached endpoint maps the real Pod IP to the VM IP.
func TestSandboxNetworkLifecycleSuccess(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.7" // host-only VM address observed after Start
	s, ipam, pnet := newNetworkedServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"

	wantIP := ipam.IP(key)
	if wantIP == "" {
		t.Fatalf("RunPodSandbox did not reserve a Pod IP")
	}
	// Before the container starts, the Pod IP is reserved but not yet attached, so
	// status must withhold it.
	if got := sandboxNetworkIP(t, s, sandboxID); got != "" {
		t.Errorf("PodSandboxStatus.Network.Ip = %q before attach, want empty", got)
	}

	id := startContainerInSandbox(t, s, sandboxID)

	if got := sandboxNetworkIP(t, s, sandboxID); got != wantIP {
		t.Errorf("PodSandboxStatus.Network.Ip = %q after attach, want %q", got, wantIP)
	}
	ep, ok := pnet.isAttached(key)
	if !ok {
		t.Fatalf("pod %q was not attached to the network", key)
	}
	if ep.PodIP != wantIP || ep.VMIP != "192.168.64.7" {
		t.Errorf("attached endpoint = {PodIP:%q VMIP:%q}, want {PodIP:%q VMIP:192.168.64.7}", ep.PodIP, ep.VMIP, wantIP)
	}

	// The container is Running, not unwound.
	cst, _ := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if cst.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_RUNNING {
		t.Errorf("container state = %v, want RUNNING", cst.GetStatus().GetState())
	}
}

// TestStartContainerAttachFailureCleansUp proves a failed attach unwinds the start:
// the workload is stopped, the container is Exited with a clear reason, and the
// sandbox is not reported as networked.
func TestStartContainerAttachFailureCleansUp(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.8"
	s, _, pnet := newNetworkedServer(t, rt)
	pnet.attachErr = errors.New("pf load failed")
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	cResp, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()

	_, err = s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id})
	if status.Code(err) != codes.Internal {
		t.Fatalf("StartContainer error = %v, want Internal", err)
	}

	cst, _ := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if cst.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_EXITED {
		t.Errorf("container state = %v, want EXITED after failed attach", cst.GetStatus().GetState())
	}
	if reason := cst.GetStatus().GetReason(); reason != "NetworkSetupFailed" {
		t.Errorf("container reason = %q, want NetworkSetupFailed", reason)
	}
	workloadID := store.DeriveWorkloadID(id)
	if len(rt.stopped) == 0 || rt.stopped[len(rt.stopped)-1] != workloadID {
		t.Errorf("workload %q was not stopped during cleanup; stopped=%v", workloadID, rt.stopped)
	}
	if got := sandboxNetworkIP(t, s, sandboxID); got != "" {
		t.Errorf("PodSandboxStatus.Network.Ip = %q after failed attach, want empty", got)
	}
}

// TestStartContainerMissingVMIPUnwinds proves an absent VM address (DHCP not yet
// acquired) surfaces as Unavailable and unwinds rather than attaching a bogus Pod.
func TestStartContainerMissingVMIPUnwinds(t *testing.T) {
	rt := newFakeRuntime() // startIP unset -> Status reports no IP
	s, _, pnet := newNetworkedServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	cResp, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()

	_, err = s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("StartContainer error = %v, want Unavailable", err)
	}
	if pnet.attachCalls != 0 {
		t.Errorf("Attach called %d times with no VM IP, want 0", pnet.attachCalls)
	}
	cst, _ := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if cst.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_EXITED {
		t.Errorf("container state = %v, want EXITED after missing VM IP", cst.GetStatus().GetState())
	}
}

// TestStopPodSandboxDetachesIdempotently proves StopPodSandbox reclaims the network
// path, retains the Pod IP reservation, and is safe to call repeatedly.
func TestStopPodSandboxDetachesIdempotently(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.9"
	s, ipam, pnet := newNetworkedServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"
	startContainerInSandbox(t, s, sandboxID)
	reservedIP := ipam.IP(key)

	for i := 0; i < 2; i++ {
		if _, err := s.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
			t.Fatalf("StopPodSandbox #%d: %v", i+1, err)
		}
	}
	if _, ok := pnet.isAttached(key); ok {
		t.Errorf("pod %q still attached after stop", key)
	}
	// The Pod IP survives a stop so a restart of the sandbox keeps the same address.
	if ipam.IP(key) != reservedIP || reservedIP == "" {
		t.Errorf("Pod IP for %q = %q after stop, want retained %q", key, ipam.IP(key), reservedIP)
	}
	if got := sandboxNetworkIP(t, s, sandboxID); got != "" {
		t.Errorf("PodSandboxStatus.Network.Ip = %q after stop, want empty", got)
	}
}

// TestStopContainerDetachesNetwork proves a direct CRI StopContainer tears down
// the Pod network path. Kubelet can stop a container while the sandbox record
// remains, so the adapter must not keep reporting or routing the Pod IP after the
// backing micro-VM is stopped.
func TestStopContainerDetachesNetwork(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.13"
	s, ipam, pnet := newNetworkedServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"
	id := startContainerInSandbox(t, s, sandboxID)
	reservedIP := ipam.IP(key)
	if reservedIP == "" || sandboxNetworkIP(t, s, sandboxID) != reservedIP {
		t.Fatalf("expected attached Pod IP %q before StopContainer", reservedIP)
	}

	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	if _, ok := pnet.isAttached(key); ok {
		t.Errorf("pod %q still attached after StopContainer", key)
	}
	if got := sandboxNetworkIP(t, s, sandboxID); got != "" {
		t.Errorf("PodSandboxStatus.Network.Ip = %q after StopContainer, want empty", got)
	}
	if got := ipam.IP(key); got != reservedIP {
		t.Errorf("Pod IP reservation = %q after StopContainer, want retained %q", got, reservedIP)
	}
}

// TestRemoveContainerDetachesNetwork proves removing the single container also
// tears down the Pod network path while keeping the sandbox's IP reservation for
// the later RemovePodSandbox call.
func TestRemoveContainerDetachesNetwork(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.14"
	s, ipam, pnet := newNetworkedServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"
	id := startContainerInSandbox(t, s, sandboxID)
	reservedIP := ipam.IP(key)

	if _, err := s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if _, ok := pnet.isAttached(key); ok {
		t.Errorf("pod %q still attached after RemoveContainer", key)
	}
	if got := sandboxNetworkIP(t, s, sandboxID); got != "" {
		t.Errorf("PodSandboxStatus.Network.Ip = %q after RemoveContainer, want empty", got)
	}
	if got := ipam.IP(key); got != reservedIP {
		t.Errorf("Pod IP reservation = %q after RemoveContainer, want retained %q", got, reservedIP)
	}
}

// TestContainerStatusSelfExitDetachesNetwork covers the case where the workload
// exits without an explicit StopContainer call. Status reconciliation must clear
// the network path as soon as it observes the terminal runtime state.
func TestContainerStatusSelfExitDetachesNetwork(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.15"
	s, _, pnet := newNetworkedServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"
	id := startContainerInSandbox(t, s, sandboxID)
	rt.statusOverride[store.DeriveWorkloadID(id)] = runtime.Status{Phase: runtime.PhaseStopped, ExitCode: 0}

	st, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_EXITED {
		t.Fatalf("container state = %v, want EXITED", st.GetStatus().GetState())
	}
	if _, ok := pnet.isAttached(key); ok {
		t.Errorf("pod %q still attached after self-exit reconcile", key)
	}
	if got := sandboxNetworkIP(t, s, sandboxID); got != "" {
		t.Errorf("PodSandboxStatus.Network.Ip = %q after self-exit, want empty", got)
	}
}

// TestRemovePodSandboxReleasesIPIdempotently proves removal tears down the network
// path, releases the Pod IP, and is safe to call repeatedly.
func TestRemovePodSandboxReleasesIPIdempotently(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.10"
	s, ipam, pnet := newNetworkedServer(t, rt)
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	key := "default/web"
	startContainerInSandbox(t, s, sandboxID)
	if ipam.IP(key) == "" {
		t.Fatalf("expected a reserved Pod IP before removal")
	}

	for i := 0; i < 2; i++ {
		if _, err := s.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
			t.Fatalf("RemovePodSandbox #%d: %v", i+1, err)
		}
	}
	if ipam.IP(key) != "" {
		t.Errorf("Pod IP for %q = %q after remove, want released", key, ipam.IP(key))
	}
	if _, ok := pnet.isAttached(key); ok {
		t.Errorf("pod %q still attached after remove", key)
	}
}

// TestRecoverNetworkReservesAndReattaches proves a restarted adapter rebuilds Pod
// IP reservations and re-attaches surviving sandboxes from persisted state, so it
// neither leaks addresses nor wipes other Pods' host rules.
func TestRecoverNetworkReservesAndReattaches(t *testing.T) {
	dir := t.TempDir()

	// First adapter incarnation: run a sandbox + container with networking, leaving
	// persisted records on disk.
	sandboxes1, _, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	containers1, _, err := store.NewContainerStore(dir + "/containers")
	if err != nil {
		t.Fatalf("NewContainerStore: %v", err)
	}
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.11"
	ipam1, _ := network.NewPodIPAM(testPodCIDR)
	s1 := New(Options{Runtime: rt, IPAM: ipam1, PodNetwork: newFakePodNet(), Sandboxes: sandboxes1, Containers: containers1})
	s1.vmIPPollAttempts = 3
	s1.vmIPPollInterval = time.Millisecond
	sandboxID := runSandbox(t, s1, "web", "default", "uid-web")
	startContainerInSandbox(t, s1, sandboxID)
	key := "default/web"
	wantIP := ipam1.IP(key)
	if wantIP == "" {
		t.Fatalf("expected a reserved Pod IP before restart")
	}

	// Second incarnation: fresh stores load from disk, fresh IPAM and Pod network
	// have empty in-memory state until recovery.
	sandboxes2, _, err := store.New(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	containers2, _, err := store.NewContainerStore(dir + "/containers")
	if err != nil {
		t.Fatalf("reopen container store: %v", err)
	}
	ipam2, _ := network.NewPodIPAM(testPodCIDR)
	pnet2 := newFakePodNet()
	s2 := New(Options{Runtime: rt, IPAM: ipam2, PodNetwork: pnet2, Sandboxes: sandboxes2, Containers: containers2})

	if ipam2.IP(key) != "" {
		t.Fatalf("fresh IPAM already holds %q before recovery", key)
	}
	s2.RecoverNetwork(context.Background())

	if got := ipam2.IP(key); got != wantIP {
		t.Errorf("recovered Pod IP for %q = %q, want %q", key, got, wantIP)
	}
	ep, ok := pnet2.isAttached(key)
	if !ok {
		t.Fatalf("pod %q not re-attached after recovery", key)
	}
	if ep.PodIP != wantIP || ep.VMIP != "192.168.64.11" {
		t.Errorf("re-attached endpoint = {PodIP:%q VMIP:%q}, want {PodIP:%q VMIP:192.168.64.11}", ep.PodIP, ep.VMIP, wantIP)
	}
	// Status still reports the Pod IP after recovery.
	if got := sandboxNetworkIP(t, s2, sandboxID); got != wantIP {
		t.Errorf("PodSandboxStatus.Network.Ip after recovery = %q, want %q", got, wantIP)
	}
}

// TestNetworkDisabledRunsSandboxesWithoutPodIP proves the default skeleton path is
// unchanged: with no IPAM/PodNetwork, sandboxes have no Pod IP and report none.
func TestNetworkDisabledRunsSandboxesWithoutPodIP(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.12"
	s := New(Options{Runtime: rt})
	ctx := context.Background()

	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	startContainerInSandbox(t, s, sandboxID)

	if got := sandboxNetworkIP(t, s, sandboxID); got != "" {
		t.Errorf("PodSandboxStatus.Network.Ip = %q with networking off, want empty", got)
	}
	// Stop/remove must not error when networking is off.
	if _, err := s.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	if _, err := s.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
}
