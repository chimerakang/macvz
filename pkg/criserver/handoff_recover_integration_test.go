package criserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// TestLiveHandoffRestartCleanup is the gated live restart-cleanup scenario for
// CRI-I4-3 (#120). Against a real apple/container backend with the experimental
// handoff path enabled and disk-backed stores, it proves that after an adapter
// restart:
//
//   - a runtime-private handoff subtree with no surviving container record (an
//     orphan, as a crash mid-create/mid-remove would leave) is reclaimed, while a
//     live container's subtree is kept;
//   - RemoveContainer still cleans the live container's subtree and leaves no
//     orphan apple/container workload behind.
//
// It is gated behind MACVZ_CRI_INTEGRATION=1 because it boots a micro-VM; the
// default test run proves the same logic hermetically in handoff_recover_test.go.
func TestLiveHandoffRestartCleanup(t *testing.T) {
	if os.Getenv("MACVZ_CRI_INTEGRATION") != "1" {
		t.Skip("set MACVZ_CRI_INTEGRATION=1 to run the handoff restart-cleanup scenario against a real apple/container service")
	}

	driver := container.New(container.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := driver.Ready(ctx); err != nil {
		t.Fatalf("apple/container not ready: %v", err)
	}

	dir := t.TempDir()
	handoffRoot := filepath.Join(dir, "handoff")

	// Disk-backed stores so the reopened server recovers persisted records.
	sb, _, err := store.New(filepath.Join(dir, "sandboxes"))
	if err != nil {
		t.Fatalf("sandbox store: %v", err)
	}
	cs, _, err := store.NewContainerStore(filepath.Join(dir, "containers"))
	if err != nil {
		t.Fatalf("container store: %v", err)
	}
	s := New(Options{Runtime: driver, Images: driver, Sandboxes: sb, Containers: cs, Handoff: runtime.NewHandoffManager(handoffRoot)})

	sbResp, err := s.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "cri-i4-3", Namespace: "default", Uid: "uid-i4-3"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	sandboxID := sbResp.GetPodSandboxId()

	if _, err := s.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: liveImage}}); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	cResp, err := s.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
			Image:    &runtimeapi.ImageSpec{Image: liveImage},
			Command:  []string{"/bin/sh", "-c", "sleep 30"},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	workloadID := store.DeriveWorkloadID(id)
	liveLayout, err := runtime.NewHandoffManager(handoffRoot).Layout(workloadID)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	defer func() {
		_, _ = s.RemoveContainer(context.Background(), &runtimeapi.RemoveContainerRequest{ContainerId: id})
		_, _ = s.RemovePodSandbox(context.Background(), &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID})
	}()

	// Stage an orphan handoff subtree with no backing record, as a crash between
	// staging and persistence (or mid-remove) would leave.
	const orphanID = "macvz-cri-liveorphan00000000"
	orphanLayout, err := runtime.NewHandoffManager(handoffRoot).Create(orphanID)
	if err != nil {
		t.Fatalf("seed orphan subtree: %v", err)
	}

	// Restart: reopen the adapter over the same on-disk state and recover.
	sb2, _, err := store.New(filepath.Join(dir, "sandboxes"))
	if err != nil {
		t.Fatalf("reopen sandbox store: %v", err)
	}
	cs2, _, err := store.NewContainerStore(filepath.Join(dir, "containers"))
	if err != nil {
		t.Fatalf("reopen container store: %v", err)
	}
	s2 := New(Options{Runtime: driver, Images: driver, Sandboxes: sb2, Containers: cs2, Handoff: runtime.NewHandoffManager(handoffRoot)})
	s2.RecoverContainers(ctx)

	if _, err := os.Stat(orphanLayout.ContainerDir); !os.IsNotExist(err) {
		t.Errorf("orphan handoff subtree not reclaimed after restart: stat err=%v", err)
	}
	if _, err := os.Stat(liveLayout.ContainerDir); err != nil {
		t.Errorf("live container handoff subtree wrongly reclaimed: %v", err)
	}

	// RemoveContainer cleans the live subtree and leaves no orphan workload.
	if _, err := s2.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("RemoveContainer after restart: %v", err)
	}
	if _, err := os.Stat(liveLayout.ContainerDir); !os.IsNotExist(err) {
		t.Errorf("live subtree not cleaned by post-restart RemoveContainer: stat err=%v", err)
	}
	if _, err := driver.Status(ctx, workloadID); err == nil {
		t.Errorf("workload %q still exists after RemoveContainer (orphan)", workloadID)
	} else if !strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Logf("workload status after remove returned non-NotFound error (acceptable if gone): %v", err)
	}
}
