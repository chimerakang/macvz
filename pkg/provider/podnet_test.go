package provider

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
)

// fakePodNet records attach/detach calls and can fail attaches.
type fakePodNet struct {
	mu        sync.Mutex
	attached  map[string]podnet.Endpoint
	detached  []string
	attachErr error
	attachCnt int
	detachCnt int
}

func newFakePodNet() *fakePodNet {
	return &fakePodNet{attached: map[string]podnet.Endpoint{}}
}

func (f *fakePodNet) Attach(_ context.Context, ep podnet.Endpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachCnt++
	if f.attachErr != nil {
		return f.attachErr
	}
	f.attached[ep.PodKey] = ep
	return nil
}

func (f *fakePodNet) Detach(_ context.Context, podKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detachCnt++
	delete(f.attached, podKey)
	f.detached = append(f.detached, podKey)
	return nil
}

func (f *fakePodNet) get(key string) (podnet.Endpoint, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ep, ok := f.attached[key]
	return ep, ok
}

func newPodNetProvider(t *testing.T) (*Provider, *recordingRuntime, *fakePodNet) {
	t.Helper()
	ipam, err := network.NewPodIPAM("10.244.1.0/24")
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	rt := newRecordingRuntime()
	rt.runningIP = "192.168.64.5" // the VM's host-only address
	pn := newFakePodNet()
	p := New("mac-01", rt, WithIPAM(ipam), WithPodNetwork(pn))
	return p, rt, pn
}

func TestCreatePodAttachesNetworkPath(t *testing.T) {
	p, _, pn := newPodNetProvider(t)
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	ep, ok := pn.get("default/p1")
	if !ok {
		t.Fatal("Pod was not attached to the network path")
	}
	if ep.PodIP != "10.244.1.2" || ep.VMIP != "192.168.64.5" {
		t.Errorf("attached endpoint = %+v, want podIP 10.244.1.2 / vmIP 192.168.64.5", ep)
	}
}

func TestDeletePodDetachesNetworkPath(t *testing.T) {
	p, _, pn := newPodNetProvider(t)
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if err := p.DeletePod(ctx, testPod("web")); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if _, ok := pn.get("default/p1"); ok {
		t.Error("Pod was not detached on delete")
	}
	if pn.detachCnt != 1 {
		t.Errorf("detach called %d times, want 1", pn.detachCnt)
	}
}

func TestCreatePodRollsBackAndReleasesIPWhenAttachFails(t *testing.T) {
	p, rt, pn := newPodNetProvider(t)
	pn.attachErr = fmt.Errorf("pfctl boom")
	if err := p.CreatePod(context.Background(), testPod("web")); err == nil {
		t.Fatal("expected CreatePod to fail when network attach fails")
	}
	// The workload must be rolled back and the IP released so a retry is clean.
	_, creates, _, _, destroys := rt.counts()
	if creates != 1 || destroys != 1 {
		t.Errorf("expected 1 create and 1 rollback destroy, got creates=%d destroys=%d", creates, destroys)
	}
	if p.ipam != nil {
		if got := len(p.ipam.Allocations()); got != 0 {
			t.Errorf("failed attach leaked %d IP allocations", got)
		}
	}
}

func TestCreatePodPreservesRecoveredIPWhenAdoptedAttachFails(t *testing.T) {
	p, rt, pn := newPodNetProvider(t)
	key := "default/p1"
	id := WorkloadID("default", "p1", "web")
	if err := p.ipam.Reserve(key, "10.244.1.5"); err != nil {
		t.Fatalf("reserve recovered IP: %v", err)
	}
	rt.seedWorkload(id, runtime.PhaseRunning, "192.168.64.5")
	pn.attachErr = fmt.Errorf("pfctl boom")

	if err := p.CreatePod(context.Background(), testPod("web")); err == nil {
		t.Fatal("expected CreatePod to fail when adopted network attach fails")
	}

	if got := p.ipam.IP(key); got != "10.244.1.5" {
		t.Fatalf("recovered PodIP reservation was released: got %q, want 10.244.1.5", got)
	}
	_, creates, _, _, destroys := rt.counts()
	if creates != 0 || destroys != 0 {
		t.Errorf("adopted workload should not be recreated or destroyed on attach failure: creates=%d destroys=%d", creates, destroys)
	}
}

func TestCreatePodFailsWhenVMIPNeverAppears(t *testing.T) {
	p, rt, pn := newPodNetProvider(t)
	rt.runningIP = "" // VM never reports an address
	// Keep the poll fast for the test by cancelling once the first probe is done.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.CreatePod(ctx, testPod("web")); err == nil {
		t.Fatal("expected CreatePod to fail when the VM IP never appears")
	}
	if pn.attachCnt != 0 {
		t.Errorf("attach should not be called without a VM IP, got %d calls", pn.attachCnt)
	}
}

func TestCreatePodWithoutPodNetworkSkipsAttach(t *testing.T) {
	// No WithPodNetwork: Pods still run, just without the network path.
	p, _ := newTestProvider()
	if err := p.CreatePod(context.Background(), testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	got, _ := p.GetPod(context.Background(), "default", "p1")
	if got.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
}
