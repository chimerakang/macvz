package criserver

import (
	"context"
	"testing"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// newPersistentServer builds a server whose sandbox and container stores are
// backed by dirs under t.TempDir, so a second server over the same dirs simulates
// an adapter restart that reloads persisted records.
func newPersistentServer(t *testing.T, rt ContainerRuntime) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	sb, _, err := store.New(dir + "/sandboxes")
	if err != nil {
		t.Fatalf("sandbox store: %v", err)
	}
	cs, _, err := store.NewContainerStore(dir + "/containers")
	if err != nil {
		t.Fatalf("container store: %v", err)
	}
	s := New(Options{Runtime: rt, Sandboxes: sb, Containers: cs})
	return s, dir, mustRunSandbox(t, s)
}

func reopenServer(t *testing.T, dir string, rt ContainerRuntime) *Server {
	t.Helper()
	sb, _, err := store.New(dir + "/sandboxes")
	if err != nil {
		t.Fatalf("reopen sandbox store: %v", err)
	}
	cs, _, err := store.NewContainerStore(dir + "/containers")
	if err != nil {
		t.Fatalf("reopen container store: %v", err)
	}
	return New(Options{Runtime: rt, Sandboxes: sb, Containers: cs})
}

// TestRecoverContainersReconcilesExited proves a container that exited while the
// adapter was down is reported Exited after restart, not stale Running.
func TestRecoverContainersReconcilesExited(t *testing.T) {
	rt := newFakeRuntime()
	s, dir, sandboxID := newPersistentServer(t, rt)
	ctx := context.Background()

	resp, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := resp.GetContainerId()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	// Simulate the workload exiting while the adapter is down: a fresh runtime that
	// reports the workload as gone, plus a reopened server over the same state.
	rt2 := newFakeRuntime()
	rt2.statusOverride[store.DeriveWorkloadID(id)] = runtime.Status{Phase: runtime.PhaseStopped, ExitCode: 7}
	s2 := reopenServer(t, dir, rt2)

	// Before recovery the persisted record still says Running.
	if c, _ := s2.containers.Get(id); c.State != store.ContainerRunning {
		t.Fatalf("pre-recovery state = %s, want Running", c.State)
	}
	s2.RecoverContainers(ctx)

	c, ok := s2.containers.Get(id)
	if !ok {
		t.Fatalf("container missing after recovery")
	}
	if c.State != store.ContainerExited {
		t.Errorf("post-recovery state = %s, want Exited", c.State)
	}
	if c.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", c.ExitCode)
	}
}

// TestRecoverContainersNoDuplication proves recovery neither starts nor recreates
// a workload: a still-running container survives as Running with no new
// create/start calls against the runtime.
func TestRecoverContainersNoDuplication(t *testing.T) {
	rt := newFakeRuntime()
	s, dir, sandboxID := newPersistentServer(t, rt)
	ctx := context.Background()

	resp, _ := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	id := resp.GetContainerId()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	// The reopened runtime still reports the workload Running.
	rt2 := newFakeRuntime()
	rt2.statusOverride[store.DeriveWorkloadID(id)] = runtime.Status{Phase: runtime.PhaseRunning}
	s2 := reopenServer(t, dir, rt2)
	s2.RecoverContainers(ctx)

	if len(rt2.created) != 0 || len(rt2.started) != 0 {
		t.Errorf("recovery must not create/start workloads: creates=%d starts=%d", len(rt2.created), len(rt2.started))
	}
	c, _ := s2.containers.Get(id)
	if c.State != store.ContainerRunning {
		t.Errorf("state = %s, want Running", c.State)
	}
	if got := s2.containers.ListBySandbox(sandboxID); len(got) != 1 {
		t.Errorf("expected exactly one container after recovery, got %d", len(got))
	}
}

// TestRestartPolicyAllowsRecreateAfterExit proves a new container can be created
// in a sandbox once its prior container has Exited (the restartPolicy path),
// while a live container still blocks a second one.
func TestRestartPolicyAllowsRecreateAfterExit(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()

	first, _ := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	firstID := first.GetContainerId()

	// A live (Created) container blocks a second create.
	if _, err := s.CreateContainer(ctx, createReq(sandboxID, "app")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second create over live container: err = %v, want FailedPrecondition", err)
	}

	// Stop it -> Exited. Now a recreate (restart) is allowed without removing first.
	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: firstID}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	second, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("recreate after exit: %v", err)
	}
	if second.GetContainerId() == firstID {
		t.Errorf("recreate returned same container id")
	}
}
