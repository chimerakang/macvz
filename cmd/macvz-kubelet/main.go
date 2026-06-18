// Command macvz-kubelet runs on each Apple Silicon Mac and registers it as a
// Kubernetes node via Virtual Kubelet, launching Pods as native micro-VMs.
//
// P0 wires configuration, logging, and version reporting. Node registration and
// the Pod lifecycle are implemented in P2.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/provider"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// envRuntimeBinary overrides the apple/container CLI path. Flag > env > config.
const envRuntimeBinary = "MACVZ_CONTAINER_BIN"

func main() {
	var (
		configPath    string
		runtimeBinary string
		showVersion   bool
	)
	flag.StringVar(&configPath, "config", "", "path to the YAML config file")
	flag.StringVar(&runtimeBinary, "runtime-binary", "",
		"apple/container CLI to drive (overrides config; env "+envRuntimeBinary+")")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")

	// Register klog's flags (e.g. -v, --logtostderr) on the default FlagSet.
	klog.InitFlags(nil)
	flag.Parse()

	if showVersion {
		fmt.Println(version.String())
		return
	}

	// Cancel the root context on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, configPath, runtimeBinary); err != nil {
		klog.ErrorS(err, "macvz-kubelet exited with error")
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}

func run(ctx context.Context, configPath, runtimeBinary string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve the runtime binary: flag > env > config default.
	bin := cfg.RuntimeBinary
	if env := os.Getenv(envRuntimeBinary); env != "" {
		bin = env
	}
	if runtimeBinary != "" {
		bin = runtimeBinary
	}

	klog.InfoS("starting macvz-kubelet",
		"version", version.Version,
		"node", cfg.NodeName,
		"runtimeSocket", cfg.RuntimeSocket,
		"runtimeBinary", bin,
		"logLevel", cfg.LogLevel,
	)

	// Build the apple/container driver and report runtime readiness (P1).
	driver := container.New(container.Config{Binary: bin})
	if err := driver.Ready(ctx); err != nil {
		klog.ErrorS(err, "apple/container runtime is not ready",
			"hint", "ensure the binary is installed and `container system start` has been run")
	} else {
		klog.InfoS("apple/container runtime is ready")
	}

	// Build the Kubernetes client from kubeconfig / in-cluster config.
	restCfg, err := cfg.RestConfig()
	if err != nil {
		return fmt.Errorf("kubernetes client config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}

	internalIP := cfg.Node.InternalIP
	if internalIP == "" {
		internalIP = detectInternalIP()
	}

	// Construct the provider over the runtime driver. The node's reachable
	// address is reported as each Pod's HostIP so Services/`-o wide` resolve it.
	p := provider.New(cfg.NodeName, driver, provider.WithHostIP(internalIP))

	// Resolve the configured node shape (capacity/taints validated at load).
	capacity, err := cfg.Capacity()
	if err != nil {
		return fmt.Errorf("node capacity: %w", err)
	}
	taints, err := cfg.Taints()
	if err != nil {
		return fmt.Errorf("node taints: %w", err)
	}
	nodeSpec := provider.NodeSpec{
		KubeletVersion: version.Version,
		OS:             cfg.Node.OS,
		Arch:           cfg.Node.Arch,
		InternalIP:     internalIP,
		Capacity:       capacity,
		Labels:         cfg.Node.Labels,
		Annotations:    cfg.Node.Annotations,
		Taints:         taints,
	}
	if cfg.Node.ServingTLSCertFile != "" && cfg.Node.ServingTLSKeyFile != "" {
		nodeSpec.KubeletPort = cfg.Node.KubeletPort
	}
	node := p.BuildNode(ctx, nodeSpec)

	pingInterval, statusInterval, err := cfg.HeartbeatIntervals()
	if err != nil {
		return fmt.Errorf("heartbeat intervals: %w", err)
	}

	klog.InfoS("registering virtual node",
		"node", cfg.NodeName,
		"cpu", cfg.Node.CPU, "memory", cfg.Node.Memory, "pods", cfg.Node.Pods,
		"internalIP", internalIP, "taints", len(taints),
		"enableLease", cfg.Node.EnableLease, "pingInterval", pingInterval, "statusInterval", statusInterval,
	)

	// Node provider re-probes runtime readiness on the status interval and
	// surfaces changes as node conditions.
	nodeProvider := p.NewNodeStatusProvider(nodeSpec, statusInterval)

	// Log transient status-update errors and keep going: the control loop retries
	// on the next interval, and client-go applies its own backoff. Returning nil
	// signals "handled, retry possible" to Virtual Kubelet.
	statusErrHandler := func(_ context.Context, err error) error {
		klog.ErrorS(err, "transient node status update error; will retry", "node", cfg.NodeName)
		return nil
	}

	opts := []vknode.NodeControllerOpt{
		vknode.WithNodePingInterval(pingInterval),
		vknode.WithNodeStatusUpdateInterval(statusInterval),
		vknode.WithNodeStatusUpdateErrorHandler(statusErrHandler),
	}
	if cfg.Node.EnableLease {
		// The Lease in kube-node-lease is the modern node heartbeat; Kubernetes
		// marks the node NotReady if it is not renewed within the lease duration.
		leaseClient := clientset.CoordinationV1().Leases(corev1.NamespaceNodeLease)
		opts = append(opts, vknode.WithNodeEnableLeaseV1(leaseClient, cfg.Node.LeaseDurationSeconds))
	}

	nc, err := vknode.NewNodeController(
		nodeProvider,
		node,
		clientset.CoreV1().Nodes(),
		opts...,
	)
	if err != nil {
		return fmt.Errorf("create node controller: %w", err)
	}

	klog.InfoS("starting virtual-kubelet node controller", "node", cfg.NodeName)
	go func() {
		if err := nc.Run(ctx); err != nil && ctx.Err() == nil {
			klog.ErrorS(err, "node controller stopped unexpectedly")
		}
	}()

	select {
	case <-nc.Ready():
		klog.InfoS("virtual node registered and ready", "node", cfg.NodeName)
	case <-nc.Done():
		return fmt.Errorf("node controller exited before becoming ready: %w", nc.Err())
	case <-ctx.Done():
		klog.InfoS("shutdown requested before node became ready")
		return nil
	}

	// Enable coordinated Pod IPAM now that Kubernetes has assigned this node a
	// Pod CIDR, recovering any existing allocations before Pods are reconciled.
	if err := setupIPAM(ctx, cfg, clientset, p); err != nil {
		return fmt.Errorf("setup pod IPAM: %w", err)
	}

	// Bring up the cross-host WireGuard mesh (if enabled) so remote Pod CIDRs are
	// routed through encrypted tunnels before Pods start landing on this node.
	stopMesh, err := setupMesh(ctx, cfg)
	if err != nil {
		return fmt.Errorf("setup mesh: %w", err)
	}
	defer stopMesh()

	// Start the host Pod network path so each micro-VM is reachable at its Pod IP
	// across the mesh, and attach it to the provider before Pods are reconciled.
	stopPodNet, err := setupPodNetwork(ctx, cfg, p)
	if err != nil {
		return fmt.Errorf("setup pod network: %w", err)
	}
	defer stopPodNet()

	// Start the Pod lifecycle controller so scheduled Pods become micro-VMs.
	stopPods, err := startPodController(ctx, cfg, clientset, p, runtime.NumCPU())
	if err != nil {
		return fmt.Errorf("start pod controller: %w", err)
	}
	defer stopPods()

	// Start the kubelet API server for kubectl logs/exec (no-op without certs).
	stopServer, err := startKubeletServer(ctx, cfg, p)
	if err != nil {
		return fmt.Errorf("start kubelet server: %w", err)
	}
	defer stopServer()

	// Block until shutdown is requested, then let the cancelled context unwind
	// the controller run loops.
	<-ctx.Done()
	klog.InfoS("shutdown signal received; stopping controllers", "node", cfg.NodeName)
	<-nc.Done()
	if err := nc.Err(); err != nil {
		return fmt.Errorf("node controller shutdown: %w", err)
	}
	klog.InfoS("macvz-kubelet stopped cleanly")
	return nil
}

// detectInternalIP returns the host's primary outbound IPv4 address, used as
// the node's InternalIP when not set in config. It opens no connection (UDP
// dial just selects a route), and returns "" if detection fails — the node then
// registers without an InternalIP.
func detectInternalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}
