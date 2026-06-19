package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/health"
	corev1 "k8s.io/api/core/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

// readyNode is a node whose Ready condition is True.
func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
			},
		},
	}
}

func freshLease(name string) *coordinationv1.Lease {
	now := metav1.NewMicroTime(time.Now())
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: corev1.NamespaceNodeLease},
		Spec:       coordinationv1.LeaseSpec{RenewTime: &now},
	}
}

func TestControlPlaneProbeNodeState(t *testing.T) {
	cs := kubernetesfake.NewSimpleClientset(readyNode("mac-a"))
	probe := controlPlaneProbe{clientset: cs, nodeName: "mac-a"}

	st, err := probe.NodeState(context.Background())
	if err != nil {
		t.Fatalf("NodeState: %v", err)
	}
	if !st.Registered || !st.Ready {
		t.Fatalf("expected registered+ready, got %+v", st)
	}

	// A node that is absent reports unregistered, not an error.
	missing := controlPlaneProbe{clientset: kubernetesfake.NewSimpleClientset(), nodeName: "ghost"}
	st, err = missing.NodeState(context.Background())
	if err != nil {
		t.Fatalf("missing node should not error: %v", err)
	}
	if st.Registered {
		t.Fatalf("missing node should be unregistered, got %+v", st)
	}
}

func TestControlPlaneProbeLeaseState(t *testing.T) {
	cs := kubernetesfake.NewSimpleClientset(freshLease("mac-a"))
	probe := controlPlaneProbe{clientset: cs, nodeName: "mac-a", leaseEnabled: true, leaseDuration: 40 * time.Second}
	st, err := probe.LeaseState(context.Background())
	if err != nil {
		t.Fatalf("LeaseState: %v", err)
	}
	if !st.Enabled || !st.Found || st.Stale != 40*time.Second {
		t.Fatalf("unexpected lease state: %+v", st)
	}
	if st.Age > 5*time.Second {
		t.Fatalf("fresh lease age too high: %s", st.Age)
	}

	// Disabled leases short-circuit without an API call.
	disabled := controlPlaneProbe{leaseEnabled: false}
	st, _ = disabled.LeaseState(context.Background())
	if st.Enabled {
		t.Fatalf("disabled lease should report not enabled, got %+v", st)
	}
}

// stubChecker lets the collector ServeHTTP test drive a fixed verdict.
type stubChecker struct{ c health.Check }

func (s stubChecker) Check(context.Context) health.Check { return s.c }

func TestDiagnosticsServeHTTPReadyVsNotReady(t *testing.T) {
	ready := &diagnosticsCollector{node: "mac-a", checkers: []health.Checker{
		stubChecker{health.Check{Name: "container-runtime", Class: health.ClassRuntime, Status: health.StatusPass}},
	}}
	notReady := &diagnosticsCollector{node: "mac-a", checkers: []health.Checker{
		stubChecker{health.Check{Name: "container-runtime", Class: health.ClassRuntime, Status: health.StatusFail}},
	}}

	// Text response: ready -> 200, not ready -> 503.
	rec := httptest.NewRecorder()
	ready.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz/diagnostics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready node code=%d want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	notReady.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz/diagnostics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready node code=%d want 503", rec.Code)
	}

	// JSON response decodes into a Report.
	rec = httptest.NewRecorder()
	notReady.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz/diagnostics?format=json", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("json content-type=%q", ct)
	}
	var report health.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode json report: %v", err)
	}
	if report.Ready {
		t.Fatal("expected not ready in json report")
	}
}

func TestForwardingProbeReadsSysctl(t *testing.T) {
	// On the macOS test host `sysctl net.inet.ip.forwarding` exists and must read
	// cleanly. The boolean value is host-dependent, so assert only that the probe
	// completes without error here.
	if _, err := (forwardingProbe{}).IPForwardingEnabled(context.Background()); err != nil {
		t.Fatalf("forwarding probe failed on host: %v", err)
	}
}
