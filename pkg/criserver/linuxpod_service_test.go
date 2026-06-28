package criserver

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// These tests drive the kubelet CRI ordering through the LinuxPod-backed CRI
// service (CRI-L2, #127) against an in-process fake LinuxPod backend: RunPodSandbox
// → CreateContainer(app)/StartContainer(app) → late CreateContainer(sidecar) after
// the app is Running → StartContainer(sidecar), then stop/remove/cleanup. They
// assert both containers live in one sandbox namespace, identity verified, and no
// stale backend state after teardown — the CRI-server-level analog of the
// backend-contract tests in pkg/runtime/linuxpod.

func newLinuxPodTestService(t *testing.T, backend linuxpod.Backend) *LinuxPodService {
	t.Helper()
	svc, err := NewLinuxPodService(LinuxPodOptions{Backend: backend})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	return svc
}

func lpRunSandbox(t *testing.T, svc *LinuxPodService) string {
	t.Helper()
	resp, err := svc.RunPodSandbox(context.Background(), &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	return resp.GetPodSandboxId()
}

func lpCreateStart(t *testing.T, svc *LinuxPodService, sandboxID, name string) string {
	t.Helper()
	ctx := context.Background()
	cresp, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: name},
			Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"},
			Command:  []string{"/bin/sh", "-c", "sleep 300"},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer(%s): %v", name, err)
	}
	if _, err := svc.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: cresp.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer(%s): %v", name, err)
	}
	return cresp.GetContainerId()
}

func lpVerboseInfo(t *testing.T, svc *LinuxPodService, id string) map[string]string {
	t.Helper()
	resp, err := svc.ContainerStatus(context.Background(), &runtimeapi.ContainerStatusRequest{ContainerId: id, Verbose: true})
	if err != nil {
		t.Fatalf("ContainerStatus(%s): %v", id, err)
	}
	return resp.GetInfo()
}

func TestLinuxPodServiceKubeletOrdering(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	if _, err := svc.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"}}); err != nil {
		t.Fatalf("PullImage: %v", err)
	}

	appID := lpCreateStart(t, svc, sandboxID, "app")
	// Sidecar created AFTER the app is already running — the late-sidecar ordering.
	sidecarID := lpCreateStart(t, svc, sandboxID, "sidecar")

	appInfo := lpVerboseInfo(t, svc, appID)
	sideInfo := lpVerboseInfo(t, svc, sidecarID)

	if appInfo["identityVerified"] != "true" || sideInfo["identityVerified"] != "true" {
		t.Errorf("both containers must be identity-verified: app=%q sidecar=%q",
			appInfo["identityVerified"], sideInfo["identityVerified"])
	}
	if appInfo["sandboxNamespace"] == "" || appInfo["sandboxNamespace"] != sideInfo["sandboxNamespace"] {
		t.Errorf("app and sidecar must share one sandbox namespace: %q vs %q",
			appInfo["sandboxNamespace"], sideInfo["sandboxNamespace"])
	}

	// Both containers are Running and visible in one sandbox.
	containers, err := svc.ListContainers(ctx, &runtimeapi.ListContainersRequest{})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers.GetContainers()) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers.GetContainers()))
	}
	for _, c := range containers.GetContainers() {
		if c.GetState() != runtimeapi.ContainerState_CONTAINER_RUNNING {
			t.Errorf("container %s state = %v, want RUNNING", c.GetMetadata().GetName(), c.GetState())
		}
		if c.GetPodSandboxId() != sandboxID {
			t.Errorf("container %s sandbox = %q, want %q", c.GetMetadata().GetName(), c.GetPodSandboxId(), sandboxID)
		}
	}

	// PodSandboxStatus surfaces the shared namespace.
	sbStatus, err := svc.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID, Verbose: true})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	if sbStatus.GetInfo()["sandboxNamespace"] != appInfo["sandboxNamespace"] {
		t.Errorf("sandbox namespace mismatch: status=%q container=%q",
			sbStatus.GetInfo()["sandboxNamespace"], appInfo["sandboxNamespace"])
	}
}

func TestLinuxPodServiceStopRemoveCleanupNoStaleState(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")
	sidecarID := lpCreateStart(t, svc, sandboxID, "sidecar")

	// Stop sidecar first, then app (one of the orderings #130 requires).
	for _, id := range []string{sidecarID, appID} {
		if _, err := svc.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id, Timeout: 5}); err != nil {
			t.Fatalf("StopContainer(%s): %v", id, err)
		}
		// Idempotent second stop.
		if _, err := svc.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id}); err != nil {
			t.Errorf("idempotent StopContainer(%s): %v", id, err)
		}
	}

	// RemovePodSandbox removes containers and tears down the Pod VM.
	if _, err := svc.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	// No stale backend state: a direct Cleanup reports nothing left to remove.
	rep, err := backend.Cleanup(ctx, sandboxID)
	if err != nil {
		t.Fatalf("backend.Cleanup: %v", err)
	}
	if rep.PodRemoved || rep.RemovedContainers != 0 || rep.RemovedRootfs != 0 {
		t.Errorf("backend left stale state after RemovePodSandbox: %+v", rep)
	}
	// Sandbox and containers are gone from the CRI view.
	if _, err := svc.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID}); status.Code(err) != codes.NotFound {
		t.Errorf("PodSandboxStatus after remove = %v, want NotFound", err)
	}
	if _, err := svc.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: appID}); status.Code(err) != codes.NotFound {
		t.Errorf("ContainerStatus after remove = %v, want NotFound", err)
	}
	// Idempotent second RemovePodSandbox.
	if _, err := svc.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Errorf("idempotent RemovePodSandbox: %v", err)
	}
}

func TestLinuxPodServiceStopPodSandboxCleansBackendState(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")
	sidecarID := lpCreateStart(t, svc, sandboxID, "sidecar")

	if _, err := svc.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}

	rep, err := backend.Cleanup(ctx, sandboxID)
	if err != nil {
		t.Fatalf("backend.Cleanup: %v", err)
	}
	if rep.PodRemoved || rep.RemovedContainers != 0 || rep.RemovedRootfs != 0 {
		t.Errorf("backend left stale state after StopPodSandbox: %+v", rep)
	}
	sb, ok := svc.sandboxes.Get(sandboxID)
	if !ok {
		t.Fatalf("sandbox record should remain until RemovePodSandbox")
	}
	if sb.State != store.StateNotReady {
		t.Errorf("sandbox state = %q, want %q", sb.State, store.StateNotReady)
	}
	for _, id := range []string{appID, sidecarID} {
		c, ok := svc.containers.Get(id)
		if !ok {
			t.Fatalf("container %s should remain until RemovePodSandbox", id)
		}
		if c.State != store.ContainerExited {
			t.Errorf("container %s state = %q, want %q", id, c.State, store.ContainerExited)
		}
	}
}

func TestLinuxPodServiceTeardownToleratesMissingBackendPod(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")
	sidecarID := lpCreateStart(t, svc, sandboxID, "sidecar")

	if _, err := backend.Cleanup(ctx, sandboxID); err != nil {
		t.Fatalf("backend.Cleanup precondition: %v", err)
	}

	if _, err := svc.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: appID}); err != nil {
		t.Fatalf("StopContainer with missing backend pod: %v", err)
	}
	if c, _ := svc.containers.Get(appID); c.State != store.ContainerExited {
		t.Errorf("container state after missing-backend StopContainer = %q, want %q", c.State, store.ContainerExited)
	}
	if _, err := svc.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: sidecarID}); err != nil {
		t.Fatalf("RemoveContainer with missing backend pod: %v", err)
	}
	if _, ok := svc.containers.Get(sidecarID); ok {
		t.Errorf("sidecar should be removed from CRI store despite missing backend pod")
	}
	if _, err := svc.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("StopPodSandbox with missing backend pod: %v", err)
	}
	if _, err := svc.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("RemovePodSandbox with missing backend pod: %v", err)
	}
	if _, err := svc.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID}); status.Code(err) != codes.NotFound {
		t.Errorf("PodSandboxStatus after missing-backend remove = %v, want NotFound", err)
	}
}

func TestLinuxPodServiceCreateContainerIdempotentForPersistedCreatedContainer(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	req := &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app", Attempt: 1},
			Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"},
			Command:  []string{"/bin/sh", "-c", "sleep 300"},
		},
	}
	first, err := svc.CreateContainer(ctx, req)
	if err != nil {
		t.Fatalf("CreateContainer first: %v", err)
	}
	second, err := svc.CreateContainer(ctx, req)
	if err != nil {
		t.Fatalf("CreateContainer retry: %v", err)
	}
	if second.GetContainerId() != first.GetContainerId() {
		t.Fatalf("CreateContainer retry id = %q, want original %q", second.GetContainerId(), first.GetContainerId())
	}
	if _, err := svc.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: first.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer first: %v", err)
	}
	if _, err := svc.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: first.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer retry running: %v", err)
	}
}

func TestLinuxPodServiceCreateContainerBackendDuplicateFallsBackToRecreate(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")
	if err := svc.containers.Delete(appID); err != nil {
		t.Fatalf("delete CRI container record: %v", err)
	}

	_, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app", Attempt: 1},
			Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"},
			Command:  []string{"/bin/sh", "-c", "sleep 300"},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateContainer backend duplicate = %v, want FailedPrecondition", err)
	}
	sb, ok := svc.sandboxes.Get(sandboxID)
	if !ok || sb.State != store.StateNotReady {
		t.Fatalf("sandbox after backend duplicate = %+v, want NotReady", sb)
	}
}

func TestLinuxPodServiceReapsAgedNotReadySandbox(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")

	if _, err := svc.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	if _, ok := svc.sandboxes.Get(sandboxID); !ok {
		t.Fatalf("sandbox should remain immediately after StopPodSandbox")
	}
	if _, ok := svc.containers.Get(appID); !ok {
		t.Fatalf("container should remain immediately after StopPodSandbox")
	}

	if _, err := svc.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{}); err != nil {
		t.Fatalf("ListPodSandbox before grace: %v", err)
	}
	if _, ok := svc.sandboxes.Get(sandboxID); !ok {
		t.Fatalf("sandbox reaped before grace")
	}

	svc.now = func() time.Time { return time.Now().Add(linuxpodNotReadyReapGrace + time.Second) }
	if _, err := svc.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{}); err != nil {
		t.Fatalf("ListPodSandbox after grace: %v", err)
	}
	if _, ok := svc.sandboxes.Get(sandboxID); ok {
		t.Fatalf("aged NotReady sandbox was not reaped")
	}
	if _, ok := svc.containers.Get(appID); ok {
		t.Fatalf("container for aged NotReady sandbox was not reaped")
	}
}

func TestLinuxPodServiceStatusMarksBackendLostSandboxNotReady(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")
	sidecarID := lpCreateStart(t, svc, sandboxID, "sidecar")

	if _, err := backend.Cleanup(ctx, sandboxID); err != nil {
		t.Fatalf("backend.Cleanup precondition: %v", err)
	}

	sbStatus, err := svc.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID})
	if err != nil {
		t.Fatalf("PodSandboxStatus after backend loss: %v", err)
	}
	if got := sbStatus.GetStatus().GetState(); got != runtimeapi.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("sandbox state after backend loss = %s, want NOTREADY", got)
	}
	for _, id := range []string{appID, sidecarID} {
		st, err := svc.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
		if err != nil {
			t.Fatalf("ContainerStatus(%s) after backend loss: %v", id, err)
		}
		if got := st.GetStatus().GetState(); got != runtimeapi.ContainerState_CONTAINER_EXITED {
			t.Errorf("container %s state after backend loss = %s, want EXITED", id, got)
		}
		if got := st.GetStatus().GetReason(); got != "BackendLost" {
			t.Errorf("container %s reason after backend loss = %q, want BackendLost", id, got)
		}
	}
	list, err := svc.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{State: &runtimeapi.ContainerStateValue{State: runtimeapi.ContainerState_CONTAINER_RUNNING}},
	})
	if err != nil {
		t.Fatalf("ListContainers running after backend loss: %v", err)
	}
	if len(list.GetContainers()) != 0 {
		t.Fatalf("running containers after backend loss = %d, want 0", len(list.GetContainers()))
	}
	sandboxes, err := svc.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: &runtimeapi.PodSandboxFilter{State: &runtimeapi.PodSandboxStateValue{State: runtimeapi.PodSandboxState_SANDBOX_READY}},
	})
	if err != nil {
		t.Fatalf("ListPodSandbox ready after backend loss: %v", err)
	}
	if len(sandboxes.GetItems()) != 0 {
		t.Fatalf("ready sandboxes after backend loss = %d, want 0", len(sandboxes.GetItems()))
	}
}

func TestLinuxPodServiceBackendReconcilerMarksBackendLostSandboxNotReady(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")

	stop := svc.StartBackendReconciler(ctx, 10*time.Millisecond)
	defer stop()
	if _, err := backend.Cleanup(context.Background(), sandboxID); err != nil {
		t.Fatalf("backend.Cleanup precondition: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		sb, _ := svc.sandboxes.Get(sandboxID)
		c, _ := svc.containers.Get(appID)
		if sb.State == store.StateNotReady && c.State == store.ContainerExited {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("backend reconciler did not mark sandbox/container lost: sandbox=%s container=%s", sb.State, c.State)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestLinuxPodServiceRunPodSandboxReplacesNotReadySandbox(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	cfg := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
	}
	first, err := svc.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{Config: cfg})
	if err != nil {
		t.Fatalf("RunPodSandbox(first): %v", err)
	}
	firstID := first.GetPodSandboxId()
	containerID := lpCreateStart(t, svc, firstID, "app")
	if _, err := backend.Cleanup(ctx, firstID); err != nil {
		t.Fatalf("backend.Cleanup precondition: %v", err)
	}
	if _, err := svc.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: firstID}); err != nil {
		t.Fatalf("PodSandboxStatus after backend loss: %v", err)
	}

	second, err := svc.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{Config: cfg})
	if err != nil {
		t.Fatalf("RunPodSandbox(replacement): %v", err)
	}
	secondID := second.GetPodSandboxId()
	if secondID == "" || secondID == firstID {
		t.Fatalf("replacement sandbox ID = %q, first ID = %q", secondID, firstID)
	}
	if _, ok := svc.sandboxes.Get(firstID); ok {
		t.Fatalf("first NotReady sandbox %s should be discarded before replacement", firstID)
	}
	if _, ok := svc.containers.Get(containerID); ok {
		t.Fatalf("container from discarded sandbox %s should be removed", containerID)
	}
	if sb, ok := svc.sandboxes.Get(secondID); !ok || sb.State != store.StateReady {
		t.Fatalf("replacement sandbox state = %q ok=%t, want Ready", sb.State, ok)
	}
}

func TestLinuxPodServiceIdentityMismatchNotRunning(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	backend.ObservedIdentityFor["app"] = "macvz-rootfs-id=WRONG"
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	cresp, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
			Image:    &runtimeapi.ImageSpec{Image: "busybox"},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cresp.GetContainerId()
	_, err = svc.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("StartContainer with wrong identity = %v, want FailedPrecondition", err)
	}
	st, err := svc.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if st.GetStatus().GetState() == runtimeapi.ContainerState_CONTAINER_RUNNING {
		t.Errorf("container must not be Running after identity mismatch")
	}
	if st.GetStatus().GetReason() != "IdentityVerificationFailed" {
		t.Errorf("reason = %q, want IdentityVerificationFailed", st.GetStatus().GetReason())
	}
}

func TestLinuxPodServiceRecreatesExitedSameName(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	firstID := lpCreateStart(t, svc, sandboxID, "app")
	if _, err := svc.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: firstID}); err != nil {
		t.Fatalf("StopContainer(first): %v", err)
	}

	resp, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app", Attempt: 1},
			Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"},
			Command:  []string{"/bin/sh", "-c", "sleep 300"},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer(replacement): %v", err)
	}
	replacementID := resp.GetContainerId()
	if replacementID == "" || replacementID == firstID {
		t.Fatalf("replacement container ID = %q, first ID = %q", replacementID, firstID)
	}
	if _, err := svc.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: replacementID}); err != nil {
		t.Fatalf("StartContainer(replacement): %v", err)
	}

	firstStatus, err := svc.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: firstID})
	if err != nil {
		t.Fatalf("ContainerStatus(first): %v", err)
	}
	if got := firstStatus.GetStatus().GetState(); got != runtimeapi.ContainerState_CONTAINER_EXITED {
		t.Errorf("first container state = %s, want EXITED", got)
	}
	replacementStatus, err := svc.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: replacementID})
	if err != nil {
		t.Fatalf("ContainerStatus(replacement): %v", err)
	}
	if got := replacementStatus.GetStatus().GetState(); got != runtimeapi.ContainerState_CONTAINER_RUNNING {
		t.Errorf("replacement container state = %s, want RUNNING", got)
	}
}

func TestLinuxPodServiceTranslatesKubeletMounts(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	podsDir := t.TempDir()
	source := filepath.Join(podsDir, "uid", "volumes", "kubernetes.io~configmap", "web")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	svc, err := NewLinuxPodService(LinuxPodOptions{
		Backend: backend,
		Mounts:  MountPolicy{KubeletPodsDir: podsDir},
	})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	resp, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
			Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"},
			Annotations: map[string]string{
				"io.kubernetes.container.hash": "fixture-hash",
			},
			Mounts: []*runtimeapi.Mount{
				{HostPath: source, ContainerPath: "/www", Readonly: true},
				{ContainerPath: "/cache"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	statusResp, err := svc.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: resp.GetContainerId()})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	mounts := statusResp.GetStatus().GetMounts()
	if len(mounts) != 2 {
		t.Fatalf("ContainerStatus mounts = %d, want 2", len(mounts))
	}
	if mounts[0].GetHostPath() != source || mounts[0].GetContainerPath() != "/www" || !mounts[0].GetReadonly() {
		t.Errorf("bind mount = %+v, want %s -> /www readonly", mounts[0], source)
	}
	if mounts[1].GetHostPath() != "" || mounts[1].GetContainerPath() != "/cache" {
		t.Errorf("tmpfs mount = %+v, want guest-local /cache", mounts[1])
	}
	if got := statusResp.GetStatus().GetAnnotations()["io.kubernetes.container.hash"]; got != "fixture-hash" {
		t.Errorf("status annotation hash = %q, want fixture-hash", got)
	}
}

func TestLinuxPodServiceKubeletSurfaces(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()
	logDir := t.TempDir()

	sbResp, err := svc.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata:     &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
			LogDirectory: logDir,
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	sandboxID := sbResp.GetPodSandboxId()
	cresp, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
			Image:    &runtimeapi.ImageSpec{Image: "busybox"},
			Command:  []string{"sleep", "300"},
			LogPath:  "app/0.log",
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cresp.GetContainerId()
	if _, err := svc.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	st, err := svc.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	wantLog := filepath.Join(logDir, "app/0.log")
	if got := st.GetStatus().GetLogPath(); got != wantLog {
		t.Errorf("ContainerStatus LogPath = %q, want %q", got, wantLog)
	}
	if _, err := os.Stat(wantLog); err != nil {
		t.Fatalf("backend should create CRI log file %s: %v", wantLog, err)
	}
	if _, err := svc.ReopenContainerLog(ctx, &runtimeapi.ReopenContainerLogRequest{ContainerId: id}); err != nil {
		t.Fatalf("ReopenContainerLog: %v", err)
	}

	exec, err := svc.ExecSync(ctx, &runtimeapi.ExecSyncRequest{ContainerId: id, Cmd: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
	if exec.GetExitCode() != 0 || !strings.Contains(string(exec.GetStdout()), "echo hi") {
		t.Errorf("ExecSync result = exit %d stdout %q", exec.GetExitCode(), exec.GetStdout())
	}

	stats, err := svc.ContainerStats(ctx, &runtimeapi.ContainerStatsRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	if stats.GetStats().GetCpu() == nil || stats.GetStats().GetMemory() == nil {
		t.Fatalf("ContainerStats should include CPU and memory samples: %+v", stats.GetStats())
	}
	listStats, err := svc.ListContainerStats(ctx, &runtimeapi.ListContainerStatsRequest{
		Filter: &runtimeapi.ContainerStatsFilter{Id: id},
	})
	if err != nil {
		t.Fatalf("ListContainerStats: %v", err)
	}
	if len(listStats.GetStats()) != 1 {
		t.Fatalf("ListContainerStats len = %d, want 1", len(listStats.GetStats()))
	}
	podStats, err := svc.PodSandboxStats(ctx, &runtimeapi.PodSandboxStatsRequest{PodSandboxId: sandboxID})
	if err != nil {
		t.Fatalf("PodSandboxStats: %v", err)
	}
	if len(podStats.GetStats().GetLinux().GetContainers()) != 1 {
		t.Errorf("PodSandboxStats container count = %d, want 1", len(podStats.GetStats().GetLinux().GetContainers()))
	}
}

func TestLinuxPodServiceLogRootOverride(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	logRoot := t.TempDir()
	svc, err := NewLinuxPodService(LinuxPodOptions{Backend: backend, LogRoot: logRoot})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	ctx := context.Background()

	sbResp, err := svc.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata:     &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
			LogDirectory: "/var/log/pods/default_pod_uid-1",
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	cresp, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sbResp.GetPodSandboxId(),
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
			Image:    &runtimeapi.ImageSpec{Image: "busybox"},
			Command:  []string{"sleep", "300"},
			LogPath:  "app/0.log",
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cresp.GetContainerId()
	if _, err := svc.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	st, err := svc.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	wantLog := filepath.Join(logRoot, "default_pod_uid-1", "app/0.log")
	if got := st.GetStatus().GetLogPath(); got != wantLog {
		t.Errorf("ContainerStatus LogPath = %q, want override path %q", got, wantLog)
	}
	if _, err := os.Stat(wantLog); err != nil {
		t.Fatalf("backend should create overridden CRI log file %s: %v", wantLog, err)
	}
}

func TestLinuxPodServiceImageFsInfoUsesKubeletPortableMountpoint(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)

	resp, err := svc.ImageFsInfo(context.Background(), &runtimeapi.ImageFsInfoRequest{})
	if err != nil {
		t.Fatalf("ImageFsInfo: %v", err)
	}
	fs := resp.GetImageFilesystems()
	if len(fs) != 1 {
		t.Fatalf("ImageFsInfo returned %d filesystem(s), want 1", len(fs))
	}
	if got := fs[0].GetFsId().GetMountpoint(); got != "/" {
		t.Errorf("ImageFsInfo mountpoint = %q, want /", got)
	}
	if fs[0].GetTimestamp() == 0 {
		t.Error("ImageFsInfo timestamp must be populated")
	}
}

func TestLinuxPodServiceImageStatusIncludesKubeletRequiredSize(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()
	ref := "busybox:1.36.1"

	if _, err := svc.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: ref}}); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	statusResp, err := svc.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{Image: &runtimeapi.ImageSpec{Image: ref}})
	if err != nil {
		t.Fatalf("ImageStatus: %v", err)
	}
	if statusResp.GetImage().GetId() == "" {
		t.Fatal("ImageStatus image ID must be populated for kubelet")
	}
	if statusResp.GetImage().GetSize() == 0 {
		t.Fatal("ImageStatus image size must be non-zero for kubelet")
	}
	listResp, err := svc.ListImages(ctx, &runtimeapi.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(listResp.GetImages()) != 1 || listResp.GetImages()[0].GetSize() == 0 {
		t.Fatalf("ListImages = %+v, want one image with non-zero size", listResp.GetImages())
	}
}

func TestLinuxPodServiceUnsupportedSurfacesAreHonest(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	backend.Capabilities.Logs = false
	backend.Capabilities.Exec = false
	backend.Capabilities.Stats = false
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()

	sandboxID := lpRunSandbox(t, svc)
	id := lpCreateStart(t, svc, sandboxID, "app")

	if _, err := svc.ReopenContainerLog(ctx, &runtimeapi.ReopenContainerLogRequest{ContainerId: id}); status.Code(err) != codes.Unimplemented {
		t.Errorf("ReopenContainerLog unsupported = %v, want Unimplemented", err)
	}
	if _, err := svc.ExecSync(ctx, &runtimeapi.ExecSyncRequest{ContainerId: id, Cmd: []string{"true"}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("ExecSync unsupported = %v, want Unimplemented", err)
	}
	stats, err := svc.ContainerStats(ctx, &runtimeapi.ContainerStatsRequest{ContainerId: id})
	if err != nil {
		t.Fatalf("ContainerStats unsupported should degrade to attributes-only, got %v", err)
	}
	if stats.GetStats().GetCpu() != nil || stats.GetStats().GetMemory() != nil {
		t.Errorf("unsupported stats should not fake samples: %+v", stats.GetStats())
	}
	if _, err := svc.Exec(ctx, &runtimeapi.ExecRequest{ContainerId: id, Cmd: []string{"true"}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("streaming Exec without streaming server = %v, want FailedPrecondition", err)
	}
	if _, err := svc.Attach(ctx, &runtimeapi.AttachRequest{ContainerId: id}); status.Code(err) != codes.Unimplemented {
		t.Errorf("Attach = %v, want Unimplemented", err)
	}
	if _, err := svc.PortForward(ctx, &runtimeapi.PortForwardRequest{PodSandboxId: sandboxID}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("PortForward without streaming server = %v, want FailedPrecondition", err)
	}
}

func TestLinuxPodServiceStreamingURLHandoff(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	id := lpCreateStart(t, svc, sandboxID, "app")
	fss := &fakeStreamingServer{}
	svc.SetStreamingServer(fss)

	exec, err := svc.Exec(ctx, &runtimeapi.ExecRequest{ContainerId: id, Cmd: []string{"echo", "hi"}, Stdout: true})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if exec.GetUrl() != "http://stream/exec/tok" || fss.execReq.GetContainerId() != id {
		t.Fatalf("Exec handoff = url %q req %+v", exec.GetUrl(), fss.execReq)
	}
	pf, err := svc.PortForward(ctx, &runtimeapi.PortForwardRequest{PodSandboxId: sandboxID, Port: []int32{8080}})
	if err != nil {
		t.Fatalf("PortForward: %v", err)
	}
	if pf.GetUrl() != "http://stream/portforward/tok" || fss.pfReq.GetPodSandboxId() != sandboxID {
		t.Fatalf("PortForward handoff = url %q req %+v", pf.GetUrl(), fss.pfReq)
	}
}

func TestLinuxPodStreamingRuntimeExecUsesBackendExecSync(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	id := lpCreateStart(t, svc, sandboxID, "app")

	var stdout, stderr bytes.Buffer
	err := svc.StreamingRuntime().Exec(ctx, id, []string{"echo", "hi"}, nil, nopWriteCloser{&stdout}, nopWriteCloser{&stderr}, false, nil)
	if err != nil {
		t.Fatalf("streaming Exec callback: %v", err)
	}
	if got := stdout.String(); got != "echo hi\n" {
		t.Errorf("stdout = %q, want backend ExecSync output", got)
	}
	if !strings.Contains(stderr.String(), "simulated exec") {
		t.Errorf("stderr = %q, want backend ExecSync stderr", stderr.String())
	}
}

type nopWriteCloser struct{ *bytes.Buffer }

func (n nopWriteCloser) Close() error { return nil }

func TestLinuxPodPortForwardTargetPrefersSandboxVMIP(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	sandboxID := lpRunSandbox(t, svc)
	lpCreateStart(t, svc, sandboxID, "app")
	sb, ok := svc.sandboxes.Get(sandboxID)
	if !ok {
		t.Fatalf("sandbox %s missing", sandboxID)
	}
	sb.Network.VMIP = "192.168.66.2"
	sb.Network.PodIP = "10.244.102.2"
	sb.Network.Interface = "bridge100"
	if err := svc.sandboxes.Put(&sb); err != nil {
		t.Fatalf("persist sandbox network: %v", err)
	}
	target, err := svc.linuxpodPortForwardTarget(sandboxID)
	if err != nil {
		t.Fatalf("linuxpodPortForwardTarget: %v", err)
	}
	if target.host != "192.168.66.2" || target.iface != "bridge100" {
		t.Fatalf("target = %+v, want VM IP on bridge100", target)
	}
}

// TestLinuxPodServiceRestartRecovery proves a restarted adapter reconciles a
// persisted container whose backend no longer knows it to Exited, without
// trusting stale identity evidence.
func TestLinuxPodServiceRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	sb, _, err := store.New(filepath.Join(dir, "sandboxes"))
	if err != nil {
		t.Fatalf("sandbox store: %v", err)
	}
	cs, _, err := store.NewContainerStore(filepath.Join(dir, "containers"))
	if err != nil {
		t.Fatalf("container store: %v", err)
	}

	backend := linuxpod.NewFakeBackend()
	svc, err := NewLinuxPodService(LinuxPodOptions{Backend: backend, Sandboxes: sb, Containers: cs})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")

	// Simulate the adapter restarting against a fresh backend that lost the Pod VM
	// (e.g. helper restarted): reopen the stores and recover.
	sb2, _, _ := store.New(filepath.Join(dir, "sandboxes"))
	cs2, _, _ := store.NewContainerStore(filepath.Join(dir, "containers"))
	freshBackend := linuxpod.NewFakeBackend()
	svc2, err := NewLinuxPodService(LinuxPodOptions{Backend: freshBackend, Sandboxes: sb2, Containers: cs2})
	if err != nil {
		t.Fatalf("reopen NewLinuxPodService: %v", err)
	}
	// Before recovery the persisted record still says Running.
	if c, _ := svc2.containers.Get(appID); c.State != store.ContainerRunning {
		t.Fatalf("pre-recovery state = %s, want Running", c.State)
	}
	svc2.RecoverContainers(ctx)
	c, ok := svc2.containers.Get(appID)
	if !ok {
		t.Fatalf("container missing after recovery")
	}
	if c.State != store.ContainerExited {
		t.Errorf("post-recovery state = %s, want Exited (backend lost the container)", c.State)
	}
}

// restartedLinuxPodService reopens the persisted stores against the given backend,
// modeling a macvz-cri restart that rediscovers what it was running.
func restartedLinuxPodService(t *testing.T, dir string, backend linuxpod.Backend) *LinuxPodService {
	t.Helper()
	sb, _, err := store.New(filepath.Join(dir, "sandboxes"))
	if err != nil {
		t.Fatalf("reopen sandbox store: %v", err)
	}
	cs, _, err := store.NewContainerStore(filepath.Join(dir, "containers"))
	if err != nil {
		t.Fatalf("reopen container store: %v", err)
	}
	svc, err := NewLinuxPodService(LinuxPodOptions{Backend: backend, Sandboxes: sb, Containers: cs})
	if err != nil {
		t.Fatalf("reopen NewLinuxPodService: %v", err)
	}
	return svc
}

type adoptErrorBackend struct {
	*linuxpod.FakeBackend
	err error
}

func (b adoptErrorBackend) Adopt(context.Context, string) (linuxpod.AdoptionResult, error) {
	return linuxpod.AdoptionResult{}, b.err
}

type missingPodStatusBackend struct {
	*linuxpod.FakeBackend
}

func (b missingPodStatusBackend) PodStatus(context.Context, string) (linuxpod.PodStatus, error) {
	return linuxpod.PodStatus{}, linuxpod.ErrPodNotFound
}

// TestLinuxPodServiceAdoptsLiveVMAfterHelperRestart proves the #138 happy path: when
// the helper restarts but keeps the Pod VM, the adapter's adoption pass reattaches
// the sandbox so it stays Ready with its container Running - kubelet never recreates
// the Pod, and the periodic reconciler leaves it Ready because PodStatus now answers.
func TestLinuxPodServiceAdoptsLiveVMAfterHelperRestart(t *testing.T) {
	dir := t.TempDir()
	sb, _, _ := store.New(filepath.Join(dir, "sandboxes"))
	cs, _, _ := store.NewContainerStore(filepath.Join(dir, "containers"))

	backend := linuxpod.NewFakeBackend()
	svc, err := NewLinuxPodService(LinuxPodOptions{Backend: backend, Sandboxes: sb, Containers: cs})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")

	// Helper restarts; the Pod VM survives. The adapter restarts too and runs the
	// adoption pass before the fail-fast reconciler.
	backend.SimulateHelperRestart()
	svc2 := restartedLinuxPodService(t, dir, backend)
	svc2.AdoptSandboxes(ctx)
	svc2.RecoverContainers(ctx)

	if sbRec, ok := svc2.sandboxes.Get(sandboxID); !ok || sbRec.State != store.StateReady {
		t.Fatalf("sandbox after adoption = %+v, want Ready (adopted, no recreate)", sbRec)
	}
	if c, _ := svc2.containers.Get(appID); c.State != store.ContainerRunning {
		t.Fatalf("container after adoption = %s, want Running", c.State)
	}
	// The periodic reconciler must not undo the adoption: PodStatus answers for the
	// reattached VM, so the sandbox stays Ready.
	svc2.reconcileAllSandboxBackendState(ctx)
	if sbRec, _ := svc2.sandboxes.Get(sandboxID); sbRec.State != store.StateReady {
		t.Fatalf("sandbox after reconcile = %s, want Ready (live VM adopted)", sbRec.State)
	}
	// Adoption is usable, not just Running-on-paper: exec reaches the live container.
	if _, err := svc2.ExecSync(ctx, &runtimeapi.ExecSyncRequest{
		ContainerId: appID, Cmd: []string{"true"}, Timeout: 5,
	}); err != nil {
		t.Fatalf("ExecSync on adopted container: %v", err)
	}
}

func TestLinuxPodServiceReconcilerAdoptsAfterHelperRestart(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")

	// macvz-cri stayed alive, but the helper's status path temporarily lost its
	// in-memory Pod handle. The reconciler must try Adopt before BackendLost.
	svc.backend = missingPodStatusBackend{FakeBackend: backend}
	svc.reconcileSandboxBackendState(ctx, sandboxID)

	if sbRec, ok := svc.sandboxes.Get(sandboxID); !ok || sbRec.State != store.StateReady {
		t.Fatalf("sandbox after reconciler adoption = %+v, want Ready", sbRec)
	}
	if c, _ := svc.containers.Get(appID); c.State != store.ContainerRunning {
		t.Fatalf("container after reconciler adoption = %s, want Running", c.State)
	}
	if _, err := svc.ExecSync(ctx, &runtimeapi.ExecSyncRequest{
		ContainerId: appID, Cmd: []string{"true"}, Timeout: 5,
	}); err != nil {
		t.Fatalf("ExecSync after reconciler adoption: %v", err)
	}
}

func TestLinuxPodServiceRunningContainerBackendMissFallsBackToRecreate(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")
	sidecarID := lpCreateStart(t, svc, sandboxID, "sidecar")

	app, ok := svc.containers.Get(appID)
	if !ok || app.LinuxPod == nil {
		t.Fatalf("app container missing LinuxPod mapping")
	}
	if err := backend.RemoveContainer(ctx, linuxpod.Ref{
		PodID:       sandboxID,
		ContainerID: app.LinuxPod.BackendContainerID,
	}); err != nil {
		t.Fatalf("RemoveContainer backend precondition: %v", err)
	}

	_, err := svc.ExecSync(ctx, &runtimeapi.ExecSyncRequest{
		ContainerId: appID, Cmd: []string{"true"}, Timeout: 5,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ExecSync after stale backend container = %v, want FailedPrecondition", err)
	}

	sb, ok := svc.sandboxes.Get(sandboxID)
	if !ok || sb.State != store.StateNotReady {
		t.Fatalf("sandbox after stale backend container = %+v, want NotReady", sb)
	}
	for _, id := range []string{appID, sidecarID} {
		c, ok := svc.containers.Get(id)
		if !ok {
			t.Fatalf("container %s missing", id)
		}
		if c.State != store.ContainerExited || c.Reason != "BackendLost" {
			t.Fatalf("container %s after stale backend container = state %s reason %q, want Exited/BackendLost",
				id, c.State, c.Reason)
		}
	}
}

// TestLinuxPodServiceFallsBackToRecreateWhenVMLost proves the #138 fallback remains
// intact: when the helper restart loses the Pod VM, adoption fails and the sandbox is
// driven to NotReady with its container BackendLost so kubelet recreates it - never a
// stale Running-but-unusable Pod.
func TestLinuxPodServiceFallsBackToRecreateWhenVMLost(t *testing.T) {
	dir := t.TempDir()
	sb, _, _ := store.New(filepath.Join(dir, "sandboxes"))
	cs, _, _ := store.NewContainerStore(filepath.Join(dir, "containers"))

	backend := linuxpod.NewFakeBackend()
	svc, err := NewLinuxPodService(LinuxPodOptions{Backend: backend, Sandboxes: sb, Containers: cs})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")

	// Helper restarts and the Pod VM dies with it: adoption is impossible.
	backend.VMSurvivesRestart[sandboxID] = false
	backend.SimulateHelperRestart()
	svc2 := restartedLinuxPodService(t, dir, backend)
	svc2.AdoptSandboxes(ctx)

	if sbRec, _ := svc2.sandboxes.Get(sandboxID); sbRec.State != store.StateNotReady {
		t.Fatalf("sandbox after lost VM = %s, want NotReady (fall back to recreate)", sbRec.State)
	}
	c, _ := svc2.containers.Get(appID)
	if c.State != store.ContainerExited || c.Reason != "BackendLost" {
		t.Fatalf("container after lost VM = state %s reason %q, want Exited/BackendLost", c.State, c.Reason)
	}
}

func TestLinuxPodServiceAdoptionTransientErrorRetainsState(t *testing.T) {
	dir := t.TempDir()
	sb, _, _ := store.New(filepath.Join(dir, "sandboxes"))
	cs, _, _ := store.NewContainerStore(filepath.Join(dir, "containers"))

	base := linuxpod.NewFakeBackend()
	svc, err := NewLinuxPodService(LinuxPodOptions{Backend: base, Sandboxes: sb, Containers: cs})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")

	svc2 := restartedLinuxPodService(t, dir, adoptErrorBackend{
		FakeBackend: base,
		err:         errors.New("temporary helper transport failure"),
	})
	svc2.AdoptSandboxes(ctx)

	if sbRec, _ := svc2.sandboxes.Get(sandboxID); sbRec.State != store.StateReady {
		t.Fatalf("sandbox after transient adoption error = %s, want Ready", sbRec.State)
	}
	c, _ := svc2.containers.Get(appID)
	if c.State != store.ContainerRunning || c.Reason == "BackendLost" {
		t.Fatalf("container after transient adoption error = state %s reason %q, want Running/non-BackendLost", c.State, c.Reason)
	}
}

// TestLinuxPodServiceAdoptionIncompleteFallsBack proves adoption never leaves a stale
// Running Pod: if the helper reattaches the VM but a recorded-Running container is not
// live, the adapter funnels the whole sandbox into the recreate fallback.
func TestLinuxPodServiceAdoptionIncompleteFallsBack(t *testing.T) {
	ctx := context.Background()
	backend := linuxpod.NewFakeBackend()
	svc := newLinuxPodTestService(t, backend)
	sandboxID := lpRunSandbox(t, svc)
	appID := lpCreateStart(t, svc, sandboxID, "app")

	// Adopt reports the VM back but WITHOUT the recorded-Running container.
	c, _ := svc.containers.Get(appID)
	res := linuxpod.AdoptionResult{PodID: sandboxID, Adopted: true} // no Containers
	svc.mu.Lock()
	ok := svc.applyAdoptionLocked(ctx, sandboxID, res)
	svc.mu.Unlock()
	if ok {
		t.Fatalf("applyAdoptionLocked must reject adoption missing a running container")
	}
	if sbRec, _ := svc.sandboxes.Get(sandboxID); sbRec.State != store.StateNotReady {
		t.Fatalf("sandbox after incomplete adoption = %s, want NotReady", sbRec.State)
	}
	got, _ := svc.containers.Get(c.ID)
	if got.State != store.ContainerExited || got.Reason != "BackendLost" {
		t.Fatalf("container after incomplete adoption = state %s reason %q, want Exited/BackendLost", got.State, got.Reason)
	}
}
