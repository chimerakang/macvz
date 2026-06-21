// Command macvz-cri runs an experimental, minimal Kubernetes CRI server for the
// MacVz CRI feasibility track (docs/CRI_FEASIBILITY.md, CRI-P1..P3).
//
// It listens on a Unix socket and serves the CRI RuntimeService/ImageService so
// kubelet or crictl can connect, run the sandbox lifecycle, and drive a single
// container per Pod sandbox as an apple/container micro-VM (CRI-P3). It stays
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
	"github.com/chimerakang/macvz/pkg/runtime/container"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

// defaultListen is the CRI socket endpoint used when --listen is not provided.
const defaultListen = "unix:///tmp/macvz-cri.sock"

func main() {
	var (
		listen        string
		stateDir      string
		runtimeBinary string
		rosetta       bool
		showVersion   bool
	)
	flag.StringVar(&listen, "listen", defaultListen,
		"CRI gRPC endpoint to serve (unix:///path/to.sock or an absolute socket path)")
	flag.StringVar(&stateDir, "state-dir", defaultStateDir(),
		"directory for restart-tolerant Pod sandbox and container state (empty = in-memory only)")
	flag.StringVar(&runtimeBinary, "runtime-binary", "",
		"apple/container CLI to drive container workloads (empty resolves \"container\" via PATH)")
	flag.BoolVar(&rosetta, "rosetta", false,
		"allow booting linux/amd64 images via Rosetta-for-Linux translation")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")

	klog.InitFlags(nil)
	flag.Parse()

	if showVersion {
		fmt.Println(version.String())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, listen, stateDir, runtimeBinary, rosetta); err != nil {
		klog.ErrorS(err, "macvz-cri exited with error")
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}

func run(ctx context.Context, listen, stateDir, runtimeBinary string, rosetta bool) error {
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

	grpcServer := grpc.NewServer()
	srv := criserver.New(criserver.Options{
		RuntimeVersion: version.Version,
		Sandboxes:      sandboxes,
		Containers:     containers,
		Runtime:        driver,
	})
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
