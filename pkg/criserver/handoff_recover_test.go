package criserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// These tests cover the restart side of the handoff lifecycle (CRI-I4-3, #120):
// a restarted adapter keeps every handoff subtree a surviving container record
// still claims, reclaims subtrees no record claims (orphans), and RemoveContainer
// stays idempotent across the restart. They are hermetic — disk-backed stores plus
// a handoff root under t.TempDir, reopened over the same dirs to simulate the
// adapter process restarting — and never touch the real /run or apple/container.

// newPersistentHandoffServer builds a handoff-enabled server whose sandbox,
// container, and handoff state all live under one temp dir, so a second server
// over the same dir simulates an adapter restart. It returns the server, the root
// dir, the handoff root, and a ready sandbox id.
func newPersistentHandoffServer(t *testing.T, rt ContainerRuntime) (*Server, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	handoffRoot := filepath.Join(dir, "handoff")
	s := openHandoffServer(t, dir, handoffRoot, rt)
	return s, dir, handoffRoot, mustRunSandbox(t, s)
}

// openHandoffServer wires disk-backed stores under dir and a HandoffManager rooted
// at handoffRoot. reopenHandoffServer is its restart-time alias for readability.
func openHandoffServer(t *testing.T, dir, handoffRoot string, rt ContainerRuntime) *Server {
	t.Helper()
	sb, _, err := store.New(filepath.Join(dir, "sandboxes"))
	if err != nil {
		t.Fatalf("sandbox store: %v", err)
	}
	cs, _, err := store.NewContainerStore(filepath.Join(dir, "containers"))
	if err != nil {
		t.Fatalf("container store: %v", err)
	}
	s := New(Options{
		Runtime:    rt,
		Sandboxes:  sb,
		Containers: cs,
		Handoff:    runtime.NewHandoffManager(handoffRoot),
	})
	fastHandoffVerify(s)
	return s
}

func reopenHandoffServer(t *testing.T, dir, handoffRoot string, rt ContainerRuntime) *Server {
	t.Helper()
	return openHandoffServer(t, dir, handoffRoot, rt)
}

// driveToRunning starts a handoff container and seeds the identity evidence the
// late process would have written, so verification passes and it reaches Running.
func driveToRunning(t *testing.T, s *Server, handoffRoot string, c store.Container) {
	t.Helper()
	seedObservedIdentity(t, handoffRoot, c.WorkloadID, handoffExpectedIdentity(c.WorkloadID))
	if _, err := s.StartContainer(context.Background(), &runtimeapi.StartContainerRequest{ContainerId: c.ID}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if got, _ := s.containers.Get(c.ID); got.State != store.ContainerRunning {
		t.Fatalf("state after start = %q, want Running", got.State)
	}
}

// TestRecoverPreservesHandoffAcrossRestartStates proves that for a handoff
// container in any pre-restart state — Created, Running, Exited, or a failed Start
// — a restart keeps the runtime-private subtree (a surviving record claims it),
// and that the post-restart RemoveContainer still cleans the subtree and is
// idempotent. Identity evidence is never reread during recovery; preservation is
// decided from the record alone.
func TestRecoverPreservesHandoffAcrossRestartStates(t *testing.T) {
	cases := []struct {
		name string
		// prep drives the freshly created container to the target state and returns
		// the runtime status the reopened adapter should report for it (so reconcile
		// keeps the intended state instead of flipping it on a fresh runtime).
		prep func(t *testing.T, s *Server, handoffRoot string, c store.Container) (runtime.Status, bool)
		want store.ContainerState
	}{
		{
			name: "Created",
			prep: func(_ *testing.T, _ *Server, _ string, _ store.Container) (runtime.Status, bool) {
				return runtime.Status{Phase: runtime.PhaseCreated}, true
			},
			want: store.ContainerCreated,
		},
		{
			name: "Running",
			prep: func(t *testing.T, s *Server, root string, c store.Container) (runtime.Status, bool) {
				driveToRunning(t, s, root, c)
				return runtime.Status{Phase: runtime.PhaseRunning}, true
			},
			want: store.ContainerRunning,
		},
		{
			name: "Exited",
			prep: func(t *testing.T, s *Server, root string, c store.Container) (runtime.Status, bool) {
				driveToRunning(t, s, root, c)
				if _, err := s.StopContainer(context.Background(), &runtimeapi.StopContainerRequest{ContainerId: c.ID}); err != nil {
					t.Fatalf("StopContainer: %v", err)
				}
				return runtime.Status{}, false // Exited records are not reconciled.
			},
			want: store.ContainerExited,
		},
		{
			name: "FailedStart",
			prep: func(t *testing.T, s *Server, _ string, c store.Container) (runtime.Status, bool) {
				// Start with no seeded evidence: identity verification times out, the
				// workload is unwound, and the container lands Exited but prepared.
				_, err := s.StartContainer(context.Background(), &runtimeapi.StartContainerRequest{ContainerId: c.ID})
				if err == nil {
					t.Fatal("StartContainer unexpectedly succeeded without identity evidence")
				}
				if got, _ := s.containers.Get(c.ID); got.State != store.ContainerExited {
					t.Fatalf("state after failed start = %q, want Exited", got.State)
				}
				return runtime.Status{}, false
			},
			want: store.ContainerExited,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeRuntime()
			s, dir, handoffRoot, sandboxID := newPersistentHandoffServer(t, rt)
			ctx := context.Background()

			c := createHandoffContainer(t, s, sandboxID)
			layout := layoutUnder(t, handoffRoot, c.WorkloadID)
			status, _ := tc.prep(t, s, handoffRoot, c)

			// Restart: a fresh runtime that only knows what the prep declared.
			rt2 := newFakeRuntime()
			if (status != runtime.Status{}) {
				rt2.statusOverride[c.WorkloadID] = status
			}
			s2 := reopenHandoffServer(t, dir, handoffRoot, rt2)
			s2.RecoverContainers(ctx)

			got, ok := s2.containers.Get(c.ID)
			if !ok {
				t.Fatalf("container record lost across restart")
			}
			if got.State != tc.want {
				t.Errorf("post-restart state = %q, want %q", got.State, tc.want)
			}
			// The claimed subtree must survive recovery — the sweep keeps it.
			if _, err := os.Stat(layout.ContainerDir); err != nil {
				t.Errorf("handoff subtree should survive restart for a known container: %v", err)
			}

			// RemoveContainer after restart still cleans the subtree...
			if _, err := s2.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID}); err != nil {
				t.Fatalf("RemoveContainer after restart: %v", err)
			}
			if _, err := os.Stat(layout.ContainerDir); !os.IsNotExist(err) {
				t.Errorf("handoff subtree not cleaned by post-restart RemoveContainer: stat err=%v", err)
			}
			// ...and remains idempotent.
			if _, err := s2.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID}); err != nil {
				t.Errorf("idempotent RemoveContainer after restart: %v", err)
			}
		})
	}
}

// TestRecoverSweepsOrphanHandoffSubtree proves the orphan sweep reclaims a
// runtime-private subtree no container record claims (a crash between staging a
// subtree and persisting its record, or a death mid-RemoveContainer after the
// record was deleted), while leaving a live container's subtree intact.
func TestRecoverSweepsOrphanHandoffSubtree(t *testing.T) {
	rt := newFakeRuntime()
	s, dir, handoffRoot, sandboxID := newPersistentHandoffServer(t, rt)
	ctx := context.Background()

	// One live, recorded container — its subtree must be kept.
	live := createHandoffContainer(t, s, sandboxID)
	liveLayout := layoutUnder(t, handoffRoot, live.WorkloadID)

	// An orphan subtree with no backing record, staged directly under the handoff
	// root the way a crashed create/remove would have left it.
	const orphanID = "macvz-cri-orphanabc123def456"
	mgr := runtime.NewHandoffManager(handoffRoot)
	orphanLayout, err := mgr.Create(orphanID)
	if err != nil {
		t.Fatalf("seed orphan subtree: %v", err)
	}
	if _, err := os.Stat(orphanLayout.ContainerDir); err != nil {
		t.Fatalf("orphan subtree missing after seed: %v", err)
	}

	rt2 := newFakeRuntime()
	rt2.statusOverride[live.WorkloadID] = runtime.Status{Phase: runtime.PhaseCreated}
	s2 := reopenHandoffServer(t, dir, handoffRoot, rt2)
	s2.RecoverContainers(ctx)

	if _, err := os.Stat(orphanLayout.ContainerDir); !os.IsNotExist(err) {
		t.Errorf("orphan handoff subtree not reclaimed by recovery: stat err=%v", err)
	}
	if _, err := os.Stat(liveLayout.ContainerDir); err != nil {
		t.Errorf("live container subtree wrongly reclaimed: %v", err)
	}
}

// TestSweepOrphanHandoffsCountsAndReports proves the sweep reports how many
// subtrees it reclaimed versus kept, and that it never reclaims a claimed one.
func TestSweepOrphanHandoffsCountsAndReports(t *testing.T) {
	rt := newFakeRuntime()
	s, _, handoffRoot, sandboxID := newPersistentHandoffServer(t, rt)

	createHandoffContainer(t, s, sandboxID) // one claimed subtree
	mgr := runtime.NewHandoffManager(handoffRoot)
	for _, id := range []string{"macvz-cri-orphanone0000000", "macvz-cri-orphantwo0000000"} {
		if _, err := mgr.Create(id); err != nil {
			t.Fatalf("seed orphan %q: %v", id, err)
		}
	}

	cleaned, kept := s.sweepOrphanHandoffs(context.Background())
	if cleaned != 2 {
		t.Errorf("cleaned = %d, want 2", cleaned)
	}
	if kept != 1 {
		t.Errorf("kept = %d, want 1", kept)
	}
}

// TestSweepOrphanHandoffsInertWhenDisabled proves the sweep is a no-op on the
// default apple/container path (no handoff manager), so non-handoff recovery is
// unaffected.
func TestSweepOrphanHandoffsInertWhenDisabled(t *testing.T) {
	rt := newFakeRuntime()
	s, _ := newServerWithRuntime(t, rt)
	if cleaned, kept := s.sweepOrphanHandoffs(context.Background()); cleaned != 0 || kept != 0 {
		t.Errorf("disabled sweep = (%d, %d), want (0, 0)", cleaned, kept)
	}
}

// TestRemovedContainerLeavesNoOrphanAfterRestart proves a container removed before
// the restart leaves no subtree to reclaim and that a redundant RemoveContainer
// after the restart is still an idempotent success.
func TestRemovedContainerLeavesNoOrphanAfterRestart(t *testing.T) {
	rt := newFakeRuntime()
	s, dir, handoffRoot, sandboxID := newPersistentHandoffServer(t, rt)
	ctx := context.Background()

	c := createHandoffContainer(t, s, sandboxID)
	layout := layoutUnder(t, handoffRoot, c.WorkloadID)
	if _, err := s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if _, err := os.Stat(layout.ContainerDir); !os.IsNotExist(err) {
		t.Fatalf("subtree not cleaned by RemoveContainer: stat err=%v", err)
	}

	rt2 := newFakeRuntime()
	s2 := reopenHandoffServer(t, dir, handoffRoot, rt2)
	s2.RecoverContainers(ctx) // nothing to sweep, no record to recover

	if _, err := s2.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID}); err != nil {
		t.Errorf("redundant RemoveContainer after restart should be idempotent: %v", err)
	}
}
