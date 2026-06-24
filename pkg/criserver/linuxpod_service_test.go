package criserver

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
