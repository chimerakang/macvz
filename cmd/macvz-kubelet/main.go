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
	"os"
	"os/signal"
	"syscall"

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/provider"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// Construct the provider over the runtime driver. Pod lifecycle methods are
	// stubbed until #16; this issue only wires the node controller loop.
	p := provider.New(cfg.NodeName, driver)
	_ = p // pod controller wiring lands with the Pod lifecycle (#16).

	// Start the Virtual Kubelet node controller. NaiveNodeProvider keeps the node
	// marked ready via Ping; capacity/conditions (#14) and lease/heartbeat (#15)
	// build on this loop.
	nc, err := vknode.NewNodeController(
		vknode.NaiveNodeProvider{},
		buildNode(cfg.NodeName),
		clientset.CoreV1().Nodes(),
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

	// Block until shutdown is requested, then let the cancelled context unwind
	// the controller's run loop.
	<-ctx.Done()
	klog.InfoS("shutdown signal received; stopping node controller", "node", cfg.NodeName)
	<-nc.Done()
	if err := nc.Err(); err != nil {
		return fmt.Errorf("node controller shutdown: %w", err)
	}
	klog.InfoS("macvz-kubelet stopped cleanly")
	return nil
}

// buildNode returns the minimal Node object the controller registers. Capacity,
// addresses, taints, and detailed conditions are filled in by #14; here we only
// establish identity and a Ready condition so the control loop can run.
func buildNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"type":                   "virtual-kubelet",
				"kubernetes.io/role":     "agent",
				"kubernetes.io/hostname": name,
				"kubernetes.io/os":       "linux",
				"kubernetes.io/arch":     "arm64",
			},
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				OperatingSystem: "linux",
				Architecture:    "arm64",
				KubeletVersion:  version.Version,
			},
			Conditions: []corev1.NodeCondition{{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionTrue,
				Reason:             "KubeletReady",
				Message:            "macvz-kubelet is ready",
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}
