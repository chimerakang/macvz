package criserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime/container"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// TestLiveLogsExecStats drives the CRI-P6 operational surfaces (#78) — file-based
// logging, ExecSync, and ContainerStats — against a real apple/container backend.
// PortForward's byte plumbing is covered hermetically by the streaming proxy unit
// test; the kubelet redirect is not reproduced here.
//
// Gated behind MACVZ_CRI_INTEGRATION=1 because it pulls an image and boots a
// micro-VM.
func TestLiveLogsExecStats(t *testing.T) {
	if os.Getenv("MACVZ_CRI_INTEGRATION") != "1" {
		t.Skip("set MACVZ_CRI_INTEGRATION=1 to run CRI-P6 surfaces against a real apple/container service")
	}

	driver := container.New(container.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := driver.Ready(ctx); err != nil {
		t.Fatalf("apple/container not ready: %v", err)
	}

	logDir := t.TempDir()
	s := New(Options{Runtime: driver, Images: driver})

	sbResp, err := s.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata:     &runtimeapi.PodSandboxMetadata{Name: "cri-p6", Namespace: "default", Uid: "uid-p6-1"},
			LogDirectory: logDir,
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
			Command:  []string{"/bin/sh", "-c", "echo macvz-cri-p6-log && sleep 60"},
			LogPath:  "app.log",
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	defer func() {
		_, _ = s.RemoveContainer(context.Background(), &runtimeapi.RemoveContainerRequest{ContainerId: id})
		_, _ = s.RemovePodSandbox(context.Background(), &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID})
	}()

	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	// Logs: the container's first line should land in the CRI log file.
	logFile := filepath.Join(logDir, "app.log")
	waitForFileContains(t, logFile, "macvz-cri-p6-log")
	if data, _ := os.ReadFile(logFile); !strings.Contains(string(data), " stdout F macvz-cri-p6-log") {
		t.Errorf("log file not in CRI format: %q", data)
	}

	// ExecSync: run a command and capture its output and exit code.
	exec, err := s.ExecSync(ctx, &runtimeapi.ExecSyncRequest{ContainerId: id, Cmd: []string{"/bin/sh", "-c", "echo hi; exit 3"}})
	if err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
	if !strings.Contains(string(exec.GetStdout()), "hi") {
		t.Errorf("ExecSync stdout = %q, want to contain hi", exec.GetStdout())
	}
	if exec.GetExitCode() != 3 {
		t.Errorf("ExecSync exit code = %d, want 3", exec.GetExitCode())
	}

	// Stats: a running container should produce a sample with CPU and memory.
	stats, err := s.ContainerStats(ctx, &runtimeapi.ContainerStatsRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	if stats.GetStats().GetMemory().GetWorkingSetBytes() == nil {
		t.Error("expected a memory sample for a running container")
	}

	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id, Timeout: 10}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
}
