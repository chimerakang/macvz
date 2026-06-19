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
	"github.com/chimerakang/macvz/pkg/network/privhelper"
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
	// Subcommands (doctor/bootstrap) are dispatched before flag parsing so the
	// node join workflow (#54) shares this binary without disturbing the default
	// `macvz-kubelet --config ...` invocation.
	if handled, code := dispatchSubcommand(os.Args[1:]); handled {
		os.Exit(code)
	}

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
		"rosetta", cfg.RuntimeRosetta,
		"logLevel", cfg.LogLevel,
	)

	// Build the apple/container driver and report runtime readiness (P1).
	driver := container.New(container.Config{Binary: bin, Rosetta: cfg.RuntimeRosetta})
	if err := driver.Ready(ctx); err != nil {
		klog.ErrorS(err, "apple/container runtime is not ready",
			"hint", "ensure the binary is installed and `container system start` has been run")
	} else {
		klog.InfoS("apple/container runtime is ready")
	}

	// Resolve Kubernetes client config now, but delay constructing the clientset
	// until after the data plane has mutated host routes. That avoids carrying a
	// client-go transport across WireGuard/vmnet route changes on remote-API nodes.
	restCfg, err := cfg.RestConfig()
	if err != nil {
		return fmt.Errorf("kubernetes client config: %w", err)
	}

	internalIP := cfg.Node.InternalIP
	if internalIP == "" {
		internalIP = detectInternalIP()
	}

	// Construct the provider over the runtime driver. The node's reachable
	// address is reported as each Pod's HostIP so Services/`-o wide` resolve it.
	p := provider.New(cfg.NodeName, driver,
		provider.WithHostIP(internalIP),
		provider.WithVolumePolicy(provider.VolumePolicy{
			Root:                    cfg.Node.Volumes.Root,
			HostPathAllowedPrefixes: cfg.Node.Volumes.HostPathAllowedPrefixes,
		}),
		provider.WithDNS(provider.DNSConfig{
			ClusterDNS:    cfg.Node.ClusterDNS,
			ClusterDomain: cfg.Node.ClusterDomain,
		}),
	)

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
	// Bring the host data plane up BEFORE the node controller connects to the API
	// server. The mesh/route changes perturb host routing, and doing them after the
	// long-lived API (HTTP/2) connection is established severs it on a node whose
	// API server is remote — client-go then wedges on "no route to host" and the
	// node flaps NotReady. Establishing the API connection over the final routing
	// avoids that (the data plane needs only static config, not registration).
	if cfg.PrivilegedHelperSocket != "" && (cfg.Mesh.Enabled || cfg.PodNetwork.Enabled) {
		hc := privhelper.NewClient(cfg.PrivilegedHelperSocket)
		st, err := hc.Status(ctx)
		if err != nil {
			return fmt.Errorf("privileged network helper not reachable at %s (start macvz-netd as root): %w", cfg.PrivilegedHelperSocket, err)
		}
		// Confirm the exec path itself works (the socket may answer status while the
		// daemon lacks the privileges to run commands).
		if err := hc.Ping(ctx); err != nil {
			return fmt.Errorf("privileged network helper at %s answered status but cannot run commands: %w", cfg.PrivilegedHelperSocket, err)
		}
		if !st.PolicyEnforced {
			return fmt.Errorf("privileged network helper at %s is running without per-request policy validation; start macvz-netd with --config", cfg.PrivilegedHelperSocket)
		}
		klog.InfoS("privileged network helper reachable",
			"socket", cfg.PrivilegedHelperSocket, "version", st.Version, "protocol", st.Protocol,
			"policyEnforced", st.PolicyEnforced, "allow", st.AllowedCommands, "uptime", st.Uptime)
	}

	mesh, stopMesh, err := setupMesh(ctx, cfg, configPath)
	if err != nil {
		return fmt.Errorf("setup mesh: %w", err)
	}
	defer stopMesh()

	podNetRouter, stopPodNet, err := setupPodNetwork(ctx, cfg, p)
	if err != nil {
		return fmt.Errorf("setup pod network: %w", err)
	}
	defer stopPodNet()

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build kubernetes client after data-plane setup: %w", err)
	}
	if err := waitForAPIServer(ctx, clientset); err != nil {
		return fmt.Errorf("kubernetes API not reachable after data-plane setup: %w", err)
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

	// Program ClusterIP Service routing into the Pod network anchor so micro-VMs
	// can reach Services (#37). The Pod network path was brought up before node
	// registration (see above); the router is reused here. No-op when disabled.
	stopSvc := startServiceController(ctx, cfg, clientset, podNetRouter)
	defer stopSvc()

	// Start the Pod lifecycle controller so scheduled Pods become micro-VMs.
	stopPods, err := startPodController(ctx, cfg, clientset, p, runtime.NumCPU())
	if err != nil {
		return fmt.Errorf("start pod controller: %w", err)
	}
	defer stopPods()

	// Aggregate node health across runtime, control-plane, and data-plane so the
	// kubelet's /healthz/diagnostics endpoint can explain why the node is or is
	// not ready for workloads (#56). Built from the live components.
	diagnostics := newDiagnosticsCollector(cfg, driver, clientset, mesh, podNetRouter)

	// Start the kubelet API server for kubectl logs/exec (no-op without certs).
	stopServer, err := startKubeletServer(ctx, cfg, p, internalIP, diagnostics)
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
