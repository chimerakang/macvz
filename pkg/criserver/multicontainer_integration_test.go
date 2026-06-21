package criserver

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime/container"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// TestLiveMultiContainerBlockedDiagnostic is the gated live validation for #86.
// It boots a real apple/container sandbox + first container with the experimental
// multi-container probe enabled, attempts a second container, and asserts the
// adapter rejects it honestly — Unimplemented, naming the missing pause-VM
// shared-netns primitive — against the *current* apple/container release.
//
// This is the live half of #86's acceptance: while apple/container exposes no
// shared sandbox namespace, the supported path cannot run, so the live validation
// proves the blocked path fails fast and loudly rather than silently mismodeling a
// Pod. The day apple/container's driver implements SharedPodNetworkRuntime, this
// test's expectation flips to a successful join (and asserts one Pod IP), making
// it the natural live gate for the unblocked feature.
//
// Gated behind MACVZ_CRI_INTEGRATION=1 because it pulls an image and boots a
// micro-VM; the default run stays hermetic via the fake runtime.
func TestLiveMultiContainerBlockedDiagnostic(t *testing.T) {
	if os.Getenv("MACVZ_CRI_INTEGRATION") != "1" {
		t.Skip("set MACVZ_CRI_INTEGRATION=1 to run the multi-container diagnostic against a real apple/container service")
	}

	driver := container.New(container.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := driver.Ready(ctx); err != nil {
		t.Fatalf("apple/container not ready: %v", err)
	}

	// The exact apple/container version under test is recorded in
	// docs/CRI_FEASIBILITY.md alongside this evidence (`container --version`).
	s := New(Options{Runtime: driver, Images: driver, MultiContainer: true})

	sbResp, err := s.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "cri-p86", Namespace: "default", Uid: "uid-mc-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	sandboxID := sbResp.GetPodSandboxId()
	defer func() {
		_, _ = s.RemovePodSandbox(context.Background(), &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID})
	}()

	if _, err := s.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: liveImage}}); err != nil {
		t.Fatalf("PullImage: %v", err)
	}

	app, err := s.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
			Image:    &runtimeapi.ImageSpec{Image: liveImage},
			Command:  []string{"/bin/sh", "-c", "sleep 30"},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer(app): %v", err)
	}
	appID := app.GetContainerId()
	defer func() {
		_, _ = s.RemoveContainer(context.Background(), &runtimeapi.RemoveContainerRequest{ContainerId: appID})
	}()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: appID}); err != nil {
		t.Fatalf("StartContainer(app): %v", err)
	}

	// The second container should be rejected: apple/container does not implement
	// the pause-VM join, so there is no honest way to share the Pod namespace.
	_, err = s.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "sidecar"},
			Image:    &runtimeapi.ImageSpec{Image: liveImage},
			Command:  []string{"/bin/sh", "-c", "sleep 30"},
		},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("second CreateContainer: code = %v, want Unimplemented (apple/container has no shared sandbox namespace)", status.Code(err))
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "shared network namespace") {
		t.Errorf("live diagnostic should name the missing primitive; got %q", msg)
	}
	t.Logf("live multi-container rejection (expected while blocked): %v", err)

	// Failed second-container must not leak a workload or CRI record: only the app
	// container remains.
	listed, err := s.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{PodSandboxId: sandboxID},
	})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if n := len(listed.GetContainers()); n != 1 {
		t.Errorf("blocked second container leaked CRI state: %d containers, want 1", n)
	}
}
