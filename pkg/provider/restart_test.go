package provider

import (
	"context"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
)

func TestShouldRestart(t *testing.T) {
	cases := []struct {
		policy corev1.RestartPolicy
		exit   int
		want   bool
	}{
		{corev1.RestartPolicyAlways, 0, true},
		{corev1.RestartPolicyAlways, 1, true},
		{corev1.RestartPolicyOnFailure, 0, false},
		{corev1.RestartPolicyOnFailure, 7, true},
		{corev1.RestartPolicyNever, 0, false},
		{corev1.RestartPolicyNever, 1, false},
	}
	for _, c := range cases {
		if got := shouldRestart(c.policy, c.exit); got != c.want {
			t.Errorf("shouldRestart(%q, %d) = %v, want %v", c.policy, c.exit, got, c.want)
		}
	}
}

func TestRestartBackoffGrowsAndCaps(t *testing.T) {
	base := 10 * time.Second
	if d := restartBackoff(base, 0); d != base {
		t.Errorf("first backoff = %v, want %v", d, base)
	}
	if d := restartBackoff(base, 1); d != 20*time.Second {
		t.Errorf("second backoff = %v, want 20s", d)
	}
	if d := restartBackoff(base, 100); d != restartBackoffMax {
		t.Errorf("large backoff = %v, want cap %v", d, restartBackoffMax)
	}
}

// waitForRestartCount polls GetPodStatus until the single container reports the
// wanted RestartCount, or fails after the deadline. The restart runs in a
// background goroutine, so the test observes it through the status it would
// surface to Kubernetes rather than reaching into internals.
func waitForRestartCount(t *testing.T, p *Provider, ns, name string, want int32) *corev1.PodStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := p.GetPodStatus(context.Background(), ns, name)
		if err != nil {
			t.Fatalf("GetPodStatus: %v", err)
		}
		if len(st.ContainerStatuses) == 1 && st.ContainerStatuses[0].RestartCount == want &&
			st.ContainerStatuses[0].State.Running != nil {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for RestartCount %d; last status = %+v", want, st.ContainerStatuses)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRestartPolicyAlwaysRestartsExitedWorkload(t *testing.T) {
	p, rt := newTestProvider()
	p.restartBackoffBase = 0 // restart immediately for a deterministic test
	ctx := context.Background()

	if err := p.CreatePod(ctx, restartTestPod(corev1.RestartPolicyAlways, "web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	// The container exits cleanly. Under Always it must be recreated, not left
	// Succeeded.
	rt.setStatus("wl-1", runtime.Status{ID: "wl-1", Phase: runtime.PhaseStopped, ExitCode: 0})

	st := waitForRestartCount(t, p, "default", "p1", 1)
	if st.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running after restart", st.Phase)
	}
	// A fresh micro-VM means a new Create+Start beyond the original pair.
	_, creates, starts, _, destroys := rt.counts()
	if creates < 2 || starts < 2 {
		t.Errorf("creates=%d starts=%d, want >=2 each (original + restart)", creates, starts)
	}
	if destroys < 1 {
		t.Errorf("destroys=%d, want >=1 (the exited workload is reaped)", destroys)
	}
}

func TestRestartPolicyOnFailureSkipsCleanExit(t *testing.T) {
	p, rt := newTestProvider()
	p.restartBackoffBase = 0
	ctx := context.Background()

	if err := p.CreatePod(ctx, restartTestPod(corev1.RestartPolicyOnFailure, "web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	// A clean exit (0) under OnFailure is terminal: the Pod Succeeds, no restart.
	rt.setStatus("wl-1", runtime.Status{ID: "wl-1", Phase: runtime.PhaseStopped, ExitCode: 0})
	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.Phase != corev1.PodSucceeded {
		t.Errorf("phase = %q, want Succeeded (no restart on clean exit)", st.Phase)
	}
	// Give any erroneously-scheduled restart a chance to run, then confirm none did.
	time.Sleep(20 * time.Millisecond)
	if _, creates, _, _, _ := rt.counts(); creates != 1 {
		t.Errorf("creates=%d, want 1 (OnFailure must not restart a clean exit)", creates)
	}
}

func TestRestartPolicyOnFailureRestartsFailedExit(t *testing.T) {
	p, rt := newTestProvider()
	p.restartBackoffBase = 0
	ctx := context.Background()

	if err := p.CreatePod(ctx, restartTestPod(corev1.RestartPolicyOnFailure, "web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rt.setStatus("wl-1", runtime.Status{ID: "wl-1", Phase: runtime.PhaseFailed, ExitCode: 3, Message: "boom"})

	st := waitForRestartCount(t, p, "default", "p1", 1)
	if st.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running after restart", st.Phase)
	}
}

func TestRestartPolicyNeverLeavesTerminated(t *testing.T) {
	p, rt := newTestProvider()
	p.restartBackoffBase = 0
	ctx := context.Background()

	if err := p.CreatePod(ctx, restartTestPod(corev1.RestartPolicyNever, "web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rt.setStatus("wl-1", runtime.Status{ID: "wl-1", Phase: runtime.PhaseFailed, ExitCode: 2, Message: "boom"})

	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.Phase != corev1.PodFailed {
		t.Errorf("phase = %q, want Failed (Never does not restart)", st.Phase)
	}
	time.Sleep(20 * time.Millisecond)
	if _, creates, _, _, _ := rt.counts(); creates != 1 {
		t.Errorf("creates=%d, want 1 (Never must not restart)", creates)
	}
}

func TestRestartReportsCrashLoopBackOffWhileInFlight(t *testing.T) {
	p, rt := newTestProvider()
	// A non-zero base keeps the restart pending long enough to observe the
	// CrashLoopBackOff waiting state before the new workload comes up.
	p.restartBackoffBase = 500 * time.Millisecond
	ctx := context.Background()

	if err := p.CreatePod(ctx, restartTestPod(corev1.RestartPolicyAlways, "web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rt.setStatus("wl-1", runtime.Status{ID: "wl-1", Phase: runtime.PhaseStopped, ExitCode: 1})

	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	cs := st.ContainerStatuses[0]
	if cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff" {
		t.Errorf("state = %+v, want Waiting/CrashLoopBackOff while restart is pending", cs.State)
	}
	if st.Phase == corev1.PodFailed {
		t.Errorf("phase = Failed, want non-terminal while restarting")
	}
}
