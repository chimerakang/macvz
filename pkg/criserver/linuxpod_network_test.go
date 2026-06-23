package criserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// These tests cover the CRI-L3 Pod networking integration for LinuxPod-backed
// sandboxes (#128): Pod IP reservation, host pf/route attach keyed off the LinuxPod
// sandbox address, honest PodSandboxStatus/NetworkReady reporting, detach + IP
// release on stop/remove, restart recovery, and the four failure diagnostics
// (helper, IP reservation, address discovery, route/pf). They reuse the fakePodNet
// and testPodCIDR helpers from network_test.go and the in-process LinuxPod
// FakeBackend, so they are fully hermetic — no pf, no route, no Pod VM.

// newLinuxPodNetService builds a LinuxPod service with the given networking
// dependencies and a fast address-discovery poll so tests do not sleep.
func newLinuxPodNetService(t *testing.T, backend linuxpod.Backend, ipam PodIPAllocator, pnet PodNetwork) *LinuxPodService {
	t.Helper()
	svc, err := NewLinuxPodService(LinuxPodOptions{Backend: backend, IPAM: ipam, PodNetwork: pnet})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	svc.addrPollAttempts = 4
	svc.addrPollInterval = time.Millisecond
	return svc
}

// lpSeedSandbox persists a ready sandbox record (store-valid id) and, unless
// withBackendPod is false, creates a matching backend Pod VM, so a test can drive
// ensureSandboxNetwork directly with a known sandbox id (RunPodSandbox mints its
// own). It returns the sandbox id. Omitting the backend Pod VM exercises the
// helper-failure path (PodStatus returns ErrPodNotFound).
func lpSeedSandbox(t *testing.T, svc *LinuxPodService, backend *linuxpod.FakeBackend, name string, withBackendPod bool) string {
	t.Helper()
	id, err := store.NewID()
	if err != nil {
		t.Fatalf("store.NewID: %v", err)
	}
	if withBackendPod {
		if _, err := backend.CreatePod(context.Background(), linuxpod.PodSpec{ID: id}); err != nil {
			t.Fatalf("backend.CreatePod(%s): %v", id, err)
		}
	}
	sb := &store.Sandbox{ID: id, State: store.StateReady, CreatedAt: 1}
	sb.Metadata.Namespace = "default"
	sb.Metadata.Name = name
	sb.Metadata.UID = "uid-" + name
	if err := svc.sandboxes.Put(sb); err != nil {
		t.Fatalf("persist sandbox %s: %v", id, err)
	}
	return id
}

func TestLinuxPodNetworkAttachOnRunPodSandbox(t *testing.T) {
	ipam, err := network.NewPodIPAM(testPodCIDR)
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	pnet := newFakePodNet()
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodNetService(t, backend, ipam, pnet)
	ctx := context.Background()

	// NetworkReady is true once IPAM + host path are wired.
	st, err := svc.Status(ctx, &runtimeapi.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !networkReadyCond(st) {
		t.Fatal("NetworkReady should be true when Pod networking is wired")
	}

	sandboxID := lpRunSandbox(t, svc)
	key := "default/pod"

	// A Pod IP was reserved from the configured CIDR and the host path attached to
	// the LinuxPod sandbox address.
	podIP := ipam.IP(key)
	if podIP == "" {
		t.Fatal("RunPodSandbox did not reserve a Pod IP")
	}
	ep, ok := pnet.isAttached(key)
	if !ok {
		t.Fatalf("Pod %q was not attached to the host path", key)
	}
	if ep.PodIP != podIP {
		t.Errorf("attached PodIP = %q, want %q", ep.PodIP, podIP)
	}
	backendStatus, _ := backend.PodStatus(ctx, sandboxID)
	if ep.VMIP != backendStatus.SandboxAddress || ep.VMIP == "" {
		t.Errorf("attached VMIP = %q, want LinuxPod sandbox address %q", ep.VMIP, backendStatus.SandboxAddress)
	}

	// PodSandboxStatus reports the Pod IP only after the attach is recorded.
	sbStatus, err := svc.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID, Verbose: true})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	if got := sbStatus.GetStatus().GetNetwork().GetIp(); got != podIP {
		t.Errorf("PodSandboxStatus IP = %q, want %q", got, podIP)
	}
	if got := sbStatus.GetInfo()["networkAttached"]; got != "true" {
		t.Errorf("verbose networkAttached = %q, want true", got)
	}
	if got := sbStatus.GetInfo()["podIP"]; got != podIP {
		t.Errorf("verbose podIP = %q, want %q", got, podIP)
	}
	if got := sbStatus.GetInfo()["vmIP"]; got != ep.VMIP {
		t.Errorf("verbose vmIP = %q, want %q", got, ep.VMIP)
	}
	if got := sbStatus.GetInfo()["interface"]; got != ep.Interface {
		t.Errorf("verbose interface = %q, want %q", got, ep.Interface)
	}
}

func TestLinuxPodNetworkDisabledRunsWithoutPodIP(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend) // no PodNetwork/IPAM
	ctx := context.Background()

	st, _ := svc.Status(ctx, &runtimeapi.StatusRequest{})
	if networkReadyCond(st) {
		t.Error("NetworkReady should be false when Pod networking is off")
	}
	sandboxID := lpRunSandbox(t, svc)
	sbStatus, err := svc.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	if sbStatus.GetStatus().GetNetwork().GetIp() != "" {
		t.Error("a networking-off sandbox must not report a Pod IP")
	}
}

func TestLinuxPodNetworkAddressDiscoveryLatencyTolerated(t *testing.T) {
	ipam, _ := network.NewPodIPAM(testPodCIDR)
	pnet := newFakePodNet()
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodNetService(t, backend, ipam, pnet)
	ctx := context.Background()

	id := lpSeedSandbox(t, svc, backend, "late", true)
	// Withhold the address for the first two PodStatus polls; the third reveals it.
	backend.SandboxAddressReadyAfter[id] = 3

	if err := svc.ensureSandboxNetwork(ctx, id); err != nil {
		t.Fatalf("ensureSandboxNetwork should tolerate address-discovery latency: %v", err)
	}
	if _, ok := pnet.isAttached(sandboxKeyOf(svc, id)); !ok {
		t.Error("sandbox should be attached after the address became available")
	}
}

func TestLinuxPodNetworkFailureClasses(t *testing.T) {
	t.Run("address-discovery", func(t *testing.T) {
		ipam, _ := network.NewPodIPAM(testPodCIDR)
		backend := linuxpod.NewFakeBackend()
		svc := newLinuxPodNetService(t, backend, ipam, newFakePodNet())
		id := lpSeedSandbox(t, svc, backend, "never", true)
		backend.SandboxAddressReadyAfter[id] = 1000 // never within the budget
		assertLinuxPodNetClass(t, svc.ensureSandboxNetwork(context.Background(), id),
			LinuxPodNetAddressDiscovery, codes.Unavailable)
	})

	t.Run("helper", func(t *testing.T) {
		ipam, _ := network.NewPodIPAM(testPodCIDR)
		backend := linuxpod.NewFakeBackend()
		svc := newLinuxPodNetService(t, backend, ipam, newFakePodNet())
		// Seed a sandbox whose backend Pod VM does not exist, so PodStatus errors.
		id := lpSeedSandbox(t, svc, backend, "ghost", false)
		assertLinuxPodNetClass(t, svc.ensureSandboxNetwork(context.Background(), id),
			LinuxPodNetHelper, codes.Unavailable)
	})

	t.Run("ip-reservation", func(t *testing.T) {
		backend := linuxpod.NewFakeBackend()
		svc := newLinuxPodNetService(t, backend, failIPAM{cidr: testPodCIDR}, newFakePodNet())
		id := lpSeedSandbox(t, svc, backend, "noip", true)
		assertLinuxPodNetClass(t, svc.ensureSandboxNetwork(context.Background(), id),
			LinuxPodNetIPReservation, codes.ResourceExhausted)
	})

	t.Run("route-pf", func(t *testing.T) {
		ipam, _ := network.NewPodIPAM(testPodCIDR)
		pnet := newFakePodNet()
		pnet.attachErr = errors.New("pfctl: load failed")
		backend := linuxpod.NewFakeBackend()
		svc := newLinuxPodNetService(t, backend, ipam, pnet)
		id := lpSeedSandbox(t, svc, backend, "pf", true)
		err := svc.ensureSandboxNetwork(context.Background(), id)
		assertLinuxPodNetClass(t, err, LinuxPodNetRoutePF, codes.Internal)
		// The Pod IP reservation is retained for a retry even though the attach failed.
		if ipam.IP(sandboxKeyOf(svc, id)) == "" {
			t.Error("Pod IP reservation should be retained after a route/pf failure")
		}
	})
}

func TestLinuxPodNetworkDetachAndReleaseOnStopRemove(t *testing.T) {
	ipam, _ := network.NewPodIPAM(testPodCIDR)
	pnet := newFakePodNet()
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodNetService(t, backend, ipam, pnet)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	key := "default/pod"
	if _, ok := pnet.isAttached(key); !ok {
		t.Fatal("precondition: sandbox should be attached")
	}

	// StopPodSandbox detaches the host path but RETAINS the IP reservation.
	if _, err := svc.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	if _, ok := pnet.isAttached(key); ok {
		t.Error("StopPodSandbox should detach the host path")
	}
	if ipam.IP(key) == "" {
		t.Error("StopPodSandbox must retain the Pod IP reservation until remove")
	}

	// RemovePodSandbox releases the IP reservation.
	if _, err := svc.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if ipam.IP(key) != "" {
		t.Errorf("RemovePodSandbox must release the Pod IP reservation; still holds %q", ipam.IP(key))
	}
	// Idempotent: a second RemovePodSandbox does not error or leak.
	if _, err := svc.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Errorf("idempotent RemovePodSandbox: %v", err)
	}
}

func TestLinuxPodNetworkRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	sb1, _, _ := store.New(dir + "/sandboxes")
	cs1, _, _ := store.NewContainerStore(dir + "/containers")
	ipam1, _ := network.NewPodIPAM(testPodCIDR)
	pnet1 := newFakePodNet()
	backend := linuxpod.NewFakeBackend()
	svc1, err := NewLinuxPodService(LinuxPodOptions{
		Backend: backend, Sandboxes: sb1, Containers: cs1, PodNetwork: pnet1, IPAM: ipam1,
	})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	svc1.addrPollAttempts = 4
	svc1.addrPollInterval = time.Millisecond
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc1)
	key := "default/pod"
	wantIP := ipam1.IP(key)

	// Restart: reopen stores with a fresh IPAM (empty) and a fresh host path (empty
	// ruleset), then recover. The reservation must be rebuilt and the rule re-attached.
	sb2, _, _ := store.New(dir + "/sandboxes")
	cs2, _, _ := store.NewContainerStore(dir + "/containers")
	ipam2, _ := network.NewPodIPAM(testPodCIDR)
	pnet2 := newFakePodNet()
	svc2, err := NewLinuxPodService(LinuxPodOptions{
		Backend: backend, Sandboxes: sb2, Containers: cs2, PodNetwork: pnet2, IPAM: ipam2,
	})
	if err != nil {
		t.Fatalf("reopen NewLinuxPodService: %v", err)
	}
	if ipam2.IP(key) != "" {
		t.Fatal("precondition: fresh IPAM must start empty")
	}
	svc2.RecoverNetwork(ctx)

	if got := ipam2.IP(key); got != wantIP {
		t.Errorf("recovery re-reserved IP = %q, want %q (no leak, same address)", got, wantIP)
	}
	if _, ok := pnet2.isAttached(key); !ok {
		t.Error("recovery should re-attach the surviving sandbox's host rule")
	}
	_ = sandboxID
}

// --- test helpers ---

// failIPAM is a PodIPAllocator whose Allocate always fails, to exercise the
// IP-reservation failure class.
type failIPAM struct{ cidr string }

func (failIPAM) Allocate(string) (string, error) { return "", errors.New("pod IP pool exhausted") }
func (failIPAM) Reserve(string, string) error    { return nil }
func (failIPAM) Release(string)                  {}
func (failIPAM) IP(string) string                { return "" }
func (f failIPAM) CIDR() string                  { return f.cidr }

// sandboxKeyOf returns the IPAM/podnet key for a seeded sandbox id.
func sandboxKeyOf(svc *LinuxPodService, id string) string {
	sb, _ := svc.sandboxes.Get(id)
	return sandboxKey(&sb)
}

// assertLinuxPodNetClass asserts err is a *LinuxPodNetworkError of the wanted class
// and maps to the wanted gRPC code.
func assertLinuxPodNetClass(t *testing.T, err error, want LinuxPodNetworkFailure, wantCode codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s failure, got nil", want)
	}
	var lpErr *LinuxPodNetworkError
	if !errors.As(err, &lpErr) {
		t.Fatalf("error %v is not a *LinuxPodNetworkError", err)
	}
	if lpErr.Class != want {
		t.Errorf("failure class = %q, want %q", lpErr.Class, want)
	}
	if got := status.Code(err); got != wantCode {
		t.Errorf("gRPC code = %v, want %v", got, wantCode)
	}
}
