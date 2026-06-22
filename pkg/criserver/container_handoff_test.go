package criserver

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
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
	wantIdentityMount, err := runtime.HandoffIdentityMount(layout)
	if err != nil {
		t.Fatalf("HandoffIdentityMount: %v", err)
	}
	if !hasMount(rt.created[0].Mounts, wantIdentityMount) {
		t.Errorf("workload spec missing staged identity mount; mounts=%+v", rt.created[0].Mounts)
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
		if m.Target == runtime.HandoffMountPoint || m.Target == runtime.RootfsIdentityPath {
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

// --- CRI-I5-1 (#121) handoff permission hardening --------------------------

// TestHandoffOwnerFromConfig covers resolving the container process owner from
// the CRI security context: both ids, uid only, none, and an absent context.
func TestHandoffOwnerFromConfig(t *testing.T) {
	cases := []struct {
		name string
		sc   *runtimeapi.LinuxContainerSecurityContext
		want runtime.HandoffOwner
	}{
		{"nil linux", nil, runtime.HandoffOwner{}},
		{"uid and gid", &runtimeapi.LinuxContainerSecurityContext{
			RunAsUser:  &runtimeapi.Int64Value{Value: 1000},
			RunAsGroup: &runtimeapi.Int64Value{Value: 2000},
		}, runtime.HandoffOwner{UID: 1000, GID: 2000, HasUID: true, HasGID: true}},
		{"uid only", &runtimeapi.LinuxContainerSecurityContext{
			RunAsUser: &runtimeapi.Int64Value{Value: 0},
		}, runtime.HandoffOwner{UID: 0, HasUID: true}},
		{"username only is ignored (unreliable)", &runtimeapi.LinuxContainerSecurityContext{
			RunAsUsername: "nobody",
		}, runtime.HandoffOwner{}},
		{"group only is ignored without uid", &runtimeapi.LinuxContainerSecurityContext{
			RunAsGroup: &runtimeapi.Int64Value{Value: 2000},
		}, runtime.HandoffOwner{}},
		{"negative uid ignored", &runtimeapi.LinuxContainerSecurityContext{
			RunAsUser:  &runtimeapi.Int64Value{Value: -1},
			RunAsGroup: &runtimeapi.Int64Value{Value: 2000},
		}, runtime.HandoffOwner{}},
		{"negative gid ignored", &runtimeapi.LinuxContainerSecurityContext{
			RunAsUser:  &runtimeapi.Int64Value{Value: 1000},
			RunAsGroup: &runtimeapi.Int64Value{Value: -1},
		}, runtime.HandoffOwner{UID: 1000, HasUID: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &runtimeapi.ContainerConfig{}
			if tc.sc != nil {
				cfg.Linux = &runtimeapi.LinuxContainerConfig{SecurityContext: tc.sc}
			}
			if got := handoffOwnerFromConfig(cfg); got != tc.want {
				t.Errorf("handoffOwnerFromConfig = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCreateContainerNarrowsHandoffToRunAsUser proves the end-to-end hardening:
// a CreateContainer carrying a runAsUser/runAsGroup the adapter can chown to
// (the test's own uid/gid) narrows the on-disk handoff dir below the
// world-writable fallback, and verbose ContainerStatus reports the policy.
func TestCreateContainerNarrowsHandoffToRunAsUser(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	rt := newFakeRuntime()
	s, sandboxID, root := newHandoffServer(t, rt, nil)

	uid, gid := os.Getuid(), os.Getgid()
	req := createReq(sandboxID, "app")
	req.Config.Linux = &runtimeapi.LinuxContainerConfig{
		SecurityContext: &runtimeapi.LinuxContainerSecurityContext{
			RunAsUser:  &runtimeapi.Int64Value{Value: int64(uid)},
			RunAsGroup: &runtimeapi.Int64Value{Value: int64(gid)},
		},
	}
	resp, err := s.CreateContainer(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := resp.GetContainerId()
	workloadID := store.DeriveWorkloadID(id)
	layout := layoutUnder(t, root, workloadID)

	fi, err := os.Stat(layout.HandoffDir)
	if err != nil {
		t.Fatalf("stat handoff dir: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o770 {
		t.Errorf("handoff dir mode = %o, want 0770 (narrowed to owner+group)", got)
	}

	rec, ok := s.containers.Get(id)
	if !ok {
		t.Fatalf("container record %q not persisted", id)
	}
	info := s.handoffStatusInfo(rec)
	if info["handoffWritePolicy"] != "owner-group" {
		t.Errorf("handoffWritePolicy = %q, want owner-group; info=%+v", info["handoffWritePolicy"], info)
	}
	if info["handoffDirMode"] != "0770" {
		t.Errorf("handoffDirMode = %q, want 0770", info["handoffDirMode"])
	}
}

// TestCreateContainerHandoffReadOnlyRootFSStillWritable proves a read-only root
// filesystem does not block evidence: the handoff bind mount is independently
// writable (ReadOnly=false), and the on-disk handoff dir is writable, so a
// readOnlyRootFilesystem container can still report its identity.
func TestCreateContainerHandoffReadOnlyRootFSStillWritable(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	rt := newFakeRuntime()
	s, sandboxID, root := newHandoffServer(t, rt, nil)

	req := createReq(sandboxID, "app")
	req.Config.Linux = &runtimeapi.LinuxContainerConfig{
		SecurityContext: &runtimeapi.LinuxContainerSecurityContext{
			ReadonlyRootfs: true,
		},
	}
	resp, err := s.CreateContainer(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	workloadID := store.DeriveWorkloadID(resp.GetContainerId())
	layout := layoutUnder(t, root, workloadID)

	// The handoff mount must remain writable regardless of read-only rootfs.
	if len(rt.created) != 1 {
		t.Fatalf("expected one workload create, got %d", len(rt.created))
	}
	wantMount := runtime.HandoffMount(layout)
	if wantMount.ReadOnly {
		t.Fatalf("test invariant: handoff mount should be writable")
	}
	if !hasMount(rt.created[0].Mounts, wantMount) {
		t.Errorf("read-only-rootfs container missing writable handoff mount; mounts=%+v", rt.created[0].Mounts)
	}
	// And the on-disk handoff dir is writable (no runAsUser -> world-writable
	// fallback, which a non-root container can write).
	probe := filepath.Join(layout.HandoffDir, "identity")
	if err := os.WriteFile(probe, []byte("identity=test\n"), 0o600); err != nil {
		t.Errorf("handoff dir not writable under read-only rootfs: %v", err)
	}
	_ = os.Remove(probe)
}

func hasMount(mounts []types.Mount, want types.Mount) bool {
	for _, m := range mounts {
		if m == want {
			return true
		}
	}
	return false
}
