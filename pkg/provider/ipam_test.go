package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newIPAMProvider(t *testing.T, cidr string) (*Provider, *recordingRuntime, *network.PodIPAM) {
	t.Helper()
	ipam, err := network.NewPodIPAM(cidr)
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	rt := newRecordingRuntime()
	return New("mac-01", rt, WithIPAM(ipam)), rt, ipam
}

func TestCreatePodAssignsStablePodIP(t *testing.T) {
	p, _, _ := newIPAMProvider(t, "10.244.1.0/24")
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	got, err := p.GetPod(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if got.Status.PodIP != "10.244.1.2" {
		t.Errorf("PodIP = %q, want 10.244.1.2", got.Status.PodIP)
	}
	// The IP must survive status reconciliation (it is not overwritten by the
	// runtime-reported address, which the fake leaves empty).
	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.PodIP != "10.244.1.2" {
		t.Errorf("reconciled PodIP = %q, want 10.244.1.2", st.PodIP)
	}
}

func TestDeletePodReleasesPodIP(t *testing.T) {
	p, _, ipam := newIPAMProvider(t, "10.244.1.0/24")
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if got := len(ipam.Allocations()); got != 1 {
		t.Fatalf("allocations after create = %d, want 1", got)
	}
	if err := p.DeletePod(ctx, testPod("web")); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if got := len(ipam.Allocations()); got != 0 {
		t.Errorf("allocations after delete = %d, want 0 (IP leaked)", got)
	}
}

func TestCreatePodReleasesIPOnFailure(t *testing.T) {
	p, rt, ipam := newIPAMProvider(t, "10.244.1.0/24")
	rt.startErr = fmt.Errorf("boom")
	if err := p.CreatePod(context.Background(), testPod("web")); err == nil {
		t.Fatal("expected CreatePod to fail when Start fails")
	}
	if got := len(ipam.Allocations()); got != 0 {
		t.Errorf("failed CreatePod leaked %d IP allocations, want 0", got)
	}
}

func TestRecoverAllocationsReservesExistingPodIPs(t *testing.T) {
	p, _, ipam := newIPAMProvider(t, "10.244.1.0/24")
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "survivor"},
		Status:     corev1.PodStatus{PodIP: "10.244.1.5"},
	}
	noIP := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pending"},
	}
	p.RecoverAllocations([]*corev1.Pod{existing, noIP})

	if got := ipam.IP("default/survivor"); got != "10.244.1.5" {
		t.Errorf("recovered IP = %q, want 10.244.1.5", got)
	}
	if got := len(ipam.Allocations()); got != 1 {
		t.Errorf("allocations after recovery = %d, want 1", got)
	}

	// A new Pod created after recovery must not reuse the reserved IP.
	if err := p.CreatePod(context.Background(), testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	got, _ := p.GetPod(context.Background(), "default", "p1")
	if got.Status.PodIP == "10.244.1.5" {
		t.Errorf("new Pod reused recovered IP 10.244.1.5")
	}
}

// TestNoIPAMLeavesPodIPToRuntime confirms the provider still works without
// coordinated IPAM: the PodIP is then derived from the runtime-reported address.
func TestNoIPAMLeavesPodIPToRuntime(t *testing.T) {
	p, rt := newTestProvider() // no WithIPAM
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rt.setStatus("wl-1", runtime.Status{ID: "wl-1", Phase: runtime.PhaseRunning, IP: "172.16.0.9"})
	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.PodIP != "172.16.0.9" {
		t.Errorf("PodIP = %q, want runtime-reported 172.16.0.9", st.PodIP)
	}
}
