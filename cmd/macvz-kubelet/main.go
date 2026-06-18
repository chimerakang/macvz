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

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/runtime/container"
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

	if err := run(configPath, runtimeBinary); err != nil {
		klog.ErrorS(err, "macvz-kubelet exited with error")
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}

func run(configPath, runtimeBinary string) error {
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
	if err := driver.Ready(context.Background()); err != nil {
		klog.ErrorS(err, "apple/container runtime is not ready",
			"hint", "ensure the binary is installed and `container system start` has been run")
	} else {
		klog.InfoS("apple/container runtime is ready")
	}
	_ = driver // handed to the provider in P2

	// TODO(P2): construct the provider from driver and start the Virtual Kubelet
	// node controller here.
	klog.InfoS("node registration not yet implemented (lands in P2); exiting cleanly")
	return nil
}
