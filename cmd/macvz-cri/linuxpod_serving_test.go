package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// TestLiveLinuxPodServingThroughHelper proves the full CRI-L2 (#127) chain end to
// end over real transports: a CRI gRPC client drives macvz-cri's LinuxPod serving
// path (serveLinuxPod), which forwards over the NDJSON socket to the Swift helper
// stub, which models the LinuxPod lifecycle. It runs the kubelet ordering —
// RunPodSandbox, app create/start, late sidecar create/start after the app is
// Running — and asserts shared sandbox namespace, identity verification, and clean
// teardown.
//
// Gated behind MACVZ_LINUXPOD_HELPER=1 (needs the Swift toolchain); the default
// run proves the same serving logic hermetically against the fake backend in
// pkg/criserver.
func TestLiveLinuxPodServingThroughHelper(t *testing.T) {
	if os.Getenv("MACVZ_LINUXPOD_HELPER") != "1" {
		t.Skip("set MACVZ_LINUXPOD_HELPER=1 to run the live LinuxPod CRI serving smoke")
	}

	helperBin := os.Getenv("MACVZ_LINUXPOD_HELPER_BIN")
	if helperBin == "" {
		helperBin = buildSwiftHelperForServing(t)
	}

	// Short dirs to stay under the unix sun_path limit.
	dir, err := os.MkdirTemp("", "lps")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)
	helperSock := filepath.Join(dir, "helper.sock")
	criSock := filepath.Join(dir, "cri.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// Start the helper. Extra args (e.g. the real CRI-L1 helper's --kernel/--work-dir)
	// come from MACVZ_LINUXPOD_HELPER_ARGS so this same test can drive either the
	// in-memory Swift stub (--socket only) or the real Apple Containerization helper
	// that boots a live LinuxPod VM.
	helperArgs := []string{"--socket", helperSock}
	if extra := os.Getenv("MACVZ_LINUXPOD_HELPER_ARGS"); extra != "" {
		helperArgs = append(helperArgs, strings.Fields(extra)...)
	}
	helper := exec.CommandContext(ctx, helperBin, helperArgs...)
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		t.Fatalf("start swift helper: %v", err)
	}
	defer func() { _ = helper.Process.Kill(); _, _ = helper.Process.Wait() }()
	waitForFile(t, helperSock)

	// Serve macvz-cri's LinuxPod path on the CRI socket.
	lis, err := net.Listen("unix", criSock)
	if err != nil {
		t.Fatalf("listen cri socket: %v", err)
	}
	sandboxes, _, _ := store.New("")
	containers, _, _ := store.NewContainerStore("")
	pn := liveLinuxPodPodNetConfig(t)
	serveErr := make(chan error, 1)
	go func() {
		// Pod networking is off by default: this smoke proves the lifecycle serving
		// chain without touching pf/route. Set MACVZ_LINUXPOD_PODNET=1 to include
		// the #128 podnet attach path in a live, operator-controlled environment.
		serveErr <- serveLinuxPod(ctx, lis, criSock, sandboxes, containers,
			pn, linuxpodConfig{enabled: true, helperSocket: helperSock})
	}()
	waitForFile(t, criSock)

	// CRI gRPC client over the unix socket.
	conn, err := grpc.NewClient("unix://"+criSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial cri socket: %v", err)
	}
	defer conn.Close()
	rt := runtimeapi.NewRuntimeServiceClient(conn)

	sb, err := rt.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-live"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	sandboxID := sb.GetPodSandboxId()
	if pn.enabled() {
		st, err := rt.Status(ctx, &runtimeapi.StatusRequest{Verbose: true})
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		var networkReady bool
		for _, cond := range st.GetStatus().GetConditions() {
			if cond.GetType() == "NetworkReady" {
				networkReady = cond.GetStatus()
			}
		}
		if !networkReady {
			t.Fatalf("NetworkReady=false with podnet enabled: %+v", st.GetStatus().GetConditions())
		}
		sbStatus, err := rt.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID, Verbose: true})
		if err != nil {
			t.Fatalf("PodSandboxStatus: %v", err)
		}
		if ip := sbStatus.GetStatus().GetNetwork().GetIp(); ip == "" {
			t.Fatalf("podnet enabled but PodSandboxStatus has empty IP: %+v", sbStatus.GetStatus())
		} else {
			t.Logf("LIVE EVIDENCE: podnet assigned Pod IP %s to sandbox %s", ip, sandboxID)
		}
	}

	start := func(name string) string {
		c, err := rt.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
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
		if _, err := rt.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: c.GetContainerId()}); err != nil {
			t.Fatalf("StartContainer(%s): %v", name, err)
		}
		return c.GetContainerId()
	}
	appID := start("app")
	sidecarID := start("sidecar") // late sidecar after app is Running

	nsOf := func(id string) string {
		st, err := rt.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id, Verbose: true})
		if err != nil {
			t.Fatalf("ContainerStatus(%s): %v", id, err)
		}
		if st.GetInfo()["identityVerified"] != "true" {
			t.Errorf("container %s identityVerified = %q, want true", id, st.GetInfo()["identityVerified"])
		}
		return st.GetInfo()["sandboxNamespace"]
	}
	if a, s := nsOf(appID), nsOf(sidecarID); a == "" || a != s {
		t.Errorf("app and sidecar must share one namespace: %q vs %q", a, s)
	}

	// Teardown: stop then remove the sandbox; expect a clean NotFound afterward.
	for _, id := range []string{sidecarID, appID} {
		if _, err := rt.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id, Timeout: 5}); err != nil {
			t.Fatalf("StopContainer(%s): %v", id, err)
		}
	}
	if _, err := rt.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if _, err := rt.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID}); err == nil {
		t.Errorf("PodSandboxStatus after remove should error NotFound")
	}

	cancel()
	select {
	case <-serveErr:
	case <-time.After(5 * time.Second):
	}
}

func liveLinuxPodPodNetConfig(t *testing.T) podNetConfig {
	t.Helper()
	if os.Getenv("MACVZ_LINUXPOD_PODNET") != "1" {
		return podNetConfig{}
	}
	pn := podNetConfig{
		podCIDR:          os.Getenv("MACVZ_LINUXPOD_POD_CIDR"),
		iface:            os.Getenv("MACVZ_LINUXPOD_PODNET_IFACE"),
		meshInterface:    os.Getenv("MACVZ_LINUXPOD_PODNET_MESH_IFACE"),
		helperSocket:     os.Getenv("MACVZ_LINUXPOD_PODNET_HELPER_SOCKET"),
		enableForwarding: os.Getenv("MACVZ_LINUXPOD_PODNET_ENABLE_FORWARDING") == "1",
	}
	for _, iface := range strings.Fields(os.Getenv("MACVZ_LINUXPOD_PODNET_INGRESS_IFACES")) {
		pn.ingressInterfaces = append(pn.ingressInterfaces, iface)
	}
	if pn.podCIDR == "" || pn.iface == "" {
		t.Fatalf("MACVZ_LINUXPOD_PODNET=1 requires MACVZ_LINUXPOD_POD_CIDR and MACVZ_LINUXPOD_PODNET_IFACE")
	}
	t.Logf("LIVE EVIDENCE: podnet enabled cidr=%s iface=%s helper=%s ingress=%v forwarding=%v",
		pn.podCIDR, pn.iface, pn.helperSocket, []string(pn.ingressInterfaces), pn.enableForwarding)
	return pn
}

func buildSwiftHelperForServing(t *testing.T) string {
	t.Helper()
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// cmd/macvz-cri -> repo root is two levels up.
	repoRoot = filepath.Clean(filepath.Join(repoRoot, "..", ".."))
	helperDir := filepath.Join(repoRoot, "test", "e2e", "cri-linuxpod-helper")
	build := exec.Command("swift", "build", "-c", "debug")
	build.Dir = helperDir
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("swift build helper: %v", err)
	}
	return filepath.Join(helperDir, ".build", "debug", "LinuxPodHelperStub")
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared", path)
}
