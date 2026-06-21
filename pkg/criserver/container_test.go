package criserver

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// fakeRuntime is a ContainerRuntime that records calls, tracks per-workload
// status, and allows per-method error injection. It lets the container
// lifecycle tests stay hermetic — no apple/container, no micro-VM.
type fakeRuntime struct {
	mu        sync.Mutex
	statuses  map[string]runtime.Status
	pulled    []string
	created   []types.ContainerSpec
	started   []string
	stopped   []string
	destroyed []string

	pullErr, createErr, startErr, stopErr, destroyErr, statusErr error
	// statusOverride, when set for a workload id, is returned by Status.
	statusOverride map[string]runtime.Status
	// startIP, when set, is the host-only address Start records on the workload so
	// the CRI-P5 network attach path can observe a VM IP in hermetic tests.
	startIP string

	// CRI-P6 streaming/logging/stats knobs (#78).
	logsData    string        // bytes the Logs follow stream emits
	logsReader  io.ReadCloser // when set, Logs returns this instead of logsData
	logsErr     error         // injected Logs failure
	logsFollow  bool          // records the last Follow option seen by Logs
	execStdout  string        // written to the Exec stdout stream
	execStderr  string        // written to the Exec stderr stream
	execErr     error         // injected Exec result (e.g. *runtime.ExitError)
	execCalls   [][]string    // commands passed to Exec
	statsSample runtime.ResourceStats
	statsErr    error // injected Stats failure (e.g. runtime.ErrStatsUnavailable)
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		statuses:       map[string]runtime.Status{},
		statusOverride: map[string]runtime.Status{},
	}
}

func (f *fakeRuntime) Pull(_ context.Context, image string, _ *runtime.RegistryAuth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullErr != nil {
		return f.pullErr
	}
	f.pulled = append(f.pulled, image)
	return nil
}

func (f *fakeRuntime) Create(_ context.Context, spec types.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, spec)
	f.statuses[spec.Name] = runtime.Status{ID: spec.Name, Phase: runtime.PhaseCreated}
	return spec.Name, nil
}

func (f *fakeRuntime) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, id)
	f.statuses[id] = runtime.Status{ID: id, Phase: runtime.PhaseRunning, StartedAt: time.Now(), IP: f.startIP}
	return nil
}

func (f *fakeRuntime) Stop(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, id)
	f.statuses[id] = runtime.Status{ID: id, Phase: runtime.PhaseStopped}
	return nil
}

func (f *fakeRuntime) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, id)
	delete(f.statuses, id)
	return nil
}

func (f *fakeRuntime) Status(_ context.Context, id string) (runtime.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return runtime.Status{}, f.statusErr
	}
	if st, ok := f.statusOverride[id]; ok {
		return st, nil
	}
	st, ok := f.statuses[id]
	if !ok {
		return runtime.Status{}, runtime.ErrNotFound
	}
	return st, nil
}

func (f *fakeRuntime) Logs(_ context.Context, _ string, opts runtime.LogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsFollow = opts.Follow
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	if f.logsReader != nil {
		return f.logsReader, nil
	}
	return io.NopCloser(strings.NewReader(f.logsData)), nil
}

func (f *fakeRuntime) Exec(_ context.Context, _ string, cmd []string, sio runtime.ExecIO) error {
	f.mu.Lock()
	f.execCalls = append(f.execCalls, cmd)
	stdout, stderr, execErr := f.execStdout, f.execStderr, f.execErr
	f.mu.Unlock()
	if sio.Stdout != nil && stdout != "" {
		_, _ = io.WriteString(sio.Stdout, stdout)
	}
	if sio.Stderr != nil && stderr != "" {
		_, _ = io.WriteString(sio.Stderr, stderr)
	}
	return execErr
}

func (f *fakeRuntime) Stats(_ context.Context, _ string) (runtime.ResourceStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statsErr != nil {
		return runtime.ResourceStats{}, f.statsErr
	}
	return f.statsSample, nil
}

// newServerWithRuntime builds a server with in-memory stores and the given fake
// runtime, plus a ready sandbox; it returns the server and the sandbox id.
func newServerWithRuntime(t *testing.T, rt ContainerRuntime) (*Server, string) {
	t.Helper()
	s := New(Options{Runtime: rt})
	resp, err := s.RunPodSandbox(context.Background(), &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	return s, resp.GetPodSandboxId()
}

func createReq(sandboxID, name string) *runtimeapi.CreateContainerRequest {
	return &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: name},
			Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/alpine:3.20"},
			Command:  []string{"/bin/sh"},
			Envs:     []*runtimeapi.KeyValue{{Key: "FOO", Value: "bar"}},
		},
	}
}

func TestCreateStartStopRemoveHappyPath(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()

	cResp, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	if !store.ValidID(id) {
		t.Fatalf("CreateContainer returned non-ID %q", id)
	}
	if len(rt.pulled) != 1 || len(rt.created) != 1 {
		t.Fatalf("expected one pull and one create, got pulls=%v creates=%d", rt.pulled, len(rt.created))
	}
	if rt.created[0].Name != store.DeriveWorkloadID(id) {
		t.Errorf("workload name = %q, want derived %q", rt.created[0].Name, store.DeriveWorkloadID(id))
	}

	// Created state before start.
	st, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus(created): %v", err)
	}
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_CREATED {
		t.Errorf("state = %v, want CREATED", st.GetStatus().GetState())
	}

	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	st, _ = s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_RUNNING {
		t.Errorf("state after start = %v, want RUNNING", st.GetStatus().GetState())
	}

	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id, Timeout: 5}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	st, _ = s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_EXITED {
		t.Errorf("state after stop = %v, want EXITED", st.GetStatus().GetState())
	}

	if _, err := s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if len(rt.destroyed) != 1 {
		t.Errorf("expected one destroy, got %v", rt.destroyed)
	}
	if _, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id}); status.Code(err) != codes.NotFound {
		t.Errorf("ContainerStatus after remove: code = %v, want NotFound", status.Code(err))
	}
}

// TestContainerStatusReportsAbsoluteLogPath asserts the CRI contract that
// ContainerStatus reports the log path as the sandbox log_directory joined with
// the container's relative log_path. crictl/kubelet resolve this path directly,
// so a relative value (e.g. "app.log") makes `crictl logs` fail to find the file.
func TestContainerStatusReportsAbsoluteLogPath(t *testing.T) {
	rt := newFakeRuntime()
	s := New(Options{Runtime: rt})
	ctx := context.Background()

	const logDir = "/var/log/pods/default_pod_uid-1"
	sbResp, err := s.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata:     &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
			LogDirectory: logDir,
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}

	cReq := createReq(sbResp.GetPodSandboxId(), "app")
	cReq.Config.LogPath = "app/0.log"
	cResp, err := s.CreateContainer(ctx, cReq)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	st, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: cResp.GetContainerId()})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	want := filepath.Join(logDir, "app/0.log")
	if got := st.GetStatus().GetLogPath(); got != want {
		t.Errorf("LogPath = %q, want absolute %q", got, want)
	}
}

func TestCreateContainerMissingSandbox(t *testing.T) {
	rt := newFakeRuntime()
	s := New(Options{Runtime: rt})
	_, err := s.CreateContainer(context.Background(), createReq("does-not-exist", "app"))
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
	if len(rt.created) != 0 {
		t.Error("no workload should be created for a missing sandbox")
	}
}

func TestCreateContainerRejectsSecondContainer(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()

	if _, err := s.CreateContainer(ctx, createReq(sandboxID, "app")); err != nil {
		t.Fatalf("first CreateContainer: %v", err)
	}
	_, err := s.CreateContainer(ctx, createReq(sandboxID, "sidecar"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second CreateContainer: code = %v, want FailedPrecondition (multi-container unsupported)", status.Code(err))
	}
	if len(rt.created) != 1 {
		t.Errorf("a rejected second container must not create a workload; creates=%d", len(rt.created))
	}
}

func TestCreateContainerConcurrentSingleContainer(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()

	const attempts = 8
	var wg sync.WaitGroup
	errs := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	successes, rejected := 0, 0
	for err := range errs {
		switch status.Code(err) {
		case codes.OK:
			successes++
		case codes.FailedPrecondition:
			rejected++
		default:
			t.Fatalf("unexpected create error: %v (code %v)", err, status.Code(err))
		}
	}
	if successes != 1 || rejected != attempts-1 {
		t.Fatalf("concurrent creates: successes=%d rejected=%d, want 1/%d", successes, rejected, attempts-1)
	}
	if got := s.containers.ListBySandbox(sandboxID); len(got) != 1 {
		t.Fatalf("containers in sandbox = %d, want 1: %+v", len(got), got)
	}
	if len(rt.created) != 1 {
		t.Fatalf("runtime creates = %d, want 1", len(rt.created))
	}
}

func TestCreateContainerOnStoppedSandbox(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()
	if _, err := s.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	_, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition for a not-Ready sandbox", status.Code(err))
	}
}

func TestStartContainerWrongState(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()
	id := mustCreate(t, s, sandboxID)

	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// Starting an already-running container is a precondition failure.
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second start: code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestStartContainerMissing(t *testing.T) {
	rt := newFakeRuntime()
	s := New(Options{Runtime: rt})
	if _, err := s.StartContainer(context.Background(), &runtimeapi.StartContainerRequest{ContainerId: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
}

func TestStopContainerIdempotentAndCapturesExit(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()
	id := mustCreate(t, s, sandboxID)
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Report a non-zero exit so StopContainer records a failure reason.
	rt.statusOverride[store.DeriveWorkloadID(id)] = runtime.Status{Phase: runtime.PhaseFailed, ExitCode: 137}

	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	st, _ := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if st.GetStatus().GetExitCode() != 137 || st.GetStatus().GetReason() != "Error" {
		t.Errorf("exit not captured: code=%d reason=%q", st.GetStatus().GetExitCode(), st.GetStatus().GetReason())
	}
	stops := len(rt.stopped)
	// A second stop is a no-op success and does not re-drive the runtime.
	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("second StopContainer: %v", err)
	}
	if len(rt.stopped) != stops {
		t.Errorf("second stop re-drove the runtime: stops went %d -> %d", stops, len(rt.stopped))
	}
}

func TestRemoveContainerIdempotent(t *testing.T) {
	rt := newFakeRuntime()
	s := New(Options{Runtime: rt})
	// Removing an absent container succeeds (CRI contract).
	if _, err := s.RemoveContainer(context.Background(), &runtimeapi.RemoveContainerRequest{ContainerId: "ghost"}); err != nil {
		t.Errorf("RemoveContainer(absent): %v", err)
	}
}

func TestStopPodSandboxStopsOwnedContainer(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()
	id := mustCreate(t, s, sandboxID)
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("start: %v", err)
	}

	if _, err := s.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	if len(rt.stopped) != 1 {
		t.Fatalf("sandbox stop did not stop workload: stopped=%v", rt.stopped)
	}
	st, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus after sandbox stop: %v", err)
	}
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_EXITED {
		t.Errorf("container state after sandbox stop = %v, want EXITED", st.GetStatus().GetState())
	}
	sb, err := s.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID})
	if err != nil {
		t.Fatalf("PodSandboxStatus after stop: %v", err)
	}
	if sb.GetStatus().GetState() != runtimeapi.PodSandboxState_SANDBOX_NOTREADY {
		t.Errorf("sandbox state = %v, want NOTREADY", sb.GetStatus().GetState())
	}
}

func TestRemovePodSandboxRemovesOwnedContainer(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()
	id := mustCreate(t, s, sandboxID)
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("start: %v", err)
	}

	if _, err := s.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if len(rt.destroyed) != 1 {
		t.Fatalf("sandbox remove did not destroy workload: destroyed=%v", rt.destroyed)
	}
	if _, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id}); status.Code(err) != codes.NotFound {
		t.Errorf("ContainerStatus after sandbox remove code = %v, want NotFound", status.Code(err))
	}
	if _, err := s.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID}); status.Code(err) != codes.NotFound {
		t.Errorf("PodSandboxStatus after sandbox remove code = %v, want NotFound", status.Code(err))
	}
}

func TestListContainersFilters(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()
	id := mustCreate(t, s, sandboxID)

	all, err := s.ListContainers(ctx, &runtimeapi.ListContainersRequest{})
	if err != nil || len(all.GetContainers()) != 1 {
		t.Fatalf("ListContainers = (%v, %v), want one", all, err)
	}
	if all.GetContainers()[0].GetPodSandboxId() != sandboxID {
		t.Errorf("listed container sandbox = %q, want %q", all.GetContainers()[0].GetPodSandboxId(), sandboxID)
	}

	// Filter by a non-matching sandbox id yields nothing.
	none, _ := s.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{PodSandboxId: "other"},
	})
	if len(none.GetContainers()) != 0 {
		t.Errorf("filtered list = %v, want empty", none.GetContainers())
	}

	// Filter by state EXITED yields nothing while the container is Created.
	exited, _ := s.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{
			State: &runtimeapi.ContainerStateValue{State: runtimeapi.ContainerState_CONTAINER_EXITED},
		},
	})
	if len(exited.GetContainers()) != 0 {
		t.Errorf("EXITED filter on a Created container = %v, want empty", exited.GetContainers())
	}
	_ = id
}

func TestContainerStatusReconcilesSelfExit(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()
	id := mustCreate(t, s, sandboxID)
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The workload exits on its own; ContainerStatus must reflect it without a Stop.
	rt.statusOverride[store.DeriveWorkloadID(id)] = runtime.Status{Phase: runtime.PhaseStopped, ExitCode: 0}

	st, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_EXITED {
		t.Errorf("state = %v, want EXITED after self-exit reconcile", st.GetStatus().GetState())
	}
}

func TestContainerStatusMarksMissingRuntimeWorkloadExited(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()
	id := mustCreate(t, s, sandboxID)
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("start: %v", err)
	}
	rt.statusErr = runtime.ErrNotFound

	st, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_EXITED {
		t.Fatalf("state = %v, want EXITED for missing runtime workload", st.GetStatus().GetState())
	}
	if st.GetStatus().GetReason() != "NotFound" {
		t.Errorf("reason = %q, want NotFound", st.GetStatus().GetReason())
	}
}

// TestContainerLifecycleSurvivesReload proves restart tolerance end to end: a new
// Server over the same persisted stores rediscovers the container and answers
// status/list. It mirrors how the adapter reloads after a restart.
func TestContainerLifecycleSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	sandboxes, _, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	containers, _, err := store.NewContainerStore(dir + "/containers")
	if err != nil {
		t.Fatalf("NewContainerStore: %v", err)
	}
	rt := newFakeRuntime()
	s := New(Options{Sandboxes: sandboxes, Containers: containers, Runtime: rt})
	ctx := context.Background()

	sbResp, err := s.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	id := mustCreate(t, s, sbResp.GetPodSandboxId())
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Reopen the stores from disk into a brand-new server (simulated restart).
	sandboxes2, _, _ := store.New(dir)
	containers2, skipped, err := store.NewContainerStore(dir + "/containers")
	if err != nil || skipped != 0 {
		t.Fatalf("reload container store: err=%v skipped=%d", err, skipped)
	}
	rt2 := newFakeRuntime()
	rt2.statusOverride[store.DeriveWorkloadID(id)] = runtime.Status{Phase: runtime.PhaseRunning}
	s2 := New(Options{Sandboxes: sandboxes2, Containers: containers2, Runtime: rt2})

	st, err := s2.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus after reload: %v", err)
	}
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_RUNNING {
		t.Errorf("reloaded state = %v, want RUNNING", st.GetStatus().GetState())
	}
	list, _ := s2.ListContainers(ctx, &runtimeapi.ListContainersRequest{})
	if len(list.GetContainers()) != 1 {
		t.Errorf("reloaded ListContainers = %d, want 1", len(list.GetContainers()))
	}
}

func mustCreate(t *testing.T, s *Server, sandboxID string) string {
	t.Helper()
	resp, err := s.CreateContainer(context.Background(), createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	return resp.GetContainerId()
}
