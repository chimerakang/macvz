// Package config loads macvz-kubelet configuration from a YAML file, applying
// sensible defaults and validating required fields.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
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
}

// Default returns a Config populated with built-in defaults.
func Default() Config {
	host, _ := os.Hostname()
	return Config{
		NodeName:      host,
		RuntimeSocket: "/var/run/com.apple.container.sock",
		RuntimeBinary: "container",
		LogLevel:      "info",
	}
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
	return nil
}
