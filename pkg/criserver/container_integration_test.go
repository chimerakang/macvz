package criserver

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// liveImage is a small public arm64 image used to prove the CRI container path
// end to end against a real apple/container service.
const liveImage = "docker.io/library/alpine:3.20"

// TestLiveSingleContainerLifecycle drives the full CreateContainer ->
// StartContainer -> StopContainer -> RemoveContainer path against a real
// apple/container backend, then asserts no orphan workload is left behind.
//
// It is gated behind MACVZ_CRI_INTEGRATION=1 because it pulls an image and boots
// a micro-VM; the default test run stays hermetic via the fake runtime.
func TestLiveSingleContainerLifecycle(t *testing.T) {
	if os.Getenv("MACVZ_CRI_INTEGRATION") != "1" {
		t.Skip("set MACVZ_CRI_INTEGRATION=1 to run the CRI lifecycle against a real apple/container service")
	}

	driver := container.New(container.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := driver.Ready(ctx); err != nil {
		t.Fatalf("apple/container not ready: %v", err)
	}

	// Wire both the container runtime and the ImageService so this exercises the
	// CRI-P4 path: the image is pulled via PullImage, and CreateContainer then
	// relies on it being present rather than pulling implicitly.
	s := New(Options{Runtime: driver, Images: driver})

	sbResp, err := s.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "cri-p3", Namespace: "default", Uid: "uid-live-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	sandboxID := sbResp.GetPodSandboxId()

	if _, err := s.PullImage(ctx, &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: liveImage},
	}); err != nil {
		t.Fatalf("PullImage: %v", err)
	}

	cResp, err := s.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
			Image:    &runtimeapi.ImageSpec{Image: liveImage},
			Command:  []string{"/bin/sh", "-c", "echo macvz-cri-p3 && sleep 30"},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	workloadID := store.DeriveWorkloadID(id)

	// Ensure cleanup even if an assertion fails mid-test.
	defer func() {
		_, _ = s.RemoveContainer(context.Background(), &runtimeapi.RemoveContainerRequest{ContainerId: id})
		_, _ = s.RemovePodSandbox(context.Background(), &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID})
	}()

	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	st, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if st.GetStatus().GetState() != runtimeapi.ContainerState_CONTAINER_RUNNING {
		t.Fatalf("container did not reach RUNNING: %v", st.GetStatus().GetState())
	}

	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id, Timeout: 10}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	if _, err := s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}

	// No orphan workload must remain.
	if _, err := driver.Status(ctx, workloadID); err == nil {
		t.Errorf("workload %q still exists after RemoveContainer (orphan)", workloadID)
	} else if !strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Logf("workload status after remove returned non-NotFound error (acceptable if it means gone): %v", err)
	}
}
