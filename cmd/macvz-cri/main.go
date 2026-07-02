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
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
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
	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	streaming "k8s.io/kubelet/pkg/cri/streaming"
)

// podNetConfig collects the CRI-P5 Pod networking flags (#77). Pod networking is
// off unless both PodCIDR and Interface are set; until then the adapter runs
// sandboxes without a Pod IP and reports NetworkReady=false honestly.
type podNetConfig struct {
	podCIDR           string
	iface             string
	meshInterface     string
	ingressInterfaces stringList
	helperSocket      string
	enableForwarding  bool
}

func (c podNetConfig) enabled() bool { return c.podCIDR != "" && c.iface != "" }

// mountConfig collects the CRI-P7 mount-policy flags (#79). It governs which
// kubelet-provided host mounts the adapter binds into a micro-VM.
type mountConfig struct {
	kubeletPodsDir  string
	hostPathAllowed []string
}

// handoffConfig collects the experimental LinuxPod runtime handoff flags
// (CRI-I, #109..#117). The handoff path is off unless enabled is set; root
// overrides the runtime-private subtree root so the path can be exercised under
// a writable per-user directory on macOS (the production /run/macvz/containers
// does not exist there).
type handoffConfig struct {
	enabled bool
	root    string
}

// stringList is a repeatable string flag (one value per occurrence), used for the
// hostPath allowlist so an operator can opt into multiple host prefixes.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	if v == "" {
		return fmt.Errorf("value must not be empty")
	}
	*l = append(*l, v)
	return nil
}

// defaultListen is the CRI socket endpoint used when --listen is not provided.
const defaultListen = "unix:///tmp/macvz-cri.sock"

// defaultStreamingAddr is the address the CRI-P6 streaming server binds for
// exec/port-forward URL handoff. It binds loopback because kubelet runs on the same
// Mac as the adapter; port 0 lets the OS pick a free port, and the actual address
// is published in the streaming URLs kubelet redirects to.
const defaultStreamingAddr = "127.0.0.1:0"

const streamingStartTimeout = 5 * time.Second

func main() {
	var (
		listen          string
		stateDir        string
		runtimeBinary   string
		rosetta         bool
		showVersion     bool
		doPreflight     bool
		streamingAddr   string
		kubeletPodsDir  string
		hostPathAllowed stringList
		multiContainer  bool
		handoff         bool
		handoffRoot     string
		linuxpodBackend bool
		linuxpodSocket  string
		linuxpodLogRoot string
		linuxpodDiag    bool
		supportBundle   bool
		bundleOut       string
		bundleLogFiles  stringList
		bundleNoArchive bool
		lpWorkDir       string
		pn              podNetConfig
	)
	flag.StringVar(&listen, "listen", defaultListen,
		"CRI gRPC endpoint to serve (unix:///path/to.sock or an absolute socket path)")
	flag.StringVar(&streamingAddr, "streaming-addr", defaultStreamingAddr,
		"host:port for the CRI-P6 exec/port-forward streaming server kubelet redirects to (empty disables exec/port-forward)")
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
	flag.Var(&pn.ingressInterfaces, "pod-network-ingress-interface",
		"extra host interface where Pod-IP traffic may arrive and need binat, such as a local test bridge (repeatable; optional)")
	flag.StringVar(&pn.helperSocket, "pod-network-helper-socket", "",
		"macvz-netd privileged helper socket for pf/route operations (empty runs pfctl/route directly, requiring root)")
	flag.BoolVar(&pn.enableForwarding, "pod-network-enable-forwarding", false,
		"enable IPv4 forwarding when starting the Pod network path")
	flag.StringVar(&kubeletPodsDir, "kubelet-pods-dir", "",
		"kubelet per-Pod volume root whose mounts (projected volumes, emptyDir) are always allowed (empty uses /var/lib/kubelet/pods)")
	flag.Var(&hostPathAllowed, "volume-host-path-allowed",
		"absolute host path prefix a hostPath volume may mount under, outside the kubelet pods dir (repeatable; empty disables arbitrary hostPath)")
	flag.BoolVar(&doPreflight, "preflight", false,
		"check runtime dependencies (apple/container CLI, socket, state dir, networking) and exit without serving (CRI-P8 operator diagnostics)")
	flag.BoolVar(&multiContainer, "experimental-multi-container", false,
		"opt into the experimental multi-container Pod probe (#82): admits a second container only if the runtime implements pause-VM shared-netns create/join support; apple/container does not, so this turns the one-container rejection into a diagnostic naming the missing primitive")
	flag.BoolVar(&handoff, "experimental-handoff", false,
		"opt into the experimental LinuxPod runtime handoff path (CRI-I, #109..#120): CreateContainer prepares a runtime-private per-container rootfs/handoff subtree and StartContainer gates Running on handoff identity verification; off by default, experimental (not production-ready), and unrelated to the shipped Virtual Kubelet runtime (docs/CRI_EXPERIMENTAL_HANDOFF_OPERATOR.md)")
	flag.StringVar(&handoffRoot, "handoff-root", "",
		"root directory for the experimental handoff subtree when --experimental-handoff is set (empty uses the production /run/macvz/containers, which is not writable on macOS; point this at a writable per-user dir to exercise the path locally)")
	flag.BoolVar(&linuxpodBackend, "experimental-linuxpod-backend", false,
		"opt into the experimental LinuxPod late-rootfs runtime backend prototype (CRI-R17, #124): connect to a LinuxPod helper over --linuxpod-helper-socket and verify the backend contract at startup; off by default, experimental (not production-ready), and does not replace the shipped apple/container CRI serving path (docs/CRI_RUNTIME_R17_LINUXPOD_BACKEND_REPORT.md)")
	flag.StringVar(&linuxpodSocket, "linuxpod-helper-socket", "",
		"unix socket of the LinuxPod helper that speaks the pkg/runtime/linuxpod NDJSON contract (required with --experimental-linuxpod-backend)")
	flag.StringVar(&linuxpodLogRoot, "linuxpod-log-root", "",
		"override LinuxPod CRI container log root for rootless/remote test topologies where /var/log/pods is not writable/readable by both helper and kubelet (empty uses kubelet's log_directory)")
	flag.BoolVar(&linuxpodDiag, "diagnose-linuxpod", false,
		"scan persisted LinuxPod CRI state for residual/stale records (CRI-L6-2, #136), print a machine-readable JSON report to stdout, and exit without serving; read-only (never mutates records, IP reservations, or host routes). Pass --linuxpod-helper-socket to probe the live helper backend, otherwise sandbox liveness is reported as unprobed")
	flag.BoolVar(&supportBundle, "support-bundle", false,
		"collect a redacted CRI-node diagnostic bundle (CRI-L9-3, #151) — adapter metadata, LinuxPod helper handshake/journals, persisted sandbox/container store summaries, macvz-netd status, socket health, and --bundle-log-file tails — print its path, and exit without serving; individual source failures are recorded inside the bundle (fail-soft) and do not fail the command")
	flag.StringVar(&bundleOut, "bundle-out", "",
		"directory the support bundle is written into (default ./macvz-cri-bundle-<timestamp>)")
	flag.Var(&bundleLogFiles, "bundle-log-file",
		"extra log file whose tail is included in the support bundle, e.g. the adapter or helper log (repeatable)")
	flag.BoolVar(&bundleNoArchive, "no-archive", false,
		"with --support-bundle, leave the bundle as a directory; do not create a tar.gz")
	flag.StringVar(&lpWorkDir, "linuxpod-helper-work-dir", "",
		"LinuxPod helper --work-dir to collect supervisor/adoption journals and a residue listing from in the support bundle (optional)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")

	klog.InitFlags(nil)
	flag.Parse()

	if showVersion {
		fmt.Println(version.String())
		return
	}

	mc := mountConfig{kubeletPodsDir: kubeletPodsDir, hostPathAllowed: hostPathAllowed}
	hc := handoffConfig{enabled: handoff, root: handoffRoot}

	if supportBundle {
		cfg := supportBundleConfig{
			outDir:        bundleOut,
			logFiles:      bundleLogFiles,
			noArchive:     bundleNoArchive,
			helperWorkDir: lpWorkDir,
			listen:        listen,
			stateDir:      stateDir,
			streamingAddr: streamingAddr,
			lc:            linuxpodConfig{enabled: linuxpodBackend, helperSocket: linuxpodSocket, logRoot: linuxpodLogRoot},
			pn:            pn,
		}
		if err := runSupportBundle(context.Background(), cfg); err != nil {
			klog.ErrorS(err, "macvz-cri support bundle failed")
			klog.Flush()
			os.Exit(1)
		}
		klog.Flush()
		return
	}

	if linuxpodDiag {
		lc := linuxpodConfig{enabled: linuxpodBackend, helperSocket: linuxpodSocket, logRoot: linuxpodLogRoot}
		if err := runLinuxPodDiagnose(context.Background(), os.Stdout, stateDir, lc); err != nil {
			klog.ErrorS(err, "macvz-cri LinuxPod diagnostic failed")
			klog.Flush()
			os.Exit(1)
		}
		klog.Flush()
		return
	}

	if doPreflight {
		if err := runPreflight(preflightConfig{
			listen:        listen,
			stateDir:      stateDir,
			runtimeBinary: runtimeBinary,
			pn:            pn,
			mc:            mc,
			hc:            hc,
		}); err != nil {
			klog.Flush()
			os.Exit(1)
		}
		klog.Flush()
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Experimental LinuxPod backend gate (CRI-R17): when enabled, handshake with the
	// helper before serving so an unreachable/misconfigured helper fails loudly here
	// rather than mid-Pod. The shipped apple/container serving path is unchanged.
	lc := linuxpodConfig{enabled: linuxpodBackend, helperSocket: linuxpodSocket, logRoot: linuxpodLogRoot}
	if info, ok, err := lc.handshake(ctx); err != nil {
		klog.ErrorS(err, "macvz-cri exited with error")
		klog.Flush()
		os.Exit(1)
	} else if ok {
		klog.InfoS("experimental LinuxPod backend handshake succeeded; CRI serving will use LinuxPod backend",
			"helper", info.Name, "protocolVersion", info.ProtocolVersion, "simulated", info.Simulated,
			"capLogs", info.Capabilities.Logs, "capExec", info.Capabilities.Exec, "capStats", info.Capabilities.Stats,
			"capAdopt", info.Capabilities.Adopt, "adoptedPods", info.Adoption.AdoptedPods, "lostPods", info.Adoption.LostPods,
			"socket", linuxpodSocket)
	}

	if err := run(ctx, listen, stateDir, runtimeBinary, rosetta, streamingAddr, pn, mc, multiContainer, hc, lc); err != nil {
		klog.ErrorS(err, "macvz-cri exited with error")
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}

func run(ctx context.Context, listen, stateDir, runtimeBinary string, rosetta bool, streamingAddr string, pn podNetConfig, mc mountConfig, multiContainer bool, hc handoffConfig, lc linuxpodConfig) error {
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

	// Experimental LinuxPod backend serving (CRI-L2, #127): when the gate is on,
	// serve the CRI lifecycle through the LinuxPod helper instead of the default
	// apple/container path. This is a SEPARATE service; the apple/container path
	// below is untouched and unreachable in this mode.
	if lc.enabled {
		return serveLinuxPod(ctx, lis, socketPath, sandboxes, containers, streamingAddr, pn, mc, lc)
	}

	driver := container.New(container.Config{Binary: runtimeBinary, Rosetta: rosetta})

	// Wire the Pod network path (CRI-P5, #77) only when explicitly configured. The
	// returned cleanup flushes host pf state on shutdown; it is a no-op when off.
	podNet, ipam, netCleanup, err := setupPodNetwork(ctx, pn)
	if err != nil {
		return err
	}
	defer netCleanup()

	// Wire the experimental LinuxPod handoff manager only when explicitly opted in
	// (CRI-I, #109..#117). Nil leaves CreateContainer/StartContainer on the default
	// apple/container path with no handoff preparation or identity gate.
	var handoffMgr *runtime.HandoffManager
	if hc.enabled {
		// Fail loudly at startup if the operator opted into the experimental path but
		// the environment cannot support it (e.g. the default /run/macvz/containers is
		// not writable on macOS). Without this the failure would surface only at the
		// first CreateContainer as an opaque Internal error.
		effRoot, err := prepareHandoffRoot(hc.root)
		if err != nil {
			return fmt.Errorf("--experimental-handoff: %w", err)
		}
		handoffMgr = runtime.NewHandoffManager(effRoot)
		klog.InfoS("experimental LinuxPod handoff path enabled (off by default; not the shipped Virtual Kubelet runtime)",
			"root", effRoot,
			"note", "CreateContainer stages a runtime-private rootfs/handoff subtree and StartContainer gates Running on identity verification (docs/CRI_EXPERIMENTAL_HANDOFF_OPERATOR.md)")
	}

	grpcServer := grpc.NewServer()
	srv := criserver.New(criserver.Options{
		RuntimeVersion: version.Version,
		Sandboxes:      sandboxes,
		Containers:     containers,
		Runtime:        driver,
		Images:         driver,
		PodNetwork:     podNet,
		IPAM:           ipam,
		Mounts: criserver.MountPolicy{
			KubeletPodsDir:          mc.kubeletPodsDir,
			HostPathAllowedPrefixes: mc.hostPathAllowed,
		},
		MultiContainer: multiContainer,
		Handoff:        handoffMgr,
	})
	// Rebuild Pod IP reservations and re-attach surviving sandboxes so a restart
	// neither leaks addresses nor wipes other Pods' host rules. No-op when off.
	srv.RecoverNetwork(ctx)
	// Reconcile persisted containers against live workloads and resume log pumps so
	// a restart keeps the kubelet's container view consistent (CRI-P7, #79). No-op
	// without a runtime configured.
	srv.RecoverContainers(ctx)

	// Wire the CRI-P6 streaming server (#78) so `kubectl exec`/`port-forward` work.
	// It is built with the CRI server's streaming runtime as its backend, then set
	// back on the server to break the mutual reference. Empty addr leaves exec and
	// port-forward returning a clear FailedPrecondition rather than a dead URL.
	stopStreaming, err := setupStreaming(srv, streamingAddr)
	if err != nil {
		return err
	}
	defer stopStreaming()

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

// setupStreaming builds and starts the CRI-P6 exec/port-forward streaming server
// (#78) and wires it onto the CRI server only after the HTTP listener is reachable.
// An empty addr leaves streaming off, in which case exec and port-forward return a
// clear FailedPrecondition. The returned stop function shuts the streaming server
// down on adapter exit. A startup failure is fatal: serving dead streaming URLs is
// worse than failing fast.
type streamingTarget interface {
	SetStreamingServer(criserver.StreamingServer)
	StreamingRuntime() streaming.Runtime
}

func setupStreaming(srv streamingTarget, addr string) (func(), error) {
	if addr == "" {
		klog.InfoS("CRI streaming disabled; exec and port-forward will return FailedPrecondition")
		return func() {}, nil
	}
	listenAddr := addr
	baseURL := &url.URL{Scheme: "http", Host: listenAddr}
	cfg := streaming.DefaultConfig
	cfg.Addr = listenAddr
	cfg.BaseURL = baseURL
	streamServer, err := streaming.NewServer(cfg, srv.StreamingRuntime())
	if err != nil {
		return nil, fmt.Errorf("build CRI streaming server on %q: %w", listenAddr, err)
	}
	errc := make(chan error, 1)
	go func() {
		// Start blocks until Stop; http.ErrServerClosed is the clean-shutdown signal.
		if err := streamServer.Start(true); err != nil {
			errc <- err
		}
	}()

	if err := waitForStreamingReady(baseURL, listenAddr, errc); err != nil {
		_ = streamServer.Stop()
		return nil, err
	}
	srv.SetStreamingServer(streamServer)
	klog.InfoS("CRI streaming server started", "addr", baseURL.Host)
	return func() { _ = streamServer.Stop() }, nil
}

func waitForStreamingReady(baseURL *url.URL, addr string, errc <-chan error) error {
	deadline := time.NewTimer(streamingStartTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("streaming server on %q closed during startup", addr)
			}
			return fmt.Errorf("start CRI streaming server on %q: %w", addr, err)
		case <-deadline.C:
			return fmt.Errorf("start CRI streaming server on %q: timed out waiting for listener", addr)
		case <-tick.C:
			readyAddr := baseURL.Host
			if _, port, err := net.SplitHostPort(readyAddr); err != nil || port == "0" {
				continue
			}
			conn, err := net.DialTimeout("tcp", readyAddr, 200*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}
	}
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
		Interface:         pn.iface,
		MeshInterface:     pn.meshInterface,
		IngressInterfaces: pn.ingressInterfaces,
		EnableForwarding:  pn.enableForwarding,
	}, pnOpts...)
	if err := router.Start(ctx); err != nil {
		return nil, nil, noop, fmt.Errorf("start pod network path: %w", err)
	}
	// Re-assert the anchor on a timer: macOS's internet-sharing/vmnet NAT
	// rewrites pf wholesale on VM lifecycle events and flushes our anchor,
	// which would otherwise sever Pod-IP translation until a sandbox recreate.
	stopKeepalive := router.StartAnchorKeepalive(ctx, 0)
	klog.InfoS("Pod networking enabled for CRI adapter",
		"podCIDR", ipam.CIDR(), "interface", pn.iface, "meshInterface", pn.meshInterface,
		"ingressInterfaces", []string(pn.ingressInterfaces))

	cleanup := func() {
		stopKeepalive()
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

// prepareHandoffRoot resolves and provisions the experimental handoff subtree root
// before the server starts, so an unsupported environment fails loudly here rather
// than as an opaque Internal error at the first CreateContainer. An empty root
// resolves to the production runtime.HandoffContainersRoot (the same fallback the
// HandoffManager applies), which is not writable on macOS — hence the explicit
// --handoff-root guidance. It returns the effective root on success.
func prepareHandoffRoot(root string) (string, error) {
	effective := root
	if effective == "" {
		effective = runtime.HandoffContainersRoot
	}
	if err := os.MkdirAll(effective, 0o755); err != nil {
		return "", fmt.Errorf("handoff root %q could not be created: %w; pass --handoff-root to a writable directory (the production %s is not writable on macOS)",
			effective, err, runtime.HandoffContainersRoot)
	}
	if err := dirWritable(effective); err != nil {
		return "", fmt.Errorf("handoff root %q is not writable: %w; pass --handoff-root to a writable directory", effective, err)
	}
	return effective, nil
}
