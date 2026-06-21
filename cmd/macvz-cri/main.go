// Command macvz-cri runs an experimental, minimal Kubernetes CRI server for the
// MacVz CRI feasibility track (docs/CRI_FEASIBILITY.md, CRI-P1..P4).
//
// It listens on a Unix socket and serves the CRI RuntimeService/ImageService so
// kubelet or crictl can connect, run the sandbox lifecycle, drive a single
// container per Pod sandbox as an apple/container micro-VM (CRI-P3), and manage
// images through the ImageService (pull/status/list/remove, CRI-P4). It stays
// narrow: one container per sandbox, no shared Pod network, no shared volumes —
// see pkg/criserver.
//
// This command is intentionally separate from cmd/macvz-kubelet (the shipped
// Virtual Kubelet provider) and is not the default MacVz runtime mode.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/criserver"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

// podNetConfig collects the CRI-P5 Pod networking flags (#77). Pod networking is
// off unless both PodCIDR and Interface are set; until then the adapter runs
// sandboxes without a Pod IP and reports NetworkReady=false honestly.
type podNetConfig struct {
	podCIDR          string
	iface            string
	meshInterface    string
	helperSocket     string
	enableForwarding bool
}

func (c podNetConfig) enabled() bool { return c.podCIDR != "" && c.iface != "" }

// defaultListen is the CRI socket endpoint used when --listen is not provided.
const defaultListen = "unix:///tmp/macvz-cri.sock"

func main() {
	var (
		listen        string
		stateDir      string
		runtimeBinary string
		rosetta       bool
		showVersion   bool
		pn            podNetConfig
	)
	flag.StringVar(&listen, "listen", defaultListen,
		"CRI gRPC endpoint to serve (unix:///path/to.sock or an absolute socket path)")
	flag.StringVar(&stateDir, "state-dir", defaultStateDir(),
		"directory for restart-tolerant Pod sandbox and container state (empty = in-memory only)")
	flag.StringVar(&runtimeBinary, "runtime-binary", "",
		"apple/container CLI to drive container workloads (empty resolves \"container\" via PATH)")
	flag.BoolVar(&rosetta, "rosetta", false,
		"allow booting linux/amd64 images via Rosetta-for-Linux translation")
	flag.StringVar(&pn.podCIDR, "pod-cidr", "",
		"node Pod CIDR for CRI-P5 Pod IP allocation (empty = Pod networking off)")
	flag.StringVar(&pn.iface, "pod-network-interface", "",
		"vmnet bridge backing the micro-VMs for the Pod network binat path (required with --pod-cidr to enable Pod networking)")
	flag.StringVar(&pn.meshInterface, "pod-network-mesh-interface", "",
		"WireGuard mesh interface for cross-node Pod binat rules (optional)")
	flag.StringVar(&pn.helperSocket, "pod-network-helper-socket", "",
		"macvz-netd privileged helper socket for pf/route operations (empty runs pfctl/route directly, requiring root)")
	flag.BoolVar(&pn.enableForwarding, "pod-network-enable-forwarding", false,
		"enable IPv4 forwarding when starting the Pod network path")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")

	klog.InitFlags(nil)
	flag.Parse()

	if showVersion {
		fmt.Println(version.String())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, listen, stateDir, runtimeBinary, rosetta, pn); err != nil {
		klog.ErrorS(err, "macvz-cri exited with error")
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}

func run(ctx context.Context, listen, stateDir, runtimeBinary string, rosetta bool, pn podNetConfig) error {
	socketPath, err := socketPath(listen)
	if err != nil {
		return err
	}

	sandboxes, skipped, err := store.New(stateDir)
	if err != nil {
		return fmt.Errorf("open sandbox store: %w", err)
	}
	if skipped > 0 {
		klog.InfoS("skipped unparseable sandbox records on load", "count", skipped, "stateDir", stateDir)
	}

	// Container records live in a sibling directory so the two stores never read
	// each other's files. An empty stateDir keeps both in-memory.
	containerDir := stateDir
	if containerDir != "" {
		containerDir = filepath.Join(stateDir, "containers")
	}
	containers, cSkipped, err := store.NewContainerStore(containerDir)
	if err != nil {
		return fmt.Errorf("open container store: %w", err)
	}
	if cSkipped > 0 {
		klog.InfoS("skipped unparseable container records on load", "count", cSkipped, "stateDir", containerDir)
	}

	// Remove only a confirmed stale Unix socket. If another CRI server is alive
	// on this path, fail fast instead of unlinking its socket and splitting
	// clients across two server processes.
	if err := prepareSocket(socketPath); err != nil {
		return err
	}

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on unix socket %s: %w", socketPath, err)
	}
	// Best-effort cleanup so we do not leave a dangling socket behind.
	defer func() { _ = os.Remove(socketPath) }()

	driver := container.New(container.Config{Binary: runtimeBinary, Rosetta: rosetta})

	// Wire the Pod network path (CRI-P5, #77) only when explicitly configured. The
	// returned cleanup flushes host pf state on shutdown; it is a no-op when off.
	podNet, ipam, netCleanup, err := setupPodNetwork(ctx, pn)
	if err != nil {
		return err
	}
	defer netCleanup()

	grpcServer := grpc.NewServer()
	srv := criserver.New(criserver.Options{
		RuntimeVersion: version.Version,
		Sandboxes:      sandboxes,
		Containers:     containers,
		Runtime:        driver,
		Images:         driver,
		PodNetwork:     podNet,
		IPAM:           ipam,
	})
	// Rebuild Pod IP reservations and re-attach surviving sandboxes so a restart
	// neither leaks addresses nor wipes other Pods' host rules. No-op when off.
	srv.RecoverNetwork(ctx)
	srv.Register(grpcServer)

	klog.InfoS("starting experimental macvz-cri server",
		"version", version.Version,
		"socket", socketPath,
		"stateDir", stateDir,
		"rosetta", rosetta,
		"note", "CRI feasibility spike (docs/CRI_FEASIBILITY.md); single-container Pods over apple/container, not the default MacVz runtime",
	)

	// Stop the gRPC server when the context is cancelled (SIGINT/SIGTERM).
	go func() {
		<-ctx.Done()
		klog.InfoS("shutdown requested; stopping CRI server")
		grpcServer.GracefulStop()
	}()

	if err := grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("serve CRI gRPC: %w", err)
	}
	klog.InfoS("macvz-cri stopped cleanly")
	return nil
}

// setupPodNetwork builds the CRI-P5 Pod networking dependencies (#77) when both a
// Pod CIDR and a vmnet interface are configured: a PodIPAM over the node's Pod
// CIDR and a started podnet.Router for the host pf binat path. It returns nil
// interfaces and a no-op cleanup when Pod networking is off, so the adapter runs
// sandboxes without a Pod IP and reports NetworkReady=false honestly.
//
// The returned values are interface-typed (not concrete pointers) so the "off"
// case yields true nils — a concrete typed-nil would make the server believe Pod
// networking is wired.
func setupPodNetwork(ctx context.Context, pn podNetConfig) (criserver.PodNetwork, criserver.PodIPAllocator, func(), error) {
	noop := func() {}
	if !pn.enabled() {
		if pn.podCIDR != "" || pn.iface != "" {
			klog.InfoS("Pod networking partially configured; both --pod-cidr and --pod-network-interface are required to enable it",
				"podCIDR", pn.podCIDR, "interface", pn.iface)
		} else {
			klog.InfoS("Pod networking disabled; sandboxes run without a Pod IP")
		}
		return nil, nil, noop, nil
	}

	ipam, err := network.NewPodIPAM(pn.podCIDR)
	if err != nil {
		return nil, nil, noop, fmt.Errorf("build pod IPAM over %q: %w", pn.podCIDR, err)
	}

	var pnOpts []podnet.Option
	if pn.helperSocket != "" {
		pnOpts = append(pnOpts, podnet.WithHelperSocket(pn.helperSocket))
	}
	router := podnet.New(podnet.Config{
		Interface:        pn.iface,
		MeshInterface:    pn.meshInterface,
		EnableForwarding: pn.enableForwarding,
	}, pnOpts...)
	if err := router.Start(ctx); err != nil {
		return nil, nil, noop, fmt.Errorf("start pod network path: %w", err)
	}
	klog.InfoS("Pod networking enabled for CRI adapter",
		"podCIDR", ipam.CIDR(), "interface", pn.iface, "meshInterface", pn.meshInterface)

	cleanup := func() {
		if err := router.Stop(context.Background()); err != nil {
			klog.ErrorS(err, "failed to stop pod network path")
		}
	}
	return router, ipam, cleanup, nil
}

// socketPath extracts the filesystem path from a CRI endpoint. It accepts the
// canonical "unix:///path" form as well as a bare absolute path, matching how
// crictl --runtime-endpoint and kubelet --container-runtime-endpoint are given.
func socketPath(endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("empty listen endpoint")
	}
	if strings.HasPrefix(endpoint, "unix://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return "", fmt.Errorf("parse endpoint %q: %w", endpoint, err)
		}
		// "unix:///tmp/x.sock" -> u.Path "/tmp/x.sock". A host (e.g.
		// "unix://tmp/x.sock") is not a valid absolute socket path.
		if u.Host != "" {
			return "", fmt.Errorf("endpoint %q must use an absolute path (unix:///path)", endpoint)
		}
		if u.Path == "" {
			return "", fmt.Errorf("endpoint %q has no socket path", endpoint)
		}
		return u.Path, nil
	}
	if strings.Contains(endpoint, "://") {
		return "", fmt.Errorf("unsupported endpoint scheme in %q (only unix:// is supported)", endpoint)
	}
	if !filepath.IsAbs(endpoint) {
		return "", fmt.Errorf("endpoint %q must be an absolute socket path", endpoint)
	}
	return endpoint, nil
}

func prepareSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat socket %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s", path)
	}
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("socket %s is already serving; refusing to replace a live CRI endpoint", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}

// defaultStateDir resolves the per-user directory for restart-tolerant sandbox
// state, falling back to a temp path when the home directory is unavailable.
func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "macvz-cri", "sandboxes")
	}
	return filepath.Join(home, ".macvz", "cri", "sandboxes")
}
