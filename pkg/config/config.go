// Package config loads macvz-kubelet configuration from a YAML file, applying
// sensible defaults and validating required fields.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config is the on-disk configuration for a macvz-kubelet node process.
type Config struct {
	// NodeName is the Kubernetes node name this process registers as.
	// Defaults to the OS hostname when empty.
	NodeName string `yaml:"nodeName"`

	// KubeconfigPath points at the kubeconfig used to reach the API server.
	// When empty, in-cluster config (or KUBECONFIG env) is used.
	KubeconfigPath string `yaml:"kubeconfigPath"`

	// RuntimeSocket is reserved for a future apple/container service API path.
	// P1 drives the runtime through RuntimeBinary, but the field remains in the
	// config so the eventual service transport has a stable home.
	RuntimeSocket string `yaml:"runtimeSocket"`

	// RuntimeBinary is the apple/container CLI to drive (path or PATH-resolved
	// name). Defaults to "container".
	RuntimeBinary string `yaml:"runtimeBinary"`

	// LogLevel is the klog verbosity ("info", "debug") or a numeric V level.
	LogLevel string `yaml:"logLevel"`

	// Node describes the shape this Mac advertises as a Kubernetes node:
	// capacity, addresses, labels, and scheduling taints.
	Node NodeConfig `yaml:"node"`
}

// NodeConfig is the configurable shape of the virtual node registered with
// Kubernetes. Capacity is intentionally conservative until P1 density data is
// recorded (see docs/MASTER_TASKS.md), and everything here can be overridden
// per host via the YAML config rather than hardcoded in the binary.
type NodeConfig struct {
	// CPU is the node's CPU capacity as a Kubernetes quantity (e.g. "4").
	CPU string `yaml:"cpu"`
	// Memory is the node's memory capacity as a quantity (e.g. "8Gi").
	Memory string `yaml:"memory"`
	// Pods is the maximum number of Pods schedulable to this node (e.g. "32").
	Pods string `yaml:"pods"`

	// OS and Arch are the operating system and CPU architecture of the workloads
	// this node runs. MacVz runs Linux micro-VMs on Apple Silicon, so these
	// default to linux/arm64 (the workload platform), not the Darwin host.
	OS   string `yaml:"os"`
	Arch string `yaml:"arch"`

	// InternalIP is the node's reachable address. When empty, the kubelet
	// detects the host's primary outbound IPv4 at startup.
	InternalIP string `yaml:"internalIP"`

	// Labels and Annotations are merged onto the built-in node metadata. User
	// entries override built-ins on key collision.
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`

	// Taints are applied to the node so workloads only land here when they
	// explicitly tolerate MacVz. Defaults to the well-known Virtual Kubelet
	// provider taint with NoSchedule.
	Taints []TaintConfig `yaml:"taints"`
}

// TaintConfig is a YAML-friendly node taint.
type TaintConfig struct {
	Key    string `yaml:"key"`
	Value  string `yaml:"value"`
	Effect string `yaml:"effect"` // NoSchedule | PreferNoSchedule | NoExecute
}

// DefaultProviderTaintKey is the well-known Virtual Kubelet taint applied so
// only Pods that explicitly tolerate MacVz are scheduled here.
const DefaultProviderTaintKey = "virtual-kubelet.io/provider"

// Default returns a Config populated with built-in defaults.
func Default() Config {
	host, _ := os.Hostname()
	return Config{
		NodeName:      host,
		RuntimeSocket: "/var/run/com.apple.container.sock",
		RuntimeBinary: "container",
		LogLevel:      "info",
		Node: NodeConfig{
			// Conservative capacity until P1 density data is recorded.
			CPU:    "2",
			Memory: "4Gi",
			Pods:   "20",
			OS:     "linux",
			Arch:   "arm64",
			Taints: []TaintConfig{{
				Key:    DefaultProviderTaintKey,
				Value:  "macvz",
				Effect: string(corev1.TaintEffectNoSchedule),
			}},
		},
	}
}

// Capacity returns the node capacity/allocatable resource list parsed from the
// configured quantities.
func (c Config) Capacity() (corev1.ResourceList, error) {
	out := corev1.ResourceList{}
	for name, raw := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:    c.Node.CPU,
		corev1.ResourceMemory: c.Node.Memory,
		corev1.ResourcePods:   c.Node.Pods,
	} {
		q, err := resource.ParseQuantity(raw)
		if err != nil {
			return nil, fmt.Errorf("node.%s quantity %q: %w", name, raw, err)
		}
		out[name] = q
	}
	return out, nil
}

// Taints returns the configured taints as Kubernetes core types, validating
// each effect.
func (c Config) Taints() ([]corev1.Taint, error) {
	out := make([]corev1.Taint, 0, len(c.Node.Taints))
	for _, t := range c.Node.Taints {
		if t.Key == "" {
			return nil, fmt.Errorf("node taint key must not be empty")
		}
		effect := corev1.TaintEffect(t.Effect)
		switch effect {
		case corev1.TaintEffectNoSchedule,
			corev1.TaintEffectPreferNoSchedule,
			corev1.TaintEffectNoExecute:
		default:
			return nil, fmt.Errorf("node taint %q: invalid effect %q", t.Key, t.Effect)
		}
		out = append(out, corev1.Taint{Key: t.Key, Value: t.Value, Effect: effect})
	}
	return out, nil
}

// Load reads and parses the YAML config at path, layering it over defaults.
// A non-existent path is not an error: defaults are returned. This lets the
// binary run with zero config during early development.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// RestConfig builds the Kubernetes client REST config used to reach the API
// server, resolving sources in this order:
//
//  1. KubeconfigPath, when set. The file must exist and parse, otherwise a clear
//     error is returned (no silent fallback — a misconfigured node should fail
//     loudly at startup).
//  2. The KUBECONFIG env var and the default ~/.kube/config loading rules.
//  3. In-cluster config, when running inside a Pod.
func (c Config) RestConfig() (*rest.Config, error) {
	if c.KubeconfigPath != "" {
		if _, err := os.Stat(c.KubeconfigPath); err != nil {
			return nil, fmt.Errorf("kubeconfig %q: %w", c.KubeconfigPath, err)
		}
		cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: c.KubeconfigPath},
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig %q: %w", c.KubeconfigPath, err)
		}
		return cfg, nil
	}

	// No explicit path: honor KUBECONFIG / default rules, then in-cluster.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err == nil {
		return cfg, nil
	}

	inCluster, inErr := rest.InClusterConfig()
	if inErr != nil {
		return nil, fmt.Errorf("no kubeconfig found (%v) and not running in-cluster (%w)", err, inErr)
	}
	return inCluster, nil
}

// Validate checks that required fields are present and coherent.
func (c Config) Validate() error {
	if c.NodeName == "" {
		return fmt.Errorf("nodeName must be set (hostname lookup failed)")
	}
	if _, err := c.Capacity(); err != nil {
		return err
	}
	if _, err := c.Taints(); err != nil {
		return err
	}
	return nil
}
