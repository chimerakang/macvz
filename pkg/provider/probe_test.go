package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// newProbeProvider builds a provider whose probe loops run on a short time unit
// so tests converge in milliseconds, with a runtime that reports a Pod IP (so a
// probe-gated Pod can still become a real endpoint).
func newProbeProvider() (*Provider, *recordingRuntime) {
	rt := newRecordingRuntime()
	rt.runningIP = "10.244.0.7"
	p := New("mac-01", rt)
	p.probeUnit = 10 * time.Millisecond
	return p, rt
}

func (r *recordingRuntime) setExecErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execErr = err
}

// probePod is a single-container Pod with the given probes attached, defaulting
// to restartPolicy Never (overridden where the restart loop is exercised).
func probePod(startup, readiness, liveness *corev1.Probe) *corev1.Pod {
	pod := testPod("web")
	pod.Spec.Containers[0].StartupProbe = startup
	pod.Spec.Containers[0].ReadinessProbe = readiness
	pod.Spec.Containers[0].LivenessProbe = liveness
	return pod
}

func execProbe(period, failure, success int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
		PeriodSeconds:    period,
		FailureThreshold: failure,
		SuccessThreshold: success,
	}
}

// waitForCondition polls GetPodStatus until cond holds or the deadline passes.
func waitForCondition(t *testing.T, p *Provider, ns, name string, cond func(*corev1.PodStatus) bool) *corev1.PodStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last *corev1.PodStatus
	for {
		st, err := p.GetPodStatus(context.Background(), ns, name)
		if err != nil {
			t.Fatalf("GetPodStatus: %v", err)
		}
		last = st
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; last status phase=%q conditions=%+v containers=%+v", last.Phase, last.Conditions, last.ContainerStatuses)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func containerReady(st *corev1.PodStatus) bool {
	return len(st.ContainerStatuses) == 1 && st.ContainerStatuses[0].Ready
}

func podReadyCondition(st *corev1.PodStatus) corev1.ConditionStatus {
	for _, c := range st.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status
		}
	}
	return corev1.ConditionUnknown
}

func TestReadinessProbeGatesReady(t *testing.T) {
	p, rt := newProbeProvider()
	ctx := context.Background()

	// Readiness probe fails initially, so the Pod runs but is not a ready endpoint.
	rt.setExecErr(fmt.Errorf("not ready yet"))
	if err := p.CreatePod(ctx, probePod(nil, execProbe(1, 1, 1), nil)); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	// Give the prober a few cycles, then assert it is Running but not Ready.
	time.Sleep(60 * time.Millisecond)
	st, _ := p.GetPodStatus(ctx, "default", "p1")
	if st.Phase != corev1.PodRunning {
		t.Fatalf("phase = %q, want Running", st.Phase)
	}
	if containerReady(st) {
		t.Fatal("container reported Ready before the readiness probe passed")
	}
	if got := podReadyCondition(st); got != corev1.ConditionFalse {
		t.Errorf("PodReady = %q, want False before readiness", got)
	}

	// The probe starts succeeding; the container must become Ready.
	rt.setExecErr(nil)
	st = waitForCondition(t, p, "default", "p1", containerReady)
	if got := podReadyCondition(st); got != corev1.ConditionTrue {
		t.Errorf("PodReady = %q, want True once readiness passes", got)
	}

	// And it must drop back out of readiness when the probe fails again.
	rt.setExecErr(fmt.Errorf("degraded"))
	waitForCondition(t, p, "default", "p1", func(s *corev1.PodStatus) bool { return !containerReady(s) })
}

func TestNoReadinessProbeReadyWhenRunning(t *testing.T) {
	p, _ := newProbeProvider()
	ctx := context.Background()
	// Only a liveness probe: with no readiness probe, a running Pod with an IP is
	// Ready immediately (pre-probe behavior preserved).
	if err := p.CreatePod(ctx, probePod(nil, nil, execProbe(1, 3, 1))); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	st, _ := p.GetPodStatus(ctx, "default", "p1")
	if !containerReady(st) {
		t.Errorf("container with only a liveness probe should be Ready when running, got %+v", st.ContainerStatuses)
	}
}

func TestStartupProbeGatesReadiness(t *testing.T) {
	p, rt := newProbeProvider()
	ctx := context.Background()

	// Startup probe fails for a while (high failure threshold so it does not kill
	// the workload), and there is no readiness probe.
	startup := execProbe(1, 100, 1)
	rt.setExecErr(fmt.Errorf("still starting"))
	if err := p.CreatePod(ctx, probePod(startup, nil, nil)); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	time.Sleep(60 * time.Millisecond)
	st, _ := p.GetPodStatus(ctx, "default", "p1")
	if cs := st.ContainerStatuses[0]; cs.Ready || (cs.Started != nil && *cs.Started) {
		t.Errorf("container should be neither Started nor Ready before startup probe passes, got started=%v ready=%v", cs.Started, cs.Ready)
	}
	if got := podReadyCondition(st); got != corev1.ConditionFalse {
		t.Errorf("PodReady = %q, want False before startup completes", got)
	}

	// Startup succeeds: the container becomes Started and (no readiness probe) Ready.
	rt.setExecErr(nil)
	st = waitForCondition(t, p, "default", "p1", containerReady)
	if cs := st.ContainerStatuses[0]; cs.Started == nil || !*cs.Started {
		t.Errorf("container should be Started after startup probe passes, got %+v", cs.Started)
	}
}

func TestLivenessProbeRestartsWorkload(t *testing.T) {
	p, rt := newProbeProvider()
	p.restartBackoffBase = 0 // restart immediately for a deterministic test
	ctx := context.Background()

	// Always policy + a failing liveness probe must rebuild the micro-VM.
	rt.setExecErr(fmt.Errorf("dead"))
	pod := probePod(nil, nil, execProbe(1, 1, 1))
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	waitForCondition(t, p, "default", "p1", func(s *corev1.PodStatus) bool {
		return len(s.ContainerStatuses) == 1 && s.ContainerStatuses[0].RestartCount >= 1
	})
	// Stop the flapping so the test ends in a stable state.
	rt.setExecErr(nil)
	if _, creates, _, _, destroys := rt.counts(); creates < 2 || destroys < 1 {
		t.Errorf("creates=%d destroys=%d, want a fresh micro-VM rebuilt by liveness restart", creates, destroys)
	}
}

func TestLivenessProbeNeverPolicyFailsPod(t *testing.T) {
	p, rt := newProbeProvider()
	ctx := context.Background()
	// Never policy: a liveness failure kills the workload and the Pod becomes
	// Failed (it is not recreated).
	rt.setExecErr(fmt.Errorf("dead"))
	if err := p.CreatePod(ctx, probePod(nil, nil, execProbe(1, 1, 1))); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	waitForCondition(t, p, "default", "p1", func(s *corev1.PodStatus) bool {
		return s.Phase == corev1.PodFailed
	})
}

func TestHTTPGetProbeReadiness(t *testing.T) {
	p, _ := newProbeProvider()
	ctx := context.Background()

	var healthy bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	readiness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Host: host,
			Port: intstr.FromInt(port),
			Path: "/healthz",
		}},
		PeriodSeconds:    1,
		TimeoutSeconds:   5,
		FailureThreshold: 1,
		SuccessThreshold: 1,
	}
	if err := p.CreatePod(ctx, probePod(nil, readiness, nil)); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	time.Sleep(60 * time.Millisecond)
	st, _ := p.GetPodStatus(ctx, "default", "p1")
	if containerReady(st) {
		t.Fatal("container Ready while HTTP probe returns 503")
	}

	healthy = true
	waitForCondition(t, p, "default", "p1", containerReady)
}

func TestUnsupportedStartupProbeDoesNotBlock(t *testing.T) {
	p, _ := newProbeProvider()
	ctx := context.Background()
	// A gRPC startup probe has no MacVz handler; it must not pin the container as
	// forever-unstarted. The container ends up Started and (no readiness) Ready.
	grpc := &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 9000}},
		PeriodSeconds: 1,
	}
	if err := p.CreatePod(ctx, probePod(grpc, nil, nil)); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	waitForCondition(t, p, "default", "p1", containerReady)
}

func TestResolvePortNamed(t *testing.T) {
	c := corev1.Container{Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}}
	if got := resolvePort(intstr.FromString("http"), c); got != 8080 {
		t.Errorf("named port resolved to %d, want 8080", got)
	}
	if got := resolvePort(intstr.FromInt(443), c); got != 443 {
		t.Errorf("numeric port resolved to %d, want 443", got)
	}
	if got := resolvePort(intstr.FromString("missing"), c); got != 0 {
		t.Errorf("unknown named port resolved to %d, want 0", got)
	}
}

func TestDeletePodStopsProbers(t *testing.T) {
	p, rt := newProbeProvider()
	ctx := context.Background()
	rt.setExecErr(fmt.Errorf("dead"))
	pod := probePod(nil, nil, execProbe(1, 1, 1))
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if err := p.DeletePod(ctx, pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	// After deletion the probers are cancelled: no further runtime activity should
	// accrue from probe-driven restarts.
	_, createsAfterDelete, _, _, _ := rt.counts()
	time.Sleep(60 * time.Millisecond)
	if _, creates, _, _, _ := rt.counts(); creates != createsAfterDelete {
		t.Errorf("probers kept running after DeletePod: creates went %d -> %d", createsAfterDelete, creates)
	}
}
