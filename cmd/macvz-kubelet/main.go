// Command macvz-kubelet runs on each Apple Silicon Mac and registers it as a
// Kubernetes node via Virtual Kubelet, launching Pods as native micro-VMs.
//
// P0 wires configuration, logging, and version reporting. Node registration and
// the Pod lifecycle are implemented in P2.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/config"
	"k8s.io/klog/v2"
)

func main() {
	var (
		configPath  string
		showVersion bool
	)
	flag.StringVar(&configPath, "config", "", "path to the YAML config file")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")

	// Register klog's flags (e.g. -v, --logtostderr) on the default FlagSet.
	klog.InitFlags(nil)
	flag.Parse()

	if showVersion {
		fmt.Println(version.String())
		return
	}

	if err := run(configPath); err != nil {
		klog.ErrorS(err, "macvz-kubelet exited with error")
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	klog.InfoS("starting macvz-kubelet",
		"version", version.Version,
		"node", cfg.NodeName,
		"runtimeSocket", cfg.RuntimeSocket,
		"logLevel", cfg.LogLevel,
	)

	// TODO(P2): build the runtime driver, construct the provider, and start the
	// Virtual Kubelet node controller here.
	klog.InfoS("node registration not yet implemented (lands in P2); exiting cleanly")
	return nil
}
