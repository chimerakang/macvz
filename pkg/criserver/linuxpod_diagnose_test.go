package criserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// These tests cover the CRI-L6-2 (#136) residual-state diagnostic: machine-readable
// classification of stale Ready, NotReady, missing-backend, and partially-removed
// (orphaned) LinuxPod CRI records, plus the read-only guarantee (the diagnostic
// never mutates records, IP reservations, or host routes) and Pod-IP stability
// across BackendLost recreation.

// sandboxCategory returns the diagnostic category for a sandbox id, failing if the
// report has no such sandbox.
func sandboxCategory(t *testing.T, rep LinuxPodResidualReport, id string) LinuxPodResidualCategory {
	t.Helper()
	for _, sb := range rep.Sandboxes {
		if sb.SandboxID == id {
			return sb.Category
		}
	}
	t.Fatalf("sandbox %s not in residual report", id)
	return ""
}

// containerCategory returns the diagnostic category for a container id.
func containerCategory(t *testing.T, rep LinuxPodResidualReport, id string) LinuxPodResidualCategory {
	t.Helper()
	for _, c := range rep.Containers {
		if c.ContainerID == id {
			return c.Category
		}
	}
	t.Fatalf("container %s not in residual report", id)
	return ""
}

func TestLinuxPodDiagnoseReadyBackendLive(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	containerID := lpCreateStart(t, svc, sandboxID, "app")

	rep := svc.Diagnose(ctx)
	if !rep.BackendProbed {
		t.Error("report should mark the backend probed when a backend is wired")
	}
	if got := sandboxCategory(t, rep, sandboxID); got != ResidualSandboxReadyBackendLive {
		t.Errorf("sandbox category = %q, want %q", got, ResidualSandboxReadyBackendLive)
	}
	if got := containerCategory(t, rep, containerID); got != ResidualContainerRunningBackendLive {
		t.Errorf("container category = %q, want %q", got, ResidualContainerRunningBackendLive)
	}
	if rep.Summary[ResidualSandboxReadyBackendLost] != 0 {
		t.Errorf("healthy state should report zero backend-lost sandboxes, got %d", rep.Summary[ResidualSandboxReadyBackendLost])
	}
}

func TestLinuxPodDiagnoseBackendLost(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	containerID := lpCreateStart(t, svc, sandboxID, "app")

	// Lose the helper backend without touching the persisted CRI records, modeling a
	// helper restart that dropped its live VM handles.
	if _, err := backend.Cleanup(ctx, sandboxID); err != nil {
		t.Fatalf("backend.Cleanup precondition: %v", err)
	}

	rep := svc.Diagnose(ctx)
	if got := sandboxCategory(t, rep, sandboxID); got != ResidualSandboxReadyBackendLost {
		t.Errorf("sandbox category = %q, want %q", got, ResidualSandboxReadyBackendLost)
	}
	if got := containerCategory(t, rep, containerID); got != ResidualContainerRunningBackendLost {
		t.Errorf("container category = %q, want %q", got, ResidualContainerRunningBackendLost)
	}

	// The diagnostic is read-only: it must NOT have reconciled the records to
	// NotReady/Exited the way the live reconciler would.
	if sb, _ := svc.sandboxes.Get(sandboxID); sb.State != store.StateReady {
		t.Errorf("diagnostic mutated sandbox state to %q; it must be read-only", sb.State)
	}
	if c, _ := svc.containers.Get(containerID); c.State != store.ContainerRunning {
		t.Errorf("diagnostic mutated container state to %q; it must be read-only", c.State)
	}
}

func TestLinuxPodDiagnoseNotReadyRetained(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	lpCreateStart(t, svc, sandboxID, "app")
	if _, err := svc.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}

	rep := svc.Diagnose(ctx)
	if got := sandboxCategory(t, rep, sandboxID); got != ResidualSandboxNotReady {
		t.Errorf("sandbox category = %q, want %q", got, ResidualSandboxNotReady)
	}
}

func TestLinuxPodDiagnoseOrphanedContainer(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	containerID := lpCreateStart(t, svc, sandboxID, "app")

	// Simulate a partially-removed sandbox: the sandbox record is gone but a
	// container record was left behind (a removal that crashed mid-way).
	if err := svc.sandboxes.Delete(sandboxID); err != nil {
		t.Fatalf("delete sandbox record: %v", err)
	}

	rep := svc.Diagnose(ctx)
	if got := containerCategory(t, rep, containerID); got != ResidualContainerOrphaned {
		t.Errorf("container category = %q, want %q", got, ResidualContainerOrphaned)
	}
}

func TestLinuxPodDiagnoseUnprobedWithoutBackend(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	containerID := lpCreateStart(t, svc, sandboxID, "app")

	// DiagnoseLinuxPodStores with a nil backend models the CLI run without a helper
	// socket: liveness cannot be probed, so it is reported honestly as unprobed.
	rep := DiagnoseLinuxPodStores(ctx, svc.sandboxes, svc.containers, nil)
	if rep.BackendProbed {
		t.Error("report should mark the backend unprobed when no backend is given")
	}
	if got := sandboxCategory(t, rep, sandboxID); got != ResidualSandboxReadyBackendUnprobed {
		t.Errorf("sandbox category = %q, want %q", got, ResidualSandboxReadyBackendUnprobed)
	}
	if got := containerCategory(t, rep, containerID); got != ResidualContainerRunningBackendUnprobed {
		t.Errorf("container category = %q, want %q", got, ResidualContainerRunningBackendUnprobed)
	}
}

func TestLinuxPodDiagnoseMaterializedMounts(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	// Seed a container record carrying one present and one absent materialized mount.
	id, err := store.NewID()
	if err != nil {
		t.Fatalf("store.NewID: %v", err)
	}
	present := filepath.Join(t.TempDir(), "present")
	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatalf("write present mount source: %v", err)
	}
	absent := filepath.Join(t.TempDir(), "absent")
	c := &store.Container{
		ID:        id,
		SandboxID: "sandbox-gone",
		State:     store.ContainerExited,
		Mounts: []store.Mount{
			{HostPath: present, ContainerPath: "/data", ReadOnly: true},
			{HostPath: absent, ContainerPath: "/cache"},
		},
	}
	c.Metadata.Name = "app"
	if err := svc.containers.Put(c); err != nil {
		t.Fatalf("persist container: %v", err)
	}

	rep := svc.Diagnose(ctx)
	var got *LinuxPodContainerResidual
	for i := range rep.Containers {
		if rep.Containers[i].ContainerID == id {
			got = &rep.Containers[i]
		}
	}
	if got == nil {
		t.Fatalf("container %s not in report", id)
	}
	if len(got.Mounts) != 2 {
		t.Fatalf("mounts reported = %d, want 2", len(got.Mounts))
	}
	for _, m := range got.Mounts {
		switch m.HostPath {
		case present:
			if !m.HostPathExists {
				t.Errorf("present mount source %q reported as missing", present)
			}
		case absent:
			if m.HostPathExists {
				t.Errorf("absent mount source %q reported as present", absent)
			}
		}
	}
}

// TestLinuxPodDiagnoseDoesNotTouchNetwork proves the diagnostic never detaches a
// Pod network path or releases an IP — it only describes residual network state,
// even for a backend-lost sandbox, leaving other Pods' host rules untouched.
func TestLinuxPodDiagnoseDoesNotTouchNetwork(t *testing.T) {
	ipam, err := network.NewPodIPAM(testPodCIDR)
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	pnet := newFakePodNet()
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodNetService(t, backend, ipam, pnet)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	key := "default/pod"
	wantIP := ipam.IP(key)
	if wantIP == "" {
		t.Fatal("precondition: sandbox should hold a Pod IP")
	}
	// Lose the backend so the sandbox is classified backend-lost.
	if _, err := backend.Cleanup(ctx, sandboxID); err != nil {
		t.Fatalf("backend.Cleanup precondition: %v", err)
	}

	rep := svc.Diagnose(ctx)
	if got := sandboxCategory(t, rep, sandboxID); got != ResidualSandboxReadyBackendLost {
		t.Fatalf("sandbox category = %q, want backend-lost", got)
	}
	// The host rule must still be attached and the IP still reserved: the diagnostic
	// changed no route and released no address.
	if _, ok := pnet.isAttached(key); !ok {
		t.Error("diagnostic detached the Pod host rule; it must be read-only")
	}
	if got := ipam.IP(key); got != wantIP {
		t.Errorf("diagnostic changed IP reservation to %q, want %q", got, wantIP)
	}
	for _, sb := range rep.Sandboxes {
		if sb.SandboxID == sandboxID {
			if !sb.Network.Attached || sb.Network.PodIP != wantIP || !sb.Network.IPReservedForKey {
				t.Errorf("network residual = %+v, want attached with IP %q reserved", sb.Network, wantIP)
			}
		}
	}
}

// TestLinuxPodDiagnoseReconcileDetachesOnlyLostSandbox proves that when one of two
// networked sandboxes loses its backend, reconciliation detaches only that
// sandbox's host rule and leaves the healthy sandbox's rule and IP intact — no
// unrelated host route is changed (issue AC: cleanup never touches unrelated
// routes).
func TestLinuxPodDiagnoseReconcileDetachesOnlyLostSandbox(t *testing.T) {
	ipam, err := network.NewPodIPAM(testPodCIDR)
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	pnet := newFakePodNet()
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodNetService(t, backend, ipam, pnet)
	ctx := context.Background()

	keep, err := svc.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{Metadata: &runtimeapi.PodSandboxMetadata{Name: "keep", Namespace: "default", Uid: "uid-keep"}},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox(keep): %v", err)
	}
	lose, err := svc.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{Metadata: &runtimeapi.PodSandboxMetadata{Name: "lose", Namespace: "default", Uid: "uid-lose"}},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox(lose): %v", err)
	}
	keepKey, loseKey := "default/keep", "default/lose"
	keepIP := ipam.IP(keepKey)

	// Lose only the second sandbox's backend, then reconcile.
	if _, err := backend.Cleanup(ctx, lose.GetPodSandboxId()); err != nil {
		t.Fatalf("backend.Cleanup precondition: %v", err)
	}
	svc.reconcileAllSandboxBackendState(ctx)

	// The kept sandbox is untouched: still Ready, attached, same IP.
	if sb, _ := svc.sandboxes.Get(keep.GetPodSandboxId()); sb.State != store.StateReady {
		t.Errorf("kept sandbox state = %q, want Ready", sb.State)
	}
	if _, ok := pnet.isAttached(keepKey); !ok {
		t.Error("reconcile detached the unrelated kept sandbox's host rule")
	}
	if got := ipam.IP(keepKey); got != keepIP {
		t.Errorf("reconcile changed kept sandbox IP to %q, want %q", got, keepIP)
	}
	// The lost sandbox was detached (its rule removed) and marked NotReady.
	if _, ok := pnet.isAttached(loseKey); ok {
		t.Error("reconcile should have detached the lost sandbox's host rule")
	}
	if sb, _ := svc.sandboxes.Get(lose.GetPodSandboxId()); sb.State != store.StateNotReady {
		t.Errorf("lost sandbox state = %q, want NotReady", sb.State)
	}
}

// TestLinuxPodReaperReleasesPodIP proves the NotReady reaper releases the Pod
// IP reservation when it discards a sandbox record: kubelet's eventual
// RemovePodSandbox misses the deleted record and skips its release, so unique
// pod names (Job churn) would otherwise exhaust the pool.
func TestLinuxPodReaperReleasesPodIP(t *testing.T) {
	ipam, err := network.NewPodIPAM(testPodCIDR)
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	pnet := newFakePodNet()
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodNetService(t, backend, ipam, pnet)
	ctx := context.Background()

	resp, err := svc.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{Metadata: &runtimeapi.PodSandboxMetadata{Name: "oneshot", Namespace: "default", Uid: "uid-1"}},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	key := "default/oneshot"
	if ipam.IP(key) == "" {
		t.Fatal("precondition: sandbox should hold a Pod IP")
	}
	if _, err := svc.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: resp.GetPodSandboxId()}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	// While the record survives (kubelet may still RemovePodSandbox), the
	// reservation is retained.
	svc.reconcileAllSandboxBackendState(ctx)
	if ipam.IP(key) == "" {
		t.Fatal("Pod IP released before the reap grace elapsed")
	}

	svc.now = func() time.Time { return time.Now().Add(linuxpodNotReadyReapGrace + time.Second) }
	svc.reconcileAllSandboxBackendState(ctx)
	if _, ok := svc.sandboxes.Get(resp.GetPodSandboxId()); ok {
		t.Fatal("aged NotReady sandbox was not reaped")
	}
	if got := ipam.IP(key); got != "" {
		t.Fatalf("reaper leaked Pod IP reservation %q for the discarded sandbox", got)
	}
}

// TestLinuxPodPodIPStableAcrossBackendLostRecreate proves a Pod keeps its IP when
// its sandbox is recreated after BackendLost: the reconciler marks the stale
// sandbox NotReady (retaining the reservation), and a same-Pod RunPodSandbox
// discards the stale record and reuses the same Pod IP keyed by namespace/name.
func TestLinuxPodPodIPStableAcrossBackendLostRecreate(t *testing.T) {
	ipam, err := network.NewPodIPAM(testPodCIDR)
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	pnet := newFakePodNet()
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodNetService(t, backend, ipam, pnet)
	ctx := context.Background()

	cfg := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
	}
	first, err := svc.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{Config: cfg})
	if err != nil {
		t.Fatalf("RunPodSandbox(first): %v", err)
	}
	key := "default/pod"
	wantIP := ipam.IP(key)
	if wantIP == "" {
		t.Fatal("precondition: first sandbox should hold a Pod IP")
	}

	// BackendLost: drop the backend and let the reconciler mark the sandbox NotReady.
	if _, err := backend.Cleanup(ctx, first.GetPodSandboxId()); err != nil {
		t.Fatalf("backend.Cleanup precondition: %v", err)
	}
	svc.reconcileAllSandboxBackendState(ctx)
	if got := ipam.IP(key); got != wantIP {
		t.Fatalf("BackendLost released the Pod IP (now %q, was %q); recreation could not keep it stable", got, wantIP)
	}

	// Recreate the same Pod (kubelet retry). The stale NotReady record is discarded
	// and the same IP is reused.
	cfg.Metadata.Uid = "uid-2" // a recreated Pod gets a new sandbox UID
	cfg.Metadata.Attempt = 1
	second, err := svc.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{Config: cfg})
	if err != nil {
		t.Fatalf("RunPodSandbox(recreate): %v", err)
	}
	if second.GetPodSandboxId() == first.GetPodSandboxId() {
		t.Fatal("recreate should mint a new sandbox id")
	}
	if got := ipam.IP(key); got != wantIP {
		t.Errorf("recreated Pod IP = %q, want stable %q", got, wantIP)
	}
	if _, ok := svc.sandboxes.Get(first.GetPodSandboxId()); ok {
		t.Error("stale NotReady sandbox record should be discarded on recreate")
	}
}
