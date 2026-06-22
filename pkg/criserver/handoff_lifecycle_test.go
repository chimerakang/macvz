package criserver

import (
	"context"
	"os"
	"testing"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// These tests cover the back half of the handoff-aware CRI lifecycle (CRI-I3-3,
// #117): Stop preserves evidence, Remove cleans up idempotently, verbose Status
// surfaces identity diagnostics, and reconcile advances a vanished workload
// without rereading identity.
//
// They use self-contained lc* helpers (uniquely named so they never collide with
// the handoff helpers in the sibling #115/#116 test files), depending only on the
// stable createReq/newFakeRuntime fixtures.

// lcServer builds a handoff-enabled CRI server rooted at a temp dir (so the
// subtree never touches the real /run) with a ready sandbox, and returns the
// server, sandbox id, and handoff root.
func lcServer(t *testing.T, rt ContainerRuntime) (*Server, string, string) {
	t.Helper()
	root := t.TempDir()
	s := New(Options{Runtime: rt, Handoff: runtime.NewHandoffManager(root)})
	resp, err := s.RunPodSandbox(context.Background(), &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	return s, resp.GetPodSandboxId(), root
}

// lcCreate creates one handoff-prepared container and returns its record.
func lcCreate(t *testing.T, s *Server, sandboxID string) store.Container {
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

// lcLayout derives the runtime-private handoff layout for a workload under root.
func lcLayout(t *testing.T, root, workloadID string) runtime.HandoffLayout {
	t.Helper()
	layout, err := runtime.NewHandoffManager(root).Layout(workloadID)
	if err != nil {
		t.Fatalf("Layout: %v", err)
	}
	return layout
}

// TestStopContainerPreservesHandoffEvidence proves StopContainer records the exit
// state but leaves the runtime-private handoff subtree (identity evidence) intact
// for post-mortem debugging, per CRI-R16: evidence survives until RemoveContainer.
func TestStopContainerPreservesHandoffEvidence(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, root := lcServer(t, rt)
	ctx := context.Background()

	c := lcCreate(t, s, sandboxID)
	layout := lcLayout(t, root, c.WorkloadID)
	// Simulate the evidence a launched process would have written into the handoff
	// directory before the container stopped.
	if err := os.WriteFile(layout.IdentityFile, []byte(runtime.FormatIdentity("id=alpha")), 0o644); err != nil {
		t.Fatalf("seed handoff evidence: %v", err)
	}

	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: c.ID}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	if got, _ := s.containers.Get(c.ID); got.State != store.ContainerExited {
		t.Errorf("state = %q, want Exited", got.State)
	}
	// Handoff evidence and subtree survive a stop.
	if _, err := os.Stat(layout.HandoffDir); err != nil {
		t.Errorf("handoff dir should survive Stop: %v", err)
	}
	if _, err := os.Stat(layout.IdentityFile); err != nil {
		t.Errorf("handoff identity evidence should survive Stop: %v", err)
	}
}

// TestRemoveContainerCleansHandoffIdempotently proves RemoveContainer deletes the
// runtime-private subtree and that removal is idempotent: a second remove (and a
// direct cleanup of an already-gone subtree) still succeeds.
func TestRemoveContainerCleansHandoffIdempotently(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, root := lcServer(t, rt)
	ctx := context.Background()

	c := lcCreate(t, s, sandboxID)
	layout := lcLayout(t, root, c.WorkloadID)
	if _, err := os.Stat(layout.ContainerDir); err != nil {
		t.Fatalf("handoff subtree should exist after create: %v", err)
	}

	if _, err := s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if _, err := os.Stat(layout.ContainerDir); !os.IsNotExist(err) {
		t.Errorf("handoff subtree should be removed after RemoveContainer, stat err = %v", err)
	}
	// The container record is gone, so a second remove is a no-op success.
	if _, err := s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID}); err != nil {
		t.Errorf("idempotent RemoveContainer: %v", err)
	}
	// Cleaning a missing subtree directly is also a no-op (HandoffManager.Cleanup
	// tolerates absence); calling it again must not error or resurrect anything.
	s.cleanupHandoff(c)
	if _, err := os.Stat(layout.ContainerDir); !os.IsNotExist(err) {
		t.Errorf("subtree unexpectedly present after repeat cleanup, stat err = %v", err)
	}
}

// TestContainerStatusVerboseHandoffDiagnostics proves verbose ContainerStatus
// surfaces runtime-private identity-verification diagnostics, and that the
// non-verbose status does not.
func TestContainerStatusVerboseHandoffDiagnostics(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, root := lcServer(t, rt)
	ctx := context.Background()

	c := lcCreate(t, s, sandboxID)
	layout := lcLayout(t, root, c.WorkloadID)
	const identity = "macvz-id=late-alpha"
	// Stage the expected identity into the prepared rootfs and the observed
	// identity (plus a proc_root diagnostic) into the handoff directory so
	// verification resolves to a match.
	if err := runtime.StageIdentityFile(layout.RootfsDir, identity); err != nil {
		t.Fatalf("StageIdentityFile: %v", err)
	}
	if err := os.WriteFile(layout.IdentityFile, []byte(runtime.FormatIdentity(identity)+"proc_root=/\n"), 0o644); err != nil {
		t.Fatalf("seed handoff evidence: %v", err)
	}

	// Non-verbose status carries no Info map.
	plain, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: c.ID})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if plain.GetInfo() != nil {
		t.Errorf("non-verbose status should carry no Info, got %v", plain.GetInfo())
	}

	verbose, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: c.ID, Verbose: true})
	if err != nil {
		t.Fatalf("verbose ContainerStatus: %v", err)
	}
	info := verbose.GetInfo()
	want := map[string]string{
		"handoffPrepared":  "true",
		"handoffPath":      layout.HandoffDir,
		"identitySource":   "handoff",
		"expectedIdentity": identity,
		"observedIdentity": identity,
		"identityVerified": "true",
		"procRoot":         "/",
	}
	for k, v := range want {
		if info[k] != v {
			t.Errorf("verbose info[%q] = %q, want %q", k, info[k], v)
		}
	}
	// The existing diagnostics are still present alongside the handoff ones.
	if info["workloadID"] != c.WorkloadID {
		t.Errorf("verbose info[workloadID] = %q, want %q", info["workloadID"], c.WorkloadID)
	}
}

// TestContainerStatusVerboseHandoffMissingEvidence proves verbose diagnostics
// degrade honestly when no evidence has been written yet: identityVerified=false
// with an explicit "missing" marker rather than a failure.
func TestContainerStatusVerboseHandoffMissingEvidence(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, _ := lcServer(t, rt)
	ctx := context.Background()

	c := lcCreate(t, s, sandboxID)
	verbose, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: c.ID, Verbose: true})
	if err != nil {
		t.Fatalf("verbose ContainerStatus: %v", err)
	}
	info := verbose.GetInfo()
	if info["handoffPrepared"] != "true" {
		t.Errorf("info[handoffPrepared] = %q, want true", info["handoffPrepared"])
	}
	if info["identityVerified"] != "false" {
		t.Errorf("info[identityVerified] = %q, want false", info["identityVerified"])
	}
	if info["identityEvidence"] != "missing" {
		t.Errorf("info[identityEvidence] = %q, want missing", info["identityEvidence"])
	}
}

// TestReconcileRuntimeNotFoundPreservesHandoff proves reconcile advances a
// container to Exited from the runtime phase alone when the workload is gone,
// without rereading or requiring identity evidence (identity is a start
// invariant, not an ongoing property). The handoff subtree is left for
// RemoveContainer to clean.
func TestReconcileRuntimeNotFoundPreservesHandoff(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, root := lcServer(t, rt)
	ctx := context.Background()

	c := lcCreate(t, s, sandboxID)
	layout := lcLayout(t, root, c.WorkloadID)
	// Make the runtime forget the workload so reconcile sees ErrNotFound. No
	// identity evidence is ever written, proving reconcile does not need it.
	if err := rt.Destroy(ctx, c.WorkloadID); err != nil {
		t.Fatalf("seed not-found: %v", err)
	}

	resp, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: c.ID})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	st := resp.GetStatus()
	if st.GetState() != runtimeapi.ContainerState_CONTAINER_EXITED {
		t.Errorf("state = %v, want EXITED", st.GetState())
	}
	if st.GetReason() != "NotFound" {
		t.Errorf("reason = %q, want NotFound", st.GetReason())
	}
	// Reconcile did not delete the handoff subtree; that is RemoveContainer's job.
	if _, err := os.Stat(layout.ContainerDir); err != nil {
		t.Errorf("handoff subtree should survive reconcile: %v", err)
	}
}
