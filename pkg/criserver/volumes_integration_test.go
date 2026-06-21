package criserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// TestLiveVolumesProbeAndRecovery exercises the CRI-P7 surfaces (#79) against a
// real apple/container backend: a kubelet-style host bind mount and a guest tmpfs,
// an exec-probe round trip through ExecSync, and adapter restart recovery that
// reconciles the persisted container without duplicating the workload.
//
// It is gated behind MACVZ_CRI_INTEGRATION=1 (the CRI track's live-test switch)
// because it pulls an image and boots a micro-VM; the default run stays hermetic.
func TestLiveVolumesProbeAndRecovery(t *testing.T) {
	if os.Getenv("MACVZ_CRI_INTEGRATION") != "1" {
		t.Skip("set MACVZ_CRI_INTEGRATION=1 to run CRI volume/probe/recovery against a real apple/container service")
	}

	driver := container.New(container.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := driver.Ready(ctx); err != nil {
		t.Fatalf("apple/container not ready: %v", err)
	}

	// Materialize a host file the way the kubelet would under its pods dir, and
	// allow that dir explicitly so the bind passes the conservative mount policy.
	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "data.txt"), []byte("macvz-cri-p7\n"), 0o644); err != nil {
		t.Fatalf("seed host volume: %v", err)
	}

	stateDir := t.TempDir()
	sandboxes, _, err := store.New(filepath.Join(stateDir, "sandboxes"))
	if err != nil {
		t.Fatalf("sandbox store: %v", err)
	}
	containers, _, err := store.NewContainerStore(filepath.Join(stateDir, "containers"))
	if err != nil {
		t.Fatalf("container store: %v", err)
	}
	s := New(Options{
		Runtime:    driver,
		Images:     driver,
		Sandboxes:  sandboxes,
		Containers: containers,
		Mounts:     MountPolicy{HostPathAllowedPrefixes: []string{hostDir}},
	})

	sbResp, err := s.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "cri-p7", Namespace: "default", Uid: "uid-p7-1"},
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
			Command:  []string{"/bin/sh", "-c", "sleep 120"},
			Mounts: []*runtimeapi.Mount{
				{HostPath: hostDir, ContainerPath: "/data", Readonly: true},
				{HostPath: "", ContainerPath: "/cache"}, // Memory emptyDir -> tmpfs
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	workloadID := store.DeriveWorkloadID(id)
	defer func() {
		_, _ = s.RemoveContainer(context.Background(), &runtimeapi.RemoveContainerRequest{ContainerId: id})
		_, _ = s.RemovePodSandbox(context.Background(), &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID})
	}()

	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	// Volume: the bind-mounted host file is readable inside the guest.
	out := execSync(t, s, ctx, id, []string{"cat", "/data/data.txt"})
	if !strings.Contains(out, "macvz-cri-p7") {
		t.Errorf("bind mount content not visible in guest: %q", out)
	}
	// Volume: the tmpfs target is writable scratch.
	_ = execSync(t, s, ctx, id, []string{"/bin/sh", "-c", "echo scratch > /cache/x && cat /cache/x"})

	// Probe: an exec readiness probe through ExecSync returns exit 0 on success and
	// non-zero on failure, which is exactly what the kubelet uses to drive probes.
	if resp, err := s.ExecSync(ctx, &runtimeapi.ExecSyncRequest{ContainerId: id, Cmd: []string{"true"}, Timeout: 5}); err != nil || resp.GetExitCode() != 0 {
		t.Errorf("exec probe (true): exit=%d err=%v, want exit 0", resp.GetExitCode(), err)
	}
	if resp, err := s.ExecSync(ctx, &runtimeapi.ExecSyncRequest{ContainerId: id, Cmd: []string{"false"}, Timeout: 5}); err != nil || resp.GetExitCode() == 0 {
		t.Errorf("exec probe (false): exit=%d err=%v, want non-zero exit", resp.GetExitCode(), err)
	}

	// Restart recovery: a fresh server over the same state dir reconciles the still
	// running container without recreating the workload, and keeps its mounts.
	driver2 := container.New(container.Config{})
	sandboxes2, _, _ := store.New(filepath.Join(stateDir, "sandboxes"))
	containers2, _, _ := store.NewContainerStore(filepath.Join(stateDir, "containers"))
	s2 := New(Options{Runtime: driver2, Images: driver2, Sandboxes: sandboxes2, Containers: containers2})
	s2.RecoverContainers(ctx)

	st, err := s2.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("post-recovery ContainerStatus: %v", err)
	}
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_RUNNING {
		t.Errorf("post-recovery state = %v, want RUNNING", st.GetStatus().GetState())
	}
	if len(st.GetStatus().GetMounts()) != 2 {
		t.Errorf("post-recovery mounts = %d, want 2", len(st.GetStatus().GetMounts()))
	}
	if _, err := driver2.Status(ctx, workloadID); err != nil {
		t.Errorf("workload %q missing after recovery (recovery must not orphan it): %v", workloadID, err)
	}
}

func execSync(t *testing.T, s *Server, ctx context.Context, id string, cmd []string) string {
	t.Helper()
	resp, err := s.ExecSync(ctx, &runtimeapi.ExecSyncRequest{ContainerId: id, Cmd: cmd, Timeout: 10})
	if err != nil {
		t.Fatalf("ExecSync %v: %v", cmd, err)
	}
	if resp.GetExitCode() != 0 {
		t.Fatalf("ExecSync %v exit=%d stderr=%q", cmd, resp.GetExitCode(), string(resp.GetStderr()))
	}
	return string(resp.GetStdout())
}
