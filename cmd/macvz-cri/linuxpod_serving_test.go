package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const liveLinuxPodReachabilityMarker = "macvz-linuxpod-podnet-ok"

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
			pn, mountConfig{}, linuxpodConfig{enabled: true, helperSocket: helperSock})
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
	var startedContainers []string
	defer func() {
		for i := len(startedContainers) - 1; i >= 0; i-- {
			_, _ = rt.StopContainer(context.Background(), &runtimeapi.StopContainerRequest{ContainerId: startedContainers[i], Timeout: 5})
		}
		_, _ = rt.RemovePodSandbox(context.Background(), &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID})
	}()
	podIP := ""
	vmIP := ""
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
			podIP = ip
			t.Logf("LIVE EVIDENCE: podnet assigned Pod IP %s to sandbox %s", ip, sandboxID)
		}
		if info := sbStatus.GetInfo(); info["networkAttached"] != "true" {
			t.Fatalf("podnet enabled but verbose networkAttached=%q: %+v", info["networkAttached"], info)
		} else if info["vmIP"] == "" {
			t.Fatalf("podnet enabled but verbose vmIP is empty: %+v", info)
		} else {
			vmIP = info["vmIP"]
			t.Logf("LIVE EVIDENCE: podnet attached Pod IP %s to LinuxPod VM IP %s on %s", info["podIP"], vmIP, info["interface"])
		}
	}

	reachability := pn.enabled() && os.Getenv("MACVZ_LINUXPOD_PODNET_REACHABILITY") == "1"
	podToHostReachability := reachability && os.Getenv("MACVZ_LINUXPOD_PODNET_POD_TO_HOST") == "1"
	callbackURL, callbackSeen, stopCallback := liveLinuxPodHostCallback(t, podToHostReachability, vmIP)
	defer stopCallback()

	start := func(name string, command []string) string {
		if len(command) == 0 {
			command = []string{"/bin/sh", "-c", "sleep 300"}
		}
		c, err := rt.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
			PodSandboxId: sandboxID,
			Config: &runtimeapi.ContainerConfig{
				Metadata: &runtimeapi.ContainerMetadata{Name: name},
				Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"},
				Command:  command,
			},
		})
		if err != nil {
			t.Fatalf("CreateContainer(%s): %v", name, err)
		}
		if _, err := rt.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: c.GetContainerId()}); err != nil {
			t.Fatalf("StartContainer(%s): %v", name, err)
		}
		startedContainers = append(startedContainers, c.GetContainerId())
		return c.GetContainerId()
	}
	appCommand := []string(nil)
	sidecarCommand := []string(nil)
	if reachability {
		appCommand = []string{"/bin/sh", "-c",
			fmt.Sprintf("mkdir -p /www && echo %s > /www/index.html && exec httpd -f -p 8080 -h /www", liveLinuxPodReachabilityMarker)}
	}
	if podToHostReachability {
		sidecarCommand = []string{"/bin/sh", "-c",
			fmt.Sprintf("wget -q -O- %s >/tmp/macvz-host-callback 2>/tmp/macvz-host-callback.err || true; exec sleep 300", callbackURL)}
	}
	appID := start("app", appCommand)
	if reachability {
		assertLiveLinuxPodHostToPod(t, podIP, vmIP)
		holdLiveLinuxPodReachability(t)
	}
	sidecarID := start("sidecar", sidecarCommand) // late sidecar after app is Running
	if podToHostReachability {
		assertLiveLinuxPodPodToHost(t, callbackSeen)
	}

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
	startedContainers = nil
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

func liveLinuxPodHostCallback(t *testing.T, enabled bool, vmIP string) (url string, seen <-chan struct{}, stop func()) {
	t.Helper()
	if !enabled {
		return "", nil, func() {}
	}
	callbackHost := os.Getenv("MACVZ_LINUXPOD_PODNET_HOST_CALLBACK_ADDR")
	if callbackHost == "" {
		var ok bool
		callbackHost, ok = liveLinuxPodGatewayForVMIP(vmIP)
		if !ok {
			t.Fatalf("derive host callback address from vmIP %q", vmIP)
		}
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen host callback: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatalf("host callback addr: %v", err)
	}
	seenCh := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/pod-to-host", func(w http.ResponseWriter, r *http.Request) {
		select {
		case seenCh <- struct{}{}:
		default:
		}
		_, _ = io.WriteString(w, liveLinuxPodReachabilityMarker)
	})
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	stop = func() {
		_ = srv.Close()
		<-done
	}
	return "http://" + net.JoinHostPort(callbackHost, port) + "/pod-to-host", seenCh, stop
}

func liveLinuxPodGatewayForVMIP(vmIP string) (string, bool) {
	ip := net.ParseIP(vmIP).To4()
	if ip == nil || ip[3] == 0 || ip[3] == 255 {
		return "", false
	}
	ip[3] = 1
	return ip.String(), true
}

func TestLiveLinuxPodGatewayForVMIP(t *testing.T) {
	got, ok := liveLinuxPodGatewayForVMIP("192.168.67.2")
	if !ok || got != "192.168.67.1" {
		t.Fatalf("gateway = %q, %v; want 192.168.67.1, true", got, ok)
	}
	for _, ip := range []string{"", "not-ip", "192.168.67.0", "192.168.67.255", "2001:db8::1"} {
		if got, ok := liveLinuxPodGatewayForVMIP(ip); ok {
			t.Fatalf("gateway(%q) = %q, true; want false", ip, got)
		}
	}
}

func assertLiveLinuxPodHostToPod(t *testing.T, podIP, vmIP string) {
	t.Helper()
	host := os.Getenv("MACVZ_LINUXPOD_PODNET_HOST_TO_POD_ADDR")
	if host == "vmIP" {
		host = vmIP
	}
	if host == "" {
		host = podIP
	}
	url := "http://" + net.JoinHostPort(host, "8080") + "/"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && strings.Contains(string(body), liveLinuxPodReachabilityMarker) {
				t.Logf("LIVE EVIDENCE: host-to-Pod HTTP reached %s", url)
				return
			}
			lastErr = fmt.Errorf("status=%s body=%q read=%v", resp.Status, string(body), readErr)
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("host-to-Pod HTTP did not reach %s: %v", url, lastErr)
}

func holdLiveLinuxPodReachability(t *testing.T) {
	t.Helper()
	raw := os.Getenv("MACVZ_LINUXPOD_PODNET_HOLD_SECONDS")
	if raw == "" {
		return
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		t.Fatalf("MACVZ_LINUXPOD_PODNET_HOLD_SECONDS=%q must be a non-negative integer", raw)
	}
	if seconds == 0 {
		return
	}
	t.Logf("LIVE EVIDENCE: holding LinuxPod podnet reachability for %ds", seconds)
	time.Sleep(time.Duration(seconds) * time.Second)
}

func assertLiveLinuxPodPodToHost(t *testing.T, seen <-chan struct{}) {
	t.Helper()
	select {
	case <-seen:
		t.Logf("LIVE EVIDENCE: Pod-to-host HTTP callback reached host")
	case <-time.After(20 * time.Second):
		t.Fatalf("Pod-to-host HTTP callback did not reach host")
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
