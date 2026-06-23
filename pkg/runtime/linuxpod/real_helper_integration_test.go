package linuxpod

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRealLinuxPodHelperLifecycle proves the real Apple Containerization LinuxPod
// helper (CRI-L1, #126) drives the same NDJSON contract as the stub but over real
// micro-VMs: it boots a LinuxPod, starts an app container, then late-creates and
// starts a sidecar *after* the app is already running, gating each on rootfs
// identity verification through the handoff evidence channel (CRI-R16). It asserts
// the app and sidecar share one Pod sandbox namespace, both reach localhost, the
// late identity handoff verifies, and Cleanup leaves no residual state.
//
// Gated behind MACVZ_LINUXPOD_REAL_HELPER=1 because it boots real VMs and needs the
// operator-provided Apple Containerization checkout (test/e2e/cri-linuxpod/
// containerization, `git clone apple/containerization`), a kernel
// (MACVZ_LINUXPOD_KERNEL or containerization/bin/vmlinux), and a vminit init image
// in the local image store. The default test run proves the contract hermetically
// with the Go fake and the stub; this is the live, real-VM proof.
func TestRealLinuxPodHelperLifecycle(t *testing.T) {
	if os.Getenv("MACVZ_LINUXPOD_REAL_HELPER") != "1" {
		t.Skip("set MACVZ_LINUXPOD_REAL_HELPER=1 to run the real LinuxPod helper lifecycle test")
	}

	bin, kernel := buildRealHelper(t)

	dir, err := os.MkdirTemp("", "lprh")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "h.sock")
	workDir := filepath.Join(dir, "work")

	// Boot can take a while (VM + image pull); keep the helper alive for the run.
	procCtx, procCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer procCancel()
	args := []string{"--socket", socket, "--kernel", kernel, "--work-dir", workDir}
	if root := os.Getenv("MACVZ_CONTAINERIZATION_ROOT"); root != "" {
		args = append(args, "--containerization-root", root)
	}
	cmd := exec.CommandContext(procCtx, bin, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start real helper: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	waitForSocket(t, socket)

	client := NewSocketClient(socket)
	const podID = "pod-l1"

	// Each op gets a generous deadline; a real VM boot/stage exceeds the client's
	// 30s default.
	call := func(d time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), d)
	}

	ctx, cancel := call(2 * time.Minute)
	info, err := client.Ping(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if info.Simulated {
		t.Errorf("real helper must report simulated=false, got %+v", info)
	}
	if info.Name != "linuxpod-helper" || info.ProtocolVersion != ProtocolVersion {
		t.Errorf("unexpected helper info: %+v (want name=linuxpod-helper version=%d)", info, ProtocolVersion)
	}

	ctx, cancel = call(5 * time.Minute)
	pod, err := client.CreatePod(ctx, PodSpec{ID: podID, Hostname: "macvz-l1", CPUs: 2, MemoryBytes: 1 << 30})
	cancel()
	if err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if pod.Phase != "Running" || pod.SandboxNamespace == "" {
		t.Fatalf("pod not running with a namespace: %+v", pod)
	}
	defer func() {
		ctx, cancel := call(3 * time.Minute)
		rep, err := client.Cleanup(ctx, podID)
		cancel()
		if err != nil {
			t.Errorf("Cleanup: %v", err)
			return
		}
		if !rep.PodRemoved || rep.StaleState {
			t.Errorf("cleanup left residual state: %+v", rep)
		}
		// Idempotent: a second cleanup of a now-unknown pod is a no-op success.
		ctx2, cancel2 := call(time.Minute)
		rep2, err := client.Cleanup(ctx2, podID)
		cancel2()
		if err != nil {
			t.Errorf("second Cleanup: %v", err)
		}
		if rep2.PodRemoved || rep2.StaleState {
			t.Errorf("second cleanup should report nothing removed: %+v", rep2)
		}
	}()

	app := startContainerLive(t, client, call, podID, "app",
		"macvz-rootfs-id=app", []string{"/bin/sh", "-c", "exec sleep 600"}, false)
	sidecar := startContainerLive(t, client, call, podID, "sidecar",
		"macvz-rootfs-id=sidecar", []string{"/bin/sh", "-c", "exec sleep 600"}, true)

	assertSharedNamespaceAndIdentity(t, app, sidecar)
	t.Logf("LIVE EVIDENCE: pod=%s sandboxNamespace=%q (shared by app+sidecar)", podID, app.SandboxNamespace)
	t.Logf("LIVE EVIDENCE: app   id=%s phase=%s identityVerified=%v observed=%q createdAfterPodRunning=%v localhostReachable=%v",
		app.ID, app.Phase, app.IdentityVerified, app.ObservedIdentity, app.CreatedAfterPodRunning, app.LocalhostReachable)
	t.Logf("LIVE EVIDENCE: sidecar id=%s phase=%s identityVerified=%v observed=%q createdAfterPodRunning=%v localhostReachable=%v",
		sidecar.ID, sidecar.Phase, sidecar.IdentityVerified, sidecar.ObservedIdentity, sidecar.CreatedAfterPodRunning, sidecar.LocalhostReachable)

	// The real helper does not yet back the kubelet surfaces (CRI-L4 #129); it must
	// say so honestly rather than fake results.
	if info.Capabilities.Logs || info.Capabilities.Exec || info.Capabilities.Stats {
		t.Errorf("real helper should advertise no kubelet surfaces yet, got %+v", info.Capabilities)
	}
	ctx, cancel = call(time.Minute)
	if _, err := client.ExecSync(ctx, ExecRequest{PodID: podID, ContainerID: app.ID, Command: []string{"echo", "hi"}}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ExecSync = %v, want ErrUnsupported", err)
	}
	if _, err := client.ContainerStats(ctx, Ref{PodID: podID, ContainerID: app.ID}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ContainerStats = %v, want ErrUnsupported", err)
	}
	if _, err := client.ContainerLogPath(ctx, Ref{PodID: podID, ContainerID: app.ID}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ContainerLogPath = %v, want ErrUnsupported", err)
	}
	cancel()
}

// startContainerLive runs PrepareContainerRootfs -> CreateContainer -> StartContainer
// for one container against the real helper and asserts it reached Running with a
// verified identity, with the expected late-sidecar ordering flag.
func startContainerLive(
	t *testing.T,
	client *HelperClient,
	call func(time.Duration) (context.Context, context.CancelFunc),
	podID, name, identity string,
	command []string,
	wantLate bool,
) ContainerStatus {
	t.Helper()

	ctx, cancel := call(3 * time.Minute)
	rootfs, err := client.PrepareContainerRootfs(ctx, RootfsRequest{
		PodID: podID, ContainerName: name, Image: "busybox", ExpectedIdentity: identity,
	})
	cancel()
	if err != nil {
		t.Fatalf("PrepareContainerRootfs(%s): %v", name, err)
	}

	ctx, cancel = call(2 * time.Minute)
	created, err := client.CreateContainer(ctx, CreateRequest{
		PodID: podID, Name: name, RootfsToken: rootfs.Token, Command: command,
	})
	cancel()
	if err != nil {
		t.Fatalf("CreateContainer(%s): %v", name, err)
	}
	if created.CreatedAfterPodRunning != wantLate {
		t.Errorf("%s createdAfterPodRunning = %v, want %v", name, created.CreatedAfterPodRunning, wantLate)
	}

	ctx, cancel = call(3 * time.Minute)
	started, err := client.StartContainer(ctx, Ref{PodID: podID, ContainerID: created.ID})
	cancel()
	if err != nil {
		t.Fatalf("StartContainer(%s): %v", name, err)
	}
	if started.Phase != "Running" || !started.IdentityVerified {
		t.Fatalf("%s not running+verified: %+v", name, started)
	}
	if started.ObservedIdentity != identity {
		t.Errorf("%s observed identity %q != expected %q", name, started.ObservedIdentity, identity)
	}
	return started
}

// buildRealHelper builds the real linuxpod-helper from the cri-linuxpod package and
// returns its binary path and the kernel path to drive it with. It skips (not
// fails) when the operator-provided Apple Containerization checkout or kernel is
// absent, since those are heavy, externally provisioned dependencies.
func buildRealHelper(t *testing.T) (bin, kernel string) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file for repo-relative paths")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	pocDir := filepath.Join(repoRoot, "test", "e2e", "cri-linuxpod")

	if _, err := os.Stat(filepath.Join(pocDir, "containerization", "Package.swift")); err != nil {
		t.Skipf("Apple Containerization checkout missing at %s/containerization (git clone apple/containerization): %v", pocDir, err)
	}
	kernel = os.Getenv("MACVZ_LINUXPOD_KERNEL")
	if kernel == "" {
		for _, cand := range []string{
			filepath.Join(pocDir, "containerization", "bin", "vmlinux"),
			filepath.Join(pocDir, "containerization", "bin", "vmlinux-arm64"),
		} {
			if _, err := os.Stat(cand); err == nil {
				kernel = cand
				break
			}
		}
	}
	if kernel == "" {
		t.Skip("no kernel found; set MACVZ_LINUXPOD_KERNEL or run `make -C containerization fetch-default-kernel`")
	}

	if b := os.Getenv("MACVZ_LINUXPOD_REAL_HELPER_BIN"); b != "" {
		return b, kernel
	}
	build := exec.Command("swift", "build", "-c", "debug", "--product", "linuxpod-helper")
	build.Dir = pocDir
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("swift build linuxpod-helper: %v", err)
	}
	show := exec.Command("swift", "build", "-c", "debug", "--product", "linuxpod-helper", "--show-bin-path")
	show.Dir = pocDir
	binDirOut, err := show.Output()
	if err != nil {
		t.Fatalf("resolve helper bin path: %v", err)
	}
	binDir := filepath.Clean(strings.TrimSpace(string(binDirOut)))
	binPath := filepath.Join(binDir, "linuxpod-helper")

	// Booting a VZVirtualMachine needs the com.apple.security.virtualization
	// entitlement; codesign the freshly built binary the way the PoC does.
	sign := exec.Command("codesign", "--force", "--sign", "-", "--timestamp=none",
		"--entitlements", filepath.Join(pocDir, "linuxpod-helper.entitlements"), binPath)
	sign.Stdout, sign.Stderr = os.Stderr, os.Stderr
	if err := sign.Run(); err != nil {
		t.Fatalf("codesign helper with virtualization entitlement: %v", err)
	}
	return binPath, kernel
}
