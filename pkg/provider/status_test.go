package provider

import (
	"context"
	"testing"

	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
)

func condition(st *corev1.PodStatus, t corev1.PodConditionType) (corev1.PodCondition, bool) {
	for _, c := range st.Conditions {
		if c.Type == t {
			return c, true
		}
	}
	return corev1.PodCondition{}, false
}

func newReadyProvider(t *testing.T) (*Provider, *recordingRuntime) {
	t.Helper()
	ipam, err := network.NewPodIPAM("10.244.1.0/24")
	if err != nil {
		t.Fatalf("NewPodIPAM: %v", err)
	}
	rt := newRecordingRuntime()
	return New("mac-01", rt, WithIPAM(ipam), WithHostIP("192.168.1.10")), rt
}

func TestStatusPopulatesPodIPsAndHostIP(t *testing.T) {
	p, _ := newReadyProvider(t)
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.PodIP != "10.244.1.2" {
		t.Errorf("PodIP = %q, want 10.244.1.2", st.PodIP)
	}
	// PodIPs is what the EndpointSlice controller reads.
	if len(st.PodIPs) != 1 || st.PodIPs[0].IP != "10.244.1.2" {
		t.Errorf("PodIPs = %v, want [10.244.1.2]", st.PodIPs)
	}
	if st.HostIP != "192.168.1.10" {
		t.Errorf("HostIP = %q, want 192.168.1.10", st.HostIP)
	}
	if len(st.HostIPs) != 1 || st.HostIPs[0].IP != "192.168.1.10" {
		t.Errorf("HostIPs = %v, want [192.168.1.10]", st.HostIPs)
	}
}

func TestRunningPodWithIPIsReady(t *testing.T) {
	p, _ := newReadyProvider(t)
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	for _, ct := range []corev1.PodConditionType{corev1.PodReady, corev1.ContainersReady} {
		c, ok := condition(st, ct)
		if !ok || c.Status != corev1.ConditionTrue {
			t.Errorf("%s = %v (ok=%v), want True", ct, c.Status, ok)
		}
	}
}

func TestRunningPodWithoutIPIsNotReady(t *testing.T) {
	// No IPAM and the fake runtime reports no address: a running Pod must NOT be
	// Ready, so EndpointSlices never include an unreachable Pod.
	p, _ := newTestProvider()
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.Phase != corev1.PodRunning {
		t.Fatalf("phase = %q, want Running", st.Phase)
	}
	if len(st.PodIPs) != 0 {
		t.Errorf("PodIPs = %v, want empty without an address", st.PodIPs)
	}
	ready, _ := condition(st, corev1.PodReady)
	if ready.Status != corev1.ConditionFalse {
		t.Errorf("Ready = %v, want False without a Pod IP", ready.Status)
	}
	if ready.Reason != "PodNetworkNotReady" {
		t.Errorf("Ready reason = %q, want PodNetworkNotReady", ready.Reason)
	}
}

func TestRuntimeErrorKeepsPodNotReady(t *testing.T) {
	p, rt := newReadyProvider(t)
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rt.statusErr = runtime.ErrNotReady
	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	ready, _ := condition(st, corev1.PodReady)
	if ready.Status != corev1.ConditionFalse || ready.Reason != "RuntimeStatusError" {
		t.Errorf("Ready = %v reason=%q, want False/RuntimeStatusError", ready.Status, ready.Reason)
	}
}
