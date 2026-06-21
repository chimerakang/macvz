package criserver

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// TestLiveSandboxNetwork drives the CRI-P5 Pod networking path against a real
// apple/container backend and a real podnet.Router: a sandbox reserves a Pod IP,
// a container boots a micro-VM, the host pf binat path is programmed, and
// PodSandboxStatus reports the real Pod IP. It then tears the path down and
// confirms the Pod IP is released.
//
// It is gated behind MACVZ_INTEGRATION=1 (or MACVZ_CRI_INTEGRATION=1) because it
// pulls an image, boots a micro-VM, and programs the host packet filter — which
// requires root (or a macvz-netd helper socket via MACVZ_CRI_POD_HELPER_SOCKET).
// The default test run stays hermetic via the fake runtime and fake Pod network.
//
// Tunables (with sensible defaults for a typical apple/container host):
//
//	MACVZ_CRI_POD_CIDR   node Pod CIDR        (default 10.244.0.0/24)
//	MACVZ_CRI_POD_IFACE  vmnet bridge         (default bridge100)
//	MACVZ_CRI_POD_HELPER_SOCKET  optional macvz-netd socket for unprivileged pf/route
func TestLiveSandboxNetwork(t *testing.T) {
	if os.Getenv("MACVZ_INTEGRATION") != "1" && os.Getenv("MACVZ_CRI_INTEGRATION") != "1" {
		t.Skip("set MACVZ_INTEGRATION=1 to run the CRI Pod networking path against a real apple/container service")
	}

	cidr := envOr("MACVZ_CRI_POD_CIDR", "10.244.0.0/24")
	iface := envOr("MACVZ_CRI_POD_IFACE", "bridge100")

	driver := container.New(container.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := driver.Ready(ctx); err != nil {
		t.Fatalf("apple/container not ready: %v", err)
	}

	ipam, err := network.NewPodIPAM(cidr)
	if err != nil {
		t.Fatalf("NewPodIPAM(%q): %v", cidr, err)
	}
	var pnOpts []podnet.Option
	if sock := os.Getenv("MACVZ_CRI_POD_HELPER_SOCKET"); sock != "" {
		pnOpts = append(pnOpts, podnet.WithHelperSocket(sock))
	}
	router := podnet.New(podnet.Config{Interface: iface, EnableForwarding: true}, pnOpts...)
	if err := router.Start(ctx); err != nil {
		t.Fatalf("start pod network path (need root or MACVZ_CRI_POD_HELPER_SOCKET): %v", err)
	}
	defer func() { _ = router.Stop(context.Background()) }()

	s := New(Options{Runtime: driver, Images: driver, IPAM: ipam, PodNetwork: router})

	sbResp, err := s.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "cri-p5", Namespace: "default", Uid: "uid-live-net"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	sandboxID := sbResp.GetPodSandboxId()
	key := "default/cri-p5"
	wantIP := ipam.IP(key)
	if wantIP == "" {
		t.Fatalf("RunPodSandbox did not reserve a Pod IP")
	}
	t.Logf("sandbox reserved Pod IP %s", wantIP)

	if _, err := s.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: liveImage}}); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	cResp, err := s.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
			Image:    &runtimeapi.ImageSpec{Image: liveImage},
			Command:  []string{"/bin/sh", "-c", "echo macvz-cri-p5 && sleep 60"},
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
		t.Fatalf("StartContainer (network attach): %v", err)
	}

	st, err := s.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	if got := st.GetStatus().GetNetwork().GetIp(); got != wantIP {
		t.Fatalf("PodSandboxStatus.Network.Ip = %q, want %q", got, wantIP)
	}
	t.Logf("sandbox reports Pod IP %s after network attach", wantIP)

	if _, err := s.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if ipam.IP(key) != "" {
		t.Errorf("Pod IP for %q = %q after remove, want released", key, ipam.IP(key))
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
