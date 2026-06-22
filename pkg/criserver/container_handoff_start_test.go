package criserver

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// newHandoffServer builds a CRI server with the experimental handoff path enabled
// (a HandoffManager rooted at a temp dir so preparation stays hermetic and never
// writes under the real /run) plus a ready sandbox. It returns the server, the
// sandbox id, and the handoff root so tests can derive layouts and seed evidence.
// A non-nil containers store overrides the default in-memory one (used to inject
// persistence failures). Shared by the handoff CreateContainer (#115) and
// StartContainer (#116) tests.
func newHandoffServer(t *testing.T, rt ContainerRuntime, containers *store.ContainerStore) (*Server, string, string) {
	t.Helper()
	root := t.TempDir()
	opts := Options{Runtime: rt, Handoff: runtime.NewHandoffManager(root)}
	if containers != nil {
		opts.Containers = containers
	}
	s := New(opts)
	return s, mustRunSandbox(t, s), root
}

// createHandoffContainer creates one container through the real CreateContainer
// path (which prepares the handoff subtree and stages the expected identity) and
// returns its stored record.
func createHandoffContainer(t *testing.T, s *Server, sandboxID string) store.Container {
	t.Helper()
	resp, err := s.CreateContainer(context.Background(), createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	c, ok := s.containers.Get(resp.GetContainerId())
	if !ok {
		t.Fatalf("container %q not found after create", resp.GetContainerId())
	}
	if !c.HandoffPrepared {
		t.Fatalf("expected HandoffPrepared after create with handoff enabled")
	}
	return c
}

// fastHandoffVerify shortens the bounded identity wait so the timeout case is
// quick.
func fastHandoffVerify(s *Server) {
	s.handoffVerifyTimeout = 60 * time.Millisecond
	s.handoffVerifyInterval = 5 * time.Millisecond
}

// layoutUnder derives a workload's handoff layout under a handoff root.
func layoutUnder(t *testing.T, root, workloadID string) runtime.HandoffLayout {
	t.Helper()
	layout, err := runtime.NewHandoffManager(root).Layout(workloadID)
	if err != nil {
		t.Fatalf("Layout: %v", err)
	}
	return layout
}

// seedObservedIdentity writes the handoff evidence the launched process would
// have produced, with the given observed identity.
func seedObservedIdentity(t *testing.T, root, workloadID, observed string) {
	t.Helper()
	layout := layoutUnder(t, root, workloadID)
	if err := os.WriteFile(layout.IdentityFile, []byte(runtime.FormatIdentity(observed)), 0o644); err != nil {
		t.Fatalf("seed observed identity: %v", err)
	}
}

// stagedIdentity returns the expected identity CreateContainer staged into the
// prepared rootfs for a workload.
func stagedIdentity(t *testing.T, root, workloadID string) string {
	t.Helper()
	id, err := runtime.ReadStagedIdentity(layoutUnder(t, root, workloadID).RootfsDir)
	if err != nil {
		t.Fatalf("ReadStagedIdentity: %v", err)
	}
	return id
}

func TestStartContainerHandoffVerifiedRunsContainer(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, root := newHandoffServer(t, rt, nil)
	fastHandoffVerify(s)
	c := createHandoffContainer(t, s, sandboxID)

	// The late process reported the identity the runtime staged.
	seedObservedIdentity(t, root, c.WorkloadID, stagedIdentity(t, root, c.WorkloadID))

	if _, err := s.StartContainer(context.Background(), &runtimeapi.StartContainerRequest{ContainerId: c.ID}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if got, _ := s.containers.Get(c.ID); got.State != store.ContainerRunning {
		t.Errorf("state = %q, want Running", got.State)
	}
	if len(rt.started) != 1 {
		t.Errorf("expected workload started once, got %v", rt.started)
	}
}

func TestStartContainerHandoffMismatchNotRunning(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, root := newHandoffServer(t, rt, nil)
	fastHandoffVerify(s)
	c := createHandoffContainer(t, s, sandboxID)

	// The process reported a different identity than was staged.
	seedObservedIdentity(t, root, c.WorkloadID, "macvz-rootfs-id=someone-else")

	_, err := s.StartContainer(context.Background(), &runtimeapi.StartContainerRequest{ContainerId: c.ID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err code = %v, want FailedPrecondition (%v)", status.Code(err), err)
	}
	got, _ := s.containers.Get(c.ID)
	if got.State != store.ContainerExited {
		t.Errorf("state = %q, want Exited", got.State)
	}
	if got.Reason != "IdentityVerificationFailed" {
		t.Errorf("reason = %q, want IdentityVerificationFailed", got.Reason)
	}
	if len(rt.stopped) != 1 || rt.stopped[0] != c.WorkloadID {
		t.Errorf("expected workload %q stopped, got %v", c.WorkloadID, rt.stopped)
	}
}

func TestStartContainerHandoffTimeoutNotRunning(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, _ := newHandoffServer(t, rt, nil)
	fastHandoffVerify(s)
	c := createHandoffContainer(t, s, sandboxID)

	// No evidence is ever written: the bounded wait must time out and fail.
	start := time.Now()
	_, err := s.StartContainer(context.Background(), &runtimeapi.StartContainerRequest{ContainerId: c.ID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err code = %v, want FailedPrecondition (%v)", status.Code(err), err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("timeout took %v, expected bounded", elapsed)
	}
	if got, _ := s.containers.Get(c.ID); got.State != store.ContainerExited {
		t.Errorf("state = %q, want Exited", got.State)
	}
	if len(rt.stopped) != 1 || rt.stopped[0] != c.WorkloadID {
		t.Errorf("expected workload %q stopped, got %v", c.WorkloadID, rt.stopped)
	}
}

func TestStartContainerHandoffRuntimeStartError(t *testing.T) {
	rt := newFakeRuntime()
	rt.startErr = context.DeadlineExceeded // any non-nil start failure
	s, sandboxID, root := newHandoffServer(t, rt, nil)
	fastHandoffVerify(s)
	c := createHandoffContainer(t, s, sandboxID)
	// Even with valid evidence present, the workload never starts, so verification
	// must not run and the container must not be Running.
	seedObservedIdentity(t, root, c.WorkloadID, stagedIdentity(t, root, c.WorkloadID))

	if _, err := s.StartContainer(context.Background(), &runtimeapi.StartContainerRequest{ContainerId: c.ID}); err == nil {
		t.Fatalf("expected StartContainer error when runtime Start fails")
	}
	if got, _ := s.containers.Get(c.ID); got.State != store.ContainerCreated {
		t.Errorf("state = %q, want Created (start failed before verification)", got.State)
	}
	if len(rt.stopped) != 0 {
		t.Errorf("workload should not be stopped when it never started, got %v", rt.stopped)
	}
}

func TestStartContainerHandoffPersistFailureStopsWorkload(t *testing.T) {
	rt := newFakeRuntime()
	// Disk-backed container store so a removed directory makes the Running Put fail.
	storeDir := t.TempDir()
	containers, _, err := store.NewContainerStore(storeDir)
	if err != nil {
		t.Fatalf("NewContainerStore: %v", err)
	}
	s, sandboxID, root := newHandoffServer(t, rt, containers)
	fastHandoffVerify(s)
	c := createHandoffContainer(t, s, sandboxID)
	seedObservedIdentity(t, root, c.WorkloadID, stagedIdentity(t, root, c.WorkloadID))

	// Break the store so persisting the Running state fails after a verified start.
	if err := os.RemoveAll(storeDir); err != nil {
		t.Fatalf("RemoveAll store dir: %v", err)
	}

	_, err = s.StartContainer(context.Background(), &runtimeapi.StartContainerRequest{ContainerId: c.ID})
	if status.Code(err) != codes.Internal {
		t.Fatalf("err code = %v, want Internal (%v)", status.Code(err), err)
	}
	// The workload was started and verified, so the persist-failure unwind must
	// stop it rather than leak a running micro-VM behind a Created record.
	if len(rt.stopped) != 1 || rt.stopped[0] != c.WorkloadID {
		t.Errorf("expected workload %q stopped after persist failure, got %v", c.WorkloadID, rt.stopped)
	}
	if got, _ := s.containers.Get(c.ID); got.State == store.ContainerRunning {
		t.Errorf("container must not be left Running after persist failure")
	}
}

// TestStartContainerNoHandoffSkipsVerification proves the gate is inert on the
// default apple/container path: a non-handoff server starts normally with no
// evidence file at all.
func TestStartContainerNoHandoffSkipsVerification(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	cResp, err := s.CreateContainer(context.Background(), createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	if _, err := s.StartContainer(context.Background(), &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer (no handoff): %v", err)
	}
	if got, _ := s.containers.Get(id); got.State != store.ContainerRunning {
		t.Errorf("state = %q, want Running", got.State)
	}
}
