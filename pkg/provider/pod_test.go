package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// recordingRuntime is a runtime.Runtime fake that records calls, allows error
// injection per method, and tracks per-workload status.
type recordingRuntime struct {
	mu       sync.Mutex
	nextID   int
	exists   map[string]bool           // id -> created and not destroyed
	statuses map[string]runtime.Status // id -> status to report

	pulls        []string
	pullAuths    []*runtime.RegistryAuth // auth passed to Pull, parallel to pulls
	createdSpecs []types.ContainerSpec
	startedIDs   []string
	stoppedIDs   []string
	destroyedIDs []string

	pullErr, createErr, startErr, stopErr, destroyErr, statusErr error

	// runningIP, when set, is reported as the workload's host-only address once
	// it is started, so tests can exercise the Pod network attach path.
	runningIP string

	// log/exec behavior
	logData     string
	lastLogOpts runtime.LogOptions
	logErr      error
	execStdout  string
	execStderr  string
	execErr     error
	lastExecCmd []string
}

func newRecordingRuntime() *recordingRuntime {
	return &recordingRuntime{
		exists:   map[string]bool{},
		statuses: map[string]runtime.Status{},
	}
}

func (r *recordingRuntime) Pull(_ context.Context, image string, auth *runtime.RegistryAuth) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pulls = append(r.pulls, image)
	r.pullAuths = append(r.pullAuths, auth)
	return r.pullErr
}

func (r *recordingRuntime) Create(_ context.Context, spec types.ContainerSpec) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return "", r.createErr
	}
	r.nextID++
	id := fmt.Sprintf("wl-%d", r.nextID)
	r.createdSpecs = append(r.createdSpecs, spec)
	r.exists[id] = true
	r.statuses[id] = runtime.Status{ID: id, Phase: runtime.PhaseCreated}
	return id, nil
}

func (r *recordingRuntime) Start(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return r.startErr
	}
	r.startedIDs = append(r.startedIDs, id)
	r.statuses[id] = runtime.Status{ID: id, Phase: runtime.PhaseRunning, StartedAt: time.Now(), IP: r.runningIP}
	return nil
}

func (r *recordingRuntime) Stop(_ context.Context, id string, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stoppedIDs = append(r.stoppedIDs, id)
	return r.stopErr
}

func (r *recordingRuntime) Destroy(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.destroyedIDs = append(r.destroyedIDs, id)
	if r.destroyErr != nil {
		return r.destroyErr
	}
	delete(r.exists, id)
	delete(r.statuses, id)
	return nil
}

func (r *recordingRuntime) Status(_ context.Context, id string) (runtime.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.statusErr != nil {
		return runtime.Status{}, r.statusErr
	}
	st, ok := r.statuses[id]
	if !ok {
		return runtime.Status{}, runtime.ErrNotFound
	}
	return st, nil
}

func (r *recordingRuntime) Logs(_ context.Context, _ string, opts runtime.LogOptions) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastLogOpts = opts
	if r.logErr != nil {
		return nil, r.logErr
	}
	return io.NopCloser(strings.NewReader(r.logData)), nil
}

func (r *recordingRuntime) Exec(_ context.Context, _ string, cmd []string, sio runtime.ExecIO) error {
	r.mu.Lock()
	stdout, stderr, execErr := r.execStdout, r.execStderr, r.execErr
	r.lastExecCmd = cmd
	r.mu.Unlock()
	if sio.Stdout != nil && stdout != "" {
		_, _ = io.WriteString(sio.Stdout, stdout)
	}
	if sio.Stderr != nil && stderr != "" {
		_, _ = io.WriteString(sio.Stderr, stderr)
	}
	return execErr
}

func (r *recordingRuntime) setStatus(id string, st runtime.Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[id] = st
}

func (r *recordingRuntime) counts() (pulls, creates, starts, stops, destroys int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pulls), len(r.createdSpecs), len(r.startedIDs), len(r.stoppedIDs), len(r.destroyedIDs)
}

func testPod(containers ...string) *corev1.Pod {
	cs := make([]corev1.Container, 0, len(containers))
	for _, name := range containers {
		cs = append(cs, corev1.Container{Name: name, Image: "alpine:3.20"})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p1"},
		// Default fixtures use restartPolicy Never so a terminated workload stays
		// terminal; tests that exercise the restart loop (#45) set Always/OnFailure
		// explicitly via restartTestPod.
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: cs},
	}
}

// restartTestPod is testPod with an explicit restart policy, for exercising the
// micro-VM restart loop (#45).
func restartTestPod(policy corev1.RestartPolicy, containers ...string) *corev1.Pod {
	pod := testPod(containers...)
	pod.Spec.RestartPolicy = policy
	return pod
}

func newTestProvider() (*Provider, *recordingRuntime) {
	rt := newRecordingRuntime()
	return New("mac-01", rt), rt
}

func TestCreatePodLaunchesContainer(t *testing.T) {
	p, rt := newTestProvider()
	if err := p.CreatePod(context.Background(), testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	pulls, creates, starts, _, _ := rt.counts()
	if pulls != 1 || creates != 1 || starts != 1 {
		t.Errorf("pulls=%d creates=%d starts=%d, want 1/1/1", pulls, creates, starts)
	}
	got, err := p.GetPod(context.Background(), "default", "p1")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if got.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if len(got.Status.ContainerStatuses) != 1 {
		t.Errorf("container statuses = %d, want 1", len(got.Status.ContainerStatuses))
	}
}

func TestCreatePodResolvesImagePullSecret(t *testing.T) {
	rt := newRecordingRuntime()
	body := `{"auths":{"registry.example.com":{"username":"alice","password":"s3cret"}}}`
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/reg": dockerConfigSecret("reg", body),
	}}
	p := New("mac-01", rt, WithSecretGetter(getter))

	pod := testPod("web")
	pod.Spec.Containers[0].Image = "registry.example.com/team/app:v1"
	pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "reg"}}

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.pullAuths) != 1 || rt.pullAuths[0] == nil {
		t.Fatalf("expected one authenticated pull, got %+v", rt.pullAuths)
	}
	got := rt.pullAuths[0]
	if got.Username != "alice" || got.Password != "s3cret" || got.Server != "registry.example.com" {
		t.Errorf("pull auth = %+v", got)
	}
}

func TestCreatePodAnonymousPullWithoutPullSecret(t *testing.T) {
	p, rt := newTestProvider()
	if err := p.CreatePod(context.Background(), testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.pullAuths) != 1 || rt.pullAuths[0] != nil {
		t.Errorf("expected a single anonymous (nil-auth) pull, got %+v", rt.pullAuths)
	}
}

func TestCreatePodMissingPullSecretIsTransient(t *testing.T) {
	rt := newRecordingRuntime()
	// Getter with no secrets: the named pull secret is absent.
	p := New("mac-01", rt, WithSecretGetter(fakeSecretGetter{secrets: map[string]*corev1.Secret{}}))

	pod := testPod("web")
	pod.Spec.Containers[0].Image = "registry.example.com/app:v1"
	pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "absent"}}

	err := p.CreatePod(context.Background(), pod)
	if err == nil {
		t.Fatal("expected a transient error so the Pod stays Pending and retries")
	}
	// The runtime must not be touched when credentials cannot be resolved.
	if pulls, creates, _, _, _ := rt.counts(); pulls != 0 || creates != 0 {
		t.Errorf("runtime touched despite unresolved pull secret: pulls=%d creates=%d", pulls, creates)
	}
	// The Pod must not be recorded as a terminal failure: it should self-heal once
	// the Secret appears.
	if _, gErr := p.GetPodStatus(context.Background(), "default", "p1"); gErr == nil {
		t.Error("Pod should not be tracked after a transient pull-credential failure")
	}
}

func TestCreatePodMarksUnsupportedShapeFailed(t *testing.T) {
	p, rt := newTestProvider()
	// A multi-container Pod is unsupported in the MVP.
	if err := p.CreatePod(context.Background(), testPod("web", "side")); err != nil {
		t.Fatalf("CreatePod should record a terminal failure, not return an error: %v", err)
	}
	pulls, creates, _, _, _ := rt.counts()
	if pulls != 0 || creates != 0 {
		t.Errorf("unsupported Pod should not touch the runtime, got pulls=%d creates=%d", pulls, creates)
	}
	st, err := p.GetPodStatus(context.Background(), "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.Phase != corev1.PodFailed {
		t.Errorf("phase = %q, want Failed", st.Phase)
	}
	if st.Reason != "UnsupportedPodSpec" || st.Message == "" {
		t.Errorf("expected a clear UnsupportedPodSpec reason/message, got reason=%q msg=%q", st.Reason, st.Message)
	}
}

func TestCreatePodIdempotent(t *testing.T) {
	p, rt := newTestProvider()
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod (retry): %v", err)
	}
	_, creates, _, _, _ := rt.counts()
	if creates != 1 {
		t.Errorf("duplicate CreatePod created %d workloads, want 1", creates)
	}
}

// seedWorkload makes the runtime report a micro-VM under the deterministic
// workload ID for (ns/name/container), as if it had been left behind by a
// previous kubelet process. The provider's in-memory store is left empty,
// modelling the state right after a kubelet restart.
func (r *recordingRuntime) seedWorkload(id string, phase runtime.Phase, ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exists[id] = true
	r.statuses[id] = runtime.Status{ID: id, Phase: phase, StartedAt: time.Now(), IP: ip}
}

func (r *recordingRuntime) seedRunningWorkload(id, ip string) {
	r.seedWorkload(id, runtime.PhaseRunning, ip)
}

func TestCreatePodAdoptsExistingWorkloadAfterRestart(t *testing.T) {
	p, rt := newTestProvider()
	id := WorkloadID("default", "p1", "web")
	rt.seedRunningWorkload(id, "")

	// A fresh provider (empty store) reconciling a Pod whose micro-VM survived a
	// kubelet restart must adopt the running VM, not pull/create/start a new one.
	if err := p.CreatePod(context.Background(), testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	pulls, creates, starts, _, _ := rt.counts()
	if pulls != 0 || creates != 0 || starts != 0 {
		t.Errorf("adoption should not pull/create/start: pulls=%d creates=%d starts=%d, want 0/0/0", pulls, creates, starts)
	}

	got, err := p.GetPod(context.Background(), "default", "p1")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if got.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if len(got.Status.ContainerStatuses) != 1 {
		t.Fatalf("container statuses = %d, want 1", len(got.Status.ContainerStatuses))
	}
}

// TestCreatePodAdoptionSkipsImagePull is the correctness payoff of #66: a
// recovered VM must come back even when the image could no longer be pulled —
// e.g. its private-registry pull Secret has since been rotated away. Without
// adoption, the re-pull would fail and leave a healthy running container stuck in
// a failing Pod.
func TestCreatePodAdoptionSkipsImagePull(t *testing.T) {
	rt := newRecordingRuntime()
	rt.pullErr = runtime.ErrNotReady // any pull now fails
	// No SecretGetter wired, so resolving an imagePullSecret would itself error —
	// proving adoption bypasses the whole pull/credentials path.
	p := New("mac-01", rt)

	id := WorkloadID("default", "p1", "web")
	rt.seedRunningWorkload(id, "")

	pod := testPod("web")
	pod.Spec.Containers[0].Image = "registry.example.com/team/app:v1"
	pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "rotated-away"}}

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod should adopt the running VM without pulling: %v", err)
	}
	pulls, creates, _, _, _ := rt.counts()
	if pulls != 0 || creates != 0 {
		t.Errorf("adoption pulled/created despite a live VM: pulls=%d creates=%d, want 0/0", pulls, creates)
	}
}

func TestCreatePodStartsCreatedWorkloadAfterRestart(t *testing.T) {
	p, rt := newTestProvider()
	id := WorkloadID("default", "p1", "web")
	rt.seedWorkload(id, runtime.PhaseCreated, "")

	// If the previous kubelet died after Create but before Start, restart
	// recovery should boot that existing VM instead of leaving it stuck in
	// Created or creating a duplicate.
	if err := p.CreatePod(context.Background(), testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	pulls, creates, starts, _, _ := rt.counts()
	if pulls != 0 || creates != 0 || starts != 1 {
		t.Errorf("created-workload adoption should only start: pulls=%d creates=%d starts=%d, want 0/0/1", pulls, creates, starts)
	}
	rt.mu.Lock()
	startedID := ""
	if len(rt.startedIDs) == 1 {
		startedID = rt.startedIDs[0]
	}
	rt.mu.Unlock()
	if startedID != id {
		t.Fatalf("started workload %q, want recovered id %q", startedID, id)
	}

	got, err := p.GetPod(context.Background(), "default", "p1")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if got.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
}

func TestCreatePodRestartsRecoveredStoppedWorkload(t *testing.T) {
	p, rt := newTestProvider()
	p.restartBackoffBase = 0
	id := WorkloadID("default", "p1", "web")
	rt.seedWorkload(id, runtime.PhaseStopped, "")

	if err := p.CreatePod(context.Background(), restartTestPod(corev1.RestartPolicyAlways, "web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	st := waitForRestartCount(t, p, "default", "p1", 1)
	if st.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running after recovered workload restart", st.Phase)
	}
	pulls, creates, starts, _, destroys := rt.counts()
	if pulls != 0 {
		t.Errorf("recovered stopped workload should not be re-pulled, got pulls=%d", pulls)
	}
	if creates != 1 || starts != 1 || destroys != 1 {
		t.Errorf("restart counts = creates %d starts %d destroys %d, want 1/1/1", creates, starts, destroys)
	}
}

func TestCreatePodConcurrentIdempotent(t *testing.T) {
	p, rt := newTestProvider()
	ctx := context.Background()
	const attempts = 10
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- p.CreatePod(ctx, testPod("web"))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
	}
	_, creates, _, _, _ := rt.counts()
	if creates != 1 {
		t.Errorf("concurrent CreatePod created %d workloads, want 1", creates)
	}
}

func TestCreatePodRollsBackOnStartError(t *testing.T) {
	p, rt := newTestProvider()
	rt.startErr = fmt.Errorf("boom")
	err := p.CreatePod(context.Background(), testPod("web"))
	if err == nil {
		t.Fatal("expected CreatePod to fail when Start fails")
	}
	_, creates, _, _, destroys := rt.counts()
	if creates != 1 || destroys != 1 {
		t.Errorf("expected 1 create and 1 rollback destroy, got creates=%d destroys=%d", creates, destroys)
	}
	// Nothing should be tracked, so a retry can start clean.
	if _, gerr := p.GetPod(context.Background(), "default", "p1"); !errdefs.IsNotFound(gerr) {
		t.Errorf("expected NotFound after failed create, got %v", gerr)
	}
}

func TestCreatePodPropagatesErrNotReady(t *testing.T) {
	p, rt := newTestProvider()
	rt.pullErr = runtime.ErrNotReady
	err := p.CreatePod(context.Background(), testPod("web"))
	if err == nil || !errors.Is(err, runtime.ErrNotReady) {
		t.Fatalf("expected error wrapping ErrNotReady, got %v", err)
	}
}

func TestGetPodNotFound(t *testing.T) {
	p, _ := newTestProvider()
	_, err := p.GetPod(context.Background(), "default", "missing")
	if !errdefs.IsNotFound(err) {
		t.Errorf("expected errdefs NotFound, got %v", err)
	}
}

func TestGetPodStatusReconcilesPhases(t *testing.T) {
	p, rt := newTestProvider()
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	// Running by default.
	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running", st.Phase)
	}

	// Flip workload to stopped exit 0 -> Succeeded.
	rt.setStatus("wl-1", runtime.Status{ID: "wl-1", Phase: runtime.PhaseStopped, ExitCode: 0})
	st, _ = p.GetPodStatus(ctx, "default", "p1")
	if st.Phase != corev1.PodSucceeded {
		t.Errorf("phase = %q, want Succeeded", st.Phase)
	}

	// Flip to failed -> Failed with non-zero exit.
	rt.setStatus("wl-1", runtime.Status{ID: "wl-1", Phase: runtime.PhaseFailed, ExitCode: 0, Message: "crash"})
	st, _ = p.GetPodStatus(ctx, "default", "p1")
	if st.Phase != corev1.PodFailed {
		t.Errorf("phase = %q, want Failed", st.Phase)
	}
	if term := st.ContainerStatuses[0].State.Terminated; term == nil || term.ExitCode == 0 {
		t.Errorf("failed container should report non-zero exit, got %+v", term)
	}
}

func TestGetPodStatusMapsLostWorkload(t *testing.T) {
	p, rt := newTestProvider()
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	// Simulate the runtime losing the workload.
	rt.mu.Lock()
	delete(rt.statuses, "wl-1")
	rt.mu.Unlock()

	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	term := st.ContainerStatuses[0].State.Terminated
	if term == nil || term.Reason != "Lost" {
		t.Errorf("lost workload should map to terminated/Lost, got %+v", st.ContainerStatuses[0].State)
	}
}

func TestGetPodStatusPreservesPreviousStatusOnRuntimeError(t *testing.T) {
	p, rt := newTestProvider()
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	st, err := p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.Phase != corev1.PodRunning {
		t.Fatalf("phase = %q, want Running before injected error", st.Phase)
	}

	rt.statusErr = runtime.ErrNotReady
	st, err = p.GetPodStatus(ctx, "default", "p1")
	if err != nil {
		t.Fatalf("GetPodStatus with transient runtime error: %v", err)
	}
	if st.Phase != corev1.PodRunning {
		t.Errorf("phase regressed to %q, want Running", st.Phase)
	}
	if st.Message == "" {
		t.Error("expected runtime error message to be surfaced on Pod status")
	}
	for _, c := range st.Conditions {
		if c.Type == corev1.PodReady && c.Reason != "RuntimeStatusError" {
			t.Errorf("Ready condition reason = %q, want RuntimeStatusError", c.Reason)
		}
	}
}

func TestDeletePodIsIdempotent(t *testing.T) {
	p, rt := newTestProvider()
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if err := p.DeletePod(ctx, testPod("web")); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	_, _, _, stops, destroys := rt.counts()
	if stops != 1 || destroys != 1 {
		t.Errorf("expected 1 stop and 1 destroy, got stops=%d destroys=%d", stops, destroys)
	}
	// Second delete: already gone -> NotFound (treated as success by VK).
	if err := p.DeletePod(ctx, testPod("web")); !errdefs.IsNotFound(err) {
		t.Errorf("duplicate DeletePod should return NotFound, got %v", err)
	}
}

func TestUpdatePod(t *testing.T) {
	p, _ := newTestProvider()
	ctx := context.Background()
	if err := p.UpdatePod(ctx, testPod("web")); !errdefs.IsNotFound(err) {
		t.Errorf("UpdatePod on unknown pod should return NotFound, got %v", err)
	}
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	updated := testPod("web")
	updated.Labels = map[string]string{"team": "infra"}
	if err := p.UpdatePod(ctx, updated); err != nil {
		t.Fatalf("UpdatePod: %v", err)
	}
	got, _ := p.GetPod(ctx, "default", "p1")
	if got.Labels["team"] != "infra" {
		t.Errorf("UpdatePod did not refresh labels: %v", got.Labels)
	}
}

func TestGetPodsListsAll(t *testing.T) {
	p, _ := newTestProvider()
	ctx := context.Background()
	if err := p.CreatePod(ctx, testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	pods, err := p.GetPods(ctx)
	if err != nil {
		t.Fatalf("GetPods: %v", err)
	}
	if len(pods) != 1 {
		t.Errorf("GetPods returned %d, want 1", len(pods))
	}
}
