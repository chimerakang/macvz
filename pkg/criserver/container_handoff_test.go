package criserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// These tests cover the CRI-I3 (#115) CreateContainer handoff preparation path.
// Shared fixtures (newHandoffServer, createHandoffContainer, layoutUnder) live in
// container_handoff_start_test.go.

func TestCreateContainerPreparesHandoff(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, root := newHandoffServer(t, rt, nil)
	ctx := context.Background()

	resp, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := resp.GetContainerId()
	workloadID := store.DeriveWorkloadID(id)

	// CreateContainer must not start the container.
	if len(rt.started) != 0 {
		t.Errorf("container was started during CreateContainer: %v", rt.started)
	}

	// The per-container handoff subtree was prepared on disk.
	layout := layoutUnder(t, root, workloadID)
	for _, dir := range []string{layout.ContainerDir, layout.RootfsDir, layout.HandoffDir} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("expected prepared dir %q: stat err=%v", dir, err)
		}
	}
	// The in-rootfs mount destination exists.
	mp := filepath.Join(layout.RootfsDir, "run", "macvz", "handoff")
	if fi, err := os.Stat(mp); err != nil || !fi.IsDir() {
		t.Errorf("expected handoff mountpoint %q: stat err=%v", mp, err)
	}
	// The expected identity was staged into the rootfs.
	staged, err := runtime.ReadStagedIdentity(layout.RootfsDir)
	if err != nil {
		t.Fatalf("ReadStagedIdentity: %v", err)
	}
	if want := handoffExpectedIdentity(workloadID); staged != want {
		t.Errorf("staged identity = %q, want %q", staged, want)
	}

	// The handoff bind mount was injected into the workload spec.
	if len(rt.created) != 1 {
		t.Fatalf("expected one workload create, got %d", len(rt.created))
	}
	if !hasMount(rt.created[0].Mounts, runtime.HandoffMount(layout)) {
		t.Errorf("workload spec missing handoff mount; mounts=%+v", rt.created[0].Mounts)
	}

	// Only the minimal CRI mapping is persisted: HandoffPrepared, and the record
	// stays Created (not started).
	rec, ok := s.containers.Get(id)
	if !ok {
		t.Fatalf("container record %q not persisted", id)
	}
	if !rec.HandoffPrepared {
		t.Errorf("HandoffPrepared not set on persisted record")
	}
	if rec.State != store.ContainerCreated {
		t.Errorf("state = %v, want Created", rec.State)
	}
}

// TestCreateContainerWithoutHandoffIsUnchanged proves the default path (no
// handoff manager) prepares nothing and injects no handoff mount.
func TestCreateContainerWithoutHandoffIsUnchanged(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)

	resp, err := s.CreateContainer(context.Background(), createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if len(rt.created) != 1 {
		t.Fatalf("expected one create, got %d", len(rt.created))
	}
	for _, m := range rt.created[0].Mounts {
		if m.Target == runtime.HandoffMountPoint {
			t.Errorf("default path injected a handoff mount: %+v", m)
		}
	}
	if rec, ok := s.containers.Get(resp.GetContainerId()); !ok || rec.HandoffPrepared {
		t.Errorf("default path marked HandoffPrepared: ok=%v rec=%+v", ok, rec)
	}
}

func TestCreateContainerPrepareFailureCleansUp(t *testing.T) {
	// Root the handoff manager under a regular file so HandoffManager.Create's
	// MkdirAll fails deterministically (ENOTDIR), regardless of test uid.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime()
	s := New(Options{Runtime: rt, Handoff: runtime.NewHandoffManager(filepath.Join(blocker, "containers"))})
	sandboxID := mustRunSandbox(t, s)

	_, err := s.CreateContainer(context.Background(), createReq(sandboxID, "app"))
	if err == nil {
		t.Fatal("CreateContainer unexpectedly succeeded with a broken handoff root")
	}
	if got := status.Code(err); got != codes.Internal {
		t.Errorf("error code = %v, want Internal", got)
	}

	// Preparation failed before the workload was created, and nothing was persisted.
	if len(rt.created) != 0 {
		t.Errorf("workload created despite prepare failure: %v", rt.created)
	}
	if n := len(s.containers.List()); n != 0 {
		t.Errorf("persisted %d container records, want 0", n)
	}
}

func TestCreateContainerPersistFailureCleansUpHandoff(t *testing.T) {
	// A disk-backed container store whose directory is removed after construction:
	// persist's CreateTemp then fails, exercising the persist-failure cleanup path.
	storeDir := t.TempDir()
	containers, _, err := store.NewContainerStore(storeDir)
	if err != nil {
		t.Fatalf("NewContainerStore: %v", err)
	}
	rt := newFakeRuntime()
	s, sandboxID, root := newHandoffServer(t, rt, containers)
	if err := os.RemoveAll(storeDir); err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateContainer(context.Background(), createReq(sandboxID, "app"))
	if err == nil {
		t.Fatal("CreateContainer unexpectedly succeeded with a broken store")
	}
	if got := status.Code(err); got != codes.Internal {
		t.Errorf("error code = %v, want Internal", got)
	}

	// The workload was reclaimed and the handoff subtree was cleaned up, leaving no
	// orphan under the handoff root.
	if len(rt.destroyed) != 1 {
		t.Errorf("expected one workload reclaim, got %v", rt.destroyed)
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadDir handoff root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("handoff root not empty after persist-failure cleanup: %v", entries)
	}
}

func TestRemoveContainerCleansUpHandoff(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID, root := newHandoffServer(t, rt, nil)
	ctx := context.Background()

	c := createHandoffContainer(t, s, sandboxID)
	layout := layoutUnder(t, root, c.WorkloadID)
	if _, err := os.Stat(layout.ContainerDir); err != nil {
		t.Fatalf("handoff subtree missing after create: %v", err)
	}

	if _, err := s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if _, err := os.Stat(layout.ContainerDir); !os.IsNotExist(err) {
		t.Errorf("handoff subtree not removed by RemoveContainer: stat err=%v", err)
	}
}

func hasMount(mounts []types.Mount, want types.Mount) bool {
	for _, m := range mounts {
		if m == want {
			return true
		}
	}
	return false
}
