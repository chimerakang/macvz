package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	createdSpecs []types.ContainerSpec
	startedIDs   []string
	stoppedIDs   []string
	destroyedIDs []string

	pullErr, createErr, startErr, stopErr, destroyErr, statusErr error
}

func newRecordingRuntime() *recordingRuntime {
	return &recordingRuntime{
		exists:   map[string]bool{},
		statuses: map[string]runtime.Status{},
	}
}

func (r *recordingRuntime) Pull(_ context.Context, image string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pulls = append(r.pulls, image)
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
	r.statuses[id] = runtime.Status{ID: id, Phase: runtime.PhaseRunning, StartedAt: time.Now()}
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

func (r *recordingRuntime) Logs(context.Context, string, runtime.LogOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (r *recordingRuntime) Exec(context.Context, string, []string, runtime.ExecIO) error { return nil }

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
		Spec:       corev1.PodSpec{Containers: cs},
	}
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
